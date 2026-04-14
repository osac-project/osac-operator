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

// VirtualNetworkFeedbackReconciler sends updates to the fulfillment service.
type VirtualNetworkFeedbackReconciler struct {
	hubClient             clnt.Client
	virtualNetworksClient privatev1.VirtualNetworksClient
	networkingNamespace   string
}

// virtualNetworkFeedbackReconcilerTask contains data that is used for the reconciliation of a specific virtual network, so there is less
// need to pass around as function parameters that and other related objects.
type virtualNetworkFeedbackReconcilerTask struct {
	r              *VirtualNetworkFeedbackReconciler
	object         *v1alpha1.VirtualNetwork
	virtualNetwork *privatev1.VirtualNetwork
}

// NewVirtualNetworkFeedbackReconciler creates a reconciler that sends to the fulfillment service updates about virtual networks.
func NewVirtualNetworkFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, networkingNamespace string) *VirtualNetworkFeedbackReconciler {
	return &VirtualNetworkFeedbackReconciler{
		hubClient:             hubClient,
		virtualNetworksClient: privatev1.NewVirtualNetworksClient(grpcConn),
		networkingNamespace:   networkingNamespace,
	}
}

// SetupWithManager adds the reconciler to the controller manager.
func (r *VirtualNetworkFeedbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("virtualnetwork-feedback").
		For(&v1alpha1.VirtualNetwork{}, builder.WithPredicates(NetworkingNamespacePredicate(r.networkingNamespace))).
		Complete(r)
}

// Reconcile is the implementation of the reconciler interface.
func (r *VirtualNetworkFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	log := ctrllog.FromContext(ctx)

	// Step 1: Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &v1alpha1.VirtualNetwork{}
	err = r.hubClient.Get(ctx, request.NamespacedName, object)
	if err != nil {
		err = clnt.IgnoreNotFound(err)
		return //nolint:nakedret
	}

	// Step 2: Get the identifier of the virtual network from the labels. If this isn't present it means that the object
	// wasn't created by the fulfillment service, so we ignore it.
	virtualNetworkID, ok := object.Labels[osacVirtualNetworkIDLabel]
	if !ok {
		// If being deleted and somehow has our finalizer, remove it to unblock deletion.
		if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacVirtualNetworkFeedbackFinalizer) {
			log.Info("CR without virtual network ID label is being deleted, removing feedback finalizer")
			if controllerutil.RemoveFinalizer(object, osacVirtualNetworkFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
			}
			return result, err
		}
		log.Info(
			"There is no label containing the virtual network identifier, will ignore it",
			"label", osacVirtualNetworkIDLabel,
		)
		return result, err
	}

	// Step 3: Fetch the virtual network from the fulfillment service so we can compare before/after.
	virtualNetwork, err := r.fetchVirtualNetwork(ctx, virtualNetworkID)
	if err != nil {
		if !object.DeletionTimestamp.IsZero() && status.Code(err) == codes.NotFound {
			log.Info("VirtualNetwork record not found during deletion, removing feedback finalizer", "virtualNetworkID", virtualNetworkID)
			if controllerutil.RemoveFinalizer(object, osacVirtualNetworkFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
			}
			return result, err
		}
		return result, err
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later compare the
	// before and after values and save only the objects that have changed.
	t := &virtualNetworkFeedbackReconcilerTask{
		r:              r,
		object:         object,
		virtualNetwork: clone(virtualNetwork),
	}

	// Step 4: Sync CR state to the fulfillment service record.
	// handleUpdate also adds our finalizer; handleDelete syncs state.
	if object.DeletionTimestamp.IsZero() {
		err = t.handleUpdate(ctx)
	} else {
		t.handleDelete()
	}
	if err != nil {
		return result, err
	}

	// Step 5: Persist synced state to the fulfillment service.
	err = r.saveVirtualNetwork(ctx, virtualNetwork, t.virtualNetwork)
	if err != nil {
		return result, err
	}

	// Step 6: Handle finalizer removal and signal for deletions. This must happen after step 5 so the
	// deletion state is persisted before the CR is garbage collected.
	if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacVirtualNetworkFeedbackFinalizer) {
		if len(object.GetFinalizers()) == 1 {
			// We're the last finalizer. Remove it to trigger CR garbage collection, then signal the
			// fulfillment service to immediately re-reconcile.
			log.Info(
				"Feedback finalizer is last remaining, removing finalizer and signaling",
				"virtualNetworkID", virtualNetworkID,
			)
			if controllerutil.RemoveFinalizer(object, osacVirtualNetworkFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
				if err != nil {
					return result, err
				}
			}
			_, signalErr := r.virtualNetworksClient.Signal(ctx, privatev1.VirtualNetworksSignalRequest_builder{
				Id: virtualNetworkID,
			}.Build())
			if signalErr != nil {
				log.Error(
					signalErr,
					"Failed to signal fulfillment service, periodic sync will handle cleanup",
					"virtualNetworkID", virtualNetworkID,
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

func (r *VirtualNetworkFeedbackReconciler) fetchVirtualNetwork(ctx context.Context, id string) (virtualNetwork *privatev1.VirtualNetwork, err error) {
	response, err := r.virtualNetworksClient.Get(ctx, privatev1.VirtualNetworksGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		return
	}
	virtualNetwork = response.GetObject()
	if !virtualNetwork.HasSpec() {
		virtualNetwork.SetSpec(&privatev1.VirtualNetworkSpec{})
	}
	if !virtualNetwork.HasStatus() {
		virtualNetwork.SetStatus(&privatev1.VirtualNetworkStatus{})
	}
	return
}

func (r *VirtualNetworkFeedbackReconciler) saveVirtualNetwork(ctx context.Context, before, after *privatev1.VirtualNetwork) error {
	log := ctrllog.FromContext(ctx)

	if !equal(after, before) {
		log.Info(
			"Updating virtual network",
			"before", before,
			"after", after,
		)
		_, err := r.virtualNetworksClient.Update(ctx, privatev1.VirtualNetworksUpdateRequest_builder{
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
func (t *virtualNetworkFeedbackReconcilerTask) handleUpdate(ctx context.Context) error {
	if controllerutil.AddFinalizer(t.object, osacVirtualNetworkFeedbackFinalizer) {
		if err := t.r.hubClient.Update(ctx, t.object); err != nil {
			return err
		}
	}
	t.syncPhase(ctx)
	return nil
}

// handleDelete syncs the deletion phase to the fulfillment service.
// VN proto has no DELETING/DELETE_FAILED states, so Deleting maps to PENDING
// and Failed maps to FAILED.
func (t *virtualNetworkFeedbackReconcilerTask) handleDelete() {
	if t.object.Status.Phase == v1alpha1.VirtualNetworkPhaseFailed {
		t.virtualNetwork.GetStatus().SetState(privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_FAILED)
		return
	}
	t.virtualNetwork.GetStatus().SetState(privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_PENDING)
}

func (t *virtualNetworkFeedbackReconcilerTask) syncPhase(ctx context.Context) {
	switch t.object.Status.Phase {
	case v1alpha1.VirtualNetworkPhaseProgressing:
		t.syncPhaseProgressing()
	case v1alpha1.VirtualNetworkPhaseFailed:
		t.syncPhaseFailed()
	case v1alpha1.VirtualNetworkPhaseReady:
		t.syncPhaseReady()
	case v1alpha1.VirtualNetworkPhaseDeleting:
		t.syncPhaseDeleting()
	default:
		log := ctrllog.FromContext(ctx)
		log.Info(
			"Unknown phase, will ignore it",
			"phase", t.object.Status.Phase,
		)
	}
}

func (t *virtualNetworkFeedbackReconcilerTask) syncPhaseProgressing() {
	t.virtualNetwork.GetStatus().SetState(privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_PENDING)
}

func (t *virtualNetworkFeedbackReconcilerTask) syncPhaseFailed() {
	t.virtualNetwork.GetStatus().SetState(privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_FAILED)
}

func (t *virtualNetworkFeedbackReconcilerTask) syncPhaseReady() {
	virtualNetworkStatus := t.virtualNetwork.GetStatus()
	virtualNetworkStatus.SetState(privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_READY)
}

func (t *virtualNetworkFeedbackReconcilerTask) syncPhaseDeleting() {
	// Deleting state maps to PENDING as deletion is in progress
	t.virtualNetwork.GetStatus().SetState(privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_PENDING)
}
