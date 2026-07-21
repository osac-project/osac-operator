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

var ErrExternalIPAttachmentNotFound = errors.New("external IP attachment not found in fulfillment service")

type ExternalIPAttachmentFeedbackReconciler struct {
	hubClient                   clnt.Client
	externalIPAttachmentsClient privatev1.ExternalIPAttachmentsClient
	externalIPsClient           privatev1.ExternalIPsClient
	networkingNamespace         string
}

type externalIPAttachmentFeedbackReconcilerTask struct {
	r                    *ExternalIPAttachmentFeedbackReconciler
	object               *v1alpha1.ExternalIPAttachment
	externalIPAttachment *privatev1.ExternalIPAttachment
}

func NewExternalIPAttachmentFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, networkingNamespace string) *ExternalIPAttachmentFeedbackReconciler {
	return &ExternalIPAttachmentFeedbackReconciler{
		hubClient:                   hubClient,
		externalIPAttachmentsClient: privatev1.NewExternalIPAttachmentsClient(grpcConn),
		externalIPsClient:           privatev1.NewExternalIPsClient(grpcConn),
		networkingNamespace:         networkingNamespace,
	}
}

func (r *ExternalIPAttachmentFeedbackReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	localMgr := mgr.GetLocalManager()
	if localMgr == nil {
		return fmt.Errorf("local manager is nil")
	}

	return ctrl.NewControllerManagedBy(localMgr).
		Named("externalipattachment-feedback").
		For(&v1alpha1.ExternalIPAttachment{}, builder.WithPredicates(NetworkingNamespacePredicate(r.networkingNamespace))).
		Complete(r)
}

func (r *ExternalIPAttachmentFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	object := &v1alpha1.ExternalIPAttachment{}
	if err := r.hubClient.Get(ctx, request.NamespacedName, object); err != nil {
		return ctrl.Result{}, clnt.IgnoreNotFound(err)
	}

	attachmentID, ok := object.Labels[osacExternalIPAttachmentIDLabel]
	if !ok {
		if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacExternalIPAttachmentFeedbackFinalizer) {
			log.Info("CR without external IP attachment ID label is being deleted, removing feedback finalizer")
			if controllerutil.RemoveFinalizer(object, osacExternalIPAttachmentFeedbackFinalizer) {
				return ctrl.Result{}, r.hubClient.Update(ctx, object)
			}
		}
		log.Info(
			"There is no label containing the external IP attachment identifier, will ignore it",
			"label", osacExternalIPAttachmentIDLabel,
		)
		return ctrl.Result{}, nil
	}

	externalIPAttachment, err := r.fetchExternalIPAttachment(ctx, attachmentID)
	if err != nil {
		if !object.DeletionTimestamp.IsZero() && errors.Is(err, ErrExternalIPAttachmentNotFound) {
			log.Info("ExternalIPAttachment record not found during deletion, removing feedback finalizer", "attachmentID", attachmentID)
			if controllerutil.RemoveFinalizer(object, osacExternalIPAttachmentFeedbackFinalizer) {
				return ctrl.Result{}, r.hubClient.Update(ctx, object)
			}
		}
		return ctrl.Result{}, err
	}

	t := &externalIPAttachmentFeedbackReconcilerTask{
		r:                    r,
		object:               object,
		externalIPAttachment: clone(externalIPAttachment),
	}

	if object.DeletionTimestamp.IsZero() {
		if err := t.handleUpdate(ctx); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		t.handleDelete()
	}

	if err := r.saveExternalIPAttachment(ctx, externalIPAttachment, t.externalIPAttachment); err != nil {
		return ctrl.Result{}, err
	}

	if !object.DeletionTimestamp.IsZero() {
		if err := t.syncAttachedOnParentExternalIP(ctx, false); err != nil {
			log.Error(err, "Failed to clear attached on parent ExternalIP, will retry")
			return ctrl.Result{}, err
		}
	}

	if !object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, osacExternalIPAttachmentFeedbackFinalizer) {
		if len(object.GetFinalizers()) == 1 {
			log.Info(
				"Feedback finalizer is last remaining, removing finalizer and signaling",
				"attachmentID", attachmentID,
			)
			if controllerutil.RemoveFinalizer(object, osacExternalIPAttachmentFeedbackFinalizer) {
				if err := r.hubClient.Update(ctx, object); err != nil {
					return ctrl.Result{}, err
				}
			}
			_, signalErr := r.externalIPAttachmentsClient.Signal(ctx, privatev1.ExternalIPAttachmentsSignalRequest_builder{
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

func (r *ExternalIPAttachmentFeedbackReconciler) fetchExternalIPAttachment(ctx context.Context, id string) (*privatev1.ExternalIPAttachment, error) {
	response, err := r.externalIPAttachmentsClient.Get(ctx, privatev1.ExternalIPAttachmentsGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("%w: %w", ErrExternalIPAttachmentNotFound, err)
		}
		return nil, err
	}
	obj := response.GetObject()
	if obj == nil {
		return nil, fmt.Errorf("%w: response contained nil object", ErrExternalIPAttachmentNotFound)
	}
	if !obj.HasSpec() {
		obj.SetSpec(&privatev1.ExternalIPAttachmentSpec{})
	}
	if !obj.HasStatus() {
		obj.SetStatus(&privatev1.ExternalIPAttachmentStatus{})
	}
	return obj, nil
}

func (r *ExternalIPAttachmentFeedbackReconciler) saveExternalIPAttachment(ctx context.Context, before, after *privatev1.ExternalIPAttachment) error {
	log := ctrllog.FromContext(ctx)

	if !equal(after, before) {
		log.Info(
			"Updating external IP attachment",
			"before", before,
			"after", after,
		)
		_, err := r.externalIPAttachmentsClient.Update(ctx, privatev1.ExternalIPAttachmentsUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *externalIPAttachmentFeedbackReconcilerTask) handleUpdate(ctx context.Context) error {
	if controllerutil.AddFinalizer(t.object, osacExternalIPAttachmentFeedbackFinalizer) {
		if err := t.r.hubClient.Update(ctx, t.object); err != nil {
			return err
		}
	}
	t.syncState(ctx)
	t.syncAddress(ctx)

	if t.object.Status.Phase == v1alpha1.ExternalIPAttachmentPhaseReady {
		if err := t.syncAttachedOnParentExternalIP(ctx, true); err != nil {
			ctrllog.FromContext(ctx).Error(err, "Failed to set attached on parent ExternalIP, will retry")
			return err
		}
	}

	return nil
}

func (t *externalIPAttachmentFeedbackReconcilerTask) handleDelete() {
	if t.object.Status.Phase == v1alpha1.ExternalIPAttachmentPhaseFailed {
		t.externalIPAttachment.GetStatus().SetState(privatev1.ExternalIPAttachmentState_EXTERNAL_IP_ATTACHMENT_STATE_FAILED)
		return
	}
	t.externalIPAttachment.GetStatus().SetState(privatev1.ExternalIPAttachmentState_EXTERNAL_IP_ATTACHMENT_STATE_DELETING)
}

func (t *externalIPAttachmentFeedbackReconcilerTask) syncState(ctx context.Context) {
	switch t.object.Status.Phase {
	case v1alpha1.ExternalIPAttachmentPhaseProgressing:
		t.externalIPAttachment.GetStatus().SetState(privatev1.ExternalIPAttachmentState_EXTERNAL_IP_ATTACHMENT_STATE_PENDING)
	case v1alpha1.ExternalIPAttachmentPhaseReady:
		t.externalIPAttachment.GetStatus().SetState(privatev1.ExternalIPAttachmentState_EXTERNAL_IP_ATTACHMENT_STATE_READY)
	case v1alpha1.ExternalIPAttachmentPhaseFailed:
		t.externalIPAttachment.GetStatus().SetState(privatev1.ExternalIPAttachmentState_EXTERNAL_IP_ATTACHMENT_STATE_FAILED)
	default:
		log := ctrllog.FromContext(ctx)
		log.Info("Unknown phase, will ignore it", "phase", t.object.Status.Phase)
	}
}

func (t *externalIPAttachmentFeedbackReconcilerTask) syncAddress(ctx context.Context) {
	externalIPID := t.externalIPAttachment.GetSpec().GetExternalIp()
	if externalIPID == "" {
		return
	}
	response, err := t.r.externalIPsClient.Get(ctx, privatev1.ExternalIPsGetRequest_builder{
		Id: externalIPID,
	}.Build())
	if err != nil {
		ctrllog.FromContext(ctx).Error(err, "Failed to fetch parent ExternalIP for address sync", "externalIPID", externalIPID)
		return
	}
	obj := response.GetObject()
	if obj == nil || !obj.HasStatus() {
		return
	}
	if addr := obj.GetStatus().GetAddress(); addr != "" {
		t.externalIPAttachment.GetStatus().SetExternalIpAddress(addr)
	}
}

func (t *externalIPAttachmentFeedbackReconcilerTask) syncAttachedOnParentExternalIP(ctx context.Context, attached bool) error {
	externalIPID := t.externalIPAttachment.GetSpec().GetExternalIp()
	if externalIPID == "" {
		return nil
	}

	response, err := t.r.externalIPsClient.Get(ctx, privatev1.ExternalIPsGetRequest_builder{
		Id: externalIPID,
	}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			ctrllog.FromContext(ctx).Info("Parent ExternalIP not found, skipping attached sync", "externalIPID", externalIPID)
			return nil
		}
		return err
	}

	publicIP := response.GetObject()
	if publicIP == nil {
		return nil
	}
	if !publicIP.HasStatus() {
		publicIP.SetStatus(&privatev1.ExternalIPStatus{})
	}

	if publicIP.GetStatus().GetAttached() == attached {
		return nil
	}

	publicIP.GetStatus().SetAttached(attached)
	_, err = t.r.externalIPsClient.Update(ctx, privatev1.ExternalIPsUpdateRequest_builder{
		Object: publicIP,
	}.Build())
	return err
}
