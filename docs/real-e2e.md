# Real local end-to-end exercise

This runbook is the single entry point for Callsheet's manual real-worker exercise (design 12a-real-e2e, amendment A3). One command in one terminal runs the whole attempt: preflight, a local plane with three sidecars and the workspace hub, a headless Claude Code coordinator launched by the harness itself with your real configuration, four worker hops, your sign-off, evidence collection, the offline check and a post-run audit. It prints PASS or FAIL.

A passing run demonstrates this installation's mechanical workflow and the deterministic feature cases on this machine at that time, with your own agent configuration. It is not a vendor or timeout qualification, not a sandbox containment claim, and it never changes the support catalog. It never runs in CI: the command refuses to start when a `CI` variable is present (even empty), without `CALLSHEET_REAL_E2E=1`, when `CLAUDE_CONFIG_DIR` is set (even empty: unset it yourself; the harness never does), or on any OS but Linux.

## Prerequisites

- Linux (macOS live execution is refused: the Grok and Cursor worker postures are Linux only. Cursor's non-Linux refusal is `cursor worker execution is not qualified on this OS; consult the support catalog`. No jail and no non-Linux Cursor qualification has shipped).
- Built binaries: `go build -o /abs/path/callsheet ./cmd/callsheet` and `go build -tags reale2e -o /abs/path/reale2e ./cmd/reale2e`, run from inside the checkout.
- Go and Git on `PATH` (development prerequisites of the throwaway Go project, not Callsheet runtime requirements).
- Logged-in vendor CLIs at or above the runtime minimum versions, given by absolute path: Claude Code `2.1.299`, Codex `0.162.1`, Grok `1.0.52`, Cursor `2026.10.01-e373342` (the adapter auto-mode version policy, independent of the historical catalog identities; Cursor's date baseline does not overcome its non-Linux gate). The harness never logs in, reads credentials or rewrites profiles: resolve login, model access, a supported model or posture outside it. Worker tasks request each tool's auto mode. A Linux Cursor worker passes `--trust --auto-review`, builds the model argument as MODEL-EFFORT and puts the prompt in one positional argument (32 KiB, visible to process inspection). Passing `--trust` may cause Cursor to maintain `~/.cursor/cli-config.json` and other state outside the task directory; the harness reports that file and does not treat it as owner configuration. The harness does not edit vendor configuration or retry a denial. `--trust --auto-review` is not a confinement claim.
- Costs and deadlines (policy, not measurements): four paid authentication preflights of at most 2 minutes each and four paid feature tasks of at most 10 minutes each; your decision within 20 minutes; the whole attempt within 90 minutes of preflight start (including the first fingerprint), then at most 2 minutes of cleanup, the final fingerprint and the audit; all final validation within 60 seconds. The headless coordinator adds vendor-dependent turns and tool calls. No retries, no fix loops, no token-price ceiling.
- Protected roots: each `--protect-root` directory (repeatable, absolute, existing, not nested in another, not inside the evidence or runtime directory) has every Git repository below it fingerprinted before and after the run. Discovery stops at each repository's working tree, whose status and tracked state (one streamed digest each) cover everything below it; a nested repository that its parent repository ignores is not separately protected. Discovery descends at most `--protect-depth N` directory levels below each root (default 3): deeper repositories and trees, such as a Go module cache, are not walked and not protected, and the report states the depth. A directory whose `.git` Git rejects is recorded by that entry's metadata, and any change to it fails. There is no default. With no `--protect-root`, only the four vendor homes are fingerprinted and the report says `No repository root was protected`.
- A quiescent owner environment: other Claude, Codex or Grok sessions writing configuration or repositories during the run make the comparison fail.
- The existing parent of a new evidence directory inside the checkout's ignored `design/` tree, for example `mkdir -p design/real-e2e-runs`. Evidence is bounded (64 MiB of text, 4 MiB of evidence repository) and stays local and private.

## Coordinator session

The coordinator runs with your user instructions, settings, permissions, skills, agents, plugins, hooks and login exactly as they are. The harness writes no agent configuration or settings file (the runtime MCP configuration and the evidence are its own files). Setting `CALLSHEET_REAL_E2E=1` is your authorization of session-only flags, identical in both coordinator phases:

- `--settings '{"sandbox":{"enabled":false}}'` merges over your settings for this session only. It is needed only because this exercise's plane listens on host loopback, which a sandboxed Bash command cannot reach (see [coordinator.md](coordinator.md)); it is not a general Callsheet requirement.
- `--permission-mode auto` requests Claude's automatic permission mode for this session only. Auto is available unless an administrator sets `permissions.disableAutoMode`. Supported models include Opus 4.6+ and Fable. The classifier pauses after 3 consecutive or 20 total blocks (owner-recorded, not a fresh upstream verification). The harness never edits that setting, never resets the classifier and never retries a denial. Exit 0 does not prove every requested action succeeded. That the installed CLI reports `permissionMode` `auto` for this exact recipe is unverified until a coordinator-run two-phase probe; a missing or different value fails the check, and the check does not infer it from the launch flags.
- `--permission-prompts none` denies anything that would prompt. The permission mode still decides everything else. Print mode cannot answer a prompt.

There is no `--allowed-tools` list, no permission bypass, no second list and no tool filtering, and none of your rules or denies is removed: your inherited permissions stay active. Auto is a permission policy, not a containment claim. A bundle recorded with the earlier 15-rule `--allowed-tools` recipe is not evidence of this recipe. The checker rejects that launch shape under setup (`launch_shape_invalid`). Check a historical bundle with the revision that recorded it.

The command prints these flags and prerequisites before any vendor runs, and the same auto-mode line again before the coordinator's first phase. A `reale2e` or runtime path containing `*`, `?`, `[`, `]`, `(`, `)`, a comma, CR, LF or NUL, or any character that would need shell quoting (a space or a quote, for example), is refused before any vendor starts (`permission_rule_invalid`). Each hop's background wait is exactly `R wait-task --run D --task TASK_ID --hop H` (H the hop): one simple command. Dropping the allow-list does not accept a redirect, a `;`, a `|` or a quote. No Bash line of the coordinator prompt contains a redirect, `;`, `&&`, `|` or a quote.

The helpers run in the coordinator's Bash, read and write only fixed files of the live runtime, follow no symlink and make one exchange with the supervisor:

- `reale2e observe --run D --task ID --hop H` reports a task the coordinator admitted for hop H.
- `reale2e wait-task --run D --task ID --hop H`, once the supervisor confirms that ID is hop H's accepted task, runs `callsheet task wait ID --until-done --json` itself with the Callsheet executable, plane URL and CA from the runtime's run file, writes its stdout, stderr and exit status to the hop's `result.json`, `error.txt` and `exit-code` under `D/coord/waits/NN`, prints the captured wait output and exits with the wait's status (1 on a supervisor refusal, before any wait runs; 2 on a usage or file error, with no `exit-code` written).
- `reale2e review-material --run D` prints the coder's `design.md`, `main.go` and `main_test.go` from the coordinator's `ws_pull`, each with its SHA-256, once they match the admitted coder commit; the coordinator appends them to the code reviewer's goal.

A denied tool call (for example a rule of yours that denies a redirect or a helper) is never retried or widened: it fails the attempt with `coordinator_tool_denied` and its command, even when Claude exits 0. Resolve it in your own configuration and rerun.

## Roles, models and efforts

The shipped flow [examples/real-e2e/flow.json](examples/real-e2e/flow.json) is the default:

| ID / hop | Sidecar / adapter | Model | Effort |
|---|---|---|---|
| designer | codex | gpt-6-astra | low |
| design-reviewer | claude | claude-fable-5-1 | low |
| coder | claude | claude-opus-5-5 | high |
| code-reviewer | grok | grok-4.7 | high |

Every role has concurrency 1 and timeout 10m. To override a model or effort, copy `flow.json` to a local file, change only `model` or `effort`, and pass it with `--flow /abs/path/flow.json`; nothing changes after startup. The report labels such a run `override-flow`, and its PASS never claims your default pairs passed.

Shipped manuals and prompts: [designer instruction](examples/real-e2e/designer-instruction.md) and [runbook](examples/real-e2e/designer-runbook.md); [design-reviewer instruction](examples/real-e2e/design-reviewer-instruction.md) and [runbook](examples/real-e2e/design-reviewer-runbook.md); [coder instruction](examples/real-e2e/coder-instruction.md) and [runbook](examples/real-e2e/coder-runbook.md); [code-reviewer instruction](examples/real-e2e/code-reviewer-instruction.md) and [runbook](examples/real-e2e/code-reviewer-runbook.md); the coordinator's [instructions](examples/real-e2e/coordinator-prompt.md), [phase 1 prompt](examples/real-e2e/coordinator-phase-1.md) and [phase 2 prompt](examples/real-e2e/coordinator-phase-2.md).

A reusable development-flow example is available at [examples/dev-flow/README.md](examples/dev-flow/README.md). The current automated exercise still uses its old manuals until the migration slices ship. The role manuals linked above are legacy four-hop exercise content and remain what this exercise loads.

The feature asks the designer, coder and reviewers for an optional `--name NAME` flag on a tiny greeting CLI, with ten fixed cases (`no_args`, `name`, `spaces`, `unicode`, `whitespace`, `empty_name`, `unknown_flag`, `missing_value`, `positional`, `help`), each a `TestRun/<case-id>` subtest.

## Run

```sh
CALLSHEET_REAL_E2E=1 /abs/path/reale2e run \
  --callsheet /abs/path/callsheet \
  --claude /abs/path/claude --codex /abs/path/codex --grok /abs/path/grok \
  --protect-root /opt/gitspace --protect-root /opt/gospace \
  --evidence /abs/callsheet/design/real-e2e-runs/NEW-ATTEMPT
```

The two `--protect-root` values are an example (the owner's); substitute your own roots or omit both flags for vendor-home-only protection.

## Sign-off

After the design review approved and the coordinator's first phase ended, the same terminal shows the full reviewed `design.md` and `design-review.md` with their paths and SHA-256 digests, then asks exactly:

```
Approve this design and start coding? [yes/no]
```

Type `yes` or `no` (surrounding spaces are ignored; anything else asks again). Yes records your decision and automatically resumes the same coordinator session for coding and the code review. No, end of input or 20 minutes without an answer ends the attempt without coding.

## Reading the result

The final output prints PASS or FAIL for the mechanical and deterministic criteria and, separately, `Result judgments: needs owner/coordinator review`, then links `report.txt` and `audit.md` in the evidence directory.

- `report.txt` lists every criterion with its fixed failure codes (or, when the attempt stopped before a criterion's step could run, `not_reached` with the stopping cause, so an early stop reports only its real cause; the run still fails), whether each authentication preflight echoed `CALLSHEET_AUTH_OK` (information only: any complete, non-empty answer from the configured pair authenticates), the protected roots, the protected changed paths (any one fails the run) and the tool-maintained changes (report-only), counted as added, changed and deleted per vendor and top-level directory (`.` for files directly in the vendor home).
- `audit.md` (and `audit.json`) walks every step in order: preflight, deployment, both coordinator phases, each hop's dispatch, wait and result, your sign-off, the final tests and CLI cases, cleanup and the environment comparison, each `correct`, `incorrect` or `unobserved` with evidence pointers (a step the attempt never reached is `unobserved` with `not reached: <cause>`). Judgment rows (does the design cover each case, is the review meaningful) stay `unobserved` until reviewed.

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

Claude's state files hold per-project tool grants and MCP-server selections, but a change to them does not fail the run. Codex appends a `[projects."<dir>"] trust_level` entry to `~/.codex/config.toml` for every directory it works in, so the whole file is tool-maintained like Claude's `.config.json`; your `AGENTS*.md`, `instructions.md` and `prompts/` stay strict. Each protected repository's status digest leaves out its untracked top-level `.claude/` directory, which may hold Claude Code sandbox placeholders; its tracked content and HEAD stay strict. Claude refreshes the `lastUpdated` timestamps in `~/.claude/plugins/known_marketplaces.json` whenever it uses its plugin marketplaces, so that file is tool-maintained; the other plugin configuration files (`installed_plugins.json`, `blocklist.json`, `config.json`) stay strict. A symlink in a vendor home or in the directories scanned above your repositories is checked as the link itself (where it points, not what it points at) and never followed. The harness never repairs, moves or deletes any of your files.

Right after the run, review `audit.md` with your external dev-flow coordinator: it checks every judgment row and any disputed mechanical verdict and appends corrections to a separate `audit-review.md` (run ID, audit and report SHA-256, row ID, original and revised verdict, who and why, evidence references). Never edit `audit.json`, `audit.md`, the report or the evidence; a correction cannot turn a FAIL into a PASS. `reale2e check --evidence DIR` re-validates a bundle offline (optional).

## Rerun

Resolve the reported cause (login, model access, a permission or configuration incompatibility, a protected change), then run the same command with a new evidence directory. Every attempt has its own run ID, runtime, workspace and coordinator session; nothing is resumed or retried, and earlier bundles keep their result. If cleanup could not prove every process stopped, the runtime is kept and the supervisor prints the process IDs to check and stop before rerunning.

## Development full-flow command (incomplete, unverified)

`reale2e run-dev-full` is a separate development-only entry point. It is not a PASS, and `reale2e check` does not accept its evidence. The default `reale2e run` command, its flow file, the legacy manuals, and `reale2e check` are unchanged.

```sh
CALLSHEET_REAL_E2E=1 /abs/path/reale2e run-dev-full \
  --callsheet /abs/path/callsheet \
  --claude /abs/path/claude --codex /abs/path/codex --grok /abs/path/grok \
  --protect-root /opt/gitspace \
  --evidence /abs/callsheet/design/real-e2e-runs/NEW-DEV-ATTEMPT
```

The same absolute `--flow` override may change only model or effort and is labeled `override-flow` unless every selection matches the default five-role table. Live startup uses the same gate as `run` (explicit opt-in, Linux, no `CI`, no `CLAUDE_CONFIG_DIR`). Adding the command does not authorize a live invocation during this slice.

On an unverified completion the command prints `EXECUTED — UNVERIFIED: slice 2d.3 verification required` and exits 3. A stop or a failure exits 1. Usage, startup, and schema errors exit 2. Exit 0 is never returned. The evidence file `dev-execution.json` uses schema `callsheet-dev-full-execution/v1` with outcome `executed-unverified`, `stopped`, or `failed`. It is not a report and it does not contain a PASS. `reale2e check` rejects that manifest as an unsupported schema (exit 2) before it interprets any task.

The exercise loads the shared manuals in [examples/dev-flow/](examples/dev-flow/README.md) plus the scenario files in [examples/real-e2e/dev-full/](examples/real-e2e/dev-full/). Role methods are not copied into the exercise prompts. Five authentication preflights replace the four-hop set. The happy path is four workflow calls. Loops allow up to twelve ordinary-pair calls and two recovery calls, so at most 19 worker calls including the five preflights, plus vendor-dependent coordinator calls in up to four phases. There is no extra paid coordinator preflight and no token ceiling. Bounds stay 2m/preflight, 10m/task, 90m/attempt, 20m/owner window, 60s reserved for later final validation, and 2m/cleanup. The 90-minute attempt deadline stops first if the task-time sum would exceed it.

Owner answers are `yes <generation>` or `no <generation>`, and a clarification is `answer <generation> <text>` (one line, at most 8192 bytes). A plain `yes` does not approve a later design. Before each decision the supervisor prints the approved `design.md`, `revision-history.md` and `design-review.md` with their SHA-256 values. Input typed while no decision window is open is discarded, including a line the supervisor's reader had begun or finished reading before the window opened, and a line naming another generation decides nothing and does not extend the window. Bytes the operating system still buffers when a window opens, unread by the reader (a canonical terminal can hold a whole line until Enter), cannot be told apart from bytes typed after it opened.

The coordinator pushes the seed itself; `dev-next` exposes no plan until the workspace's main branch holds the seed commit. Once a plan's task is admitted, `dev-next` never names that plan again: it admits the consumed result and names the successor, or answers `pending` until it can. Every control request (`dev-next`, `dev-observe`, `dev-wait-task`) is served by the supervisor's single loop. Each workflow task's wait counts only with its exact background command, its own completion notification and a following `task_show` of that task, and the captured wait exit 0. Results are pulled with the product's transfer code into a new ordinary inspection repository and admitted against each role's allowlist and artifact contract. A coordinator phase ends at an owner or clarification gate with the exact line `gate=<owner|answer> generation=<N>`. The bundle keeps every plan, dispatch document, task observation, wait, admission, transition, decision, receipt, phase stream and launch record, the cleanup record and an evidence repository with `refs/heads/seed` and `refs/heads/task-NN`.
