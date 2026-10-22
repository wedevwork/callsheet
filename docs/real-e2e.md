# Real local end-to-end exercise

This runbook is the single entry point for Callsheet's manual real-worker exercise (designs 12a-real-e2e, 12a-dev-flow-harness and 12a-dev-flow-verify). One command in one terminal runs one attempt of the full development flow: preflight, a local plane with three sidecars and the workspace hub, a headless Claude Code coordinator launched by the harness itself with your real configuration, the five-role workflow with its two bounded revision loops, your generation-qualified sign-off, the final candidate's deterministic validation, evidence collection, the offline check and a dynamic audit. It prints PASS or FAIL.

`reale2e run` selects the checked full development flow and writes the versioned evidence bundle `callsheet-dev-full-evidence/v1`. Its exits are 0 for a mechanical PASS, 1 for an executed failure or incomplete evidence, and 2 for a usage, gate or schema error. `reale2e run-dev-full` is a documented deprecated alias of `reale2e run`: the same checked full flow, the same arguments and the same exits 0/1/2. It no longer exits 3, and new runs no longer write `dev-execution.json`. This is a deliberate compatibility change of the run command.

A passing run demonstrates this installation's recorded mechanics (selections, admissions, transitions, waits, publications, owner receipts and the reviewer's execution) and the final candidate's deterministic feature checks on this machine at that time, with your own agent configuration. Mechanical PASS never means that a model's design, diagnosis or review was semantically approved: the judgment rows stay pending your external review. It is not a vendor or timeout qualification, not a sandbox containment claim, and it never changes the support catalog. It never runs in CI: the command refuses to start when a `CI` variable is present (even empty), without `CALLSHEET_REAL_E2E=1`, when `CLAUDE_CONFIG_DIR` is set (even empty: unset it yourself; the harness never does), or on any OS but Linux.

Live status: pending. No owner-run qualification probe or full live attempt of this method has been recorded yet; until it is, this runbook documents a checked, offline-tested harness, not a demonstrated live PASS.

## Prerequisites

- Linux (macOS live execution is refused: the Grok and Cursor worker postures are Linux only. Cursor's non-Linux refusal is `cursor worker execution is not qualified on this OS; consult the support catalog`. No jail and no non-Linux Cursor qualification has shipped).
- Built binaries: `go build -o /abs/path/callsheet ./cmd/callsheet` and `go build -tags reale2e -o /abs/path/reale2e ./cmd/reale2e`, run from inside the checkout.
- Go and Git on `PATH` (development prerequisites of the throwaway Go project, not Callsheet runtime requirements). The reviewer's verification helper and the final validation use the `go` the harness resolved, with an isolated offline environment and fresh caches.
- Logged-in vendor CLIs at or above the runtime minimum versions, given by absolute path: Claude Code `2.1.299`, Codex `0.162.1`, Grok `1.0.52`, Cursor `2026.10.01-e373342` (the adapter auto-mode version policy, independent of the historical catalog identities; Cursor's date baseline does not overcome its non-Linux gate). The harness never logs in, reads credentials or rewrites profiles: resolve login, model access, a supported model or posture outside it. Worker tasks request each tool's auto mode. A Linux Cursor worker passes `--trust --auto-review`, builds the model argument as MODEL-EFFORT and puts the prompt in one positional argument (32 KiB, visible to process inspection). Passing `--trust` may cause Cursor to maintain `~/.cursor/cli-config.json` and other state outside the task directory; the harness reports that file and does not treat it as owner configuration. The harness does not edit vendor configuration or retry a denial. `--trust --auto-review` is not a confinement claim.
- Costs and deadlines (policy, not measurements): five paid authentication preflights of at most 2 minutes each; four paid workflow tasks on the happy path; the two bounded revision loops allow at most two extra design rounds, two extra fix rounds and two recovery dispatches, so at most fourteen workflow tasks and at most 19 worker calls including the five preflights, each workflow task at most 10 minutes from admission through publication; the headless coordinator adds vendor-dependent turns and tool calls in at most four phases; each owner decision window at most 20 minutes; the whole attempt within 90 minutes of preflight start (including the first fingerprint); the final candidate's validation shares one 60-second budget; then at most 2 minutes of cleanup. The theoretical task-time sum can exceed 90 minutes; the attempt deadline stops first. No automatic retries, no token-price ceiling and no currency estimate.
- Protected roots: each `--protect-root` directory (repeatable, absolute, existing, not nested in another, not inside the evidence or runtime directory) has every Git repository below it fingerprinted before and after the run. Discovery stops at each repository's working tree, whose status and tracked state (one streamed digest each) cover everything below it; a nested repository that its parent repository ignores is not separately protected. Discovery descends at most `--protect-depth N` directory levels below each root (default 3): deeper repositories and trees, such as a Go module cache, are not walked and not protected, and the report states the depth. A directory whose `.git` Git rejects is recorded by that entry's metadata, and any change to it fails. There is no default. With no `--protect-root`, only the four vendor homes are fingerprinted and the report says `No repository root was protected`.
- A quiescent owner environment: other Claude, Codex or Grok sessions writing configuration or repositories during the run make the comparison fail.
- The existing parent of a new evidence directory inside the checkout's ignored `design/` tree, for example `mkdir -p design/real-e2e-runs`. Evidence is bounded (64 MiB of text, 4 MiB of evidence repository) and stays local and private.

## Coordinator session

The coordinator runs with your user instructions, settings, permissions, skills, agents, plugins, hooks and login exactly as they are. The harness writes no agent configuration or settings file (the runtime MCP configuration and the evidence are its own files). Setting `CALLSHEET_REAL_E2E=1` is your authorization of session-only flags, identical in every coordinator phase:

- `--settings '{"sandbox":{"enabled":false}}'` merges over your settings for this session only. It is needed only because this exercise's plane listens on host loopback, which a sandboxed Bash command cannot reach (see [coordinator.md](coordinator.md)); it is not a general Callsheet requirement.
- `--permission-mode auto` requests Claude's automatic permission mode for this session only. Auto is available unless an administrator sets `permissions.disableAutoMode`. Supported models include Opus 4.6+ and Fable. The classifier pauses after 3 consecutive or 20 total blocks (owner-recorded, not a fresh upstream verification). The harness never edits that setting, never resets the classifier and never retries a denial. Exit 0 does not prove every requested action succeeded. Every init record of every phase must report `permissionMode` `auto`; the check never infers it from the launch flags.
- `--permission-prompts none` denies anything that would prompt. The permission mode still decides everything else. Print mode cannot answer a prompt.

There is no `--allowed-tools` list, no permission bypass, no second list and no tool filtering, and none of your rules or denies is removed: your inherited permissions stay active. Auto is a permission policy, not a containment claim. A bundle recorded with the earlier 15-rule `--allowed-tools` recipe is not evidence of this recipe: the checker rejects such a launch shape (`launch_shape_invalid`). Check a historical bundle with the revision that recorded it.

The exercise restricts the coordinator, by its prompt rather than by tool filtering, to exactly nine Callsheet MCP tools: `dispatch` (asynchronous, never with a wait), `task_ls`, `task_show`, `task_logs`, `task_cancel` (only this attempt's own dispatched tasks, after the attempt stopped), `ws_status`, `ws_diff`, `ws_push` (only the seed push, before the first dispatch) and `ws_pull`. `task_wait` and every registration or workspace administration tool are forbidden; an unqualified wake ends the phase instead of the playbook's `task_wait` fallback. The offline check judges every recorded Callsheet call: the tool policy, the server's own argument rules (whatever the answer), exactly one result per call, and each successful answer bound to its request and to the captured plane task records, prepared plans, seed publication and evidence objects. A violation fails coordinator setup with `mcp_call_invalid` (a `task_wait` use with `coordinator_long_poll`). A well-formed call answered with an error is no evidence and never satisfies a gate; workspace inspection calls are optional, and any that are made must hold.

The command prints these flags and prerequisites before any vendor runs. A `reale2e` or runtime path containing `*`, `?`, `[`, `]`, `(`, `)`, a comma, CR, LF or NUL, or any character that would need shell quoting (a space or a quote, for example), is refused before any vendor starts (`permission_rule_invalid`). No Bash line of the coordinator prompt contains a redirect, `;`, `&&`, `|` or a quote.

The coordinator's helpers run in its Bash, read and write only fixed files of the live runtime, follow no symlink and make one exchange with the supervisor's single loop:

- `reale2e dev-next --run D` names the next prepared dispatch document (`prepared PATH SHA256`), the gate that ends the phase (`paused gate=KIND generation=N`), `complete`, or `pending` while the previous result is still being admitted. It never dispatches and never names an admitted plan again.
- `reale2e dev-observe --run D --ordinal N --task ID` reports the task the coordinator dispatched for workflow ordinal N.
- Each task's background wait is exactly `R dev-wait-task --run D --task TASK_ID --ordinal N` (R the reale2e path): one simple command, run as the coordinator's harness-managed background job. Once the supervisor accepts that ID as ordinal N's only wait, it runs `callsheet task wait ID --until-done --json` itself and captures its output and exit under `D/coord/waits/NN`.

A denied tool call is never retried or widened: it fails the attempt, even when Claude exits 0. Resolve it in your own configuration and rerun.

Legacy-runtime compatibility helpers: `reale2e observe`, `reale2e wait-task` and `reale2e review-material` remain callable for legacy run files only (they refuse a development run file, and the development helpers refuse a legacy one). They are not this workflow's instructions.

## Roles, models and efforts

The shipped flow [examples/real-e2e/dev-full/flow.json](examples/real-e2e/dev-full/flow.json) is the default:

| ID | Sidecar / adapter | Model | Effort |
|---|---|---|---|
| designer | codex | gpt-6-astra | low |
| design-reviewer | claude | claude-fable-5-1 | low |
| coder | claude | claude-opus-5-5 | high |
| code-reviewer | codex | gpt-6.1-sol | high |
| triage | grok | grok-4.7 | xhigh |

Every role has concurrency 1 and timeout 10m. To override a model or effort, copy that `flow.json` to a local file, change only `model` or `effort`, and pass it with `--flow /abs/path/flow.json`; nothing changes after startup. `--flow` accepts only this five-role development schema (`callsheet-dev-full-flow/v1`); a legacy four-role flow is refused before any paid work. The report labels such a run `override-flow`, and its PASS never claims your default pairs passed. There is no automatic fallback.

The role method lives only in the shared development flow, [examples/dev-flow/README.md](examples/dev-flow/README.md), loaded as literal bytes: [coordinator playbook](examples/dev-flow/coordinator-playbook.md); [designer instruction](examples/dev-flow/designer-instruction.md) and [runbook](examples/dev-flow/designer-runbook.md); [design-reviewer instruction](examples/dev-flow/design-reviewer-instruction.md) and [runbook](examples/dev-flow/design-reviewer-runbook.md); [coder instruction](examples/dev-flow/coder-instruction.md) and [runbook](examples/dev-flow/coder-runbook.md); [code-reviewer instruction](examples/dev-flow/code-reviewer-instruction.md) and [runbook](examples/dev-flow/code-reviewer-runbook.md); [triage instruction](examples/dev-flow/triage-instruction.md) and [runbook](examples/dev-flow/triage-runbook.md). The exercise directory [examples/real-e2e/README.md](examples/real-e2e/README.md) adds only its mechanics: [coordinator prompt](examples/real-e2e/dev-full/coordinator-prompt.md), [phase prompt](examples/real-e2e/dev-full/phase.md), [scenario](examples/real-e2e/dev-full/scenario.md) and [flow](examples/real-e2e/dev-full/flow.json). The task envelopes add the fixed reviewer helper and evidence fields; no runbook is copied into them.

The feature asks for an optional `--name NAME` flag on a tiny greeting CLI, with ten fixed cases (`no_args`, `name`, `spaces`, `unicode`, `whitespace`, `empty_name`, `unknown_flag`, `missing_value`, `positional`, `help`), each a `TestRun/<case-id>` subtest, from a generated workspace-root `FEATURE.md`.

## Reviewer verification helper

Each code review runs the fixed helper `reale2e dev-review-verify --task ID --candidate COMMIT --initial-source COMMIT --go ABS --deadline YYYY-MM-DDTHH:MM:SSZ` exactly once from its task checkout; the envelope supplies every value but the actual task ID. The `--deadline` is RFC 3339 UTC at whole seconds with a `Z` suffix (the attempt deadline rounded down); any other spelling is refused. The helper is a fixture-specific test runner, not a reviewer: before running anything it refuses pre-existing scratch, a symlinked or wrong checkout, a missing commit, a modified input (an in-progress `review.md` aside) or an unexpected file; it exports the pinned candidate from the checkout's own objects into a fresh `.callsheet-review-scratch/<task-id>/after/`, runs `go test ./...`, `go test -coverprofile=<scratch>/coverage.out ./...` and `go test -json -count=1 ./...` there with the pinned Go in an isolated offline environment, derives the changed-code coverage, removes only its own scratch and verifies its absence and the unchanged inputs, and prints one JSON receipt of at most 64 KiB (exit 0 passed, 1 executed failure, 2 invalid invocation).

The reviewer still reads the design, code and tests and judges every FP. `review.md` names the receipt in one plain line `Reviewer run sha256: <64 lowercase hex>` (the SHA-256 of the helper's complete JSON output, including its final LF), besides its reviewed candidate, source hashes, both verification commands with their exits, the coverage and the cleanup. The supervisor corroborates the receipt only from the reviewer's complete, untruncated task log: exactly one top-level command record of exactly that command. An approval needs a complete successful receipt; a failed receipt can support changes requested; prose alone never proves execution.

## Run

```sh
CALLSHEET_REAL_E2E=1 /abs/path/reale2e run \
  --callsheet /abs/path/callsheet \
  --claude /abs/path/claude --codex /abs/path/codex --grok /abs/path/grok \
  --protect-root /opt/gitspace --protect-root /opt/gospace \
  --evidence /abs/callsheet/design/real-e2e-runs/NEW-ATTEMPT
```

The two `--protect-root` values are an example (the owner's); substitute your own roots or omit both flags for vendor-home-only protection.

## Qualification probe and owner authorization

Qualify the installation first with the owner-only probe, then authorize the full attempt after reading its results. The probe uses the same gate, flags, bounds, fingerprinting and cleanup, prints QUALIFIED or BLOCKED (never a workflow PASS) and exits 0 qualified, 1 blocked, 2 usage or gate error. It runs three sequential stages in one bounded invocation, stopping at the first failure: the five model registrations and authentication preflights; three coordinator phases resuming the same session with one harmless no-workspace designer wake task each and two owner continuation windows (`continue 1` and `continue 2`, or `no 1` / `no 2` to stop); and, in the third phase, the print-stream notification evidence plus one code-reviewer task on a harness-owned known-good fixture that must invoke the verification helper and publish only `review.md`. At most nine worker calls and three coordinator phases.

```sh
CALLSHEET_REAL_E2E=1 /abs/reale2e probe-dev-full --callsheet /abs/callsheet --claude /abs/claude --codex /abs/codex --grok /abs/grok --protect-root /abs/projects --evidence /abs/callsheet/design/real-e2e-runs/NEW-PROBE
CALLSHEET_REAL_E2E=1 /abs/reale2e run --callsheet /abs/callsheet --claude /abs/claude --codex /abs/codex --grok /abs/grok --protect-root /abs/projects --evidence /abs/callsheet/design/real-e2e-runs/NEW-FULL
/abs/reale2e check --evidence /abs/callsheet/design/real-e2e-runs/NEW-FULL
```

Authorize the probe's cost first, then the full attempt after seeing the qualification results and the total disclosure: up to 28 worker calls across one complete probe plus one full attempt, coordinator phases accounted separately. Record the probe reference with the live-run authorization; the full run still performs its own five authentication probes. A changed relevant version, configuration or selection needs a fresh qualification. Approving a design or passing the offline tests authorizes no paid call, and a qualification is not a permanent vendor claim.

## Sign-off

After an approving design audit and the coordinator phase's exit, the same terminal shows the full approved `design.md`, `revision-history.md` and `design-review.md` with their paths and SHA-256 digests, then asks, with N the actual design generation:

```
Approve design generation N and start coding? Type exactly "yes N" or "no N".
```

Answer `yes <generation>` or `no <generation>`, exactly. A plain `yes` decides nothing, and neither does a line naming another generation (the window's deadline is unchanged); input typed while no window is open is discarded. A yes records a generation-qualified receipt and resumes the same coordinator session for coding and the code review. A later design revision invalidates the receipt at the designer's dispatch, even when its bytes are unchanged, and needs a fresh audit and a fresh decision. No, end of input or the 20-minute window ending (never past the attempt deadline) ends the attempt without coding. A designer's clarification question is answered with `answer <generation> <text>` (one line, at most 8192 bytes, no secrets).

## Reading the result

The final output prints PASS or FAIL for the mechanical and deterministic criteria and, separately, `Result judgments: needs owner/coordinator review`, then links `report.txt` and `audit.md` in the evidence directory.

- `report.txt` lists every criterion (manifest, provenance, roles, preflight, coordinator setup, admissions, transitions, waits, publications, owner receipts, reviewer execution, baseline, final tests, coverage, feature oracle, cleanup, owner environment, deadlines, cost) with its fixed failure codes, or, when the attempt verifiably stopped before a criterion's step, `not_reached` with the stopping cause; the run still fails. It lists the protected roots, the protected changed paths (any one fails the run) and the tool-maintained changes (report-only), counted as added, changed and deleted per vendor and top-level directory (`.` for files directly in the vendor home).
- `audit.md` (and `audit.json`) walks every actual step in order with stable row IDs: the setup, each preflight and phase, every task's dispatch, wait, publication, marker and (for a code review) reviewer execution, every transition, decision and receipt, the validation and each CLI case, cleanup, the environment comparison and the accounting, each `correct`, `incorrect` or `unobserved` with evidence pointers; the next blocked step of a stopped attempt is `unobserved` with its cause, and nothing is invented for loops that did not happen. A separate judgment layer (each design's completeness, each audit's quality, each implementation's assertions, each review's quality, each triage's causal evidence) stays `unobserved` until reviewed.
- `cost.json` lists every admitted worker call, failed ones included, every phase, the owner windows, the attempt and the cleanup. Provider usage is `unknown` unless provider-reported; nothing is estimated.

Only your repositories and this exact owner-configuration list are protected; any change to them, a removal, a replacement by another file type or link, or an addition fails the run (`owner_environment_modified`), and listed directories cover every descendant:

| Vendor home | Owner configuration |
|---|---|
| `~/.claude` | `CLAUDE.md`, root `settings*.json`, `agents/`, `skills/`, `commands/`, `hooks/`, `plugins/installed_plugins.json`, `plugins/blocklist.json`, `plugins/config.json` |
| `~/.codex` | root `AGENTS*.md`, `instructions.md`, `prompts/` |
| `~/.grok` | `config.toml`, `managed_config.toml`, `settings.json`, `user-settings.json` |
| `~/.cursor` | `mcp.json` |

An owner-configuration entry changes only when its content, type, mode or link target differs: a tool rewriting a listed file with identical bytes (a new inode and timestamps) is no change, while a same-size content change still fails, because these files are content-hashed. A listed file over 1 MiB is fingerprinted by metadata only, so its size and modification time stand in for its content. The vendor homes must stay the same real directories (a home may be created during the run, not removed or replaced), and the `plugins/` directory must stay a real directory; size and timestamp changes caused by their children are fine. Everything else in a vendor home is tool-maintained, whatever its name or type: session transcripts, history, logs, caches, state databases, token refreshes and Cursor files other than `mcp.json`. `~/.cursor/cli-config.json` is one of those: Cursor maintains it, and the harness reports it without failing the run. `~/.cursor/mcp.json` stays owner configuration. A first normal run is expected to list such changes; they are reported as counts only and never fail the run. Their per-entry metadata (no contents) is detailed in `environment/diff.json` up to its 4 MiB bound, and the report and audit state how many entries were omitted beyond it. These are accepted coverage exclusions, shown together in `report.txt` on passing and failed runs alike:

```
not covered: ~/.claude.json
not covered: ~/.claude/.config.json (Claude state file; holds per-project tool grants, MCP-server lists and trust flags)
not covered: ~/.claude/backups/.config.json.backup.* (backups of Claude state, including per-project grants)
not covered: ~/.codex/config.toml (Codex records a trust entry per working folder)
not covered: untracked .claude/ directories inside protected repositories
not covered: ~/.claude/plugins/known_marketplaces.json (Claude refreshes marketplace timestamps)
not covered: ~/.cursor/cli-config.json (Cursor maintains this file; report-only)
```

Claude's state files hold per-project tool grants and MCP-server selections, but a change to them does not fail the run. Codex appends a `[projects."<dir>"] trust_level` entry to `~/.codex/config.toml` for every directory it works in, so the whole file is tool-maintained like Claude's `.config.json`; your `AGENTS*.md`, `instructions.md` and `prompts/` stay strict. Each protected repository's status digest leaves out its untracked top-level `.claude/` directory, which may hold Claude Code sandbox placeholders; its tracked content and HEAD stay strict. Claude refreshes the `lastUpdated` timestamps in `~/.claude/plugins/known_marketplaces.json` whenever it uses its plugin marketplaces, so that file is tool-maintained; the other plugin configuration files (`installed_plugins.json`, `blocklist.json`, `config.json`) stay strict. In the development full-flow attempt and its qualification probe, Claude Code's synced-skills bookkeeping is tool-maintained too (owner decision, 2026-10-22): exactly `skills/synced/<id>/.last-complete-round`, `skills/synced/<id>/.staging` with everything below it, and the `skills/synced/.bucket-*` entries directly under `skills/synced/`, fingerprinted by metadata only and reported without failing the run. Synced skill content, `skills/synced` and its account directories, and every other `skills/` path stay strict. A symlink in a vendor home or in the directories scanned above your repositories is checked as the link itself (where it points, not what it points at) and never followed. The harness never repairs, moves, restores or deletes any of your files or vendor state.

Right after the run, review `audit.md` with your external dev-flow coordinator: it checks every judgment row and any disputed mechanical verdict and appends a separate `audit-review.md` (run ID, report and both audit hashes, reviewer identity and date, every applicable judgment row with its verdict and reasons, artifact commit, hash and line references, and any disputed mechanical row). Never edit `audit.json`, `audit.md`, the report or the evidence; a correction cannot turn a FAIL into a PASS. `reale2e check --evidence DIR` re-validates a bundle offline: it routes a `callsheet-dev-full-evidence/v1` bundle to the full-flow checker and a legacy `callsheet-real-e2e/v3` bundle to the unchanged legacy checker, whose PASS keeps its original legacy scope. An execution-only `dev-execution.json` bundle, a qualification record, an earlier legacy version or an unknown schema is refused with exit 2; old evidence is never migrated.

## Rerun

Resolve the reported cause (login, model access, a permission or configuration incompatibility, a protected change), then run the same command with a new evidence directory. Every attempt has its own run ID, runtime, workspace and coordinator session; nothing is resumed or retried, and earlier bundles keep their result. A later attempt after a failure needs fresh owner authorization. If cleanup could not prove every process stopped, the runtime is kept and the supervisor prints the process IDs to check and stop before rerunning.

## Evidence bundle

The bundle's `manifest.json` carries the schema `callsheet-dev-full-evidence/v1`, the run's provenance, the frozen selections, the attempt window, the harness executable's SHA-256 before and after, the stop and the final candidate and review, and the inventory of every primary file and stored Git object. Every structured record is a strict schema-bearing envelope; the layout is closed (every plan, dispatch document, task view, log, observation, admission, record, wait, review evidence, transition, decision, receipt, phase stream, launch record, the admissions sweeps, the cleanup and cost records, the validation records and an evidence repository with `refs/heads/seed` and `refs/heads/task-NN`). The checker replays the policy from the seed, rebuilds every envelope, re-admits every publication, re-derives every session fact and reviewer receipt and the final oracle, and compares the stored report and audit byte for byte. Checksums provide consistency, not signatures: the checker does not claim cryptographic proof of a paid run.
