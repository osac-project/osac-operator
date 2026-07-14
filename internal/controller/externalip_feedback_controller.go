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
	"errors"
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

var ErrExternalIPNotFound = errors.New("external IP not found in fulfillment service")

// ExternalIPFeedbackReconciler sends updates to the fulfillment service.
type ExternalIPFeedbackReconciler struct {
	hubClient           clnt.Client
	publicIPsClient     privatev1.ExternalIPsClient
	networkingNamespace string
}

type externalIPFeedbackReconcilerTask struct {
	r        *ExternalIPFeedbackReconciler
	object   *v1alpha1.ExternalIP
	publicIP *privatev1.ExternalIP
}

// NewExternalIPFeedbackReconciler creates a reconciler that sends to the fulfillment service updates about external IPs.
func NewExternalIPFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, networkingNamespace string) *ExternalIPFeedbackReconciler {
	return &ExternalIPFeedbackReconciler{
		hubClient:           hubClient,
		publicIPsClient:     privatev1.NewExternalIPsClient(grpcConn),
		networkingNamespace: networkingNamespace,
	}
}

// SetupWithManager adds the reconciler to the controller manager.
func (r *ExternalIPFeedbackReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	localMgr := mgr.GetLocalManager()
	if localMgr == nil {
		return fmt.Errorf("local manager is nil")
	}

	return ctrl.NewControllerManagedBy(localMgr).
		Named("externalip-feedback").
		For(&v1alpha1.ExternalIP{}, builder.WithPredicates(NetworkingNamespacePredicate(r.networkingNamespace))).
		Complete(r)
}

// Reconcile is the implementation of the reconciler interface.
func (r *ExternalIPFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Step 1: Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &v1alpha1.ExternalIP{}
	if err := r.hubClient.Get(ctx, request.NamespacedName, object); err != nil {
		return ctrl.Result{}, clnt.IgnoreNotFound(err)
	}

	// Step 2: Get the identifier of the external IP from the labels. If this isn't present it means that the object
	// wasn't created by the fulfillment service, so we ignore it.
	publicIPID, ok := object.Labels[osacExternalIPIDLabel]
	if !ok {
		if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacExternalIPFeedbackFinalizer) {
			log.Info("CR without external IP ID label is being deleted, removing feedback finalizer")
			if controllerutil.RemoveFinalizer(object, osacExternalIPFeedbackFinalizer) {
				return ctrl.Result{}, r.hubClient.Update(ctx, object)
			}
		}
		log.Info(
			"There is no label containing the external IP identifier, will ignore it",
			"label", osacExternalIPIDLabel,
		)
		return ctrl.Result{}, nil
	}

	// Step 3: Fetch the external IP from the fulfillment service so we can compare before/after.
	publicIP, err := r.fetchExternalIP(ctx, publicIPID)
	if err != nil {
		if !object.DeletionTimestamp.IsZero() && errors.Is(err, ErrExternalIPNotFound) {
			log.Info("ExternalIP record not found during deletion, removing feedback finalizer", "publicIPID", publicIPID)
			if controllerutil.RemoveFinalizer(object, osacExternalIPFeedbackFinalizer) {
				return ctrl.Result{}, r.hubClient.Update(ctx, object)
			}
		}
		return ctrl.Result{}, err
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later compare the
	// before and after values and save only the objects that have changed.
	t := &externalIPFeedbackReconcilerTask{
		r:        r,
		object:   object,
		publicIP: clone(publicIP),
	}

	// Step 4: Sync CR state to the fulfillment service record.
	if object.DeletionTimestamp.IsZero() {
		if err := t.handleUpdate(ctx); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		t.handleDelete()
	}

	// Step 5: Persist synced state to the fulfillment service.
	if err := r.saveExternalIP(ctx, publicIP, t.publicIP); err != nil {
		return ctrl.Result{}, err
	}

	// Step 6: Handle finalizer removal and signal for deletions.
	if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacExternalIPFeedbackFinalizer) {
		if len(object.GetFinalizers()) == 1 {
			log.Info(
				"Feedback finalizer is last remaining, removing finalizer and signaling",
				"publicIPID", publicIPID,
			)
			if controllerutil.RemoveFinalizer(object, osacExternalIPFeedbackFinalizer) {
				if err := r.hubClient.Update(ctx, object); err != nil {
					return ctrl.Result{}, err
				}
			}
			_, signalErr := r.publicIPsClient.Signal(ctx, privatev1.ExternalIPsSignalRequest_builder{
				Id: publicIPID,
			}.Build())
			if signalErr != nil {
				log.Error(
					signalErr,
					"Failed to signal fulfillment service, periodic sync will handle cleanup",
					"publicIPID", publicIPID,
				)
			}
		} else {
			log.Info(
				"Other finalizers still present, waiting",
				"finalizers", object.GetFinalizers(),
			)
		}
	}

	return ctrl.Result{}, nil
}

func (r *ExternalIPFeedbackReconciler) fetchExternalIP(ctx context.Context, id string) (*privatev1.ExternalIP, error) {
	response, err := r.publicIPsClient.Get(ctx, privatev1.ExternalIPsGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("%w: %w", ErrExternalIPNotFound, err)
		}
		return nil, err
	}
	publicIP := response.GetObject()
	if publicIP == nil {
		return nil, fmt.Errorf("%w: response contained nil object", ErrExternalIPNotFound)
	}
	if !publicIP.HasSpec() {
		publicIP.SetSpec(&privatev1.ExternalIPSpec{})
	}
	if !publicIP.HasStatus() {
		publicIP.SetStatus(&privatev1.ExternalIPStatus{})
	}
	return publicIP, nil
}

func (r *ExternalIPFeedbackReconciler) saveExternalIP(ctx context.Context, before, after *privatev1.ExternalIP) error {
	log := ctrllog.FromContext(ctx)

	if !equal(after, before) {
		log.Info(
			"Updating external IP",
			"before", before,
			"after", after,
		)
		_, err := r.publicIPsClient.Update(ctx, privatev1.ExternalIPsUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *externalIPFeedbackReconcilerTask) handleUpdate(ctx context.Context) error {
	if controllerutil.AddFinalizer(t.object, osacExternalIPFeedbackFinalizer) {
		if err := t.r.hubClient.Update(ctx, t.object); err != nil {
			return err
		}
	}
	t.syncState(ctx)
	t.syncAddress()
	return nil
}

func (t *externalIPFeedbackReconcilerTask) handleDelete() {
	if t.object.Status.State == v1alpha1.ExternalIPStateFailed {
		t.publicIP.GetStatus().SetState(privatev1.ExternalIPState_EXTERNAL_IP_STATE_FAILED)
		return
	}
	t.publicIP.GetStatus().SetState(privatev1.ExternalIPState_EXTERNAL_IP_STATE_DELETING)
}

func (t *externalIPFeedbackReconcilerTask) syncState(ctx context.Context) {
	switch t.object.Status.State {
	case v1alpha1.ExternalIPStatePending:
		t.publicIP.GetStatus().SetState(privatev1.ExternalIPState_EXTERNAL_IP_STATE_PENDING)
	case v1alpha1.ExternalIPStateAllocated:
		t.publicIP.GetStatus().SetState(privatev1.ExternalIPState_EXTERNAL_IP_STATE_ALLOCATED)
	case v1alpha1.ExternalIPStateFailed:
		t.publicIP.GetStatus().SetState(privatev1.ExternalIPState_EXTERNAL_IP_STATE_FAILED)
	default:
		log := ctrllog.FromContext(ctx)
		log.Info("Unknown state, will ignore it", "state", t.object.Status.State)
	}
}

func (t *externalIPFeedbackReconcilerTask) syncAddress() {
	if t.object.Status.Address != "" {
		t.publicIP.GetStatus().SetAddress(t.object.Status.Address)
	}
}
