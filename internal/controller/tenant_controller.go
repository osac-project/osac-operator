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
	"strings"

	ovnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mc "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/osac-project/osac-operator/api/v1alpha1"
)

// TenantReconciler reconciles a Tenant object
type TenantReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Recorder        events.EventRecorder
	tenantNamespace string
	mgr             mcmanager.Manager
	targetCluster   mc.ClusterName
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=userdefinednetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

func NewTenantReconciler(mgr mcmanager.Manager, tenantNamespace string, targetCluster mc.ClusterName) *TenantReconciler {
	return &TenantReconciler{
		Client:          mgr.GetLocalManager().GetClient(),
		Scheme:          mgr.GetLocalManager().GetScheme(),
		Recorder:        mgr.GetLocalManager().GetEventRecorder(tenantControllerName),
		tenantNamespace: tenantNamespace,
		mgr:             mgr,
		targetCluster:   targetCluster,
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

	// Reset all status fields to their Progressing defaults. They are only set to
	// meaningful values if ALL prerequisites are satisfied and the phase advances to
	// Ready. Any early return below leaves the status in a clean Progressing state.
	instance.Status.Phase = v1alpha1.TenantPhaseProgressing
	instance.Status.Namespace = ""
	instance.Status.StorageClass = ""

	// Get target cluster client where namespace, StorageClass, and UDN are reconciled
	targetClient, err := r.getTargetClient(ctx)
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

	// Prerequisite 2: valid StorageClass (tenant-specific or shared Default)
	scResult, err := r.getTenantStorageClass(ctx, targetClient, instance.GetName())
	if err != nil {
		return ctrl.Result{}, err
	}

	if scResult.name == "" {
		instance.SetStatusCondition(v1alpha1.TenantConditionStorageClassReady,
			metav1.ConditionFalse,
			scResult.reason,
			scResult.message)
		if scResult.reason == v1alpha1.TenantReasonMultipleFound || scResult.reason == v1alpha1.TenantReasonMultipleDefaultsFound {
			r.Recorder.Eventf(instance, nil, corev1.EventTypeWarning, eventReasonDuplicateStorageClass, eventActionDetectDuplicate, "%s", scResult.message)
		}
		return ctrl.Result{}, nil
	}

	instance.SetStatusCondition(v1alpha1.TenantConditionStorageClassReady,
		metav1.ConditionTrue,
		scResult.reason,
		scResult.message)

	instance.Status.Namespace = namespace.GetName()
	instance.Status.StorageClass = scResult.name
	instance.Status.Phase = v1alpha1.TenantPhaseReady

	return ctrl.Result{}, nil
}

// storageClassResult holds the outcome of a StorageClass lookup, including
// the resolved name (empty if none found) and the reason/message for the
// condition that should be set on the Tenant.
type storageClassResult struct {
	name    string
	reason  string
	message string
}

// joinStorageClassNames returns StorageClass metadata names as a comma-separated string for
// messages and the same values as a slice for structured logging.
func joinStorageClassNames(items []storagev1.StorageClass) (joined string, names []string) {
	names = make([]string, len(items))
	for i := range items {
		names[i] = items[i].GetName()
	}
	return strings.Join(names, ", "), names
}

// getTenantStorageClass looks up the StorageClass for a tenant using a two-step
// fallback: tenant-specific SC first, then shared Default SC. Returns a
// storageClassResult with the resolved name and condition metadata. A non-nil
// error is only returned for API call failures.
func (r *TenantReconciler) getTenantStorageClass(ctx context.Context, targetClient client.Client, tenantName string) (storageClassResult, error) {
	log := ctrllog.FromContext(ctx)

	// Step 1 — Tenant-specific StorageClasses (label osac.openshift.io/tenant=<tenantName>).
	// Exactly one → use it. More than one → error (do not consider Default SC). Zero → Step 2.
	tenantSCList := &storagev1.StorageClassList{}
	if err := targetClient.List(ctx, tenantSCList, client.MatchingLabels{osacTenantAnnotation: tenantName}); err != nil {
		return storageClassResult{}, err
	}

	switch len(tenantSCList.Items) {
	case 1:
		scName := tenantSCList.Items[0].GetName()
		return storageClassResult{
			name:    scName,
			reason:  v1alpha1.TenantReasonFound,
			message: fmt.Sprintf("StorageClass %q found for tenant %q", scName, tenantName),
		}, nil
	case 0:
		// No tenant SCs — evaluate shared Default SCs (Step 2) below.
	default:
		joined, names := joinStorageClassNames(tenantSCList.Items)
		msg := fmt.Sprintf("Multiple StorageClasses found for tenant %q: [%s]. Exactly one is required; remove the extras to resolve.",
			tenantName, joined)
		log.Info(msg, "tenant", tenantName, "storageClasses", names)
		return storageClassResult{
			reason:  v1alpha1.TenantReasonMultipleFound,
			message: msg,
		}, nil
	}

	// Step 2 — Shared Default StorageClasses (label osac.openshift.io/tenant=Default).
	// Only reached when Step 1 found zero tenant-specific SCs.
	defaultSCList := &storagev1.StorageClassList{}
	if err := targetClient.List(ctx, defaultSCList, client.MatchingLabels{osacTenantAnnotation: defaultStorageClassSentinel}); err != nil {
		return storageClassResult{}, err
	}

	switch len(defaultSCList.Items) {
	case 1:
		scName := defaultSCList.Items[0].GetName()
		msg := fmt.Sprintf("No tenant-specific StorageClass found for tenant %q. "+
			"Using shared Default StorageClass %q. Storage is not isolated for this tenant.",
			tenantName, scName)
		log.Info(msg)
		return storageClassResult{
			name:    scName,
			reason:  v1alpha1.TenantReasonSharedDefault,
			message: msg,
		}, nil
	case 0:
		msg := fmt.Sprintf("No StorageClass found for tenant %q (no tenant SC, no Default SC)", tenantName)
		log.Info(msg)
		return storageClassResult{
			reason:  v1alpha1.TenantReasonNotFound,
			message: msg,
		}, nil
	default:
		joined, names := joinStorageClassNames(defaultSCList.Items)
		msg := fmt.Sprintf("Multiple shared Default StorageClasses found: [%s]. Exactly one is required; remove the extras to resolve. Tenant %q has no dedicated StorageClass and is affected.",
			joined, tenantName)
		log.Info(msg, "tenant", tenantName, "storageClasses", names)
		return storageClassResult{
			reason:  v1alpha1.TenantReasonMultipleDefaultsFound,
			message: msg,
		}, nil
	}
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

func (r *TenantReconciler) getTargetClient(ctx context.Context) (client.Client, error) {
	targetCluster, err := r.mgr.GetCluster(ctx, r.targetCluster)
	if err != nil {
		return nil, err
	}
	return targetCluster.GetClient(), nil
}
