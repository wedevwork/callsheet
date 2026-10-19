# Code reviewer runbook

Review the pinned candidate. The envelope names the mode (full or light), the approved design or fix brief, the candidate base, and, in light mode, the `initial source base`. Read that field as the full immutable buggy commit. Validate it as a full commit ID.

## Report

Publish `review.md` next to the governing brief. Include Summary, Critical issues (C1, ...), Warnings (W1, ...), Suggestions (S1, ...), Test bar check, and Evidence. Evidence includes the reviewed base or commit, the commands, the exit codes, relevant quoted failures, and the coverage numerator and denominator or the tool percentage. The report names its input commit and digests, never the as-yet-unknown publication commit.

End the report and the final answer with the same marker:

- `STATUS: REVIEW_APPROVED`
- `STATUS: REVIEW_CHANGES_REQUESTED`
- `STATUS: NEEDS_DESIGN`

## Later rounds

On a later review round, classify each old finding as fixed, persisting, or reopened, with evidence, and distinguish new findings.

## Full mode

Verify all approved FPs, a 100% suite pass, and coverage greater than 80% on the new or changed code (or the approved explicit substitute). Require a function test per FP and benchmarks for performance-sensitive paths. A specification defect is first a review finding. If a Critical persists into a second round because the design cannot be satisfied, `NEEDS_DESIGN` routes it to design revision.

## Light mode

Independently verify that the root cause is addressed, that the regression is meaningful, that it was red before the fix and green after, that the full suite passes at 100%, that coverage has not decreased, and that the scope stayed a restoration. A light-mode scope escalation may emit `NEEDS_DESIGN` immediately.

## Scratch

Tests run in fresh disposable copies under `.callsheet-review-scratch/<task-id>/before/` and `.callsheet-review-scratch/<task-id>/after/`, inside the task's private checkout. Use the Callsheet task ID from the task envelope, not a free-form goal string.

Take the initial digest manifest before creating scratch. Refuse a pre-existing `.callsheet-review-scratch` path rather than reusing or deleting it. Store temporary archives, manifests, and test output under that same fresh task directory. Copy required command results into `review.md`.

## Pinned export

The task checkout includes the selected history in its own object database (`docs/workspaces.md`, Workspace tasks). Export the initial source base with `git archive --format=tar` into `before/`. Export the admitted candidate base into `after/`. Pass the commit ID as an argument. Never interpolate goal text into shell code.

Captured local command evidence: `git archive -h` prints `usage: git archive [<options>] <tree-ish> [<path>...]` and `--[no-]format <fmt>   archive format`.

Both exports must succeed, and the relevant file bytes must match the named commits. If history or required files are unavailable, including archive exclusions, report the blocker. Do not substitute another revision and do not fetch an unpinned source.

When the task envelope names a harness-owned verification helper with its exact command, running that command once is the equivalent of this pinned archive export for the exports and commands it performs: it exports the pinned candidate from the checkout's own objects into `after/` and runs the envelope's commands there. Every input, scratch, and cleanup guarantee of this runbook still applies, and the helper's receipt is evidence for your report, not a review: you still read the design, the code, and the tests and judge every FP yourself. This clause is conditional on such an envelope.

## Independent red and green

A test absent from the buggy revision must be copied unchanged from the candidate into the exported buggy source. Prove it fails for the asserted defect, not compilation or setup. Then run those identical test bytes against the candidate source. Use the same toolchain, flags, coverage scope, and environment for the comparison.

## Cleanup

After every owned test process has exited, remove the scratch tree this task created, verify its absence, and compute the final manifest over the checkout. Compare all candidate project files except the explicitly allowed report paths. Scratch is absent at both comparisons. Cleanup failure blocks approval.

Callsheet snapshots visible files only after the child's process group is gone (`docs/workspaces.md`, Workspace tasks), so scratch removed before exit is not published. Leftover scratch is not an allowed report artifact. The only published change is `review.md`. Before admitting the report, the coordinator verifies the entire published changed-path set. An unauthorized change invalidates the review even if the final marker approves.

## UNOBSERVED is not approval

Any unverified bar prevents approval. Environment-only partial counts are `UNOBSERVED` only with all three of the following:

1. Quoted positive denial evidence. For user namespaces, `unshare -Ur true` and its output.
2. An exact missing-case list consisting solely of cases dependent on that facility.
3. Identical case-for-case counts under pipe and pty.

Otherwise partial counts fail. `UNOBSERVED` still emits `REVIEW_CHANGES_REQUESTED` with the blocker. Do not weaken assertions and do not skip cases.

## Marker

`REVIEW_APPROVED` requires the mode's bar, a clean publication of only `review.md`, and scratch verified absent. Anything short of that is `REVIEW_CHANGES_REQUESTED` or, when the specification itself cannot be satisfied, `NEEDS_DESIGN`.

## Development envelope blocker

When the task envelope requires it, both the final answer and `review.md` contain exactly one plain unindented line `CALLSHEET_REVIEW_BLOCKER: environment` or `CALLSHEET_REVIEW_BLOCKER: none`, immediately before the terminal STATUS line, with no intervening blank line. The key is uppercase, the value is lowercase, with exactly one ASCII space after the colon and no trailing spaces. The values in the two artifacts match. `environment` is valid only with `REVIEW_CHANGES_REQUESTED` and stops for the owner without a fix round. A heading, table cell, list item, quotation, code fence, different case, or extra space is not the field. Missing, repeated, or conflicting fields fail closed. This contract is conditional on the development envelope. It is not mandatory for callers that do not request it.
