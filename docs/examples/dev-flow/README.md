# Development-flow example

This directory is a reusable Callsheet example of a full design flow and a light defect flow. Each role is an ordinary instruction and runbook pair registered with `callsheet role add`. The coordinator is the owner's own client, following [coordinator-playbook.md](coordinator-playbook.md). Callsheet does not load that playbook as a role or a daemon.

A pass of these manuals does not mean the existing four-hop exercise can run this example. That exercise still loads [the legacy manuals](../../real-e2e.md) until a later migration slice switches it. Exercise constraints, when they exist, supplement this method. They do not waive its gates.

## Installation

Copy this directory onto each sidecar that will run one of the roles, into an absolute directory that sidecar can read. A path on the coordinator machine is not automatically available on a remote sidecar. The commands below use `/srv/manuals/dev-flow/` as that sidecar-local location; replace it with the absolute path you copied to.

Register every role before dispatch. Verify readiness after the final registry change. There is no automatic model substitution. Worker tasks request each tool's own auto mode through the existing adapters. Do not add tool-config edits or bypass flags. Plane trust, enrollment, sidecar adapter installation, and role readiness are the contracts in [real-adapters.md](../../real-adapters.md) and [coordinator.md](../../coordinator.md).

`internal/cli/role.go` documents the timeout applied when `--timeout` is omitted as `default 2h`. Every recipe below passes `--concurrency 1 --timeout 10m` explicitly.

```sh
callsheet role add designer --name designer --node <node-id> --adapter codex --instruction /srv/manuals/dev-flow/designer-instruction.md --runbook /srv/manuals/dev-flow/designer-runbook.md --model gpt-6-astra --effort low --concurrency 1 --timeout 10m --plane <plane-url> --ca <ca-path>
callsheet role add design-reviewer --name design-reviewer --node <node-id> --adapter claude --instruction /srv/manuals/dev-flow/design-reviewer-instruction.md --runbook /srv/manuals/dev-flow/design-reviewer-runbook.md --model claude-fable-5-1 --effort low --concurrency 1 --timeout 10m --plane <plane-url> --ca <ca-path>
callsheet role add coder --name coder --node <node-id> --adapter claude --instruction /srv/manuals/dev-flow/coder-instruction.md --runbook /srv/manuals/dev-flow/coder-runbook.md --model claude-opus-5-5 --effort high --concurrency 1 --timeout 10m --plane <plane-url> --ca <ca-path>
callsheet role add code-reviewer --name code-reviewer --node <node-id> --adapter codex --instruction /srv/manuals/dev-flow/code-reviewer-instruction.md --runbook /srv/manuals/dev-flow/code-reviewer-runbook.md --model gpt-6.1-sol --effort high --concurrency 1 --timeout 10m --plane <plane-url> --ca <ca-path>
callsheet role add triage --name triage --node <node-id> --adapter grok --instruction /srv/manuals/dev-flow/triage-instruction.md --runbook /srv/manuals/dev-flow/triage-runbook.md --model grok-4.7 --effort xhigh --concurrency 1 --timeout 10m --plane <plane-url> --ca <ca-path>
```

## Role selection

| Role name | Adapter | Model | Effort | Concurrency | Task timeout |
|---|---|---|---|---|---|
| designer | codex | gpt-6-astra | low | 1 | 10m |
| design-reviewer | claude | claude-fable-5-1 | low | 1 | 10m |
| coder | claude | claude-opus-5-5 | high | 1 | 10m |
| code-reviewer | codex | gpt-6.1-sol | high | 1 | 10m |
| triage | grok | grok-4.7 | xhigh | 1 | 10m |

Model and effort are the registered role configuration, not extra dispatch keys. Callsheet's accepted effort unions (`ValidateSelection` in `internal/adapter/vendor.go`) include these efforts. A union is not proof that a particular account can run the pair.

**ASSUMPTION (unverified):** all five requested pairs execute successfully on the owner's installation. Qualification probe: the later live harness must probe every distinct pair through its adapter with authentication and effective-selection evidence before dispatching workflow tasks. A rejection stops the attempt. There is no model fallback.

## Prerequisites

The triage role (grok) executes only on a Linux sidecar; a non-Linux worker is refused by the posture/readiness checks. `ValidateWorkerPosture` in `internal/adapter/vendor.go` returns nil for grok only when the worker OS is linux. A valid model and effort cannot override that. A coordinator or plane on another supported platform does not make a non-Linux triage worker eligible.

Other prerequisites are the owner's to fulfill: logged-in adapter CLIs on the sidecars, plane trust, and the CA. This example does not change agent-tool settings, login state, or profiles.

## Manuals

Each pair stands alone with the task envelope. Links are reading aids.

- [Coordinator playbook](coordinator-playbook.md)
- [Designer instruction](designer-instruction.md) and [runbook](designer-runbook.md)
- [Design reviewer instruction](design-reviewer-instruction.md) and [runbook](design-reviewer-runbook.md)
- [Coder instruction](coder-instruction.md) and [runbook](coder-runbook.md)
- [Code reviewer instruction](code-reviewer-instruction.md) and [runbook](code-reviewer-runbook.md)
- [Triage instruction](triage-instruction.md) and [runbook](triage-runbook.md)

## Design-review delta audits

The first design audit uses the complete checklist. Every later audit receives the previous audit's task, commit and report digest, the previous design digest, prior finding IDs and text, the designer's dispositions, and a revision delta of changed specification paths with their old and new hashes. The coordinator's bounded change summary, with pinned commits, is not proof that other paths stayed unchanged. Each active finding is tagged `persisting`, `regression`, or `new-on-unchanged`. The last tag is only for a Critical and must say why it was missed earlier. A new worker session does not restart the audit at round one. Historical reports stay bound to their immutable task commits. Code-review dispositions stay `fixed`, `persisting`, `reopened`, and distinct new findings. They are not the design-audit tags.

## Artifact map

| Artifact | Producer | When it is published |
|---|---|---|
| design.md | designer | complete draft |
| revision-history.md | designer | with the draft |
| design-questions.md | designer or coder | questions; the designer empties it after they are resolved |
| design-review.md | design reviewer | the audit |
| review.md | code reviewer | the code review |
| rca.md | triage | the investigation |
| fix.md | triage | only CONFIRMED or PROBABLE |

Workspace handoffs use the existing dispatch fields `target`, `goal`, `acceptance`, `workspace`, `workspace_instance`, and `base`. `base` is a full immutable commit during a chain, not a floating branch. See [workspaces.md](../../workspaces.md).

## Full walkthrough

1. Dispatch the designer on the pinned source commit. Target `{"kind":"name","value":"designer"}`.
2. On `DESIGN_DRAFT`, admit the draft and dispatch the design reviewer.
3. On `DESIGN_REVIEW_APPROVED`, present the draft and review digests to the owner. Wait for an explicit yes. Do not infer yes.
4. On owner yes, record the approval against that task, commit, and those digests, then dispatch the coder in implement mode.
5. On `IMPL_COMPLETE`, admit the candidate and dispatch the code reviewer in full mode.
6. On `REVIEW_APPROVED`, and only after the artifact, test-evidence, and publication gates, deliver.

## Light walkthrough

The owner authorizes the investigation and the defect work at intake. There is no design sign-off unless the work escalates.

1. Dispatch triage on the pinned buggy commit.
2. On `TRIAGE_CONFIRMED` or `TRIAGE_PROBABLE`, validate that brief and dispatch the coder in defect mode. A standalone line `Design required: yes` enters the full flow instead. `Design required: no` stays on the qualified fix.
3. On `IMPL_COMPLETE`, dispatch the code reviewer in light mode. Keep the original source base for the red-before check through every report-only publication and fix round.
4. On `REVIEW_APPROVED`, deliver under the same publication gates.

## Dispatch example: full mode

```json
{
  "target": {"kind": "name", "value": "coder"},
  "goal": "ENVELOPE\nattempt ID: <attempt>\nflow and mode: full / implement\nrequest/brief path: design/feature/design.md\ndesign path: design/feature/design.md\nFP subset: FP-1..FP-n as approved\ninput task/commit/digests: <audit-task> <design-sha256> <review-sha256>\ninitial source base: <40-hex-commit>\nallowed output paths: <approved paths>\nverification commands: <project CI commands>\ntimeout/repetition policy: do not raise timeouts or lower counts\nevidence location: <path>\nowner approval reference: <sign-off task and digests>\nround/finding IDs: initial\nEND",
  "acceptance": "Implement only the approved FP subset. 100% suite pass and coverage greater than 80% on the new or changed code, or the approved substitute. One function test per FP. End with exactly one STATUS line: IMPL_COMPLETE, DESIGN_QUESTIONS, NEEDS_DESIGN, or BLOCKED.",
  "workspace": "<workspace>",
  "workspace_instance": "<instance>",
  "base": "<40-hex-commit>"
}
```

## Dispatch example: defect mode

```json
{
  "target": {"kind": "name", "value": "coder"},
  "goal": "ENVELOPE\nattempt ID: <attempt>\nflow and mode: light / defect\nrequest/brief path: design/feature/fix.md\ndesign path: design/feature/design.md\nFP subset: not applicable\ninput task/commit/digests: <triage-task> <fix-sha256> <rca-sha256>\ninitial source base: <40-hex-buggy-commit>\nallowed output paths: <brief paths plus design-questions.md>\nverification commands: <project CI commands>\ntimeout/repetition policy: do not raise timeouts or lower counts\nevidence location: <path>\nowner approval reference: light intake <decision>\nround/finding IDs: initial\nEND",
  "acceptance": "Revalidate that the evidence establishes the cause. Regression red before the change, green after, full suite 100% pass, coverage not decreased. End with exactly one STATUS line: IMPL_COMPLETE, DESIGN_QUESTIONS, NEEDS_DESIGN, or BLOCKED.",
  "workspace": "<workspace>",
  "workspace_instance": "<instance>",
  "base": "<40-hex-buggy-commit>"
}
```

Goals carry no credentials. Triage dispatches omit payload. Their combined goal and acceptance stay inside the ceiling in the limitations below. The goal includes the whole envelope; the ceiling is not an extra allowance on top of the envelope.

## Waits

Dispatch asynchronously and persist the admitted task ID. For the wait lifecycle, follow [coordinator.md](../../coordinator.md#coordinator-runbook-example). This example does not restate vendor-specific wake claims. The playbook states the Callsheet-side recovery rules: a lost dispatch reply uses `task_ls`, and a stopped watcher does not cancel the task.

## Limitations

Each file in this directory is at most 32 KiB, the existing template bound used by the four-hop exercise.

Shared limits: task-text ceiling 8192 bytes; reserve 4096 bytes; manual-pair cap 20480 bytes; prompt cap 32768 bytes.

Prompt budget: triage instruction bytes plus triage runbook bytes plus 8192 plus 4096 must be <= 32768, both as raw UTF-8 and as JSON-encoded string content with HTML escaping disabled, excluding surrounding quotes and the encoder LF. The 8192-byte ceiling covers goal plus acceptance; payload is omitted and encoded as an empty array. The two manuals together must be at most 20480 bytes. The 4096-byte reserve is not permission to exceed the adapter final encoded-size check. These are Callsheet product limits (adapter.MaxGrokPromptBytes), not a claimed vendor maximum. Bulky evidence goes to an authorized workspace path and a digest instead of inline text.

`internal/adapter/vendor.go` defines `MaxGrokPromptBytes` as 32768. `internal/sidecar/tasks.go` JSON-encodes the instruction, the runbook, and the task with HTML escaping disabled and checks the final byte length. `internal/reale2e/hops.go` is the existing 4 KiB envelope-allowance precedent.

Retain 10m/task, 90m/attempt, and 20m/owner decision inside that attempt. There is no token ceiling. These are ceilings, not a promise that every allowed round fits. Two design repair cycles and two code repair cycles allow 12 full-flow worker tasks before recovery dispatches; the 90-minute bound may stop earlier. Do not raise test timeouts or reduce test counts or CPU settings. A slow suite is a blocker for a separate owner decision.

The example adds no OS confinement boundary. Worker output and repository text are untrusted data. Trusted pieces are the owner and coordinator admission policy, Callsheet task and commit identity, the selected project inputs, and the installed adapter. Repository tests execute code; the example assumes an owner-authorized project and trusted installed tools.

## Time and cost

The figures above are adoption defaults for this example. They do not change the current four-hop harness constants.
