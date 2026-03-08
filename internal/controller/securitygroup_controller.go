/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

const (
	osacSecurityGroupFinalizer = "osac.openshift.io/securitygroup-finalizer"
)

// SecurityGroupReconciler reconciles a SecurityGroup object
type SecurityGroupReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	SecurityGroupNamespace string
	ProvisioningProvider   provisioning.ProvisioningProvider
	StatusPollInterval     time.Duration
	MaxJobHistory          int
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=securitygroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=securitygroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=securitygroups/finalizers,verbs=update
// +kubebuilder:rbac:groups=osac.openshift.io,resources=virtualnetworks,verbs=get;list;watch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=networkclasses,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SecurityGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	sg := &v1alpha1.SecurityGroup{}
	err := r.Get(ctx, req.NamespacedName, sg)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("start reconcile")

	oldstatus := sg.Status.DeepCopy()

	var res ctrl.Result
	if sg.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.handleUpdate(ctx, sg)
	} else {
		res, err = r.handleDelete(ctx, sg)
	}

	if !equality.Semantic.DeepEqual(sg.Status, *oldstatus) {
		log.Info("status requires update")
		if updateErr := r.Status().Update(ctx, sg); updateErr != nil {
			log.Error(updateErr, "failed to update status")
			return res, updateErr
		}
	}

	log.Info("end reconcile")
	return res, err
}

func (r *SecurityGroupReconciler) handleUpdate(ctx context.Context, sg *v1alpha1.SecurityGroup) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Add finalizer if not present
	if controllerutil.AddFinalizer(sg, osacSecurityGroupFinalizer) {
		if err := r.Update(ctx, sg); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch so we have the latest resourceVersion and status
		if err := r.Get(ctx, client.ObjectKeyFromObject(sg), sg); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set initial phase to Progressing
	if sg.Status.Phase == "" {
		sg.Status.Phase = v1alpha1.SecurityGroupPhaseProgressing
	}

	// Lookup parent VirtualNetwork
	vnet := &v1alpha1.VirtualNetwork{}
	err := r.Get(ctx, client.ObjectKey{
		Name:      sg.Spec.VirtualNetwork,
		Namespace: sg.Namespace,
	}, vnet)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("parent VirtualNetwork not found, requeueing", "name", sg.Spec.VirtualNetwork)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get parent VirtualNetwork: %w", err)
	}

	// Lookup NetworkClass from parent VirtualNetwork
	networkClass := &v1alpha1.NetworkClass{}
	err = r.Get(ctx, client.ObjectKey{Name: vnet.Spec.NetworkClass, Namespace: sg.Namespace}, networkClass)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("NetworkClass not found, requeueing", "name", vnet.Spec.NetworkClass)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get NetworkClass: %w", err)
	}

	// Handle provisioning
	return r.handleProvisioning(ctx, sg, networkClass)
}

func (r *SecurityGroupReconciler) handleDelete(ctx context.Context, sg *v1alpha1.SecurityGroup) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("deleting security group")

	sg.Status.Phase = v1alpha1.SecurityGroupPhaseDeleting

	// Finalizer already removed, cleanup complete
	if !controllerutil.ContainsFinalizer(sg, osacSecurityGroupFinalizer) {
		return ctrl.Result{}, nil
	}

	// Handle deprovisioning
	result, err := r.handleDeprovisioning(ctx, sg)
	if err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(sg, osacSecurityGroupFinalizer)
	if err := r.Update(ctx, sg); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handleProvisioning manages the provisioning job lifecycle for a SecurityGroup.
// It triggers provisioning if needed and polls job status until completion.
func (r *SecurityGroupReconciler) handleProvisioning(ctx context.Context, sg *v1alpha1.SecurityGroup, networkClass *v1alpha1.NetworkClass) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// If no provider configured, skip provisioning
	if r.ProvisioningProvider == nil {
		log.Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	// Check if we need to trigger a provision job
	latestProvisionJob := v1alpha1.FindLatestJobByType(sg.Status.Jobs, v1alpha1.JobTypeProvision)

	if r.needsProvisionJob(sg, latestProvisionJob) {
		log.Info("triggering provisioning", "provider", r.ProvisioningProvider.Name(), "strategy", networkClass.Spec.ImplementationStrategy)
		result, err := r.ProvisioningProvider.TriggerProvision(ctx, sg)
		if err != nil {
			// Check if this is a rate limit error
			var rateLimitErr *provisioning.RateLimitError
			if errors.As(err, &rateLimitErr) {
				log.Info("provisioning request rate-limited, will retry", "retryAfter", rateLimitErr.RetryAfter)
				return ctrl.Result{RequeueAfter: rateLimitErr.RetryAfter}, nil
			}

			// Actual error - mark as failed
			log.Error(err, "failed to trigger provisioning")
			newJob := v1alpha1.JobStatus{
				JobID:     "",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(time.Now().UTC()),
				State:     v1alpha1.JobStateFailed,
				Message:   fmt.Sprintf("Failed to trigger provisioning: %v", err),
			}
			sg.Status.Jobs = r.appendJob(sg.Status.Jobs, newJob)
			sg.Status.Phase = v1alpha1.SecurityGroupPhaseFailed
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		newJob := v1alpha1.JobStatus{
			JobID:                  result.JobID,
			Type:                   v1alpha1.JobTypeProvision,
			Timestamp:              metav1.NewTime(time.Now().UTC()),
			State:                  result.InitialState,
			Message:                result.Message,
			BlockDeletionOnFailure: false,
		}
		sg.Status.Jobs = r.appendJob(sg.Status.Jobs, newJob)
		log.Info("provisioning job triggered", "jobID", result.JobID)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// We have a job ID, check its status
	status, err := r.ProvisioningProvider.GetProvisionStatus(ctx, sg, latestProvisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get provision job status", "jobID", latestProvisionJob.JobID)
		updatedJob := *latestProvisionJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		r.updateJob(sg.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Update job status
	updatedJob := *latestProvisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.Message
	if status.ErrorDetails != "" {
		updatedJob.Message = fmt.Sprintf("%s: %s", status.Message, status.ErrorDetails)
	}
	r.updateJob(sg.Status.Jobs, updatedJob)

	// If job is still running, requeue
	if !status.State.IsTerminal() {
		log.Info("provision job still running", "jobID", latestProvisionJob.JobID, "state", status.State)
		sg.Status.Phase = v1alpha1.SecurityGroupPhaseProgressing
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Job is complete
	if status.State.IsSuccessful() {
		log.Info("provision job succeeded", "jobID", latestProvisionJob.JobID)
		sg.Status.Phase = v1alpha1.SecurityGroupPhaseReady
		return ctrl.Result{}, nil
	}

	// Job failed
	log.Info("provision job failed", "jobID", latestProvisionJob.JobID, "message", updatedJob.Message)
	sg.Status.Phase = v1alpha1.SecurityGroupPhaseFailed
	return ctrl.Result{}, nil
}

// handleDeprovisioning manages the deprovisioning job lifecycle for a SecurityGroup.
// It triggers deprovisioning if needed and polls job status until completion.
func (r *SecurityGroupReconciler) handleDeprovisioning(ctx context.Context, sg *v1alpha1.SecurityGroup) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// If no provider configured, skip deprovisioning
	if r.ProvisioningProvider == nil {
		log.Info("no provisioning provider configured, skipping deprovisioning")
		return ctrl.Result{}, nil
	}

	// Check if we already have a deprovision job
	latestDeprovisionJob := v1alpha1.FindLatestJobByType(sg.Status.Jobs, v1alpha1.JobTypeDeprovision)

	// Trigger deprovisioning
	if latestDeprovisionJob == nil || latestDeprovisionJob.JobID == "" {
		log.Info("triggering deprovisioning", "provider", r.ProvisioningProvider.Name())

		result, err := r.ProvisioningProvider.TriggerDeprovision(ctx, sg)
		if err != nil {
			// Check if this is a rate limit error
			var rateLimitErr *provisioning.RateLimitError
			if errors.As(err, &rateLimitErr) {
				log.Info("deprovisioning request rate-limited, will retry", "retryAfter", rateLimitErr.RetryAfter)
				return ctrl.Result{RequeueAfter: rateLimitErr.RetryAfter}, nil
			}

			// Actual error
			log.Error(err, "failed to trigger deprovisioning")
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		// Handle provider action
		switch result.Action {
		case provisioning.DeprovisionWaiting:
			log.Info("deprovisioning not ready, requeueing")
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil

		case provisioning.DeprovisionSkipped:
			log.Info("provider skipped deprovisioning")
			return ctrl.Result{}, nil

		case provisioning.DeprovisionTriggered:
			newJob := v1alpha1.JobStatus{
				JobID:                  result.JobID,
				Type:                   v1alpha1.JobTypeDeprovision,
				Timestamp:              metav1.NewTime(time.Now().UTC()),
				State:                  v1alpha1.JobStatePending,
				Message:                "Deprovisioning job triggered",
				BlockDeletionOnFailure: result.BlockDeletionOnFailure,
			}
			sg.Status.Jobs = r.appendJob(sg.Status.Jobs, newJob)
			log.Info("deprovisioning job triggered", "jobID", result.JobID)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	// We have a job ID, check its status
	status, err := r.ProvisioningProvider.GetDeprovisionStatus(ctx, sg, latestDeprovisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get deprovision job status", "jobID", latestDeprovisionJob.JobID)
		updatedJob := *latestDeprovisionJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		r.updateJob(sg.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Update job status
	updatedJob := *latestDeprovisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.Message
	if status.ErrorDetails != "" {
		updatedJob.Message = fmt.Sprintf("%s: %s", status.Message, status.ErrorDetails)
	}
	r.updateJob(sg.Status.Jobs, updatedJob)

	// If job is still running, requeue
	if !status.State.IsTerminal() {
		log.Info("deprovision job still running", "jobID", latestDeprovisionJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Job reached terminal state
	if status.State.IsSuccessful() {
		log.Info("deprovision job succeeded", "jobID", latestDeprovisionJob.JobID)
		return ctrl.Result{}, nil
	}

	// Job failed or was canceled - check policy
	if latestDeprovisionJob.BlockDeletionOnFailure {
		log.Info("deprovision job failed, blocking deletion to prevent orphaned resources",
			"jobID", latestDeprovisionJob.JobID,
			"state", status.State,
			"message", updatedJob.Message)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	log.Info("deprovision job did not succeed, allowing process to continue",
		"jobID", latestDeprovisionJob.JobID,
		"state", status.State,
		"message", updatedJob.Message)
	return ctrl.Result{}, nil
}

// needsProvisionJob determines if a new provision job should be triggered.
func (r *SecurityGroupReconciler) needsProvisionJob(sg *v1alpha1.SecurityGroup, latestJob *v1alpha1.JobStatus) bool {
	// No job exists yet
	if latestJob == nil {
		return true
	}

	// Job exists but has no ID (trigger failed)
	if latestJob.JobID == "" {
		return false
	}

	// Job is still running
	if !latestJob.State.IsTerminal() {
		return false
	}

	// Job reached terminal state - don't trigger another
	return false
}

// appendJob adds a new job to the jobs array and trims to maxHistory.
func (r *SecurityGroupReconciler) appendJob(jobs []v1alpha1.JobStatus, newJob v1alpha1.JobStatus) []v1alpha1.JobStatus {
	jobs = append(jobs, newJob)
	if len(jobs) > r.MaxJobHistory {
		jobs = jobs[len(jobs)-r.MaxJobHistory:]
	}
	return jobs
}

// updateJob updates an existing job by ID with new values.
func (r *SecurityGroupReconciler) updateJob(jobs []v1alpha1.JobStatus, updatedJob v1alpha1.JobStatus) {
	for i := range jobs {
		if jobs[i].JobID == updatedJob.JobID {
			jobs[i] = updatedJob
			return
		}
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SecurityGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SecurityGroup{}).
		WithEventFilter(SecurityGroupNamespacePredicate(r.SecurityGroupNamespace)).
		Complete(r)
}

// SecurityGroupNamespacePredicate filters events to only the configured namespace.
func SecurityGroupNamespacePredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(
		func(obj client.Object) bool {
			return obj.GetNamespace() == namespace
		},
	)
}
