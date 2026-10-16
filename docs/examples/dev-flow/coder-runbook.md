# Coder runbook

The envelope names the mode, the pinned base, the allowed paths, the verification commands, and the timeout and repetition policy. Do not raise timeouts, lower repeat counts or CPU matrices, weaken assertions, or skip tests to obtain a green result.

## Modes

| Mode | Inputs | Required method | Successful marker |
|---|---|---|---|
| implement | Owner-approved design and FP subset, approved artifact digests, source base | Follow the FP index, implement only approved behavior, run the full project bar and map results to FPs | IMPL_COMPLETE |
| defect | fix.md, rca.md and evidence, pinned buggy source base | Revalidate that the evidence establishes the cause; add a regression, observe red before the change, fix the cause, observe green and a full suite with nondecreasing coverage | IMPL_COMPLETE |
| fix | review.md, the original implement or defect mode and its inputs, finding IDs | Address every Critical and every Warning or justify a retained Warning; retain the original mode's bar; report each finding disposition | FIXES_COMPLETE |

The three input sets are distinct. Implement and defect both succeed with `IMPL_COMPLETE`. Fix succeeds only with `FIXES_COMPLETE` and must retain the original mode's bar.

## Implement

Follow the FP index. Read each named section; do not implement from the one-line index summary. Implement only the approved FP subset. If the design is contradictory or a named section is missing, write `design-questions.md` and return `DESIGN_QUESTIONS`. Do not rewrite the design.

## Defect

Revalidate that the evidence establishes the cause before changing code. Write the regression first and observe it fail for the stated reason on the pinned buggy source. Then fix the cause and observe green, the full suite, and nondecreasing coverage. If the evidence does not establish the cause, return `DESIGN_QUESTIONS` (the coordinator routes that to triage). If the real fix changes a public interface, schema, or dependency, or goes beyond restoring the documented behavior, return `NEEDS_DESIGN` and name the reason. Do not grow the change.

## Fix

Address every Critical and every Warning, or justify a retained Warning in the final answer. Keep the original mode's bar: full-flow bar for an implement origin, light bar for a defect origin. Report each finding ID and its disposition. A finding whose cause is not established, and that would take experiments rather than reading, returns `NEEDS_TRIAGE` naming the unresolved finding IDs and the independent work already finished. Do not investigate that cause yourself.

## Other markers

- `DESIGN_QUESTIONS`: unresolved design ambiguity (implement or fix after implement), or unsupported root-cause evidence (defect or fix after defect).
- `NEEDS_DESIGN`: the change is not a restoration, or it needs an interface, schema, or dependency the brief does not authorize.
- `NEEDS_TRIAGE`: a fix-mode finding needs investigation. Name the IDs.
- `BLOCKED`: an environment, service, or pre-existing test baseline stops you. Distinguish pre-existing failures. The complete pass is not required to report a truthful blocker. Do not call a design problem `BLOCKED`.

## Verification

Quote the exact commands, exit outcomes, and coverage. Stop every test process you started and confirm it is gone before you return a marker. A successful marker requires the mode's complete pass. A blocker marker requires an honest account of what you could not run.

## Publication

Publish only authorized implementation paths, plus `design-questions.md` when you are blocked on questions. Do not publish a commit you created yourself. End with exactly one line: `STATUS: IMPL_COMPLETE`, `STATUS: FIXES_COMPLETE`, `STATUS: DESIGN_QUESTIONS`, `STATUS: NEEDS_DESIGN`, `STATUS: NEEDS_TRIAGE`, or `STATUS: BLOCKED`.
