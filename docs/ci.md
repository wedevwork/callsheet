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

Eighteen fixed jobs run on every trigger. Exactly four of them are the
required status check contexts on `main`: `ci-linux`, `ci-macos`,
`ci-linux-stress` and `ci-macos-stress`. The other fourteen are the stress
workers (iteration 02c; the two plane workers since iteration 05b, the two
sidecar workers since its sidecar follow-up, the two plane CPU1 and the two
sidecar CPU1 workers since iteration 06a-perf), seven shards per platform;
their names are unique diagnostic checks, not required contexts:

| Check context | Runner | Timeout | Kind | Steps after setup |
|---|---|---|---|---|
| `ci-linux` | `ubuntu-24.04` | 45 min | required | `devcheck test` (native suite, then the same suite with `-race`), `devcheck coverage` (unit coverage must be greater than 80.0%), `devcheck bench` (git transport payload byte limits and commit/tree invariants, then the plane trust benchmarks: issuance, initialization and verified TLS health, the plane node benchmarks: heartbeat, snapshot of 100 nodes and durable enrollment, and the plane role benchmarks: role list and node views for 1 and 100 roles and durable add/set/rm transactions, then the node and role frame encode/decode benchmarks in `internal/contract`, then the sidecar ready-check benchmark (100 manual pairs, one shared probe) and the adapter's real fake-probe benchmark, then the task benchmarks: plane admission over 100 roles (first, last and no match, no filesystem), full-tail checkpoint writes of 0, 64 KiB and 10 MiB, the task envelope encode/decode at its maximum legal size in `internal/contract`, and the sidecar log-tail ring and maximum prompt composition, then the iteration 06a control benchmarks: the plane's per-task writer committing natural and lost terminal records and late evidence with 0, 64 KiB and 10 MiB tails (`BenchmarkControlCommit`: bytes written and bounded allocation), and one maximum sealed result, one maximum outbox and one 64-entry inventory page (`BenchmarkControlReplay` in `internal/contract` and `internal/sidecar`), each checking its invariants; timings are reported, never gated), `devcheck cross` (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64: 12 artifacts) |
| `ci-macos` | `macos-15` | 30 min | required | `devcheck native`: the complete suite as `go test -json`, which must show passing run and pass events in `github.com/wedevwork/callsheet/tests/function` for `TestFP6ProcessGroups` and its `cooperative`, `resistant` and `leader-exits-first` scenarios, for the plane trust tests, the node tests, the role tests, the task tests and the control tests (iteration 06a: the eight control function parents and the native group qualification), and in `github.com/wedevwork/callsheet/internal/sidecar` for `TestTaskExecutionContract` and its `process` subtest (see below) |
| `ci-linux-stress-packages` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-packages` on Linux (see Stress checks) |
| `ci-linux-stress-plane-cpu1` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-plane-cpu1` on Linux: `internal/plane` at CPU 1, one invocation alone on its worker (iteration 06a-perf) |
| `ci-linux-stress-plane` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-plane` on Linux: `internal/plane` CPU 2 and CPU 4 as two concurrent invocations (iteration 05b; CPU 1 on its own worker since iteration 06a-perf) |
| `ci-linux-stress-sidecar-cpu1` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-sidecar-cpu1` on Linux: `internal/sidecar` at CPU 1, one invocation alone on its worker (iteration 06a-perf) |
| `ci-linux-stress-sidecar` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-sidecar` on Linux: `internal/sidecar` CPU 2 and CPU 4 as two concurrent invocations (iteration 05b sidecar follow-up; CPU 1 on its own worker since iteration 06a-perf) |
| `ci-linux-stress-processgroup` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-processgroup` on Linux |
| `ci-linux-stress-functions` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-functions` on Linux |
| `ci-macos-stress-packages` | `macos-15` | 20 min | worker | `devcheck stress-packages` on Darwin, with the same commands, repeat count and CPU settings as Linux |
| `ci-macos-stress-plane-cpu1` | `macos-15` | 20 min | worker | `devcheck stress-plane-cpu1` on Darwin, likewise |
| `ci-macos-stress-plane` | `macos-15` | 20 min | worker | `devcheck stress-plane` on Darwin, likewise |
| `ci-macos-stress-sidecar-cpu1` | `macos-15` | 20 min | worker | `devcheck stress-sidecar-cpu1` on Darwin, likewise |
| `ci-macos-stress-sidecar` | `macos-15` | 20 min | worker | `devcheck stress-sidecar` on Darwin, likewise |
| `ci-macos-stress-processgroup` | `macos-15` | 20 min | worker | `devcheck stress-processgroup` on Darwin, likewise |
| `ci-macos-stress-functions` | `macos-15` | 20 min | worker | `devcheck stress-functions` on Darwin, likewise |
| `ci-linux-stress` | `ubuntu-24.04` | 5 min | required summary | no setup; succeeds only if the seven Linux workers all concluded `success` |
| `ci-macos-stress` | `ubuntu-24.04` | 5 min | required summary | no setup; succeeds only if the seven macOS workers all concluded `success` (Ubuntu only evaluates their status; it qualifies nothing about Darwin) |

The two main jobs and the fourteen workers start together on every trigger
and run independently: none waits for, depends on or is conditional on
another (no `needs`, matrix, job or step condition, path filter,
concurrency cancellation or `continue-on-error`), and each fails on its
own. Iteration 02b moved stress out of the two main jobs; iteration 02c
split each platform's stress job into three workers, so its shards run in
parallel with each other and with the main jobs; iteration 05b gave
`internal/plane` a fourth worker per platform, and its sidecar follow-up
gave `internal/sidecar` a fifth; iteration 06a-perf moved the plane and
sidecar CPU 1 invocations to a sixth and seventh worker per platform, so
the plane and sidecar workers now run CPU 2 and CPU 4 only. The main jobs
keep their stages and budgets.

Only the two summaries have dependencies, each on its own platform's seven
workers, and they keep the required stress contexts, so branch protection
needs no change. Each summary is the same small template with literal
job IDs, never a matrix or a dynamic expression:

```yaml
needs: [linux-stress-packages, linux-stress-plane-cpu1, linux-stress-plane, linux-stress-sidecar-cpu1, linux-stress-sidecar, linux-stress-processgroup, linux-stress-functions]
if: ${{ always() }}
defaults:
  run:
    shell: bash
steps:
  - name: Require every stress shard
    env:
      PACKAGES_RESULT: ${{ needs['linux-stress-packages'].result }}
      PLANE_CPU1_RESULT: ${{ needs['linux-stress-plane-cpu1'].result }}
      PLANE_RESULT: ${{ needs['linux-stress-plane'].result }}
      SIDECAR_CPU1_RESULT: ${{ needs['linux-stress-sidecar-cpu1'].result }}
      SIDECAR_RESULT: ${{ needs['linux-stress-sidecar'].result }}
      PROCESSGROUP_RESULT: ${{ needs['linux-stress-processgroup'].result }}
      FUNCTIONS_RESULT: ${{ needs['linux-stress-functions'].result }}
    run: test "$PACKAGES_RESULT" = success && test "$PLANE_CPU1_RESULT" = success && test "$PLANE_RESULT" = success && test "$SIDECAR_CPU1_RESULT" = success && test "$SIDECAR_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success
```

(`ci-macos-stress` is identical with `macos-` job IDs; its `run` line is
byte-identical.) `if: ${{ always() }}`
makes the summary evaluate after unsuccessful dependencies instead of being
skipped. Only all seven results `success` exit zero; `failure`,
`cancelled`, `skipped`, an empty or any unknown result in any position fails
the summary. The all-success
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

The node tests required on `ci-macos` (iteration 03) are one function test
per node FP, each with its mandatory subtests: `TestNodeTrust` `ca`, `pin`
and `rejections`; `TestNodeEnrollment` `identity`, `recovery` and
`locking`; `TestNodeProtocol` `hello` and `limits`; `TestNodeReconnect`
`restart`, `disconnect` and `shutdown`; `TestNodeLease` `expiry` and
`return`; `TestNodeRegistry` `restore` and `validation`;
`TestNodeDiscovery` `text`, `json` and `errors`; `TestNodePlatform`
`paths`, `native-state`, `policy` and `sticky-write`. That is 30 more
names, 58 in all, with the 28 earlier names unchanged and first. The
subtests that delegate to package contracts (`recovery`, `restart`,
`disconnect`, `expiry`, `return`, `paths`, `native-state`, `policy` and
`sticky-write`) require run and pass evidence for the named contract and
subtest, and they prove on the runner itself the sidecar's native state
modes, kernel `flock` between processes, atomic enrollment replacement,
verified TLS trust, stream reconnect and signal cleanup.

The role tests required on `ci-macos` (iteration 04) are one function test
per role FP, each with its mandatory subtests: `TestRoleConfiguration`
`fields` and `order`; `TestRoleAdapter` `disabled` and `probe`;
`TestRoleValidation` `remote` and `rejections`; `TestRoleProtocol` `duplex`
and `bounds`; `TestRoleReadiness` `changes` and `reconnect`;
`TestRolePersistence` `restore` and `failures`; `TestRoleCommands` `text`,
`json` and `trust`; `TestRoleMutation` `races` and `remove`;
`TestRolePlatform` `manuals`, `executable` and `policy`. That is 29 more
names, 87 in all, with the 58 earlier names unchanged and first. The
subtests that delegate to package contracts follow the design's four-column
delegation table, declared once in `internal/devcheck` (`RoleDelegations`):
`duplex` and `bounds` each run both the `internal/plane` and the
`internal/sidecar` `TestRoleStreamContract` and pass only when both
packages independently show run and pass evidence for the named contract
and subtest (identical names in one package never satisfy the other);
`changes` runs the sidecar's `TestRoleReadinessContract/changes`,
`reconnect` the plane's `TestRoleDistributionContract/reconnect`,
`failures` the plane's `TestRoleRegistryContract/failures`, `races` the
plane's `TestRoleMutationContract/races` including
`races/snapshot-in-flight`, `manuals` the sidecar's and `executable` the
adapter's `TestRolePlatformContract`, and `policy` devcheck's
`TestRolePolicy/policy`, which also proves that missing either protocol
package's evidence fails qualification. A delegated run whose output shows
a failure, a skip, `no tests to run` or missing run/pass lines fails even
when its binary exited zero. On the runner itself they prove the worker's
native manual checks (symlinks, spaces, FIFOs, devices, permissions by the
actual open result, case behavior of the volume), the real fake adapter's
probe, loopback TLS, the plane and sidecar stream arbitration and the
durable role registry.

The task tests required on `ci-macos` (iteration 05) are one function test
per task FP, each with its mandatory subtests: `TestTaskModel` `envelope`
and `states`; `TestTaskDispatch` `selection`, `gate-race` and
`reserved-slot`; `TestTaskProtocol` `duplex`, `bounds`, `result-receipt`
and `result-ack-loss`; `TestTaskExecution` `compose`, `invoke`, `exit`,
`process`, `platform` and `policy`; `TestTaskLogs` `retention`,
`backpressure` and `final`; `TestTaskPersistence` `durability` and
`restore`; `TestTaskCommands` `dispatch`, `ls`, `show`, `logs` and
`trust`; `TestTaskRoles` `counts`, `mutation`, `remaining-capacity` and
`recovery-remove`; `TestTaskRecoveryBoundary` `disconnect`,
`remaining-capacity` and `recovery-remove`. That is 41 more names, 128 in
all, with the 87 earlier names unchanged and first. Every subtest except
the five `TestTaskCommands` ones delegates by the design's four-column
table, declared once in `internal/devcheck` (`TaskDelegations`, 41 rows for
27 delegating wrappers): the protocol, log, role and recovery wrappers run
the same-named contract in both `internal/plane` and `internal/sidecar`
(`TestTaskStream`, `TestTaskOutput`, `TestTaskRoleIntegration`,
`TestTaskInterruption`) and pass only when both packages independently
show run and pass evidence; `envelope` runs `internal/contract`'s
`TestTaskContract/envelope`, `states`, `durability` and `restore` the
plane's `TestTaskStore`, the dispatch wrappers the plane's
`TestTaskAdmission`, `compose`, `exit` and `process` the sidecar's
`TestTaskExecutionContract`, `invoke` the adapter's `TestTaskAdapter`
(`invocation`, `extraction` and `signals`), `platform` the sidecar's
`TestTaskPlatform/platforms`, and `policy` devcheck's
`TestTaskPolicy/policy`. The same delegation rules as the role table
apply: a failure, skip, `no tests to run` or missing run/pass line fails
the wrapper even when its binary exited zero, and one package's evidence
never satisfies the other's row. Separately from the function package, the
native stream must itself show run and pass events for
`TestTaskExecutionContract` and `TestTaskExecutionContract/process` in
`internal/sidecar` (`NativeTaskProcessPackage`): that is the real
task-process qualification on the runner (actual stdin, stdout and stderr,
a new process group whose PGID is the child's PID and differs from the
supervisor's, working directory and environment, scratch permissions,
heartbeats after exit, two sequential fresh children and group cleanup;
the first child reports its actual working directory, that directory's
permissions and its environment through the fixture-only
`CALLSHEET_FAKE_TASK_REPORT` stderr line, asserted on both systems), so
a function wrapper alone never qualifies Darwin task execution. Since
iteration 06a that qualification is guardian-backed: every task child is
led by its internal guardian (the callsheet binary re-executed with the
reserved `__callsheet_task_guardian_v1` argument), whose PID is the task
group's PGID and differs from the supervisor's, while the adapter's PID is
a distinct member of that group; the process subtest launches one probe
and two sequential guardian/adapter pairs.

The control tests required on `ci-macos` (iteration 06a, resilient
execution, protocol 4) are one function parent per FP:
`TestControlDurability`, `TestControlReconnect`, `TestControlNodeLoss`,
`TestControlLaunchSafety`, `TestControlWorkerRecovery`,
`TestControlPlaneRecovery`, `TestControlLateResult` and
`TestControlLegacy`, plus the direct real-binary group qualification
`TestControlNativeGroups` and its `cooperative`, `resistant`,
`orphan-restart` and `plane-restart` scenarios. That is 13 more names, 141
in all, with the 128 earlier names unchanged and first; no 06b name is
required. Each parent delegates by the single control table in
`internal/devcheck` (`ControlDelegations`, 14 rows for 8 wrappers) to the
package contracts of its FP, as a conjunction over every listed package:
`TestControlCommit` (plane), `TestControlProtocol` (contract, plane and
sidecar), `TestControlLease` (plane and sidecar), `TestControlLaunch`
(sidecar), `TestControlRestart` (sidecar), `TestControlPlaneRestart`
(plane and sidecar), `TestControlLate` (plane and sidecar) and
`TestControlMigration` (contract and plane). The delegation rules of the
earlier tables apply unchanged. `TestControlNativeGroups` is not an FP: each
scenario starts exactly one sidecar fixture (the sidecar package's compiled
test binary running a production Run whose only change is a probe-free
fake adapter), one guardian, one fake CLI leader and one fake descendant,
four children per scenario and sixteen per invocation (asserted), with the
plane in process. It asserts the guardian's PGID equals its PID and
differs from the sidecar's, the leader and descendant in that group, no new
session, TERM delivery and whole-group completion (`cooperative`), grace
then KILL of a TERM-resistant descendant after the leader exits
(`resistant`, triggered by the sidecar's cleanup, never a timeout), the
guardian's reparenting and identity after the sidecar fixture is SIGKILLed
and the recovery by this test process as the replacement sidecar
(`orphan-restart`), and the same leader PID producing output and exiting
across a plane restart (`plane-restart`), each ending with the group
absent (ESRCH). It runs in the normal, race and native suites only, never
in a stress shard.

The main jobs and all fourteen workers check out the event's revision without
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
In CI it runs as seven shards per platform (iteration 02c; the plane shard
since iteration 05b, the sidecar shard since its sidecar follow-up, the
plane CPU1 and sidecar CPU1 shards since iteration 06a-perf), one worker
job each: `ci-linux-stress-packages`, `ci-linux-stress-plane-cpu1`,
`ci-linux-stress-plane`, `ci-linux-stress-sidecar-cpu1`,
`ci-linux-stress-sidecar`, `ci-linux-stress-processgroup` and
`ci-linux-stress-functions` run `devcheck stress-packages`,
`devcheck stress-plane-cpu1`, `devcheck stress-plane`,
`devcheck stress-sidecar-cpu1`, `devcheck stress-sidecar`,
`devcheck stress-processgroup` and `devcheck stress-functions` on Linux,
and the seven `ci-macos-stress-*` workers run the same stages on macOS.
The project's declared repeat count is 20 per CPU setting (1, 2, 4). The
flow's coder and reviewer use this count when they re-run timing-dependent
tests they add or modify: `devcheck stress` for tests in its covered packages,
and the equivalent `go test -race -count=20 -cpu=1,2,4 -run '<tests>'
<package>` command for changed timing tests outside that set, such as the
coordinator's own `TestStressConcurrencyContract` (see Local verification).

The count, CPU list, shards, package groups, selectors, wave schedule and
time budgets are declared once, in `internal/devcheck/stress.go`
(`StressCount`, `StressShards`, `stressWaves`, and `StressSteps`, the
flattened inspection view). The seven shards run thirteen commands (argv,
never a shell), each with `CGO_ENABLED=1`, named `stress packages`,
`stress plane cpu1`, `stress plane cpu2`, `stress plane cpu4`
(iteration 05b), `stress sidecar cpu1`, `stress sidecar cpu2`,
`stress sidecar cpu4` (the iteration 05b sidecar follow-up),
`stress processgroup cpu1`, `stress processgroup cpu2`,
`stress processgroup cpu4`, `stress function`, `stress plane function` and
`stress node function` (iteration 03); the packages command gained
`./internal/adapter` in iteration 04, gave `./internal/plane` to the plane
shard in iteration 05b and gave `./internal/sidecar` to the sidecar shard
in its sidecar follow-up. Iteration 06a-perf changed no command, only the
grouping: `stress plane cpu1` and `stress sidecar cpu1` each have a shard
of their own:

```
go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/contract ./internal/adapter
go test -race -count=20 -cpu=1 -timeout=6m ./internal/plane
go test -race -count=20 -cpu=2 -timeout=6m ./internal/plane
go test -race -count=20 -cpu=4 -timeout=6m ./internal/plane
go test -race -count=20 -cpu=1 -timeout=6m ./internal/sidecar
go test -race -count=20 -cpu=2 -timeout=6m ./internal/sidecar
go test -race -count=20 -cpu=4 -timeout=6m ./internal/sidecar
go test -race -count=20 -cpu=1 -timeout=6m ./internal/spikes/processgroup
go test -race -count=20 -cpu=2 -timeout=6m ./internal/spikes/processgroup
go test -race -count=20 -cpu=4 -timeout=6m ./internal/spikes/processgroup
go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function
go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$ ./tests/function
go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$ ./tests/function
```

| Shard | Stage | Commands | Execution |
|---|---|---|---|
| `packages` | `devcheck stress-packages` | `stress packages` | one invocation |
| `plane-cpu1` | `devcheck stress-plane-cpu1` | `stress plane cpu1` | one invocation, alone on its worker (iteration 06a-perf) |
| `plane` | `devcheck stress-plane` | `stress plane cpu2`, `cpu4` | two concurrent invocations, one per CPU setting (iteration 05b; CPU 1 moved to `plane-cpu1` in iteration 06a-perf) |
| `sidecar-cpu1` | `devcheck stress-sidecar-cpu1` | `stress sidecar cpu1` | one invocation, alone on its worker (iteration 06a-perf) |
| `sidecar` | `devcheck stress-sidecar` | `stress sidecar cpu2`, `cpu4` | two concurrent invocations, one per CPU setting (iteration 05b sidecar follow-up; CPU 1 moved to `sidecar-cpu1` in iteration 06a-perf) |
| `processgroup` | `devcheck stress-processgroup` | `stress processgroup cpu1`, `cpu2`, `cpu4` | three concurrent invocations, one per CPU setting |
| `functions` | `devcheck stress-functions` | `stress function`, then `stress plane function`, then `stress node function` | sequential |

`StressSteps` lists the thirteen commands in shard and CPU order, the same
commands in the same order as before iteration 06a-perf. It is an
inspection view, never the execution order: a concurrent shard runs its
invocations in the waves `stressWaves` schedules, and that schedule is
empty, so every concurrent shard starts all of its invocations at once:
the plane and sidecar CPU1 shards their one, the plane and sidecar pairs
their two and processgroup its three.

Since iteration 06a-perf `devcheck stress-plane` and `devcheck stress-sidecar`
now run CPU 2 and CPU 4 only. To repeat all three CPU settings of plane,
run both `devcheck stress-plane-cpu1` and `devcheck stress-plane` (or
`devcheck stress`); likewise `devcheck stress-sidecar-cpu1` and
`devcheck stress-sidecar` for sidecar.

These are argv displays, not shell-ready commands: quote the entire `-run`
argument when running one through a shell. The plane selector is one
two-level pattern: `go test` splits a `-run` pattern into per-level patterns
only at a `/` outside parentheses and brackets, so it selects the three
parents at the top level and only the eight named subtests below them. It
must not be "simplified" into a flat alternation, which would select
different tests. The node selector (iteration 03) is built the same way:
`TestNodeEnrollment` and `TestNodeReconnect` at the top level, and only
their `locking` and `shutdown` subtests below them.

- Selected packages: `internal/testkit`, `internal/testkit/fakeadapter`,
  `internal/spikes/gittransport`, `internal/client`, `internal/contract`
  and `internal/adapter` in the packages shard, `internal/plane` in the
  plane shards (iteration 05b; CPU 1 in `plane-cpu1` and CPU 2 and 4 in
  `plane` since iteration 06a-perf), `internal/sidecar` in the sidecar
  shards (its sidecar follow-up; split the same way since iteration
  06a-perf), and `internal/spikes/processgroup` in the
  processgroup shard, complete
  package tests (not benchmarks). The fake adapter is included because its
  signal handling and descendant lifecycle are timing-sensitive too; the
  plane package (iteration 02) because its listener, shutdown, lock-holder
  helper process and contracts are, and they run there directly under the
  race detector. Iteration 03 added the node packages completely: the
  plane's lease, registry and stream tests (`TestNodeLeaseContract` among
  them) already run in the plane package, and `internal/client`,
  `internal/sidecar` (whose `TestNodeReconnectContract` starts real plane
  subprocesses and drives an injected retry clock) and `internal/contract`
  joined the packages invocation (sidecar left it for its own shard in the
  iteration 05b sidecar follow-up). Iteration 04 added `internal/adapter`
  (probe deadlines, cancellation and child waits on an injected clock, the
  real fake fixture) to the same invocation. Every timing-dependent role
  contract (`TestRoleStreamContract` of plane and sidecar,
  `TestRoleDistributionContract`, `TestRoleReadinessContract`,
  `TestRoleMutationContract`, `TestRoleRegistryContract`,
  `TestRoleValidationContract`, the client and contract role tests and
  `TestAdapterContract`) runs in its own package's shard, so the role
  function wrappers are not repeated: no new function selector exists and
  no delegated contract repeats twice.
  Iteration 06a (resilient execution) adds no package, selector or shard
  either: its contracts (`TestControlCommit`, `TestControlProtocol`,
  `TestControlLease`, `TestControlLaunch`, `TestControlRestart`,
  `TestControlPlaneRestart`, `TestControlLate`, `TestControlMigration`,
  `TestControlJournalCodec` and devcheck's `TestControlPolicy`) run in
  their packages' existing shards (plane and sidecar with CPU 2 and 4
  concurrent and, since iteration 06a-perf, CPU 1 alone in a shard of its
  own; contract and client in the packages shard) with
  injected guardians, groups and clocks and zero additional children; the
  control function wrappers and `TestControlNativeGroups` are not
  repeated. The only real-child change is the sidecar's existing process
  qualification (below): five children per repetition instead of three.
  Iteration 05 (tasks) adds no package, selector or shard: the task
  contracts (`TestTaskContract`, `TestTaskAdmission`, `TestTaskStream` and
  `TestTaskOutput` of plane and sidecar, `TestTaskStore`,
  `TestTaskExecutionContract`, `TestTaskRoleIntegration`,
  `TestTaskInterruption`, `TestTaskPlatform`, `TestTaskAdapter` and the
  client's `TestTaskClient`) already run in their own packages, and the
  task function wrappers are not repeated. Only
  `TestTaskExecutionContract/process` launches real operating-system
  children: one probe and two sequential task children per repetition
  in iteration 05, which was 3 × 20 × 3 = 180 additional children, in the
  sidecar shard since the iteration 05b sidecar follow-up (with
  `internal/sidecar`; in the packages shard before, the count unchanged);
  since iteration 06a each task child is a guardian/adapter pair, so one
  probe and two pairs are five per repetition, 5 × 20 × 3 = 300 in the
  sidecar shards (two more per repetition, 120 more in all), 100 in each of
  its three CPU invocations (CPU 1 alone in `sidecar-cpu1` since iteration
  06a-perf, CPU 2 and 4 concurrent in `sidecar`), and none in the packages,
  plane, processgroup or functions shards. Each natural completion also charges
  the guardian's one-second group cleanup grace (design 06a DW1), about
  40 s per CPU invocation at count 20. Every other new task test
  uses a counted injected process factory and asserts zero launches
  (`TestTaskPolicy/policy` holds that ledger and a source guard over the
  new test files).
- Selected function tests: only `TestFP4TransportHarness` and
  `TestFP5GitRoundTrip` (`stress function`), and the process-boundary
  subtests of the listener- and lock-bearing plane trust tests
  (`stress plane function`): `TestPlaneState` `paths`, `persistence`,
  `locking` and `validation`; `TestPlaneTLS` `https-only`,
  `prelisten-validation` and `bounded-shutdown`; `TestPlaneReissue`
  `process`. All are in `tests/function`; unrelated function tests such as
  the twelve-artifact cross-build test are deliberately excluded. The
  node step (`stress node function`, iteration 03) selects only the two
  unique process-boundary node scenarios, `TestNodeEnrollment` `locking`
  (real competing sidecar processes) and `TestNodeReconnect` `shutdown`
  (a real CLI sidecar's SIGTERM cleanup). The delegated clock contracts
  behind `TestNodeReconnect` `restart` and `disconnect` and behind
  `TestNodeLease` are repeated directly in the sidecar and plane shards, so their
  function wrappers are excluded here, exactly as the plane `contracts`
  subtests are. The three selectors are disjoint, and each plane parent's
  `contracts` subtest is not selected. The other plane trust tests (`TestPlaneCommands`,
  `TestPlaneBind`, `TestPlaneInit`, `TestPlaneStatus`, `TestPlanePlatform`)
  run in the normal native suites only; their listener mechanisms are
  repeated by the plane package tests and by `TestPlaneTLS` and
  `TestPlaneReissue`. Each function command also fails when `go test`
  reports `no tests to run` for its selector (`[no tests to run]` or
  `testing: warning: no tests to run`): an empty selection is never a pass.
- The shards' union is exactly the iteration 02b selection plus iteration
  03's additions (the three node packages and the node selector) and
  iteration 04's `internal/adapter`: every (package, selector, CPU setting,
  count) combination appears exactly once.
  Only processgroup's (iteration 02c), plane's (iteration 05b) and
  sidecar's (its sidecar follow-up) single `-cpu=1,2,4` invocations became
  three invocations of one CPU setting each; the per-test repetitions (60), the CPU settings, `-race` and the
  timeouts are unchanged. The plane function wrappers whose contracts
  delegate to the plane package stay excluded from stress; those contracts
  now repeat in the plane shards. Iteration 06a-perf moved plane's and
  sidecar's CPU 1 invocations into shards of their own without changing a
  command, count or tuple: the union is still the same 36 (package,
  selector, CPU setting, count) tuples, each exactly once, on both
  platforms.
- `-count=20` applies at each CPU setting: every selected test runs
  60 times per platform across its shards (20 per CPU setting). This is repeated testing, never retry-until-green:
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
plane-cpu1, plane, sidecar-cpu1, sidecar, processgroup, functions; a shard
stage runs exactly its own commands. Sequential commands stop at the first
failure, and a failed shard prevents the next. A failure in `plane-cpu1`
during `devcheck stress` prevents `plane` and every later shard from
starting, and a failed CPU 2 invocation still waits for CPU 4 before its
shard fails. The plane-cpu1, plane, sidecar-cpu1, sidecar and processgroup
shards are concurrent (Parallel): each one's commands start together (one
in a CPU1 shard, two in the plane and sidecar pairs, three in
processgroup), at most three at once within that shard (no two shards
overlap, locally or on a worker), under the same watchdog, and the shard
waits for all of them before it returns; a CPU1 shard goes through the same
coordinator for the same log format, failure handling and join. Each
command writes only to its own log in the scratch directory
(`stress-plane-cpu1.log`, `stress-sidecar-cpu1.log` and so on), and each
stage owns only the logs of its own commands: `devcheck stress-plane-cpu1`
owns only `stress-plane-cpu1.log`, `devcheck stress-plane` only
`stress-plane-cpu2.log` and `stress-plane-cpu4.log`, and likewise for
sidecar; no log is duplicated into the CPU 2 and CPU 4 worker. The
commands and log paths are printed before launch, and after all of a
shard's commands have finished each log is replayed from disk under its
command name in CPU order, with its elapsed time and outcome (for example
`devcheck: stress plane cpu4: ok in 97.3s`; that elapsed time includes the
go command's build). A failing invocation does not cancel its siblings: all
of the shard's results are collected, then the shard fails, listing every
failure in CPU order with its command. A log that cannot be created, written, closed or replayed fails the
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

Why processgroup is concurrent (iteration 02c), and its timing risk. Its
stress time was dominated by waiting, the 1 s TERM-to-KILL grace of its
resistant and leader-exits-first cases repeated one after another (hosted
211 s Linux and 234 s macOS in one `-cpu=1,2,4` binary), not by CPU. The
function commands stay sequential. Processgroup runs alone on its worker,
with at most three top-level `go test` commands. `-cpu` sets each top-level test binary's
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

Why plane has its own concurrent shard (iteration 05b), and its timing
risk. In the first hosted run of iteration 05 (pull request #5, run
36315881191, commit c4ab9e4) the `internal/plane` binary of the packages
command reached its 6-minute timeout on both runners
(`FAIL internal/plane 360.096s` on Linux, `360.066s` on macOS); every test
running at the alarm had just started, which is cumulative cost, not a
hang. Plane's stress time is CPU-bound under the race detector (about
12 ms per TLS plane-and-worker fixture of the task tests), unlike
processgroup's. That 360 s is a censored, contended observation, not an
isolated plane time: `go test` runs different package binaries
concurrently (its `-p` default is GOMAXPROCS, four on the assumed Linux
runner and three on macOS, independent of each binary's `-cpu`), so plane,
sidecar and contract competed for the same cores. Plane therefore left the
packages command for its own worker per platform, and its three CPU
settings run as three concurrent invocations of the same coordinator, each
at the declared count: 60 repetitions per test, as before. A lone
sequential plane worker was rejected, because isolation alone leaves all 60
repetitions under one 6-minute timer. (Since iteration 06a-perf plane's
CPU 1 invocation runs on its own worker; see below.)

`-cpu` sets GOMAXPROCS per test binary, not a core reservation: until
iteration 06a-perf 1 + 2 + 4 permitted seven Go execution threads across
the three plane binaries of one worker; since then CPU 1 runs alone and
2 + 4 permits six on the plane worker, and runtime and compiler work add
contention. The standard runners assumed
here have four vCPUs (`ubuntu-24.04`) and three M1 cores (`macos-15`,
arm64), per GitHub's hosted-runner reference checked on 2026-09-27; record
the actual architecture and core count with every hosted measurement, since
a different allocation invalidates the estimate, not the policy. The plane
task, role and node tests synchronize on events and injected clocks, and
concurrency does not shorten their simulated deadlines, but real bounds
remain: `testWait` and helper joins (20 s), HTTP test clients (10 s), TLS
dial (5 s), `TestServerFailureContract/shutdown-deadline` (completion within
4 s after an injected 50 ms shutdown grace), node shutdown within the
production 5 s bound (the blocked-stream case with a 50 ms close grace), and
the server's 5/10/10/30 s header, read, write and idle deadlines.
Millisecond polling in the role stream tests is bounded by `testWait`, not a
correctness sleep. Added scheduling, GC and TLS pressure can still violate
the real bounds: event synchronization is not immunity to overload. Every
assertion and duration is unchanged. A newly observed timing failure blocks
qualification and is investigated; it is never retried to green, relaxed,
or answered with other CPU settings or a platform-specific plan.

Pre-authorised plane fallback (superseded). `stressWaves` in
`internal/devcheck/stress.go` is the static wave schedule of the concurrent
shards, keyed by shard name. It is empty, so every concurrent shard runs
all of its invocations as one wave. Iteration 05b pre-authorised a plane
fallback for its three-way plane shard, decided in advance: on the first
hosted pull-request run containing that shard, both plane workers'
`devcheck stress-plane` logs were read. The fallback triggered if any line
`devcheck: stress plane cpuN: ok in Xs` or
`devcheck: stress plane cpuN: FAILED after Xs` reported X > 300.0 s (exactly
300.0 does not), or if the CPU-labelled replay of `stress-plane-cpuN.log`
showed `panic: test timed out after 6m0s` or the plane binary's timeout, even
if the worker ended before its outcome line. X was the elapsed time
`devcheck` reports, go command build included; never the job time or the
package summary. Missing logs were an evidence blocker, not a trigger. A
`FAILED` line over 300.0 s caused by a non-timeout assertion was both a
trigger and an investigation blocker: the failure was to be diagnosed first
and never masked by the fallback. When triggered on either platform, the
light flow would have set `"plane": {{4}, {1, 2}}` for both: CPU 4 alone,
then CPU 1 and 2 together only after the first wave succeeded, in the same
stage, worker, watchdog and scratch directory. The fallback did not
trigger: in run 36325089074, the first hosted run with the plane workers,
the slowest plane invocation took 146.6 s on Linux and 202.8 s on macOS
(both CPU 1), with no timeout, so `stressWaves` stayed empty. Iteration
06a-perf supersedes it: plane's CPU 1 invocation runs on its own worker,
and the two-step plane shard now rejects it before any scratch directory or
child exists (a wave schedule must cover a shard of one step per CPU
setting). There is one fallback policy, and it is not active: the wave
mechanism is proven on the three-CPU processgroup shard
(`TestStressConcurrencyContract/waves`: CPU 4 alone, then CPU 1 and 2, and
a first-wave failure, log error or cancellation prevents the second), and
a future wave policy for any shard requires a design revision.

Sidecar has its own concurrent shard (iteration 05b sidecar follow-up,
applied). With iteration 05b, sidecar first stayed in the packages shard.
Its contended macOS binary time in run 36315881191 was 310 s: 50 s (14%)
below the 6-minute limit, tolerating only a 16% slowdown. Removing plane
removed a concrete concurrent competitor under the default `-p`; that was
the basis of a 260–310 s post-change estimate, not a measured isolated
time. Contract's 236 s was the next-heaviest observed binary, not an
assumed floor. The follow-up rule was decided in advance: it triggers when
the first post-split hosted packages run on either platform reports
sidecar over 300.0 s, read from the `ok`/`FAIL` line of `internal/sidecar`
in the `devcheck stress-packages` log (not the job time), or a sidecar
binary timeout. The evidence is that sidecar line of the pull request's
hosted packages worker, taken after plane left the shard; no temporary
workflow is added. When the rule triggers, the follow-up is a fifth shard
running sidecar's three CPU settings as concurrent invocations. A fifth
sequential shard is not an option, because no isolated sidecar measurement
exists to justify it. The rule triggered on Linux in run 36325089074, the
first hosted run after the plane split: `ok internal/sidecar 307.788s` in
`ci-linux-stress-packages`, 52 s below the 6-minute limit in the job on the
critical path (macOS `247.885s`, below the trigger; one platform is
enough). The follow-up is therefore applied on both platforms through the
light flow: stage `stress-sidecar` and shard `sidecar` after plane, workers
`ci-linux-stress-sidecar` and `ci-macos-stress-sidecar`, fourteen jobs and
five-result summaries, the four required contexts unchanged, and the 180
task children moving with sidecar (no count change). The concurrent
sidecar binaries overlap their real process work (the task and probe
children, the reconnect contracts' real plane subprocesses and the lock
helper processes), so both native hosted sidecar workers must pass the
unchanged process, cleanup and timing assertions. A sidecar timeout or
assertion failure remains a failed qualification, and no further
concurrency escalation or timing relaxation is authorised; the concurrent
split promises no threefold speedup.

Plane and sidecar CPU 1 run on workers of their own (iteration 06a-perf).
CPU 1 is the slowest invocation of both packages on every host measured,
and each worker's concurrent race binaries compete for the same few cores
(three on the macOS runner). The owner chose to move each CPU 1 invocation
away from competing race binaries onto an independent worker per platform,
with its own diagnostics and without lengthening another worker's critical
path, while CPU 2 and CPU 4 stay concurrent on the plane and sidecar
workers. Commands, counts, CPU settings, `-race`, timeouts, tests and the
36-tuple selection are unchanged; only the grouping and the CI topology
change: eighteen jobs, seven-result summaries and the same four required
contexts. Evidence supplied to design 06a-perf (local, not hosted): a
single race-built iteration of plane and sidecar at CPU 1 took 7.829 s and
8.019 s; three concurrent count-20 plane invocations passed at 167.493 s
(CPU 1), 128.827 s (CPU 2) and 106.710 s (CPU 4); the CPU 1 binary's
profile holds 118.77 s of CPU samples over 166.32 s: racecall 15.22% flat,
tsan reads 13.90%, TLS handshakes 12.51% cumulative, initialization 4.13%.
That supports avoiding competing race binaries; it does not measure an
isolated hosted worker. The reported local `devcheck stress-plane` range of
153–164 s is not an isolated-CPU baseline. The local sidecar count-20 CPU 1
range was 135–168 s. Earlier socket-denied runs are excluded.

Hosted reference invocation times (seconds) supplied to design 06a-perf:

| Package | Linux CPU1 / CPU2 / CPU4 | macOS CPU1 / CPU2 / CPU4 |
|---|---|---|
| plane | 223.8 / 173.9 / 149.3 | 322.4 / 229.2 / 197.2 |
| sidecar | 238.6 / 188.6 / 164.3 | 327.1 / 240.0 / 207.9 |

The historical macOS plane CPU 4 run failed; its elapsed time is not a
correctness pass. The hosted CPU 1 macOS/Linux ratios are 1.441 (plane) and
1.371 (sidecar); they are never applied to a different local host as a
hosted forecast. No workload-cost saving is assumed. The macOS CPU 1 target
of ≤250 s is a hypothesis for the first remote run, not a guarantee; the
owner's completion rule, passing CPU 1 invocations at ≤300.0 s on both
platforms, and its report-to-owner procedure are in First remote run.

Deduplication (iteration 02b): stress repeats nothing that another stress
command already repeats at the declared count. Only these repeated
invocations were removed; each still runs once in the ordinary suites,
`devcheck test` on Linux and `devcheck native` on macOS:

| Removed stress work | Repeated 60 times by | Still run ordinarily |
|---|---|---|
| `TestFP6ProcessGroups` | `internal/spikes/processgroup` `TestExperiment` (processgroup shard): the same `RunExperiment` with the cooperative, resistant and leader-exits-first cases, requiring `rep.Pass()`; the package's evaluation and cleanup tests remain | the whole FP-6 test with all three scenario assertions; still mandatory Darwin native evidence |
| `TestPlaneState`'s delegated native state contract | `internal/plane` `TestNativeStateContract` (packages shard until iteration 05b, now the plane shard): modes, no-replace, rename, flock | `TestPlaneState` `contracts` invokes the identical contract with its mandatory evidence list |
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
  seven shards in one process, one after another, under one shared
  15-minute watchdog rather than seven; it is not a simulation of
  independent hosted workers, and a local expiry is therefore not evidence
  about any hosted worker.
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
  weakened. Iteration 05b applied this to `internal/plane` on both
  platforms, by design revision, as its own shard of three concurrent
  single-CPU invocations, and its pre-decided sidecar follow-up applied it
  to `internal/sidecar` likewise when the sidecar binary passed its
  300.0 s trigger. Iteration 06a-perf, by design revision, moved each
  package's CPU 1 invocation to a shard and worker of its own, changing no
  command.
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
- Iteration 03 allocation (design 03, CI plan; planning estimates from the
  hosted run 36236333755, not measurements): the node packages add about
  8 s to the packages shard's critical path on Linux and 21 s on macOS
  (plane's 185 s / 239 s binary becomes about 193 s / 260 s; the new
  `internal/client`, `internal/sidecar` and `internal/contract` binaries
  run concurrently in the same invocation and must stay below the plane
  binary), and `stress node function` adds about 15 s on Linux and 39 s on
  macOS to the functions shard (about 138 s and 308 s including setup). The
  normal `devcheck test` and `devcheck native` function package keeps its
  shared `-timeout=180s` for all `tests/function` cases together. If hosted
  measurements exceed these allocations, repeated helper compilation and
  fixture setup are removed first, keeping every case and count; a shard or
  budget change requires a design revision.
- Iteration 04 allocation (design 04, CI plan; planning allocations from the
  coordinator-supplied latest hosted baseline, not measurements): whole
  workflow 255 s, macOS packages worker 243 s, macOS functions worker
  206 s. These supersede the iteration 03 hosted entry that was still
  pending below. The packages shard may grow by at most 25 s on each
  platform (about 268 s per packages job), the functions shard by at most
  5 s of compile overhead with no new repeated cases (about 211 s on
  macOS), processgroup and the summaries by nothing; each main job by at
  most 20 s (normal role functions, Linux benchmarks), for a critical-path
  target of about 280 s (4 min 40 s) at comparable setup and queue
  conditions. The new `internal/adapter` binary and the grown sidecar
  binary must stay below the plane binary, the packages critical path. If
  hosted measurements miss these allocations, redundant builds and setup
  are removed first; counts, cases, shards and budgets never change without
  a design revision. The normal function package keeps its shared
  `-timeout=180s` for all `tests/function` cases together.
- Iteration 05 allocation (design 05, CI plan; planning allocations from the
  supplied 264 s workflow baseline, not measurements or pass thresholds):
  each main job may grow by at most 20 s; the packages shard by at most
  25 s on each platform (Linux 235 s to about 260 s, macOS 253 s to about
  278 s); the processgroup shard by no execution, only up to 5 s of shared
  compile overhead; the functions shard by no repeated wrapper, up to 5 s of
  compile overhead (macOS 228 s to about 233 s); the summaries by nothing.
  That is at most 25 s of critical-path growth, about 289 s (4 min 49 s).
  The shards, counts, CPU settings, 6-minute binary limits, 15-minute
  watchdogs and 20-minute worker limits are unchanged, and no case is
  reduced or skipped. Repeated tests keep full-tail checkpoints mostly
  injected or in memory with only small real-disk cases; the maximum-size
  disk write is benchmarked once in the main job. If measurements exceed
  the allocation, duplicate fixture setup and builds are removed first; a
  new shard or budget requires a design revision.
- Iteration 05b allocation (design 05b, CI plan; explicit planning
  estimates, not measured speedups or hard upper bounds). Local plane at
  about 270–280 s over three CPU settings gives about 90–93 s per
  invocation, assuming roughly balanced settings; allowing 1.3–1.5× hosted
  per-setting cost (117–140 s) and contention multipliers of 1.2–1.6 on the
  four-core Linux runner and 1.3–1.8 on the three-core macOS runner gives
  about 140–225 s per Linux and 150–255 s per macOS plane invocation, for
  each of CPU 1, 2 and 4. The upper estimates leave 135 s / 105 s below the
  6-minute limit (38% / 29% margin). Neither the CPU-setting balance nor an
  isolated hosted cost can be recovered from a timed-out aggregate, dividing
  the hosted alarm by three would be unsound, and a threefold contention
  slowdown could erase the benefit. The per-binary 6-minute limit,
  per-stage 15-minute watchdog and 20-minute worker budget are unchanged.
  The remaining packages worker is expected to set the critical path at
  about 300–380 s plus summary scheduling (5–6.3 minutes), about five
  minutes if isolation brings sidecar near 260 s and setup near 35 s;
  current evidence does not support promising 300 s or less, and queueing,
  cache misses or hosted contention can add more.
- Iteration 05b sidecar follow-up allocation (revised planning estimates,
  not measurements or pass thresholds, from hosted run 36325089074 and the
  local measurements below). Locally the three concurrent sidecar
  invocations take 50.7–71.8 s, CPU 1 the slowest. In run 36325089074 the
  hosted plane invocations ran 1.14–1.22× their local times on Linux and
  1.47–1.67× on macOS, and the contended hosted sidecar binary 1.76×
  (Linux) and 1.42× (macOS) its local time; allowing 1.2–2.0× gives about
  60–145 s per sidecar invocation on either platform, well below the
  300.0 s mark and the 6-minute limit, and about 100–195 s per sidecar
  worker job with 20–50 s of setup and build. The packages command without
  sidecar takes 100.3 s locally (slowest binary `internal/contract`
  99.8 s); hosted, its slowest remaining binaries measured 122–127 s on
  Linux and 184 s on macOS while sidecar still shared the command, which
  gives about 175–230 s (Linux) and 215–285 s (macOS) per packages job,
  including the build and setup seen in that run. The critical path moves
  to the macOS workers, functions (measured 275 s), plane (249 s) and
  packages: about 270–320 s plus summary scheduling (4.5–5.4 minutes),
  against the 402 s measured. The concurrent binaries still share the
  runner's cores, and sidecar's real children and subprocess waits do not
  shrink with the CPU setting, so no threefold speedup is assumed. The
  per-binary 6-minute limit, per-stage 15-minute watchdog and 20-minute
  worker budget are unchanged.
- Iteration 06a-perf allocation (design 06a-perf): planning estimates of
  command time, not job upper bounds and not measurements. Until the first
  remote run replaces them, each new or changed worker's reference is its
  unchanged work at the hosted reference times above, assuming no
  quantified isolation benefit; setup, compilation differences and queue
  time are separate. Each pair worker is estimated by the larger of the
  historical CPU 2 and CPU 4 invocation times, not their sum; actual pairs
  may improve when CPU 1 is absent.

  | Worker suffix | Linux command-time reference | macOS command-time reference |
  |---|---:|---:|
  | `stress-plane-cpu1` | about 224 s | about 322 s; desired ≤250 s |
  | `stress-plane` (CPU 2 and 4) | about 174 s | about 229 s |
  | `stress-sidecar-cpu1` | about 239 s | about 327 s; desired ≤250 s |
  | `stress-sidecar` (CPU 2 and 4) | about 189 s | about 240 s |

  The ≤250 s macOS target needs CPU 1 improvements of 22.46% (plane) and
  23.57% (sidecar); the ≤300.0 s completion threshold only 6.95% and 8.29%.
  Removing sibling binaries is a plausible remedy on the three-core macOS
  runner, but no supplied data proves either speedup. Do not divide time by
  three, subtract sampled race CPU, or claim a numerical isolated-worker
  speedup. The ten unchanged jobs (the two main jobs, the packages,
  processgroup and functions workers of both platforms, and the two
  summaries) keep their documented estimates and every timeout. A summary
  completes after its slowest dependency plus queue and dispatch time; its
  own shell work is negligible. The split adds four setups and may increase
  total runner minutes even if the critical path shortens. The per-binary
  6-minute limit, per-stage 15-minute watchdog and 20-minute worker budget
  are unchanged.

Measurements, newest first. Hosted and local figures come from different
machines and are never combined into one number.

- Measured with iteration 06a-perf (CPU 1 shards): Linux, go1.26.4
  linux/amd64 on the same 16-thread Intel i7-11800H developer workstation
  (kernel 6.8), warm build cache, 2026-09-28, each of the seven shard
  stages run alone on the host, one after another; per-command times are
  the `devcheck` outcome lines, binary times the `go test` package
  summaries. This validates execution only: local timing is not a hosted
  macOS qualification, and this 16-thread host is not a hosted estimate.
  - `devcheck stress-plane-cpu1` 135.3 s: `stress plane cpu1` 135.3 s
    (binary 134.956 s), alone, against 167.493 s for the same invocation
    running concurrently with CPU 2 and 4 (design 06a-perf evidence above).
  - `devcheck stress-plane` 116.4 s: `stress plane cpu2` 116.3 s
    (115.970 s) and `stress plane cpu4` 97.2 s (96.840 s), concurrently.
  - `devcheck stress-sidecar-cpu1` 146.5 s: `stress sidecar cpu1` 146.5 s
    (binary 146.119 s), alone.
  - `devcheck stress-sidecar` 99.7 s: `stress sidecar cpu2` 99.6 s
    (99.273 s) and `stress sidecar cpu4` 83.0 s (82.664 s), concurrently.
  - Unchanged plans: `devcheck stress-packages` 115.9 s (`stress packages`
    115.8 s, slowest binary `internal/contract` 115.231 s),
    `devcheck stress-processgroup` 71.6 s (`cpu1`, `cpu2` and `cpu4`
    71.3–71.6 s, concurrently) and `devcheck stress-functions` 111.2 s
    (`stress function` 31.1 s, `stress plane function` 72.0 s,
    `stress node function` 8.1 s).
  - Hosted iteration 06a-perf times: pending (the first remote run of pull
    request #6; see First remote run).

- Expected per-job wall-clock after iteration 06a-perf: planning estimates,
  not measurements (see the iteration 06a-perf allocation in Budgets). The
  four new or changed workers take the command-time references, with setup,
  build and queue time on top; jobs whose work is unchanged keep their
  estimates after the iteration 05b sidecar follow-up (below):
  - `ci-linux` about 165 s and `ci-macos` about 95 s (unchanged work).
  - `ci-linux-stress-packages` about 175–230 s and
    `ci-macos-stress-packages` about 215–285 s (unchanged work).
  - `ci-linux-stress-plane-cpu1` about 224 s and
    `ci-macos-stress-plane-cpu1` about 322 s of command time (desired
    ≤250 s on macOS), plus setup and build.
  - `ci-linux-stress-plane` about 174 s and `ci-macos-stress-plane` about
    229 s of command time for the CPU 2 and CPU 4 pair, plus setup and
    build.
  - `ci-linux-stress-sidecar-cpu1` about 239 s and
    `ci-macos-stress-sidecar-cpu1` about 327 s of command time (desired
    ≤250 s on macOS), plus setup and build.
  - `ci-linux-stress-sidecar` about 189 s and `ci-macos-stress-sidecar`
    about 240 s of command time for the pair, plus setup and build.
  - `ci-linux-stress-processgroup` about 95 s,
    `ci-macos-stress-processgroup` about 110 s,
    `ci-linux-stress-functions` about 160 s and `ci-macos-stress-functions`
    about 275 s (unchanged work).
  - `ci-linux-stress` about 3 s and `ci-macos-stress` about 3 s of
    execution after the slowest worker of their platform, plus scheduling.
  - Overall critical path: expected on the macOS CPU 1 workers, about
    322–327 s of command time plus setup and summary scheduling, unless
    isolation brings them toward the 250 s target; the first remote run
    measures it (see First remote run). Report trigger-to-completion time
    separately from job runtime.

- Measured with the iteration 05b sidecar follow-up (sidecar shard): Linux,
  go1.26.4 linux/amd64 on the same 16-thread Intel i7-11800H developer
  workstation (kernel 6.8), warm build cache, 2026-09-27, each stage run
  alone on the host, one after another; times as for iteration 05b below.
  - `devcheck stress-sidecar` 71.9 s: `stress sidecar cpu1` 71.8 s
    (binary 71.3 s), `stress sidecar cpu2` 58.1 s (57.7 s) and
    `stress sidecar cpu4` 50.7 s (50.3 s), all three concurrently, against
    174.5 s for the single `-cpu=1,2,4` sidecar binary inside
    `stress packages` with iteration 05b on the same host. CPU 1 is again
    the slowest, about 19% above the 60.2 s mean. This host has 16
    hardware threads: it is not a hosted estimate.
  - `devcheck stress-packages` 100.3 s: `stress packages` 100.3 s, slowest
    binary `internal/contract` 99.8 s, `internal/testkit/fakeadapter`
    78.1 s, `internal/client` 65.1 s, `internal/testkit` 37.9 s,
    `internal/spikes/gittransport` 28.4 s and `internal/adapter` 15.6 s,
    against 177.5 s with sidecar in the same invocation.
  - `devcheck stress`, the five shards in one process under one shared
    watchdog: 468.3 s (7 min 48 s), with plane's three invocations at
    117.9 s, 102.5 s and 93.9 s and sidecar's at 72.2 s, 56.9 s and 51.8 s;
    hosted, the shards run on separate workers, so the critical path is the
    slowest worker, not this sum.
  - Hosted sidecar follow-up times: pending (the next remote run of pull
    request #5, with the sidecar workers; see First remote run).

- Hosted run 36325089074 (pull request #5, iteration 05b at 306b392,
  measured, all twelve jobs green with no failure events; the first hosted
  run after the plane split): workflow 14:12:42 to 14:19:24 UTC on
  2026-09-27, 6 min 42 s from trigger to completion. Runners:
  `ubuntu-24.04` image 20260920.314.1 (x64) and `macos-15-arm64` image
  20260907.0337.1 (macOS 15.7.9, arm64), with the setup-go module and build
  cache restored on every worker; the core count is not printed in the
  job logs, so the four-vCPU and three-core assumptions stay unverified.
  - Plane workers (`devcheck stress-plane`, three concurrent invocations):
    Linux `devcheck: stress plane cpu1: ok in 146.6s` (binary 141.237 s),
    cpu2 123.0 s (117.654 s) and cpu4 114.0 s (108.489 s), job 169 s with
    16 s of setup before the stage; macOS
    `devcheck: stress plane cpu1: ok in 202.8s` (binary 195.425 s), cpu2
    158.2 s (150.869 s) and cpu4 150.9 s (143.553 s), job 249 s with 38 s
    of setup. The go command's build adds 5–7 s to each invocation. CPU 1
    is the slowest on both platforms, as locally. All invocations are
    within or below the 140–225 s (Linux) and 150–255 s (macOS) estimates,
    and the plane fallback did not trigger.
  - Packages workers (`devcheck stress-packages`, still with sidecar):
    Linux `devcheck: stress packages: ok in 366.5s`, binaries
    `internal/sidecar` 307.788 s, `internal/testkit/fakeadapter`
    126.925 s, `internal/contract` 122.138 s, `internal/client` 83.217 s,
    `internal/spikes/gittransport` 64.067 s, `internal/testkit` 53.253 s
    and `internal/adapter` 30.861 s, job 394 s with 24 s of setup; macOS
    `317.9s`, binaries `internal/sidecar` 247.885 s, `internal/contract`
    184.029 s, `internal/testkit/fakeadapter` 89.859 s, `internal/client`
    89.033 s, `internal/testkit` 63.256 s, `internal/spikes/gittransport`
    53.261 s and `internal/adapter` 27.861 s, job 354 s with 28 s of setup.
    The Linux job is above its 240–330 s estimate: the sidecar binary was
    at the top of its 260–310 s estimate and the command ran 59 s beyond
    it (70 s on macOS), consistent with the build and with sidecar waiting
    for a free `-p` slot (four on Linux, three on macOS) behind the first
    packages of the command.
  - Other jobs: `ci-linux` 164 s, `ci-macos` 96 s, processgroup 95 s
    (Linux) and 110 s (macOS), functions 162 s (Linux) and 275 s (macOS).
    Each summary started 2–3 s after the last worker of its platform ended
    and ran 3 s. The critical path was `ci-linux-stress-packages` (394 s)
    and then `ci-linux-stress`: 402 s, above the 300–380 s estimate because
    of the packages worker.
  - Trigger decisions: plane fallback not triggered (146.6 s and 202.8 s at
    most, no timeout); sidecar follow-up triggered on Linux (307.788 s over
    300.0 s) and applied on both platforms (see Stress checks above).

- Measured with iteration 05b (plane shard): Linux, go1.26.4 linux/amd64
  on the same 16-thread Intel i7-11800H developer workstation (kernel 6.8),
  warm build cache, 2026-09-27, each stage run alone on the host, one after
  another. Elapsed times are wall-clock including `go run` of the driver;
  per-command times are the `devcheck` outcome lines, binary times the
  `go test` package summaries.
  - `devcheck stress-plane` 120.3 s: `stress plane cpu1` 120.1 s
    (binary 116.7 s), `stress plane cpu2` 106.2 s (102.8 s) and
    `stress plane cpu4` 97.3 s (93.9 s), all three concurrently, against
    270.4 s for the single `-cpu=1,2,4` plane binary measured with
    iteration 05 on the same host. This host has 16 hardware threads, not a
    runner's three or four cores: it is not a hosted estimate. The three
    settings are not evenly balanced: CPU 1 is the slowest, about 11% above
    their 107.9 s mean, whereas the hosted planning model assumed rough
    balance, so CPU 1 is the invocation to watch against the fallback's
    300.0 s trigger.
  - `devcheck stress-packages` 177.5 s: `stress packages` 177.5 s,
    slowest binary `internal/sidecar` 174.5 s (179.8 s alongside plane
    with iteration 05: on this 16-thread host plane was no material
    competitor), `internal/contract` 108.7 s,
    `internal/testkit/fakeadapter` 80.6 s, `internal/client` 68.9 s,
    `internal/testkit` 40.8 s, `internal/spikes/gittransport` 30.5 s and
    `internal/adapter` 17.0 s, against 273.5 s for the stage with plane in
    the same invocation.
  - `devcheck stress-processgroup` 70.6 s: `stress processgroup cpu1`,
    `cpu2` and `cpu4` 70.6 s each, concurrently (unchanged plan).
  - `devcheck stress-functions` 104.4 s: `stress function` 30.6 s,
    `stress plane function` 66.1 s and `stress node function` 7.6 s
    (unchanged plan).
  - `devcheck stress`, the four shards in one process under one shared
    watchdog: 465.7 s (7 min 46 s), with plane's three invocations at
    120.4 s, 102.2 s and 94.8 s; hosted, the shards run on separate
    workers, so the critical path is the slowest worker, not this sum.
  - Main-job work (`devcheck all`: test, coverage, bench, cross) 89.2 s
    (unchanged work; coverage total 93.5%).
  - Hosted iteration 05b times: measured by run 36325089074 (above).

- Hosted run 36315881191 (pull request #5, iteration 05 at c4ab9e4,
  measured, failed): `devcheck stress-packages` failed on both platforms
  because `internal/plane` reached its 6-minute timeout
  (`FAIL internal/plane 360.096s` Linux, `360.066s` macOS; censored and
  contended, see above). The packages jobs failed after 395.8 s (Linux) and
  432.6 s (macOS); the job-minus-plane residuals of 36 s and 73 s are setup
  and build overhead. Other macOS binaries in that command: sidecar 310 s,
  contract 236 s, client 89 s, fakeadapter 85 s, testkit 61 s,
  gittransport 48 s and adapter 39 s (all contended). The processgroup and
  functions workers passed within 153–218 s (per-platform assignments not
  supplied). Earlier hosted packages jobs (pull request #4) took 235 s
  (Linux) and 253 s (macOS).

- Expected per-job wall-clock after iteration 05b: planning estimates, not
  measurements, with setup and build included and queue time excluded (see
  the iteration 05b allocation in Budgets):
  - `ci-linux` about 100–240 s and `ci-macos` about 60–180 s (unchanged
    work, low-confidence planning ranges).
  - `ci-linux-stress-packages` about 240–330 s and
    `ci-macos-stress-packages` about 295–380 s: sidecar is now the slowest
    binary, estimated at 260–310 s.
  - `ci-linux-stress-plane` about 170–285 s and `ci-macos-stress-plane`
    about 180–315 s: the slowest of the three concurrent invocations plus
    about 30–60 s of overhead.
  - `ci-linux-stress-processgroup`, `ci-linux-stress-functions`,
    `ci-macos-stress-processgroup` and `ci-macos-stress-functions` about
    153–218 s each (the supplied worker envelope).
  - `ci-linux-stress` and `ci-macos-stress` about 1–5 s of execution after
    the slowest worker of their platform, plus scheduling.
  - Overall critical path: the packages workers, about 300–380 s plus
    summary scheduling; report trigger-to-completion time separately from
    job runtime.
  - Compared with run 36325089074 (above): the plane, processgroup and
    functions workers and the macOS packages worker were within or below
    these ranges; `ci-linux-stress-packages` (394 s) and the critical path
    (402 s) were above them, as explained there, and the sidecar follow-up
    supersedes these estimates (below).

- Expected per-job wall-clock after the iteration 05b sidecar follow-up:
  planning estimates, not measurements, with setup and build included and
  queue time excluded (see the sidecar follow-up allocation in Budgets);
  jobs whose work is unchanged take their run 36325089074 times:
  - `ci-linux` about 165 s and `ci-macos` about 95 s (unchanged work,
    measured 164 s and 96 s).
  - `ci-linux-stress-packages` about 175–230 s and
    `ci-macos-stress-packages` about 215–285 s, now bounded by
    `internal/testkit/fakeadapter` and `internal/contract` (Linux) and
    `internal/contract` (macOS).
  - `ci-linux-stress-plane` about 170 s and `ci-macos-stress-plane` about
    250 s (unchanged work, measured 169 s and 249 s).
  - `ci-linux-stress-sidecar` about 100–195 s and
    `ci-macos-stress-sidecar` about 100–195 s: the slowest of the three
    concurrent sidecar invocations plus 20–50 s of setup and build.
  - `ci-linux-stress-processgroup` about 95 s,
    `ci-macos-stress-processgroup` about 110 s,
    `ci-linux-stress-functions` about 160 s and `ci-macos-stress-functions`
    about 275 s (unchanged work, measured).
  - `ci-linux-stress` about 3 s and `ci-macos-stress` about 3 s of
    execution after the slowest worker of their platform, plus scheduling.
  - Overall critical path: the macOS workers, about 270–320 s plus summary
    scheduling, against 402 s measured before the follow-up; report
    trigger-to-completion time separately from job runtime.

- Measured with iteration 05 (tasks): Linux, go1.26.4 linux/amd64 on the
  same 16-thread Intel i7-11800H developer workstation (kernel 6.8), warm
  build cache, 2026-09-27, each shard stage run alone on the host, paired
  with the iteration 04 revision (04d6f7e) measured the same day on the
  same host as the before figure:
  - `devcheck stress-packages` 273.5 s against 221.9 s before (+51.6 s,
    allocation 25 s: over the allocation; earlier runs of the same tree
    266.7 s and 287.5 s against 223.0 s and 225.5 s): slowest binary
    `internal/plane` 270.4 s (before 221.3 s), `internal/sidecar` 179.8 s
    (114.6 s), `internal/contract` 110.5 s (44.8 s; the maximum-size task
    envelope, view and 10 MiB log response cases under the race
    detector), `internal/testkit/fakeadapter` 80.7 s (54.0 s),
    `internal/client` 69.1 s (51.2 s), `internal/testkit` 42.0 s,
    `internal/spikes/gittransport` 31.8 s and `internal/adapter` 17.0 s
    (10.7 s), all below the plane binary and its 6-minute limit. Run alone
    at count 2, the plane binary grows from 21.5 s to 25.9 s (+1.5 s per
    count at `-cpu=1`, +0.9 s at 2 and +0.3 s at 4): the new task
    contracts are CPU-bound under the race detector at one CPU, about
    12 ms of it per TLS plane-and-worker fixture. Duplicate fixtures were
    removed first (the plane's plain and forced recovery-only removal runs
    once, with its reload, in `TestTaskInterruption`; maximum-size cases
    run once per bound); no case, count or CPU setting was reduced.
  - `devcheck stress-functions` 105.1 s against 103.4 s before (+1.8 s,
    allocation 5 s): no new repeated case.
  - `devcheck stress-processgroup` 71.3 s against 69.5 s before (+1.7 s,
    allocation 5 s): unchanged plan.
  - Main-job work (`devcheck all`: test, coverage, bench, cross) 87.8 s
    against 90.8 s before (within run-to-run variation; allocation 20 s).
  - Hosted iteration 05 times: pending (first remote run).

- Measured with iteration 04 (roles): Linux, go1.26.4 linux/amd64 on the
  same 16-thread Intel i7-11800H developer workstation (kernel 6.8), warm
  build cache, 2026-09-26, each shard stage run alone on the host, with
  the iteration 03 revision measured the same day on the same host as the
  before figure:
  - `devcheck stress-packages` 220.0 s against 202.8 s before (+17.2 s,
    allocation 25 s; an earlier run 219.3 s): slowest binary
    `internal/plane` 219.4 s (before 201.5 s), `internal/sidecar` 118.2 s
    (98.6 s), `internal/testkit/fakeadapter` 54.3 s (53.2 s),
    `internal/contract` 45.7 s (1.5 s; the exact 1 MiB and 2 MiB boundary
    cases under the race detector), `internal/testkit` 39.6 s,
    `internal/client` 35.1 s, `internal/spikes/gittransport` 31.0 s and
    the new `internal/adapter` 11.3 s, all below the plane binary. The plane role contracts run as
    parallel top-level tests after the serial ones, so they add their
    whole CPU time at `-cpu=1`; `TestServerLimits`, whose plane shutdown
    waits out net/http's half-second close delay after its 431 response,
    now runs in parallel with them (same case and count), which kept the
    plane binary inside the allocation (238.0 s before that change).
  - `devcheck stress-functions` 101.6 s (iteration 03: 101.8 s; allocation
    5 s): `stress function` 29.3 s, `stress plane function` 64.9 s,
    `stress node function` 7.4 s.
  - `devcheck stress-processgroup` 69.6 s (unchanged plan; each CPU setting
    69.5 to 69.6 s, concurrently).
  - Main-job work (`devcheck all`: test, coverage, bench, cross) 78.4 s
    against 70.4 s before (+8.0 s, allocation 20 s). The `tests/function`
    package takes 24.3 s ordinary and 27.5 s under the race detector
    (before 20.4 s and 19.9 s), against its shared 180 s timeout. Three
    role function tests (`TestRoleAdapter`, `TestRolePersistence`,
    `TestRoleCommands`) each wait about 5 s for a real sidecar's next
    heartbeat to report readiness; they run in parallel with each other
    (38.3 s and 38.0 s for the package when they ran one after another).
  - Hosted iteration 04 times: pending (first remote run).

- Measured with iteration 03 (nodes): Linux, go1.26.4 linux/amd64 on the
  same 16-thread Intel i7-11800H developer workstation (kernel 6.8), warm
  build cache, 2026-09-26, each shard stage run alone on the host:
  - `devcheck stress-packages` 201.2 s: `stress packages` 201.2 s, slowest
    binary `internal/plane` 200.6 s (`internal/sidecar` 99.1 s,
    `internal/testkit/fakeadapter` 53.2 s, `internal/testkit` 34.6 s,
    `internal/client` 27.3 s, `internal/spikes/gittransport` 26.5 s,
    `internal/contract` 1.6 s). That is 17 s below 02c's 218.4 s although
    the plane binary gained the node registry, lease and stream tests
    (about 0.46 s per repetition, 28 s for 60): the node tests copy one
    initialized template state instead of generating keys per test, and
    the race-built lock-holder helper processes of the plane and sidecar
    `flock` subtests now exit without the race runtime's default
    one-second exit sleep (`GORACE=atexit_sleep_ms=0`), which had cost the
    plane binary about 60 s per shard. Every case and count is unchanged.
    The new `internal/sidecar`, `internal/client` and `internal/contract`
    binaries run concurrently in the same invocation, below the plane
    binary.
  - `devcheck stress-functions` 101.8 s: `stress function` 30.4 s (binary
    28.9 s), `stress plane function` 64.1 s (binary 63.7 s) and
    `stress node function` 7.4 s (binary 7.0 s; design estimate 15 s on a
    hosted Linux worker).
  - Hosted iteration 03 worker times: superseded by the coordinator-supplied
    latest hosted baseline recorded in Budgets (iteration 04 allocation):
    total 255 s, macOS packages 243 s, macOS functions 206 s.

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

Iteration 04 adds no host OS read and no exemption: the guard's five
wrappers and four exempt files are unchanged (checked by
`TestRolePolicy/policy` in `internal/devcheck`). The worker's manual checks
use the shared POSIX `syscall.O_NONBLOCK` open and descriptor `fstat` on
both systems; the adapter probe runs the explicit absolute executable path
(no `PATH` lookup) with the same code on both. `internal/adapter` imports
no plane, sidecar, devcheck or testkit package, and no production file
imports the fake fixture `internal/testkit/fakeadapter`: the product knows
only the probe's argument and output, and filters the fixture's two
file-descriptor variables by their literal names (both guarded by the same
policy test).

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

The sidecar (iteration 03) mirrors this exactly: it resolves its state
directory in the pure `sidecar.ResolveStateDir(goos, ...)`, fed by the same
`cli.Run` wrapper, and keeps its advisory state lock and directory-sync
fallback in the build-selected files `internal/sidecar/lock_unix.go`
(`linux || darwin`) and `internal/sidecar/lock_other.go`
(`!linux && !darwin`). Like the plane's, they need no exemption and the
guard's exception list is unchanged. The node client and sidecar never
listen; the only production TLS configuration that replaces Go's verifier
is the pinned bootstrap's mandatory `VerifyConnection` (guarded by
`TestInsecureSkipVerifyGuard` in `internal/client`), and the test-only
variable `CALLSHEET_TEST_CLI_BINARY` is read only in `_test.go` files
(guarded by `TestNodeVerificationPolicyContract` `test-only-env` in
`internal/devcheck`).

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
concurrent processgroup, plane and sidecar plans, in the next macOS worker runs). No Windows or
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
  fourteen worker contexts are not required (six since iteration 02c, the
  two plane workers since iteration 05b, the two sidecar workers since its
  sidecar follow-up and the four CPU1 workers since iteration 06a-perf; the
  summaries gate on them), so no protection change is needed for 02c, 05b,
  its sidecar follow-up or 06a-perf.
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

### Plane and sidecar workers (pull request #5)

Iteration 05b adds the workers `ci-linux-stress-plane` and
`ci-macos-stress-plane`, and its sidecar follow-up the workers
`ci-linux-stress-sidecar` and `ci-macos-stress-sidecar`; both summaries
now take five results. The required contexts stay exactly `ci-linux`,
`ci-macos`, `ci-linux-stress` and `ci-macos-stress`, so no protection
change is made:

1. Finish the flow's code review of iterations 05 and 05b, including its sidecar follow-up (`REVIEW_APPROVED`), and commit the reviewed code on `iter-05-dispatch`.
2. Push `iter-05-dispatch`, updating pull request #5.
3. Observe all fourteen jobs report success on the current PR merge revision: the ten stress workers and all four required checks.
4. The owner verifies protection with the read-only command above: the same four required contexts, and no plane or sidecar worker added as a required context.
5. Merge iterations 05 and 05b together, only with all four checks green on the current merge revision.

No skipped, canceled, pending or unobserved result qualifies, and a later
push requires fresh current-revision evidence (return to step 3).

### CPU1 workers (pull request #6)

Iteration 06a-perf adds the workers `ci-linux-stress-plane-cpu1`,
`ci-macos-stress-plane-cpu1`, `ci-linux-stress-sidecar-cpu1` and
`ci-macos-stress-sidecar-cpu1`; both summaries now take seven results. The
new workers are diagnostic checks, not required contexts. The required
contexts stay exactly `ci-linux`, `ci-macos`, `ci-linux-stress` and
`ci-macos-stress`, so no protection change is made:

1. Finish the flow's code review of iteration 06a-perf, with iteration 06a (`REVIEW_APPROVED`), and commit the reviewed code on `iter-06-task-control`.
2. Push `iter-06-task-control`, updating pull request #6.
3. Observe all eighteen jobs report success on the current PR merge revision: the fourteen stress workers and all four required checks.
4. The owner verifies protection with the read-only command above: the same four required contexts, and no CPU1 worker added as a required context.
5. Merge iterations 06a and 06a-perf together, only with all four checks green on the current merge revision.

No skipped, canceled, pending or unobserved result qualifies, and a later
push requires fresh current-revision evidence (return to step 3).

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

Iteration 05b (the plane stress shard) is delivered with iteration 05 in
pull request #5 (branch `iter-05-dispatch`), together with its sidecar
follow-up (the sidecar stress shard). They add four worker jobs and change
no required context, so they need no protection change; the pull request
merges only after all fourteen jobs, and so all four checks, are green on
its current merge revision (Branch protection, Plane and sidecar workers).

Iteration 06a-perf (the plane and sidecar CPU1 stress workers) is delivered
with iteration 06a in pull request #6 (branch `iter-06-task-control`). It
adds four worker jobs and changes no required context, so it needs no
protection change; the pull request merges only after all eighteen jobs,
and so all four checks, are green on its current merge revision (Branch
protection, CPU1 workers).

## First remote run

Local checks cannot prove runner provisioning, Go and action download or cache
behavior on GitHub, Darwin runtime results or repository protection. Those
remain pending until observed. After pushing, the owner records in the flow
handoff:

- the run URL and the commit it ran;
- the conclusions of all eighteen jobs: all four checks, `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`, and the fourteen stress workers;
- native evidence from the `ci-macos` log: the line `devcheck: native qualification passed on darwin/<arch>` naming `TestFP6ProcessGroups` and its three scenarios;
- stress evidence from the fourteen worker logs (the summaries hold none): the lines `devcheck: stage stress-packages ok`, `devcheck: stage stress-plane-cpu1 ok`, `devcheck: stage stress-plane ok`, `devcheck: stage stress-sidecar-cpu1 ok`, `devcheck: stage stress-sidecar ok`, `devcheck: stage stress-processgroup ok` and `devcheck: stage stress-functions ok` on each platform, the `-count=20` commands, every CPU invocation's outcome, and the elapsed time of each stress command with the runner's OS, architecture and cache state;
- the actual job and step times of all eighteen jobs, setup, queue and summary wait time included, and the overall workflow critical path; compare each worker with the expected per-job wall-clock in Stress checks, and diagnose any miss of the 4–5 minute goal and the remaining bottleneck without weakening tests or reducing counts;
- the branch protection verification described above (finishing the conditional 02b prerequisite first if it is needed);
- for iteration 03, native evidence for the 30 node names (see Checks) on `ci-macos`, the `stress node function` step and the enlarged `stress packages` step on both platforms with their times, compared with the iteration 03 allocation in Budgets;
- for iteration 04, native evidence for the 29 role names (see Checks) on `ci-macos`, the `stress packages` step with `internal/adapter` and the `internal/plane`, `internal/sidecar` and `internal/adapter` binary times on both platforms, the function package time on both platforms, and the before/after job and command durations, compared with the iteration 04 allocation in Budgets. Hosted evidence pending at local review remains pending, not passed;
- for iteration 05b, from both plane workers, each `devcheck: stress plane cpuN: ok in Xs` (or `FAILED after Xs`) line and each plane binary's own time separately from its go command's build, evaluated against the pre-authorised plane fallback trigger (X > 300.0 s, or a plane binary timeout in the CPU-labelled replay); from both packages workers, the `internal/sidecar` and `internal/contract` binary times, the sidecar line evaluated against the sidecar follow-up rule; setup, summary wait, the runner's core count and architecture, and the critical path, compared with the iteration 05b estimates in Budgets. Collect one complete successful run per platform; failed runs stay in the evidence, never discarded as retries. Values above the estimated ranges need a documented explanation or revised estimate, and any timeout or assertion failure blocks qualification.
- for the iteration 05b sidecar follow-up, from both sidecar workers, each `devcheck: stress sidecar cpuN: ok in Xs` (or `FAILED after Xs`) line and each sidecar binary's own time separately from its go command's build, with every process, cleanup and timing assertion passing unchanged; from both packages workers, the remaining binaries' times; setup, summary wait, the runner's core count and architecture, and the critical path, compared with the sidecar follow-up estimates in Budgets. Any timeout or assertion failure blocks qualification, and no further concurrency escalation or timing relaxation is authorised.
- for iteration 06a-perf, the owner's pre-decided post-push qualification (not the local code-review bar): record all eighteen job conclusions and step and job times, setup and queue costs, the runner's architecture and core count, and every plane and sidecar CPU-labelled `devcheck` outcome. Use `devcheck: stress <package> cpu1: ok in Xs` as each CPU 1 invocation's elapsed value (it includes the command's build), and keep the binary's own time separately. Evaluate all four CPU1 invocations, Linux and macOS, plane and sidecar. If every CPU1 invocation passes and is ≤300.0 seconds, and all ordinary correctness, coverage and CI gates pass, this slice is done. A value above 250 but at or below 300 succeeds under the owner's rule; record that the aspirational target was missed. Otherwise report the measured values and failures to the owner. Missing, cancelled or timed-out invocations do not count as passes. In that case make no automatic further change to topology, waves, flags, counts, timeouts, workload tests or fixtures. Do not discard a failed first run by retrying until green. The ≤250-second macOS CPU1 goal is a first-run hypothesis, not an additional acceptance gate. There is no two-run requirement. All CPU 2 and CPU 4 invocations and every other job must still pass their unchanged gates, and until that run exists hosted qualification is pending, not passed.

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
go test -race -count=20 -cpu=1,2,4 -timeout=6m -run '^TestStressConcurrencyContract$' ./internal/devcheck
go test -json -count=1 -run '^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$' ./tests/function
go test -json -count=1 -run '^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$' ./tests/function
go test -count=1 -run '^TestRole' -v ./tests/function
go test -count=1 -run '^TestTask' -v ./tests/function
go test -race -covermode=atomic -count=20 -cpu=1,2,4 -timeout=6m -run '^TestTask' ./internal/plane ./internal/sidecar ./internal/adapter ./internal/client ./internal/contract
```

A single shard, as one CI worker runs it, is also available on its own:

```
go run ./cmd/devcheck stress-packages
go run ./cmd/devcheck stress-plane-cpu1
go run ./cmd/devcheck stress-plane
go run ./cmd/devcheck stress-sidecar-cpu1
go run ./cmd/devcheck stress-sidecar
go run ./cmd/devcheck stress-processgroup
go run ./cmd/devcheck stress-functions
```

Measure a shard without other stress work running on the same host, which
matches a hosted worker's isolation. `devcheck stress-plane` and
`devcheck stress-sidecar` run CPU 2 and CPU 4 only; add their `-cpu1` stages
for CPU 1. The `TestStressConcurrencyContract`
command repeats the concurrent coordinator's timing-dependent contract at
the declared count, for all five concurrent shards and the wave mechanism
on the processgroup shard; coder and reviewer run it on both native platforms, and it
is not part of any stress stage or of CI. The last
command of the first block is the focused evidence for the plane stress
selector: its events must show run and pass for exactly the eight selected
subtests and none for a `contracts` subtest; the next one is the same
evidence for the node selector, exactly `locking` and `shutdown` and none
of the delegated `restart` or `disconnect` wrappers.
The last command of the first block (iteration 05) repeats the new task
contracts under coverage at the declared count, as the design's review
command; it adds no hosted shard.

`all` runs test (with race on Linux), coverage, bench and cross; `stress` is
explicit (see Stress checks), and release and review verification run both.
`internal/cicheck` and the `TestCI*`, `TestHardening*` and
`TestStressShard*` function tests validate the workflow structure, its
devcheck stages against the driver's dispatch, the summaries' predicate,
and this page, offline. `go run ./cmd/devcheck native` works on macOS only; on other hosts it
exits 1 with an unsupported-stage error. Optionally, a locally installed
`actionlint .github/workflows/ci.yml` can lint the workflow; it is not a
required dependency and nothing invokes it automatically.
