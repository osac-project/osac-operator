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
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mc "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/pkg/provisioning"
)

const (
	// osacPublicIPPoolFinalizer is the finalizer for PublicIPPool resources
	osacPublicIPPoolFinalizer = "osac.openshift.io/publicippool-finalizer"
)

// PublicIPPoolReconciler reconciles PublicIPPool CRs created by the fulfillment-service.
//
// A PublicIPPool defines a range of public IP addresses (CIDRs) that can be allocated
// as individual PublicIP resources. Unlike PublicIP (which inherits its strategy from
// the parent pool), PublicIPPool reads the implementation strategy from its own spec.
//
// The controller adds a finalizer, triggers AAP provisioning/deprovisioning jobs via
// the shared provisioning lifecycle, and transitions phases:
// "" -> Progressing -> Ready/Failed; on delete: Deleting.
type PublicIPPoolReconciler struct {
	client.Client
	APIReader            client.Reader
	Scheme               *runtime.Scheme
	mgr                  mcmanager.Manager
	NetworkingNamespace  string
	ProvisioningProvider provisioning.ProvisioningProvider
	StatusPollInterval   time.Duration
	MaxJobHistory        int
	targetCluster        mc.ClusterName
}

// NewPublicIPPoolReconciler creates a new reconciler for PublicIPPool resources.
func NewPublicIPPoolReconciler(
	mgr mcmanager.Manager,
	networkingNamespace string,
	provisioningProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
	targetCluster mc.ClusterName,
) *PublicIPPoolReconciler {
	if mgr == nil {
		panic("mgr must not be nil")
	}
	if statusPollInterval <= 0 {
		statusPollInterval = provisioning.DefaultStatusPollInterval
	}
	if maxJobHistory <= 0 {
		maxJobHistory = provisioning.DefaultMaxJobHistory
	}
	return &PublicIPPoolReconciler{
		Client:               mgr.GetLocalManager().GetClient(),
		APIReader:            mgr.GetLocalManager().GetAPIReader(),
		Scheme:               mgr.GetLocalManager().GetScheme(),
		mgr:                  mgr,
		NetworkingNamespace:  networkingNamespace,
		ProvisioningProvider: provisioningProvider,
		StatusPollInterval:   statusPollInterval,
		MaxJobHistory:        maxJobHistory,
		targetCluster:        targetCluster,
	}
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicippools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicippools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicippools/finalizers,verbs=update

// Reconcile handles create/update/delete for a PublicIPPool CR.
// On create/update it ensures a finalizer, reads the implementation strategy from spec,
// and runs provisioning. On delete it triggers deprovisioning and removes the finalizer.
func (r *PublicIPPoolReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	pool := &v1alpha1.PublicIPPool{}
	err := r.Get(ctx, req.NamespacedName, pool)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	val, exists := pool.Annotations[osacManagementStateAnnotation]
	if pool.ObjectMeta.DeletionTimestamp.IsZero() && exists && val == ManagementStateUnmanaged {
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
		if err := r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(pool), pool.Status); err != nil {
			return res, err
		}
	}

	log.Info("end reconcile")
	return res, err
}

// handleUpdate processes a non-deleted PublicIPPool: adds finalizer, reads the
// implementation strategy from the pool's own spec, and runs provisioning.
func (r *PublicIPPoolReconciler) handleUpdate(ctx context.Context, pool *v1alpha1.PublicIPPool) (ctrl.Result, error) {
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

	// Read implementation strategy from spec
	implementationStrategy := pool.Spec.ImplementationStrategy
	if implementationStrategy == "" {
		implementationStrategy = defaultPublicIPPoolImplementationStrategy
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
		Reason:             conditionReasonConfigurationApplied,
		Message:            conditionMessageConfigurationApplied,
		LastTransitionTime: metav1.Now(),
	})

	// Set phase to Progressing on first provision (empty phase) or when spec changed
	// after a previous success. Don't override Failed during backoff.
	if pool.Status.Phase == "" || (pool.Status.Phase == v1alpha1.PublicIPPoolPhaseReady &&
		!provisioning.IsConfigApplied(&pool.Status.ProvisioningJobs, pool.Status.DesiredConfigVersion)) {
		pool.Status.Phase = v1alpha1.PublicIPPoolPhaseProgressing
	}

	// Handle provisioning
	return r.handleProvisioning(ctx, pool)
}

// handleDelete sets the Deleting phase, runs deprovisioning, and removes the finalizer
// once deprovisioning completes (or is skipped).
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

// handleProvisioning delegates to the shared provisioning lifecycle, which triggers
// an AAP job (e.g., osac-create-public-ip-pool) and polls its status until completion.
func (r *PublicIPPoolReconciler) handleProvisioning(ctx context.Context, pool *v1alpha1.PublicIPPool) (ctrl.Result, error) {
	if r.ProvisioningProvider == nil {
		ctrllog.FromContext(ctx).Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	return provisioning.RunProvisioningLifecycle(ctx, r.ProvisioningProvider, pool,
		&provisioning.State{Jobs: &pool.Status.ProvisioningJobs, DesiredConfigVersion: pool.Status.DesiredConfigVersion},
		r.MaxJobHistory, r.StatusPollInterval,
		&provisioning.PollCallbacks{
			OnFailed:  func(_ string) { pool.Status.Phase = v1alpha1.PublicIPPoolPhaseFailed },
			OnSuccess: func(_ provisioning.ProvisionStatus) { pool.Status.Phase = v1alpha1.PublicIPPoolPhaseReady },
		},
		func() bool {
			return provisioning.CheckAPIServerForNonTerminalProvisionJob(
				ctx, r.APIReader, client.ObjectKeyFromObject(pool), &v1alpha1.PublicIPPool{}, func(obj client.Object) []v1alpha1.JobStatus {
					return obj.(*v1alpha1.PublicIPPool).Status.ProvisioningJobs
				})
		},
		func() error {
			return r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(pool), pool.Status)
		},
	)
}

// handleDeprovisioning triggers an AAP deprovisioning job (e.g., osac-delete-public-ip-pool)
// and polls its status. On failure, it either blocks deletion (to prevent orphaned
// resources) or allows the process to continue, depending on provider policy.
func (r *PublicIPPoolReconciler) handleDeprovisioning(ctx context.Context, pool *v1alpha1.PublicIPPool) (ctrl.Result, error) {
	if r.ProvisioningProvider == nil {
		ctrllog.FromContext(ctx).Info("no provisioning provider configured, skipping deprovisioning")
		return ctrl.Result{}, nil
	}
	result, done, err := provisioning.RunDeprovisioningLifecycle(ctx, r.ProvisioningProvider, pool,
		&pool.Status.ProvisioningJobs, r.MaxJobHistory, r.StatusPollInterval)
	if err != nil || !done {
		return result, err
	}
	return ctrl.Result{}, nil
}

// updateStatusWithRetry updates the public IP pool status with retry on conflict.
func (r *PublicIPPoolReconciler) updateStatusWithRetry(ctx context.Context, key client.ObjectKey, newStatus v1alpha1.PublicIPPoolStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &v1alpha1.PublicIPPool{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		latest.Status = newStatus
		return r.Status().Update(ctx, latest)
	})
}

// SetupWithManager registers this controller with the multicluster manager.
// It watches PublicIPPool CRs in the networking namespace on the local cluster only.
func (r *PublicIPPoolReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return mcbuilder.ControllerManagedBy(mgr).
		For(&v1alpha1.PublicIPPool{},
			mcbuilder.WithPredicates(NetworkingNamespacePredicate(r.NetworkingNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Complete(r)
}
