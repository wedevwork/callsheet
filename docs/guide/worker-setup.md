# Worker setup

[User guide](index.md)

A worker is a vendor CLI you enable explicitly on one sidecar. Login is an operator prerequisite, not a Callsheet operation. Current versions, efforts and argv live in [real adapters](../real-adapters.md), and dated evidence lives in the [support catalog](../support-catalog.md); this chapter is the ordered checklist that links them.

## Before enabling a worker

Choose the sidecar's dedicated OS user: see [authenticate the worker](../deploy.md#authenticate-the-worker) and the [security posture](../deploy.md#security-posture). Use the vendor's own help for account-specific login.

Current auto policies are not OS confinement. Grok and Cursor prompts are passed in argv and are visible to process inspection, with the product's 32 KiB prompt bound. Never include credentials in goals.

An authentication or permission refusal stops the operation for owner diagnosis, without fallback, settings edits or broad permission bypasses.

macOS: Claude and Codex are eligible with an unverified macOS runtime warning; Grok and Cursor execution is refused. A flag being accepted is not execution eligibility, and Linux evidence does not certify macOS.

## Claude Code

Adapter flag `--claude-adapter`, executable `claude`.

1. Install it using the vendor's instructions.
2. Authenticate as the actual dedicated sidecar user.
3. Ensure its credentials and configuration are available in that account's service environment.
4. Check its version against the linked [minimum](../real-adapters.md#selection-and-versions).
5. Inspect its effective permissions.
6. Pass an explicit absolute executable path on every sidecar start, as [enabling a worker](../real-adapters.md#enabling-a-worker) shows.
7. Confirm the node is online and the role is ready.

Evidence limits: Callsheet requests `--permission-mode auto --permission-prompts none` and never retries with broader permissions. The catalog's plan, administrator and classifier statements are owner-recorded, not a fresh upstream verification. See the [Claude Code evidence](../support-catalog.md#claude-code).

## OpenAI Codex

Adapter flag `--codex-adapter`, executable `codex`.

1. Install it using the vendor's instructions.
2. Authenticate as the actual dedicated sidecar user.
3. Ensure its credentials and configuration are available in that account's service environment.
4. Check its version against the linked [minimum](../real-adapters.md#selection-and-versions).
5. Inspect its effective permissions.
6. Pass an explicit absolute executable path on every sidecar start, as [enabling a worker](../real-adapters.md#enabling-a-worker) shows.
7. Confirm the node is online and the role is ready.

Evidence limits: Callsheet requests `--approve-for-me`, and workspace-write is the selected vendor policy. The older `-a never` argv is historical. Neither is your coordinator's configuration. See the [OpenAI Codex evidence](../support-catalog.md#openai-codex).

## Grok Build

Adapter flag `--grok-adapter`, executable `grok`. Linux only.

1. Install it using the vendor's instructions.
2. Authenticate as the actual dedicated sidecar user.
3. Ensure its credentials and configuration are available in that account's service environment.
4. Check its version against the linked [minimum](../real-adapters.md#selection-and-versions).
5. Inspect its effective permissions.
6. Pass an explicit absolute executable path on every sidecar start, as [Wave 2](../real-adapters.md#wave-2-grok-and-cursor) shows.
7. Confirm the node is online and the role is ready.

Evidence limits: Callsheet requests `--permission-mode auto`. The October 4 `dontAsk` canaries all exited 0 with `stopReason` `cancelled` and wrote nothing; that proves the old observation only. Useful tool execution under current auto remains unverified. See the [Grok Build evidence](../support-catalog.md#grok-build).

ASSUMPTION (unverified): The owner reported during 12a that Grok headless could not run compound shell commands; applicability to other versions and configurations is unverified.

Before depending on Grok for such a workload, the owner tests simple versus compound commands with the actual authenticated version and records the argv, output and actions. Prefer separate simple tool commands, not shell bypass flags. The default Claude and Codex example does not depend on this assumption.

## Cursor Agent

Adapter flag `--cursor-adapter`, executable `cursor-agent`. Linux only.

1. Install it using the vendor's instructions.
2. Authenticate as the actual dedicated sidecar user.
3. Ensure its credentials and configuration are available in that account's service environment.
4. Check its version against the linked [minimum](../real-adapters.md#selection-and-versions).
5. Inspect its effective permissions.
6. Pass an explicit absolute executable path on every sidecar start, as [Wave 2](../real-adapters.md#wave-2-grok-and-cursor) shows.
7. Confirm the node is online and the role is ready.

Evidence limits: Callsheet runs `--trust --auto-review` with empty stdin, on Linux only, with no confinement claim. The historical `--force --trust` candidate is not a recipe to recommend. See the [Cursor Agent evidence](../support-catalog.md#cursor-agent).

The [Cursor coder qualification](../maintainers/real-e2e.md#cursor-coder-qualification) (maintainer evidence) is complete for the recorded binary identity and selection: Cursor `2026.10.22-27dcf5f`, coder `grok-4.7/high`, 2026-10-28. A2 remains unobserved: no approval-needing action occurred under `--auto-review`. That is no general approval-behavior or confinement qualification.

## Verify readiness

Run `callsheet role ls` and confirm the node is online and the role can accept; see [check readiness](roles-and-manuals.md#check-readiness). Readiness checks invocability and version, and does not prove login or useful work. The first authorized small task must inspect both the requested result and the exit.

## Vendor changes

A vendor update can change behavior that any recorded evidence describes. The evidence is dated; it does not describe the latest vendor release.

Recorded evidence: "Cursor self-updated to 2026.10.22-27dcf5f during the full child's coder run"; see the [superseded first Cursor qualification](../maintainers/real-e2e.md#superseded-first-cursor-qualification-and-exercise) (maintainer evidence). Record before and after versions and any changed identity, and stop or requalify the affected work. Production readiness is not the harness's resolved-target and hash binding, and Callsheet does not claim to detect every mid-task replacement.

Next: [Example](example.md).
