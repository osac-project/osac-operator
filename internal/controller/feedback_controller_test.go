/*
Copyright 2025.

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
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/osac-project/osac-operator/api/v1alpha1"
	privatev1 "github.com/osac-project/osac-operator/internal/api/osac/private/v1"
)

var _ = Describe("FeedbackController", func() {
	const (
		clusterOrderName      = "test-order"
		clusterOrderNamespace = "test-namespace"
		clusterID             = "cluster-123"
	)

	var (
		testCtx    context.Context
		k8sTestCl  client.Client
		mockServer *mockClustersServer
		reconciler *FeedbackReconciler
		grpcServer *grpc.Server
		listener   *bufconn.Listener
	)

	BeforeEach(func() {
		testCtx = context.Background()

		scheme := runtime.NewScheme()
		Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(hypershiftv1beta1.AddToScheme(scheme)).To(Succeed())
		k8sTestCl = fake.NewClientBuilder().WithScheme(scheme).Build()

		mockServer = &mockClustersServer{
			clusters: make(map[string]*privatev1.Cluster),
			updates:  make([]*privatev1.Cluster, 0),
			signals:  make([]string, 0),
		}
		listener = bufconn.Listen(1024 * 1024)
		grpcServer = grpc.NewServer()
		privatev1.RegisterClustersServer(grpcServer, mockServer)

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

		reconciler = NewFeedbackReconciler(
			zap.New(zap.WriteTo(GinkgoWriter)),
			k8sTestCl,
			conn,
			clusterOrderNamespace,
		)
	})

	AfterEach(func() {
		if grpcServer != nil {
			grpcServer.Stop()
		}
		if listener != nil {
			_ = listener.Close()
		}
	})

	Context("when reconciling a ClusterOrder CR", func() {
		It("should sync Phase=Deleting to database state=CLUSTER_STATE_DELETING", func() {
			cluster := &privatev1.Cluster{
				Id:   clusterID,
				Spec: &privatev1.ClusterSpec{},
				Status: &privatev1.ClusterStatus{
					State: privatev1.ClusterState_CLUSTER_STATE_READY,
				},
			}
			mockServer.addCluster(cluster)

			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
					Labels: map[string]string{
						osacClusterOrderIDLabel: clusterID,
					},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseDeleting,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.ClusterState_CLUSTER_STATE_DELETING))

			updated := &v1alpha1.ClusterOrder{}
			Expect(k8sTestCl.Get(testCtx, types.NamespacedName{Name: clusterOrderName, Namespace: clusterOrderNamespace}, updated)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(updated, osacClusterOrderFeedbackFinalizer)).To(BeTrue())
		})

		It("should sync Phase=Ready to database state=CLUSTER_STATE_READY", func() {
			cluster := &privatev1.Cluster{
				Id:   clusterID,
				Spec: &privatev1.ClusterSpec{},
				Status: &privatev1.ClusterStatus{
					State: privatev1.ClusterState_CLUSTER_STATE_PROGRESSING,
				},
			}
			mockServer.addCluster(cluster)

			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
					Labels: map[string]string{
						osacClusterOrderIDLabel: clusterID,
					},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseReady,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.ClusterState_CLUSTER_STATE_READY))
		})

		It("should sync Phase=Progressing to database state=CLUSTER_STATE_PROGRESSING", func() {
			cluster := &privatev1.Cluster{
				Id:   clusterID,
				Spec: &privatev1.ClusterSpec{},
				Status: &privatev1.ClusterStatus{
					State: privatev1.ClusterState_CLUSTER_STATE_READY,
				},
			}
			mockServer.addCluster(cluster)

			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
					Labels: map[string]string{
						osacClusterOrderIDLabel: clusterID,
					},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseProgressing,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.ClusterState_CLUSTER_STATE_PROGRESSING))
		})

		It("should sync Phase=Failed to database state=CLUSTER_STATE_FAILED", func() {
			cluster := &privatev1.Cluster{
				Id:   clusterID,
				Spec: &privatev1.ClusterSpec{},
				Status: &privatev1.ClusterStatus{
					State: privatev1.ClusterState_CLUSTER_STATE_PROGRESSING,
				},
			}
			mockServer.addCluster(cluster)

			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
					Labels: map[string]string{
						osacClusterOrderIDLabel: clusterID,
					},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseFailed,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.ClusterState_CLUSTER_STATE_FAILED))
		})

		It("should skip CRs without cluster ID label", func() {
			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
					Labels:    map[string]string{},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseReady,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(BeEmpty())
		})

		It("should sync Phase=Deleting during deletion and keep finalizer when others present", func() {
			cluster := &privatev1.Cluster{
				Id:   clusterID,
				Spec: &privatev1.ClusterSpec{},
				Status: &privatev1.ClusterStatus{
					State: privatev1.ClusterState_CLUSTER_STATE_READY,
				},
			}
			mockServer.addCluster(cluster)

			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
					Labels: map[string]string{
						osacClusterOrderIDLabel: clusterID,
					},
					Finalizers: []string{osacClusterOrderFeedbackFinalizer, osacFinalizer},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseDeleting,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())

			Expect(k8sTestCl.Delete(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.ClusterState_CLUSTER_STATE_DELETING))

			updated := &v1alpha1.ClusterOrder{}
			Expect(k8sTestCl.Get(testCtx, types.NamespacedName{Name: clusterOrderName, Namespace: clusterOrderNamespace}, updated)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(updated, osacClusterOrderFeedbackFinalizer)).To(BeTrue())
		})

		It("should remove feedback finalizer and signal when it is the last finalizer", func() {
			cluster := &privatev1.Cluster{
				Id:   clusterID,
				Spec: &privatev1.ClusterSpec{},
				Status: &privatev1.ClusterStatus{
					State: privatev1.ClusterState_CLUSTER_STATE_READY,
				},
			}
			mockServer.addCluster(cluster)

			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
					Labels: map[string]string{
						osacClusterOrderIDLabel: clusterID,
					},
					Finalizers: []string{osacClusterOrderFeedbackFinalizer},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseDeleting,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())

			Expect(k8sTestCl.Delete(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.ClusterState_CLUSTER_STATE_DELETING))

			Expect(mockServer.signals).To(HaveLen(1))
			Expect(mockServer.signals[0]).To(Equal(clusterID))

			updated := &v1alpha1.ClusterOrder{}
			err = k8sTestCl.Get(testCtx, types.NamespacedName{Name: clusterOrderName, Namespace: clusterOrderNamespace}, updated)
			Expect(err).To(HaveOccurred())
		})

		It("should remove feedback finalizer when cluster record is NotFound during deletion", func() {
			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
					Labels: map[string]string{
						osacClusterOrderIDLabel: clusterID,
					},
					Finalizers: []string{osacClusterOrderFeedbackFinalizer},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseDeleting,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())

			Expect(k8sTestCl.Delete(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(BeEmpty())
			Expect(mockServer.signals).To(BeEmpty())

			updated := &v1alpha1.ClusterOrder{}
			err = k8sTestCl.Get(testCtx, types.NamespacedName{Name: clusterOrderName, Namespace: clusterOrderNamespace}, updated)
			Expect(err).To(HaveOccurred())
		})

		It("should still remove finalizer when signal fails", func() {
			cluster := &privatev1.Cluster{
				Id:   clusterID,
				Spec: &privatev1.ClusterSpec{},
				Status: &privatev1.ClusterStatus{
					State: privatev1.ClusterState_CLUSTER_STATE_READY,
				},
			}
			mockServer.addCluster(cluster)
			mockServer.signalErr = fmt.Errorf("signal unavailable")

			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
					Labels: map[string]string{
						osacClusterOrderIDLabel: clusterID,
					},
					Finalizers: []string{osacClusterOrderFeedbackFinalizer},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseDeleting,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())
			Expect(k8sTestCl.Delete(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &v1alpha1.ClusterOrder{}
			err = k8sTestCl.Get(testCtx, types.NamespacedName{Name: clusterOrderName, Namespace: clusterOrderNamespace}, updated)
			Expect(err).To(HaveOccurred())
		})

		It("should not update if status unchanged", func() {
			cluster := &privatev1.Cluster{
				Id:   clusterID,
				Spec: &privatev1.ClusterSpec{},
				Status: &privatev1.ClusterStatus{
					State: privatev1.ClusterState_CLUSTER_STATE_READY,
				},
			}
			mockServer.addCluster(cluster)

			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
					Labels: map[string]string{
						osacClusterOrderIDLabel: clusterID,
					},
					Finalizers: []string{osacClusterOrderFeedbackFinalizer},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseReady,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(BeEmpty())
		})

		It("should add feedback finalizer on first reconcile", func() {
			cluster := &privatev1.Cluster{
				Id:   clusterID,
				Spec: &privatev1.ClusterSpec{},
				Status: &privatev1.ClusterStatus{
					State: privatev1.ClusterState_CLUSTER_STATE_PROGRESSING,
				},
			}
			mockServer.addCluster(cluster)

			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
					Labels: map[string]string{
						osacClusterOrderIDLabel: clusterID,
					},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseProgressing,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &v1alpha1.ClusterOrder{}
			Expect(k8sTestCl.Get(testCtx, types.NamespacedName{Name: clusterOrderName, Namespace: clusterOrderNamespace}, updated)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(updated, osacClusterOrderFeedbackFinalizer)).To(BeTrue())
		})

		It("should remove feedback finalizer when CR without ID label is being deleted", func() {
			cr := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:       clusterOrderName,
					Namespace:  clusterOrderNamespace,
					Labels:     map[string]string{},
					Finalizers: []string{osacClusterOrderFeedbackFinalizer},
				},
				Status: v1alpha1.ClusterOrderStatus{
					Phase: v1alpha1.ClusterOrderPhaseDeleting,
				},
			}
			Expect(k8sTestCl.Create(testCtx, cr)).To(Succeed())

			Expect(k8sTestCl.Delete(testCtx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterOrderName,
					Namespace: clusterOrderNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &v1alpha1.ClusterOrder{}
			err = k8sTestCl.Get(testCtx, types.NamespacedName{Name: clusterOrderName, Namespace: clusterOrderNamespace}, updated)
			Expect(err).To(HaveOccurred())
		})
	})
})

// mockClustersServer implements privatev1.ClustersServer for testing
type mockClustersServer struct {
	privatev1.UnimplementedClustersServer
	mu        sync.Mutex
	clusters  map[string]*privatev1.Cluster
	updates   []*privatev1.Cluster
	signals   []string
	signalErr error
}

func (m *mockClustersServer) addCluster(cluster *privatev1.Cluster) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clusters[cluster.GetId()] = cluster
}

func (m *mockClustersServer) Get(_ context.Context, req *privatev1.ClustersGetRequest) (*privatev1.ClustersGetResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cluster, ok := m.clusters[req.GetId()]
	if !ok {
		return nil, grpcstatus.Errorf(codes.NotFound, "object with identifier '%s' not found", req.GetId())
	}

	return &privatev1.ClustersGetResponse{
		Object: cluster,
	}, nil
}

func (m *mockClustersServer) Update(_ context.Context, req *privatev1.ClustersUpdateRequest) (*privatev1.ClustersUpdateResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cluster := req.GetObject()
	m.clusters[cluster.GetId()] = cluster
	m.updates = append(m.updates, cluster)

	return &privatev1.ClustersUpdateResponse{
		Object: cluster,
	}, nil
}

func (m *mockClustersServer) Signal(_ context.Context, req *privatev1.ClustersSignalRequest) (*privatev1.ClustersSignalResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.signals = append(m.signals, req.GetId())

	if m.signalErr != nil {
		return nil, m.signalErr
	}
	return &privatev1.ClustersSignalResponse{}, nil
}
