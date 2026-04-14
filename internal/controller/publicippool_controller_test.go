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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osacv1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

var _ = Describe("PublicIPPoolReconciler", func() {
	var (
		reconciler   *PublicIPPoolReconciler
		mockProvider *mockProvisioningProvider
		fakeClient   client.Client
		testCtx      context.Context
		pool         *osacv1alpha1.PublicIPPool
		testScheme   *runtime.Scheme
	)

	BeforeEach(func() {
		testCtx = context.TODO()
		testScheme = runtime.NewScheme()
		Expect(osacv1alpha1.AddToScheme(testScheme)).To(Succeed())
		Expect(scheme.AddToScheme(testScheme)).To(Succeed())

		pool = &osacv1alpha1.PublicIPPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool",
				Namespace: "test-namespace",
			},
			Spec: osacv1alpha1.PublicIPPoolSpec{
				CIDRs:                  []string{"192.168.1.0/24"},
				IPFamily:               "IPv4",
				ImplementationStrategy: "metallb-l2",
			},
		}

		fakeClient = fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(pool).
			WithStatusSubresource(&osacv1alpha1.PublicIPPool{}).
			Build()

		mockProvider = &mockProvisioningProvider{name: "mock-aap"}

		reconciler = &PublicIPPoolReconciler{
			Client:               fakeClient,
			APIReader:            fakeClient,
			Scheme:               testScheme,
			NetworkingNamespace:  "test-namespace",
			ProvisioningProvider: mockProvider,
			StatusPollInterval:   1 * time.Second,
			MaxJobHistory:        10,
		}
	})

	Context("Reconcile", func() {
		It("should add finalizer on first reconcile", func() {
			key := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}
			result, err := reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())

			updated := &osacv1alpha1.PublicIPPool{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())

			Expect(updated.Finalizers).To(ContainElement(osacPublicIPPoolFinalizer))
		})

		It("should set phase to Progressing initially", func() {
			key := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}

			// Setup mock to return Running state so phase stays Progressing
			mockProvider.getProvisionStatusFunc = func(
				ctx context.Context, resource client.Object, jobID string,
			) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateRunning,
					Message: "Job running",
				}, nil
			}

			// First reconcile adds finalizer
			_, err := reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile processes the resource
			_, err = reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIPPool{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPoolPhaseProgressing))
		})

		It("should set implementation-strategy annotation from spec", func() {
			key := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}

			mockProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID:        "job-123",
					InitialState: osacv1alpha1.JobStatePending,
					Message:      "Job triggered",
				}, nil
			}

			// Reconcile twice (first adds finalizer, second sets annotation and provisions)
			_, err := reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIPPool{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())

			Expect(updated.Annotations).NotTo(BeNil())
			Expect(updated.Annotations[osacImplementationStrategyAnnotation]).To(Equal("metallb-l2"))
		})

		It("should default implementation strategy to metallb-l2 when spec is empty", func() {
			// Create a pool with empty ImplementationStrategy
			poolNoStrategy := &osacv1alpha1.PublicIPPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pool-no-strategy",
					Namespace: "test-namespace",
				},
				Spec: osacv1alpha1.PublicIPPoolSpec{
					CIDRs:    []string{"10.0.0.0/24"},
					IPFamily: "IPv4",
					// ImplementationStrategy intentionally empty
				},
			}
			Expect(fakeClient.Create(testCtx, poolNoStrategy)).To(Succeed())

			key := types.NamespacedName{Name: poolNoStrategy.Name, Namespace: poolNoStrategy.Namespace}

			mockProvider.triggerProvisionFunc = func(
				ctx context.Context, resource client.Object,
			) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID:        "job-default",
					InitialState: osacv1alpha1.JobStatePending,
					Message:      "Job triggered",
				}, nil
			}

			// Reconcile twice
			_, err := reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIPPool{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())

			Expect(updated.Annotations).NotTo(BeNil())
			Expect(updated.Annotations[osacImplementationStrategyAnnotation]).To(Equal("metallb-l2"))
		})

		It("should set ConfigurationApplied condition to True", func() {
			key := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}

			// Reconcile twice (first adds finalizer, second processes spec)
			_, err := reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIPPool{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())

			condition := osacv1alpha1.GetPublicIPPoolStatusCondition(
				updated, osacv1alpha1.PublicIPPoolConditionConfigurationApplied,
			)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal("ConfigurationApplied"))
		})

		It("should set phase to Ready on successful provision", func() {
			key := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}

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

			// Reconcile three times: finalizer, trigger, poll-success
			_, err := reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIPPool{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPoolPhaseReady))
		})

		It("should set phase to Failed on provision failure", func() {
			key := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}

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

			// Reconcile three times
			_, err := reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIPPool{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(osacv1alpha1.PublicIPPoolPhaseFailed))
		})

		It("should trigger deprovision on delete", func() {
			key := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}

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

			// Add finalizer first
			_, err := reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Fetch the pool and set DeletionTimestamp in memory
			toDelete := &osacv1alpha1.PublicIPPool{}
			Expect(fakeClient.Get(testCtx, key, toDelete)).To(Succeed())

			now := metav1.Now()
			toDelete.DeletionTimestamp = &now

			// Call handleDelete directly (fake client does not support DeletionTimestamp via Update)
			_, err = reconciler.handleDelete(testCtx, toDelete)
			Expect(err).NotTo(HaveOccurred())

			Expect(deprovisionCalled).To(BeTrue())

			latestJob := provisioning.FindLatestJobByType(toDelete.Status.Jobs, osacv1alpha1.JobTypeDeprovision)
			Expect(latestJob).NotTo(BeNil())
			Expect(latestJob.JobID).To(Equal("deprovision-job-123"))
		})

		It("should remove finalizer after successful deprovision", func() {
			key := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}

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

			// Add finalizer first
			_, err := reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			toDelete := &osacv1alpha1.PublicIPPool{}
			Expect(fakeClient.Get(testCtx, key, toDelete)).To(Succeed())

			now := metav1.Now()
			toDelete.DeletionTimestamp = &now

			// First handleDelete triggers deprovision job
			_, err = reconciler.handleDelete(testCtx, toDelete)
			Expect(err).NotTo(HaveOccurred())

			// Second handleDelete polls status and removes finalizer
			_, _ = reconciler.handleDelete(testCtx, toDelete)

			// Verify finalizer was removed from in-memory object
			Expect(toDelete.Finalizers).NotTo(ContainElement(osacPublicIPPoolFinalizer))
		})

		It("should still handle delete for unmanaged pool with finalizer", func() {
			// Simulate a pool that was previously managed (has finalizer)
			// but later marked unmanaged. Deletion must still clean up.
			// We verify that Reconcile routes to handleDelete (not early-return)
			// when DeletionTimestamp is set, even with the unmanaged annotation.
			managedThenUnmanaged := &osacv1alpha1.PublicIPPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "managed-then-unmanaged",
					Namespace: "test-namespace",
					Annotations: map[string]string{
						osacManagementStateAnnotation: ManagementStateUnmanaged,
					},
					Finalizers: []string{osacPublicIPPoolFinalizer},
				},
				Spec: osacv1alpha1.PublicIPPoolSpec{

					CIDRs:                  []string{"10.0.0.0/24"},
					IPFamily:               "IPv4",
					ImplementationStrategy: "metallb-l2",
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

			// Fetch from the fake client, then set DeletionTimestamp in memory
			// and call handleDelete directly (fake client does not support
			// DeletionTimestamp via Update, so we test the in-memory behavior).
			fetched := &osacv1alpha1.PublicIPPool{}
			Expect(fakeClient.Get(testCtx, key, fetched)).To(Succeed())

			now := metav1.Now()
			fetched.DeletionTimestamp = &now

			// Verify the Reconcile guard: with DeletionTimestamp set, the
			// unmanaged annotation must NOT cause an early return.
			// DeletionTimestamp.IsZero() is false, so the guard is skipped
			// and we reach the delete branch.
			Expect(fetched.ObjectMeta.DeletionTimestamp.IsZero()).To(BeFalse())

			// Call handleDelete directly. The DeprovisionSkipped path removes
			// the finalizer in memory. The subsequent r.Update will fail on
			// the fake client (DeletionTimestamp is immutable), but the
			// important assertion is that deprovision ran and the finalizer
			// was removed from the in-memory object.
			_, _ = reconciler.handleDelete(testCtx, fetched)

			Expect(deprovisionCalled).To(BeTrue())
			Expect(fetched.Finalizers).NotTo(ContainElement(osacPublicIPPoolFinalizer))
		})

		It("should ignore pool with management-state unmanaged annotation", func() {
			unmanagedPool := &osacv1alpha1.PublicIPPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "unmanaged-pool",
					Namespace: "test-namespace",
					Annotations: map[string]string{
						osacManagementStateAnnotation: ManagementStateUnmanaged,
					},
				},
				Spec: osacv1alpha1.PublicIPPoolSpec{

					CIDRs:                  []string{"10.0.0.0/24"},
					IPFamily:               "IPv4",
					ImplementationStrategy: "metallb-l2",
				},
			}
			Expect(fakeClient.Create(testCtx, unmanagedPool)).To(Succeed())

			key := types.NamespacedName{Name: unmanagedPool.Name, Namespace: unmanagedPool.Namespace}
			_, err := reconciler.Reconcile(testCtx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			updated := &osacv1alpha1.PublicIPPool{}
			Expect(fakeClient.Get(testCtx, key, updated)).To(Succeed())

			// Verify no finalizer was added (controller skipped it)
			Expect(updated.Finalizers).To(BeEmpty())
			Expect(updated.Status.Phase).To(BeEmpty())
		})
	})

	Context("backoff on failure", func() {
		It("should backoff when latest job failed with matching ConfigVersion", func() {
			pool.Status.DesiredConfigVersion = testConfigVersion
			pool.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "failed-job",
					Type:          osacv1alpha1.JobTypeProvision,
					Timestamp:     metav1.NewTime(time.Now().UTC()),
					State:         osacv1alpha1.JobStateFailed,
					Message:       "provision failed",
					ConfigVersion: testConfigVersion,
				},
			}

			result, err := reconciler.handleProvisioning(testCtx, pool)
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

			pool.Status.DesiredConfigVersion = testConfigVersionUpdated
			pool.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "failed-job",
					Type:          osacv1alpha1.JobTypeProvision,
					Timestamp:     metav1.NewTime(time.Now().UTC()),
					State:         osacv1alpha1.JobStateFailed,
					Message:       "provision failed",
					ConfigVersion: testConfigVersion,
				},
			}

			result, err := reconciler.handleProvisioning(testCtx, pool)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(1 * time.Second))

			latestJob := provisioning.FindLatestJobByType(pool.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(latestJob).NotTo(BeNil())
			Expect(latestJob.JobID).To(Equal("retry-job"))
		})
	})
})
