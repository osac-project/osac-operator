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

	ovnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
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

	"github.com/osac/osac-operator/api/v1alpha1"
)

// TenantReconciler reconciles a Tenant object
type TenantReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	tenantNamespace string
}

// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=userdefinednetworks,verbs=get;list;watch;create;update;patch;delete

func NewTenantReconciler(client client.Client, scheme *runtime.Scheme, tenantNamespace string) *TenantReconciler {
	return &TenantReconciler{
		Client:          client,
		Scheme:          scheme,
		tenantNamespace: tenantNamespace,
	}
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	instance := &v1alpha1.Tenant{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
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
		log.Info("status requires update", "old", *oldstatus, "new", instance.Status)
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	log.Info("end reconcile")
	return res, err
}

// handleUpdate handles creation and update operations for Tenant
func (r *TenantReconciler) handleUpdate(ctx context.Context, req ctrl.Request, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	log.Info("handling update for Tenant", "name", instance.GetName(), "tenant", instance.Spec.Name)

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(instance, tenantFinalizer) {
		controllerutil.AddFinalizer(instance, tenantFinalizer)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Set phase to Progressing
	instance.Status.Phase = v1alpha1.TenantPhaseProgressing

	// Create namespace
	if err := r.createOrUpdateTenantNamespace(ctx, instance); err != nil {
		instance.Status.Phase = v1alpha1.TenantPhaseFailed
		return ctrl.Result{}, err
	}

	// Create UDN
	if err := r.createOrUpdateTenantUDN(ctx, instance); err != nil {
		instance.Status.Phase = v1alpha1.TenantPhaseFailed
		return ctrl.Result{}, err
	}

	// Update status to Ready if everything succeeded
	instance.Status.Phase = v1alpha1.TenantPhaseReady

	return ctrl.Result{}, nil
}

// handleDelete handles deletion operations for Tenant
func (r *TenantReconciler) handleDelete(ctx context.Context, req ctrl.Request, instance *v1alpha1.Tenant) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("handling delete for Tenant", "name", instance.Name)

	// Set deleting phase
	instance.Status.Phase = v1alpha1.TenantPhaseDeleting

	if !controllerutil.ContainsFinalizer(instance, tenantFinalizer) {
		return ctrl.Result{}, nil
	}

	// Delete UDN first
	udn, err := r.deleteTenantUDN(ctx, instance)
	if err != nil || udn != nil {
		return ctrl.Result{}, err
	}

	// Delete namespace
	ns, err := r.deleteTenantNamespace(ctx, instance)
	if err != nil || ns != nil {
		return ctrl.Result{}, err
	}

	// All resources deleted, remove finalizer
	if controllerutil.RemoveFinalizer(instance, tenantFinalizer) {
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	log.Info("Tenant deletion completed", "name", instance.Name)
	return ctrl.Result{}, nil
}

// mapObjectToTenant maps an event for a watched UDN to the associated Tenant resource.
func (r *TenantReconciler) mapObjectToTenant(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	tenantName, exists := obj.GetLabels()[cloudkitTenantRefLabel]
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
func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Predicate to filter resources with tenant label
	tenantLabelPredicate, err := predicate.LabelSelectorPredicate(tenantLabelSelector(r.tenantNamespace))
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Tenant{}, builder.WithPredicates(tenantNamespacePredicate(r.tenantNamespace))).
		Named("tenant").
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.mapObjectToTenant),
			builder.WithPredicates(tenantLabelPredicate),
		).
		Watches(
			&ovnv1.UserDefinedNetwork{},
			handler.EnqueueRequestsFromMapFunc(r.mapObjectToTenant),
			builder.WithPredicates(tenantLabelPredicate),
		).
		Complete(r)
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
				Key:      cloudkitTenantRefLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
			{
				Key:      cloudkitProjectRefLabel,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{project},
			},
		},
	}
}
