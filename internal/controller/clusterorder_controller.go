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

// Package controller implements the controller logic
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
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

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/helpers"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

// NewComponentFn is the type of a function that creates a required component
type NewComponentFn func(context.Context, *v1alpha1.ClusterOrder) (*appResource, error)

type appResource struct {
	object   client.Object
	mutateFn controllerutil.MutateFn
}

type component struct {
	name string
	fn   NewComponentFn
}

func (r *ClusterOrderReconciler) components() []component {
	return []component{
		{"Namespace", r.newNamespace},
		{"ServiceAccount", r.newServiceAccount},
		{"RoleBinding", r.newAdminRoleBinding},
	}
}

// ClusterOrderReconciler reconciles a ClusterOrder object
type ClusterOrderReconciler struct {
	client.Client
	Scheme                *runtime.Scheme
	CreateClusterWebhook  string
	DeleteClusterWebhook  string
	ClusterOrderNamespace string
	webhookClient         *WebhookClient
	ProvisioningProvider  provisioning.ProvisioningProvider
	StatusPollInterval    time.Duration
	MaxJobHistory         int
}

func NewClusterOrderReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	createClusterWebhook string,
	deleteClusterWebhook string,
	clusterOrderNamespace string,
	minimumRequestInterval time.Duration,
	provisioningProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
) *ClusterOrderReconciler {

	if clusterOrderNamespace == "" {
		clusterOrderNamespace = defaultClusterOrderNamespace
	}

	if statusPollInterval == 0 {
		statusPollInterval = 30 * time.Second
	}

	if maxJobHistory <= 0 {
		maxJobHistory = DefaultMaxJobHistory
	}

	return &ClusterOrderReconciler{
		Client:                client,
		Scheme:                scheme,
		CreateClusterWebhook:  createClusterWebhook,
		DeleteClusterWebhook:  deleteClusterWebhook,
		ClusterOrderNamespace: clusterOrderNamespace,
		webhookClient:         NewWebhookClient(10*time.Second, minimumRequestInterval),
		ProvisioningProvider:  provisioningProvider,
		StatusPollInterval:    statusPollInterval,
		MaxJobHistory:         maxJobHistory,
	}
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=clusterorders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=clusterorders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=clusterorders/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedclusters;nodepools,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ClusterOrderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	instance := &v1alpha1.ClusterOrder{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	val, exists := instance.Annotations[osacManagementStateAnnotation]
	if exists && val == ManagementStateUnmanaged {
		log.Info("ignoring ClusterOrder due to management-state annotation", "management-state", val)
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

func NamespacePredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(
		func(obj client.Object) bool {
			return obj.GetNamespace() == namespace
		},
	)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterOrderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	labelPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      osacClusterOrderNameLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ClusterOrder{}, builder.WithPredicates(NamespacePredicate(r.ClusterOrderNamespace))).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.mapObjectToCluster),
			builder.WithPredicates(labelPredicate),
		).
		Watches(
			&corev1.ServiceAccount{},
			handler.EnqueueRequestsFromMapFunc(r.mapObjectToCluster),
			builder.WithPredicates(labelPredicate),
		).
		Watches(
			&rbacv1.RoleBinding{},
			handler.EnqueueRequestsFromMapFunc(r.mapObjectToCluster),
			builder.WithPredicates(labelPredicate),
		).
		Watches(
			&hypershiftv1beta1.HostedCluster{},
			handler.EnqueueRequestsFromMapFunc(r.mapObjectToCluster),
			builder.WithPredicates(labelPredicate),
		).
		Watches(
			&hypershiftv1beta1.NodePool{},
			handler.EnqueueRequestsFromMapFunc(r.mapObjectToCluster),
			builder.WithPredicates(labelPredicate),
		).
		Complete(r)
}

// mapObjectToCluster maps an event for a watched object to the associated
// ClusterOrder resource.
func (r *ClusterOrderReconciler) mapObjectToCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	clusterOrderName, exists := obj.GetLabels()[osacClusterOrderNameLabel]
	if !exists {
		return nil
	}

	// Verify that the referenced ClusterOrder exists in this controller's namespace
	// to filter out notifications for resources managed by other controller instances
	clusterOrder := &v1alpha1.ClusterOrder{}
	key := client.ObjectKey{
		Name:      clusterOrderName,
		Namespace: r.ClusterOrderNamespace,
	}
	if err := r.Get(ctx, key, clusterOrder); err != nil {
		// ClusterOrder doesn't exist in our namespace, ignore this notification
		log.V(2).Info("ignoring notification for resource not managed by this controller instance",
			"kind", obj.GetObjectKind().GroupVersionKind().Kind,
			"namespace", obj.GetNamespace(),
			"name", obj.GetName(),
			"clusterorder", clusterOrderName,
			"controller_namespace", r.ClusterOrderNamespace,
		)
		return nil
	}

	log.Info("mapped change notification",
		"kind", obj.GetObjectKind().GroupVersionKind().Kind,
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
		"clusterorder", clusterOrderName,
	)

	return []reconcile.Request{
		{
			NamespacedName: key,
		},
	}
}

func (r *ClusterOrderReconciler) handleUpdate(ctx context.Context, _ ctrl.Request, instance *v1alpha1.ClusterOrder) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	r.initializeStatusConditions(instance)
	instance.Status.Phase = v1alpha1.ClusterOrderPhaseProgressing

	if controllerutil.AddFinalizer(instance, osacFinalizer) {
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	for _, component := range r.components() {
		log.Info("handling component", "component", component.name)

		resource, err := component.fn(ctx, instance)
		if err != nil {
			log.Error(err, "failed to mutate resource", "component", component.name)
			return ctrl.Result{}, err
		}

		result, err := controllerutil.CreateOrUpdate(ctx, r.Client, resource.object, resource.mutateFn)
		if err != nil {
			log.Error(err, "failed to create or update component", "component", component.name)
			return ctrl.Result{}, err
		}
		switch result {
		case controllerutil.OperationResultCreated:
			log.Info("created component", "component", component.name)
		case controllerutil.OperationResultUpdated:
			log.Info("updated component", "component", component.name)
		}
	}

	instance.SetStatusCondition(v1alpha1.ConditionNamespaceCreated, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)

	// Handle provisioning via provider (hybrid approach: job tracking + HC watching)
	// Non-blocking: returns RequeueAfter for polling without delaying reconciliation
	provisionResult, err := r.handleProvisioning(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	ns, err := r.findNamespace(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	if hc, _ := r.findHostedCluster(ctx, instance, ns.GetName()); hc != nil {
		if err := r.handleHostedCluster(ctx, instance, hc); err != nil {
			return ctrl.Result{}, err
		}
	}

	if url := r.CreateClusterWebhook; url != "" {
		val, exists := instance.Annotations[osacManagementStateAnnotation]
		if exists && val == ManagementStateManual {
			log.Info("not triggering create webhook due to management-state annotation", "url", url, "management-state", val)
		} else {
			remainingTime, err := r.webhookClient.TriggerWebhook(ctx, url, instance)
			if err != nil {
				log.Error(err, "failed to trigger webhook", "url", url, "error", err)
				return ctrl.Result{Requeue: true}, nil
			}

			// Verify if we are within the minimum request window
			if remainingTime != 0 {
				log.Info("request is within minimum request window", "url", url)
				return ctrl.Result{RequeueAfter: remainingTime}, nil
			}
		}
	}

	// If provision job needs polling, requeue for status updates
	if provisionResult.RequeueAfter > 0 {
		return provisionResult, nil
	}

	return ctrl.Result{}, nil
}

func (r *ClusterOrderReconciler) handleHostedCluster(ctx context.Context, instance *v1alpha1.ClusterOrder,
	hc *hypershiftv1beta1.HostedCluster) error {

	log := ctrllog.FromContext(ctx)

	name := hc.GetName()
	instance.SetClusterReferenceHostedClusterName(name)
	instance.SetStatusCondition(v1alpha1.ConditionControlPlaneCreated, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)

	if hostedClusterControlPlaneIsAvailable(hc) {
		log.Info("hosted control plane is available", "clusterorder", instance.GetName())
		instance.SetStatusCondition(v1alpha1.ConditionControlPlaneAvailable, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)

		if hostedClusterIsReady(hc) {
			log.Info("hosted cluster is ready", "clusterorder", instance.GetName())
			instance.SetStatusCondition(v1alpha1.ConditionClusterAvailable, metav1.ConditionTrue, "", v1alpha1.ReasonAsExpected)
			instance.Status.Phase = v1alpha1.ClusterOrderPhaseReady
		}
	}

	// Fetch the node pools and handle them:
	nodePools := &hypershiftv1beta1.NodePoolList{}
	if err := r.List(ctx, nodePools, client.InNamespace(hc.Namespace), labelSelectorFromInstance(instance)); err != nil {
		return err
	}
	if err := r.handleNodePools(ctx, instance, nodePools); err != nil {
		return err
	}
	return nil
}

func (r *ClusterOrderReconciler) handleNodePools(ctx context.Context, instance *v1alpha1.ClusterOrder,
	nodePools *hypershiftv1beta1.NodePoolList) error {
	for i := range len(nodePools.Items) {
		err := r.handleNodePool(ctx, instance, &nodePools.Items[i])
		if err != nil {
			return fmt.Errorf("failed to handle node pool %d: %w", i, err)
		}
	}
	return nil
}

func (r *ClusterOrderReconciler) handleNodePool(ctx context.Context, instance *v1alpha1.ClusterOrder,
	nodePool *hypershiftv1beta1.NodePool) error {
	log := ctrllog.FromContext(ctx)

	// TODO: Currently there is no way to know what is the item of the `nodeRequests` field that corresponds to a
	// node pool. The best we can do is check if there is exactly one, and then assume that this node pool
	// corresponds to that node request.

	log.Info("processing nodepool", "nodepool", nodePool.GetName())
	nodeRequestsCount := len(instance.Spec.NodeRequests)
	if nodeRequestsCount != 1 {
		log.Info(
			"expected exactly one node request, will ignore the node pool",
			"node_pool", nodePool.Name,
			"node_requests", nodeRequestsCount,
		)
		return nil
	}

	// Find the matching item inside the `nodeRequests` field of the status, or create a new one if there is no
	// matching item yet.
	resourceClass := instance.Spec.NodeRequests[0].ResourceClass
	var nodeRequestStatus *v1alpha1.NodeRequest
	for i, nodeRequestsItem := range instance.Status.NodeRequests {
		log.Info("looking for resource class", "want", resourceClass, "have", nodeRequestsItem.ResourceClass)
		if nodeRequestsItem.ResourceClass == resourceClass {
			nodeRequestStatus = &instance.Status.NodeRequests[i]
		}
	}
	if nodeRequestStatus == nil {
		instance.Spec.NodeRequests = append(instance.Spec.NodeRequests, v1alpha1.NodeRequest{
			ResourceClass: resourceClass,
		})
		nodeRequestStatus = &instance.Spec.NodeRequests[len(instance.Spec.NodeRequests)-1]
	}

	// Update the selected `nodeRequests` item:
	oldValue := nodeRequestStatus.NumberOfNodes
	newValue := int(nodePool.Status.Replicas)
	if newValue != oldValue {
		log.Info(
			"updating number of nodes from node pool",
			"node_pool", nodePool.Name,
			"resource_class", resourceClass,
			"old_value", oldValue,
			"new_value", newValue,
		)
		nodeRequestStatus.NumberOfNodes = newValue
	}

	return nil
}

func hostedClusterControlPlaneIsAvailable(hc *hypershiftv1beta1.HostedCluster) bool {
	return (meta.IsStatusConditionTrue(hc.Status.Conditions, "Available") &&
		meta.IsStatusConditionFalse(hc.Status.Conditions, "Degraded"))
}

func hostedClusterIsReady(hc *hypershiftv1beta1.HostedCluster) bool {
	return (meta.IsStatusConditionTrue(hc.Status.Conditions, "ClusterVersionSucceeding") &&
		meta.IsStatusConditionFalse(hc.Status.Conditions, "Degraded"))
}

func (r *ClusterOrderReconciler) findHostedCluster(ctx context.Context, instance *v1alpha1.ClusterOrder, nsName string) (*hypershiftv1beta1.HostedCluster, error) {
	log := ctrllog.FromContext(ctx)

	var hostedClusterList hypershiftv1beta1.HostedClusterList
	if err := r.List(ctx, &hostedClusterList, client.InNamespace(nsName), labelSelectorFromInstance(instance)); err != nil {
		log.Error(err, "failed to list hosted clusters")
		return nil, err
	}

	if len(hostedClusterList.Items) > 1 {
		return nil, fmt.Errorf("found too many (%d) matching hosted clusters for %s", len(hostedClusterList.Items), instance.GetName())
	}

	if len(hostedClusterList.Items) == 0 {
		return nil, nil
	}

	return &hostedClusterList.Items[0], nil
}

func (r *ClusterOrderReconciler) findNamespace(ctx context.Context, instance *v1alpha1.ClusterOrder) (*corev1.Namespace, error) {
	log := ctrllog.FromContext(ctx)

	var namespaceList corev1.NamespaceList
	if err := r.List(ctx, &namespaceList, labelSelectorFromInstance(instance)); err != nil {
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

func (r *ClusterOrderReconciler) handleDelete(ctx context.Context, _ ctrl.Request, instance *v1alpha1.ClusterOrder) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("deleting clusterorder")

	instance.Status.Phase = v1alpha1.ClusterOrderPhaseDeleting

	// Handle deprovisioning via provider
	// Waits for provision job termination and polls deprovision job if needed
	deprovisionResult, err := r.handleDeprovisioning(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	// If deprovision job is still running, requeue and wait
	if deprovisionResult.RequeueAfter > 0 {
		return deprovisionResult, nil
	}

	ns, err := r.findNamespace(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	if ns != nil {
		hc, err := r.findHostedCluster(ctx, instance, ns.GetName())
		if err != nil {
			return ctrl.Result{}, err
		}

		// We expect AAP to delete the hosted cluster, so we wait for that
		// to happen before deleting the containing namespace.
		if hc == nil {
			log.Info("deleting cluster namespace", "namespace", ns.GetName())
			if err := r.Client.Delete(ctx, ns); err != nil {
				log.Error(err, "failed to delete namespace", "namespace", ns.GetName(), "error", err)
				return ctrl.Result{}, err
			}
		}
	} else {
		// If we get this far, we are no longer monitoring any kubernetes resources.
		// Allow kubernetes to delete the clusterorder.
		if controllerutil.ContainsFinalizer(instance, osacFinalizer) {
			if controllerutil.RemoveFinalizer(instance, osacFinalizer) {
				if err := r.Update(ctx, instance); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
	}

	// We always trigger the delete webhook, since this is responsible both for
	// cleaning up the kubernetres resources and the underlying infrastructure.
	if url := r.DeleteClusterWebhook; url != "" {
		val, exists := instance.Annotations[osacManagementStateAnnotation]
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

	return ctrl.Result{}, nil
}

// handleProvisioning manages the provisioning job lifecycle for ClusterOrder.
// Uses hybrid approach: provider for job tracking + existing watch-based HC readiness.
// Non-blocking: returns RequeueAfter for polling, doesn't delay reconciliation.
func (r *ClusterOrderReconciler) handleProvisioning(ctx context.Context, instance *v1alpha1.ClusterOrder) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// If provider not configured, skip (webhook fallback)
	if r.ProvisioningProvider == nil {
		return ctrl.Result{}, nil
	}

	// Check if provision job already exists
	latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)

	// If no provision job exists, trigger one
	if latestProvisionJob == nil {
		log.Info("triggering provision job")
		result, err := r.ProvisioningProvider.TriggerProvision(ctx, instance)
		if err != nil {
			var rateLimitErr *provisioning.RateLimitError
			if errors.As(err, &rateLimitErr) {
				log.Info("provision request rate-limited, requeueing", "retryAfter", rateLimitErr.RetryAfter)
				return ctrl.Result{RequeueAfter: rateLimitErr.RetryAfter}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to trigger provision: %w", err)
		}

		// Append new job to status
		instance.Status.Jobs = helpers.AppendJob(instance.Status.Jobs, v1alpha1.JobStatus{
			JobID:     result.JobID,
			Type:      v1alpha1.JobTypeProvision,
			State:     result.InitialState,
			Message:   result.Message,
			Timestamp: metav1.Now(),
		}, r.MaxJobHistory)

		// Requeue to poll status
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	// Poll existing provision job status (non-blocking)
	if !latestProvisionJob.State.IsTerminal() {
		log.Info("polling provision job status", "jobID", latestProvisionJob.JobID, "currentState", latestProvisionJob.State)
		status, err := r.ProvisioningProvider.GetProvisionStatus(ctx, instance, latestProvisionJob.JobID)
		if err != nil {
			log.Error(err, "failed to get provision status", "jobID", latestProvisionJob.JobID)
			// Don't block reconciliation on polling errors, just requeue
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		// Update job status if changed
		if status.State != latestProvisionJob.State || status.Message != latestProvisionJob.Message {
			log.Info("provision job status changed", "jobID", latestProvisionJob.JobID, "oldState", latestProvisionJob.State, "newState", status.State)
			updatedJob := *latestProvisionJob
			updatedJob.State = status.State
			updatedJob.Message = status.Message
			helpers.UpdateJob(instance.Status.Jobs, updatedJob)

			// If job failed, set Phase to Failed
			if status.State == v1alpha1.JobStateFailed {
				log.Info("provision job failed", "jobID", latestProvisionJob.JobID)
				instance.Status.Phase = v1alpha1.ClusterOrderPhaseFailed
			}
		}

		// Continue polling if still running
		if !status.State.IsTerminal() {
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	// Job is terminal or no polling needed, continue with normal reconciliation
	return ctrl.Result{}, nil
}

// handleDeprovisioning manages the deprovisioning job lifecycle for ClusterOrder.
// Waits for provision job termination if needed, then triggers deprovision job.
func (r *ClusterOrderReconciler) handleDeprovisioning(ctx context.Context, instance *v1alpha1.ClusterOrder) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// If provider not configured, skip (webhook fallback)
	if r.ProvisioningProvider == nil {
		return ctrl.Result{}, nil
	}

	// Check if deprovision job already exists
	latestDeprovisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeDeprovision)

	// If no deprovision job exists, trigger one
	if latestDeprovisionJob == nil {
		log.Info("triggering deprovision job")
		result, err := r.ProvisioningProvider.TriggerDeprovision(ctx, instance)
		if err != nil {
			var rateLimitErr *provisioning.RateLimitError
			if errors.As(err, &rateLimitErr) {
				log.Info("deprovision request rate-limited, requeueing", "retryAfter", rateLimitErr.RetryAfter)
				return ctrl.Result{RequeueAfter: rateLimitErr.RetryAfter}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to trigger deprovision: %w", err)
		}

		// Handle different deprovision actions
		switch result.Action {
		case provisioning.DeprovisionSkipped:
			log.Info("deprovisioning skipped by provider")
			return ctrl.Result{}, nil

		case provisioning.DeprovisionWaiting:
			log.Info("waiting for provision job to terminate before deprovisioning")
			// Update provision job status if provided
			if result.ProvisionJobStatus != nil {
				latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
				if latestProvisionJob != nil {
					updatedJob := *latestProvisionJob
					updatedJob.State = result.ProvisionJobStatus.State
					updatedJob.Message = result.ProvisionJobStatus.Message
					helpers.UpdateJob(instance.Status.Jobs, updatedJob)
				}
			}
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil

		case provisioning.DeprovisionTriggered:
			log.Info("deprovision job triggered", "jobID", result.JobID)
			// Update provision job status if provided
			if result.ProvisionJobStatus != nil {
				latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
				if latestProvisionJob != nil {
					updatedJob := *latestProvisionJob
					updatedJob.State = result.ProvisionJobStatus.State
					updatedJob.Message = result.ProvisionJobStatus.Message
					helpers.UpdateJob(instance.Status.Jobs, updatedJob)
				}
			}
			// Append deprovision job
			instance.Status.Jobs = helpers.AppendJob(instance.Status.Jobs, v1alpha1.JobStatus{
				JobID:     result.JobID,
				Type:      v1alpha1.JobTypeDeprovision,
				State:     v1alpha1.JobStatePending,
				Message:   "Deprovision job triggered",
				Timestamp: metav1.Now(),
			}, r.MaxJobHistory)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	// Poll existing deprovision job status
	if !latestDeprovisionJob.State.IsTerminal() {
		log.Info("polling deprovision job status", "jobID", latestDeprovisionJob.JobID, "currentState", latestDeprovisionJob.State)
		status, err := r.ProvisioningProvider.GetDeprovisionStatus(ctx, instance, latestDeprovisionJob.JobID)
		if err != nil {
			log.Error(err, "failed to get deprovision status", "jobID", latestDeprovisionJob.JobID)
			// Don't block on polling errors, just requeue
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		// Update job status if changed
		if status.State != latestDeprovisionJob.State || status.Message != latestDeprovisionJob.Message {
			log.Info("deprovision job status changed", "jobID", latestDeprovisionJob.JobID, "oldState", latestDeprovisionJob.State, "newState", status.State)
			updatedJob := *latestDeprovisionJob
			updatedJob.State = status.State
			updatedJob.Message = status.Message
			helpers.UpdateJob(instance.Status.Jobs, updatedJob)
		}

		// Continue polling if still running
		if !status.State.IsTerminal() {
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	// Job is terminal, ready to proceed with deletion
	return ctrl.Result{}, nil
}

// initializeStatusConditions initializes the conditions that haven't already been initialized.
func (r *ClusterOrderReconciler) initializeStatusConditions(instance *v1alpha1.ClusterOrder) {
	r.initializeStatusCondition(
		instance,
		v1alpha1.ConditionAccepted,
		metav1.ConditionTrue,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ConditionDeleting,
		metav1.ConditionFalse,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ConditionProgressing,
		metav1.ConditionTrue,
		v1alpha1.ReasonProgressing,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ConditionNamespaceCreated,
		metav1.ConditionFalse,
		v1alpha1.ReasonInitialized,
	)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ConditionControlPlaneCreated,
		metav1.ConditionFalse,
		v1alpha1.ReasonInitialized)
	r.initializeStatusCondition(
		instance,
		v1alpha1.ConditionControlPlaneAvailable,
		metav1.ConditionFalse,
		v1alpha1.ReasonInitialized,
	)
}

// initializeStatusCondition initializes a condition, but only it is not already initialized.
func (r *ClusterOrderReconciler) initializeStatusCondition(instance *v1alpha1.ClusterOrder,
	conditionType string, status metav1.ConditionStatus, reason string) {
	if instance.Status.Conditions == nil {
		instance.Status.Conditions = []metav1.Condition{}
	}
	condition := meta.FindStatusCondition(instance.Status.Conditions, conditionType)
	if condition != nil {
		return
	}
	_ = meta.SetStatusCondition(
		&instance.Status.Conditions,
		metav1.Condition{
			Type:   conditionType,
			Status: status,
			Reason: reason,
		},
	)
}

func labelSelectorFromInstance(instance *v1alpha1.ClusterOrder) client.MatchingLabels {
	return client.MatchingLabels{
		osacClusterOrderNameLabel: instance.GetName(),
	}
}
