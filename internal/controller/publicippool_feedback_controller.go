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

// PublicIPPoolFeedbackReconciler watches PublicIPPool CRs on the hub cluster and reports their phase and capacity back
// to the fulfillment service via gRPC. This is the reverse channel: the provisioning controller drives the CR forward,
// while this controller keeps the fulfillment service in sync with the CR's current state.
type PublicIPPoolFeedbackReconciler struct {
	hubClient           clnt.Client
	publicIPPoolClient  privatev1.PublicIPPoolsClient
	networkingNamespace string
}

// publicIPPoolFeedbackReconcilerTask is a per-reconciliation scratch pad. It holds a clone of the proto object so we
// can mutate it freely and then diff against the original to decide whether a gRPC Update is actually needed, avoiding
// unnecessary API calls when nothing changed.
type publicIPPoolFeedbackReconcilerTask struct {
	r            *PublicIPPoolFeedbackReconciler
	object       *v1alpha1.PublicIPPool
	publicIPPool *privatev1.PublicIPPool
}

// NewPublicIPPoolFeedbackReconciler creates a reconciler that sends to the fulfillment service updates about
// public IP pools.
func NewPublicIPPoolFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, networkingNamespace string) *PublicIPPoolFeedbackReconciler {
	return &PublicIPPoolFeedbackReconciler{
		hubClient:           hubClient,
		publicIPPoolClient:  privatev1.NewPublicIPPoolsClient(grpcConn),
		networkingNamespace: networkingNamespace,
	}
}

// SetupWithManager registers this controller with controller-runtime. Named("publicippool-feedback") distinguishes it
// from the provisioning controller, which also watches PublicIPPool CRs. The namespace predicate limits reconciliation
// to CRs in the networking namespace.
func (r *PublicIPPoolFeedbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("publicippool-feedback").
		For(&v1alpha1.PublicIPPool{}, builder.WithPredicates(NetworkingNamespacePredicate(r.networkingNamespace))).
		Complete(r)
}

// Reconcile syncs the PublicIPPool CR's phase and capacity to the fulfillment service. The flow has six steps:
//  1. Fetch the CR (exit if deleted between event and processing)
//  2. Check for the fulfillment-service ID label (skip manually-created CRs)
//  3. Fetch the current proto record from the fulfillment service (handle NotFound during deletion)
//  4. Map CR state to proto state via handleUpdate/handleDelete
//  5. Persist changes via gRPC Update (only if proto.Equal detects a diff)
//  6. On deletion: remove finalizer and Signal the fulfillment service to re-reconcile immediately
func (r *PublicIPPoolFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	log := ctrllog.FromContext(ctx)

	// Step 1: Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &v1alpha1.PublicIPPool{}
	err = r.hubClient.Get(ctx, request.NamespacedName, object)
	if err != nil {
		err = clnt.IgnoreNotFound(err)
		return //nolint:nakedret
	}

	// Step 2: Get the identifier of the public IP pool from the labels. CRs created by the fulfillment service carry
	// this label; manually-created CRs don't, so we skip them.
	publicIPPoolID, ok := object.Labels[osacPublicIPPoolIDLabel]
	if !ok {
		// If being deleted and somehow has our finalizer, remove it to unblock deletion.
		if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacPublicIPPoolFeedbackFinalizer) {
			log.Info("CR without public IP pool ID label is being deleted, removing feedback finalizer")
			if controllerutil.RemoveFinalizer(object, osacPublicIPPoolFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
			}
			return result, err
		}
		log.Info(
			"There is no label containing the public IP pool identifier, will ignore it",
			"label", osacPublicIPPoolIDLabel,
		)
		return result, err
	}

	// Step 3: Fetch the public IP pool from the fulfillment service so we can compare before/after.
	publicIPPool, err := r.fetchPublicIPPool(ctx, publicIPPoolID)
	if err != nil {
		// If the fulfillment service record is already deleted during CR deletion, remove the feedback
		// finalizer and exit gracefully. This prevents the controller from blocking CR garbage collection
		// when the fulfillment service record has been deleted before K8s CR cleanup completes.
		if !object.DeletionTimestamp.IsZero() && status.Code(err) == codes.NotFound {
			log.Info(
				"Public IP pool record not found during deletion, removing feedback finalizer",
				"public_ip_pool_id", publicIPPoolID,
			)
			if controllerutil.RemoveFinalizer(object, osacPublicIPPoolFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
			}
			return result, err
		}
		return result, err
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later compare the
	// before and after values and save only the objects that have changed.
	t := &publicIPPoolFeedbackReconcilerTask{
		r:            r,
		object:       object,
		publicIPPool: clone(publicIPPool),
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
	err = r.savePublicIPPool(ctx, publicIPPool, t.publicIPPool)
	if err != nil {
		return result, err
	}

	// Step 6: Handle finalizer removal and signal for deletions. This must happen after step 5 so the
	// DELETING state is persisted before the CR is garbage collected.
	if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacPublicIPPoolFeedbackFinalizer) {
		if len(object.GetFinalizers()) == 1 {
			// We're the last finalizer. Remove it to trigger CR garbage collection, then signal the
			// fulfillment service to immediately re-reconcile (instead of waiting for periodic sync).
			log.Info(
				"Feedback finalizer is last remaining, removing finalizer and signaling",
				"publicIPPoolID", publicIPPoolID,
			)
			if controllerutil.RemoveFinalizer(object, osacPublicIPPoolFeedbackFinalizer) {
				err = r.hubClient.Update(ctx, object)
				if err != nil {
					return result, err
				}
			}
			_, signalErr := r.publicIPPoolClient.Signal(ctx, privatev1.PublicIPPoolsSignalRequest_builder{
				Id: publicIPPoolID,
			}.Build())
			if signalErr != nil {
				log.Error(
					signalErr,
					"Failed to signal fulfillment service, periodic sync will handle cleanup",
					"publicIPPoolID", publicIPPoolID,
				)
			}
		} else {
			// Other finalizers still present, another controller hasn't finished cleanup yet.
			log.Info(
				"Other finalizers still present, waiting",
				"finalizers", object.GetFinalizers(),
			)
		}
	}

	return result, err
}

// fetchPublicIPPool retrieves the proto record from the fulfillment service. It also initializes empty Spec/Status
// if the proto object doesn't have them, so downstream code can safely call SetState/SetTotal without nil checks.
func (r *PublicIPPoolFeedbackReconciler) fetchPublicIPPool(ctx context.Context, id string) (publicIPPool *privatev1.PublicIPPool, err error) {
	response, err := r.publicIPPoolClient.Get(ctx, privatev1.PublicIPPoolsGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		return
	}
	publicIPPool = response.GetObject()
	if !publicIPPool.HasSpec() {
		publicIPPool.SetSpec(&privatev1.PublicIPPoolSpec{})
	}
	if !publicIPPool.HasStatus() {
		publicIPPool.SetStatus(&privatev1.PublicIPPoolStatus{})
	}
	return
}

// savePublicIPPool persists the proto record only if it actually changed (compared via proto.Equal), avoiding
// unnecessary gRPC calls and audit log noise when the CR is reconciled but nothing is different.
func (r *PublicIPPoolFeedbackReconciler) savePublicIPPool(ctx context.Context, before, after *privatev1.PublicIPPool) error {
	log := ctrllog.FromContext(ctx)

	if !equal(after, before) {
		log.Info(
			"Updating public IP pool",
			"before", before,
			"after", after,
		)
		_, err := r.publicIPPoolClient.Update(ctx, privatev1.PublicIPPoolsUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

// handleUpdate is called when the CR is alive (not being deleted). It ensures our feedback finalizer is present so we
// get a chance to sync DELETING state before the CR disappears, then syncs both phase and capacity to the proto object.
func (t *publicIPPoolFeedbackReconcilerTask) handleUpdate(ctx context.Context) error {
	if controllerutil.AddFinalizer(t.object, osacPublicIPPoolFeedbackFinalizer) {
		if err := t.r.hubClient.Update(ctx, t.object); err != nil {
			return err
		}
	}
	t.syncPhase(ctx)
	t.syncCapacity()
	return nil
}

// handleDelete is called when the CR is being deleted. Only syncs deletion-related state (DELETING or DELETE_FAILED).
// Capacity is not synced during deletion because the pool is going away and the counts are no longer meaningful.
func (t *publicIPPoolFeedbackReconcilerTask) handleDelete() {
	if t.object.Status.Phase == v1alpha1.PublicIPPoolPhaseFailed {
		t.publicIPPool.GetStatus().SetState(privatev1.PublicIPPoolState_PUBLIC_IP_POOL_STATE_DELETE_FAILED)
		return
	}
	t.publicIPPool.GetStatus().SetState(privatev1.PublicIPPoolState_PUBLIC_IP_POOL_STATE_DELETING)
}

// syncPhase maps the CR's phase to the corresponding proto state. The default case logs and ignores unknown phases
// for forward compatibility if a new phase is added to the CRD before the feedback controller is updated.
func (t *publicIPPoolFeedbackReconcilerTask) syncPhase(ctx context.Context) {
	switch t.object.Status.Phase {
	case v1alpha1.PublicIPPoolPhaseProgressing:
		t.syncPhaseProgressing()
	case v1alpha1.PublicIPPoolPhaseFailed:
		t.syncPhaseFailed()
	case v1alpha1.PublicIPPoolPhaseReady:
		t.syncPhaseReady()
	case v1alpha1.PublicIPPoolPhaseDeleting:
		t.syncPhaseDeleting()
	default:
		log := ctrllog.FromContext(ctx)
		log.Info(
			"Unknown phase, will ignore it",
			"phase", t.object.Status.Phase,
		)
	}
}

// syncPhaseProgressing maps to PENDING. The fulfillment service uses PENDING (not PROGRESSING) for all networking
// resources, consistent with VirtualNetwork, Subnet, SecurityGroup, NetworkClass, and HostClass.
func (t *publicIPPoolFeedbackReconcilerTask) syncPhaseProgressing() {
	t.publicIPPool.GetStatus().SetState(privatev1.PublicIPPoolState_PUBLIC_IP_POOL_STATE_PENDING)
}

func (t *publicIPPoolFeedbackReconcilerTask) syncPhaseFailed() {
	t.publicIPPool.GetStatus().SetState(privatev1.PublicIPPoolState_PUBLIC_IP_POOL_STATE_FAILED)
}

func (t *publicIPPoolFeedbackReconcilerTask) syncPhaseReady() {
	t.publicIPPool.GetStatus().SetState(privatev1.PublicIPPoolState_PUBLIC_IP_POOL_STATE_READY)
}

func (t *publicIPPoolFeedbackReconcilerTask) syncPhaseDeleting() {
	t.publicIPPool.GetStatus().SetState(privatev1.PublicIPPoolState_PUBLIC_IP_POOL_STATE_DELETING)
}

// syncCapacity copies the capacity fields (total, allocated, available) from the CR status to the proto status.
// Unlike SecurityGroup/Subnet/VirtualNetwork, PublicIPPool tracks address space usage. The fulfillment service uses
// these numbers to report pool utilization and enforce allocation limits.
func (t *publicIPPoolFeedbackReconcilerTask) syncCapacity() {
	poolStatus := t.publicIPPool.GetStatus()
	poolStatus.SetTotal(t.object.Status.Total)
	poolStatus.SetAllocated(t.object.Status.Allocated)
	poolStatus.SetAvailable(t.object.Status.Available)
}
