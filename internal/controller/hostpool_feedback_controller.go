/*
Copyright (c) 2025 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package controller

import (
	"context"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	clnt "sigs.k8s.io/controller-runtime/pkg/client"

	ckv1alpha1 "github.com/osac/osac-operator/api/v1alpha1"
	privatev1 "github.com/osac/osac-operator/internal/api/private/v1"
	sharedv1 "github.com/osac/osac-operator/internal/api/shared/v1"
)

// HostPoolFeedbackReconciler sends updates to the fulfillment service.
type HostPoolFeedbackReconciler struct {
	logger            logr.Logger
	hubClient         clnt.Client
	hostPoolsClient   privatev1.HostPoolsClient
	hostPoolNamespace string
}

// hostPoolFeedbackReconcilerTask contains data that is used for the reconciliation of a specific host pool, so there is less
// need to pass around as function parameters that and other related objects.
type hostPoolFeedbackReconcilerTask struct {
	r        *HostPoolFeedbackReconciler
	object   *ckv1alpha1.HostPool
	hostPool *privatev1.HostPool
}

// NewHostPoolFeedbackReconciler creates a reconciler that sends to the fulfillment service updates about host pools.
func NewHostPoolFeedbackReconciler(logger logr.Logger, hubClient clnt.Client, grpcConn *grpc.ClientConn, hostPoolNamespace string) *HostPoolFeedbackReconciler {
	return &HostPoolFeedbackReconciler{
		hubClient:         hubClient,
		hostPoolsClient:   privatev1.NewHostPoolsClient(grpcConn),
		hostPoolNamespace: hostPoolNamespace,
	}
}

// SetupWithManager adds the reconciler to the controller manager.
func (r *HostPoolFeedbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("hostpool-feedback").
		For(&ckv1alpha1.HostPool{}, builder.WithPredicates(HostPoolNamespacePredicate(r.hostPoolNamespace))).
		Complete(r)
}

// Reconcile is the implementation of the reconciler interface.
func (r *HostPoolFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	// Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &ckv1alpha1.HostPool{}
	err = r.hubClient.Get(ctx, request.NamespacedName, object)
	if err != nil {
		err = clnt.IgnoreNotFound(err)
		return //nolint:nakedret
	}

	// Get the identifier of the host pool from the labels. If this isn't present it means that the object wasn't
	// created by the fulfillment service, so we ignore it.
	hostPoolID, ok := object.Labels[cloudkitHostPoolIDLabel]
	if !ok {
		r.logger.Info(
			"There is no label containing the host pool identifier, will ignore it",
			"label", cloudkitHostPoolIDLabel,
		)
		return
	}

	// Check if the HostPool is being deleted before fetching from fulfillment service
	if !object.ObjectMeta.DeletionTimestamp.IsZero() {
		r.logger.Info(
			"HostPool is being deleted, skipping feedback reconciliation",
		)
		return
	}

	// Fetch the host pool:
	hostPool, err := r.fetchHostPool(ctx, hostPoolID)
	if err != nil {
		return
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later compare the
	// before and after values and save only the objects that have changed.
	t := &hostPoolFeedbackReconcilerTask{
		r:        r,
		object:   object,
		hostPool: clone(hostPool),
	}

	result, err = t.handleUpdate(ctx)
	if err != nil {
		return
	}
	// Save the objects that have changed:
	err = r.saveHostPool(ctx, hostPool, t.hostPool)
	if err != nil {
		return
	}
	return
}

func (r *HostPoolFeedbackReconciler) fetchHostPool(ctx context.Context, id string) (hostPool *privatev1.HostPool, err error) {
	response, err := r.hostPoolsClient.Get(ctx, privatev1.HostPoolsGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		return
	}
	hostPool = response.GetObject()
	if !hostPool.HasSpec() {
		hostPool.SetSpec(&privatev1.HostPoolSpec{})
	}
	if !hostPool.HasStatus() {
		hostPool.SetStatus(&privatev1.HostPoolStatus{})
	}
	return
}

func (r *HostPoolFeedbackReconciler) saveHostPool(ctx context.Context, before, after *privatev1.HostPool) error {
	if !equal(after, before) {
		_, err := r.hostPoolsClient.Update(ctx, privatev1.HostPoolsUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *hostPoolFeedbackReconcilerTask) handleUpdate(ctx context.Context) (result ctrl.Result, err error) {
	err = t.syncConditions(ctx)
	if err != nil {
		return
	}
	err = t.syncPhase(ctx)
	if err != nil {
		return
	}
	return
}

func (t *hostPoolFeedbackReconcilerTask) syncConditions(ctx context.Context) error {
	for _, condition := range t.object.Status.Conditions {
		err := t.syncCondition(ctx, condition)
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *hostPoolFeedbackReconcilerTask) syncCondition(ctx context.Context, condition metav1.Condition) error {
	switch ckv1alpha1.HostPoolConditionType(condition.Type) {
	case ckv1alpha1.HostPoolConditionAccepted:
		return t.syncConditionAccepted(condition)
	case ckv1alpha1.HostPoolConditionProgressing:
		return t.syncConditionProgressing(condition)
	case ckv1alpha1.HostPoolConditionAvailable:
		return t.syncConditionAvailable(condition)
	case ckv1alpha1.HostPoolConditionDeleting:
		return t.syncConditionDeleting(condition)
	default:
		t.r.logger.Info(
			"Unknown condition, will ignore it",
			"condition", condition.Type,
		)
	}
	return nil
}

func (t *hostPoolFeedbackReconcilerTask) syncConditionAccepted(condition metav1.Condition) error {
	hostPoolCondition := t.findHostPoolCondition(privatev1.HostPoolConditionType_HOST_POOL_CONDITION_TYPE_PROGRESSING)
	oldStatus := hostPoolCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	hostPoolCondition.SetStatus(newStatus)
	hostPoolCondition.SetMessage(condition.Message)
	if newStatus != oldStatus {
		hostPoolCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *hostPoolFeedbackReconcilerTask) syncConditionProgressing(condition metav1.Condition) error {
	hostPoolCondition := t.findHostPoolCondition(privatev1.HostPoolConditionType_HOST_POOL_CONDITION_TYPE_PROGRESSING)
	oldStatus := hostPoolCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	hostPoolCondition.SetStatus(newStatus)
	hostPoolCondition.SetMessage(condition.Message)
	if newStatus != oldStatus {
		hostPoolCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *hostPoolFeedbackReconcilerTask) syncConditionAvailable(condition metav1.Condition) error {
	hostPoolCondition := t.findHostPoolCondition(privatev1.HostPoolConditionType_HOST_POOL_CONDITION_TYPE_READY)
	oldStatus := hostPoolCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	hostPoolCondition.SetStatus(newStatus)
	hostPoolCondition.SetMessage(condition.Message)
	if newStatus != oldStatus {
		hostPoolCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *hostPoolFeedbackReconcilerTask) syncConditionDeleting(condition metav1.Condition) error {
	hostPoolCondition := t.findHostPoolCondition(privatev1.HostPoolConditionType_HOST_POOL_CONDITION_TYPE_PROGRESSING)
	oldStatus := hostPoolCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	hostPoolCondition.SetStatus(newStatus)
	hostPoolCondition.SetMessage(condition.Message)
	if newStatus != oldStatus {
		hostPoolCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *hostPoolFeedbackReconcilerTask) mapConditionStatus(status metav1.ConditionStatus) sharedv1.ConditionStatus {
	switch status {
	case metav1.ConditionFalse:
		return sharedv1.ConditionStatus_CONDITION_STATUS_FALSE
	case metav1.ConditionTrue:
		return sharedv1.ConditionStatus_CONDITION_STATUS_TRUE
	default:
		return sharedv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
	}
}

func (t *hostPoolFeedbackReconcilerTask) syncPhase(ctx context.Context) error {
	switch t.object.Status.Phase {
	case ckv1alpha1.HostPoolPhaseProgressing:
		return t.syncPhaseProgressing()
	case ckv1alpha1.HostPoolPhaseFailed:
		return t.syncPhaseFailed()
	case ckv1alpha1.HostPoolPhaseReady:
		return t.syncPhaseReady(ctx)
	case ckv1alpha1.HostPoolPhaseDeleting:
		// TODO: There is no equivalent phase in the fulfillment service.
		// return t.syncPhaseDeleting(ctx)
	default:
		t.r.logger.Info(
			"Unknown phase, will ignore it",
			"phase", t.object.Status.Phase,
		)
		return nil
	}
	return nil
}

func (t *hostPoolFeedbackReconcilerTask) syncPhaseProgressing() error {
	t.hostPool.GetStatus().SetState(privatev1.HostPoolState_HOST_POOL_STATE_PROGRESSING)
	return nil
}

func (t *hostPoolFeedbackReconcilerTask) syncPhaseFailed() error {
	t.hostPool.GetStatus().SetState(privatev1.HostPoolState_HOST_POOL_STATE_FAILED)
	return nil
}

func (t *hostPoolFeedbackReconcilerTask) syncPhaseReady(ctx context.Context) error {
	// Set the status of the host pool:
	hostPoolStatus := t.hostPool.GetStatus()
	hostPoolStatus.SetState(privatev1.HostPoolState_HOST_POOL_STATE_READY)

	// TODO: Add any additional status fields that need to be synced when the HostPool is ready
	// For example, actual allocated hosts, etc.

	return nil
}

func (t *hostPoolFeedbackReconcilerTask) findHostPoolCondition(kind privatev1.HostPoolConditionType) *privatev1.HostPoolCondition {
	var condition *privatev1.HostPoolCondition
	for _, current := range t.hostPool.Status.Conditions {
		if current.Type == kind {
			condition = current
			break
		}
	}
	if condition == nil {
		condition = &privatev1.HostPoolCondition{
			Type:   kind,
			Status: sharedv1.ConditionStatus_CONDITION_STATUS_FALSE,
		}
		t.hostPool.Status.Conditions = append(t.hostPool.Status.Conditions, condition)
	}
	return condition
}
