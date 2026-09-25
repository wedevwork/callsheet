# First-wave CLI support catalog — offline baseline

Transcribed from the iteration 01 design catalog; evidence links point at the checked-in copies.

## Catalog contract

FP-7 covers this entire document. Captured **2026-09-25, Linux amd64**. `VERIFIED` means supported by the named local help/version output or installed-file excerpt; it does **not** mean a model invocation succeeded. `UNVERIFIED` means unavailable from that evidence and must not be promoted to a runtime guarantee. No model, login, models-list, MCP-connect or network command was run. `--help` and `--version` ran only. Codex printed a read-only PATH-alias warning and still returned help/version successfully; that warning is not evidence that worker execution would succeed.

All commands below are illustrative argv contracts, not commands run during this job. `<model>`, `<effort>`, `<prompt>`, `<plane-url>`, `<ca-path>` and `<runbook>` are placeholders, never factory-default model choices. Final adapter selection/allowed efforts is iteration 08 for Claude/Codex and 11 for Grok/Cursor. Unknown model/effort compatibility must be rejected during qualification rather than silently dropping effort. Do not pass `--resume`, `--continue`, vendor worktree flags, cloud-worker mode or vendor MCP-server mode for task workers.

The baseline has intentionally incomplete runtime facts. Record worker launch recipe, model, effort, approval suppression, sandbox effect for Linux/macOS, final-message extraction and exit behavior; coordinator stdio config, call timeout, override, progress extension and runbook loading, even when `UNVERIFIED`. A help claim and its unverified runtime consequences are separate fields.

The machine-readable snapshot of this catalog is [`tests/testdata/support-catalog.json`](../tests/testdata/support-catalog.json): four entries (`id`, `version`, `platform`, `facts`), each with the thirteen facts `headless`, `model`, `effort`, `approval`, `sandbox_linux`, `sandbox_macos`, `final_message`, `exit_codes`, `mcp_config`, `mcp_timeout`, `mcp_timeout_override`, `mcp_progress_extension` and `runbook`. Every fact is `VERIFIED` or `UNVERIFIED` with a value, repository-root-relative evidence paths and the verifying iteration (`08` for Claude/Codex, `11` for Grok/Cursor). Mixed facts are wholly `UNVERIFIED`, with the verified subset described in the value. The captured help/version/excerpt evidence is checked in under [`tests/testdata/cli-help/`](../tests/testdata/cli-help/); `TestFP7CatalogContract` validates structure and completeness, not vendor behaviour.

## Claude Code

**Version:** `2.1.282 (Claude Code)` — VERIFIED by [version](../tests/testdata/cli-help/claude-version.txt). Flag claims below: [help](../tests/testdata/cli-help/claude-help.txt); coordinator setup: [mcp add help](../tests/testdata/cli-help/claude-mcp-add.txt).

| Worker fact | Evidence and status |
|---|---|
| Headless | VERIFIED: `claude -p <prompt>` is noninteractive print mode; `--output-format text|json|stream-json` supported. Candidate use `--output-format json`. |
| Model | VERIFIED: `--model <model>`; Callsheet supplies it explicitly. Model availability/compatibility is UNVERIFIED. |
| Effort | VERIFIED: `--effort <level>`, help lists `low, medium, high, xhigh, max`. Which models accept each value is UNVERIFIED. |
| Approval | VERIFIED: `--permission-prompts none` in print mode denies anything that would prompt, while the configured permission mode still decides other calls. This is the preferred narrow candidate, leaving permission mode and sandbox untouched by Callsheet flags. It can refuse work requiring approval; no fallback to permission bypass. |
| Sandbox, Linux/macOS | UNVERIFIED runtime/OS behavior. Help does not prove OS containment or whether configured hooks affect it. Do not pass sandbox overrides or `--dangerously-skip-permissions`; the latter explicitly bypasses all permission checks in help. Candidate operator setting is vendor `settings.json` sandbox configuration; exact per-OS keys, availability and enforcement must be confirmed in 08. |
| Final message | VERIFIED help promises JSON single-result output; UNVERIFIED exact JSON schema, `result`/error fields and final-message behavior on failure. Iteration 08 must capture success/error fixtures before choosing a parser; do not return concatenated tool output as the final answer. |
| Exit codes | UNVERIFIED model-success, model-failure, authentication, denial and cancellation numeric exits. Offline help/version exited 0 only. Iteration 08 verifies actual mapping. |

Preferred worker candidate: `claude -p <prompt> --model <model> --effort <effort> --permission-prompts none --output-format json`. This is a proposal assembled from locally verified flags, not a qualified worker recipe.

Coordinator stdio registration syntax is VERIFIED: `claude mcp add --transport stdio --scope user callsheet -- callsheet mcp --plane <plane-url> --ca <ca-path>`. The Callsheet flags are reserved for iteration 07, not functional in the skeleton. Help supports scoped configuration and `--mcp-config` JSON files/strings. Candidate file format is `{"mcpServers":{"callsheet":{"command":"callsheet","args":["mcp","--plane","<plane-url>","--ca","<ca-path>"]}}}`; exact file schema/location is **UNVERIFIED** from help and must be validated in 08 (the registration command avoids guessing it).

- **MCP call timeout: UNVERIFIED.** Do not confuse startup timeout with tool-call timeout.
- **How to raise it: UNVERIFIED.** `MCP_TOOL_TIMEOUT` is a candidate to investigate, not an endorsed setting/value.
- **Progress extends calls: UNVERIFIED.** Require a delayed test tool with progress on/off; no assumption that notifications reset the hard deadline.
- **Coordinator runbook:** VERIFIED `--append-system-prompt <text>` exists; a coordinator launcher may read the owned UTF-8 runbook and pass its text as that argument. Automatic `CLAUDE.md` discovery is mentioned by help; precedence and exact loading scope are UNVERIFIED. The user's own interactive configuration is not modified by Callsheet. Instruction-file flags mentioned in prose but absent from the option list must be checked before use.

## OpenAI Codex

**Version:** `codex-cli 0.156.1` — VERIFIED by [version](../tests/testdata/cli-help/codex-version.txt). Sources: [root help](../tests/testdata/cli-help/codex-help.txt), [exec help](../tests/testdata/cli-help/codex-exec.txt), [mcp add help](../tests/testdata/cli-help/codex-mcp-add.txt). No official web lookup was performed because this job requires offline evidence.

| Worker fact | Evidence and status |
|---|---|
| Headless | VERIFIED: `codex exec`; initial prompt is an argument or stdin (`-`). `--json` emits JSONL events. |
| Model | VERIFIED: `-m/--model <model>`. |
| Effort | UNVERIFIED key/allowed values from these help pages. `-c key=value` TOML overrides are VERIFIED, but `-c model_reasoning_effort="<effort>"` is only the candidate mapping to verify in 08. Do not interpret generic override acceptance as proof of a recognized key. |
| Approval | VERIFIED root flag `-a/--ask-for-approval never` never asks and returns execution failures to the model. Candidate argv puts this before `exec`: `codex -a never exec ...`. Global/subcommand combination runtime acceptance remains UNVERIFIED until 08. |
| Sandbox, Linux/macOS | VERIFIED help distinguishes approval policy from `--sandbox` and says `--dangerously-bypass-approvals-and-sandbox` disables sandboxing. Preferred candidate omits both flags and preserves operator configuration. UNVERIFIED effective default, managed-profile precedence and OS enforcement. `~/.codex/config.toml` is a VERIFIED config source; exact per-OS sandbox keys and boundaries are UNVERIFIED here. `--approve-for-me` selects workspace-write automatic review and therefore is not a sandbox-preserving substitute. |
| Final message | VERIFIED: `-o/--output-last-message <file>` writes last agent message. Use a per-task file, read UTF-8 after process exit; missing/unreadable file is extraction failure, not empty success. UNVERIFIED whether every vendor error writes that file; 08 must test. `--json` is event logging, not an assumed final-answer schema. |
| Exit codes | UNVERIFIED model success/error/auth/cancel numeric behavior. Help/version 0 does not establish it. |

Candidate worker: `codex -a never exec --model <model> -c 'model_reasoning_effort="<effort>"' --json --output-last-message <task-final-file> -`, with prompt on stdin. No `--ignore-user-config`, sandbox mode override or dangerous bypass: these would disturb the operator's posture. The effort key and full argv require qualification before shipping the real adapter.

Coordinator registration syntax is VERIFIED: `codex mcp add callsheet -- callsheet mcp --plane <plane-url> --ca <ca-path>`. Candidate manual TOML format below is **UNVERIFIED** by these help pages (only the config path and generic TOML overrides are verified):

```toml
[mcp_servers.callsheet]
command = "callsheet"
args = ["mcp", "--plane", "<plane-url>", "--ca", "<ca-path>"]
# tool_timeout_sec = 120  # UNVERIFIED candidate; do not ship as established fact
```

- **MCP call timeout: UNVERIFIED.**
- **How to raise it: UNVERIFIED;** candidate `mcp_servers.callsheet.tool_timeout_sec` needs schema/runtime evidence in 08.
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

For coordinator waiting, use a local deterministic MCP test server (no model call when the client offers an offline protocol harness). Measure default tool timeout, configured increase, progress-enabled versus silent requests, and any absolute cap. Record supported setting syntax with evidence. Production wait cap in 06/07 must be below the tightest verified effective timeout with an explicit response margin; this catalog cannot supply that number yet. Do not set a multi-minute wait because an SDK contains a reset-timeout symbol. Iteration 07 may need to pull this qualification forward from 08/11; if unavailable it must expose the dependency as a blocker, not manufacture a timeout default.

The catalog is a fact sheet, not a claim that all four vendors currently meet the frozen requirements. Its explicit UNVERIFIED entries are the brief's authorized handoff to real-adapter qualification.
