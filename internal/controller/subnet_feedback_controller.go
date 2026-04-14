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

// SubnetFeedbackReconciler sends updates to the fulfillment service.
type SubnetFeedbackReconciler struct {
	hubClient           clnt.Client
	subnetsClient       privatev1.SubnetsClient
	networkingNamespace string
}

// subnetFeedbackReconcilerTask contains data that is used for the reconciliation of a specific subnet, so there is less
// need to pass around as function parameters that and other related objects.
type subnetFeedbackReconcilerTask struct {
	r      *SubnetFeedbackReconciler
	object *v1alpha1.Subnet
	subnet *privatev1.Subnet
}

// NewSubnetFeedbackReconciler creates a reconciler that sends to the fulfillment service updates about subnets.
func NewSubnetFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, networkingNamespace string) *SubnetFeedbackReconciler {
	return &SubnetFeedbackReconciler{
		hubClient:           hubClient,
		subnetsClient:       privatev1.NewSubnetsClient(grpcConn),
		networkingNamespace: networkingNamespace,
	}
}

// SetupWithManager adds the reconciler to the controller manager.
func (r *SubnetFeedbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("subnet-feedback").
		For(&v1alpha1.Subnet{}, builder.WithPredicates(NetworkingNamespacePredicate(r.networkingNamespace))).
		Complete(r)
}

// Reconcile is the implementation of the reconciler interface.
func (r *SubnetFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	log := ctrllog.FromContext(ctx)

	// Step 1: Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &v1alpha1.Subnet{}
	err = r.hubClient.Get(ctx, request.NamespacedName, object)
	if err != nil {
		err = clnt.IgnoreNotFound(err)
		return //nolint:nakedret
	}

	// Step 2: Get the identifier of the subnet from the labels. If this isn't present it means that the object
	// wasn't created by the fulfillment service, so we ignore it.
	subnetID, ok := object.Labels[osacSubnetIDLabel]
	if !ok {
		// If being deleted and somehow has our finalizer, remove it to unblock deletion.
		if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacSubnetFeedbackFinalizer) {
			log.Info("CR without subnet ID label is being deleted, removing feedback finalizer")
			if controllerutil.RemoveFinalizer(object, osacSubnetFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
			}
			return result, err
		}
		log.Info(
			"There is no label containing the subnet identifier, will ignore it",
			"label", osacSubnetIDLabel,
		)
		return result, err
	}

	// Step 3: Fetch the subnet from the fulfillment service so we can compare before/after.
	subnet, err := r.fetchSubnet(ctx, subnetID)
	if err != nil {
		if !object.DeletionTimestamp.IsZero() && status.Code(err) == codes.NotFound {
			log.Info("Subnet record not found during deletion, removing feedback finalizer", "subnetID", subnetID)
			if controllerutil.RemoveFinalizer(object, osacSubnetFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
			}
			return result, err
		}
		return result, err
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later compare the
	// before and after values and save only the objects that have changed.
	t := &subnetFeedbackReconcilerTask{
		r:      r,
		object: object,
		subnet: clone(subnet),
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
	err = r.saveSubnet(ctx, subnet, t.subnet)
	if err != nil {
		return result, err
	}

	// Step 6: Handle finalizer removal and signal for deletions. This must happen after step 5 so the
	// DELETING state is persisted before the CR is garbage collected.
	if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacSubnetFeedbackFinalizer) {
		if len(object.GetFinalizers()) == 1 {
			// We're the last finalizer. Remove it to trigger CR garbage collection, then signal the
			// fulfillment service to immediately re-reconcile.
			log.Info(
				"Feedback finalizer is last remaining, removing finalizer and signaling",
				"subnetID", subnetID,
			)
			if controllerutil.RemoveFinalizer(object, osacSubnetFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
				if err != nil {
					return result, err
				}
			}
			_, signalErr := r.subnetsClient.Signal(ctx, privatev1.SubnetsSignalRequest_builder{
				Id: subnetID,
			}.Build())
			if signalErr != nil {
				log.Error(
					signalErr,
					"Failed to signal fulfillment service, periodic sync will handle cleanup",
					"subnetID", subnetID,
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

func (r *SubnetFeedbackReconciler) fetchSubnet(ctx context.Context, id string) (subnet *privatev1.Subnet, err error) {
	response, err := r.subnetsClient.Get(ctx, privatev1.SubnetsGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		return
	}
	subnet = response.GetObject()
	if !subnet.HasSpec() {
		subnet.SetSpec(&privatev1.SubnetSpec{})
	}
	if !subnet.HasStatus() {
		subnet.SetStatus(&privatev1.SubnetStatus{})
	}
	return
}

func (r *SubnetFeedbackReconciler) saveSubnet(ctx context.Context, before, after *privatev1.Subnet) error {
	log := ctrllog.FromContext(ctx)

	if !equal(after, before) {
		log.Info(
			"Updating subnet",
			"before", before,
			"after", after,
		)
		_, err := r.subnetsClient.Update(ctx, privatev1.SubnetsUpdateRequest_builder{
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
func (t *subnetFeedbackReconcilerTask) handleUpdate(ctx context.Context) error {
	if controllerutil.AddFinalizer(t.object, osacSubnetFeedbackFinalizer) {
		if err := t.r.hubClient.Update(ctx, t.object); err != nil {
			return err
		}
	}
	t.syncPhase(ctx)
	t.syncBackendNetworkID()
	return nil
}

// handleDelete syncs the deletion phase to the fulfillment service.
// Only the phase is synced during deletion (not backend network ID),
// because we only need to communicate DELETING/DELETE_FAILED state.
func (t *subnetFeedbackReconcilerTask) handleDelete() {
	if t.object.Status.Phase == v1alpha1.SubnetPhaseFailed {
		t.subnet.GetStatus().SetState(privatev1.SubnetState_SUBNET_STATE_DELETE_FAILED)
		return
	}
	t.subnet.GetStatus().SetState(privatev1.SubnetState_SUBNET_STATE_DELETING)
}

func (t *subnetFeedbackReconcilerTask) syncPhase(ctx context.Context) {
	switch t.object.Status.Phase {
	case v1alpha1.SubnetPhaseProgressing:
		t.syncPhaseProgressing()
	case v1alpha1.SubnetPhaseFailed:
		t.syncPhaseFailed()
	case v1alpha1.SubnetPhaseReady:
		t.syncPhaseReady()
	case v1alpha1.SubnetPhaseDeleting:
		t.syncPhaseDeleting()
	default:
		log := ctrllog.FromContext(ctx)
		log.Info(
			"Unknown phase, will ignore it",
			"phase", t.object.Status.Phase,
		)
	}
}

func (t *subnetFeedbackReconcilerTask) syncPhaseProgressing() {
	t.subnet.GetStatus().SetState(privatev1.SubnetState_SUBNET_STATE_PENDING)
}

func (t *subnetFeedbackReconcilerTask) syncPhaseFailed() {
	t.subnet.GetStatus().SetState(privatev1.SubnetState_SUBNET_STATE_FAILED)
}

func (t *subnetFeedbackReconcilerTask) syncPhaseReady() {
	subnetStatus := t.subnet.GetStatus()
	subnetStatus.SetState(privatev1.SubnetState_SUBNET_STATE_READY)
}

func (t *subnetFeedbackReconcilerTask) syncPhaseDeleting() {
	t.subnet.GetStatus().SetState(privatev1.SubnetState_SUBNET_STATE_DELETING)
}

func (t *subnetFeedbackReconcilerTask) syncBackendNetworkID() {
	if t.object.Status.BackendNetworkID != "" {
		t.subnet.GetStatus().SetMessage(t.object.Status.BackendNetworkID)
	}
}
