# Real local end-to-end exercise (first-draft runbook)

This runbook is the single entry point for Callsheet's manual real-worker exercise (design 12a-real-e2e): a real Claude Code coordinator session, set up only from this runbook, drives the example development flow through a local plane, three sidecars, the MCP server and the workspace hub with real workers, pausing for your real sign-off between design approval and coding. A passing run demonstrates this installation and this workflow on this machine at that time. It is not a general vendor qualification, a timeout qualification or a sandbox containment claim, and it never changes the support catalog.

It is manual and local only. It never runs in CI: the command refuses to start when a `CI` variable is present (even empty), without `CALLSHEET_REAL_E2E=1`, or on any OS but Linux. Evidence stays local under the checkout's ignored `design/` tree; the repository carries only the harness, the checker and this runbook.

## Costs and bounds

Bounds are policy, not measured performance:

- four paid authentication preflight tasks (one per role/pair), each at most 2 minutes (the supervisor cancels a longer one through the plane);
- four paid feature tasks (designer, design reviewer, coder, code reviewer), each at most 10 minutes from admission through publication;
- your decision at most 20 minutes; the whole attempt at most 90 minutes from preflight start, then at most 2 minutes of cleanup; the final local tests at most 60 seconds;
- the interactive Claude Code coordinator adds vendor-dependent turns and tool calls (no fixed model-call count is knowable);
- no automatic retries or fix loops; no token-price ceiling is claimed.

## Prerequisites

- Linux (macOS live execution is refused: the Grok worker posture is Linux only).
- The Callsheet checkout, built: `go build -o /abs/path/callsheet ./cmd/callsheet` and `go build -tags reale2e -o /abs/path/reale2e ./cmd/reale2e`.
- Logged-in vendor CLIs at or above the minimum versions, given by absolute path: Claude Code `2.1.285 (Claude Code)`, Codex `codex-cli 0.159.0`, Grok `grok 1.0.46 (...) [stable]` (the existing adapter version policy; newer versions are accepted silently). The harness never logs in, inspects credentials or rewrites profiles: resolve login, model access or posture outside it.
- Go and Git on `PATH` (development prerequisites of the throwaway Go project, not Callsheet runtime requirements).
- The parent of a new evidence directory inside the checkout's ignored `design/` tree: `mkdir -p design/real-e2e-runs`. Each attempt names its own new bundle directory there (it must not exist yet), for example `design/real-e2e-runs/<date>`.

## Roles, models and efforts

The shipped flow [examples/real-e2e/flow.json](examples/real-e2e/flow.json) is the default:

| ID / hop | Sidecar / adapter | Model | Effort |
|---|---|---|---|
| designer | codex | gpt-6-astra | low |
| design-reviewer | claude | claude-fable-5-1 | low |
| coder | claude | claude-opus-5-5 | high |
| code-reviewer | grok | grok-4.7 | high |

Every role has concurrency 1 and timeout 10m. To override a model or effort, copy `flow.json` to a local file, edit only `model`/`effort`, and pass it with `--flow /abs/path/flow.json` at startup. Roles, order and adapters are fixed; nothing changes after startup. The report labels such a run `override-flow`, and its PASS never claims your default pairs passed.

Worker manuals (instruction and runbook per role):

- designer: [instruction](examples/real-e2e/designer-instruction.md), [runbook](examples/real-e2e/designer-runbook.md)
- design-reviewer: [instruction](examples/real-e2e/design-reviewer-instruction.md), [runbook](examples/real-e2e/design-reviewer-runbook.md)
- coder: [instruction](examples/real-e2e/coder-instruction.md), [runbook](examples/real-e2e/coder-runbook.md)
- code-reviewer: [instruction](examples/real-e2e/code-reviewer-instruction.md), [runbook](examples/real-e2e/code-reviewer-runbook.md)

The coordinator's prompt is [examples/real-e2e/coordinator-prompt.md](examples/real-e2e/coordinator-prompt.md); the supervisor renders it with the run's concrete values (placeholders only) and records the hashes of every source template and rendered file.

## The feature

The seed is a dependency-free Go module whose program prints `Hello, world!` and a newline, with baseline tests, plus `FEATURE.md`: add an optional `--name NAME` flag. With no flag print `Hello, world!\n`; with `--name Ada` print `Hello, Ada!\n`; preserve spaces and Unicode in a nonempty name; reject an empty name, unknown flags, a missing value and positional arguments with exit 2, no stdout and a nonempty stderr diagnostic; `--help` prints usage with exit 0; a testable `run(args []string, stdout, stderr io.Writer) int` with table tests; the whole project under 16 KiB.

Each hop builds on the previous hop's published result commit, on one pinned workspace instance:

1. designer writes `design.md` only, ending `STATUS: DESIGN_DRAFT`;
2. design-reviewer writes `design-review.md` only, ending `STATUS: DESIGN_REVIEW_APPROVED` (or `STATUS: DESIGN_CHANGES_REQUESTED`, which ends the attempt);
3. after your recorded yes, the coder changes only `main.go` and `main_test.go`, ending `STATUS: IMPL_COMPLETE`;
4. the Grok code reviewer gets `design.md`, `main.go` and `main_test.go` inline in its goal (Grok runs dontAsk and cannot write files), writes nothing and returns its review solely as its final message, ending `STATUS: REVIEW_APPROVED`. Callsheet still publishes a fourth result commit with the coder's unchanged tree; the review is that task's final message (`task_show`), stored as `hops/04/final.txt`.

## Running it

1. Start the supervisor in a terminal you keep open (it is the decision terminal):

   ```sh
   cd /abs/path/to/callsheet-checkout
   CALLSHEET_REAL_E2E=1 /abs/path/reale2e run \
     --callsheet /abs/path/callsheet \
     --claude /abs/path/claude --codex /abs/path/codex --grok /abs/path/grok \
     --evidence "$PWD/design/real-e2e-runs/$(date -u +%Y%m%dT%H%M%SZ)" [--flow /abs/path/flow.json]
   ```

   It prints the bounds, checks versions and the private temporary directory, creates a short private runtime `/tmp/ce-*` (mode 0700), starts the plane on 127.0.0.1 with an ephemeral port, three sidecars and the four roles, reconnects every sidecar and awaits the roles ready, runs the four authenticated preflights, creates the seed and its baseline tests and a fresh workspace, and prints the coordinator launch command, the run ID and the runtime path. `--evidence` is the bundle directory itself: it must not exist yet (a reused path is refused before anything starts) and its existing parent must lie inside the checkout's `design/` tree; the run creates it and records the generated run ID in its `manifest.json`.

2. Prepare a clean coordinator session. Inspect the active instruction sources (user and project `CLAUDE.md`, skills, plugins, hooks, agents) and disable unrelated ones for this session with supported vendor controls. If a clean session cannot be established without weakening security, abort. Keep your normal authentication and sandbox posture: do not use `--bare` (it changes OAuth and keychain behavior) or any broad permission bypass.

3. In a new terminal, run exactly the printed command. It starts a fresh session in the new empty directory `<runtime>/coord`, never resumed:

   ```sh
   cd <runtime>/coord && /abs/path/claude --session-id <uuid> --mcp-config <runtime>/mcp.json --strict-mcp-config \
     --append-system-prompt "$(cat <runtime>/coordinator-prompt.md)"
   ```

   The session-local MCP configuration starts `callsheet mcp --plane URL --ca PATH --wait-call-budget 10s` (see [coordinator.md](coordinator.md)). Tell the coordinator to begin (for example "start the run").

4. The coordinator pushes the seed (`ws_push`), dispatches each hop without a wait, reports it with `reale2e observe --run <runtime> --task ID --hop NAME`, and waits with `callsheet task wait ID --until-done --json` in Claude Code's Bash `run_in_background: true` facility, ending its turn until the harness wakes it. `observe` answers `accepted` or `already_observed` (exit 0), a refusal code (exit 1: `invalid_request`, `run_mismatch`, `invalid_hop`, `task_mismatch`, `observation_conflict`, `owner_gate_closed`, `closing`, `internal_error`) or a local error (exit 2). It can never approve a design.

## Your decision

After an approved design review, the supervisor terminal prints the reviewed `design.md` and `design-review.md` paths and SHA-256 digests and waits up to 20 minutes. Read them, then type exactly `yes <run-id>` or `no <run-id>` there. EOF, timeout, blank input and anything else are not approval; a recorded decision cannot change. A yes creates the read-only receipt `<runtime>/owner-receipt.json`; then send the coordinator the message `continue after recorded decision` (that message alone approves nothing; the coordinator checks the receipt). A no records the rejection and stops with no coding task. Any coding admission without the receipt is cancelled and fails the attempt. This is a cooperative workflow gate, not an authorization boundary of Callsheet's shared trusted-network API.

## Finishing and evidence

When the coordinator reports the finished chain (and the supervisor printed that the final tests passed), close the Claude Code session, make sure its background waits and the MCP child have ended, and place these files in `<runtime>/owner/`:

- `transcript.jsonl`: the session transcript exported with the installed Claude Code version's supported mechanism (for example the session's JSONL file named after its session ID). It stays private and local.
- `setup-attestation.json`: your attestation of the clean setup:

  ```json
  {"schema":"callsheet-real-e2e-setup-attestation/v1","run_id":"<run-id>","session_id":"<uuid>","claude_version":"<claude --version>",
   "fresh_session":true,"resumed":false,"empty_coordinator_dir":true,"no_prior_conversation":true,"no_extra_prompt":true,
   "no_unrelated_instructions":true,"instruction_sources_checked":true,"setup_inputs":["runbook","coordinator-prompt","mcp-config"],
   "transcript_export":"<how you exported it>"}
  ```

- `event-map.json`: your reviewed map of transcript lines (1-based inclusive spans of JSONL records) to the session launch, each hop's dispatch, background wait start (`"background":true`, handle `hop-01`..`hop-04`) and its consumption (`"automatic_wake":true,"user_message_between":false` where the harness woke the session), the owner-gate pause and resume, and the final result, in that order; a recovery `task_wait` is an `mcp_wait` entry with `"recovery":true` and its budget (at most 10000 ms). Every event states every field (`kind`, `hop`, `task`, `handle`, `background`, `automatic_wake`, `user_message_between`, `budget_ms`, `recovery`, `lines`), with `""`, `false` or `0` where it does not apply: an omitted field is refused, never read as "none":

  ```json
  {"schema":"callsheet-real-e2e-event-map/v1","run_id":"<run-id>","session_id":"<uuid>","transcript_sha256":"<sha256 of transcript.jsonl>",
   "attested_by_owner":true,"events":[
   {"kind":"session_launch","hop":"","task":"","handle":"","background":false,"automatic_wake":false,"user_message_between":false,"budget_ms":0,"recovery":false,"lines":[1,1]},
   {"kind":"dispatch","hop":"designer","task":"t_...","handle":"","background":false,"automatic_wake":false,"user_message_between":false,"budget_ms":0,"recovery":false,"lines":[10,11]},
   {"kind":"wait_start","hop":"designer","task":"t_...","handle":"hop-01","background":true,"automatic_wake":false,"user_message_between":false,"budget_ms":0,"recovery":false,"lines":[12,12]},
   {"kind":"wait_consumed","hop":"designer","task":"t_...","handle":"hop-01","background":false,"automatic_wake":true,"user_message_between":false,"budget_ms":0,"recovery":false,"lines":[20,21]}, "..."]}
  ```

- `coordinator-exit.json`: the owner-started processes' exits, one wait handle per hop that was dispatched (every field required; an exit you did not observe is a failure, never a 0). Without it the supervisor does not consider cleanup complete: it keeps the runtime and prints what to stop:

  ```json
  {"schema":"callsheet-real-e2e-coordinator-exit/v1","session_closed":true,"mcp_child_exited":true,
   "wait_handles":[{"handle":"hop-01","exit_code":0},{"handle":"hop-02","exit_code":0},{"handle":"hop-03","exit_code":0},{"handle":"hop-04","exit_code":0}]}
  ```

Then run `reale2e finish --run <runtime>`. It acknowledges collection and stop (not completion or PASS). The supervisor collects the evidence, stops the sidecars and the plane by their captured process groups (SIGTERM to the group, then a bounded SIGKILL, until the leader is reaped and no process of the group remains, also when the leader exited first), judges the bundle with the offline checker, writes `report.json` and `report.txt`, prints the result and exits 0 only for PASS. The event map's truth is your trusted attestation, not cryptographic proof of hidden vendor context.

The bundle (version 1): `manifest.json`, `setup/` (rendered prompt, flow, sanitized MCP config, launch record, roles snapshot, manuals), `preflight/01..04/`, `session/` (private transcript, event map, setup attestation), `hops/01..04/` (task, final answer, bounded logs, dispatch and wait records), `owner-decision.json`, `events.jsonl`, `workspace/repo.git/` (a minimal bare repository with the seed and the four results), `workspace/tree-manifest.json`, `validation/` (baseline and final `go test -count=1 ./...` runs with `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`, `GOWORK=off`), `cleanup.json`, `report.json`, `report.txt`.

Re-check any bundle offline (read-only, no model call, runs nothing named in evidence):

```sh
/abs/path/reale2e check --evidence "$PWD/design/real-e2e-runs/<date>"
```

It prints the JSON report and exits 0 for PASS, 1 for failed or incomplete evidence, 2 for schema or usage errors. Every missing or negative criterion is a hard failure: the gate, versions and four authenticated preflights; the exact role/pair snapshot; the clean setup attestation, template digests, transcript and valid event-map spans; exactly four unique succeeded feature tasks with their selections, markers, complete final answers and publications; the pinned instance, seed, base bindings, single parents, allowed changes per hop, required artifacts, immutable approved design and the unchanged Grok tree; the approved design review before your yes, the decision bound to that commit and digests, coding only after it, no other admission; background waits with at least one automatic wake and no long blocking MCP call; the Grok review's approval and its inline files equal to the coder's bytes; the final tests on the fourth result; verified cleanup; every deadline. The shareable `report.json` holds only structured fields with `$HOME`, `$RUN` and `$CHECKOUT` tokens and fixed codes, never logs, prompts, transcript or final prose. Nothing is published or uploaded automatically.

## Failure, rerun and cleanup

On any failure the supervisor stops further work, records partial evidence, cancels active tasks through the plane and awaits their cleanup, then stops what it started. Every bound is checked before a result, a decision or a next hop is accepted: a success that lands late, a yes after the 20-minute decision window or a dispatch after the attempt's bound is refused and its task cancelled. The runtime is removed only after every child's process group was proved gone, every cancelled task settled and your `coordinator-exit.json` accounts for the session, its MCP child and its waits (ownership marker and canonical path checked, never through a symlink); otherwise it is kept and the supervisor prints the unverified process IDs, tasks and owner-started processes to stop. The evidence always survives.

There is no resume and no reuse of vendor sessions. After a rejected design, a no, a failed hop, a supervisor crash or the overall timeout, start a new attempt: a new run ID, runtime, workspace and coordinator session from the seed. The earlier bundle keeps its FAIL or incomplete status. A coordinator that lost the supervisor may reconnect only to collect or stop; a passing run needs the original fresh session's continuous evidence.

## Limitations

- Linux only for live runs (Grok worker execution is not qualified elsewhere); the checker runs on Linux and macOS.
- Cursor is not a worker here (slice 3).
- The run depends on no long blocking MCP call: `task_wait` stays a 10s short poll; MCP timeout compatibility remains unverified.
- Generated code runs under your existing vendor and OS posture; containing malicious generated code, hostile same-user processes, tampered evidence, hidden vendor prompts and telemetry is outside this exercise. Grok's goal is visible in its argv: use only this public throwaway project.
