Exercise scenario (light flow). The light request is the first paragraph of the workspace-root FEATURE.md from the pushed seed; the rest of FEATURE.md is the unchanged behavior contract. A light attempt has no design, design audit, owner design gate or escalation work: a design need stops the attempt.

The source fixture is FEATURE.md, go.mod, main.go and main_test.go, together strictly below 16 KiB; FEATURE.md and go.mod never change. Each planning or report artifact is at most 32 KiB, all of them together at most 128 KiB. Every file is a regular nonexecutable UTF-8 file at the workspace root; no directory, symlink, submodule, ignore file or scratch remnant is published.

Triage investigates the seed read-only and publishes rca.md, and a qualified fix.md only at CONFIRMED or PROBABLE. The coder restores the behavior in defect mode (fix rounds keep the defect bar), keeps the nine existing TestRun cases and adds TestRun/whitespace. A light code review runs the harness verification helper with --scenario light exactly as its envelope says and publishes only review.md with the helper's report lines pasted verbatim.

When the envelope requires it, both review.md and the final answer include exactly one line `CALLSHEET_REVIEW_BLOCKER: environment` or `CALLSHEET_REVIEW_BLOCKER: none` immediately before the STATUS line. A confidence-qualified fix.md includes exactly one line `Design required: yes` or `Design required: no`, exactly one line `Confidence: CONFIRMED` or `Confidence: PROBABLE` matching its STATUS marker, and the one root-cause reference line the envelope specifies.

This scenario does not waive the regression's red/green evidence, the ten CLI cases, the coverage comparison or the review's execution evidence.
