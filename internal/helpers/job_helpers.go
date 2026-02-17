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

package helpers

import (
	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

// FindJobByID finds a job by its ID in the jobs array.
// Returns a pointer to the job if found, nil otherwise.
// The returned pointer can be used to update the job in place.
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

// AppendJob adds a new job to the jobs array and trims to maxHistory.
// Jobs are appended in chronological order (oldest first, newest last).
func AppendJob(jobs []v1alpha1.JobStatus, newJob v1alpha1.JobStatus, maxHistory int) []v1alpha1.JobStatus {
	jobs = append(jobs, newJob)
	if len(jobs) > maxHistory {
		// Keep only the last maxHistory jobs
		jobs = jobs[len(jobs)-maxHistory:]
	}
	return jobs
}
