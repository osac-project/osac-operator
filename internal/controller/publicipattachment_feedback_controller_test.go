/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"net"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	privatev1 "github.com/osac-project/osac-operator/internal/api/osac/private/v1"
)

var _ = Describe("PublicIPAttachmentFeedbackController", func() {
	const (
		attachmentName      = "test-attachment"
		attachmentNamespace = "test-namespace"
		attachmentID        = "attachment-123"
		parentPublicIPID    = "publicip-456"
	)

	var (
		ctx                   context.Context
		k8sClient             client.Client
		mockAttachmentsServer *mockPublicIPAttachmentsServer
		mockPublicIPsServer2  *mockPublicIPsServerForAttachment
		reconciler            *PublicIPAttachmentFeedbackReconciler
		grpcServer            *grpc.Server
		listener              *bufconn.Listener
	)

	BeforeEach(func() {
		ctx = context.Background()

		scheme := runtime.NewScheme()
		Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()

		mockAttachmentsServer = &mockPublicIPAttachmentsServer{
			attachments: make(map[string]*privatev1.PublicIPAttachment),
			updates:     make([]*privatev1.PublicIPAttachment, 0),
			signals:     make([]string, 0),
		}
		mockPublicIPsServer2 = &mockPublicIPsServerForAttachment{
			publicIPs: make(map[string]*privatev1.PublicIP),
			updates:   make([]*privatev1.PublicIP, 0),
		}
		listener = bufconn.Listen(1024 * 1024)
		grpcServer = grpc.NewServer()
		privatev1.RegisterPublicIPAttachmentsServer(grpcServer, mockAttachmentsServer)
		privatev1.RegisterPublicIPsServer(grpcServer, mockPublicIPsServer2)

		go func() {
			_ = grpcServer.Serve(listener)
		}()

		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, s string) (net.Conn, error) {
				return listener.Dial()
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		Expect(err).NotTo(HaveOccurred())

		reconciler = NewPublicIPAttachmentFeedbackReconciler(k8sClient, conn, attachmentNamespace)
	})

	AfterEach(func() {
		if grpcServer != nil {
			grpcServer.Stop()
		}
		if listener != nil {
			_ = listener.Close()
		}
	})

	Context("when reconciling a PublicIPAttachment CR", func() {
		It("should sync Phase=Progressing to state=PENDING", func() {
			attachment := &privatev1.PublicIPAttachment{
				Id: attachmentID,
				Metadata: &privatev1.Metadata{
					Name: attachmentName,
				},
				Spec: &privatev1.PublicIPAttachmentSpec{},
				Status: &privatev1.PublicIPAttachmentStatus{
					State: privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_UNSPECIFIED,
				},
			}
			attachment.GetSpec().SetPublicIp(parentPublicIPID)
			mockAttachmentsServer.addAttachment(attachment)

			cr := &v1alpha1.PublicIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
					Labels: map[string]string{
						osacPublicIPAttachmentIDLabel: attachmentID,
					},
				},
				Spec: v1alpha1.PublicIPAttachmentSpec{
					PublicIP: "some-publicip",
				},
				Status: v1alpha1.PublicIPAttachmentStatus{
					Phase: v1alpha1.PublicIPAttachmentPhaseProgressing,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockAttachmentsServer.updates).To(HaveLen(1))
			Expect(mockAttachmentsServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_PENDING))

			updated := &v1alpha1.PublicIPAttachment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: attachmentName, Namespace: attachmentNamespace}, updated)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(updated, osacPublicIPAttachmentFeedbackFinalizer)).To(BeTrue())
		})

		It("should sync Phase=Ready to state=READY and set parent PublicIP attached=true", func() {
			attachment := &privatev1.PublicIPAttachment{
				Id: attachmentID,
				Metadata: &privatev1.Metadata{
					Name: attachmentName,
				},
				Spec: &privatev1.PublicIPAttachmentSpec{},
				Status: &privatev1.PublicIPAttachmentStatus{
					State: privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_PENDING,
				},
			}
			attachment.GetSpec().SetPublicIp(parentPublicIPID)
			mockAttachmentsServer.addAttachment(attachment)

			parentPublicIP := &privatev1.PublicIP{
				Id: parentPublicIPID,
				Metadata: &privatev1.Metadata{
					Name: "parent-publicip",
				},
				Spec:   &privatev1.PublicIPSpec{},
				Status: &privatev1.PublicIPStatus{},
			}
			mockPublicIPsServer2.addPublicIP(parentPublicIP)

			cr := &v1alpha1.PublicIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
					Labels: map[string]string{
						osacPublicIPAttachmentIDLabel: attachmentID,
					},
				},
				Spec: v1alpha1.PublicIPAttachmentSpec{
					PublicIP: "some-publicip",
				},
				Status: v1alpha1.PublicIPAttachmentStatus{
					Phase: v1alpha1.PublicIPAttachmentPhaseReady,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockAttachmentsServer.updates).To(HaveLen(1))
			Expect(mockAttachmentsServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_READY))

			Expect(mockPublicIPsServer2.updates).To(HaveLen(1))
			Expect(mockPublicIPsServer2.updates[0].GetStatus().GetAttached()).To(BeTrue())
		})

		It("should sync Phase=Failed to state=FAILED", func() {
			attachment := &privatev1.PublicIPAttachment{
				Id: attachmentID,
				Metadata: &privatev1.Metadata{
					Name: attachmentName,
				},
				Spec: &privatev1.PublicIPAttachmentSpec{},
				Status: &privatev1.PublicIPAttachmentStatus{
					State: privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_PENDING,
				},
			}
			attachment.GetSpec().SetPublicIp(parentPublicIPID)
			mockAttachmentsServer.addAttachment(attachment)

			cr := &v1alpha1.PublicIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
					Labels: map[string]string{
						osacPublicIPAttachmentIDLabel: attachmentID,
					},
				},
				Spec: v1alpha1.PublicIPAttachmentSpec{
					PublicIP: "some-publicip",
				},
				Status: v1alpha1.PublicIPAttachmentStatus{
					Phase: v1alpha1.PublicIPAttachmentPhaseFailed,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockAttachmentsServer.updates).To(HaveLen(1))
			Expect(mockAttachmentsServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_FAILED))
		})

		It("should sync state=DELETING during deletion and clear parent PublicIP attached", func() {
			attachment := &privatev1.PublicIPAttachment{
				Id: attachmentID,
				Metadata: &privatev1.Metadata{
					Name: attachmentName,
				},
				Spec: &privatev1.PublicIPAttachmentSpec{},
				Status: &privatev1.PublicIPAttachmentStatus{
					State: privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_READY,
				},
			}
			attachment.GetSpec().SetPublicIp(parentPublicIPID)
			mockAttachmentsServer.addAttachment(attachment)

			parentPublicIP := &privatev1.PublicIP{
				Id: parentPublicIPID,
				Metadata: &privatev1.Metadata{
					Name: "parent-publicip",
				},
				Spec:   &privatev1.PublicIPSpec{},
				Status: &privatev1.PublicIPStatus{},
			}
			parentPublicIP.GetStatus().SetAttached(true)
			mockPublicIPsServer2.addPublicIP(parentPublicIP)

			cr := &v1alpha1.PublicIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
					Labels: map[string]string{
						osacPublicIPAttachmentIDLabel: attachmentID,
					},
					Finalizers: []string{osacPublicIPAttachmentFeedbackFinalizer, osacPublicIPAttachmentFinalizer},
				},
				Spec: v1alpha1.PublicIPAttachmentSpec{
					PublicIP: "some-publicip",
				},
				Status: v1alpha1.PublicIPAttachmentStatus{
					Phase: v1alpha1.PublicIPAttachmentPhaseDeleting,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockAttachmentsServer.updates).To(HaveLen(1))
			Expect(mockAttachmentsServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_DELETING))

			Expect(mockPublicIPsServer2.updates).To(HaveLen(1))
			Expect(mockPublicIPsServer2.updates[0].GetStatus().GetAttached()).To(BeFalse())
		})

		It("should sync state=FAILED during deletion when phase is Failed", func() {
			attachment := &privatev1.PublicIPAttachment{
				Id: attachmentID,
				Metadata: &privatev1.Metadata{
					Name: attachmentName,
				},
				Spec: &privatev1.PublicIPAttachmentSpec{},
				Status: &privatev1.PublicIPAttachmentStatus{
					State: privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_READY,
				},
			}
			attachment.GetSpec().SetPublicIp(parentPublicIPID)
			mockAttachmentsServer.addAttachment(attachment)

			parentPublicIP := &privatev1.PublicIP{
				Id: parentPublicIPID,
				Metadata: &privatev1.Metadata{
					Name: "parent-publicip",
				},
				Spec:   &privatev1.PublicIPSpec{},
				Status: &privatev1.PublicIPStatus{},
			}
			mockPublicIPsServer2.addPublicIP(parentPublicIP)

			cr := &v1alpha1.PublicIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
					Labels: map[string]string{
						osacPublicIPAttachmentIDLabel: attachmentID,
					},
					Finalizers: []string{osacPublicIPAttachmentFeedbackFinalizer, osacPublicIPAttachmentFinalizer},
				},
				Spec: v1alpha1.PublicIPAttachmentSpec{
					PublicIP: "some-publicip",
				},
				Status: v1alpha1.PublicIPAttachmentStatus{
					Phase: v1alpha1.PublicIPAttachmentPhaseFailed,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockAttachmentsServer.updates).To(HaveLen(1))
			Expect(mockAttachmentsServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_FAILED))
		})

		It("should skip CRs without publicipattachment-uuid label", func() {
			cr := &v1alpha1.PublicIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
					Labels:    map[string]string{},
				},
				Spec: v1alpha1.PublicIPAttachmentSpec{
					PublicIP: "some-publicip",
				},
				Status: v1alpha1.PublicIPAttachmentStatus{
					Phase: v1alpha1.PublicIPAttachmentPhaseReady,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockAttachmentsServer.updates).To(BeEmpty())
		})

		It("should remove feedback finalizer when CR without ID label is being deleted", func() {
			cr := &v1alpha1.PublicIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name:       attachmentName,
					Namespace:  attachmentNamespace,
					Labels:     map[string]string{},
					Finalizers: []string{osacPublicIPAttachmentFeedbackFinalizer},
				},
				Spec: v1alpha1.PublicIPAttachmentSpec{
					PublicIP: "some-publicip",
				},
				Status: v1alpha1.PublicIPAttachmentStatus{
					Phase: v1alpha1.PublicIPAttachmentPhaseDeleting,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &v1alpha1.PublicIPAttachment{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: attachmentName, Namespace: attachmentNamespace}, updated)
			Expect(err).To(HaveOccurred())
		})

		It("should remove feedback finalizer when attachment record is NotFound during deletion", func() {
			cr := &v1alpha1.PublicIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
					Labels: map[string]string{
						osacPublicIPAttachmentIDLabel: attachmentID,
					},
					Finalizers: []string{osacPublicIPAttachmentFeedbackFinalizer},
				},
				Spec: v1alpha1.PublicIPAttachmentSpec{
					PublicIP: "some-publicip",
				},
				Status: v1alpha1.PublicIPAttachmentStatus{
					Phase: v1alpha1.PublicIPAttachmentPhaseDeleting,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockAttachmentsServer.updates).To(BeEmpty())
			Expect(mockAttachmentsServer.signals).To(BeEmpty())

			updated := &v1alpha1.PublicIPAttachment{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: attachmentName, Namespace: attachmentNamespace}, updated)
			Expect(err).To(HaveOccurred())
		})

		It("should not update if status unchanged", func() {
			attachment := &privatev1.PublicIPAttachment{
				Id: attachmentID,
				Metadata: &privatev1.Metadata{
					Name: attachmentName,
				},
				Spec: &privatev1.PublicIPAttachmentSpec{},
				Status: &privatev1.PublicIPAttachmentStatus{
					State: privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_READY,
				},
			}
			attachment.GetSpec().SetPublicIp(parentPublicIPID)
			mockAttachmentsServer.addAttachment(attachment)

			parentPublicIP := &privatev1.PublicIP{
				Id: parentPublicIPID,
				Metadata: &privatev1.Metadata{
					Name: "parent-publicip",
				},
				Spec:   &privatev1.PublicIPSpec{},
				Status: &privatev1.PublicIPStatus{},
			}
			parentPublicIP.GetStatus().SetAttached(true)
			mockPublicIPsServer2.addPublicIP(parentPublicIP)

			cr := &v1alpha1.PublicIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
					Labels: map[string]string{
						osacPublicIPAttachmentIDLabel: attachmentID,
					},
					Finalizers: []string{osacPublicIPAttachmentFeedbackFinalizer},
				},
				Spec: v1alpha1.PublicIPAttachmentSpec{
					PublicIP: "some-publicip",
				},
				Status: v1alpha1.PublicIPAttachmentStatus{
					Phase: v1alpha1.PublicIPAttachmentPhaseReady,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockAttachmentsServer.updates).To(BeEmpty())
			Expect(mockPublicIPsServer2.updates).To(BeEmpty())
		})

		It("should remove feedback finalizer and signal when it is the last finalizer", func() {
			attachment := &privatev1.PublicIPAttachment{
				Id: attachmentID,
				Metadata: &privatev1.Metadata{
					Name: attachmentName,
				},
				Spec: &privatev1.PublicIPAttachmentSpec{},
				Status: &privatev1.PublicIPAttachmentStatus{
					State: privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_READY,
				},
			}
			attachment.GetSpec().SetPublicIp(parentPublicIPID)
			mockAttachmentsServer.addAttachment(attachment)

			parentPublicIP := &privatev1.PublicIP{
				Id: parentPublicIPID,
				Metadata: &privatev1.Metadata{
					Name: "parent-publicip",
				},
				Spec:   &privatev1.PublicIPSpec{},
				Status: &privatev1.PublicIPStatus{},
			}
			mockPublicIPsServer2.addPublicIP(parentPublicIP)

			cr := &v1alpha1.PublicIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
					Labels: map[string]string{
						osacPublicIPAttachmentIDLabel: attachmentID,
					},
					Finalizers: []string{osacPublicIPAttachmentFeedbackFinalizer},
				},
				Spec: v1alpha1.PublicIPAttachmentSpec{
					PublicIP: "some-publicip",
				},
				Status: v1alpha1.PublicIPAttachmentStatus{
					Phase: v1alpha1.PublicIPAttachmentPhaseDeleting,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      attachmentName,
					Namespace: attachmentNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockAttachmentsServer.updates).To(HaveLen(1))
			Expect(mockAttachmentsServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPAttachmentState_PUBLIC_IP_ATTACHMENT_STATE_DELETING))

			Expect(mockAttachmentsServer.signals).To(HaveLen(1))
			Expect(mockAttachmentsServer.signals[0]).To(Equal(attachmentID))

			updated := &v1alpha1.PublicIPAttachment{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: attachmentName, Namespace: attachmentNamespace}, updated)
			Expect(err).To(HaveOccurred())
		})
	})
})

type mockPublicIPAttachmentsServer struct {
	privatev1.UnimplementedPublicIPAttachmentsServer
	mu          sync.Mutex
	attachments map[string]*privatev1.PublicIPAttachment
	updates     []*privatev1.PublicIPAttachment
	signals     []string
}

func (m *mockPublicIPAttachmentsServer) addAttachment(attachment *privatev1.PublicIPAttachment) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attachments[attachment.GetId()] = attachment
}

func (m *mockPublicIPAttachmentsServer) Get(ctx context.Context, req *privatev1.PublicIPAttachmentsGetRequest) (*privatev1.PublicIPAttachmentsGetResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	attachment, ok := m.attachments[req.GetId()]
	if !ok {
		return nil, grpcstatus.Errorf(codes.NotFound, "object with identifier '%s' not found", req.GetId())
	}

	return &privatev1.PublicIPAttachmentsGetResponse{
		Object: attachment,
	}, nil
}

func (m *mockPublicIPAttachmentsServer) Update(ctx context.Context, req *privatev1.PublicIPAttachmentsUpdateRequest) (*privatev1.PublicIPAttachmentsUpdateResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	attachment := req.GetObject()
	m.attachments[attachment.GetId()] = attachment
	m.updates = append(m.updates, attachment)

	return &privatev1.PublicIPAttachmentsUpdateResponse{
		Object: attachment,
	}, nil
}

func (m *mockPublicIPAttachmentsServer) Signal(ctx context.Context, req *privatev1.PublicIPAttachmentsSignalRequest) (*privatev1.PublicIPAttachmentsSignalResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.signals = append(m.signals, req.GetId())
	return &privatev1.PublicIPAttachmentsSignalResponse{}, nil
}

type mockPublicIPsServerForAttachment struct {
	privatev1.UnimplementedPublicIPsServer
	mu        sync.Mutex
	publicIPs map[string]*privatev1.PublicIP
	updates   []*privatev1.PublicIP
}

func (m *mockPublicIPsServerForAttachment) addPublicIP(publicIP *privatev1.PublicIP) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publicIPs[publicIP.GetId()] = publicIP
}

func (m *mockPublicIPsServerForAttachment) Get(ctx context.Context, req *privatev1.PublicIPsGetRequest) (*privatev1.PublicIPsGetResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	publicIP, ok := m.publicIPs[req.GetId()]
	if !ok {
		return nil, grpcstatus.Errorf(codes.NotFound, "object with identifier '%s' not found", req.GetId())
	}

	return &privatev1.PublicIPsGetResponse{
		Object: publicIP,
	}, nil
}

func (m *mockPublicIPsServerForAttachment) Update(ctx context.Context, req *privatev1.PublicIPsUpdateRequest) (*privatev1.PublicIPsUpdateResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	publicIP := req.GetObject()
	m.publicIPs[publicIP.GetId()] = publicIP
	m.updates = append(m.updates, publicIP)

	return &privatev1.PublicIPsUpdateResponse{
		Object: publicIP,
	}, nil
}
