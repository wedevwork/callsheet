# Example

[User guide](index.md)

The [development-flow example](../examples/dev-flow/README.md) is the runnable v1 example, and its README is the canonical set of instructions. This chapter is the short entry to it, with the evidence checklist for a walkthrough.

## Follow the example

1. Read [Start here](../examples/dev-flow/README.md#start-here) for the default selection and the three prerequisites.
2. Prepare with [Run with Callsheet](../examples/dev-flow/README.md#run-with-callsheet): copy the manuals, register five roles and check readiness.
3. Load the [coordinator playbook](../examples/dev-flow/coordinator-playbook.md) in your own coordinator; see [coordinator setup](coordinator-setup.md).
4. Run the [full walkthrough](../examples/dev-flow/README.md#full-walkthrough) for a design change, with the owner's explicit yes before coding, or the [light walkthrough](../examples/dev-flow/README.md#light-walkthrough) for a defect. Its triage can reproduce only in a project that `FEATURE.md`, `go.mod`, `main.go` and `main_test.go` fully reproduce; see the [seed-project restriction](../examples/dev-flow/README.md#seed-project-restriction).
5. Inspect the publication and pull the accepted task into a new destination; see [inspect and deliver](workspaces.md#inspect-and-deliver). The delivered tree contains the flow's documents under `design/`, so decide what to keep before copying it into a project.

## What the evidence shows

Three things must stay separate:

- **Mechanics.** Offline tests with fake workers exercise the commands, manuals and routing. They are not vendor qualification.
- **Recorded live evidence.** The 12a exercises prove an earlier selection's method and observed results using a verification harness; see the [real end-to-end exercise](../maintainers/real-e2e.md#combined-exercise) (maintainer evidence). The example's current selection has no recorded live evidence. That harness is not a user dependency.
- **M5 usability.** The owner's M5 docs-only run tests whether a new user can follow the published example with only Callsheet over vendor CLIs. Neither the offline tests nor the 12a exercise is proof that a docs-only walkthrough already happened.

## M5 walkthrough checklist

M5 follows the README, then an available release and its checksum or the pre-release source fallback, then the quickstart, then this example. Whether a release is available is conditional. The owner records the actual pass or blockers here; this document does not prefill them.

| Evidence | Recorded value |
|---|---|
| OS and architecture |  |
| Callsheet and vendor versions |  |
| Selected roles |  |
| Source, instance, commits and task IDs |  |
| Time to first dispatch |  |
| Full or light route taken |  |
| Approval and delivery |  |
| Every snag and its disposition |  |

Next: [Troubleshooting](troubleshooting.md).
