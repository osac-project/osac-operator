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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

const (
	// DefaultMaxJobHistory is the default number of jobs to keep in status.jobs array
	DefaultMaxJobHistory = 10

	// DefaultStatusPollInterval is the default interval for polling provider job status
	DefaultStatusPollInterval = 30 * time.Second
)

// ComputeInstanceReconciler reconciles a ComputeInstance object
type ComputeInstanceReconciler struct {
	client.Client
	Scheme                   *runtime.Scheme
	mgr                      mcmanager.Manager
	ComputeInstanceNamespace string
	TenantNamespace          string
	ProvisioningProvider     provisioning.ProvisioningProvider
	// StatusPollInterval defines how often to check provisioning job status
	StatusPollInterval time.Duration
	// MaxJobHistory defines how many jobs to keep in status.jobs array
	MaxJobHistory int
	targetCluster string
}

func NewComputeInstanceReconciler(
	mgr mcmanager.Manager,
	computeInstanceNamespace string,
	tenantNamespace string,
	provisioningProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
	targetCluster string,
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
		Client:                   mgr.GetLocalManager().GetClient(),
		Scheme:                   mgr.GetLocalManager().GetScheme(),
		mgr:                      mgr,
		ComputeInstanceNamespace: computeInstanceNamespace,
		TenantNamespace:          tenantNamespace,
		ProvisioningProvider:     provisioningProvider,
		StatusPollInterval:       statusPollInterval,
		MaxJobHistory:            maxJobHistory,
		targetCluster:            targetCluster,
	}
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=computeinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=computeinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=computeinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines;virtualmachineinstances,verbs=get;list;watch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ComputeInstanceReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	instance := &v1alpha1.ComputeInstance{}
	err := r.Get(ctx, req.NamespacedName, instance)
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
		res, err = r.handleUpdate(ctx, req.Request, instance)
	} else {
		res, err = r.handleDelete(ctx, req.Request, instance)
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

// getTargetClient returns the client for the target cluster where VirtualMachine/VMI are managed.
func (r *ComputeInstanceReconciler) getTargetClient(ctx context.Context) (client.Client, error) {
	targetCluster, err := r.mgr.GetCluster(ctx, r.targetCluster)
	if err != nil {
		return nil, err
	}
	return targetCluster.GetClient(), nil
}

// tenantAnnotationIndexField is the field path used for cache index and List MatchingFields.
var tenantAnnotationIndexField = fmt.Sprintf("metadata.annotations.%s", osacTenantAnnotation)

// SetupWithManager sets up the controller with the Manager.
func (r *ComputeInstanceReconciler) SetupWithManager(mgr mcmanager.Manager) error {

	// Index tenant annotation in order to list compute instances by tenant
	ctx := context.Background()
	localMgr := mgr.GetLocalManager()
	if err := localMgr.GetFieldIndexer().IndexField(ctx, &v1alpha1.ComputeInstance{}, tenantAnnotationIndexField,
		func(obj client.Object) []string {
			if v, ok := obj.GetAnnotations()[osacTenantAnnotation]; ok {
				return []string{v}
			}
			return nil
		}); err != nil {
		return fmt.Errorf("failed to index ComputeInstance by tenant annotation: %w", err)
	}

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

	return mcbuilder.ControllerManagedBy(mgr).
		For(&v1alpha1.ComputeInstance{},
			mcbuilder.WithPredicates(ComputeInstanceNamespacePredicate(r.ComputeInstanceNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Watches(
			&kubevirtv1.VirtualMachine{},
			mchandler.EnqueueRequestsFromMapFunc(r.mapObjectToComputeInstance),
			mcbuilder.WithPredicates(labelPredicate),
		).
		Watches(
			&kubevirtv1.VirtualMachineInstance{},
			mchandler.EnqueueRequestsFromMapFunc(r.mapObjectToComputeInstance),
			mcbuilder.WithPredicates(labelPredicate),
		).
		Watches(
			&v1alpha1.Tenant{},
			mchandler.EnqueueRequestsFromMapFunc(r.mapTenantToComputeInstances),
			mcbuilder.WithPredicates(ComputeInstanceNamespacePredicate(r.ComputeInstanceNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false),
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

func (r *ComputeInstanceReconciler) mapTenantToComputeInstances(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	// Get all compute instances matching the tenant reference
	computeInstances := &v1alpha1.ComputeInstanceList{}
	err := r.List(ctx,
		computeInstances,
		client.InNamespace(r.ComputeInstanceNamespace),
		client.MatchingFields{tenantAnnotationIndexField: obj.GetName()})
	if err != nil {
		log.Error(err, "failed to list compute instances", "annotationKey", tenantAnnotationIndexField, "tenant", obj.GetName())
		return nil
	}

	requests := []reconcile.Request{}
	for _, computeInstance := range computeInstances.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{
				Namespace: computeInstance.GetNamespace(),
				Name:      computeInstance.GetName(),
			},
		})
	}

	log.Info("mapped change notification",
		"kind", obj.GetObjectKind().GroupVersionKind().Kind,
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
		"computeinstances", computeInstances.Items,
	)
	return requests
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

	provState := r.provisionState(instance)
	action, latestProvisionJob := r.shouldTriggerProvision(ctx, instance)
	trigger := func() (ctrl.Result, error) {
		return provisioning.TriggerJob(ctx, r.ProvisioningProvider, instance, provState, r.MaxJobHistory, r.StatusPollInterval)
	}

	switch action {
	case provisioning.Skip:
		return ctrl.Result{}, nil
	case provisioning.Trigger:
		return trigger()
	case provisioning.Requeue:
		// Use RequeueAfter (not Requeue: true) to avoid hammering the API server
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	case provisioning.Backoff:
		return provisioning.HandleBackoff(ctx, provState, latestProvisionJob, trigger)
	default: // provisioning.Poll
		return provisioning.PollJob(ctx, r.ProvisioningProvider, instance, provState, latestProvisionJob, r.StatusPollInterval, &provisioning.PollCallbacks{
			OnFailed: func(_ string) {
				// Only set Failed phase if no VM exists yet (first-time provisioning failure).
				// If the VM already exists (re-provisioning failure), the phase is driven by KubeVirt
				// PrintableStatus and the failed job is visible in status.jobs.
				if instance.Status.VirtualMachineReference == nil {
					instance.Status.Phase = v1alpha1.ComputeInstancePhaseFailed
				}
			},
			OnSuccess: func(status provisioning.ProvisionStatus) {
				if status.ReconciledVersion != "" {
					instance.Status.ReconciledConfigVersion = status.ReconciledVersion
				}
			},
		})
	}
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
	latestDeprovisionJob := provisioning.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeDeprovision)

	// Trigger deprovisioning - provider decides internally if ready
	if !provisioning.HasJobID(latestDeprovisionJob) {
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
				latestProvisionJob := provisioning.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
				if latestProvisionJob != nil {
					updatedJob := *latestProvisionJob
					updatedJob.State = result.ProvisionJobStatus.State
					updatedJob.Message = result.ProvisionJobStatus.Message
					provisioning.UpdateJob(instance.Status.Jobs, updatedJob)
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
				latestProvisionJob := provisioning.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
				if latestProvisionJob != nil {
					updatedJob := *latestProvisionJob
					updatedJob.State = result.ProvisionJobStatus.State
					updatedJob.Message = result.ProvisionJobStatus.Message
					provisioning.UpdateJob(instance.Status.Jobs, updatedJob)
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
			instance.Status.Jobs = provisioning.AppendJob(instance.Status.Jobs, newJob, r.MaxJobHistory)
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
		provisioning.UpdateJob(instance.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Update job status
	updatedJob := *latestDeprovisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.Message
	if status.ErrorDetails != "" {
		updatedJob.Message = fmt.Sprintf("%s: %s", status.Message, status.ErrorDetails)
	}
	provisioning.UpdateJob(instance.Status.Jobs, updatedJob)

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

// resolveSubnetNamespace looks up the Subnet CR referenced by spec.subnetRef
// and returns the subnet namespace (which equals the Subnet CR name).
// Returns empty string if subnetRef is not set.
// Returns error if Subnet CR lookup fails.
func (r *ComputeInstanceReconciler) resolveSubnetNamespace(ctx context.Context, instance *v1alpha1.ComputeInstance) (string, error) {
	log := ctrllog.FromContext(ctx)

	if instance.Spec.SubnetRef == "" {
		// No subnet reference, no namespace to resolve
		return "", nil
	}

	// Look up Subnet CR in the same namespace as ComputeInstance
	subnet := &v1alpha1.Subnet{}
	subnetKey := types.NamespacedName{
		Name:      instance.Spec.SubnetRef,
		Namespace: instance.Namespace,
	}

	err := r.Get(ctx, subnetKey, subnet)
	if err != nil {
		return "", fmt.Errorf("failed to get Subnet CR %s: %w", instance.Spec.SubnetRef, err)
	}

	// Subnet namespace = Subnet CR name (established pattern from Phase 17)
	subnetNamespace := subnet.Name

	log.Info("Resolved subnet namespace from Subnet CR",
		"subnetRef", instance.Spec.SubnetRef,
		"subnetNamespace", subnetNamespace,
	)

	return subnetNamespace, nil
}

func (r *ComputeInstanceReconciler) handleUpdate(ctx context.Context, _ reconcile.Request, instance *v1alpha1.ComputeInstance) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	if controllerutil.AddFinalizer(instance, osacComputeInstanceFinalizer) {
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch so we have the latest resourceVersion and status; Update() may not
		// return the full status (status subresource is separate), and we need the
		// latest version to avoid 409 conflicts on later status updates.
		if err := r.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Initialize status after the finalizer update, because r.Update() overwrites
	// the in-memory status with the server response (status subresource is separate).
	r.initializeStatusConditions(instance)
	// Initialize phase to Starting for brand-new CIs (Phase is empty until first set).
	// Overridden by determinePhaseFromPrintableStatus() once a KubeVirt VM exists.
	if instance.Status.Phase == "" {
		instance.Status.Phase = v1alpha1.ComputeInstancePhaseStarting
	}

	// Get the tenant (on local cluster)
	tenant, err := r.getTenant(ctx, instance)
	if err != nil {
		tenantName := instance.GetAnnotations()[osacTenantAnnotation]
		log.Info("tenant does not exist or is being deleted, requeueing", "tenant", tenantName)
		return ctrl.Result{}, err
	}

	// If the tenant is not ready, requeue
	if tenant.Status.Phase != v1alpha1.TenantPhaseReady {
		log.Info("tenant is not ready, requeueing", "tenant", tenant.GetName())
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	targetClient, err := r.getTargetClient(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	// When a subnetRef is set, the VM is created in the subnet namespace
	// (by the AAP playbook), not in the tenant namespace.
	vmSearchNamespace := tenant.Status.Namespace
	if instance.Spec.SubnetRef != "" {
		subnetNS, err := r.resolveSubnetNamespace(ctx, instance)
		if err != nil {
			log.Error(err, "Failed to resolve subnet namespace for VM lookup")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		if subnetNS != "" {
			vmSearchNamespace = subnetNS
		}
	}

	kv, err := r.findKubeVirtVMs(ctx, targetClient, instance, vmSearchNamespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	if kv != nil {
		if err := r.handleKubeVirtVM(ctx, targetClient, instance, kv); err != nil {
			return ctrl.Result{}, err
		}
		instance.Status.Phase = determinePhaseFromPrintableStatus(ctx, kv, instance.Status.Phase)
	} else {
		// No KubeVirt VM exists yet: infrastructure is being provisioned.
		instance.Status.Phase = v1alpha1.ComputeInstancePhaseStarting
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionProvisioned, metav1.ConditionFalse, "", v1alpha1.ReasonAsExpected)
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionAvailable, metav1.ConditionFalse, "", v1alpha1.ReasonAsExpected)
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionRestartRequired, metav1.ConditionFalse, "", v1alpha1.ReasonAsExpected)
	}

	if err := r.handleDesiredConfigVersion(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	// Resolve subnet namespace if subnetRef is set
	// This must happen before provisioning.TriggerJob so the annotation is available for AAP
	if instance.Spec.SubnetRef != "" {
		subnetNamespace, err := r.resolveSubnetNamespace(ctx, instance)
		if err != nil {
			log.Error(err, "Failed to resolve subnet namespace")
			// Return error to retry - Subnet CR might not exist yet
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}

		// Store subnet namespace in annotation for AAP provider to use
		if instance.Annotations == nil {
			instance.Annotations = make(map[string]string)
		}
		instance.Annotations["osac.openshift.io/subnet-namespace"] = subnetNamespace
	}

	if err := r.handleReconciledConfigVersion(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	if instance.Status.DesiredConfigVersion == instance.Status.ReconciledConfigVersion {
		// Phase is now driven by KubeVirt PrintableStatus, set above. Only update the condition.
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionConfigurationApplied, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)

		// Update lastRestartedAt when a restart was requested and provisioning has reconciled it.
		if instance.Spec.RestartRequestedAt != nil {
			if instance.Status.LastRestartedAt == nil || instance.Spec.RestartRequestedAt.After(instance.Status.LastRestartedAt.Time) {
				log.Info("restart completed via provisioning", "restartRequestedAt", instance.Spec.RestartRequestedAt)
				instance.Status.LastRestartedAt = instance.Spec.RestartRequestedAt.DeepCopy()
				instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionRestartInProgress, metav1.ConditionFalse, "", v1alpha1.ReasonAsExpected)
			}
		}

		// If we're tracking a provision job that hasn't reached terminal state, mark it as
		// succeeded since config versions match — that's proof the provision completed.
		// This is essential for EDA where GetProvisionStatus() always returns Unknown.
		latestProvisionJob := provisioning.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
		if latestProvisionJob != nil && !latestProvisionJob.State.IsTerminal() {
			log.Info("marking provision job as succeeded (config versions match)", "jobID", latestProvisionJob.JobID, "previousState", latestProvisionJob.State)
			latestProvisionJob.State = v1alpha1.JobStateSucceeded
			latestProvisionJob.Message = "provision completed (config versions match)"
		}

		return ctrl.Result{}, nil
	}

	instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionConfigurationApplied, metav1.ConditionFalse, "Applying configuration", v1alpha1.ReasonAsExpected)

	// Set RestartInProgress condition when a restart is pending provisioning.
	if instance.Spec.RestartRequestedAt != nil {
		if instance.Status.LastRestartedAt == nil || instance.Spec.RestartRequestedAt.After(instance.Status.LastRestartedAt.Time) {
			instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionRestartInProgress, metav1.ConditionTrue,
				fmt.Sprintf("Restart initiated at %s", instance.Spec.RestartRequestedAt.UTC().Format(time.RFC3339)),
				"RestartInProgress")
		}
	}

	// Handle provisioning via provider abstraction
	return r.handleProvisioning(ctx, instance)
}

func (r *ComputeInstanceReconciler) handleDelete(ctx context.Context, _ reconcile.Request, instance *v1alpha1.ComputeInstance) (ctrl.Result, error) {
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
		if err := r.mgr.GetLocalManager().GetClient().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// initializeStatusConditions initializes the conditions that haven't already been initialized.
func (r *ComputeInstanceReconciler) initializeStatusConditions(instance *v1alpha1.ComputeInstance) {
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionConfigurationApplied,
		metav1.ConditionFalse,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionAvailable,
		metav1.ConditionFalse,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionProvisioned,
		metav1.ConditionFalse,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionRestartRequired,
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

func (r *ComputeInstanceReconciler) findKubeVirtVMs(ctx context.Context, targetClient client.Client, instance *v1alpha1.ComputeInstance, nsName string) (*kubevirtv1.VirtualMachine, error) {
	log := ctrllog.FromContext(ctx)

	var kubeVirtVMList kubevirtv1.VirtualMachineList
	if err := targetClient.List(ctx, &kubeVirtVMList, client.InNamespace(nsName), labelSelectorFromComputeInstanceInstance(instance)); err != nil {
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

func (r *ComputeInstanceReconciler) handleKubeVirtVM(ctx context.Context, targetClient client.Client, instance *v1alpha1.ComputeInstance,
	kv *kubevirtv1.VirtualMachine) error {
	log := ctrllog.FromContext(ctx)

	name := kv.GetName()
	instance.SetVirtualMachineReferenceKubeVirtVirtualMachineName(name)
	instance.SetVirtualMachineReferenceNamespace(kv.GetNamespace())

	// Provisioned reflects whether compute AND storage resources are allocated.
	// While PrintableStatus="Provisioning", KubeVirt is still creating DataVolumes
	// (storage not yet ready). For all other states the VM CR exists and both compute
	// and storage are allocated or in an operational state.
	if kv.Status.PrintableStatus == kubevirtv1.VirtualMachineStatusProvisioning {
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionProvisioned, metav1.ConditionFalse, "Provisioning infrastructure resources", v1alpha1.ReasonAsExpected)
	} else {
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionProvisioned, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)
	}

	// Available mirrors VirtualMachine.Status.Ready, synced from the VirtualMachineInstance
	// Ready condition (set by the virt-launcher pod's readiness probe).
	if kvVMHasConditionWithStatus(kv, kubevirtv1.VirtualMachineReady, corev1.ConditionTrue) {
		ipAddress := r.getFirstVMIIPAddress(ctx, targetClient, kv.GetNamespace(), name)

		log.Info("KubeVirt virtual machine (kubevirt resource) is ready", "computeinstance", instance.GetName(), "ipAddress", ipAddress)
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionAvailable, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)
		instance.SetIPAddress(ipAddress)
	} else {
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionAvailable, metav1.ConditionFalse, "", v1alpha1.ReasonAsExpected)
	}

	// RestartRequired mirrors KubeVirt's RestartRequired condition, which is set when
	// CPU/memory/device changes have been applied to the VM spec but require a reboot.
	if kvVMHasConditionWithStatus(kv, kubevirtv1.VirtualMachineRestartRequired, corev1.ConditionTrue) {
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionRestartRequired, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)
	} else {
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionRestartRequired, metav1.ConditionFalse, "", v1alpha1.ReasonAsExpected)
	}

	return nil
}

// getFirstVMIIPAddress fetches the VirtualMachineInstance and returns the first non-empty
// IP from .status.interfaces[*].ipAddress, or "" if none or on error.
func (r *ComputeInstanceReconciler) getFirstVMIIPAddress(ctx context.Context, targetClient client.Client, namespace, name string) string {
	log := ctrllog.FromContext(ctx)

	vmi := &kubevirtv1.VirtualMachineInstance{}
	if err := targetClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, vmi); err != nil {
		log.Error(err, "failed to get VirtualMachineInstance", "namespace", namespace, "name", name)
		return ""
	}
	for _, iface := range vmi.Status.Interfaces {
		if iface.IP != "" {
			return iface.IP
		}
	}

	log.Info("no IP address found for VirtualMachineInstance", "namespace", namespace, "name", name)
	return ""
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

// determinePhaseFromPrintableStatus maps a KubeVirt VirtualMachine's PrintableStatus
// to a ComputeInstancePhaseType.
//
// Transient startup states (Provisioning, WaitingForVolumeBinding) map to Starting
// because they are normal steps in the VM creation sequence, not error conditions.
//
// Paused is checked via both PrintableStatus (KubeVirt v1.6.0+) and the VirtualMachinePaused
// condition (older versions where PrintableStatus stayed "Running" when paused).
//
// Migrating and WaitingForReceiver map to Running because the source VM remains accessible
// throughout live migration. OSAC does not trigger live migration but it can be infra-initiated.
//
// Unknown preserves currentPhase: the hypervisor host is temporarily unreachable and the VM
// may still be healthy. The phase clears automatically when the host recovers.
//
// An empty PrintableStatus ("") occurs when our watch fires before KubeVirt's controller
// has processed the new VM CR. Like Unknown, it preserves the current phase to avoid a
// transient Failed.
//
// All remaining values (Terminating, CrashLoopBackOff, ErrorUnschedulable, ErrImagePull,
// ImagePullBackOff, ErrorPvcNotFound, DataVolumeError) map to Failed.
func determinePhaseFromPrintableStatus(ctx context.Context, kv *kubevirtv1.VirtualMachine, currentPhase v1alpha1.ComputeInstancePhaseType) v1alpha1.ComputeInstancePhaseType {
	log := ctrllog.FromContext(ctx)
	log.V(1).Info("mapping KubeVirt PrintableStatus to ComputeInstance phase",
		"printableStatus", kv.Status.PrintableStatus,
		"currentPhase", currentPhase)

	switch kv.Status.PrintableStatus {
	case kubevirtv1.VirtualMachineStatusProvisioning,
		kubevirtv1.VirtualMachineStatusWaitingForVolumeBinding,
		kubevirtv1.VirtualMachineStatusStarting:
		return v1alpha1.ComputeInstancePhaseStarting
	case kubevirtv1.VirtualMachineStatusPaused:
		return v1alpha1.ComputeInstancePhasePaused
	case kubevirtv1.VirtualMachineStatusRunning:
		// Defensive fallback for older KubeVirt versions where PrintableStatus stayed
		// "Running" when the VM was paused.
		if kvVMHasConditionWithStatus(kv, kubevirtv1.VirtualMachinePaused, corev1.ConditionTrue) {
			return v1alpha1.ComputeInstancePhasePaused
		}
		return v1alpha1.ComputeInstancePhaseRunning
	case kubevirtv1.VirtualMachineStatusMigrating,
		kubevirtv1.VirtualMachineStatusWaitingForReceiver:
		return v1alpha1.ComputeInstancePhaseRunning
	case kubevirtv1.VirtualMachineStatusStopping:
		return v1alpha1.ComputeInstancePhaseStopping
	case kubevirtv1.VirtualMachineStatusStopped:
		return v1alpha1.ComputeInstancePhaseStopped
	case kubevirtv1.VirtualMachineStatusUnknown:
		// Host is temporarily unreachable. Preserve the last known phase rather than
		// asserting Failed. Clears automatically when the host recovers.
		log.Info("KubeVirt PrintableStatus is Unknown, preserving current phase",
			"currentPhase", currentPhase)
		return currentPhase
	case "":
		// PrintableStatus not yet set by KubeVirt — race condition at VM creation.
		// Our watch fires before KubeVirt's controller processes the new VM CR.
		// Preserve the current phase (always Starting at this point) to avoid a
		// transient Failed.
		log.Info("KubeVirt PrintableStatus not yet set, preserving current phase",
			"currentPhase", currentPhase)
		return currentPhase
	default:
		// Covers: Terminating, CrashLoopBackOff, ErrorUnschedulable, ErrImagePull,
		// ImagePullBackOff, ErrorPvcNotFound, DataVolumeError.
		// If a new KubeVirt PrintableStatus is introduced and falls here, update this switch.
		log.Info("unhandled KubeVirt PrintableStatus, defaulting to Failed",
			"printableStatus", kv.Status.PrintableStatus)
		return v1alpha1.ComputeInstancePhaseFailed
	}
}

// shouldTriggerProvision determines the next provisioning action.
// Returns provisioning.Poll with the in-progress job when one is already running.
// Returns provisioning.Skip when config versions match (no change needed).
// Returns provisioning.Requeue when the API server has a non-terminal job that the cache missed (stale cache).
// Returns provisioning.Trigger when provisioning is needed and no in-flight job exists.
func (r *ComputeInstanceReconciler) provisionState(instance *v1alpha1.ComputeInstance) *provisioning.State {
	return &provisioning.State{
		Jobs:                    &instance.Status.Jobs,
		DesiredConfigVersion:    instance.Status.DesiredConfigVersion,
		ReconciledConfigVersion: instance.Status.ReconciledConfigVersion,
	}
}

func (r *ComputeInstanceReconciler) shouldTriggerProvision(ctx context.Context, instance *v1alpha1.ComputeInstance) (provisioning.Action, *v1alpha1.JobStatus) {
	return provisioning.EvaluateAction(r.provisionState(instance), func() bool {
		return provisioning.CheckAPIServerForNonTerminalProvisionJob(ctx, r.mgr.GetLocalManager().GetAPIReader(), client.ObjectKeyFromObject(instance), &v1alpha1.ComputeInstance{})
	})
}

func (r *ComputeInstanceReconciler) handleDesiredConfigVersion(ctx context.Context, instance *v1alpha1.ComputeInstance) error {
	version, err := provisioning.ComputeDesiredConfigVersion(instance.Spec)
	if err != nil {
		return err
	}
	instance.Status.DesiredConfigVersion = version
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
