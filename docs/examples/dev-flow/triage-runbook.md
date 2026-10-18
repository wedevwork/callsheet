# Triage runbook

Investigate the defect the envelope names. The envelope bounds sanitized excerpts and digests. Large raw inputs stay in coordinator-controlled storage. Do not paste them into `rca.md` or `fix.md`.

## Read-only project

Project source, tests, and configuration are read-only. The publication allowlist is `rca.md`, a confidence-qualified `fix.md`, and the task-authorized evidence subtree. Fetch each artifact once, save the raw bytes before analysis, and keep a provenance manifest. Each entry records the source command or URL, the fetch time, the size, and the hash.

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

Use the envelope-specified evidence directory, such as `investigation/<attempt-id>/`, only for explicitly approved, non-secret retained evidence. No secrets, local login material, or full vendor home snapshots are published.

## Reproductions

Run reproductions on disposable copies. Never patch source to test a hypothesis. If a facility or a credential is missing, return `TRIAGE_BLOCKED`. Do not substitute a different revision and do not invent the missing observation.

## Task text

The coordinator keeps the combined goal and acceptance within the envelope ceiling, with payload omitted. If your own notes would overflow that ceiling, shorten excerpts and keep the decisive quotation, the path, and the digest. Do not truncate silently.

## Marker

End `rca.md` when it states a verdict, and end the final answer, with exactly one of `STATUS: TRIAGE_CONFIRMED`, `STATUS: TRIAGE_PROBABLE`, `STATUS: TRIAGE_UNCONFIRMED`, or `STATUS: TRIAGE_BLOCKED`. The marker matches the confidence table. `TRIAGE_UNCONFIRMED` and `TRIAGE_BLOCKED` do not publish a new `fix.md`.
