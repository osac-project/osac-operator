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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mc "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

const (
	osacPublicIPFinalizer = "osac.openshift.io/publicip-finalizer"
)

// PublicIPReconciler reconciles PublicIP CRs created by the fulfillment-service.
//
// Each PublicIP belongs to a parent PublicIPPool (referenced by UUID in spec.pool).
// The controller adds a finalizer, inherits the implementation strategy from the
// parent pool, then delegates to the shared provisioning lifecycle to trigger AAP
// jobs for provisioning and deprovisioning.
//
// Phase transitions: "" -> Progressing -> Ready/Failed; on delete: Deleting.
type PublicIPReconciler struct {
	client.Client
	APIReader                  client.Reader
	Scheme                     *runtime.Scheme
	Recorder                   events.EventRecorder
	mgr                        mcmanager.Manager
	NetworkingNamespace        string
	ComputeInstanceNamespace   string
	ProvisioningProvider       provisioning.ProvisioningProvider
	PublicIPAttachmentProvider provisioning.ProvisioningProvider
	StatusPollInterval         time.Duration
	MaxJobHistory              int
	targetCluster              mc.ClusterName
}

// NewPublicIPReconciler creates a new reconciler for PublicIP resources.
func NewPublicIPReconciler(
	mgr mcmanager.Manager,
	networkingNamespace string,
	computeInstanceNamespace string,
	provisioningProvider provisioning.ProvisioningProvider,
	publicIPAttachmentProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
	targetCluster mc.ClusterName,
) *PublicIPReconciler {
	if mgr == nil {
		panic("mgr must not be nil")
	}
	if statusPollInterval <= 0 {
		statusPollInterval = provisioning.DefaultStatusPollInterval
	}
	if maxJobHistory <= 0 {
		maxJobHistory = provisioning.DefaultMaxJobHistory
	}
	if computeInstanceNamespace == "" {
		computeInstanceNamespace = defaultComputeInstanceNamespace
	}
	return &PublicIPReconciler{
		Client:                     mgr.GetLocalManager().GetClient(),
		APIReader:                  mgr.GetLocalManager().GetAPIReader(),
		Scheme:                     mgr.GetLocalManager().GetScheme(),
		Recorder:                   mgr.GetLocalManager().GetEventRecorder(publicipControllerName),
		mgr:                        mgr,
		NetworkingNamespace:        networkingNamespace,
		ComputeInstanceNamespace:   computeInstanceNamespace,
		ProvisioningProvider:       provisioningProvider,
		PublicIPAttachmentProvider: publicIPAttachmentProvider,
		StatusPollInterval:         statusPollInterval,
		MaxJobHistory:              maxJobHistory,
		targetCluster:              targetCluster,
	}
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicips,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicips/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicips/finalizers,verbs=update
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicippools,verbs=get;list;watch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=computeinstances,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=osac.openshift.io,resources=computeinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get

// Reconcile handles create/update/delete for a PublicIP CR.
// On create/update it ensures a finalizer, resolves the parent pool, and runs provisioning.
// On delete it triggers deprovisioning and removes the finalizer when complete.
func (r *PublicIPReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	publicIP := &v1alpha1.PublicIP{}
	err := r.Get(ctx, req.NamespacedName, publicIP)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip unmanaged resources, but still allow deletion to proceed
	val, exists := publicIP.Annotations[osacManagementStateAnnotation]
	if publicIP.ObjectMeta.DeletionTimestamp.IsZero() && exists && val == ManagementStateUnmanaged {
		log.Info("ignoring PublicIP due to management-state annotation", "management-state", val)
		return ctrl.Result{}, nil
	}

	log.Info("start reconcile", "pool", publicIP.Spec.Pool, "phase", publicIP.Status.Phase)

	oldstatus := publicIP.Status.DeepCopy()

	var res ctrl.Result
	if publicIP.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.handleUpdate(ctx, publicIP)
	} else {
		res, err = r.handleDelete(ctx, publicIP)
	}

	if !equality.Semantic.DeepEqual(publicIP.Status, *oldstatus) {
		log.Info("status requires update", "phase", publicIP.Status.Phase)
		if updateErr := r.Status().Update(ctx, publicIP); updateErr != nil {
			log.Error(updateErr, "failed to update status")
			return res, updateErr
		}
	}

	log.Info("end reconcile", "phase", publicIP.Status.Phase)
	return res, err
}

// handleUpdate processes a non-deleted PublicIP: adds finalizer, resolves the parent
// PublicIPPool, inherits the implementation strategy, and runs provisioning.
func (r *PublicIPReconciler) handleUpdate(ctx context.Context, publicIP *v1alpha1.PublicIP) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Add finalizer if not present
	if controllerutil.AddFinalizer(publicIP, osacPublicIPFinalizer) {
		log.Info("adding finalizer")
		if err := r.Update(ctx, publicIP); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch to get the latest resourceVersion after the metadata update
		if err := r.Get(ctx, client.ObjectKeyFromObject(publicIP), publicIP); err != nil {
			return ctrl.Result{}, err
		}
	}

	if publicIP.Status.Phase == "" {
		publicIP.Status.Phase = v1alpha1.PublicIPPhaseProgressing
		publicIP.Status.State = v1alpha1.PublicIPStatePending
	}

	// Resolve the parent PublicIPPool by the fulfillment-service UUID stored in spec.pool.
	// The fulfillment-service creates pool CRs with a UUID label; spec.pool contains that
	// UUID, not the K8s object name.
	poolList := &v1alpha1.PublicIPPoolList{}
	err := r.List(ctx, poolList,
		client.InNamespace(publicIP.Namespace),
		client.MatchingLabels{osacPublicIPPoolIDLabel: publicIP.Spec.Pool},
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(poolList.Items) == 0 {
		log.Info("parent PublicIPPool not found, requeueing", "poolUUID", publicIP.Spec.Pool)
		return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
	}
	pool := &poolList.Items[0]
	log.Info("resolved parent PublicIPPool", "poolName", pool.Name, "poolUUID", publicIP.Spec.Pool)

	// Inherit implementation strategy from the parent pool. Unlike PublicIPPool (which
	// reads strategy from its own spec), PublicIP must look it up from the parent.
	implementationStrategy := pool.Spec.ImplementationStrategy
	if implementationStrategy == "" {
		implementationStrategy = defaultPublicIPPoolImplementationStrategy
	}

	if publicIP.Annotations == nil {
		publicIP.Annotations = make(map[string]string)
	}

	// Capture CI UUID before syncComputeInstanceTargetNamespaceAnnotation, which may
	// clear spec.computeInstance via handleAutoDetach. handleProvisioning's OnSuccess
	// needs this to remove the CI detach finalizer after a detach job completes.
	priorCIUUID := publicIP.Spec.ComputeInstance

	// Resolve the target namespace for the ComputeInstance attachment, if any.
	needsUpdate, requeueForCI, err := r.syncComputeInstanceTargetNamespaceAnnotation(ctx, publicIP)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeueForCI {
		return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
	}

	// Annotate the CR so AAP playbooks can select the appropriate role without
	// having to look up the parent pool themselves.
	if publicIP.Annotations[osacImplementationStrategyAnnotation] != implementationStrategy {
		publicIP.Annotations[osacImplementationStrategyAnnotation] = implementationStrategy
		log.Info("setting implementation-strategy annotation", "strategy", implementationStrategy)
		needsUpdate = true
	}
	if publicIP.Annotations[osacPublicIPPoolNameAnnotation] != pool.Name {
		publicIP.Annotations[osacPublicIPPoolNameAnnotation] = pool.Name
		log.Info("setting publicippool-name annotation", "poolName", pool.Name)
		needsUpdate = true
	}
	if needsUpdate {
		if err := r.Update(ctx, publicIP); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch to get the latest resourceVersion after the metadata update
		if err := r.Get(ctx, client.ObjectKeyFromObject(publicIP), publicIP); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Compute desired config version from spec and inherited implementation strategy.
	// This hash drives the provisioning lifecycle: a new version triggers re-provisioning.
	desiredVersion, err := provisioning.ComputeDesiredConfigVersion(struct {
		Spec                   v1alpha1.PublicIPSpec
		ImplementationStrategy string
	}{publicIP.Spec, implementationStrategy})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to compute desired config version: %w", err)
	}
	publicIP.Status.DesiredConfigVersion = desiredVersion

	v1alpha1.SetPublicIPStatusCondition(publicIP, metav1.Condition{
		Type:               string(v1alpha1.PublicIPConditionConfigurationApplied),
		Status:             metav1.ConditionTrue,
		Reason:             "ConfigurationApplied",
		Message:            "Controller has processed the current spec",
		LastTransitionTime: metav1.Now(),
	})

	// Detect attach/detach transitions and set transitional state before provisioning
	if publicIP.Status.State == v1alpha1.PublicIPStateAllocated && publicIP.Spec.ComputeInstance != "" {
		publicIP.Status.Phase = v1alpha1.PublicIPPhaseProgressing
		publicIP.Status.State = v1alpha1.PublicIPStateAttaching
	} else if publicIP.Status.State == v1alpha1.PublicIPStateAttached && publicIP.Spec.ComputeInstance == "" {
		publicIP.Status.Phase = v1alpha1.PublicIPPhaseProgressing
		publicIP.Status.State = v1alpha1.PublicIPStateReleasing
		// Emit detach event only for manual detach. Auto-detach already emitted
		// an AutoDetached event in handleAutoDetach; needsUpdate is true when
		// the spec was cleared by auto-detach in this reconcile.
		if !needsUpdate && r.Recorder != nil {
			r.Recorder.Eventf(publicIP, nil, corev1.EventTypeNormal,
				eventReasonDetached, eventActionReconcile,
				"PublicIP detached from ComputeInstance")
		}
	} else if publicIP.Status.Phase == "" || (publicIP.Status.Phase == v1alpha1.PublicIPPhaseReady &&
		!provisioning.IsConfigApplied(&publicIP.Status.Jobs, publicIP.Status.DesiredConfigVersion)) {
		// Transition to Progressing on first provision or when spec changed after a previous
		// success. Don't override Failed during backoff (the provisioning lifecycle handles retry).
		publicIP.Status.Phase = v1alpha1.PublicIPPhaseProgressing
		if publicIP.Status.State == "" {
			publicIP.Status.State = v1alpha1.PublicIPStatePending
		}
	}

	r.maybePopulateAddress(ctx, publicIP)

	return r.routeProvisioning(ctx, publicIP, priorCIUUID)
}

// maybePopulateAddress sets status.address from the MetalLB LoadBalancer Service
// after initial provisioning succeeds.
//
// State == Allocated is set exclusively by OnSuccess after the AAP provisioning
// job reports success, so this guard ensures address population happens strictly
// after provisioning completes. One-shot attach (Pending -> Attached) skips
// Allocated, so that path populates the address inside OnSuccess instead.
func (r *PublicIPReconciler) maybePopulateAddress(ctx context.Context, publicIP *v1alpha1.PublicIP) {
	if publicIP.Status.State != v1alpha1.PublicIPStateAllocated || publicIP.Status.Address != "" {
		return
	}
	log := ctrllog.FromContext(ctx)
	targetClient, err := getTargetClient(ctx, r.mgr, r.targetCluster)
	if err != nil {
		log.Error(err, "failed to get target cluster client for address lookup")
		return
	}
	ipAddress := r.getPublicIPAddress(ctx, targetClient, publicIP.Name)
	if ipAddress != "" {
		publicIP.Status.Address = ipAddress
		log.Info("populated PublicIP address from LoadBalancer Service", "address", ipAddress)
	}
}

// syncComputeInstanceTargetNamespaceAnnotation resolves the VM namespace for the
// ComputeInstance referenced by spec.computeInstance and writes it as an annotation.
// When spec.computeInstance is cleared, the annotation is removed.
func (r *PublicIPReconciler) syncComputeInstanceTargetNamespaceAnnotation(
	ctx context.Context, publicIP *v1alpha1.PublicIP,
) (changed bool, requeue bool, err error) {
	log := ctrllog.FromContext(ctx)

	if publicIP.Spec.ComputeInstance == "" {
		if _, ok := publicIP.Annotations[osacPublicIPTargetNamespaceAnnotation]; ok {
			delete(publicIP.Annotations, osacPublicIPTargetNamespaceAnnotation)
			log.Info("cleared publicip-target-namespace annotation")
			return true, false, nil
		}
		return false, false, nil
	}

	if r.ComputeInstanceNamespace == "" {
		return false, false, fmt.Errorf(
			"ComputeInstanceNamespace is not configured; cannot resolve target namespace for computeInstance %q",
			publicIP.Spec.ComputeInstance,
		)
	}

	ciList := &v1alpha1.ComputeInstanceList{}
	if err := r.List(ctx, ciList,
		client.InNamespace(r.ComputeInstanceNamespace),
		client.MatchingLabels{osacComputeInstanceIDLabel: publicIP.Spec.ComputeInstance},
	); err != nil {
		return false, false, err
	}
	if len(ciList.Items) == 0 {
		// CI not found. If state is Attached or Failed, the CI was deleted externally
		// (e.g., finalizer removed manually). Clear the stale reference.
		if publicIP.Status.State == v1alpha1.PublicIPStateAttached ||
			publicIP.Status.State == v1alpha1.PublicIPStateFailed {
			log.Info("ComputeInstance not found, clearing stale spec reference",
				"computeInstanceUUID", publicIP.Spec.ComputeInstance,
				"state", publicIP.Status.State)
			publicIP.Spec.ComputeInstance = ""
			return true, false, nil
		}
		log.Info("ComputeInstance not found, requeueing", "computeInstanceUUID", publicIP.Spec.ComputeInstance)
		return false, true, nil
	}

	ci := &ciList.Items[0]

	// Auto-detach: if CI is being deleted, handle per current PublicIP state
	if !ci.DeletionTimestamp.IsZero() {
		autoDetachResult, err := r.handleAutoDetach(ctx, publicIP, ci)
		if err != nil {
			return false, false, err
		}
		// autoDetachResult: specChanged=true means we cleared spec.computeInstance
		// and need to persist+refetch; requeue=true means requeue for in-flight ops
		return autoDetachResult.specChanged, autoDetachResult.requeue, nil
	}

	if ci.Status.VirtualMachineReference == nil {
		log.Info("ComputeInstance has no VirtualMachineReference yet, requeueing",
			"computeInstance", ci.Name, "computeInstanceUUID", publicIP.Spec.ComputeInstance)
		return false, true, nil
	}

	targetNamespace := ci.Status.VirtualMachineReference.Namespace
	if publicIP.Annotations == nil {
		publicIP.Annotations = make(map[string]string)
	}
	if publicIP.Annotations[osacPublicIPTargetNamespaceAnnotation] != targetNamespace {
		publicIP.Annotations[osacPublicIPTargetNamespaceAnnotation] = targetNamespace
		log.Info("setting publicip-target-namespace annotation", "targetNamespace", targetNamespace)
		return true, false, nil
	}

	return false, false, nil
}

type autoDetachResult struct {
	specChanged bool
	requeue     bool
}

// handleAutoDetach processes a PublicIP whose referenced ComputeInstance is being
// deleted. It adds a finalizer to the CI to block premature garbage collection, and
// clears spec.computeInstance based on the current state to trigger the existing detach flow.
func (r *PublicIPReconciler) handleAutoDetach(
	ctx context.Context,
	publicIP *v1alpha1.PublicIP,
	ci *v1alpha1.ComputeInstance,
) (autoDetachResult, error) {
	log := ctrllog.FromContext(ctx)

	// Add finalizer to CI to guarantee detach completes before CI deletion
	if controllerutil.AddFinalizer(ci, osacPublicIPDetachFinalizer) {
		log.Info("adding publicip-detach finalizer to ComputeInstance",
			"computeInstance", ci.Name,
			"computeInstanceUUID", publicIP.Spec.ComputeInstance)
		if err := r.Update(ctx, ci); err != nil {
			return autoDetachResult{}, err
		}
	}

	switch publicIP.Status.State {
	case v1alpha1.PublicIPStateAttached:
		// Clear spec to trigger existing detach flow (Attached -> Releasing)
		log.Info("auto-detaching PublicIP: ComputeInstance is being deleted",
			"computeInstanceUUID", publicIP.Spec.ComputeInstance)
		publicIP.Spec.ComputeInstance = ""
		// Emit warning event with generic message (no CI details for tenant reuse safety)
		if r.Recorder != nil {
			r.Recorder.Eventf(publicIP, nil, corev1.EventTypeWarning,
				eventReasonAutoDetached, eventActionReconcile,
				"Auto-detached: referenced ComputeInstance was deleted")
		}
		return autoDetachResult{specChanged: true}, nil

	case v1alpha1.PublicIPStateFailed:
		// Clear stale reference, no AAP call needed. State stays Failed.
		log.Info("clearing stale ComputeInstance reference on Failed PublicIP",
			"computeInstanceUUID", publicIP.Spec.ComputeInstance)
		ciUUID := publicIP.Spec.ComputeInstance
		publicIP.Spec.ComputeInstance = ""
		// No AAP detach job runs for Failed state, so OnSuccess will never fire.
		// Attempt CI finalizer removal directly.
		if err := r.maybeRemoveCIFinalizer(ctx, ciUUID, publicIP.Name); err != nil {
			return autoDetachResult{}, err
		}
		return autoDetachResult{specChanged: true}, nil

	case v1alpha1.PublicIPStateAttaching:
		// Let the in-flight attach complete, then auto-detach on next reconcile.
		// OnSuccess will set state to Attached, which triggers this path again.
		log.Info("ComputeInstance is being deleted but attach is in-flight, waiting",
			"computeInstanceUUID", publicIP.Spec.ComputeInstance)
		return autoDetachResult{requeue: true}, nil

	case v1alpha1.PublicIPStateReleasing:
		// Detach already in progress, no-op. When detach completes,
		// maybeRemoveCIFinalizer will handle finalizer cleanup.
		log.Info("ComputeInstance is being deleted but detach is already in progress",
			"computeInstanceUUID", publicIP.Spec.ComputeInstance)
		return autoDetachResult{}, nil

	case v1alpha1.PublicIPStateAllocated:
		// Either detach already completed (spec empty) or user just set the CI ref
		// and the CI is being deleted before attach started. Clear spec if set to
		// prevent the attach flow from proceeding, then attempt finalizer removal.
		specChanged := false
		if publicIP.Spec.ComputeInstance != "" {
			publicIP.Spec.ComputeInstance = ""
			specChanged = true
		}
		if err := r.maybeRemoveCIFinalizer(ctx, ci.Labels[osacComputeInstanceIDLabel], publicIP.Name); err != nil {
			return autoDetachResult{}, err
		}
		return autoDetachResult{specChanged: specChanged}, nil

	default:
		// Covers Pending and any future states. Clear the spec to prevent the
		// finalizer (added above) from being stranded with no removal path.
		log.Info("clearing ComputeInstance reference on PublicIP in unexpected state during auto-detach",
			"state", publicIP.Status.State,
			"computeInstanceUUID", publicIP.Spec.ComputeInstance)
		publicIP.Spec.ComputeInstance = ""
		if err := r.maybeRemoveCIFinalizer(ctx, ci.Labels[osacComputeInstanceIDLabel], publicIP.Name); err != nil {
			return autoDetachResult{}, err
		}
		return autoDetachResult{specChanged: true}, nil
	}
}

// maybeRemoveCIFinalizer removes the publicip-detach finalizer from the
// ComputeInstance identified by ciUUID if no PublicIPs still reference it.
// The finalizer stays until ALL PublicIPs are detached (multi-attach safe).
//
// excludePublicIP is the name of a PublicIP whose spec.computeInstance has been
// cleared in memory but not yet persisted. The API server still shows the old
// value, so we skip it to avoid a false "still referenced" result.
// Pass "" when no exclusion is needed (e.g. from OnSuccess, where the spec is
// already persisted).
func (r *PublicIPReconciler) maybeRemoveCIFinalizer(ctx context.Context, ciUUID string, excludePublicIP string) error {
	log := ctrllog.FromContext(ctx)

	ciList := &v1alpha1.ComputeInstanceList{}
	if err := r.List(ctx, ciList,
		client.InNamespace(r.ComputeInstanceNamespace),
		client.MatchingLabels{osacComputeInstanceIDLabel: ciUUID},
	); err != nil {
		return err
	}
	if len(ciList.Items) == 0 {
		return nil // CI already gone
	}

	ci := &ciList.Items[0]
	if !controllerutil.ContainsFinalizer(ci, osacPublicIPDetachFinalizer) {
		return nil // finalizer already removed
	}

	// Check if any PublicIPs still reference this CI
	publicIPs := &v1alpha1.PublicIPList{}
	if err := r.List(ctx, publicIPs, client.InNamespace(r.NetworkingNamespace)); err != nil {
		return err
	}

	for i := range publicIPs.Items {
		if publicIPs.Items[i].Name == excludePublicIP {
			continue
		}
		if publicIPs.Items[i].Spec.ComputeInstance == ciUUID {
			log.Info("other PublicIPs still reference CI, keeping finalizer",
				"computeInstanceUUID", ciUUID,
				"publicIP", publicIPs.Items[i].Name)
			return nil
		}
	}

	// No more references, remove finalizer
	log.Info("all PublicIPs detached, removing CI finalizer",
		"computeInstance", ci.Name, "computeInstanceUUID", ciUUID)
	if controllerutil.RemoveFinalizer(ci, osacPublicIPDetachFinalizer) {
		if err := r.Update(ctx, ci); err != nil {
			return err
		}
	}
	return nil
}

// handleDelete sets the Deleting phase, runs deprovisioning, and removes the finalizer
// once deprovisioning completes (or is skipped).
func (r *PublicIPReconciler) handleDelete(ctx context.Context, publicIP *v1alpha1.PublicIP) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("deleting public IP")

	publicIP.Status.Phase = v1alpha1.PublicIPPhaseDeleting

	if !controllerutil.ContainsFinalizer(publicIP, osacPublicIPFinalizer) {
		return ctrl.Result{}, nil
	}

	result, err := r.handleDeprovisioning(ctx, publicIP)
	if err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	// Deprovisioning complete, remove finalizer to allow K8s garbage collection
	log.Info("removing finalizer after successful deprovisioning")
	controllerutil.RemoveFinalizer(publicIP, osacPublicIPFinalizer)
	if err := r.Update(ctx, publicIP); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// routeProvisioning dispatches to the correct handler based on state.
// Failed state routes based on spec intent: spec.computeInstance set means retry
// attach, empty + failed detach job means retry detach, otherwise provision.
// This prevents the create playbook from running after a failed attach/detach
// which could conflict with a partially-moved MetalLB Service.
func (r *PublicIPReconciler) routeProvisioning(ctx context.Context, publicIP *v1alpha1.PublicIP, priorCIUUID string) (ctrl.Result, error) {
	switch publicIP.Status.State {
	case v1alpha1.PublicIPStateAttaching:
		return r.handleAttaching(ctx, publicIP)
	case v1alpha1.PublicIPStateReleasing:
		return r.handleDetaching(ctx, publicIP, priorCIUUID)
	case v1alpha1.PublicIPStateFailed:
		// Route based on spec intent. spec.computeInstance unambiguously signals
		// attach intent, so no job-type lookup is needed for that case.
		// For empty spec.computeInstance, we check the latest detach job to
		// disambiguate "failed detach" (needs handleDetaching to retry moving
		// the Service back) from "failed provision" (needs handleProvisioning
		// with backoff).
		if publicIP.Spec.ComputeInstance != "" {
			return r.handleAttaching(ctx, publicIP)
		}
		latestDetach := provisioning.FindLatestJobByType(publicIP.Status.Jobs, v1alpha1.JobTypeDetach)
		if latestDetach != nil && latestDetach.State == v1alpha1.JobStateFailed {
			return r.handleDetaching(ctx, publicIP, priorCIUUID)
		}
		return r.handleProvisioning(ctx, publicIP, priorCIUUID)
	default:
		return r.handleProvisioning(ctx, publicIP, priorCIUUID)
	}
}

// handleAttaching triggers an AAP attach job (osac-attach-public-ip) via the
// PublicIPAttachmentProvider and polls until completion. On success, state transitions
// to Attached. Jobs are tracked as JobTypeAttach in Status.Jobs.
//
// Uses TriggerProvision (not a dedicated attach method) because the
// PublicIPAttachmentProvider maps TriggerProvision to osac-attach-public-ip.
// When the PublicIPAttachment CRD is introduced, this provider moves to the new
// controller where attach IS the standard provision operation.
func (r *PublicIPReconciler) handleAttaching(ctx context.Context, publicIP *v1alpha1.PublicIP) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	if r.PublicIPAttachmentProvider == nil {
		log.Info("no attachment provider configured, skipping attach")
		return ctrl.Result{}, nil
	}

	latestAttachJob := provisioning.FindLatestJobByType(publicIP.Status.Jobs, v1alpha1.JobTypeAttach)

	// Trigger a new attach job if none exists, or if the previous one completed for
	// a different config version (spec changed since last attach, e.g., re-attach to
	// a different ComputeInstance).
	if latestAttachJob == nil || latestAttachJob.JobID == "" ||
		(latestAttachJob.State.IsTerminal() && latestAttachJob.ConfigVersion != publicIP.Status.DesiredConfigVersion) {

		log.Info("triggering attach job", "provider", r.PublicIPAttachmentProvider.Name())
		result, err := r.PublicIPAttachmentProvider.TriggerProvision(ctx, publicIP)
		if err != nil {
			log.Error(err, "failed to trigger attach job")
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		newJob := v1alpha1.JobStatus{
			JobID:         result.JobID,
			Type:          v1alpha1.JobTypeAttach,
			Timestamp:     metav1.NewTime(time.Now().UTC()),
			State:         result.InitialState,
			Message:       result.Message,
			ConfigVersion: publicIP.Status.DesiredConfigVersion,
		}
		publicIP.Status.Jobs = provisioning.AppendJob(publicIP.Status.Jobs, newJob, r.MaxJobHistory)

		// Reset state/phase so the next reconcile routes back to handleAttaching.
		// Without this, a retry from Failed state would leave phase=Failed and
		// routeProvisioning would misroute to handleProvisioning on the next reconcile.
		publicIP.Status.Phase = v1alpha1.PublicIPPhaseProgressing
		publicIP.Status.State = v1alpha1.PublicIPStateAttaching

		// Flush status immediately so the job ID survives a controller restart
		// and we don't trigger a duplicate job on the next reconcile.
		if err := r.Status().Update(ctx, publicIP); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("attach job triggered", "jobID", result.JobID)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	if !latestAttachJob.State.IsTerminal() {
		status, err := r.PublicIPAttachmentProvider.GetProvisionStatus(ctx, publicIP, latestAttachJob.JobID)
		if err != nil {
			log.Error(err, "failed to get attach job status", "jobID", latestAttachJob.JobID)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		if status.State != latestAttachJob.State || status.Message != latestAttachJob.Message {
			updatedJob := *latestAttachJob
			updatedJob.State = status.State
			updatedJob.Message = status.MessageWithDetails()
			provisioning.UpdateJob(publicIP.Status.Jobs, updatedJob)
		}

		if status.State == v1alpha1.JobStateSucceeded {
			publicIP.Status.Phase = v1alpha1.PublicIPPhaseReady
			publicIP.Status.State = v1alpha1.PublicIPStateAttached
			if r.Recorder != nil {
				r.Recorder.Eventf(publicIP, nil, corev1.EventTypeNormal,
					eventReasonAttached, eventActionReconcile,
					"PublicIP attached to ComputeInstance")
			}
			// The attach playbook deletes the parking Service (osac-pip-<name>
			// in metallb-system) and creates a new LB Service with a different
			// name (osac-pip-<name>-ingress in the VM namespace). The IP is
			// preserved via MetalLB annotations, so status.address stays valid
			// if it was already populated. This lookup only fires for one-shot
			// attach (Pending -> Attached) where the Allocated phase was skipped
			// and address was never populated.
			if publicIP.Status.Address == "" {
				if targetClient, err := getTargetClient(ctx, r.mgr, r.targetCluster); err == nil {
					if ns := publicIP.Annotations[osacPublicIPTargetNamespaceAnnotation]; ns != "" {
						ingressServiceName := publicIPServiceNamePrefix + publicIP.Name + "-ingress"
						svc := &corev1.Service{}
						if svcErr := targetClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: ingressServiceName}, svc); svcErr == nil {
							if len(svc.Status.LoadBalancer.Ingress) > 0 && svc.Status.LoadBalancer.Ingress[0].IP != "" {
								publicIP.Status.Address = svc.Status.LoadBalancer.Ingress[0].IP
							}
						}
					}
				}
			}
			log.Info("attach job succeeded", "jobID", latestAttachJob.JobID)
			return ctrl.Result{}, nil
		}

		if status.State == v1alpha1.JobStateFailed {
			publicIP.Status.Phase = v1alpha1.PublicIPPhaseFailed
			publicIP.Status.State = v1alpha1.PublicIPStateFailed
			log.Info("attach job failed", "jobID", latestAttachJob.JobID, "message", status.MessageWithDetails())
			return ctrl.Result{}, nil
		}

		log.Info("attach job still running", "jobID", latestAttachJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	return ctrl.Result{}, nil
}

// handleDetaching triggers an AAP detach job (osac-detach-public-ip) via the
// PublicIPAttachmentProvider and polls until completion. On success, state transitions
// to Allocated and the CI detach finalizer is cleaned up. Jobs are tracked as
// JobTypeDetach in Status.Jobs.
//
// Uses TriggerDeprovision because detach is the inverse of attach: the same
// provider maps TriggerDeprovision to osac-detach-public-ip. When the
// PublicIPAttachment CRD is introduced, detach becomes the standard deprovision
// operation (PublicIPAttachment deletion).
func (r *PublicIPReconciler) handleDetaching(ctx context.Context, publicIP *v1alpha1.PublicIP, priorCIUUID string) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	if r.PublicIPAttachmentProvider == nil {
		log.Info("no attachment provider configured, skipping detach")
		return ctrl.Result{}, nil
	}

	// Cannot detach while an attach job is still running: the MetalLB Service
	// may be mid-move between namespaces.
	latestAttachJob := provisioning.FindLatestJobByType(publicIP.Status.Jobs, v1alpha1.JobTypeAttach)
	if latestAttachJob != nil && !latestAttachJob.State.IsTerminal() {
		log.Info("attach job still running, waiting before detach", "jobID", latestAttachJob.JobID)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	latestDetachJob := provisioning.FindLatestJobByType(publicIP.Status.Jobs, v1alpha1.JobTypeDetach)

	if latestDetachJob == nil || latestDetachJob.JobID == "" ||
		(latestDetachJob.State.IsTerminal() && latestDetachJob.ConfigVersion != publicIP.Status.DesiredConfigVersion) {

		log.Info("triggering detach job", "provider", r.PublicIPAttachmentProvider.Name())
		result, err := r.PublicIPAttachmentProvider.TriggerDeprovision(ctx, publicIP)
		if err != nil {
			log.Error(err, "failed to trigger detach job")
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		switch result.Action {
		case provisioning.DeprovisionWaiting:
			log.Info("detach not ready, requeueing")
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		case provisioning.DeprovisionSkipped:
			log.Info("provider skipped detach")
			publicIP.Status.Phase = v1alpha1.PublicIPPhaseReady
			publicIP.Status.State = v1alpha1.PublicIPStateAllocated
			// Remove the CI detach finalizer even when the provider skips detach,
			// otherwise it blocks the ComputeInstance from being garbage collected.
			if priorCIUUID != "" {
				if err := r.maybeRemoveCIFinalizer(ctx, priorCIUUID, ""); err != nil {
					log.Error(err, "failed to remove CI finalizer after skipped detach",
						"computeInstanceUUID", priorCIUUID)
				}
			}
			return ctrl.Result{}, nil
		case provisioning.DeprovisionTriggered:
			newJob := v1alpha1.JobStatus{
				JobID:         result.JobID,
				Type:          v1alpha1.JobTypeDetach,
				Timestamp:     metav1.NewTime(time.Now().UTC()),
				State:         v1alpha1.JobStatePending,
				Message:       "Detach job triggered",
				ConfigVersion: publicIP.Status.DesiredConfigVersion,
			}
			publicIP.Status.Jobs = provisioning.AppendJob(publicIP.Status.Jobs, newJob, r.MaxJobHistory)

			// Reset state/phase so the next reconcile routes back to handleDetaching.
			publicIP.Status.Phase = v1alpha1.PublicIPPhaseProgressing
			publicIP.Status.State = v1alpha1.PublicIPStateReleasing

			// Flush status immediately so the job ID survives a controller restart.
			if err := r.Status().Update(ctx, publicIP); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("detach job triggered", "jobID", result.JobID)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		default:
			log.Info("unknown deprovision action returned by provider", "action", result.Action)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	if latestDetachJob != nil && !latestDetachJob.State.IsTerminal() {
		status, err := r.PublicIPAttachmentProvider.GetDeprovisionStatus(ctx, publicIP, latestDetachJob.JobID)
		if err != nil {
			log.Error(err, "failed to get detach job status", "jobID", latestDetachJob.JobID)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		if status.State != latestDetachJob.State || status.Message != latestDetachJob.Message {
			updatedJob := *latestDetachJob
			updatedJob.State = status.State
			updatedJob.Message = status.MessageWithDetails()
			provisioning.UpdateJob(publicIP.Status.Jobs, updatedJob)
		}

		if status.State == v1alpha1.JobStateSucceeded {
			publicIP.Status.Phase = v1alpha1.PublicIPPhaseReady
			publicIP.Status.State = v1alpha1.PublicIPStateAllocated
			if priorCIUUID != "" {
				if err := r.maybeRemoveCIFinalizer(ctx, priorCIUUID, ""); err != nil {
					log.Error(err, "failed to remove CI finalizer after detach",
						"computeInstanceUUID", priorCIUUID)
				}
			}
			log.Info("detach job succeeded", "jobID", latestDetachJob.JobID)
			return ctrl.Result{}, nil
		}

		if status.State == v1alpha1.JobStateFailed {
			publicIP.Status.Phase = v1alpha1.PublicIPPhaseFailed
			publicIP.Status.State = v1alpha1.PublicIPStateFailed
			log.Info("detach job failed", "jobID", latestDetachJob.JobID, "message", status.MessageWithDetails())
			return ctrl.Result{}, nil
		}

		log.Info("detach job still running", "jobID", latestDetachJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	return ctrl.Result{}, nil
}

// handleProvisioning delegates to the shared provisioning lifecycle, which triggers
// an AAP job (e.g., osac-create-public-ip) and polls its status until completion.
func (r *PublicIPReconciler) handleProvisioning(ctx context.Context, publicIP *v1alpha1.PublicIP, priorCIUUID string) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	if r.ProvisioningProvider == nil {
		log.Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	return provisioning.RunProvisioningLifecycle(ctx, r.ProvisioningProvider, publicIP,
		&provisioning.State{Jobs: &publicIP.Status.Jobs, DesiredConfigVersion: publicIP.Status.DesiredConfigVersion},
		r.MaxJobHistory, r.StatusPollInterval,
		&provisioning.PollCallbacks{
			OnFailed: func(_ string) {
				publicIP.Status.Phase = v1alpha1.PublicIPPhaseFailed
				publicIP.Status.State = v1alpha1.PublicIPStateFailed
			},
			OnSuccess: func(_ provisioning.ProvisionStatus) {
				publicIP.Status.Phase = v1alpha1.PublicIPPhaseReady
				// Determine final state from spec: if CI ref is present, an attach job
				// just completed; if empty, this was either initial allocation or detach.
				if publicIP.Spec.ComputeInstance != "" {
					publicIP.Status.State = v1alpha1.PublicIPStateAttached
					if r.Recorder != nil {
						r.Recorder.Eventf(publicIP, nil, corev1.EventTypeNormal,
							eventReasonAttached, eventActionReconcile,
							"PublicIP attached to ComputeInstance")
					}
					// One-shot attach (Pending -> Attached) skips the Allocated state,
					// so the address population guard in handleUpdate never fires.
					// Populate address here to cover that path.
					if publicIP.Status.Address == "" {
						if targetClient, err := getTargetClient(ctx, r.mgr, r.targetCluster); err == nil {
							if ip := r.getPublicIPAddress(ctx, targetClient, publicIP.Name); ip != "" {
								publicIP.Status.Address = ip
							}
						}
					}
				} else {
					publicIP.Status.State = v1alpha1.PublicIPStateAllocated
					// After detach completes, attempt CI finalizer removal
					if priorCIUUID != "" {
						if err := r.maybeRemoveCIFinalizer(ctx, priorCIUUID, ""); err != nil {
							log.Error(err, "failed to remove CI finalizer after detach",
								"computeInstanceUUID", priorCIUUID)
							// Non-fatal: finalizer cleanup will retry on next reconcile
						}
					}
				}
			},
		},
		func() bool {
			return provisioning.CheckAPIServerForNonTerminalProvisionJob(
				ctx, r.APIReader, client.ObjectKeyFromObject(publicIP), &v1alpha1.PublicIP{})
		},
		func() error { return r.Status().Update(ctx, publicIP) },
	)
}

// handleDeprovisioning triggers an AAP deprovisioning job (e.g., osac-delete-public-ip)
// and polls its status. On failure, it either blocks deletion (to prevent orphaned
// resources) or allows the process to continue, depending on provider policy.
func (r *PublicIPReconciler) handleDeprovisioning(ctx context.Context, publicIP *v1alpha1.PublicIP) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	if r.ProvisioningProvider == nil {
		log.Info("no provisioning provider configured, skipping deprovisioning")
		return ctrl.Result{}, nil
	}

	latestDeprovisionJob := provisioning.FindLatestJobByType(publicIP.Status.Jobs, v1alpha1.JobTypeDeprovision)

	// Trigger a new deprovisioning job if none exists yet
	if latestDeprovisionJob == nil || latestDeprovisionJob.JobID == "" {
		log.Info("triggering deprovisioning", "provider", r.ProvisioningProvider.Name())

		result, err := r.ProvisioningProvider.TriggerDeprovision(ctx, publicIP)
		if err != nil {
			log.Error(err, "failed to trigger deprovisioning")
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

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
				Message:                deprovisioningJobTriggeredMessage,
				BlockDeletionOnFailure: result.BlockDeletionOnFailure,
			}
			publicIP.Status.Jobs = provisioning.AppendJob(publicIP.Status.Jobs, newJob, r.MaxJobHistory)
			log.Info("deprovisioning job triggered", "jobID", result.JobID)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	// Poll the existing deprovisioning job
	status, err := r.ProvisioningProvider.GetDeprovisionStatus(ctx, publicIP, latestDeprovisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get deprovision job status", "jobID", latestDeprovisionJob.JobID)
		updatedJob := *latestDeprovisionJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		provisioning.UpdateJob(publicIP.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	updatedJob := *latestDeprovisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.MessageWithDetails()
	provisioning.UpdateJob(publicIP.Status.Jobs, updatedJob)

	if !status.State.IsTerminal() {
		log.Info("deprovision job still running", "jobID", latestDeprovisionJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	if status.State.IsSuccessful() {
		log.Info("deprovision job succeeded", "jobID", latestDeprovisionJob.JobID)
		return ctrl.Result{}, nil
	}

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

// getPublicIPAddress fetches the LoadBalancer Service created by the AAP create_public_ip
// playbook and returns the assigned IP from status.loadBalancer.ingress[0].ip.
// Returns "" on any error or if no IP is assigned yet (best-effort).
func (r *PublicIPReconciler) getPublicIPAddress(ctx context.Context, targetClient client.Client, publicIPName string) string {
	log := ctrllog.FromContext(ctx)

	svc := &corev1.Service{}
	serviceName := publicIPServiceNamePrefix + publicIPName
	if err := targetClient.Get(ctx, types.NamespacedName{Namespace: defaultMetalLBNamespace, Name: serviceName}, svc); err != nil {
		log.Error(err, "failed to get LoadBalancer Service", "namespace", defaultMetalLBNamespace, "name", serviceName)
		return ""
	}

	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		log.Info("LoadBalancer Service has no ingress IP yet", "name", serviceName)
		return ""
	}

	ip := svc.Status.LoadBalancer.Ingress[0].IP
	if ip == "" {
		log.Info("LoadBalancer Service ingress IP is empty", "name", serviceName)
		return ""
	}

	return ip
}

// SetupWithManager registers this controller with the multicluster manager.
// It watches PublicIP CRs in the networking namespace on the local cluster only,
// and also watches ComputeInstance resources so that PublicIPs reconcile immediately
// when a referenced CI's status changes (e.g., VirtualMachineReference is set).
func (r *PublicIPReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return mcbuilder.ControllerManagedBy(mgr).
		For(&v1alpha1.PublicIP{},
			mcbuilder.WithPredicates(NetworkingNamespacePredicate(r.NetworkingNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Watches(
			&v1alpha1.ComputeInstance{},
			mchandler.EnqueueRequestsFromMapFunc(r.mapComputeInstanceToPublicIPs),
			mcbuilder.WithPredicates(ComputeInstanceNamespacePredicate(r.ComputeInstanceNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false),
		).
		Complete(r)
}

// mapComputeInstanceToPublicIPs maps a ComputeInstance change to all PublicIPs
// that reference it via spec.computeInstance, so the PublicIP reconciler can
// update the target namespace annotation when the CI's VM reference appears.
func (r *PublicIPReconciler) mapComputeInstanceToPublicIPs(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	ciUUID, exists := obj.GetLabels()[osacComputeInstanceIDLabel]
	if !exists {
		return nil
	}

	publicIPs := &v1alpha1.PublicIPList{}
	if err := r.List(ctx, publicIPs, client.InNamespace(r.NetworkingNamespace)); err != nil {
		log.Error(err, "failed to list PublicIPs for ComputeInstance watch")
		return nil
	}

	var requests []reconcile.Request
	for i := range publicIPs.Items {
		if publicIPs.Items[i].Spec.ComputeInstance == ciUUID {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&publicIPs.Items[i]),
			})
		}
	}

	if len(requests) > 0 {
		log.Info("mapped ComputeInstance change to PublicIPs",
			"computeInstance", obj.GetName(),
			"computeInstanceUUID", ciUUID,
			"publicIPCount", len(requests),
		)
	}

	return requests
}
