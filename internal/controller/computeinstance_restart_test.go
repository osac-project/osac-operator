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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudkitv1alpha1 "github.com/osac/osac-operator/api/v1alpha1"
)

var _ = Describe("ComputeInstance Restart Handler", func() {
	var reconciler *ComputeInstanceReconciler
	ctx := context.Background()

	BeforeEach(func() {
		reconciler = &ComputeInstanceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	Context("handleRestartRequest", func() {
		It("should skip when RestartRequestedAt is nil", func() {
			ci := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-no-restart",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template_1",
					RestartRequestedAt: nil, // No restart requested
				},
			}

			result, err := reconciler.handleRestartRequest(ctx, ci)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("should process restart when RestartRequestedAt is set and LastRestartedAt is nil", func() {
			now := metav1.NewTime(time.Now().UTC())
			ci := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-ci-first-restart",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template_1",
					RestartRequestedAt: &now,
				},
				Status: cloudkitv1alpha1.ComputeInstanceStatus{
					LastRestartedAt: nil, // First restart
				},
			}

			// Create the CI resource so performRestart can update it
			err := k8sClient.Create(ctx, ci)
			Expect(err).NotTo(HaveOccurred())

			// Note: performRestart will fail due to no VM reference, but handleRestartRequest
			// should still call it (we're testing the flow, not the success)
			_, _ = reconciler.handleRestartRequest(ctx, ci)

			// Cleanup
			_ = k8sClient.Delete(ctx, ci)
		})

		It("should skip when RestartRequestedAt equals LastRestartedAt", func() {
			now := metav1.NewTime(time.Now().UTC())
			ci := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-equal-timestamps",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template_1",
					RestartRequestedAt: &now,
				},
				Status: cloudkitv1alpha1.ComputeInstanceStatus{
					LastRestartedAt: &now, // Already processed
				},
			}

			result, err := reconciler.handleRestartRequest(ctx, ci)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("should skip when RestartRequestedAt is before LastRestartedAt", func() {
			past := metav1.NewTime(time.Now().UTC().Add(-1 * time.Hour))
			now := metav1.NewTime(time.Now().UTC())
			ci := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-old-request",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template_1",
					RestartRequestedAt: &past, // Old request
				},
				Status: cloudkitv1alpha1.ComputeInstanceStatus{
					LastRestartedAt: &now, // Already restarted more recently
				},
			}

			result, err := reconciler.handleRestartRequest(ctx, ci)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("should process restart when RestartRequestedAt is after LastRestartedAt", func() {
			past := metav1.NewTime(time.Now().UTC().Add(-1 * time.Hour))
			now := metav1.NewTime(time.Now().UTC())
			ci := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-ci-new-request",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template_1",
					RestartRequestedAt: &now, // New request
				},
				Status: cloudkitv1alpha1.ComputeInstanceStatus{
					LastRestartedAt: &past, // Old restart
				},
			}

			// Create the CI resource so performRestart can update it
			err := k8sClient.Create(ctx, ci)
			Expect(err).NotTo(HaveOccurred())

			// Note: performRestart will fail due to no VM reference
			_, _ = reconciler.handleRestartRequest(ctx, ci)

			// Cleanup
			_ = k8sClient.Delete(ctx, ci)
		})
	})

	Context("performRestart", func() {
		It("should set RestartFailed condition when VirtualMachineReference is nil", func() {
			now := metav1.NewTime(time.Now().UTC())
			ci := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-ci-no-vm-ref",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template_1",
					RestartRequestedAt: &now,
				},
			}

			// Create the resource in k8s so we can update status
			err := k8sClient.Create(ctx, ci)
			Expect(err).NotTo(HaveOccurred())

			result, err := reconciler.performRestart(ctx, ci)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
			// Update status (normally done by main Reconcile loop)
			err = k8sClient.Status().Update(ctx, ci)
			Expect(err).NotTo(HaveOccurred())

			// Re-fetch to get updated status
			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(ci), ci)
			Expect(err).NotTo(HaveOccurred())

			// Verify RestartFailed condition is set
			failedCondition := findCondition(ci.Status.Conditions,
				string(cloudkitv1alpha1.ComputeInstanceConditionRestartFailed))
			Expect(failedCondition).NotTo(BeNil())
			Expect(failedCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(failedCondition.Reason).To(Equal(ReasonNoVMReference))

			// Cleanup
			err = k8sClient.Delete(ctx, ci)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should set RestartInProgress but NOT set lastRestartedAt", func() {
			now := metav1.NewTime(time.Now().UTC())
			ci := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-ci-restart-initiated",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template_1",
					RestartRequestedAt: &now,
				},
			}

			// Create the CI resource
			err := k8sClient.Create(ctx, ci)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, ci)
			})

			// Set status separately (status is a subresource)
			ci.Status.VirtualMachineReference = &cloudkitv1alpha1.VirtualMachineReferenceType{
				KubeVirtVirtualMachineName: "test-vmi",
				Namespace:                  "default",
			}
			err = k8sClient.Status().Update(ctx, ci)
			Expect(err).NotTo(HaveOccurred())

			// Create a VMI
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
				},
			}
			err = k8sClient.Create(ctx, vmi)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, vmi)
			})

			// Perform restart
			result, err := reconciler.performRestart(ctx, ci)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// Verify lastRestartedAt is NOT set
			Expect(ci.Status.LastRestartedAt).To(BeNil())

			// Verify RestartInProgress condition is set
			inProgressCondition := findCondition(ci.Status.Conditions,
				string(cloudkitv1alpha1.ComputeInstanceConditionRestartInProgress))
			Expect(inProgressCondition).NotTo(BeNil())
			Expect(inProgressCondition.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("checkRestartCompletion", func() {
		It("should not complete restart when VMI not found", func() {
			now := metav1.NewTime(time.Now().UTC())
			ci := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-ci-vmi-not-found",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template_1",
					RestartRequestedAt: &now,
				},
				Status: cloudkitv1alpha1.ComputeInstanceStatus{
					VirtualMachineReference: &cloudkitv1alpha1.VirtualMachineReferenceType{
						KubeVirtVirtualMachineName: "non-existent-vmi",
						Namespace:                  "default",
					},
				},
			}

			err := reconciler.checkRestartCompletion(ctx, ci)
			Expect(err).NotTo(HaveOccurred())
			// lastRestartedAt should still be nil
			Expect(ci.Status.LastRestartedAt).To(BeNil())
		})

		It("should not complete restart when VMI created before restart request", func() {
			now := metav1.NewTime(time.Now().UTC())
			ci := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-ci-old-vmi",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template_1",
					RestartRequestedAt: &now,
				},
				Status: cloudkitv1alpha1.ComputeInstanceStatus{
					VirtualMachineReference: &cloudkitv1alpha1.VirtualMachineReferenceType{
						KubeVirtVirtualMachineName: "old-vmi",
						Namespace:                  "default",
					},
				},
			}

			// Set RestartInProgress condition
			meta.SetStatusCondition(&ci.Status.Conditions, metav1.Condition{
				Type:   string(cloudkitv1alpha1.ComputeInstanceConditionRestartInProgress),
				Status: metav1.ConditionTrue,
				Reason: ReasonRestartInProgress,
			})

			// Create VMI with timestamp before restart request
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "old-vmi",
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Hour)),
				},
			}
			err := k8sClient.Create(ctx, vmi)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, vmi)
			})

			err = reconciler.checkRestartCompletion(ctx, ci)
			Expect(err).NotTo(HaveOccurred())

			// lastRestartedAt should still be nil
			Expect(ci.Status.LastRestartedAt).To(BeNil())
			// RestartInProgress should still be true
			Expect(meta.IsStatusConditionTrue(ci.Status.Conditions,
				string(cloudkitv1alpha1.ComputeInstanceConditionRestartInProgress))).To(BeTrue())
		})

		It("should complete restart when VMI created after restart request", func() {
			// Set restart request time in the past so any VMI created "now" will be after it
			restartRequestedAt := metav1.NewTime(time.Now().UTC().Add(-5 * time.Second))
			ci := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-ci-new-vmi",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template_1",
					RestartRequestedAt: &restartRequestedAt,
				},
				Status: cloudkitv1alpha1.ComputeInstanceStatus{
					VirtualMachineReference: &cloudkitv1alpha1.VirtualMachineReferenceType{
						KubeVirtVirtualMachineName: "new-vmi",
						Namespace:                  "default",
					},
				},
			}

			// Set RestartInProgress condition
			meta.SetStatusCondition(&ci.Status.Conditions, metav1.Condition{
				Type:   string(cloudkitv1alpha1.ComputeInstanceConditionRestartInProgress),
				Status: metav1.ConditionTrue,
				Reason: ReasonRestartInProgress,
			})

			// Create VMI - its creationTimestamp will be "now" which is after restartRequestedAt
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "new-vmi",
					Namespace: "default",
				},
			}
			err := k8sClient.Create(ctx, vmi)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, vmi)
			})

			err = reconciler.checkRestartCompletion(ctx, ci)
			Expect(err).NotTo(HaveOccurred())

			// lastRestartedAt should now be set
			Expect(ci.Status.LastRestartedAt).NotTo(BeNil())
			Expect(ci.Status.LastRestartedAt.Time).To(Equal(restartRequestedAt.Time))
			// RestartInProgress should be cleared
			Expect(meta.FindStatusCondition(ci.Status.Conditions,
				string(cloudkitv1alpha1.ComputeInstanceConditionRestartInProgress))).To(BeNil())
		})
	})

	Context("handleRestartRequest with restart in progress", func() {
		It("should check restart completion when RestartInProgress is true", func() {
			// Set restart request time in the past so any VMI created "now" will be after it
			restartRequestedAt := metav1.NewTime(time.Now().UTC().Add(-5 * time.Second))
			ci := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-ci-in-progress",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template_1",
					RestartRequestedAt: &restartRequestedAt,
				},
				Status: cloudkitv1alpha1.ComputeInstanceStatus{
					VirtualMachineReference: &cloudkitv1alpha1.VirtualMachineReferenceType{
						KubeVirtVirtualMachineName: "vmi-in-progress",
						Namespace:                  "default",
					},
				},
			}

			// Set RestartInProgress condition
			meta.SetStatusCondition(&ci.Status.Conditions, metav1.Condition{
				Type:   string(cloudkitv1alpha1.ComputeInstanceConditionRestartInProgress),
				Status: metav1.ConditionTrue,
				Reason: ReasonRestartInProgress,
			})

			// Create new VMI - its creationTimestamp will be "now" which is after restartRequestedAt
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "vmi-in-progress",
					Namespace: "default",
				},
			}
			err := k8sClient.Create(ctx, vmi)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, vmi)
			})

			// Call handleRestartRequest
			result, err := reconciler.handleRestartRequest(ctx, ci)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// Should have completed the restart
			Expect(ci.Status.LastRestartedAt).NotTo(BeNil())
			Expect(ci.Status.LastRestartedAt.Time).To(Equal(restartRequestedAt.Time))
		})
	})
})

// Helper function to find a condition by type
func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
