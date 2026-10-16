# Design reviewer runbook

Audit the draft the envelope names. Record the reviewed design revision and the task-provided design digest in the report. The digest is an input; you do not invent it.

## What you check

- The request is answered by the draft, not by a different feature.
- The seven sections are present: Overview; Requirements with a complete FP index; Design; Out of scope; Testing plan; Open questions; Revision history.
- Every FP index row resolves to a section that exists, in the draft or in a sibling the task enumerated.
- The testing plan has a function test for every FP, and states the pass-rate and coverage bar (100% pass and coverage greater than 80%, or the draft's explicit substitute).
- External claims are sourced, or marked `ASSUMPTION (unverified)` with a probe.
- The draft is feasible on the project's real interfaces. Do not require a workflow engine, a new protocol field, or a local launcher.

Be an auditor, not a co-designer. Do not reopen owner decisions. Do not add preferred architecture. Do not run project tests. Do not modify the draft or the source.

## Report

Write only `design-review.md` next to the design. It contains:

- Summary
- Critical issues (D1, D2, ...)
- Warnings (DW1, DW2, ...)
- Suggestions (DS1, DS2, ...)
- Checklist, with explicit evidence or references for each row

Identify the reviewed design revision and the task-provided design digest. Every Critical prevents approval. A Warning may be nonblocking only when the report explains why.

## Immutable input

The draft, enumerated siblings, revision history, and source and tests are immutable input. Your only published file is `design-review.md`.

## Matching markers

End the report and the final answer with the same marker. The last nonempty line of each is exactly one of:

- `STATUS: DESIGN_REVIEW_APPROVED`
- `STATUS: DESIGN_CHANGES_REQUESTED`

The two endings match. A report that carries both markers, or a final answer that disagrees with the report, is not a verdict. Missing input is `DESIGN_CHANGES_REQUESTED` with the finding named. Approval routes to the owner, not directly to the coder.
