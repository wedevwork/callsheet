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

## Delta audit

On every audit after the first, verify the designer's dispositions of the prior findings and verify the changed design plus affected dependencies against the original checklist. The supervisor derives the changed-path and hash list from the two admitted trees. Use that list. The coordinator's change summary is not proof that an unlisted path stayed unchanged.

Tag each active finding `persisting`, `regression`, or `new-on-unchanged`. `regression` names the changed dependency when the affected text itself did not change. `new-on-unchanged` is only a Critical, with a specific reason it was missed earlier. Do not file a new Warning or Suggestion against unchanged material. List resolved IDs separately with evidence; they are not active findings and do not take one of these tags. Keep IDs for persisting and reintroduced issues. New IDs do not reuse closed ones. If the prior report, the prior design digest, or the revision delta is missing or unreadable, report `DESIGN_CHANGES_REQUESTED` with that named finding. Do not open an unbound first audit because a new worker session started.
