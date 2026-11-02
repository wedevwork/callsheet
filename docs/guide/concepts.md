# Concepts

[User guide](index.md)

This chapter names the pieces Callsheet is made of and the identities you will copy between commands.

## Components

- **Plane.** The one server that holds the shared roster, the task log and the workspace hub. Every other component connects to it over verified TLS.
- **Sidecar and node.** A sidecar runs on a worker machine, connects out to the plane and supervises worker CLI processes. The enrolled machine is a **node**, with its own node ID.
- **Worker CLI.** A vendor CLI you installed and authenticated, enabled explicitly on one sidecar.
- **Coordinator.** Your own client: a CLI session you talk to, connected through `callsheet mcp`. Callsheet hosts no coordinator. You, or the coordinator acting for you, choose every task and every follow-up.

## Roles and tasks

A **role** selects the manuals, adapter, model, effort and concurrency for one CLI on one node.

- The **role ID** is unique and names exactly one role instance. The **role name** is a logical label that several roles may share. Dispatch by ID pins an instance; dispatch by name takes the first role of that name, in registration order, that can accept now.
- The **instruction** is standing guidance and the **runbook** is the procedure. Both are manuals read on the sidecar for every task.
- A **task ID** identifies one admitted task. Keep it: every later command names it.

Dispatch is asynchronous admission: the plane reserves one of the role's slots and returns a task ID, or refuses at once. There is no queue and no reroute. Each task starts a fresh worker process, and no worker conversation is resumed between tasks. Continuity between tasks is carried by files, not by conversation reuse.

Acceptance text is passed to the worker, not evaluated by Callsheet. Whether a result is good is your judgment or your coordinator's.

## Files and results

A task without a workspace gets a scratch directory, not a delivered project. It answers with a final message and has no result commit.

A **workspace** is a named git hub on the plane. These identities pin a task to exact files:

- The **workspace name** is the label you choose, for example `myproject`.
- The **workspace instance** is a token naming one incarnation of that name, so a recreated workspace is never mistaken for the old one.
- The **base** is the commit a task starts from, fixed at admission.
- The **result commit** is what a workspace task publishes when it ends. A published result does not advance a branch: it is a separate task ref that you inspect and deliver yourself.

The [workspaces chapter](workspaces.md) walks through them.

## Trust and limits

Callsheet needs no Callsheet SaaS account, no workflow object, no vendor CLI patches and no bundled VPN. Vendor accounts and authentication may still be needed.

Read the deployment [security posture](../deploy.md#security-posture) before sharing a plane. In short: only trusted peers may reach it, there is no per-client authorization, Callsheet adds no sandbox of its own, and sidecars belong under a dedicated worker account. Certificate provisioning is covered in [Give clients the CA](../deploy.md#give-clients-the-ca).

Next: [Roles and manuals](roles-and-manuals.md).
