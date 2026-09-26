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

Exactly four independent jobs, each its own required status check context:

| Check context | Runner | Timeout | Steps after setup |
|---|---|---|---|
| `ci-linux` | `ubuntu-24.04` | 45 min | `devcheck test` (native suite, then the same suite with `-race`), `devcheck coverage` (unit coverage must be greater than 80.0%), `devcheck bench` (git transport payload byte limits and commit/tree invariants, then the plane trust benchmarks: issuance, initialization and verified TLS health, each checking its invariants; timings are reported, never gated), `devcheck cross` (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64: 12 artifacts) |
| `ci-macos` | `macos-15` | 30 min | `devcheck native`: the complete suite as `go test -json`, which must show passing run and pass events in `github.com/wedevwork/callsheet/tests/function` for `TestFP6ProcessGroups` and its `cooperative`, `resistant` and `leader-exits-first` scenarios, and for the plane trust tests (see below) |
| `ci-linux-stress` | `ubuntu-24.04` | 20 min | `devcheck stress` on Linux (see Stress checks) |
| `ci-macos-stress` | `macos-15` | 20 min | `devcheck stress` on Darwin, with the same commands, repeat count and CPU settings as Linux |

The four jobs start together on every trigger and run independently: none
waits for, depends on or is conditional on another (no `needs`, matrix, job
or step condition, path filter, concurrency cancellation or
`continue-on-error`), and each fails on its own. Iteration 02b moved stress
out of the two main jobs into the two stress jobs so that it runs in
parallel with them; the main jobs keep their other stages and their
budgets.

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

All four jobs check out the event's revision without persisted
credentials, take the Go version from `go.mod` with module caching, run
`go mod download`, and then run their check steps with `GOPROXY=off` and
`GOSUMDB=off`. A cold or concurrently populated cache affects speed only,
never correctness. The workflow has read-only repository permission
(`contents: read`), uses no secrets, exchanges no artifacts between jobs and
never changes repository settings.

A missing, skipped or failing qualification test fails `ci-macos` with
`native qualification unobserved` even when `go test` itself exits zero. A
timeout, an unavailable runner or a canceled, skipped or pending check is
unobserved qualification, never a pass. `ci-macos` qualifies the runner's own
CPU architecture only; cross-builds do not prove runtime behavior on the other
Darwin architecture. Coverage and bench remain Linux reference gates; they are
not claimed for Darwin.

## Stress checks

`go run ./cmd/devcheck stress` repeats the timing- and concurrency-sensitive
tests under the race detector with varied parallelism, on Linux and macOS,
in the dedicated jobs `ci-linux-stress` and `ci-macos-stress`.
The project's declared repeat count is 20 per CPU setting (1, 2, 4). The
flow's coder and reviewer use this count when they re-run timing-dependent
tests they add or modify: `devcheck stress` for tests in its covered packages,
and the equivalent `go test -race -count=20 -cpu=1,2,4 -run '<tests>'
<package>` command for changed timing tests outside that set.

The count, CPU list, package groups, selectors and time budgets are declared
once, in `internal/devcheck/stress.go` (`StressCount`, `StressSteps`). The
stage runs three sequential commands (argv, never a shell), `stress
packages`, `stress function` and `stress plane function`, each with
`CGO_ENABLED=1`:

```
go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/processgroup ./internal/spikes/gittransport ./internal/plane
go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function
go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$ ./tests/function
```

These are argv displays, not shell-ready commands: quote the entire `-run`
argument when running one through a shell. The plane selector is one
two-level pattern: `go test` splits a `-run` pattern into per-level patterns
only at a `/` outside parentheses and brackets, so it selects the three
parents at the top level and only the eight named subtests below them. It
must not be "simplified" into a flat alternation, which would select
different tests.

- Selected packages: `internal/testkit`, `internal/testkit/fakeadapter`,
  `internal/spikes/processgroup`, `internal/spikes/gittransport` and
  `internal/plane`, complete package tests (not benchmarks). The fake
  adapter is included because its signal handling and descendant lifecycle
  are timing-sensitive too; the plane package (iteration 02) because its
  listener, shutdown, lock-holder helper process and contracts are, and
  they run there directly under the race detector.
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
  `TestPlaneReissue`.
- `-count=20` applies at each CPU setting: every selected test runs
  60 times per invocation. This is repeated testing, never retry-until-green:
  any failure fails the stage. There is no count override, no lighter macOS
  count and no environment-based bypass.
- `all` does not include stress: `all` stays test, coverage, bench and cross,
  because repeated subprocess builds and process experiments cost minutes.
  Local release and review verification therefore runs both `all` and
  `stress`.
- Stress runs in its own jobs, `ci-linux-stress` and `ci-macos-stress`, with
  identical commands and count on both platforms: a Linux-only stress pass
  cannot qualify Darwin.
- `-race` needs the native C compiler on each runner (cgo). A missing compiler
  is a failed prerequisite, never permission to omit `-race`. Shipped
  cross-built binaries remain CGO-disabled.

Deduplication (iteration 02b): stress repeats nothing that another stress
command already repeats at the declared count. Only these repeated
invocations were removed; each still runs once in the ordinary suites,
`devcheck test` on Linux and `devcheck native` on macOS:

| Removed stress work | Repeated 60 times in `stress packages` by | Still run ordinarily |
|---|---|---|
| `TestFP6ProcessGroups` | `internal/spikes/processgroup` `TestExperiment`: the same `RunExperiment` with the cooperative, resistant and leader-exits-first cases, requiring `rep.Pass()`; the package's evaluation and cleanup tests remain | the whole FP-6 test with all three scenario assertions; still mandatory Darwin native evidence |
| `TestPlaneState`'s delegated native state contract | `internal/plane` `TestNativeStateContract`: modes, no-replace, rename, flock | `TestPlaneState` `contracts` invokes the identical contract with its mandatory evidence list |
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
- The whole stage: a 15-minute watchdog (or the caller's earlier deadline)
  bounds compilation as well as test execution; when it expires the stage
  fails and no further step starts.
- Stress jobs: 20 minutes each, five minutes beyond the unchanged 15-minute
  watchdog for setup. The main jobs keep 45 minutes (`ci-linux`) and
  30 minutes (`ci-macos`). None of these durations is a performance target.
- Target: under 10 minutes for the complete stage on each hosted runner, a
  diagnostic budget. Exceeding the target is recorded, not a failure;
  timeout and watchdog failures remain failures.
- Pull request wall-clock target (iteration 02b): about 5–6 minutes for
  the slowest of the four jobs, down from about 15 minutes. It is an
  optimization target, not a measurement and not a hardware-independent
  pass threshold.
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
- Hosted baseline before iteration 02b (pull request #2's last run, both
  jobs still running stress serially): `ci-linux` 685 s, of which
  stress 576 s; `ci-macos` 889 s, of which stress 806 s. Everything else
  took about 109 s and 83 s.
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
  311.5 s (219.8 s, 32.2 s and 59.5 s). The unchanged packages step is now
  the bottleneck (71% of the stage).
- Expected per-job wall-clock after iteration 02b (estimates, not
  measurements; the main-job figures are the hosted non-stress remainders
  above, the stress-job figures scale the hosted stress times by the local
  Linux reduction, a platform extrapolation for Darwin, and add fresh setup,
  which is not measured separately): `ci-linux` about 109 s and `ci-macos`
  about 83 s (their stages are unchanged apart from the new offline
  tests); `ci-linux-stress` about 325 s of stress (576 s × 309.1/548.0),
  about 5.5 minutes plus setup; `ci-macos-stress` about 455 s of stress
  (806 s × 309.1/548.0), about 7.6 minutes plus setup. The expected
  critical path is therefore `ci-macos-stress` at roughly 8 minutes, down
  from about 15 minutes but above the 5–6 minute target; its remaining
  bottleneck is the unchanged `stress packages` step (locally the
  `internal/plane` and `internal/spikes/processgroup` binaries, about 220 s
  and 200 s).
- Expected macOS stress margin: stress took 806 s of the 900 s watchdog
  before iteration 02b (a 94 s margin). The expected stress time after the
  FP-6 and contract removals is about 455 s (the estimate above), a margin
  of about 445 s, well under the watchdog. macOS: pending
  until the first `ci-macos-stress` run of pull request #2; that run's
  stress time is compared with this figure and recorded in the flow
  handoff (see First remote run).

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
(test) and `ci-linux-stress`, Darwin behavior only by `ci-macos` (native)
and `ci-macos-stress`; host-independent tests prove both decision branches
on any host, and only the next `ci-macos` run proves Darwin runtime behavior
(its repeated stress in the next `ci-macos-stress` run). `runtime.GOARCH`
only labels output and is outside this guard.

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
  expected source when available.
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

Iteration 02b adds the contexts `ci-linux-stress` and `ci-macos-stress`.
GitHub offers a context for selection only after it has reported once, so
the order is fixed:

1. Finish the flow's code review of iterations 02 and 02b (`REVIEW_APPROVED`) and commit the reviewed code on `iter-02-plane-trust`.
2. Push `iter-02-plane-trust`, updating pull request #2.
3. Observe all four jobs, `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`, report success on the current PR merge revision.
4. The owner adds the two new required contexts `ci-linux-stress` and `ci-macos-stress`, only after they have reported, with the additive request below.
5. The owner verifies protection with the read-only command below.
6. Merge iterations 02 and 02b together, only with all four checks green on the current merge revision.

No skipped, canceled, pending or unobserved result qualifies, and a later
push requires fresh current-revision evidence (return to step 3). The pull
request #1 protection handoff remains a prerequisite: the original two
contexts are already required.

For existing classic branch protection with the original two required
checks, the owner runs this exact **owner-only** additive request, never
executed by CI, tests or this flow:

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

Iteration 02b (CI speed) is delivered the same way: iterations 02 and 02b
join pull request #2 (branch `iter-02-plane-trust`) and merge together only
after all four checks are green on its current merge revision and the owner
has added the two stress contexts, in the order given in Branch protection
(Adding the stress contexts).

## First remote run

Local checks cannot prove runner provisioning, Go and action download or cache
behavior on GitHub, Darwin runtime results or repository protection. Those
remain pending until observed. After pushing, the owner records in the flow
handoff:

- the run URL and the commit it ran;
- the conclusions of all four checks, `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`;
- native evidence from the `ci-macos` log: the line `devcheck: native qualification passed on darwin/<arch>` naming `TestFP6ProcessGroups` and its three scenarios;
- stress evidence from the `ci-linux-stress` and `ci-macos-stress` logs: the line `devcheck: stage stress ok`, the `-count=20 -cpu=1,2,4` commands, and the elapsed time of each stress step with the runner's OS, architecture and cache state;
- the actual job and step times of all four jobs, setup and queue time included, and the overall critical path (the slowest job); compare the `ci-macos-stress` stress time with the expected macOS stress margin in Stress checks, and report any miss of the 5–6 minute target and the remaining bottleneck without reducing counts;
- the branch protection verification described above, once applied.

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
go test -count=1 -run '^(TestCI|TestHardening)' ./tests/function
go test -json -count=1 -run '^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$' ./tests/function
```

The last command is the focused evidence for the plane stress selector: its
events must show run and pass for exactly the eight selected subtests and
none for a `contracts` subtest.

`all` runs test (with race on Linux), coverage, bench and cross; `stress` is
explicit (see Stress checks), and release and review verification run both.
`internal/cicheck` and the `TestCI*` and `TestHardening*` function tests
validate the workflow structure, its devcheck stages against the driver's
dispatch, and this page, offline. `go run ./cmd/devcheck native` works on macOS only; on other hosts it
exits 1 with an unsupported-stage error. Optionally, a locally installed
`actionlint .github/workflows/ci.yml` can lint the workflow; it is not a
required dependency and nothing invokes it automatically.
