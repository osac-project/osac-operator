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

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	clnt "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	ckv1alpha1 "github.com/innabox/cloudkit-operator/api/v1alpha1"
	privatev1 "github.com/innabox/cloudkit-operator/internal/api/private/v1"
	sharedv1 "github.com/innabox/cloudkit-operator/internal/api/shared/v1"
)

// HostFeedbackReconciler sends updates to the fulfillment service.
type HostFeedbackReconciler struct {
	hubClient     clnt.Client
	hostsClient   privatev1.HostsClient
	hostNamespace string
}

// hostFeedbackReconcilerTask contains data that is used for the reconciliation of a specific host, so there is less
// need to pass around as function parameters that and other related objects.
type hostFeedbackReconcilerTask struct {
	r      *HostFeedbackReconciler
	object *ckv1alpha1.Host
	host   *privatev1.Host
}

// NewHostFeedbackReconciler creates a reconciler that sends to the fulfillment service updates about hosts.
func NewHostFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, hostNamespace string) *HostFeedbackReconciler {
	return &HostFeedbackReconciler{
		hubClient:     hubClient,
		hostsClient:   privatev1.NewHostsClient(grpcConn),
		hostNamespace: hostNamespace,
	}
}

// SetupWithManager adds the reconciler to the controller manager.
func (r *HostFeedbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("host-feedback").
		For(&ckv1alpha1.Host{}, builder.WithPredicates(HostNamespacePredicate(r.hostNamespace))).
		Complete(r)
}

// Reconcile is the implementation of the reconciler interface.
func (r *HostFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	log := ctrllog.FromContext(ctx)

	// Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &ckv1alpha1.Host{}
	err = r.hubClient.Get(ctx, request.NamespacedName, object)
	if err != nil {
		err = clnt.IgnoreNotFound(err)
		return //nolint:nakedret
	}

	// Get the identifier of the host from the labels. If this isn't present it means that the object wasn't
	// created by the fulfillment service, so we ignore it.
	hostID, ok := object.Labels[cloudkitHostIDLabel]
	if !ok {
		log.Info(
			"There is no label containing the host identifier, will ignore it",
			"label", cloudkitHostIDLabel,
		)
		return
	}

	// Check if the Host is being deleted before fetching from fulfillment service
	if !object.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info(
			"Host is being deleted, skipping feedback reconciliation",
		)
		return
	}

	// Fetch the host:
	host, err := r.fetchHost(ctx, hostID)
	if err != nil {
		return
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later compare the
	// before and after values and save only the objects that have changed.
	t := &hostFeedbackReconcilerTask{
		r:      r,
		object: object,
		host:   clone(host),
	}

	result, err = t.handleUpdate(ctx)
	if err != nil {
		return
	}
	// Save the objects that have changed:
	err = r.saveHost(ctx, host, t.host)
	if err != nil {
		return
	}
	return
}

func (r *HostFeedbackReconciler) fetchHost(ctx context.Context, id string) (host *privatev1.Host, err error) {
	response, err := r.hostsClient.Get(ctx, privatev1.HostsGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		return
	}
	host = response.GetObject()
	if !host.HasSpec() {
		host.SetSpec(&privatev1.HostSpec{})
	}
	if !host.HasStatus() {
		host.SetStatus(&privatev1.HostStatus{})
	}
	return
}

func (r *HostFeedbackReconciler) saveHost(ctx context.Context, before, after *privatev1.Host) error {
	if !equal(after, before) {
		_, err := r.hostsClient.Update(ctx, privatev1.HostsUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *hostFeedbackReconcilerTask) handleUpdate(ctx context.Context) (result ctrl.Result, err error) {
	err = t.syncConditions(ctx)
	if err != nil {
		return
	}
	err = t.syncState(ctx)
	if err != nil {
		return
	}
	err = t.syncPhase(ctx)
	if err != nil {
		return
	}
	err = t.syncPowerState(ctx)
	if err != nil {
		return
	}
	return
}

func (t *hostFeedbackReconcilerTask) syncConditions(ctx context.Context) error {
	for _, condition := range t.object.Status.Conditions {
		err := t.syncCondition(ctx, condition)
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *hostFeedbackReconcilerTask) syncCondition(ctx context.Context, condition metav1.Condition) error {
	switch ckv1alpha1.HostConditionType(condition.Type) {
	case ckv1alpha1.HostConditionAccepted:
		return t.syncConditionAccepted(condition)
	case ckv1alpha1.HostConditionProgressing:
		return t.syncConditionProgressing(condition)
	case ckv1alpha1.HostConditionReady:
		return t.syncConditionReady(condition)
	case ckv1alpha1.HostConditionFailed:
		return t.syncConditionFailed(condition)
	case ckv1alpha1.HostConditionAvailable:
		return t.syncConditionAvailable(condition)
	case ckv1alpha1.HostConditionDeleting:
		return t.syncConditionDeleting(condition)
	default:
		log := ctrllog.FromContext(ctx)
		log.Info(
			"Unknown condition, will ignore it",
			"condition", condition.Type,
		)
	}
	return nil
}

func (t *hostFeedbackReconcilerTask) syncConditionAccepted(condition metav1.Condition) error {
	// Map Accepted condition to Progressing in the private API
	// This represents that the host has been accepted and is being processed
	hostCondition := t.findOrCreateHostCondition(privatev1.HostConditionType_HOST_CONDITION_TYPE_PROGRESSING)
	oldStatus := hostCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	hostCondition.SetStatus(newStatus)
	hostCondition.SetMessage(condition.Message)
	if condition.Reason != "" {
		hostCondition.SetReason(condition.Reason)
	}
	if newStatus != oldStatus {
		hostCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *hostFeedbackReconcilerTask) syncConditionProgressing(condition metav1.Condition) error {
	// Map Progressing condition directly to the private API
	hostCondition := t.findOrCreateHostCondition(privatev1.HostConditionType_HOST_CONDITION_TYPE_PROGRESSING)
	oldStatus := hostCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	hostCondition.SetStatus(newStatus)
	hostCondition.SetMessage(condition.Message)
	if condition.Reason != "" {
		hostCondition.SetReason(condition.Reason)
	}
	if newStatus != oldStatus {
		hostCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *hostFeedbackReconcilerTask) syncConditionReady(condition metav1.Condition) error {
	// Map Ready condition directly to the private API
	hostCondition := t.findOrCreateHostCondition(privatev1.HostConditionType_HOST_CONDITION_TYPE_READY)
	oldStatus := hostCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	hostCondition.SetStatus(newStatus)
	hostCondition.SetMessage(condition.Message)
	if condition.Reason != "" {
		hostCondition.SetReason(condition.Reason)
	}
	if newStatus != oldStatus {
		hostCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *hostFeedbackReconcilerTask) syncConditionFailed(condition metav1.Condition) error {
	// Map Failed condition directly to the private API
	hostCondition := t.findOrCreateHostCondition(privatev1.HostConditionType_HOST_CONDITION_TYPE_FAILED)
	oldStatus := hostCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	hostCondition.SetStatus(newStatus)
	hostCondition.SetMessage(condition.Message)
	if condition.Reason != "" {
		hostCondition.SetReason(condition.Reason)
	}
	if newStatus != oldStatus {
		hostCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *hostFeedbackReconcilerTask) syncConditionAvailable(condition metav1.Condition) error {
	// Map Available condition to Ready in the private API
	// This represents that the host is available and ready to use
	hostCondition := t.findOrCreateHostCondition(privatev1.HostConditionType_HOST_CONDITION_TYPE_READY)
	oldStatus := hostCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	hostCondition.SetStatus(newStatus)
	hostCondition.SetMessage(condition.Message)
	if condition.Reason != "" {
		hostCondition.SetReason(condition.Reason)
	}
	if newStatus != oldStatus {
		hostCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *hostFeedbackReconcilerTask) syncConditionDeleting(condition metav1.Condition) error {
	// When deleting, we can set the host as degraded
	// Use degraded to indicate the host is being removed
	hostCondition := t.findOrCreateHostCondition(privatev1.HostConditionType_HOST_CONDITION_TYPE_DEGRADED)
	oldStatus := hostCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	hostCondition.SetStatus(newStatus)
	hostCondition.SetMessage(condition.Message)
	if condition.Reason != "" {
		hostCondition.SetReason(condition.Reason)
	}
	if newStatus != oldStatus {
		hostCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *hostFeedbackReconcilerTask) mapConditionStatus(status metav1.ConditionStatus) sharedv1.ConditionStatus {
	switch status {
	case metav1.ConditionFalse:
		return sharedv1.ConditionStatus_CONDITION_STATUS_FALSE
	case metav1.ConditionTrue:
		return sharedv1.ConditionStatus_CONDITION_STATUS_TRUE
	default:
		return sharedv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
	}
}

func (t *hostFeedbackReconcilerTask) syncState(ctx context.Context) error {
	// Sync the state from Kubernetes to the private API
	log := ctrllog.FromContext(ctx)

	// Map the state directly to the private API state field
	switch t.object.Status.State {
	case ckv1alpha1.HostStateProgressing:
		t.host.GetStatus().SetState(privatev1.HostState_HOST_STATE_PROGRESSING)
		log.Info("Synced host state to progressing", "hostId", t.host.GetId())
	case ckv1alpha1.HostStateReady:
		t.host.GetStatus().SetState(privatev1.HostState_HOST_STATE_READY)
		log.Info("Synced host state to ready", "hostId", t.host.GetId())
	case ckv1alpha1.HostStateFailed:
		t.host.GetStatus().SetState(privatev1.HostState_HOST_STATE_FAILED)
		log.Info("Synced host state to failed", "hostId", t.host.GetId())
	case ckv1alpha1.HostStateUnspecified:
		t.host.GetStatus().SetState(privatev1.HostState_HOST_STATE_UNSPECIFIED)
		log.Info("Synced host state to unspecified", "hostId", t.host.GetId())
	default:
		log.Info("Unknown host state, using unspecified", "hostId", t.host.GetId(), "state", t.object.Status.State)
		t.host.GetStatus().SetState(privatev1.HostState_HOST_STATE_UNSPECIFIED)
	}

	return nil
}

// findOrCreateHostCondition finds an existing condition of the given type or creates a new one
func (t *hostFeedbackReconcilerTask) findOrCreateHostCondition(conditionType privatev1.HostConditionType) *privatev1.HostCondition {
	var condition *privatev1.HostCondition
	conditions := t.host.GetStatus().GetConditions()

	for _, current := range conditions {
		if current.GetType() == conditionType {
			condition = current
			break
		}
	}

	if condition == nil {
		condition = &privatev1.HostCondition{
			Type:   conditionType,
			Status: sharedv1.ConditionStatus_CONDITION_STATUS_FALSE,
		}
		conditions = append(conditions, condition)
		t.host.GetStatus().SetConditions(conditions)
	}

	return condition
}

func (t *hostFeedbackReconcilerTask) syncPhase(ctx context.Context) error {
	switch t.object.Status.Phase {
	case ckv1alpha1.HostPhaseProgressing:
		// Map to appropriate host state when available in private API
		return nil
	case ckv1alpha1.HostPhaseFailed:
		// Map to appropriate host state when available in private API
		return nil
	case ckv1alpha1.HostPhaseReady:
		return t.syncPhaseReady(ctx)
	case ckv1alpha1.HostPhaseDeleting:
		// Map to appropriate host state when available in private API
		return nil
	default:
		log := ctrllog.FromContext(ctx)
		log.Info(
			"Unknown phase, will ignore it",
			"phase", t.object.Status.Phase,
		)
		return nil
	}
}

func (t *hostFeedbackReconcilerTask) syncPhaseReady(ctx context.Context) error {
	// Set the private API state when the host is ready
	t.host.GetStatus().SetState(privatev1.HostState_HOST_STATE_READY)

	// Sync power state when ready
	return t.syncPowerState(ctx)
}

func (t *hostFeedbackReconcilerTask) syncPowerState(ctx context.Context) error {
	// Sync the power state from Kubernetes to the private API
	if t.object.Status.PowerState != "" {
		var privatePowerState privatev1.HostPowerState
		switch t.object.Status.PowerState {
		case ckv1alpha1.HostPowerStateOn:
			privatePowerState = privatev1.HostPowerState_HOST_POWER_STATE_ON
		case ckv1alpha1.HostPowerStateOff:
			privatePowerState = privatev1.HostPowerState_HOST_POWER_STATE_OFF
		default:
			privatePowerState = privatev1.HostPowerState_HOST_POWER_STATE_UNSPECIFIED
		}

		// Update the power state in the private API
		t.host.GetStatus().SetPowerState(privatePowerState)

		log := ctrllog.FromContext(ctx)
		log.Info("Synced host power state",
			"hostId", t.host.GetId(),
			"powerState", privatePowerState.String(),
		)
	}

	return nil
}
