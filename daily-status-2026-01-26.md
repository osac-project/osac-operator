# Daily Status Report - January 26, 2026

**Engineer:** Etabak
**Ticket:** [MGMT-22682](https://issues.redhat.com/browse/MGMT-22682) - VM Restart Request Implementation
**GitHub Issue:** [#316](https://github.com/innabox/issues/issues/316)
**Phase:** E2E Testing & Integration Validation
**Branch:** feature/vm-scheduled-reboot-controller-mgmt-22682

---

## Task Summary (MGMT-22682)

Implementing VM restart capability for ComputeInstances using a declarative signal pattern. This feature is critical for:
- Operations requiring VM restart (CPU/memory decrease operations)
- User-initiated troubleshooting restarts
- Supporting automated restart workflows via external schedulers

**Current Status:** Code implementation complete, now in testing phase addressing environment configuration issues to validate the full restart flow.

---

## Accomplishments

### Phase 1: API Schema Implementation (Completed - MGMT-22682)

**Why this phase matters:** The API schema defines the contract between users, CLI, and the controller. Getting this right first ensures all components can communicate the restart request and status consistently across the entire system.

- Successfully merged restart API fields to fulfillment-api (v0.0.25) and fulfillment-service
- Implemented declarative signal pattern using `restartRequestedAt` and `lastRestartedAt` timestamps
- Updated terminology from "reboot" to "restart" across all APIs for consistency with KubeVirt
- Bumped fulfillment-common dependency to v0.0.35

**Business Impact:** Provides the foundation for users to request VM restarts through the API, enabling both immediate restarts and future scheduled restart capabilities.

### Phase 2: Controller Logic Implementation (Completed - MGMT-22682)

**Why this phase matters:** The controller is the brain that watches for restart requests and executes them. This logic must be idempotent, handle edge cases, and ensure VM restarts happen reliably without data loss or unnecessary downtime.

- Implemented restart controller logic in cloudkit-operator with timestamp comparison
- Created comprehensive unit test suite (6 tests, all passing)
- Implemented VMI deletion-based restart mechanism (leveraging KubeVirt's auto-recreation)
- Fixed linter issues and ensured all pre-commit checks pass

**Business Impact:** Enables automated restart execution when users request it, ensuring VMs restart cleanly and status is tracked accurately.

### Phase 3: E2E Test Infrastructure (Completed - MGMT-22682)

**Why this phase matters:** E2E tests validate the complete user journey from API request to VM restart completion. These tests catch integration issues that unit tests miss and ensure the feature works in real Kubernetes environments.

- Developed complete E2E test suite in fulfillment-service repository
- Resolved architectural dependency by placing tests in fulfillment-service (avoids circular dependency)
- Created supporting automation: `run-local.sh` script, Kubernetes Job manifest, comprehensive README
- Deployed custom images to vmaas-dev namespace for testing

**Business Impact:** Ensures confidence that the restart feature works end-to-end before releasing to production, reducing risk of user-facing failures.

### Environment Issue Resolution (Ongoing - MGMT-22682)

**Why this phase matters:** Environment configuration issues block E2E test validation. These must be resolved to prove the restart feature works in a real deployment scenario, which is critical before merging code and releasing the feature.

- Fixed AuthConfig missing cloudkit-operator service account in admin_subjects list
- Corrected webhook service name from `innabox-eda-service:5000` to `innabox-aap-eda-api:8000`
- Fixed environment variable names (CLOUDKIT_COMPUTE_INSTANCE_*_WEBHOOK vs CLOUDKIT_VM_*_WEBHOOK)

**Business Impact:** Unblocks testing pipeline and ensures the vmaas-dev environment can support full ComputeInstance provisioning and restart workflows.

---

## Risks and Challenges (MGMT-22682)

### Critical Blocker: Webhook Integration Failure
**Ticket Impact:** Blocks MGMT-22682 E2E test validation
**Issue:** Webhook endpoint returns HTTP 405 Method Not Allowed
**Impact:** VirtualMachine provisioning cannot proceed, preventing validation of the complete restart flow
**Service:** `http://innabox-aap-eda-api:8000/create-vm`
**Status:** Under investigation
**Why it matters:** The restart feature depends on ComputeInstances being fully provisioned (with VirtualMachines created) before testing restart functionality. Without working webhooks, we cannot create the baseline VMs needed to test restarts.

### Permission Issues in vmaas Namespace
**Ticket Impact:** Complicates MGMT-22682 E2E test execution
**Issue:** Difficulty creating ComputeInstance resources in vmaas namespace during testing
**Context:** E2E tests require creating test ComputeInstances via fulfillment-service API
**Current approach:** Using admin service account token for private API access
**Concern:** May indicate RBAC configuration gaps or missing service account permissions
**Why it matters:** Proper RBAC is essential for production deployment. Permission issues during testing may indicate similar issues users will face in production environments.

### Environment Configuration Drift
**Ticket Impact:** Delays MGMT-22682 validation timeline
**Issue:** Multiple configuration mismatches discovered (webhook URLs, env vars, AuthConfig)
**Risk:** Custom deployed images (quay.io/etabak/*) may be in inconsistent state
**Mitigation strategy:** Planning to reset to official public images first to establish baseline
**Why it matters:** Cannot confidently validate the restart feature if the underlying environment is broken. Need to prove basic provisioning works before testing advanced restart functionality.

### Test Execution Blocked
**Ticket Impact:** Cannot validate MGMT-22682 is ready for production
**Status:** E2E test compiles successfully but cannot run to completion
**Dependencies:**
- Webhook integration must be functional (baseline requirement)
- Complete ComputeInstance provisioning flow must work end-to-end (pre-requisite for restart)
- VirtualMachineReference must be set correctly in status (restart target identification)
**Why it matters:** E2E test validation is the final gate before merging MGMT-22682 code. Without passing E2E tests, cannot confidently release the restart feature to users.

---

## Key Efforts (MGMT-22682)

### Current Focus: Environment Reset and Validation

**Strategic Rationale:** Before validating the MGMT-22682 restart feature, we must establish that the baseline ComputeInstance provisioning works. This de-risks the E2E testing by separating environment issues from feature-specific issues.

#### 1. Reset to Official Images (MGMT-22682)
**Why this is critical:** Multiple configuration issues suggest the environment may be in an inconsistent state. Starting with known-good official images provides a clean baseline and helps determine if problems are environmental or feature-specific.

- Plan to deploy all official public images (cloudkit-operator, fulfillment-service, fulfillment-api, fulfillment-controller)
- Establish known-good baseline before deploying custom restart feature
- Validate basic ComputeInstance provisioning works correctly

**Success Criteria:** Can create a ComputeInstance and observe full provisioning (VirtualMachine created, status updated, VMI running)

#### 2. Webhook Investigation (Prerequisite for MGMT-22682)
**Why this is critical:** The restart feature requires a fully provisioned ComputeInstance to restart. If webhooks are broken, VirtualMachines never get created, making restart testing impossible.

- Determine why `innabox-aap-eda-api:8000/create-vm` returns 405
- Verify webhook integration is configured for VM provisioning
- Document expected webhook contract and behavior

**Success Criteria:** Webhook calls succeed and VirtualMachines are created in response to ComputeInstance creation

#### 3. E2E Test Validation (Final Gate for MGMT-22682)
**Why this is critical:** This validates the complete user journey and proves the restart feature works in a real Kubernetes environment with KubeVirt. This is the confidence gate before releasing to production.

- Once baseline environment is stable, run E2E tests with official images
- Deploy custom restart feature images only after validation succeeds
- Test complete restart flow: request → VMI deletion → recreation → status update

**Success Criteria:** E2E test passes end-to-end, demonstrating:
1. ComputeInstance creates successfully
2. Restart request via API updates `restartRequestedAt`
3. Controller detects restart request and deletes VMI
4. KubeVirt recreates VMI automatically
5. Status shows `lastRestartedAt` updated correctly
6. No RestartFailed conditions present

### Next Steps (MGMT-22682 Testing Pipeline)
1. **Baseline Validation:** Identify all deployments using custom images (quay.io/etabak/*)
2. **Environment Reset:** Switch to official public images for baseline validation
3. **Smoke Test:** Create test ComputeInstance and verify complete provisioning flow
4. **Blocker Resolution:** Investigate and resolve webhook 405 error
5. **Feature Deployment:** Re-deploy custom restart feature images after baseline works
6. **Final Validation:** Execute E2E restart test suite

### Testing Approach (Phased Validation for MGMT-22682)

**Why phased:** This approach isolates variables and makes debugging easier. If baseline fails, we know it's environmental. If baseline passes but restart fails, we know it's feature-specific.

- **Phase 1 - Baseline:** Validate basic provisioning with official images
  - *Purpose:* Prove environment can provision VMs without restart feature
- **Phase 2 - API Layer:** Deploy custom fulfillment-service with restart API
  - *Purpose:* Validate API schema changes don't break existing functionality
- **Phase 3 - Controller:** Deploy custom cloudkit-operator with restart controller
  - *Purpose:* Validate controller logic can detect and execute restart requests
- **Phase 4 - Integration:** Run comprehensive E2E restart test
  - *Purpose:* Validate complete restart flow end-to-end in real environment

---

## Technical Context (MGMT-22682)

### Test Architecture
- **Ticket:** MGMT-22682
- **E2E Test Location:** `/home/etabak/work/src/github/fulfillment-service/test/e2e/computeinstance_restart_test.go`
- **Authentication:** Using admin service account token (system:serviceaccount:vmaas-dev:admin)
  - *Rationale:* Private API (privatev1.ComputeInstancesClient) requires admin permissions per Authorino config
- **Test Execution:** Requires port-forward to fulfillment-api service (kubectl port-forward -n vmaas-dev svc/fulfillment-api 8000:8000)
- **Test Strategy:** Validates complete restart lifecycle from API request to VMI recreation

### Environment Details
- **Ticket:** MGMT-22682
- **Namespace:** vmaas-dev
- **Cluster:** KubeVirt-enabled Kubernetes cluster
- **Auth:** Authorino-based authentication
- **Current Images:** Custom builds at quay.io/etabak/* (restart-test tag)
  - fulfillment-service:restart-test (includes MGMT-22682 API fields)
  - cloudkit-operator:restart-test (includes MGMT-22682 controller logic)

### Code Status (MGMT-22682)
- **Branch:** feature/vm-scheduled-reboot-controller-mgmt-22682
- **Repository:** cloudkit-operator
- **Commits:** 3 commits ahead of main
  1. VM restart request implementation with declarative signal pattern
  2. Remove 2-minute minimum lead time requirement
  3. Add unit tests for VM restart request functionality
- **Pre-commit Checks:** All passing (fmt, vet, lint, test, build)
- **Unit Tests:** 6/6 passing (timestamp comparison, restart execution logic)
- **Integration Status:** E2E test infrastructure complete, blocked on environment issues

---

## Metrics

- **Lines Changed:** ~400+ across fulfillment-api, fulfillment-service, cloudkit-operator
- **New Files Created:** 5 (restart.go, restart_test.go, computeinstance_restart_test.go, suite_test.go, run-local.sh)
- **PRs Merged:** 3 (fulfillment-api v0.0.25, fulfillment-common v0.0.35 bump, fulfillment-service restart API)
- **Tests Written:** 6 unit tests + comprehensive E2E test suite
- **Issues Fixed:** 4 (AuthConfig, webhook service name, env vars, wrong operator version)

---

## Notes (MGMT-22682)

The MGMT-22682 restart feature implementation is functionally complete with all unit tests passing. The current blocker is environmental - establishing a stable baseline for E2E testing.

**Key Insight:** The strategy of resetting to official images first is sound and will help isolate whether issues are related to the new restart feature (MGMT-22682) or pre-existing environment configuration problems. This de-risking approach ensures we don't spend time debugging feature code when the underlying platform is broken.

**Confidence Level:** High confidence in code quality (unit tests passing, pre-commit checks clean). Medium confidence in E2E readiness pending environment stabilization.

**Path Forward:** Once baseline provisioning works with official images, deploying the MGMT-22682 custom images should be straightforward, enabling final E2E validation before PR approval.
