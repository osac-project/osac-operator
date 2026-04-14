package v1alpha1_test

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/osac-project/osac-operator/api/v1alpha1"
)

var _ = Describe("PublicIPPoolSpec", func() {
	It("should accept a valid spec with all required fields", func() {
		spec := v1alpha1.PublicIPPoolSpec{
			Region:                 "us-east-1",
			CIDRs:                  []string{"192.168.1.0/24", "192.168.2.0/24"},
			IPFamily:               "IPv4",
			ImplementationStrategy: "metallb-l2",
		}

		Expect(spec.Region).To(Equal("us-east-1"))
		Expect(spec.CIDRs).To(Equal([]string{"192.168.1.0/24", "192.168.2.0/24"}))
		Expect(spec.IPFamily).To(Equal("IPv4"))
		Expect(spec.ImplementationStrategy).To(Equal("metallb-l2"))
	})

	It("should accept a minimal spec without optional fields", func() {
		spec := v1alpha1.PublicIPPoolSpec{
			Region:   "eu-west-1",
			CIDRs:    []string{"10.0.0.0/16"},
			IPFamily: "IPv4",
		}

		Expect(spec.Region).To(Equal("eu-west-1"))
		Expect(spec.CIDRs).To(HaveLen(1))
		Expect(spec.IPFamily).To(Equal("IPv4"))
		Expect(spec.ImplementationStrategy).To(BeEmpty())
	})
})

var _ = Describe("PublicIPPoolPhaseType", func() {
	DescribeTable("should have correct string values",
		func(phase v1alpha1.PublicIPPoolPhaseType, expected string) {
			Expect(string(phase)).To(Equal(expected))
		},
		Entry("Progressing phase", v1alpha1.PublicIPPoolPhaseProgressing, "Progressing"),
		Entry("Failed phase", v1alpha1.PublicIPPoolPhaseFailed, "Failed"),
		Entry("Ready phase", v1alpha1.PublicIPPoolPhaseReady, "Ready"),
		Entry("Deleting phase", v1alpha1.PublicIPPoolPhaseDeleting, "Deleting"),
	)
})

var _ = Describe("PublicIPPoolConditionType", func() {
	It("should have ConfigurationApplied condition type", func() {
		Expect(string(v1alpha1.PublicIPPoolConditionConfigurationApplied)).To(Equal("ConfigurationApplied"))
	})
})

var _ = Describe("PublicIPPool", func() {
	Describe("GetName", func() {
		It("should return the name", func() {
			pool := &v1alpha1.PublicIPPool{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pool",
				},
			}

			Expect(pool.GetName()).To(Equal("test-pool"))
		})
	})
})

var _ = Describe("PublicIPPool Condition Helpers", func() {
	It("should set and get a condition", func() {
		pool := &v1alpha1.PublicIPPool{}

		condition := metav1.Condition{
			Type:               string(v1alpha1.PublicIPPoolConditionConfigurationApplied),
			Status:             metav1.ConditionTrue,
			Reason:             "ConfigurationApplied",
			Message:            "applied",
			LastTransitionTime: metav1.Now(),
		}

		v1alpha1.SetPublicIPPoolStatusCondition(pool, condition)

		got := v1alpha1.GetPublicIPPoolStatusCondition(pool, v1alpha1.PublicIPPoolConditionConfigurationApplied)
		Expect(got).ToNot(BeNil())
		Expect(got.Status).To(Equal(metav1.ConditionTrue))
	})

	It("should return nil for missing condition", func() {
		pool := &v1alpha1.PublicIPPool{}

		got := v1alpha1.GetPublicIPPoolStatusCondition(pool, v1alpha1.PublicIPPoolConditionConfigurationApplied)
		Expect(got).To(BeNil())
	})
})

var _ = Describe("PublicIPPoolStatus", func() {
	It("should accept capacity fields", func() {
		status := v1alpha1.PublicIPPoolStatus{
			Total:     256,
			Allocated: 10,
			Available: 246,
		}

		Expect(status.Total).To(Equal(int64(256)))
		Expect(status.Allocated).To(Equal(int64(10)))
		Expect(status.Available).To(Equal(int64(246)))
	})
})
