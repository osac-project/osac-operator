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
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

// provisionAction represents the outcome of shouldTriggerProvision.
type provisionAction int

const (
	provisionSkip    provisionAction = iota // nothing to do
	provisionTrigger                        // trigger a new provision job
	provisionPoll                           // poll an existing non-terminal job
	provisionRequeue                        // stale cache detected, requeue to refresh
	provisionBackoff                        // failed job with same config, retry after backoff
)

const (
	backoffBaseDelay = 2 * time.Minute
	backoffMaxDelay  = 30 * time.Minute
)

// handleProvisionBackoff checks if the backoff period has elapsed since the last failed job.
// If elapsed, it calls triggerFn to retry. Otherwise, it returns a RequeueAfter with the remaining delay.
func handleProvisionBackoff(ctx context.Context, jobs []v1alpha1.JobStatus, configVersion string, latestJob *v1alpha1.JobStatus, triggerFn func() (ctrl.Result, error)) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	backoff := computeBackoffFromJobs(jobs, configVersion)
	elapsed := time.Now().UTC().Sub(latestJob.Timestamp.Time.UTC())
	if elapsed >= backoff {
		log.Info("backoff elapsed, retrying provision", "jobID", latestJob.JobID, "backoff", backoff, "elapsed", elapsed)
		return triggerFn()
	}
	remaining := backoff - elapsed
	log.Info("provision failed, backing off", "jobID", latestJob.JobID, "backoff", backoff, "remaining", remaining)
	return ctrl.Result{RequeueAfter: remaining}, nil
}

// hasJobID returns true if the job is non-nil and has a non-empty JobID.
func hasJobID(job *v1alpha1.JobStatus) bool {
	return job != nil && job.JobID != ""
}

// computeBackoffFromJobs determines the next backoff duration based on the gap
// between the last two failed provision jobs with the same ConfigVersion.
// First failure uses backoffBaseDelay. Subsequent failures double the previous gap.
func computeBackoffFromJobs(jobs []v1alpha1.JobStatus, configVersion string) time.Duration {
	// Find last two failed provision jobs with matching ConfigVersion (reverse order)
	var last, prev *v1alpha1.JobStatus
	for i := len(jobs) - 1; i >= 0; i-- {
		j := &jobs[i]
		if j.Type != v1alpha1.JobTypeProvision || j.State != v1alpha1.JobStateFailed || j.ConfigVersion != configVersion {
			continue
		}
		if last == nil {
			last = j
		} else {
			prev = j
			break
		}
	}

	if last == nil {
		return backoffBaseDelay
	}
	if prev == nil {
		return backoffBaseDelay
	}

	gap := last.Timestamp.Time.UTC().Sub(prev.Timestamp.Time.UTC())
	if gap <= 0 {
		return backoffBaseDelay
	}
	nextDelay := gap * 2
	if nextDelay < backoffBaseDelay {
		return backoffBaseDelay
	}
	if nextDelay > backoffMaxDelay {
		return backoffMaxDelay
	}
	return nextDelay
}
