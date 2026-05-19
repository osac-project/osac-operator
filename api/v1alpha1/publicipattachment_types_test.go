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

var _ = Describe("PublicIPAttachmentSpec", func() {
	It("should accept a valid spec with all fields", func() {
		ci := "my-instance"
		spec := v1alpha1.PublicIPAttachmentSpec{
			PublicIP:         "my-public-ip",
			ComputeInstance: &ci,
		}

		Expect(spec.PublicIP).To(Equal("my-public-ip"))
		Expect(spec.ComputeInstance).ToNot(BeNil())
		Expect(*spec.ComputeInstance).To(Equal("my-instance"))
	})

	It("should accept a minimal spec without optional target", func() {
		spec := v1alpha1.PublicIPAttachmentSpec{
			PublicIP: "my-public-ip",
		}

		Expect(spec.PublicIP).To(Equal("my-public-ip"))
		Expect(spec.ComputeInstance).To(BeNil())
	})
})

var _ = Describe("PublicIPAttachmentPhaseType", func() {
	DescribeTable("should have correct string values",
		func(phase v1alpha1.PublicIPAttachmentPhaseType, expected string) {
			Expect(string(phase)).To(Equal(expected))
		},
		Entry("Progressing phase", v1alpha1.PublicIPAttachmentPhaseProgressing, "Progressing"),
		Entry("Failed phase", v1alpha1.PublicIPAttachmentPhaseFailed, "Failed"),
		Entry("Ready phase", v1alpha1.PublicIPAttachmentPhaseReady, "Ready"),
		Entry("Deleting phase", v1alpha1.PublicIPAttachmentPhaseDeleting, "Deleting"),
	)
})

var _ = Describe("PublicIPAttachmentConditionType", func() {
	It("should have ConfigurationApplied condition type", func() {
		Expect(string(v1alpha1.PublicIPAttachmentConditionConfigurationApplied)).To(Equal("ConfigurationApplied"))
	})
})

var _ = Describe("PublicIPAttachment", func() {
	Describe("GetName", func() {
		It("should return the name", func() {
			attachment := &v1alpha1.PublicIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-attachment",
				},
			}

			Expect(attachment.GetName()).To(Equal("test-attachment"))
		})
	})
})

var _ = Describe("PublicIPAttachment Condition Helpers", func() {
	It("should set and get a condition", func() {
		attachment := &v1alpha1.PublicIPAttachment{}

		condition := metav1.Condition{
			Type:               string(v1alpha1.PublicIPAttachmentConditionConfigurationApplied),
			Status:             metav1.ConditionTrue,
			Reason:             "ConfigurationApplied",
			Message:            "applied",
			LastTransitionTime: metav1.Now(),
		}

		v1alpha1.SetPublicIPAttachmentStatusCondition(attachment, condition)

		got := v1alpha1.GetPublicIPAttachmentStatusCondition(attachment, v1alpha1.PublicIPAttachmentConditionConfigurationApplied)
		Expect(got).ToNot(BeNil())
		Expect(got.Status).To(Equal(metav1.ConditionTrue))
	})

	It("should return nil for missing condition", func() {
		attachment := &v1alpha1.PublicIPAttachment{}

		got := v1alpha1.GetPublicIPAttachmentStatusCondition(attachment, v1alpha1.PublicIPAttachmentConditionConfigurationApplied)
		Expect(got).To(BeNil())
	})
})
