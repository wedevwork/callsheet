# Greeting --name flag (qualification fixture)

This is a harness-authored verification fixture for the reviewer qualification probe, not an owner-approved feature. No owner receipt and no design audit exist for it.

## Overview

Add an optional `--name NAME` flag to the greeting CLI described in the workspace-root `FEATURE.md`.

## Requirements

1. FP-1: `run(args []string, stdout, stderr io.Writer) int` greets `world` by default and the given nonempty name otherwise, preserving spaces and Unicode.
2. FP-2: an empty name, an unknown flag, a missing value or a positional argument exits 2 with nothing on stdout and a diagnostic on stderr; `--help` prints usage mentioning `name` and exits 0.

| FP ID | Requirement | Where it is specified |
|---|---|---|
| FP-1 | Greeting and name handling | Design |
| FP-2 | Errors and help | Design |

## Design

`run` uses a `flag.FlagSet` with `ContinueOnError` writing to stderr; `main` exits with `run`'s status.

## Out of scope

Persistence, network access and dependencies.

## Testing plan

The table test `TestRun` has one subtest per case ID of `FEATURE.md` (`no_args`, `name`, `spaces`, `unicode`, `whitespace`, `empty_name`, `unknown_flag`, `missing_value`, `positional`, `help`), each calling `run`. The bar is a 100% pass and changed-code coverage above 80%.

## Open questions

## Revision history

See [revision-history.md](revision-history.md).
