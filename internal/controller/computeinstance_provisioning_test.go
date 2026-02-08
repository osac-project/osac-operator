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

	cloudkitv1alpha1 "github.com/innabox/cloudkit-operator/api/v1alpha1"
	"github.com/innabox/cloudkit-operator/internal/provisioning"
)

// mockProvisioningProvider implements the ProvisioningProvider interface for testing
type mockProvisioningProvider struct {
	name                     string
	triggerProvisionFunc     func(ctx context.Context, resource client.Object) (string, error)
	getProvisionStatusFunc   func(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error)
	triggerDeprovisionFunc   func(ctx context.Context, resource client.Object) (string, error)
	getDeprovisionStatusFunc func(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error)
}

func (m *mockProvisioningProvider) TriggerProvision(ctx context.Context, resource client.Object) (string, error) {
	if m.triggerProvisionFunc != nil {
		return m.triggerProvisionFunc(ctx, resource)
	}
	return "mock-job-id", nil
}

func (m *mockProvisioningProvider) GetProvisionStatus(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error) {
	if m.getProvisionStatusFunc != nil {
		return m.getProvisionStatusFunc(ctx, jobID)
	}
	return provisioning.ProvisionStatus{
		JobID:   jobID,
		State:   provisioning.JobStateSucceeded,
		Message: "Job completed successfully",
	}, nil
}

func (m *mockProvisioningProvider) CancelProvision(ctx context.Context, jobID string) error {
	// Mock implementation - always succeeds
	return nil
}

func (m *mockProvisioningProvider) TriggerDeprovision(ctx context.Context, resource client.Object) (string, error) {
	if m.triggerDeprovisionFunc != nil {
		return m.triggerDeprovisionFunc(ctx, resource)
	}
	return "mock-deprovision-job-id", nil
}

func (m *mockProvisioningProvider) GetDeprovisionStatus(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error) {
	if m.getDeprovisionStatusFunc != nil {
		return m.getDeprovisionStatusFunc(ctx, jobID)
	}
	return provisioning.ProvisionStatus{
		JobID:   jobID,
		State:   provisioning.JobStateSucceeded,
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
		}
	})

	Context("handleProvisioning", func() {
		It("should trigger provision when no job ID exists", func() {
			provider := &mockProvisioningProvider{
				triggerProvisionFunc: func(ctx context.Context, resource client.Object) (string, error) {
					return "new-job-123", nil
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(instance.Status.ProvisionJob).NotTo(BeNil())
			Expect(instance.Status.ProvisionJob.ID).To(Equal("new-job-123"))
			Expect(instance.Status.ProvisionJob.State).To(Equal(string(provisioning.JobStatePending)))
			Expect(instance.Status.ProvisionJob.Message).To(Equal("Provisioning job triggered"))
		})

		It("should handle rate limit error on trigger", func() {
			provider := &mockProvisioningProvider{
				triggerProvisionFunc: func(ctx context.Context, resource client.Object) (string, error) {
					return "", &provisioning.RateLimitError{RetryAfter: 5 * time.Second}
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Second))
			if instance.Status.ProvisionJob != nil {
				Expect(instance.Status.ProvisionJob.ID).To(BeEmpty())
			}
		})

		It("should handle trigger error", func() {
			provider := &mockProvisioningProvider{
				triggerProvisionFunc: func(ctx context.Context, resource client.Object) (string, error) {
					return "", errors.New("trigger failed")
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(instance.Status.ProvisionJob).NotTo(BeNil())
			Expect(instance.Status.ProvisionJob.State).To(Equal(string(provisioning.JobStateFailed)))
			Expect(instance.Status.ProvisionJob.Message).To(ContainSubstring("Failed to trigger provisioning"))
		})

		It("should poll for status when job ID exists and job is running", func() {
			instance.Status.ProvisionJob = &cloudkitv1alpha1.JobStatus{
				ID: "existing-job-456",
			}
			provider := &mockProvisioningProvider{
				getProvisionStatusFunc: func(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error) {
					Expect(jobID).To(Equal("existing-job-456"))
					return provisioning.ProvisionStatus{
						JobID:   jobID,
						State:   provisioning.JobStateRunning,
						Message: "Job is running",
					}, nil
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(instance.Status.ProvisionJob.State).To(Equal(string(provisioning.JobStateRunning)))
			Expect(instance.Status.ProvisionJob.Message).To(Equal("Job is running"))
		})

		It("should complete when job succeeds", func() {
			instance.Status.ProvisionJob = &cloudkitv1alpha1.JobStatus{
				ID: "successful-job-789",
			}
			provider := &mockProvisioningProvider{
				getProvisionStatusFunc: func(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error) {
					return provisioning.ProvisionStatus{
						JobID:             jobID,
						State:             provisioning.JobStateSucceeded,
						Message:           "Job completed",
						ReconciledVersion: "v1.2.3",
					}, nil
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(instance.Status.ProvisionJob.State).To(Equal(string(provisioning.JobStateSucceeded)))
			Expect(instance.Status.ReconciledConfigVersion).To(Equal("v1.2.3"))
		})

		It("should mark as failed when job fails", func() {
			instance.Status.ProvisionJob = &cloudkitv1alpha1.JobStatus{
				ID: "failed-job-999",
			}
			provider := &mockProvisioningProvider{
				getProvisionStatusFunc: func(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error) {
					return provisioning.ProvisionStatus{
						JobID:        jobID,
						State:        provisioning.JobStateFailed,
						Message:      "Job failed",
						ErrorDetails: "Playbook execution failed",
					}, nil
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(instance.Status.ProvisionJob.State).To(Equal(string(provisioning.JobStateFailed)))
			Expect(instance.Status.ProvisionJob.Message).To(ContainSubstring("Job failed"))
			Expect(instance.Status.ProvisionJob.Message).To(ContainSubstring("Playbook execution failed"))
			Expect(instance.Status.Phase).To(Equal(cloudkitv1alpha1.ComputeInstancePhaseFailed))
		})

		It("should handle status check error", func() {
			instance.Status.ProvisionJob = &cloudkitv1alpha1.JobStatus{
				ID: "error-job-111",
			}
			provider := &mockProvisioningProvider{
				getProvisionStatusFunc: func(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error) {
					return provisioning.ProvisionStatus{}, errors.New("API unavailable")
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(instance.Status.ProvisionJob.Message).To(ContainSubstring("Failed to get job status"))
		})

		It("should skip provisioning when provider is nil", func() {
			reconciler.ProvisioningProvider = nil

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			if instance.Status.ProvisionJob != nil {
				Expect(instance.Status.ProvisionJob.ID).To(BeEmpty())
			}
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
			if instance.Status.ProvisionJob != nil {
				Expect(instance.Status.ProvisionJob.ID).To(BeEmpty())
			}
		})
	})

	Context("handleDeprovisioning", func() {
		BeforeEach(func() {
			// Add finalizer for deletion tests
			instance.Finalizers = []string{cloudkitAAPComputeInstanceFinalizer}
		})

		It("should trigger deprovision when no job ID exists", func() {
			provider := &mockProvisioningProvider{
				triggerDeprovisionFunc: func(ctx context.Context, resource client.Object) (string, error) {
					return "deprovision-job-123", nil
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(instance.Status.DeprovisionJob).NotTo(BeNil())
			Expect(instance.Status.DeprovisionJob.ID).To(Equal("deprovision-job-123"))
			Expect(instance.Status.DeprovisionJob.State).To(Equal(string(provisioning.JobStatePending)))
			Expect(instance.Status.DeprovisionJob.Message).To(Equal("Deprovisioning job triggered"))
		})

		It("should handle rate limit error on deprovision trigger", func() {
			provider := &mockProvisioningProvider{
				triggerDeprovisionFunc: func(ctx context.Context, resource client.Object) (string, error) {
					return "", &provisioning.RateLimitError{RetryAfter: 10 * time.Second}
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(10 * time.Second))
			if instance.Status.DeprovisionJob != nil {
				Expect(instance.Status.DeprovisionJob.ID).To(BeEmpty())
			}
		})

		It("should handle deprovision trigger error", func() {
			provider := &mockProvisioningProvider{
				triggerDeprovisionFunc: func(ctx context.Context, resource client.Object) (string, error) {
					return "", errors.New("deprovision trigger failed")
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(instance.Status.DeprovisionJob).NotTo(BeNil())
			Expect(instance.Status.DeprovisionJob.State).To(Equal(string(provisioning.JobStateFailed)))
			Expect(instance.Status.DeprovisionJob.Message).To(ContainSubstring("Failed to trigger deprovisioning"))
		})

		It("should poll for status when deprovision job is running", func() {
			instance.Status.DeprovisionJob = &cloudkitv1alpha1.JobStatus{
				ID: "deprovision-running-456",
			}
			provider := &mockProvisioningProvider{
				getDeprovisionStatusFunc: func(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error) {
					Expect(jobID).To(Equal("deprovision-running-456"))
					return provisioning.ProvisionStatus{
						JobID:   jobID,
						State:   provisioning.JobStateRunning,
						Message: "Deprovisioning in progress",
					}, nil
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(instance.Status.DeprovisionJob.State).To(Equal(string(provisioning.JobStateRunning)))
			Expect(instance.Status.DeprovisionJob.Message).To(Equal("Deprovisioning in progress"))
			// Finalizer should still be present while job is running
			Expect(instance.Finalizers).To(ContainElement(cloudkitAAPComputeInstanceFinalizer))
		})

		It("should handle deprovision status check error", func() {
			instance.Status.DeprovisionJob = &cloudkitv1alpha1.JobStatus{
				ID: "deprovision-error-222",
			}
			provider := &mockProvisioningProvider{
				getDeprovisionStatusFunc: func(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error) {
					return provisioning.ProvisionStatus{}, errors.New("status check failed")
				},
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(instance.Status.DeprovisionJob.Message).To(ContainSubstring("Failed to get job status"))
		})

		It("should skip deprovisioning when ManagementStateManual annotation is set", func() {
			instance.Annotations = map[string]string{
				cloudkitComputeInstanceManagementStateAnnotation: ManagementStateManual,
			}
			provider := &mockProvisioningProvider{
				name: provisioning.ProviderTypeAAP,
			}
			reconciler.ProvisioningProvider = provider

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			// Should return immediately without triggering deprovision
			Expect(result.RequeueAfter).To(BeZero())
			if instance.Status.DeprovisionJob != nil {
				Expect(instance.Status.DeprovisionJob.ID).To(BeEmpty())
			}
		})
	})
})
