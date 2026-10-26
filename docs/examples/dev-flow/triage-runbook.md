# Triage runbook

Investigate the defect the envelope names. The envelope bounds sanitized excerpts and digests. Large raw inputs stay in coordinator-controlled storage. Do not paste them into `rca.md` or `fix.md`.

## Read-only project

Project source, tests, and configuration are read-only. The publication allowlist is `rca.md`, a confidence-qualified `fix.md`, and retained evidence only where the envelope explicitly authorizes it as a published output. Temporary scratch is never a published output. Fetch each artifact once, save the raw bytes before analysis, and keep a provenance manifest. Each entry records the source command or URL, the fetch time, the size, and the hash.

Use a working and broken range when one is available. Enumerate hypotheses and the evidence that would disconfirm each. Distinguish the symptom, the mechanism, and the cause. Read project-specific operating instructions when the task explicitly provides them.

## Confidence and markers

| Confidence | Marker | fix.md |
|---|---|---|
| CONFIRMED | TRIAGE_CONFIRMED | published; reproduced or proven by direct evidence |
| PROBABLE | TRIAGE_PROBABLE | published; strong consistent evidence without reproduction |
| unconfirmed | TRIAGE_UNCONFIRMED | not published; rca.md only, naming what would settle the cause |
| blocked | TRIAGE_BLOCKED | not published; name the unavailable evidence, auth, or facility |

Only CONFIRMED and PROBABLE produce `fix.md`. An inherited `fix.md` must not be overwritten at `TRIAGE_UNCONFIRMED`, and it must not be reused. The coordinator accepts a brief only from the current successful triage publication whose confidence matches the cause. A stale brief does not authorize a fix.

If any item requires design, the whole brief is held for the full design flow. Do not split it into simultaneous designer and coder tasks. Say so with exactly one standalone line `Design required: yes`. `Design required: no` permits the qualified fix continuation.

## fix.md

When you publish it, include Symptom, Root cause at file:line, Evidence with decisive quotations, Confidence, exactly one standalone line `Design required: yes` or `Design required: no`, Fix, Blast radius, CI impact, Regression test, and Investigation links. The key has precisely that capitalization, no question mark, exactly one ASCII space after the colon, a lowercase value, and no trailing spaces. The line sits outside a code fence. It is not a heading, table cell, list item, or quotation. The key has no question mark. `yes` routes the whole brief to the designer. `no` permits the qualified fix. `rca.md` holds the hypothesis trail, evidence references, and missing evidence. Quote decisive evidence so a later worker does not need a vanished scratch path.

## Evidence directory

Retained evidence and temporary scratch differ. Retained evidence is explicitly authorized as a published output: use the envelope-specified evidence directory, such as `investigation/<attempt-id>/`, only for explicitly approved, non-secret retained evidence. Temporary scratch is the disposable directory an envelope's `scratch_policy` names; it is removed before the final answer and is never retained evidence. When the envelope names no evidence directory, only the reports are retained: keep bounded decisive quotations, command and commit provenance, and digests in `rca.md` and a qualified `fix.md`. A scratch-only link is not retained evidence. No secrets, local login material, or full vendor home snapshots are published or placed in scratch.

## Reproductions

Run reproductions on disposable copies inside the scratch directory the envelope names, never in an external temporary directory. If reproduction needs scratch and the envelope names no scratch path, as in a full-flow envelope, report the missing prerequisite as `TRIAGE_BLOCKED` rather than inventing an outside directory; an investigation that needs no reproduction can still finish from direct evidence. Never patch source to test a hypothesis. If a facility or a credential is missing, return `TRIAGE_BLOCKED`. Do not substitute a different revision and do not invent the missing observation.

For a light triage envelope naming .callsheet-triage-scratch in scratch_policy, that directory inside the original checkout is the only temporary-write exception. Existing checkout inputs remain byte/mode-identical; scratch export modes may differ; add no other path beyond allowed_outputs and this temporary scratch. There is no retained evidence directory: only rca.md and confidence-qualified fix.md may be published. After joining all reproduction children, return to the original checkout root and execute the envelope's exact removal recipe only for scratch you created. Scratch must be absent before the final answer. If creation, reproduction, a removal-recipe guard (test -d or test ! -L) or cleanup fails or is refused, report TRIAGE_BLOCKED when the turn can continue; no permission fallback, retry with another tool or re-spelling of a denied command.

That scratch has this lifecycle:

- Resolve scratch relative to the initial checkout root, even after changing directories. Use only this directory for reproduction copies, raw temporary evidence, logs, and tool temporary or cache output created for the investigation. No `/tmp/triage-*`, external path, symlink escape, or writes into `.git` or adapter runtime directories.
- Create a fresh directory only if absent; an existing file, directory, or symlink at that name is a blocker, not permission to reuse or recursively erase unknown contents. Use `copy/` for the disposable project and `raw/` for evidence. Export the four seed files (`FEATURE.md`, `go.mod`, `main.go`, `main_test.go`) of the explicitly named `initial_source_commit` into `copy/`, without `.git` or scratch recursion. Recovery still reproduces the immutable seed. Do not patch the checkout's source, tests, or configuration, or the exported source, to fabricate a reproduction.
- Where Go commands need temporary or cache directories, set their per-command `TMPDIR`, `GOTMPDIR`, `GOCACHE`, and `GOMODCACHE` beneath the scratch root. Do not persist environment or configuration changes.
- Record the decisive evidence before removal: bounded decisive quotations, command and commit provenance, and digests in `rca.md` and a qualified `fix.md`.
- The removal recipe is `test -d .callsheet-triage-scratch && test ! -L .callsheet-triage-scratch && chmod -R u+w .callsheet-triage-scratch && rm -r .callsheet-triage-scratch && test ! -e .callsheet-triage-scratch`. Its two guards establish a real directory, not a symlink, before `chmod -R u+w` makes owned read-only cache entries writable; the non-force `rm -r` removes it and `test ! -e` verifies absence. A failed command stops the chain. Never use a recursive-force `rm` spelling.

## Task text

The coordinator keeps the combined goal and acceptance within the envelope ceiling, with payload omitted. If your own notes would overflow that ceiling, shorten excerpts and keep the decisive quotation, the path, and the digest. Do not truncate silently.

## Marker

End `rca.md` when it states a verdict, and end the final answer, with exactly one of `STATUS: TRIAGE_CONFIRMED`, `STATUS: TRIAGE_PROBABLE`, `STATUS: TRIAGE_UNCONFIRMED`, or `STATUS: TRIAGE_BLOCKED`. The marker matches the confidence table. `TRIAGE_UNCONFIRMED` and `TRIAGE_BLOCKED` do not publish a new `fix.md`. A final answer without one of these markers, such as a turn that ends after a refused command, is stopped as `invalid_marker`; it is not retried or given a marker.
