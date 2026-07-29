package provisioning

import (
	"github.com/osac-project/osac-operator/api/v1alpha1"
)

// FindJobByID finds a job by its ID in the jobs array.
// Returns a pointer to the job if found, nil otherwise.
func FindJobByID(jobs []v1alpha1.JobStatus, jobID string) *v1alpha1.JobStatus {
	for i := range jobs {
		if jobs[i].JobID == jobID {
			return &jobs[i]
		}
	}
	return nil
}

// UpdateJob updates an existing job by ID with new values.
// Returns true if the job was found and updated, false otherwise.
func UpdateJob(jobs []v1alpha1.JobStatus, updatedJob v1alpha1.JobStatus) bool {
	job := FindJobByID(jobs, updatedJob.JobID)
	if job == nil {
		return false
	}
	*job = updatedJob
	return true
}

// AppendJob adds a new job to the jobs array and trims history to maxHistory entries
// per target group (see normalizeTarget) rather than across the whole slice. Without
// this scoping, a target that retries repeatedly (e.g. failing backoff) could evict
// another target's sole tracked job out of history entirely, making
// FindLatestJobByTypeAndTarget see "no job" for it and trigger a duplicate. Provision
// and Deprovision jobs for the same target still share one budget, matching
// pre-existing single-target behavior — single-target resources never set Target, so
// all their jobs normalize to one group and this is equivalent to the old
// whole-slice trim.
func AppendJob(jobs []v1alpha1.JobStatus, newJob v1alpha1.JobStatus, maxHistory int) []v1alpha1.JobStatus {
	jobs = append(jobs, newJob)

	want := normalizeTarget(newJob.Target)
	matchCount := 0
	for i := range jobs {
		if normalizeTarget(jobs[i].Target) == want {
			matchCount++
		}
	}
	if matchCount <= maxHistory {
		return jobs
	}

	toDrop := matchCount - maxHistory
	trimmed := make([]v1alpha1.JobStatus, 0, len(jobs)-toDrop)
	dropped := 0
	for i := range jobs {
		if dropped < toDrop && normalizeTarget(jobs[i].Target) == want {
			dropped++
			continue
		}
		trimmed = append(trimmed, jobs[i])
	}
	return trimmed
}

// NeedsProvisionJob determines if a new provision job should be triggered.
// Returns true if no job exists, or if the previous job failed (allowing retry).
// Used by controllers without config-version-based provisioning (SecurityGroup,
// Subnet, VirtualNetwork). Controllers with ConfigVersion support should use
// EvaluateAction instead, which adds backoff and spec-change detection.
func NeedsProvisionJob(latestJob *v1alpha1.JobStatus) bool {
	// No job exists yet
	if latestJob == nil || latestJob.JobID == "" {
		return true
	}

	// Job is still running
	if !latestJob.State.IsTerminal() {
		return false
	}

	// Trigger new job if previous job failed (retry logic)
	return !latestJob.State.IsSuccessful()
}

// FindLatestJobByType finds the most recent job of the specified type by timestamp.
// Returns nil if no job of that type exists.
func FindLatestJobByType(jobs []v1alpha1.JobStatus, jobType v1alpha1.JobType) *v1alpha1.JobStatus {
	var latest *v1alpha1.JobStatus
	for i := range jobs {
		if jobs[i].Type == jobType {
			if latest == nil || jobs[i].Timestamp.After(latest.Timestamp.Time) {
				latest = &jobs[i]
			}
		}
	}
	return latest
}

// normalizeTarget maps an empty JobTarget to JobTargetFabric. Fabric was the only
// manager that existed before multi-target dispatch was introduced, so jobs persisted
// before this feature shipped (and single-target resources, which never set Target at
// all) are treated as fabric jobs for matching purposes.
func normalizeTarget(target v1alpha1.JobTarget) v1alpha1.JobTarget {
	if target == "" {
		return v1alpha1.JobTargetFabric
	}
	return target
}

// FindLatestJobByTypeAndTarget finds the most recent job of the specified type and
// target by timestamp. Returns nil if no matching job exists. See normalizeTarget for
// how empty Target values (legacy jobs and single-target resources) are matched.
func FindLatestJobByTypeAndTarget(jobs []v1alpha1.JobStatus, jobType v1alpha1.JobType, target v1alpha1.JobTarget) *v1alpha1.JobStatus {
	want := normalizeTarget(target)
	var latest *v1alpha1.JobStatus
	for i := range jobs {
		if jobs[i].Type != jobType || normalizeTarget(jobs[i].Target) != want {
			continue
		}
		if latest == nil || jobs[i].Timestamp.After(latest.Timestamp.Time) {
			latest = &jobs[i]
		}
	}
	return latest
}
