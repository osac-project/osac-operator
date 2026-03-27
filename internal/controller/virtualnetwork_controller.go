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
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

const (
	osacVirtualNetworkFinalizer = "osac.openshift.io/virtualnetwork-finalizer"
	defaultStatusPollInterval   = 30 * time.Second
	defaultMaxJobHistory        = 10
)

// VirtualNetworkReconciler reconciles a VirtualNetwork object
type VirtualNetworkReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	NetworkingNamespace  string
	ProvisioningProvider provisioning.ProvisioningProvider
	StatusPollInterval   time.Duration
	MaxJobHistory        int
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=virtualnetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=virtualnetworks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=virtualnetworks/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *VirtualNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	vnet := &v1alpha1.VirtualNetwork{}
	if err := r.Get(ctx, req.NamespacedName, vnet); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("start reconcile")

	oldstatus := vnet.Status.DeepCopy()

	var res ctrl.Result
	var err error
	if vnet.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.handleUpdate(ctx, vnet)
	} else {
		res, err = r.handleDelete(ctx, vnet)
	}

	if !equality.Semantic.DeepEqual(vnet.Status, *oldstatus) {
		log.Info("status requires update")
		if err := r.Status().Update(ctx, vnet); err != nil {
			return res, err
		}
	}

	log.Info("end reconcile")
	return res, err
}

// handleUpdate processes VirtualNetwork creation and updates
func (r *VirtualNetworkReconciler) handleUpdate(ctx context.Context, vnet *v1alpha1.VirtualNetwork) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Add finalizer if not present
	if controllerutil.AddFinalizer(vnet, osacVirtualNetworkFinalizer) {
		if err := r.Update(ctx, vnet); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set phase to Progressing
	vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseProgressing

	// Read implementation strategy from spec (populated by fulfillment-service from NetworkClass)
	implementationStrategy := vnet.Spec.ImplementationStrategy
	if implementationStrategy == "" {
		log.Info("implementation strategy not set, requeueing", "virtualNetwork", vnet.Name)
		return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
	}

	// Add implementation-strategy annotation if not present or different
	// This allows AAP playbooks to select the appropriate role without doing lookups
	if vnet.Annotations == nil {
		vnet.Annotations = make(map[string]string)
	}
	if vnet.Annotations[osacImplementationStrategyAnnotation] != implementationStrategy {
		vnet.Annotations[osacImplementationStrategyAnnotation] = implementationStrategy
		log.Info("setting implementation-strategy annotation", "strategy", implementationStrategy)
		// Preserve the status we've set since Update returns server state (empty status)
		currentStatus := vnet.Status.DeepCopy()
		if err := r.Update(ctx, vnet); err != nil {
			return ctrl.Result{}, err
		}
		vnet.Status = *currentStatus
	}

	// Handle provisioning
	return r.handleProvisioning(ctx, vnet)
}

// handleProvisioning manages the provisioning job lifecycle for a VirtualNetwork.
// It triggers provisioning if needed and polls job status until completion.
func (r *VirtualNetworkReconciler) handleProvisioning(ctx context.Context, vnet *v1alpha1.VirtualNetwork) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// If no provider configured, skip provisioning
	if r.ProvisioningProvider == nil {
		log.Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	// Check if we need to trigger a (new) provision job
	latestProvisionJob := provisioning.FindLatestJobByType(vnet.Status.Jobs, v1alpha1.JobTypeProvision)

	if provisioning.NeedsProvisionJob(latestProvisionJob) {
		log.Info("triggering provisioning", "provider", r.ProvisioningProvider.Name())
		result, err := r.ProvisioningProvider.TriggerProvision(ctx, vnet)
		if err != nil {
			log.Error(err, "failed to trigger provisioning")
			return ctrl.Result{}, err
		}

		newJob := v1alpha1.JobStatus{
			JobID:     result.JobID,
			Type:      v1alpha1.JobTypeProvision,
			Timestamp: metav1.NewTime(time.Now().UTC()),
			State:     result.InitialState,
			Message:   result.Message,
		}
		vnet.Status.Jobs = provisioning.AppendJob(vnet.Status.Jobs, newJob, r.MaxJobHistory)
		log.Info("provisioning job triggered", "jobID", result.JobID)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// We have a job ID, check its status
	status, err := r.ProvisioningProvider.GetProvisionStatus(ctx, vnet, latestProvisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get provision job status", "jobID", latestProvisionJob.JobID)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Update job status
	updatedJob := *latestProvisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.Message
	provisioning.UpdateJob(vnet.Status.Jobs, updatedJob)

	// If job is still running, requeue
	if !status.State.IsTerminal() {
		log.Info("provision job still running", "jobID", latestProvisionJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Job is complete
	if status.State.IsSuccessful() {
		log.Info("provision job succeeded", "jobID", latestProvisionJob.JobID)
		vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseReady
	} else {
		log.Info("provision job failed", "jobID", latestProvisionJob.JobID, "message", updatedJob.Message)
		vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseFailed
	}

	return ctrl.Result{}, nil
}

// handleDelete processes VirtualNetwork deletion
func (r *VirtualNetworkReconciler) handleDelete(ctx context.Context, vnet *v1alpha1.VirtualNetwork) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("deleting virtual network")

	vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseDeleting

	// Base finalizer has already been removed, cleanup complete
	if !controllerutil.ContainsFinalizer(vnet, osacVirtualNetworkFinalizer) {
		return ctrl.Result{}, nil
	}

	// Handle deprovisioning
	result, err := r.handleDeprovisioning(ctx, vnet)
	if err != nil {
		return result, err
	}

	// If we need to requeue (jobs still running), do so
	if result.RequeueAfter > 0 {
		return result, nil
	}

	// Deprovisioning complete or skipped, remove base finalizer
	if controllerutil.RemoveFinalizer(vnet, osacVirtualNetworkFinalizer) {
		if err := r.Update(ctx, vnet); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// handleDeprovisioning manages the deprovisioning job lifecycle for a VirtualNetwork.
// It triggers deprovisioning if needed and polls job status until completion.
func (r *VirtualNetworkReconciler) handleDeprovisioning(ctx context.Context, vnet *v1alpha1.VirtualNetwork) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Check if we already have a deprovision job
	latestDeprovisionJob := provisioning.FindLatestJobByType(vnet.Status.Jobs, v1alpha1.JobTypeDeprovision)

	// Trigger deprovisioning - provider decides internally if ready
	if latestDeprovisionJob == nil || latestDeprovisionJob.JobID == "" {
		log.Info("triggering deprovisioning", "provider", r.ProvisioningProvider.Name())

		result, err := r.ProvisioningProvider.TriggerDeprovision(ctx, vnet)
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
			vnet.Status.Jobs = provisioning.AppendJob(vnet.Status.Jobs, newJob, r.MaxJobHistory)
			log.Info("deprovisioning job triggered", "jobID", result.JobID)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	// We have a job ID, check its status
	status, err := r.ProvisioningProvider.GetDeprovisionStatus(ctx, vnet, latestDeprovisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get deprovision job status", "jobID", latestDeprovisionJob.JobID)
		updatedJob := *latestDeprovisionJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		provisioning.UpdateJob(vnet.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Update job status
	updatedJob := *latestDeprovisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.MessageWithDetails()
	provisioning.UpdateJob(vnet.Status.Jobs, updatedJob)

	// If job is still running, requeue
	if !status.State.IsTerminal() {
		log.Info("deprovision job still running", "jobID", latestDeprovisionJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Job reached terminal state (Succeeded, Failed, or Canceled)
	if status.State.IsSuccessful() {
		log.Info("deprovision job succeeded", "jobID", latestDeprovisionJob.JobID)
		return ctrl.Result{}, nil
	}

	// Job failed or was canceled
	// Check policy stored in job status
	if latestDeprovisionJob.BlockDeletionOnFailure {
		// Block deletion to prevent orphaned resources
		log.Info("deprovision job failed, blocking deletion to prevent orphaned resources",
			"jobID", latestDeprovisionJob.JobID,
			"state", status.State,
			"message", updatedJob.Message)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	} else {
		// Allow process to continue
		log.Info("deprovision job did not succeed, allowing process to continue",
			"jobID", latestDeprovisionJob.JobID,
			"state", status.State,
			"message", updatedJob.Message)
		return ctrl.Result{}, nil
	}
}

// NetworkingNamespacePredicate filters events by namespace for networking resources.
func NetworkingNamespacePredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(
		func(obj client.Object) bool {
			return obj.GetNamespace() == namespace
		},
	)
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.VirtualNetwork{}).
		WithEventFilter(NetworkingNamespacePredicate(r.NetworkingNamespace)).
		Complete(r)
}
