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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	osacv1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

var _ = Describe("VirtualNetworkReconciler", func() {
	var (
		reconciler   *VirtualNetworkReconciler
		mockProvider *mockVirtualNetworkProvider
		ctx          context.Context
		vnet         *osacv1alpha1.VirtualNetwork
	)

	BeforeEach(func() {
		ctx = context.TODO()
		mockProvider = &mockVirtualNetworkProvider{}
		reconciler = &VirtualNetworkReconciler{
			Client:               k8sClient,
			APIReader:            k8sClient,
			Scheme:               k8sClient.Scheme(),
			NetworkingNamespace:  "default",
			ProvisioningProvider: mockProvider,
			StatusPollInterval:   1 * time.Second,
			MaxJobHistory:        10,
		}

		// Create VirtualNetwork fixture with ImplementationStrategy
		vnet = &osacv1alpha1.VirtualNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-vnet",
				Namespace: "default",
			},
			Spec: osacv1alpha1.VirtualNetworkSpec{
				Region:                 "us-west-1",
				IPv4CIDR:               "10.0.0.0/16",
				NetworkClass:           "cudn-net",
				ImplementationStrategy: "cudn-net",
			},
		}
	})

	AfterEach(func() {
		// Cleanup VirtualNetwork if it exists
		vnetKey := types.NamespacedName{Name: vnet.Name, Namespace: vnet.Namespace}
		existingVnet := &osacv1alpha1.VirtualNetwork{}
		if err := k8sClient.Get(ctx, vnetKey, existingVnet); err == nil {
			existingVnet.Finalizers = nil
			_ = k8sClient.Update(ctx, existingVnet)
			_ = k8sClient.Delete(ctx, existingVnet)
		}
	})

	Context("Reconcile", func() {
		It("should add finalizer on first reconcile", func() {
			Expect(k8sClient.Create(ctx, vnet)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      vnet.Name,
					Namespace: vnet.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Fetch updated VirtualNetwork
			updatedVnet := &osacv1alpha1.VirtualNetwork{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: vnet.Name, Namespace: vnet.Namespace}, updatedVnet)).To(Succeed())
			Expect(updatedVnet.Finalizers).To(ContainElement(osacVirtualNetworkFinalizer))
		})

		It("should set phase to Progressing on first reconcile", func() {
			Expect(k8sClient.Create(ctx, vnet)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      vnet.Name,
					Namespace: vnet.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Fetch updated VirtualNetwork
			updatedVnet := &osacv1alpha1.VirtualNetwork{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: vnet.Name, Namespace: vnet.Namespace}, updatedVnet)).To(Succeed())
			Expect(updatedVnet.Status.Phase).To(Equal(osacv1alpha1.VirtualNetworkPhaseProgressing))
		})

		It("should read ImplementationStrategy during reconcile", func() {
			Expect(k8sClient.Create(ctx, vnet)).To(Succeed())

			mockProvider.triggerProvisionFunc = func(ctx context.Context, resource client.Object) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID:        "test-job-123",
					InitialState: osacv1alpha1.JobStatePending,
					Message:      "Provisioning triggered",
				}, nil
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      vnet.Name,
					Namespace: vnet.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify job was triggered (confirms ImplementationStrategy was read successfully)
			updatedVnet := &osacv1alpha1.VirtualNetwork{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: vnet.Name, Namespace: vnet.Namespace}, updatedVnet)).To(Succeed())
			latestJob := provisioning.FindLatestJobByType(updatedVnet.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(latestJob).NotTo(BeNil())
		})

		It("should requeue if ImplementationStrategy not set", func() {
			// Create VirtualNetwork without ImplementationStrategy
			vnetNoStrategy := &osacv1alpha1.VirtualNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vnet-no-strategy",
					Namespace: "default",
				},
				Spec: osacv1alpha1.VirtualNetworkSpec{
					Region:       "us-west-1",
					IPv4CIDR:     "10.0.0.0/16",
					NetworkClass: "some-class",
					// ImplementationStrategy intentionally not set
				},
			}
			Expect(k8sClient.Create(ctx, vnetNoStrategy)).To(Succeed())
			defer func() {
				vnetNoStrategy.Finalizers = nil
				_ = k8sClient.Update(ctx, vnetNoStrategy)
				_ = k8sClient.Delete(ctx, vnetNoStrategy)
			}()

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      vnetNoStrategy.Name,
					Namespace: vnetNoStrategy.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(defaultPreconditionRequeueInterval))
		})
	})

	Context("handleProvisioning", func() {
		It("should trigger provision job when no job exists", func() {
			mockProvider.triggerProvisionFunc = func(ctx context.Context, resource client.Object) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID:        "new-job-456",
					InitialState: osacv1alpha1.JobStatePending,
					Message:      "Provisioning job triggered",
				}, nil
			}

			result, err := reconciler.handleProvisioning(ctx, vnet)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(1 * time.Second))

			latestJob := provisioning.FindLatestJobByType(vnet.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(latestJob).NotTo(BeNil())
			Expect(latestJob.JobID).To(Equal("new-job-456"))
			Expect(latestJob.State).To(Equal(osacv1alpha1.JobStatePending))
		})

		It("should poll job status when job exists", func() {
			// Create initial job
			vnet.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:     "existing-job-789",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     osacv1alpha1.JobStateRunning,
					Message:   "Job running",
				},
			}

			mockProvider.getProvisionStatusFunc = func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateRunning,
					Message: "Still running",
				}, nil
			}

			result, err := reconciler.handleProvisioning(ctx, vnet)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(1 * time.Second))

			latestJob := provisioning.FindLatestJobByType(vnet.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(latestJob.State).To(Equal(osacv1alpha1.JobStateRunning))
		})

		It("should set phase to Ready when job succeeds", func() {
			vnet.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:     "success-job-101",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     osacv1alpha1.JobStateRunning,
					Message:   "Job running",
				},
			}

			mockProvider.getProvisionStatusFunc = func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateSucceeded,
					Message: "Job succeeded",
				}, nil
			}

			result, err := reconciler.handleProvisioning(ctx, vnet)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(0 * time.Second))
			Expect(vnet.Status.Phase).To(Equal(osacv1alpha1.VirtualNetworkPhaseReady))
		})

		It("should set phase to Failed when job fails", func() {
			vnet.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:     "failed-job-202",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     osacv1alpha1.JobStateRunning,
					Message:   "Job running",
				},
			}

			mockProvider.getProvisionStatusFunc = func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateFailed,
					Message: "Job failed",
				}, nil
			}

			result, err := reconciler.handleProvisioning(ctx, vnet)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(0 * time.Second))
			Expect(vnet.Status.Phase).To(Equal(osacv1alpha1.VirtualNetworkPhaseFailed))
		})
	})

	Context("backoff on failure", func() {
		It("should backoff when latest job failed with matching ConfigVersion", func() {
			vnet.Status.DesiredConfigVersion = testConfigVersion
			vnet.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "failed-job",
					Type:          osacv1alpha1.JobTypeProvision,
					Timestamp:     metav1.NewTime(time.Now().UTC()),
					State:         osacv1alpha1.JobStateFailed,
					Message:       "provision failed",
					ConfigVersion: testConfigVersion,
				},
			}

			result, err := reconciler.handleProvisioning(ctx, vnet)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(result.RequeueAfter).To(BeNumerically("<=", provisioning.BackoffMaxDelay))
		})

		It("should trigger immediately when spec changed after failure", func() {
			mockProvider.triggerProvisionFunc = func(ctx context.Context, resource client.Object) (*provisioning.ProvisionResult, error) {
				return &provisioning.ProvisionResult{
					JobID:        "retry-job",
					InitialState: osacv1alpha1.JobStatePending,
				}, nil
			}

			vnet.Status.DesiredConfigVersion = testConfigVersionUpdated
			vnet.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "failed-job",
					Type:          osacv1alpha1.JobTypeProvision,
					Timestamp:     metav1.NewTime(time.Now().UTC()),
					State:         osacv1alpha1.JobStateFailed,
					Message:       "provision failed",
					ConfigVersion: testConfigVersion,
				},
			}

			result, err := reconciler.handleProvisioning(ctx, vnet)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(1 * time.Second))

			latestJob := provisioning.FindLatestJobByType(vnet.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(latestJob).NotTo(BeNil())
			Expect(latestJob.JobID).To(Equal("retry-job"))
		})

		It("should skip when config already applied", func() {
			vnet.Status.DesiredConfigVersion = testConfigVersion
			vnet.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:         "succeeded-job",
					Type:          osacv1alpha1.JobTypeProvision,
					Timestamp:     metav1.NewTime(time.Now().UTC()),
					State:         osacv1alpha1.JobStateSucceeded,
					ConfigVersion: testConfigVersion,
				},
			}

			result, err := reconciler.handleProvisioning(ctx, vnet)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		})
	})

	Context("Job history management", func() {
		It("should limit job history to MaxJobHistory", func() {
			reconciler.MaxJobHistory = 3

			// Add 5 jobs
			for i := 1; i <= 5; i++ {
				newJob := osacv1alpha1.JobStatus{
					JobID:     "job-" + string(rune('0'+i)),
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC().Add(time.Duration(i) * time.Second)),
					State:     osacv1alpha1.JobStatePending,
					Message:   "Job triggered",
				}
				vnet.Status.Jobs = provisioning.AppendJob(vnet.Status.Jobs, newJob, reconciler.MaxJobHistory)
			}

			// Should only keep last 3 jobs
			Expect(vnet.Status.Jobs).To(HaveLen(3))
			Expect(vnet.Status.Jobs[0].JobID).To(Equal("job-3"))
			Expect(vnet.Status.Jobs[1].JobID).To(Equal("job-4"))
			Expect(vnet.Status.Jobs[2].JobID).To(Equal("job-5"))
		})
	})

	Context("handleDelete", func() {
		It("should trigger deprovision job on deletion", func() {
			vnet.Finalizers = []string{osacVirtualNetworkFinalizer}
			vnet.DeletionTimestamp = &metav1.Time{Time: time.Now()}

			mockProvider.triggerDeprovisionFunc = func(ctx context.Context, resource client.Object) (*provisioning.DeprovisionResult, error) {
				return &provisioning.DeprovisionResult{
					Action:                 provisioning.DeprovisionTriggered,
					JobID:                  "deprovision-job-303",
					BlockDeletionOnFailure: true,
				}, nil
			}

			result, err := reconciler.handleDelete(ctx, vnet)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(1 * time.Second))

			latestJob := provisioning.FindLatestJobByType(vnet.Status.Jobs, osacv1alpha1.JobTypeDeprovision)
			Expect(latestJob).NotTo(BeNil())
			Expect(latestJob.JobID).To(Equal("deprovision-job-303"))
			Expect(latestJob.BlockDeletionOnFailure).To(BeTrue())
		})

		It("should remove finalizer after successful deprovision", func() {
			vnet.Finalizers = []string{osacVirtualNetworkFinalizer}

			mockProvider.getDeprovisionStatusFunc = func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateSucceeded,
					Message: "Deprovision succeeded",
				}, nil
			}

			// Create VirtualNetwork in cluster
			Expect(k8sClient.Create(ctx, vnet)).To(Succeed())

			// Set up the status with a running deprovision job (status is a subresource)
			vnet.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:     "deprovision-job-404",
					Type:      osacv1alpha1.JobTypeDeprovision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     osacv1alpha1.JobStateRunning,
					Message:   "Deprovisioning",
				},
			}
			Expect(k8sClient.Status().Update(ctx, vnet)).To(Succeed())

			// Delete the VirtualNetwork to set DeletionTimestamp
			Expect(k8sClient.Delete(ctx, vnet)).To(Succeed())

			// Fetch the updated vnet with DeletionTimestamp set
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: vnet.Name, Namespace: vnet.Namespace}, vnet)).To(Succeed())

			result, err := reconciler.handleDelete(ctx, vnet)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(0 * time.Second))

			// Verify finalizer was removed from the in-memory object
			// (the resource may already be garbage collected after finalizer removal)
			Expect(vnet.Finalizers).NotTo(ContainElement(osacVirtualNetworkFinalizer))
		})
	})

	Context("Phase transitions", func() {
		It("should transition from Progressing to Ready on success", func() {
			vnet.Status.Phase = osacv1alpha1.VirtualNetworkPhaseProgressing
			vnet.Status.Jobs = []osacv1alpha1.JobStatus{
				{
					JobID:     "transition-job-505",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     osacv1alpha1.JobStateRunning,
					Message:   "Job running",
				},
			}

			mockProvider.getProvisionStatusFunc = func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
				return provisioning.ProvisionStatus{
					JobID:   jobID,
					State:   osacv1alpha1.JobStateSucceeded,
					Message: "Job succeeded",
				}, nil
			}

			_, err := reconciler.handleProvisioning(ctx, vnet)
			Expect(err).NotTo(HaveOccurred())
			Expect(vnet.Status.Phase).To(Equal(osacv1alpha1.VirtualNetworkPhaseReady))
		})
	})
})

// mockVirtualNetworkProvider implements the ProvisioningProvider interface for VirtualNetwork testing
type mockVirtualNetworkProvider struct {
	triggerProvisionFunc     func(ctx context.Context, resource client.Object) (*provisioning.ProvisionResult, error)
	getProvisionStatusFunc   func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error)
	triggerDeprovisionFunc   func(ctx context.Context, resource client.Object) (*provisioning.DeprovisionResult, error)
	getDeprovisionStatusFunc func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error)
}

func (m *mockVirtualNetworkProvider) TriggerProvision(ctx context.Context, resource client.Object) (*provisioning.ProvisionResult, error) {
	if m.triggerProvisionFunc != nil {
		return m.triggerProvisionFunc(ctx, resource)
	}
	return &provisioning.ProvisionResult{
		JobID:        "mock-job-id",
		InitialState: osacv1alpha1.JobStatePending,
		Message:      "Provisioning job triggered",
	}, nil
}

func (m *mockVirtualNetworkProvider) GetProvisionStatus(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
	if m.getProvisionStatusFunc != nil {
		return m.getProvisionStatusFunc(ctx, resource, jobID)
	}
	return provisioning.ProvisionStatus{
		JobID:   jobID,
		State:   osacv1alpha1.JobStateSucceeded,
		Message: "Job completed successfully",
	}, nil
}

func (m *mockVirtualNetworkProvider) TriggerDeprovision(ctx context.Context, resource client.Object) (*provisioning.DeprovisionResult, error) {
	if m.triggerDeprovisionFunc != nil {
		return m.triggerDeprovisionFunc(ctx, resource)
	}
	return &provisioning.DeprovisionResult{
		Action:                 provisioning.DeprovisionTriggered,
		JobID:                  "mock-deprovision-job-id",
		BlockDeletionOnFailure: true,
	}, nil
}

func (m *mockVirtualNetworkProvider) GetDeprovisionStatus(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
	if m.getDeprovisionStatusFunc != nil {
		return m.getDeprovisionStatusFunc(ctx, resource, jobID)
	}
	return provisioning.ProvisionStatus{
		JobID:   jobID,
		State:   osacv1alpha1.JobStateSucceeded,
		Message: "Deprovision completed successfully",
	}, nil
}

func (m *mockVirtualNetworkProvider) Name() string {
	return "mock-virtualnetwork-provider"
}
