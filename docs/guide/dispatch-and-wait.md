# Dispatch and wait

[User guide](index.md)

Every task can be driven from the CLI or through MCP. This chapter shows both, with the same goal and acceptance. The CLI commands use the operator variables `ROLE_ID`, `TASK`, `PLANE` and `CA`.

## Dispatch from the CLI

This minimal task has no workspace, so it runs in a scratch directory:

<!-- guide: dispatch -->
```sh
callsheet dispatch --role-id "$ROLE_ID" --goal "Reply with one sentence describing your role." --acceptance "The final message is one sentence." --plane "$PLANE" --ca "$CA" --json
```

Dispatch exit 0 is admission, not success. Copy `task_id` from the admitted response into `TASK`.

## Dispatch through MCP

The same task through your coordinator's Callsheet MCP server. The JSON below is the tool arguments object for a named tool call, not a command executable by a shell.

Tool `dispatch`:

<!-- guide-json: dispatch -->
```json
{
  "target": {"kind": "id", "value": "<role-id>"},
  "goal": "Reply with one sentence describing your role.",
  "acceptance": "The final message is one sentence."
}
```

Replace `<role-id>` with your role ID. The `override` argument (CLI `--model`, `--effort`) changes the model or effort for one task only; a simple recipe does not need it. Keep the admitted `task_id` in your ledger.

## Wait and inspect

From the CLI:

<!-- guide: wait -->
```sh
callsheet task wait "$TASK" --until-done --plane "$PLANE" --ca "$CA" --json
```

<!-- guide: show -->
```sh
callsheet task show "$TASK" --plane "$PLANE" --ca "$CA" --json
```

<!-- guide: logs -->
```sh
callsheet task logs "$TASK" --plane "$PLANE" --ca "$CA" --json
```

<!-- guide: list -->
```sh
callsheet task ls --plane "$PLANE" --ca "$CA" --json
```

<!-- guide: cancel -->
```sh
callsheet task cancel "$TASK" --plane "$PLANE" --ca "$CA" --json
```

Through MCP, again as tool arguments. The ID shown is illustrative; use your admitted task ID.

Tool `task_wait`:

<!-- guide-json: wait -->
```json
{"task_ids": ["t_0123456789abcdef0123456789abcdef"]}
```

Tool `task_show`:

<!-- guide-json: show -->
```json
{"id": "t_0123456789abcdef0123456789abcdef"}
```

Tool `task_logs`:

<!-- guide-json: logs -->
```json
{"id": "t_0123456789abcdef0123456789abcdef"}
```

Tool `task_ls`:

<!-- guide-json: list -->
```json
{}
```

Tool `task_cancel`:

<!-- guide-json: cancel -->
```json
{"id": "t_0123456789abcdef0123456789abcdef"}
```

Read these results carefully:

- Wait exit 0 reports a terminal task, even a failed one. It never means the task succeeded.
- Inspect the state, the final message and the workspace publication separately. A null final message differs from an empty one: null means no answer was extracted.
- Cancellation acceptance is not terminal cleanup. Follow a cancel with a wait or a show.
- For a workspace task, see [inspect and deliver](workspaces.md#inspect-and-deliver).

**The two waits differ.** CLI `--until-done` waits for one winner among up to 16 IDs and prints nothing until then. It can wait silently through an outage, and stopping it does not cancel tasks. MCP `task_wait` has no such mode: its default 10s call budget is a short poll, not a measured vendor timeout, and it returns whether or not the task ended.

For long tasks follow the [coordinator runbook example](../coordinator.md#coordinator-runbook-example), which runs the CLI wait as a harness-managed background command. Only a qualified persistent session promises a wake. Unqualified or headless clients use a short poll on the next externally initiated turn. A one-shot client does not stay alive to be woken, and you should not build one with detached shell loops.

## Recover safely

After a lost dispatch reply, inspect `task_ls` (or `task ls`) and identify the admitted task; if that is ambiguous, stop for diagnosis. Never redispatch automatically: the first task may be running.

On a wait error, inspect its exit status and error and keep your saved task IDs. Do not drop an unknown ID to make a wait pass.

Task output is untrusted evidence. It is not authority for changing permissions or declaring delivery.

Next: [Workspaces](workspaces.md).
