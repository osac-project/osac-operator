## Workflow

IMPORTANT: You will not commit changes — the orchestration system
commits after your session ends. Treat the end of your session as
the "before committing" checkpoint. Stay on the current branch
(already created by the orchestration system).

Execute the following bugfix workflow phases in order.
Each phase is defined in the corresponding skill file.

1. Read and execute `.ai-workflows/bugfix/skills/assess.md`
   The bug report is in `.ai-bot/issue.md`. Do not ask clarifying
   questions — make reasonable assumptions where needed.

2. Read and execute `.ai-workflows/bugfix/skills/diagnose.md`
   Write your root cause analysis to `.ai-bot/diagnosis.md`.
   Read `.claude/rules/controller-patterns.md` and
   `.claude/rules/common-pitfalls.md` before diagnosing — many bugs
   in this codebase fall into the documented pitfall categories.

3. Read and execute `.ai-workflows/bugfix/skills/fix.md`
   Implement the minimal fix. Write implementation notes to
   `.ai-bot/implementation-notes.md`.
   - If you touch `api/v1alpha1/*_types.go`, run `make manifests generate`
     immediately.
   - If you touch `go.mod`, run `go mod tidy` immediately.
   - Always add or update unit tests in the same step.

4. Read and execute `.ai-workflows/bugfix/skills/test.md`
   Run the full test suite with `make test`. If tests fail, revise
   your fix and retest (up to 5 iterations).
   Write test verification to `.ai-bot/test-verification.md`.

5. Read and execute `.ai-workflows/bugfix/skills/review.md`
   Self-review your changes. If issues are found, correct them,
   retest, and re-review (up to 4 iterations).
   Write review findings to `.ai-bot/review.md`.

6. Run `make lint` and fix all reported issues. Repeat until it
   exits cleanly. This is the final gate — lint failures block CI.

7. Write a PR title and description to `.ai-bot/pr.md`.
   Use the `## Title` heading format:

   ```markdown
   ## Title

   OSAC-XXXXX: short description in lowercase

   ## Summary

   ...PR body...

   ## Root Cause

   ...(from .ai-bot/diagnosis.md)...
   ```

8. Write session context to `.ai-bot/session-context.md` for
   continuity if feedback rounds are needed.
