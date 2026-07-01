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
	"os"
	"strings"
	"time"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
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

const (
	storageFinalizer        = "osac.openshift.io/storage"
	clusterStorageFinalizer = "osac.openshift.io/cluster-storage"
	storageControllerName   = "storage-controller"
)

// StorageReconciler reconciles storage lifecycle on Tenant CRs.
// It owns StorageBackendReady, ClusterStorageReady conditions,
// status.storageClasses, status.storageBackendJobs, and status.clusterStorageJobs on the Tenant CR.
type StorageReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	Recorder               events.EventRecorder
	tenantNamespace        string
	mgr                    mcmanager.Manager
	targetCluster          mc.ClusterName
	BackendProvider        provisioning.ProvisioningProvider
	ClusterStorageProvider provisioning.ProvisioningProvider
	StatusPollInterval     time.Duration
	MaxJobHistory          int
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=tenants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups=osac.openshift.io,resources=clusterorders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedcontrolplanes,verbs=get
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func NewStorageReconciler(
	mgr mcmanager.Manager,
	tenantNamespace string,
	targetCluster mc.ClusterName,
	backendProvider provisioning.ProvisioningProvider,
	clusterStorageProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
) *StorageReconciler {
	if mgr == nil {
		panic("mgr must not be nil")
	}

	if statusPollInterval == 0 {
		statusPollInterval = 30 * time.Second
	}

	if maxJobHistory <= 0 {
		maxJobHistory = provisioning.DefaultMaxJobHistory
	}

	return &StorageReconciler{
		Client:                 mgr.GetLocalManager().GetClient(),
		Scheme:                 mgr.GetLocalManager().GetScheme(),
		Recorder:               mgr.GetLocalManager().GetEventRecorder(storageControllerName),
		tenantNamespace:        tenantNamespace,
		mgr:                    mgr,
		targetCluster:          targetCluster,
		BackendProvider:        backendProvider,
		ClusterStorageProvider: clusterStorageProvider,
		StatusPollInterval:     statusPollInterval,
		MaxJobHistory:          maxJobHistory,
	}
}

func (r *StorageReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	instance := &v1alpha1.Tenant{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if val, exists := instance.Annotations[osacManagementStateAnnotation]; instance.ObjectMeta.DeletionTimestamp.IsZero() && exists && val == ManagementStateUnmanaged {
		log.Info("skipping storage reconciliation — management state is Unmanaged")
		return ctrl.Result{}, nil
	}

	if instance.Status.Phase != v1alpha1.TenantPhaseReady && instance.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	log.Info("start storage reconcile")

	oldstatus := instance.Status.DeepCopy()

	var res ctrl.Result
	var err error
	if instance.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.handleUpdate(ctx, instance)
	} else {
		res, err = r.handleDelete(ctx, instance)
	}

	if !equality.Semantic.DeepEqual(instance.Status, *oldstatus) {
		log.Info("storage status requires update")
		if updateErr := r.Status().Update(ctx, instance); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
	}

	log.Info("end storage reconcile")
	return res, err
}

func (r *StorageReconciler) handleUpdate(ctx context.Context, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	tenantName := instance.GetName()

	log.Info("handling storage update for Tenant", "name", tenantName)

	if !controllerutil.ContainsFinalizer(instance, storageFinalizer) {
		controllerutil.AddFinalizer(instance, storageFinalizer)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Stage 1: check hub Secret
	hubSecretReady, err := r.hubSecretExists(ctx, tenantName)
	if err != nil {
		return ctrl.Result{}, err
	}

	// TODO(OSAC-1957): BackendProvider != nil only means AAP is configured, not
	// that a storage backend (e.g. VAST) is registered. When AAP is configured
	// for compute provisioning but no backend exists, the controller triggers
	// a backend provisioning job that will fail. Wire the Backend API
	// (private.v1.StorageBackends/List) to check if a backend is registered
	// before entering the AAP path.
	if !hubSecretReady {
		if r.BackendProvider != nil {
			instance.SetStatusCondition(v1alpha1.TenantConditionStorageBackendReady,
				metav1.ConditionFalse,
				v1alpha1.TenantReasonNotFound,
				fmt.Sprintf("Hub Secret for tenant %q not found", tenantName))
			return r.handleBackendProvisioning(ctx, instance)
		}
		// When no provisioning provider is configured (no AAP URL/token),
		// the controller cannot create hub Secrets via AAP. This is the
		// normal state for prepare-tenant.sh environments that run OSAC
		// without a VAST storage backend. Instead of blocking here, fall
		// through to Stage 2 so the controller can resolve StorageClasses
		// from manually labeled SCs and populate status.storageClasses,
		// which the compute instance controller needs to provision VMs.
		instance.SetStatusCondition(v1alpha1.TenantConditionStorageBackendReady,
			metav1.ConditionFalse,
			v1alpha1.TenantReasonNoProvider,
			"No backend provider configured")
	} else {
		instance.SetStatusCondition(v1alpha1.TenantConditionStorageBackendReady,
			metav1.ConditionTrue,
			v1alpha1.TenantReasonFound,
			fmt.Sprintf("Hub Secret for tenant %q exists", tenantName))
	}
	// TODO(OSAC-1111): populate StorageBackendStatus once StorageBackend API provides name/provider

	// Stage 2: resolve StorageClasses on target cluster.
	targetClient, err := getTargetClient(ctx, r.mgr, r.targetCluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	clusterName := string(r.targetCluster)

	if r.ClusterStorageProvider != nil {
		// When AAP is configured, only accept tenant-specific StorageClasses
		// (labeled osac.openshift.io/tenant=<tenantName>). Default tenant SCs
		// (labeled tenant=Default) are not used as a substitute for
		// AAP-provisioned storage. If no tenant-specific SC exists, trigger
		// the AAP cluster storage job to create one.
		result, err := r.resolveTenantSpecificStorageClasses(ctx, targetClient, tenantName)
		if err != nil {
			return ctrl.Result{}, err
		}

		if len(result) == 0 {
			instance.SetStatusCondition(v1alpha1.TenantConditionClusterStorageReady,
				metav1.ConditionFalse,
				v1alpha1.TenantReasonNotFound,
				fmt.Sprintf("no tenant-specific StorageClass found for tenant %q", tenantName))
			instance.Status.StorageClasses = nil
			instance.Status.ClusterStorage = []v1alpha1.ClusterStorageStatus{
				{ClusterName: clusterName, Ready: false, Reason: v1alpha1.TenantReasonNotFound},
			}
			return r.handleClusterStorageProvisioning(ctx, instance)
		}

		instance.SetStatusCondition(v1alpha1.TenantConditionClusterStorageReady,
			metav1.ConditionTrue,
			v1alpha1.TenantReasonFound,
			formatResolvedStorageClasses(result))
		instance.Status.StorageClasses = result
		instance.Status.ClusterStorage = []v1alpha1.ClusterStorageStatus{
			{ClusterName: clusterName, Ready: true, Reason: v1alpha1.TenantReasonFound},
		}
	} else {
		// When no provisioning provider is configured, resolve StorageClasses
		// using the full tier resolution logic: tenant-specific SCs take
		// priority, with shared default SCs (labeled tenant=Default) as
		// fallback. This serves environments running OSAC without AAP/VAST
		// where an admin or prepare-tenant.sh has labeled existing
		// StorageClasses manually.
		result, err := getTenantStorageClasses(ctx, targetClient, tenantName)
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
			instance.SetStatusCondition(v1alpha1.TenantConditionClusterStorageReady,
				metav1.ConditionFalse,
				reason,
				result.conditionMessage())
			instance.Status.StorageClasses = nil
			instance.Status.ClusterStorage = []v1alpha1.ClusterStorageStatus{
				{ClusterName: clusterName, Ready: false, Reason: reason},
			}
			return ctrl.Result{}, nil
		}

		instance.SetStatusCondition(v1alpha1.TenantConditionClusterStorageReady,
			metav1.ConditionTrue,
			v1alpha1.TenantReasonFound,
			result.conditionMessage())
		instance.Status.StorageClasses = result.resolved
		instance.Status.ClusterStorage = []v1alpha1.ClusterStorageStatus{
			{ClusterName: clusterName, Ready: true, Reason: v1alpha1.TenantReasonFound},
		}
	}

	// Poll any non-terminal class provision job to update its status
	latestClassJob := provisioning.FindLatestJobByType(instance.Status.ClusterStorageJobs, v1alpha1.JobTypeProvision)
	if latestClassJob != nil && !latestClassJob.State.IsTerminal() && r.ClusterStorageProvider != nil {
		return provisioning.PollJob(ctx, r.ClusterStorageProvider, instance,
			&provisioning.State{Jobs: &instance.Status.ClusterStorageJobs},
			latestClassJob, r.StatusPollInterval, nil)
	}

	// Stage 3: provision cluster-side storage on CaaS clusters owned by this tenant.
	// Runs after VMaaS (Stage 2) because CaaS requires StorageBackendReady (Stage 1)
	// to have completed during tenant onboarding before cluster-side resources can
	// be installed.
	if r.ClusterStorageProvider != nil {
		caasResult, caasErr := r.handleCaaSUpdate(ctx, instance)
		if caasErr != nil || caasResult.RequeueAfter > 0 {
			return caasResult, caasErr
		}

		// Handle individual ClusterOrder deletions while the Tenant is still
		// alive. When a ClusterOrder is deleted, mapClusterOrderToTenant
		// triggers Tenant reconciliation, and we clean up cluster-side storage
		// for that specific cluster here.
		caasDelResult, caasDelErr := r.handleCaaSDelete(ctx, instance, false)
		if caasDelErr != nil || caasDelResult.RequeueAfter > 0 {
			return caasDelResult, caasDelErr
		}
	}

	return ctrl.Result{}, nil
}

// handleCaaSUpdate provisions cluster-side storage on CaaS clusters belonging
// to this tenant. For each Ready ClusterOrder without ClusterStorageReady=True,
// it retrieves the kubeconfig and triggers the AAP provisioning job.
func (r *StorageReconciler) handleCaaSUpdate(ctx context.Context, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	tenantName := instance.GetName()

	clusterOrders := &v1alpha1.ClusterOrderList{}
	if err := r.List(ctx, clusterOrders); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ClusterOrders: %w", err)
	}

	for i := range clusterOrders.Items {
		co := &clusterOrders.Items[i]

		annotation, exists := co.GetAnnotations()[osacTenantKey]
		if !exists || annotation != tenantName {
			continue
		}

		if co.Status.Phase != v1alpha1.ClusterOrderPhaseReady {
			continue
		}

		if co.IsStatusConditionTrue(string(v1alpha1.ClusterOrderConditionClusterStorageReady)) {
			continue
		}

		// ClusterOrders being deleted are handled by handleCaaSDelete
		if !co.DeletionTimestamp.IsZero() {
			continue
		}

		log.Info("processing CaaS cluster storage", "clusterOrder", co.Name, "tenant", tenantName)

		if controllerutil.AddFinalizer(co, clusterStorageFinalizer) {
			if err := r.Update(ctx, co); err != nil {
				return ctrl.Result{}, fmt.Errorf("add storage finalizer to ClusterOrder %s: %w", co.Name, err)
			}
		}

		kubeconfig, err := r.getClusterKubeconfig(ctx, co)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("get kubeconfig for ClusterOrder %s: %w", co.Name, err)
		}

		// Continue to the next ClusterOrder rather than returning so that
		// other clusters for this tenant can still be provisioned. The
		// HostedControlPlane may not be ready yet for this cluster.
		if kubeconfig == nil {
			co.SetStatusCondition(
				string(v1alpha1.ClusterOrderConditionClusterStorageReady),
				metav1.ConditionFalse,
				"Kubeconfig not yet available for CaaS cluster",
				"KubeConfigNotAvailable")
			if err := r.Status().Update(ctx, co); err != nil {
				return ctrl.Result{}, fmt.Errorf("update ClusterOrder %s status: %w", co.Name, err)
			}
			continue
		}

		provCtx := provisioning.WithAdminKubeconfig(ctx, string(kubeconfig))

		provResult, provErr := provisioning.RunProvisioningLifecycle(provCtx, r.ClusterStorageProvider, co,
			&provisioning.State{Jobs: &co.Status.ClusterStorageJobs},
			r.MaxJobHistory, r.StatusPollInterval, nil,
			func() bool {
				return provisioning.CheckAPIServerForNonTerminalProvisionJob(ctx, r.Client,
					client.ObjectKeyFromObject(co), &v1alpha1.ClusterOrder{},
					func(obj client.Object) []v1alpha1.JobStatus {
						return obj.(*v1alpha1.ClusterOrder).Status.ClusterStorageJobs
					})
			},
			func() error { return r.Status().Update(ctx, co) },
		)
		if provErr != nil {
			co.SetStatusCondition(
				string(v1alpha1.ClusterOrderConditionClusterStorageReady),
				metav1.ConditionFalse,
				fmt.Sprintf("Provisioning failed: %v", provErr),
				"ProvisionFailed")
			if updateErr := r.Status().Update(ctx, co); updateErr != nil {
				log.Error(updateErr, "failed to update ClusterOrder status after provision error", "clusterOrder", co.Name)
			}
			return provResult, provErr
		}

		if provResult.RequeueAfter > 0 {
			if err := r.Status().Update(ctx, co); err != nil {
				return ctrl.Result{}, fmt.Errorf("update ClusterOrder %s status: %w", co.Name, err)
			}
			return provResult, nil
		}

		// Provisioning complete: discover StorageClasses on the CaaS cluster
		caasClient, err := r.buildClientFromKubeconfig(kubeconfig)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("build CaaS client for ClusterOrder %s: %w", co.Name, err)
		}

		scResult, err := getTenantStorageClasses(ctx, caasClient, tenantName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("get StorageClasses on CaaS cluster %s: %w", co.Name, err)
		}

		for _, msg := range scResult.duplicateMessages {
			r.Recorder.Eventf(co, nil, corev1.EventTypeWarning, eventReasonDuplicateStorageClass, eventActionDetectDuplicate, "%s", msg)
		}

		if len(scResult.resolved) > 0 {
			co.SetStatusCondition(
				string(v1alpha1.ClusterOrderConditionClusterStorageReady),
				metav1.ConditionTrue,
				scResult.conditionMessage(),
				v1alpha1.TenantReasonFound)
			r.Recorder.Eventf(co, nil, corev1.EventTypeNormal, "ClusterStorageProvisioned", "Provision",
				"Storage provisioned on CaaS cluster %s", co.Name)
		} else {
			reason := v1alpha1.TenantReasonNotFound
			if len(scResult.duplicateMessages) > 0 {
				reason = v1alpha1.TenantReasonMultipleFound
			}
			co.SetStatusCondition(
				string(v1alpha1.ClusterOrderConditionClusterStorageReady),
				metav1.ConditionFalse,
				scResult.conditionMessage(),
				reason)
		}

		if err := r.Status().Update(ctx, co); err != nil {
			return ctrl.Result{}, fmt.Errorf("update ClusterOrder %s status: %w", co.Name, err)
		}

		r.updateTenantClusterStorage(instance, co)
	}

	return ctrl.Result{}, nil
}

func (r *StorageReconciler) buildClientFromKubeconfig(kubeconfig []byte) (client.Client, error) {
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	c, err := client.New(restConfig, client.Options{Scheme: r.Scheme})
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	return c, nil
}

// updateTenantClusterStorage adds or updates the CaaS cluster entry in
// Tenant.status.clusterStorage without overwriting existing entries.
func (r *StorageReconciler) updateTenantClusterStorage(tenant *v1alpha1.Tenant, co *v1alpha1.ClusterOrder) {
	clusterName := co.Name
	ready := co.IsStatusConditionTrue(string(v1alpha1.ClusterOrderConditionClusterStorageReady))
	reason := v1alpha1.TenantReasonNotFound
	if ready {
		reason = v1alpha1.TenantReasonFound
	} else if cond := apimeta.FindStatusCondition(co.Status.Conditions, string(v1alpha1.ClusterOrderConditionClusterStorageReady)); cond != nil {
		reason = cond.Reason
	}

	for i, cs := range tenant.Status.ClusterStorage {
		if cs.ClusterName == clusterName {
			tenant.Status.ClusterStorage[i].Ready = ready
			tenant.Status.ClusterStorage[i].Reason = reason
			return
		}
	}
	tenant.Status.ClusterStorage = append(tenant.Status.ClusterStorage, v1alpha1.ClusterStorageStatus{
		ClusterName: clusterName,
		Ready:       ready,
		Reason:      reason,
	})
}

func (r *StorageReconciler) handleDelete(ctx context.Context, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("handling storage delete for Tenant", "name", instance.Name)

	if !controllerutil.ContainsFinalizer(instance, storageFinalizer) {
		return ctrl.Result{}, nil
	}

	// CaaS cleanup: remove cluster-side storage (StorageClasses, CSI) from
	// all CaaS clusters and remove our finalizer from their ClusterOrders.
	// The ClusterOrders themselves are not being deleted (they have no
	// OwnerReference to the Tenant), but the storage backend is about to be
	// torn down so cluster-side resources must be removed first.
	if r.ClusterStorageProvider != nil {
		caasResult, caasErr := r.handleCaaSDelete(ctx, instance, true)
		if caasErr != nil || caasResult.RequeueAfter > 0 {
			return caasResult, caasErr
		}
	}

	// Stage 1: class cleanup
	classDeprovJob := provisioning.FindLatestJobByType(instance.Status.ClusterStorageJobs, v1alpha1.JobTypeDeprovision)
	classCleanupDone := classDeprovJob != nil && classDeprovJob.State.IsTerminal() && classDeprovJob.State.IsSuccessful()

	if !classCleanupDone {
		result, err := r.handleClusterStorageDeprovisioning(ctx, instance)
		if err != nil {
			return result, err
		}
		if result.RequeueAfter > 0 {
			return result, nil
		}
		// Class cleanup just completed successfully — fall through to backend
	}

	// Stage 2: backend teardown
	result, err := r.handleBackendDeprovisioning(ctx, instance)
	if err != nil {
		return result, err
	}
	if result.RequeueAfter > 0 {
		return result, nil
	}

	controllerutil.RemoveFinalizer(instance, storageFinalizer)
	if err := r.Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("storage finalizer removed, deletion will proceed")
	return ctrl.Result{}, nil
}

// --- Stage 1: Backend provisioning ---

func (r *StorageReconciler) handleBackendProvisioning(ctx context.Context, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	latestJob := provisioning.FindLatestJobByType(instance.Status.StorageBackendJobs, v1alpha1.JobTypeProvision)
	if latestJob != nil && latestJob.State == v1alpha1.JobStateFailed {
		ctrllog.FromContext(ctx).Info("latest backend provision job failed, waiting for external trigger to retry",
			"message", latestJob.Message)
		return ctrl.Result{}, nil
	}

	return provisioning.RunProvisioningLifecycle(ctx, r.BackendProvider, instance,
		&provisioning.State{Jobs: &instance.Status.StorageBackendJobs},
		r.MaxJobHistory, r.StatusPollInterval, nil,
		func() bool {
			return provisioning.CheckAPIServerForNonTerminalProvisionJob(ctx, r.Client,
				client.ObjectKeyFromObject(instance), &v1alpha1.Tenant{},
				func(obj client.Object) []v1alpha1.JobStatus {
					return obj.(*v1alpha1.Tenant).Status.StorageBackendJobs
				})
		},
		func() error { return r.Status().Update(ctx, instance) },
	)
}

// --- Stage 2: Cluster storage provisioning ---

func (r *StorageReconciler) handleClusterStorageProvisioning(ctx context.Context, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	latestJob := provisioning.FindLatestJobByType(instance.Status.ClusterStorageJobs, v1alpha1.JobTypeProvision)
	if latestJob != nil && latestJob.State == v1alpha1.JobStateFailed {
		ctrllog.FromContext(ctx).Info("latest cluster storage provision job failed, waiting for external trigger to retry",
			"message", latestJob.Message)
		return ctrl.Result{}, nil
	}

	return provisioning.RunProvisioningLifecycle(ctx, r.ClusterStorageProvider, instance,
		&provisioning.State{Jobs: &instance.Status.ClusterStorageJobs},
		r.MaxJobHistory, r.StatusPollInterval, nil,
		func() bool {
			return provisioning.CheckAPIServerForNonTerminalProvisionJob(ctx, r.Client,
				client.ObjectKeyFromObject(instance), &v1alpha1.Tenant{},
				func(obj client.Object) []v1alpha1.JobStatus {
					return obj.(*v1alpha1.Tenant).Status.ClusterStorageJobs
				})
		},
		func() error { return r.Status().Update(ctx, instance) },
	)
}

// --- CaaS Deprovisioning ---

// handleCaaSDelete removes cluster-side storage from CaaS clusters that have
// the storage finalizer.
//
// When tenantDeleting=true (called from handleDelete): processes ALL ClusterOrders
// with our finalizer because the backend is about to be torn down.
//
// When tenantDeleting=false (called from handleUpdate): processes only ClusterOrders
// with a DeletionTimestamp, handling the case where a single cluster is being
// destroyed while the Tenant is still alive.
//
// If the HostedControlPlane is already gone (cluster destroyed outside OSAC),
// cleanup is skipped because there are no cluster-side resources left to remove.
func (r *StorageReconciler) handleCaaSDelete(ctx context.Context, instance *v1alpha1.Tenant, tenantDeleting bool) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	tenantName := instance.GetName()

	clusterOrders := &v1alpha1.ClusterOrderList{}
	if err := r.List(ctx, clusterOrders); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ClusterOrders for CaaS teardown: %w", err)
	}

	for i := range clusterOrders.Items {
		co := &clusterOrders.Items[i]

		if !controllerutil.ContainsFinalizer(co, clusterStorageFinalizer) {
			continue
		}

		annotation, exists := co.GetAnnotations()[osacTenantKey]
		if !exists || annotation != tenantName {
			continue
		}

		// When called from handleUpdate, only process ClusterOrders that are
		// actively being deleted. When the Tenant itself is being deleted, we
		// process all ClusterOrders to clean up before backend teardown.
		if !tenantDeleting && co.DeletionTimestamp.IsZero() {
			continue
		}

		log.Info("tearing down CaaS cluster storage", "clusterOrder", co.Name, "tenant", tenantName)

		kubeconfig, err := r.getClusterKubeconfig(ctx, co)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("get kubeconfig for teardown of ClusterOrder %s: %w", co.Name, err)
		}

		if kubeconfig == nil {
			// HostedControlPlane is gone, so the cluster no longer exists.
			// Nothing to clean up on the cluster side.
			log.Info("HostedControlPlane not found during teardown, skipping cluster-side cleanup",
				"clusterOrder", co.Name, "tenant", tenantName)
			r.Recorder.Eventf(co, nil, corev1.EventTypeWarning, "KubeConfigNotAvailable", "Teardown",
				"HostedControlPlane not found during storage teardown, skipping cleanup")
		} else {
			provCtx := provisioning.WithAdminKubeconfig(ctx, string(kubeconfig))

			result, done, err := provisioning.RunDeprovisioningLifecycle(provCtx, r.ClusterStorageProvider, co,
				&co.Status.ClusterStorageJobs, r.MaxJobHistory, r.StatusPollInterval)
			if err != nil {
				if updateErr := r.Status().Update(ctx, co); updateErr != nil {
					log.Error(updateErr, "failed to update ClusterOrder status after teardown error", "clusterOrder", co.Name)
				}
				return result, err
			}
			if !done {
				if updateErr := r.Status().Update(ctx, co); updateErr != nil {
					log.Error(updateErr, "failed to update ClusterOrder status during teardown", "clusterOrder", co.Name)
				}
				return result, nil
			}
		}

		r.removeTenantClusterStorageEntry(instance, co.Name)

		controllerutil.RemoveFinalizer(co, clusterStorageFinalizer)
		if err := r.Update(ctx, co); err != nil {
			return ctrl.Result{}, fmt.Errorf("remove storage finalizer from ClusterOrder %s: %w", co.Name, err)
		}

		log.Info("CaaS cluster storage teardown complete", "clusterOrder", co.Name, "tenant", tenantName)
	}

	return ctrl.Result{}, nil
}

func (r *StorageReconciler) removeTenantClusterStorageEntry(tenant *v1alpha1.Tenant, clusterName string) {
	filtered := make([]v1alpha1.ClusterStorageStatus, 0, len(tenant.Status.ClusterStorage))
	for _, cs := range tenant.Status.ClusterStorage {
		if cs.ClusterName != clusterName {
			filtered = append(filtered, cs)
		}
	}
	tenant.Status.ClusterStorage = filtered
}

// --- VMaaS Deprovisioning ---

func (r *StorageReconciler) handleClusterStorageDeprovisioning(ctx context.Context, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	if r.ClusterStorageProvider == nil {
		ctrllog.FromContext(ctx).Info("no class provider configured, skipping cluster-side cleanup")
		return ctrl.Result{}, nil
	}

	result, done, err := provisioning.RunDeprovisioningLifecycle(ctx, r.ClusterStorageProvider, instance,
		&instance.Status.ClusterStorageJobs, r.MaxJobHistory, r.StatusPollInterval)
	if err != nil || !done {
		return result, err
	}
	return ctrl.Result{}, nil
}

func (r *StorageReconciler) handleBackendDeprovisioning(ctx context.Context, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	if r.BackendProvider == nil {
		log.Info("no backend provider configured, skipping backend teardown")
		return ctrl.Result{}, nil
	}

	hubSecretReady, err := r.hubSecretExists(ctx, instance.GetName())
	if err != nil {
		log.Error(err, "failed to check hub Secret existence, requeueing")
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	latestJob := provisioning.FindLatestJobByType(instance.Status.StorageBackendJobs, v1alpha1.JobTypeDeprovision)
	deprovJobActive := latestJob != nil && latestJob.JobID != "" && !latestJob.State.IsTerminal()
	deprovJobBlocking := latestJob != nil &&
		latestJob.State.IsTerminal() &&
		!latestJob.State.IsSuccessful() &&
		latestJob.BlockDeletionOnFailure

	if !hubSecretReady && !deprovJobActive && !deprovJobBlocking {
		return ctrl.Result{}, nil
	}

	result, done, err := provisioning.RunDeprovisioningLifecycle(ctx, r.BackendProvider, instance,
		&instance.Status.StorageBackendJobs, r.MaxJobHistory, r.StatusPollInterval)
	if err != nil || !done {
		return result, err
	}
	return ctrl.Result{}, nil
}

// --- Helpers ---

func (r *StorageReconciler) hubSecretExists(ctx context.Context, tenantName string) (bool, error) {
	var secretList corev1.SecretList
	if err := r.List(ctx, &secretList,
		client.InNamespace(storageConfigNamespace()),
		client.MatchingLabels{osacTenantKey: tenantName},
	); err != nil {
		return false, err
	}
	return len(secretList.Items) > 0, nil
}

// storageConfigNamespace returns the namespace where storage config Secrets are stored.
// In production, OSAC_STORAGE_CONFIG_NAMESPACE is set by the Helm chart or kustomize
// overlay to match the deployment namespace. The fallback is for local development only.
func storageConfigNamespace() string {
	if ns := os.Getenv("OSAC_STORAGE_CONFIG_NAMESPACE"); ns != "" {
		return ns
	}
	return "osac-system"
}

// --- Watch mapping ---

func (r *StorageReconciler) mapStorageClassToTenant(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	tenantName, exists := obj.GetLabels()[osacTenantKey]
	if !exists || tenantName == "" {
		return nil
	}

	if tenantName == defaultStorageClassSentinel {
		log.Info("shared Default StorageClass changed, reconciling all tenants",
			"storageClass", obj.GetName())
		return r.allTenantReconcileRequests(ctx)
	}

	tenant := &v1alpha1.Tenant{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.tenantNamespace, Name: tenantName}, tenant); err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "unable to get Tenant for StorageClass",
				"storageClass", obj.GetName(), "tenant", tenantName)
		}
		return nil
	}

	return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(tenant)}}
}

func (r *StorageReconciler) mapSecretToTenant(ctx context.Context, obj client.Object) []reconcile.Request {
	tenantName, exists := obj.GetLabels()[osacTenantKey]
	if !exists || tenantName == "" {
		return nil
	}

	tenant := &v1alpha1.Tenant{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.tenantNamespace, Name: tenantName}, tenant); err != nil {
		return nil
	}

	return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(tenant)}}
}

func (r *StorageReconciler) allTenantReconcileRequests(ctx context.Context) []reconcile.Request {
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
	return requests
}

// mapClusterOrderToTenant maps ClusterOrder events to the owning Tenant so the
// storage controller can evaluate CaaS cluster storage provisioning/teardown.

// resolveTenantSpecificStorageClasses lists only StorageClasses labeled with the
// given tenant name, ignoring shared defaults (labeled tenant=Default). Used when
// AAP is configured and the controller should not fall back to shared defaults.
func (r *StorageReconciler) resolveTenantSpecificStorageClasses(
	ctx context.Context, targetClient client.Client, tenantName string,
) ([]v1alpha1.ResolvedStorageClass, error) {
	scList := &storagev1.StorageClassList{}
	if err := targetClient.List(ctx, scList, client.MatchingLabels{osacTenantKey: tenantName}); err != nil {
		return nil, err
	}
	byTier := groupByTier(scList.Items)
	var resolved []v1alpha1.ResolvedStorageClass
	for tier, scs := range byTier {
		if len(scs) == 1 {
			resolved = append(resolved, v1alpha1.ResolvedStorageClass{
				Name: scs[0].GetName(),
				Tier: tier,
			})
		}
	}
	return resolved, nil
}

// formatResolvedStorageClasses builds a human-readable condition message
// listing each resolved tier and its StorageClass name.
func formatResolvedStorageClasses(classes []v1alpha1.ResolvedStorageClass) string {
	parts := make([]string, len(classes))
	for i, sc := range classes {
		parts[i] = fmt.Sprintf("tier %q: StorageClass %q (tenant-specific)", sc.Tier, sc.Name)
	}
	return strings.Join(parts, "; ")
}

func (r *StorageReconciler) mapClusterOrderToTenant(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	tenantName, exists := obj.GetAnnotations()[osacTenantKey]
	if !exists || tenantName == "" {
		return nil
	}

	tenant := &v1alpha1.Tenant{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.tenantNamespace, Name: tenantName}, tenant); err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "unable to get Tenant for ClusterOrder",
				"clusterOrder", obj.GetName(), "tenant", tenantName)
		}
		return nil
	}

	return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(tenant)}}
}

// getClusterKubeconfig retrieves the admin kubeconfig for a CaaS cluster by
// reading the HostedControlPlane's kubeConfig Secret reference. Returns nil
// without error if the HostedControlPlane or Secret is not yet available.
func (r *StorageReconciler) getClusterKubeconfig(ctx context.Context, clusterOrder *v1alpha1.ClusterOrder) ([]byte, error) {
	log := ctrllog.FromContext(ctx)
	ref := clusterOrder.Status.ClusterReference
	if ref == nil || ref.Namespace == "" || ref.HostedClusterName == "" {
		log.V(1).Info("no cluster reference on ClusterOrder", "clusterOrder", clusterOrder.Name)
		return nil, nil
	}

	hcp := &hypershiftv1beta1.HostedControlPlane{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: ref.Namespace,
		Name:      ref.HostedClusterName,
	}, hcp); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("get HostedControlPlane %s/%s: %w", ref.Namespace, ref.HostedClusterName, err)
		}
		log.Info("HostedControlPlane not found", "clusterOrder", clusterOrder.Name,
			"namespace", ref.Namespace, "hostedClusterName", ref.HostedClusterName)
		return nil, nil
	}

	if hcp.Status.KubeConfig == nil || hcp.Status.KubeConfig.Name == "" {
		log.Info("HostedControlPlane kubeConfig reference not yet populated",
			"clusterOrder", clusterOrder.Name, "hostedControlPlane", hcp.Name)
		return nil, nil
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: ref.Namespace,
		Name:      hcp.Status.KubeConfig.Name,
	}, secret); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("get kubeconfig Secret %s/%s: %w", ref.Namespace, hcp.Status.KubeConfig.Name, err)
		}
		log.Info("kubeconfig Secret not found", "clusterOrder", clusterOrder.Name,
			"secret", hcp.Status.KubeConfig.Name, "namespace", ref.Namespace)
		return nil, nil
	}

	kubeconfig, exists := secret.Data[hcp.Status.KubeConfig.Key]
	if !exists || len(kubeconfig) == 0 {
		log.Info("kubeconfig key missing or empty in Secret", "clusterOrder", clusterOrder.Name,
			"secret", secret.Name, "key", hcp.Status.KubeConfig.Key)
		return nil, nil
	}

	return kubeconfig, nil
}

func (r *StorageReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return mcbuilder.ControllerManagedBy(mgr).
		For(&v1alpha1.Tenant{},
			mcbuilder.WithPredicates(tenantNamespacePredicate(r.tenantNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Named("storage").
		Watches(
			&v1alpha1.ClusterOrder{},
			mchandler.EnqueueRequestsFromMapFunc(r.mapClusterOrderToTenant),
		).
		Watches(
			&storagev1.StorageClass{},
			mchandler.EnqueueRequestsFromMapFunc(r.mapStorageClassToTenant),
			mcbuilder.WithPredicates(storageClassTenantPredicate()),
		).
		Watches(
			&corev1.Secret{},
			mchandler.EnqueueRequestsFromMapFunc(r.mapSecretToTenant),
			mcbuilder.WithPredicates(secretTenantPredicate()),
		).
		Complete(r)
}

func secretTenantPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		if obj.GetNamespace() != storageConfigNamespace() {
			return false
		}
		_, exists := obj.GetLabels()[osacTenantKey]
		return exists
	})
}
