# Roles and manuals

[User guide](index.md)

Complete the [quickstart](../quickstart.md) first: it starts the plane and a sidecar and shows minimal manual contents.

## Prepare manuals

The instruction is standing guidance; the runbook is the procedure. Both are UTF-8 files read on the selected sidecar when a task runs. Copying them only to the coordinator is insufficient: a path on your own machine means nothing to a remote sidecar. Callsheet does not distribute manuals, and you copy them to each node yourself.

These worker manuals are different from the coordinator's user-loaded playbook, which your coordinator client reads. Callsheet never reads that playbook.

## Register a role

The commands use these operator variables. They are your own selections, not built-in environment configuration:

| Variable | How to obtain it |
|---|---|
| `NODE` | The node ID from the node list below |
| `ROLE_ID` | A unique ID you choose |
| `ROLE_NAME` | A logical name you choose; several roles may share it |
| `INSTRUCTION` | The absolute path of the instruction on that node |
| `RUNBOOK` | The absolute path of the runbook on that node |
| `MODEL` | A model your authenticated account can use; see [selection and versions](../real-adapters.md#selection-and-versions) |
| `EFFORT` | An effort from the same reference; the quickstart's Claude example uses `low` |
| `PLANE` | The plane URL from the [quickstart](../quickstart.md#one-machine) |
| `CA` | The plane's CA certificate from the [quickstart](../quickstart.md#one-machine) |

Start the sidecar and wait until the node list reports the node online:

<!-- guide: nodes -->
```sh
callsheet node ls --plane "$PLANE" --ca "$CA"
```

A node identity can exist before its first online heartbeat. Registering then fails with `node <id> is offline; start its sidecar and retry`: nothing is recorded, and you retry only after the node is online.

Then register the role. Selection stays explicit: this recipe uses the quickstart's Claude adapter and names every field.

<!-- guide: register -->
```sh
callsheet role add "$ROLE_ID" --name "$ROLE_NAME" --node "$NODE" --adapter claude --instruction "$INSTRUCTION" --runbook "$RUNBOOK" --model "$MODEL" --effort "$EFFORT" --concurrency 1 --timeout 10m --plane "$PLANE" --ca "$CA"
```

Callsheet validates its own input. Vendor acceptance is distinct from Callsheet validation: a syntactically valid model the vendor refuses fails as that task's result.

## Check readiness

Inspect the roster after all registry changes, not before the last one:

<!-- guide: roles -->
```sh
callsheet role ls --plane "$PLANE" --ca "$CA"
```

<!-- guide: role -->
```sh
callsheet role show "$ROLE_ID" --plane "$PLANE" --ca "$CA"
```

Expect the node online and the role's `can_accept` true (`CAN_ACCEPT` in the text listing). Readiness is reported after the sidecar's probe, so `false` right after `role add` can be transient: run `role ls` again first, and go to [troubleshooting](troubleshooting.md#connection-and-readiness) if it stays false. A successful version probe is not authentication: readiness shows that the executable can be invoked at an eligible version, not that it is logged in or can do useful work.

`concurrency` sets the role's slots. A dispatch by name takes the first ready role of that name in registration order, and `--role-id` pins an instance. With no free slot the dispatch is refused; it does not wait.

## Change a role

Read `callsheet role set --help` and `callsheet role rm --help` for the lifecycle details. A change applies to future work only and needs the node online; a removal has its own handling of tasks in flight.

Do not change a model or permissions automatically after a refusal. A refusal is a finding for the owner to diagnose.

Next: [Dispatch and wait](dispatch-and-wait.md).
