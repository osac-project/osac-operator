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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osacv1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

// mockClient wraps a real client and allows injecting errors
type mockClient struct {
	client.Client
	createFunc       func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error
	updateFunc       func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error
	deleteFunc       func(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error
	statusUpdateFunc func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error
}

func (m *mockClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if m.createFunc != nil {
		return m.createFunc(ctx, obj, opts...)
	}
	return m.Client.Create(ctx, obj, opts...)
}

func (m *mockClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if m.updateFunc != nil {
		return m.updateFunc(ctx, obj, opts...)
	}
	return m.Client.Update(ctx, obj, opts...)
}

func (m *mockClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, obj, opts...)
	}
	return m.Client.Delete(ctx, obj, opts...)
}

func (m *mockClient) Status() client.SubResourceWriter {
	return &mockStatusWriter{
		SubResourceWriter: m.Client.Status(),
		updateFunc:        m.statusUpdateFunc,
	}
}

type mockStatusWriter struct {
	client.SubResourceWriter
	updateFunc func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error
}

func (m *mockStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if m.updateFunc != nil {
		return m.updateFunc(ctx, obj, opts...)
	}
	return m.SubResourceWriter.Update(ctx, obj, opts...)
}

var _ = Describe("BareMetalPool Controller", func() {
	var (
		reconciler    *BareMetalPoolReconciler
		mockK8sClient *mockClient
		testPool      *osacv1alpha1.BareMetalPool
		testNamespace string
		testPoolName  string
	)

	// Common setup for ALL tests
	BeforeEach(func() {
		testNamespace = "default"
		mockK8sClient = &mockClient{Client: k8sClient}

		reconciler = &BareMetalPoolReconciler{
			Client: mockK8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	// Common cleanup for ALL tests
	AfterEach(func() {
		// Reset all mock functions
		mockK8sClient.createFunc = nil
		mockK8sClient.updateFunc = nil
		mockK8sClient.deleteFunc = nil
		mockK8sClient.statusUpdateFunc = nil

		if testPoolName != "" && testNamespace != "" {
			pool := &osacv1alpha1.BareMetalPool{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, pool)
			if err == nil {
				// Remove finalizer and delete
				pool.Finalizers = []string{}
				_ = k8sClient.Update(ctx, pool)
				_ = k8sClient.Delete(ctx, pool)
			}
		}
	})

	Context("When reconciling a completely new BareMetalPool without finalizer", func() {
		BeforeEach(func() {
			testPoolName = "test-pool-new"
			testPool = &osacv1alpha1.BareMetalPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
				Spec: osacv1alpha1.BareMetalPoolSpec{
					HostSets: []osacv1alpha1.BareMetalHostSet{
						{
							HostClass: "fc430",
							Replicas:  2,
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, testPool)).To(Succeed())
		})

		It("should add finalizer on first reconciliation", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			Expect(updatedPool.Finalizers).To(ContainElement(BareMetalPoolFinalizer))
		})

		It("should handle finalizer update error", func() {
			mockK8sClient.updateFunc = func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
				return errors.New("update failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("update failed"))
		})
	})

	Context("When reconciling a BareMetalPool with finalizer", func() {
		BeforeEach(func() {
			testPoolName = "test-pool-with-finalizer"
			testPool = &osacv1alpha1.BareMetalPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testPoolName,
					Namespace:  testNamespace,
					Finalizers: []string{BareMetalPoolFinalizer},
				},
				Spec: osacv1alpha1.BareMetalPoolSpec{
					HostSets: []osacv1alpha1.BareMetalHostSet{
						{
							HostClass: "fc430",
							Replicas:  3,
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, testPool)).To(Succeed())
		})

		It("should create Host CRs for the specified replicas", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			hostList := &osacv1alpha1.HostList{}
			err = k8sClient.List(ctx, hostList,
				client.InNamespace(testNamespace),
				client.MatchingLabels{"osac.openshift.io/pool-id": string(updatedPool.UID)},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(hostList.Items).To(HaveLen(3))
		})

		It("should set Ready condition to True", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			condition := updatedPool.GetStatusCondition(osacv1alpha1.BareMetalPoolConditionTypeReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(osacv1alpha1.BareMetalPoolReasonReady))
		})

		It("should verify Host CRs have correct labels and owner references", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			hostList := &osacv1alpha1.HostList{}
			err = k8sClient.List(ctx, hostList,
				client.InNamespace(testNamespace),
				client.MatchingLabels{"osac.openshift.io/pool-id": string(updatedPool.UID)},
			)
			Expect(err).NotTo(HaveOccurred())

			for _, host := range hostList.Items {
				Expect(host.Labels["osac.openshift.io/pool-id"]).To(Equal(string(updatedPool.UID)))
				Expect(host.Labels["osac.openshift.io/host-class"]).To(Equal("fc430"))
				Expect(host.Spec.HostClass).To(Equal("fc430"))
				Expect(host.OwnerReferences).To(HaveLen(1))
				Expect(host.OwnerReferences[0].Name).To(Equal(updatedPool.Name))
				Expect(host.OwnerReferences[0].Kind).To(Equal("BareMetalPool"))
			}
		})

		It("should update status.HostSets", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			Expect(updatedPool.Status.HostSets).To(HaveLen(1))
			Expect(updatedPool.Status.HostSets[0].HostClass).To(Equal("fc430"))
			Expect(updatedPool.Status.HostSets[0].Replicas).To(Equal(int32(3)))
		})

		It("should handle error when creating Host CR fails", func() {
			// Update pool to have 5 replicas instead of 3
			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())
			updatedPool.Spec.HostSets[0].Replicas = 5
			Expect(k8sClient.Update(ctx, updatedPool)).To(Succeed())

			// Mock create to succeed for first 2 hosts, then fail
			hostsCreated := 0
			mockK8sClient.createFunc = func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*osacv1alpha1.Host); ok {
					if hostsCreated >= 2 {
						return errors.New("create host failed")
					}
					hostsCreated++
					return mockK8sClient.Client.Create(ctx, obj, opts...)
				}
				return mockK8sClient.Client.Create(ctx, obj, opts...)
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("create host failed"))

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			// Verify only 2 hosts were created
			hostList := &osacv1alpha1.HostList{}
			err = k8sClient.List(ctx, hostList,
				client.InNamespace(testNamespace),
				client.MatchingLabels{"osac.openshift.io/pool-id": string(updatedPool.UID)},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(hostList.Items).To(HaveLen(2))

			// Verify status reflects the actual number of hosts created (2, not 5)
			Expect(updatedPool.Status.HostSets).To(HaveLen(1))
			Expect(updatedPool.Status.HostSets[0].HostClass).To(Equal("fc430"))
			Expect(updatedPool.Status.HostSets[0].Replicas).To(Equal(int32(2)))

			// Verify error condition is set
			condition := updatedPool.GetStatusCondition(osacv1alpha1.BareMetalPoolConditionTypeReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(osacv1alpha1.BareMetalPoolReasonFailed))
			Expect(condition.Message).To(Equal("Failed to create Host CR"))
		})
	})

	Context("When scaling down hosts", func() {
		BeforeEach(func() {
			testPoolName = "test-pool-scale-down"
			testPool = &osacv1alpha1.BareMetalPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testPoolName,
					Namespace:  testNamespace,
					Finalizers: []string{BareMetalPoolFinalizer},
				},
				Spec: osacv1alpha1.BareMetalPoolSpec{
					HostSets: []osacv1alpha1.BareMetalHostSet{
						{
							HostClass: "fc430",
							Replicas:  3,
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, testPool)).To(Succeed())

			// Initial reconcile to create hosts
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should delete Host CRs when replicas are reduced", func() {
			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			updatedPool.Spec.HostSets[0].Replicas = 1
			Expect(k8sClient.Update(ctx, updatedPool)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			hostList := &osacv1alpha1.HostList{}
			err = k8sClient.List(ctx, hostList,
				client.InNamespace(testNamespace),
				client.MatchingLabels{"osac.openshift.io/pool-id": string(updatedPool.UID)},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(hostList.Items).To(HaveLen(1))
		})

		It("should handle error when deleting Host CR during scale-down", func() {
			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			// Update to scale down from 3 to 1
			updatedPool.Spec.HostSets[0].Replicas = 1
			Expect(k8sClient.Update(ctx, updatedPool)).To(Succeed())

			// Mock delete to fail after first successful deletion
			hostsDeleted := 0
			mockK8sClient.deleteFunc = func(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*osacv1alpha1.Host); ok {
					if hostsDeleted >= 1 {
						return errors.New("delete host failed")
					}
					hostsDeleted++
					return mockK8sClient.Client.Delete(ctx, obj, opts...)
				}
				return mockK8sClient.Client.Delete(ctx, obj, opts...)
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("delete host failed"))

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			// Verify only 1 host was deleted (2 remain)
			hostList := &osacv1alpha1.HostList{}
			err = k8sClient.List(ctx, hostList,
				client.InNamespace(testNamespace),
				client.MatchingLabels{"osac.openshift.io/pool-id": string(updatedPool.UID)},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(hostList.Items).To(HaveLen(2))

			// Verify status reflects actual host count (2, not 1)
			Expect(updatedPool.Status.HostSets).To(HaveLen(1))
			Expect(updatedPool.Status.HostSets[0].Replicas).To(Equal(int32(2)))

			// Verify error condition is set
			condition := updatedPool.GetStatusCondition(osacv1alpha1.BareMetalPoolConditionTypeReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(osacv1alpha1.BareMetalPoolReasonFailed))
			Expect(condition.Message).To(Equal("Failed to delete Host CR"))
		})
	})

	Context("When managing multiple host classes", func() {
		BeforeEach(func() {
			testPoolName = "test-pool-multi-class"
			testPool = &osacv1alpha1.BareMetalPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testPoolName,
					Namespace:  testNamespace,
					Finalizers: []string{BareMetalPoolFinalizer},
				},
				Spec: osacv1alpha1.BareMetalPoolSpec{
					HostSets: []osacv1alpha1.BareMetalHostSet{
						{
							HostClass: "fc430",
							Replicas:  2,
						},
						{
							HostClass: "h100",
							Replicas:  3,
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, testPool)).To(Succeed())
		})

		It("should create hosts for all host classes", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			fc430Hosts := &osacv1alpha1.HostList{}
			err = k8sClient.List(ctx, fc430Hosts,
				client.InNamespace(testNamespace),
				client.MatchingLabels{
					"osac.openshift.io/pool-id":    string(updatedPool.UID),
					"osac.openshift.io/host-class": "fc430",
				},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(fc430Hosts.Items).To(HaveLen(2))

			h100Hosts := &osacv1alpha1.HostList{}
			err = k8sClient.List(ctx, h100Hosts,
				client.InNamespace(testNamespace),
				client.MatchingLabels{
					"osac.openshift.io/pool-id":    string(updatedPool.UID),
					"osac.openshift.io/host-class": "h100",
				},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(h100Hosts.Items).To(HaveLen(3))
		})

		It("should delete hosts when a host class is removed", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			updatedPool.Spec.HostSets = []osacv1alpha1.BareMetalHostSet{
				{
					HostClass: "fc430",
					Replicas:  2,
				},
			}
			Expect(k8sClient.Update(ctx, updatedPool)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			h100Hosts := &osacv1alpha1.HostList{}
			err = k8sClient.List(ctx, h100Hosts,
				client.InNamespace(testNamespace),
				client.MatchingLabels{
					"osac.openshift.io/pool-id":    string(updatedPool.UID),
					"osac.openshift.io/host-class": "h100",
				},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(h100Hosts.Items).To(BeEmpty())
		})

		It("should handle error when deleting hosts for removed host class", func() {
			// First reconcile to create hosts for both classes
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			// Remove h100 from spec
			updatedPool.Spec.HostSets = []osacv1alpha1.BareMetalHostSet{
				{
					HostClass: "fc430",
					Replicas:  2,
				},
			}
			Expect(k8sClient.Update(ctx, updatedPool)).To(Succeed())

			// Mock delete to fail when deleting h100 hosts (after first deletion)
			hostsDeleted := 0
			mockK8sClient.deleteFunc = func(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
				if host, ok := obj.(*osacv1alpha1.Host); ok {
					if host.Labels["osac.openshift.io/host-class"] == "h100" {
						if hostsDeleted >= 1 {
							return errors.New("delete host failed")
						}
						hostsDeleted++
					}
					return mockK8sClient.Client.Delete(ctx, obj, opts...)
				}
				return mockK8sClient.Client.Delete(ctx, obj, opts...)
			}

			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("delete host failed"))

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			// Verify fc430 hosts still exist
			fc430Hosts := &osacv1alpha1.HostList{}
			err = k8sClient.List(ctx, fc430Hosts,
				client.InNamespace(testNamespace),
				client.MatchingLabels{
					"osac.openshift.io/pool-id":    string(updatedPool.UID),
					"osac.openshift.io/host-class": "fc430",
				},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(fc430Hosts.Items).To(HaveLen(2))

			// Verify some h100 hosts were deleted but not all (2 remain)
			h100Hosts := &osacv1alpha1.HostList{}
			err = k8sClient.List(ctx, h100Hosts,
				client.InNamespace(testNamespace),
				client.MatchingLabels{
					"osac.openshift.io/pool-id":    string(updatedPool.UID),
					"osac.openshift.io/host-class": "h100",
				},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(h100Hosts.Items).To(HaveLen(2))

			// Verify status reflects both host classes still exist
			Expect(updatedPool.Status.HostSets).To(HaveLen(2))

			// Verify error condition is set
			condition := updatedPool.GetStatusCondition(osacv1alpha1.BareMetalPoolConditionTypeReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(osacv1alpha1.BareMetalPoolReasonFailed))
			Expect(condition.Message).To(Equal("Failed to delete Host CR"))
		})
	})

	Context("When BareMetalPool has a profile", func() {
		BeforeEach(func() {
			testPoolName = "test-pool-profile"
			testPool = &osacv1alpha1.BareMetalPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testPoolName,
					Namespace:  testNamespace,
					Finalizers: []string{BareMetalPoolFinalizer},
				},
				Spec: osacv1alpha1.BareMetalPoolSpec{
					HostSets: []osacv1alpha1.BareMetalHostSet{
						{
							HostClass: "fc430",
							Replicas:  1,
						},
					},
					Profile: &osacv1alpha1.ProfileSpec{
						Name:               "test-profile",
						TemplateParameters: `{"key":"value"}`,
					},
				},
			}

			Expect(k8sClient.Create(ctx, testPool)).To(Succeed())
		})

		It("should propagate profile template parameters to hosts", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedPool := &osacv1alpha1.BareMetalPool{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, updatedPool)).To(Succeed())

			hostList := &osacv1alpha1.HostList{}
			err = k8sClient.List(ctx, hostList,
				client.InNamespace(testNamespace),
				client.MatchingLabels{"osac.openshift.io/pool-id": string(updatedPool.UID)},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(hostList.Items).To(HaveLen(1))
			Expect(hostList.Items[0].Spec.TemplateParameters).To(Equal(`{"key":"value"}`))
		})
	})

	Context("When deleting a BareMetalPool", func() {
		BeforeEach(func() {
			testPoolName = "test-pool-delete"
		})

		It("should unassign hosts and remove finalizer", func() {
			testPool = &osacv1alpha1.BareMetalPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:       testPoolName,
					Namespace:  testNamespace,
					Finalizers: []string{BareMetalPoolFinalizer},
				},
				Spec: osacv1alpha1.BareMetalPoolSpec{
					HostSets: []osacv1alpha1.BareMetalHostSet{
						{
							HostClass: "fc430",
							Replicas:  1,
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, testPool)).To(Succeed())
			Expect(k8sClient.Delete(ctx, testPool)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				deletedPool := &osacv1alpha1.BareMetalPool{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      testPoolName,
					Namespace: testNamespace,
				}, deletedPool)
				return apierrors.IsNotFound(err)
			}, 5*time.Second).Should(BeTrue())
		})
	})

	Context("When resource does not exist", func() {
		It("should handle not found error gracefully", func() {
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "non-existent-pool",
					Namespace: "test-namespace",
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})
	})
})
