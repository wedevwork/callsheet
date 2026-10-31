# Real worker adapters: Claude, Codex, Grok and Cursor

Iteration 08 adds production `claude` and `codex` worker adapters beside the
test-only `fake`. A worker runs a goal-and-answer task through the vendor CLI
it is explicitly given, with the prompt on stdin, and returns the vendor's
final message; no workspace, checkout or pushed result is involved. The
qualification evidence and every per-fact status are in the
[support catalog](support-catalog.md); this page is the operator procedure.
Iteration 11 adds `grok` and `cursor`. Both run on Linux only. Cursor's
Linux recipe is `--trust --auto-review` with a positional prompt; every
other OS refuses Cursor task execution. See
[Wave 2: Grok and Cursor](#wave-2-grok-and-cursor).

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

Each worker executable is eligible at or above its **runtime minimum
version**, which is independent of the historical capture identity in the
support catalog, with no warning for a newer version and no claim that a
newer version is compatible. These baselines are conservative product
floors from the captured help identities, not claims that auto first
appeared there. Cursor's date baseline does not overcome its non-Linux
refusal.

| Adapter | `--version` line (literal spaces) | Runtime minimum | Ordering |
|---|---|---|---|
| `claude` | `SEMVER (Claude Code)` | `2.1.299 (Claude Code)` | SemVer precedence |
| `codex` | `codex-cli SEMVER` | `codex-cli 0.162.1` | SemVer precedence |
| `grok` | `grok SEMVER (HASH) [CHANNEL]` | `grok 1.0.52 (0920c8169e27) [stable]` | SemVer precedence; the hash and channel never order |
| `cursor` | `YYYY.MM.DD-HASH` | `2026.10.01-e373342` | the calendar date; a same-date version with another hash is equal and accepted |

`SEMVER` is `major.minor.patch` with an optional `-prerelease` and `+build`:
the core compares numerically, a release sorts above its prereleases (so
`codex-cli 0.162.1-rc.1` is below the minimum and `0.163.0-rc.1` above it),
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
historical observations, not the current argv and not the runtime minima
(the coordinator's host has since run Claude `2.1.292 (Claude Code)` and
Codex `codex-cli 0.160.0`, both now below the runtime minima). The observed
pairs are evidence, not the only selections.

## Current worker recipes

Every role — designer, design-reviewer, coder and code-reviewer — receives
the same adapter recipe. Role instructions still constrain review work;
they are not enforcement, and the adapter infers nothing from the role
name. Callsheet does not edit vendor configuration, predict allowed tools,
reset a classifier or retry a denial with broader permissions. Classifier
modes are permission policies, not evidence of containment. An untouched
sentinel is a limited observation. Readiness checks invocability and
version, not authentication, account access, a tool-supported model or an
operator-approved working directory.

| Adapter | Current arguments after the executable |
|---|---|
| `claude` | `-p --model MODEL --effort EFFORT --permission-mode auto --permission-prompts none --output-format json` (prompt on stdin) |
| `codex` | `exec --approve-for-me --model MODEL -c model_reasoning_effort="EFFORT" --json --skip-git-repo-check --output-last-message <final>/callsheet-final.txt -` (prompt on stdin; `<final>` is the scratch directory, or the workspace task's owned final-output directory) |
| `grok` | Linux only: `--output-format json --model MODEL --reasoning-effort EFFORT --permission-mode auto -p <composed-prompt>`, stdin empty |
| `cursor` | Linux only: `-p --trust --auto-review --output-format json --model MODEL-EFFORT <composed-prompt>`, stdin empty |

Claude auto is available on all plans unless an administrator sets
`permissions.disableAutoMode`. Supported models include Opus 4.6+ and
Fable, and the classifier pauses after 3 consecutive or 20 total blocks.
Those three facts are the owner's recorded source, not a fresh upstream
verification. Operators resolve a policy or model incompatibility
themselves. `--approve-for-me` selects Codex's workspace-write sandbox.
Callsheet still writes no Codex configuration and adds no independent
sandbox override. Grok's useful headless tool execution under auto is
UNVERIFIED until an owner-run Linux scratch-file probe of this exact
recipe. Auto does not imply that every requested action succeeds.

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
configured sandbox and classifier provide (on the recorded host, host reads
and `/tmp` writes were allowed). Callsheet creates no sandbox of its own.
Codex's `--approve-for-me` selects that vendor's workspace-write sandbox;
Callsheet adds no further sandbox override and never relaxes one. It never
edits vendor settings or trust entries, and never retries a denied task
with broader permissions. Inspect the effective configuration with the
vendor's own tools, for example `claude sandbox status` and your Claude
`settings.json`, or Codex's `config.toml`.

## How a task runs

Each task gets a fresh private scratch directory (mode 0700, not a git
checkout) as its working directory, in its own process group led by the
task guardian. For Claude and Codex, the composed prompt is the only stdin;
it is never an argument or a prompt file. Grok and Cursor differ: each
receives the complete composed prompt as one argument (Grok's `-p` value,
Cursor's final positional argument), with stdin empty, so the prompt is
visible to process inspection; see the argv exposure warnings in
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
(the versions are historical capture identities, not the runtime minima,
and the pairs are evidence; selection and versions follow
[Selection and versions](#selection-and-versions)):

| Adapter | Observed version (`--version` output) | Observed model | Observed effort | Worker execution |
|---|---|---|---|---|
| `grok` | `grok 1.0.46 (2765805b9442) [stable]` | `grok-4.7` | `low` | Historical Linux capture, not current argv: `--output-format json --model grok-4.7 --reasoning-effort low --permission-mode dontAsk -p <composed-prompt>`, stdin empty |
| `cursor` | `2026.10.01-e373342` | `grok-4.7` | `low` | Historical rejected candidate, not current argv: `-p --output-format json --model grok-4.7-low --force --trust`, prompt on stdin |

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

Cursor encodes effort in the model argument as `MODEL-EFFORT`, with no
trimming, alias lookup, fast-suffix relocation or fallback: the role pair
(`grok-4.7`, `low`) is the argument `grok-4.7-low`, and (`grok-4.7`,
`high`) is `grok-4.7-high`. A name that already contains an effort is not
rewritten, so `grok-4.7-high-fast` with effort `low` is
`grok-4.7-high-fast-low`. This is a Callsheet transformation, not a vendor
fact: the owner probe of `(grok-4.7, high)` is a deployment prerequisite,
and a pair the vendor does not support fails as that vendor's result.
The iteration-11 observation that only the literal `grok-4.7-low` was
measured, and that Callsheet then applied no general concatenation, is
historical.

**Grok** requests auto. The historical `dontAsk` capture cancelled every
measured write, including permitted in-directory writes (`stopReason`
`cancelled`, exit 0); that cancellation is evidence of that capture, not a
current-mode assertion. Useful tool execution under auto is UNVERIFIED.
Exit 0 does not prove the requested work completed. Grok requires the
prompt in its `-p` argument, so the complete composed prompt (manuals and
task envelope) is visible to process inspection by the OS and same-user
tools; Callsheet never logs or journals it. The composed prompt is limited
to 32 KiB (a product argv policy, not a measured vendor limit): a larger,
empty, non-UTF-8 or NUL-containing prompt is refused before launch as
`start_failed`. The final message is the decoded `text` of Grok's one JSON
object, or its `message` when `type` is `error`; `stopReason` is kept in
the log and never decides the task's state. On macOS Grok roles are refused
pending qualification. At start the sidecar logs once:

```text
grok adapter enabled: auto is requested; prompts are passed in argv and may be visible to process inspection; the composed prompt limit is 32 KiB; exit 0 does not prove requested work completed
```

with ` grok worker execution on macOS is refused pending qualification; consult the support catalog` appended on macOS.

**Cursor** on Linux runs
`-p --trust --auto-review --output-format json --model MODEL-EFFORT <composed-prompt>`
with empty stdin. The prompt is one positional argument, limited to 32 KiB
(a product argv policy, not a measured vendor limit): a larger, empty,
non-UTF-8 or NUL-containing prompt is refused before launch as
`start_failed`. The final message is the `result` string of the one JSON
object on stdout (`type` `result`, boolean `is_error`); that extraction is
live, and the observed result need not be the single word DONE. Passing
`--trust` may cause Cursor to record trust and other state outside the
task directory. Callsheet does not create, patch, restore or delete that
state. `--trust --auto-review` is the owner-authorized recipe; it is a
permission policy, not evidence of OS confinement, and an untouched
sentinel is a limited observation. No jail and no non-Linux qualification
has shipped. On every other OS, role registration, every ready check and
task preparation (including a stale role) refuse with `invalid_argument`,
field `adapter` or `start_failed`, and the message `cursor worker
execution is not qualified on this OS; consult the support catalog`;
nothing is launched (sidecar log `task worker posture not qualified`,
`reason=worker_posture_not_qualified`). This non-Linux fail-closed boundary
is the delivered refusal.

Iteration 11's `--force --trust` candidate wrote outside its scratch
directory (home and `/tmp`), the baseline without those flags refused
workspace trust, and adding `--sandbox enabled` failed authentication
before any containment could be observed. Those captures stay historical.
Callsheet never passes `--force`, `--yolo`, `--approve-mcps` or
`--sandbox`. The model-argument encoding above is unverified against a
live CLI until the owner probe of `(grok-4.7, high)`.

At start the sidecar logs once:

```text
cursor adapter enabled: on Linux, tasks run with --trust --auto-review; prompts are passed in argv and may be visible to process inspection; the prompt limit is 32 KiB; Cursor records trust and maintains state outside the task directory
```

with ` cursor worker execution is not qualified on this OS; consult the support catalog` appended when the OS is not Linux.

Neither adapter passes `--sandbox`, `--force`, `--yolo` or `--approve-mcps`,
edits operator configuration, selects a profile or retries with broader
permissions. Cursor's recipe does pass `--trust` and `--auto-review`, which
is not a confinement claim. Run sidecars under a dedicated OS user. No
configuration profile, key or enforcement mechanism can be prescribed as
verified from these captures: they hold no sanitized effective
configuration and no proof of all-tool containment. What a later
qualification must record to change either limit is in the catalog's
qualification handoff.

### Optional Cursor coder in the development flow

The real end-to-end development flow can select Cursor for its coder only,
through `docs/examples/real-e2e/dev-full/flow-cursor-coder.json`: adapter
`cursor`, model `grok-4.7`, effort `high` (model argument `grok-4.7-high`).
The default flow is unchanged, with its Claude coder; the designer, the code
reviewer and triage stay on Codex and the design reviewer on Claude, and no
Cursor reviewer or triage selection ships. The separate four-hop sample in
which Cursor reviews code is a different scenario, not exercised by this
selection. Live execution is Linux-only. The recipe above, the minimum
version `2026.10.01-e373342` and the posture checks are unchanged: no
`--force`, `--yolo`, `--approve-mcps`, `--sandbox` or permission fallback,
no jail and no confinement claim. The Cursor sidecar runs with its user's
ordinary access, so run it under a dedicated OS user. Expected vendor state,
cache and temporary-file activity is observed report-only: the harness's
shallow samples of `~/.cache`, `/tmp` and `/var/tmp` miss deep or transient
writes, reads, network and other home paths, and cannot attribute a change
to Cursor; only the strict protected snapshots gate the owner environment.
The owner's 2026-10-27 slice 3a diagnostic observed `grok-4.7-high`
authenticate and complete one fixture repair on Cursor `2026.10.14-0f9edde`;
that observation is not qualification, and headless approval behaviour
remains unexercised. A fresh qualification probe and its independent check
are an operator prerequisite before any Cursor attempt; see
[Cursor coder qualification](maintainers/real-e2e.md#cursor-coder-qualification).

## Offline proof in CI

CI never runs a vendor CLI or a model. The function tests replay the
checked-in captures byte for byte through a stub enabled with the real
`--claude-adapter`, `--codex-adapter`, `--grok-adapter` and
`--cursor-adapter` flags, against a real plane, sidecar and `callsheet
mcp`, with an isolated HOME and a PATH of launch-recording traps. They
prove Callsheet's argv, stdin, extraction and exit handling. On Linux the
Cursor parents use a synthetic current-recipe result, not a vendor
capture and not a claim that confinement or non-Linux execution has
shipped. On macOS they assert Cursor's OS refusal with zero task launches.
Alternate selections replay recorded bytes under
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
refused with Grok's posture message and nothing is launched. On Linux the
Cursor subtest dispatches a synthetic goal under the current recipe and
expects exit 0 and the final bytes `pong`: answer-path operability of
`--trust --auto-review`, not containment, not a jail, and not a non-Linux
qualification.
On macOS it is an expected-refusal smoke: the role is refused with
Cursor's OS-posture message and nothing is launched. Neither a Linux pass
nor a macOS refusal is vendor qualification. Unset Claude/Codex paths
skip those subtests as before.

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
