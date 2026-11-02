# Callsheet user guide

This guide teaches daily operation after the first dispatch: how the pieces fit, how to register roles, dispatch and wait, carry files through workspaces, connect a coordinator and enable workers. It links the references rather than repeating them.

## Start here

Complete the [quickstart](../quickstart.md) first. It installs Callsheet and takes you from an empty folder to a first completed dispatch on one machine, then two.

## Learn Callsheet

Read these chapters in order, or go straight to the one you need:

1. [Concepts](concepts.md): components, identities and the task lifecycle.
2. [Roles and manuals](roles-and-manuals.md): manuals on the sidecar, explicit selection and readiness.
3. [Dispatch and wait](dispatch-and-wait.md): the CLI and MCP task lifecycle, and recovery without accidental redispatch.
4. [Workspaces](workspaces.md): seed a folder, chain pinned results, inspect and deliver.
5. [Coordinator setup](coordinator-setup.md): the connection and playbook route for each of the four coordinator CLIs.
6. [Worker setup](worker-setup.md): the ordered enabling procedure for each of the four worker CLIs.
7. [Example](example.md): the development-flow example and its walkthrough checklist.
8. [Troubleshooting](troubleshooting.md): symptoms, checks and safe next actions.

For ongoing operation, services, address changes and recovery, continue with [deployment](../deploy.md).

## References

These retained references own the complete details:

- [Coordinator setup and MCP timeout qualification](../coordinator.md): vendor MCP registration, runbook loading and wait/wake qualification.
- [Workspaces reference](../workspaces.md): transfer, selector, ignore, conflict, paging and error semantics.
- [Real worker adapters](../real-adapters.md): current worker argv, selection, versions, enabling and output extraction.
- [CLI support catalog](../support-catalog.md): dated evidence, current policy summary and qualification limits.

## Example

The [development-flow example](../examples/dev-flow/README.md) is the runnable v1 example: five roles on two default vendor CLIs, with owner approval gates. The [example chapter](example.md) is the short entry to it.

Maintainer evidence, not a prerequisite of this path: the [real end-to-end exercise](../maintainers/real-e2e.md) records how the method was verified.
