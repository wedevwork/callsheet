# Real worker adapters: Claude and Codex

Iteration 08 adds production `claude` and `codex` worker adapters beside the
test-only `fake`. A worker runs a goal-and-answer task through the vendor CLI
it is explicitly given, with the prompt on stdin, and returns the vendor's
final message; no workspace, checkout or pushed result is involved. The
qualification evidence and every per-fact status are in the
[support catalog](support-catalog.md); this page is the operator procedure.

## What is qualified

| Adapter | Version (`--version` output) | Model | Effort | Recipe (arguments after the executable) |
|---|---|---|---|---|
| `claude` | `2.1.285 (Claude Code)` | `sonnet` | `low` | `-p --model sonnet --effort low --permission-prompts none --output-format json` |
| `codex` | `codex-cli 0.159.0` | `gpt-6.1-sol` | `low` | `-a never exec --model gpt-6.1-sol -c model_reasoning_effort="low" --json --skip-git-repo-check --output-last-message <scratch>/callsheet-final.txt -` |

The recipes were captured on Linux x86_64 on 2026-09-30
([host](../tests/testdata/real-adapters/linux-2026-09-30/host.txt),
[captures](../tests/testdata/real-adapters/linux-2026-09-30/)). Other
versions, models or efforts are refused, not approximated: the worker's
`--version` probe fails for an unqualified version (the role is not ready),
and any other model/effort is refused at role registration, at every ready
check and at task start. Only role registration returns the detail:
`invalid_argument` with reason `probe_failed` and the message `the <id>
model/effort selection is not qualified; supported: model <m>, effort <e>`.
Every ready check revalidates the selection and leaves such a role not
ready. A task-start refusal of an unqualified effective model/effort (a
per-task override) is a generic `start_failed` and launches nothing; its
detail is only in the sidecar's own log, a warning `task model/effort
selection not qualified` with `reason=selection_not_qualified`. Operator
defaults such as Claude's `opus[1m]`/`medium` or Codex's `medium` are never
substituted, and no paid call is made to discover compatibility. Adding a
version or pair needs new evidence and a table and catalog change.

macOS is **not** qualified. The same adapters may be enabled there
explicitly; the sidecar then logs, once at start, that the recipe is
Linux-qualified and that macOS vendor sandbox and exit behavior are
UNVERIFIED. See the catalog's qualification handoff for what to capture.

## Enabling a worker

Give the absolute path of each CLI on every `sidecar run` (nothing is
persisted or sent to the plane, and PATH is never searched):

```sh
callsheet sidecar run --claude-adapter /opt/vendor/bin/claude --codex-adapter /opt/vendor/bin/codex
```

An omitted flag disables that adapter on the node whatever is installed; an
empty, relative or repeated flag is a usage error (exit 2). The probe runs
`<path> --version` in the sidecar's state directory within one second on
every ready check and accepts exactly the qualified version line; bounded
stderr (environment warnings) is allowed and never logged. A probe checks
invocability, not authentication, provenance or the configured sandbox: the
operator controls the installed binaries, and replacing one between a probe
and a launch is not detected.

Register roles with the qualified pair, for example:

```sh
callsheet role add coder-claude --name coder --node <node-id> --adapter claude \
  --instruction /srv/manuals/instruction.md --runbook /srv/manuals/runbook.md \
  --model sonnet --effort low --concurrency 1 --plane <plane-url> --ca <ca-path>
```

Run sidecars as a dedicated OS user. Containment is whatever the vendor's
configured sandbox provides (on the recorded host, host reads and `/tmp`
writes were allowed); Callsheet creates no sandbox, never selects or relaxes
a sandbox profile, never edits vendor settings or trust entries, and never
retries a denied task with broader permissions. Inspect the effective
configuration with the vendor's own tools, for example `claude sandbox
status` and your Claude `settings.json`, or Codex's `config.toml`.

## How a task runs

Each task gets a fresh private scratch directory (mode 0700, not a git
checkout) as its working directory, in its own process group led by the
task guardian. The composed prompt is the only stdin; it is never an argument
or a prompt file.

- **Claude**: the final message is the `result` string of the single JSON
  object on stdout, exactly as decoded (an empty string is a valid answer),
  also when `is_error` is true. `subtype` and other metadata never decide the
  task's state.
- **Codex**: the final message is the bytes of `callsheet-final.txt` in the
  scratch directory, read after the whole process group is gone and before
  the directory is removed, through a retained directory handle without
  following links. The `--json` event stream is kept in the task log and
  never read as the answer.

Stdout input is bounded to 8 MiB and the answer to 64 KiB (longer answers are
cut at a UTF-8 boundary and marked truncated).

A task's state is the observed process result, as before: exit 0 is
`succeeded` and a nonzero exit `failed` with that exit code, whatever the
answer says; cancellations, timeouts and lost workers keep their established
meaning (a cancelled Codex task whose group is proved gone still reports a
safe final file as its partial answer). A `null` final message means no
answer was extracted, **not** an empty answer. When extraction fails the
sidecar logs one structured warning and appends one line to the task's log
(visible through `callsheet task logs` and `task_logs`):

```text
callsheet: <adapter> final-message extraction failed (<code>)
```

where `<code>` is `invalid_final_output`, `final_output_too_large`,
`final_output_missing`, `final_output_unsafe`, `final_output_unreadable` or
`final_output_unavailable` (the group's cleanup could not be confirmed, so
the file was not read). A missing answer on a nonzero exit keeps that exit.
Denial prose in an answer ("Read-only file system") is the model's text, not
evidence that a tool ran or failed.

## Offline proof in CI

CI never runs a vendor CLI or a model. The function tests replay the
checked-in captures byte for byte through a stub enabled with the real
`--claude-adapter` and `--codex-adapter` flags, against a real plane,
sidecar and `callsheet mcp`, with an isolated HOME and a PATH of
launch-recording traps. They prove Callsheet's argv, stdin, extraction and
exit handling of the captured contract, not that vendor behavior is stable.

## Opt-in real smoke

On a machine with the qualified CLIs, logged in as the operator, run the
paid smoke by hand (it is not a CI job):

```sh
CALLSHEET_REAL_ADAPTER_SMOKE=1 \
CALLSHEET_CLAUDE_PATH=/absolute/path/to/claude \
CALLSHEET_CODEX_PATH=/absolute/path/to/codex \
go test -tags=realadaptersmoke ./tests/smoke -run '^TestRealWorkerSmoke$' -count=1 -timeout=5m
```

Without the build tag the package does not exist for `go test ./...`.
Without `CALLSHEET_REAL_ADAPTER_SMOKE=1`, or with `CI` set, it skips before
touching anything. An unset or nonexistent path skips only that vendor; a
present but unusable or unqualified binary, and any authentication, model or
sandbox refusal, fails. For each vendor it starts a local plane and sidecar
with the explicit path, registers the qualified pair, dispatches the captured
no-tools prompt and expects exit 0 and the final bytes `pong` within two
minutes (on timeout it cancels the task and waits for its cleanup). A skipped
smoke is not qualification, and a passing one is not the remote acceptance
below.

## Remote acceptance (M3)

M3 has not been demonstrated. It is demonstrated only when the following
manual procedure passes on real machines; the checked-in captures and the
local smoke are qualification inputs, not a remote coordinator run. Record
the results beside the evidence when it is done.

1. Use the existing coordinator setup ([coordinator guide](coordinator.md)):
   a plane, and a real coordinator CLI registered with `callsheet mcp`.
2. On two other enrolled worker machines run `callsheet sidecar run` with
   `--claude-adapter` on one and `--codex-adapter` on the other (explicit
   absolute paths), and register one role each with the exact qualified
   pair and existing manuals.
3. From the coordinator, dispatch one no-workspace goal-and-answer task to
   each role (no workspace or base fields). Follow each with short
   `task_wait` calls, repeated as needed under the unchanged interim wait
   budget; do not change the 07b timeout policy or run `mcpqual` for this.
4. Inspect `task_show` and `task_logs` for each task. Record the
   coordinator's and each worker's OS, architecture and CLI versions, the
   node and task IDs, the final message bytes, the process exits, and that
   the task completed and its scratch directory and process group were
   cleaned up.
5. Register (or dispatch with an override) an unqualified model on one
   worker and record the unqualified-selection refusal.
6. Confirm no result was pushed and no vendor session was resumed or reused.

Known issue (found while building iteration 08's offline rig, not changed
by it): the plane stamps each task start with the registry-wide roles
revision but sends a new roles snapshot only to the node whose role changed.
After a role change on one worker, a worker whose roles were registered
earlier refuses every start as `role_changed` until its own roles change or
its sidecar reconnects (a restart installs the current snapshot). Until that
is fixed, register the roles of step 2 and then restart the sidecar of every
worker except the one registered last before dispatching, and record that
you did so.

Linux worker evidence supports the initial deployment; record macOS workers'
observations separately, as macOS remains unverified.
