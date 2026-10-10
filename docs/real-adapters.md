# Real worker adapters: Claude, Codex, Grok and Cursor

Iteration 08 adds production `claude` and `codex` worker adapters beside the
test-only `fake`. A worker runs a goal-and-answer task through the vendor CLI
it is explicitly given, with the prompt on stdin, and returns the vendor's
final message; no workspace, checkout or pushed result is involved. The
qualification evidence and every per-fact status are in the
[support catalog](support-catalog.md); this page is the operator procedure.
Iteration 11 adds `grok`, which runs on Linux only with narrow limits, and
`cursor`, which is registered and version-probed but refused for task
execution; see [Wave 2: Grok and Cursor](#wave-2-grok-and-cursor).

## Selection and versions

Callsheet validates its own input and leaves model compatibility to the
vendor (requirement Q12, design 12a-worker-selection). Every role, and every
dispatch override, names an explicit **model**: free text of 1 to 1024
bytes of UTF-8 without control characters, passed unchanged as one argument
(spaces, punctuation and leading hyphens included; no shell). Callsheet
keeps no model list and never infers, normalizes, substitutes or defaults a
model. The **effort** must be one of the adapter's efforts (case-sensitive):

| Adapter | Allowed efforts | Source of the set |
|---|---|---|
| `claude` | `low`, `medium`, `high`, `xhigh`, `max` | the captured `--effort` enumeration ([help](../tests/testdata/cli-help/claude-help.txt)) |
| `codex` | `low`, `medium`, `high`, `xhigh`, `max`, `ultra` | the coordinator's dated observation and the owner's decision of 2026-10-10 (below; no committed capture) |
| `grok` | `low`, `medium`, `high`, `xhigh` | the captured accepted-effort list ([NOTES](../tests/testdata/real-adapters/linux-2026-10-04/NOTES.md)) |
| `cursor` | `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max` | the listed None, Minimal, Low, Medium, High, Extra High and Max model variants ([models](../tests/testdata/real-adapters/linux-2026-10-04/cursor-models.txt)); Extra High is `xhigh` |

The Codex set records a coordinator observation, not a checked-in
capture: on 2026-10-10 the installed Codex 0.160.0's model cache offered
`low` through `ultra` for gpt-6.1-sol, gpt-6-astra, gpt-6-sol, gpt-5.6-sol and
gpt-5.6-terra, `low` through `max` for gpt-6-luna, gpt-5.6-luna, gpt-reserve
and codex-auto-review, and `low` through `xhigh` for gpt-5.5. The owner
approved the union, not a per-model table, so Callsheet never reads that
cache and keeps no per-model effort table for any vendor.

A syntactically valid unknown model, or a valid pair that the vendor does
not support, passes Callsheet's validation without a warning: the vendor
refuses it when the task runs, and the task reports that through its
existing result (process exit, final message and logs; there is no
separate compatibility error or retry with another selection). Some
recipes exit 0 with refusal text, notably Grok's, so exit 0 does not prove
that the requested model ran. An effort outside the adapter's set, an empty
or invalid model and an empty explicit override are refused before anything
runs: at role registration and dispatch by the plane (`invalid_argument`,
for example `effort is not allowed for adapter claude; allowed: low,
medium, high, xhigh, max`; a refused dispatch creates no task), and on the
worker (role registration and every ready check refuse it with reason
`probe_failed` and a static message naming the field). A task start whose delivered
selection is invalid, which only a plane of another policy can send, is a
generic `start_failed` that launches nothing; its detail is only the
sidecar's warning `task model/effort selection invalid` with
`reason=selection_invalid`, never the submitted values. An override applies
to one task only: an omitted override field inherits the role's value, and
the task's effective model and effort are shown with the task. Operator
defaults such as Claude's `opus[1m]`/`medium` or Codex's `medium` are never
substituted, and no paid call is made to discover compatibility.

Each worker executable is eligible at or above its **minimum version**,
the version its capture recorded, with no warning for a newer version:

| Adapter | `--version` line (literal spaces) | Minimum | Ordering |
|---|---|---|---|
| `claude` | `SEMVER (Claude Code)` | `2.1.285 (Claude Code)` | SemVer precedence |
| `codex` | `codex-cli SEMVER` | `codex-cli 0.159.0` | SemVer precedence |
| `grok` | `grok SEMVER (HASH) [CHANNEL]` | `grok 1.0.46 (2765805b9442) [stable]` | SemVer precedence; the hash and channel never order |
| `cursor` | `YYYY.MM.DD-HASH` | `2026.10.01-e373342` | the calendar date; a same-date version with another hash is equal and accepted |

`SEMVER` is `major.minor.patch` with an optional `-prerelease` and `+build`:
the core compares numerically, a release sorts above its prereleases (so
`codex-cli 0.159.0-rc.1` is below the minimum and `0.160.0-rc.1` above it),
and build metadata never orders. After at most one trailing newline, the
whole line must be at most 256 bytes of printable ASCII in exactly that
grammar; Cursor's date must be a real Gregorian date. An older version,
recognizable but malformed or multi-line output, and another program's
output are refused with distinct messages (`has an older <id> version;
minimum <v>`, `has an invalid <id> version; expected an orderable version
at or above <v>` and `is not the <id> CLI (unexpected version output)`),
which makes every role of that adapter not ready until an eligible version
is installed. This is an eligibility policy, not vendor authentication and
not a qualification of every later release; ready checks never test
authentication, model existence or pair compatibility.

Example selections (requested selections, not observed successful
executions or defaults): Codex `gpt-6-astra` with `low`, Claude
`claude-fable-5-1` with `low`, Claude `claude-opus-5-5` with `high`, and Grok
`grok-4.7-build-fast` with `low` (Grok's fast variant is the model
`grok-4.7-build-fast`, listed in the captured
[models](../tests/testdata/real-adapters/linux-2026-10-04/grok-models.txt),
with a separate effort).

Restart the plane and every sidecar when deploying this policy: a component
of an older version keeps its narrower policy and may reject the expanded
efforts or selections (there is no compatibility retry). Old explicit
selections remain valid and no role is rewritten.

## Captured recipes (historical observations)

| Adapter | Observed version (`--version` output) | Observed model | Observed effort | Recipe (arguments after the executable) |
|---|---|---|---|---|
| `claude` | `2.1.285 (Claude Code)` | `sonnet` | `low` | `-p --model sonnet --effort low --permission-prompts none --output-format json` |
| `codex` | `codex-cli 0.159.0` | `gpt-6.1-sol` | `low` | `-a never exec --model gpt-6.1-sol -c model_reasoning_effort="low" --json --skip-git-repo-check --output-last-message <scratch>/callsheet-final.txt -` |

The recipes were captured on Linux x86_64 on 2026-09-30
([host](../tests/testdata/real-adapters/linux-2026-09-30/host.txt),
[captures](../tests/testdata/real-adapters/linux-2026-09-30/)). They are
historical observations: the recorded versions are the minimum baselines,
not the installed versions (the coordinator's host has since run Claude
`2.1.292 (Claude Code)` and Codex `codex-cli 0.160.0`), and the observed
pairs are evidence, not the only selections. Every task passes its own
selected model and effort in the same argument positions of the same
recipe.

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
every ready check and accepts a version line at or above the adapter's
minimum (above); bounded stderr (environment warnings) is allowed, never
logged and never read as a version. A probe checks
invocability, not authentication, provenance or the configured sandbox: the
operator controls the installed binaries, and replacing one between a probe
and a launch is not detected.

Register roles with an explicit model and an effort from the adapter's
set, for example the requested selection Claude `claude-opus-5-5` with
`high` (the vendor decides at run time whether it runs that pair):

```sh
callsheet role add coder-claude --name coder --node <node-id> --adapter claude \
  --instruction /srv/manuals/instruction.md --runbook /srv/manuals/runbook.md \
  --model claude-opus-5-5 --effort high --concurrency 1 --plane <plane-url> --ca <ca-path>
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
task guardian. For Claude and Codex, the composed prompt is the only stdin;
it is never an argument or a prompt file. Grok differs: it receives the
complete composed prompt as its `-p` argument, with stdin empty, so the
prompt is visible to process inspection; see the argv exposure warning in
[Wave 2: Grok and Cursor](#wave-2-grok-and-cursor).

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

## Wave 2: Grok and Cursor

The coordinator's Linux captures of 2026-10-04 ([host](../tests/testdata/real-adapters/linux-2026-10-04/host.txt),
[captures](../tests/testdata/real-adapters/linux-2026-10-04/)) decide two
explicit support limits. The table records those historical observations
(the versions are the minimum baselines and the pairs evidence; selection
and versions follow [Selection and versions](#selection-and-versions)):

| Adapter | Observed version (`--version` output) | Observed model | Observed effort | Worker execution |
|---|---|---|---|---|
| `grok` | `grok 1.0.46 (2765805b9442) [stable]` | `grok-4.7` | `low` | Linux only: `--output-format json --model grok-4.7 --reasoning-effort low --permission-mode dontAsk -p <composed-prompt>`, stdin empty |
| `cursor` | `2026.10.01-e373342` | `grok-4.7` | `low` | Refused on every OS |

Enable them like the others, with absolute paths on every start (the
captured Cursor executable is `cursor-agent`; its `agent` alias was not
qualified):

```sh
callsheet sidecar run --grok-adapter /opt/vendor/bin/grok --cursor-adapter /opt/vendor/bin/cursor-agent
```

An omitted flag disables that adapter; an empty, relative or repeated flag
is a usage error (exit 2). The flags are accepted even where execution is
refused, so other enabled adapters keep working and role registration
reports the precise refusal. Each probe runs only `<path> --version` and
accepts the version above or a later one (for Cursor the same or a later
date, whatever its hash); an eligible version is invocability, not
permission to execute tasks.

Cursor encodes effort in a vendor model name, with no standalone effort
flag: the observed pair (`grok-4.7`, `low`) ran as the vendor model
`grok-4.7-low`, and requirement Q12 lists (`grok-4.7`, `xhigh`) as
`grok-4.7-xhigh` (likewise `medium` and `high`). These are evidence and
examples, not a model allowlist, and Callsheet has no executable Cursor
mapping: every valid Cursor selection is refused before any argument is
built.

**Grok** answers, but under `dontAsk` every measured write was cancelled,
including permitted in-directory writes (`stopReason` `cancelled`, exit 0).
Every measured write was cancelled; useful tool execution remains
unqualified. Exit 0 does not prove the requested work completed. Grok requires the prompt in its `-p` argument, so
the complete composed prompt (manuals and task envelope) is visible to
process inspection by the OS and same-user tools; Callsheet never logs or
journals it. The composed prompt is limited to 32 KiB (a product argv
policy, not a measured vendor limit): a larger, empty, non-UTF-8 or
NUL-containing prompt is refused before launch as `start_failed`. The final
message is the decoded `text` of Grok's one JSON object, or its `message`
when `type` is `error`; `stopReason` is kept in the log and never decides
the task's state. On macOS Grok roles are refused pending qualification.
At start the sidecar logs once:

```text
grok adapter enabled: prompts are passed in argv and may be visible to process inspection; the composed prompt limit is 32 KiB; dontAsk cancelled all measured writes, including permitted writes; exit 0 does not prove requested work completed
```

with ` grok worker execution on macOS is refused pending qualification; consult the support catalog` appended on macOS.

**Cursor** is refused: its measured `--force --trust` candidate wrote
outside its scratch directory (home and `/tmp`), the baseline without those
flags refused workspace trust, and adding `--sandbox enabled` failed
authentication before any containment could be observed. Registering a
`cursor` role after a matching version probe returns `invalid_argument`,
field `adapter`, reason `probe_failed` and the message `cursor worker
execution is refused: no qualified unattended recipe preserves the operator
posture; consult the support catalog`; a role can never become ready, and a
stale role's task is refused `start_failed` before any launch (sidecar log
`task worker posture not qualified`, `reason=worker_posture_not_qualified`).
At start the sidecar logs once `cursor adapter enabled for version probing
only: unattended worker execution is refused because no qualified recipe
preserves the operator posture; consult the support catalog`. Its JSON
result parser exists for offline fixtures only. This fail-closed boundary
is the delivered Cursor outcome.

Neither adapter passes a sandbox override, approval bypass, trust or force
flag, edits operator configuration, selects a profile or retries with
broader permissions. Run sidecars under a dedicated OS user. No
configuration profile, key or enforcement mechanism can be prescribed as
verified from these captures: they hold no sanitized effective
configuration and no proof of all-tool containment. What a later
qualification must record to change either limit is in the catalog's
qualification handoff.

## Offline proof in CI

CI never runs a vendor CLI or a model. The function tests replay the
checked-in captures byte for byte through a stub enabled with the real
`--claude-adapter`, `--codex-adapter`, `--grok-adapter` and
`--cursor-adapter` flags, against a real plane, sidecar and `callsheet
mcp`, with an isolated HOME and a PATH of launch-recording traps. They
prove Callsheet's argv, stdin, extraction and exit handling of the captured
contract, and Cursor's refusal with zero task launches, not that vendor
behavior is stable. Alternate selections replay recorded bytes under
explicitly synthetic normalization: they prove Callsheet's routing of the
selected model and effort, never a real execution of that pair.

## Opt-in real smoke

On a machine with CLIs at or above their minimum versions, logged in as the
operator, run the paid smoke by hand (it is not a CI job):

```sh
CALLSHEET_REAL_ADAPTER_SMOKE=1 \
CALLSHEET_CLAUDE_PATH=/absolute/path/to/claude \
CALLSHEET_CODEX_PATH=/absolute/path/to/codex \
go test -tags=realadaptersmoke ./tests/smoke -run '^TestRealWorkerSmoke$' -count=1 -timeout=5m
```

Without the build tag the package does not exist for `go test ./...`.
Without `CALLSHEET_REAL_ADAPTER_SMOKE=1`, or with `CI` set, it skips before
touching anything. An unset or nonexistent path skips only that vendor; a
present but unusable binary or one below its minimum version, and any
authentication, model or sandbox refusal, fails. For each vendor it starts a
local plane and sidecar with the explicit path, registers the observed smoke
pair (the captured model and effort, an intentional explicit test input,
not the only valid selection or a default; other selections are registered
with `callsheet role add`), dispatches the captured no-tools prompt and
expects exit 0 and the final bytes `pong` within two minutes (on timeout it
cancels the task and waits for its cleanup). The recorded host versions
Claude `2.1.292 (Claude Code)` and Codex `codex-cli 0.160.0` are above their
minimums, so the smoke can run there when the explicit paths,
authentication and the opt-in are in place. A pass proves only that the
smoke pair completed this small goal through a plane and sidecar on that
host at that time: not the full coordinator workflow, every model/effort
pair, future releases, useful Grok tool writes or containment. A skipped
smoke is not qualification, and a passing one is not the remote acceptance
below.

The wave-2 smoke uses the same test with its own variables, so the command
above stays valid:

```sh
CALLSHEET_REAL_ADAPTER_SMOKE=1 \
CALLSHEET_GROK_PATH=/absolute/path/to/grok \
CALLSHEET_CURSOR_PATH=/absolute/path/to/cursor-agent \
go test -tags=realadaptersmoke ./tests/smoke -run '^TestRealWorkerSmoke$' -count=1 -timeout=5m
```

On Linux the Grok subtest dispatches the no-tools goal and expects exit 0
and the final bytes `pong`: answer-path operability, not useful tool work
or containment. On macOS it is an expected-refusal smoke: the role is
refused with Grok's posture message and nothing is launched. The Cursor
subtest, on either system, is a non-model refusal smoke: after a matching
version probe the role registration returns Cursor's posture refusal and
the role never becomes ready or dispatches. Neither refusal is vendor
qualification. Unset Claude/Codex paths skip those subtests as before.

## Remote acceptance (M3)

M3 is gated by the container acceptance, `go run ./cmd/devcheck
container-e2e`, which the Linux `go run ./cmd/devcheck test` stage that
`ci-linux` requires also runs. In one isolated Linux container its
deterministic coordinator prepares four fake roles on two real sidecars
through the real CLI and checks the two-worker goal-and-answer exchange in
one session: FP-2, `TestContainerCoordinator` with its `prepare` and
`goal_answer` subtests (the ordered design, design-review, code and
code-review stub flow is FP-11, `TestContainerSampleFlow`). The M4 mapping
and the required evidence are in [Manual M4 checks](workspaces.md#manual-m4-checks).

A complete green run demonstrates M3/M4 under Linux loopback, fake workers
and a deterministic CLI coordinator; it does not demonstrate two machines,
two operating systems, a real vendor process, vendor authentication or
paid model calls. No macOS container proof is required. Until the first
passing CI run records its revision and evidence, M3 has not been
demonstrated. Fake roles do not qualify vendor models: the out-of-union
effort refusal stays covered by the adapter tests and the optional
observation below.

The former manual procedure is an optional, non-gating deployment
observation on real machines, never a second milestone requirement; the
checked-in captures and the local smoke are qualification inputs, not a
remote coordinator run. If you run it, record the results beside the
evidence:

1. Use the existing coordinator setup ([coordinator guide](coordinator.md)):
   a plane, and a real coordinator CLI registered with `callsheet mcp`.
2. On two other enrolled worker machines run `callsheet sidecar run` with
   `--claude-adapter` on one and `--codex-adapter` on the other (explicit
   absolute paths), and register one role each with an explicit model and
   effort (for example the observed pair) and existing manuals.
3. From the coordinator, dispatch one no-workspace goal-and-answer task to
   each role (no workspace or base fields). Follow each with short
   `task_wait` calls, repeated as needed under the unchanged short-poll wait
   budget (or the background wait of the coordinator guide's runbook
   example); do not change the 07b timeout policy or run `mcpqual` for this.
4. Inspect `task_show` and `task_logs` for each task. Record the
   coordinator's and each worker's OS, architecture and CLI versions, the
   node and task IDs, the final message bytes, the process exits, and that
   the task completed and its scratch directory and process group were
   cleaned up.
5. Register (or dispatch with an override) an out-of-union effort on one
   worker and record Callsheet's `invalid_argument` refusal of the
   out-of-union effort. A valid but unknown model is not refused by
   Callsheet: it demonstrates only the vendor's own rejection, and only when
   a task actually runs it; record that separately if you run one.
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
