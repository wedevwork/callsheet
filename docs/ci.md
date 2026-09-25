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
| `ci-linux` | `ubuntu-24.04` | 45 min | `devcheck test` (native suite, then the same suite with `-race`), `devcheck coverage` (unit coverage must be greater than 80.0%), `devcheck bench` (git transport payload byte limits and commit/tree invariants; timings are reported, never gated), `devcheck cross` (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64: 12 artifacts) |
| `ci-macos` | `macos-15` | 30 min | `devcheck native`: the complete suite as `go test -json`, which must show passing run and pass events for `TestFP6ProcessGroups` and its `cooperative`, `resistant` and `leader-exits-first` scenarios in `github.com/wedevwork/callsheet/tests/function` |

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
Darwin architecture.

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
`iter-02-plane-trust`. From iteration 02 on, every iteration lands on `main`
through a pull request:

1. Create the iteration branch `iter-NN-<slug>` from `main`.
2. Implement, then run the flow's code review and fix loop until the reviewer returns `REVIEW_APPROVED`.
3. Commit the reviewed code.
4. Push the branch and open a pull request targeting `main`.
5. Wait for both checks, `ci-linux` and `ci-macos`, to succeed on the current PR merge revision. A skipped, canceled, pending or unobserved check is not acceptable evidence.
6. If a check fails, fix it, have the changed code re-reviewed, push, and return to step 5. Updating the branch from `main` may require another run, because branches must be up to date.
7. Merge only after both checks are green and the branch protection requirements are met.

Do not enable a merge queue, automate merging, or upload gitignored design or
review artifacts to the pull request.

Bootstrap exception (one time, already settled by the product owner): iteration
01b, which adds this CI, may land directly on `main` after code review, before
CI exists. The owner then pushes, observes the first CI run, fixes any actual
platform failures through the flow, and enables branch protection after both
contexts are available and successful. Do not begin merging iteration 02 before
that handoff is complete. The first real pull request (iteration 02) verifies
the `pull_request` trigger and actual merge blocking. No fake first-run
evidence belongs in repository files.

## First remote run

Local checks cannot prove runner provisioning, Go and action download or cache
behavior on GitHub, Darwin runtime results or repository protection. Those
remain pending until observed. After pushing, the owner records in the flow
handoff:

- the run URL and the commit it ran;
- the conclusions of both `ci-linux` and `ci-macos`;
- native evidence from the `ci-macos` log: the line `devcheck: native qualification passed on darwin/<arch>` naming `TestFP6ProcessGroups` and its three scenarios;
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
go test -count=1 -run '^TestCI' ./tests/function
```

`all` runs test (with race on Linux), coverage, bench and cross.
`internal/cicheck` and the `TestCI*` function tests validate the workflow
structure, its devcheck stages against the driver's dispatch, and this page,
offline. `go run ./cmd/devcheck native` works on macOS only; on other hosts it
exits 1 with an unsupported-stage error. Optionally, a locally installed
`actionlint .github/workflows/ci.yml` can lint the workflow; it is not a
required dependency and nothing invokes it automatically.
