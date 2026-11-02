# Troubleshooting

[User guide](index.md)

Each row maps a symptom to supported checks and a safe next action. If nothing here applies, stop and diagnose before changing anything.

## Connection and readiness

| Symptom | Check | Next action |
|---|---|---|
| Trust error, `connection not trusted` | The URL against the server SANs, a readable CA and the clock, in the order of [Connection not trusted](../deploy.md#connection-not-trusted) | Correct the URL or CA copy. Verification stays on |
| Offline node when adding a role | The sidecar is running and the node list shows it online; see [check readiness](roles-and-manuals.md#check-readiness) | Start the sidecar, wait for its heartbeat, then retry `role add` |
| Unready role | The adapter path, the [minimum version](../real-adapters.md#selection-and-versions) and that both manuals are readable on the node | Correct that item and read role readiness again |

## Authentication and permission failures

| Symptom | Check | Next action |
|---|---|---|
| Login or permission failure | The actual service user and the vendor policy; see [worker setup](worker-setup.md#before-enabling-a-worker) | Stop for owner diagnosis. Authenticate that user yourself, with no permission bypass |
| Grok compound command fails | The [Grok Build](worker-setup.md#grok-build) note and its probe | Use separate simple tool commands, or run the probe before depending on Grok |

ASSUMPTION (unverified): The owner reported during 12a that Grok headless could not run compound shell commands; applicability to other versions and configurations is unverified.

The owner's probe tests simple versus compound commands with the actual authenticated version and records the argv, output and actions; see [Grok Build](worker-setup.md#grok-build).

## Task and wait results

| Symptom | Check | Next action |
|---|---|---|
| Successful exit without the work | The final output plus the artifacts, not the exit alone | Treat the task as not done; dispatch new work only when you decide to |
| Null final message | The extraction diagnostic in the task logs; see [how a task runs](../real-adapters.md#how-a-task-runs) | Diagnose the output. Null differs from an empty answer |
| Lost dispatch response | `task_ls` or `task ls`, to identify the admitted task | Use that task. If it is ambiguous, stop for diagnosis. Never dispatch a duplicate |
| Silent long wait | The saved task status and any plane or node outage; see [recover safely](dispatch-and-wait.md#recover-safely) | Fix the outage and wait again. Do not dispatch a duplicate |

## Workspace delivery

| Symptom | Check | Next action |
|---|---|---|
| Failed publication | The task's publication and `available` flag; see [task results](../workspaces.md#task-results-by-task-id) | Inspect and keep it for diagnosis. A failed publication is not delivery |
| Stale instance | The current workspace instance and the [errors](../workspaces.md#errors) table | Inspect afresh and decide; do not overwrite blindly |
| Paginated diff | The continuation fields of [task results](../workspaces.md#task-results-by-task-id) | Read every page before judging |
| Pull conflict | The destination, which must be [new or empty](../workspaces.md#pulling-into-a-folder) | Pull into a new destination |

## Vendor changes

| Symptom | Check | Next action |
|---|---|---|
| A vendor CLI changed version | The recorded Cursor update and its evidence in [vendor changes](worker-setup.md#vendor-changes) | Record before and after versions, then stop or requalify the affected work |
| Behavior differs from the catalog | The date and version of the [support catalog](../support-catalog.md) evidence | Treat the evidence as dated, and test your own version before relying on it |

Next: [Deployment](../deploy.md).
