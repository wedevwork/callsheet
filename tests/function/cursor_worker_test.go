package function

import (
	"fmt"
	"strings"
	"testing"
)

// Function and unit tests of design 12a-3-cursor-worker, slice 3b FP-4
// (parent FP-10, the owner procedure) and FP-5 (parent FP-11, the Cursor
// catalog, posture and delivery documentation). The validators are pure
// functions over the documentation text; each table-driven mutation must
// make its validator refuse. They validate the procedure and the pending
// claims, never a paid result.

// cursorStatusPin is the current Cursor status, verbatim.
const cursorStatusPin = "Cursor coder status: diagnostic observed; fresh qualification, combined full/light execution and external child judgment audits pending. The 2026-10-27 slice 3a receipt is not qualification. Default-selection probe 11 and its combined exercise do not qualify Cursor."

// cursorCommandTemplates are the procedure's literal command templates,
// in order.
var cursorCommandTemplates = []string{
	`CALLSHEET_REAL_E2E=1 /abs/reale2e probe-dev-full --scenario both --flow /abs/callsheet/docs/examples/real-e2e/dev-full/flow-cursor-coder.json --cursor /abs/cursor-agent --callsheet /abs/callsheet --claude /abs/claude --codex /abs/codex --grok /abs/grok --protect-root /abs/projects --evidence "$CURSOR_PROBE_EVIDENCE"`,
	`/abs/reale2e check --evidence "$CURSOR_PROBE_EVIDENCE"`,
	`CALLSHEET_REAL_E2E=1 /abs/reale2e run-dev-exercise --flow /abs/callsheet/docs/examples/real-e2e/dev-full/flow-cursor-coder.json --cursor /abs/cursor-agent --callsheet /abs/callsheet --claude /abs/claude --codex /abs/codex --grok /abs/grok --protect-root /abs/projects --evidence "$CURSOR_EXERCISE_EVIDENCE"`,
	`/abs/reale2e check --evidence "$CURSOR_EXERCISE_EVIDENCE"`,
	`/abs/reale2e check --evidence "$CURSOR_EXERCISE_EVIDENCE/attempts/full"`,
	`/abs/reale2e check --evidence "$CURSOR_EXERCISE_EVIDENCE/attempts/light"`,
}

// cursorProcedureClauses are the current procedure's required clauses:
// selection binding, posture, fresh qualification, the smoke, fresh
// evidence directories, bounds, stop and no-retry rules, the inspections,
// both external child audits and the completion rule.
var cursorProcedureClauses = []string{
	"The Cursor selection is accepted by run, run-dev-full, run-dev-light and run-dev-exercise only with an explicit --cursor executable.",
	"There is no jail and no confinement claim.",
	"A fresh Cursor probe writes qualification.json and exits 0 only after its independent check qualifies it.",
	"A slice 3a observation cannot qualify Cursor; a fresh complete probe and its independent checker are required before any Cursor attempt.",
	"Fresh qualification is an operator prerequisite, not a startup file lookup.",
	"Run the [pre-probe smoke](#pre-probe-smoke) first, unchanged: it is required before the fresh Cursor probe, it is non-qualifying even when it passes, and there is no separate Cursor smoke recipe.",
	"Before each invocation assign a fresh absolute nonexistent directory to the relevant variable (`CURSOR_PROBE_EVIDENCE` or `CURSOR_EXERCISE_EVIDENCE`).",
	"Paths to saved evidence are history, never reusable output destinations; the slice 3a bundle is never an output path.",
	"The owner stops unless the fresh both qualification and its offline check agree, then separately authorizes the combined cost, full design and light intake.",
	"Record A2 as unobserved unless an approval-needing action actually occurs; never manufacture one.",
	"On any failure keep the evidence and stop: no retry around a refusal, no timeout increase, no permission or configuration change and no fallback to another selection.",
	"the probe at most 12 worker calls and 90 minutes plus 2 minutes of cleanup",
	"the combined exercise at most 33 worker calls (full 19 plus light 14; 17 on the happy path), 90 + 2 minutes per child, 184 minutes plus the 20-minute authorization window",
	"at most ten worker calls for `--scenario full`, twelve for `light` or `both`",
	"After both children and root pass, the external coordinator audits every applicable judgment row in each child's audit.md",
	"records run ID, report/audit hashes, reviewer identity/date, verdict/reasons and artifact references in each child's `audit-review.md`",
	"Root aggregation cannot replace either child judgment audit.",
	"Never cd into a bundle; use absolute evidence paths.",
	"No automatic implementation/CI paid call or automatic owner sign-off.",
	"a probe alone, a full child alone or an unaudited exercise is not completion",
}

// cursorProcedureForbidden are claims the current procedure must never
// make: an automatic or startup qualification, a promoted diagnostic, a
// completed or observed Cursor status, or the retired 3a refusal.
var cursorProcedureForbidden = []string{
	"Cursor coder status: observed", "Cursor coder status: qualified", "Cursor coder status: complete", "qualified automatically",
	"looks up a qualification", "startup lookup of", "receipt qualifies", "observation qualifies", "cursor_execution_pending_3b",
	"20261027-cursor-probe1\"", "--evidence /abs/callsheet/design/real-e2e-runs/20261027-cursor-probe1", "retry the probe",
}

// cursorSections splits real-e2e.md's Cursor section into its current
// procedure and historical subsection.
func cursorSections(doc string) (string, string, error) {
	const head, hist = "\n## Cursor coder qualification\n", "\n### Historical slice 3a diagnostic observation\n"
	i, j := strings.Index(doc, head), strings.Index(doc, hist)
	if i < 0 || j < i || strings.Count(doc, head) != 1 || strings.Count(doc, hist) != 1 {
		return "", "", fmt.Errorf("docs/real-e2e.md lacks its single Cursor section and historical subsection")
	}
	rest := doc[j+len(hist):]
	if k := strings.Index(rest, "\n## "); k >= 0 {
		rest = rest[:k]
	} else {
		return "", "", fmt.Errorf("the historical subsection does not end before the next section")
	}
	if !strings.Contains(doc[j:], "\n## Sign-off\n") {
		return "", "", fmt.Errorf("the Cursor section is not a peer before Sign-off")
	}
	return doc[i:j], rest, nil
}

// flatText collapses whitespace runs (Markdown wrapping does not matter).
func flatText(s string) string { return strings.Join(strings.Fields(s), " ") }

// checkCursorProcedure is the owner procedure's independent validator: the
// verbatim pending status, the six literal command templates in one shell
// block with one selection and one executable throughout, every required
// clause, no forbidden claim, and the historical subsection's past-tense
// diagnostic record.
func checkCursorProcedure(doc string) error {
	current, historical, err := cursorSections(doc)
	if err != nil {
		return err
	}
	flat := flatText(current)
	if strings.Count(flat, cursorStatusPin) != 1 {
		return fmt.Errorf("the current status is not pinned verbatim once")
	}
	block := current[strings.Index(current, "```sh\n")+1:]
	if !strings.Contains(current, "```sh\n") || !strings.Contains(block, "\n```") {
		return fmt.Errorf("no command template block")
	}
	block = block[strings.Index(block, "\n")+1 : strings.Index(block, "\n```")]
	if lines := strings.Split(block, "\n"); len(lines) != len(cursorCommandTemplates) || strings.Join(lines, "\n") != strings.Join(cursorCommandTemplates, "\n") {
		return fmt.Errorf("the command templates are not exactly the procedure's six commands: %q", lines)
	}
	if strings.Count(current, "```sh\n") != 1 {
		return fmt.Errorf("a second command block")
	}
	for _, c := range cursorProcedureClauses {
		if !strings.Contains(flat, flatText(c)) {
			return fmt.Errorf("the procedure lacks %q", c)
		}
	}
	lower := strings.ToLower(flat)
	for _, f := range cursorProcedureForbidden {
		if strings.Contains(lower, strings.ToLower(f)) {
			return fmt.Errorf("the procedure claims %q", f)
		}
	}
	hflat := flatText(historical)
	for _, c := range []string{"The 2026-10-27 slice 3a receipt was a diagnostic-only observation, not qualification.",
		"The historical slice 3a probe wrote cursor-probe-observation.json, never qualification.json, and exited 1 even when all stages were observed.",
		"`design/real-e2e-runs/20261027-cursor-probe1`", "`qualification: false`", "no approval-requiring action occurred, so headless approval behaviour stays unobserved",
		"never rewritten or promoted", "`diagnostic_not_qualification`"} {
		if !strings.Contains(hflat, flatText(c)) {
			return fmt.Errorf("the historical subsection lacks %q", c)
		}
	}
	if strings.Contains(hflat, "cursor_execution_pending_3b") || strings.Contains(strings.ToLower(hflat), "is qualified") {
		return fmt.Errorf("the historical subsection keeps a retired or promoting claim")
	}
	return nil
}

func TestCursor3bFP4OwnerProcedure(t *testing.T) {
	doc := string(repoFile(t, "docs/real-e2e.md"))
	if err := checkCursorProcedure(doc); err != nil {
		t.Fatal(err)
	}
	// UT-B4: every command, scope, selection, executable, smoke, bound,
	// directory, qualification, check, audit, stop and status mutation,
	// and every promoting claim, is refused.
	probe, exercise := cursorCommandTemplates[0], cursorCommandTemplates[2]
	mutations := [][2]string{
		{cursorStatusPin, strings.Replace(cursorStatusPin, "pending.", "complete.", 1)},
		{cursorStatusPin, strings.Replace(cursorStatusPin, "The 2026-10-27 slice 3a receipt is not qualification. ", "", 1)},
		{probe + "\n", ""},
		{probe, strings.Replace(probe, "--scenario both", "--scenario full", 1)},
		{probe, strings.Replace(probe, "--cursor /abs/cursor-agent ", "", 1)},
		{exercise, strings.Replace(exercise, "flow-cursor-coder.json", "flow.json", 1)},
		{exercise, strings.Replace(exercise, "--cursor /abs/cursor-agent", "--cursor /abs/other-cursor-agent", 1)},
		{cursorCommandTemplates[1] + "\n", ""},
		{cursorCommandTemplates[4] + "\n", ""},
		{cursorCommandTemplates[5] + "\n", ""},
		{`"$CURSOR_PROBE_EVIDENCE"` + "\n/abs", `/abs/callsheet/design/real-e2e-runs/20261027-cursor-probe1` + "\n/abs"},
		{"[pre-probe smoke](#pre-probe-smoke) first", "Cursor smoke first"},
		{"it is non-qualifying even when it passes", "it qualifies when it passes"},
		{"fresh absolute nonexistent directory", "directory"},
		{"A fresh Cursor probe writes qualification.json and exits 0 only after its independent check qualifies it.", "A Cursor probe writes qualification.json."},
		{"A slice 3a observation cannot qualify Cursor;", "A slice 3a observation qualifies Cursor;"},
		// The required clause itself (design 12a-3c-cursor-binding's second
		// binding paragraph also says "not a startup file lookup").
		{"Fresh qualification is an operator prerequisite, not a startup file lookup", "Fresh qualification is an operator prerequisite, a startup file lookup"},
		{"The owner stops unless the fresh both qualification and its offline check agree", "The owner continues"},
		{"keep the evidence and stop: no retry around a refusal", "keep the evidence and stop: a retry around a refusal"},
		{"at most 12 worker calls", "at most 24 worker calls"},
		{"at most 33 worker calls", "at most 34 worker calls"},
		{"every applicable judgment row in each child's audit.md", "the full child's audit.md"},
		{"Root aggregation cannot replace either child judgment audit.", "The root aggregation replaces the child audits."},
		{"a probe alone, a full child alone or an unaudited exercise is not completion", "a probe alone is completion"},
		{"Record A2 as unobserved", "Record A2 as observed"},
		{"There is no jail and no confinement claim.", "Cursor runs in a jail."},
		{"Fresh qualification is an operator prerequisite", "Fresh qualification is looks up a qualification at startup; an operator prerequisite"},
		{"The 2026-10-27 slice 3a receipt was a diagnostic-only observation, not qualification.", "The 2026-10-27 slice 3a receipt is qualified."},
		{"### Historical slice 3a diagnostic observation", "### Cursor coder diagnostic probe (slice 3a)"},
		{"never rewritten or promoted", "promoted"},
	}
	for _, m := range mutations {
		if !strings.Contains(doc, m[0]) {
			t.Fatalf("the mutation target %q is not in docs/real-e2e.md", m[0])
		}
		if checkCursorProcedure(strings.Replace(doc, m[0], m[1], 1)) == nil {
			t.Errorf("the procedure mutation %q -> %q was accepted", m[0], m[1])
		}
	}
	// A promoting or startup-lookup claim added anywhere in the current
	// procedure is refused.
	for _, claim := range []string{"The harness looks up a qualification file at startup.", "A probe-11 pass is qualified automatically for Cursor.",
		"Cursor coder status: observed.", "Commands refuse a Cursor flow with cursor_execution_pending_3b."} {
		if checkCursorProcedure(strings.Replace(doc, cursorStatusPin, cursorStatusPin+" "+claim, 1)) == nil {
			t.Errorf("the claim %q was accepted", claim)
		}
	}
}

// cursorBindingParagraphs are design 12a-3c-cursor-binding's three
// normative paragraphs, in order, right after the status pin.
var cursorBindingParagraphs = []string{
	"Cursor identity is the symlink-resolved target of the explicit --cursor path plus that file's SHA-256. The harness rechecks the original path before vendor execution, after cleanup and before starting the light child. A changed target fails even when the wrapper bytes are identical (`cursor_executable_mismatch`); a change observed during the full attempt also prevents that attempt from passing. A between-children mismatch leaves the light child unstarted.",
	"The offline exercise check compares stored bindings and the complete stored Cursor version strings of reached children that recorded `versions_checked`. The root stores a binding, not a version; qualification is still an owner prerequisite, not a startup file lookup. Stored paths are labels and are never opened offline. Check historical bundles with the revision recorded in their manifest or qualification record (for an exercise, its reached child manifests); do not rewrite them to claim the stronger binding.",
	"After this binding fix, requalify with a fresh probe and combined exercise on one unchanged Cursor version. This check detects changes at its observation boundaries; it does not hash the wrapper's dependencies or provide confinement.",
}

// cursorBindingClauses are the two rewritten binding clauses, and
// cursorHashOnlyClauses the hash-only claims they replace.
var (
	cursorBindingClauses = []string{
		"binds the explicit executable by its resolved target and file SHA-256 before any vendor runs and rechecks that identity after cleanup (a changed or unreadable identity is `cursor_executable_mismatch`)",
		"the selection bytes and the executable identity are frozen before the cost authorization, both children capture those bytes and must carry the root's binding, and a changed identity or previously observed binding failure stops before the light child starts",
	}
	cursorHashOnlyClauses = []string{
		"binds the explicit executable by its SHA-256 before any vendor runs and again after cleanup",
		"a changed or unreadable executable is `cursor_executable_mismatch`",
		"the selection bytes and the executable are frozen before the cost authorization",
		"a changed executable stops before the light child's paid work",
	}
)

// checkCursorBinding is the binding documentation's validator: the owner
// procedure still validates, the three paragraphs follow the status pin in
// order before the optional-selection paragraph, the rewritten clauses are
// present and no hash-only claim remains.
func checkCursorBinding(doc string) error {
	if err := checkCursorProcedure(doc); err != nil {
		return err
	}
	current, _, err := cursorSections(doc)
	if err != nil {
		return err
	}
	flat := flatText(current)
	at := strings.Index(flat, cursorStatusPin)
	for _, p := range cursorBindingParagraphs {
		i := strings.Index(flat, flatText(p))
		if i < 0 || i < at || strings.Count(flat, flatText(p)) != 1 {
			return fmt.Errorf("the binding paragraph %.60q is missing, repeated or out of order", p)
		}
		at = i
	}
	if sel := strings.Index(flat, "The optional coder-only selection"); sel < at {
		return fmt.Errorf("the binding paragraphs are not before the optional-selection paragraph")
	}
	for _, c := range cursorBindingClauses {
		if !strings.Contains(flat, flatText(c)) {
			return fmt.Errorf("the procedure lacks %q", c)
		}
	}
	for _, c := range cursorHashOnlyClauses {
		if strings.Contains(flat, flatText(c)) {
			return fmt.Errorf("the procedure keeps the hash-only claim %q", c)
		}
	}
	return nil
}

func TestCursorBindingFP5(t *testing.T) {
	doc := string(repoFile(t, "docs/real-e2e.md"))
	if err := checkCursorBinding(doc); err != nil {
		t.Fatal(err)
	}
	// UT-5: deleting or weakening each new clause, restoring a hash-only
	// claim, reordering the paragraphs or adding a promoting claim is
	// refused; the existing procedure pins still apply.
	var mutations [][2]string
	for _, p := range cursorBindingParagraphs {
		mutations = append(mutations, [2]string{p + "\n\n", ""})
	}
	for _, c := range cursorBindingClauses {
		mutations = append(mutations, [2]string{c, ""})
	}
	mutations = append(mutations,
		[2]string{"symlink-resolved target of the explicit --cursor path plus", "SHA-256 of the explicit --cursor path plus"},
		[2]string{"A changed target fails even when the wrapper bytes are identical", "A changed target passes when the wrapper bytes are identical"},
		[2]string{"leaves the light child unstarted", "lets the light child start"},
		[2]string{"the complete stored Cursor version strings", "the stored Cursor version dates"},
		[2]string{"Stored paths are labels and are never opened offline.", "Stored paths are opened offline."},
		[2]string{"with the revision recorded in their manifest or qualification record", "with the current checkout"},
		[2]string{"requalify with a fresh probe and combined exercise on one unchanged Cursor version", "keep the earlier qualification"},
		[2]string{"it does not hash the wrapper's dependencies or provide confinement", "it hashes the wrapper's dependencies"},
		[2]string{cursorBindingClauses[0], "binds the explicit executable by its SHA-256 before any vendor runs and again after cleanup (a changed or unreadable executable is `cursor_executable_mismatch`)"},
		[2]string{cursorBindingClauses[1], "the selection bytes and the executable are frozen before the cost authorization, both children capture those bytes and must carry the root's binding, a changed executable stops before the light child's paid work"},
		[2]string{cursorBindingParagraphs[0] + "\n\n" + cursorBindingParagraphs[1], cursorBindingParagraphs[1] + "\n\n" + cursorBindingParagraphs[0]},
		[2]string{cursorStatusPin, strings.Replace(cursorStatusPin, "pending.", "complete.", 1)},
	)
	for _, m := range mutations {
		if !strings.Contains(doc, m[0]) {
			t.Fatalf("the mutation target %q is not in docs/real-e2e.md", m[0])
		}
		if checkCursorBinding(strings.Replace(doc, m[0], m[1], 1)) == nil {
			t.Errorf("the binding mutation %.80q -> %.80q was accepted", m[0], m[1])
		}
	}
	// A paragraph moved after the optional-selection paragraph is refused.
	moved := strings.Replace(doc, cursorBindingParagraphs[2]+"\n\n", "", 1)
	moved = strings.Replace(moved, "### Cursor owner procedure", cursorBindingParagraphs[2]+"\n\n### Cursor owner procedure", 1)
	if checkCursorBinding(moved) == nil {
		t.Error("a binding paragraph after the optional-selection paragraph was accepted")
	}
	for _, claim := range []string{"Cursor coder status: qualified.", "The harness looks up a qualification file at startup."} {
		if checkCursorBinding(strings.Replace(doc, cursorBindingParagraphs[2], cursorBindingParagraphs[2]+" "+claim, 1)) == nil {
			t.Errorf("the claim %q was accepted", claim)
		}
	}
}

// cursorCatalogClauses are each delivery document's required Cursor
// catalog, posture, default and history clauses.
var cursorCatalogClauses = map[string][]string{
	"docs/real-adapters.md": {
		"### Optional Cursor coder in the development flow",
		"The real end-to-end development flow can select Cursor for its coder only",
		"adapter `cursor`, model `grok-4.7`, effort `high` (model argument `grok-4.7-high`)",
		"The default flow is unchanged, with its Claude coder; the designer, the code reviewer and triage stay on Codex and the design reviewer on Claude, and no Cursor reviewer or triage selection ships.",
		"The separate four-hop sample in which Cursor reviews code is a different scenario, not exercised by this selection.",
		"Live execution is Linux-only.",
		"the minimum version `2026.10.01-e373342` and the posture checks are unchanged",
		"no `--force`, `--yolo`, `--approve-mcps`, `--sandbox` or permission fallback, no jail and no confinement claim",
		"run it under a dedicated OS user",
		"is observed report-only",
		"cannot attribute a change to Cursor; only the strict protected snapshots gate the owner environment",
		"that observation is not qualification, and headless approval behaviour remains unexercised",
		"A fresh qualification probe and its independent check are an operator prerequisite before any Cursor attempt",
	},
	"docs/real-e2e.md": {
		"For the optional Cursor coder only: an eligible Cursor executable (`cursor-agent`) at an absolute `--cursor` path on Linux, deployed as a fourth sidecar beside the idle Grok node",
		"The optional coder-only Cursor selection [examples/real-e2e/dev-full/flow-cursor-coder.json](examples/real-e2e/dev-full/flow-cursor-coder.json) is described in [Cursor coder qualification](#cursor-coder-qualification).",
		"The default flow, the Claude design reviewer, the Codex code reviewer and triage, the minimum versions and every gate are unchanged; the run is labelled `override-flow`, and its PASS never demonstrates the default pairs or approves model judgment.",
		"Cursor runs the unchanged recipe `-p --trust --auto-review --output-format json --model grok-4.7-high <prompt>` with the sidecar user's ordinary access; run the sidecars under a dedicated OS user.",
		"No `--force`, `--yolo`, `--approve-mcps`, `--sandbox`, canary, permission fallback or configuration write is added",
		"They are report-only observations of all processes, not attribution to Cursor, not recursive totals and not a complete diff",
		"deep or transient writes, other home paths (`.local/bin`, `.npm`), reads and network are not covered",
		"Headless approval behaviour under `--auto-review` has not been exercised",
		// Preserved history and default pins.
		"Live status: observed; roadmap 12a's live condition is MET.",
		"Probes 8 and 9b, both with Grok triage, failed and stay failed history",
		"Probe 10, with Codex triage, also remains failed history",
		"**Observed on 2026-10-26:** this installed Codex recipe reproduced and cleaned up under the triage envelope.",
		"Combined exercise `20261026T102944Z-d3827a6b` passed on main `387759a`",
	},
	"docs/examples/real-e2e/README.md": {
		"[dev-full/flow-cursor-coder.json](dev-full/flow-cursor-coder.json): the optional coder-only Cursor selection (`cursor`, `grok-4.7`, `high`); the other four rows are the default's.",
		"Linux only, with an explicit `--cursor` executable and a fresh checked qualification first; no jail or confinement claim, and cache and temporary samples are report-only.",
		"[dev-full/flow.json](dev-full/flow.json): the five frozen role selections",
	},
	"docs/examples/dev-flow/README.md": {
		"**Optional Cursor coder.** An owner may select the coder alone on the `cursor` adapter with its Grok model at effort `high`, exactly as the real end-to-end exercise ships it in `docs/examples/real-e2e/dev-full/flow-cursor-coder.json`",
		"the default table above is unchanged and the code reviewer and triage stay on Codex",
		"the unchanged `--trust --auto-review` recipe is no jail and no confinement claim, and the sidecar keeps its user's ordinary access",
		"It stays cross-family: the GPT code reviewer and triage judge a Grok implementation; Cursor is the tool, not the model family.",
		"The four-hop sample in which Cursor reviews code is a separate scenario, not exercised by this selection.",
		"| coder | claude | claude-opus-5-5 | high | 1 | 10m |",
	},
	"docs/examples/dev-flow/coordinator-playbook.md": {
		"An owner's optional selection may put the coder alone on the `cursor` adapter with its Grok model at effort `high`; the other four rows stay as shown, and the code reviewer (GPT) then checks a Grok implementation, still cross-family.",
		"| coder | claude | claude-opus-5-5 | high | 1 | 10m |",
	},
}

// cursorCatalogForbidden are claims no delivery document may make about
// the optional Cursor coder.
var cursorCatalogForbidden = []string{"Cursor is qualified", "Cursor coder is qualified", "the default coder is Cursor", "Cursor reviews the code in this flow",
	"jailed Cursor", "confines Cursor", "confinement is enforced", "Cursor triage", "--yolo is passed", "headless approvals work"}

// checkCursorCatalog is the delivery documentation's independent
// validator over each document's text.
func checkCursorCatalog(docs map[string]string) error {
	for rel, clauses := range cursorCatalogClauses {
		flat := flatText(docs[rel])
		for _, c := range clauses {
			if !strings.Contains(flat, flatText(c)) {
				return fmt.Errorf("%s lacks %q", rel, c)
			}
		}
		lower := strings.ToLower(flat)
		for _, f := range cursorCatalogForbidden {
			if strings.Contains(lower, strings.ToLower(f)) {
				return fmt.Errorf("%s claims %q", rel, f)
			}
		}
	}
	return nil
}

func TestCursor3bFP5Delivery(t *testing.T) {
	docs := map[string]string{}
	for rel := range cursorCatalogClauses {
		docs[rel] = string(repoFile(t, rel))
	}
	if err := checkCursorCatalog(docs); err != nil {
		t.Fatal(err)
	}
	// UT-B5: deleting or substituting each catalog, posture, default and
	// history clause is refused.
	for rel, clauses := range cursorCatalogClauses {
		for _, c := range clauses {
			target := c
			if !strings.Contains(docs[rel], target) {
				// A wrapped clause: mutate its first word run instead.
				target = strings.Fields(c)[0] + " " + strings.Fields(c)[1]
				if !strings.Contains(flatText(docs[rel]), flatText(c)) {
					t.Fatalf("%s lacks %q", rel, c)
				}
				mutated := map[string]string{}
				for k, v := range docs {
					mutated[k] = v
				}
				mutated[rel] = flatText(docs[rel])
				mutated[rel] = strings.Replace(mutated[rel], flatText(c), "", 1)
				if checkCursorCatalog(mutated) == nil {
					t.Errorf("%s: deleting %q was accepted", rel, c)
				}
				continue
			}
			mutated := map[string]string{}
			for k, v := range docs {
				mutated[k] = v
			}
			mutated[rel] = strings.Replace(docs[rel], target, "", 1)
			if checkCursorCatalog(mutated) == nil {
				t.Errorf("%s: deleting %q was accepted", rel, c)
			}
		}
	}
	for _, s := range []struct{ rel, from, to string }{
		{"docs/real-adapters.md", "for its coder only", "for every role"},
		{"docs/real-adapters.md", "The default flow is unchanged", "The default flow is now Cursor"},
		{"docs/real-adapters.md", "no jail and no confinement claim", "a jail"},
		{"docs/examples/dev-flow/README.md", "not exercised by this selection", "exercised by this selection"},
		{"docs/examples/real-e2e/README.md", "report-only", "enforced"},
		{"docs/real-e2e.md", "Probes 8 and 9b, both with Grok triage, failed", "Probes 8 and 9b, both with Grok triage, passed"},
	} {
		mutated := map[string]string{}
		for k, v := range docs {
			mutated[k] = v
		}
		if !strings.Contains(flatText(docs[s.rel]), s.from) {
			t.Fatalf("%s lacks the substitution target %q", s.rel, s.from)
		}
		mutated[s.rel] = strings.Replace(flatText(docs[s.rel]), s.from, s.to, 1)
		if checkCursorCatalog(mutated) == nil {
			t.Errorf("%s: substituting %q -> %q was accepted", s.rel, s.from, s.to)
		}
	}
	for _, claim := range cursorCatalogForbidden {
		mutated := map[string]string{}
		for k, v := range docs {
			mutated[k] = v
		}
		mutated["docs/examples/real-e2e/README.md"] += "\n" + claim + ".\n"
		if checkCursorCatalog(mutated) == nil {
			t.Errorf("the claim %q was accepted", claim)
		}
	}
}
