# Callsheet

Callsheet is a distributed AI agent dispatcher.

It makes the agent CLIs you already run addressable through a shared roster, dispatch path, task log and versioned workspace across machines. You talk to one CLI as your coordinator; other CLI processes do worker tasks. It is not an agent, agent orchestration, a deployment tool, SaaS, or a workflow engine; pipelines live in coordinator runbooks.

## How it works

```text
Coordinator CLI -> local callsheet mcp -> Plane
                                       (roles, tasks, workspace hub)
Worker node A: sidecar -> Plane (outbound TLS connection)
              roles -> worker CLI -> task checkout -> result commit
Worker node B: sidecar -> Plane (outbound TLS connection)
              roles -> worker CLI -> task checkout -> result commit
```

The plane holds the shared roster and tasks. Each node's sidecar connects out to the plane and supervises worker CLI processes. A role selects the manuals, model, effort and concurrency for one CLI on one node. The coordinator, your own CLI session, chooses tasks and every follow-up: Callsheet hosts no coordinator, runs no pipeline, merges no results and starts a fresh worker process for each task. Workspaces are optional: a workspace task starts from a base commit in a fresh checkout and publishes a result commit, and a task without one runs in a scratch directory. For a first setup, the plane, a sidecar and the coordinator can share one machine.

## Quickstart summary

A five-step orientation, not a complete installation recipe; the linked references hold the details.

1. Build the current checkout with `go build -o callsheet ./cmd/callsheet` (Go version in [go.mod](go.mod)) and put the binary on every participating machine. This is the current source-build path; prebuilt release binaries and an install quickstart are coming in v1.
2. Initialize and run the plane (`plane init`, `plane run`) and give its CA certificate to the other machines. Until the deployment guide exists, each subcommand's `--help` lists its current flags.
3. Enroll and run a sidecar (`sidecar enroll`, `sidecar run`) with an explicitly enabled, already authenticated vendor CLI, then register a role with its manuals, model and effort (`role add`); see [worker setup](docs/real-adapters.md).
4. Configure your coordinator's `callsheet mcp` connection and load your runbook; see the [coordinator guide](docs/coordinator.md). Direct CLI dispatch remains available.
5. Dispatch to a role and inspect or wait for the task (`dispatch`, `task show`, `task wait`); optionally push a [workspace](docs/workspaces.md) and pull results. The [development-flow example](docs/examples/dev-flow/README.md) is a reusable starting point.

## Supported CLIs and OSes

Callsheet's plane, sidecar and coordinator commands support Linux and macOS; Windows is not supported.

| CLI | Worker availability |
|---|---|
| Claude Code (`claude`) | Linux; macOS can be enabled explicitly, vendor sandbox and exit behavior unverified |
| Codex (`codex`) | Linux; macOS can be enabled explicitly, vendor sandbox and exit behavior unverified |
| Grok Build (`grok`) | Linux only; macOS worker execution refused |
| Cursor Agent (`cursor`) | Linux only; non-Linux worker execution refused |

These rows describe workers; all four CLIs can also be the coordinator. See [coordinator setup](docs/coordinator.md) for those integrations and the [support catalog](docs/support-catalog.md) for per-version, per-fact evidence. Product support is not blanket vendor qualification.

## Security

Use a trusted network and a dedicated OS user: TLS protects transport but provides no client identity or authorization, any reachable peer holding the [CA](docs/coordinator.md#what-every-configuration-shares) can dispatch work and read or write every workspace, and worker containment depends on the [vendor configuration](docs/real-adapters.md#enabling-a-worker) because Callsheet adds no sandbox of its own.

## Documentation

- [Coordinator setup](docs/coordinator.md): coordinator connection, runbooks and MCP setup; its [coordinator runbook example](docs/coordinator.md#coordinator-runbook-example) shows background waits.
- [Real worker adapters](docs/real-adapters.md): worker enablement, prerequisites and vendor caveats.
- [Workspaces](docs/workspaces.md): workspace transfer, tasks and results.
- [Support catalog](docs/support-catalog.md): CLI support and qualification evidence.
- [Development-flow example](docs/examples/dev-flow/README.md): a reusable development flow, indexing its role manuals and coordinator playbook.
- `docs/quickstart.md`: first dispatch on one machine, then two; coming in v1.
- `docs/guide/index.md`: the user guide; coming in v1.
- `docs/deploy.md`: deployment, trust and service management; coming in v1.

## For maintainers

- [CI](docs/maintainers/ci.md): the CI checks, their commands and budgets.
- [Real end-to-end exercise](docs/maintainers/real-e2e.md): the opt-in real-CLI qualification gate.
- [Exercise files](docs/examples/real-e2e/README.md): the exercise's scenario and mechanics assets.

The real-e2e harness is for maintainers and is not a prerequisite for ordinary use.
