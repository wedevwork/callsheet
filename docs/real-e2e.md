# Real local end-to-end exercise

This runbook is the single entry point for Callsheet's manual real-worker exercise (design 12a-real-e2e, amendment A3). One command in one terminal runs the whole attempt: preflight, a local plane with three sidecars and the workspace hub, a headless Claude Code coordinator launched by the harness itself with your real configuration, four worker hops, your sign-off, evidence collection, the offline check and a post-run audit. It prints PASS or FAIL.

A passing run demonstrates this installation's mechanical workflow and the deterministic feature cases on this machine at that time, with your own agent configuration. It is not a vendor or timeout qualification, not a sandbox containment claim, and it never changes the support catalog. It never runs in CI: the command refuses to start when a `CI` variable is present (even empty), without `CALLSHEET_REAL_E2E=1`, when `CLAUDE_CONFIG_DIR` is set (even empty: unset it yourself; the harness never does), or on any OS but Linux.

## Prerequisites

- Linux (macOS live execution is refused: the Grok worker posture is Linux only).
- Built binaries: `go build -o /abs/path/callsheet ./cmd/callsheet` and `go build -tags reale2e -o /abs/path/reale2e ./cmd/reale2e`, run from inside the checkout.
- Go and Git on `PATH` (development prerequisites of the throwaway Go project, not Callsheet runtime requirements).
- Logged-in vendor CLIs at or above the minimum versions, given by absolute path: Claude Code `2.1.285`, Codex `0.159.0`, Grok `1.0.46` (the existing adapter version policy). The harness never logs in, reads credentials or rewrites profiles: resolve login, model access or posture outside it.
- Costs and deadlines (policy, not measurements): four paid authentication preflights of at most 2 minutes each and four paid feature tasks of at most 10 minutes each; your decision within 20 minutes; the whole attempt within 90 minutes of preflight start (including the first fingerprint), then at most 2 minutes of cleanup, the final fingerprint and the audit; all final validation within 60 seconds. The headless coordinator adds vendor-dependent turns and tool calls. No retries, no fix loops, no token-price ceiling.
- Protected roots: each `--protect-root` directory (repeatable, absolute, existing, not nested in another, not inside the evidence or runtime directory) has every Git repository below it fingerprinted before and after the run. Discovery stops at each repository's working tree, whose status and tracked state (one streamed digest each) cover everything below it; a nested repository that its parent repository ignores is not separately protected. Discovery descends at most `--protect-depth N` directory levels below each root (default 3): deeper repositories and trees, such as a Go module cache, are not walked and not protected, and the report states the depth. A directory whose `.git` Git rejects is recorded by that entry's metadata, and any change to it fails. There is no default. With no `--protect-root`, only the four vendor homes are fingerprinted and the report says `No repository root was protected`.
- A quiescent owner environment: other Claude, Codex or Grok sessions writing configuration or repositories during the run make the comparison fail.
- The existing parent of a new evidence directory inside the checkout's ignored `design/` tree, for example `mkdir -p design/real-e2e-runs`. Evidence is bounded (64 MiB of text, 4 MiB of evidence repository) and stays local and private.

The coordinator runs with your user instructions, settings, permissions, skills, agents, plugins, hooks and login exactly as they are. The only additional input is the session-only `--settings '{"sandbox":{"enabled":false}}'` (it merges over your settings and changes no file); there is no permission bypass. A denied or failed tool call fails the run and is reported with its command, as a compatibility finding.

## Roles, models and efforts

The shipped flow [examples/real-e2e/flow.json](examples/real-e2e/flow.json) is the default:

| ID / hop | Sidecar / adapter | Model | Effort |
|---|---|---|---|
| designer | codex | gpt-6-astra | low |
| design-reviewer | claude | claude-fable-5-1 | low |
| coder | claude | claude-opus-5-5 | high |
| code-reviewer | grok | grok-4.7 | high |

Every role has concurrency 1 and timeout 10m. To override a model or effort, copy `flow.json` to a local file, change only `model` or `effort`, and pass it with `--flow /abs/path/flow.json`; nothing changes after startup. The report labels such a run `override-flow`, and its PASS never claims your default pairs passed.

Shipped manuals and prompts: [designer instruction](examples/real-e2e/designer-instruction.md) and [runbook](examples/real-e2e/designer-runbook.md); [design-reviewer instruction](examples/real-e2e/design-reviewer-instruction.md) and [runbook](examples/real-e2e/design-reviewer-runbook.md); [coder instruction](examples/real-e2e/coder-instruction.md) and [runbook](examples/real-e2e/coder-runbook.md); [code-reviewer instruction](examples/real-e2e/code-reviewer-instruction.md) and [runbook](examples/real-e2e/code-reviewer-runbook.md); the coordinator's [instructions](examples/real-e2e/coordinator-prompt.md), [phase 1 prompt](examples/real-e2e/coordinator-phase-1.md) and [phase 2 prompt](examples/real-e2e/coordinator-phase-2.md).

The feature asks the designer, coder and reviewers for an optional `--name NAME` flag on a tiny greeting CLI, with ten fixed cases (`no_args`, `name`, `spaces`, `unicode`, `whitespace`, `empty_name`, `unknown_flag`, `missing_value`, `positional`, `help`), each a `TestRun/<case-id>` subtest.

## Run

```sh
CALLSHEET_REAL_E2E=1 /abs/path/reale2e run \
  --callsheet /abs/path/callsheet \
  --claude /abs/path/claude --codex /abs/path/codex --grok /abs/path/grok \
  --protect-root /opt/gitspace --protect-root /opt/gospace \
  --evidence /abs/callsheet/design/real-e2e-runs/NEW-ATTEMPT
```

The two `--protect-root` values are an example (the owner's); substitute your own roots or omit both flags for vendor-home-only protection.

## Sign-off

After the design review approved and the coordinator's first phase ended, the same terminal shows the full reviewed `design.md` and `design-review.md` with their paths and SHA-256 digests, then asks exactly:

```
Approve this design and start coding? [yes/no]
```

Type `yes` or `no` (surrounding spaces are ignored; anything else asks again). Yes records your decision and automatically resumes the same coordinator session for coding and the code review. No, end of input or 20 minutes without an answer ends the attempt without coding.

## Reading the result

The final output prints PASS or FAIL for the mechanical and deterministic criteria and, separately, `Result judgments: needs owner/coordinator review`, then links `report.txt` and `audit.md` in the evidence directory.

- `report.txt` lists every criterion with its fixed failure codes, the protected roots, the protected changed paths (any one fails the run) and the vendor housekeeping changes (report-only, with their before and after sizes). `~/.claude.json` is not covered by the fingerprint.
- `audit.md` (and `audit.json`) walks every step in order: preflight, deployment, both coordinator phases, each hop's dispatch, wait and result, your sign-off, the final tests and CLI cases, cleanup and the environment comparison, each `correct`, `incorrect` or `unobserved` with evidence pointers. Judgment rows (does the design cover each case, is the review meaningful) stay `unobserved` until reviewed.

A first normal run is expected to list housekeeping such as Claude history, the new session transcript, debug logs, shell snapshots, todos and statsig state, a possible token refresh, and Codex or Grok session, history and log updates. Unlisted vendor state (for example Codex's state database or global-state JSON, or Grok caches) fails as configuration until a design revision reclassifies it. Settings, instructions or repository changes always fail. A symlink in a vendor home or in the directories scanned above your repositories is checked as the link itself (where it points, not what it points at) and never followed: a dangling link is fine, but a link that is removed, added or pointed elsewhere during the run fails. The harness never repairs, moves or deletes any of your files.

Right after the run, review `audit.md` with your external dev-flow coordinator: it checks every judgment row and any disputed mechanical verdict and appends corrections to a separate `audit-review.md` (run ID, audit and report SHA-256, row ID, original and revised verdict, who and why, evidence references). Never edit `audit.json`, `audit.md`, the report or the evidence; a correction cannot turn a FAIL into a PASS. `reale2e check --evidence DIR` re-validates a bundle offline (optional).

## Rerun

Resolve the reported cause (login, model access, a permission or configuration incompatibility, a protected change), then run the same command with a new evidence directory. Every attempt has its own run ID, runtime, workspace and coordinator session; nothing is resumed or retried, and earlier bundles keep their result. If cleanup could not prove every process stopped, the runtime is kept and the supervisor prints the process IDs to check and stop before rerunning.
