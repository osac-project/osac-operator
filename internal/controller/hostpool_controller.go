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
	"time"

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
	Scheme                *runtime.Scheme
	CreateHostPoolWebhook string
	DeleteHostPoolWebhook string
	HostPoolNamespace     string
	webhookClient         *WebhookClient
}

func NewHostPoolReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	createHostPoolWebhook string,
	deleteHostPoolWebhook string,
	hostPoolNamespace string,
	minimumRequestInterval time.Duration,
) *HostPoolReconciler {

	if hostPoolNamespace == "" {
		hostPoolNamespace = defaultHostPoolNamespace
	}

	return &HostPoolReconciler{
		Client:                client,
		Scheme:                scheme,
		CreateHostPoolWebhook: createHostPoolWebhook,
		DeleteHostPoolWebhook: deleteHostPoolWebhook,
		HostPoolNamespace:     hostPoolNamespace,
		webhookClient:         NewWebhookClient(10*time.Second, minimumRequestInterval),
	}
}

// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=hostpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=hostpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=hostpools/finalizers,verbs=update
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

	val, exists := instance.Annotations[cloudkitHostPoolManagementStateAnnotation]
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
func (r *HostPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	labelPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      cloudkitHostPoolNameLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
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

	hostPoolName, exists := obj.GetLabels()[cloudkitHostPoolNameLabel]
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
func (r *HostPoolReconciler) handleUpdate(ctx context.Context, req ctrl.Request, instance *v1alpha1.HostPool) (ctrl.Result, error) {
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

	// Call webhook to create/update host pool resources
	if r.CreateHostPoolWebhook != "" {
		_, err := r.webhookClient.TriggerWebhook(ctx, r.CreateHostPoolWebhook, instance)
		if err != nil {
			instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionFalse, "WebhookFailed", fmt.Sprintf("Webhook call failed: %v", err))
			instance.Status.Phase = v1alpha1.HostPoolPhaseFailed
			return ctrl.Result{}, err
		}
	}

	// Update status to Ready if everything succeeded
	instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionFalse, "HostPoolReady", "HostPool is ready")
	instance.SetCondition(v1alpha1.HostPoolConditionAvailable, metav1.ConditionTrue, "HostPoolAvailable", "HostPool is available")
	instance.Status.Phase = v1alpha1.HostPoolPhaseReady

	// Update status with host sets info
	instance.Status.HostSets = instance.Spec.HostSets

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
func (r *HostPoolReconciler) handleDelete(ctx context.Context, req ctrl.Request, instance *v1alpha1.HostPool) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("handling delete for HostPool", "name", instance.Name)

	// Set deleting condition
	instance.SetCondition(v1alpha1.HostPoolConditionDeleting, metav1.ConditionTrue, "HostPoolDeleting", "HostPool is being deleted")
	instance.Status.Phase = v1alpha1.HostPoolPhaseDeleting

	if !controllerutil.ContainsFinalizer(instance, hostPoolFinalizer) {
		return ctrl.Result{}, nil
	}

	ns, err := r.findNamespace(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	if ns != nil {
		// Call webhook to delete host pool resources
		if url := r.DeleteHostPoolWebhook; url != "" {
			val, exists := instance.Annotations[cloudkitHostPoolManagementStateAnnotation]
			if exists && val == ManagementStateManual {
				log.Info("not triggering delete webhook due to management-state annotation", "url", url, "management-state", val)
			} else {
				remainingTime, err := r.webhookClient.TriggerWebhook(ctx, url, instance)
				if err != nil {
					log.Error(err, "failed to trigger webhook", "url", url, "error", err)
					return ctrl.Result{Requeue: true}, nil
				}

				if remainingTime != 0 {
					return ctrl.Result{RequeueAfter: remainingTime}, nil
				}
			}
		}

		// Delete working namespace
		log.Info("deleting host pool namespace", "namespace", ns.GetName())
		if err := r.Client.Delete(ctx, ns); err != nil {
			log.Error(err, "failed to delete namespace", "namespace", ns.GetName(), "error", err)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
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
