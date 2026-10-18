Exercise scenario (full flow). The feature request is the workspace-root FEATURE.md from the pushed seed. There is no second FEATURE.md in the exercise directory.

The source fixture is FEATURE.md, go.mod, main.go and main_test.go, together strictly below 16 KiB. Each planning or report artifact is at most 32 KiB. All of design.md, revision-history.md, design-questions.md, design-review.md, review.md, rca.md and fix.md together are at most 128 KiB. Every file is a regular nonexecutable UTF-8 file at the workspace root; no directory, symlink, submodule, ignore file or scratch remnant is published.

Keep the ten TestRun cases. A code review runs `go test ./...` and `go test -coverprofile=<task scratch path>/coverage.out ./...` in `.callsheet-review-scratch/<task-id>/after/` inside its checkout, where the task scratch path is `.callsheet-review-scratch/<task-id>` and the task ID is the actual Callsheet task ID. A `before/` export is optional. Remove the scratch and verify it absent before publication. Publish only review.md from a code review, with the review fields the envelope lists.

When the envelope requires it, both review.md and the final answer include exactly one line `CALLSHEET_REVIEW_BLOCKER: environment` or `CALLSHEET_REVIEW_BLOCKER: none` immediately before the STATUS line. A confidence-qualified fix.md includes exactly one line `Design required: yes` or `Design required: no`, and exactly one line `Confidence: CONFIRMED` or `Confidence: PROBABLE` matching its STATUS marker.

This scenario does not waive the seven-section design, review execution, test bars, the owner gate or the delta-audit policy.
