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
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/helpers"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

const (
	osacSecurityGroupFinalizer = "osac.openshift.io/securitygroup-finalizer"
)

// SecurityGroupReconciler reconciles a SecurityGroup object
type SecurityGroupReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	NetworkingNamespace  string
	ProvisioningProvider provisioning.ProvisioningProvider
	StatusPollInterval   time.Duration
	MaxJobHistory        int
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=securitygroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=securitygroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=securitygroups/finalizers,verbs=update
// +kubebuilder:rbac:groups=osac.openshift.io,resources=virtualnetworks,verbs=get;list;watch

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

	// Lookup parent VirtualNetwork by UUID label to get implementation strategy
	vnetList := &v1alpha1.VirtualNetworkList{}
	err := r.List(ctx, vnetList,
		client.InNamespace(sg.Namespace),
		client.MatchingLabels{osacVirtualNetworkIDLabel: sg.Spec.VirtualNetwork},
	)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list VirtualNetworks: %w", err)
	}
	if len(vnetList.Items) == 0 {
		log.Info("parent VirtualNetwork not found, requeueing", "uuid", sg.Spec.VirtualNetwork)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	vnet := &vnetList.Items[0]

	// Read implementation strategy from parent VirtualNetwork spec
	implementationStrategy := vnet.Spec.ImplementationStrategy
	if implementationStrategy == "" {
		log.Info("implementation strategy not set on parent VirtualNetwork, requeueing", "virtualNetwork", vnet.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Handle provisioning
	return r.handleProvisioning(ctx, sg, implementationStrategy)
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
func (r *SecurityGroupReconciler) handleProvisioning(ctx context.Context, sg *v1alpha1.SecurityGroup, implementationStrategy string) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// If no provider configured, skip provisioning
	if r.ProvisioningProvider == nil {
		log.Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	// Check if we need to trigger a provision job
	latestProvisionJob := v1alpha1.FindLatestJobByType(sg.Status.Jobs, v1alpha1.JobTypeProvision)

	if helpers.NeedsProvisionJob(latestProvisionJob) {
		// Fetch fresh CR from API server to avoid stale cache issues (see PR #131)
		freshSG := &v1alpha1.SecurityGroup{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sg), freshSG); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to fetch fresh SecurityGroup: %w", err)
		}

		// Re-check with fresh data
		latestProvisionJob = v1alpha1.FindLatestJobByType(freshSG.Status.Jobs, v1alpha1.JobTypeProvision)
		if !helpers.NeedsProvisionJob(latestProvisionJob) {
			log.Info("provision job already exists (cache was stale), skipping trigger")
			// Update local copy with fresh status
			sg.Status = freshSG.Status
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		log.Info("triggering provisioning", "provider", r.ProvisioningProvider.Name(), "strategy", implementationStrategy)
		result, err := r.ProvisioningProvider.TriggerProvision(ctx, sg)
		if err != nil {
			log.Error(err, "failed to trigger provisioning")
			newJob := v1alpha1.JobStatus{
				JobID:     "",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(time.Now().UTC()),
				State:     v1alpha1.JobStateFailed,
				Message:   fmt.Sprintf("Failed to trigger provisioning: %v", err),
			}
			sg.Status.Jobs = helpers.AppendJob(sg.Status.Jobs, newJob, r.MaxJobHistory)
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
		sg.Status.Jobs = helpers.AppendJob(sg.Status.Jobs, newJob, r.MaxJobHistory)
		log.Info("provisioning job triggered", "jobID", result.JobID)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// We have a job ID, check its status
	status, err := r.ProvisioningProvider.GetProvisionStatus(ctx, sg, latestProvisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get provision job status", "jobID", latestProvisionJob.JobID)
		updatedJob := *latestProvisionJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		helpers.UpdateJob(sg.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Update job status
	updatedJob := *latestProvisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.Message
	if status.ErrorDetails != "" {
		updatedJob.Message = fmt.Sprintf("%s: %s", status.Message, status.ErrorDetails)
	}
	helpers.UpdateJob(sg.Status.Jobs, updatedJob)

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
			sg.Status.Jobs = helpers.AppendJob(sg.Status.Jobs, newJob, r.MaxJobHistory)
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
		helpers.UpdateJob(sg.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Update job status
	updatedJob := *latestDeprovisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.Message
	if status.ErrorDetails != "" {
		updatedJob.Message = fmt.Sprintf("%s: %s", status.Message, status.ErrorDetails)
	}
	helpers.UpdateJob(sg.Status.Jobs, updatedJob)

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

// SetupWithManager sets up the controller with the Manager.
func (r *SecurityGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SecurityGroup{}).
		WithEventFilter(NetworkingNamespacePredicate(r.NetworkingNamespace)).
		Complete(r)
}
