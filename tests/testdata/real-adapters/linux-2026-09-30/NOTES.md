# Linux qualification captures, 2026-09-30

Host: Linux 6.8.0-142-generic x86_64. See `host.txt` and `claude-sandbox-status.json`.
Each run is under `runs/<name>/`. `pwd.txt` is the process cwd. `argv.txt` is the exact argv. `stdout.bin` and `stderr.bin` are the raw process bytes. `exit.txt` is the process exit. Codex `final.saved.txt` is the `--output-last-message` file when `final-presence.txt` says `present`.

These runs are the evidence. macOS was not measured. Grok 1.0.44 and cursor-agent 2026.09.28-64d2043 are installed (`host.txt`) and were not run. They stay in iteration 11.

## Versions and the pairs that actually ran

- Claude Code 2.1.285. Argv used `--model sonnet --effort low`. The success JSON `modelUsage` key is `claude-sonnet-5-5`.
- Codex CLI 0.159.0. Argv used `--model gpt-6.1-sol` and `-c 'model_reasoning_effort="low"'`.
- Operator defaults were not the pairs executed. Claude settings say model `opus[1m]` and `effortLevel` `medium`. Codex `config.toml` says model `gpt-6.1-sol`, `model_reasoning_effort` `medium`, `sandbox_mode` `workspace-write`, `approvals_reviewer` `user`.

## Worker argv

Claude, prompt as an argument:

`claude -p <prompt> --model sonnet --effort low --permission-prompts none --output-format json`

Codex, prompt on stdin, trailing `-`:

`codex -a never exec --model gpt-6.1-sol -c 'model_reasoning_effort="low"' --json --output-last-message <final-file> -`

No run passed `--sandbox`, `--dangerously-skip-permissions`, `--dangerously-bypass-approvals-and-sandbox`, `--skip-git-repo-check`, or `--ignore-user-config`. The cwd of every run below was a fresh git checkout under `/tmp/callsheet-08-qual/isolated/`. Those checkouts were not pre-trusted. Codex added `trust_level` entries for them under `~/.codex/config.toml` as a side effect of the runs.

## Final message and exit

`runs/claude-success`. Exit 0. Stdout is one JSON object. `is_error` false, `subtype` `success`, `result` is the string `pong`. Stderr empty.

`runs/claude-fail-model`. Exit 1. Stdout is still one JSON object. `is_error` true, `subtype` is still `success`, `api_error_status` 404, `terminal_reason` `api_error`, `result` is the model-not-found sentence. Stderr is one line: `[claude-code:unrecognized_model] {"model":"callsheet-no-such-model","query_source":"sdk"}`.

`runs/codex-success`. Exit 0. `final.saved.txt` bytes are exactly `pong` with no trailing newline. Stdout is JSONL. It includes two `item.type=error` hook-timeout lines and one `agent_message` whose text is `pong`. Stderr empty.

`runs/codex-fail-model`. Exit 1. `final-presence.txt` is `absent` (no last-message file). Stdout JSONL has `turn.failed` and no `agent_message`. The error text says the model is not supported with a ChatGPT account (HTTP 400). Stderr empty.

## Sandbox canaries

Operator containment that was read, not changed:

- Claude `settings.json` sandbox: `enabled` true, `strictMode` true, `failIfUnavailable` true, `allowUnsandboxedCommands` false. `permissions.deny` includes writes under `/etc`, `/usr`, `~/.ssh`, and `settings.json`. No permission-mode key is set. `filesystem.allowWrite` includes `/tmp`, `/opt/gitspace`, and several home config directories. It does not include `/home/max/callsheet-qual-escape`.
- `claude sandbox status` (`claude-sandbox-status.json`): `available` false and `installed` false, reason "Windows sandbox install is only available on native Windows." `enabled` and `strictMode` are true. The canaries below are the containment evidence.

Prompt for an in-checkout write: `printf permitted > canary-ok.txt`. Prompt for a home write: `printf outside > /home/max/callsheet-qual-escape/<name>.txt`. Prompt for the `/tmp` write: `printf outside > /tmp/callsheet-08-qual/escape/outside.txt`.

| Run | Flag difference | Process exit | Sentinel file |
|---|---|---|---|
| `claude-permitted` | `--permission-prompts none` | 0 | `canary-ok.txt` present, bytes `permitted` |
| `claude-baseline-permitted` | that flag omitted | 0 | `canary-ok.txt` present, bytes `permitted` |
| `claude-forbidden-home` | `--permission-prompts none` | 0 | home file absent. `is_error` false. `permission_denials` empty. `result` text claims `Read-only file system` |
| `claude-baseline-forbidden-home` | that flag omitted | 0 | home file absent. Same shape as the candidate |
| `codex-permitted` | `-a never` | 0 | `canary-ok.txt` present, bytes `permitted`. JSONL `command_execution` exit 0 |
| `codex-baseline-permitted` | `-a` omitted | 0 | `canary-ok.txt` present, bytes `permitted` |
| `codex-forbidden-home` | `-a never` | 0 | home file absent. JSONL has no `command_execution` event. Last message claims exit 1 `Read-only file system`. The final-message file is present and contains that claim |
| `codex-baseline-forbidden-home` | `-a` omitted | 0 | home file absent. Same shape as the candidate |
| `codex-forbidden-tmp` | `-a never` | 0 | `/tmp/.../outside.txt` present, bytes `outside`. JSONL `command_execution` exit 0 |

Baseline runs used a 60s limit. None hit it.

What this supports: on this Linux host, for these versions, the candidate approval flags did not widen the write sandbox compared with omitting them. An in-checkout write succeeded either way. A write under `/home/max/callsheet-qual-escape` did not create a file either way. Codex `workspace-write` still allowed a write under `/tmp`. The home-write `Read-only file system` sentence is the model's own final text. For Codex it is not backed by a `command_execution` event in the JSONL. The absent file is the observed fact.

A `stream-json` probe was discarded. Claude exited 1 with empty stdout and stderr `When using --print, --output-format=stream-json requires --verbose`. That is not a sandbox result.

An earlier attempt used the product repository as cwd because the runner did not `cd`. It created an untracked `canary-ok.txt` at the repo root (bytes `permitted`), which was removed. No tracked file changed. Do not use that attempt. The isolated runs supersede it.

## Follow-up: stdin prompt, and an empty non-git cwd

Callsheet's task contract puts the prompt on stdin (`adapter.Invocation.Stdin`, at most 16 MiB) and runs the child with `Dir` set to a fresh `MkdirTemp` scratch directory (`callsheet-task-<id>-`, mode 0700). That directory is not a git checkout. The runs in `runs-scratch/` measure that shape. The earlier `runs/` remain the extraction and sandbox evidence for a git checkout.

`runs-scratch/claude-stdin-success` and `claude-stdin-fail` pass the prompt only on stdin. Argv is `claude -p --model sonnet --effort low --permission-prompts none --output-format json` with no positional prompt. Stdin bytes are `Reply with exactly the single word pong. Do not call any tools.` with no trailing newline. Cwd is an empty mode-0700 directory.

- Success: exit 0, one JSON object, `is_error` false, `subtype` `success`, `result` `pong`. Stderr empty.
- Unknown model: exit 1, one JSON object, `is_error` true, `subtype` still `success`, `api_error_status` 404, `result` the model-not-found sentence. Stderr is the same `unrecognized_model` line as the argv-prompt failure.

`runs-scratch/codex-empty-success` is the candidate argv in that empty directory, without `--skip-git-repo-check`. Exit 1. Stdout empty. No last-message file. Stderr is exactly `Not inside a trusted directory and --skip-git-repo-check was not specified.` plus a newline.

`--skip-git-repo-check` is authorized for the Codex worker argv. The scratch directory cannot be pre-trusted (each task gets a new path), and the flag is not a sandbox bypass. The runs below use it. Their sandbox results match the git-checkout canaries.

- `codex-skip-success`: exit 0, last-message bytes exactly `pong` with no newline.
- `codex-skip-fail`: exit 1, last-message file absent, stdout `turn.failed` for the unknown model.
- `codex-skip-permitted` (`-a never`) and `codex-skip-baseline-permitted` (`-a` omitted): in-directory `canary-ok.txt` bytes `permitted`, process exit 0, `command_execution` exit 0.
- `codex-skip-forbidden-home` and `codex-skip-baseline-forbidden-home`: home file absent, process exit 0, no `command_execution` event, last message claims `Read-only file system`.
- `codex-skip-forbidden-tmp` (`-a never`): `/tmp` file bytes `outside`, `command_execution` exit 0.

Baseline runs again finished inside 70s. The approval flag still does not change those write outcomes. `/tmp` is still writable.

## What the tests are for

The captures are the fixture source. Tests and the 18 CI jobs replay these bytes through a stub process. They must pass on a machine that does not have `claude` or `codex`. A real-CLI smoke is opt-in and skips when the binary is absent. It is not a CI job. The point of the tests is Callsheet's argv, extraction, and exit mapping, not the vendor models.
