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

package provisioning

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

// mockProvider implements ProvisioningProvider for unit tests in the provisioning package.
type mockProvider struct {
	triggerDeprovisionFunc   func(ctx context.Context, resource client.Object) (*DeprovisionResult, error)
	getDeprovisionStatusFunc func(ctx context.Context, resource client.Object, jobID string) (ProvisionStatus, error)
}

func (m *mockProvider) TriggerProvision(_ context.Context, _ client.Object) (*ProvisionResult, error) {
	return &ProvisionResult{JobID: "mock-job", InitialState: v1alpha1.JobStatePending}, nil
}
func (m *mockProvider) GetProvisionStatus(_ context.Context, _ client.Object, jobID string) (ProvisionStatus, error) {
	return ProvisionStatus{JobID: jobID, State: v1alpha1.JobStateSucceeded}, nil
}
func (m *mockProvider) TriggerDeprovision(ctx context.Context, resource client.Object) (*DeprovisionResult, error) {
	return m.triggerDeprovisionFunc(ctx, resource)
}
func (m *mockProvider) GetDeprovisionStatus(ctx context.Context, resource client.Object, jobID string) (ProvisionStatus, error) {
	return m.getDeprovisionStatusFunc(ctx, resource, jobID)
}
func (m *mockProvider) Name() string { return "mock" }

var ctx = context.Background()

var _ = ginkgo.Describe("EvaluateAction", func() {
	noAPIServerJob := func() bool { return false }
	apiServerHasJob := func() bool { return true }

	ginkgo.DescribeTable("returns the correct action",
		func(jobs []v1alpha1.JobStatus, desiredVersion string, checkAPIServer func() bool, expectedAction Action) {
			state := &State{
				Jobs:                 &jobs,
				DesiredConfigVersion: desiredVersion,
			}
			action, _ := EvaluateAction(state, checkAPIServer)
			Expect(action).To(Equal(expectedAction))
		},

		ginkgo.Entry("no jobs -> trigger",
			[]v1alpha1.JobStatus{},
			"v1",
			noAPIServerJob,
			Trigger,
		),

		ginkgo.Entry("no jobs, API server has job -> requeue",
			[]v1alpha1.JobStatus{},
			"v2",
			apiServerHasJob,
			Requeue,
		),

		ginkgo.Entry("running job -> poll",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateRunning, Timestamp: metav1.NewTime(time.Now())},
			},
			"v1",
			noAPIServerJob,
			Poll,
		),

		ginkgo.Entry("succeeded job with matching config version -> skip",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateSucceeded, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v1",
			noAPIServerJob,
			Skip,
		),

		ginkgo.Entry("failed job with matching config version -> backoff",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v1",
			noAPIServerJob,
			Backoff,
		),

		ginkgo.Entry("failed job with different config version -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v2",
			noAPIServerJob,
			Trigger,
		),

		ginkgo.Entry("terminal job without config version, succeeded -> skip (legacy)",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateSucceeded, Timestamp: metav1.NewTime(time.Now())},
			},
			"v1",
			noAPIServerJob,
			Skip,
		),

		ginkgo.Entry("terminal job without config version, failed -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, Timestamp: metav1.NewTime(time.Now())},
			},
			"v2",
			noAPIServerJob,
			Trigger,
		),

		ginkgo.Entry("job with empty ID (trigger failed) -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, Timestamp: metav1.NewTime(time.Now())},
			},
			"v2",
			noAPIServerJob,
			Trigger,
		),
	)
})

var _ = ginkgo.Describe("ComputeDesiredConfigVersion", func() {
	ginkgo.It("produces consistent hashes for the same input", func() {
		spec := map[string]string{"key": "value"}
		v1, err := ComputeDesiredConfigVersion(spec)
		Expect(err).NotTo(HaveOccurred())
		v2, err := ComputeDesiredConfigVersion(spec)
		Expect(err).NotTo(HaveOccurred())
		Expect(v1).To(Equal(v2))
	})

	ginkgo.It("produces different hashes for different inputs", func() {
		v1, err := ComputeDesiredConfigVersion(map[string]string{"key": "a"})
		Expect(err).NotTo(HaveOccurred())
		v2, err := ComputeDesiredConfigVersion(map[string]string{"key": "b"})
		Expect(err).NotTo(HaveOccurred())
		Expect(v1).NotTo(Equal(v2))
	})
})

var _ = ginkgo.Describe("FindLatestJobByType", func() {
	var baseTime time.Time

	ginkgo.BeforeEach(func() {
		baseTime = time.Now().UTC()
	})

	ginkgo.It("should return nil when jobs array is empty", func() {
		jobs := []v1alpha1.JobStatus{}
		result := FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).To(BeNil())
	})

	ginkgo.It("should return nil when no jobs of requested type exist", func() {
		jobs := []v1alpha1.JobStatus{
			{
				JobID:     "job1",
				Type:      v1alpha1.JobTypeDeprovision,
				Timestamp: metav1.NewTime(baseTime),
				State:     v1alpha1.JobStateRunning,
			},
		}
		result := FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).To(BeNil())
	})

	ginkgo.It("should return the only job when only one job of that type exists", func() {
		jobs := []v1alpha1.JobStatus{
			{
				JobID:     "job1",
				Type:      v1alpha1.JobTypeProvision,
				Timestamp: metav1.NewTime(baseTime),
				State:     v1alpha1.JobStateRunning,
			},
		}
		result := FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).NotTo(BeNil())
		Expect(result.JobID).To(Equal("job1"))
	})

	ginkgo.It("should return the job with latest timestamp when multiple jobs exist", func() {
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
		result := FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).NotTo(BeNil())
		Expect(result.JobID).To(Equal("job3"))
	})

	ginkgo.It("should find latest by timestamp regardless of array order", func() {
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
		result := FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).NotTo(BeNil())
		Expect(result.JobID).To(Equal("job3"))
	})

	ginkgo.It("should only consider jobs of the requested type", func() {
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
		result := FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).NotTo(BeNil())
		Expect(result.JobID).To(Equal("provision2"))
		Expect(result.Type).To(Equal(v1alpha1.JobTypeProvision))

		// Should find latest deprovision job
		result = FindLatestJobByType(jobs, v1alpha1.JobTypeDeprovision)
		Expect(result).NotTo(BeNil())
		Expect(result.JobID).To(Equal("deprovision1"))
		Expect(result.Type).To(Equal(v1alpha1.JobTypeDeprovision))
	})

	ginkgo.It("should handle jobs with identical timestamps", func() {
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
		result := FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(result).NotTo(BeNil())
		// When timestamps are equal, returns first one found
		Expect(result.JobID).To(Equal("job1"))
	})
})

var _ = ginkgo.Describe("GetJobsFromResource", func() {
	ginkgo.It("returns jobs from ComputeInstance", func() {
		ci := &v1alpha1.ComputeInstance{}
		ci.Status.Jobs = []v1alpha1.JobStatus{{JobID: "j1"}}
		Expect(GetJobsFromResource(ci)).To(HaveLen(1))
	})

	ginkgo.It("returns jobs from ClusterOrder", func() {
		co := &v1alpha1.ClusterOrder{}
		co.Status.Jobs = []v1alpha1.JobStatus{{JobID: "j1"}, {JobID: "j2"}}
		Expect(GetJobsFromResource(co)).To(HaveLen(2))
	})

	ginkgo.It("returns nil for unsupported type", func() {
		subnet := &v1alpha1.Subnet{}
		Expect(GetJobsFromResource(subnet)).To(BeNil())
	})
})

var _ = ginkgo.Describe("HandleBackoff", func() {
	ginkgo.It("triggers when backoff elapsed", func() {
		jobs := []v1alpha1.JobStatus{
			{JobID: "j1", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, ConfigVersion: "v1",
				Timestamp: metav1.NewTime(time.Now().UTC().Add(-BackoffBaseDelay - time.Minute))},
		}
		provState := &State{Jobs: &jobs, DesiredConfigVersion: "v1"}
		latestJob := &jobs[0]
		triggered := false
		result, err := HandleBackoff(ctx, provState, latestJob, func() (ctrl.Result, error) {
			triggered = true
			return ctrl.Result{RequeueAfter: BackoffBaseDelay}, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(triggered).To(BeTrue())
		Expect(result.RequeueAfter).To(Equal(BackoffBaseDelay))
	})

	ginkgo.It("backs off when not enough time elapsed", func() {
		jobs := []v1alpha1.JobStatus{
			{JobID: "j1", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, ConfigVersion: "v1",
				Timestamp: metav1.NewTime(time.Now().UTC().Add(-30 * time.Second))},
		}
		provState := &State{Jobs: &jobs, DesiredConfigVersion: "v1"}
		latestJob := &jobs[0]
		triggered := false
		result, err := HandleBackoff(ctx, provState, latestJob, func() (ctrl.Result, error) {
			triggered = true
			return ctrl.Result{}, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(triggered).To(BeFalse())
		Expect(result.RequeueAfter).To(BeNumerically("~", BackoffBaseDelay, 35*time.Second))
	})
})

var _ = ginkgo.Describe("TriggerDeprovisionJob", func() {
	const pollInterval = 30 * time.Second
	const maxHistory = 5
	resource := &v1alpha1.ClusterOrder{}

	ginkgo.It("handles DeprovisionSkipped — returns done with no requeue", func() {
		provider := &mockProvider{
			triggerDeprovisionFunc: func(_ context.Context, _ client.Object) (*DeprovisionResult, error) {
				return &DeprovisionResult{Action: DeprovisionSkipped}, nil
			},
		}
		jobs := []v1alpha1.JobStatus{}
		result, err := TriggerDeprovisionJob(ctx, provider, resource, &jobs, maxHistory, pollInterval)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(jobs).To(BeEmpty())
	})

	ginkgo.It("handles DeprovisionWaiting — updates provision job and requeues", func() {
		provider := &mockProvider{
			triggerDeprovisionFunc: func(_ context.Context, _ client.Object) (*DeprovisionResult, error) {
				return &DeprovisionResult{
					Action: DeprovisionWaiting,
					ProvisionJobStatus: &ProvisionStatus{
						State:   v1alpha1.JobStateFailed,
						Message: "cancelled",
					},
				}, nil
			},
		}
		jobs := []v1alpha1.JobStatus{
			{JobID: "prov-1", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateRunning, Timestamp: metav1.NewTime(time.Now())},
		}
		result, err := TriggerDeprovisionJob(ctx, provider, resource, &jobs, maxHistory, pollInterval)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(pollInterval))
		// Provision job should be updated
		provJob := FindLatestJobByType(jobs, v1alpha1.JobTypeProvision)
		Expect(provJob.State).To(Equal(v1alpha1.JobStateFailed))
		Expect(provJob.Message).To(Equal("cancelled"))
	})

	ginkgo.It("handles DeprovisionTriggered — appends deprovision job and requeues", func() {
		provider := &mockProvider{
			triggerDeprovisionFunc: func(_ context.Context, _ client.Object) (*DeprovisionResult, error) {
				return &DeprovisionResult{
					Action:                 DeprovisionTriggered,
					JobID:                  "deprov-1",
					BlockDeletionOnFailure: true,
				}, nil
			},
		}
		jobs := []v1alpha1.JobStatus{}
		result, err := TriggerDeprovisionJob(ctx, provider, resource, &jobs, maxHistory, pollInterval)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(pollInterval))
		deprovJob := FindLatestJobByType(jobs, v1alpha1.JobTypeDeprovision)
		Expect(deprovJob).NotTo(BeNil())
		Expect(deprovJob.JobID).To(Equal("deprov-1"))
		Expect(deprovJob.State).To(Equal(v1alpha1.JobStatePending))
		Expect(deprovJob.BlockDeletionOnFailure).To(BeTrue())
	})

	ginkgo.It("handles rate-limit error — requeues with RetryAfter", func() {
		provider := &mockProvider{
			triggerDeprovisionFunc: func(_ context.Context, _ client.Object) (*DeprovisionResult, error) {
				return nil, &RateLimitError{RetryAfter: 45 * time.Second}
			},
		}
		jobs := []v1alpha1.JobStatus{}
		result, err := TriggerDeprovisionJob(ctx, provider, resource, &jobs, maxHistory, pollInterval)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(45 * time.Second))
	})

	ginkgo.It("returns error on non-rate-limit provider error", func() {
		provider := &mockProvider{
			triggerDeprovisionFunc: func(_ context.Context, _ client.Object) (*DeprovisionResult, error) {
				return nil, fmt.Errorf("connection refused")
			},
		}
		jobs := []v1alpha1.JobStatus{}
		_, err := TriggerDeprovisionJob(ctx, provider, resource, &jobs, maxHistory, pollInterval)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("connection refused"))
	})
})

var _ = ginkgo.Describe("PollDeprovisionJob", func() {
	const pollInterval = 30 * time.Second
	resource := &v1alpha1.ClusterOrder{}

	ginkgo.It("returns done=true when job already succeeded", func() {
		jobs := []v1alpha1.JobStatus{
			{JobID: "d1", Type: v1alpha1.JobTypeDeprovision, State: v1alpha1.JobStateSucceeded, Timestamp: metav1.NewTime(time.Now())},
		}
		latestJob := &jobs[0]
		provider := &mockProvider{}
		result, done, err := PollDeprovisionJob(ctx, provider, resource, &jobs, latestJob, pollInterval)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	ginkgo.It("blocks deletion when already-terminal job failed with BlockDeletionOnFailure", func() {
		jobs := []v1alpha1.JobStatus{
			{JobID: "d1", Type: v1alpha1.JobTypeDeprovision, State: v1alpha1.JobStateFailed,
				BlockDeletionOnFailure: true, Timestamp: metav1.NewTime(time.Now())},
		}
		latestJob := &jobs[0]
		provider := &mockProvider{}
		result, done, err := PollDeprovisionJob(ctx, provider, resource, &jobs, latestJob, pollInterval)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(result.RequeueAfter).To(Equal(pollInterval))
	})

	ginkgo.It("continues polling when job is still running", func() {
		jobs := []v1alpha1.JobStatus{
			{JobID: "d1", Type: v1alpha1.JobTypeDeprovision, State: v1alpha1.JobStatePending, Timestamp: metav1.NewTime(time.Now())},
		}
		latestJob := &jobs[0]
		provider := &mockProvider{
			getDeprovisionStatusFunc: func(_ context.Context, _ client.Object, _ string) (ProvisionStatus, error) {
				return ProvisionStatus{State: v1alpha1.JobStateRunning, Message: "in progress"}, nil
			},
		}
		result, done, err := PollDeprovisionJob(ctx, provider, resource, &jobs, latestJob, pollInterval)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(result.RequeueAfter).To(Equal(pollInterval))
		// Job status should be updated
		Expect(jobs[0].State).To(Equal(v1alpha1.JobStateRunning))
		Expect(jobs[0].Message).To(Equal("in progress"))
	})

	ginkgo.It("returns done=true when poll shows job succeeded", func() {
		jobs := []v1alpha1.JobStatus{
			{JobID: "d1", Type: v1alpha1.JobTypeDeprovision, State: v1alpha1.JobStateRunning, Timestamp: metav1.NewTime(time.Now())},
		}
		latestJob := &jobs[0]
		provider := &mockProvider{
			getDeprovisionStatusFunc: func(_ context.Context, _ client.Object, _ string) (ProvisionStatus, error) {
				return ProvisionStatus{State: v1alpha1.JobStateSucceeded, Message: "done"}, nil
			},
		}
		result, done, err := PollDeprovisionJob(ctx, provider, resource, &jobs, latestJob, pollInterval)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	ginkgo.It("blocks deletion when poll shows failure with BlockDeletionOnFailure", func() {
		jobs := []v1alpha1.JobStatus{
			{JobID: "d1", Type: v1alpha1.JobTypeDeprovision, State: v1alpha1.JobStateRunning,
				BlockDeletionOnFailure: true, Timestamp: metav1.NewTime(time.Now())},
		}
		latestJob := &jobs[0]
		provider := &mockProvider{
			getDeprovisionStatusFunc: func(_ context.Context, _ client.Object, _ string) (ProvisionStatus, error) {
				return ProvisionStatus{State: v1alpha1.JobStateFailed, Message: "deprovision failed"}, nil
			},
		}
		result, done, err := PollDeprovisionJob(ctx, provider, resource, &jobs, latestJob, pollInterval)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(result.RequeueAfter).To(Equal(pollInterval))
	})

	ginkgo.It("persists error message on poll failure and requeues", func() {
		jobs := []v1alpha1.JobStatus{
			{JobID: "d1", Type: v1alpha1.JobTypeDeprovision, State: v1alpha1.JobStateRunning, Timestamp: metav1.NewTime(time.Now())},
		}
		latestJob := &jobs[0]
		provider := &mockProvider{
			getDeprovisionStatusFunc: func(_ context.Context, _ client.Object, _ string) (ProvisionStatus, error) {
				return ProvisionStatus{}, fmt.Errorf("timeout")
			},
		}
		result, done, err := PollDeprovisionJob(ctx, provider, resource, &jobs, latestJob, pollInterval)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(result.RequeueAfter).To(Equal(pollInterval))
		Expect(jobs[0].Message).To(ContainSubstring("timeout"))
	})

	ginkgo.It("allows deletion when failed without BlockDeletionOnFailure", func() {
		jobs := []v1alpha1.JobStatus{
			{JobID: "d1", Type: v1alpha1.JobTypeDeprovision, State: v1alpha1.JobStateRunning,
				BlockDeletionOnFailure: false, Timestamp: metav1.NewTime(time.Now())},
		}
		latestJob := &jobs[0]
		provider := &mockProvider{
			getDeprovisionStatusFunc: func(_ context.Context, _ client.Object, _ string) (ProvisionStatus, error) {
				return ProvisionStatus{State: v1alpha1.JobStateFailed, Message: "failed"}, nil
			},
		}
		result, done, err := PollDeprovisionJob(ctx, provider, resource, &jobs, latestJob, pollInterval)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		Expect(result).To(Equal(ctrl.Result{}))
	})
})
