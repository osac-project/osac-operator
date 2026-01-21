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
	kubevirtv1 "kubevirt.io/api/core/v1"
)

// ComputeInstanceReconciler reconciles a ComputeInstance object
type ComputeInstanceReconciler struct {
	client.Client
	Scheme                       *runtime.Scheme
	CreateComputeInstanceWebhook string
	DeleteComputeInstanceWebhook string
	ComputeInstanceNamespace     string
	webhookClient                *WebhookClient
}

func NewComputeInstanceReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	createVMWebhook string,
	deleteVMWebhook string,
	computeInstanceNamespace string,
	minimumRequestInterval time.Duration,
) *ComputeInstanceReconciler {

	if computeInstanceNamespace == "" {
		computeInstanceNamespace = defaultComputeInstanceNamespace
	}

	return &ComputeInstanceReconciler{
		Client:                       client,
		Scheme:                       scheme,
		CreateComputeInstanceWebhook: createVMWebhook,
		DeleteComputeInstanceWebhook: deleteVMWebhook,
		ComputeInstanceNamespace:     computeInstanceNamespace,
		webhookClient:                NewWebhookClient(10*time.Second, minimumRequestInterval),
	}
}

// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=computeinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=computeinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloudkit.openshift.io,resources=computeinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=computeinstances,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ComputeInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	instance := &v1alpha1.ComputeInstance{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	val, exists := instance.Annotations[cloudkitComputeInstanceManagementStateAnnotation]
	if exists && val == ManagementStateUnmanaged {
		log.Info("ignoring ComputeInstance due to management-state annotation", "management-state", val)
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

	if !equality.Semantic.DeepEqual(instance.Status, *oldstatus) {
		log.Info("status requires update")
		if err := r.Status().Update(ctx, instance); err != nil {
			return res, err
		}
	}

	log.Info("end reconcile")
	return res, err
}

func ComputeInstanceNamespacePredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(
		func(obj client.Object) bool {
			return obj.GetNamespace() == namespace
		},
	)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ComputeInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	labelPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      cloudkitComputeInstanceNameLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ComputeInstance{}, builder.WithPredicates(ComputeInstanceNamespacePredicate(r.ComputeInstanceNamespace))).
		Watches(
			&kubevirtv1.VirtualMachine{},
			handler.EnqueueRequestsFromMapFunc(r.mapObjectToComputeInstance),
			builder.WithPredicates(labelPredicate),
		).
		Watches(
			&v1alpha1.Tenant{},
			handler.EnqueueRequestForOwner(
				mgr.GetScheme(),
				mgr.GetRESTMapper(),
				&v1alpha1.ComputeInstance{},
			),
			builder.WithPredicates(ComputeInstanceNamespacePredicate(r.ComputeInstanceNamespace)),
		).
		Complete(r)
}

// mapObjectToComputeInstance maps an event for a watched object to the associated
// ComputeInstance resource.
func (r *ComputeInstanceReconciler) mapObjectToComputeInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	computeInstanceName, exists := obj.GetLabels()[cloudkitComputeInstanceNameLabel]
	if !exists {
		return nil
	}

	// Verify that the referenced ComputeInstance exists in this controller's namespace
	// to filter out notifications for resources managed by other controller instances
	computeInstance := &v1alpha1.ComputeInstance{}
	key := client.ObjectKey{
		Name:      computeInstanceName,
		Namespace: r.ComputeInstanceNamespace,
	}
	if err := r.Get(ctx, key, computeInstance); err != nil {
		// ComputeInstance doesn't exist in our namespace, ignore this notification
		log.V(2).Info("ignoring notification for resource not managed by this controller instance",
			"kind", obj.GetObjectKind().GroupVersionKind().Kind,
			"namespace", obj.GetNamespace(),
			"name", obj.GetName(),
			"computeinstance", computeInstanceName,
			"controller_namespace", r.ComputeInstanceNamespace,
		)
		return nil
	}

	log.Info("mapped change notification",
		"kind", obj.GetObjectKind().GroupVersionKind().Kind,
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
		"computeinstance", computeInstanceName,
	)

	return []reconcile.Request{
		{
			NamespacedName: key,
		},
	}
}

func (r *ComputeInstanceReconciler) handleUpdate(ctx context.Context, _ ctrl.Request, instance *v1alpha1.ComputeInstance) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	r.initializeStatusConditions(instance)
	instance.Status.Phase = v1alpha1.ComputeInstancePhaseProgressing

	if controllerutil.AddFinalizer(instance, cloudkitComputeInstanceFinalizer) {
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Get the tenant
	tenant, err := r.getTenant(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	// If the tenant doesn't exist, create it and requeue
	if tenant == nil {
		return ctrl.Result{}, r.createOrUpdateTenant(ctx, instance)
	}

	// If the tenant is not ready, requeue
	if tenant.Status.Phase != v1alpha1.TenantPhaseReady {
		log.Info("tenant is not ready, requeueing", "tenant", tenant.GetName())
		return ctrl.Result{}, nil
	}

	instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionAccepted, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)

	kv, err := r.findKubeVirtVMs(ctx, instance, tenant.Status.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	if kv != nil {
		if err := r.handleKubeVirtVM(ctx, instance, kv); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Handle restart request
	if result, err := r.handleRestartRequest(ctx, instance); err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	if err := r.handleDesiredConfigVersion(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.handleReconciledConfigVersion(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	if instance.Status.DesiredConfigVersion == instance.Status.ReconciledConfigVersion {
		instance.Status.Phase = v1alpha1.ComputeInstancePhaseReady
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionProgressing, metav1.ConditionFalse, "", v1alpha1.ReasonAsExpected)
		r.webhookClient.ResetCache()
		return ctrl.Result{}, nil
	}

	instance.Status.Phase = v1alpha1.ComputeInstancePhaseProgressing
	instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionProgressing, metav1.ConditionTrue, "Applying configuration", v1alpha1.ReasonAsExpected)

	if url := r.CreateComputeInstanceWebhook; url != "" {
		val, exists := instance.Annotations[cloudkitComputeInstanceManagementStateAnnotation]
		if exists && val == ManagementStateManual {
			log.Info("not triggering create webhook due to management-state annotation", "url", url, "management-state", val)
		} else {
			remainingTime, err := r.webhookClient.TriggerWebhook(ctx, url, instance)
			if err != nil {
				log.Error(err, "failed to trigger webhook", "url", url, "error", err)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}

			// Verify if we are within the minimum request window
			if remainingTime != 0 {
				log.Info("request is within minimum request window", "url", url)
				return ctrl.Result{RequeueAfter: remainingTime}, nil
			}
		}
	}

	return ctrl.Result{}, nil
}

func (r *ComputeInstanceReconciler) handleDelete(ctx context.Context, _ ctrl.Request, instance *v1alpha1.ComputeInstance) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("deleting compute instance")

	instance.Status.Phase = v1alpha1.ComputeInstancePhaseDeleting

	// Finalizer has already been removed, return
	if !controllerutil.ContainsFinalizer(instance, cloudkitComputeInstanceFinalizer) {
		return ctrl.Result{}, nil
	}

	// AAP Finalizer is here, trigger deletion webhook
	if controllerutil.ContainsFinalizer(instance, cloudkitAAPComputeInstanceFinalizer) {
		if url := r.DeleteComputeInstanceWebhook; url != "" {
			val, exists := instance.Annotations[cloudkitComputeInstanceManagementStateAnnotation]
			if exists && val == ManagementStateManual {
				log.Info("not triggering delete webhook due to management-state annotation", "url", url, "management-state", val)
			} else {
				remainingTime, err := r.webhookClient.TriggerWebhook(ctx, url, instance)
				if err != nil {
					log.Error(err, "failed to trigger webhook", "url", url, "error", err)
					return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
				}

				if remainingTime != 0 {
					return ctrl.Result{RequeueAfter: remainingTime}, nil
				}
			}
		}
		return ctrl.Result{}, nil
	}

	// Let this CR be deleted
	if controllerutil.RemoveFinalizer(instance, cloudkitComputeInstanceFinalizer) {
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// initializeStatusConditions initializes the conditions that haven't already been initialized.
func (r *ComputeInstanceReconciler) initializeStatusConditions(instance *v1alpha1.ComputeInstance) {
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionAccepted,
		metav1.ConditionTrue,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionDeleting,
		metav1.ConditionFalse,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionProgressing,
		metav1.ConditionTrue,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ComputeInstanceConditionAvailable,
		metav1.ConditionFalse,
		v1alpha1.ReasonInitialized,
	)
}

// initializeStatusCondition initializes a condition, but only it is not already initialized.
func (r *ComputeInstanceReconciler) initializeStatusCondition(instance *v1alpha1.ComputeInstance,
	conditionType v1alpha1.ComputeInstanceConditionType, status metav1.ConditionStatus, reason string) {
	if instance.Status.Conditions == nil {
		instance.Status.Conditions = []metav1.Condition{}
	}
	condition := instance.GetStatusCondition(conditionType)
	if condition != nil {
		return
	}
	instance.SetStatusCondition(conditionType, status, "", reason)
}

func (r *ComputeInstanceReconciler) findKubeVirtVMs(ctx context.Context, instance *v1alpha1.ComputeInstance, nsName string) (*kubevirtv1.VirtualMachine, error) {
	log := ctrllog.FromContext(ctx)

	var kubeVirtVMList kubevirtv1.VirtualMachineList
	if err := r.List(ctx, &kubeVirtVMList, client.InNamespace(nsName), labelSelectorFromComputeInstanceInstance(instance)); err != nil {
		log.Error(err, "failed to list KubeVirt VMs")
		return nil, err
	}

	if len(kubeVirtVMList.Items) > 1 {
		return nil, fmt.Errorf("found too many (%d) matching KubeVirt VMs for %s", len(kubeVirtVMList.Items), instance.GetName())
	}

	if len(kubeVirtVMList.Items) == 0 {
		return nil, nil
	}

	return &kubeVirtVMList.Items[0], nil
}

func (r *ComputeInstanceReconciler) handleKubeVirtVM(ctx context.Context, instance *v1alpha1.ComputeInstance,
	kv *kubevirtv1.VirtualMachine) error {
	log := ctrllog.FromContext(ctx)

	name := kv.GetName()
	instance.SetVirtualMachineReferenceKubeVirtVirtualMachineName(name)
	instance.SetVirtualMachineReferenceNamespace(kv.GetNamespace())
	instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionAccepted, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)

	if kvVMHasConditionWithStatus(kv, kubevirtv1.VirtualMachineReady, corev1.ConditionTrue) {
		log.Info("KubeVirt virtual machine (kubevirt resource) is ready", "computeinstance", instance.GetName())
		instance.SetStatusCondition(v1alpha1.ComputeInstanceConditionAvailable, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)
	}

	return nil
}

func kvVMGetCondition(vm *kubevirtv1.VirtualMachine, cond kubevirtv1.VirtualMachineConditionType) *kubevirtv1.VirtualMachineCondition {
	if vm == nil {
		return nil
	}
	for _, c := range vm.Status.Conditions {
		if c.Type == cond {
			return &c
		}
	}
	return nil
}

func kvVMHasConditionWithStatus(vm *kubevirtv1.VirtualMachine, cond kubevirtv1.VirtualMachineConditionType, status corev1.ConditionStatus) bool {
	c := kvVMGetCondition(vm, cond)
	return c != nil && c.Status == status
}

// handleDesiredConfigVersion computes a version (hash) of the spec (using FNV-1a) and stores it as hexadecimal in status.DesiredConfigVersion.
// The hashing is idempotent - the same spec will always produce the same version.
func (r *ComputeInstanceReconciler) handleDesiredConfigVersion(ctx context.Context, instance *v1alpha1.ComputeInstance) error {
	// Hash the spec using FNV-1a for idempotent hashing
	specJSON, err := json.Marshal(instance.Spec)
	if err != nil {
		return fmt.Errorf("failed to marshal spec to JSON: %w", err)
	}

	hasher := fnv.New64a()
	if _, err := hasher.Write(specJSON); err != nil {
		return fmt.Errorf("failed to write to hash: %w", err)
	}

	hashBytes := hasher.Sum(nil)
	desiredConfigVersion := hex.EncodeToString(hashBytes)
	instance.Status.DesiredConfigVersion = desiredConfigVersion

	return nil
}

// handleReconciledConfigVersion copies the annotation cloudkitAAPReconciledConfigVersionAnnotation to status.ReconciledConfigVersion.
// If the annotation doesn't exist, it clears status.ReconciledConfigVersion.
func (r *ComputeInstanceReconciler) handleReconciledConfigVersion(ctx context.Context, instance *v1alpha1.ComputeInstance) error {
	log := ctrllog.FromContext(ctx)

	// Copy the reconciled config version from annotation if it exists
	if version, exists := instance.Annotations[cloudkitAAPReconciledConfigVersionAnnotation]; exists {
		instance.Status.ReconciledConfigVersion = version
		log.V(1).Info("copied reconciled config version from annotation", "version", version)
	} else {
		// Clear the reconciled config version if annotation doesn't exist
		instance.Status.ReconciledConfigVersion = ""
	}

	return nil
}
