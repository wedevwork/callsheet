# CI and pull requests

GitHub Actions runs the same verification bar the development flow enforces
locally, on every push to `main` and every pull request targeting `main`
(workflow `.github/workflows/ci.yml`, name `CI`). All verification policy
lives in the `devcheck` driver (`go run ./cmd/devcheck ...`); the workflow only
selects its stages.

Platform scope: all v1 components are Linux/macOS only. Windows coordinator
support is backlog E9, lowest priority; no Windows build, job or check exists
in v1.

## Checks

Ten fixed jobs run on every trigger. Exactly four of them are the required
status check contexts on `main`: `ci-linux`, `ci-macos`, `ci-linux-stress`
and `ci-macos-stress`. The other six are the stress workers (iteration 02c),
three shards per platform; their names are unique diagnostic checks, not
required contexts:

| Check context | Runner | Timeout | Kind | Steps after setup |
|---|---|---|---|---|
| `ci-linux` | `ubuntu-24.04` | 45 min | required | `devcheck test` (native suite, then the same suite with `-race`), `devcheck coverage` (unit coverage must be greater than 80.0%), `devcheck bench` (git transport payload byte limits and commit/tree invariants, then the plane trust benchmarks: issuance, initialization and verified TLS health, each checking its invariants; timings are reported, never gated), `devcheck cross` (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64: 12 artifacts) |
| `ci-macos` | `macos-15` | 30 min | required | `devcheck native`: the complete suite as `go test -json`, which must show passing run and pass events in `github.com/wedevwork/callsheet/tests/function` for `TestFP6ProcessGroups` and its `cooperative`, `resistant` and `leader-exits-first` scenarios, and for the plane trust tests (see below) |
| `ci-linux-stress-packages` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-packages` on Linux (see Stress checks) |
| `ci-linux-stress-processgroup` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-processgroup` on Linux |
| `ci-linux-stress-functions` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-functions` on Linux |
| `ci-macos-stress-packages` | `macos-15` | 20 min | worker | `devcheck stress-packages` on Darwin, with the same commands, repeat count and CPU settings as Linux |
| `ci-macos-stress-processgroup` | `macos-15` | 20 min | worker | `devcheck stress-processgroup` on Darwin, likewise |
| `ci-macos-stress-functions` | `macos-15` | 20 min | worker | `devcheck stress-functions` on Darwin, likewise |
| `ci-linux-stress` | `ubuntu-24.04` | 5 min | required summary | no setup; succeeds only if the three Linux workers all concluded `success` |
| `ci-macos-stress` | `ubuntu-24.04` | 5 min | required summary | no setup; succeeds only if the three macOS workers all concluded `success` (Ubuntu only evaluates their status; it qualifies nothing about Darwin) |

The two main jobs and the six workers start together on every trigger and
run independently: none waits for, depends on or is conditional on another
(no `needs`, matrix, job or step condition, path filter, concurrency
cancellation or `continue-on-error`), and each fails on its own. Iteration
02b moved stress out of the two main jobs; iteration 02c split each
platform's stress job into three workers, so its three shards run in
parallel with each other and with the main jobs. The main jobs keep their
stages and budgets.

Only the two summaries have dependencies, each on its own platform's three
workers, and they keep the required stress contexts, so branch protection
needs no change. Each summary is the same small template with literal
job IDs, never a matrix or a dynamic expression:

```yaml
needs: [linux-stress-packages, linux-stress-processgroup, linux-stress-functions]
if: ${{ always() }}
defaults:
  run:
    shell: bash
steps:
  - name: Require every stress shard
    env:
      PACKAGES_RESULT: ${{ needs['linux-stress-packages'].result }}
      PROCESSGROUP_RESULT: ${{ needs['linux-stress-processgroup'].result }}
      FUNCTIONS_RESULT: ${{ needs['linux-stress-functions'].result }}
    run: test "$PACKAGES_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success
```

(`ci-macos-stress` is identical with `macos-` job IDs.) `if: ${{ always() }}`
makes the summary evaluate after unsuccessful dependencies instead of being
skipped. An all-success triple alone exits zero; `failure`, `cancelled`,
`skipped`, an empty or any unknown result fails the summary. The all-success
predicate is never put on the job's `if`, where it could skip the gate
instead of failing it. A canceled workflow or an unavailable summary runner
may still leave a non-success summary; no canceled, pending or skipped
summary qualifies. Summaries hold no stress logs: those are in the worker
jobs.

The plane trust tests required on `ci-macos` (iteration 02) are the eight
function tests `TestPlaneCommands`, `TestPlaneState`, `TestPlaneBind`,
`TestPlaneInit`, `TestPlaneTLS`, `TestPlaneReissue`, `TestPlaneStatus` and
`TestPlanePlatform`, plus the mandatory subtests of the compound ones:
`TestPlaneState` `paths`, `persistence`, `locking`, `validation` and
`contracts`; `TestPlaneInit` `issuance`, `fingerprint` and
`restart-invariance`; `TestPlaneTLS` `https-only`, `prelisten-validation`,
`bounded-shutdown` and `contracts`; `TestPlaneReissue` `process` and
`contracts`; `TestPlaneStatus` `inspection` and `expiry-warnings`. With the
four process-group names that is 28 required names. The `contracts`
subtests (iteration 02b) run the delegated `internal/plane` contracts, each
with its own complete run/pass evidence; `process` is `TestPlaneReissue`'s
real CLI scenario. They prove the native state modes, no-replace
publication, atomic rename, kernel `flock` between processes, loopback TLS
and signal cleanup on the runner itself; neither cross-compilation nor a
Linux pass qualifies macOS. The plane trust benchmarks and coverage remain
Linux reference gates, not Darwin claims.

The main jobs and all six workers check out the event's revision without
persisted credentials, take the Go version from `go.mod` with module
caching, run `go mod download`, and then run their check steps with
`GOPROXY=off` and `GOSUMDB=off`. The summaries have no checkout, Go setup,
downloads or job environment. A cold or concurrently populated cache
affects speed only, never correctness. The workflow has read-only repository
permission (`contents: read`), uses no secrets, exchanges no artifacts
between jobs and never changes repository settings.

A missing, skipped or failing qualification test fails `ci-macos` with
`native qualification unobserved` even when `go test` itself exits zero. A
timeout, an unavailable runner or a canceled, skipped or pending check is
unobserved qualification, never a pass. `ci-macos` qualifies the runner's own
CPU architecture only; cross-builds do not prove runtime behavior on the other
Darwin architecture. Coverage and bench remain Linux reference gates; they are
not claimed for Darwin.

## Stress checks

`go run ./cmd/devcheck stress` repeats the timing- and concurrency-sensitive
tests under the race detector with varied parallelism, on Linux and macOS.
In CI it runs as three shards per platform (iteration 02c), one worker job
each: `ci-linux-stress-packages`, `ci-linux-stress-processgroup` and
`ci-linux-stress-functions` run `devcheck stress-packages`,
`devcheck stress-processgroup` and `devcheck stress-functions` on Linux, and
the three `ci-macos-stress-*` workers run the same stages on macOS.
The project's declared repeat count is 20 per CPU setting (1, 2, 4). The
flow's coder and reviewer use this count when they re-run timing-dependent
tests they add or modify: `devcheck stress` for tests in its covered packages,
and the equivalent `go test -race -count=20 -cpu=1,2,4 -run '<tests>'
<package>` command for changed timing tests outside that set, such as the
coordinator's own `TestStressConcurrencyContract` (see Local verification).

The count, CPU list, shards, package groups, selectors and time budgets are
declared once, in `internal/devcheck/stress.go` (`StressCount`,
`StressShards`, and `StressSteps`, the flattened inspection view). The
three shards run six commands (argv, never a shell), each with
`CGO_ENABLED=1`, named `stress packages`, `stress processgroup cpu1`,
`stress processgroup cpu2`, `stress processgroup cpu4`, `stress function`
and `stress plane function`:

```
go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/plane
go test -race -count=20 -cpu=1 -timeout=6m ./internal/spikes/processgroup
go test -race -count=20 -cpu=2 -timeout=6m ./internal/spikes/processgroup
go test -race -count=20 -cpu=4 -timeout=6m ./internal/spikes/processgroup
go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function
go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$ ./tests/function
```

| Shard | Stage | Commands | Execution |
|---|---|---|---|
| `packages` | `devcheck stress-packages` | `stress packages` | one invocation |
| `processgroup` | `devcheck stress-processgroup` | `stress processgroup cpu1`, `cpu2`, `cpu4` | three concurrent invocations, one per CPU setting |
| `functions` | `devcheck stress-functions` | `stress function`, then `stress plane function` | sequential |

These are argv displays, not shell-ready commands: quote the entire `-run`
argument when running one through a shell. The plane selector is one
two-level pattern: `go test` splits a `-run` pattern into per-level patterns
only at a `/` outside parentheses and brackets, so it selects the three
parents at the top level and only the eight named subtests below them. It
must not be "simplified" into a flat alternation, which would select
different tests.

- Selected packages: `internal/testkit`, `internal/testkit/fakeadapter`,
  `internal/spikes/gittransport` and `internal/plane` in the packages
  shard, and `internal/spikes/processgroup` in the processgroup shard,
  complete package tests (not benchmarks). The fake adapter is included
  because its signal handling and descendant lifecycle are timing-sensitive
  too; the plane package (iteration 02) because its listener, shutdown,
  lock-holder helper process and contracts are, and they run there directly
  under the race detector.
- Selected function tests: only `TestFP4TransportHarness` and
  `TestFP5GitRoundTrip` (`stress function`), and the process-boundary
  subtests of the listener- and lock-bearing plane trust tests
  (`stress plane function`): `TestPlaneState` `paths`, `persistence`,
  `locking` and `validation`; `TestPlaneTLS` `https-only`,
  `prelisten-validation` and `bounded-shutdown`; `TestPlaneReissue`
  `process`. All are in `tests/function`; unrelated function tests such as
  the twelve-artifact cross-build test are deliberately excluded. The two
  selectors are disjoint, and each plane parent's `contracts` subtest is not
  selected. The other plane trust tests (`TestPlaneCommands`,
  `TestPlaneBind`, `TestPlaneInit`, `TestPlaneStatus`, `TestPlanePlatform`)
  run in the normal native suites only; their listener mechanisms are
  repeated by the plane package tests and by `TestPlaneTLS` and
  `TestPlaneReissue`. Each function command also fails when `go test`
  reports `no tests to run` for its selector (`[no tests to run]` or
  `testing: warning: no tests to run`): an empty selection is never a pass.
- The shards' union is exactly the iteration 02b selection: every
  (package, selector, CPU setting, count) combination appears exactly once.
  Only processgroup's single `-cpu=1,2,4` invocation became three
  invocations of one CPU setting each; the per-test repetitions (60), the
  CPU settings, `-race` and the timeouts are unchanged.
- `-count=20` applies at each CPU setting: every selected test runs
  60 times per shard. This is repeated testing, never retry-until-green:
  any failure fails the stage. There is no count override, no lighter macOS
  count and no environment-based bypass, and no stress stage accepts
  operands or flags.
- `all` does not include stress: `all` stays test, coverage, bench and cross,
  because repeated subprocess builds and process experiments cost minutes.
  Local release and review verification therefore runs both `all` and
  `stress`.
- Stress runs in its own worker jobs with identical commands and count on
  both platforms: a Linux-only stress pass cannot qualify Darwin.
- `-race` needs the native C compiler on each runner (cgo). A missing compiler
  is a failed prerequisite, never permission to omit `-race`. Shipped
  cross-built binaries remain CGO-disabled.

Execution. `devcheck stress` runs the shards in order, packages,
processgroup, functions; a shard stage runs exactly its own commands.
Sequential commands stop at the first failure, and a failed shard prevents
the next. Only the processgroup shard is concurrent: its three commands start
together, at most three at once, under the same watchdog, and the shard
waits for all of them before it returns. Each writes only to its own log in
the scratch directory; the commands and log paths are printed before
launch, and after all three have finished each log is replayed from disk
under its command name in CPU order, with its elapsed time and outcome. A
failing invocation does not cancel its siblings: all three results are
collected, then the shard fails, listing every failure in CPU order with its
command. A log that cannot be created, written, closed or replayed fails the
shard even if the child exited zero; if a log cannot be created, nothing
starts. There are no retries. Logs are kept, and their directory printed, on
any failure.

Watchdog and orphans. When the watchdog (or the caller) ends the context,
every outstanding invocation is canceled and the shard still waits for all
of them, then fails, even if a command reported success. A context already
ended starts nothing, and no further invocation starts once it ends.
`devcheck` ends each direct `go test` child; like every forcibly interrupted
run since iteration 01c, up to three test binaries (and their descendants)
can remain as orphans until the runner ends. That does not change the
verdict: the shard fails. A forcibly interrupted or timed-out run fails
qualification and makes no clean-teardown claim, and there is no
process-tree kill.

Why only processgroup is concurrent, and its timing risk. Its stress time
was dominated by waiting, the 1 s TERM-to-KILL grace of its resistant and
leader-exits-first cases repeated one after another (hosted 211 s Linux and
234 s macOS in one `-cpu=1,2,4` binary), not by CPU. Plane (185 s and
239 s) does real state, fsync and TLS work, and there is no evidence that
overlapping copies would help, so it and the function commands stay
sequential. Processgroup runs alone on its worker, with at most three
top-level `go test` commands. `-cpu` sets each top-level test binary's
GOMAXPROCS; it is not a reservation of seven cores and does not set the
GOMAXPROCS of independently launched helpers. The observed 380 ms
cooperative exit under load leaves about 620 ms below the unchanged 1 s
grace; that is historical evidence, not a scheduling guarantee. Bounding
to three and isolating the shard limits the added load, and native runs on
both hosted runners must show that the bound is viable. Every timing
assertion and the grace stay unchanged. A newly observed timing failure is
a blocker to investigate, never permission to retry until green, increase
the grace, skip a case, lower counts or choose a platform-specific plan; a
revised bound requires a design revision.

Deduplication (iteration 02b): stress repeats nothing that another stress
command already repeats at the declared count. Only these repeated
invocations were removed; each still runs once in the ordinary suites,
`devcheck test` on Linux and `devcheck native` on macOS:

| Removed stress work | Repeated 60 times by | Still run ordinarily |
|---|---|---|
| `TestFP6ProcessGroups` | `internal/spikes/processgroup` `TestExperiment` (processgroup shard): the same `RunExperiment` with the cooperative, resistant and leader-exits-first cases, requiring `rep.Pass()`; the package's evaluation and cleanup tests remain | the whole FP-6 test with all three scenario assertions; still mandatory Darwin native evidence |
| `TestPlaneState`'s delegated native state contract | `internal/plane` `TestNativeStateContract` (packages shard): modes, no-replace, rename, flock | `TestPlaneState` `contracts` invokes the identical contract with its mandatory evidence list |
| `TestPlaneState`'s delegated failure contract | `internal/plane` `TestStateFailureContract`: create, write, sync, close, publish, partial, corrupt | `TestPlaneState` `contracts`, likewise |
| `TestPlaneTLS`'s delegated failure contract | `internal/plane` `TestServerFailureContract`: listen, serve, shutdown-deadline | `TestPlaneTLS` `contracts`, likewise |
| `TestPlaneReissue`'s delegated failure contract | `internal/plane` `TestReissueFailureContract`: expired-leaf, ca-horizon, before-rename, after-rename | `TestPlaneReissue` `contracts`, likewise |

`TestFP6ProcessGroups` asserts more than `rep.Pass()`: the deadline equals
the TERM time plus the grace, the process-group structure (a group above 1,
not the test's own, leader and descendant in the same group), no emergency
kill, and two orderings the package evaluator does not check (the
pre-deadline observation before the deadline for the resistant and
leader-exits-first cases, and the leader's exit before that observation for
leader-exits-first). Those orderings lose their 60 repetitions; they keep
the full 1 s grace as margin and run once per platform in `devcheck test`
and `devcheck native`, while `TestExperiment` repeats the same `Escalate`
and observation code path at the declared count. Nothing else was removed:
real cross-process locking, TLS and plaintext checks, prelisten rejection,
the stopped backup, reissue and restart, and signal cleanup are still
repeated 60 times on both platforms.

Budgets:

- Each test binary: `-timeout=6m`.
- Each stress command: a 15-minute watchdog (or the caller's earlier
  deadline) bounds compilation as well as test execution; when it expires
  the command fails and no further step or shard starts. Each shard stage,
  that is each worker job, has its own. A local `devcheck stress` runs all
  three shards in one process under one shared 15-minute watchdog rather
  than three; a local expiry is therefore not evidence about any hosted
  worker.
- Worker jobs: 20 minutes each, five minutes beyond the unchanged 15-minute
  watchdog for setup. Summary jobs: 5 minutes each. The main jobs keep
  45 minutes (`ci-linux`) and 30 minutes (`ci-macos`). None of these
  durations is a performance target.
- Target: under 10 minutes for each stress command on each hosted runner, a
  diagnostic budget. Exceeding the target is recorded, not a failure;
  timeout and watchdog failures remain failures.
- Pull request wall-clock goal (iteration 02c): about 4–5 minutes for the
  whole workflow's critical path, down from about 9.5 minutes. It is a
  planning goal, not an acceptance threshold and not a measurement; the
  macOS packages worker may miss it. (The iteration 02b target was about
  5–6 minutes for the slowest of four jobs: an optimization target, not a
  measurement.)
- If one package's test binary exceeds its 6-minute timeout, that package
  moves into its own stress step with its own 6-minute timeout; the repeat
  count, the CPU list and the package set never change, and no test is
  weakened.
- Function-binary remedy (pre-authorized by design 02): if a function
  step, measured from its `ok  ./tests/function <N>s` line, reaches 5
  minutes on either supported host or hits its 6-minute timeout, it splits
  statically for both platforms with identical flags. The first stage of
  the remedy is applied: the combined six-test function step measured
  322.7 s on Linux (below), so it became `stress function` (the
  iteration 01 tests) and `stress plane function` (the plane tests).
  A group that still reaches 5 minutes splits into one
  `stress function <TestName>` step per test. Counts, the CPU list and the
  watchdog never change, and nothing is retried or split dynamically.
  These static remedies remain available only with their documented
  measurements, identical selections and counts on both systems, and
  updated exact-plan tests and docs; splitting stress across further
  hosted jobs requires a design revision.
- Estimated local cost with a warm build cache: 3–10 minutes (a planning
  estimate, not a hardware-independent limit).

Measurements, newest first. Hosted and local figures come from different
machines and are never combined into one number.

- Measured with iteration 02c: Linux, go1.26.4 linux/amd64 on the same
  16-thread Intel i7-11800H developer workstation (kernel 6.8), warm build
  cache, 2026-09-26, each command run alone on the host (the shard stages
  one after another, never overlapping, as on separate hosted workers):
  - `devcheck stress-packages` 218.4 s: `stress packages` 218.3 s, slowest
    binary `internal/plane` 217.9 s (`internal/testkit/fakeadapter` 53.3 s,
    `internal/testkit` 31.1 s, `internal/spikes/gittransport` 24.5 s).
    Unchanged from 02b's 219.5 s locally: plane already bounded it.
  - `devcheck stress-processgroup` 70.2 s: `stress processgroup cpu1`,
    `cpu2` and `cpu4` each ok in 70.2 s (binaries 69.8 s each), all three
    concurrently, against 201.4 s for the single `-cpu=1,2,4` binary of
    iteration 02b: the concurrent invocations did not slow each other on
    this host, and every timing assertion passed at the unchanged 1 s grace.
  - `devcheck stress-functions` 92.3 s: `stress function` 32.1 s (binary
    30.7 s), then `stress plane function` 60.2 s (binary 59.1 s).
  - `devcheck stress`, the three shards in one process under one watchdog:
    385.0 s (6 min 25 s): `stress packages` 220.1 s (`internal/plane`
    219.7 s), `stress processgroup cpu1`, `cpu2` and `cpu4` 69.8 s each,
    `stress function` 31.4 s and `stress plane function` 63.7 s. That is
    76 s more than 02b's 309.1 s: in 02b processgroup overlapped plane
    inside one `go test`, while a local `stress` now runs its shard after
    the packages shard. Hosted, the shards run on separate workers, so the
    critical path is the slowest worker, not this sum.

- Hosted baseline, measured: run 36236333755 (iteration 02b, four jobs,
  all green). Jobs: `ci-linux` 101 s, `ci-macos` 49 s, `ci-linux-stress`
  350 s and `ci-macos-stress` 568 s, the critical path (about 9.5 minutes).
  Stress steps, Linux / macOS: `stress packages` 227 s / 298 s (binaries
  `internal/spikes/processgroup` 211 s / 234 s, `internal/plane` 185 s /
  239 s); `stress function` 41 s / 76 s; `stress plane function` 63 s /
  163 s; the serial stress step total 331 s / 538 s. This measurement
  supersedes iteration 02b's pending first-run timing language.

- Expected per-job wall-clock after iteration 02c: planning estimates, not
  measurements. They take the hosted run 36236333755 figures above,
  subtract what moved out of each job and divide processgroup's test time by
  its three concurrent invocations, as locally observed (below); job setup
  is taken from that run's job-minus-step remainders, about 19 s on Linux
  and 30 s on macOS. No runner capacity, warm cache, core count or linear
  speedup is guaranteed, and six workers cost extra runner minutes and may
  queue behind the account's concurrency limit.
  - `ci-linux` about 1.8 minutes (101 s plus the new offline tests);
    `ci-macos` about 1 minute (49 s plus the new offline tests).
  - `ci-linux-stress-packages` about 3.5–4 minutes: the slowest binary is now
    `internal/plane` (185 s), plus compilation and setup; losing
    processgroup may reduce contention, but that is not a guaranteed saving.
  - `ci-linux-stress-processgroup` about 1.5–2 minutes: 211 s / 3 is an
    optimistic 70 s of test time, plus compilation, setup and cold-cache
    variation.
  - `ci-linux-stress-functions` about 2–2.5 minutes (41 s + 63 s, plus
    setup).
  - `ci-macos-stress-packages` about 4.5–5.5 minutes: `internal/plane`
    (239 s) plus compilation and setup; the likely critical path.
  - `ci-macos-stress-processgroup` about 1.5–2.5 minutes: 234 s / 3 is an
    optimistic 78 s, plus compilation, setup and cold-cache variation.
  - `ci-macos-stress-functions` about 4–4.5 minutes (76 s + 163 s, plus
    setup).
  - `ci-linux-stress` about 4 minutes and `ci-macos-stress` about 5–6
    minutes after the workflow starts: each summary itself runs in seconds
    plus provisioning and queue time, but completes only after the slowest
    of its three workers.
  - Overall critical path: `ci-macos-stress`, about 5–6 minutes (down from
    about 9.5 minutes), bounded by `ci-macos-stress-packages`; the 4–5
    minute goal is likely missed there. macOS 02c worker times: pending
    until the first macOS worker runs of pull request #2, which are
    recorded in the flow handoff (see First remote run).

History (earlier iterations, kept as measured or estimated at the time):

- Measured: Linux, go1.26.4 linux/amd64 on a 16-thread Intel i7-11800H
  developer workstation (kernel 6.8), warm build cache, 2026-09-26, with the
  process-group spike's 1 s TERM-to-KILL grace: the whole stage took 372 s
  (6 min 12 s), of which `stress packages` about 201 s (slowest binary
  `internal/spikes/processgroup` 201 s; `internal/testkit/fakeadapter` 55 s,
  `internal/testkit` 31 s, `internal/spikes/gittransport` 24 s) and
  `stress function` 170 s. The resistant and leader-exits-first cases of the
  process-group experiment (`TestExperiment` and, until iteration 02b,
  `TestFP6ProcessGroups`) wait out the full grace on every repetition,
  which dominated both steps. The slowest binary used 201 s of its 6-minute
  timeout, so no package was split into its own step. With the earlier
  200 ms grace (2026-09-25) the stage took 180 s: processgroup 105 s,
  `stress function` 74 s.
- Measured with iteration 02 (plane trust): Linux, the same workstation,
  go1.26.4 linux/amd64, warm build cache, 2026-09-26. The combined six-test
  function step took 322.7 s (`ok  ./tests/function 322.742s`), reaching
  the 5-minute split trigger, so the static split above was applied and the
  full stage rerun: 546 s (9 min 6 s) in total, of which `stress packages`
  about 221 s (slowest binary `internal/plane` 220 s,
  `internal/spikes/processgroup` 200 s, `internal/testkit/fakeadapter`
  53 s, `internal/testkit` 31 s, `internal/spikes/gittransport` 24 s),
  `stress function` 170 s and `stress plane function` 154 s.
- Hosted baseline before iteration 02b (pull request #2's run at the time,
  both main jobs still running stress serially): `ci-linux` 685 s, of which
  stress 576 s; `ci-macos` 889 s, of which stress 806 s. Everything else
  took about 109 s and 83 s. macOS stress then used 806 s of the 900 s
  watchdog.
- Measured with iteration 02b (deduplicated): Linux, the same
  workstation, go1.26.4 linux/amd64, warm build cache, 2026-09-26,
  sequential runs on the same host (the "before" run from an unmodified
  copy of the parent commit). Before: 548.0 s in total, of which
  `stress packages` 220.7 s (slowest binary `internal/plane` 219.4 s,
  `internal/spikes/processgroup` 200.8 s, `internal/testkit/fakeadapter`
  53.4 s, `internal/testkit` 30.5 s, `internal/spikes/gittransport`
  23.9 s), `stress function` 171.9 s (binary 170.6 s) and
  `stress plane function` 155.3 s (binary 155.0 s). After: 309.1 s
  (5 min 9 s) in total, 238.9 s (44%) less: `stress packages` 219.5 s
  (slowest binary `internal/plane` 219.0 s, `internal/spikes/processgroup`
  201.4 s), `stress function` 30.0 s (binary 28.9 s) and
  `stress plane function` 59.4 s (binary 59.1 s); a repeat run took
  311.5 s (219.8 s, 32.2 s and 59.5 s).
- Expected per-job wall-clock after iteration 02b (an estimate at the
  time, since measured by run 36236333755 above): `ci-linux-stress` about
  325 s of stress and `ci-macos-stress` about 455 s of stress, plus setup.

Race and CPU scope: the top-level test packages are race-built, so the
process-group package's self-executed helper (its own test binary) is
instrumented. Helper binaries built by `testkit.BuildBinary` and
`testkit.BuildTestBinary` force CGO off, so the fake-adapter children, the
FP-5 helper binaries and the plane CLI children are not race-built; their
scenarios still repeat, and the direct package tests cover the same Go logic
under race. `-cpu` varies the top-level tests' GOMAXPROCS, not that of
independently launched children.

## Platform code

Convention: host wrappers may supply `runtime.GOOS`; every decision and every
OS-dependent formatting takes an explicit `goos` argument, and every
supported branch has a unit-test path for both `linux` and `darwin`, run on
any host. Injecting an OS string is evidence of decision logic, never proof of
foreign kernel behavior.

The guard is `devcheck.CheckPlatformSources` in
[internal/devcheck/platform.go](../internal/devcheck/platform.go), run by
`TestPlatformSourceGuard` in every ordinary test and native run. It parses
every non-test Go file of the repository with `go/parser`, irrespective of
build constraints (skipping `.git`, `.agents`, `.codex`, `.claude`, `.github`,
`design`, `vendor`, `testdata` and other dot-prefixed directories), rejects a
dot import of `runtime` and symlinked sources, and allows `runtime.GOOS` only
as the single forwarded argument of exactly these wrappers:

| File | Wrapper | Required body |
|---|---|---|
| `internal/cli/cli.go` | `Run` | `return runFor(ctx, runtime.GOOS, args, in, out, errOut)` |
| `internal/devcheck/devcheck.go` | `Run` | `return runFor(ctx, runtime.GOOS, args, out, errOut, run)` |
| `internal/spikes/processgroup/experiment.go` | `evaluate` | `evaluateFor(r, runtime.GOOS)` |
| `internal/spikes/processgroup/experiment.go` | `RunHelper` | `return runHelperFor(getenv, runtime.GOOS, runtime.GOARCH)` |
| `internal/testkit/fakeadapter/fakeadapter.go` | `Parse` | `return parseFor(args, runtime.GOOS, signalsSupported)` |

A missing or renamed wrapper fails the guard, so moving one is an intentional
policy change. Syscall exceptions are the four build-selected native files,
exempt from the wrapper policy only while they keep their build expressions:

- `internal/spikes/processgroup/sys_linux.go` (`linux`): the child subreaper
  and `Wait4` reaping.
- `internal/spikes/processgroup/sys_darwin.go` (`darwin`): the lifetime pipe
  and ESRCH polling while launchd reaps orphans.
- `internal/testkit/fakeadapter/signals_unix.go` (`linux || darwin`): OS
  signal registration and names.
- `internal/testkit/fakeadapter/signals_other.go` (`!linux && !darwin`): the
  generic unsupported fallback, outside supported-platform runtime
  qualification.

Exempt files are not scanned for `runtime.GOOS`, so
`internal/testkit/fakeadapter/signals_unix.go` must not grow a host branch:
it is compiled for both `linux` and `darwin`, and such a branch would be an
untested decision the guard cannot see.

The plane package (iteration 02) resolves its default state directory in
the pure `plane.ResolveStateDir(goos, ...)`, fed by `cli.Run`'s existing
wrapper, and adds no `runtime.GOOS` read. Its advisory state lock
(`syscall.Flock`) and its directory-sync fallback (a plain `syscall.Fsync`
on the directory descriptor when `File.Sync` reports `ENOTSUP`, `ENOTTY`
or `EINVAL`, the same policy on both systems) live in the build-selected
files `internal/plane/lock_unix.go` (`linux || darwin`) and
`internal/plane/lock_other.go` (`!linux && !darwin`, unsupported-system
errors that the CLI's platform rejection keeps unreachable). They make no
host decision and need no exemption: the guard does not list them, and
they would fail it if they read `runtime.GOOS`.

Native evidence limits: these files cannot be simulated by an OS parameter.
Linux behavior is proven by the native process experiments of `ci-linux`
(test) and the Linux stress workers (the process-group experiments in
`ci-linux-stress-processgroup`), Darwin behavior only by `ci-macos` (native)
and the macOS stress workers (`ci-macos-stress-processgroup`); the summaries
`ci-linux-stress` and `ci-macos-stress` only aggregate their workers'
results. Linux subreaper reaping and Darwin lifetime-pipe/ESRCH cleanup are
proven separately, each on its own runner, which needs a native cgo
compiler, processes, loopback and local POSIX filesystems. Host-independent
tests prove both decision branches on any host, and only the next
`ci-macos` run proves Darwin runtime behavior (its repeated stress, and the
concurrent processgroup plan, in the next macOS worker runs). No Windows or
foreign-architecture runtime claim is made. `runtime.GOARCH` only labels
output and is outside this guard.

## Branch protection

Nothing in this repository applies branch protection. The product owner applies
these settings by hand, with repository administration rights, in Settings →
Branches for repository `wedevwork/callsheet`, branch name pattern `main`,
after the check contexts exist (after they have reported in a remote run).
Keep any existing stronger rule; do not replace unrelated protections with a
blanket API write.

- Require a pull request before merging. No extra approving-review count is
  imposed by this iteration: the flow's code review already precedes PR
  creation.
- Require status checks to pass before merging: `ci-linux`, `ci-macos`,
  `ci-linux-stress`, `ci-macos-stress`. Select GitHub Actions as their
  expected source when available. Since iteration 02c the two stress
  contexts are reported by the summary jobs under the same names, and the
  six worker contexts are not required (the summaries gate on them), so no
  protection change is needed for 02c.
- Require branches to be up to date before merging.
- Do not allow bypassing the above settings (include administrators). No pull
  request bypass actors, no force pushes, no branch deletion.

Confirm the result with the read-only command

```
gh api repos/wedevwork/callsheet/branches/main/protection
```

and check that `required_status_checks.strict` is `true`, that all four
contexts `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress` are
listed (under `contexts` or `checks` as returned), that a pull request is
required, that `enforce_admins` is enabled, and that force pushes and
deletions are not allowed. Also inspect any repository or organization
rulesets that apply to `main`. If the repository plan or organization policy
prevents this configuration, record that as an owner-side blocker: a green
workflow alone does not mean merges are protected.

### Adding the stress contexts (pull request #2)

Iteration 02b added the contexts `ci-linux-stress` and `ci-macos-stress`;
iteration 02c keeps them unchanged, now reported by the summary jobs, so it
needs no protection migration of its own. GitHub offers a context for
selection only after it has reported once, and a green run does not prove
that the 02b contexts were ever added, so the order is fixed:

1. Finish the flow's code review of iterations 02, 02b and 02c (`REVIEW_APPROVED`) and commit the reviewed code on `iter-02-plane-trust`.
2. Push `iter-02-plane-trust`, updating pull request #2.
3. Observe all ten jobs report success on the current PR merge revision: the six stress workers and all four required checks, `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`.
4. Conditional 02b prerequisite, only if `ci-linux-stress` and `ci-macos-stress` are not yet required: the owner adds them, only after they have reported, with the additive request below.
5. The owner verifies protection with the read-only command below: all four required contexts, their GitHub Actions source bindings and the stronger settings.
6. Merge iterations 02, 02b and 02c together, only with all four checks green on the current merge revision.

No skipped, canceled, pending or unobserved result qualifies, and a later
push requires fresh current-revision evidence (return to step 3). The pull
request #1 protection handoff remains a prerequisite: the original two
contexts are already required.

For existing classic branch protection with the original two required
checks, the conditional prerequisite of step 4 is this exact
**owner-only** additive request, never executed by CI, tests or this flow:

```bash
gh api --method POST \
  -H 'Accept: application/vnd.github+json' \
  repos/wedevwork/callsheet/branches/main/protection/required_status_checks/contexts \
  -f 'contexts[]=ci-linux-stress' \
  -f 'contexts[]=ci-macos-stress'
```

It uses GitHub's
[add status check contexts endpoint](https://docs.github.com/en/rest/branches/branch-protection#add-status-check-contexts),
preserving the existing contexts rather than replacing branch protection.
The owner runs the read-only command before and after the request:

```bash
gh api repos/wedevwork/callsheet/branches/main/protection
```

and verifies all four contexts in `contexts` or `checks`, the retained
stronger checks and their source bindings, `strict` set to `true`, required
pull requests, `enforce_admins` enabled, and no bypass actors, force pushes
or deletions. Select or verify GitHub Actions as the expected source where
available. Inspect the applicable repository and organization rulesets too.
If classic protection is absent, privileges or the plan deny the request, or
rulesets own enforcement, stop the handoff and have the owner apply
equivalent settings in the appropriate rule; do not replace unrelated rules.
A green workflow alone does not prove protection. No settings are applied
automatically.

## PR flow

Iteration branches are named `iter-NN-<slug>`, for example
`iter-02-plane-trust`. Every iteration lands on `main` through a pull request
(for iterations 01, 01b and 01c, the single pull request #1 described below):

1. Create the iteration branch `iter-NN-<slug>` from `main`.
2. Implement, then run the flow's code review and fix loop until the reviewer returns `REVIEW_APPROVED`.
3. Commit the reviewed code.
4. Push the branch and open a pull request targeting `main`.
5. Wait for all four checks, `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`, to succeed on the current PR merge revision. A skipped, canceled, pending or unobserved check is not acceptable evidence.
6. If a check fails, fix it, have the changed code re-reviewed, push, and return to step 5. Updating the branch from `main` may require another run, because branches must be up to date.
7. Merge only after all four checks are green and the branch protection requirements are met.

Do not enable a merge queue, automate merging, or upload gitignored design or
review artifacts to the pull request.

Bootstrap exception (one time, already settled by the product owner):
iterations 01, 01b and 01c join pull request #1 (branch `iter-01b-ci`, which
adds this CI) and merge together after both checks, `ci-linux` and
`ci-macos`, succeed on its current merge revision. Actual platform failures
found by its runs are fixed through the flow and re-reviewed before they are
pushed. Because a required check must have reported before it can be
selected, the owner enables branch protection after both contexts are
available and successful. Do not begin merging iteration 02 before that
handoff is complete. The first pull request after protection (iteration 02)
verifies actual merge blocking. No fake first-run evidence belongs in
repository files.

Iterations 02b (CI speed) and 02c (stress shards) are delivered the same
way: iterations 02, 02b and 02c join pull request #2 (branch
`iter-02-plane-trust`) and merge together only after all ten jobs, and so
all four checks, are green on its current merge revision and the owner has
verified protection, adding the two stress contexts first only if they are
not yet required, in the order given in Branch protection (Adding the
stress contexts).

## First remote run

Local checks cannot prove runner provisioning, Go and action download or cache
behavior on GitHub, Darwin runtime results or repository protection. Those
remain pending until observed. After pushing, the owner records in the flow
handoff:

- the run URL and the commit it ran;
- the conclusions of all ten jobs: all four checks, `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`, and the six stress workers;
- native evidence from the `ci-macos` log: the line `devcheck: native qualification passed on darwin/<arch>` naming `TestFP6ProcessGroups` and its three scenarios;
- stress evidence from the six worker logs (the summaries hold none): the lines `devcheck: stage stress-packages ok`, `devcheck: stage stress-processgroup ok` and `devcheck: stage stress-functions ok` on each platform, the `-count=20` commands, every CPU invocation's outcome, and the elapsed time of each stress command with the runner's OS, architecture and cache state;
- the actual job and step times of all ten jobs, setup, queue and summary wait time included, and the overall workflow critical path; compare each worker with the expected per-job wall-clock in Stress checks, and diagnose any miss of the 4–5 minute goal and the remaining bottleneck without weakening tests or reducing counts;
- the branch protection verification described above (finishing the conditional 02b prerequisite first if it is needed).

The local validator checks action identity and full-SHA format only, not that
a SHA exists or matches its release comment. Confirm each pin against its
release with these read-only commands (update the tag names when pins change):

```
git ls-remote https://github.com/actions/checkout.git 'refs/tags/v6.0.2' 'refs/tags/v6.0.2^{}'
git ls-remote https://github.com/actions/setup-go.git 'refs/tags/v6.3.0' 'refs/tags/v6.3.0^{}'
```

For an annotated tag compare the peeled commit (the `^{}` line) with the
workflow SHA, otherwise the direct tag target. Successful action resolution is
then observed in the first remote run; running an action does not by itself
prove it corresponds to its release comment. Record unavailable or mismatching
resolution as a handoff blocker. These commands never run in local tests or in
CI.

## Local verification

Run from the repository root:

```
go run ./cmd/devcheck all
go run ./cmd/devcheck stress
go test -count=1 -run '^(TestCI|TestHardening|TestStressShard)' ./tests/function
go test -race -count=20 -cpu=1,2,4 -run '^TestStressConcurrencyContract$' ./internal/devcheck
go test -json -count=1 -run '^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$' ./tests/function
```

A single shard, as one CI worker runs it, is also available on its own:

```
go run ./cmd/devcheck stress-packages
go run ./cmd/devcheck stress-processgroup
go run ./cmd/devcheck stress-functions
```

Measure a shard without other stress work running on the same host, which
matches a hosted worker's isolation. The `TestStressConcurrencyContract`
command repeats the concurrent coordinator's timing-dependent contract at
the declared count; it is not part of any stress stage or of CI. The last
command of the first block is the focused evidence for the plane stress
selector: its events must show run and pass for exactly the eight selected
subtests and none for a `contracts` subtest.

`all` runs test (with race on Linux), coverage, bench and cross; `stress` is
explicit (see Stress checks), and release and review verification run both.
`internal/cicheck` and the `TestCI*`, `TestHardening*` and
`TestStressShard*` function tests validate the workflow structure, its
devcheck stages against the driver's dispatch, the summaries' predicate,
and this page, offline. `go run ./cmd/devcheck native` works on macOS only; on other hosts it
exits 1 with an unsupported-stage error. Optionally, a locally installed
`actionlint .github/workflows/ci.yml` can lint the workflow; it is not a
required dependency and nothing invokes it automatically.
