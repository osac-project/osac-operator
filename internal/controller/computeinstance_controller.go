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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

const (
	// DefaultMaxJobHistory is the default number of jobs to keep in status.jobs array
	DefaultMaxJobHistory = 10
)

// ComputeInstanceReconciler reconciles a ComputeInstance object
type ComputeInstanceReconciler struct {
	client.Client
	Scheme                   *runtime.Scheme
	ComputeInstanceNamespace string
	ProvisioningProvider     provisioning.ProvisioningProvider
	// StatusPollInterval defines how often to check provisioning job status
	StatusPollInterval time.Duration
	// MaxJobHistory defines how many jobs to keep in status.jobs array
	MaxJobHistory int
}

func NewComputeInstanceReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	computeInstanceNamespace string,
	provisioningProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
) *ComputeInstanceReconciler {

	if computeInstanceNamespace == "" {
		computeInstanceNamespace = defaultComputeInstanceNamespace
	}

	if statusPollInterval == 0 {
		statusPollInterval = 30 * time.Second
	}

	if maxJobHistory <= 0 {
		maxJobHistory = DefaultMaxJobHistory
	}

	return &ComputeInstanceReconciler{
		Client:                   client,
		Scheme:                   scheme,
		ComputeInstanceNamespace: computeInstanceNamespace,
		ProvisioningProvider:     provisioningProvider,
		StatusPollInterval:       statusPollInterval,
		MaxJobHistory:            maxJobHistory,
	}
}

// findJobByID finds a job by its ID in the jobs array.
// Returns a pointer to the job if found, nil otherwise.
// The returned pointer can be used to update the job in place.
func findJobByID(jobs []v1alpha1.JobStatus, jobID string) *v1alpha1.JobStatus {
	for i := range jobs {
		if jobs[i].JobID == jobID {
			return &jobs[i]
		}
	}
	return nil
}

// appendJob adds a new job to the jobs array and trims to maxHistory.
// Jobs are appended in chronological order (oldest first, newest last).
func (r *ComputeInstanceReconciler) appendJob(jobs []v1alpha1.JobStatus, newJob v1alpha1.JobStatus) []v1alpha1.JobStatus {
	jobs = append(jobs, newJob)
	if len(jobs) > r.MaxJobHistory {
		// Keep only the last MaxJobHistory jobs
		jobs = jobs[len(jobs)-r.MaxJobHistory:]
	}
	return jobs
}

// updateJob updates an existing job by ID with new values.
// Returns true if the job was found and updated, false otherwise.
func updateJob(jobs []v1alpha1.JobStatus, updatedJob v1alpha1.JobStatus) bool {
	job := findJobByID(jobs, updatedJob.JobID)
	if job == nil {
		return false
	}
	*job = updatedJob
	return true
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=computeinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=computeinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=computeinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=computeinstances,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ComputeInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	instance := &v1alpha1.ComputeInstance{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	val, exists := instance.Annotations[osacComputeInstanceManagementStateAnnotation]
	if exists && val == ManagementStateUnmanaged {
		log.Info("ignoring ComputeInstance due to management-state annotation", "management-state", val)
		return ctrl.Result{}, nil
	}

	log.Info("start reconcile")

	oldstatus := instance.Status.DeepCopy()

	var res ctrl.Result
	if instance.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.handleUpdate(ctx, req, instance)
	} else {
		res, err = r.handleDelete(ctx, req, instance)
	}

	if !equality.Semantic.DeepEqual(instance.Status, *oldstatus) {
		log.Info("status requires update")
		if err := r.updateStatusWithRetry(ctx, req.NamespacedName, instance.Status); err != nil {
			return res, err
		}
	}

	log.Info("end reconcile")
	return res, err
}

// updateStatusWithRetry updates the instance status with retry on conflict.
// This prevents duplicate job triggers when status updates fail due to optimistic concurrency conflicts.
func (r *ComputeInstanceReconciler) updateStatusWithRetry(ctx context.Context, key client.ObjectKey, newStatus v1alpha1.ComputeInstanceStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch latest version to get current resourceVersion
		latest := &v1alpha1.ComputeInstance{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		// Copy status updates to the latest version
		latest.Status = newStatus
		// Attempt to update with fresh resourceVersion
		return r.Status().Update(ctx, latest)
	})
}

func ComputeInstanceNamespacePredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(
		func(obj client.Object) bool {
			return obj.GetNamespace() == namespace
		},
	)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ComputeInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	labelPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      osacComputeInstanceNameLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ComputeInstance{}, builder.WithPredicates(ComputeInstanceNamespacePredicate(r.ComputeInstanceNamespace))).
		Watches(
			&kubevirtv1.VirtualMachine{},
			handler.EnqueueRequestsFromMapFunc(r.mapObjectToComputeInstance),
			builder.WithPredicates(labelPredicate),
		).
		Watches(
			&v1alpha1.Tenant{},
			handler.EnqueueRequestForOwner(
				mgr.GetScheme(),
				mgr.GetRESTMapper(),
				&v1alpha1.ComputeInstance{},
			),
			builder.WithPredicates(ComputeInstanceNamespacePredicate(r.ComputeInstanceNamespace)),
		).
		Complete(r)
}

// mapObjectToComputeInstance maps an event for a watched object to the associated
// ComputeInstance resource.
func (r *ComputeInstanceReconciler) mapObjectToComputeInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	computeInstanceName, exists := obj.GetLabels()[osacComputeInstanceNameLabel]
	if !exists {
		return nil
	}

	// Verify that the referenced ComputeInstance exists in this controller's namespace
	// to filter out notifications for resources managed by other controller instances
	computeInstance := &v1alpha1.ComputeInstance{}
	key := client.ObjectKey{
		Name:      computeInstanceName,
		Namespace: r.ComputeInstanceNamespace,
	}
	if err := r.Get(ctx, key, computeInstance); err != nil {
		// ComputeInstance doesn't exist in our namespace, ignore this notification
		log.V(2).Info("ignoring notification for resource not managed by this controller instance",
			"kind", obj.GetObjectKind().GroupVersionKind().Kind,
			"namespace", obj.GetNamespace(),
			"name", obj.GetName(),
			"computeinstance", computeInstanceName,
			"controller_namespace", r.ComputeInstanceNamespace,
		)
		return nil
	}

	log.Info("mapped change notification",
		"kind", obj.GetObjectKind().GroupVersionKind().Kind,
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
		"computeinstance", computeInstanceName,
	)

	return []reconcile.Request{
		{
			NamespacedName: key,
		},
	}
}

// handleProvisioning manages the provisioning job lifecycle for a ComputeInstance.
// It triggers provisioning if needed and polls job status until completion.
// Status updates are handled by the main reconcile loop.
func (r *ComputeInstanceReconciler) handleProvisioning(ctx context.Context, instance *v1alpha1.ComputeInstance) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Check for ManagementStateManual annotation
	val, exists := instance.Annotations[osacComputeInstanceManagementStateAnnotation]
	if exists && val == ManagementStateManual {
		log.Info("skipping provisioning due to management-state annotation", "management-state", val)
		return ctrl.Result{}, nil
	}

	// If no provider configured, skip provisioning
	if r.ProvisioningProvider == nil {
		log.Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	// Check if we need to trigger a (new) provision job
	latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)

	if r.needsProvisionJob(instance, latestProvisionJob) {
		log.Info("triggering provisioning", "provider", r.ProvisioningProvider.Name())
		result, err := r.ProvisioningProvider.TriggerProvision(ctx, instance)
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
			instance.Status.Jobs = r.appendJob(instance.Status.Jobs, newJob)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		newJob := v1alpha1.JobStatus{
			JobID:                  result.JobID,
			Type:                   v1alpha1.JobTypeProvision,
			Timestamp:              metav1.NewTime(time.Now().UTC()),
			State:                  result.InitialState,
			Message:                result.Message,
			BlockDeletionOnFailure: false, // Provision failures don't block deletion
		}
		instance.Status.Jobs = r.appendJob(instance.Status.Jobs, newJob)
		log.Info("provisioning job triggered", "jobID", result.JobID)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// We have a job ID, check its status
	status, err := r.ProvisioningProvider.GetProvisionStatus(ctx, instance, latestProvisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get provision job status", "jobID", latestProvisionJob.JobID)
		updatedJob := *latestProvisionJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		updateJob(instance.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Update job status
	updatedJob := *latestProvisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.Message
	if status.ErrorDetails != "" {
		updatedJob.Message = fmt.Sprintf("%s: %s", status.Message, status.ErrorDetails)
	}
	updateJob(instance.Status.Jobs, updatedJob)

	// If job is still running, requeue
	if !status.State.IsTerminal() {
		log.Info("provision job still running", "jobID", latestProvisionJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Job is complete
	if status.State.IsSuccessful() {
		log.Info("provision job succeeded", "jobID", latestProvisionJob.JobID)
		// Update reconciled version if provided
		if status.ReconciledVersion != "" {
			instance.Status.ReconciledConfigVersion = status.ReconciledVersion
		}
		return ctrl.Result{}, nil
	}

	// Job failed
	log.Info("provision job failed", "jobID", latestProvisionJob.JobID, "message", updatedJob.Message)
	instance.Status.Phase = v1alpha1.ComputeInstancePhaseFailed
	return ctrl.Result{}, nil
}

// handleDeprovisioning manages the deprovisioning job lifecycle for a ComputeInstance.
// It triggers deprovisioning if needed and polls job status until completion.
// For EDA provider: This is called only when AAP finalizer exists (set by playbook).
// For AAP Direct provider: This is always called to handle cancellation and deprovision.
// Note: Finalizer management is handled by handleDelete(), not here.
func (r *ComputeInstanceReconciler) handleDeprovisioning(ctx context.Context, instance *v1alpha1.ComputeInstance) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Check for ManagementStateManual annotation
	val, exists := instance.Annotations[osacComputeInstanceManagementStateAnnotation]
	if exists && val == ManagementStateManual {
		log.Info("skipping deprovisioning due to management-state annotation", "management-state", val)
		// For EDA: AAP playbook handles finalizer removal
		// For AAP Direct: handleDelete() removes base finalizer
		return ctrl.Result{}, nil
	}

	// Check if we already have a deprovision job
	latestDeprovisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeDeprovision)

	// Trigger deprovisioning - provider decides internally if ready
	if latestDeprovisionJob == nil || latestDeprovisionJob.JobID == "" {
		log.Info("triggering deprovisioning", "provider", r.ProvisioningProvider.Name())

		result, err := r.ProvisioningProvider.TriggerDeprovision(ctx, instance)
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
			// Provider not ready yet (e.g., canceling provision job)
			// Update provision job status if provider returned one (e.g., cancellation in progress)
			if result.ProvisionJobStatus != nil {
				latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
				if latestProvisionJob != nil {
					updatedJob := *latestProvisionJob
					updatedJob.State = result.ProvisionJobStatus.State
					updatedJob.Message = result.ProvisionJobStatus.Message
					updateJob(instance.Status.Jobs, updatedJob)
					log.Info("updated provision job status while waiting for deprovision", "state", result.ProvisionJobStatus.State, "message", result.ProvisionJobStatus.Message)
				}
			}
			log.Info("deprovisioning not ready, requeueing")
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil

		case provisioning.DeprovisionSkipped:
			// Provider determined deprovisioning not needed (e.g., EDA without finalizer)
			log.Info("provider skipped deprovisioning")
			return ctrl.Result{}, nil

		case provisioning.DeprovisionTriggered:
			// Deprovision started successfully
			// Update provision job status if provider returned one (job was terminal before deprovision)
			if result.ProvisionJobStatus != nil {
				latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
				if latestProvisionJob != nil {
					updatedJob := *latestProvisionJob
					updatedJob.State = result.ProvisionJobStatus.State
					updatedJob.Message = result.ProvisionJobStatus.Message
					updateJob(instance.Status.Jobs, updatedJob)
					log.Info("updated provision job status before starting deprovision", "state", result.ProvisionJobStatus.State, "message", result.ProvisionJobStatus.Message)
				}
			}
			newJob := v1alpha1.JobStatus{
				JobID:                  result.JobID,
				Type:                   v1alpha1.JobTypeDeprovision,
				Timestamp:              metav1.NewTime(time.Now().UTC()),
				State:                  v1alpha1.JobStatePending,
				Message:                "Deprovisioning job triggered",
				BlockDeletionOnFailure: result.BlockDeletionOnFailure,
			}
			instance.Status.Jobs = r.appendJob(instance.Status.Jobs, newJob)
			log.Info("deprovisioning job triggered", "jobID", result.JobID)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	// We have a job ID, check its status
	status, err := r.ProvisioningProvider.GetDeprovisionStatus(ctx, instance, latestDeprovisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get deprovision job status", "jobID", latestDeprovisionJob.JobID)
		updatedJob := *latestDeprovisionJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		updateJob(instance.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Update job status
	updatedJob := *latestDeprovisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.Message
	if status.ErrorDetails != "" {
		updatedJob.Message = fmt.Sprintf("%s: %s", status.Message, status.ErrorDetails)
	}
	updateJob(instance.Status.Jobs, updatedJob)

	// If job is still running, requeue
	if !status.State.IsTerminal() {
		log.Info("deprovision job still running", "jobID", latestDeprovisionJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Job reached terminal state (Succeeded, Failed, or Canceled)
	if status.State.IsSuccessful() {
		log.Info("deprovision job succeeded", "jobID", latestDeprovisionJob.JobID)
		// For EDA: AAP playbook removes AAP finalizer on success
		// For AAP Direct: handleDelete() removes base finalizer
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
		// Allow process to continue (webhook handles cleanup)
		log.Info("deprovision job did not succeed, allowing process to continue",
			"jobID", latestDeprovisionJob.JobID,
			"state", status.State,
			"message", updatedJob.Message)
		return ctrl.Result{}, nil
	}
}

func (r *ComputeInstanceReconciler) handleUpdate(ctx context.Context, _ ctrl.Request, instance *v1alpha1.ComputeInstance) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	if controllerutil.AddFinalizer(instance, osacComputeInstanceFinalizer) {
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Initialize status after the finalizer update, because r.Update() overwrites
	// the in-memory status with the server response (status subresource is separate).
	r.initializeStatusConditions(instance)
	// Only set Starting phase for first-time provisioning.
	// If the instance was already successfully provisioned (has a ReconciledConfigVersion),
	// keep the current phase to avoid regressing from Running to Starting during re-provisioning.
	if instance.Status.ReconciledConfigVersion == "" {
		instance.Status.Phase = v1alpha1.ComputeInstancePhaseStarting
	}

	// Get the tenant
	tenant, err := r.getTenant(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	// If the tenant is being deleted, wait for deletion to complete before creating a new one.
	// This can happen when a previous ComputeInstance owned the tenant via ownerReference
	// and its deletion triggered garbage collection on the shared tenant.
	if tenant != nil && tenant.DeletionTimestamp != nil {
		log.Info("tenant is being deleted, waiting for deletion to complete", "tenant", tenant.GetName())
		instance.SetTenantReferenceName("")
		instance.SetTenantReferenceNamespace("")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// If the tenant doesn't exist, create it and requeue
	if tenant == nil {
		if err := r.createOrUpdateTenant(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// If the tenant is not ready, requeue
	if tenant.Status.Phase != v1alpha1.TenantPhaseReady {
		log.Info("tenant is not ready, requeueing", "tenant", tenant.GetName())
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionAccepted, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)

	kv, err := r.findKubeVirtVMs(ctx, instance, tenant.Status.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	if kv != nil {
		if err := r.handleKubeVirtVM(ctx, instance, kv); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Handle restart request
	if result, err := r.handleRestartRequest(ctx, instance); err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	if err := r.handleDesiredConfigVersion(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.handleReconciledConfigVersion(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	if instance.Status.DesiredConfigVersion == instance.Status.ReconciledConfigVersion {
		instance.Status.Phase = v1alpha1.ComputeInstancePhaseRunning
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionProgressing, metav1.ConditionFalse, "", v1alpha1.ReasonAsExpected)

		// If we're tracking a provision job that hasn't reached terminal state, continue polling
		// This ensures job status fields are accurate and reflect the final job outcome
		latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
		if r.ProvisioningProvider != nil && latestProvisionJob != nil {
			if !latestProvisionJob.State.IsTerminal() {
				log.Info("VM ready but provision job not terminal, continuing to poll", "jobID", latestProvisionJob.JobID, "state", latestProvisionJob.State)
				return r.handleProvisioning(ctx, instance)
			}
		}

		return ctrl.Result{}, nil
	}

	// Only regress to Starting for first-time provisioning; keep Running during re-provisioning
	if instance.Status.ReconciledConfigVersion == "" {
		instance.Status.Phase = v1alpha1.ComputeInstancePhaseStarting
	}
	instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionProgressing, metav1.ConditionTrue, "Applying configuration", v1alpha1.ReasonAsExpected)

	// Handle provisioning via provider abstraction
	return r.handleProvisioning(ctx, instance)
}

func (r *ComputeInstanceReconciler) handleDelete(ctx context.Context, _ ctrl.Request, instance *v1alpha1.ComputeInstance) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("deleting compute instance")

	instance.Status.Phase = v1alpha1.ComputeInstancePhaseDeleting

	// Base finalizer has already been removed, cleanup complete
	if !controllerutil.ContainsFinalizer(instance, osacComputeInstanceFinalizer) {
		return ctrl.Result{}, nil
	}

	// Handle deprovisioning - provider decides internally if needed
	log.Info("handling deletion")
	result, err := r.handleDeprovisioning(ctx, instance)
	if err != nil {
		return result, err
	}

	// If we need to requeue (jobs still running or provider needs time), do so
	if result.RequeueAfter > 0 {
		return result, nil
	}

	// Deprovisioning complete or skipped, remove base finalizer
	if controllerutil.RemoveFinalizer(instance, osacComputeInstanceFinalizer) {
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// initializeStatusConditions initializes the conditions that haven't already been initialized.
func (r *ComputeInstanceReconciler) initializeStatusConditions(instance *v1alpha1.ComputeInstance) {
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionAccepted,
		metav1.ConditionTrue,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionDeleting,
		metav1.ConditionFalse,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionProgressing,
		metav1.ConditionTrue,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionAvailable,
		metav1.ConditionFalse,
		v1alpha1.ReasonInitialized,
	)
}

// initializeStatusCondition initializes a condition, but only if it is not already initialized.
func (r *ComputeInstanceReconciler) initializeStatusCondition(instance *v1alpha1.ComputeInstance,
	conditionType v1alpha1.ComputeInstanceConditionType, status metav1.ConditionStatus, reason string) {
	if instance.Status.Conditions == nil {
		instance.Status.Conditions = []metav1.Condition{}
	}
	condition := instance.GetStatusCondition(conditionType)
	if condition != nil {
		return
	}
	instance.SetStatusCondition(conditionType, status, "", reason)
}

func (r *ComputeInstanceReconciler) findKubeVirtVMs(ctx context.Context, instance *v1alpha1.ComputeInstance, nsName string) (*kubevirtv1.VirtualMachine, error) {
	log := ctrllog.FromContext(ctx)

	var kubeVirtVMList kubevirtv1.VirtualMachineList
	if err := r.List(ctx, &kubeVirtVMList, client.InNamespace(nsName), labelSelectorFromComputeInstanceInstance(instance)); err != nil {
		log.Error(err, "failed to list KubeVirt VMs")
		return nil, err
	}

	if len(kubeVirtVMList.Items) > 1 {
		return nil, fmt.Errorf("found too many (%d) matching KubeVirt VMs for %s", len(kubeVirtVMList.Items), instance.GetName())
	}

	if len(kubeVirtVMList.Items) == 0 {
		return nil, nil
	}

	return &kubeVirtVMList.Items[0], nil
}

func (r *ComputeInstanceReconciler) handleKubeVirtVM(ctx context.Context, instance *v1alpha1.ComputeInstance,
	kv *kubevirtv1.VirtualMachine) error {
	log := ctrllog.FromContext(ctx)

	name := kv.GetName()
	instance.SetVirtualMachineReferenceKubeVirtVirtualMachineName(name)
	instance.SetVirtualMachineReferenceNamespace(kv.GetNamespace())
	instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionAccepted, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)

	if kvVMHasConditionWithStatus(kv, kubevirtv1.VirtualMachineReady, corev1.ConditionTrue) {
		log.Info("KubeVirt virtual machine (kubevirt resource) is ready", "computeinstance", instance.GetName())
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionAvailable, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)
	}

	return nil
}

func kvVMGetCondition(vm *kubevirtv1.VirtualMachine, cond kubevirtv1.VirtualMachineConditionType) *kubevirtv1.VirtualMachineCondition {
	if vm == nil {
		return nil
	}
	for _, c := range vm.Status.Conditions {
		if c.Type == cond {
			return &c
		}
	}
	return nil
}

func kvVMHasConditionWithStatus(vm *kubevirtv1.VirtualMachine, cond kubevirtv1.VirtualMachineConditionType, status corev1.ConditionStatus) bool {
	c := kvVMGetCondition(vm, cond)
	return c != nil && c.Status == status
}

// needsProvisionJob returns true when we should trigger a new provision job.
// This is the case when no job exists yet, or when the spec has changed since the last successful provision.
func (r *ComputeInstanceReconciler) needsProvisionJob(instance *v1alpha1.ComputeInstance, latestJob *v1alpha1.JobStatus) bool {
	if latestJob == nil || latestJob.JobID == "" {
		return true
	}
	if !latestJob.State.IsTerminal() {
		return false
	}
	return instance.Status.DesiredConfigVersion != instance.Status.ReconciledConfigVersion
}

// handleDesiredConfigVersion computes a version (hash) of the spec (using FNV-1a) and stores it as hexadecimal in status.DesiredConfigVersion.
// The hashing is idempotent - the same spec will always produce the same version.
func (r *ComputeInstanceReconciler) handleDesiredConfigVersion(ctx context.Context, instance *v1alpha1.ComputeInstance) error {
	// Hash the spec using FNV-1a for idempotent hashing
	specJSON, err := json.Marshal(instance.Spec)
	if err != nil {
		return fmt.Errorf("failed to marshal spec to JSON: %w", err)
	}

	hasher := fnv.New64a()
	if _, err := hasher.Write(specJSON); err != nil {
		return fmt.Errorf("failed to write to hash: %w", err)
	}

	hashBytes := hasher.Sum(nil)
	desiredConfigVersion := hex.EncodeToString(hashBytes)
	instance.Status.DesiredConfigVersion = desiredConfigVersion

	return nil
}

// handleReconciledConfigVersion copies the annotation osacAAPReconciledConfigVersionAnnotation to status.ReconciledConfigVersion.
// If the annotation doesn't exist, it clears status.ReconciledConfigVersion.
func (r *ComputeInstanceReconciler) handleReconciledConfigVersion(ctx context.Context, instance *v1alpha1.ComputeInstance) error {
	log := ctrllog.FromContext(ctx)

	// Copy the reconciled config version from annotation if it exists
	if version, exists := instance.Annotations[osacAAPReconciledConfigVersionAnnotation]; exists {
		instance.Status.ReconciledConfigVersion = version
		log.V(1).Info("copied reconciled config version from annotation", "version", version)
	} else {
		// Clear the reconciled config version if annotation doesn't exist
		instance.Status.ReconciledConfigVersion = ""
	}

	return nil
}
