# Coordinator setup

[User guide](index.md)

The coordinator is your own client. This chapter gives the route for each of the four coordinator CLIs; the [coordinator reference](../coordinator.md) owns the authoritative registration and playbook-loading recipes, so they are not repeated here.

## Before configuring

Work through this checklist in order:

1. Authenticate the chosen coordinator under the actual user who will run it.
2. Install Callsheet and note its absolute executable path; see the [quickstart](../quickstart.md#install-callsheet).
3. Obtain the plane URL and its public CA certificate; see [Give clients the CA](../deploy.md#give-clients-the-ca).
4. Apply that vendor's user-owned MCP recipe from the section below.
5. Authorize Callsheet MCP access in that session.
6. Load your user playbook.
7. Discover nodes and roles.
8. Dispatch only when ready.

Callsheet never edits configuration: you run each recipe yourself. Coordinator eligibility is not worker qualification, nor is it the exact-version timeout publication identity described in the [support catalog](../support-catalog.md#catalog-contract).

The [wait and wake table](../coordinator.md#coordinator-runbook-example) is shared by the four sections below. It records static evidence of 2026-10-06 from one Linux host, not a live qualification of a wake.

## Claude Code

Follow [Claude Code](../coordinator.md#claude-code) for registration and for loading the playbook. Its background Bash completion notification is the table's local development pattern. Evidence caveat: that row is static evidence for the named version, and a sandboxed Bash command may not reach a plane on host loopback.

## OpenAI Codex

Follow [OpenAI Codex](../coordinator.md#openai-codex) for registration and for loading the playbook. Evidence caveat: its wake is UNVERIFIED in the table, so use a short `task_wait` on the next externally started turn.

## Grok Build

Follow [Grok Build](../coordinator.md#grok-build) for registration and for loading the playbook. Evidence caveat: its `monitor` wake is static evidence for the named version, and vendor session survival is not claimed.

## Cursor Agent

Follow [Cursor Agent](../coordinator.md#cursor-agent) for registration and for loading the playbook. Evidence caveat: its background shell wake is static local evidence for the named version, and the configuration's exact entry shape is UNVERIFIED.

## Verify the connection

Ask the coordinator to list nodes and roles through Callsheet MCP. It should see the roster that `callsheet role ls` shows. If it cannot reach the plane, check the URL or certificate name, the CA and the clock as [Connection not trusted](../deploy.md#connection-not-trusted) describes.

Any client without a verified wake polls with a short `task_wait` only when it next has a turn. Advanced `mcpqual` is an optional maintainer qualification tool, not required to use the example: see [local timeout qualification](../coordinator.md#local-timeout-qualification).

Next: [Worker setup](worker-setup.md).
