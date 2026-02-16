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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudkitv1alpha1 "github.com/osac/osac-operator/api/v1alpha1"
	"github.com/osac/osac-operator/internal/provisioning"
)

// mockProvisioningProvider implements the ProvisioningProvider interface for testing
type mockProvisioningProvider struct {
	name                     string
	triggerProvisionFunc     func(ctx context.Context, resource client.Object) (*provisioning.ProvisionResult, error)
	getProvisionStatusFunc   func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error)
	triggerDeprovisionFunc   func(ctx context.Context, resource client.Object) (*provisioning.DeprovisionResult, error)
	getDeprovisionStatusFunc func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error)
}

func (m *mockProvisioningProvider) TriggerProvision(ctx context.Context, resource client.Object) (*provisioning.ProvisionResult, error) {
	if m.triggerProvisionFunc != nil {
		return m.triggerProvisionFunc(ctx, resource)
	}
	return &provisioning.ProvisionResult{
		JobID:        "mock-job-id",
		InitialState: cloudkitv1alpha1.JobStatePending,
		Message:      "Provisioning job triggered",
	}, nil
}

func (m *mockProvisioningProvider) GetProvisionStatus(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
	if m.getProvisionStatusFunc != nil {
		return m.getProvisionStatusFunc(ctx, resource, jobID)
	}
	return provisioning.ProvisionStatus{
		JobID:   jobID,
		State:   cloudkitv1alpha1.JobStateSucceeded,
		Message: "Job completed successfully",
	}, nil
}

func (m *mockProvisioningProvider) TriggerDeprovision(ctx context.Context, resource client.Object) (*provisioning.DeprovisionResult, error) {
	if m.triggerDeprovisionFunc != nil {
		return m.triggerDeprovisionFunc(ctx, resource)
	}
	return &provisioning.DeprovisionResult{
		Action:                 provisioning.DeprovisionTriggered,
		JobID:                  "mock-deprovision-job-id",
		BlockDeletionOnFailure: true,
	}, nil
}

func (m *mockProvisioningProvider) GetDeprovisionStatus(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
	if m.getDeprovisionStatusFunc != nil {
		return m.getDeprovisionStatusFunc(ctx, resource, jobID)
	}
	return provisioning.ProvisionStatus{
		JobID:   jobID,
		State:   cloudkitv1alpha1.JobStateSucceeded,
		Message: "Deprovision completed successfully",
	}, nil
}

func (m *mockProvisioningProvider) Name() string {
	if m.name != "" {
		return m.name
	}
	return "mock"
}

var _ = Describe("ComputeInstance Provisioning", func() {
	var reconciler *ComputeInstanceReconciler
	var instance *cloudkitv1alpha1.ComputeInstance
	ctx := context.Background()

	BeforeEach(func() {
		instance = &cloudkitv1alpha1.ComputeInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-instance",
				Namespace: "default",
			},
			Spec: cloudkitv1alpha1.ComputeInstanceSpec{
				TemplateID: "test_template",
			},
		}
		reconciler = &ComputeInstanceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			StatusPollInterval: 30 * time.Second,
			MaxJobHistory:      DefaultMaxJobHistory,
		}
	})

	Context("handleProvisioning", func() {
		It("should trigger provision when no job ID exists", func() {
			provider := &mockProvisioningProvider{
				triggerProvisionFunc: func(ctx context.Context, resource client.Object) (*provisioning.ProvisionResult, error) {
					return &provisioning.ProvisionResult{
						JobID:        "new-job-123",
						InitialState: cloudkitv1alpha1.JobStatePending,
						Message:      "Provisioning job triggered",
					}, nil
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			latestProvisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeProvision)
			Expect(latestProvisionJob).NotTo(BeNil())
			Expect(latestProvisionJob.JobID).To(Equal("new-job-123"))
			Expect(latestProvisionJob.State).To(Equal(cloudkitv1alpha1.JobStatePending))
			Expect(latestProvisionJob.Message).To(Equal("Provisioning job triggered"))
		})

		It("should handle rate limit error on trigger", func() {
			provider := &mockProvisioningProvider{
				triggerProvisionFunc: func(ctx context.Context, resource client.Object) (*provisioning.ProvisionResult, error) {
					return nil, &provisioning.RateLimitError{RetryAfter: 5 * time.Second}
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Second))
			// Rate limit error should not create a job
			latestProvisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeProvision)
			Expect(latestProvisionJob).To(BeNil())
		})

		It("should handle trigger error", func() {
			provider := &mockProvisioningProvider{
				triggerProvisionFunc: func(ctx context.Context, resource client.Object) (*provisioning.ProvisionResult, error) {
					return nil, errors.New("trigger failed")
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			latestProvisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeProvision)
			Expect(latestProvisionJob).NotTo(BeNil())
			Expect(latestProvisionJob.State).To(Equal(cloudkitv1alpha1.JobStateFailed))
			Expect(latestProvisionJob.Message).To(ContainSubstring("Failed to trigger provisioning"))
		})

		It("should poll for status when job ID exists and job is running", func() {
			instance.Status.Jobs = []cloudkitv1alpha1.JobStatus{
				{
					JobID:     "existing-job-456",
					Type:      cloudkitv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     cloudkitv1alpha1.JobStatePending,
				},
			}
			provider := &mockProvisioningProvider{
				getProvisionStatusFunc: func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
					Expect(jobID).To(Equal("existing-job-456"))
					return provisioning.ProvisionStatus{
						JobID:   jobID,
						State:   cloudkitv1alpha1.JobStateRunning,
						Message: "Job is running",
					}, nil
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			latestProvisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeProvision)
			Expect(latestProvisionJob).NotTo(BeNil())
			Expect(latestProvisionJob.State).To(Equal(cloudkitv1alpha1.JobStateRunning))
			Expect(latestProvisionJob.Message).To(Equal("Job is running"))
		})

		It("should complete when job succeeds", func() {
			instance.Status.Jobs = []cloudkitv1alpha1.JobStatus{
				{
					JobID:     "successful-job-789",
					Type:      cloudkitv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     cloudkitv1alpha1.JobStatePending,
				},
			}
			provider := &mockProvisioningProvider{
				getProvisionStatusFunc: func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
					return provisioning.ProvisionStatus{
						JobID:             jobID,
						State:             cloudkitv1alpha1.JobStateSucceeded,
						Message:           "Job completed",
						ReconciledVersion: "v1.2.3",
					}, nil
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			latestProvisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeProvision)
			Expect(latestProvisionJob).NotTo(BeNil())
			Expect(latestProvisionJob.State).To(Equal(cloudkitv1alpha1.JobStateSucceeded))
			Expect(instance.Status.ReconciledConfigVersion).To(Equal("v1.2.3"))
		})

		It("should mark as failed when job fails", func() {
			instance.Status.Jobs = []cloudkitv1alpha1.JobStatus{
				{
					JobID:     "failed-job-999",
					Type:      cloudkitv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     cloudkitv1alpha1.JobStatePending,
				},
			}
			provider := &mockProvisioningProvider{
				getProvisionStatusFunc: func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
					return provisioning.ProvisionStatus{
						JobID:        jobID,
						State:        cloudkitv1alpha1.JobStateFailed,
						Message:      "Job failed",
						ErrorDetails: "Playbook execution failed",
					}, nil
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			latestProvisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeProvision)
			Expect(latestProvisionJob).NotTo(BeNil())
			Expect(latestProvisionJob.State).To(Equal(cloudkitv1alpha1.JobStateFailed))
			Expect(latestProvisionJob.Message).To(ContainSubstring("Job failed"))
			Expect(latestProvisionJob.Message).To(ContainSubstring("Playbook execution failed"))
			Expect(instance.Status.Phase).To(Equal(cloudkitv1alpha1.ComputeInstancePhaseFailed))
		})

		It("should handle status check error", func() {
			instance.Status.Jobs = []cloudkitv1alpha1.JobStatus{
				{
					JobID:     "error-job-111",
					Type:      cloudkitv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     cloudkitv1alpha1.JobStatePending,
				},
			}
			provider := &mockProvisioningProvider{
				getProvisionStatusFunc: func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
					return provisioning.ProvisionStatus{}, errors.New("API unavailable")
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			latestProvisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeProvision)
			Expect(latestProvisionJob).NotTo(BeNil())
			Expect(latestProvisionJob.Message).To(ContainSubstring("Failed to get job status"))
		})

		It("should skip provisioning when provider is nil", func() {
			reconciler.ProvisioningProvider = nil

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			latestProvisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeProvision)
			Expect(latestProvisionJob).To(BeNil())
		})

		It("should skip provisioning when ManagementStateManual annotation is set", func() {
			instance.Annotations = map[string]string{
				cloudkitComputeInstanceManagementStateAnnotation: ManagementStateManual,
			}
			provider := &mockProvisioningProvider{}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			latestProvisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeProvision)
			Expect(latestProvisionJob).To(BeNil())
		})
	})

	Context("handleDeprovisioning", func() {
		BeforeEach(func() {
			// Add finalizer for deletion tests
			instance.Finalizers = []string{cloudkitAAPComputeInstanceFinalizer}
		})

		It("should trigger deprovision when no job ID exists", func() {
			provider := &mockProvisioningProvider{
				triggerDeprovisionFunc: func(ctx context.Context, resource client.Object) (*provisioning.DeprovisionResult, error) {
					return &provisioning.DeprovisionResult{
						Action:                 provisioning.DeprovisionTriggered,
						JobID:                  "deprovision-job-123",
						BlockDeletionOnFailure: true,
					}, nil
				},
			}
			reconciler.ProvisioningProvider = provider

			_, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			latestDeprovisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeDeprovision)
			Expect(latestDeprovisionJob).NotTo(BeNil())
			Expect(latestDeprovisionJob.JobID).To(Equal("deprovision-job-123"))
			Expect(latestDeprovisionJob.State).To(Equal(cloudkitv1alpha1.JobStatePending))
			Expect(latestDeprovisionJob.Message).To(Equal("Deprovisioning job triggered"))
			Expect(latestDeprovisionJob.BlockDeletionOnFailure).To(BeTrue())
		})

		It("should handle rate limit error on deprovision trigger", func() {
			provider := &mockProvisioningProvider{
				triggerDeprovisionFunc: func(ctx context.Context, resource client.Object) (*provisioning.DeprovisionResult, error) {
					return nil, &provisioning.RateLimitError{RetryAfter: 10 * time.Second}
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(10 * time.Second))
			latestDeprovisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeDeprovision)
			Expect(latestDeprovisionJob).To(BeNil())
		})

		It("should handle deprovision trigger error", func() {
			provider := &mockProvisioningProvider{
				triggerDeprovisionFunc: func(ctx context.Context, resource client.Object) (*provisioning.DeprovisionResult, error) {
					return nil, errors.New("deprovision trigger failed")
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		})

		It("should poll for status when deprovision job is running", func() {
			instance.Status.Jobs = []cloudkitv1alpha1.JobStatus{
				{
					JobID:     "deprovision-running-456",
					Type:      cloudkitv1alpha1.JobTypeDeprovision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     cloudkitv1alpha1.JobStatePending,
				},
			}
			provider := &mockProvisioningProvider{
				getDeprovisionStatusFunc: func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
					Expect(jobID).To(Equal("deprovision-running-456"))
					return provisioning.ProvisionStatus{
						JobID:   jobID,
						State:   cloudkitv1alpha1.JobStateRunning,
						Message: "Deprovisioning in progress",
					}, nil
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			latestDeprovisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeDeprovision)
			Expect(latestDeprovisionJob).NotTo(BeNil())
			Expect(latestDeprovisionJob.State).To(Equal(cloudkitv1alpha1.JobStateRunning))
			Expect(latestDeprovisionJob.Message).To(Equal("Deprovisioning in progress"))
			// Finalizer should still be present while job is running
			Expect(instance.Finalizers).To(ContainElement(cloudkitAAPComputeInstanceFinalizer))
		})

		It("should handle deprovision status check error", func() {
			instance.Status.Jobs = []cloudkitv1alpha1.JobStatus{
				{
					JobID:     "deprovision-error-222",
					Type:      cloudkitv1alpha1.JobTypeDeprovision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     cloudkitv1alpha1.JobStatePending,
				},
			}
			provider := &mockProvisioningProvider{
				getDeprovisionStatusFunc: func(ctx context.Context, resource client.Object, jobID string) (provisioning.ProvisionStatus, error) {
					return provisioning.ProvisionStatus{}, errors.New("status check failed")
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			latestDeprovisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeDeprovision)
			Expect(latestDeprovisionJob).NotTo(BeNil())
			Expect(latestDeprovisionJob.Message).To(ContainSubstring("Failed to get job status"))
		})

		It("should skip deprovisioning when ManagementStateManual annotation is set", func() {
			instance.Annotations = map[string]string{
				cloudkitComputeInstanceManagementStateAnnotation: ManagementStateManual,
			}
			provider := &mockProvisioningProvider{
				name: string(provisioning.ProviderTypeAAP),
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			// Should return immediately without triggering deprovision
			Expect(result.RequeueAfter).To(BeZero())
			latestDeprovisionJob := cloudkitv1alpha1.FindLatestJobByType(instance.Status.Jobs, cloudkitv1alpha1.JobTypeDeprovision)
			Expect(latestDeprovisionJob).To(BeNil())
		})
	})
})
