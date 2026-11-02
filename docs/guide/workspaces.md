# Workspaces

[User guide](index.md)

A workspace gives tasks real files and gives you a pinned result to inspect. The [workspaces reference](../workspaces.md) and the [quickstart](../quickstart.md#first-workspace-task) hold the executable commands; this chapter teaches the sequence.

## Seed a folder

Start from an ordinary non-git source folder and a fresh destination. Git is not required: an ordinary folder is a valid source, as [pushing a plain folder](../workspaces.md#pushing-a-plain-folder) describes. Its [ignore rules](../workspaces.md#ignore-rules) decide which files travel.

Workspace refs live on the plane. Your local source is not automatically synchronized: to send an edit, push again.

## Chain results

The whole sequence, in order:

1. Create the workspace.
2. Push the source folder.
3. Obtain the workspace instance and the seed commit, and keep both.
4. Dispatch the first task at that immutable base, with the instance.
5. Inspect the task's publication and read all diff pages.
6. Chain the next role using the result commit and the bound instance, never a floating branch.
7. Pull the accepted task into a new destination.

The selected base and instance are fixed at admission, so later branch movement cannot change what a task sees. No branch moves on task completion. See [chaining tasks](../workspaces.md#chaining-tasks).

## Inspect and deliver

A succeeded process can have failed publication, and a failed publication is not delivery. Check the publication and the `available` flag before you rely on a result; [task results by task ID](../workspaces.md#task-results-by-task-id) lists the fields. Unavailable results cannot be delivered.

A diff is paged, so read every page before judging a change. Publication and semantic approval are separate questions: a published commit says the files were recorded, not that they are right.

[Pulling into a folder](../workspaces.md#pulling-into-a-folder) refuses overwrite: the destination must be new or empty. Inspect the pulled files separately before incorporating results into the owner's project.

For multi-role work with approval gates, see the [example](example.md).

## Conflicts and limits

A stale instance or generation is a conflict requiring fresh inspection, not blind overwrite. Read the current state again and decide; do not retry the same command until it passes. The reference lists every code under [errors](../workspaces.md#errors).

Next: [Coordinator setup](coordinator-setup.md).
