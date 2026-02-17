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
	"errors"
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

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/helpers"
	"github.com/osac-project/osac-operator/internal/provisioning"
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
	ProvisioningProvider  provisioning.ProvisioningProvider
	StatusPollInterval    time.Duration
	MaxJobHistory         int
}

func NewHostPoolReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	createHostPoolWebhook string,
	deleteHostPoolWebhook string,
	hostPoolNamespace string,
	minimumRequestInterval time.Duration,
	provisioningProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
) *HostPoolReconciler {

	if hostPoolNamespace == "" {
		hostPoolNamespace = defaultHostPoolNamespace
	}

	if statusPollInterval == 0 {
		statusPollInterval = 30 * time.Second
	}

	if maxJobHistory <= 0 {
		maxJobHistory = DefaultMaxJobHistory
	}

	return &HostPoolReconciler{
		Client:                client,
		Scheme:                scheme,
		CreateHostPoolWebhook: createHostPoolWebhook,
		DeleteHostPoolWebhook: deleteHostPoolWebhook,
		HostPoolNamespace:     hostPoolNamespace,
		webhookClient:         NewWebhookClient(10*time.Second, minimumRequestInterval),
		ProvisioningProvider:  provisioningProvider,
		StatusPollInterval:    statusPollInterval,
		MaxJobHistory:         maxJobHistory,
	}
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostpools/finalizers,verbs=update
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

	val, exists := instance.Annotations[osacHostPoolManagementStateAnnotation]
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
				Key:      osacHostPoolNameLabel,
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

	hostPoolName, exists := obj.GetLabels()[osacHostPoolNameLabel]
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

	// Handle provisioning via provider
	// Polls job status and sets Phase=Ready only when job succeeds (replaces fire-and-forget webhook)
	provisionResult, err := r.handleProvisioning(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	// If using provider, check provision job status before setting Ready
	if r.ProvisioningProvider != nil {
		latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
		if latestProvisionJob != nil {
			// Only set Ready if provision job succeeded
			if latestProvisionJob.State == v1alpha1.JobStateSucceeded {
				instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionFalse, "HostPoolReady", "HostPool is ready")
				instance.SetCondition(v1alpha1.HostPoolConditionAvailable, metav1.ConditionTrue, "HostPoolAvailable", "HostPool is available")
				instance.Status.Phase = v1alpha1.HostPoolPhaseReady
				// Update status with host sets info
				instance.Status.HostSets = instance.Spec.HostSets
			} else if !latestProvisionJob.State.IsTerminal() {
				// Job still running, keep Progressing phase
				instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionTrue, "ProvisionJobRunning", fmt.Sprintf("Provision job %s is %s", latestProvisionJob.JobID, latestProvisionJob.State))
			}
			// If job failed, Phase was already set to Failed in handleProvisioning
		}
	} else {
		// Webhook fallback - call webhook if configured
		if r.CreateHostPoolWebhook != "" {
			_, err := r.webhookClient.TriggerWebhook(ctx, r.CreateHostPoolWebhook, instance)
			if err != nil {
				instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionFalse, "WebhookFailed", fmt.Sprintf("Webhook call failed: %v", err))
				instance.Status.Phase = v1alpha1.HostPoolPhaseFailed
				return ctrl.Result{}, err
			}
		}

		// Fire-and-forget webhook - set Ready immediately (old behavior)
		instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionFalse, "HostPoolReady", "HostPool is ready")
		instance.SetCondition(v1alpha1.HostPoolConditionAvailable, metav1.ConditionTrue, "HostPoolAvailable", "HostPool is available")
		instance.Status.Phase = v1alpha1.HostPoolPhaseReady
		// Update status with host sets info
		instance.Status.HostSets = instance.Spec.HostSets
	}

	// If provision job needs polling, requeue for status updates
	if provisionResult.RequeueAfter > 0 {
		return provisionResult, nil
	}

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
		// Call webhook to delete host pool resources
		if url := r.DeleteHostPoolWebhook; url != "" {
			val, exists := instance.Annotations[osacHostPoolManagementStateAnnotation]
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

// handleProvisioning manages the provisioning job lifecycle for HostPool.
// Polls job status and sets Phase=Ready only when job succeeds (replaces fire-and-forget webhook).
func (r *HostPoolReconciler) handleProvisioning(ctx context.Context, instance *v1alpha1.HostPool) (ctrl.Result, error) {
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

	// Poll existing provision job status
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
				instance.SetCondition(v1alpha1.HostPoolConditionProgressing, metav1.ConditionFalse, "ProvisionJobFailed", fmt.Sprintf("Provision job %s failed: %s", latestProvisionJob.JobID, status.Message))
				instance.Status.Phase = v1alpha1.HostPoolPhaseFailed
			}
		}

		// Continue polling if still running
		if !status.State.IsTerminal() {
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	// Job is terminal - if succeeded, we can proceed with setting Phase=Ready in handleUpdate
	return ctrl.Result{}, nil
}

// handleDeprovisioning manages the deprovisioning job lifecycle for HostPool.
// Waits for provision job termination if needed, then triggers deprovision job.
func (r *HostPoolReconciler) handleDeprovisioning(ctx context.Context, instance *v1alpha1.HostPool) (ctrl.Result, error) {
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
