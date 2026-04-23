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

package v1alpha1_test

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/osac-project/osac-operator/api/v1alpha1"
)

var _ = Describe("PublicIPSpec", func() {
	It("should accept a valid spec with all fields", func() {
		spec := v1alpha1.PublicIPSpec{
			Pool:            "my-pool",
			ComputeInstance: "my-instance",
		}

		Expect(spec.Pool).To(Equal("my-pool"))
		Expect(spec.ComputeInstance).To(Equal("my-instance"))
	})

	It("should accept a minimal spec without optional fields", func() {
		spec := v1alpha1.PublicIPSpec{
			Pool: "my-pool",
		}

		Expect(spec.Pool).To(Equal("my-pool"))
		Expect(spec.ComputeInstance).To(BeEmpty())
	})
})

var _ = Describe("PublicIPPhaseType", func() {
	DescribeTable("should have correct string values",
		func(phase v1alpha1.PublicIPPhaseType, expected string) {
			Expect(string(phase)).To(Equal(expected))
		},
		Entry("Progressing phase", v1alpha1.PublicIPPhaseProgressing, "Progressing"),
		Entry("Failed phase", v1alpha1.PublicIPPhaseFailed, "Failed"),
		Entry("Ready phase", v1alpha1.PublicIPPhaseReady, "Ready"),
		Entry("Deleting phase", v1alpha1.PublicIPPhaseDeleting, "Deleting"),
	)
})

var _ = Describe("PublicIPStateType", func() {
	DescribeTable("should have correct string values",
		func(state v1alpha1.PublicIPStateType, expected string) {
			Expect(string(state)).To(Equal(expected))
		},
		Entry("Pending state", v1alpha1.PublicIPStatePending, "Pending"),
		Entry("Allocated state", v1alpha1.PublicIPStateAllocated, "Allocated"),
		Entry("Attached state", v1alpha1.PublicIPStateAttached, "Attached"),
		Entry("Releasing state", v1alpha1.PublicIPStateReleasing, "Releasing"),
		Entry("Failed state", v1alpha1.PublicIPStateFailed, "Failed"),
	)
})

var _ = Describe("PublicIPConditionType", func() {
	It("should have ConfigurationApplied condition type", func() {
		Expect(string(v1alpha1.PublicIPConditionConfigurationApplied)).To(Equal("ConfigurationApplied"))
	})
})

var _ = Describe("PublicIP", func() {
	Describe("GetName", func() {
		It("should return the name", func() {
			ip := &v1alpha1.PublicIP{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ip",
				},
			}

			Expect(ip.GetName()).To(Equal("test-ip"))
		})
	})
})

var _ = Describe("PublicIP Condition Helpers", func() {
	It("should set and get a condition", func() {
		ip := &v1alpha1.PublicIP{}

		condition := metav1.Condition{
			Type:               string(v1alpha1.PublicIPConditionConfigurationApplied),
			Status:             metav1.ConditionTrue,
			Reason:             "ConfigurationApplied",
			Message:            "applied",
			LastTransitionTime: metav1.Now(),
		}

		v1alpha1.SetPublicIPStatusCondition(ip, condition)

		got := v1alpha1.GetPublicIPStatusCondition(ip, v1alpha1.PublicIPConditionConfigurationApplied)
		Expect(got).ToNot(BeNil())
		Expect(got.Status).To(Equal(metav1.ConditionTrue))
	})

	It("should return nil for missing condition", func() {
		ip := &v1alpha1.PublicIP{}

		got := v1alpha1.GetPublicIPStatusCondition(ip, v1alpha1.PublicIPConditionConfigurationApplied)
		Expect(got).To(BeNil())
	})
})
