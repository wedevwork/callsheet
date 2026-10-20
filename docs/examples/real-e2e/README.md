# Real end-to-end exercise files

The real end-to-end exercise ([../../real-e2e.md](../../real-e2e.md)) runs the shared development method in [../dev-flow/README.md](../dev-flow/README.md): its ten role manuals and its coordinator playbook, loaded as literal bytes. This directory holds only the exercise's own mechanics and scenario, in `dev-full/`:

- [dev-full/flow.json](dev-full/flow.json): the five frozen role selections (schema `callsheet-dev-full-flow/v1`).
- [dev-full/coordinator-prompt.md](dev-full/coordinator-prompt.md): the exercise mechanics appended to the shared playbook.
- [dev-full/phase.md](dev-full/phase.md): each coordinator phase's prompt.
- [dev-full/scenario.md](dev-full/scenario.md): the fixture's constraints, added to every task envelope.

No role method is copied here. The retired four-hop role pairs, four-role flow and two-phase prompts are no longer shipped documentation.
