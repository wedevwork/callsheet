# Real end-to-end exercise files

The real end-to-end exercise ([../../real-e2e.md](../../real-e2e.md)) runs the shared development method in [../dev-flow/README.md](../dev-flow/README.md): its ten role manuals and its coordinator playbook, loaded as literal bytes. This directory holds only the exercise's own mechanics and scenario, in `dev-full/`:

- [dev-full/flow.json](dev-full/flow.json): the five frozen role selections (schema `callsheet-dev-full-flow/v1`).
- [dev-full/flow-cursor-coder.json](dev-full/flow-cursor-coder.json): the optional coder-only Cursor selection (`cursor`, `grok-4.7`, `high`); the other four rows are the default's. Linux only, with an explicit `--cursor` executable and a fresh checked qualification first; no jail or confinement claim, and cache and temporary samples are report-only. See [Cursor coder qualification](../../real-e2e.md#cursor-coder-qualification).
- [dev-full/coordinator-prompt.md](dev-full/coordinator-prompt.md): the exercise mechanics appended to the shared playbook.
- [dev-full/phase.md](dev-full/phase.md): each coordinator phase's prompt.
- [dev-full/scenario.md](dev-full/scenario.md): the full scenario's fixture constraints, added to every full task envelope.
- [dev-full/scenario-light.md](dev-full/scenario-light.md): the light scenario's constraints, added instead to every light task envelope; the scenario selects which one is loaded and captured as provenance.

No role method is copied here. The retired four-hop role pairs, four-role flow and two-phase prompts are no longer shipped documentation.
