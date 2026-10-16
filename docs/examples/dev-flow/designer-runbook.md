# Designer runbook

Follow this method for the task's mode. The envelope names the attempt, the flow, `design_path`, the pinned source base, the allowed output paths, and any owner answers from a previous `DESIGN_NEEDS_INPUT`.

## Read first

Read the requested feature, the pinned source, and the project conventions before writing. Do not start from a template that imports a local launcher, a home-directory flow, or a reference snapshot. Adapt the common method to this project's names: functional points, artifacts, markers, gates, and the test bar below.

## Required sections

The design document must contain these seven sections:

1. Overview
2. Requirements, including a numbered list and a complete FP index
3. Design
4. Out of scope
5. Testing plan
6. Open questions
7. Revision history

Include a threat model when the change affects trust boundaries, credentials, isolation, or published artifacts. The FP index is the routing table: each row names the section, or the sibling document and the section, where that FP is specified. A row that names a missing file or a missing heading is an unresolved index. Do not invent the section.

Link a sibling `revision-history.md` rather than accumulating history in design.md. The revision section of design.md names the current revision and links that sibling. Split large work into individually gated designs. No design silently broadens past the FP budget the owner set for the slice.

## Testing plan you require

The testing plan must name unit tests, one function test per FP, benchmark tests for any performance-sensitive path, and the CI plan. The full-flow bar is a 100% pass rate and coverage greater than 80% on the new or changed code, or an explicitly justified enforceable project-specific substitute. A documentation-only slice may state that the production coverage denominator is not applicable and must still keep the project's existing coverage gate.

## External claims

Callsheet behavior is cited from Callsheet interfaces (`docs/coordinator.md`, `docs/workspaces.md`, `docs/real-adapters.md`, and the packages those guides name). A statement about a vendor CLI needs captured evidence (command plus quoted output) or an upstream URL with the supporting quotation. Otherwise label it `ASSUMPTION (unverified)` and state the qualification probe that would settle it. Do not copy historical model defaults or vendor launch recipes.

## Clarification

If a true owner decision or an unavailable measurement blocks the design, return focused questions and `DESIGN_NEEDS_INPUT`. Do not revise the design in that task. The coordinator acquires the answers and starts a new task. Do not invent the answer inside the draft.

## Audit revision

On an audit revision, address every Critical and every Warning, or justify a retained Warning. Bump the revision and append the history in `revision-history.md`. Do not edit `design-review.md`.

## Downstream questions

When a later worker writes `design-questions.md` beside the design, read that sibling and empty it after resolving all questions. Leave it empty of resolved text so a later reader does not treat old questions as open.

## Markers

- `DESIGN_DRAFT`: the seven sections are present, the FP index resolves, and the testing plan names the bar. This is not owner approval.
- `DESIGN_NEEDS_INPUT`: questions only. No audit, and no consumption of a stale draft.
- `BLOCKED`: an environmental obstacle (missing source, unreadable pin, tool failure). Not a design question.

The designer never grants owner sign-off and the designer never emits DESIGN_FINAL.

## Publication

Publish `design.md` (or the task's `design_path`), enumerated FP siblings, and `revision-history.md`. Source and tests stay unchanged. A previous `design-review.md` is immutable input. End the final answer with exactly one status line, `STATUS: DESIGN_DRAFT`, `STATUS: DESIGN_NEEDS_INPUT`, or `STATUS: BLOCKED`.
