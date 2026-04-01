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
	// osacSubnetFinalizer is the finalizer for Subnet resources
	osacSubnetFinalizer = "osac.openshift.io/subnet-finalizer"

	// defaultSubnetStatusPollInterval is the default interval for polling job status
	defaultSubnetStatusPollInterval = 30 * time.Second

	// defaultSubnetMaxJobHistory is the default maximum number of jobs to keep in history
	defaultSubnetMaxJobHistory = 10
)

// SubnetReconciler reconciles a Subnet object
type SubnetReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	NetworkingNamespace  string
	ProvisioningProvider provisioning.ProvisioningProvider
	StatusPollInterval   time.Duration
	MaxJobHistory        int
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=subnets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=subnets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=subnets/finalizers,verbs=update
// +kubebuilder:rbac:groups=osac.openshift.io,resources=virtualnetworks,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SubnetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	subnet := &v1alpha1.Subnet{}
	err := r.Client.Get(ctx, req.NamespacedName, subnet)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	val, exists := subnet.Annotations[osacManagementStateAnnotation]
	if exists && val == ManagementStateUnmanaged {
		log.Info("ignoring Subnet due to management-state annotation", "management-state", val)
		return ctrl.Result{}, nil
	}

	log.Info("start reconcile")

	oldstatus := subnet.Status.DeepCopy()

	var res ctrl.Result
	if subnet.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.handleUpdate(ctx, subnet)
	} else {
		res, err = r.handleDelete(ctx, subnet)
	}

	if !equality.Semantic.DeepEqual(subnet.Status, *oldstatus) {
		log.Info("status requires update")
		if err := r.Status().Update(ctx, subnet); err != nil {
			return res, err
		}
	}

	log.Info("end reconcile")
	return res, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *SubnetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Subnet{}).
		WithEventFilter(NetworkingNamespacePredicate(r.NetworkingNamespace)).
		Complete(r)
}

func (r *SubnetReconciler) handleUpdate(ctx context.Context, subnet *v1alpha1.Subnet) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Add finalizer if not present
	if controllerutil.AddFinalizer(subnet, osacSubnetFinalizer) {
		if err := r.Update(ctx, subnet); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set phase to Progressing
	subnet.Status.Phase = v1alpha1.SubnetPhaseProgressing

	// Get parent VirtualNetwork by UUID label to read implementation strategy
	vnetList := &v1alpha1.VirtualNetworkList{}
	err := r.List(ctx, vnetList,
		client.InNamespace(subnet.Namespace),
		client.MatchingLabels{osacVirtualNetworkIDLabel: subnet.Spec.VirtualNetwork},
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(vnetList.Items) == 0 {
		log.Info("parent VirtualNetwork not found, requeueing", "uuid", subnet.Spec.VirtualNetwork)
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
	if subnet.Annotations == nil {
		subnet.Annotations = make(map[string]string)
	}
	if subnet.Annotations[osacImplementationStrategyAnnotation] != implementationStrategy {
		subnet.Annotations[osacImplementationStrategyAnnotation] = implementationStrategy
		log.Info("setting implementation-strategy annotation", "strategy", implementationStrategy)
		// Preserve the status we've set since Update returns server state (empty status)
		currentStatus := subnet.Status.DeepCopy()
		if err := r.Update(ctx, subnet); err != nil {
			return ctrl.Result{}, err
		}
		subnet.Status = *currentStatus
	}

	// Compute desired config version from spec
	desiredVersion, err := provisioning.ComputeDesiredConfigVersion(subnet.Spec)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to compute desired config version: %w", err)
	}
	subnet.Status.DesiredConfigVersion = desiredVersion

	// Handle provisioning
	return r.handleProvisioning(ctx, subnet)
}

func (r *SubnetReconciler) handleDelete(ctx context.Context, subnet *v1alpha1.Subnet) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("deleting subnet")

	subnet.Status.Phase = v1alpha1.SubnetPhaseDeleting

	// Base finalizer has already been removed, cleanup complete
	if !controllerutil.ContainsFinalizer(subnet, osacSubnetFinalizer) {
		return ctrl.Result{}, nil
	}

	// Handle deprovisioning
	result, err := r.handleDeprovisioning(ctx, subnet)
	if err != nil {
		return result, err
	}

	// If we need to requeue (jobs still running), do so
	if result.RequeueAfter > 0 {
		return result, nil
	}

	// Deprovisioning complete or skipped, remove base finalizer
	if controllerutil.RemoveFinalizer(subnet, osacSubnetFinalizer) {
		if err := r.Update(ctx, subnet); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// handleProvisioning manages the provisioning job lifecycle for a Subnet.
// Uses shared RunProvisioningLifecycle with config-version-based backoff on failure.
func (r *SubnetReconciler) handleProvisioning(ctx context.Context, subnet *v1alpha1.Subnet) (ctrl.Result, error) {
	if r.ProvisioningProvider == nil {
		ctrllog.FromContext(ctx).Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	return provisioning.RunProvisioningLifecycle(ctx, r.ProvisioningProvider, subnet,
		&provisioning.State{Jobs: &subnet.Status.Jobs, DesiredConfigVersion: subnet.Status.DesiredConfigVersion},
		r.getMaxJobHistory(), r.getStatusPollInterval(),
		&provisioning.PollCallbacks{
			OnFailed:  func(_ string) { subnet.Status.Phase = v1alpha1.SubnetPhaseFailed },
			OnSuccess: func(_ provisioning.ProvisionStatus) { subnet.Status.Phase = v1alpha1.SubnetPhaseReady },
		},
		func() bool {
			return provisioning.CheckAPIServerForNonTerminalProvisionJob(ctx, r.Client, client.ObjectKeyFromObject(subnet), &v1alpha1.Subnet{})
		},
	)
}

// handleDeprovisioning manages the deprovisioning job lifecycle for a Subnet.
// It triggers deprovisioning if needed and polls job status until completion.
func (r *SubnetReconciler) handleDeprovisioning(ctx context.Context, subnet *v1alpha1.Subnet) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// If no provider configured, skip deprovisioning
	if r.ProvisioningProvider == nil {
		log.Info("no provisioning provider configured, skipping deprovisioning")
		return ctrl.Result{}, nil
	}

	// Check if we already have a deprovision job
	latestDeprovisionJob := provisioning.FindLatestJobByType(subnet.Status.Jobs, v1alpha1.JobTypeDeprovision)

	// Trigger deprovisioning - provider decides internally if ready
	if latestDeprovisionJob == nil || latestDeprovisionJob.JobID == "" {
		log.Info("triggering deprovisioning", "provider", r.ProvisioningProvider.Name())

		result, err := r.ProvisioningProvider.TriggerDeprovision(ctx, subnet)
		if err != nil {
			log.Error(err, "failed to trigger deprovisioning")
			return ctrl.Result{RequeueAfter: r.getStatusPollInterval()}, nil
		}

		// Handle provider action
		switch result.Action {
		case provisioning.DeprovisionWaiting:
			log.Info("deprovisioning not ready, requeueing")
			return ctrl.Result{RequeueAfter: r.getStatusPollInterval()}, nil

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
			subnet.Status.Jobs = provisioning.AppendJob(subnet.Status.Jobs, newJob, r.getMaxJobHistory())
			log.Info("deprovisioning job triggered", "jobID", result.JobID)
			return ctrl.Result{RequeueAfter: r.getStatusPollInterval()}, nil
		}
	}

	// We have a job ID, check its status
	status, err := r.ProvisioningProvider.GetDeprovisionStatus(ctx, subnet, latestDeprovisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get deprovision job status", "jobID", latestDeprovisionJob.JobID)
		updatedJob := *latestDeprovisionJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		provisioning.UpdateJob(subnet.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.getStatusPollInterval()}, nil
	}

	// Update job status
	updatedJob := *latestDeprovisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.MessageWithDetails()
	provisioning.UpdateJob(subnet.Status.Jobs, updatedJob)

	// If job is still running, requeue
	if !status.State.IsTerminal() {
		log.Info("deprovision job still running", "jobID", latestDeprovisionJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.getStatusPollInterval()}, nil
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
		return ctrl.Result{RequeueAfter: r.getStatusPollInterval()}, nil
	} else {
		// Allow process to continue
		log.Info("deprovision job did not succeed, allowing process to continue",
			"jobID", latestDeprovisionJob.JobID,
			"state", status.State,
			"message", updatedJob.Message)
		return ctrl.Result{}, nil
	}
}

// getMaxJobHistory returns the configured max job history or the default.
func (r *SubnetReconciler) getMaxJobHistory() int {
	if r.MaxJobHistory > 0 {
		return r.MaxJobHistory
	}
	return defaultSubnetMaxJobHistory
}

// getStatusPollInterval returns the configured status poll interval or the default.
func (r *SubnetReconciler) getStatusPollInterval() time.Duration {
	if r.StatusPollInterval > 0 {
		return r.StatusPollInterval
	}
	return defaultSubnetStatusPollInterval
}
