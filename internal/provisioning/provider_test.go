package provisioning_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/innabox/cloudkit-operator/internal/provisioning"
)

func TestProvisioning(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Provisioning Suite")
}

var _ = Describe("JobState", func() {
	Describe("IsTerminal", func() {
		It("should return false for pending state", func() {
			Expect(provisioning.JobStatePending.IsTerminal()).To(BeFalse())
		})

		It("should return false for running state", func() {
			Expect(provisioning.JobStateRunning.IsTerminal()).To(BeFalse())
		})

		It("should return true for succeeded state", func() {
			Expect(provisioning.JobStateSucceeded.IsTerminal()).To(BeTrue())
		})

		It("should return true for failed state", func() {
			Expect(provisioning.JobStateFailed.IsTerminal()).To(BeTrue())
		})
	})

	Describe("IsSuccessful", func() {
		It("should return false for pending state", func() {
			Expect(provisioning.JobStatePending.IsSuccessful()).To(BeFalse())
		})

		It("should return false for running state", func() {
			Expect(provisioning.JobStateRunning.IsSuccessful()).To(BeFalse())
		})

		It("should return true for succeeded state", func() {
			Expect(provisioning.JobStateSucceeded.IsSuccessful()).To(BeTrue())
		})

		It("should return false for failed state", func() {
			Expect(provisioning.JobStateFailed.IsSuccessful()).To(BeFalse())
		})
	})
})
