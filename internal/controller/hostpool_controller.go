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
	"fmt"
	"hash/fnv"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/helpers"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

// NewHostPoolComponentFn is the type of a function that creates a required component
type NewHostPoolComponentFn func(context.Context, *v1alpha1.HostPool) (*appResource, error)

type hostPoolComponent struct {
	name string
	fn   NewHostPoolComponentFn
}

func (r *HostPoolReconciler) hostPoolComponents() []hostPoolComponent {
	return []hostPoolComponent{
		{"Namespace", r.newHostPoolNamespace},
	}
}

// HostPoolReconciler reconciles a HostPool object
type HostPoolReconciler struct {
	client.Client
	apiReader            client.Reader
	Scheme               *runtime.Scheme
	HostPoolNamespace    string
	ProvisioningProvider provisioning.ProvisioningProvider
	StatusPollInterval   time.Duration
	MaxJobHistory        int
}

func NewHostPoolReconciler(
	client client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	hostPoolNamespace string,
	provisioningProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
) *HostPoolReconciler {

	if hostPoolNamespace == "" {
		hostPoolNamespace = defaultHostPoolNamespace
	}

	if statusPollInterval <= 0 {
		statusPollInterval = DefaultStatusPollInterval
	}

	if maxJobHistory <= 0 {
		maxJobHistory = DefaultMaxJobHistory
	}

	return &HostPoolReconciler{
		Client:               client,
		apiReader:            apiReader,
		Scheme:               scheme,
		HostPoolNamespace:    hostPoolNamespace,
		ProvisioningProvider: provisioningProvider,
		StatusPollInterval:   statusPollInterval,
		MaxJobHistory:        maxJobHistory,
	}
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostpools/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *HostPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	instance := &v1alpha1.HostPool{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	val, exists := instance.Annotations[osacHostPoolManagementStateAnnotation]
	if exists && val == ManagementStateUnmanaged {
		log.Info("ignoring HostPool due to management-state annotation", "management-state", val)
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

	if err == nil {
		if !equality.Semantic.DeepEqual(instance.Status, *oldstatus) {
			log.Info("status requires update")
			if err := r.Status().Update(ctx, instance); err != nil {
				return res, err
			}
		}
	}

	log.Info("end reconcile")
	return res, err
}

func HostPoolNamespacePredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(
		func(obj client.Object) bool {
			return obj.GetNamespace() == namespace
		},
	)
}

// SetupWithManager sets up the controller with the Manager.
func (r *HostPoolReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	labelPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      osacHostPoolNameLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	})
	if err != nil {
		return err
	}

	localMgr := mgr.GetLocalManager()
	if localMgr == nil {
		return fmt.Errorf("local manager is nil")
	}

	return ctrl.NewControllerManagedBy(localMgr).
		For(&v1alpha1.HostPool{}, builder.WithPredicates(HostPoolNamespacePredicate(r.HostPoolNamespace))).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.mapObjectToHostPool),
			builder.WithPredicates(labelPredicate),
		).
		Complete(r)
}

// mapObjectToHostPool maps an event for a watched object to the associated
// HostPool resource.
func (r *HostPoolReconciler) mapObjectToHostPool(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	hostPoolName, exists := obj.GetLabels()[osacHostPoolNameLabel]
	if !exists {
		return nil
	}

	// Verify that the referenced HostPool exists in this controller's namespace
	// to filter out notifications for resources managed by other controller instances
	hostPool := &v1alpha1.HostPool{}
	key := client.ObjectKey{
		Name:      hostPoolName,
		Namespace: r.HostPoolNamespace,
	}
	if err := r.Get(ctx, key, hostPool); err != nil {
		log.Error(err, "unable to find referenced HostPool", "name", hostPoolName)
		return nil
	}

	log.Info("mapping object to HostPool", "hostPool", hostPoolName)
	return []reconcile.Request{
		{
			NamespacedName: key,
		},
	}
}

// handleUpdate handles creation and update operations for HostPool
func (r *HostPoolReconciler) handleUpdate(ctx context.Context, req reconcile.Request, instance *v1alpha1.HostPool) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	log.Info("handling update for HostPool", "name", instance.Name)

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(instance, hostPoolFinalizer) {
		controllerutil.AddFinalizer(instance, hostPoolFinalizer)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Set initial conditions
	if len(instance.Status.Conditions) == 0 {
		instance.SetCondition(v1alpha1.HostPoolConditionAccepted, metav1.ConditionTrue, "HostPoolAccepted", "HostPool has been accepted for processing")
		instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionTrue, "HostPoolProgressing", "HostPool is being processed")
		instance.Status.Phase = v1alpha1.HostPoolPhaseProgressing
	}

	// Create required components
	for _, comp := range r.hostPoolComponents() {
		resource, err := comp.fn(ctx, instance)
		if err != nil {
			instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionFalse, "ComponentCreationFailed", fmt.Sprintf("Failed to create %s: %v", comp.name, err))
			instance.Status.Phase = v1alpha1.HostPoolPhaseFailed
			return ctrl.Result{}, err
		}

		result, err := controllerutil.CreateOrUpdate(ctx, r.Client, resource.object, resource.mutateFn)
		if err != nil {
			instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionFalse, "ComponentUpdateFailed", fmt.Sprintf("Failed to update %s: %v", comp.name, err))
			instance.Status.Phase = v1alpha1.HostPoolPhaseFailed
			return ctrl.Result{}, err
		}

		log.Info("component operation completed", "component", comp.name, "result", result)
	}

	// Compute config version from spec and copy reconciled version from annotation
	if err := r.handleDesiredConfigVersion(instance); err != nil {
		return ctrl.Result{}, err
	}
	r.handleReconciledConfigVersion(ctx, instance)

	// Handle provisioning via provider
	provisionResult, err := r.handleProvisioning(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check provision job status before setting Ready
	latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
	if latestProvisionJob != nil {
		if latestProvisionJob.State == v1alpha1.JobStateSucceeded {
			instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionFalse, "HostPoolReady", "HostPool is ready")
			instance.SetCondition(v1alpha1.HostPoolConditionAvailable, metav1.ConditionTrue, "HostPoolAvailable", "HostPool is available")
			instance.Status.Phase = v1alpha1.HostPoolPhaseReady
			instance.Status.HostSets = instance.Spec.HostSets
		} else if !latestProvisionJob.State.IsTerminal() {
			instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionTrue, "ProvisionJobRunning", fmt.Sprintf("Provision job %s is %s", latestProvisionJob.JobID, latestProvisionJob.State))
		}
	} else if instance.Status.DesiredConfigVersion == instance.Status.ReconciledConfigVersion {
		// No provision job and config is up to date — resource is ready
		instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionFalse, "HostPoolReady", "HostPool is ready")
		instance.SetCondition(v1alpha1.HostPoolConditionAvailable, metav1.ConditionTrue, "HostPoolAvailable", "HostPool is available")
		instance.Status.Phase = v1alpha1.HostPoolPhaseReady
		instance.Status.HostSets = instance.Spec.HostSets
	}

	if provisionResult.RequeueAfter > 0 {
		return provisionResult, nil
	}

	return ctrl.Result{}, nil
}

func (r *HostPoolReconciler) findNamespace(ctx context.Context, instance *v1alpha1.HostPool) (*corev1.Namespace, error) {
	log := ctrllog.FromContext(ctx)

	var namespaceList corev1.NamespaceList
	if err := r.List(ctx, &namespaceList, labelSelectorFromHostPoolInstance(instance)); err != nil {
		log.Error(err, "failed to list namespaces")
		return nil, err
	}

	if len(namespaceList.Items) > 1 {
		return nil, fmt.Errorf("found too many (%d) matching namespaces for %s", len(namespaceList.Items), instance.GetName())
	}

	if len(namespaceList.Items) == 0 {
		return nil, nil
	}

	return &namespaceList.Items[0], nil
}

// handleDelete handles deletion operations for HostPool
func (r *HostPoolReconciler) handleDelete(ctx context.Context, req reconcile.Request, instance *v1alpha1.HostPool) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("handling delete for HostPool", "name", instance.Name)

	// Set deleting condition
	instance.SetCondition(v1alpha1.HostPoolConditionDeleting, metav1.ConditionTrue, "HostPoolDeleting", "HostPool is being deleted")
	instance.Status.Phase = v1alpha1.HostPoolPhaseDeleting

	if !controllerutil.ContainsFinalizer(instance, hostPoolFinalizer) {
		return ctrl.Result{}, nil
	}

	// Handle deprovisioning via provider
	// Waits for provision job termination and polls deprovision job if needed
	deprovisionResult, err := r.handleDeprovisioning(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	// If deprovision job is still running, requeue and wait
	if deprovisionResult.RequeueAfter > 0 {
		return deprovisionResult, nil
	}

	ns, err := r.findNamespace(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	if ns != nil {
		// Delete working namespace
		log.Info("deleting host pool namespace", "namespace", ns.GetName())
		if err := r.Client.Delete(ctx, ns); err != nil {
			log.Error(err, "failed to delete namespace", "namespace", ns.GetName(), "error", err)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Allow kubernetes to delete the hostpool
	if controllerutil.RemoveFinalizer(instance, hostPoolFinalizer) {
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	log.Info("HostPool deletion completed", "name", instance.Name)
	return ctrl.Result{}, nil
}

// handleProvisioning manages the provisioning job lifecycle for HostPool.
// Uses shouldTriggerProvision to decide action, with API server read-through guard.
func (r *HostPoolReconciler) handleProvisioning(ctx context.Context, instance *v1alpha1.HostPool) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Check for ManagementStateManual annotation
	val, exists := instance.Annotations[osacHostPoolManagementStateAnnotation]
	if exists && val == ManagementStateManual {
		log.Info("skipping provisioning due to management-state annotation", "management-state", val)
		return ctrl.Result{}, nil
	}

	action, latestProvisionJob := r.shouldTriggerProvision(ctx, instance)
	switch action {
	case provisionSkip:
		return ctrl.Result{}, nil
	case provisionRequeue:
		return ctrl.Result{Requeue: true}, nil
	case provisionTrigger:
		return r.triggerProvisionJob(ctx, instance)
	default: // provisionPoll
		return r.pollProvisionJob(ctx, log, instance, latestProvisionJob)
	}
}

func (r *HostPoolReconciler) triggerProvisionJob(ctx context.Context, instance *v1alpha1.HostPool) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("triggering provision job")

	result, err := r.ProvisioningProvider.TriggerProvision(ctx, instance)
	if err != nil {
		if rateLimitErr, ok := provisioning.AsRateLimitError(err); ok {
			log.Info("provision request rate-limited, requeueing", "retryAfter", rateLimitErr.RetryAfter)
			return ctrl.Result{RequeueAfter: rateLimitErr.RetryAfter}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to trigger provision: %w", err)
	}

	instance.Status.Jobs = helpers.AppendJob(instance.Status.Jobs, v1alpha1.JobStatus{
		JobID:         result.JobID,
		Type:          v1alpha1.JobTypeProvision,
		State:         result.InitialState,
		Message:       result.Message,
		Timestamp:     metav1.NewTime(time.Now().UTC()),
		ConfigVersion: instance.Status.DesiredConfigVersion,
	}, r.MaxJobHistory)
	latestJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
	log.Info("provision job triggered", "jobID", latestJob.JobID, "configVersion", latestJob.ConfigVersion)
	return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
}

func (r *HostPoolReconciler) pollProvisionJob(ctx context.Context, log logr.Logger,
	instance *v1alpha1.HostPool, latestProvisionJob *v1alpha1.JobStatus) (ctrl.Result, error) {

	log.Info("polling provision job status", "jobID", latestProvisionJob.JobID, "currentState", latestProvisionJob.State)
	status, err := r.ProvisioningProvider.GetProvisionStatus(ctx, instance, latestProvisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get provision status", "jobID", latestProvisionJob.JobID)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	if status.State != latestProvisionJob.State || status.Message != latestProvisionJob.Message {
		log.Info("provision job status changed", "jobID", latestProvisionJob.JobID, "oldState", latestProvisionJob.State, "newState", status.State)
		updatedJob := *latestProvisionJob
		updatedJob.State = status.State
		updatedJob.Message = status.Message
		helpers.UpdateJob(instance.Status.Jobs, updatedJob)

		if status.State == v1alpha1.JobStateFailed {
			log.Info("provision job failed", "jobID", latestProvisionJob.JobID)
			instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionFalse, "ProvisionJobFailed", fmt.Sprintf("Provision job %s failed: %s", latestProvisionJob.JobID, status.Message))
			instance.Status.Phase = v1alpha1.HostPoolPhaseFailed
		}
	}

	if !status.State.IsTerminal() {
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// shouldTriggerProvision determines the next provisioning action.
func (r *HostPoolReconciler) shouldTriggerProvision(ctx context.Context, instance *v1alpha1.HostPool) (provisionAction, *v1alpha1.JobStatus) {
	latestJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)

	if !hasJobID(latestJob) {
		// No job ever ran (or trigger failed before getting a job ID) — check if config is already reconciled
		if instance.Status.DesiredConfigVersion == instance.Status.ReconciledConfigVersion {
			return provisionSkip, latestJob
		}
	} else if !latestJob.State.IsTerminal() {
		// Job still running — poll for status
		return provisionPoll, latestJob
	} else if latestJob.ConfigVersion != "" {
		// Terminal job with ConfigVersion — skip if same config
		if latestJob.ConfigVersion == instance.Status.DesiredConfigVersion {
			return provisionSkip, latestJob
		}
	} else if instance.Status.DesiredConfigVersion == instance.Status.ReconciledConfigVersion {
		// Terminal job without ConfigVersion (pre-existing job) — use annotation-based check
		return provisionSkip, latestJob
	}

	if r.checkAPIServerForNonTerminalJob(ctx, instance) {
		return provisionRequeue, nil
	}
	return provisionTrigger, latestJob
}

func (r *HostPoolReconciler) checkAPIServerForNonTerminalJob(ctx context.Context, instance *v1alpha1.HostPool) bool {
	log := ctrllog.FromContext(ctx)
	fresh := &v1alpha1.HostPool{}
	if err := r.apiReader.Get(ctx, client.ObjectKeyFromObject(instance), fresh); err != nil {
		return false
	}
	freshJob := v1alpha1.FindLatestJobByType(fresh.Status.Jobs, v1alpha1.JobTypeProvision)
	if hasJobID(freshJob) && !freshJob.State.IsTerminal() {
		log.Info("skipping provision trigger: non-terminal job found via API server", "jobID", freshJob.JobID, "state", freshJob.State)
		return true
	}
	return false
}

func (r *HostPoolReconciler) handleDesiredConfigVersion(instance *v1alpha1.HostPool) error {
	specJSON, err := json.Marshal(instance.Spec)
	if err != nil {
		return fmt.Errorf("failed to marshal spec to JSON: %w", err)
	}
	hasher := fnv.New64a()
	if _, err := hasher.Write(specJSON); err != nil {
		return fmt.Errorf("failed to write to hash: %w", err)
	}
	instance.Status.DesiredConfigVersion = hex.EncodeToString(hasher.Sum(nil))
	return nil
}

func (r *HostPoolReconciler) handleReconciledConfigVersion(ctx context.Context, instance *v1alpha1.HostPool) {
	log := ctrllog.FromContext(ctx)
	if version, exists := instance.Annotations[osacReconciledConfigVersionAnnotation]; exists {
		instance.Status.ReconciledConfigVersion = version
		log.V(1).Info("copied reconciled config version from annotation", "version", version)
	} else {
		instance.Status.ReconciledConfigVersion = ""
	}
}

// handleDeprovisioning manages the deprovisioning job lifecycle for HostPool.
// Waits for provision job termination if needed, then triggers deprovision job.
func (r *HostPoolReconciler) handleDeprovisioning(ctx context.Context, instance *v1alpha1.HostPool) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Check for ManagementStateManual annotation
	val, exists := instance.Annotations[osacHostPoolManagementStateAnnotation]
	if exists && val == ManagementStateManual {
		log.Info("skipping deprovisioning due to management-state annotation", "management-state", val)
		return ctrl.Result{}, nil
	}

	// Check if deprovision job already exists
	latestDeprovisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeDeprovision)

	// If no deprovision job exists, trigger one
	if latestDeprovisionJob == nil {
		log.Info("triggering deprovision job")
		result, err := r.ProvisioningProvider.TriggerDeprovision(ctx, instance)
		if err != nil {
			if rateLimitErr, ok := provisioning.AsRateLimitError(err); ok {
				log.Info("deprovision request rate-limited, requeueing", "retryAfter", rateLimitErr.RetryAfter)
				return ctrl.Result{RequeueAfter: rateLimitErr.RetryAfter}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to trigger deprovision: %w", err)
		}

		// Handle different deprovision actions
		switch result.Action {
		case provisioning.DeprovisionSkipped:
			log.Info("deprovisioning skipped by provider")
			return ctrl.Result{}, nil

		case provisioning.DeprovisionWaiting:
			log.Info("waiting for provision job to terminate before deprovisioning")
			// Update provision job status if provided
			if result.ProvisionJobStatus != nil {
				latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
				if latestProvisionJob != nil {
					updatedJob := *latestProvisionJob
					updatedJob.State = result.ProvisionJobStatus.State
					updatedJob.Message = result.ProvisionJobStatus.Message
					helpers.UpdateJob(instance.Status.Jobs, updatedJob)
				}
			}
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil

		case provisioning.DeprovisionTriggered:
			log.Info("deprovision job triggered", "jobID", result.JobID)
			// Update provision job status if provided
			if result.ProvisionJobStatus != nil {
				latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
				if latestProvisionJob != nil {
					updatedJob := *latestProvisionJob
					updatedJob.State = result.ProvisionJobStatus.State
					updatedJob.Message = result.ProvisionJobStatus.Message
					helpers.UpdateJob(instance.Status.Jobs, updatedJob)
				}
			}
			// Append deprovision job
			instance.Status.Jobs = helpers.AppendJob(instance.Status.Jobs, v1alpha1.JobStatus{
				JobID:                  result.JobID,
				Type:                   v1alpha1.JobTypeDeprovision,
				State:                  v1alpha1.JobStatePending,
				Message:                "Deprovision job triggered",
				Timestamp:              metav1.NewTime(time.Now().UTC()),
				BlockDeletionOnFailure: result.BlockDeletionOnFailure,
			}, r.MaxJobHistory)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil

		default:
			return ctrl.Result{}, fmt.Errorf("unknown deprovision action: %v", result.Action)
		}
	}

	// Poll existing deprovision job status
	if !latestDeprovisionJob.State.IsTerminal() {
		log.Info("polling deprovision job status", "jobID", latestDeprovisionJob.JobID, "currentState", latestDeprovisionJob.State)
		status, err := r.ProvisioningProvider.GetDeprovisionStatus(ctx, instance, latestDeprovisionJob.JobID)
		if err != nil {
			log.Error(err, "failed to get deprovision status", "jobID", latestDeprovisionJob.JobID)
			// Don't block on polling errors, just requeue
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		// Update job status if changed
		if status.State != latestDeprovisionJob.State || status.Message != latestDeprovisionJob.Message {
			log.Info("deprovision job status changed", "jobID", latestDeprovisionJob.JobID, "oldState", latestDeprovisionJob.State, "newState", status.State)
			updatedJob := *latestDeprovisionJob
			updatedJob.State = status.State
			updatedJob.Message = status.Message
			helpers.UpdateJob(instance.Status.Jobs, updatedJob)
		}

		// Continue polling if still running
		if !status.State.IsTerminal() {
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		// Job reached terminal state
		if !status.State.IsSuccessful() && latestDeprovisionJob.BlockDeletionOnFailure {
			log.Info("deprovision job failed, blocking deletion to prevent orphaned resources",
				"jobID", latestDeprovisionJob.JobID, "state", status.State)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	// Job is terminal and successful (or not blocking), proceed with deletion
	return ctrl.Result{}, nil
}
