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
	"regexp"
	"sort"
	"strings"
	"time"

	ovnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mc "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/pkg/provisioning"
)

const tenantFinalizer = "osac.openshift.io/tenant"

// TenantReconciler reconciles a Tenant object
type TenantReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	Recorder             events.EventRecorder
	tenantNamespace      string
	mgr                  mcmanager.Manager
	targetCluster        mc.ClusterName
	ProvisioningProvider provisioning.ProvisioningProvider
	StatusPollInterval   time.Duration
	MaxJobHistory        int
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=userdefinednetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

func NewTenantReconciler(
	mgr mcmanager.Manager,
	tenantNamespace string,
	targetCluster mc.ClusterName,
	provisioningProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
) *TenantReconciler {
	if mgr == nil {
		panic("mgr must not be nil")
	}

	if statusPollInterval == 0 {
		statusPollInterval = 30 * time.Second
	}

	if maxJobHistory <= 0 {
		maxJobHistory = provisioning.DefaultMaxJobHistory
	}

	return &TenantReconciler{
		Client:               mgr.GetLocalManager().GetClient(),
		Scheme:               mgr.GetLocalManager().GetScheme(),
		Recorder:             mgr.GetLocalManager().GetEventRecorder(tenantControllerName),
		tenantNamespace:      tenantNamespace,
		mgr:                  mgr,
		targetCluster:        targetCluster,
		ProvisioningProvider: provisioningProvider,
		StatusPollInterval:   statusPollInterval,
		MaxJobHistory:        maxJobHistory,
	}
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *TenantReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	instance := &v1alpha1.Tenant{}
	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("start reconcile")

	oldstatus := instance.Status.DeepCopy()

	var res ctrl.Result
	if instance.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.handleUpdate(ctx, req.Request, instance)
	} else {
		res, err = r.handleDelete(ctx, instance)
	}

	if !equality.Semantic.DeepEqual(instance.Status, *oldstatus) {
		log.Info("status requires update", "old", *oldstatus, "new", instance.Status)
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	log.Info("end reconcile")
	return res, err
}

// handleUpdate handles creation and update operations for Tenant
func (r *TenantReconciler) handleUpdate(ctx context.Context, req reconcile.Request, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	log.Info("handling update for Tenant", "name", instance.GetName())

	// Add finalizer for storage deprovisioning on delete
	if !controllerutil.ContainsFinalizer(instance, tenantFinalizer) {
		controllerutil.AddFinalizer(instance, tenantFinalizer)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Reset all status fields to their Progressing defaults. They are only set to
	// meaningful values if ALL prerequisites are satisfied and the phase advances to
	// Ready. Any early return below leaves the status in a clean Progressing state.
	instance.Status.Phase = v1alpha1.TenantPhaseProgressing
	instance.Status.Namespace = ""
	instance.Status.StorageClass = ""
	instance.Status.StorageClasses = nil

	// Get target cluster client where namespace, StorageClass, and UDN are reconciled
	targetClient, err := getTargetClient(ctx, r.mgr, r.targetCluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Prerequisite 1: namespace must exist on the target cluster
	var namespace corev1.Namespace
	if err = targetClient.Get(ctx, client.ObjectKey{Name: instance.GetName()}, &namespace); err != nil {
		instance.SetStatusCondition(v1alpha1.TenantConditionNamespaceReady,
			metav1.ConditionFalse,
			v1alpha1.TenantReasonNotFound,
			fmt.Sprintf("Namespace %q not found on target cluster", instance.GetName()))
		instance.SetStatusCondition(v1alpha1.TenantConditionStorageClassReady,
			metav1.ConditionFalse,
			v1alpha1.TenantReasonNotFound,
			"Cannot evaluate StorageClass: namespace not ready")
		return ctrl.Result{}, err
	}

	instance.SetStatusCondition(v1alpha1.TenantConditionNamespaceReady,
		metav1.ConditionTrue,
		v1alpha1.TenantReasonFound,
		fmt.Sprintf("Namespace %q found on target cluster", instance.GetName()))

	// Prerequisite 2: resolve all storage tiers
	result, err := r.getTenantStorageClasses(ctx, targetClient, instance.GetName())
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, msg := range result.duplicateMessages {
		r.Recorder.Eventf(instance, nil, corev1.EventTypeWarning, eventReasonDuplicateStorageClass, eventActionDetectDuplicate, "%s", msg)
	}

	if len(result.resolved) == 0 {
		reason := v1alpha1.TenantReasonNotFound
		if len(result.duplicateMessages) > 0 {
			reason = v1alpha1.TenantReasonMultipleFound
		}
		condMsg := result.conditionMessage()
		instance.SetStatusCondition(v1alpha1.TenantConditionStorageClassReady,
			metav1.ConditionFalse,
			reason,
			condMsg)
		r.Recorder.Eventf(instance, nil, corev1.EventTypeWarning, eventReasonStorageClassNotReady, "StorageClassResolution", "%s", condMsg)
		// No StorageClass found — trigger AAP to provision one if provider is configured
		if r.ProvisioningProvider != nil && reason == v1alpha1.TenantReasonNotFound {
			return r.handleStorageProvisioning(ctx, instance)
		}
		return ctrl.Result{}, nil
	}

	instance.SetStatusCondition(v1alpha1.TenantConditionStorageClassReady,
		metav1.ConditionTrue,
		v1alpha1.TenantReasonFound,
		result.conditionMessage())

	instance.Status.Namespace = namespace.GetName()
	instance.Status.StorageClass = result.resolved[0].Name
	instance.Status.StorageClasses = result.resolved

	// SC found, but if a provision job is still non-terminal, poll it before
	// declaring Ready — AAP is the source of truth for job status.
	latestProvJob := provisioning.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
	if latestProvJob != nil && !latestProvJob.State.IsTerminal() && r.ProvisioningProvider != nil {
		return r.pollProvisionJob(ctx, instance, latestProvJob)
	}

	instance.Status.Phase = v1alpha1.TenantPhaseReady
	return ctrl.Result{}, nil
}

// tierResolutionResult holds the outcome of resolving all storage tiers for a tenant.
type tierResolutionResult struct {
	resolved          []v1alpha1.ResolvedStorageClass
	resolvedMessages  []string
	errorMessages     []string
	duplicateMessages []string
}

// handleStorageProvisioning manages the AAP job lifecycle for provisioning
// tenant storage. Follows the same pattern as ComputeInstance provisioning.
func (r *TenantReconciler) handleStorageProvisioning(ctx context.Context, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	latestJob := provisioning.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)

	// Don't retry if the latest job failed — wait for an external trigger
	// (Tenant CR update, SC watch event) to avoid infinite retry loops.
	if latestJob != nil && latestJob.State == v1alpha1.JobStateFailed {
		log.Info("latest provision job failed, waiting for external trigger to retry",
			"message", latestJob.Message)
		instance.Status.Phase = v1alpha1.TenantPhaseFailed
		return ctrl.Result{}, nil
	}

	if r.needsProvisionJob(latestJob) {
		log.Info("triggering storage provisioning", "provider", r.ProvisioningProvider.Name())
		result, err := r.ProvisioningProvider.TriggerProvision(ctx, instance)
		if err != nil {
			var rateLimitErr *provisioning.RateLimitError
			if errors.As(err, &rateLimitErr) {
				log.Info("provisioning request rate-limited, will retry", "retryAfter", rateLimitErr.RetryAfter)
				return ctrl.Result{RequeueAfter: rateLimitErr.RetryAfter}, nil
			}

			log.Error(err, "failed to trigger storage provisioning")
			newJob := v1alpha1.JobStatus{
				JobID:     "",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(time.Now().UTC()),
				State:     v1alpha1.JobStateFailed,
				Message:   fmt.Sprintf("Failed to trigger storage provisioning: %v", err),
			}
			instance.Status.Jobs = provisioning.AppendJob(instance.Status.Jobs, newJob, r.MaxJobHistory)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		newJob := v1alpha1.JobStatus{
			JobID:     result.JobID,
			Type:      v1alpha1.JobTypeProvision,
			Timestamp: metav1.NewTime(time.Now().UTC()),
			State:     result.InitialState,
			Message:   result.Message,
		}
		instance.Status.Jobs = provisioning.AppendJob(instance.Status.Jobs, newJob, r.MaxJobHistory)
		log.Info("storage provisioning job triggered", "jobID", result.JobID)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	return r.pollProvisionJob(ctx, instance, latestJob)
}

// pollProvisionJob polls an existing AAP provision job and updates status.
// Called from both handleStorageProvisioning (no SC yet) and handleUpdate
// (SC found but job still non-terminal). On success, requeues so the caller
// can re-evaluate with the terminal job state.
func (r *TenantReconciler) pollProvisionJob(ctx context.Context, instance *v1alpha1.Tenant, latestJob *v1alpha1.JobStatus) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	status, err := r.ProvisioningProvider.GetProvisionStatus(ctx, instance, latestJob.JobID)
	if err != nil {
		log.Error(err, "failed to get provision job status", "jobID", latestJob.JobID)
		updatedJob := *latestJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		provisioning.UpdateJob(instance.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	updatedJob := *latestJob
	updatedJob.State = status.State
	updatedJob.Message = status.MessageWithDetails()
	provisioning.UpdateJob(instance.Status.Jobs, updatedJob)

	if !status.State.IsTerminal() {
		log.Info("storage provisioning job still running", "jobID", latestJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	if status.State.IsSuccessful() {
		log.Info("storage provisioning job succeeded, requeueing to confirm StorageClass", "jobID", latestJob.JobID)
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("storage provisioning job failed", "jobID", latestJob.JobID, "message", updatedJob.Message)
	instance.Status.Phase = v1alpha1.TenantPhaseFailed
	return ctrl.Result{}, nil
}

// needsProvisionJob returns true when a new provision job should be triggered.
// This is called only when no StorageClass exists, so:
// - nil/empty job → first provision
// - successful job → SC was deleted after provision, need to re-provision
// - failed job → do NOT retry (avoid infinite loops, wait for external trigger)
// - running job → do NOT create a duplicate
func (r *TenantReconciler) needsProvisionJob(latestJob *v1alpha1.JobStatus) bool {
	if latestJob == nil || latestJob.JobID == "" {
		return true
	}
	if latestJob.State == v1alpha1.JobStateFailed {
		return false
	}
	return latestJob.State.IsSuccessful()
}

// handleDelete handles deletion operations for Tenant
func (r *TenantReconciler) handleDelete(ctx context.Context, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("handling delete for Tenant", "name", instance.Name)

	if !controllerutil.ContainsFinalizer(instance, tenantFinalizer) {
		return ctrl.Result{}, nil
	}

	instance.Status.Phase = v1alpha1.TenantPhaseDeleting

	// Deprovision storage before removing the finalizer.
	deprovJob := provisioning.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeDeprovision)
	deprovJobRunning := deprovJob != nil && deprovJob.JobID != "" && !deprovJob.State.IsTerminal()
	deprovJobFailedBlocking := deprovJob != nil &&
		deprovJob.State.IsTerminal() &&
		!deprovJob.State.IsSuccessful() &&
		deprovJob.BlockDeletionOnFailure
	scExists, err := r.tenantStorageClassExists(ctx, instance)
	if err != nil {
		log.Error(err, "failed to check StorageClass existence, requeueing")
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}
	if scExists || deprovJobRunning || deprovJobFailedBlocking {
		result, err := r.handleStorageDeprovisioning(ctx, instance)
		if err != nil {
			return result, err
		}
		if result.RequeueAfter > 0 {
			return result, nil
		}
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(instance, tenantFinalizer)
	if err := r.Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("tenant finalizer removed, deletion will proceed")
	return ctrl.Result{}, nil
}

// handleStorageDeprovisioning triggers an AAP job to remove tenant storage and
// polls until it completes. Returns a result with RequeueAfter > 0 if the job
// is still running.
func (r *TenantReconciler) handleStorageDeprovisioning(ctx context.Context, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	if r.ProvisioningProvider == nil {
		log.Info("no provisioning provider configured, cannot clean up storage — waiting for manual StorageClass removal")
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	latestJob := provisioning.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeDeprovision)

	if latestJob == nil || latestJob.JobID == "" {
		log.Info("triggering storage deprovisioning", "provider", r.ProvisioningProvider.Name())
		result, err := r.ProvisioningProvider.TriggerDeprovision(ctx, instance)
		if err != nil {
			var rateLimitErr *provisioning.RateLimitError
			if errors.As(err, &rateLimitErr) {
				log.Info("deprovisioning request rate-limited, will retry", "retryAfter", rateLimitErr.RetryAfter)
				return ctrl.Result{RequeueAfter: rateLimitErr.RetryAfter}, nil
			}
			log.Error(err, "failed to trigger storage deprovisioning")
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		switch result.Action {
		case provisioning.DeprovisionTriggered:
			newJob := v1alpha1.JobStatus{
				JobID:                  result.JobID,
				Type:                   v1alpha1.JobTypeDeprovision,
				Timestamp:              metav1.NewTime(time.Now().UTC()),
				State:                  v1alpha1.JobStatePending,
				Message:                "Storage deprovisioning job triggered",
				BlockDeletionOnFailure: result.BlockDeletionOnFailure,
			}
			instance.Status.Jobs = provisioning.AppendJob(instance.Status.Jobs, newJob, r.MaxJobHistory)
			log.Info("storage deprovisioning job triggered", "jobID", result.JobID)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil

		case provisioning.DeprovisionSkipped:
			log.Info("provider skipped storage deprovisioning")
			return ctrl.Result{}, nil

		case provisioning.DeprovisionWaiting:
			log.Info("storage deprovisioning not ready, requeueing")
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	// Poll existing job status
	status, err := r.ProvisioningProvider.GetDeprovisionStatus(ctx, instance, latestJob.JobID)
	if err != nil {
		log.Error(err, "failed to get deprovision job status", "jobID", latestJob.JobID)
		updatedJob := *latestJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		provisioning.UpdateJob(instance.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	updatedJob := *latestJob
	updatedJob.State = status.State
	updatedJob.Message = status.MessageWithDetails()
	provisioning.UpdateJob(instance.Status.Jobs, updatedJob)

	if !status.State.IsTerminal() {
		log.Info("storage deprovisioning job still running", "jobID", latestJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	if status.State.IsSuccessful() {
		log.Info("storage deprovisioning job succeeded", "jobID", latestJob.JobID)
		return ctrl.Result{}, nil
	}

	if latestJob.BlockDeletionOnFailure {
		log.Info("storage deprovisioning job failed, blocking deletion",
			"jobID", latestJob.JobID, "message", updatedJob.Message)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	log.Info("storage deprovisioning job failed, continuing with deletion",
		"jobID", latestJob.JobID, "message", updatedJob.Message)
	return ctrl.Result{}, nil
}

// tenantStorageClassExists checks whether at least one StorageClass labeled for
// this tenant already exists on the cluster.
func (r *TenantReconciler) tenantStorageClassExists(ctx context.Context, instance *v1alpha1.Tenant) (bool, error) {
	var scList storagev1.StorageClassList
	if err := r.List(ctx, &scList, client.MatchingLabels{
		osacTenantAnnotation: instance.GetName(),
	}); err != nil {
		return false, err
	}
	return len(scList.Items) > 0, nil
}

func (r *tierResolutionResult) conditionMessage() string {
	parts := make([]string, 0, len(r.resolvedMessages)+len(r.errorMessages))
	parts = append(parts, r.resolvedMessages...)
	parts = append(parts, r.errorMessages...)
	return strings.Join(parts, "; ")
}

// tierLabelPattern matches values that conform to the ResolvedStorageClass.Tier
// CRD validation: lowercase alphanumeric with dashes, dots, underscores, 1-63 chars.
var tierLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)

// joinStorageClassNames returns StorageClass metadata names as a comma-separated string for
// messages and the same values as a slice for structured logging.
func joinStorageClassNames(items []storagev1.StorageClass) (joined string, names []string) {
	names = make([]string, len(items))
	for i := range items {
		names[i] = items[i].GetName()
	}
	return strings.Join(names, ", "), names
}

// groupByTier groups StorageClasses by their osac.openshift.io/storage-tier label value.
// StorageClasses missing the label or with values that don't match the CRD tier
// pattern (after lowercase normalization) are ignored.
func groupByTier(scList []storagev1.StorageClass) map[string][]storagev1.StorageClass {
	groups := make(map[string][]storagev1.StorageClass)
	for _, sc := range scList {
		raw, exists := sc.GetLabels()[osacStorageTierLabel]
		if !exists || raw == "" {
			continue
		}
		tier := strings.ToLower(raw)
		if !tierLabelPattern.MatchString(tier) {
			continue
		}
		groups[tier] = append(groups[tier], sc)
	}
	return groups
}

// getTenantStorageClasses resolves all storage tiers for a tenant. For each
// distinct storage-tier value found across tenant-specific and shared Default
// StorageClasses, it applies a two-step fallback per tier: tenant-specific first,
// then shared Default. StorageClasses missing the storage-tier label are ignored.
func (r *TenantReconciler) getTenantStorageClasses(ctx context.Context, targetClient client.Client, tenantName string) (tierResolutionResult, error) {
	log := ctrllog.FromContext(ctx)

	tenantSCList := &storagev1.StorageClassList{}
	if err := targetClient.List(ctx, tenantSCList, client.MatchingLabels{osacTenantAnnotation: tenantName}); err != nil {
		return tierResolutionResult{}, err
	}

	defaultSCList := &storagev1.StorageClassList{}
	if err := targetClient.List(ctx, defaultSCList, client.MatchingLabels{osacTenantAnnotation: defaultStorageClassSentinel}); err != nil {
		return tierResolutionResult{}, err
	}

	tenantByTier := groupByTier(tenantSCList.Items)
	defaultByTier := groupByTier(defaultSCList.Items)

	allTiers := make(map[string]struct{})
	for t := range tenantByTier {
		allTiers[t] = struct{}{}
	}
	for t := range defaultByTier {
		allTiers[t] = struct{}{}
	}

	sortedTiers := make([]string, 0, len(allTiers))
	for t := range allTiers {
		sortedTiers = append(sortedTiers, t)
	}
	sort.Strings(sortedTiers)

	var result tierResolutionResult

	for _, tier := range sortedTiers {
		tenantSCs := tenantByTier[tier]
		defaultSCs := defaultByTier[tier]

		switch len(tenantSCs) {
		case 1:
			scName := tenantSCs[0].GetName()
			result.resolved = append(result.resolved, v1alpha1.ResolvedStorageClass{
				Name: scName,
				Tier: tier,
			})
			msg := fmt.Sprintf("tier %q: StorageClass %q (tenant-specific)", tier, scName)
			result.resolvedMessages = append(result.resolvedMessages, msg)
			continue
		case 0:
			// Fall through to Default resolution below.
		default:
			joined, names := joinStorageClassNames(tenantSCs)
			msg := fmt.Sprintf("tier %q: multiple tenant StorageClasses [%s]", tier, joined)
			log.Info(msg, "tenant", tenantName, "tier", tier, "storageClasses", names)
			result.errorMessages = append(result.errorMessages, msg)
			result.duplicateMessages = append(result.duplicateMessages, msg)
			continue
		}

		switch len(defaultSCs) {
		case 1:
			scName := defaultSCs[0].GetName()
			result.resolved = append(result.resolved, v1alpha1.ResolvedStorageClass{
				Name: scName,
				Tier: tier,
			})
			msg := fmt.Sprintf("tier %q: StorageClass %q (shared Default)", tier, scName)
			result.resolvedMessages = append(result.resolvedMessages, msg)
		case 0:
			// Tier not available — not an error at the Tenant level.
		default:
			joined, names := joinStorageClassNames(defaultSCs)
			msg := fmt.Sprintf("tier %q: multiple shared Default StorageClasses [%s]", tier, joined)
			log.Info(msg, "tenant", tenantName, "tier", tier, "storageClasses", names)
			result.errorMessages = append(result.errorMessages, msg)
			result.duplicateMessages = append(result.duplicateMessages, msg)
		}
	}

	if len(result.resolved) == 0 && len(result.errorMessages) == 0 {
		result.errorMessages = append(result.errorMessages,
			fmt.Sprintf("no StorageClasses with %s label found for tenant %q", osacStorageTierLabel, tenantName))
	}

	return result, nil
}

// mapStorageClassToTenant maps a StorageClass event to Tenant reconcile requests.
// For tenant-specific SCs, it maps to the single named Tenant.
// For shared Default SCs (osac.openshift.io/tenant=Default), it maps to ALL
// Tenants since any tenant without a dedicated SC could be affected.
func (r *TenantReconciler) mapStorageClassToTenant(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	tenantName, exists := obj.GetLabels()[osacTenantAnnotation]
	if !exists || tenantName == "" {
		return nil
	}

	if tenantName == defaultStorageClassSentinel {
		log.Info("shared Default StorageClass changed, reconciling all tenants",
			"storageClass", obj.GetName())
		return r.allTenantReconcileRequests(ctx)
	}

	tenant := &v1alpha1.Tenant{}
	err := r.Get(ctx, client.ObjectKey{Namespace: r.tenantNamespace, Name: tenantName}, tenant)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "unable to get Tenant for StorageClass",
				"storageClass", obj.GetName(), "tenant", tenantName)
		}
		return nil
	}

	log.Info("mapping StorageClass to Tenant", "storageClass", obj.GetName(), "tenant", tenantName)
	return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(tenant)}}
}

// allTenantReconcileRequests returns reconcile requests for every Tenant in the
// tenant namespace. Used when a shared Default StorageClass is created or
// deleted, since any tenant without a dedicated SC could be affected.
func (r *TenantReconciler) allTenantReconcileRequests(ctx context.Context) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	tenantList := &v1alpha1.TenantList{}
	if err := r.List(ctx, tenantList, client.InNamespace(r.tenantNamespace)); err != nil {
		log.Error(err, "unable to list Tenants for Default SC reconciliation")
		return nil
	}

	requests := make([]reconcile.Request, 0, len(tenantList.Items))
	for i := range tenantList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&tenantList.Items[i]),
		})
	}
	log.Info("enqueuing all tenants for reconciliation", "count", len(requests))
	return requests
}

// mapObjectToTenant maps an event for a watched UDN to the associated Tenant resource.
func (r *TenantReconciler) mapObjectToTenant(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	tenantName, exists := obj.GetLabels()[osacTenantRefLabel]
	if !exists {
		return nil
	}

	// Get tenant
	tenant := &v1alpha1.Tenant{}
	err := r.Get(ctx, client.ObjectKey{Namespace: r.tenantNamespace, Name: tenantName}, tenant)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "unable to get Tenant from object", "kind", obj.GetObjectKind(), "name", obj.GetName(), "namespace", obj.GetNamespace(), "tenant", tenantName)
		}
		return nil
	}

	log.Info("mapping object to Tenant", "kind", obj.GetObjectKind(), "name", obj.GetName(), "namespace", obj.GetNamespace(), "tenant", tenantName)
	return []reconcile.Request{
		{
			NamespacedName: client.ObjectKeyFromObject(tenant),
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *TenantReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	// Predicate to filter resources with tenant label
	tenantLabelPredicate, err := predicate.LabelSelectorPredicate(tenantLabelSelector(r.tenantNamespace))
	if err != nil {
		return err
	}

	// Tenant CR is reconciled from local cluster only
	return mcbuilder.ControllerManagedBy(mgr).
		For(&v1alpha1.Tenant{},
			mcbuilder.WithPredicates(tenantNamespacePredicate(r.tenantNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Named("tenant").
		Watches(
			&corev1.Namespace{},
			mchandler.EnqueueRequestsFromMapFunc(r.mapObjectToTenant),
			mcbuilder.WithPredicates(tenantLabelPredicate),
		).
		Watches(
			&ovnv1.UserDefinedNetwork{},
			mchandler.EnqueueRequestsFromMapFunc(r.mapObjectToTenant),
			mcbuilder.WithPredicates(tenantLabelPredicate),
		).
		Watches(
			&storagev1.StorageClass{},
			mchandler.EnqueueRequestsFromMapFunc(r.mapStorageClassToTenant),
			mcbuilder.WithPredicates(storageClassTenantPredicate()),
		).
		Complete(r)
}

// storageClassTenantPredicate returns a predicate that passes only StorageClasses
// carrying the osac.openshift.io/tenant label (any value).
func storageClassTenantPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		_, exists := obj.GetLabels()[osacTenantAnnotation]
		return exists
	})
}

// tenantNamespacePredicate filters resources based on the tenant namespace.
func tenantNamespacePredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(
		func(obj client.Object) bool {
			return obj.GetNamespace() == namespace
		},
	)
}

// tenantLabelSelector returns a label selector for resources associated with a tenant in the given project (namespace where tenant object lives).
func tenantLabelSelector(project string) metav1.LabelSelector {
	return metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      osacTenantRefLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
			{
				Key:      osacProjectRefLabel,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{project},
			},
		},
	}
}
