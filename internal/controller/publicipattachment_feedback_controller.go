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

var ErrPublicIPAttachmentNotFound = errors.New("public IP attachment not found in fulfillment service")

type PublicIPAttachmentFeedbackReconciler struct {
	hubClient                 clnt.Client
	publicIPAttachmentsClient privatev1.PublicIPAttachmentsClient
	publicIPsClient           privatev1.PublicIPsClient
	networkingNamespace       string
}

type publicIPAttachmentFeedbackReconcilerTask struct {
	r                  *PublicIPAttachmentFeedbackReconciler
	object             *v1alpha1.PublicIPAttachment
	publicIPAttachment *privatev1.PublicIPAttachment
}

func NewPublicIPAttachmentFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, networkingNamespace string) *PublicIPAttachmentFeedbackReconciler {
	return &PublicIPAttachmentFeedbackReconciler{
		hubClient:                 hubClient,
		publicIPAttachmentsClient: privatev1.NewPublicIPAttachmentsClient(grpcConn),
		publicIPsClient:           privatev1.NewPublicIPsClient(grpcConn),
		networkingNamespace:       networkingNamespace,
	}
}

func (r *PublicIPAttachmentFeedbackReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	localMgr := mgr.GetLocalManager()
	if localMgr == nil {
		return fmt.Errorf("local manager is nil")
	}

	return ctrl.NewControllerManagedBy(localMgr).
		Named("publicipattachment-feedback").
		For(&v1alpha1.PublicIPAttachment{}, builder.WithPredicates(NetworkingNamespacePredicate(r.networkingNamespace))).
		Complete(r)
}

func (r *PublicIPAttachmentFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	object := &v1alpha1.PublicIPAttachment{}
	if err := r.hubClient.Get(ctx, request.NamespacedName, object); err != nil {
		return ctrl.Result{}, clnt.IgnoreNotFound(err)
	}

	attachmentID, ok := object.Labels[osacPublicIPAttachmentIDLabel]
	if !ok {
		if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacPublicIPAttachmentFeedbackFinalizer) {
			log.Info("CR without public IP attachment ID label is being deleted, removing feedback finalizer")
			if controllerutil.RemoveFinalizer(object, osacPublicIPAttachmentFeedbackFinalizer) {
				return ctrl.Result{}, r.hubClient.Update(ctx, object)
			}
		}
		log.Info(
			"There is no label containing the public IP attachment identifier, will ignore it",
			"label", osacPublicIPAttachmentIDLabel,
		)
		return ctrl.Result{}, nil
	}

	publicIPAttachment, err := r.fetchPublicIPAttachment(ctx, attachmentID)
	if err != nil {
		if !object.DeletionTimestamp.IsZero() && errors.Is(err, ErrPublicIPAttachmentNotFound) {
			log.Info("PublicIPAttachment record not found during deletion, removing feedback finalizer", "attachmentID", attachmentID)
			if controllerutil.RemoveFinalizer(object, osacPublicIPAttachmentFeedbackFinalizer) {
				return ctrl.Result{}, r.hubClient.Update(ctx, object)
			}
		}
		return ctrl.Result{}, err
	}

	t := &publicIPAttachmentFeedbackReconcilerTask{
		r:                  r,
		object:             object,
		publicIPAttachment: clone(publicIPAttachment),
	}

	if object.DeletionTimestamp.IsZero() {
		if err := t.handleUpdate(ctx); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		t.handleDelete()
	}

	if err := r.savePublicIPAttachment(ctx, publicIPAttachment, t.publicIPAttachment); err != nil {
		return ctrl.Result{}, err
	}

	if !object.DeletionTimestamp.IsZero() {
		if err := t.syncAttachedOnParentPublicIP(ctx, false); err != nil {
			log.Error(err, "Failed to clear attached on parent PublicIP, will retry")
			return ctrl.Result{}, err
		}
	}

	if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacPublicIPAttachmentFeedbackFinalizer) {
		if len(object.GetFinalizers()) == 1 {
			log.Info(
				"Feedback finalizer is last remaining, removing finalizer and signaling",
				"attachmentID", attachmentID,
			)
			if controllerutil.RemoveFinalizer(object, osacPublicIPAttachmentFeedbackFinalizer) {
				if err := r.hubClient.Update(ctx, object); err != nil {
					return ctrl.Result{}, err
				}
			}
			_, signalErr := r.publicIPAttachmentsClient.Signal(ctx, privatev1.PublicIPAttachmentsSignalRequest_builder{
				Id: attachmentID,
			}.Build())
			if signalErr != nil {
				log.Error(
					signalErr,
					"Failed to signal fulfillment service, periodic sync will handle cleanup",
					"attachmentID", attachmentID,
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

func (r *PublicIPAttachmentFeedbackReconciler) fetchPublicIPAttachment(ctx context.Context, id string) (*privatev1.PublicIPAttachment, error) {
	response, err := r.publicIPAttachmentsClient.Get(ctx, privatev1.PublicIPAttachmentsGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("%w: %w", ErrPublicIPAttachmentNotFound, err)
		}
		return nil, err
	}
	obj := response.GetObject()
	if obj == nil {
		return nil, fmt.Errorf("%w: response contained nil object", ErrPublicIPAttachmentNotFound)
	}
	if !obj.HasSpec() {
		obj.SetSpec(&privatev1.PublicIPAttachmentSpec{})
	}
	if !obj.HasStatus() {
		obj.SetStatus(&privatev1.PublicIPAttachmentStatus{})
	}
	return obj, nil
}

func (r *PublicIPAttachmentFeedbackReconciler) savePublicIPAttachment(ctx context.Context, before, after *privatev1.PublicIPAttachment) error {
	log := ctrllog.FromContext(ctx)

	if !equal(after, before) {
		log.Info(
			"Updating public IP attachment",
			"before", before,
			"after", after,
		)
		_, err := r.publicIPAttachmentsClient.Update(ctx, privatev1.PublicIPAttachmentsUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *publicIPAttachmentFeedbackReconcilerTask) handleUpdate(ctx context.Context) error {
	if controllerutil.AddFinalizer(t.object, osacPublicIPAttachmentFeedbackFinalizer) {
		if err := t.r.hubClient.Update(ctx, t.object); err != nil {
			return err
		}
	}
	t.syncState(ctx)

	if t.object.Status.Phase == v1alpha1.PublicIPAttachmentPhaseReady {
		if err := t.syncAttachedOnParentPublicIP(ctx, true); err != nil {
			ctrllog.FromContext(ctx).Error(err, "Failed to set attached on parent PublicIP, will retry")
			return err
		}
	}

	return nil
}

func (t *publicIPAttachmentFeedbackReconcilerTask) handleDelete() {
	if t.object.Status.Phase == v1alpha1.PublicIPAttachmentPhaseFailed {
		t.publicIPAttachment.GetStatus().SetState(privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_FAILED)
		return
	}
	t.publicIPAttachment.GetStatus().SetState(privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_DELETING)
}

func (t *publicIPAttachmentFeedbackReconcilerTask) syncState(ctx context.Context) {
	switch t.object.Status.Phase {
	case v1alpha1.PublicIPAttachmentPhaseProgressing:
		t.publicIPAttachment.GetStatus().SetState(privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_PENDING)
	case v1alpha1.PublicIPAttachmentPhaseReady:
		t.publicIPAttachment.GetStatus().SetState(privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_READY)
	case v1alpha1.PublicIPAttachmentPhaseFailed:
		t.publicIPAttachment.GetStatus().SetState(privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_FAILED)
	default:
		log := ctrllog.FromContext(ctx)
		log.Info("Unknown phase, will ignore it", "phase", t.object.Status.Phase)
	}
}

func (t *publicIPAttachmentFeedbackReconcilerTask) syncAttachedOnParentPublicIP(ctx context.Context, attached bool) error {
	publicIPID := t.publicIPAttachment.GetSpec().GetPublicIp()
	if publicIPID == "" {
		return nil
	}

	response, err := t.r.publicIPsClient.Get(ctx, privatev1.PublicIPsGetRequest_builder{
		Id: publicIPID,
	}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			ctrllog.FromContext(ctx).Info("Parent PublicIP not found, skipping attached sync", "publicIPID", publicIPID)
			return nil
		}
		return err
	}

	publicIP := response.GetObject()
	if publicIP == nil {
		return nil
	}
	if !publicIP.HasStatus() {
		publicIP.SetStatus(&privatev1.PublicIPStatus{})
	}

	if publicIP.GetStatus().GetAttached() == attached {
		return nil
	}

	publicIP.GetStatus().SetAttached(attached)
	_, err = t.r.publicIPsClient.Update(ctx, privatev1.PublicIPsUpdateRequest_builder{
		Object: publicIP,
	}.Build())
	return err
}
