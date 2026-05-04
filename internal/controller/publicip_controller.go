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
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
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
	APIReader                client.Reader
	Scheme                   *runtime.Scheme
	mgr                      mcmanager.Manager
	NetworkingNamespace      string
	ComputeInstanceNamespace string
	ProvisioningProvider     provisioning.ProvisioningProvider
	StatusPollInterval       time.Duration
	MaxJobHistory            int
	targetCluster            mc.ClusterName
}

// NewPublicIPReconciler creates a new reconciler for PublicIP resources.
func NewPublicIPReconciler(
	mgr mcmanager.Manager,
	networkingNamespace string,
	computeInstanceNamespace string,
	provisioningProvider provisioning.ProvisioningProvider,
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
		Client:                   mgr.GetLocalManager().GetClient(),
		APIReader:                mgr.GetLocalManager().GetAPIReader(),
		Scheme:                   mgr.GetLocalManager().GetScheme(),
		mgr:                      mgr,
		NetworkingNamespace:      networkingNamespace,
		ComputeInstanceNamespace: computeInstanceNamespace,
		ProvisioningProvider:     provisioningProvider,
		StatusPollInterval:       statusPollInterval,
		MaxJobHistory:            maxJobHistory,
		targetCluster:            targetCluster,
	}
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicips,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicips/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicips/finalizers,verbs=update
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicippools,verbs=get;list;watch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=computeinstances,verbs=get;list;watch

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

	// Transition to Progressing on first provision or when spec changed after a previous
	// success. Don't override Failed during backoff (the provisioning lifecycle handles retry).
	if publicIP.Status.Phase == "" || (publicIP.Status.Phase == v1alpha1.PublicIPPhaseReady &&
		!provisioning.IsConfigApplied(&publicIP.Status.Jobs, publicIP.Status.DesiredConfigVersion)) {
		publicIP.Status.Phase = v1alpha1.PublicIPPhaseProgressing
	}

	return r.handleProvisioning(ctx, publicIP)
}

// syncComputeInstanceTargetNamespaceAnnotation resolves the tenant namespace for the
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

	ciList := &v1alpha1.ComputeInstanceList{}
	if err := r.List(ctx, ciList,
		client.InNamespace(r.ComputeInstanceNamespace),
		client.MatchingLabels{osacComputeInstanceIDLabel: publicIP.Spec.ComputeInstance},
	); err != nil {
		return false, false, err
	}
	if len(ciList.Items) == 0 {
		log.Info("ComputeInstance not found, requeueing", "computeInstanceUUID", publicIP.Spec.ComputeInstance)
		return false, true, nil
	}

	ci := &ciList.Items[0]
	if ci.Status.TenantReference == nil {
		log.Info("ComputeInstance has no TenantReference yet, requeueing",
			"computeInstance", ci.Name, "computeInstanceUUID", publicIP.Spec.ComputeInstance)
		return false, true, nil
	}

	targetNamespace := ci.Status.TenantReference.Namespace
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

// handleProvisioning delegates to the shared provisioning lifecycle, which triggers
// an AAP job (e.g., osac-create-public-ip) and polls its status until completion.
func (r *PublicIPReconciler) handleProvisioning(ctx context.Context, publicIP *v1alpha1.PublicIP) (ctrl.Result, error) {
	if r.ProvisioningProvider == nil {
		ctrllog.FromContext(ctx).Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	return provisioning.RunProvisioningLifecycle(ctx, r.ProvisioningProvider, publicIP,
		&provisioning.State{Jobs: &publicIP.Status.Jobs, DesiredConfigVersion: publicIP.Status.DesiredConfigVersion},
		r.MaxJobHistory, r.StatusPollInterval,
		&provisioning.PollCallbacks{
			OnFailed:  func(_ string) { publicIP.Status.Phase = v1alpha1.PublicIPPhaseFailed },
			OnSuccess: func(_ provisioning.ProvisionStatus) { publicIP.Status.Phase = v1alpha1.PublicIPPhaseReady },
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
				Message:                "Deprovisioning job triggered",
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

// SetupWithManager registers this controller with the multicluster manager.
// It watches PublicIP CRs in the networking namespace on the local cluster only.
func (r *PublicIPReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return mcbuilder.ControllerManagedBy(mgr).
		For(&v1alpha1.PublicIP{},
			mcbuilder.WithPredicates(NetworkingNamespacePredicate(r.NetworkingNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Complete(r)
}
