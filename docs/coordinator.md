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

> Default 10s interim call budget is UNVERIFIED against coordinator client timeouts; repeat task_wait. Raising it requires local timeout qualification.

The 10s default ships under the catalog's [owner-authorized interim exception](support-catalog.md#interim-mcp-wait-exception). Choose a larger B only from a named verified evidence set: after qualification, B + max(2s, 10% of T) < T, where T is the smallest verified effective silent tool-call timeout across every intended client, version, platform and configuration sharing that server configuration. Use conservative measured lower bounds, never the midpoint of an interval, and include any measured absolute maximum. Keep B at most 5m and keep the plane's clamp. If any intended client is unqualified, keep the shared B at 10s. Never raise B on progress evidence alone. Raising a vendor's own timeout does not change Callsheet's B or the plane clamp. A verified fact names its exact OS, architecture, version and settings; Linux observations never certify macOS. Until such evidence exists this guide prints no supposedly safe larger value.

For each vendor below, the timeout rows are `UNVERIFIED; no supported override is established—run local qualification`, with candidate settings named as candidates only.

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

Captured version `grok 1.0.41 (4220f3b224a6) [stable]` ([version](../tests/testdata/cli-help/grok-version.txt), [help](../tests/testdata/cli-help/grok-help.txt)). Registration command syntax is VERIFIED by [mcp add help](../tests/testdata/cli-help/grok-mcp-add.txt); the TOML table schema is a candidate.

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

Captured version `2026.09.23-86fc751` ([version](../tests/testdata/cli-help/cursor-version.txt), [help](../tests/testdata/cli-help/cursor-help.txt)). The configuration locations `.cursor/mcp.json` (project) and `~/.cursor/mcp.json` (user) are VERIFIED by [mcp help](../tests/testdata/cli-help/cursor-mcp.txt); the exact entry shape is UNVERIFIED. There is no `cursor-agent mcp add`: edit the user-owned file with your editor. `cursor-agent mcp list-tools callsheet` can list the tools where the installed version supports it, but a help command alone does not verify that the configuration loaded or that a tool call works.

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
- `--publish-catalog /absolute/repo` is a separate explicit step, given with `qualify` so the catalog's base hashes are recorded before the first launch: it installs the named evidence under `tests/testdata/mcp-qualification/<run>/`, updates the three timeout facts in both catalog representations and records clientInfo in `mcp_config`, and refuses (exit 4) if either catalog changed since the run started. It never changes worker facts or the shipping 10s default. `mcpqual publish --out <dir> --repo <repo>` only retries an interrupted publication of that run's proposed patch; a run qualified without `--publish-catalog` has no patch and cannot be published later (qualify again with the option). While the catalog keeps the 07a interim exception, publication refuses a `VERIFIED` `mcp_timeout` (exit 4): ending that exception is a design decision.
