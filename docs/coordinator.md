# Coordinator setup and MCP timeout qualification

This guide connects the four first-wave coordinator CLIs to Callsheet's MCP server (`callsheet mcp`, iteration 07a) and explains how an owner measures their MCP tool-call timeouts locally (iteration 07b). Every fact below is qualified by the [support catalog](support-catalog.md) and its machine-readable snapshot [`tests/testdata/support-catalog.json`](../tests/testdata/support-catalog.json). This document does not claim that any vendor runtime configuration was tested: `VERIFIED` means backed by the named captured help/version evidence, and `UNVERIFIED` means not established. Callsheet's own side of every configuration is tested natively on Linux and macOS; each vendor's side needs local qualification.

## What every configuration shares

- `callsheet mcp` is a stdio MCP server that the coordinator CLI starts from its own MCP configuration. Examples use the placeholders `<callsheet-binary>` (the absolute path of the `callsheet` executable), `<plane-url>` (the shared plane URL, for example `https://plane.example:7443`), `<ca-path>` (the absolute path of the plane's CA certificate file) and `<runbook>` (your own coordinator runbook file). No CA key is bundled; install the CA certificate the plane operator gives you.
- Install the `callsheet` binary for Linux (`callsheet-linux-amd64` or `callsheet-linux-arm64`) or macOS (`callsheet-darwin-amd64` or `callsheet-darwin-arm64`) from the release artifacts, make it executable and use its absolute path. Install the vendor CLI with its vendor's own instructions for Linux or macOS; the captured versions below are the ones the catalog describes, not the latest release.
- `--ca-fingerprint <sha256>` can replace `--ca <ca-path>`; never pass both. When the server cannot reach the plane, check the wrong URL or certificate name (SAN), the CA file and the clock first.
- User configuration stays user-owned. Callsheet never registers itself and never modifies a vendor's configuration; you run the registration command or edit the vendor-owned file yourself. Vendor permission approval may be required; do not turn on broad approval or disable sandboxing to make a tool call pass.
- Runbook loading is the user's launcher reading the user's file, never Callsheet reading it. The shell examples pass the file's text on the vendor side (initial prompt, appended system prompt or rules); no runbook flag or path appears in the Callsheet command and no runbook bytes are sent to Callsheet. Initial-prompt loading is the established fallback for Codex and Cursor; it is not a claim about automatic `AGENTS.md` precedence or a system-level instruction. Shell substitution strips trailing newlines and is subject to argument-length limits; for a large manual use a vendor-supported file mechanism once it is qualified, and state its status. Unverified file-discovery mechanisms are not established behaviour.
- The local transfer tools `ws_push` and `ws_pull` (and the `callsheet ws push` / `ws pull` commands) read and write local files on the machine running `callsheet mcp`, resolving a relative path against that server process's working directory, not the coordinator's. Their commands, supported repositories, ignore rules, results and refusals are in [workspaces.md](workspaces.md).
- Workspace tasks (iteration 10b, protocol 6; CLI and MCP since iteration 10c) are selected by the `dispatch` tool's optional `workspace`, `base` and `workspace_instance` arguments, `callsheet dispatch --workspace NAME [--base SELECTOR] [--workspace-instance TOKEN]`, or the HTTP dispatch request's fields of the same names. A task's result carries its `workspace` publication object: a failed publication never changes the task's state, so check `result.workspace.publication` before handing a result to the next task. To continue, dispatch the next task with `workspace` and `workspace_instance` from the first task's `workspace_binding` and `base` set to its `result.workspace.commit`, for example `{"target":{"kind":"name","value":"reviewer"},"goal":"review the change","acceptance":"findings listed","workspace":"myproject","base":"<commit>","workspace_instance":"<instance>"}`. Inspect and deliver a result by task ID: `ws_status {"task_id":"t_..."}` (its `available` flag), `ws_diff {"task_id":"t_..."}` (paged metadata against the admitted base) and `ws_pull {"task_id":"t_...","path":"..."}` (CLI: `callsheet ws status|diff|pull TASK_ID`). See [Workspace tasks](workspaces.md#workspace-tasks), [Task results by task ID](workspaces.md#task-results-by-task-id) and the [coordinator workflow](workspaces.md#coordinator-workflow).

## Waiting-call budget

`callsheet mcp --wait-call-budget <duration>` sets B, the outer budget of one `task_wait` or dispatch-with-wait tool call from admission through delivery of its answer: 1s to 5m, default 10s (for example `--wait-call-budget 10s`). The plane wait a call asks for is shorter (response and transport reserves, plus an admission allowance for a dispatch), and the plane clamps it again with its own `--max-task-wait`; each answer reports the wait actually used as `effective_wait_ms`. The waiting tools say, verbatim:

> The budget includes trust setup and response delivery. An incomplete reply ends the session at the budget deadline unless its final write is in progress; then a reply not fully written before the deadline ends the session once it is written, or the 1 s writer progress watchdog ends it first, and a short final write returning at or after the deadline also ends the session.

> Default 10s call budget is a short poll, not a verified coordinator timeout. For long tasks, run callsheet task wait --until-done in a harness-managed background command; without wake support, poll task_wait on the next turn. Raising the budget requires local timeout qualification.

The 10s default is the catalog's [short-poll policy](support-catalog.md#interim-mcp-wait-exception): `task_wait` is a compact short poll and dispatch's optional wait the short-task fast path, never a multi-hour coordinator wait. A long task is followed by the background CLI wait of the [coordinator runbook example](#coordinator-runbook-example), which never blocks a model turn. Choose a larger B only from a named verified evidence set: after qualification, B + max(2s, 10% of T) < T, where T is the smallest verified effective silent tool-call timeout across every intended client, version, platform and configuration sharing that server configuration. Use conservative measured lower bounds, never the midpoint of an interval, and include any measured absolute maximum. Keep B at most 5m and keep the plane's clamp. If any intended client is unqualified, keep the shared B at 10s. Never raise B on progress evidence alone. Raising a vendor's own timeout does not change Callsheet's B or the plane clamp. A verified fact names its exact OS, architecture, version and settings; Linux observations never certify macOS. Until such evidence exists this guide prints no supposedly safe larger value.

For each vendor below, the timeout rows are `UNVERIFIED; no supported override is established—run local qualification`, with candidate settings named as candidates only.

## Coordinator runbook example

Dispatch never blocks the coordinator: it is asynchronous unless you ask for a short wait. For a task that may run for minutes or hours, the coordinator starts one quiet, unbounded CLI wait as a **background command of its own harness** (the coordinator CLI's background shell facility), keeps working or ends its turn, and resumes when the harness reports that the command finished. This section is a copyable example of that pattern for your own coordinator runbook. It is user-owned text that you load through your vendor's runbook mechanism above; Callsheet never reads, stores or registers a runbook, and there is no Callsheet coordinator role or daemon.

The wait is `callsheet task wait ID [ID ...] --until-done --json --plane <plane-url> --ca <ca-path>`:

- It waits, with no overall deadline, for the first of up to 16 tasks to end durably, and prints nothing until then: no banner, progress or retry notice on stdout or stderr. It then prints exactly one compact terminal wait JSON value and LF (without `--json`, the `task show` rendering after a first line matching `^winner: t_[0-9a-f]{32}$`) and exits 0, whatever the task's own result. It never prints a still-running answer; `effective_wait_ms` describes the final plane request only.
- It asks the plane for 30s at a time (each request bounded by 40s and the plane's `--max-task-wait`) and renews at once, at most ten requests a second, so an idle wait costs about two requests a minute. Plane restarts, refused or dropped connections, a full wait capacity and HTTP 503 are retried silently after 250ms, 500ms, 1s, 2s, 4s, then every 5s, with trust resolved once and kept.
- **A plane gone for good looks like a long outage: the command then waits, silently, until it is stopped.** That is the contract, not a hang to work around; no task is cancelled, redispatched or moved.
- Permanent errors end it with one diagnostic on stderr and no stdout: an invalid ID, count, duplicate or conflicting flag, or an invalid answer exits 2; any unknown ID 3 (the whole list fails, also when the other IDs are known: diagnose it, never drop an ID silently); a conflict 4; a trust failure 6; a protocol mismatch 7; not implemented 8; anything else 1. SIGINT or SIGTERM exits 130 with `callsheet: interrupted`, on Linux and macOS alike; no task is stopped. SIGKILL prints nothing, and its killed status is the harness's to report: never infer a result from a killed wait.
- The winner is the plane's: a task already durably terminal wins at once (the first in your order when several are); after a connection loss, the first already-terminal ID in your order may win. A failed workspace publication is a property of the returned task to inspect, not a different exit.

Runbook sequence:

1. Dispatch with no wait, or with a deliberately short MCP or CLI wait as the fast path. Record each admitted task ID and handle a fast-path winner at once. Never redispatch after a lost dispatch answer: inspect `task_ls` first.
2. In a persistent session whose harness can wake you, start one foreground `callsheet task wait ... --until-done --json` inside the harness's background command facility, with no timeout of your own. One wait holds all outstanding IDs, up to 16; for more, use stable batches of at most 16 with one wait each (coalesce when you can). A newly admitted ID means stopping and reaping the old wait and re-arming the updated set; the tasks themselves keep running.
3. Continue independent work. When none is left, end your turn. Do not keep the turn open with a foreground sleep, a polling loop, a terminal read loop, `setsid`, `nohup`, `&` or `disown`: the harness owns the command and its completion notification.
4. When woken, read the finished command's exit status and captured output. On exit 0, decode the terminal wait answer, inspect the task's state, result and workspace publication, remove that winner from your outstanding IDs and act on it. Several tasks that ended together are collected by immediate re-arms. Output and exit notifications may both arrive: handle them idempotently by wait handle and winner task ID, never twice.
5. Re-arm for the remaining IDs, in their original order; never re-arm an empty list. `task ls --json` is shared, paginated and not filtered by coordinator, so it is a recovery aid only; `task show --json ID` can inspect a known ID but is not needed before re-arming.
6. A nonzero exit is an error path, never a task success: keep the outstanding IDs and diagnose the permanent error. If the harness stopped the watcher (its own lifetime limit or your stop), re-arm on your next actual turn. Stopping a watcher never cancels a task.

For a harness that wakes on every stdout line, run this script as the background command so it prints one line after the wait ends. `$id1`, `$id2`, `$plane`, `$ca`, `$handle` and `$waitdir` are concrete values you supply (never goal text); `$waitdir` is a unique absolute directory for this wait handle, created before launch:

```sh
callsheet task wait "$id1" "$id2" --until-done --json \
  --plane "$plane" --ca "$ca" >"$waitdir/result.json" 2>"$waitdir/error.txt"
rc=$?
printf '%s\n' "$rc" >"$waitdir/exit-code"
printf 'callsheet-wait-ended: %s exit=%s\n' "$handle" "$rc"
exit "$rc"
```

Match the notification with `^callsheet-wait-ended: ` and read the files only after that line or the command's exit. Nothing from the wait itself reaches stdout. Do not run it under `set -e` without guarding the Callsheet command: a nonzero exit must still be published. A missing `exit-code` file means the wrapper was killed first: treat it as an interrupted watcher. Stop a wait with the harness's group-stop facility so the foreground child stops too, and confirm the old command finished before replacing its handle. A harness that notifies on exit can capture the plain command without the wrapper.

Coordinator CLI support, as checked on 2026-10-06 from bundled skills, help text and binaries on one Linux host, with no model calls (static evidence, not a live qualification of a wake):

| Client | Recipe and boundary |
|---|---|
| Claude Code 2.1.291 | Bash with `run_in_background: true` and its completion notification: the proven local development pattern (background commands of seconds to hours with no timeout, waking the session when they end). The Monitor tool, which wakes on each output line, can run the single-line wrapper. Give the background job no timeout. |
| Grok 1.0.46 | The `monitor` tool with the single-line wrapper: its bundled skill says it wakes the session on every stdout line (static evidence). A monitor lasts at most 10 hours: that is the harness's limit, not a Callsheet deadline; when it expires, report and re-arm on a later turn with the same IDs. Vendor session survival is not claimed. |
| Cursor Agent 2026.10.01 (local) | A background shell with `notify_on_output` matching `^callsheet-wait-ended: ` (its bundled loop skill documents a regex wake), or its completion notification when the shell exits. Static local evidence only; cloud agents and their timers are outside this pattern, and the headless `--background-shell-timeout` does not make a persistent coordinator. |
| Codex 0.160.0 | **UNVERIFIED wake.** Background terminals exist (`unified_exec`), but no documented wake on output or exit was found, so polling the terminal would cost one model step per poll. Use a short `task_wait` on the next externally started turn instead; never hold a turn open or present terminal polling as a free wake. |

To change Codex's status, or to turn any row's static evidence into a runtime qualification, record the exact version, OS and architecture, the effective session and configuration, the background tool invocation, independent coordinator work and the end of its turn, the task's completion, and an automatically resumed turn that consumes the result **without another user message or a model poll**, plus clean process termination. Linux evidence does not certify macOS, and no vendor model session runs in CI.

Headless one-shot runs (`claude -p`, `codex exec`, `grok -p` and their Cursor equivalent) end with their turn, so nothing is left to wake: an external orchestrator must start them again. Any client or run without a verified wake polls with a short `task_wait` only when it next has a turn; neither Callsheet nor this guide promises an autonomous wake there.

## Claude Code

Captured version `2.1.282 (Claude Code)` ([version](../tests/testdata/cli-help/claude-version.txt), [help](../tests/testdata/cli-help/claude-help.txt)). Registration command syntax is VERIFIED by [mcp add help](../tests/testdata/cli-help/claude-mcp-add.txt); whether the registered server loads in a session is UNVERIFIED until local qualification. The catalog's version is now the worker adapter's qualified `2.1.285 (Claude Code)` ([host capture](../tests/testdata/real-adapters/linux-2026-09-30/host.txt), iteration 08); this coordinator help was not recaptured from it, and a local timeout qualification must observe the catalog's version before it can publish.

Register:

```sh
claude mcp add --transport stdio --scope user callsheet -- <callsheet-binary> mcp --plane <plane-url> --ca <ca-path>
```

Launch with your runbook appended to the system prompt:

```sh
claude --append-system-prompt "$(cat -- '<runbook>')"
```

Timeouts: call timeout, override and progress extension are UNVERIFIED; no supported override is established—run local qualification. `MCP_TOOL_TIMEOUT` is a candidate only.

| Fact | Status | Evidence |
|---|---|---|
| `mcp_config` | UNVERIFIED | [mcp add help](../tests/testdata/cli-help/claude-mcp-add.txt) |
| `mcp_timeout` | UNVERIFIED | [help](../tests/testdata/cli-help/claude-help.txt) |
| `mcp_timeout_override` | UNVERIFIED | [help](../tests/testdata/cli-help/claude-help.txt) |
| `mcp_progress_extension` | UNVERIFIED | [help](../tests/testdata/cli-help/claude-help.txt) |
| `runbook` | UNVERIFIED | [help](../tests/testdata/cli-help/claude-help.txt) |

## OpenAI Codex

Captured version `codex-cli 0.156.1` ([version](../tests/testdata/cli-help/codex-version.txt), [help](../tests/testdata/cli-help/codex-help.txt)). Registration command syntax is VERIFIED by [mcp add help](../tests/testdata/cli-help/codex-mcp-add.txt); the manual TOML table shape is a candidate. The catalog's version is now the worker adapter's qualified `codex-cli 0.159.0` ([host capture](../tests/testdata/real-adapters/linux-2026-09-30/host.txt), iteration 08); this coordinator help was not recaptured from it, and a local timeout qualification must observe the catalog's version before it can publish.

Register:

```sh
codex mcp add callsheet -- <callsheet-binary> mcp --plane <plane-url> --ca <ca-path>
```

Launch with your runbook as the initial prompt:

```sh
codex "$(cat -- '<runbook>')"
```

Timeouts: all three UNVERIFIED; no supported override is established—run local qualification. `mcp_servers.callsheet.tool_timeout_sec` is a candidate only.

| Fact | Status | Evidence |
|---|---|---|
| `mcp_config` | UNVERIFIED | [mcp add help](../tests/testdata/cli-help/codex-mcp-add.txt) |
| `mcp_timeout` | UNVERIFIED | [help](../tests/testdata/cli-help/codex-help.txt) |
| `mcp_timeout_override` | UNVERIFIED | [help](../tests/testdata/cli-help/codex-help.txt) |
| `mcp_progress_extension` | UNVERIFIED | [help](../tests/testdata/cli-help/codex-help.txt) |
| `runbook` | UNVERIFIED | [help](../tests/testdata/cli-help/codex-help.txt) |

## Grok Build

Captured version `grok 1.0.41 (4220f3b224a6) [stable]` ([version](../tests/testdata/cli-help/grok-version.txt), [help](../tests/testdata/cli-help/grok-help.txt)). Registration command syntax is VERIFIED by [mcp add help](../tests/testdata/cli-help/grok-mcp-add.txt); the TOML table schema is a candidate. The catalog's version is now the worker adapter's qualified `grok 1.0.46 (2765805b9442) [stable]` ([host capture](../tests/testdata/real-adapters/linux-2026-10-04/host.txt), iteration 11); this coordinator help was not recaptured from it, and a local timeout qualification must observe the catalog's version before it can publish.

Register:

```sh
grok mcp add --transport stdio --scope user callsheet -- <callsheet-binary> mcp --plane <plane-url> --ca <ca-path>
```

Launch with your runbook appended as rules:

```sh
grok --rules "$(cat -- '<runbook>')"
```

Timeouts: all three UNVERIFIED; no supported override is established—run local qualification. There is no established override key.

| Fact | Status | Evidence |
|---|---|---|
| `mcp_config` | UNVERIFIED | [mcp add help](../tests/testdata/cli-help/grok-mcp-add.txt) |
| `mcp_timeout` | UNVERIFIED | [help](../tests/testdata/cli-help/grok-help.txt) |
| `mcp_timeout_override` | UNVERIFIED | [help](../tests/testdata/cli-help/grok-help.txt) |
| `mcp_progress_extension` | UNVERIFIED | [help](../tests/testdata/cli-help/grok-help.txt) |
| `runbook` | UNVERIFIED | [help](../tests/testdata/cli-help/grok-help.txt) |

## Cursor Agent

Captured version `2026.09.23-86fc751` ([version](../tests/testdata/cli-help/cursor-version.txt), [help](../tests/testdata/cli-help/cursor-help.txt)). The catalog's version is now the known worker version `2026.10.01-e373342` ([host capture](../tests/testdata/real-adapters/linux-2026-10-04/host.txt), iteration 11; Cursor worker execution is refused); this coordinator help was not recaptured from it, and a local timeout qualification must observe the catalog's version before it can publish. The configuration locations `.cursor/mcp.json` (project) and `~/.cursor/mcp.json` (user) are VERIFIED by [mcp help](../tests/testdata/cli-help/cursor-mcp.txt); the exact entry shape is UNVERIFIED. There is no `cursor-agent mcp add`: edit the user-owned file with your editor. `cursor-agent mcp list-tools callsheet` can list the tools where the installed version supports it, but a help command alone does not verify that the configuration loaded or that a tool call works.

Candidate entry (UNVERIFIED until an actual registration plus tools/list evidence):

```json
{"mcpServers":{"callsheet":{"command":"<callsheet-binary>","args":["mcp","--plane","<plane-url>","--ca","<ca-path>"]}}}
```

Launch with your runbook as the initial prompt:

```sh
cursor-agent "$(cat -- '<runbook>')"
```

Timeouts: all three UNVERIFIED; no supported override is established—run local qualification. The installed bundle's `resetTimeoutOnProgress` symbol is not call-site evidence.

| Fact | Status | Evidence |
|---|---|---|
| `mcp_config` | UNVERIFIED | [mcp help](../tests/testdata/cli-help/cursor-mcp.txt) |
| `mcp_timeout` | UNVERIFIED | [mcp help](../tests/testdata/cli-help/cursor-mcp.txt) |
| `mcp_timeout_override` | UNVERIFIED | [mcp help](../tests/testdata/cli-help/cursor-mcp.txt) |
| `mcp_progress_extension` | UNVERIFIED | [installed excerpts](../tests/testdata/cli-help/cursor-installed-excerpts.json) |
| `runbook` | UNVERIFIED | [help](../tests/testdata/cli-help/cursor-help.txt) |

## Dispatch attribution (clientInfo)

A dispatch records `requested_by` from the client's own initialize `clientInfo.name` and `clientInfo.version` plus the coordinator hostname: self-reported audit context, not an authenticated identity. Callsheet's grammar requires 1-128 printable ASCII bytes without spaces for the name and the version. Local qualification records both strings exactly (JSON-escaped, never normalized) with the grammar result in the client's `mcp_config` fact and its report; a space-containing name is a dispatch compatibility failure even when configuration and tool discovery work, and a qualified registration alone does not qualify dispatch. Missing observations stay UNVERIFIED, and `mcp_config` keeps its 08/11 owner.

## Local timeout qualification

The developer command `cmd/mcpqual` measures a client's effective silent tool-call timeout, a raised-timeout setting, whether progress notifications extend a call and any absolute cap, against a deterministic probe server (`mcpqual serve`, one `slow` tool whose delay comes from a case file). It runs only when you invoke it; it never runs from CI, from Callsheet, from a tool call, from a test or at installation, and it refuses to start when `CI` is set:

```text
go run ./cmd/mcpqual qualify --plan /absolute/qualification-plan.json --out /absolute/evidence-dir --allow-model-calls
```

- Start from an example plan and complete its owner placeholders (the absolute executable path, `<model>` and any raised setting): [Claude Code](../internal/mcpqual/testdata/plans/claude.json), [Codex](../internal/mcpqual/testdata/plans/codex.json), [Grok Build](../internal/mcpqual/testdata/plans/grok.json), [Cursor Agent](../internal/mcpqual/testdata/plans/cursor.json). The examples are templates, never vendor evidence.
- Version and help checks run before that gate, so a plan cannot choose them: `version_argv` must be `--version` and `help_argv` one of `--help`, `exec --help`, `mcp --help`, `mcp add --help` or `agent --help`; any other plan is rejected before anything is launched, and the observed version must equal the plan's expected version exactly.
- Without `--allow-model-calls` only version/help/config checks and a genuinely model-free vendor driver run; model-required phases are reported not qualified. With it, model sessions run up to the plan's budget (by default at most 12 sessions per client, 15 minutes per case and 90 minutes per client); the planned upper bound is printed before the first launch.
- The probe configuration is written into a disposable workspace; `~/.claude`, `~/.codex`, `~/.grok` and `~/.cursor` are never rewritten. Absent or unauthenticated clients produce a partial report, not a retry loop or a fabricated timeout.
- Every launched process group is stopped (TERM, a 1 s grace, KILL) and proven gone before the run ends; a surviving group is a cleanup failure and blocks publication.
- Exit codes: 0 when every requested phase is conclusive, 2 for an invalid plan (or `CI` set), 4 for a publication conflict, 5 for a partial or unqualified client or a cleanup failure, 130 when interrupted. A partial run still writes its report.
- The evidence directory holds `report.json`, `report.md`, `manifest.json`, sanitized configuration snapshots and bounded per-case probe and vendor event files, redacted before hashing. `report.json` has the same 8 MiB bound: a case whose summary would exceed its share keeps a cut summary, is `inconclusive` with reason `report_bound`, and still references its full sanitized event files. The report separates "harness implementation verified by fake tests" from "vendor behavior measured locally". A decoder version is qualified for a `VERIFIED` fact only by your redacted actual transcript; until then measurements stay `UNVERIFIED`.
- `--publish-catalog /absolute/repo` is a separate explicit step, given with `qualify` so the catalog's base hashes are recorded before the first launch: it installs the named evidence under `tests/testdata/mcp-qualification/<run>/`, updates the three timeout facts in both catalog representations and records clientInfo in `mcp_config`, and refuses (exit 4) if either catalog changed since the run started. It never changes worker facts or the shipping 10s default. `mcpqual publish --out <dir> --repo <repo>` only retries an interrupted publication of that run's proposed patch; a run qualified without `--publish-catalog` has no patch and cannot be published later (qualify again with the option). Publication of a VERIFIED lower bound or measured timeout against a new-policy catalog is permitted, subject to the evidence gates; an old-policy catalog is refused with ‘update catalog policy first’ (exit 4).

### Short confirmation

The short-poll budget needs only a cheap setup and default-only confirmation per intended client, version, configuration and platform: that each client's silent tool-call timeout leaves room for B plus its response margin. Start from a short plan template (setup, one silent 15s call, and a repeat of that successful bound: at most three sessions per client, a 120s case ceiling for model startup and 6 minutes per client): [Claude Code](../internal/mcpqual/testdata/plans/claude-short.json), [Codex](../internal/mcpqual/testdata/plans/codex-short.json), [Grok Build](../internal/mcpqual/testdata/plans/grok-short.json), [Cursor Agent](../internal/mcpqual/testdata/plans/cursor-short.json). Complete their owner placeholders: the absolute executable path, `<model>` and `<exact-cli-version>`, the exact version your CLI reports to `--version` (an unfilled placeholder is refused before anything runs); none has a raised-timeout setting, override or long phase. A conclusive result needs a decoder enrolled from a redacted actual transcript of that exact version. mcpqual ships synthetic decoder fixtures only, for the captured help versions of the full templates, so until such a decoder exists the run fails closed with `unsupported_decoder_version` before its first session and compatibility stays UNVERIFIED; the version is never edited to match a fixture. Publication still requires the run's observed version to equal the catalog entry's exact version. Run it outside CI with the same command, giving `--publish-catalog` at the start if a published fact is wanted:

```text
go run ./cmd/mcpqual qualify --plan /absolute/vendor-short.json --out /absolute/evidence-dir --allow-model-calls --publish-catalog /absolute/repo
```

The decision is conservative: with L the longest silent call that completed (a lower bound), B is compatible when B + max(2s, 0.1L) < L. For the short plan's L = 15s: 10s + max(2s, 1.5s) = 12s < 15s. A lower bound is safe because T - max(2s, 0.1T) grows with T; never use a timeout's upper bound or an interval midpoint as the safe value. `report.md` prints this decision for each client after its phases, as a vendor verdict only from qualified, correlated evidence (a decoder qualified by an actual transcript, a conclusive setup, the successful bound repeated, a clean and uninterrupted run); otherwise it says compatibility UNVERIFIED and shows the inequality as measurement only.

| Observation | Interpretation |
|---|---|
| Setup succeeds and both 15s silent calls complete with qualified, correlated evidence | Compatible for that measured tuple (L = 15s). With no timeout observed, publish only a verified **lower bound**, never "the timeout is 15s" or "unlimited". |
| A typed timeout whose upper bound is at or below 12s | 10s plus its margin does not fit: report the incompatibility. Use a smaller per-client B only with its own successful lower bound proving its margin; otherwise dispatch without an MCP wait and use the background CLI wait or next-turn checks. The global default never changes automatically. |
| A timeout before the 15s result that does not prove the inequality (including an interval spanning the threshold) | Failed or inconclusive for compatibility, even when mcpqual exits 0 for a conclusive timeout observation. A new bounded default-only plan with smaller ascending delays may find a safe setting; never infer safety from an upper bound. |
| Setup, authentication, model, configuration, decoder or cleanup failure, a missing repeat, an interrupted run or an exhausted budget | Compatibility UNVERIFIED; keep the evidence and the blocker; never substitute another client. |

A failed first 15s call schedules no repeat. A synthetic decoder fixture or a version that differs from the catalog's is a blocker to record, not something to bypass: qualify a redacted actual transcript and reconcile the version before publishing a vendor fact. Linux and macOS are reported separately, and no simulated vendor result is live compatibility evidence.
