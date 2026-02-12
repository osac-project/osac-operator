package v1alpha1_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/innabox/cloudkit-operator/api/v1alpha1"
)

var _ = Describe("JobState", func() {
	Describe("IsTerminal", func() {
		It("should return false for pending state", func() {
			Expect(v1alpha1.JobStatePending.IsTerminal()).To(BeFalse())
		})

		It("should return false for waiting state", func() {
			Expect(v1alpha1.JobStateWaiting.IsTerminal()).To(BeFalse())
		})

		It("should return false for running state", func() {
			Expect(v1alpha1.JobStateRunning.IsTerminal()).To(BeFalse())
		})

		It("should return false for unknown state", func() {
			Expect(v1alpha1.JobStateUnknown.IsTerminal()).To(BeFalse())
		})

		It("should return true for succeeded state", func() {
			Expect(v1alpha1.JobStateSucceeded.IsTerminal()).To(BeTrue())
		})

		It("should return true for failed state", func() {
			Expect(v1alpha1.JobStateFailed.IsTerminal()).To(BeTrue())
		})

		It("should return true for canceled state", func() {
			Expect(v1alpha1.JobStateCanceled.IsTerminal()).To(BeTrue())
		})
	})

	Describe("IsSuccessful", func() {
		It("should return false for pending state", func() {
			Expect(v1alpha1.JobStatePending.IsSuccessful()).To(BeFalse())
		})

		It("should return false for waiting state", func() {
			Expect(v1alpha1.JobStateWaiting.IsSuccessful()).To(BeFalse())
		})

		It("should return false for running state", func() {
			Expect(v1alpha1.JobStateRunning.IsSuccessful()).To(BeFalse())
		})

		It("should return false for unknown state", func() {
			Expect(v1alpha1.JobStateUnknown.IsSuccessful()).To(BeFalse())
		})

		It("should return true for succeeded state", func() {
			Expect(v1alpha1.JobStateSucceeded.IsSuccessful()).To(BeTrue())
		})

		It("should return false for failed state", func() {
			Expect(v1alpha1.JobStateFailed.IsSuccessful()).To(BeFalse())
		})

		It("should return false for canceled state", func() {
			Expect(v1alpha1.JobStateCanceled.IsSuccessful()).To(BeFalse())
		})
	})
})

var _ = Describe("FindLatestJobByType", func() {
	var baseTime time.Time

	BeforeEach(func() {
		baseTime = time.Now().UTC()
	})

	It("should return nil when jobs array is empty", func() {
		jobs := []v1alpha1.JobStatus{}
		result := v1alpha1.FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).To(BeNil())
	})

	It("should return nil when no jobs of requested type exist", func() {
		jobs := []v1alpha1.JobStatus{
			{
				JobID:     "job1",
				Type:      v1alpha1.JobTypeDeprovision,
				Timestamp: metav1.NewTime(baseTime),
				State:     v1alpha1.JobStateRunning,
			},
		}
		result := v1alpha1.FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).To(BeNil())
	})

	It("should return the only job when only one job of that type exists", func() {
		jobs := []v1alpha1.JobStatus{
			{
				JobID:     "job1",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(baseTime),
				State:     v1alpha1.JobStateRunning,
			},
		}
		result := v1alpha1.FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).NotTo(BeNil())
		Expect(result.JobID).To(Equal("job1"))
	})

	It("should return the job with latest timestamp when multiple jobs exist", func() {
		jobs := []v1alpha1.JobStatus{
			{
				JobID:     "job1",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(baseTime.Add(-2 * time.Hour)),
				State:     v1alpha1.JobStateSucceeded,
			},
			{
				JobID:     "job2",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(baseTime.Add(-1 * time.Hour)),
				State:     v1alpha1.JobStateRunning,
			},
			{
				JobID:     "job3",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(baseTime.Add(-30 * time.Minute)),
				State:     v1alpha1.JobStatePending,
			},
		}
		result := v1alpha1.FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).NotTo(BeNil())
		Expect(result.JobID).To(Equal("job3"))
	})

	It("should find latest by timestamp regardless of array order", func() {
		// Jobs deliberately NOT in chronological order
		jobs := []v1alpha1.JobStatus{
			{
				JobID:     "job3",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(baseTime.Add(-30 * time.Minute)), // Most recent
				State:     v1alpha1.JobStatePending,
			},
			{
				JobID:     "job1",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(baseTime.Add(-2 * time.Hour)), // Oldest
				State:     v1alpha1.JobStateSucceeded,
			},
			{
				JobID:     "job2",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(baseTime.Add(-1 * time.Hour)), // Middle
				State:     v1alpha1.JobStateRunning,
			},
		}
		result := v1alpha1.FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).NotTo(BeNil())
		Expect(result.JobID).To(Equal("job3"))
	})

	It("should only consider jobs of the requested type", func() {
		jobs := []v1alpha1.JobStatus{
			{
				JobID:     "provision1",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(baseTime.Add(-2 * time.Hour)),
				State:     v1alpha1.JobStateSucceeded,
			},
			{
				JobID:     "deprovision1",
				Type:      v1alpha1.JobTypeDeprovision,
				Timestamp: metav1.NewTime(baseTime.Add(-30 * time.Minute)), // Most recent overall
				State:     v1alpha1.JobStateRunning,
			},
			{
				JobID:     "provision2",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(baseTime.Add(-1 * time.Hour)),
				State:     v1alpha1.JobStateRunning,
			},
		}
		// Should find latest provision job, not latest overall
		result := v1alpha1.FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).NotTo(BeNil())
		Expect(result.JobID).To(Equal("provision2"))
		Expect(result.Type).To(Equal(v1alpha1.JobTypeProvision))

		// Should find latest deprovision job
		result = v1alpha1.FindLatestJobByType(jobs, v1alpha1.JobTypeDeprovision)
		Expect(result).NotTo(BeNil())
		Expect(result.JobID).To(Equal("deprovision1"))
		Expect(result.Type).To(Equal(v1alpha1.JobTypeDeprovision))
	})

	It("should handle jobs with identical timestamps", func() {
		sameTime := metav1.NewTime(baseTime)
		jobs := []v1alpha1.JobStatus{
			{
				JobID:     "job1",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: sameTime,
				State:     v1alpha1.JobStateSucceeded,
			},
			{
				JobID:     "job2",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: sameTime,
				State:     v1alpha1.JobStateRunning,
			},
		}
		result := v1alpha1.FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).NotTo(BeNil())
		// When timestamps are equal, returns first one found
		Expect(result.JobID).To(Equal("job1"))
	})
})
