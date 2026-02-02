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

	"github.com/innabox/cloudkit-operator/api/v1alpha1"
)

// NewHostComponentFn is the type of a function that creates a required component
type NewHostComponentFn func(context.Context, *v1alpha1.Host) (*appResource, error)

type hostComponent struct {
	name string
	fn   NewHostComponentFn
}

func (r *HostReconciler) hostComponents() []hostComponent {
	return []hostComponent{
		{"Namespace", r.newHostNamespace},
	}
}

// HostReconciler reconciles a Host object
type HostReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	CreateHostWebhook string
	DeleteHostWebhook string
	HostNamespace     string
	webhookClient     *WebhookClient
}

func NewHostReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	createHostWebhook string,
	deleteHostWebhook string,
	hostNamespace string,
	minimumRequestInterval time.Duration,
) *HostReconciler {

	if hostNamespace == "" {
		hostNamespace = defaultHostNamespace
	}

	return &HostReconciler{
		Client:            client,
		Scheme:            scheme,
		CreateHostWebhook: createHostWebhook,
		DeleteHostWebhook: deleteHostWebhook,
		HostNamespace:     hostNamespace,
		webhookClient:     NewWebhookClient(10*time.Second, minimumRequestInterval),
	}
}

// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=hosts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=hosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=hosts/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *HostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	instance := &v1alpha1.Host{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	val, exists := instance.Annotations[cloudkitHostManagementStateAnnotation]
	if exists && val == ManagementStateUnmanaged {
		log.Info("ignoring Host due to management-state annotation", "management-state", val)
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

func HostNamespacePredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(
		func(obj client.Object) bool {
			return obj.GetNamespace() == namespace
		},
	)
}

// SetupWithManager sets up the controller with the Manager.
func (r *HostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	labelPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      cloudkitHostNameLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Host{}, builder.WithPredicates(HostNamespacePredicate(r.HostNamespace))).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.mapObjectToHost),
			builder.WithPredicates(labelPredicate),
		).
		Complete(r)
}

// mapObjectToHost maps an event for a watched object to the associated
// Host resource.
func (r *HostReconciler) mapObjectToHost(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	hostName, exists := obj.GetLabels()[cloudkitHostNameLabel]
	if !exists {
		return nil
	}

	// Verify that the referenced Host exists in this controller's namespace
	// to filter out notifications for resources managed by other controller instances
	host := &v1alpha1.Host{}
	key := client.ObjectKey{
		Name:      hostName,
		Namespace: r.HostNamespace,
	}
	if err := r.Get(ctx, key, host); err != nil {
		log.Error(err, "unable to find referenced Host", "name", hostName)
		return nil
	}

	log.Info("mapping object to Host", "host", hostName)
	return []reconcile.Request{
		{
			NamespacedName: key,
		},
	}
}

// handleUpdate handles creation and update operations for Host
func (r *HostReconciler) handleUpdate(ctx context.Context, req ctrl.Request, instance *v1alpha1.Host) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	log.Info("handling update for Host", "name", instance.Name)

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(instance, hostFinalizer) {
		controllerutil.AddFinalizer(instance, hostFinalizer)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Set initial conditions
	if len(instance.Status.Conditions) == 0 {
		instance.SetStatusCondition(v1alpha1.HostConditionAccepted, metav1.ConditionTrue, "HostAccepted", "Host has been accepted for processing")
		instance.SetStatusCondition(v1alpha1.HostConditionProgressing, metav1.ConditionTrue, "HostProgressing", "Host is being processed")
		instance.Status.Phase = v1alpha1.HostPhaseProgressing
		instance.Status.State = v1alpha1.HostStateProgressing
	}

	// Create required components
	for _, comp := range r.hostComponents() {
		resource, err := comp.fn(ctx, instance)
		if err != nil {
			instance.SetStatusCondition(v1alpha1.HostConditionProgressing, metav1.ConditionFalse, "ComponentCreationFailed", fmt.Sprintf("Failed to create %s: %v", comp.name, err))
			instance.Status.Phase = v1alpha1.HostPhaseFailed
			return ctrl.Result{}, err
		}

		result, err := controllerutil.CreateOrUpdate(ctx, r.Client, resource.object, resource.mutateFn)
		if err != nil {
			instance.SetStatusCondition(v1alpha1.HostConditionProgressing, metav1.ConditionFalse, "ComponentUpdateFailed", fmt.Sprintf("Failed to update %s: %v", comp.name, err))
			instance.Status.Phase = v1alpha1.HostPhaseFailed
			return ctrl.Result{}, err
		}

		log.Info("component operation completed", "component", comp.name, "result", result)
	}

	// Call webhook to create/update host resources
	if r.CreateHostWebhook != "" {
		_, err := r.webhookClient.TriggerWebhook(ctx, r.CreateHostWebhook, instance)
		if err != nil {
			instance.SetStatusCondition(v1alpha1.HostConditionProgressing, metav1.ConditionFalse, "WebhookFailed", fmt.Sprintf("Webhook call failed: %v", err))
			instance.Status.Phase = v1alpha1.HostPhaseFailed
			return ctrl.Result{}, err
		}
	}

	// Update status to Ready if everything succeeded
	instance.SetStatusCondition(v1alpha1.HostConditionProgressing, metav1.ConditionFalse, "HostReady", "Host is ready")
	instance.SetStatusCondition(v1alpha1.HostConditionReady, metav1.ConditionTrue, "HostReady", "Host is ready to use")
	instance.SetStatusCondition(v1alpha1.HostConditionAvailable, metav1.ConditionTrue, "HostAvailable", "Host is available")
	instance.Status.Phase = v1alpha1.HostPhaseReady
	instance.Status.State = v1alpha1.HostStateReady

	// Update power state if not set
	if instance.Status.PowerState == "" {
		instance.Status.PowerState = instance.Spec.PowerState
	}

	return ctrl.Result{}, nil
}

// handleDelete handles deletion operations for Host
func (r *HostReconciler) handleDelete(ctx context.Context, req ctrl.Request, instance *v1alpha1.Host) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	log.Info("handling delete for Host", "name", instance.Name)

	// Set deleting condition
	instance.SetStatusCondition(v1alpha1.HostConditionDeleting, metav1.ConditionTrue, "HostDeleting", "Host is being deleted")
	instance.Status.Phase = v1alpha1.HostPhaseDeleting

	// Call webhook to delete host resources
	if r.DeleteHostWebhook != "" {
		_, err := r.webhookClient.TriggerWebhook(ctx, r.DeleteHostWebhook, instance)
		if err != nil {
			instance.SetStatusCondition(v1alpha1.HostConditionDeleting, metav1.ConditionFalse, "WebhookDeleteFailed", fmt.Sprintf("Webhook delete call failed: %v", err))
			return ctrl.Result{}, err
		}
	}

	// Remove our finalizer to allow deletion
	controllerutil.RemoveFinalizer(instance, hostFinalizer)
	if err := r.Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Host deletion completed", "name", instance.Name)
	return ctrl.Result{}, nil
}
