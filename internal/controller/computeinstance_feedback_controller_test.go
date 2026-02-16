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
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudkitv1alpha1 "github.com/osac/osac-operator/api/v1alpha1"
	privatev1 "github.com/osac/osac-operator/internal/api/private/v1"
	sharedv1 "github.com/osac/osac-operator/internal/api/shared/v1"
)

// mockComputeInstancesClient is a mock implementation of ComputeInstancesClient for testing.
type mockComputeInstancesClient struct {
	getResponse    *privatev1.ComputeInstancesGetResponse
	getError       error
	updateResponse *privatev1.ComputeInstancesUpdateResponse
	updateError    error
	updateCalled   bool
	updateCount    int
	lastUpdate     *privatev1.ComputeInstance
}

func (m *mockComputeInstancesClient) List(ctx context.Context, in *privatev1.ComputeInstancesListRequest, opts ...grpc.CallOption) (*privatev1.ComputeInstancesListResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *mockComputeInstancesClient) Get(ctx context.Context, in *privatev1.ComputeInstancesGetRequest, opts ...grpc.CallOption) (*privatev1.ComputeInstancesGetResponse, error) {
	if m.getError != nil {
		return nil, m.getError
	}
	return m.getResponse, nil
}

func (m *mockComputeInstancesClient) Create(ctx context.Context, in *privatev1.ComputeInstancesCreateRequest, opts ...grpc.CallOption) (*privatev1.ComputeInstancesCreateResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *mockComputeInstancesClient) Delete(ctx context.Context, in *privatev1.ComputeInstancesDeleteRequest, opts ...grpc.CallOption) (*privatev1.ComputeInstancesDeleteResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *mockComputeInstancesClient) Update(ctx context.Context, in *privatev1.ComputeInstancesUpdateRequest, opts ...grpc.CallOption) (*privatev1.ComputeInstancesUpdateResponse, error) {
	m.updateCalled = true
	m.updateCount++
	m.lastUpdate = in.GetObject()
	if m.updateError != nil {
		return nil, m.updateError
	}
	return m.updateResponse, nil
}

func (m *mockComputeInstancesClient) Signal(ctx context.Context, in *privatev1.ComputeInstancesSignalRequest, opts ...grpc.CallOption) (*privatev1.ComputeInstancesSignalResponse, error) {
	return nil, errors.New("not implemented")
}

var _ = Describe("ComputeInstanceFeedbackReconciler", func() {
	const (
		resourceName      = "test-ci"
		vmNamespace       = "default"
		ciID              = "test-ci-id"
		computeInstanceNS = "cloudkit-computeinstance"
	)

	var (
		ctx                context.Context
		typeNamespacedName types.NamespacedName
		mockClient         *mockComputeInstancesClient
		reconciler         *ComputeInstanceFeedbackReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		typeNamespacedName = types.NamespacedName{
			Name:      resourceName,
			Namespace: computeInstanceNS,
		}
		mockClient = &mockComputeInstancesClient{}
		reconciler = &ComputeInstanceFeedbackReconciler{
			hubClient:                k8sClient,
			computeInstancesClient:   mockClient,
			computeInstanceNamespace: computeInstanceNS,
		}

		// Create the namespace if it doesn't exist
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: computeInstanceNS,
			},
		}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: computeInstanceNS}, namespace)
		if err != nil && apierrors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		}
	})

	Context("When reconciling a resource that doesn't exist", func() {
		It("should return without error", func() {
			request := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "non-existent",
					Namespace: computeInstanceNS,
				},
			}
			result, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockClient.updateCalled).To(BeFalse())
		})
	})

	Context("When reconciling a resource without the VM ID label", func() {
		BeforeEach(func() {
			vm := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: computeInstanceNS,
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "test_template",
				},
			}
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		})

		AfterEach(func() {
			vm := &cloudkitv1alpha1.ComputeInstance{}
			err := k8sClient.Get(ctx, typeNamespacedName, vm)
			if err == nil {
				Expect(k8sClient.Delete(ctx, vm)).To(Succeed())
			}
		})

		It("should skip reconciliation", func() {
			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}
			result, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockClient.updateCalled).To(BeFalse())
		})
	})

	Context("When reconciling a resource that is being deleted", func() {
		BeforeEach(func() {
			vm := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: computeInstanceNS,
					Labels: map[string]string{
						cloudkitComputeInstanceIDLabel: ciID,
					},
					Finalizers: []string{cloudkitComputeInstanceFinalizer},
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "test_template",
				},
			}
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			Expect(k8sClient.Delete(ctx, vm)).To(Succeed())
			// Set a valid getResponse in case the reconciler tries to fetch (shouldn't happen but prevents panic)
			mockClient.getResponse = &privatev1.ComputeInstancesGetResponse{
				Object: &privatev1.ComputeInstance{
					Id:   ciID,
					Spec: &privatev1.ComputeInstanceSpec{},
					Status: &privatev1.ComputeInstanceStatus{
						State: privatev1.ComputeInstanceState_COMPUTE_INSTANCE_STATE_UNSPECIFIED,
					},
				},
			}
		})

		AfterEach(func() {
			vm := &cloudkitv1alpha1.ComputeInstance{}
			err := k8sClient.Get(ctx, typeNamespacedName, vm)
			if err == nil {
				// Force delete by removing finalizers
				vm.Finalizers = nil
				Expect(k8sClient.Update(ctx, vm)).To(Succeed())
			}
		})

		It("should skip feedback reconciliation", func() {
			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}
			result, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockClient.updateCalled).To(BeFalse()) // Should not update when deleting
		})
	})

	Context("When reconciling a valid resource", func() {
		BeforeEach(func() {
			// Reset mock client state
			mockClient.updateCalled = false
			mockClient.lastUpdate = nil

			vm := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: computeInstanceNS,
					Labels: map[string]string{
						cloudkitComputeInstanceIDLabel: ciID,
					},
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "test_template",
				},
			}
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			// Update status separately since Status is a subresource - need to get fresh copy
			err := k8sClient.Get(ctx, typeNamespacedName, vm)
			Expect(err).NotTo(HaveOccurred())
			vm.Status.Phase = cloudkitv1alpha1.ComputeInstancePhaseRunning
			vm.Status.Conditions = []metav1.Condition{
				{
					Type:               string(cloudkitv1alpha1.ComputeInstanceConditionAccepted),
					Status:             metav1.ConditionFalse,
					Reason:             "Accepted",
					Message:            "VM is accepted",
					LastTransitionTime: metav1.NewTime(time.Now().UTC()),
				},
				{
					Type:               string(cloudkitv1alpha1.ComputeInstanceConditionProgressing),
					Status:             metav1.ConditionTrue,
					Reason:             "Progressing",
					Message:            "VM is progressing",
					LastTransitionTime: metav1.NewTime(time.Now().UTC()),
				},
			}
			Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())

			// Setup mock response
			mockClient.getResponse = &privatev1.ComputeInstancesGetResponse{
				Object: &privatev1.ComputeInstance{
					Id:   ciID,
					Spec: &privatev1.ComputeInstanceSpec{},
					Status: &privatev1.ComputeInstanceStatus{
						State: privatev1.ComputeInstanceState_COMPUTE_INSTANCE_STATE_UNSPECIFIED,
					},
				},
			}
			mockClient.updateResponse = &privatev1.ComputeInstancesUpdateResponse{}
		})

		AfterEach(func() {
			vm := &cloudkitv1alpha1.ComputeInstance{}
			err := k8sClient.Get(ctx, typeNamespacedName, vm)
			if err == nil {
				Expect(k8sClient.Delete(ctx, vm)).To(Succeed())
			}
		})

		It("should successfully sync conditions and phase", func() {
			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}
			result, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockClient.updateCalled).To(BeTrue())
			Expect(mockClient.lastUpdate).NotTo(BeNil())
			Expect(mockClient.lastUpdate.GetStatus().GetState()).To(Equal(privatev1.ComputeInstanceState_COMPUTE_INSTANCE_STATE_RUNNING))
		})

		It("should sync Progressing condition to Progressing condition", func() {
			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}
			_, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockClient.updateCalled).To(BeTrue())

			// Check that the condition was synced
			vm := mockClient.lastUpdate
			found := false
			for _, cond := range vm.GetStatus().GetConditions() {
				if cond.GetType() == privatev1.ComputeInstanceConditionType_COMPUTE_INSTANCE_CONDITION_TYPE_PROGRESSING {
					Expect(cond.GetStatus()).To(Equal(sharedv1.ConditionStatus_CONDITION_STATUS_TRUE))
					Expect(cond.GetMessage()).To(Equal("VM is progressing"))
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())
		})

		It("should sync Starting phase", func() {
			vm := &cloudkitv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, vm)).To(Succeed())
			vm.Status.Phase = cloudkitv1alpha1.ComputeInstancePhaseStarting
			Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())

			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}
			_, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockClient.updateCalled).To(BeTrue())
			Expect(mockClient.lastUpdate.GetStatus().GetState()).To(Equal(privatev1.ComputeInstanceState_COMPUTE_INSTANCE_STATE_STARTING))
		})

		It("should sync Failed phase", func() {
			vm := &cloudkitv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, vm)).To(Succeed())
			vm.Status.Phase = cloudkitv1alpha1.ComputeInstancePhaseFailed
			Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())

			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}
			_, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockClient.updateCalled).To(BeTrue())
			Expect(mockClient.lastUpdate.GetStatus().GetState()).To(Equal(privatev1.ComputeInstanceState_COMPUTE_INSTANCE_STATE_FAILED))
		})

		It("should update only once when reconciliation is run twice with same data", func() {
			// Reset update count
			mockClient.updateCount = 0
			mockClient.updateCalled = false

			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}

			// First reconciliation - should trigger an update
			result, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockClient.updateCount).To(Equal(1))
			Expect(mockClient.updateCalled).To(BeTrue())

			// Second reconciliation with same data - should NOT trigger another update
			// because the VM object in the fulfillment service now matches what we're trying to sync
			// We need to update the mock's getResponse to reflect the state after the first update
			mockClient.getResponse = &privatev1.ComputeInstancesGetResponse{
				Object: mockClient.lastUpdate,
			}

			// Run reconciliation again
			result, err = reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			// Update count should still be 1, not 2, because only the timestamp changed, not the status
			Expect(mockClient.updateCount).To(Equal(1))
		})

		It("should sync lastRestartedAt when set in K8s CR", func() {
			// Get the ComputeInstance and update its status with lastRestartedAt
			computeInstance := &cloudkitv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, computeInstance)).To(Succeed())

			// Use time without nanoseconds to match protobuf precision
			restartTime := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
			computeInstance.Status.LastRestartedAt = &restartTime
			Expect(k8sClient.Status().Update(ctx, computeInstance)).To(Succeed())

			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}
			_, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockClient.updateCalled).To(BeTrue())
			Expect(mockClient.lastUpdate).NotTo(BeNil())

			// Verify lastRestartedAt was synced
			Expect(mockClient.lastUpdate.GetStatus().HasLastRestartedAt()).To(BeTrue())
			Expect(mockClient.lastUpdate.GetStatus().GetLastRestartedAt().AsTime()).To(Equal(restartTime.Time))
		})

		It("should not set lastRestartedAt when nil in K8s CR", func() {
			// Get the ComputeInstance - lastRestartedAt should be nil by default
			computeInstance := &cloudkitv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, computeInstance)).To(Succeed())
			Expect(computeInstance.Status.LastRestartedAt).To(BeNil())

			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}
			_, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockClient.updateCalled).To(BeTrue())
			Expect(mockClient.lastUpdate).NotTo(BeNil())

			// Verify lastRestartedAt was NOT set (still using default from mock)
			Expect(mockClient.lastUpdate.GetStatus().HasLastRestartedAt()).To(BeFalse())
		})

		It("should sync RestartInProgress condition when set to True", func() {
			// Get the ComputeInstance and add RestartInProgress condition
			computeInstance := &cloudkitv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, computeInstance)).To(Succeed())

			restartInProgressMessage := "Restart initiated at 2026-02-01T10:20:58Z"
			computeInstance.Status.Conditions = append(computeInstance.Status.Conditions, metav1.Condition{
				Type:               string(cloudkitv1alpha1.ComputeInstanceConditionRestartInProgress),
				Status:             metav1.ConditionTrue,
				Reason:             ReasonRestartInProgress,
				Message:            restartInProgressMessage,
				LastTransitionTime: metav1.NewTime(time.Now().UTC()),
			})
			Expect(k8sClient.Status().Update(ctx, computeInstance)).To(Succeed())

			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}
			_, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockClient.updateCalled).To(BeTrue())

			// Verify RestartInProgress condition was synced
			found := false
			for _, cond := range mockClient.lastUpdate.GetStatus().GetConditions() {
				if cond.GetType() == privatev1.ComputeInstanceConditionType_COMPUTE_INSTANCE_CONDITION_TYPE_RESTART_IN_PROGRESS {
					Expect(cond.GetStatus()).To(Equal(sharedv1.ConditionStatus_CONDITION_STATUS_TRUE))
					Expect(cond.GetMessage()).To(Equal(restartInProgressMessage))
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())
		})

		It("should sync RestartFailed condition when set to True", func() {
			// Get the ComputeInstance and add RestartFailed condition
			computeInstance := &cloudkitv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, computeInstance)).To(Succeed())

			restartFailedMessage := "No VirtualMachine reference found"
			computeInstance.Status.Conditions = append(computeInstance.Status.Conditions, metav1.Condition{
				Type:               string(cloudkitv1alpha1.ComputeInstanceConditionRestartFailed),
				Status:             metav1.ConditionTrue,
				Reason:             ReasonNoVMReference,
				Message:            restartFailedMessage,
				LastTransitionTime: metav1.NewTime(time.Now().UTC()),
			})
			Expect(k8sClient.Status().Update(ctx, computeInstance)).To(Succeed())

			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}
			_, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockClient.updateCalled).To(BeTrue())

			// Verify RestartFailed condition was synced
			found := false
			for _, cond := range mockClient.lastUpdate.GetStatus().GetConditions() {
				if cond.GetType() == privatev1.ComputeInstanceConditionType_COMPUTE_INSTANCE_CONDITION_TYPE_RESTART_FAILED {
					Expect(cond.GetStatus()).To(Equal(sharedv1.ConditionStatus_CONDITION_STATUS_TRUE))
					Expect(cond.GetMessage()).To(Equal(restartFailedMessage))
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())
		})

		It("should not crash when restart conditions are not present", func() {
			// RestartInProgress and RestartFailed conditions should not be present by default
			computeInstance := &cloudkitv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, computeInstance)).To(Succeed())

			// Verify conditions don't include restart conditions
			for _, cond := range computeInstance.Status.Conditions {
				Expect(cond.Type).NotTo(Equal(string(cloudkitv1alpha1.ComputeInstanceConditionRestartInProgress)))
				Expect(cond.Type).NotTo(Equal(string(cloudkitv1alpha1.ComputeInstanceConditionRestartFailed)))
			}

			request := reconcile.Request{
				NamespacedName: typeNamespacedName,
			}
			_, err := reconciler.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockClient.updateCalled).To(BeTrue())
		})
	})
})
