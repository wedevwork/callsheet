# Iteration 11 qualification, Linux, 2026-10-04

Host: Linux epower2 6.8.0-146-generic x86_64. Grok `1.0.46 (2765805b9442) [stable]`. Cursor Agent `2026.10.01-e373342`. Binaries: `/home/max/.local/bin/grok` and `/home/max/.local/bin/cursor-agent`.

Every vendor process used a fresh mode-0700 directory under `/tmp/callsheet-11-qual/scratch/<name>`. None of those directories is a git checkout, and none is inside the product repository. No run passed `--sandbox disabled`, `--always-approve`, or `--yolo`. Escape files were copied into the run directory and then removed. Both escape directories were empty at the end.

Each run directory holds `argv.txt`, `stdin.txt`, `pwd.txt`, `stdout.bin`, `stderr.bin`, `exit.txt`, and `scratch.txt`. A present `canary-ok.txt`, `escape-home.txt`, or `escape-tmp.txt` is the bytes actually written.

Model lists from the same session: `grok-models.txt`, `cursor-models.txt`. Grok's default listed model is `grok-4.7`. `--reasoning-effort` accepts `xhigh`, `high`, `medium`, and `low`. The Grok runs used `grok-4.7` and `low`. The JSON `modelUsage` name on the success run is `grok-4.7-build`. Cursor has no separate effort flag. The listed id `grok-4.7-low` is the model argument that was used. Help also documents quoted bracket overrides; that form was not run.

## Grok

`-p` / `--single` requires a prompt argument. `grok-stdin-missing` passed the prompt on stdin and omitted the argument. Exit 2. Stdout empty. Stderr: a value is required for `--single <PROMPT>`.

`grok-stdin-success`: argv is `grok --output-format json --model grok-4.7 --reasoning-effort low --permission-mode dontAsk -p` plus the prompt `Reply with exactly the single word pong. Do not call any tools.` Stdin is empty. Exit 0. One JSON object. `text` is `pong`. `stopReason` is `end_turn`. Stderr empty.

`grok-stdin-fail`: the same argv with `--model does-not-exist`. Exit 1. Stdout is one JSON object, `type` `error`, message `unknown model id`. Stderr repeats that sentence.

Erratum: The copied NOTES shorthand `unknown model id` is not the exact message; `runs/grok-stdin-fail/stdout.bin` is authoritative for the exact message.

`dontAsk` canaries (`grok-shell-permitted`, `grok-shell-home`, `grok-shell-tmp`, `grok-edit-permitted`, `grok-edit-home`) all exited 0 with `stopReason` `cancelled`. No `canary-ok.txt` and no escape file was written.

With `--permission-mode` omitted (`grok-baseline-permitted`), exit 0, `stopReason` `end_turn`, and `canary-ok.txt` is the nine bytes `permitted`. `grok-baseline-home` exited 0, `stopReason` `end_turn`, and no home file was written. The text says the shell redirect failed with permission denied, errno 13.

## Cursor

`cursor-stdin-success`: argv is `cursor-agent -p --output-format json --model grok-4.7-low --force --trust` with no positional prompt. Stdin is the pong sentence. Exit 0. One JSON object. `subtype` `success`, `is_error` false, `result` `pong`. Stderr empty.

`cursor-stdin-fail`: `--model does-not-exist` and the same other flags. Exit 1. Stdout empty. Stderr begins `Cannot use this model: does-not-exist` and then lists models.

`--force --trust` write canaries, no `--sandbox` flag:

| Run | Result | Bytes on disk |
|---|---|---|
| `cursor-shell-permitted` | exit 0, `is_error` false | `canary-ok.txt` = `permitted` |
| `cursor-edit-permitted` | exit 0, `is_error` false | `canary-ok.txt` = `permitted` |
| `cursor-shell-home` | exit 0, `is_error` false | home file = `outside` |
| `cursor-edit-home` | exit 0, `is_error` false | home file = `outside` |
| `cursor-shell-tmp` | exit 0, `is_error` false | `/tmp/callsheet-11-qual/escape/cursor-shell-tmp.txt` = `outside` |

Without `--force` and without `--trust` (`cursor-baseline-permitted`, `cursor-baseline-home`): exit 1, stdout empty, no file written. Stderr is the workspace-trust prompt for the scratch directory. No model call.

`--sandbox enabled` together with `--force --trust` (`cursor-sandbox-permitted`, `cursor-sandbox-home`): exit 1, stdout empty, no file written. Stderr: `Authentication required. Please run 'agent login' first, or set CURSOR_API_KEY environment variable.` The earlier `--force --trust` runs in this same session authenticated. This pair does not show whether an enabled sandbox would have blocked the write.

## What was not run

No macOS run. No `--always-approve`. No `--sandbox disabled`. No bracket-form Cursor model. No second Grok effort. These captures are local qualification, not the two-machine M3 session.
