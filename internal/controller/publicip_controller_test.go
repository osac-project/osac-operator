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
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	runtimecluster "sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mc "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	osacv1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

// mockMulticlusterManager is a minimal implementation for testing address population.
// Embeds the full Manager interface; only GetCluster is overridden.
type mockMulticlusterManager struct {
	mcmanager.Manager
	targetClient client.Client
}

func (m *mockMulticlusterManager) GetCluster(_ context.Context, _ mc.ClusterName) (runtimecluster.Cluster, error) {
	return &mockCluster{client: m.targetClient}, nil
}

// mockCluster satisfies cluster.Cluster; only GetClient is overridden.
type mockCluster struct {
	runtimecluster.Cluster
	client client.Client
}

func (c *mockCluster) GetClient() client.Client {
	return c.client
}

// Tests for the PublicIP provisioning controller.
//
// Key concepts for understanding these tests:
//
//   - A PublicIP belongs to a parent PublicIPPool. The relationship uses fulfillment-service
//     UUIDs, not K8s object names: publicIP.spec.pool contains a UUID, and the parent
//     PublicIPPool CR carries that UUID in its osac.openshift.io/publicippool-uuid label.
//     The controller resolves the parent by listing pools with a matching label.
//
//   - The reconcile loop requires multiple passes because each pass does one thing and
//     returns: first pass adds the finalizer (metadata write), second pass processes the
//     spec and triggers provisioning, third pass polls the provisioning job status.
//
//   - Deletion tests call handleDelete directly because the fake client does not support
//     setting DeletionTimestamp via Update. We set it in memory and invoke the handler.
//
//   - The mock provisioning provider (defined in computeinstance_provisioning_test.go)
//     simulates AAP job triggers and status polls. By default it returns success; tests
//     override specific funcs to simulate failures or running states.
var _ = Describe("PublicIPReconciler", func() {
	var (
		reconciler   *PublicIPReconciler
		mockProvider *mockProvisioningProvider
		fakeClient   client.Client
		testCtx      context.Context
		publicIP     *osacv1alpha1.PublicIP
		parentPool   *osacv1alpha1.PublicIPPool
		testScheme   *runtime.Scheme
	)

	const (
		testNamespace            = "test-namespace"
		testPoolUUID             = "pool-uuid-123"
		testConfigVersion        = "version-1-abc123"
		testConfigVersionUpdated = "version-2-def456"
		testComputeInstance      = "some-ci"
	)

	BeforeEach(func() {
		testCtx = context.TODO()
		testScheme = runtime.NewScheme()
		Expect(osacv1alpha1.AddToScheme(testScheme)).To(Succeed())
		Expect(scheme.AddToScheme(testScheme)).To(Succeed())

		// The parent pool has a K8s name ("pool-k8s-name") that differs from the UUID
		// ("pool-uuid-123") to verify the controller uses label-based lookup, not name-based.
		parentPool = &osacv1alpha1.PublicIPPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pool-k8s-name",
				Namespace: testNamespace,
				Labels: map[string]string{
					osacPublicIPPoolIDLabel: testPoolUUID,
				},
			},
			Spec: osacv1alpha1.PublicIPPoolSpec{
				CIDRs:                  []string{"192.168.1.0/24"},
				IPFamily:               "IPv4",
				ImplementationStrategy: "metallb-l2",
			},
		}

		// The PublicIP references the parent pool by UUID, not by K8s name.
		publicIP = &osacv1alpha1.PublicIP{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-publicip",
				Namespace: testNamespace,
			},
			Spec: osacv1alpha1.PublicIPSpec{
				Pool: testPoolUUID,
			},
		}

		// ComputeInstance referenced by attach/detach tests via testComputeInstance UUID.
		testCI := &osacv1alpha1.ComputeInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-ci-k8s",
				Namespace: testNamespace,
				Labels: map[string]string{
					osacComputeInstanceIDLabel: testComputeInstance,
				},
			},
			Status: osacv1alpha1.ComputeInstanceStatus{
				VirtualMachineReference: &osacv1alpha1.VirtualMachineReferenceType{
					Namespace:                  testNamespace,
					KubeVirtVirtualMachineName: "test-vm",
				},
			},
		}

		fakeClient = fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(publicIP, parentPool, testCI).
			WithStatusSubresource(&osacv1alpha1.PublicIP{}, &osacv1alpha1.ComputeInstance{}).
			Build()

		// WithStatusSubresource strips status from WithObjects, so persist it separately.
		Expect(fakeClient.Status().Update(testCtx, testCI)).To(Succeed())

		mockProvider = &mockProvisioningProvider{name: "mock-aap"}

		// Default mock mgr provides an empty workload-cluster client so the
		// address-population guard in handleUpdate does not nil-panic. Tests
		// that need a Service on the workload cluster override reconciler.mgr.
		emptyTargetClient := fake.NewClientBuilder().WithScheme(testScheme).Build()

		reconciler = &PublicIPReconciler{
			Client:                   fakeClient,
			APIReader:                fakeClient,
			Scheme:                   testScheme,
			mgr:                      &mockMulticlusterManager{targetClient: emptyTargetClient},
			NetworkingNamespace:      testNamespace,
			ComputeInstanceNamespace: testNamespace,
			ProvisioningProvider:     mockProvider,
			StatusPollInterval:       1 * time.Second,
			MaxJobHistory:            10,
		}
	})

	Context("Reconcile", func() {
		It("should add finalizer on first reconcile", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}
			result, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(osacPublicIPFinalizer))
		})

		It("should set phase to Progressing initially", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Keep the job in Running state so the phase stays Progressing
			// (a Succeeded job would transition to Ready)
			mockProvider.getProvisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateRunning,
					Message: "Job running",
				}, nil
			}

			// Pass 1: adds finalizer
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			// Pass 2: resolves parent pool, triggers provisioning, sets Progressing
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseProgressing))
		})

		It("should set implementation-strategy annotation from parent pool", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Pass 1: adds finalizer
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			// Pass 2: resolves parent pool and copies its implementation strategy
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Annotations[osacImplementationStrategyAnnotation]).To(Equal("metallb-l2"))
			Expect(updated.Annotations[osacPublicIPPoolNameAnnotation]).To(Equal("pool-k8s-name"))
		})

		It("should requeue when parent PublicIPPool is not found", func() {
			// This PublicIP references a pool UUID that doesn't exist as a label
			// on any PublicIPPool CR. The controller should requeue and wait for
			// the pool to appear (it may not have been reconciled to K8s yet).
			orphanIP := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "orphan-publicip",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool: "nonexistent-pool-uuid",
				},
			}
			Expect(fakeClient.Create(testCtx, orphanIP)).To(Succeed())

			key := types.NamespacedName{Name: orphanIP.Name, Namespace: orphanIP.Namespace}

			// Pass 1: adds finalizer
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			// Pass 2: parent pool not found, requeues after precondition interval
			result, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(defaultPreconditionRequeueInterval))
		})

		It("should use default implementation strategy when pool has none", func() {
			// A pool with no ImplementationStrategy in its spec should fall back
			// to defaultPublicIPPoolImplementationStrategy ("metallb-l2").
			poolNoStrategy := &osacv1alpha1.PublicIPPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pool-no-strategy",
					Namespace: testNamespace,
					Labels: map[string]string{
						osacPublicIPPoolIDLabel: "pool-no-strategy-uuid",
					},
				},
				Spec: osacv1alpha1.PublicIPPoolSpec{
					CIDRs:    []string{"10.0.0.0/24"},
					IPFamily: "IPv4",
				},
			}
			Expect(fakeClient.Create(testCtx, poolNoStrategy)).To(Succeed())

			ipNoStrategy := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ip-no-strategy",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool: "pool-no-strategy-uuid",
				},
			}
			Expect(fakeClient.Create(testCtx, ipNoStrategy)).To(Succeed())

			key := types.NamespacedName{Name: ipNoStrategy.Name, Namespace: ipNoStrategy.Namespace}

			// Pass 1: adds finalizer
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			// Pass 2: inherits default strategy since pool has none
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Annotations[osacImplementationStrategyAnnotation]).To(Equal(defaultPublicIPPoolImplementationStrategy))
			Expect(updated.Annotations[osacPublicIPPoolNameAnnotation]).To(Equal("pool-no-strategy"))
		})

		It("should set ConfigurationApplied condition to True", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Pass 1: finalizer, Pass 2: process spec and set condition
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())

			condition := osacv1alpha1.GetPublicIPStatusCondition(
				updated, osacv1alpha1.PublicIPConditionConfigurationApplied,
			)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal("ConfigurationApplied"))
		})

		It("should set phase to Ready on successful provision", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			mockProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID:        "job-success",
					InitialState: osacv1alpha1.JobStatePending,
					Message:      "Job triggered",
				}, nil
			}

			mockProvider.getProvisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateSucceeded,
					Message: "Provisioning completed",
				}, nil
			}

			// Pass 1: finalizer, Pass 2: trigger AAP job, Pass 3: poll job -> Succeeded -> Ready
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseReady))
		})

		It("should set phase to Failed on provision failure", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			mockProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID:        "job-fail",
					InitialState: osacv1alpha1.JobStatePending,
					Message:      "Job triggered",
				}, nil
			}

			mockProvider.getProvisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:        jobID,
					State:        osacv1alpha1.JobStateFailed,
					Message:      "Provisioning failed",
					ErrorDetails: "MetalLB unreachable",
				}, nil
			}

			// Pass 1: finalizer, Pass 2: trigger, Pass 3: poll -> Failed
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseFailed))
		})

		// Deletion tests call handleDelete directly because the fake client does not
		// support setting DeletionTimestamp via Update. We add the finalizer via a
		// normal Reconcile, then set DeletionTimestamp in memory and call handleDelete.

		It("should trigger deprovision on delete", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			deprovisionCalled := false
			mockProvider.triggerDeprovisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.DeprovisionResult, error) {
				deprovisionCalled = true
				return &provisioning.DeprovisionResult{
					Action:                 provisioning.DeprovisionTriggered,
					JobID:                  "deprovision-job-123",
					BlockDeletionOnFailure: true,
				}, nil
			}

			// Add finalizer via normal reconcile
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			// Simulate K8s delete by setting DeletionTimestamp in memory
			toDelete := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, toDelete)).To(Succeed())
			now := metav1.Now()
			toDelete.DeletionTimestamp = &now

			_, err = reconciler.handleDelete(testCtx, toDelete)
			Expect(err).NotTo(HaveOccurred())

			Expect(deprovisionCalled).To(BeTrue())
			Expect(toDelete.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseDeleting))

			latestJob := provisioning.FindLatestJobByType(toDelete.Status.Jobs, osacv1alpha1.JobTypeDeprovision)
			Expect(latestJob).NotTo(BeNil())
			Expect(latestJob.JobID).To(Equal("deprovision-job-123"))
		})

		It("should remove finalizer after successful deprovision", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			mockProvider.triggerDeprovisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.DeprovisionResult, error) {
				return &provisioning.DeprovisionResult{
					Action:                 provisioning.DeprovisionTriggered,
					JobID:                  "deprovision-success",
					BlockDeletionOnFailure: true,
				}, nil
			}

			mockProvider.getDeprovisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateSucceeded,
					Message: "Deprovision completed",
				}, nil
			}

			// Add finalizer via normal reconcile
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			toDelete := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, toDelete)).To(Succeed())
			now := metav1.Now()
			toDelete.DeletionTimestamp = &now

			// First call triggers the deprovision job
			_, err = reconciler.handleDelete(testCtx, toDelete)
			Expect(err).NotTo(HaveOccurred())

			// Second call polls status (Succeeded) and removes the finalizer
			_, _ = reconciler.handleDelete(testCtx, toDelete)

			Expect(toDelete.Finalizers).NotTo(ContainElement(osacPublicIPFinalizer))
		})

		It("should still handle delete for unmanaged PublicIP with finalizer", func() {
			// Edge case: a PublicIP was managed (has finalizer), then an admin marked
			// it unmanaged. The management-state guard in Reconcile skips processing
			// for non-deleted resources, but deletion must still proceed to clean up
			// the AAP-provisioned resources and remove the finalizer.
			managedThenUnmanaged := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "managed-then-unmanaged",
					Namespace: testNamespace,
					Annotations: map[string]string{
						osacManagementStateAnnotation: ManagementStateUnmanaged,
					},
					Finalizers: []string{osacPublicIPFinalizer},
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool: testPoolUUID,
				},
			}
			Expect(fakeClient.Create(testCtx, managedThenUnmanaged)).To(Succeed())

			key := types.NamespacedName{Name: managedThenUnmanaged.Name, Namespace: managedThenUnmanaged.Namespace}

			deprovisionCalled := false
			mockProvider.triggerDeprovisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.DeprovisionResult, error) {
				deprovisionCalled = true
				return &provisioning.DeprovisionResult{
					Action: provisioning.DeprovisionSkipped,
				}, nil
			}

			fetched := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, fetched)).To(Succeed())
			now := metav1.Now()
			fetched.DeletionTimestamp = &now

			// Verify the guard logic: DeletionTimestamp is set, so the unmanaged
			// annotation is ignored and the delete branch runs.
			Expect(fetched.ObjectMeta.DeletionTimestamp.IsZero()).To(BeFalse())

			_, _ = reconciler.handleDelete(testCtx, fetched)

			Expect(deprovisionCalled).To(BeTrue())
			Expect(fetched.Finalizers).NotTo(ContainElement(osacPublicIPFinalizer))
		})

		It("should set state to Pending on initial provisioning", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			mockProvider.getProvisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateRunning,
					Message: "Job running",
				}, nil
			}

			// Pass 1: finalizer, Pass 2: trigger provisioning -> Progressing + Pending
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseProgressing))
			Expect(updated.Status.State).To(Equal(osacv1alpha1.PublicIPStatePending))
		})

		It("should set state to Allocated on successful initial provision with no ComputeInstance", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			mockProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID:        "job-allocated",
					InitialState: osacv1alpha1.JobStatePending,
					Message:      "Job triggered",
				}, nil
			}

			mockProvider.getProvisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateSucceeded,
					Message: "Provisioning completed",
				}, nil
			}

			// Pass 1: finalizer, Pass 2: trigger, Pass 3: poll -> Ready + Allocated
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseReady))
			Expect(updated.Status.State).To(Equal(osacv1alpha1.PublicIPStateAllocated))
		})

		It("should set state to Attaching when ComputeInstance is set on allocated IP", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Start with an Allocated PublicIP (Ready phase, Allocated state)
			publicIP.Status.Phase = osacv1alpha1.PublicIPPhaseReady
			publicIP.Status.State = osacv1alpha1.PublicIPStateAllocated
			publicIP.Status.DesiredConfigVersion = testConfigVersion
			publicIP.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "initial-provision",
					Type:          osacv1alpha1.JobTypeProvision,
					State:         osacv1alpha1.JobStateSucceeded,
					ConfigVersion: testConfigVersion,
					Timestamp:     metav1.NewTime(time.Now().UTC()),
				},
			}
			Expect(fakeClient.Status().Update(testCtx, publicIP)).To(Succeed())

			// Update spec to set ComputeInstance, which changes config version
			publicIP.Spec.ComputeInstance = testComputeInstance
			Expect(fakeClient.Update(testCtx, publicIP)).To(Succeed())

			mockProvider.getProvisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateRunning,
					Message: "Job running",
				}, nil
			}

			// Reconcile should detect attach transition -> Progressing + Attaching
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseProgressing))
			Expect(updated.Status.State).To(Equal(osacv1alpha1.PublicIPStateAttaching))
		})

		It("should set state to Attached when attach job succeeds via attachment provider", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Start in Attaching state: persist spec first, then status.
			publicIP.Spec.ComputeInstance = testComputeInstance
			Expect(fakeClient.Update(testCtx, publicIP)).To(Succeed())

			publicIP.Status.Phase = osacv1alpha1.PublicIPPhaseProgressing
			publicIP.Status.State = osacv1alpha1.PublicIPStateAttaching
			Expect(fakeClient.Status().Update(testCtx, publicIP)).To(Succeed())

			// Wire the attachment provider; handleAttaching uses TriggerProvision
			// which maps to osac-attach-public-ip.
			attachmentProvider := &mockProvisioningProvider{name: "mock-attachment"}
			reconciler.PublicIPAttachmentProvider = attachmentProvider

			attachmentProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID:        "attach-job",
					InitialState: osacv1alpha1.JobStatePending,
					Message:      "Attach job triggered",
				}, nil
			}

			attachmentProvider.getProvisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateSucceeded,
					Message: "Attach completed",
				}, nil
			}

			// Trigger attach job, then poll -> Ready + Attached
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseReady))
			Expect(updated.Status.State).To(Equal(osacv1alpha1.PublicIPStateAttached))

			// Verify the job was tracked as JobTypeAttach
			latestAttach := provisioning.FindLatestJobByType(updated.Status.Jobs, osacv1alpha1.JobTypeAttach)
			Expect(latestAttach).NotTo(BeNil())
			Expect(latestAttach.JobID).To(Equal("attach-job"))
			Expect(latestAttach.State).To(Equal(osacv1alpha1.JobStateSucceeded))
		})

		It("should set state to Releasing when ComputeInstance is cleared on attached IP", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Start with an Attached PublicIP: set spec first, then status.
			// With WithStatusSubresource, Update() strips status from the
			// in-memory object, so Status().Update() must come last.
			publicIP.Spec.ComputeInstance = testComputeInstance
			Expect(fakeClient.Update(testCtx, publicIP)).To(Succeed())

			publicIP.Status.Phase = osacv1alpha1.PublicIPPhaseReady
			publicIP.Status.State = osacv1alpha1.PublicIPStateAttached
			publicIP.Status.DesiredConfigVersion = testConfigVersionUpdated
			publicIP.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "attach-job",
					Type:          osacv1alpha1.JobTypeProvision,
					State:         osacv1alpha1.JobStateSucceeded,
					ConfigVersion: testConfigVersionUpdated,
					Timestamp:     metav1.NewTime(time.Now().UTC()),
				},
			}
			Expect(fakeClient.Status().Update(testCtx, publicIP)).To(Succeed())

			// Clear ComputeInstance (re-read first to get current resourceVersion)
			Expect(fakeClient.Get(testCtx, key, publicIP)).To(Succeed())
			publicIP.Spec.ComputeInstance = ""
			Expect(fakeClient.Update(testCtx, publicIP)).To(Succeed())

			mockProvider.getProvisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateRunning,
					Message: "Job running",
				}, nil
			}

			// Reconcile should detect detach transition -> Progressing + Releasing
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseProgressing))
			Expect(updated.Status.State).To(Equal(osacv1alpha1.PublicIPStateReleasing))
		})

		It("should set state to Allocated when detach job succeeds via attachment provider", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Start in Releasing state: persist spec first, then status.
			publicIP.Spec.ComputeInstance = ""
			Expect(fakeClient.Update(testCtx, publicIP)).To(Succeed())

			publicIP.Status.Phase = osacv1alpha1.PublicIPPhaseProgressing
			publicIP.Status.State = osacv1alpha1.PublicIPStateReleasing
			Expect(fakeClient.Status().Update(testCtx, publicIP)).To(Succeed())

			// Wire the attachment provider; handleDetaching uses TriggerDeprovision
			// which maps to osac-detach-public-ip.
			attachmentProvider := &mockProvisioningProvider{name: "mock-attachment"}
			reconciler.PublicIPAttachmentProvider = attachmentProvider

			attachmentProvider.triggerDeprovisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.DeprovisionResult, error) {
				return &provisioning.DeprovisionResult{
					Action: provisioning.DeprovisionTriggered,
					JobID:  "detach-job",
				}, nil
			}

			attachmentProvider.getDeprovisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateSucceeded,
					Message: "Detach completed",
				}, nil
			}

			// Trigger detach job, then poll -> Ready + Allocated
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseReady))
			Expect(updated.Status.State).To(Equal(osacv1alpha1.PublicIPStateAllocated))

			// Verify the job was tracked as JobTypeDetach
			latestDetach := provisioning.FindLatestJobByType(updated.Status.Jobs, osacv1alpha1.JobTypeDetach)
			Expect(latestDetach).NotTo(BeNil())
			Expect(latestDetach.JobID).To(Equal("detach-job"))
			Expect(latestDetach.State).To(Equal(osacv1alpha1.JobStateSucceeded))
		})

		It("should not re-trigger attach when attach job is still running and detach is requested", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Start in Releasing state with a running attach job still in flight.
			publicIP.Spec.ComputeInstance = ""
			Expect(fakeClient.Update(testCtx, publicIP)).To(Succeed())

			publicIP.Status.Phase = osacv1alpha1.PublicIPPhaseProgressing
			publicIP.Status.State = osacv1alpha1.PublicIPStateReleasing
			publicIP.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:     "running-attach",
					Type:      osacv1alpha1.JobTypeAttach,
					State:     osacv1alpha1.JobStateRunning,
					Timestamp: metav1.NewTime(time.Now().UTC()),
				},
			}
			Expect(fakeClient.Status().Update(testCtx, publicIP)).To(Succeed())

			attachmentProvider := &mockProvisioningProvider{name: "mock-attachment"}
			reconciler.PublicIPAttachmentProvider = attachmentProvider

			// handleDetaching should wait for the running attach job, not trigger detach.
			triggered := false
			attachmentProvider.triggerDeprovisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.DeprovisionResult, error) {
				triggered = true
				return &provisioning.DeprovisionResult{Action: provisioning.DeprovisionTriggered, JobID: "detach-job"}, nil
			}

			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			Expect(triggered).To(BeFalse(), "detach should not trigger while attach is running")

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.State).To(Equal(osacv1alpha1.PublicIPStateReleasing))
		})

		It("should route failed attach back to handleAttaching when spec intent matches", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Failed attach with spec.computeInstance still set: the user still wants
			// the attach to happen, so routing should go to handleAttaching.
			publicIP.Spec.ComputeInstance = testComputeInstance
			Expect(fakeClient.Update(testCtx, publicIP)).To(Succeed())

			publicIP.Status.Phase = osacv1alpha1.PublicIPPhaseFailed
			publicIP.Status.State = osacv1alpha1.PublicIPStateFailed
			publicIP.Status.DesiredConfigVersion = testConfigVersionUpdated
			publicIP.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "failed-attach",
					Type:          osacv1alpha1.JobTypeAttach,
					State:         osacv1alpha1.JobStateFailed,
					ConfigVersion: testConfigVersionUpdated,
					Timestamp:     metav1.NewTime(time.Now().UTC()),
				},
			}
			Expect(fakeClient.Status().Update(testCtx, publicIP)).To(Succeed())

			// Wire attachment provider: handleAttaching should be called, not handleProvisioning.
			attachmentProvider := &mockProvisioningProvider{name: "mock-attachment"}
			reconciler.PublicIPAttachmentProvider = attachmentProvider

			provisionCalled := false
			mockProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				provisionCalled = true
				return &provisioning.ProvisionResult{JobID: "prov-job", InitialState: osacv1alpha1.JobStatePending}, nil
			}

			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			Expect(provisionCalled).To(BeFalse(), "handleProvisioning should not be called for a failed attach")
		})

		It("should not route to handleAttaching when spec intent changed after failed attach", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Failed attach, but user cleared spec.computeInstance: the intent is no
			// longer to attach, so routing should NOT go to handleAttaching.
			publicIP.Spec.ComputeInstance = ""
			Expect(fakeClient.Update(testCtx, publicIP)).To(Succeed())

			publicIP.Status.Phase = osacv1alpha1.PublicIPPhaseFailed
			publicIP.Status.State = osacv1alpha1.PublicIPStateFailed
			publicIP.Status.DesiredConfigVersion = testConfigVersionUpdated
			publicIP.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "failed-attach",
					Type:          osacv1alpha1.JobTypeAttach,
					State:         osacv1alpha1.JobStateFailed,
					ConfigVersion: testConfigVersion,
					Timestamp:     metav1.NewTime(time.Now().UTC()),
				},
			}
			Expect(fakeClient.Status().Update(testCtx, publicIP)).To(Succeed())

			attachmentProvider := &mockProvisioningProvider{name: "mock-attachment"}
			reconciler.PublicIPAttachmentProvider = attachmentProvider

			attachCalled := false
			attachmentProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				attachCalled = true
				return &provisioning.ProvisionResult{JobID: "attach-job", InitialState: osacv1alpha1.JobStatePending}, nil
			}

			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			Expect(attachCalled).To(BeFalse(), "handleAttaching should not be called when spec intent changed")
		})

		It("should reset state to Attaching when retrying from Failed and route correctly on next reconcile", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Start in Failed state with spec.computeInstance still set (user wants retry).
			publicIP.Spec.ComputeInstance = testComputeInstance
			Expect(fakeClient.Update(testCtx, publicIP)).To(Succeed())

			publicIP.Status.Phase = osacv1alpha1.PublicIPPhaseFailed
			publicIP.Status.State = osacv1alpha1.PublicIPStateFailed
			publicIP.Status.DesiredConfigVersion = testConfigVersionUpdated
			publicIP.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "failed-attach",
					Type:          osacv1alpha1.JobTypeAttach,
					State:         osacv1alpha1.JobStateFailed,
					ConfigVersion: testConfigVersion,
					Timestamp:     metav1.NewTime(time.Now().UTC()),
				},
			}
			Expect(fakeClient.Status().Update(testCtx, publicIP)).To(Succeed())

			attachmentProvider := &mockProvisioningProvider{name: "mock-attachment"}
			reconciler.PublicIPAttachmentProvider = attachmentProvider

			attachmentProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID: "retry-attach", InitialState: osacv1alpha1.JobStatePending, Message: "Retry triggered",
				}, nil
			}

			attachmentProvider.getProvisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID: jobID, State: osacv1alpha1.JobStateRunning, Message: "Running",
				}, nil
			}

			// Pass 1: routes Failed -> handleAttaching, triggers retry, resets state.
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseProgressing),
				"phase should reset to Progressing after retry trigger")
			Expect(updated.Status.State).To(Equal(osacv1alpha1.PublicIPStateAttaching),
				"state should reset to Attaching after retry trigger")

			// Pass 2: state is now Attaching, should route to handleAttaching (poll),
			// not fall through to handleProvisioning.
			provisionCalled := false
			mockProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				provisionCalled = true
				return &provisioning.ProvisionResult{JobID: "bad-prov", InitialState: osacv1alpha1.JobStatePending}, nil
			}

			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			Expect(provisionCalled).To(BeFalse(),
				"second reconcile should route to handleAttaching, not handleProvisioning")
		})

		It("should not re-provision after successful attach changes config version", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// handleUpdate recomputes DesiredConfigVersion from the actual spec,
			// so we can't hardcode it. Reconcile once to let the controller compute
			// the real hash, then set up the attach job to match.
			publicIP.Spec.ComputeInstance = testComputeInstance
			Expect(fakeClient.Update(testCtx, publicIP)).To(Succeed())

			// First reconcile: adds finalizer, computes config version, sets Attaching.
			reconciler.PublicIPAttachmentProvider = &mockProvisioningProvider{name: "mock-attachment"}
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			// Read back to get the computed DesiredConfigVersion.
			Expect(fakeClient.Get(testCtx, key, publicIP)).To(Succeed())
			computedVersion := publicIP.Status.DesiredConfigVersion
			Expect(computedVersion).NotTo(BeEmpty())

			// Simulate successful attach: set Attached + Ready with a matching attach job.
			publicIP.Status.Phase = osacv1alpha1.PublicIPPhaseReady
			publicIP.Status.State = osacv1alpha1.PublicIPStateAttached
			publicIP.Status.Jobs = append(publicIP.Status.Jobs, osacv1alpha1.JobStatus{
				JobID:         "attach-job",
				Type:          osacv1alpha1.JobTypeAttach,
				State:         osacv1alpha1.JobStateSucceeded,
				ConfigVersion: computedVersion,
				Timestamp:     metav1.NewTime(time.Now().UTC()),
			})
			Expect(fakeClient.Status().Update(testCtx, publicIP)).To(Succeed())

			// Next reconcile should NOT trigger a new provision job because
			// IsConfigApplied finds the attach job with the matching config version.
			provisionCalled := false
			mockProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				provisionCalled = true
				return &provisioning.ProvisionResult{JobID: "reprov-job", InitialState: osacv1alpha1.JobStatePending}, nil
			}

			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			Expect(provisionCalled).To(BeFalse(), "should not re-provision when attach job already applied the config")

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseReady))
			Expect(updated.Status.State).To(Equal(osacv1alpha1.PublicIPStateAttached))
		})

		It("should set state to Failed on provisioning failure", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			mockProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID:        "job-fail",
					InitialState: osacv1alpha1.JobStatePending,
					Message:      "Job triggered",
				}, nil
			}

			mockProvider.getProvisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:        jobID,
					State:        osacv1alpha1.JobStateFailed,
					Message:      "Provisioning failed",
					ErrorDetails: "MetalLB unreachable",
				}, nil
			}

			// Pass 1: finalizer, Pass 2: trigger, Pass 3: poll -> Failed state + phase
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseFailed))
			Expect(updated.Status.State).To(Equal(osacv1alpha1.PublicIPStateFailed))
		})

		It("should not change state on deletion", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// Start with Allocated state: persist metadata first, then status.
			publicIP.Finalizers = []string{osacPublicIPFinalizer}
			Expect(fakeClient.Update(testCtx, publicIP)).To(Succeed())

			publicIP.Status.Phase = osacv1alpha1.PublicIPPhaseReady
			publicIP.Status.State = osacv1alpha1.PublicIPStateAllocated
			Expect(fakeClient.Status().Update(testCtx, publicIP)).To(Succeed())

			// Return a running deprovision job so handleDelete requeues before
			// reaching the finalizer-removal Update (which the envtest server
			// rejects when DeletionTimestamp is set in-memory).
			mockProvider.triggerDeprovisionFunc = func(
				_ context.Context, _ client.Object,
			) (*provisioning.DeprovisionResult, error) {
				return &provisioning.DeprovisionResult{
					Action: provisioning.DeprovisionTriggered,
					JobID:  "deprov-job",
				}, nil
			}

			toDelete := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, toDelete)).To(Succeed())
			now := metav1.Now()
			toDelete.DeletionTimestamp = &now

			_, err := reconciler.handleDelete(testCtx, toDelete)
			Expect(err).NotTo(HaveOccurred())

			// Phase transitions to Deleting but State remains unchanged
			Expect(toDelete.Status.Phase).To(Equal(osacv1alpha1.PublicIPPhaseDeleting))
			Expect(toDelete.Status.State).To(Equal(osacv1alpha1.PublicIPStateAllocated))
		})

		It("should ignore PublicIP with management-state unmanaged annotation", func() {
			// When a PublicIP has the unmanaged annotation and is NOT being deleted,
			// the controller should skip it entirely: no finalizer, no phase change.
			unmanagedIP := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "unmanaged-ip",
					Namespace: testNamespace,
					Annotations: map[string]string{
						osacManagementStateAnnotation: ManagementStateUnmanaged,
					},
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool: testPoolUUID,
				},
			}
			Expect(fakeClient.Create(testCtx, unmanagedIP)).To(Succeed())

			key := types.NamespacedName{Name: unmanagedIP.Name, Namespace: unmanagedIP.Namespace}
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())

			Expect(updated.Finalizers).To(BeEmpty())
			Expect(updated.Status.Phase).To(BeEmpty())
		})
	})

	Context("ComputeInstance target namespace annotation", func() {
		const (
			testCINamespace = "osac-computeinstance"
			testCIUUID      = "ci-uuid-456"
			testTenantNS    = "tenant-ns-abc"
		)

		It("should set publicip-target-namespace annotation when computeInstance is set", func() {
			ci := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci",
					Namespace: testCINamespace,
					Labels: map[string]string{
						osacComputeInstanceIDLabel: testCIUUID,
					},
				},
			}

			ipWithCI := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ip-with-ci",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: testCIUUID,
				},
			}

			ciClient := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(ipWithCI, parentPool, ci).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}, &osacv1alpha1.ComputeInstance{}).
				Build()

			// Apply status separately — WithStatusSubresource strips status from WithObjects.
			ci.Status.VirtualMachineReference = &osacv1alpha1.VirtualMachineReferenceType{
				Namespace:                  testTenantNS,
				KubeVirtVirtualMachineName: "test-vm",
			}
			Expect(ciClient.Status().Update(testCtx, ci)).To(Succeed())

			reconciler.Client = ciClient
			reconciler.APIReader = ciClient
			reconciler.ComputeInstanceNamespace = testCINamespace

			key := types.NamespacedName{Name: ipWithCI.Name, Namespace: ipWithCI.Namespace}

			// Pass 1: adds finalizer
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			// Pass 2: resolves pool and CI annotations
			_, err = reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(ciClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Annotations[osacPublicIPTargetNamespaceAnnotation]).To(Equal(testTenantNS))
		})

		It("should clear publicip-target-namespace annotation when computeInstance is cleared", func() {
			ipWithAnnotation := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ip-clear-ci",
					Namespace: testNamespace,
					Annotations: map[string]string{
						osacPublicIPTargetNamespaceAnnotation: testTenantNS,
					},
					Finalizers: []string{osacPublicIPFinalizer},
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool: testPoolUUID,
				},
			}

			clearClient := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(ipWithAnnotation, parentPool).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}).
				Build()

			reconciler.Client = clearClient
			reconciler.APIReader = clearClient

			key := types.NamespacedName{Name: ipWithAnnotation.Name, Namespace: ipWithAnnotation.Namespace}

			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIP{}
			Expect(clearClient.Get(testCtx, key, updated)).To(Succeed())
			_, exists := updated.Annotations[osacPublicIPTargetNamespaceAnnotation]
			Expect(exists).To(BeFalse())
		})

		It("should requeue when ComputeInstance not found", func() {
			ipMissingCI := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ip-missing-ci",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: "nonexistent-ci-uuid",
				},
			}

			missingClient := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(ipMissingCI, parentPool).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}).
				Build()

			reconciler.Client = missingClient
			reconciler.APIReader = missingClient
			reconciler.ComputeInstanceNamespace = testCINamespace

			key := types.NamespacedName{Name: ipMissingCI.Name, Namespace: ipMissingCI.Namespace}

			// Pass 1: adds finalizer
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			// Pass 2: CI not found, requeue
			result, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(defaultPreconditionRequeueInterval))
		})

		It("should requeue when ComputeInstance has no VirtualMachineReference", func() {
			ciNoTenant := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ci-no-tenant",
					Namespace: testCINamespace,
					Labels: map[string]string{
						osacComputeInstanceIDLabel: testCIUUID,
					},
				},
			}

			ipNoTenant := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ip-no-tenant",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: testCIUUID,
				},
			}

			noTenantClient := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(ipNoTenant, parentPool, ciNoTenant).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}, &osacv1alpha1.ComputeInstance{}).
				Build()

			reconciler.Client = noTenantClient
			reconciler.APIReader = noTenantClient
			reconciler.ComputeInstanceNamespace = testCINamespace

			key := types.NamespacedName{Name: ipNoTenant.Name, Namespace: ipNoTenant.Namespace}

			// Pass 1: adds finalizer
			_, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())

			// Pass 2: CI found but no VirtualMachineReference, requeue
			result, err := reconciler.Reconcile(testCtx, mcreconcile.Request{Request: ctrl.Request{NamespacedName: key}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(defaultPreconditionRequeueInterval))
		})
	})

	Context("mapComputeInstanceToPublicIPs", func() {
		const (
			mapTestCINamespace = "osac-computeinstance"
			mapTestCIUUID      = "ci-map-uuid-789"
		)

		It("should return requests for PublicIPs referencing the ComputeInstance UUID", func() {
			matchingIP := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ip-matching",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: mapTestCIUUID,
				},
			}
			unrelatedIP := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ip-unrelated",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: "other-ci-uuid",
				},
			}

			mapClient := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(matchingIP, unrelatedIP).
				Build()

			reconciler.Client = mapClient
			reconciler.NetworkingNamespace = testNamespace
			reconciler.ComputeInstanceNamespace = mapTestCINamespace

			ci := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci",
					Namespace: mapTestCINamespace,
					Labels: map[string]string{
						osacComputeInstanceIDLabel: mapTestCIUUID,
					},
				},
			}

			requests := reconciler.mapComputeInstanceToPublicIPs(testCtx, ci)
			Expect(requests).To(HaveLen(1))
			Expect(requests[0].NamespacedName).To(Equal(types.NamespacedName{
				Name:      "ip-matching",
				Namespace: testNamespace,
			}))
		})

		It("should return nil when ComputeInstance has no UUID label", func() {
			ci := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ci-no-label",
					Namespace: mapTestCINamespace,
				},
			}

			requests := reconciler.mapComputeInstanceToPublicIPs(testCtx, ci)
			Expect(requests).To(BeNil())
		})

		It("should return empty when no PublicIPs reference the ComputeInstance", func() {
			unrelatedIP := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ip-no-match",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: "different-uuid",
				},
			}

			mapClient := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(unrelatedIP).
				Build()

			reconciler.Client = mapClient
			reconciler.NetworkingNamespace = testNamespace

			ci := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ci-no-matches",
					Namespace: mapTestCINamespace,
					Labels: map[string]string{
						osacComputeInstanceIDLabel: mapTestCIUUID,
					},
				},
			}

			requests := reconciler.mapComputeInstanceToPublicIPs(testCtx, ci)
			Expect(requests).To(BeEmpty())
		})

		It("should return multiple requests when several PublicIPs reference the same ComputeInstance", func() {
			ip1 := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ip-multi-1",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: mapTestCIUUID,
				},
			}
			ip2 := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ip-multi-2",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: mapTestCIUUID,
				},
			}

			mapClient := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(ip1, ip2).
				Build()

			reconciler.Client = mapClient
			reconciler.NetworkingNamespace = testNamespace

			ci := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ci-multi",
					Namespace: mapTestCINamespace,
					Labels: map[string]string{
						osacComputeInstanceIDLabel: mapTestCIUUID,
					},
				},
			}

			requests := reconciler.mapComputeInstanceToPublicIPs(testCtx, ci)
			Expect(requests).To(HaveLen(2))
			Expect(requests).To(ContainElements(
				reconcile.Request{NamespacedName: types.NamespacedName{Name: "ip-multi-1", Namespace: testNamespace}},
				reconcile.Request{NamespacedName: types.NamespacedName{Name: "ip-multi-2", Namespace: testNamespace}},
			))
		})
	})

	// The provisioning lifecycle uses a config version (hash of spec + strategy) to
	// detect spec changes. When provisioning fails, the controller backs off with
	// exponential delay. But if the spec changes (new config version), it retries
	// immediately instead of waiting for the backoff to expire.
	Context("backoff on failure", func() {
		It("should backoff when latest job failed with matching ConfigVersion", func() {
			publicIP.Status.DesiredConfigVersion = testConfigVersion
			publicIP.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "failed-job",
					Type:          osacv1alpha1.JobTypeProvision,
					Timestamp:     metav1.NewTime(time.Now().UTC()),
					State:         osacv1alpha1.JobStateFailed,
					Message:       "provision failed",
					ConfigVersion: testConfigVersion,
				},
			}

			result, err := reconciler.handleProvisioning(testCtx, publicIP, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(result.RequeueAfter).To(BeNumerically("<=", provisioning.BackoffMaxDelay))
		})

		It("should trigger immediately when spec changed after failure", func() {
			mockProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID:        "retry-job",
					InitialState: osacv1alpha1.JobStatePending,
				}, nil
			}

			// The desired version is "version-2" but the failed job was for "version-1",
			// meaning the spec changed. The controller should retry immediately.
			publicIP.Status.DesiredConfigVersion = testConfigVersionUpdated
			publicIP.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "failed-job",
					Type:          osacv1alpha1.JobTypeProvision,
					Timestamp:     metav1.NewTime(time.Now().UTC().Add(-2 * time.Second)),
					State:         osacv1alpha1.JobStateFailed,
					Message:       "provision failed",
					ConfigVersion: testConfigVersion,
				},
			}

			result, err := reconciler.handleProvisioning(testCtx, publicIP, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(1 * time.Second))

			latestJob := provisioning.FindLatestJobByType(publicIP.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(latestJob).NotTo(BeNil())
			Expect(latestJob.JobID).To(Equal("retry-job"))
		})
	})

	Context("getPublicIPAddress", func() {
		It("should return IP from LoadBalancer Service ingress", func() {
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPServiceNamePrefix + "test-publicip",
					Namespace: defaultMetalLBNamespace,
				},
				Status: corev1.ServiceStatus{
					LoadBalancer: corev1.LoadBalancerStatus{
						Ingress: []corev1.LoadBalancerIngress{
							{IP: "203.0.113.42"},
						},
					},
				},
			}
			targetClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(svc).Build()

			ip := reconciler.getPublicIPAddress(testCtx, targetClient, "test-publicip")
			Expect(ip).To(Equal("203.0.113.42"))
		})

		It("should return empty string when Service not found", func() {
			targetClient := fake.NewClientBuilder().WithScheme(testScheme).Build()

			ip := reconciler.getPublicIPAddress(testCtx, targetClient, "nonexistent")
			Expect(ip).To(Equal(""))
		})

		It("should return empty string when ingress list is empty", func() {
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPServiceNamePrefix + "test-publicip",
					Namespace: defaultMetalLBNamespace,
				},
				Status: corev1.ServiceStatus{
					LoadBalancer: corev1.LoadBalancerStatus{
						Ingress: []corev1.LoadBalancerIngress{},
					},
				},
			}
			targetClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(svc).Build()

			ip := reconciler.getPublicIPAddress(testCtx, targetClient, "test-publicip")
			Expect(ip).To(Equal(""))
		})

		It("should return empty string when ingress IP is empty", func() {
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPServiceNamePrefix + "test-publicip",
					Namespace: defaultMetalLBNamespace,
				},
				Status: corev1.ServiceStatus{
					LoadBalancer: corev1.LoadBalancerStatus{
						Ingress: []corev1.LoadBalancerIngress{
							{IP: ""},
						},
					},
				},
			}
			targetClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(svc).Build()

			ip := reconciler.getPublicIPAddress(testCtx, targetClient, "test-publicip")
			Expect(ip).To(Equal(""))
		})

		It("should not populate address before provisioning succeeds (temporal ordering)", func() {
			key := types.NamespacedName{Name: publicIP.Name, Namespace: publicIP.Namespace}

			// A LoadBalancer Service with an assigned IP exists on the workload cluster.
			// The guard condition (state==Allocated && address=="") must prevent
			// address population until provisioning has completed.
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      publicIPServiceNamePrefix + "test-publicip",
					Namespace: defaultMetalLBNamespace,
				},
				Status: corev1.ServiceStatus{
					LoadBalancer: corev1.LoadBalancerStatus{
						Ingress: []corev1.LoadBalancerIngress{
							{IP: "203.0.113.42"},
						},
					},
				},
			}
			targetClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(svc).Build()
			reconciler.mgr = &mockMulticlusterManager{targetClient: targetClient}

			// Keep jobs in Running state so OnSuccess does not fire and
			// override the state we set for each sub-test.
			mockProvider.getProvisionStatusFunc = func(
				_ context.Context, _ client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateRunning,
					Message: "Job running",
				}, nil
			}

			// Pending state: address must NOT be populated (pre-provisioning).
			// Call handleUpdate and verify the in-memory object.
			pendingIP := publicIP.DeepCopy()
			pendingIP.Status.Phase = osacv1alpha1.PublicIPPhaseProgressing
			pendingIP.Status.State = osacv1alpha1.PublicIPStatePending
			pendingIP.Status.Address = ""

			_, err := reconciler.handleUpdate(testCtx, pendingIP)
			Expect(err).NotTo(HaveOccurred())
			Expect(pendingIP.Status.Address).To(Equal(""), "address must not be populated in Pending state")

			// Allocated state: address SHOULD be populated (post-provisioning).
			// Re-read the object from the fake client to get the current
			// resourceVersion (handleUpdate modifies the object in the store).
			allocatedIP := &osacv1alpha1.PublicIP{}
			Expect(fakeClient.Get(testCtx, key, allocatedIP)).To(Succeed())
			allocatedIP.Status.State = osacv1alpha1.PublicIPStateAllocated
			allocatedIP.Status.Address = ""

			_, err = reconciler.handleUpdate(testCtx, allocatedIP)
			Expect(err).NotTo(HaveOccurred())
			Expect(allocatedIP.Status.Address).To(Equal("203.0.113.42"), "address should be populated in Allocated state")
		})
	})

	// -----------------------------------------------------------------------
	// Auto-detach on ComputeInstance deletion
	//
	// These tests verify the behavior introduced in Phase 3 Plan 01:
	//   - handleAutoDetach: detects CI deletion and acts based on PublicIP state
	//   - maybeRemoveCIFinalizer: removes CI finalizer when all PublicIPs detached
	//   - syncComputeInstanceTargetNamespaceAnnotation: CI not found edge cases
	//
	// Each test creates a fresh fakeClient and reconciler to avoid shared state.
	// The Recorder field is left nil (production code has nil-check guards).
	// -----------------------------------------------------------------------
	Context("auto-detach on ComputeInstance deletion", func() {

		// createDeletingCI creates a ComputeInstance with DeletionTimestamp set,
		// simulating a CI that is being deleted. The CI must have at least one
		// finalizer for DeletionTimestamp to be valid in Kubernetes.
		createDeletingCI := func(uuid string, finalizers ...string) *osacv1alpha1.ComputeInstance {
			if len(finalizers) == 0 {
				finalizers = []string{"some-other-controller-finalizer"}
			}
			ci := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "deleting-ci",
					Namespace:  testNamespace,
					Labels:     map[string]string{osacComputeInstanceIDLabel: uuid},
					Finalizers: finalizers,
				},
				Status: osacv1alpha1.ComputeInstanceStatus{
					VirtualMachineReference: &osacv1alpha1.VirtualMachineReferenceType{
						Namespace:                  testNamespace,
						KubeVirtVirtualMachineName: "test-vm",
					},
				},
			}
			now := metav1.Now()
			ci.DeletionTimestamp = &now
			return ci
		}

		It("should auto-detach from Attached state and retain status.address", func() {
			deletingCI := createDeletingCI(testComputeInstance)

			pip := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pip-attached",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: testComputeInstance,
				},
			}

			fc := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(pip, parentPool, deletingCI).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}, &osacv1alpha1.ComputeInstance{}).
				Build()

			// Persist CI status (WithStatusSubresource strips it from WithObjects)
			Expect(fc.Status().Update(testCtx, deletingCI)).To(Succeed())

			pip.Status.Phase = osacv1alpha1.PublicIPPhaseReady
			pip.Status.State = osacv1alpha1.PublicIPStateAttached
			pip.Status.Address = "10.0.0.1"
			Expect(fc.Status().Update(testCtx, pip)).To(Succeed())

			emptyTarget := fake.NewClientBuilder().WithScheme(testScheme).Build()
			rec := &PublicIPReconciler{
				Client:                   fc,
				APIReader:                fc,
				Scheme:                   testScheme,
				mgr:                      &mockMulticlusterManager{targetClient: emptyTarget},
				NetworkingNamespace:      testNamespace,
				ComputeInstanceNamespace: testNamespace,
				ProvisioningProvider:     mockProvider,
				StatusPollInterval:       1 * time.Second,
				MaxJobHistory:            10,
			}

			// Re-set DeletionTimestamp in memory (fake client stores without it)
			now := metav1.Now()
			deletingCI.DeletionTimestamp = &now

			result, err := rec.handleAutoDetach(testCtx, pip, deletingCI)
			Expect(err).NotTo(HaveOccurred())

			Expect(result.specChanged).To(BeTrue(), "spec should be changed (computeInstance cleared)")
			Expect(pip.Spec.ComputeInstance).To(Equal(""), "spec.computeInstance should be cleared")
			Expect(pip.Status.Address).To(Equal("10.0.0.1"), "status.address should be retained during auto-detach")

			// Verify CI now has the detach finalizer
			updatedCI := &osacv1alpha1.ComputeInstance{}
			Expect(fc.Get(testCtx, client.ObjectKeyFromObject(deletingCI), updatedCI)).To(Succeed())
			Expect(updatedCI.Finalizers).To(ContainElement(osacPublicIPDetachFinalizer))
		})

		It("should requeue auto-detach when CI deleted during Attaching state", func() {
			deletingCI := createDeletingCI(testComputeInstance)

			pip := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pip-attaching",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: testComputeInstance,
				},
			}

			fc := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(pip, parentPool, deletingCI).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}, &osacv1alpha1.ComputeInstance{}).
				Build()

			Expect(fc.Status().Update(testCtx, deletingCI)).To(Succeed())

			pip.Status.Phase = osacv1alpha1.PublicIPPhaseProgressing
			pip.Status.State = osacv1alpha1.PublicIPStateAttaching
			Expect(fc.Status().Update(testCtx, pip)).To(Succeed())

			emptyTarget := fake.NewClientBuilder().WithScheme(testScheme).Build()
			rec := &PublicIPReconciler{
				Client:                   fc,
				APIReader:                fc,
				Scheme:                   testScheme,
				mgr:                      &mockMulticlusterManager{targetClient: emptyTarget},
				NetworkingNamespace:      testNamespace,
				ComputeInstanceNamespace: testNamespace,
				ProvisioningProvider:     mockProvider,
				StatusPollInterval:       1 * time.Second,
				MaxJobHistory:            10,
			}

			now := metav1.Now()
			deletingCI.DeletionTimestamp = &now

			result, err := rec.handleAutoDetach(testCtx, pip, deletingCI)
			Expect(err).NotTo(HaveOccurred())

			Expect(result.requeue).To(BeTrue(), "should requeue for in-flight attach")
			Expect(result.specChanged).To(BeFalse(), "spec should not change during in-flight attach")
			Expect(pip.Spec.ComputeInstance).To(Equal(testComputeInstance), "spec.computeInstance should be unchanged")

			// CI should still get the detach finalizer (even though no detach yet)
			updatedCI := &osacv1alpha1.ComputeInstance{}
			Expect(fc.Get(testCtx, client.ObjectKeyFromObject(deletingCI), updatedCI)).To(Succeed())
			Expect(updatedCI.Finalizers).To(ContainElement(osacPublicIPDetachFinalizer))
		})

		It("should no-op auto-detach when CI deleted during Releasing state", func() {
			deletingCI := createDeletingCI(testComputeInstance)

			pip := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pip-releasing",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: "",
				},
			}

			fc := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(pip, parentPool, deletingCI).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}, &osacv1alpha1.ComputeInstance{}).
				Build()

			Expect(fc.Status().Update(testCtx, deletingCI)).To(Succeed())

			pip.Status.Phase = osacv1alpha1.PublicIPPhaseProgressing
			pip.Status.State = osacv1alpha1.PublicIPStateReleasing
			Expect(fc.Status().Update(testCtx, pip)).To(Succeed())

			emptyTarget := fake.NewClientBuilder().WithScheme(testScheme).Build()
			rec := &PublicIPReconciler{
				Client:                   fc,
				APIReader:                fc,
				Scheme:                   testScheme,
				mgr:                      &mockMulticlusterManager{targetClient: emptyTarget},
				NetworkingNamespace:      testNamespace,
				ComputeInstanceNamespace: testNamespace,
				ProvisioningProvider:     mockProvider,
				StatusPollInterval:       1 * time.Second,
				MaxJobHistory:            10,
			}

			now := metav1.Now()
			deletingCI.DeletionTimestamp = &now

			result, err := rec.handleAutoDetach(testCtx, pip, deletingCI)
			Expect(err).NotTo(HaveOccurred())

			Expect(result.requeue).To(BeFalse(), "should not requeue (detach already in progress)")
			Expect(result.specChanged).To(BeFalse(), "spec should not change")
		})

		It("should auto-detach by clearing spec on Failed state when CI deleted", func() {
			deletingCI := createDeletingCI(testComputeInstance)

			pip := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pip-failed",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: testComputeInstance,
				},
			}

			fc := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(pip, parentPool, deletingCI).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}, &osacv1alpha1.ComputeInstance{}).
				Build()

			Expect(fc.Status().Update(testCtx, deletingCI)).To(Succeed())

			pip.Status.Phase = osacv1alpha1.PublicIPPhaseFailed
			pip.Status.State = osacv1alpha1.PublicIPStateFailed
			Expect(fc.Status().Update(testCtx, pip)).To(Succeed())

			emptyTarget := fake.NewClientBuilder().WithScheme(testScheme).Build()
			rec := &PublicIPReconciler{
				Client:                   fc,
				APIReader:                fc,
				Scheme:                   testScheme,
				mgr:                      &mockMulticlusterManager{targetClient: emptyTarget},
				NetworkingNamespace:      testNamespace,
				ComputeInstanceNamespace: testNamespace,
				ProvisioningProvider:     mockProvider,
				StatusPollInterval:       1 * time.Second,
				MaxJobHistory:            10,
			}

			now := metav1.Now()
			deletingCI.DeletionTimestamp = &now

			result, err := rec.handleAutoDetach(testCtx, pip, deletingCI)
			Expect(err).NotTo(HaveOccurred())

			Expect(result.specChanged).To(BeTrue(), "spec should be changed (stale ref cleared)")
			Expect(pip.Spec.ComputeInstance).To(Equal(""), "spec.computeInstance should be cleared")
			Expect(pip.Status.State).To(Equal(osacv1alpha1.PublicIPStateFailed), "state should remain Failed")
		})

		It("should keep CI auto-detach finalizer when multiple PublicIPs still reference CI", func() {
			deletingCI := createDeletingCI(testComputeInstance)

			pip1 := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pip-multi-1",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: testComputeInstance,
				},
			}
			pip2 := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pip-multi-2",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: testComputeInstance,
				},
			}

			fc := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(pip1, pip2, parentPool, deletingCI).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}, &osacv1alpha1.ComputeInstance{}).
				Build()

			Expect(fc.Status().Update(testCtx, deletingCI)).To(Succeed())

			pip1.Status.Phase = osacv1alpha1.PublicIPPhaseReady
			pip1.Status.State = osacv1alpha1.PublicIPStateAttached
			Expect(fc.Status().Update(testCtx, pip1)).To(Succeed())

			pip2.Status.Phase = osacv1alpha1.PublicIPPhaseReady
			pip2.Status.State = osacv1alpha1.PublicIPStateAttached
			Expect(fc.Status().Update(testCtx, pip2)).To(Succeed())

			emptyTarget := fake.NewClientBuilder().WithScheme(testScheme).Build()
			rec := &PublicIPReconciler{
				Client:                   fc,
				APIReader:                fc,
				Scheme:                   testScheme,
				mgr:                      &mockMulticlusterManager{targetClient: emptyTarget},
				NetworkingNamespace:      testNamespace,
				ComputeInstanceNamespace: testNamespace,
				ProvisioningProvider:     mockProvider,
				StatusPollInterval:       1 * time.Second,
				MaxJobHistory:            10,
			}

			now := metav1.Now()
			deletingCI.DeletionTimestamp = &now

			// Auto-detach first PublicIP
			result, err := rec.handleAutoDetach(testCtx, pip1, deletingCI)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.specChanged).To(BeTrue())
			Expect(pip1.Spec.ComputeInstance).To(Equal(""))

			// Persist the cleared spec for pip1 so maybeRemoveCIFinalizer sees it
			Expect(fc.Get(testCtx, client.ObjectKeyFromObject(pip1), pip1)).To(Succeed())
			pip1.Spec.ComputeInstance = ""
			Expect(fc.Update(testCtx, pip1)).To(Succeed())

			// pip2 still references the CI, so finalizer should remain
			err = rec.maybeRemoveCIFinalizer(testCtx, testComputeInstance, "")
			Expect(err).NotTo(HaveOccurred())

			updatedCI := &osacv1alpha1.ComputeInstance{}
			Expect(fc.Get(testCtx, client.ObjectKeyFromObject(deletingCI), updatedCI)).To(Succeed())
			Expect(updatedCI.Finalizers).To(ContainElement(osacPublicIPDetachFinalizer),
				"finalizer should remain because pip2 still references CI")

			// Now clear pip2's spec (simulate second auto-detach completing)
			Expect(fc.Get(testCtx, client.ObjectKeyFromObject(pip2), pip2)).To(Succeed())
			pip2.Spec.ComputeInstance = ""
			Expect(fc.Update(testCtx, pip2)).To(Succeed())

			// Now finalizer should be removed (no more references)
			err = rec.maybeRemoveCIFinalizer(testCtx, testComputeInstance, "")
			Expect(err).NotTo(HaveOccurred())

			Expect(fc.Get(testCtx, client.ObjectKeyFromObject(deletingCI), updatedCI)).To(Succeed())
			Expect(updatedCI.Finalizers).NotTo(ContainElement(osacPublicIPDetachFinalizer),
				"finalizer should be removed after all PublicIPs detached")
		})

		It("should remove CI finalizer after single PublicIP detaches", func() {
			// CI already has the detach finalizer (simulates state after handleAutoDetach ran)
			ci := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ci-with-finalizer",
					Namespace: testNamespace,
					Labels:    map[string]string{osacComputeInstanceIDLabel: testComputeInstance},
					Finalizers: []string{
						"some-other-controller-finalizer",
						osacPublicIPDetachFinalizer,
					},
				},
				Status: osacv1alpha1.ComputeInstanceStatus{
					VirtualMachineReference: &osacv1alpha1.VirtualMachineReferenceType{
						Namespace:                  testNamespace,
						KubeVirtVirtualMachineName: "test-vm",
					},
				},
			}

			// PublicIP has already detached (spec.computeInstance is empty, state is Allocated)
			pip := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pip-already-detached",
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: "",
				},
			}

			fc := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(pip, parentPool, ci).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}, &osacv1alpha1.ComputeInstance{}).
				Build()

			Expect(fc.Status().Update(testCtx, ci)).To(Succeed())

			pip.Status.Phase = osacv1alpha1.PublicIPPhaseReady
			pip.Status.State = osacv1alpha1.PublicIPStateAllocated
			Expect(fc.Status().Update(testCtx, pip)).To(Succeed())

			emptyTarget := fake.NewClientBuilder().WithScheme(testScheme).Build()
			rec := &PublicIPReconciler{
				Client:                   fc,
				APIReader:                fc,
				Scheme:                   testScheme,
				mgr:                      &mockMulticlusterManager{targetClient: emptyTarget},
				NetworkingNamespace:      testNamespace,
				ComputeInstanceNamespace: testNamespace,
				ProvisioningProvider:     mockProvider,
				StatusPollInterval:       1 * time.Second,
				MaxJobHistory:            10,
			}

			err := rec.maybeRemoveCIFinalizer(testCtx, testComputeInstance, "")
			Expect(err).NotTo(HaveOccurred())

			updatedCI := &osacv1alpha1.ComputeInstance{}
			Expect(fc.Get(testCtx, client.ObjectKeyFromObject(ci), updatedCI)).To(Succeed())
			Expect(updatedCI.Finalizers).NotTo(ContainElement(osacPublicIPDetachFinalizer),
				"detach finalizer should be removed after all PublicIPs detached")
			Expect(updatedCI.Finalizers).To(ContainElement("some-other-controller-finalizer"),
				"other finalizers should be preserved")
		})

		It("should clear spec when CI not found and PublicIP is Attached (Pitfall 3 edge case)", func() {
			// PublicIP has a stale reference to a CI that no longer exists.
			// syncComputeInstanceTargetNamespaceAnnotation should clear the reference.
			pip := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "pip-stale-ref",
					Namespace:  testNamespace,
					Finalizers: []string{osacPublicIPFinalizer},
					Annotations: map[string]string{
						osacPublicIPTargetNamespaceAnnotation: "old-tenant-ns",
					},
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: "missing-ci-uuid",
				},
			}

			fc := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(pip, parentPool).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}).
				Build()

			pip.Status.Phase = osacv1alpha1.PublicIPPhaseReady
			pip.Status.State = osacv1alpha1.PublicIPStateAttached
			pip.Status.Address = "10.0.0.99"
			Expect(fc.Status().Update(testCtx, pip)).To(Succeed())

			emptyTarget := fake.NewClientBuilder().WithScheme(testScheme).Build()
			rec := &PublicIPReconciler{
				Client:                   fc,
				APIReader:                fc,
				Scheme:                   testScheme,
				mgr:                      &mockMulticlusterManager{targetClient: emptyTarget},
				NetworkingNamespace:      testNamespace,
				ComputeInstanceNamespace: testNamespace,
				ProvisioningProvider:     mockProvider,
				StatusPollInterval:       1 * time.Second,
				MaxJobHistory:            10,
			}

			// Re-read to get current resourceVersion
			Expect(fc.Get(testCtx, client.ObjectKeyFromObject(pip), pip)).To(Succeed())

			changed, requeue, err := rec.syncComputeInstanceTargetNamespaceAnnotation(testCtx, pip)
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue(), "should return changed=true (stale ref cleared)")
			Expect(requeue).To(BeFalse(), "should not requeue")
			Expect(pip.Spec.ComputeInstance).To(Equal(""), "spec.computeInstance should be cleared for missing CI")
		})

		It("should requeue when CI not found and PublicIP is Pending (existing behavior)", func() {
			// A Pending PublicIP with a CI reference should requeue, not clear the ref.
			// The CI may just not have been created yet.
			pip := &osacv1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "pip-pending-missing-ci",
					Namespace:  testNamespace,
					Finalizers: []string{osacPublicIPFinalizer},
				},
				Spec: osacv1alpha1.PublicIPSpec{
					Pool:            testPoolUUID,
					ComputeInstance: "missing-ci-uuid",
				},
			}

			fc := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(pip, parentPool).
				WithStatusSubresource(&osacv1alpha1.PublicIP{}).
				Build()

			pip.Status.Phase = osacv1alpha1.PublicIPPhaseProgressing
			pip.Status.State = osacv1alpha1.PublicIPStatePending
			Expect(fc.Status().Update(testCtx, pip)).To(Succeed())

			emptyTarget := fake.NewClientBuilder().WithScheme(testScheme).Build()
			rec := &PublicIPReconciler{
				Client:                   fc,
				APIReader:                fc,
				Scheme:                   testScheme,
				mgr:                      &mockMulticlusterManager{targetClient: emptyTarget},
				NetworkingNamespace:      testNamespace,
				ComputeInstanceNamespace: testNamespace,
				ProvisioningProvider:     mockProvider,
				StatusPollInterval:       1 * time.Second,
				MaxJobHistory:            10,
			}

			Expect(fc.Get(testCtx, client.ObjectKeyFromObject(pip), pip)).To(Succeed())

			changed, requeue, err := rec.syncComputeInstanceTargetNamespaceAnnotation(testCtx, pip)
			Expect(err).NotTo(HaveOccurred())
			Expect(requeue).To(BeTrue(), "should requeue for Pending PublicIP with missing CI")
			Expect(changed).To(BeFalse(), "spec should not change")
			Expect(pip.Spec.ComputeInstance).To(Equal("missing-ci-uuid"), "spec.computeInstance should be unchanged")
		})
	})
})
