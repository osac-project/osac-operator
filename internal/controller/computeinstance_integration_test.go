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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudkitv1alpha1 "github.com/innabox/cloudkit-operator/api/v1alpha1"
	"github.com/innabox/cloudkit-operator/internal/provisioning"
)

// controllableProvider is a mock provider that allows test control over job progression
type controllableProvider struct {
	mu sync.Mutex

	provisionJobID      string
	provisionJobState   provisioning.JobState
	provisionJobMsg     string
	provisionTriggerErr error

	deprovisionJobID      string
	deprovisionJobState   provisioning.JobState
	deprovisionJobMsg     string
	deprovisionTriggerErr error
}

func newControllableProvider() *controllableProvider {
	return &controllableProvider{
		provisionJobState:   provisioning.JobStatePending,
		deprovisionJobState: provisioning.JobStatePending,
	}
}

func (p *controllableProvider) TriggerProvision(ctx context.Context, resource client.Object) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.provisionTriggerErr != nil {
		return "", p.provisionTriggerErr
	}

	p.provisionJobID = "prov-job-" + resource.GetName()
	p.provisionJobState = provisioning.JobStatePending
	p.provisionJobMsg = "Job triggered"
	return p.provisionJobID, nil
}

func (p *controllableProvider) GetProvisionStatus(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return provisioning.ProvisionStatus{
		JobID:   jobID,
		State:   p.provisionJobState,
		Message: p.provisionJobMsg,
	}, nil
}

func (p *controllableProvider) CancelProvision(ctx context.Context, jobID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// In tests, immediately transition to canceled state
	p.provisionJobState = provisioning.JobStateCanceled
	p.provisionJobMsg = "Job canceled"
	return nil
}

func (p *controllableProvider) TriggerDeprovision(ctx context.Context, resource client.Object) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.deprovisionTriggerErr != nil {
		return "", p.deprovisionTriggerErr
	}

	p.deprovisionJobID = "deprov-job-" + resource.GetName()
	p.deprovisionJobState = provisioning.JobStatePending
	p.deprovisionJobMsg = "Deprovision job triggered"
	return p.deprovisionJobID, nil
}

func (p *controllableProvider) GetDeprovisionStatus(ctx context.Context, jobID string) (provisioning.ProvisionStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return provisioning.ProvisionStatus{
		JobID:   jobID,
		State:   p.deprovisionJobState,
		Message: p.deprovisionJobMsg,
	}, nil
}

func (p *controllableProvider) Name() string {
	return provisioning.ProviderTypeAAP
}

// setProvisionJobState updates the provision job state (thread-safe)
func (p *controllableProvider) setProvisionJobState(state provisioning.JobState, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.provisionJobState = state
	p.provisionJobMsg = message
}

// setDeprovisionJobState updates the deprovision job state (thread-safe)
func (p *controllableProvider) setDeprovisionJobState(state provisioning.JobState, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deprovisionJobState = state
	p.deprovisionJobMsg = message
}

var _ = Describe("ComputeInstance Integration Tests", func() {
	const (
		testNamespace = "default"
		timeout       = 10 * time.Second
		interval      = 250 * time.Millisecond
	)

	var (
		reconciler *ComputeInstanceReconciler
		provider   *controllableProvider
	)

	BeforeEach(func() {
		provider = newControllableProvider()
		reconciler = &ComputeInstanceReconciler{
			Client:                   k8sClient,
			Scheme:                   k8sClient.Scheme(),
			ComputeInstanceNamespace: testNamespace,
			ProvisioningProvider:     provider,
			StatusPollInterval:       100 * time.Millisecond,
		}
	})

	Context("Provisioning workflow", func() {
		It("should provision a ComputeInstance successfully", func() {
			instanceName := "test-provision-success"
			instance := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      instanceName,
					Namespace: testNamespace,
					Annotations: map[string]string{
						cloudkitTenantAnnotation: "test-tenant",
					},
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "test_template",
				},
			}

			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, instance)

			// First call should trigger the job
			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(100 * time.Millisecond))

			// Update status to persist the job ID
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Verify job was triggered
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: testNamespace}, instance)).To(Succeed())
			Expect(instance.Status.ProvisionJobID).To(Equal("prov-job-" + instanceName))
			Expect(instance.Status.ProvisionJobState).To(Equal(string(provisioning.JobStatePending)))

			// Simulate job transitioning to running
			provider.setProvisionJobState(provisioning.JobStateRunning, "Job is running")

			result, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(100 * time.Millisecond))

			// Update status
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Verify status updated to running
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: testNamespace}, instance)).To(Succeed())
			Expect(instance.Status.ProvisionJobState).To(Equal(string(provisioning.JobStateRunning)))

			// Simulate job completing successfully
			provider.setProvisionJobState(provisioning.JobStateSucceeded, "Job completed")

			result, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			// Update status
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Verify final status
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: testNamespace}, instance)).To(Succeed())
			Expect(instance.Status.ProvisionJobState).To(Equal(string(provisioning.JobStateSucceeded)))
		})

		It("should handle provision job failure", func() {
			instanceName := "test-provision-failure"
			instance := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      instanceName,
					Namespace: testNamespace,
					Annotations: map[string]string{
						cloudkitTenantAnnotation: "test-tenant",
					},
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "test_template",
				},
			}

			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, instance)

			// Trigger the job
			_, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Simulate job failing
			provider.setProvisionJobState(provisioning.JobStateFailed, "Provisioning failed")

			_, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Verify status shows failure
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: testNamespace}, instance)).To(Succeed())
			Expect(instance.Status.ProvisionJobState).To(Equal(string(provisioning.JobStateFailed)))
			Expect(instance.Status.Phase).To(Equal(cloudkitv1alpha1.ComputeInstancePhaseFailed))
		})
	})

	Context("Deprovisioning workflow", func() {
		It("should deprovision a ComputeInstance successfully (AAP Direct)", func() {
			instanceName := "test-deprovision-success"
			instance := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      instanceName,
					Namespace: testNamespace,
					// AAP Direct uses base finalizer, not AAP-specific finalizer
					Finalizers: []string{cloudkitComputeInstanceFinalizer},
					Annotations: map[string]string{
						cloudkitTenantAnnotation: "test-tenant",
					},
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "test_template",
				},
			}

			Expect(k8sClient.Create(ctx, instance)).To(Succeed())

			// Trigger deprovisioning
			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(100 * time.Millisecond))

			// Verify deprovision job was triggered (in-memory state)
			Expect(instance.Status.DeprovisionJobID).To(Equal("deprov-job-" + instanceName))
			Expect(instance.Status.DeprovisionJobState).To(Equal(string(provisioning.JobStatePending)))

			// Simulate job completing
			provider.setDeprovisionJobState(provisioning.JobStateSucceeded, "Deprovision completed")

			result, err = reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			// For AAP Direct: handleDeprovisioning() doesn't remove finalizers
			// Finalizer removal is handled by handleDelete()
			// Verify job succeeded
			Expect(instance.Status.DeprovisionJobState).To(Equal(string(provisioning.JobStateSucceeded)))
		})

		It("should block deletion when deprovision job fails (AAP Direct)", func() {
			instanceName := "test-deprovision-failure"
			instance := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      instanceName,
					Namespace: testNamespace,
					Annotations: map[string]string{
						cloudkitTenantAnnotation: "test-tenant",
					},
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "test_template",
				},
			}

			Expect(k8sClient.Create(ctx, instance)).To(Succeed())

			// Trigger deprovisioning
			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			// Should requeue to poll job status
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Simulate job failing
			provider.setDeprovisionJobState(provisioning.JobStateFailed, "Resource not found")

			// Call again with failed job
			result, err = reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())

			// For AAP Direct: Should requeue (block deletion) when deprovision fails
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Verify deletion is blocked - status shows failed deprovision
			Expect(instance.Status.DeprovisionJobState).To(Equal(string(provisioning.JobStateFailed)))
		})
	})

	Context("Long-running job polling", func() {
		It("should poll for job status until completion", func() {
			instanceName := "test-long-running"
			instance := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      instanceName,
					Namespace: testNamespace,
					Annotations: map[string]string{
						cloudkitTenantAnnotation: "test-tenant",
					},
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "test_template",
				},
			}

			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, instance)

			// Trigger job
			_, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())

			// Verify job is pending (in-memory)
			Expect(instance.Status.ProvisionJobState).To(Equal(string(provisioning.JobStatePending)))

			// Set to running and poll multiple times
			provider.setProvisionJobState(provisioning.JobStateRunning, "Job running - step 1")
			for i := 0; i < 3; i++ {
				result, err := reconciler.handleProvisioning(ctx, instance)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(100 * time.Millisecond))

				Expect(instance.Status.ProvisionJobState).To(Equal(string(provisioning.JobStateRunning)))
			}

			// Finally complete
			provider.setProvisionJobState(provisioning.JobStateSucceeded, "Job completed")
			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			Expect(instance.Status.ProvisionJobState).To(Equal(string(provisioning.JobStateSucceeded)))
		})
	})
})
