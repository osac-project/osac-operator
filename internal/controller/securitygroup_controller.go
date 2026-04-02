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
	"github.com/osac-project/osac-operator/internal/provisioning"
)

const (
	osacSecurityGroupFinalizer = "osac.openshift.io/securitygroup-finalizer"
)

// SecurityGroupReconciler reconciles a SecurityGroup object
type SecurityGroupReconciler struct {
	client.Client
	APIReader            client.Reader
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
		return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
	}
	vnet := &vnetList.Items[0]

	// Read implementation strategy from parent VirtualNetwork spec
	implementationStrategy := vnet.Spec.ImplementationStrategy
	if implementationStrategy == "" {
		log.Info("implementation strategy not set on parent VirtualNetwork, requeueing", "virtualNetwork", vnet.Name)
		return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
	}

	// Add implementation-strategy annotation if not present or different
	// This allows AAP playbooks to select the appropriate role without doing lookups
	if sg.Annotations == nil {
		sg.Annotations = make(map[string]string)
	}
	if sg.Annotations[osacImplementationStrategyAnnotation] != implementationStrategy {
		sg.Annotations[osacImplementationStrategyAnnotation] = implementationStrategy
		log.Info("setting implementation-strategy annotation", "strategy", implementationStrategy)
		if err := r.Update(ctx, sg); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sg), sg); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Compute desired config version from spec and inherited implementation strategy
	desiredVersion, err := provisioning.ComputeDesiredConfigVersion(struct {
		Spec                   v1alpha1.SecurityGroupSpec
		ImplementationStrategy string
	}{sg.Spec, implementationStrategy})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to compute desired config version: %w", err)
	}
	sg.Status.DesiredConfigVersion = desiredVersion

	// Set phase to Progressing only on first provision (empty phase) or when spec changed
	// after a previous success. Don't override Failed during backoff.
	if sg.Status.Phase == "" || (sg.Status.Phase == v1alpha1.SecurityGroupPhaseReady &&
		!provisioning.IsConfigApplied(&sg.Status.Jobs, sg.Status.DesiredConfigVersion)) {
		sg.Status.Phase = v1alpha1.SecurityGroupPhaseProgressing
	}

	// Handle provisioning
	return r.handleProvisioning(ctx, sg)
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
// Uses shared RunProvisioningLifecycle with config-version-based backoff on failure.
func (r *SecurityGroupReconciler) handleProvisioning(ctx context.Context, sg *v1alpha1.SecurityGroup) (ctrl.Result, error) {
	if r.ProvisioningProvider == nil {
		ctrllog.FromContext(ctx).Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	return provisioning.RunProvisioningLifecycle(ctx, r.ProvisioningProvider, sg,
		&provisioning.State{Jobs: &sg.Status.Jobs, DesiredConfigVersion: sg.Status.DesiredConfigVersion},
		r.MaxJobHistory, r.StatusPollInterval,
		&provisioning.PollCallbacks{
			OnFailed:  func(_ string) { sg.Status.Phase = v1alpha1.SecurityGroupPhaseFailed },
			OnSuccess: func(_ provisioning.ProvisionStatus) { sg.Status.Phase = v1alpha1.SecurityGroupPhaseReady },
		},
		func() bool {
			return provisioning.CheckAPIServerForNonTerminalProvisionJob(ctx, r.APIReader, client.ObjectKeyFromObject(sg), &v1alpha1.SecurityGroup{})
		},
	)
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
	latestDeprovisionJob := provisioning.FindLatestJobByType(sg.Status.Jobs, v1alpha1.JobTypeDeprovision)

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
			sg.Status.Jobs = provisioning.AppendJob(sg.Status.Jobs, newJob, r.MaxJobHistory)
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
		provisioning.UpdateJob(sg.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Update job status
	updatedJob := *latestDeprovisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.MessageWithDetails()
	provisioning.UpdateJob(sg.Status.Jobs, updatedJob)

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
