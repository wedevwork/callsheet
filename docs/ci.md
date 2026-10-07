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

Twenty fixed jobs run on every trigger. Exactly four of them are the
required status check contexts on `main`: `ci-linux`, `ci-macos`,
`ci-linux-stress` and `ci-macos-stress`. The other sixteen are the stress
workers (iteration 02c; the two plane workers since iteration 05b, the two
sidecar workers since its sidecar follow-up, the two plane CPU1 and the two
sidecar CPU1 workers since iteration 06a-perf, the two packages-cpu workers
since the stress worker rebalance of 2026-10-07), eight shards per
platform; their names are unique diagnostic checks, not required contexts:

| Check context | Runner | Timeout | Kind | Steps after setup |
|---|---|---|---|---|
| `ci-linux` | `ubuntu-24.04` | 45 min | required | `devcheck test` (native suite, then the same suite with `-race`, then iteration 08's `realadaptercheck`-tagged sidecar contract `TestRealAdapterLocal` and its `-race` counterpart), `devcheck coverage` (unit coverage must be greater than 80.0%; the profile run compiles the `realadaptercheck` tag), `devcheck bench` (git transport payload byte limits and commit/tree invariants, then the plane trust benchmarks: issuance, initialization and verified TLS health, the plane node benchmarks: heartbeat, snapshot of 100 nodes and durable enrollment, and the plane role benchmarks: role list and node views for 1 and 100 roles and durable add/set/rm transactions, then the node and role frame encode/decode benchmarks in `internal/contract`, then the sidecar ready-check benchmark (100 manual pairs, one shared probe) and the adapter's real fake-probe benchmark, then the task benchmarks: plane admission over 100 roles (first, last and no match, no filesystem), full-tail checkpoint writes of 0, 64 KiB and 10 MiB, the task envelope encode/decode at its maximum legal size in `internal/contract`, and the sidecar log-tail ring and maximum prompt composition, then the iteration 06a control benchmarks: the plane's per-task writer committing natural and lost terminal records and late evidence with 0, 64 KiB and 10 MiB tails (`BenchmarkControlCommit`: bytes written and bounded allocation; since iteration 06b also a stop intent's publication followed by its cancelled terminal record and a timed_out late append at the same tails), and one maximum sealed result, one maximum outbox and one 64-entry inventory page (`BenchmarkControlReplay` in `internal/contract` and `internal/sidecar`), then the iteration 06b bounded-wait benchmark (`BenchmarkControlWait` in `internal/plane`: registering and unregistering 1 and 16 IDs, waking 1 and 1,000 waiters; every waiter woken and no registration retained), then the iteration 07a MCP benchmarks in `internal/mcp` (`BenchmarkMCPCodec`: the discovery frame, the maximum 10 MiB log and the maximum 100-role list, bytes encoded and exact round trips; since iteration 10c also the maximum workspace task view and task workspace status (a result DTO of at most 32 KiB, metadata only) and the flat two-form argument validation of `ws_pull`, `ws_status` and `ws_diff`; `BenchmarkMCPRelay`: `node_show` end to end over a real local TLS plane; `BenchmarkMCPWaitBudget`: `task_wait` of 1 and 16 IDs with a fake clock and client, nothing retained), then the iteration 07b qualification-harness benchmarks in `internal/mcpqual` (`BenchmarkMCPQualificationTranscript`: each vendor decoder over its fixture at 1 KiB and at the 8 MiB per-file limit, exact event correlation, no model-prose timeout and an oversized transcript unqualified; `BenchmarkMCPQualificationProbe`: small-frame probe throughput on a fake clock with nonce and progress correlation and nothing retained), then the iteration 08 real-adapter benchmarks (`BenchmarkVendorFinal` and `BenchmarkVendorInvocation` in the `internal/adapter` step: both vendor extractors over the captured outputs, near-8-MiB and oversized inputs at chunk sizes 1, 4096 and 65536, and invocations with the smallest and the maximum legal prompt; since iteration 11 also Grok's success, error and cancelled captures, Cursor's result and absent output and synthetic near-8-MiB valid and oversized Grok and Cursor documents, and Grok invocations with a small and the 32 KiB prompt and Cursor's refusal; then the tagged step `BenchmarkRealAdapterFile` in `internal/sidecar`, `-bench=^BenchmarkRealAdapterFile$`: the final-file reader over a real 8 MiB and an oversized file, at most 8 MiB + 1 bytes consumed), with the iteration 09a workspace benchmarks in `internal/workspace` before that tagged step (`BenchmarkWorkspaceInitialTransfer` and `BenchmarkWorkspaceIncrementalTransfer` over the production TLS endpoint with the 1,024 × 4 KiB fixture: initial payload under 8 MiB, one-file incremental push plus fetch under 256 KiB and under 10% of the initial, each push a full guarded transaction with generation copy, closure validation, sync, publication and cleanup, reporting payload, copied bytes and files and visited objects per operation; then status and list pagination, show disk accounting, tree-metadata diff and prune retaining one branch while collecting an orphan history, each asserting exact output), then the iteration 09b local transfer benchmarks in `internal/workspacetransfer` (`BenchmarkTransferStatus`: clean and dirty cleanliness over the 1,024 × 4 KiB fixture with nested ignores, visited files and bytes read; `BenchmarkTransferSnapshot`: folder snapshots of the same fixture, initial, unchanged and one-change, with parent histories of 1 and 8 commits, inbound full-history bytes and outbound incremental bytes reported separately, objects created and bytes read; `BenchmarkTransferPush`: initial and incremental git-source pushes over the production TLS endpoint, initial payload under 8 MiB and incremental push plus fetch under 256 KiB and under 10% of the initial; `BenchmarkTransferPullGit`: one new commit into a repository holding the history, selected by branch and separately by hash, and since iteration 10c by task ID (a task ref pulled with the expected instance and commit, the exact-ref check reported separately as `ref-check-ns/op`), objects verified and installed; `BenchmarkTransferExport`: a 64-file export, entries, objects verified and bytes copied; every operation with real syncs, exact commits, trees and unchanged checkouts asserted), then the iteration 10b task workspace benchmarks in `internal/taskworkspace` (`BenchmarkTaskWorkspacePrepare`: a task's preparation over the node route of a real local TLS hub with the 1,024 × 4 KiB fixture, cold and warm cache, base history of 1 and 8 commits, fetched, saved and copied bytes and the independent copy's disk bytes reported separately, initial transfer under 8 MiB and a warm one-change fetch under 256 KiB and under 10% of it, and a cancelled preparation's cleanup; `BenchmarkTaskWorkspaceSnapshot`: the result snapshot of that checkout unchanged, with one changed file and with every file changed, scanned paths, bytes read and objects created; `BenchmarkTaskWorkspacePublish`: one publication of a one-file change through production persistence on both sides, base history of 1 and 8 commits: a real in-process plane (its per-task writer persists the intent, the terminal record and the result receipt) and the worker's own task storage (the sealed, authorized, push and settled checkpoints, the outbox journal and the removal of the whole task directory), with Begin, the deterministic commit, the guarded create-once receive with its generation transaction, the settlement's observation and the task_result exchange up to the committed receipt; it reports the measured pack payload (under 256 KiB), the copied hub generation's bytes and allocations; `BenchmarkTaskWorkspaceMetadata`: the plane's recomputed result metadata and bounded DTO for 100 and 10,000 changed paths, exact totals and at most 32 KiB, and since iteration 10c the hub side of a task status and of one task diff page, timed separately as `task-status-ns/op` and `task-diff-page-ns/op`), and, in the `bench sidecar` step, the iteration 10a benchmarks (`BenchmarkPromptReadiness`: event-to-send time of an immediate readiness report and exactly one report per withheld acknowledgement however many changes coalesce; `BenchmarkGuardianCompletion`: the guardian's cleanup decision with an injected group observation, alone, busy and error, probes per cleanup and no observer left armed, plus one native probe calibration in a helper process), then the non-blocking coordinator waits benchmark (`BenchmarkWaitUntilDone` in `internal/client`, the `bench client wait` step, `-bench=^BenchmarkWaitUntilDone$`: the renewable wait's loop over a fake single-attempt connection and an event-armed fake clock, for 1 and 16 IDs, 120 full 30 s slices (one synthetic hour), 600 capped 100 ms slices, ten immediate snapshots, ten failures through the capped backoff and an immediate winner, reporting `requests/op` and `timers/op`, asserting the exact request and timer schedules, a bounded terminal answer and that no timer, connection or goroutine is retained; no real hour, process or network), each checking its invariants; timings are reported, never gated), `devcheck cross` (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64: 12 artifacts, then each verified, never executed: it exists, is nonempty and has an ELF or Mach-O header naming its GOOS/GOARCH; see Cross-build matrix) |
| `ci-macos` | `macos-15` | 30 min | required | `devcheck native`: the complete suite as one `go test -json -tags=realadaptercheck` invocation, which must show passing run and pass events in `github.com/wedevwork/callsheet/tests/function` for `TestFP6ProcessGroups` and its `cooperative`, `resistant` and `leader-exits-first` scenarios, for the plane trust tests, the node tests, the role tests, the task tests the control tests (iteration 06a: the eight control function parents and the native group qualification; iteration 06b: the four task-control function parents) the MCP tests (iteration 07a: the eight MCP function parents and their mandatory subtests) and the coordinator setup and timeout qualification tests (iteration 07b: the eight function parents and their mandatory subtests) and the real-adapter tests (iteration 08: the nine function parents and their mandatory subtests) and the workspace tests (iteration 09a: the ten function tests and `TestWorkspaceRefSet/max-path`) and the wave-2 real-adapter tests (iteration 11: the nine `TestWave2*` function parents and their mandatory subtests) and the non-blocking coordinator waits tests (the eight function parents), and in `github.com/wedevwork/callsheet/internal/sidecar` for `TestTaskExecutionContract` and its `process` subtest and for the tagged `TestRealAdapterLocal` and its nine subtests (iteration 08's five and iteration 11's four `wave2-*`; see below); and the workspace local transfer tests (iteration 09b: the ten function tests); then, outside that stream, `devcheck coverage`, the workspace benchmark step (iteration 09a), the transfer benchmark step (iteration 09b), the task workspace benchmark step (iteration 10b) and the client wait benchmark step (non-blocking coordinator waits) |
| `ci-linux-stress-packages` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-packages` on Linux: the combined nine-package invocation alone (see Stress checks) |
| `ci-linux-stress-packages-cpu` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-packages-cpu` on Linux: the `internal/contract`, `internal/mcpqual` and `internal/workspace` per-CPU groups, one after another, each three concurrent invocations at CPU 1, 2 and 4 (the stress worker rebalance) |
| `ci-linux-stress-plane-cpu1` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-plane-cpu1` on Linux: `internal/plane` at CPU 1, one invocation alone on its worker (iteration 06a-perf) |
| `ci-linux-stress-plane` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-plane` on Linux: `internal/plane` CPU 2 and CPU 4 as two concurrent invocations (iteration 05b; CPU 1 on its own worker since iteration 06a-perf) |
| `ci-linux-stress-sidecar-cpu1` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-sidecar-cpu1` on Linux: `internal/sidecar` at CPU 1, one invocation alone on its worker (iteration 06a-perf) |
| `ci-linux-stress-sidecar` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-sidecar` on Linux: `internal/sidecar` CPU 2 and CPU 4 as two concurrent invocations (iteration 05b sidecar follow-up; CPU 1 on its own worker since iteration 06a-perf) |
| `ci-linux-stress-processgroup` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-processgroup` on Linux |
| `ci-linux-stress-functions` | `ubuntu-24.04` | 20 min | worker | `devcheck stress-functions` on Linux |
| `ci-macos-stress-packages` | `macos-15` | 20 min | worker | `devcheck stress-packages` on Darwin, with the same commands, repeat count and CPU settings as Linux |
| `ci-macos-stress-packages-cpu` | `macos-15` | 20 min | worker | `devcheck stress-packages-cpu` on Darwin, likewise |
| `ci-macos-stress-plane-cpu1` | `macos-15` | 20 min | worker | `devcheck stress-plane-cpu1` on Darwin, likewise |
| `ci-macos-stress-plane` | `macos-15` | 20 min | worker | `devcheck stress-plane` on Darwin, likewise |
| `ci-macos-stress-sidecar-cpu1` | `macos-15` | 20 min | worker | `devcheck stress-sidecar-cpu1` on Darwin, likewise |
| `ci-macos-stress-sidecar` | `macos-15` | 20 min | worker | `devcheck stress-sidecar` on Darwin, likewise |
| `ci-macos-stress-processgroup` | `macos-15` | 20 min | worker | `devcheck stress-processgroup` on Darwin, likewise |
| `ci-macos-stress-functions` | `macos-15` | 20 min | worker | `devcheck stress-functions` on Darwin, likewise |
| `ci-linux-stress` | `ubuntu-24.04` | 5 min | required summary | no setup; succeeds only if the eight Linux workers all concluded `success` |
| `ci-macos-stress` | `ubuntu-24.04` | 5 min | required summary | no setup; succeeds only if the eight macOS workers all concluded `success` (Ubuntu only evaluates their status; it qualifies nothing about Darwin) |

The two main jobs and the sixteen workers start together on every trigger
and run independently: none waits for, depends on or is conditional on
another (no `needs`, matrix, job or step condition, path filter,
concurrency cancellation or `continue-on-error`), and each fails on its
own. Iteration 02b moved stress out of the two main jobs; iteration 02c
split each platform's stress job into three workers, so its shards run in
parallel with each other and with the main jobs; iteration 05b gave
`internal/plane` a fourth worker per platform, and its sidecar follow-up
gave `internal/sidecar` a fifth; iteration 06a-perf moved the plane and
sidecar CPU 1 invocations to a sixth and seventh worker per platform, so
the plane and sidecar workers now run CPU 2 and CPU 4 only; the stress
worker rebalance (2026-10-07) moved the contract, mcpqual and workspace
per-CPU groups to an eighth worker per platform, `packages-cpu`, so the
packages workers now run the combined invocation only. The main jobs
keep their stages and budgets.

`ci-linux`'s `devcheck test` also runs the M3/M4 container acceptance
(design m3-m4-container-e2e) after its native, race and tagged commands,
as a separate operation of the same stage: it builds static `callsheet`,
`fake-adapter` and the `containeracceptance`-tagged test binary offline,
runs them in one fresh isolated Linux container (`--network none`,
read-only root, an executable 1 GiB `/tmp` tmpfs, no capabilities, user
65532) and accepts the stage only with complete evidence: the 14 case
records, the end record written after cleanup, every container parent and
required subtest passing, a zero exit and the container and image removed.
devcheck retains that evidence in `/tmp/callsheet-container-e2e-evidence`
(`report.txt`, `ledger.jsonl` and `runtime.log`, replaced by each run,
outside the disposable scratch tree, on success and on failure; diagnostic
text is bounded to 8 MiB per run, the ledger is complete). The job's eighth
and final step, `Publish container E2E evidence`, is the workflow's only
step condition: the literal `if: always()` with the single command
`cat /tmp/callsheet-container-e2e-evidence/report.txt`, so the report
reaches the job log even after a failed check step. It is a plain run step
(no action, no expression, no other key); the evidence is published in the
Actions job log under the repository's existing log retention, not as a
downloadable artifact, and the check steps' exit statuses remain the gate.
If setup failed before devcheck created the report, the `cat` fails
visibly; `always()` does not guarantee the step after infrastructure loss
or a forced job termination. The container stage never runs on macOS:
Darwin's `native`, `test` and `all` never invoke it, and the standalone
`container-e2e` stage refuses any non-Linux host before anything starts.

Only the two summaries have dependencies, each on its own platform's eight
workers, and they keep the required stress contexts, so branch protection
needs no change. Each summary is the same small template with literal
job IDs, never a matrix or a dynamic expression:

```yaml
needs: [linux-stress-packages, linux-stress-packages-cpu, linux-stress-plane-cpu1, linux-stress-plane, linux-stress-sidecar-cpu1, linux-stress-sidecar, linux-stress-processgroup, linux-stress-functions]
if: ${{ always() }}
defaults:
  run:
    shell: bash
steps:
  - name: Require every stress shard
    env:
      PACKAGES_RESULT: ${{ needs['linux-stress-packages'].result }}
      PACKAGES_CPU_RESULT: ${{ needs['linux-stress-packages-cpu'].result }}
      PLANE_CPU1_RESULT: ${{ needs['linux-stress-plane-cpu1'].result }}
      PLANE_RESULT: ${{ needs['linux-stress-plane'].result }}
      SIDECAR_CPU1_RESULT: ${{ needs['linux-stress-sidecar-cpu1'].result }}
      SIDECAR_RESULT: ${{ needs['linux-stress-sidecar'].result }}
      PROCESSGROUP_RESULT: ${{ needs['linux-stress-processgroup'].result }}
      FUNCTIONS_RESULT: ${{ needs['linux-stress-functions'].result }}
    run: test "$PACKAGES_RESULT" = success && test "$PACKAGES_CPU_RESULT" = success && test "$PLANE_CPU1_RESULT" = success && test "$PLANE_RESULT" = success && test "$SIDECAR_CPU1_RESULT" = success && test "$SIDECAR_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success
```

(`ci-macos-stress` is identical with `macos-` job IDs; its `run` line is
byte-identical.) `if: ${{ always() }}`
makes the summary evaluate after unsuccessful dependencies instead of being
skipped. Only all eight results `success` exit zero; `failure`,
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
in all, with the 128 earlier names unchanged and first. Each parent delegates by the single control table in
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

Iteration 06b (task controls, protocol 5, milestone M2) appends one
function parent per FP, 145 names in all, with the 141 earlier names
unchanged and first: `TestControlCancellation`,
`TestControlExecutionTimeout`, `TestControlBoundedWait` and
`TestControlForceRemove`. They extend the same control table
(`ControlDelegations`, now 27 rows for 12 wrappers: the 14 earlier rows
unchanged, then 13 more), and each 06b row requires its package contract's
parent **and every mandatory subcase** by name, so a vacuous parent never
qualifies: `TestControlCancel` (plane `unsent`, `loaded-pending`,
`durable-order`, `storage-retry`, `offline`, `duplicate`; sidecar
`preparing`, `guardian-control`, `partial-output`, `cleanup-unconfirmed`,
`duplicate`; client and cli `accepted`, `terminal`, `errors`),
`TestControlTimeout` (sidecar `default-override-zero`, `deadline-tie`,
`slow-start`, `plane-outage`, `restart`, `status-failure`; contract
`policy`, `outcome`, `migration`), `TestControlWait` (plane
`register-race`, `any-of`, `deadline`, `capacity`, `shutdown`, `dispatch`;
client `restart-budget`, `no-dispatch-retry`, `deadline`, `correlation`;
cli `text`, `json`, `exit`; contract `bounds`, `validation`) and
`TestControlRemove` (plane `fence`, `drain`, `restart`, `storage-retry`,
`instance-reuse`; sidecar `removed-instance-cleanup`; cli `pending`,
`completed`, `retry`). A failed, skipped or missing subcase, an empty
selection or an invocation of another package fails the wrapper. The 06a
families gain the 06b cases in place (stop-versus-result durability in
`TestControlCommit`, protocol 5 control strictness and control/heartbeat
fairness in `TestControlProtocol`, cancelled and timed_out late outcomes
and intent replay in `TestControlLate`, journal migration in
`TestControlRestart`). `TestControlNativeGroups` keeps its four scenarios
and sixteen children, but `cooperative` now stops its group through the
public cancel API (after the leader's output and the descendant are
ready) and `resistant` through the guardian's own execution timeout (a
5 s dispatch override, the descendant-ready barrier checked to fall
inside it), adding at most 5 s per ordinary invocation and never repeated
in stress.

Iteration 07a (the coordinator door, `callsheet mcp`) appends one function
parent per FP, each followed by its mandatory direct subtests in the
design's order, 77 more names, 222 in all, with the 145 earlier names
unchanged and first: `TestMCPProtocol` (`initialize`, `version`,
`discovery`, `framing`, `concurrency`, `cancellation`), `TestMCPRelay`
(`ca`, `pin`, `trust-errors`, `protocol-mismatch`, `contract-errors`,
`no-cache`, `recovery`), `TestMCPNodes` (`node-ls`, `node-show`,
`shared-roster`), `TestMCPRoles` (`role-add`, `role-set`, `role-ls`,
`role-show`, `role-rm`, `global-slots`, `force-pending`,
`force-completed`, `operation-rejoin`, `instance-reuse`),
`TestMCPDispatch` (`target-id`, `target-name`, `overrides`, `async`,
`attribution`, `invalid-attribution`, `last-slot`, `wait-fast`,
`wait-slow`, `lost-response`), `TestMCPTaskReads` (`task-ls`,
`task-show`, `task-logs`, `pagination`, `tails`, `binary-logs`,
`late-logs`, `late-not-found`, `output-bounds`), `TestMCPWaitCancel`
(`cancel-accepted`, `cancel-terminal`, `cancel-errors`, `wait-one`,
`wait-many`, `any-terminal`, `snapshot`, `interim-default`,
`budget-deadline`, `delivery-deadline`, `plane-cap`, `restart-budget`,
`own-deadline`, `cancel-wait-only`, `catalog-interim`) and
`TestMCPLifetime` (`eof`, `sigterm`, `sigint`, `closed-stdout`,
`stalled-reader`, `slow-reader-max-logs`, `outstanding-wait`,
`task-survives`, `reaping`). Each name needs its own run and pass event:
a missing, failed or skipped child, a substitute name or a vacuous parent
(its own events without its children's) fails the native check. They
drive the real `callsheet mcp` binary over subprocess pipes with a Go MCP
client (one response dispatcher) against an in-process TLS plane, a
scripted protocol 5 worker and, where a response must be observed, held,
dropped or rewritten, a TLS proxy in front of the plane; no vendor
executable is launched. They prove on the runner itself the stdio
framing, the process's exit status on a broken stdout (`closed-stdout`:
exit 5 by EPIPE, never a SIGPIPE death, with stdin held open), the
writer's progress timeout against a stalled and a paced reader, SIGINT and
SIGTERM, and the outer call budget through the public
`--wait-call-budget 1s` (at most five one-second deadline cases, one
one-second stall and a two-to-three-second paced maximum-log transfer per
invocation; the shipping 10 s default is asserted with an immediately
terminal task). They run in the normal, race and native suites only, never
in a stress shard: the MCP server's timing-dependent unit tests (fake
clocks, no real one-second waits) run in the packages shard with
`internal/mcp`.

Iteration 07b (coordinator setup and timeout qualification) appends one
function parent per FP (FP-9 to FP-16), each followed by its mandatory
direct subtests in the design's order, 51 more names, 273 in all, with the
222 earlier names unchanged and first: `TestMCPSetup` (`claude`, `codex`,
`grok`, `cursor`, `runbook-ownership`, `client-info`),
`TestMCPQualificationProbe` (`immediate`, `slow`, `progress`, `no-token`,
`cancellation`), `TestMCPQualificationSchema` (`plan`, `report`,
`limits`, `paths`), `TestMCPQualificationDecoders` (`claude`, `codex`,
`grok`, `cursor`, `unknown-version`, `non-tool-error`),
`TestMCPQualificationMeasurements` (`default`, `override`, `progress`,
`absolute`, `lower-bound`, `partial`, `budget`),
`TestMCPQualificationPublish` (`verified`, `partial`, `redaction`,
`hashes`, `refuse-conflict`, `worker-facts`),
`TestMCPQualificationReaping` (`cooperative`, `resistant`,
`parent-exits-first`, `interrupt`, `cleanup-failure`) and
`TestMCPQualificationInvocation` (`denied`, `allowed`, `model-free` and
the CI refusal child `TestMCPQualificationInvocation/ci-denied`). The same
run/pass rule applies. They run the real developer
executable `cmd/mcpqual` against fake vendor processes (a small fake vendor
command, `tests/function/testdata/fakevendor`, built once per test process
without the race detector and launched by absolute path with an isolated
`HOME` and a `PATH` that holds only launch-recording traps), and
`TestMCPSetup` and `cleanup-failure` also start the real `callsheet mcp`
against a real test plane through pipes; no test invokes an installed
vendor CLI or a model, and nothing a fake produces becomes a `VERIFIED`
catalog fact. Every qualify-driven child except the CI refusal child
clears `CI` in the launched `mcpqual` process's environment; that child
sets it and proves exit 2 with no launch. Real one-second cleanup graces are limited to
`resistant` and `parent-exits-first` (two per invocation); they run in the
normal, race and native suites only, never in a stress shard.

Iteration 08 (real adapters: Claude and Codex) appends one function parent
per FP (FP-1 to FP-9), each followed by its mandatory direct subtests in the
design's order, 39 more names, 312 in all, with the 273 earlier names
unchanged and first: `TestRealAdapterRegistration` (`registry`, `paths`,
`selection`), `TestRealAdapterProbe` (`claude`, `codex`, `refusal`),
`TestRealAdapterInvocation` (`claude`, `codex`, `stdin`),
`TestRealAdapterClaudeFinal` (`success`, `failure`, `malformed`),
`TestRealAdapterCodexFinal` (`success`, `absent`, `unsafe`, `cleanup`),
`TestRealAdapterOutcomes` (`exits`, `denials`, `controls`, `replay`),
`TestRealAdapterCatalog` (`recipes`, `evidence`, `ownership`),
`TestRealAdapterDispatch` (`claude`, `codex`, `no-vendors`) and
`TestRealAdapterSmokeGate` (`default-off`,
`TestRealAdapterSmokeGate/ci-off`, `absent`, `enabled`).
Iteration 09a (workspace hub) appends its ten function tests in FP order
(FP-1 to FP-10) and FP-7's D1 maximum-path subcase, 11 more names, 323 in
all, with the 312 earlier names unchanged and first: `TestWorkspaceCreate`,
`TestWorkspaceList`, `TestWorkspaceShow`, `TestWorkspaceRemove`,
`TestWorkspacePrune`, `TestWorkspaceTransport`, `TestWorkspaceRefSet`,
`TestWorkspaceRefSet/max-path`, `TestWorkspaceStatus`, `TestWorkspaceDiff`
and `TestWorkspaceNoGit`, so their absence or a skip never satisfies native
qualification.
Iteration 09b (workspace local transfers) appends its ten function tests in
FP order (FP-1 to FP-10) as a separate group, 10 more names, 333 in all,
with the 323 earlier names unchanged and first: `TestWorkspacePushGit`,
`TestWorkspacePushCleanliness`, `TestWorkspacePushFolder`,
`TestWorkspaceTransferIgnores`, `TestWorkspacePullGit`,
`TestWorkspacePullFolder`, `TestWorkspaceTransferCLI`,
`TestWorkspaceTransferMCP`, `TestWorkspaceTransferNoGit` and
`TestWorkspaceTransferEligibility`; Linux and macOS run the same parents
natively.
Iteration 10a (prompt readiness and group completion) appends its two
function tests in FP order (FP-1, FP-2) as a separate group, 2 more names,
335 in all, with the 333 earlier names unchanged and first:
`TestTaskPromptReadiness` and `TestTaskFastGroupCleanup`. Their scenarios
assert inside each parent (no required subtest), so none can silently skip;
on macOS they exercise the native process-group list, on Linux the child
subreaper (see Platform code).
Iteration 10b (workspace execution) appends its nine function tests in FP
order (FP-1 to FP-9) as a separate group with the three named acceptance
scenarios, each right after its parent, 12 more names, 347 in all, with
the 335 earlier names unchanged and first: `TestTaskWorkspaceAdmission`,
`TestTaskWorkspaceAccess`, `TestTaskWorkspaceCache`,
`TestTaskWorkspaceCheckout`, `TestTaskWorkspaceCommit`,
`TestTaskWorkspaceCommit/AC-WS-1`, `TestTaskWorkspacePublication`,
`TestTaskWorkspacePublication/AC-WS-5`, `TestTaskWorkspaceMetadata`,
`TestTaskWorkspaceRecovery`, `TestTaskWorkspaceIsolation` and
`TestTaskWorkspaceIsolation/AC-WS-2`; the three AC scenarios are
native-required on both hosts and a skip never satisfies them.
Iteration 10c (coordinator delivery) appends its six function tests in FP
order (FP-1 to FP-6) as a separate group, 6 more names, 353 in all, with
the 347 earlier names unchanged and first: `TestWorkspaceDispatchDoors`,
`TestWorkspaceTaskPull`, `TestWorkspaceTaskInspect`, `TestWorkspaceTaskMCP`,
`TestWorkspaceMultiHop` and `TestWorkspaceOperatorWorkflow`. Their named
subtests (`status`, `diff`; `schemas`, `content`, `lifetime`;
`documentation`, `local`, `manual_checklist`) assert inside each parent and
are not inventory entries; `manual_checklist` checks that the operator guide
maps every M4 check to its container acceptance case and keeps the
two-machine session an optional, non-gating observation.
The same run/pass rule applies. Separately, the sidecar tuple
(`NativeTaskProcessPackage`) appends `TestRealAdapterLocal` and its
`selection`, `file`, `ordering`, `diagnostic` and `restart` subtests after
`TestTaskExecutionContract` and its `process` subtest: those six names must
run and pass in `internal/sidecar` itself, and the same names in
`tests/function` or any other package never satisfy them. That contract is
in a `realadaptercheck`-tagged file, so the native stage is one invocation
of `go test -json -tags=realadaptercheck -count=1 -timeout=300s ./...`
(the tag reaches compilation, one package start per package: a second
sidecar invocation would be a duplicate start and fails), `devcheck test`
runs it as `go test -tags=realadaptercheck ./internal/sidecar
-run=^TestRealAdapterLocal$ -count=1` and, on Linux, the same with `-race`,
and `devcheck coverage` compiles the tag into its single profile. No stress
shard passes the tag, so the tagged contract never enters the sidecar CPU1
or CPU2/CPU4 workers. The function parents drive real Callsheet processes
(plane, sidecars, guardians and `callsheet mcp`) against a replay stub
(`tests/function/testdata/worker-replay`, built once per test process)
enabled only through the product's `--claude-adapter` and `--codex-adapter`
options, with an isolated `HOME` and a `PATH` of launch-recording `claude`
and `codex` traps (asserted unused): no job installs or runs a vendor CLI,
calls a model or needs credentials. The stub copies the byte-identical
Linux captures of `tests/testdata/real-adapters/linux-2026-09-30/`; replay
proves Callsheet's handling of the captured contract, not vendor stability,
and no macOS vendor behavior is claimed. The opt-in real smoke
(`tests/smoke`) exists only with the `realadaptersmoke` tag, which no
devcheck command or job enables, so neither `./...` nor the tagged native
stream contains it or a skip event from it; `TestRealAdapterSmokeGate`
proves its gate decisions without calling `t.Skip`. The new adapter unit
tests (pure extractors and argv, and the version probe on an injected
runner and clock) run in the packages shard with the rest of
`internal/adapter`; the function parents run in the normal, race and native
suites only, never in a stress shard.
Iteration 11 (real adapters, wave 2: Grok and Cursor) appends one function
parent per FP (FP-1 to FP-9) as a separate group, each followed by its
mandatory direct subtests in the design's order, 42 more names, 395 in all,
with the 353 earlier names unchanged and first:
`TestWave2Registration` (`TestWave2Registration/registry`,
`TestWave2Registration/paths`, `TestWave2Registration/selection`,
`TestWave2Registration/posture`), `TestWave2Probe`
(`TestWave2Probe/grok`, `TestWave2Probe/cursor`, `TestWave2Probe/refusal`),
`TestWave2Invocation` (`TestWave2Invocation/grok`,
`TestWave2Invocation/cursor-refused`, `TestWave2Invocation/prompt`),
`TestWave2GrokFinal` (`TestWave2GrokFinal/success`,
`TestWave2GrokFinal/error`, `TestWave2GrokFinal/cancelled`,
`TestWave2GrokFinal/malformed`), `TestWave2CursorFinal`
(`TestWave2CursorFinal/success`, `TestWave2CursorFinal/absent`,
`TestWave2CursorFinal/malformed`, `TestWave2CursorFinal/blocked`),
`TestWave2Outcomes` (`TestWave2Outcomes/exits`,
`TestWave2Outcomes/controls`, `TestWave2Outcomes/refusal`,
`TestWave2Outcomes/retry`), `TestWave2Catalog`
(`TestWave2Catalog/recipes`, `TestWave2Catalog/evidence`,
`TestWave2Catalog/ownership`), `TestWave2Dispatch`
(`TestWave2Dispatch/grok`, `TestWave2Dispatch/cursor-refused`,
`TestWave2Dispatch/no-vendors`) and `TestWave2SmokeGate`
(`TestWave2SmokeGate/default-off`, `TestWave2SmokeGate/ci-off`,
`TestWave2SmokeGate/absent`, `TestWave2SmokeGate/enabled`,
`TestWave2SmokeGate/posture`). The sidecar tuple appends the four tagged
direct subtests `TestRealAdapterLocal/wave2-posture`,
`TestRealAdapterLocal/wave2-invocation`, `TestRealAdapterLocal/wave2-outcomes`
and `TestRealAdapterLocal/wave2-retry` after the iteration 08 names (12
names in all), in the same `realadaptercheck`-tagged file set: the native
command, the tagged `test` and `test -race` steps (`-run=^TestRealAdapterLocal$`)
and the tagged coverage profile are unchanged and select them, and no stress
shard does. Every parent and subtest runs and passes on both hosts: on Linux
the Grok parents replay the 2026-10-04 captures
(`tests/testdata/real-adapters/linux-2026-10-04/`) through the replay stub,
now also impersonating `grok` and `cursor-agent` and enabled only through
`--grok-adapter` and `--cursor-adapter`, with launch-recording `grok`,
`cursor-agent`, `agent`, `claude` and `codex` traps asserted unused; on
macOS they assert the native Grok refusal and the tagged local contract
exercises the identical Linux task logic through its role environment's OS
value, never by faking a vendor run's OS. Cursor is refused on both, with
zero task launches; no job runs a vendor CLI, calls a model or needs
credentials, and `TestWave2SmokeGate` proves the wave-2 smoke's gate and
refusal paths without calling `t.Skip`. The new adapter unit tests are pure
or injected and run in the packages shard with the rest of
`internal/adapter`; no job, shard, selector, timeout or repeat count
changes, and CI stays 18 jobs.
Non-blocking coordinator waits (`callsheet task wait --until-done`, the
coordinator runbook example, the MCP short-poll guidance, the retired 07a
interim exception and the short confirmation templates) appends one
function parent per FP (FP-1 to FP-8) as a separate group, 8 more names,
403 in all, with the 395 earlier names unchanged and first:
`TestWaitUntilDoneCLI`, `TestWaitUntilDoneRenewal`,
`TestWaitUntilDoneFailure`, `TestWaitUntilDoneOutput`,
`TestCoordinatorBackgroundWait`, `TestMCPShortPollGuidance`,
`TestShortPollCatalogPolicy` and `TestMCPShortConfirmation`. Their
scenarios assert within each parent (no new inventory entries); they drive
the built `callsheet` and `mcpqual` against real TLS planes, the scripted
worker and the fake vendor, send real SIGINT and SIGTERM, and delegate the
injected clock and error matrices to `TestWaitUntilDoneContract`
(`internal/client`) and `TestUntilDoneCommand` (`internal/cli`). They run
in the normal, race and native suites only, never in a stress shard; the
client's injected matrix runs in the packages shard's unchanged combined
invocation with the rest of `internal/client` (argv and package order
byte-identical), and no CLI package, function selector or container case
enters any shard. The container acceptance's `TestContainerCoordinator`
gains the mandatory `background_wait` subtest (the coordinator's three
subtests, eight required subtests in all): a background `task wait
--until-done` child on the two goal-and-answer tasks while the coordinator
runs `node ls`, the second task released first, the exit notification
handled once and the wait re-armed for the first; still 14 case records
and one end record, once per `devcheck test`, never with `--count=20` in
CI. `devcheck bench` gains `bench client wait` (13 steps; Linux `all`
makes 32 ordinary calls) and `devcheck native` runs it after the task
workspace benchmarks (8 ordinary calls). CI stays 18 jobs.
Decoder enrollment, slice A (`mcpqual capture`, the capture manifest and
bundle validator, the enrollment index, oracle and registry agreement with
an empty production index, the capability-scoped qualification guards and
the capture runbook) appends one function parent per FP (FP-1 to FP-9) as a
separate group, 9 more names, 412 in all, with the 403 earlier names
unchanged and first: `TestMCPCaptureInvocation`, `TestMCPCaptureRecipes`,
`TestMCPCaptureEvidence`, `TestMCPCaptureLifecycle`,
`TestMCPEnrollmentContract`, `TestMCPEnrollmentReplay`,
`TestMCPEnrolledShortConfirmation`, `TestMCPCaptureRunbook` and
`TestMCPEnrollmentCIPolicy`. Each parent asserts its literal case inventory
(count and labels) before running its cases and that every case completed;
the cases are not inventory entries. They run the built `mcpqual` with the
fake vendor and the real probe (never an installed vendor CLI or a model:
capture and qualify refuse any `CI` presence, even empty), and the
enrollment and CI-policy parents run the production validators over
temporary copies. They run in the normal, race and native suites only,
never in a stress shard; the new `internal/mcpqual` unit tests are injected
and fake-clock only (no subprocess or grace sleep) and run in that
package's unchanged per-CPU stress invocations. `devcheck bench` keeps its
13 steps (Linux `all` still makes 32 ordinary calls): `bench mcpqual` picks
up `BenchmarkMCPCaptureEvidence`, the only user of the production 8 MiB
capture limits. `devcheck native` now also runs that same `bench mcpqual`
step after the client wait benchmark (9 ordinary calls). CI stays 18 jobs.
Its per-package timeout is 300 s (iteration 09b, raised from 180 s by the
owner's decision of 2026-10-01; Linux `devcheck test`, coverage and every
stress stage keep their own limits, and the `ci-macos` job keeps its
30-minute limit): the macOS function package takes about 110–146 s on a
normal hosted runner, and runners up to 1.6–2× slower were observed, so a
180 s bound left no headroom. Moving FP-8's cross builds out of the package
(below) took about 6–18 s off a local macOS-like run (3 CPUs, host cache
warm, cross targets cold: 94.0 and 104.5 s before, 86.0 and 88.1 s after,
2026-10-02); the limit stays 300 s.

Cross-build matrix (FP-8; moved out of `tests/function` on 2026-10-02).
`devcheck cross` is FP-8's proof. It builds callsheet, fake-adapter and the
process-group test binary (`go test -c`) with `CGO_ENABLED=0` for
linux/amd64, linux/arm64, darwin/amd64 and darwin/arm64, and then
`devcheck.Cross` itself verifies the output directory without executing
anything: it must hold exactly the 12 planned artifacts, each a nonempty
regular file whose ELF (linux) or Mach-O (darwin) header, read with
`debug/elf` and `debug/macho`, names its GOOS/GOARCH. A missing, empty,
unplanned or mis-targeted artifact fails the stage, and the error names
every bad artifact. The stage runs on `ci-linux`. Until then
`TestFP8BuildMatrix` built the same 12 artifacts a second time inside
`tests/function`, the package's largest single cost (43–70 s on hosted
macOS runners). It no longer cross-compiles the matrix. It checks
`devcheck.Matrix`, `CrossPlan` and `CrossArtifacts` against a supported set
written out independently (4 targets, 12 artifact names, exact argv and
environment). It runs `Cross` with `ExecRunner` for the host's own target,
three real builds whose packages the suite's CGO-disabled helper builds
have already compiled; Cross verifies them and the test re-inspects them
independently. It then shows the verifier refusing those real host
artifacts planned under every other target's names. It is an ordinary
parallel test in the normal, race and macOS native suites, and
`TestFunctionParallelSafety` still requires that no parallel test changes
the working directory or environment and that no test starts a goroutine
before `t.Parallel()`. `ci-macos` has no cross step, deliberately: a
CGO-disabled Go cross build does not depend on its build host (GOOS and
GOARCH select the sources and the code generator, the same toolchain
version runs on both), the darwin artifacts are already built and verified
from the Linux host, and on macOS FP-8's function test still runs `Cross`
for the runner's own darwin target and verifies its Mach-O output. A
darwin-host cross step would repeat the 12 builds on the slower runner to
prove nothing the Linux stage does not.

The main jobs and all sixteen workers check out the event's revision without
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
Darwin architecture. Since iteration 09a, once the JSON qualification has
succeeded, and outside its parsed event stream, `devcheck native` also runs
the coverage stage (the project-wide gate and the new/changed coverage
manifest below) and the workspace benchmark step (`bench workspace`) on the
macOS runner, since iteration 09b the transfer benchmark step
(`bench workspacetransfer`) after it, and since iteration 10b the task
workspace benchmark step (`bench taskworkspace`, `go test
./internal/taskworkspace -run=^$ -bench=. -benchmem -benchtime=3x -count=1
-timeout=180s`) after that (seven runner invocations in all); the other
benchmarks remain Linux reference measurements.

Coverage of new and changed code (iteration 09a): besides the project-wide
gate, `devcheck coverage` reads its own generated profile and computes the
statement-weighted coverage of the blocks selected by
`WorkspaceCoverageManifest` (`internal/devcheck/coverage_manifest_data.go`):
every new production file of the iteration (all blocks) and the changed
executable line ranges (the new side of the diff against the iteration's
base) of existing production files, each duplicated block counted once
(covered if any test binary covered it). Two groups must each be strictly
greater than 80.0%: the new `internal/workspace` package, and the union of
every other new or changed production block (devcheck's own policy code
included). A manifest file absent from the profile, a range that selects no
block, an empty group or exactly 80.0% fails. The manifest is committed: CI
reads no git base and needs no network; the reviewer compares it with the
implementation diff and rejects omissions.

Iteration 09b extends the policy. A third group, `workspacetransfer`
(`GroupTransfer`), holds every production file of `internal/workspacetransfer`;
the other 09b new or changed files (the transfer contracts, the git session,
the CLI leaves, the MCP tools and devcheck) stay in the changed group, and
the 09a entries are retained. Every 09b entry has an empty range list (its
whole file). `CoverageEntry.OS` is `""` for a common file or `linux` /
`darwin` for a native-only file (`publish_linux.go` and `publish_darwin.go`);
`CheckCoverageManifest(goos, profile, manifest)` takes the driver's explicit
`goos`. Every entry's group, OS, path and ranges are validated before any
filtering (an unknown OS or group fails on every host, and a file listed
with conflicting OS or group fails); a valid other-OS entry is skipped on
this host, which is never a passing observation. Each applicable listed
file must also pass on its own, over all of its profile blocks whatever its
legacy ranges select: no blocks, zero statements or at most 80.0% fails.
All three groups must have applicable statements and pass on each host;
duplicate entries count each block once; diagnostics are sorted by file and
group. Linux and macOS coverage jobs must each pass: numerators and
denominators are never added across hosts, and a foreign-OS omission is
never a pass.

Iteration 10a adds six whole-file entries to the changed group: its two
build-selected guardian primitives, `internal/sidecar/group_alone_linux.go`
(`linux`) and `internal/sidecar/group_alone_darwin.go` (`darwin`), and the
four existing sidecar files it changes, common to both hosts:
`internal/sidecar/guardian.go`, `session.go`, `task_process_unix.go` and
`tasks.go` (`TestCoverageManifestFiles` requires all six).

Iteration 10b lists every new or changed production file as a whole file
for both hosts (none has a build constraint): the hub's task routes
(`internal/workspace/tasks.go`) in the workspace group and the transfer
package's task primitives (`task.go`, `task_snapshot.go`) in the transfer
group, as `TestCoverageManifestFiles` requires of those packages; the two
new packages `internal/taskworkspace` and `internal/taskpublication` and
the changed contract, adapter, client, plane, sidecar and devcheck files in
the changed group. The 09a range entry of `internal/plane/server.go` is
replaced by its whole file. The unit coverage command is unchanged: its
`-coverpkg=./internal/...,./cmd/...` makes the external plane-wiring
harness in `internal/taskpublication` (an in-process plane, a simulated
protocol-6 node and the real worker library over HTTPS) observable for
the plane files; function tests never count.

## Stress checks

`go run ./cmd/devcheck stress` repeats the timing- and concurrency-sensitive
tests under the race detector with varied parallelism, on Linux and macOS.
In CI it runs as eight shards per platform (iteration 02c; the plane shard
since iteration 05b, the sidecar shard since its sidecar follow-up, the
plane CPU1 and sidecar CPU1 shards since iteration 06a-perf, the
packages-cpu shard since the stress worker rebalance), one worker
job each: `ci-linux-stress-packages`, `ci-linux-stress-packages-cpu`,
`ci-linux-stress-plane-cpu1`,
`ci-linux-stress-plane`, `ci-linux-stress-sidecar-cpu1`,
`ci-linux-stress-sidecar`, `ci-linux-stress-processgroup` and
`ci-linux-stress-functions` run `devcheck stress-packages`,
`devcheck stress-packages-cpu`,
`devcheck stress-plane-cpu1`, `devcheck stress-plane`,
`devcheck stress-sidecar-cpu1`, `devcheck stress-sidecar`,
`devcheck stress-processgroup` and `devcheck stress-functions` on Linux,
and the eight `ci-macos-stress-*` workers run the same stages on macOS.
The project's declared repeat count is 20 per CPU setting (1, 2, 4). The
flow's coder and reviewer use this count when they re-run timing-dependent
tests they add or modify: `devcheck stress` for tests in its covered packages,
and the equivalent `go test -race -count=20 -cpu=1,2,4 -run '<tests>'
<package>` command for changed timing tests outside that set, such as the
coordinator's own `TestStressConcurrencyContract` (see Local verification).

The count, CPU list, shards, package groups, selectors, wave schedule and
time budgets are declared once, in `internal/devcheck/stress.go`
(`StressCount`, `StressShards`, `stressWaves`, and `StressSteps`, the
flattened inspection view). The eight shards run twenty-two commands
(argv, never a shell), each with `CGO_ENABLED=1`, named `stress packages`,
`stress contract cpu1`, `stress contract cpu2`, `stress contract cpu4`
(the contract headroom fix), `stress mcpqual cpu1`, `stress mcpqual cpu2`,
`stress mcpqual cpu4`, `stress workspace cpu1`, `stress workspace cpu2`,
`stress workspace cpu4` (the workspace and mcpqual headroom fix),
`stress plane cpu1`, `stress plane cpu2`, `stress plane cpu4`
(iteration 05b), `stress sidecar cpu1`, `stress sidecar cpu2`,
`stress sidecar cpu4` (the iteration 05b sidecar follow-up),
`stress processgroup cpu1`, `stress processgroup cpu2`,
`stress processgroup cpu4`, `stress function`, `stress plane function` and
`stress node function` (iteration 03); the packages command gained
`./internal/adapter` in iteration 04, gave `./internal/plane` to the plane
shard in iteration 05b, gave `./internal/sidecar` to the sidecar shard
in its sidecar follow-up, gained `./internal/mcp` in iteration 07a,
gained `./internal/mcpqual` in iteration 07b, gained
`./internal/workspace` in iteration 09a and gained
`./internal/workspacetransfer` (after it) in iteration 09b, gained
`./internal/taskworkspace` and `./internal/taskpublication` (after it, in
that order) in iteration 10b, gave
`./internal/contract` to the packages shard's own per-CPU group in the
contract headroom fix (2026-10-02), and gave `./internal/mcpqual` and
`./internal/workspace` to per-CPU groups of their own after it in the
workspace and mcpqual headroom fix (2026-10-02).
Iteration 06a-perf changed no command, only the
grouping: `stress plane cpu1` and `stress sidecar cpu1` each have a shard
of their own. The stress worker rebalance (2026-10-07) also changed no
command, flag, selection or order, only command ownership: the packages
shard (`devcheck stress-packages`) now owns one command, the combined
nine-package invocation, and the new packages-cpu shard
(`devcheck stress-packages-cpu`) owns nine, the contract, mcpqual and
workspace per-CPU groups. packages-cpu contains all three per-CPU groups
at CPU 1, 2 and 4; unlike plane-cpu1 and sidecar-cpu1 it is not a
CPU1-only shard, and there is no `packages-cpu1` stage.
`internal/workspacetransfer` stays in the combined invocation. The
twenty-two commands, in flattened order:

```
go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/adapter ./internal/mcp ./internal/workspacetransfer ./internal/taskworkspace ./internal/taskpublication
go test -race -count=20 -cpu=1 -timeout=6m ./internal/contract
go test -race -count=20 -cpu=2 -timeout=6m ./internal/contract
go test -race -count=20 -cpu=4 -timeout=6m ./internal/contract
go test -race -count=20 -cpu=1 -timeout=6m ./internal/mcpqual
go test -race -count=20 -cpu=2 -timeout=6m ./internal/mcpqual
go test -race -count=20 -cpu=4 -timeout=6m ./internal/mcpqual
go test -race -count=20 -cpu=1 -timeout=6m ./internal/workspace
go test -race -count=20 -cpu=2 -timeout=6m ./internal/workspace
go test -race -count=20 -cpu=4 -timeout=6m ./internal/workspace
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
| `packages` | `devcheck stress-packages` | `stress packages` | one invocation, the combined nine-package command alone (since the stress worker rebalance) |
| `packages-cpu` | `devcheck stress-packages-cpu` | `stress contract cpu1`, `cpu2`, `cpu4`, then `stress mcpqual cpu1`, `cpu2`, `cpu4`, then `stress workspace cpu1`, `cpu2`, `cpu4` | one group per split package (`internal/contract`, `internal/mcpqual`, `internal/workspace`), one group after another, each three concurrent invocations, one per CPU setting (the headroom fixes; their own shard since the stress worker rebalance) |
| `plane-cpu1` | `devcheck stress-plane-cpu1` | `stress plane cpu1` | one invocation, alone on its worker (iteration 06a-perf) |
| `plane` | `devcheck stress-plane` | `stress plane cpu2`, `cpu4` | two concurrent invocations, one per CPU setting (iteration 05b; CPU 1 moved to `plane-cpu1` in iteration 06a-perf) |
| `sidecar-cpu1` | `devcheck stress-sidecar-cpu1` | `stress sidecar cpu1` | one invocation, alone on its worker (iteration 06a-perf) |
| `sidecar` | `devcheck stress-sidecar` | `stress sidecar cpu2`, `cpu4` | two concurrent invocations, one per CPU setting (iteration 05b sidecar follow-up; CPU 1 moved to `sidecar-cpu1` in iteration 06a-perf) |
| `processgroup` | `devcheck stress-processgroup` | `stress processgroup cpu1`, `cpu2`, `cpu4` | three concurrent invocations, one per CPU setting |
| `functions` | `devcheck stress-functions` | `stress function`, then `stress plane function`, then `stress node function` | sequential |

`StressSteps` lists the twenty-two commands in shard and CPU order (a
shard's sequential commands, then its per-CPU groups in order): thirteen
until the contract headroom fix, the same commands in the same order as
before iteration 06a-perf, and sixteen until the workspace and mcpqual
headroom fix; the stress worker rebalance left the twenty-two and their
order unchanged. It is an inspection view, never the execution order:
a concurrent shard runs its invocations in the waves `stressWaves`
schedules, and that schedule is empty, so every concurrent shard starts
all of its invocations at once: the plane and sidecar CPU1 shards their
one, the plane and sidecar pairs their two and processgroup its three.
The packages-cpu shard's per-CPU groups are not a wave schedule and
`stressWaves` never applies to them: the shard is sequential and has no
step of its own, so contract's three invocations start together, then
mcpqual's once contract's have all succeeded and been joined, then
workspace's likewise. In `devcheck stress` the packages-cpu shard starts
only once `stress packages` has succeeded.

Since the stress worker rebalance `devcheck stress-packages` runs the
combined invocation only. To repeat everything it ran before, run both
`devcheck stress-packages` and `devcheck stress-packages-cpu` (or
`devcheck stress`); each stage then has its own 15-minute watchdog.

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
  (its own per-CPU group since the contract headroom fix),
  `internal/adapter`, (iteration 07a) `internal/mcp`, (iteration 07b)
  `internal/mcpqual`, (iteration 09a) `internal/workspace` (each its own
  per-CPU group since the workspace and mcpqual headroom fix) and
  (iteration 09b) `internal/workspacetransfer` in the packages shard (the
  contract, mcpqual and workspace groups in the packages-cpu shard since
  the stress worker rebalance), `internal/plane` in the
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
  Iteration 06b (task controls) adds no package, selector, shard or job
  either (the pre-authorised CPU1 split was not applied: 18 jobs): its
  contracts (`TestControlCancel`, `TestControlTimeout`, `TestControlWait`
  and `TestControlRemove`, with the 06a families' new cases) run in their
  packages' existing shards (plane and sidecar CPU 1 alone and CPU 2 and 4
  concurrent; contract and client in the packages shard; cli and devcheck
  in the ordinary suites) with injected guardians, groups, timers and
  clocks: zero real children per repetition, the sidecar's process
  qualification unchanged at five.
  Iteration 07a (the coordinator door) adds exactly one package,
  `internal/mcp`, to the packages invocation and nothing else: no
  selector, count, CPU setting, shard or job changes, and nothing moves
  to or from the plane and sidecar CPU1 shards. Its unit tests prove the
  server's framing, lifecycle, cancellation, writer progress timer and
  call-budget races on event-armed fake clocks, with no real one-second
  wait and no child process. Its one maximum-size case
  (`TestTaskLogsMaximum`, a 10 MiB log in a 14 MB frame) is built with
  `//go:build !race`: it runs in the ordinary and coverage suites, while
  the race builds of this shard keep every concurrency case at smaller
  sizes; the eight `TestMCP*` function parents, which
  start `callsheet mcp` subprocesses and use real one-second budgets, are
  not repeated in stress.
  Iteration 07b (coordinator setup and timeout qualification) adds exactly
  one package, `internal/mcpqual`, to the packages invocation and nothing
  else: no selector, count, CPU setting, shard or job changes, and nothing
  moves to or from the plane and sidecar CPU1 (or CPU 2 and 4) shards. It
  stays there for its timing-dependent boundaries, not for its pure
  schemas and decoders: the probe's cancellation versus completion,
  progress versus completion and deadline versus result races on
  event-armed fake clocks, and the scheduler and cleanup escalation with
  injected process runners, signalers and clocks: zero real subprocesses
  and no real grace per repetition; every one of its tests runs in the race
  builds of this shard. Read-only publication fixtures (completed fake runs)
  are built once per test process rather than once per repetition. The real
  process launcher lives in `internal/mcpqual/procexec`, outside the stress
  selection: its tests start short-lived real subprocesses in the ordinary,
  race and coverage suites only. The eight 07b function parents, which
  start real `mcpqual` and fake vendor processes and use real one-second
  cleanup graces, are not repeated in stress.
  Iteration 09a (workspace hub) adds exactly one package,
  `internal/workspace`, to the packages invocation and nothing else: no
  selector, count, CPU setting, shard or job changes. Its concurrency tests
  repeat there with deterministic barriers and hooks (cancellable lock
  waits, readers during a mutation, the create/remove registry race,
  concurrent equal-old ref set and receive-pack writers with exactly one
  CAS winner, cancellation before publication, and the git handler's
  halt/join of a pending body read); they start no subprocess. Under a
  repeated run the exhaustive failure-injection matrix
  (`TestDurabilityBoundaries`) injects every twentieth boundary with an
  offset rotating per repetition, so the 20 repetitions at each CPU
  setting together still inject every boundary once; an ordinary run
  injects them all. The ten `TestWorkspace*` function tests, which start
  plane, CLI and `callsheet mcp` subprocesses, are not repeated in stress.
  Iteration 09b (workspace local transfers) adds exactly one package,
  `internal/workspacetransfer`, after `internal/workspace` in the packages
  invocation and nothing else: no selector, count, CPU setting, shard or
  job changes, and nothing in the plane or sidecar shards. Its
  timing-dependent tests repeat there every time: the git session's
  event-armed no-progress watchdog (a fake clock advanced only after each
  "armed" event), two concurrent writers on one absent branch with exactly
  one CAS winner (a barrier at the observation point), the local ref locks
  and cancellation joins. Orchestration tests use an in-memory plane and
  tiny fixtures; the production handler proofs use a real TLS test plane
  (one shared per test process for read-only proofs). Deterministic case
  matrices (failure injection at every storage boundary, eligibility and
  unsafe-tree cases, the zero-object CAS proof and the selector and export
  proofs) follow the 09a sampling convention: under a repeated run each
  invocation runs every twentieth case with an offset rotating per
  repetition, so the 20 repetitions at each CPU setting together still run
  every case; an ordinary run runs them all. No test starts a subprocess.
  The ten 09b function tests are not repeated in stress.
  Iteration 10b (workspace execution) adds exactly two packages,
  `internal/taskworkspace` and `internal/taskpublication`, after
  `internal/workspacetransfer` in the combined packages invocation, in that
  order, and nothing else: no selector, count, CPU setting, shard or job
  changes, no plane stress case and no new repeated function selector; the
  three per-CPU groups (contract, mcpqual, workspace) stay exact. The new
  lifecycle matrices live there: the cache's locking, eviction and
  corruption cases, the preparation, snapshot and publication state
  machine with fake clocks armed before every advance, the publication
  transaction on the real hub with a durable file-backed task store and
  its crash/replay boundaries, and the external plane-wiring harness (an
  in-process plane and a simulated node; no real child). The sidecar's
  small injected workspace session cases repeat with the sidecar package;
  the nine `TestTaskWorkspace*` function tests are not repeated in stress.
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
  FP-8's cross-build entry-point test are deliberately excluded. The
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
  platforms. The contract headroom fix likewise turned `internal/contract`'s
  share of the combined packages invocation into three invocations of one
  CPU setting each (`stress contract cpu1`, `cpu2`, `cpu4`) inside the
  packages shard: no tuple, count, CPU setting, shard, stage or job
  changed, and every contract test still runs 60 times per platform. The
  workspace and mcpqual headroom fix did the same for `internal/mcpqual`
  (`stress mcpqual cpu1`, `cpu2`, `cpu4`) and `internal/workspace`
  (`stress workspace cpu1`, `cpu2`, `cpu4`), again without changing a
  tuple, count, CPU setting, shard, stage or job.
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
packages-cpu, plane-cpu1, plane, sidecar-cpu1, sidecar, processgroup,
functions; a shard stage runs exactly its own commands. The packages shard
runs `stress packages` alone; in `devcheck stress` a failed packages shard
never starts packages-cpu. The packages-cpu shard (the stress worker
rebalance) has no sequential command of its own and runs its per-CPU
groups one after another: `stress contract cpu1`, `cpu2` and `cpu4`,
then `stress mcpqual cpu1`, `cpu2` and `cpu4`, then
`stress workspace cpu1`, `cpu2` and `cpu4`. Each group's three
invocations start together through the same concurrent coordinator as the
Parallel shards (at most three at once, the same watchdog, logs such as
`stress-contract-cpu1.log`, `stress-mcpqual-cpu2.log` and
`stress-workspace-cpu4.log` owned by `devcheck stress-packages-cpu`,
replayed in CPU order, all three joined before the group fails or the next
group starts), and a failure in a group prevents the later groups,
plane-cpu1 and every later shard from starting. Sequential commands stop at the first
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

Why contract, mcpqual and workspace run per CPU setting (the headroom
fixes, 2026-10-02). First, the contract headroom fix.
In PR #15's run 36992345488 the `internal/contract` binary took 348.9 s of
its 360 s `-timeout=6m` inside `stress packages` on a slow Linux runner (232 s
in run 36974512401, 218 s on macOS), with no contract change: its time is
CPU-bound race-detector work on maximum-size JSON (`TestTaskContract`
and `TestFrameLimits` at the pinned 2 MiB frame and log limits).
Test-only input caching took the local binary from 112.3 s to about 105 s,
not enough, so the owner chose the plane and sidecar pattern: three
single-CPU invocations, each with its own 6-minute limit, kept inside the
packages shard so that the 18 jobs, required checks and job names stay.
Measured locally on three pinned cores (`taskset -c 0-2`, Linux amd64, a
warm build cache), sequentially: each contract invocation alone 35.5 s
(CPU 1), 35.0 s (CPU 2) and 34.3 s (CPU 4), the three concurrently 41.2 s,
41.5 s and 42.4 s (42.7 s wall), and the other nine packages' combined
invocation 247.2 s. Run one after another the four commands would take
about 352 s; the combined invocation followed by the concurrent group
about 290 s, so the group runs concurrently. The whole stage, measured the
same way: `devcheck stress-packages` 291.5 s (`stress packages` 247.6 s,
then `stress contract cpu1`, `cpu2` and `cpu4` 42.8 s, 42.8 s and 43.6 s)
against 297.9 s and 314.1 s for two runs before the change. At the
observed worst-case factor of about 3.2 (hosted time over local time), a
contract invocation projects to about 140 s against its 360 s limit.
These are local planning figures, not a hosted measurement.

Then the workspace and mcpqual headroom fix. In PR #16's run 37009626145
the combined invocation took 273.3 s (Linux) and 291.5 s (macOS) for
`internal/workspace`, 236.6 s and 243.0 s for `internal/mcpqual` and
198.4 s and 192.1 s for `internal/workspacetransfer`, while the split
contract invocations took 70–94 s each. At the 1.5× slow-runner factor
seen in PR #15's run, workspace projects to about 410 s (a timeout) and
mcpqual to about 355 s, so both took contract's mechanism: three
single-CPU invocations each, in groups of their own after contract's.
Their cost is CPU-bound race work in the workspace fixture copies,
validation, durability boundaries and transport tests, and in mcpqual's
JSON report stages.
`internal/workspacetransfer` (about 300 s at 1.5×) stayed in the combined
invocation at this fix; a group of its own was later tried and withdrawn
(iteration 10b's r0.5 and r0.6 schedules, below), and it stays combined.
The schedule was chosen by measurement, sequentially on pinned cores
(Linux amd64, warm build cache), against an export of 473ceb2:

| Pinned cores | Stage before | Seven-package combined | Groups as waves of three (contract / mcpqual / workspace) | The nine as a pool of three, longest first | Stage after |
|---|---|---|---|---|---|
| 3 (`taskset -c 0-2`) | 297.0 s | 167.0 s | 42.9 / 44.4 / 60.0 s | 148.6 s | 304.5 s |
| 4 (`taskset -c 0-3`) | 255.0 s | 145.5 s | 44.0 / 38.9 / 45.4 s | 125.9 s | 276.7 s |

Running the nine invocations as three per-package groups costs about the
same as a pool of three (147.3 s against 148.6 s on three cores, 128.3 s
against 125.9 s on four) and needs no new coordinator, so the groups run
one after another through the existing one; at most three invocations run
at once on either runner. In the measured stage after the change
(`devcheck stress-packages`) the invocations took 42.5, 42.4 and 43.3 s
(contract CPU 1, 2, 4), 38.9, 39.9 and 43.0 s (mcpqual) and 47.0, 53.2 and
57.8 s (workspace) on three cores, and 41.5–41.9 s, 38.0–38.8 s and
44.9–45.8 s on four; the stage grew from 297.0 s to 304.5 s on three cores
and from 255.0 s to 276.7 s on four. Projected from CI's own figures, at
the highest hosted-to-local ratio observed for a split invocation (about
2.2, contract's 94 s against 43 s) and then the 1.5× slow-runner factor,
the worst cases are about 191 s for a workspace invocation, about 142 s
for mcpqual and about 141 s for contract (94 s × 1.5), each against its
own 360 s limit. These are planning figures, not hosted measurements.

Then iteration 10b's r0.5 schedule (2026-10-04), since withdrawn. With the
task workspace packages added to the combined invocation, run 37188932800
measured the `internal/workspacetransfer` binary at 216.1 s (Linux) and
278.9 s (macOS) there, against 143.9 s and 138.4 s on main's run
37101472176; locally the package was not slower alone, so the growth was
contention in the combined invocation. r0.5 gave it the same mechanism as
a fourth per-CPU group after workspace's, with the 15-minute watchdog, the
360 s binary timeout, the repetition counts and the 18 jobs unchanged.

Then iteration 10b's r0.6 schedule (2026-10-04) returned
`internal/workspacetransfer` to the combined invocation, after
`internal/mcp` and before `internal/taskworkspace` and
`internal/taskpublication`, and the per-CPU groups to the original three
(contract, mcpqual, workspace). Run 37199285026 on 56fa0d8 (the r0.5
schedule), against run 37188932800 on 1c3b8b3: the macOS stress-packages
step fell from 724 s to 689 s and its combined command from 471 s to
320 s, with the transfer group adding 113 s; the Linux step grew from
635 s to 779 s although its combined command fell from 360 s to 318 s,
because the transfer group added 145 s sequentially after contract's
140 s, mcpqual's 70 s and workspace's 105 s, whose waves were already
full. Before the split, transfer had extended the Linux combined command
by about 42 s while its own binary took 216 s: the overlap inside the
combined invocation was the saving. macOS taskworkspace inside the
combined command fell from 249 s to 171 s after the compressed loose copy
and the task-upload pack window, reducing the contention that motivated
the split. The watchdog, binary timeout, repetition counts and jobs are
unchanged; these figures are historical evidence, not new allowances.

Then the stress worker rebalance (2026-10-07, design stress-rebalance
r0.2, owner sign-off 2026-10-07). On slow macOS runners
`ci-macos-stress-packages`, the combined invocation followed by the three
per-CPU groups under one 15-minute watchdog, no longer fit: main run
37596057274 (5e02892, slice A) passed 900 s with the combined command at
574 s, contract at 116 s and mcpqual at 130 s, and the watchdog killed
workspace; PR #31's run 37640343460 passed 900 s with the combined command
at 515 s, contract at 123 s and mcpqual at 119 s, and workspace was killed
at 143 s (a censored observation, not a completion or an upper bound).
Completed runs between them took 703–837 s (combined 407–508 s; each
group's time is that of its CPU 1 invocation). Linux has the same
structure at 733–743 s on recent runs. The owner chose a structural
rebalance over further test-cost cuts or reverting slice A: the three
per-CPU groups moved unchanged to a new group-only shard, `packages-cpu`,
with its own worker per platform (`ci-linux-stress-packages-cpu`,
`ci-macos-stress-packages-cpu`) and its own 15-minute watchdog, and the
packages workers keep the combined invocation alone. It adds two jobs
(18 to 20) and no required context; no test workload, command, flag,
repetition count, CPU setting, binary timeout or watchdog changed, and
`internal/workspacetransfer` stays combined. Moving one or two groups to a
lighter worker, or overlapping the groups with the combined invocation on
one worker, was rejected: the remaining packages worker would still exceed
the target, and the overlap adds contention on the same three macOS cores
(the pool-of-three measurement above saved nothing, and r0.5's transfer
group grew the Linux step). The per-worker review target for this
rebalance is 675 seconds (75% of the 900 s watchdog) on a slow runner, a
review target and not a runtime timeout; values above 675 need an
explanation and an owner disposition before the headroom objective is
claimed, and no test is weakened to meet it.

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
  that is each worker job, has its own (`devcheck stress-packages` and
  `devcheck stress-packages-cpu` each one, since the stress worker
  rebalance). A local `devcheck stress` runs all
  eight shards in one process, one after another, under one shared
  15-minute watchdog rather than eight, with no reset at the
  packages/packages-cpu boundary; it is not a simulation of
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
  command. The stress worker rebalance (2026-10-07), by design revision and
  owner sign-off, moved the contract, mcpqual and workspace per-CPU groups
  to the packages-cpu shard and a worker of its own per platform, changing
  no command.
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

- Iteration 06b allocation (design 06b DW6): a conservative screening
  estimate, not a gate and not a measurement. The new plane matrix counts
  21 case-equivalents and the sidecar's 16 (17 and 12 new first-level
  subtests plus four for the 06a families' extensions), costed at the
  measured persistence cases (0.10 s plane, 0.12 s sidecar per
  repetition, Linux `-race -count=1 -cpu=1`): 42.0 s and 38.4 s added per
  CPU invocation at count 20 on Linux, 63.6 s and 69.6 s on macOS (scaled by
  the hosted CPU1 macOS/Linux ratios 1.5143 and 1.8124). Against the hosted
  06a-perf references that projects plane CPU1 at 199.5 s (Linux) and
  302.1 s (macOS) and sidecar CPU1 at 180.2 s and 326.6 s: both macOS
  values plausibly breach 300 s. The owner chose to build with the current
  matrix and measure on the pull request; a hosted CPU1 invocation strictly
  above 300.0 s, or its timeout, triggers the pre-authorised split of that
  package's CPU1 worker into two alphabetical halves on both platforms (up
  to 22 jobs), through the light flow. CPU 2 and 4 keep the full CPU 1
  increment as a no-speedup allowance. The packages stage keeps its
  provisional 10 s allocation; processgroup and functions have no workload
  delta, and the function package adds at most 5 s per ordinary invocation
  (the native `resistant` timeout) within its 180 s bound.
- Iteration 07b allocation (design 07b, CI plan; a planning allocation,
  not a measurement): the packages stage may grow by at most 10 s on Linux
  and 15 s on macOS relative to landed 07a, for `internal/mcpqual`; the
  `tests/function` package may grow by at most 10 s per ordinary or race
  invocation on Linux and 15 s per native invocation on macOS, within the
  unchanged shared `-timeout=180s`, including at most two one-second
  cleanup graces per invocation (`resistant` and `parent-exits-first`).
  Unit timing matrices use fake clocks. No plane or sidecar CPU1, CPU2 or
  CPU4 work is added (macOS plane-cpu1 is already about 270 s against its
  300 s trigger), and the processgroup and functions workloads are
  unchanged. The first hosted run records the before/after command and
  binary times with OS, architecture and cache state; a shard or budget
  change needs a design revision, and exceeding an allocation needs
  investigation and a revised allocation, never weaker assertions or
  skipped cases.
- Iteration 08 (design 08 and its sign-off reading of DW3: the shared
  180 s function timeout and the stress allowances are not raised). The
  nine real-adapter parents share one replay deployment (a plane and three
  sidecars) started once per test process and run as parallel parents; the
  tagged sidecar contract adds well under a second to each tagged test and
  race step; no stress shard, count, CPU setting or timeout changes. The
  new `internal/adapter` unit tests are pure or on an injected runner and
  clock (about 0.3 s per repetition under the race detector, far below the
  packages shard's slowest binary). The first hosted run records the
  function package time in the normal, race and native invocations against
  the unchanged 180 s bound; a miss is investigated (fixture sharing first),
  never answered by a raised timeout, a weaker assertion or a skipped case.
- Iteration 09b allocation (planning allowances, not measurements or pass
  thresholds): `stress packages` command growth up to 15 s Linux / 25 s
  macOS; `tests/function` binary growth up to 10 s Linux / 15 s macOS in
  each normal, race and native invocation, retaining the shared 180 s
  limit. New transfer benchmarks up to 10 s Linux / 15 s macOS; no added
  execution in plane, sidecar, processgroup or stress-functions shards, up
  to 5 s shared compile overhead in those workers. No new jobs,
  count/CPU/timeout/watchdog changes. Compare before/after on the same
  runner/cache conditions and record per-package and command durations;
  exceeding an allowance requires explanation and fixture/setup reduction,
  not skipped cases. Any timeout/assertion failure remains failure. Hosted
  evidence stays pending until observed.

- Iteration 10a allocation (planning targets against CI run 37022060367,
  Go 1.26.0, warm cache, Linux amd64 and macOS arm64; diagnostic, not
  numerical acceptance gates): no net plane CPU1 binary or command growth
  from 147.6/153.1 s Linux and 295.1/303.8 s macOS (no plane test is added
  or changed in repeated workloads, and zero H1 saving is credited to plane
  CPU1 until observed); sidecar per-CPU growth zero net relative to
  147.5/153.6 s Linux and 220.8/227.1 s macOS, with a structural saving of
  up to 40 s per CPU invocation from `TestTaskExecutionContract/process`'s
  two natural cleanups (no longer a full one-second grace each) × count 20,
  before probe and fixture cost (an estimate, not a promised measurement);
  packages at most 5 s extra per platform; the normal, race and native
  function binaries no net growth (the two new parents run once each, not
  in any stress shard; H1 shortens real deployments' readiness waits by up
  to a heartbeat interval each). No new repeated sidecar real-child or
  deployment fixture, and no job, selector, timeout, repeat or CPU change.
  The before/after binary and command times are recorded below and on both
  hosts once observed; an allocation miss is investigated (fixture costs
  first), runner variance is not a code defect, and the measured reduction
  is allocated to 10b only once measured on both hosts. Existing test
  timeouts and the two FP latency regression assertions still fail
  normally.

- Iteration 10b allocation (design 10b r0.2 Budgets; planning allowances,
  not measurements or pass thresholds; Go 1.26.0, warm cache, Linux amd64
  and macOS arm64), verbatim:
  10b: place all new lifecycle matrices outside plane; add no plane stress cases or repeated selectors and no child-heavy sidecar matrix. Use post-10a CPU1 CI ranges from runs 37095473701, 37090552825 and 37101472176: sidecar 155.0–173.2 s Linux / 189.0–252.5 s macOS and plane 144.2–213.9 s Linux / 229.9–257.0 s macOS. Allocate sidecar binary growth of 20 s Linux / 30 s macOS for 10b, giving CPU1 planning targets of 193.2 s / 282.5 s from the observed maxima; reserve a further 30 s diagnostic variance envelope, giving 223.2 s / 312.5 s and leaving 136.8 s / 47.5 s below the unchanged 360 s binary timeout. Plane has zero planned growth, with observed maxima 213.9 s / 257.0 s and a 30 s diagnostic envelope of 243.9 s / 287.0 s. Other sidecar CPUs retain the 20 s / 30 s incremental allowance against matched post-10a runs; CPU1 measurements do not establish their baselines. These are planning and investigation thresholds, not comparative wall-clock acceptance gates; one noisy run above a target is an allocation miss to investigate, not proof of a regression or permission to ignore a failure. Record binary and command times separately; use matched alternating base/change observations to distinguish persistent growth from runner variance, retaining all results. The 360 s timeout and all test assertions remain gates. Do not spend an estimated saving twice or weaken tests to meet an allocation. Packages stage growth allowance is 20 s Linux / 30 s macOS; each new combined package binary is allocated 45 s Linux / 60 s macOS across all 60 repetitions. Workspace per-CPU growth allowance is 5 s each; transfer combined growth allowance is 10 s. Function binary growth allowance is 12 s Linux / 18 s macOS per normal/race/native run against 10a. New benchmark step allowance is 10 s Linux / 15 s macOS. Unit coverage execution growth allowance is 20 s Linux / 30 s macOS per coverage command, reported separately from stress. Preserve 18 jobs, four required checks and all existing timeouts. An allocation miss requires investigation and fixture reduction or design revision, never weaker tests.

- Iteration 10c allocation (design 10c r0.4 CI plan and the parent design's
  10c Budgets entry; planning allowances, not measurements or pass
  thresholds; Go 1.26.0, warm cache, Linux amd64 and macOS arm64; compared
  with landed 10b under matched runner and cache conditions), verbatim:
  10c: plane/sidecar stress workload unchanged; packages growth ≤5 s Linux / 8 s macOS; function binary growth ≤8 s Linux / 12 s macOS. No added benchmark step. Other shards: no new execution, ≤5 s shared compile overhead across the whole iteration; summaries unchanged. Preserve all timeouts. Record binary and command times separately and compare matched runner/cache conditions. An allocation miss requires investigation and fixture reduction or design revision, never weaker tests. The owner specifically retains 18 jobs despite the historical 06b split trigger: no automatic expansion to 22 jobs in this iteration.

- Non-blocking coordinator waits allocation (design
  nonblocking-coordinator-waits r0.2 Budgets; planning allocations, not
  measured deltas or pass/fail timing gates; baseline: the coordinator's
  observations of green main run 37415353383 attempt 2 on a09b711, per
  job, command and binary), verbatim:
  Nonblocking waits: preserve 18 jobs, four required checks and every timeout. Against green main run 37415353383 attempt 2 (a09b711), main-job growth allowance is 20s Linux / 20s macOS (764→784s / 464→484s); packages-job growth is 10s each (711→721s / 731→741s), combined packages command 433.9→443.9s / 455.3→465.3s. Client binary growth is 8s each (127.5→135.5s / 106.9→114.9s); each mcpqual CPU binary and the MCP binary gets 2s. Workspacetransfer gets zero new stress work (308.9s Linux / 280.1s macOS against 360s limit). Normal/race function binary growth is 5s each (Linux 110.9→115.9s / 126.6→131.6s); new benchmark execution allocation is 2s per platform. The container-e2e iteration inside ci-linux test gets 2s growth (9.8→11.8s); its static binary build allocation is unchanged at 24.2s. Separately, the internal/testkit/containeracceptance test binary in ci-linux gets 3s growth (94.7→97.7s). Stress-function execution is unchanged (Linux function/plane/node 24.8/19.6/7.9s, macOS 30.6/46.7/17.2s). Other shards get no execution growth and at most 5s shared compilation growth. Reserve a separate ±30s runner-variance envelope; do not spend it as test workload. Compare binary, command and job times separately, retaining all first-run evidence. An allocation miss requires investigation or design revision, never weakened assertions, skips, changed repetition counts or timeout increases.

- Decoder enrollment allocation (design decoder-enrollment r0.2 Budgets;
  planning allowances, not measured deltas or pass/fail timing gates;
  baseline: main run 37523901881 on ff8058f, as named by the coordinator),
  verbatim:
  Decoder enrollment: baseline is main run 37523901881 (ff8058f), as named by the coordinator. Supplied mcpqual per-CPU stress duration is approximately 60–85s per invocation; allocate at most 5s additional test execution per invocation (planning envelope 65–90s, not a measured result), with unchanged 360s binary timeout. Allocate 10s additional packages-job wall time per host, 15s main-job growth Linux and 20s macOS, 5s function-binary growth per native/race invocation and 5s new benchmark execution per host (macOS adds the mcpqual benchmark command). Other shard execution and container workloads get zero growth; shared compilation allowance is 5s/job. Reserve a separate ±30s runner-variance envelope, not spendable test workload. Keep all eighteen jobs, four required checks and existing watchdogs. Compare binary, command and job times separately against run 37523901881, retain failed first-run evidence, and investigate an allocation miss without reducing counts, skipping cases, weakening assertions or increasing timeouts.

- Stress worker rebalance allocation (design stress-rebalance r0.2,
  Documentation and timing qualification; planning envelopes and
  projections, not measurements, guarantees or pass/fail timing gates;
  owner sign-off 2026-10-07). The 15-minute watchdog, the 20-minute worker
  limit, the 6-minute binary limit, `-count=20` and CPU 1, 2 and 4 are
  unchanged. The existing under-10-minute diagnostic target remains
  aspirational; for this rebalance the review target is 675 seconds (75% of
  900) per affected worker on a slow runner, with 11–12 minutes the owner's
  approximate range. It is a review target, not a runtime timeout: a value
  above 675 needs an explicit explanation and owner disposition before the
  headroom objective is claimed met, and no test is weakened to achieve it.
  Planning evidence (seconds; stage time excludes checkout and setup):

  | Platform / affected worker | Expected screening range | Slow-runner planning envelope |
  |---|---|---|
  | macOS packages | combined observations 407–574 | 574 + 45 allowance = 619 (10.3 min) |
  | macOS packages-cpu | completed-run CPU1 group sums 296–333, plus overhead | 123 contract + 130 mcpqual + 191 workspace + 45 allowance = 489 (8.2 min) |
  | Linux packages | approximately 436–441 before allowance | 441 × 1.5 + 15 ≈ 677 (11.3 min) |
  | Linux packages-cpu | approximately 297–302 before allowance | 302 × 1.5 + 45 = 498 (8.3 min) |

  PR #31's workspace was killed at 143 s: censored, not a completion or
  an upper bound; the 191-second workspace figure is this document's
  earlier 1.5× slow-runner projection. CPU 1 timings are proxies for group
  wall time, not proof that CPU 1 is always last; the allowance covers the
  CPU 2/4 tails and changed compilation and cache behaviour and is an
  assumption, not measured overhead. The observed 574 s is already a slow
  combined invocation, so it is not multiplied by 1.5 again. The Linux rows
  are a lower-confidence cross-platform proxy: recent Linux runs give only
  the 733–743 s total, allocated with PR #29 macOS's combined share
  (486/818), then the 1.5× factor. Owner dispositions at sign-off: the
  Linux packages projection of about 677 s (2 s above 675, 223 s below the
  watchdog) is accepted as a marginal target exception pending the first
  remote run, with the existing setup-go build cache retained and no saving
  credited to it; the macOS sensitivity cases are accepted as well: the
  486 s representative combined value (PR #29) at 1.5× gives about
  730 + 45 = 775 s (86% of the watchdog, 100 s above the target) and the
  rounded six-observation median of 497 s about 791 s (88%), both below
  900 s; they are sensitivity cases, not measured bounds, and 75%
  utilization is not guaranteed on every runner. A material measured
  overrun needs owner disposition or a further design revision; neither
  this exception nor the sensitivity case changes the watchdog. Other
  workers have unchanged workloads and budgets; their times are recorded
  for regression context, with no new prediction. The pull request must
  replace these proxies with actual values (First remote run).

Measurements, newest first. Hosted and local figures come from different
machines and are never combined into one number.

- Expected per-job wall-clock after the stress worker rebalance: planning
  envelopes, not measurements (see the stress worker rebalance allocation
  in Budgets; stage time, checkout and setup excluded). Only the packages
  workers and the new packages-cpu workers change:
  - `ci-linux-stress-packages` about 436–441 s before allowance, about
    677 s slow-runner envelope, and `ci-macos-stress-packages` about
    407–574 s, about 619 s slow-runner envelope (775–791 s in the owner's
    sensitivity cases): the combined invocation alone.
  - `ci-linux-stress-packages-cpu` about 297–302 s before allowance, about
    498 s slow-runner envelope, and `ci-macos-stress-packages-cpu` about
    296–333 s of CPU1 group sums plus overhead, about 489 s slow-runner
    envelope: the three per-CPU groups, one after another.
  - `ci-linux` about the same as before, `ci-macos` about the same as
    before, `ci-linux-stress-plane-cpu1` about the same as before,
    `ci-macos-stress-plane-cpu1` about the same as before,
    `ci-linux-stress-plane` about the same as before,
    `ci-macos-stress-plane` about the same as before,
    `ci-linux-stress-sidecar-cpu1` about the same as before,
    `ci-macos-stress-sidecar-cpu1` about the same as before,
    `ci-linux-stress-sidecar` about the same as before,
    `ci-macos-stress-sidecar` about the same as before,
    `ci-linux-stress-processgroup` about the same as before,
    `ci-macos-stress-processgroup` about the same as before,
    `ci-linux-stress-functions` about the same as before and
    `ci-macos-stress-functions` about the same as before: unchanged
    workloads and budgets; their times are recorded for regression context,
    with no new prediction.
  - `ci-linux-stress` about 3 s and `ci-macos-stress` about 3 s of
    execution after the slowest of their platform's eight workers, plus
    scheduling.
  - Overall critical path: expected on a packages worker, now the combined
    invocation alone; the first remote run measures it (see First remote
    run). Hosted stress worker rebalance times: pending (the implementation
    pull request's first remote run), not passed.

- Matched base/change observation with iteration 10b (code-review round 2,
  W1), Linux, go1.26.4 linux/amd64 on the same 16-thread developer
  workstation, warm build cache, 2026-10-03, unpinned. One alternating pair
  per stage: an export of `7f2e534` (the branch base), then the change tree,
  each command alone and strictly sequential (never overlapping another
  test), load average 1.2 to 1.8 before each. Local execution evidence
  only, not a hosted estimate or qualification. One pair cannot separate
  small deltas from runner variance; the misses below are allocation
  misses to investigate, not gates. All assertions, cases, selectors,
  counts and timeouts are unchanged and every run passed.
  - `stress-packages` command 225.4 s base / 287.4 s change. The base
    figure includes compiling the export cold (a new directory gives new
    action IDs); the change tree was warm, so the delta understates growth.
    The `stress packages` step went from 92.8 s to 135.3 s (+42.5 s against
    the 20 s allowance). The new binaries are taskworkspace 127.9 s and
    taskpublication 131.7 s (45 s allocation each). They run beside the
    existing binaries, which slowed with them: testkit 44.7 to 71.0 s,
    fakeadapter 82.3 to 88.6 s, gittransport 32.7 to 35.0 s, client 79.8 to
    88.4 s, adapter 60.0 to 69.5 s, mcp 57.8 to 64.7 s and workspacetransfer
    (whose tests also grew) 87.4 to 119.4 s. The contract shard went from
    42.2/42.0/42.1 s to 62.1/62.0/61.7 s at cpu1/2/4, and the workspace
    shard from 45.8/43.9/43.9 s to 50.2/48.2/48.0 s (+4.1 to +4.4 s, inside
    the 5 s per-CPU allowance). The mcpqual shard is unchanged (38.7/38.1/
    38.3 s to 38.6/38.1/38.0 s).
  - `bench` command 70.9 s base / 115.3 s change. The new `bench
    taskworkspace` binary takes 51.4 s against the 10 s allocation. Every
    existing bench binary is within 0.4 s of base: gittransport 17.0/16.8 s,
    plane 4.3/4.0 s, adapter 1.3/1.5 s, mcpqual 1.5/1.5 s, workspace
    17.6/17.5 s and workspacetransfer 19.0/18.8 s.
  - `stress-sidecar-cpu1` binary 186.5 s base / 206.6 s change (step 188.8
    s / 207.1 s, command 188.9 s / 207.1 s). The +20.2 s delta matches the
    20 s sidecar growth allowance. The change misses the 193.2 s CPU1
    planning target by 13.4 s. That target came from the hosted 173.2 s
    maximum, and the unchanged base already takes 186.5 s on this host, so
    the absolute miss mostly reflects the host. The matched delta is the
    comparable figure.
  - This replaces the round-1 note that matched observations against
    `7f2e534` could not be taken here. Hosted matched pairs remain pending.
  - The change side of these pairs was measured before the round-2 plane
    fix. That fix holds a node fetch that overtakes its own start reply.
    It adds one subtest of about 0.3 s (`TestPlaneWiring/reply-overtaken`)
    to the taskpublication binary and nothing to the bench or sidecar
    binaries.

- Measured with iteration 10b (workspace execution, code-review round 1
  fixes), Linux, go1.26.4 linux/amd64 on the 16-thread developer
  workstation, warm build cache, 2026-10-03, one complete sequential pass of
  the Linux stages, unpinned (the sandbox refuses `taskset` and running
  another tree). Local execution evidence only, not a hosted estimate or
  qualification. Every figure below is an allocation miss pending
  investigation, not a gate. All assertions, counts and timeouts are
  unchanged and every run passed.
  - `bench taskworkspace`: binary 51.7 s (51.5 s in a standalone run)
    against the 10 s allocation. Before the fixes the same host measured
    53.7 s with a publication benchmark that never reached the production
    plane. The step now also publishes through a real in-process plane (its
    per-task writer, receipt and the worker's own task storage) at history 1
    and 8 (about 10.5 s of that step, plane startups and seeding included).
    It saved about 12 s of fixture cost while keeping every case, metric and
    assertion: the warm runs copy a cache warmed once per process instead of
    a cold preparation per run, the snapshot cases and the 100/10,000-path
    metadata hubs are prepared once for the N=1 probe and the timed run, and
    each publication copies a checkout prepared once over the node route.
    The rest is the timed operations themselves (cold preparations of
    1.7 s/op and warm ones of 0.75 s/op at four runs each, history 1 and 8).
    The 10 s allocation cannot hold them without dropping cases: a design
    revision of the allocation, not a weaker benchmark.
  - `stress packages`: taskworkspace binary 127.7 s and taskpublication
    132.7 s inside the 133.5 s command (the review's run measured 152.5 s
    and 134.8 s), against 45 s each. The race CPU1 repetition of
    taskworkspace fell from 3.47 s to 2.95 s: TestPublish publishes from a
    copy of one sealed template per run instead of 19 fresh preparations
    (2.33 s to 0.58 s). The remainder is TestMetadata, TestCache and
    TestSnapshot over real TLS hubs and real syncs, whose cases are the
    UT-B3/B5/B7 matrices.
  - `stress sidecar cpu1`: binary 202.3 s, command 202.8 s, against the
    193.2 s planning target (the review measured 214.8 s). Matched
    alternating observations against `7f2e534` could not be taken in this
    sandbox and remain for hosted runners. `stress sidecar` cpu2/cpu4:
    133.1 s and 99.8 s (commands 133.6 s and 100.3 s).
  - `stress plane cpu1`: binary 172.9 s, command 173.3 s, inside the
    213.9 s observed maximum (no plane test was added or changed in
    repeated workloads). cpu2/cpu4: 137.4 s and 113.2 s.
  - The nine workspace function parents run in 16.9 s once (`-run
    TestTaskWorkspace`). Twenty repetitions in one combined selector (about
    340 to 390 s) exceed a 360 s binary timeout. CI has no such repeated
    selector, and no case was dropped. The heaviest subtests are the real
    child scenarios that the design lets pay a grace (about 1 s each).

- Measured with iteration 10a (prompt readiness and group completion):
  Linux, go1.26.4 linux/amd64 on the same 16-thread developer workstation,
  warm build cache, 2026-10-02, every command pinned to 3 cores with
  `taskset -c 0-2`, strictly sequential and alternating with an export of
  landed 09b (`48078df`), both trees with `GOFLAGS=-buildvcs=false`. Local
  execution evidence only, not a hosted estimate or qualification:
  - `stress sidecar-cpu1`: binary 171.3 s and 172.3 s (command 171.8 s and
    172.7 s) against 161.4 s and 164.4 s (161.9 s and 164.8 s) for
    `48078df`: +9.9 s and +7.9 s, over the zero-net 10a target.
    Investigated: `TestTaskExecutionContract/process` no longer pays its
    two one-second natural cleanups (2.43 s to 0.48 s per repetition under
    `-race -cpu=1`), but that test is not the CPU1 binary's critical path
    on 3 cores, whose parallel phase is CPU-bound, so the structural saving
    does not show. The growth is the new unit tests' fixtures (about 5 s:
    the prompt-readiness role runs, the in-process guardian rigs with their
    durable owner records, the cleanup-diagnostics task run) and the
    immediate reports' extra heartbeat frames and waits in the existing
    sidecar tests (about 4 s: 165.3 s with the new tests skipped). A first
    version measured 173.9 s and 174.1 s; folding two prompt-readiness
    role runs and two diagnostics task runs into existing ones cut about
    2 to 4 s. No assertion or scenario was removed.
  - `stress plane-cpu1`: binary 167.8 s and 166.9 s (command 168.2 s and
    167.3 s) against 168.5 s and 167.7 s (168.9 s and 168.1 s): unchanged,
    as planned (no plane test is added or changed).
  - The FP latencies the new function tests observed (20 plain and 20
    race runs each, unpinned): ready visibility 10.4 to 12.7 ms after the
    sidecar's `role ready` record (bound 500 ms; the first serial ShowRole
    precedes the report's arrival and the next, 10 ms later, sees it); the
    adapter's exit status to proven group absence 0.27 to 0.64 ms for an
    immediate exit (bound 750 ms), 5.4 to 5.9 ms with a cooperative
    descendant, 0.27 to 5.8 ms for a cancelled group, and the full grace
    (1.0006 to 1.0059 s) for a TERM-resistant descendant, fork/exit churn
    and an injected unknown observation. Against `48078df`'s session and
    guardian code the same tests fail: 4.99 s and 1.0017 s.
  - Hosted iteration 10a times: pending (see First remote run).

- Measured with iteration 09b (workspace local transfers): Linux, go1.26.4
  linux/amd64 on a 16-thread developer workstation, warm build cache,
  2026-10-01, each command alone and strictly sequential (never
  overlapping), alternating with an export of landed 09a (`631cbc0`).
  Local execution evidence only, not a hosted estimate or qualification:
  - `stress packages` 168.2 s and 170.7 s against 162.3 s and 165.2 s for
    09a (+5.9 s and +5.4 s; allocation 15 s Linux). The slowest binary is
    still `internal/workspace` (167.5 s and 170.0 s against 157.3 s and
    164.6 s, unchanged tests running beside one more binary); the new
    `internal/workspacetransfer` binary 63.4 s and 64.2 s in the stage.
  - `tests/function` 86.6 s plain and 98.1 s under the race detector
    against 92.2 s and 95.3 s for 09a (race +2.8 s; allocation 10 s),
    within the 180 s bound; the ten 09b parents take 2.5 s together when
    run alone.
  - Transfer benchmarks (`bench workspacetransfer`): 18.8 s against the
    10 s Linux allocation, **over the allocation**. The measured
    operations alone take about 12.8 s at `-benchtime=3x`; about 8.6 s of
    that is the two parent-history snapshot cases, whose mandatory
    full-history fetch of the 1,024 × 4 KiB fixture is served by the 09a
    upload-pack handler, where go-git's delta search takes about 63% of
    the benchmark's CPU. A first version that used `b.N` loops ran each
    setup twice (the one-iteration probe, then three) and took 25.9 s;
    the benchmarks now use `b.Loop`. No case, fixture size or assertion
    was reduced; a further cut needs a design decision (a smaller history
    fixture or a 09a pack-window change).
  - After the code review r1 fixes (same host, 2026-10-01, both trees
    run with `GOFLAGS=-buildvcs=false` because an empty `/tmp/.git` stub
    on this host breaks Go's VCS stamping of the baseline's helper
    builds): `stress packages` 170 s and 172 s against 158 s and 163 s for
    09a (+12 s and +9 s; allocation 15 s), the slowest binary still
    `internal/workspace` (169.2 s and 170.7 s against 157.9 s and
    162.5 s), `internal/workspacetransfer` 73.5 s and 74.1 s in the stage
    (its new regression matrices follow the sampling convention); the
    transfer benchmarks 18.8 s, unchanged.
  - After the code review r5 fixes (same host, 2026-10-01, both trees with
    `GOFLAGS=-buildvcs=false`), including the Git-oracle tables:
    `stress packages` 176 s and 182 s against 165 s and 171 s for 09a
    (+11 s and +11 s; allocation 15 s), the slowest binary still
    `internal/workspace` (174.9 s and 181.0 s against 163.9 s and
    170.6 s), `internal/workspacetransfer` 110.2 s and 115.0 s in the
    stage (the oracle tests sample their rows across repetitions).
  - After the code review r6 fixes (same host and settings), with the
    grammar and repository-state oracles: `stress packages` 170 s and
    173 s against 163 s and 161 s for 09a (+7 s and +12 s; allocation
    15 s), the slowest binary still `internal/workspace` (169.3 s and
    172.0 s against 161.7 s and 160.4 s), `internal/workspacetransfer`
    111.5 s and 111.7 s in the stage.
  - After the ready-probe pipe fix (flakes B and C, 2026-10-02, same host
    and settings, but every command pinned to 3 cores with
    `taskset -c 0-2` to approximate the 3-core macOS runner, strictly
    sequential, against an export of `4e4c12d`): `internal/adapter`
    under `-race -count=20 -cpu=1,2,4` 50.8 s against 31.0 s (+19.8 s),
    and 55.3 s against 37.3 s inside `stress packages` (+18.1 s); the
    whole `stress packages` stage 319.2 s against 315.0 s (+4.2 s), its
    slowest binary still `internal/workspace` (144.1 s and 141.2 s). The
    growth is `TestAdapterContract/saturated`, which runs on every
    repetition because it is timing-dependent: each repetition starts a
    fresh test process that keeps 16 busy goroutines on two Ps through
    one real probe (about 0.25 s plain, 0.30 s under the race detector,
    with the race runtime's exit sleep disabled). 16 goroutines
    reproduced the pre-fix false "left its output open" in 47 of 50
    plain and 16 of 50 race runs on these 3 cores, more often and at
    half the cost of 32 (37 and 7 of 50) or 64. The first hosted run
    with the fix used 64 goroutines and timed out at the 6-minute limit
    in `ci-macos-stress-packages` (run 36929701957; the adapter binary
    took 44.5 s there before the fix, run 36889659609). On these 3-core
    measurements the adapter binary grew by about 18–20 s while the
    stage grew by 4.2 s, within the 09b allocation; the owner accepts the
    added adapter cost. The hosted macOS figure stays pending until
    observed.
  - Hosted iteration 09b times: pending (see First remote run).

- Measured with iteration 08 (real adapters): Linux, go1.26.4 linux/amd64
  on the same 16-thread developer workstation as 07b below, warm build
  cache, 2026-09-30. Local execution evidence only, not a hosted estimate
  or qualification:
  - `tests/function` 77.4 s plain and 89.3 s under the race detector
    (92 s wall) within its 180 s bound, against 68.5 s and 79.9 s before
    iteration 08 on the same host. The nine `TestRealAdapter*` parents take
    about 14 s together when run alone, most of it the shared rig's first
    readiness heartbeat and the smoke gate's two stub-backed deployments.
    A first version that ran the cancellation on the shared worker, waiting
    out the node's post-cleanup heartbeat in a sequential parent, took 27 s.
  - Hosted iteration 08 times: pending (see First remote run).

- Measured with iteration 07b (coordinator setup and timeout
  qualification, after code review r1): Linux, go1.26.4 linux/amd64 on the
  same 16-thread developer workstation as 07a below, warm build cache,
  2026-09-29, each stage alone and strictly sequential (never overlapping),
  alternating with an export of landed 07a (`70a8a0e`). Local execution
  evidence only, not a hosted estimate or qualification:
  - `stress packages` 132.3 s and 130.4 s against 124.7 s and 126.0 s for
    07a (+7.6 s and +4.4 s; allocation 10 s), slowest binary still
    `internal/contract` (131.7 s and 129.8 s against 124.2 s and 125.5 s);
    the new `internal/mcpqual` binary 76.3 s and 74.3 s in the stage,
    about 55 s alone, with every test in its race builds. An earlier
    version that left its sequential tests out of race builds took 30 s
    in the stage; the review rejected that exclusion, and the fixtures were
    made cheaper instead (shared read-only fake runs, direct validation of
    schema tables, linked read-only evidence).
  - `tests/function`: the eight 07b parents take about 6 s plain and 7 s
    under the race detector when run alone, including the two one-second
    cleanup graces, within the 180 s bound. Their first version
    re-executed the race-built function test binary as the fake vendor and
    took 65 s under the race detector; the fake is now the small
    uninstrumented `testdata/fakevendor` command.
  - Hosted iteration 07b times: pending (see First remote run).

- Measured with iteration 07a (coordinator door): Linux, go1.26.4
  linux/amd64 on the same 16-thread developer workstation as 06b below,
  warm build cache, 2026-09-28, `devcheck stress` (all seven shards in one
  process, 14 min 52 s) and then `devcheck stress-packages` again after
  the change below. Local execution evidence only, not a hosted estimate
  or qualification:
  - `stress packages` 124.3 s (06b: 122.4 s), slowest binary still
    `internal/contract` (123.7 s); the new `internal/mcp` binary 42.2 s.
    A first version whose unit suite also crossed the 10 MiB maximum log
    under the race detector took 167.0 s for `internal/mcp` alone (stage
    167.6 s); that one size case is now `//go:build !race` (see the
    packages bullet above).
  - Unchanged shards: `stress plane cpu1` 157.0 s (06b: 158.8 s) and
    `stress sidecar cpu1` 159.4 s (160.3 s); `stress plane cpu2` 125.3 s
    and `cpu4` 100.1 s, `stress sidecar cpu2` 101.3 s and `cpu4` 85.4 s;
    `stress processgroup` 71.1–71.4 s per CPU setting; `stress function`
    30.7 s, `stress plane function` 70.9 s, `stress node function` 8.0 s.
  - `tests/function` 62.5 s (race 75.1 s) within its 180 s bound (45.0 s
    plain before 07a on the same host). The eight `TestMCP*` parents take
    16.7 s plain and 21.0 s under the race detector; their
    deliberate waiting is about 5.5 s (`budget-deadline` 0.5 s,
    `own-deadline` 0.75 s, `delivery-deadline` 1 s, `stalled-reader` 1 s,
    `slow-reader-max-logs` 2.4 s paced; `wait-slow` and `outstanding-wait`
    do not wait), plus the plane's own 6 s forced-removal budget in
    `TestMCPRoles/force-pending`.
  - Hosted iteration 07a times: pending (see First remote run).

- Measured with iteration 06b (task controls): Linux, go1.26.4
  linux/amd64 on the same 16-thread developer workstation as 06a-perf
  below, warm build cache, 2026-09-28, each shard stage run alone, one after
  another (`devcheck` outcome lines). Local execution evidence only, not a
  hosted estimate or qualification:
  - `stress plane cpu1` 158.8 s (06a-perf: 135.3 s, +23.5 s; the Linux
    screening allowance was +42.0 s); `stress sidecar cpu1` 160.3 s
    (146.5 s, +13.8 s; allowance +38.4 s).
  - `stress plane cpu2` 126.3 s and `cpu4` 100.4 s, concurrently (116.3 s
    and 97.2 s); `stress sidecar cpu2` 109.7 s and `cpu4` 90.7 s,
    concurrently (99.6 s and 83.0 s).
  - `stress packages` 122.4 s; `stress processgroup` 69.9 s per CPU
    setting; `stress function` 33.0 s, `stress plane function` 72.2 s,
    `stress node function` 8.4 s (unchanged workloads).
  - `tests/function` 49.1 s (race 54.1 s) within its 180 s bound; the
    native `resistant` scenario takes about 5 s longer (its timeout).
  - Hosted iteration 06b times: pending (the pull request's first remote
    run; see First remote run for the CPU1 split trigger).

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
| `cmd/mcpqual/main.go` | `run` | `return runFor(runtime.GOOS, runtime.GOARCH, args, getenv, stdin, stdout, stderr)` |

A missing or renamed wrapper fails the guard, so moving one is an intentional
policy change. Syscall exceptions are the six build-selected native files
(iteration 09b added the two transfer publication files), exempt from the
wrapper policy only while they keep their build expressions:

- `internal/spikes/processgroup/sys_linux.go` (`linux`): the child subreaper
  and `Wait4` reaping.
- `internal/spikes/processgroup/sys_darwin.go` (`darwin`): the lifetime pipe
  and ESRCH polling while launchd reaps orphans.
- `internal/testkit/fakeadapter/signals_unix.go` (`linux || darwin`): OS
  signal registration and names.
- `internal/testkit/fakeadapter/signals_other.go` (`!linux && !darwin`): the
  generic unsupported fallback, outside supported-platform runtime
  qualification.
- `internal/workspacetransfer/publish_linux.go` (`linux`): the folder
  export's no-replace publication (`renameat2` with `RENAME_NOREPLACE`) and
  `fsync`.
- `internal/workspacetransfer/publish_darwin.go` (`darwin`): the same
  publication through `renameatx_np` with `RENAME_EXCL`, and `F_FULLFSYNC`
  with an `fsync` fallback.

Exempt files are not scanned for `runtime.GOOS`, so
`internal/testkit/fakeadapter/signals_unix.go` must not grow a host branch:
it is compiled for both `linux` and `darwin`, and such a branch would be an
untested decision the guard cannot see.

Iteration 04 adds no host OS read and no exemption: the guard's then five
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

Iteration 07b adds exactly one wrapper and no exemption: the developer-only
qualification command's `run` in `cmd/mcpqual/main.go` forwards
`runtime.GOOS` and `runtime.GOARCH` to `runFor`, which selects the
linux/darwin cleanup policy (`mcpqual.PolicyFor`, the same TERM, 1 s grace,
KILL and 5 s ESRCH proof on both, EPERM never absence) and records the
host platform in its report; every decision below it takes `goos`
explicitly and is unit-tested for both systems. Its process-group code is
build-selected (`internal/mcpqual/proc_unix.go` and
`internal/mcpqual/procexec/exec_unix.go`, `linux || darwin`, with their
`_other.go` counterparts) without a host read, so it needs no exemption. The
developer fault variable `MCPQUAL_TEST_FAULT` is read only by that entrypoint
and never by `callsheet`.

Iteration 08 adds no wrapper and no exemption. The sidecar's macOS warning
for enabled Claude/Codex adapters is decided by `RunOptions.GOOS` (fed by
`cli.Run`'s wrapper) and unit-tested for both values; the Codex final-file
reader (`openat` relative to a retained no-follow directory descriptor,
`O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC`, `fstat` regular file with one link) is
build-selected in `internal/sidecar/finalfile_unix.go` (`linux || darwin`,
the existing `golang.org/x/sys/unix` dependency) with the refusing
`internal/sidecar/finalfile_other.go` (`!linux && !darwin`), and makes no
host decision. `internal/adapter`'s Claude and Codex adapters import only
`internal/contract`, and the replay stub lives under `testdata`.

Iteration 09b adds no wrapper and two exemptions, the build-selected
publication files listed above; they hold only the native no-replace rename
and file sync and make no host decision. Every other transfer decision that
depends on the host (darwin's `core.precomposeUnicode` default and NFC
directory names, the `.git` HFS aliases, the 4095/1023-byte path limits and
the publication failure wording) takes the `GOOS` of `workspacetransfer.Options`,
fed by `cli.Run`'s wrapper through the CLI leaves and `callsheet mcp`, and is
unit-tested for both values on every host. The rest of the descriptor-relative
filesystem code uses `golang.org/x/sys/unix` calls common to both systems.
The folder export and the transfers do not build for other systems: the
CLI's platform rejection already refuses them, and `devcheck cross` builds
only linux and darwin.

Iteration 10a adds no wrapper and no exemption. The task guardian's early
group completion uses two build-selected native primitives, selected by
build constraints alone (the guardian entrypoint receives no `goos`, and
neither file reads `runtime.GOOS`, so the guard scans both like any other
file and its six wrappers and six exemptions are unchanged):

- `internal/sidecar/group_alone_linux.go` (`linux`): the child subreaper
  (`unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)`, installed before
  the adapter starts; the behavior of the spike's `setSubreaper`, not a
  call to it) and the reap-and-probe loop
  (`unix.Wait4(-1, &status, unix.WNOHANG, nil)` after the adapter's own
  `Wait` joined): only `ECHILD` proves the guardian has no task descendant
  left; a live child is busy, `EINTR` is retried a bounded number of times
  and any other error is unknown.
- `internal/sidecar/group_alone_darwin.go` (`darwin`): `proc_listpids`
  through the stdlib's direct trap,
  `syscall.Syscall6(syscall.SYS_PROC_INFO, 1, 2, pgid, 0, buf, 8)` with
  `PROC_INFO_CALL_LISTPIDS` and `PROC_PGRP_ONLY` and a two-entry buffer:
  exactly four bytes naming the guardian prove it is the group's last
  member, eight bytes are busy, anything else (or an errno) is unknown.
  macOS has no subreaper; its `subreap` is a no-op.

Each file's lower-level call (prctl and wait4, or the Syscall6 function) is
injected in its unit tests (`TestGroupAloneLinux`, `TestGroupAloneDarwin`);
an unknown observation, a probe error or a failed subreaper keeps the full
grace and the KILL, and the sidecar's own ESRCH observation remains the
only final absence. The real fork, adoption and group enumeration run in
`TestTaskFastGroupCleanup` on each platform's native runner; both files
are whole-file coverage-manifest entries evaluated on their own OS (see
Checks).

Iteration 10b adds no wrapper and no exemption. Every host decision of the
workspace execution takes an explicit `goos` (the sidecar's task platform,
the checkout's and snapshot's alias, path-limit and physical-path rules,
the test harness's worker); nothing new reads `runtime.GOOS`. The task
object database's batched durability is build-selected inside the two
existing exempt publication files, as `syncBatch` next to `fsyncFD`:

- `internal/workspacetransfer/publish_linux.go` (`linux`): one
  `syncfs(2)` of the database's filesystem makes a batch of new loose
  objects, their fan-out directories and the objects directory durable
  together.
- `internal/workspacetransfer/publish_darwin.go` (`darwin`): a plain
  `fsync(2)` of each new object and directory (which on darwin does not
  flush the drive's cache), then one `F_FULLFSYNC` for all of them.

The seam's fallback (each path synced through `syncFD`) is unit-tested on
every host; the batch runs before any checkpoint or push depends on the
objects. The task checkout itself is never synced: its work directory is
not recovery evidence (a crash before publication discards it).

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
  sixteen worker contexts are not required (six since iteration 02c, the
  two plane workers since iteration 05b, the two sidecar workers since its
  sidecar follow-up, the four CPU1 workers since iteration 06a-perf and the
  two packages-cpu workers since the stress worker rebalance; the
  summaries gate on them), so no protection change is needed for 02c, 05b,
  its sidecar follow-up, 06a-perf or the stress worker rebalance.
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

### packages-cpu workers (stress worker rebalance)

The stress worker rebalance (design stress-rebalance, 2026-10-07) adds the
workers `ci-linux-stress-packages-cpu` and `ci-macos-stress-packages-cpu`;
both summaries now take eight results. The new workers are diagnostic
checks, not required contexts. The required contexts stay exactly
`ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`, so no
protection change is made:

1. Finish the flow's code review of the stress worker rebalance (`REVIEW_APPROVED`) and commit the reviewed code on `stress-rebalance`.
2. Push `stress-rebalance` and open its pull request targeting `main`.
3. Observe all twenty jobs report success on the current PR merge revision: the sixteen stress workers and all four required checks.
4. The owner verifies protection with the read-only command above: the same four required contexts, and no packages-cpu worker added as a required context.
5. Merge only with all four checks green on the current merge revision and the first-remote-run evidence recorded (First remote run).

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

Iteration 06b (task controls, milestone M2) is delivered in its own pull
request (branch `iter-06b-task-controls`). It adds no job and changes no
required context: the workflow keeps its eighteen jobs, and the
pre-authorised CPU1 split (Budgets, iteration 06b) is applied only if the
first remote run triggers it.

The stress worker rebalance (the packages-cpu stress workers) is delivered
in its own pull request (branch `stress-rebalance`). It adds two worker
jobs and changes no required context, so it needs no protection change;
the workflow now has twenty jobs, and the pull request merges only after
all twenty jobs, and so all four checks, are green on its current merge
revision (Branch protection, packages-cpu workers).

## First remote run

Local checks cannot prove runner provisioning, Go and action download or cache
behavior on GitHub, Darwin runtime results or repository protection. Those
remain pending until observed. After pushing, the owner records in the flow
handoff:

- the run URL and the commit it ran;
- the conclusions of all twenty jobs: all four checks, `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`, and the sixteen stress workers;
- native evidence from the `ci-macos` log: the line `devcheck: native qualification passed on darwin/<arch>` naming `TestFP6ProcessGroups` and its three scenarios;
- stress evidence from the sixteen worker logs (the summaries hold none): the lines `devcheck: stage stress-packages ok`, `devcheck: stage stress-packages-cpu ok`, `devcheck: stage stress-plane-cpu1 ok`, `devcheck: stage stress-plane ok`, `devcheck: stage stress-sidecar-cpu1 ok`, `devcheck: stage stress-sidecar ok`, `devcheck: stage stress-processgroup ok` and `devcheck: stage stress-functions ok` on each platform, the `-count=20` commands, every CPU invocation's outcome, and the elapsed time of each stress command with the runner's OS, architecture and cache state;
- the actual job and step times of all twenty jobs, setup, queue and summary wait time included, and the overall workflow critical path; compare each worker with the expected per-job wall-clock in Stress checks, and diagnose any miss of the 4–5 minute goal and the remaining bottleneck without weakening tests or reducing counts;
- the branch protection verification described above (finishing the conditional 02b prerequisite first if it is needed);
- for iteration 03, native evidence for the 30 node names (see Checks) on `ci-macos`, the `stress node function` step and the enlarged `stress packages` step on both platforms with their times, compared with the iteration 03 allocation in Budgets;
- for iteration 04, native evidence for the 29 role names (see Checks) on `ci-macos`, the `stress packages` step with `internal/adapter` and the `internal/plane`, `internal/sidecar` and `internal/adapter` binary times on both platforms, the function package time on both platforms, and the before/after job and command durations, compared with the iteration 04 allocation in Budgets. Hosted evidence pending at local review remains pending, not passed;
- for iteration 05b, from both plane workers, each `devcheck: stress plane cpuN: ok in Xs` (or `FAILED after Xs`) line and each plane binary's own time separately from its go command's build, evaluated against the pre-authorised plane fallback trigger (X > 300.0 s, or a plane binary timeout in the CPU-labelled replay); from both packages workers, the `internal/sidecar` and `internal/contract` binary times, the sidecar line evaluated against the sidecar follow-up rule; setup, summary wait, the runner's core count and architecture, and the critical path, compared with the iteration 05b estimates in Budgets. Collect one complete successful run per platform; failed runs stay in the evidence, never discarded as retries. Values above the estimated ranges need a documented explanation or revised estimate, and any timeout or assertion failure blocks qualification.
- for the iteration 05b sidecar follow-up, from both sidecar workers, each `devcheck: stress sidecar cpuN: ok in Xs` (or `FAILED after Xs`) line and each sidecar binary's own time separately from its go command's build, with every process, cleanup and timing assertion passing unchanged; from both packages workers, the remaining binaries' times; setup, summary wait, the runner's core count and architecture, and the critical path, compared with the sidecar follow-up estimates in Budgets. Any timeout or assertion failure blocks qualification, and no further concurrency escalation or timing relaxation is authorised.
- for iteration 06a-perf, the owner's pre-decided post-push qualification (not the local code-review bar): record all eighteen job conclusions and step and job times, setup and queue costs, the runner's architecture and core count, and every plane and sidecar CPU-labelled `devcheck` outcome. Use `devcheck: stress <package> cpu1: ok in Xs` as each CPU 1 invocation's elapsed value (it includes the command's build), and keep the binary's own time separately. Evaluate all four CPU1 invocations, Linux and macOS, plane and sidecar. If every CPU1 invocation passes and is ≤300.0 seconds, and all ordinary correctness, coverage and CI gates pass, this slice is done. A value above 250 but at or below 300 succeeds under the owner's rule; record that the aspirational target was missed. Otherwise report the measured values and failures to the owner. Missing, cancelled or timed-out invocations do not count as passes. In that case make no automatic further change to topology, waves, flags, counts, timeouts, workload tests or fixtures. Do not discard a failed first run by retrying until green. The ≤250-second macOS CPU1 goal is a first-run hypothesis, not an additional acceptance gate. There is no two-run requirement. All CPU 2 and CPU 4 invocations and every other job must still pass their unchanged gates, and until that run exists hosted qualification is pending, not passed.
- for iteration 06b (owner decision on DW6, 2026-09-28: build with the current matrix and measure on the pull request): native evidence for the four task-control parents (145 names) on `ci-macos`, and all four CPU1 invocations' `devcheck: stress <package> cpu1: ok in Xs` values, Linux and macOS, plane and sidecar, against the screening estimates in Budgets. A value strictly greater than 300.0 s, or that invocation's timeout, triggers the pre-authorised CPU1 split for that package on both platforms, through the light flow (a fix brief preserving the triggering log, implementation and review), without another design round; exactly 300.0 s does not trigger. An assertion failure is a correctness failure, never cured by splitting. Until that run exists hosted qualification is pending, not passed.
- for iteration 07a: native evidence for the eight MCP parents and their 69 mandatory subtests (222 names) on `ci-macos`, notably `TestMCPLifetime/closed-stdout` (exit 5, not signaled, on Darwin), `stalled-reader` and `slow-reader-max-logs`; the `stress packages` step with `internal/mcp` and its binary time on both platforms; the `bench mcp` step; the `tests/function` package time in the normal and race invocations against its 180 s bound; and the unchanged plane and sidecar CPU1 invocation times. Until that run exists hosted qualification is pending, not passed.
- for iteration 08: native evidence for the nine real-adapter parents and their 30 mandatory subtests (312 names) on `ci-macos`, and for `TestRealAdapterLocal` and its five subtests in `internal/sidecar` from the single tagged native stream (no duplicate package start, no skip event from this iteration, no `tests/smoke` package); the tagged `test`, `test -race` and `bench sidecar realadaptercheck` steps and the tagged coverage profile on `ci-linux`; the `tests/function` package time in the normal, race and native invocations against its unchanged 180 s bound; and the `stress packages` step with the grown `internal/adapter` binary on both platforms. No vendor CLI or model is involved; M3's acceptance is the Linux container gate described in [real adapters](real-adapters.md), not this iteration's record.
- for iteration 11: native evidence for the nine wave-2 parents and their 33 mandatory subtests (395 names) on `ci-macos`, and for `TestRealAdapterLocal` with its four `wave2-*` subtests (12 sidecar names) in `internal/sidecar` from the single tagged native stream; on Linux the Grok replay parents, the tagged `test`, `test -race` and `bench sidecar realadaptercheck` steps, the tagged coverage profile and the extended `BenchmarkVendorFinal` and `BenchmarkVendorInvocation`; the `tests/function` package time against its unchanged bound and the `stress packages` step with the grown `internal/adapter` binary on both platforms, investigated within the existing budgets. No vendor CLI, credential or model is involved, and the opt-in wave-2 smoke is never run by CI.
- for iteration 07b: native evidence for the eight setup and qualification parents and their 43 mandatory subtests (273 names) on `ci-macos`, notably `TestMCPQualificationReaping/parent-exits-first` (a surviving descendant, then ESRCH, on Darwin) and `cleanup-failure`; the `stress packages` step with `internal/mcpqual` and its binary time on both platforms against the 10 s (Linux) and 15 s (macOS) allocation; the `bench mcpqual` step; the `tests/function` package time in the normal, race and native invocations against its 180 s bound and the 10 s / 15 s allocation; and the unchanged plane and sidecar CPU1 invocation times, with OS, architecture and cache state. Until that run exists hosted qualification is pending, not passed.
- for iteration 10a: native evidence for `TestTaskPromptReadiness` and `TestTaskFastGroupCleanup` (335 names) on `ci-macos`, where the second exercises the process-group list proof (`proc_listpids`) with real guardians, descendants and fork/exit churn, and on `ci-linux` the child subreaper; each FP latency the tests log (ready visibility under 500 ms, exit status to proven absence under 750 ms) on both hosts; and all four CPU1 invocations' binary and command times (Linux and macOS, plane and sidecar) against run 37022060367 and the 10a planning targets in Budgets, recorded without a numerical gate. Until that run exists hosted qualification is pending, not passed.
- for iteration 10b: native evidence for the nine `TestTaskWorkspace*` parents and the three acceptance scenarios `TestTaskWorkspaceCommit/AC-WS-1`, `TestTaskWorkspacePublication/AC-WS-5` and `TestTaskWorkspaceIsolation/AC-WS-2` (347 names) on `ci-macos` (APFS aliases, the physical work path, darwin's batched `F_FULLFSYNC` durability and the native group proof) and `ci-linux`; the per-file coverage of every 10b manifest entry on both hosts; the `bench taskworkspace` step on both hosts; and the stress, function and benchmark times against the 10b planning allowances in Budgets, recorded without a numerical gate. Until that run exists hosted qualification is pending, not passed.
- for iteration 10c: native evidence for the six coordinator delivery parents `TestWorkspaceDispatchDoors`, `TestWorkspaceTaskPull`, `TestWorkspaceTaskInspect`, `TestWorkspaceTaskMCP`, `TestWorkspaceMultiHop` and `TestWorkspaceOperatorWorkflow` (353 names) on `ci-macos` and `ci-linux`; the per-file coverage of every 10c manifest entry on both hosts; the unchanged bench call count (30, seven on the native tail) with the extended `BenchmarkMCPCodec` (Linux), `BenchmarkTransferPullGit` and `BenchmarkTaskWorkspaceMetadata` (both hosts); and the `stress packages` and function binary and command times against landed 10b and the 10c planning allowances in Budgets, recorded without a numerical gate. The M4 checks named in [workspaces.md](workspaces.md#manual-m4-checks) are gated by the container acceptance, not by a two-machine session. Until that run exists hosted qualification is pending, not passed.
- for the stress worker rebalance (design stress-rebalance, delivery gate; hosted evidence pending at local code review remains pending, not passed, and is outside the implementation acceptance bar): collect the entire successful run of the implementation pull request on both platforms: every stage's elapsed time (`devcheck: stage stress-packages ok` and `devcheck: stage stress-packages-cpu ok` with the `devcheck: stress packages: ok in Xs` and per-CPU outcome lines), the combined command's time and each of its nine binaries' times, all nine per-CPU invocation times (`devcheck: stress contract cpuN: ok in Xs`, `devcheck: stress mcpqual cpuN: ok in Xs`, `devcheck: stress workspace cpuN: ok in Xs`) and each group's maximum, job setup duration, the setup-go build cache state, the runner's architecture and core count, both summary outcomes and the workflow critical path. Preserve failed runs too; they stay in the evidence, never discarded as retries. Record the results in this document with projections distinguished from measurements, comparing each affected stage (`stress-packages` and `stress-packages-cpu`, Linux and macOS) to the 675-second review target and each binary to its 360-second limit; any timeout or assertion failure blocks qualification. A green fast-runner pull request validates execution but does not establish a slow-runner upper bound; record that uncertainty explicitly. A material measured overrun of the target needs owner disposition or a further design revision, never a raised timeout, a reduced count or a weakened test. Record the other workers' times for regression context. Until that run exists hosted qualification is pending, not passed.
- for m3-m4-container-e2e: the `ci-linux` run whose `devcheck test` passes with the container acceptance (its revision, the run URL and the published `Publish container E2E evidence` report with all 14 case records and the end record) is the first M3/M4 demonstration; until it is observed M3/M4 remain undemonstrated. Record the cold build and container elapsed times printed by the stage and the complete `ci-linux` job time against its unchanged 45-minute limit; if the added ten-minute allocation does not fit in practice, report the blocker instead of moving or weakening tests.

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
go test -count=1 -run '^TestControl(Cancellation|ExecutionTimeout|BoundedWait|ForceRemove|NativeGroups)$' -v ./tests/function
go test -race -count=20 -cpu=1,2,4 -timeout=6m -run '^TestControl(Cancel|Timeout|Wait|Remove)$' ./internal/plane ./internal/sidecar ./internal/client ./internal/cli ./internal/contract
go test -count=1 -run '^TestRealAdapter' -v ./tests/function
go test -tags=realadaptercheck -count=1 -run '^TestRealAdapterLocal$' -v ./internal/sidecar
```

The real worker smoke is never part of these commands or of CI; it needs
the `realadaptersmoke` tag, installed CLIs and an explicit opt-in (see
[real adapters](real-adapters.md)).

The M3/M4 container acceptance runs on its own on a Linux host with Docker
(it is also part of the Linux `test` stage, once):

```
go run ./cmd/devcheck container-e2e
go run ./cmd/devcheck container-e2e --count=20
```

`--count=N` (1 to 20, accepted only by this stage) builds once and runs N
fresh containers in sequence, each with `-test.count=1 -test.timeout=6m`,
each within its own ten-minute deadline (the first also covers the build),
and stops at the first failed iteration. Every run replaces the report in
`/tmp/callsheet-container-e2e-evidence`; a missing Docker executable fails
with `docker executable not found`, an unreachable daemon with `docker
daemon unavailable`, and neither is ever a skip. Implementers and reviewers
run the count-20 form locally on Linux; CI runs one iteration.

A single shard, as one CI worker runs it, is also available on its own:

```
go run ./cmd/devcheck stress-packages
go run ./cmd/devcheck stress-packages-cpu
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
for CPU 1. `devcheck stress-packages` runs the combined invocation only; add
`devcheck stress-packages-cpu` for the contract, mcpqual and workspace
per-CPU groups (the stress worker rebalance; there is no
`stress-packages-cpu1` stage). The `TestStressConcurrencyContract`
command repeats the concurrent coordinator's timing-dependent contract at
the declared count, for all five concurrent shards, the packages-cpu
groups and the wave mechanism
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
