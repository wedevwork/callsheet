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

Exactly two jobs, each its own required status check context:

| Check context | Runner | Timeout | Steps after setup |
|---|---|---|---|
| `ci-linux` | `ubuntu-24.04` | 45 min | `devcheck test` (native suite, then the same suite with `-race`), `devcheck coverage` (unit coverage must be greater than 80.0%), `devcheck bench` (git transport payload byte limits and commit/tree invariants; timings are reported, never gated), `devcheck cross` (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64: 12 artifacts), then `devcheck stress` (see Stress checks) |
| `ci-macos` | `macos-15` | 30 min | `devcheck native`: the complete suite as `go test -json`, which must show passing run and pass events for `TestFP6ProcessGroups` and its `cooperative`, `resistant` and `leader-exits-first` scenarios in `github.com/wedevwork/callsheet/tests/function`; then `devcheck stress` at the same repeat count as Linux |

Both jobs check out the event's revision without persisted credentials, take
the Go version from `go.mod` with module caching, run `go mod download`, and
then run the check steps with `GOPROXY=off` and `GOSUMDB=off`. The workflow has
read-only repository permission (`contents: read`), uses no secrets and never
changes repository settings.

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
The project's declared repeat count is 20 per CPU setting (1, 2, 4). The
flow's coder and reviewer use this count when they re-run timing-dependent
tests they add or modify: `devcheck stress` for tests in its covered packages,
and the equivalent `go test -race -count=20 -cpu=1,2,4 -run '<tests>'
<package>` command for changed timing tests outside that set.

The count, CPU list, package groups, selector and time budgets are declared
once, in `internal/devcheck/stress.go` (`StressCount`, `StressSteps`). The
stage runs two sequential commands (argv, never a shell), each with
`CGO_ENABLED=1`:

```
go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/processgroup ./internal/spikes/gittransport
go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip|TestFP6ProcessGroups)$ ./tests/function
```

- Selected packages: `internal/testkit`, `internal/testkit/fakeadapter`,
  `internal/spikes/processgroup` and `internal/spikes/gittransport`, complete
  package tests (not benchmarks). The fake adapter is included because its
  signal handling and descendant lifecycle are timing-sensitive too.
- Selected function tests: only `TestFP4TransportHarness`,
  `TestFP5GitRoundTrip` and `TestFP6ProcessGroups` in `tests/function`,
  deliberately excluding unrelated function tests such as the twelve-artifact
  cross-build test.
- `-count=20` applies at each CPU setting: every selected top-level test runs
  60 times per invocation. This is repeated testing, never retry-until-green:
  any failure fails the stage. There is no count override, no lighter macOS
  count and no environment-based bypass.
- `all` does not include stress: `all` stays test, coverage, bench and cross,
  because repeated subprocess builds and process experiments cost minutes.
  Local release and review verification therefore runs both `all` and
  `stress`.
- Both CI jobs run stress as their last step, at the same count: a Linux-only
  stress pass cannot qualify Darwin.
- `-race` needs the native C compiler on each runner (cgo). A missing compiler
  is a failed prerequisite, never permission to omit `-race`. Shipped
  cross-built binaries remain CGO-disabled.

Budgets:

- Each test binary: `-timeout=6m`.
- The whole stage: a 15-minute watchdog (or the caller's earlier deadline)
  bounds compilation as well as test execution; when it expires the stage
  fails and no further step starts.
- Target: under 10 minutes for the complete stage on each hosted runner. The
  watchdog leaves 15 minutes of the 30-minute `ci-macos` job for setup and
  native tests; the job timeouts (45 and 30 minutes) remain the final bound.
  Exceeding the target is recorded, not a failure.
- If one package's test binary exceeds its 6-minute timeout, that package
  moves into its own stress step with its own 6-minute timeout; the repeat
  count, the CPU list and the package set never change, and no test is
  weakened.
- Estimated local cost with a warm build cache: 3–10 minutes (a planning
  estimate, not a hardware-independent limit).
- Measured: Linux, go1.26.4 linux/amd64 on a 16-thread Intel i7-11800H
  developer workstation (kernel 6.8), warm build cache, 2026-09-26, with the
  process-group spike's 1 s TERM-to-KILL grace: the whole stage took 372 s
  (6 min 12 s), of which `stress packages` about 201 s (slowest binary
  `internal/spikes/processgroup` 201 s; `internal/testkit/fakeadapter` 55 s,
  `internal/testkit` 31 s, `internal/spikes/gittransport` 24 s) and
  `stress function` 170 s. The resistant and leader-exits-first cases of the
  process-group experiment (`TestExperiment` and `TestFP6ProcessGroups`) wait
  out the full grace on every repetition, which dominates both steps. The
  slowest binary used 201 s of its 6-minute timeout, so no package was split
  into its own step. With the earlier 200 ms grace (2026-09-25) the stage took
  180 s: processgroup 105 s, `stress function` 74 s. Hosted runners are
  expected to be slower; their times are recorded from the CI logs.
  macOS: pending until the next `ci-macos` run of pull request #1, recorded
  from its log in the flow handoff (see First remote run).

Race and CPU scope: the top-level test packages are race-built, so the
process-group package's self-executed helper (its own test binary) is
instrumented. Helper binaries built by `testkit.BuildBinary` and
`testkit.BuildTestBinary` force CGO off, so the fake-adapter children and the
FP-5/FP-6 helper binaries are not race-built; their scenarios still repeat,
and the direct package tests cover the same Go logic under race. `-cpu` varies
the top-level tests' GOMAXPROCS, not that of independently launched children.

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

Native evidence limits: these files cannot be simulated by an OS parameter.
Linux behavior is proven by the native process experiments of `ci-linux`
(test and stress), Darwin behavior only by `ci-macos` (native and stress);
host-independent tests prove both decision branches on any host, and only the
next `ci-macos` run proves Darwin runtime behavior. `runtime.GOARCH` only
labels output and is outside this guard.

## Branch protection

Nothing in this repository applies branch protection. The product owner applies
these settings by hand, with repository administration rights, in Settings →
Branches for repository `wedevwork/callsheet`, branch name pattern `main`,
after both check contexts exist (after the first remote run). Keep any existing
stronger rule; do not replace unrelated protections with a blanket API write.

- Require a pull request before merging. No extra approving-review count is
  imposed by this iteration: the flow's code review already precedes PR
  creation.
- Require status checks to pass before merging: `ci-linux`, `ci-macos`. Select
  GitHub Actions as their expected source when available.
- Require branches to be up to date before merging.
- Do not allow bypassing the above settings (include administrators). No pull
  request bypass actors, no force pushes, no branch deletion.

Confirm the result with the read-only command

```
gh api repos/wedevwork/callsheet/branches/main/protection
```

and check that `required_status_checks.strict` is `true`, that both contexts
`ci-linux` and `ci-macos` are listed (under `contexts` or `checks` as
returned), that a pull request is required, that `enforce_admins` is enabled,
and that force pushes and deletions are not allowed. Also inspect any
repository or organization rulesets that apply to `main`. If the repository
plan or organization policy prevents this configuration, record that as an
owner-side blocker: a green workflow alone does not mean merges are protected.

## PR flow

Iteration branches are named `iter-NN-<slug>`, for example
`iter-02-plane-trust`. Every iteration lands on `main` through a pull request
(for iterations 01, 01b and 01c, the single pull request #1 described below):

1. Create the iteration branch `iter-NN-<slug>` from `main`.
2. Implement, then run the flow's code review and fix loop until the reviewer returns `REVIEW_APPROVED`.
3. Commit the reviewed code.
4. Push the branch and open a pull request targeting `main`.
5. Wait for both checks, `ci-linux` and `ci-macos`, to succeed on the current PR merge revision. A skipped, canceled, pending or unobserved check is not acceptable evidence.
6. If a check fails, fix it, have the changed code re-reviewed, push, and return to step 5. Updating the branch from `main` may require another run, because branches must be up to date.
7. Merge only after both checks are green and the branch protection requirements are met.

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

## First remote run

Local checks cannot prove runner provisioning, Go and action download or cache
behavior on GitHub, Darwin runtime results or repository protection. Those
remain pending until observed. After pushing, the owner records in the flow
handoff:

- the run URL and the commit it ran;
- the conclusions of both `ci-linux` and `ci-macos`;
- native evidence from the `ci-macos` log: the line `devcheck: native qualification passed on darwin/<arch>` naming `TestFP6ProcessGroups` and its three scenarios;
- stress evidence from both logs: the line `devcheck: stage stress ok`, the `-count=20 -cpu=1,2,4` commands, and the elapsed time of each stress step with the runner's OS, architecture and cache state (the macOS elapsed time is the handoff record for the stress budget);
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
```

`all` runs test (with race on Linux), coverage, bench and cross; `stress` is
explicit (see Stress checks), and release and review verification run both.
`internal/cicheck` and the `TestCI*` and `TestHardening*` function tests
validate the workflow structure, its devcheck stages against the driver's
dispatch, and this page, offline. `go run ./cmd/devcheck native` works on macOS only; on other hosts it
exits 1 with an unsupported-stage error. Optionally, a locally installed
`actionlint .github/workflows/ci.yml` can lint the workflow; it is not a
required dependency and nothing invokes it automatically.
