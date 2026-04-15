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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	clnt "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	privatev1 "github.com/osac-project/osac-operator/internal/api/osac/private/v1"
)

// SecurityGroupFeedbackReconciler sends updates to the fulfillment service.
type SecurityGroupFeedbackReconciler struct {
	hubClient            clnt.Client
	securityGroupsClient privatev1.SecurityGroupsClient
	networkingNamespace  string
}

// securityGroupFeedbackReconcilerTask contains data that is used for the reconciliation of a specific security group, so there is less
// need to pass around as function parameters that and other related objects.
type securityGroupFeedbackReconcilerTask struct {
	r             *SecurityGroupFeedbackReconciler
	object        *v1alpha1.SecurityGroup
	securityGroup *privatev1.SecurityGroup
}

// NewSecurityGroupFeedbackReconciler creates a reconciler that sends to the fulfillment service updates about security groups.
func NewSecurityGroupFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, networkingNamespace string) *SecurityGroupFeedbackReconciler {
	return &SecurityGroupFeedbackReconciler{
		hubClient:            hubClient,
		securityGroupsClient: privatev1.NewSecurityGroupsClient(grpcConn),
		networkingNamespace:  networkingNamespace,
	}
}

// SetupWithManager adds the reconciler to the controller manager.
func (r *SecurityGroupFeedbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("securitygroup-feedback").
		For(&v1alpha1.SecurityGroup{}, builder.WithPredicates(NetworkingNamespacePredicate(r.networkingNamespace))).
		Complete(r)
}

// Reconcile is the implementation of the reconciler interface.
func (r *SecurityGroupFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	log := ctrllog.FromContext(ctx)

	// Step 1: Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &v1alpha1.SecurityGroup{}
	err = r.hubClient.Get(ctx, request.NamespacedName, object)
	if err != nil {
		err = clnt.IgnoreNotFound(err)
		return //nolint:nakedret
	}

	// Step 2: Get the identifier of the security group from the labels. If this isn't present it means that the object wasn't
	// created by the fulfillment service, so we ignore it.
	securityGroupID, ok := object.Labels[osacSecurityGroupIDLabel]
	if !ok {
		// If being deleted and somehow has our finalizer, remove it to unblock deletion.
		if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacSecurityGroupFeedbackFinalizer) {
			log.Info("CR without security group ID label is being deleted, removing feedback finalizer")
			if controllerutil.RemoveFinalizer(object, osacSecurityGroupFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
			}
			return result, err
		}
		log.Info(
			"There is no label containing the security group identifier, will ignore it",
			"label", osacSecurityGroupIDLabel,
		)
		return result, err
	}

	// Step 3: Fetch the security group from the fulfillment service so we can compare before/after.
	securityGroup, err := r.fetchSecurityGroup(ctx, securityGroupID)
	if err != nil {
		// If the fulfillment service record is already deleted during CR deletion, remove the feedback
		// finalizer and exit gracefully. This prevents the controller from blocking CR garbage collection
		// when the fulfillment service record has been deleted before K8s CR cleanup completes.
		if !object.DeletionTimestamp.IsZero() && status.Code(err) == codes.NotFound {
			log.Info(
				"Security group record not found during deletion, removing feedback finalizer",
				"security_group_id", securityGroupID,
			)
			if controllerutil.RemoveFinalizer(object, osacSecurityGroupFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
			}
			return result, err
		}
		return result, err
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later compare the
	// before and after values and save only the objects that have changed.
	t := &securityGroupFeedbackReconcilerTask{
		r:             r,
		object:        object,
		securityGroup: clone(securityGroup),
	}

	// Step 4: Sync CR state to the fulfillment service record.
	// handleUpdate also adds our finalizer; handleDelete syncs state (e.g. DELETING phase).
	if object.DeletionTimestamp.IsZero() {
		err = t.handleUpdate(ctx)
	} else {
		t.handleDelete()
	}
	if err != nil {
		return result, err
	}

	// Step 5: Persist synced state to the fulfillment service.
	err = r.saveSecurityGroup(ctx, securityGroup, t.securityGroup)
	if err != nil {
		return result, err
	}

	// Step 6: Handle finalizer removal and signal for deletions. This must happen after step 5 so the
	// DELETING state is persisted before the CR is garbage collected.
	if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacSecurityGroupFeedbackFinalizer) {
		if len(object.GetFinalizers()) == 1 {
			// We're the last finalizer. Remove it to trigger CR garbage collection, then signal the
			// fulfillment service to immediately re-reconcile.
			log.Info(
				"Feedback finalizer is last remaining, removing finalizer and signaling",
				"securityGroupID", securityGroupID,
			)
			if controllerutil.RemoveFinalizer(object, osacSecurityGroupFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
				if err != nil {
					return result, err
				}
			}
			_, signalErr := r.securityGroupsClient.Signal(ctx, privatev1.SecurityGroupsSignalRequest_builder{
				Id: securityGroupID,
			}.Build())
			if signalErr != nil {
				log.Error(
					signalErr,
					"Failed to signal fulfillment service, periodic sync will handle cleanup",
					"securityGroupID", securityGroupID,
				)
			}
		} else {
			// Other finalizers still present — another controller hasn't finished cleanup yet.
			log.Info(
				"Other finalizers still present, waiting",
				"finalizers", object.GetFinalizers(),
			)
		}
	}

	return result, err
}

func (r *SecurityGroupFeedbackReconciler) fetchSecurityGroup(ctx context.Context, id string) (securityGroup *privatev1.SecurityGroup, err error) {
	response, err := r.securityGroupsClient.Get(ctx, privatev1.SecurityGroupsGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		return
	}
	securityGroup = response.GetObject()
	if !securityGroup.HasSpec() {
		securityGroup.SetSpec(&privatev1.SecurityGroupSpec{})
	}
	if !securityGroup.HasStatus() {
		securityGroup.SetStatus(&privatev1.SecurityGroupStatus{})
	}
	return
}

func (r *SecurityGroupFeedbackReconciler) saveSecurityGroup(ctx context.Context, before, after *privatev1.SecurityGroup) error {
	log := ctrllog.FromContext(ctx)

	if !equal(after, before) {
		log.Info(
			"Updating security group",
			"before", before,
			"after", after,
		)
		_, err := r.securityGroupsClient.Update(ctx, privatev1.SecurityGroupsUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

// handleUpdate ensures our finalizer is present and syncs the CR state to the
// fulfillment service. Called when the CR is not being deleted.
func (t *securityGroupFeedbackReconcilerTask) handleUpdate(ctx context.Context) error {
	if controllerutil.AddFinalizer(t.object, osacSecurityGroupFeedbackFinalizer) {
		if err := t.r.hubClient.Update(ctx, t.object); err != nil {
			return err
		}
	}
	t.syncPhase(ctx)
	return nil
}

// handleDelete syncs the deletion phase to the fulfillment service.
// Only the phase is synced during deletion (not other status fields),
// because we only need to communicate DELETING/DELETE_FAILED state.
func (t *securityGroupFeedbackReconcilerTask) handleDelete() {
	if t.object.Status.Phase == v1alpha1.SecurityGroupPhaseFailed {
		t.securityGroup.GetStatus().SetState(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_DELETE_FAILED)
		return
	}
	t.securityGroup.GetStatus().SetState(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_DELETING)
}

func (t *securityGroupFeedbackReconcilerTask) syncPhase(ctx context.Context) {
	switch t.object.Status.Phase {
	case v1alpha1.SecurityGroupPhaseProgressing:
		t.syncPhaseProgressing()
	case v1alpha1.SecurityGroupPhaseFailed:
		t.syncPhaseFailed()
	case v1alpha1.SecurityGroupPhaseReady:
		t.syncPhaseReady()
	case v1alpha1.SecurityGroupPhaseDeleting:
		t.syncPhaseDeleting()
	default:
		log := ctrllog.FromContext(ctx)
		log.Info(
			"Unknown phase, will ignore it",
			"phase", t.object.Status.Phase,
		)
	}
}

func (t *securityGroupFeedbackReconcilerTask) syncPhaseProgressing() {
	t.securityGroup.GetStatus().SetState(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_PENDING)
}

func (t *securityGroupFeedbackReconcilerTask) syncPhaseFailed() {
	t.securityGroup.GetStatus().SetState(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_FAILED)
}

func (t *securityGroupFeedbackReconcilerTask) syncPhaseReady() {
	securityGroupStatus := t.securityGroup.GetStatus()
	securityGroupStatus.SetState(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_READY)
}

func (t *securityGroupFeedbackReconcilerTask) syncPhaseDeleting() {
	t.securityGroup.GetStatus().SetState(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_DELETING)
}
