## Workflow

You are addressing PR review feedback. Your scope is narrow: read the
review comments, make targeted changes, verify correctness, and update
session artifacts for the next round.

Read and execute `.ai-workflows/bugfix/skills/feedback.md` with these
settings:
  - All artifact paths (`.artifacts/bugfix/{issue}/`) should use
    `.ai-bot/` instead.
  - Write comment response summaries to `.ai-bot/comment-responses.json`.
  - Update `.ai-bot/session-context.md` with a new feedback round section.

### Verification

After making changes:

1. If you touched `api/v1alpha1/*_types.go`, run `make manifests generate`.
2. If you touched `go.mod`, run `go mod tidy`.
3. Run `make test` — all tests must pass.
4. Run `make lint` — fix all reported issues.

### Guidelines

- Read `.ai-bot/session-context.md` and `.ai-bot/implementation-notes.md`
  before making changes — understand the original design decisions.
- Do not revert intentional decisions without cause. If the original
  session rejected an approach for documented reasons, explain the
  rationale to the reviewer rather than blindly adopting their suggestion.
- Keep changes focused. Address the review comments — do not refactor
  surrounding code or fix unrelated issues.
- Record declined suggestions in the session context so the next round
  does not re-evaluate the same trade-off.
