/*
Copyright 2026.

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
	// osacPublicIPPoolFinalizer is the finalizer for PublicIPPool resources
	osacPublicIPPoolFinalizer = "osac.openshift.io/publicippool-finalizer"

	// defaultPublicIPPoolStatusPollInterval is the default interval for polling job status
	defaultPublicIPPoolStatusPollInterval = 30 * time.Second

	// defaultPublicIPPoolMaxJobHistory is the default maximum number of jobs to keep in history
	defaultPublicIPPoolMaxJobHistory = 10
)

// PublicIPPoolReconciler reconciles a PublicIPPool object
type PublicIPPoolReconciler struct {
	client.Client
	APIReader            client.Reader
	Scheme               *runtime.Scheme
	NetworkingNamespace  string
	ProvisioningProvider provisioning.ProvisioningProvider
	StatusPollInterval   time.Duration
	MaxJobHistory        int
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicippools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicippools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicippools/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *PublicIPPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	pool := &v1alpha1.PublicIPPool{}
	err := r.Get(ctx, req.NamespacedName, pool)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	val, exists := pool.Annotations[osacManagementStateAnnotation]
	if exists && val == ManagementStateUnmanaged {
		log.Info("ignoring PublicIPPool due to management-state annotation", "management-state", val)
		return ctrl.Result{}, nil
	}

	log.Info("start reconcile")

	oldstatus := pool.Status.DeepCopy()

	var res ctrl.Result
	if pool.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.handleUpdate(ctx, pool)
	} else {
		res, err = r.handleDelete(ctx, pool)
	}

	if !equality.Semantic.DeepEqual(pool.Status, *oldstatus) {
		log.Info("status requires update")
		if updateErr := r.Status().Update(ctx, pool); updateErr != nil {
			log.Error(updateErr, "failed to update status")
			return res, updateErr
		}
	}

	log.Info("end reconcile")
	return res, err
}

func (r *PublicIPPoolReconciler) handleUpdate(ctx context.Context, pool *v1alpha1.PublicIPPool) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Add finalizer if not present
	if controllerutil.AddFinalizer(pool, osacPublicIPPoolFinalizer) {
		if err := r.Update(ctx, pool); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch so we have the latest resourceVersion and status
		if err := r.Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set initial phase to Progressing
	if pool.Status.Phase == "" {
		pool.Status.Phase = v1alpha1.PublicIPPoolPhaseProgressing
	}

	// Read implementation strategy from spec, defaulting to metallb-l2
	implementationStrategy := pool.Spec.ImplementationStrategy
	if implementationStrategy == "" {
		implementationStrategy = "metallb-l2"
	}

	// Add implementation-strategy annotation if not present or different.
	// This allows AAP playbooks to select the appropriate role without doing lookups.
	if pool.Annotations == nil {
		pool.Annotations = make(map[string]string)
	}
	if pool.Annotations[osacImplementationStrategyAnnotation] != implementationStrategy {
		pool.Annotations[osacImplementationStrategyAnnotation] = implementationStrategy
		log.Info("setting implementation-strategy annotation", "strategy", implementationStrategy)
		if err := r.Update(ctx, pool); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Compute desired config version from spec and implementation strategy
	desiredVersion, err := provisioning.ComputeDesiredConfigVersion(struct {
		Spec                   v1alpha1.PublicIPPoolSpec
		ImplementationStrategy string
	}{pool.Spec, implementationStrategy})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to compute desired config version: %w", err)
	}
	pool.Status.DesiredConfigVersion = desiredVersion

	// Set ConfigurationApplied condition to True after processing the spec
	v1alpha1.SetPublicIPPoolStatusCondition(pool, metav1.Condition{
		Type:               string(v1alpha1.PublicIPPoolConditionConfigurationApplied),
		Status:             metav1.ConditionTrue,
		Reason:             "ConfigurationApplied",
		Message:            "Controller has processed the current spec",
		LastTransitionTime: metav1.Now(),
	})

	// Set phase to Progressing on first provision (empty phase) or when spec changed
	// after a previous success. Don't override Failed during backoff.
	if pool.Status.Phase == "" || (pool.Status.Phase == v1alpha1.PublicIPPoolPhaseReady &&
		!provisioning.IsConfigApplied(&pool.Status.Jobs, pool.Status.DesiredConfigVersion)) {
		pool.Status.Phase = v1alpha1.PublicIPPoolPhaseProgressing
	}

	// Handle provisioning
	return r.handleProvisioning(ctx, pool)
}

func (r *PublicIPPoolReconciler) handleDelete(ctx context.Context, pool *v1alpha1.PublicIPPool) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("deleting public IP pool")

	pool.Status.Phase = v1alpha1.PublicIPPoolPhaseDeleting

	// Finalizer already removed, cleanup complete
	if !controllerutil.ContainsFinalizer(pool, osacPublicIPPoolFinalizer) {
		return ctrl.Result{}, nil
	}

	// Handle deprovisioning
	result, err := r.handleDeprovisioning(ctx, pool)
	if err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(pool, osacPublicIPPoolFinalizer)
	if err := r.Update(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handleProvisioning manages the provisioning job lifecycle for a PublicIPPool.
// Uses shared RunProvisioningLifecycle with config-version-based backoff on failure.
func (r *PublicIPPoolReconciler) handleProvisioning(ctx context.Context, pool *v1alpha1.PublicIPPool) (ctrl.Result, error) {
	if r.ProvisioningProvider == nil {
		ctrllog.FromContext(ctx).Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	return provisioning.RunProvisioningLifecycle(ctx, r.ProvisioningProvider, pool,
		&provisioning.State{Jobs: &pool.Status.Jobs, DesiredConfigVersion: pool.Status.DesiredConfigVersion},
		r.getMaxJobHistory(), r.getStatusPollInterval(),
		&provisioning.PollCallbacks{
			OnFailed:  func(_ string) { pool.Status.Phase = v1alpha1.PublicIPPoolPhaseFailed },
			OnSuccess: func(_ provisioning.ProvisionStatus) { pool.Status.Phase = v1alpha1.PublicIPPoolPhaseReady },
		},
		func() bool {
			return provisioning.CheckAPIServerForNonTerminalProvisionJob(
				ctx, r.APIReader, client.ObjectKeyFromObject(pool), &v1alpha1.PublicIPPool{})
		},
	)
}

// handleDeprovisioning manages the deprovisioning job lifecycle for a PublicIPPool.
// It triggers deprovisioning if needed and polls job status until completion.
func (r *PublicIPPoolReconciler) handleDeprovisioning(ctx context.Context, pool *v1alpha1.PublicIPPool) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// If no provider configured, skip deprovisioning
	if r.ProvisioningProvider == nil {
		log.Info("no provisioning provider configured, skipping deprovisioning")
		return ctrl.Result{}, nil
	}

	// Check if we already have a deprovision job
	latestDeprovisionJob := provisioning.FindLatestJobByType(pool.Status.Jobs, v1alpha1.JobTypeDeprovision)

	// Trigger deprovisioning
	if latestDeprovisionJob == nil || latestDeprovisionJob.JobID == "" {
		log.Info("triggering deprovisioning", "provider", r.ProvisioningProvider.Name())

		result, err := r.ProvisioningProvider.TriggerDeprovision(ctx, pool)
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
			pool.Status.Jobs = provisioning.AppendJob(pool.Status.Jobs, newJob, r.getMaxJobHistory())
			log.Info("deprovisioning job triggered", "jobID", result.JobID)
			return ctrl.Result{RequeueAfter: r.getStatusPollInterval()}, nil
		}
	}

	// We have a job ID, check its status
	status, err := r.ProvisioningProvider.GetDeprovisionStatus(ctx, pool, latestDeprovisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get deprovision job status", "jobID", latestDeprovisionJob.JobID)
		updatedJob := *latestDeprovisionJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		provisioning.UpdateJob(pool.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.getStatusPollInterval()}, nil
	}

	// Update job status
	updatedJob := *latestDeprovisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.MessageWithDetails()
	provisioning.UpdateJob(pool.Status.Jobs, updatedJob)

	// If job is still running, requeue
	if !status.State.IsTerminal() {
		log.Info("deprovision job still running", "jobID", latestDeprovisionJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.getStatusPollInterval()}, nil
	}

	// Job reached terminal state
	if status.State.IsSuccessful() {
		log.Info("deprovision job succeeded", "jobID", latestDeprovisionJob.JobID)
		return ctrl.Result{}, nil
	}

	// Job failed or was canceled, check policy
	if latestDeprovisionJob.BlockDeletionOnFailure {
		log.Info("deprovision job failed, blocking deletion to prevent orphaned resources",
			"jobID", latestDeprovisionJob.JobID,
			"state", status.State,
			"message", updatedJob.Message)
		return ctrl.Result{RequeueAfter: r.getStatusPollInterval()}, nil
	}

	log.Info("deprovision job did not succeed, allowing process to continue",
		"jobID", latestDeprovisionJob.JobID,
		"state", status.State,
		"message", updatedJob.Message)
	return ctrl.Result{}, nil
}

// getMaxJobHistory returns the configured max job history or the default.
func (r *PublicIPPoolReconciler) getMaxJobHistory() int {
	if r.MaxJobHistory > 0 {
		return r.MaxJobHistory
	}
	return defaultPublicIPPoolMaxJobHistory
}

// getStatusPollInterval returns the configured status poll interval or the default.
func (r *PublicIPPoolReconciler) getStatusPollInterval() time.Duration {
	if r.StatusPollInterval > 0 {
		return r.StatusPollInterval
	}
	return defaultPublicIPPoolStatusPollInterval
}

// SetupWithManager sets up the controller with the Manager.
func (r *PublicIPPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PublicIPPool{}).
		WithEventFilter(NetworkingNamespacePredicate(r.NetworkingNamespace)).
		Complete(r)
}
