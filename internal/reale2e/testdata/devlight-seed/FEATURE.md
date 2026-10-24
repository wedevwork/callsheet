Restore the documented preservation of nonempty names, including whitespace-only names. `--name " "` currently exits 2 with no stdout and `greeting: --name must not be empty` on stderr. It must exit 0, print `Hello,  !` followed by a newline, and leave stderr empty. Investigate the cause, add the missing regression and restore this behavior without changing the interface or adding dependencies.

# Feature request: --name

Add an optional --name NAME flag to this greeting CLI.

- With no flag, print "Hello, world!" and a newline (exit 0).
- With --name Ada, print "Hello, Ada!" and a newline (exit 0).
- Preserve spaces and Unicode in a nonempty name.
- Reject an empty name, unknown flags, a missing value and positional
  arguments with exit status 2, nothing on stdout and a nonempty diagnostic
  on stderr.
- --help prints usage and exits 0.
- Standard Go flag parsing is acceptable. Implement a testable
  run(args []string, stdout, stderr io.Writer) int with table tests,
  including Unicode, a whitespace name, every error and help.
- No dependencies, network or persistence. Keep FEATURE.md, go.mod, main.go and main_test.go together strictly below 16 KiB. Each planning/report artifact is at most 32 KiB; all planning/report artifacts together are at most 128 KiB.

Cases (each is one subtest of the table test TestRun, named exactly by its
case ID, calling run):

| Case ID | Arguments | Result |
|---|---|---|
| no_args | (none) | exit 0, stdout "Hello, world!\n", empty stderr |
| name | --name Ada | exit 0, stdout "Hello, Ada!\n", empty stderr |
| spaces | --name "Ada Lovelace" | exit 0, stdout "Hello, Ada Lovelace!\n", empty stderr |
| unicode | --name 世界 | exit 0, stdout "Hello, 世界!\n", empty stderr |
| whitespace | --name " " | exit 0, stdout "Hello,  !\n", empty stderr |
| empty_name | --name "" | exit 2, empty stdout, nonempty stderr |
| unknown_flag | --unknown | exit 2, empty stdout, nonempty stderr |
| missing_value | --name | exit 2, empty stdout, nonempty stderr |
| positional | Ada | exit 2, empty stdout, nonempty stderr |
| help | --help | exit 0, usage mentioning name |
