# First-wave CLI support catalog — offline baseline

Transcribed from the iteration 01 design catalog; evidence links point at the checked-in copies.

## Catalog contract

FP-7 covers this entire document. Captured **2026-09-25, Linux amd64**. `VERIFIED` means supported by the named local help/version output or installed-file excerpt; it does **not** mean a model invocation succeeded. `UNVERIFIED` means unavailable from that evidence and must not be promoted to a runtime guarantee. No model, login, models-list, MCP-connect or network command was run. `--help` and `--version` ran only. Codex printed a read-only PATH-alias warning and still returned help/version successfully; that warning is not evidence that worker execution would succeed.

All commands below are illustrative argv contracts, not commands run during this job. `<model>`, `<effort>`, `<prompt>`, `<plane-url>`, `<ca-path>` and `<runbook>` are placeholders, never factory-default model choices. Final adapter selection/allowed efforts is iteration 08 for Claude/Codex and 11 for Grok/Cursor. Unknown model/effort compatibility must be rejected during qualification rather than silently dropping effort. Do not pass `--resume`, `--continue`, vendor worktree flags, cloud-worker mode or vendor MCP-server mode for task workers.

**Iteration 08 runtime evidence (Claude and Codex workers).** The owner's Linux qualification captures of **2026-09-30** (Linux 6.8.0 x86_64, `claude` 2.1.285, `codex` 0.159.0) are checked in byte-for-byte under [`tests/testdata/real-adapters/linux-2026-09-30/`](../tests/testdata/real-adapters/linux-2026-09-30/) with a provenance [manifest](../tests/testdata/real-adapters/linux-2026-09-30/manifest.json) (destination, source name, bytes and SHA-256 of every file; absent capture files listed as absent). Unlike the help captures above, these runs did call models: exact argv, stdin, working directory, stdout, stderr, exit and final-file bytes of successful and failed headless runs, and baseline/candidate write canaries. They supersede the help-level worker candidates for Claude and Codex only; the older help captures (Claude 2.1.282, Codex 0.156.1) remain the evidence for the coordinator facts and are not relabeled as captures of the newer versions. The Claude and Codex worker adapters and their operator procedure are described in [real adapters](real-adapters.md).

The baseline has intentionally incomplete runtime facts. Record worker launch recipe, model, effort, approval suppression, sandbox effect for Linux/macOS, final-message extraction and exit behavior; coordinator stdio config, call timeout, override, progress extension and runbook loading, even when `UNVERIFIED`. A help claim and its unverified runtime consequences are separate fields.

The machine-readable snapshot of this catalog is [`tests/testdata/support-catalog.json`](../tests/testdata/support-catalog.json): four entries (`id`, `version`, `platform`, `facts`), each with the thirteen facts `headless`, `model`, `effort`, `approval`, `sandbox_linux`, `sandbox_macos`, `final_message`, `exit_codes`, `mcp_config`, `mcp_timeout`, `mcp_timeout_override`, `mcp_progress_extension` and `runbook`. Every fact is `VERIFIED` or `UNVERIFIED` with a value, repository-root-relative evidence paths and the verifying iteration, one per-key rule (`catalog.Owner`): `07b` for the three coordinator timeout facts `mcp_timeout`, `mcp_timeout_override` and `mcp_progress_extension` of every vendor (iteration 07b's local timeout qualification), and otherwise `08` for Claude/Codex and `11` for Grok/Cursor, including `mcp_config`. Mixed facts are wholly `UNVERIFIED`, with the verified subset described in the value. The captured help/version/excerpt evidence is checked in under [`tests/testdata/cli-help/`](../tests/testdata/cli-help/); `TestFP7CatalogContract` validates structure and completeness, not vendor behaviour.

## Claude Code

**Version:** `2.1.285 (Claude Code)` — the worker adapter's qualified version, VERIFIED by the [host capture](../tests/testdata/real-adapters/linux-2026-09-30/host.txt) of 2026-09-30 (iteration 08). The coordinator facts below still rest on the older 2.1.282 captures: [version](../tests/testdata/cli-help/claude-version.txt), [help](../tests/testdata/cli-help/claude-help.txt), [mcp add help](../tests/testdata/cli-help/claude-mcp-add.txt).

| Worker fact | Evidence and status (iteration 08, Linux x86_64, 2.1.285) |
|---|---|
| Headless | VERIFIED: `claude -p --model sonnet --effort low --permission-prompts none --output-format json` with the prompt only on stdin and a fresh empty mode-0700 non-git working directory ([success argv](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/claude-stdin-success/argv.txt), [stdin](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/claude-stdin-success/stdin.txt), [stdout](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/claude-stdin-success/stdout.bin)). |
| Model | VERIFIED for `sonnet` only. Callsheet passes exactly that value; every other model is refused by the worker before launch (role validation, readiness and task preparation), never probed with a paid call. The success run's `modelUsage` key `claude-sonnet-5-5` is evidence, not a second accepted selection. The operator default (`opus[1m]`) is never substituted. |
| Effort | VERIFIED for `low` with `sonnet` only; help lists more levels, which are not qualified and are refused. |
| Approval | VERIFIED narrow observation: `--permission-prompts none` ran unattended on the recorded host and configuration. It denies what would prompt; it can refuse work requiring approval, and Callsheet never retries with broader permissions. Not a claim for other configurations or operating systems. |
| Sandbox, Linux | VERIFIED narrow observation (below): the flag did not widen the observed write outcomes versus baseline on that host. |
| Sandbox, macOS | UNVERIFIED: iteration 08 did not measure macOS. |
| Final message | VERIFIED: stdout is one JSON object with `type` `result`, boolean `is_error` and string `result`; success `pong` (exit 0), and the failed run's model-not-found sentence with `is_error` true and `subtype` `success` (exit 1, [failure stdout](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/claude-stdin-fail/stdout.bin)). Callsheet returns `result` exactly (input bounded to 8 MiB, answer to 64 KiB); malformed or oversized output leaves the final message null with a diagnostic. |
| Exit codes | UNVERIFIED as a mapping: success 0 and unknown model 1 observed on Linux; authentication, denial, cancellation, other failures and every macOS exit were not measured. Callsheet reports the observed exit; `subtype` never decides success. |

Qualified worker recipe (Linux): `claude -p --model sonnet --effort low --permission-prompts none --output-format json`, prompt on stdin, cwd the task's private scratch directory. Callsheet passes no `--dangerously-skip-permissions`, sandbox override, resume/continue, worktree or permission-mode flag.

Observed configuration (read, not changed): `settings.json` sandbox `enabled`, `strictMode` and `failIfUnavailable` true, `allowUnsandboxedCommands` false, explicit deny paths, and a filesystem write allowlist including `/tmp` and `/opt/gitspace`; the home sentinel directory was outside it. [`claude sandbox status`](../tests/testdata/real-adapters/linux-2026-09-30/claude-sandbox-status.json) reported `available` false and `installed` false with a Windows-specific reason: that contradiction is retained, and the canaries do not claim that any named Linux sandbox mechanism was active. On this host and this version the selected approval flag did not widen the **observed write outcomes** versus baseline: the in-directory write succeeded with and without it ([candidate](../tests/testdata/real-adapters/linux-2026-09-30/runs/claude-permitted/sentinel.txt), [baseline](../tests/testdata/real-adapters/linux-2026-09-30/runs/claude-baseline-permitted/sentinel.txt)), and the home sentinel stayed absent in both ([candidate](../tests/testdata/real-adapters/linux-2026-09-30/runs/claude-forbidden-home/sentinel.txt), [baseline](../tests/testdata/real-adapters/linux-2026-09-30/runs/claude-baseline-forbidden-home/sentinel.txt)). The model's "Read-only file system" sentence is prose; the absent file is the observation. This is not proof of read isolation, every tool's containment, every directory, managed-configuration precedence or future versions.

Coordinator stdio registration syntax is VERIFIED: `claude mcp add --transport stdio --scope user callsheet -- callsheet mcp --plane <plane-url> --ca <ca-path>`. The Callsheet flags are reserved for iteration 07, not functional in the skeleton. Help supports scoped configuration and `--mcp-config` JSON files/strings. Candidate file format is `{"mcpServers":{"callsheet":{"command":"callsheet","args":["mcp","--plane","<plane-url>","--ca","<ca-path>"]}}}`; exact file schema/location is **UNVERIFIED** from help and must be validated in 08 (the registration command avoids guessing it).

- **MCP call timeout: UNVERIFIED.** Do not confuse startup timeout with tool-call timeout.
- **How to raise it: UNVERIFIED.** `MCP_TOOL_TIMEOUT` is a candidate to investigate, not an endorsed setting/value.
- **Progress extends calls: UNVERIFIED.** Require a delayed test tool with progress on/off; no assumption that notifications reset the hard deadline.
- **Coordinator runbook:** VERIFIED `--append-system-prompt <text>` exists; a coordinator launcher may read the owned UTF-8 runbook and pass its text as that argument. Automatic `CLAUDE.md` discovery is mentioned by help; precedence and exact loading scope are UNVERIFIED. The user's own interactive configuration is not modified by Callsheet. Instruction-file flags mentioned in prose but absent from the option list must be checked before use.

## OpenAI Codex

**Version:** `codex-cli 0.159.0` — the worker adapter's qualified version, VERIFIED by the [host capture](../tests/testdata/real-adapters/linux-2026-09-30/host.txt) of 2026-09-30 (iteration 08). The coordinator facts below still rest on the older 0.156.1 captures: [version](../tests/testdata/cli-help/codex-version.txt), [root help](../tests/testdata/cli-help/codex-help.txt), [exec help](../tests/testdata/cli-help/codex-exec.txt), [mcp add help](../tests/testdata/cli-help/codex-mcp-add.txt). No official web lookup was performed because this job requires offline evidence.

| Worker fact | Evidence and status (iteration 08, Linux x86_64, codex-cli 0.159.0) |
|---|---|
| Headless | VERIFIED: `codex -a never exec --model gpt-6.1-sol -c 'model_reasoning_effort="low"' --json --skip-git-repo-check --output-last-message <final-file> -` with the prompt on stdin in a fresh empty mode-0700 non-git working directory ([success argv](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/codex-skip-success/argv.txt)). `--skip-git-repo-check` is required there: without it the run exited 1 before any model call ([stderr](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/codex-empty-success/stderr.bin)). It is not a sandbox override; Callsheet never edits trust entries (earlier checkout runs showed Codex adding `trust_level` entries itself). |
| Model | VERIFIED for `gpt-6.1-sol` only; every other model is refused by the worker before launch. |
| Effort | VERIFIED for `low` with `gpt-6.1-sol` only, as `-c model_reasoning_effort="low"` (the quotes are literal bytes of one argument, not shell quoting); the operator default `medium` is never substituted and no other level is qualified. |
| Approval | VERIFIED narrow observation: root `-a never` before `exec` ran unattended on the recorded host and configuration. Execution failures return to the model; Callsheet never retries with broader permissions. |
| Sandbox, Linux | VERIFIED narrow observation (below). |
| Sandbox, macOS | UNVERIFIED: iteration 08 did not measure macOS. |
| Final message | VERIFIED: `--output-last-message` wrote exactly the last agent message on success (`pong`, 4 bytes, no newline, [file](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/codex-skip-success/final.saved.txt)) and no file on the failed run (exit 1). Callsheet reads the task-private `callsheet-final.txt` after the process group is gone, relative to a retained directory handle without following links, bounded to 8 MiB (answer 64 KiB). A missing, unsafe, unreadable or invalid file leaves the final message null with a diagnostic, never an empty answer; an existing empty file is the empty answer. `--json` stdout, including intermediate `agent_message` items and benign `item.type=error` hook warnings, is event logging and stays in the task log, never the answer. |
| Exit codes | UNVERIFIED as a mapping: success 0 and unknown model 1 observed on Linux; authentication, denial, cancellation, other failures and every macOS exit were not measured. Callsheet reports the observed exit. |

Qualified worker recipe (Linux): `codex -a never exec --model gpt-6.1-sol -c 'model_reasoning_effort="low"' --json --skip-git-repo-check --output-last-message <scratch>/callsheet-final.txt -`, prompt on stdin, cwd the task's private scratch directory. No `--ignore-user-config`, `--sandbox`, dangerous bypass, `--approve-for-me`, resume or cloud mode: these would disturb the operator's posture.

Observed configuration (read, not changed): `config.toml` `sandbox_mode` `workspace-write`, `approvals_reviewer` `user` (operator defaults `gpt-6.1-sol`/`medium` are not what the adapter passes). On this host and this version the selected approval flag did not widen the **observed write outcomes** versus baseline: in-directory writes succeeded with and without `-a never` ([candidate](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/codex-skip-permitted/sentinel.txt), [baseline](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/codex-skip-baseline-permitted/sentinel.txt)), the home sentinel stayed absent in both ([candidate](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/codex-skip-forbidden-home/sentinel.txt), [baseline](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/codex-skip-baseline-forbidden-home/sentinel.txt)), and a `/tmp` write succeeded ([sentinel](../tests/testdata/real-adapters/linux-2026-09-30/runs-scratch/codex-skip-forbidden-tmp/sentinel.txt)). The home-denial captures contain no `command_execution` event, so the absent file is the observation, not proof of an attempted system call; the "Read-only file system" sentence is model prose. This is not proof of read isolation, every tool's containment, every directory, managed-configuration precedence or future versions; `workspace-write` still allowed `/tmp` writes.

Coordinator registration syntax is VERIFIED: `codex mcp add callsheet -- callsheet mcp --plane <plane-url> --ca <ca-path>`. Candidate manual TOML format below is **UNVERIFIED** by these help pages (only the config path and generic TOML overrides are verified):

```toml
[mcp_servers.callsheet]
command = "callsheet"
args = ["mcp", "--plane", "<plane-url>", "--ca", "<ca-path>"]
# tool_timeout_sec = 120  # UNVERIFIED candidate; do not ship as established fact
```

- **MCP call timeout: UNVERIFIED.**
- **How to raise it: UNVERIFIED;** candidate `mcp_servers.callsheet.tool_timeout_sec` needs schema/runtime evidence in iteration 07b.
- **Progress extends calls: UNVERIFIED;** test idle timeout and absolute timeout separately.
- **Coordinator runbook:** VERIFIED root accepts an initial prompt argument; a launcher can read the user's runbook into that argument. Automatic project `AGENTS.md` loading and precedence are **UNVERIFIED** from captured help; confirm in 08. Do not alter the runbook or register a coordinator role.

## Grok Build

**Version:** `grok 1.0.41 (4220f3b224a6) [stable]` — VERIFIED by [version](../tests/testdata/cli-help/grok-version.txt). Sources: [help](../tests/testdata/cli-help/grok-help.txt), [agent help](../tests/testdata/cli-help/grok-agent.txt), [mcp add help](../tests/testdata/cli-help/grok-mcp-add.txt).

| Worker fact | Evidence and status |
|---|---|
| Headless | VERIFIED `grok -p/--single <prompt>` prints response to stdout and exits. `--prompt-file <path>` is another single-turn input. Use this, not `grok agent serve/headless` (other transports). |
| Model/effort | VERIFIED `--model <model>` and `--reasoning-effort <effort>` (alias `--effort`). Allowed effort set and per-model mapping are UNVERIFIED. |
| Approval | VERIFIED `--permission-mode dontAsk` is a supported mode; its exact no-prompt/denial behavior is UNVERIFIED. `--always-approve` explicitly auto-approves all tools. Prefer qualifying `dontAsk` first; do not assume always-approve preserves containment. |
| Sandbox, Linux/macOS | VERIFIED `--sandbox <profile>`/`GROK_SANDBOX` select filesystem/network profile. Candidate worker leaves both unchanged. UNVERIFIED default/profile discovery, whether either approval flag changes sandbox enforcement, and OS/TTY requirements. Do not infer enforcement from the presence of a flag. Operator profile format and Linux/macOS enforcement must be documented from vendor evidence in 11. |
| Final message | VERIFIED `--output-format plain|json|streaming-json|streaming-messages-json`; help calls streaming-json ACP updates. `-p` prints the response. UNVERIFIED JSON final-record schema and error extraction; 11 needs saved success/failure transcripts. |
| Exit codes | UNVERIFIED runtime success/auth/tool failure/cancel numeric exits. Help/version exited 0 only. |

Candidate worker: `grok -p <prompt> --model <model> --reasoning-effort <effort> --permission-mode dontAsk --output-format json`. No sandbox override. Until 11 proves no interactive hang and the sandbox effect, this is not a qualified adapter.

Coordinator registration is VERIFIED: `grok mcp add --transport stdio --scope user callsheet -- callsheet mcp --plane <plane-url> --ca <ca-path>`. Help names user `~/.grok/config.toml` or project `./.grok/config.toml`. Exact TOML table schema is **UNVERIFIED**; use vendor registration command for later setup rather than inventing tables. Stdio command/args/env are exposed by help.

- **MCP call timeout: UNVERIFIED.**
- **How to raise it: UNVERIFIED.** No candidate key is established in local help.
- **Progress extends calls: UNVERIFIED.**
- **Coordinator runbook:** VERIFIED `--rules <text>` appends rules to the system prompt; a launcher can read the owned file and supply its content. Automatic manual filenames/loading precedence are UNVERIFIED. `--system-prompt-override` replaces the vendor system prompt and is not the preferred way to append a coordinator runbook.

## Cursor Agent

**Version:** `2026.09.23-86fc751` — VERIFIED by [version](../tests/testdata/cli-help/cursor-version.txt). Executable inspected: `cursor-agent`; its help calls itself `agent`. The `agent` alias exists on this machine but identical behavior/version is **UNVERIFIED** (it was not executed). Sources: [help](../tests/testdata/cli-help/cursor-help.txt), [mcp help](../tests/testdata/cli-help/cursor-mcp.txt), [installed excerpts](../tests/testdata/cli-help/cursor-installed-excerpts.json).

| Worker fact | Evidence and status |
|---|---|
| Headless | VERIFIED `cursor-agent -p/--print <prompt>` has write/shell tools, and `--output-format text|json|stream-json`. |
| Model | VERIFIED `--model <model>`. Help also documents bracket parameterization, e.g. a quoted model with `effort=high`. It is syntax evidence, not verification of any available model. |
| Effort | No standalone effort flag in captured help. UNVERIFIED Q12 suffix mapping (`model` + `-effort`) and allowed model/effort sets; bracket override syntax is VERIFIED but compatibility/model catalog is not. Iteration 11 must establish a mapping table, not blindly concatenate every model and effort. |
| Approval | VERIFIED `--force`/`-f` force-allows commands unless explicitly denied; `--yolo` is its alias. `--auto-review` may still prompt and cannot guarantee unattended work. `--trust` suppresses workspace trust prompt; `--approve-mcps` approves all MCP servers and is a distinct, wider permission. |
| Sandbox, Linux/macOS | VERIFIED `--sandbox enabled|disabled` overrides configuration. Candidate omits it and uses `--force --trust`; the interaction of force with the configured sandbox is UNVERIFIED. Shell sandboxing versus the CLI's own file tools and per-OS boundaries must be behaviorally qualified in 11. Never describe all writes as OS-confined based on this help. Config paths/keys for operator sandbox setup are UNVERIFIED. |
| Final message | VERIFIED print output formats; UNVERIFIED final JSON field/event, whether errors carry text, and distinction between final answer and tool/status output. Capture fixtures in 11 before parser design. |
| Exit codes | UNVERIFIED worker success/error/auth/denial/cancellation numeric behavior. Help/version exited 0 only. |

Candidate worker: `cursor-agent -p <prompt> --model <mapped-model> --force --trust --output-format json`, with model/effort mapping and sandbox preservation explicitly unqualified. Do not pass `--sandbox disabled`, automatically approve unrelated MCP servers or use plan/ask as a worker substitute.

Coordinator help VERIFIES configuration files `.cursor/mcp.json` and `~/.cursor/mcp.json`. Candidate JSON is `{"mcpServers":{"callsheet":{"command":"callsheet","args":["mcp","--plane","<plane-url>","--ca","<ca-path>"]}}}`. The exact entry schema is **UNVERIFIED** from help; installed bundles contain `mcpServers` but mere symbol presence does not prove this file shape. There is no `mcp add` in captured help; do not recommend a nonexistent setup command.

- **MCP call timeout: UNVERIFIED.**
- **How to raise it: UNVERIFIED.**
- **Progress extends calls: UNVERIFIED.** Installed bundle contains `resetTimeoutOnProgress` in protocol code; this does not establish the option passed at the actual MCP call site, or whether a separate maximum timeout applies.
- **Coordinator runbook:** VERIFIED initial prompt argument; a launcher can read the user's file and pass its text. Installed bundle mentions `AGENTS.md`, but automatic loading/priority as coordinator instructions is UNVERIFIED. Confirm actual rule loading in 11.

## Qualification handoff

For **each** CLI, 08/11 must record OS and exact version, a successful and failed headless run, the exact final-message bytes and exit code, and a configured-sandbox canary under the selected approval flags. Compare operator baseline to adapter invocation; a narrower denial-only mode is preferred even when it prevents unapproved tools. No unknown is evidence that sandbox bypass is unavoidable. Callsheet must fail clearly if a qualified unattended recipe cannot preserve the required operator posture; it must not silently widen it.

Iteration 08 recorded this for Claude and Codex on Linux (above). Operators run sidecars as a dedicated OS user: containment is whatever the configured vendor sandbox provides, which often allows host reads and `/tmp` writes; Callsheet creates no sandbox and selects no sandbox profile. Supply the executables explicitly (`callsheet sidecar run --claude-adapter /abs/claude --codex-adapter /abs/codex` on every start; no PATH lookup) and register roles with the qualified pairs (claude `sonnet`/`low`, codex `gpt-6.1-sol`/`low`). Inspect the effective vendor configuration with the vendor's own tools (for example `claude sandbox status` and your `settings.json`, or Codex's `config.toml`); Callsheet neither reads nor rewrites it. An unknown or new version fails the worker's `--version` probe until it is qualified.

To qualify macOS (or a new version or model/effort pair) capture: OS and architecture, the exact binary version, the sandbox- and approval-relevant configuration keys without secrets, a successful and a failed-model stdin run in a fresh non-git mode-0700 directory (exact argv, stdin, stdout, stderr, exit, final-file presence and bytes), then baseline and candidate write canaries for an allowed in-directory path, a disallowed home path and `/tmp`, recording the actual settings keys and enforcement observations for that OS. Until then the adapters may be explicitly enabled on macOS, where the sidecar logs a warning that the recipe is Linux-qualified and macOS vendor sandbox and exit behavior are UNVERIFIED; replay fixtures cannot establish real vendor behavior, and Linux canaries attest to nothing an operator changed after capture.

For coordinator waiting, use a local deterministic MCP test server (no model call when the client offers an offline protocol harness). Measure default tool timeout, configured increase, progress-enabled versus silent requests, and any absolute cap. Record supported setting syntax with evidence. Iteration 07b supplies that server and the explicitly invoked local harness; the per-client setup, runbook loading and qualification steps are in the [coordinator setup guide](coordinator.md). <a id="interim-mcp-wait-exception"></a>Owner-authorized interim exception: iteration 07a ships task_wait and dispatch-with-wait with an UNVERIFIED 10s outer call budget, shorter plane waits reserve transport/admission/response time, and any increase requires local timeout qualification with an explicit response margin. Do not set a multi-minute wait because an SDK contains a reset-timeout symbol.

Iteration 06b adds bounded waiting (`callsheet task wait`, `callsheet dispatch --wait`, `POST /api/v1/tasks/wait`) with a plane-side cap (`plane run --max-task-wait`, default 30 s, at most 5 m) that is a **CLI/API cap only**. It is not MCP-qualified and this catalog claims no MCP-safe value for it: no vendor's effective MCP tool-call timeout or progress behaviour above is VERIFIED. Iteration 07a's MCP waits follow the [owner-authorized interim exception](#interim-mcp-wait-exception) above and never reuse the CLI cap as an MCP timeout.

The catalog is a fact sheet, not a claim that all four vendors currently meet the frozen requirements. Its explicit UNVERIFIED entries are the brief's authorized handoff to real-adapter qualification.
