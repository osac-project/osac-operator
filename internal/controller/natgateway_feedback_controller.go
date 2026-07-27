/*
Copyright (c) 2026 Red Hat Inc.

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
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	clnt "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	privatev1 "github.com/osac-project/osac-operator/internal/api/osac/private/v1"
)

// NATGatewayFeedbackReconciler sends updates to the fulfillment service.
type NATGatewayFeedbackReconciler struct {
	hubClient           clnt.Client
	natGatewaysClient   privatev1.NATGatewaysClient
	networkingNamespace string
}

// natGatewayFeedbackReconcilerTask contains data that is used for the reconciliation of a specific
// NATGateway, so there is less need to pass around as function parameters.
type natGatewayFeedbackReconcilerTask struct {
	r          *NATGatewayFeedbackReconciler
	object     *v1alpha1.NATGateway
	natGateway *privatev1.NATGateway
}

// NewNATGatewayFeedbackReconciler creates a reconciler that sends to the fulfillment service
// updates about NAT gateways.
func NewNATGatewayFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, networkingNamespace string) *NATGatewayFeedbackReconciler {
	return &NATGatewayFeedbackReconciler{
		hubClient:           hubClient,
		natGatewaysClient:   privatev1.NewNATGatewaysClient(grpcConn),
		networkingNamespace: networkingNamespace,
	}
}

// SetupWithManager adds the reconciler to the controller manager.
func (r *NATGatewayFeedbackReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	localMgr := mgr.GetLocalManager()
	if localMgr == nil {
		return fmt.Errorf("local manager is nil")
	}

	return ctrl.NewControllerManagedBy(localMgr).
		Named("natgateway-feedback").
		For(&v1alpha1.NATGateway{}, builder.WithPredicates(NetworkingNamespacePredicate(r.networkingNamespace))).
		Complete(r)
}

// Reconcile is the implementation of the reconciler interface.
func (r *NATGatewayFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	log := ctrllog.FromContext(ctx)

	// Step 1: Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &v1alpha1.NATGateway{}
	err = r.hubClient.Get(ctx, request.NamespacedName, object)
	if err != nil {
		err = clnt.IgnoreNotFound(err)
		return //nolint:nakedret
	}

	// Step 2: Get the identifier of the NAT gateway from the labels. If this isn't present it means
	// that the object wasn't created by the fulfillment service, so we ignore it.
	natGatewayID, ok := object.Labels[osacNATGatewayIDLabel]
	if !ok {
		if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacNATGatewayFeedbackFinalizer) {
			log.Info("CR without NAT gateway ID label is being deleted, removing feedback finalizer")
			if controllerutil.RemoveFinalizer(object, osacNATGatewayFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
			}
			return result, err
		}
		log.Info(
			"There is no label containing the NAT gateway identifier, will ignore it",
			"label", osacNATGatewayIDLabel,
		)
		return result, err
	}

	// Step 3: Fetch the NAT gateway from the fulfillment service so we can compare before/after.
	natGateway, err := r.fetchNATGateway(ctx, natGatewayID)
	if err != nil {
		if !object.DeletionTimestamp.IsZero() && status.Code(err) == codes.NotFound {
			log.Info("NATGateway record not found during deletion, removing feedback finalizer", "natGatewayID", natGatewayID)
			if controllerutil.RemoveFinalizer(object, osacNATGatewayFeedbackFinalizer) {
				return result, r.hubClient.Update(ctx, object)
			}
			return result, nil
		}
		return result, err
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later
	// compare the before and after values and save only the objects that have changed.
	t := &natGatewayFeedbackReconcilerTask{
		r:          r,
		object:     object,
		natGateway: clone(natGateway),
	}

	// Step 4: Sync CR state to the fulfillment service record.
	if object.DeletionTimestamp.IsZero() {
		err = t.handleUpdate(ctx)
	} else {
		t.handleDelete()
	}
	if err != nil {
		return result, err
	}

	// Step 5: Persist synced state to the fulfillment service.
	err = r.saveNATGateway(ctx, natGateway, t.natGateway)
	if err != nil {
		return result, err
	}

	// Step 6: Handle finalizer removal and signal for deletions.
	if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacNATGatewayFeedbackFinalizer) {
		if len(object.GetFinalizers()) == 1 {
			log.Info(
				"Feedback finalizer is last remaining, removing finalizer and signaling",
				"natGatewayID", natGatewayID,
			)
			if controllerutil.RemoveFinalizer(object, osacNATGatewayFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
				if err != nil {
					return result, err
				}
			}
			_, signalErr := r.natGatewaysClient.Signal(ctx, privatev1.NATGatewaysSignalRequest_builder{
				Id: natGatewayID,
			}.Build())
			if signalErr != nil {
				log.Error(
					signalErr,
					"Failed to signal fulfillment service, periodic sync will handle cleanup",
					"natGatewayID", natGatewayID,
				)
			}
		} else {
			log.Info(
				"Other finalizers still present, waiting",
				"finalizers", object.GetFinalizers(),
			)
		}
	}

	return result, err
}

func (r *NATGatewayFeedbackReconciler) fetchNATGateway(ctx context.Context, id string) (natGateway *privatev1.NATGateway, err error) {
	response, err := r.natGatewaysClient.Get(ctx, privatev1.NATGatewaysGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		return
	}
	natGateway = response.GetObject()
	if !natGateway.HasSpec() {
		natGateway.SetSpec(&privatev1.NATGatewaySpec{})
	}
	if !natGateway.HasStatus() {
		natGateway.SetStatus(&privatev1.NATGatewayStatus{})
	}
	return
}

func (r *NATGatewayFeedbackReconciler) saveNATGateway(ctx context.Context, before, after *privatev1.NATGateway) error {
	log := ctrllog.FromContext(ctx)

	if !equal(after, before) {
		log.Info(
			"Updating NAT gateway",
			"before", before,
			"after", after,
		)
		_, err := r.natGatewaysClient.Update(ctx, privatev1.NATGatewaysUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *natGatewayFeedbackReconcilerTask) handleUpdate(ctx context.Context) error {
	if controllerutil.AddFinalizer(t.object, osacNATGatewayFeedbackFinalizer) {
		if err := t.r.hubClient.Update(ctx, t.object); err != nil {
			return err
		}
	}
	t.syncPhase(ctx)
	return nil
}

func (t *natGatewayFeedbackReconcilerTask) handleDelete() {
	if t.object.Status.Phase == v1alpha1.NATGatewayPhaseFailed {
		t.natGateway.GetStatus().SetState(privatev1.NATGatewayState_NAT_GATEWAY_STATE_FAILED)
		return
	}
	t.natGateway.GetStatus().SetState(privatev1.NATGatewayState_NAT_GATEWAY_STATE_DELETING)
}

func (t *natGatewayFeedbackReconcilerTask) syncPhase(ctx context.Context) {
	switch t.object.Status.Phase {
	case v1alpha1.NATGatewayPhaseProgressing:
		t.natGateway.GetStatus().SetState(privatev1.NATGatewayState_NAT_GATEWAY_STATE_PENDING)
	case v1alpha1.NATGatewayPhaseFailed:
		t.natGateway.GetStatus().SetState(privatev1.NATGatewayState_NAT_GATEWAY_STATE_FAILED)
	case v1alpha1.NATGatewayPhaseReady:
		t.natGateway.GetStatus().SetState(privatev1.NATGatewayState_NAT_GATEWAY_STATE_READY)
	case v1alpha1.NATGatewayPhaseDeleting:
		t.natGateway.GetStatus().SetState(privatev1.NATGatewayState_NAT_GATEWAY_STATE_DELETING)
	default:
		log := ctrllog.FromContext(ctx)
		log.Info(
			"Unknown phase, will ignore it",
			"phase", t.object.Status.Phase,
		)
	}
}
