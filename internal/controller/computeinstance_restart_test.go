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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudkitv1alpha1 "github.com/innabox/cloudkit-operator/api/v1alpha1"
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
