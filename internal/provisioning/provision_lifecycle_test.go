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
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

var ctx = context.Background()

var _ = ginkgo.Describe("EvaluateAction", func() {
	noAPIServerJob := func() bool { return false }
	apiServerHasJob := func() bool { return true }

	ginkgo.DescribeTable("returns the correct action",
		func(jobs []v1alpha1.JobStatus, desiredVersion string, reconciledVersion string, checkAPIServer func() bool, expectedAction Action) {
			state := &State{
				Jobs:                    &jobs,
				DesiredConfigVersion:    desiredVersion,
				ReconciledConfigVersion: reconciledVersion,
			}
			action, _ := EvaluateAction(state, checkAPIServer)
			Expect(action).To(Equal(expectedAction))
		},

		ginkgo.Entry("no jobs, versions match -> skip",
			[]v1alpha1.JobStatus{},
			"v1", "v1",
			noAPIServerJob,
			Skip,
		),

		ginkgo.Entry("no jobs, versions differ -> trigger",
			[]v1alpha1.JobStatus{},
			"v2", "v1",
			noAPIServerJob,
			Trigger,
		),

		ginkgo.Entry("no jobs, versions differ, API server has job -> requeue",
			[]v1alpha1.JobStatus{},
			"v2", "v1",
			apiServerHasJob,
			Requeue,
		),

		ginkgo.Entry("running job -> poll",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateRunning, Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "",
			noAPIServerJob,
			Poll,
		),

		ginkgo.Entry("succeeded job with matching config version -> skip",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateSucceeded, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "",
			noAPIServerJob,
			Skip,
		),

		ginkgo.Entry("failed job with matching config version -> backoff",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "",
			noAPIServerJob,
			Backoff,
		),

		ginkgo.Entry("failed job with different config version -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v2", "",
			noAPIServerJob,
			Trigger,
		),

		ginkgo.Entry("terminal job without config version, versions match -> skip",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateSucceeded, Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "v1",
			noAPIServerJob,
			Skip,
		),

		ginkgo.Entry("terminal job without config version, versions differ -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateSucceeded, Timestamp: metav1.NewTime(time.Now())},
			},
			"v2", "v1",
			noAPIServerJob,
			Trigger,
		),

		ginkgo.Entry("job with empty ID (trigger failed) and versions differ -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, Timestamp: metav1.NewTime(time.Now())},
			},
			"v2", "v1",
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

var _ = ginkgo.Describe("SyncReconciledConfigVersion", func() {
	ginkgo.It("returns the annotation value when present", func() {
		annotations := map[string]string{"osac.openshift.io/reconciled-config-version": "v1"}
		Expect(SyncReconciledConfigVersion(ctx, annotations, "osac.openshift.io/reconciled-config-version")).To(Equal("v1"))
	})

	ginkgo.It("returns empty string when annotation is absent", func() {
		Expect(SyncReconciledConfigVersion(ctx, map[string]string{}, "osac.openshift.io/reconciled-config-version")).To(BeEmpty())
	})

	ginkgo.It("returns empty string when annotations map is nil", func() {
		Expect(SyncReconciledConfigVersion(ctx, nil, "osac.openshift.io/reconciled-config-version")).To(BeEmpty())
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

	ginkgo.It("returns jobs from HostPool", func() {
		hp := &v1alpha1.HostPool{}
		Expect(GetJobsFromResource(hp)).To(BeEmpty())
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
