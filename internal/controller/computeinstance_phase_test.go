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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	osacv1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

var _ = Describe("ComputeInstance Phase State Machine", func() {

	newInstance := func(phase osacv1alpha1.ComputeInstancePhaseType, desired, reconciled string) *osacv1alpha1.ComputeInstance {
		return &osacv1alpha1.ComputeInstance{
			Status: osacv1alpha1.ComputeInstanceStatus{
				Phase:                   phase,
				DesiredConfigVersion:    desired,
				ReconciledConfigVersion: reconciled,
			},
		}
	}

	newJob := func(state osacv1alpha1.JobState) *osacv1alpha1.JobStatus {
		return &osacv1alpha1.JobStatus{
			State: state,
		}
	}

	// "" (empty) -> Starting
	Describe("resolveEmptyPhase", func() {
		It("should transition to Starting", func() {
			instance := newInstance("", "", "")
			Expect(resolveEmptyPhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
		})
	})

	// Starting -> Running   when config versions match and no active provision job
	// Starting -> Failed    when provision job failed
	// Starting -> Starting  otherwise
	Describe("resolveStartingPhase", func() {
		It("should transition to Running when versions match and no active job", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseStarting, "abc", "abc")
			Expect(resolveStartingPhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseRunning))
		})

		It("should transition to Running when versions match and job succeeded", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseStarting, "abc", "abc")
			job := newJob(osacv1alpha1.JobStateSucceeded)
			Expect(resolveStartingPhase(instance, job)).To(Equal(osacv1alpha1.ComputeInstancePhaseRunning))
		})

		It("should transition to Failed when job failed", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseStarting, "abc", "")
			job := newJob(osacv1alpha1.JobStateFailed)
			Expect(resolveStartingPhase(instance, job)).To(Equal(osacv1alpha1.ComputeInstancePhaseFailed))
		})

		It("should transition to Failed when job canceled", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseStarting, "abc", "")
			job := newJob(osacv1alpha1.JobStateCanceled)
			Expect(resolveStartingPhase(instance, job)).To(Equal(osacv1alpha1.ComputeInstancePhaseFailed))
		})

		It("should stay Starting when job is running", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseStarting, "abc", "")
			job := newJob(osacv1alpha1.JobStateRunning)
			Expect(resolveStartingPhase(instance, job)).To(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
		})

		It("should stay Starting when job is pending", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseStarting, "abc", "")
			job := newJob(osacv1alpha1.JobStatePending)
			Expect(resolveStartingPhase(instance, job)).To(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
		})

		It("should stay Starting when versions differ and no job", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseStarting, "abc", "")
			Expect(resolveStartingPhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
		})

		It("should stay Starting when versions match but job still active", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseStarting, "abc", "abc")
			job := newJob(osacv1alpha1.JobStateRunning)
			Expect(resolveStartingPhase(instance, job)).To(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
		})
	})

	// Running -> Updating  when config versions differ
	// Running -> Running   otherwise
	Describe("resolveRunningPhase", func() {
		It("should transition to Updating when versions differ", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseRunning, "def", "abc")
			Expect(resolveRunningPhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseUpdating))
		})

		It("should stay Running when versions match", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseRunning, "abc", "abc")
			Expect(resolveRunningPhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseRunning))
		})
	})

	// Updating -> Running   when config versions match and no active provision job
	// Updating -> Failed    when provision job failed
	// Updating -> Updating  otherwise
	Describe("resolveUpdatingPhase", func() {
		It("should transition to Running when versions match and no active job", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseUpdating, "def", "def")
			Expect(resolveUpdatingPhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseRunning))
		})

		It("should transition to Running when versions match and job succeeded", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseUpdating, "def", "def")
			job := newJob(osacv1alpha1.JobStateSucceeded)
			Expect(resolveUpdatingPhase(instance, job)).To(Equal(osacv1alpha1.ComputeInstancePhaseRunning))
		})

		It("should transition to Failed when job failed", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseUpdating, "def", "abc")
			job := newJob(osacv1alpha1.JobStateFailed)
			Expect(resolveUpdatingPhase(instance, job)).To(Equal(osacv1alpha1.ComputeInstancePhaseFailed))
		})

		It("should transition to Failed when job canceled", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseUpdating, "def", "abc")
			job := newJob(osacv1alpha1.JobStateCanceled)
			Expect(resolveUpdatingPhase(instance, job)).To(Equal(osacv1alpha1.ComputeInstancePhaseFailed))
		})

		It("should stay Updating when job is running", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseUpdating, "def", "abc")
			job := newJob(osacv1alpha1.JobStateRunning)
			Expect(resolveUpdatingPhase(instance, job)).To(Equal(osacv1alpha1.ComputeInstancePhaseUpdating))
		})

		It("should stay Updating when versions match but job still active", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseUpdating, "def", "def")
			job := newJob(osacv1alpha1.JobStateRunning)
			Expect(resolveUpdatingPhase(instance, job)).To(Equal(osacv1alpha1.ComputeInstancePhaseUpdating))
		})
	})

	// Failed -> Failed (terminal)
	Describe("resolveFailedPhase", func() {
		It("should stay Failed", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseFailed, "abc", "")
			Expect(resolveFailedPhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseFailed))
		})
	})

	// Deleting -> Deleting (terminal)
	Describe("resolveDeletingPhase", func() {
		It("should stay Deleting", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseDeleting, "abc", "abc")
			Expect(resolveDeletingPhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseDeleting))
		})
	})

	// resolvePhase dispatches to the correct resolver
	Describe("resolvePhase", func() {
		It("should dispatch empty phase to resolveEmptyPhase", func() {
			instance := newInstance("", "", "")
			Expect(resolvePhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
		})

		It("should dispatch Starting phase", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseStarting, "abc", "abc")
			Expect(resolvePhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseRunning))
		})

		It("should dispatch Running phase", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseRunning, "def", "abc")
			Expect(resolvePhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseUpdating))
		})

		It("should dispatch Updating phase", func() {
			instance := newInstance(osacv1alpha1.ComputeInstancePhaseUpdating, "def", "def")
			Expect(resolvePhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseRunning))
		})

		It("should conserve unknown phase", func() {
			instance := newInstance("SomeUnknownPhase", "abc", "abc")
			Expect(resolvePhase(instance, nil)).To(Equal(osacv1alpha1.ComputeInstancePhaseType("SomeUnknownPhase")))
		})
	})

	// Helper functions
	Describe("configVersionsMatch", func() {
		It("should return true when versions match", func() {
			instance := newInstance("", "abc", "abc")
			Expect(configVersionsMatch(instance)).To(BeTrue())
		})

		It("should return false when versions differ", func() {
			instance := newInstance("", "abc", "def")
			Expect(configVersionsMatch(instance)).To(BeFalse())
		})

		It("should return false when desired is empty", func() {
			instance := newInstance("", "", "")
			Expect(configVersionsMatch(instance)).To(BeFalse())
		})

		It("should return false when reconciled is empty", func() {
			instance := newInstance("", "abc", "")
			Expect(configVersionsMatch(instance)).To(BeFalse())
		})
	})

	Describe("hasActiveJob", func() {
		It("should return false when job is nil", func() {
			Expect(hasActiveJob(nil)).To(BeFalse())
		})

		It("should return true when job is running", func() {
			job := newJob(osacv1alpha1.JobStateRunning)
			Expect(hasActiveJob(job)).To(BeTrue())
		})

		It("should return true when job is pending", func() {
			job := newJob(osacv1alpha1.JobStatePending)
			Expect(hasActiveJob(job)).To(BeTrue())
		})

		It("should return false when job succeeded", func() {
			job := newJob(osacv1alpha1.JobStateSucceeded)
			Expect(hasActiveJob(job)).To(BeFalse())
		})

		It("should return false when job failed", func() {
			job := newJob(osacv1alpha1.JobStateFailed)
			Expect(hasActiveJob(job)).To(BeFalse())
		})
	})
})
