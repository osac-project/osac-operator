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
	"errors"
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

var _ = Describe("PublicIPFeedbackController", func() {
	const (
		publicIPName      = "test-publicip"
		publicIPNamespace = "test-namespace"
		publicIPID        = "publicip-123"
		testPool          = "pool-abc"
		testAddress       = "192.168.1.100"
	)

	var (
		ctx        context.Context
		k8sClient  client.Client
		mockServer *mockPublicIPsServer
		reconciler *PublicIPFeedbackReconciler
		grpcServer *grpc.Server
		listener   *bufconn.Listener
	)

	BeforeEach(func() {
		ctx = context.Background()

		scheme := runtime.NewScheme()
		Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()

		mockServer = &mockPublicIPsServer{
			publicIPs: make(map[string]*privatev1.PublicIP),
			updates:   make([]*privatev1.PublicIP, 0),
			signals:   make([]string, 0),
		}
		listener = bufconn.Listen(1024 * 1024)
		grpcServer = grpc.NewServer()
		privatev1.RegisterPublicIPsServer(grpcServer, mockServer)

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

		reconciler = NewPublicIPFeedbackReconciler(k8sClient, conn, publicIPNamespace)
	})

	AfterEach(func() {
		if grpcServer != nil {
			grpcServer.Stop()
		}
		if listener != nil {
			_ = listener.Close()
		}
	})

	Context("when reconciling a PublicIP CR", func() {
		It("should sync Phase=Ready to database state=ALLOCATED", func() {
			publicIP := &privatev1.PublicIP{
				Id: publicIPID,
				Metadata: &privatev1.Metadata{
					Name: publicIPName,
				},
				Spec: &privatev1.PublicIPSpec{
					Pool: testPool,
				},
				Status: &privatev1.PublicIPStatus{
					State: privatev1.PublicIPState_PUBLIC_IP_STATE_PENDING,
				},
			}
			mockServer.addPublicIP(publicIP)

			cr := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
					Labels: map[string]string{
						osacPublicIPIDLabel: publicIPID,
					},
				},
				Spec: v1alpha1.PublicIPSpec{
					Pool: testPool,
				},
				Status: v1alpha1.PublicIPStatus{
					Phase: v1alpha1.PublicIPPhaseReady,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPState_PUBLIC_IP_STATE_ALLOCATED))

			updated := &v1alpha1.PublicIP{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: publicIPName, Namespace: publicIPNamespace}, updated)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(updated, osacPublicIPFeedbackFinalizer)).To(BeTrue())
		})

		It("should sync Phase=Progressing to database state=PENDING", func() {
			publicIP := &privatev1.PublicIP{
				Id: publicIPID,
				Metadata: &privatev1.Metadata{
					Name: publicIPName,
				},
				Spec: &privatev1.PublicIPSpec{
					Pool: testPool,
				},
				Status: &privatev1.PublicIPStatus{
					State: privatev1.PublicIPState_PUBLIC_IP_STATE_ALLOCATED,
				},
			}
			mockServer.addPublicIP(publicIP)

			cr := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
					Labels: map[string]string{
						osacPublicIPIDLabel: publicIPID,
					},
				},
				Spec: v1alpha1.PublicIPSpec{
					Pool: testPool,
				},
				Status: v1alpha1.PublicIPStatus{
					Phase: v1alpha1.PublicIPPhaseProgressing,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPState_PUBLIC_IP_STATE_PENDING))
		})

		It("should sync Phase=Failed to database state=FAILED", func() {
			publicIP := &privatev1.PublicIP{
				Id: publicIPID,
				Metadata: &privatev1.Metadata{
					Name: publicIPName,
				},
				Spec: &privatev1.PublicIPSpec{
					Pool: testPool,
				},
				Status: &privatev1.PublicIPStatus{
					State: privatev1.PublicIPState_PUBLIC_IP_STATE_PENDING,
				},
			}
			mockServer.addPublicIP(publicIP)

			cr := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
					Labels: map[string]string{
						osacPublicIPIDLabel: publicIPID,
					},
				},
				Spec: v1alpha1.PublicIPSpec{
					Pool: testPool,
				},
				Status: v1alpha1.PublicIPStatus{
					Phase: v1alpha1.PublicIPPhaseFailed,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPState_PUBLIC_IP_STATE_FAILED))
		})

		It("should sync address to database address field", func() {
			publicIP := &privatev1.PublicIP{
				Id: publicIPID,
				Metadata: &privatev1.Metadata{
					Name: publicIPName,
				},
				Spec: &privatev1.PublicIPSpec{
					Pool: testPool,
				},
				Status: &privatev1.PublicIPStatus{
					State: privatev1.PublicIPState_PUBLIC_IP_STATE_PENDING,
				},
			}
			mockServer.addPublicIP(publicIP)

			cr := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
					Labels: map[string]string{
						osacPublicIPIDLabel: publicIPID,
					},
				},
				Spec: v1alpha1.PublicIPSpec{
					Pool: testPool,
				},
				Status: v1alpha1.PublicIPStatus{
					Phase:   v1alpha1.PublicIPPhaseReady,
					Address: testAddress,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetAddress()).To(Equal(testAddress))
		})

		It("should skip CRs without publicip-uuid label", func() {
			cr := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
					Labels:    map[string]string{},
				},
				Spec: v1alpha1.PublicIPSpec{
					Pool: testPool,
				},
				Status: v1alpha1.PublicIPStatus{
					Phase: v1alpha1.PublicIPPhaseReady,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(BeEmpty())
		})

		It("should sync Phase=Deleting to database state=RELEASING during deletion", func() {
			publicIP := &privatev1.PublicIP{
				Id: publicIPID,
				Metadata: &privatev1.Metadata{
					Name: publicIPName,
				},
				Spec: &privatev1.PublicIPSpec{
					Pool: testPool,
				},
				Status: &privatev1.PublicIPStatus{
					State: privatev1.PublicIPState_PUBLIC_IP_STATE_ALLOCATED,
				},
			}
			mockServer.addPublicIP(publicIP)

			cr := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
					Labels: map[string]string{
						osacPublicIPIDLabel: publicIPID,
					},
					Finalizers: []string{osacPublicIPFeedbackFinalizer, "osac.openshift.io/publicip-finalizer"},
				},
				Spec: v1alpha1.PublicIPSpec{
					Pool: testPool,
				},
				Status: v1alpha1.PublicIPStatus{
					Phase: v1alpha1.PublicIPPhaseDeleting,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPState_PUBLIC_IP_STATE_RELEASING))

			updated := &v1alpha1.PublicIP{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: publicIPName, Namespace: publicIPNamespace}, updated)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(updated, osacPublicIPFeedbackFinalizer)).To(BeTrue())
		})

		It("should sync Phase=Failed during deletion to database state=FAILED", func() {
			publicIP := &privatev1.PublicIP{
				Id: publicIPID,
				Metadata: &privatev1.Metadata{
					Name: publicIPName,
				},
				Spec: &privatev1.PublicIPSpec{
					Pool: testPool,
				},
				Status: &privatev1.PublicIPStatus{
					State: privatev1.PublicIPState_PUBLIC_IP_STATE_ALLOCATED,
				},
			}
			mockServer.addPublicIP(publicIP)

			cr := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
					Labels: map[string]string{
						osacPublicIPIDLabel: publicIPID,
					},
					Finalizers: []string{osacPublicIPFeedbackFinalizer, "osac.openshift.io/publicip-finalizer"},
				},
				Spec: v1alpha1.PublicIPSpec{
					Pool: testPool,
				},
				Status: v1alpha1.PublicIPStatus{
					Phase: v1alpha1.PublicIPPhaseFailed,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPState_PUBLIC_IP_STATE_FAILED))
		})

		It("should remove feedback finalizer and signal when it is the last finalizer", func() {
			publicIP := &privatev1.PublicIP{
				Id: publicIPID,
				Metadata: &privatev1.Metadata{
					Name: publicIPName,
				},
				Spec: &privatev1.PublicIPSpec{
					Pool: testPool,
				},
				Status: &privatev1.PublicIPStatus{
					State: privatev1.PublicIPState_PUBLIC_IP_STATE_ALLOCATED,
				},
			}
			mockServer.addPublicIP(publicIP)

			cr := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
					Labels: map[string]string{
						osacPublicIPIDLabel: publicIPID,
					},
					Finalizers: []string{osacPublicIPFeedbackFinalizer},
				},
				Spec: v1alpha1.PublicIPSpec{
					Pool: testPool,
				},
				Status: v1alpha1.PublicIPStatus{
					Phase: v1alpha1.PublicIPPhaseDeleting,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.PublicIPState_PUBLIC_IP_STATE_RELEASING))

			Expect(mockServer.signals).To(HaveLen(1))
			Expect(mockServer.signals[0]).To(Equal(publicIPID))

			updated := &v1alpha1.PublicIP{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: publicIPName, Namespace: publicIPNamespace}, updated)
			Expect(err).To(HaveOccurred())
		})

		It("should remove feedback finalizer when publicip record is NotFound during deletion", func() {
			cr := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
					Labels: map[string]string{
						osacPublicIPIDLabel: publicIPID,
					},
					Finalizers: []string{osacPublicIPFeedbackFinalizer},
				},
				Spec: v1alpha1.PublicIPSpec{
					Pool: testPool,
				},
				Status: v1alpha1.PublicIPStatus{
					Phase: v1alpha1.PublicIPPhaseDeleting,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(BeEmpty())
			Expect(mockServer.signals).To(BeEmpty())

			updated := &v1alpha1.PublicIP{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: publicIPName, Namespace: publicIPNamespace}, updated)
			Expect(err).To(HaveOccurred())
		})

		It("should return error when publicip record is NotFound but CR is not being deleted", func() {
			cr := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
					Labels: map[string]string{
						osacPublicIPIDLabel: publicIPID,
					},
					Finalizers: []string{osacPublicIPFeedbackFinalizer},
				},
				Spec: v1alpha1.PublicIPSpec{
					Pool: testPool,
				},
				Status: v1alpha1.PublicIPStatus{
					Phase: v1alpha1.PublicIPPhaseReady,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
				},
			})
			Expect(errors.Is(err, ErrPublicIPNotFound)).To(BeTrue())
		})

		It("should not update if status unchanged", func() {
			publicIP := &privatev1.PublicIP{
				Id: publicIPID,
				Metadata: &privatev1.Metadata{
					Name: publicIPName,
				},
				Spec: &privatev1.PublicIPSpec{
					Pool: testPool,
				},
				Status: &privatev1.PublicIPStatus{
					State: privatev1.PublicIPState_PUBLIC_IP_STATE_ALLOCATED,
				},
			}
			mockServer.addPublicIP(publicIP)

			cr := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
					Labels: map[string]string{
						osacPublicIPIDLabel: publicIPID,
					},
					Finalizers: []string{osacPublicIPFeedbackFinalizer},
				},
				Spec: v1alpha1.PublicIPSpec{
					Pool: testPool,
				},
				Status: v1alpha1.PublicIPStatus{
					Phase: v1alpha1.PublicIPPhaseReady,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      publicIPName,
					Namespace: publicIPNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(BeEmpty())
		})
	})
})

type mockPublicIPsServer struct {
	privatev1.UnimplementedPublicIPsServer
	mu        sync.Mutex
	publicIPs map[string]*privatev1.PublicIP
	updates   []*privatev1.PublicIP
	signals   []string
}

func (m *mockPublicIPsServer) addPublicIP(publicIP *privatev1.PublicIP) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publicIPs[publicIP.GetId()] = publicIP
}

func (m *mockPublicIPsServer) Get(ctx context.Context, req *privatev1.PublicIPsGetRequest) (*privatev1.PublicIPsGetResponse, error) {
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

func (m *mockPublicIPsServer) Update(ctx context.Context, req *privatev1.PublicIPsUpdateRequest) (*privatev1.PublicIPsUpdateResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	publicIP := req.GetObject()
	m.publicIPs[publicIP.GetId()] = publicIP
	m.updates = append(m.updates, publicIP)

	return &privatev1.PublicIPsUpdateResponse{
		Object: publicIP,
	}, nil
}

func (m *mockPublicIPsServer) Signal(ctx context.Context, req *privatev1.PublicIPsSignalRequest) (*privatev1.PublicIPsSignalResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.signals = append(m.signals, req.GetId())

	return &privatev1.PublicIPsSignalResponse{}, nil
}
