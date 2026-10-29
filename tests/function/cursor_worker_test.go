package function

import (
	"fmt"
	"strings"
	"testing"
)

// Function and unit tests of design 12a-3-cursor-worker, slice 3b FP-4
// (parent FP-10, the owner procedure) and FP-5 (parent FP-11, the Cursor
// catalog, posture and delivery documentation), and of design
// 12a-3-cursor-closeout FP-1 to FP-7 (the owner's recorded completion of
// slice 3; its FP-8 neutral operator paths are in codex_triage_test.go). The validators are pure functions over the documentation text;
// each table-driven mutation must make its validator refuse. They validate
// the procedure and the recorded documentary claims, never a paid result,
// and never read the cited evidence.

// cursorStatusPin is the current Cursor status, verbatim: complete for the
// recorded binary identity and selection (design 12a-3-cursor-closeout,
// FP-1).
const cursorStatusPin = "Cursor coder status: complete for the recorded binary identity and selection; roadmap 12a slice 3, the optional Cursor coder, was closed by the owner on 2026-10-28. Completion rests on the fresh QUALIFIED both-scope Cursor probe and audited combined full/light exercise recorded below, all using Cursor `2026.10.22-27dcf5f` after the binding fix in #76. A2 remains unobserved: no approval-needing action occurred under `--auto-review`. There is no jail and no confinement claim; activity samples are report-only. A changed relevant binary identity, version or selection requires owner-authorized requalification before a later live run; this is not a permanent vendor qualification. The 2026-10-27 slice 3a receipt is not qualification. Default-selection probe 11 and its combined exercise do not qualify Cursor."

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
// make: an automatic or startup qualification, a promoted diagnostic, an
// observed or qualified alternative Cursor status label, or the retired 3a
// refusal. (The scoped complete status is pinned instead; design
// 12a-3-cursor-closeout, Offline validators.)
var cursorProcedureForbidden = []string{
	"Cursor coder status: observed", "Cursor coder status: qualified", "qualified automatically",
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
// verbatim scoped status, the six literal command templates in one shell
// block with one selection and one executable throughout, every required
// clause, no forbidden claim, the historical subsection's past-tense
// diagnostic record and the closeout validators (checkCursorCloseout).
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
	return checkCursorCloseout(doc)
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
		{cursorStatusPin, strings.Replace(cursorStatusPin, "Cursor coder status: complete for", "Cursor coder status: pending for", 1)},
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
		if m[0] == m[1] {
			t.Fatalf("the mutation of %.80q is a no-op", m[0])
		}
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
		[2]string{cursorStatusPin, strings.Replace(cursorStatusPin, "complete for the recorded binary identity and selection;", "pending fresh qualification;", 1)},
	)
	for _, m := range mutations {
		if m[0] == m[1] {
			t.Fatalf("the mutation of %.80q is a no-op", m[0])
		}
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

// Design 12a-3-cursor-closeout: the literal pins of its qualification
// wording (FP-1 to FP-5), its status consistency (FP-6) and its citation
// form (FP-7). Paths in the pins are documentary citation labels; no
// validator opens them.

// cursorNavigationPin is FP-1's navigation paragraph in the introduction,
// after the unchanged default live status.
const cursorNavigationPin = "The default-selection observations above remain separate from the optional Cursor coder. Roadmap 12a slice 3 is complete for the recorded Cursor binary identity and selection; see [Cursor coder qualification](#cursor-coder-qualification) for its 2026-10-28 requalification, audited combined exercise, limits and future requalification procedure."

// cursorProbePin is FP-2's qualified-probe paragraph, after #76's three
// binding paragraphs.
const cursorProbePin = "The requalification required after #76 is recorded on 2026-10-28: probe `20261028T201303Z-b2fcc67a` is `QUALIFIED` with `--scenario both` at revision `6e57006ad5a7bbe01fff4e4476650bdff6e1c47e`, using Cursor `2026.10.22-27dcf5f` and coder `cursor / grok-4.7 / high`. Its `callsheet-dev-cursor-qualification/v1` record is `design/real-e2e-runs/20261028-cursor-qual2/qualification.json`; all seven stages passed. Its recorded Cursor identity is the resolved target ending `versions/2026.10.22-27dcf5f/cursor-agent` and file SHA-256 `2ccc9a8e167797641448b5e5c936f006ba137a2555f117f38c5eb76a5238a233`, equal before and after. The external coordinator's record `design/real-e2e-runs/20261028-cursor-exercise2/attempts/full/audit-review.md` also records this probe's offline check as QUALIFIED with codes `[]`."

// cursorExercisePin is FP-3's audited-exercise paragraph, right after the
// qualified probe.
const cursorExercisePin = "Combined Cursor exercise `20261028T203540Z-11107d40` is `PASS`, recorded in `design/real-e2e-runs/20261028-cursor-exercise2/exercise.json`, `design/real-e2e-runs/20261028-cursor-exercise2/report.txt` and `design/real-e2e-runs/20261028-cursor-exercise2/report.json`, with 17 worker calls. Full child `20261028T203547Z-d9d43e67` is mechanical PASS with 9 calls; light child `20261028T204730Z-d51eefed` is mechanical PASS with 8 calls. Their manifests `design/real-e2e-runs/20261028-cursor-exercise2/attempts/full/manifest.json` and `design/real-e2e-runs/20261028-cursor-exercise2/attempts/light/manifest.json` record the same revision `6e57006ad5a7bbe01fff4e4476650bdff6e1c47e` and Cursor version `2026.10.22-27dcf5f` as the probe; the probe, root and both children carry the same resolved-target binding and equal before/after file hashes. The external coordinator's 2026-10-28 judgment audits `design/real-e2e-runs/20261028-cursor-exercise2/attempts/full/audit-review.md` and `design/real-e2e-runs/20261028-cursor-exercise2/attempts/light/audit-review.md` each record all four judgment rows as correct and no disputed mechanical row. Those audit records also state that offline checks of the root and both children exited 0 and matched their canonical report.json. The mechanical reports alone do not approve model judgment; the separate child audits satisfy that requirement without rewriting the canonical reports."

// cursorSupersededHeading and cursorSupersededPin are FP-4's superseded
// history, the subsection between the owner procedure and the slice 3a
// history.
const (
	cursorSupersededHeading = "### Superseded first Cursor qualification and exercise"
	cursorSupersededPin     = "The earlier probe `design/real-e2e-runs/20261028-cursor-qual1/qualification.json` recorded QUALIFIED for Cursor `2026.10.14-0f9edde` at revision `167d38f508421035d54db4b72cd5549133db0ba5`. The first combined exercise `20261028T074203Z-f6ae17b1`, recorded in `design/real-e2e-runs/20261028-cursor-exercise/report.txt`, recorded mechanical PASS but does not complete slice 3. Cursor self-updated to `2026.10.22-27dcf5f` during the full child's coder run (task started 07:52:08Z on the qualified `2026.10.14-0f9edde`; the new version directory appeared 07:52:30Z). The light child then ran the unqualified version. The former shim-hash binding missed that change because the wrapper bytes were unchanged; #76 fixed that gap with resolved-target binding and child-version comparison as described above. The dated 2026-10-28 addenda in `design/real-e2e-runs/20261028-cursor-exercise/attempts/full/audit-review.md` and `design/real-e2e-runs/20261028-cursor-exercise/attempts/light/audit-review.md` record the owner's decision to fix the binding and requalify. Qual1 and the first exercise are superseded history, never completion evidence. Keep their original records unchanged and use their producing revision for historical checking; a historical mechanical PASS does not establish the stronger binding or completion."
)

// cursorCompletionRulePin is FP-5's completion rule, the owner procedure's
// final sentences, and cursorInvalidationSentence the existing full
// invalidation sentence that the scoped status does not narrow.
const (
	cursorCompletionRulePin    = "For any Cursor closeout, completion requires the fresh qualification, the combined exercise with both children and the root passing, and both external child judgment audits to be recorded; a probe alone, a full child alone or an unaudited exercise is not completion. The qual2 and exercise2 records above meet that rule for their recorded binary identity and selection; future requalification follows this same owner procedure."
	cursorInvalidationSentence = "Any changed relevant binary/version, manual, policy, account context or selection invalidates use of this qualification for a later live run; owner disposition is required rather than silently spending another probe."
)

// cursorOptionalSelection opens the optional-selection paragraph that the
// closeout evidence precedes.
const cursorOptionalSelection = "The optional coder-only selection"

// cursorStaleStatus are the replaced pending status's prefix and clause,
// refused anywhere in the runbook (FP-6).
var cursorStaleStatus = []string{"Cursor coder status: diagnostic observed", "fresh qualification, combined full/light execution and external child judgment audits pending"}

// cursorSupersededOnly are FP-6's exclusive-occurrence strings: the
// superseded run IDs, bundle directories and revision, allowed only inside
// the superseded history. Version strings are deliberately not in the set.
var cursorSupersededOnly = []string{"20261028T065706Z-4f7cb8ea", "20261028T074203Z-f6ae17b1", "20261028T074210Z-70dc1229", "20261028T075929Z-560a5bdc",
	"20261028-cursor-qual1/", "20261028-cursor-exercise/", "167d38f508421035d54db4b72cd5549133db0ba5"}

// cursorCitation is one FP-7 evidence citation: the closeout part citing
// it ("cursor" for the Cursor section before its owner procedure,
// "superseded" for the superseded history), its repository-relative path
// and how many times that part cites it.
type cursorCitation struct {
	part, path string
	count      int
}

// cursorCitations are every new and historical evidence citation of the
// closeout wording.
var cursorCitations = []cursorCitation{
	{"cursor", "design/real-e2e-runs/20261028-cursor-qual2/qualification.json", 1},
	{"cursor", "design/real-e2e-runs/20261028-cursor-exercise2/exercise.json", 1},
	{"cursor", "design/real-e2e-runs/20261028-cursor-exercise2/report.txt", 1},
	{"cursor", "design/real-e2e-runs/20261028-cursor-exercise2/report.json", 1},
	{"cursor", "design/real-e2e-runs/20261028-cursor-exercise2/attempts/full/manifest.json", 1},
	{"cursor", "design/real-e2e-runs/20261028-cursor-exercise2/attempts/light/manifest.json", 1},
	{"cursor", "design/real-e2e-runs/20261028-cursor-exercise2/attempts/full/audit-review.md", 2},
	{"cursor", "design/real-e2e-runs/20261028-cursor-exercise2/attempts/light/audit-review.md", 1},
	{"superseded", "design/real-e2e-runs/20261028-cursor-qual1/qualification.json", 1},
	{"superseded", "design/real-e2e-runs/20261028-cursor-exercise/report.txt", 1},
	{"superseded", "design/real-e2e-runs/20261028-cursor-exercise/attempts/full/audit-review.md", 1},
	{"superseded", "design/real-e2e-runs/20261028-cursor-exercise/attempts/light/audit-review.md", 1},
}

// cursorCloseoutParts are the runbook parts the closeout validators read.
type cursorCloseoutParts struct {
	intro      string // before the first level-two heading
	cursor     string // the Cursor section up to its owner procedure
	procedure  string // the owner procedure, up to the superseded history
	superseded string // the superseded history, up to the slice 3a history
}

// cursorCloseoutSections splits the runbook into its closeout parts; the
// superseded-history heading occurs once, after the owner procedure and
// before the slice 3a history (cursorSections' boundaries are unchanged).
func cursorCloseoutSections(doc string) (cursorCloseoutParts, error) {
	var p cursorCloseoutParts
	current, _, err := cursorSections(doc)
	if err != nil {
		return p, err
	}
	const proc, sup = "\n### Cursor owner procedure\n", "\n" + cursorSupersededHeading + "\n"
	i, j := strings.Index(current, proc), strings.Index(current, sup)
	if i < 0 || strings.Count(doc, proc) != 1 {
		return p, fmt.Errorf("the Cursor section lacks its single owner procedure")
	}
	if j < i || strings.Count(doc, sup) != 1 {
		return p, fmt.Errorf("the superseded history is not one subsection between the owner procedure and the slice 3a history")
	}
	p.intro, _, _ = strings.Cut(doc, "\n## ")
	p.cursor, p.procedure, p.superseded = current[:i], current[i+len(proc):j], current[j+len(sup):]
	return p, nil
}

// cursorPinOnce requires pin once in part and once in the whole runbook,
// comparing whitespace-flattened text on both sides.
func cursorPinOnce(doc, part, pin, what string) error {
	if p := flatText(pin); strings.Count(flatText(part), p) != 1 || strings.Count(flatText(doc), p) != 1 {
		return fmt.Errorf("the %s is not pinned verbatim once in its section", what)
	}
	return nil
}

// cursorInOrder requires each text to occur in flat after the end of the
// previous one.
func cursorInOrder(flat string, texts ...string) error {
	at := 0
	for _, s := range texts {
		f := flatText(s)
		i := strings.Index(flat[at:], f)
		if i < 0 {
			return fmt.Errorf("%.60q is missing or out of order", s)
		}
		at += i + len(f)
	}
	return nil
}

// checkCursorCloseoutStatus validates FP-1: the scoped status once in the
// Cursor section, and the navigation paragraph once in the introduction
// after the default live status.
func checkCursorCloseoutStatus(doc string) error {
	p, err := cursorCloseoutSections(doc)
	if err != nil {
		return err
	}
	if err := cursorPinOnce(doc, p.cursor, cursorStatusPin, "scoped Cursor status"); err != nil {
		return err
	}
	if err := cursorPinOnce(doc, p.intro, cursorNavigationPin, "Cursor navigation paragraph"); err != nil {
		return err
	}
	return cursorInOrder(flatText(p.intro), "Live status:", cursorNavigationPin)
}

// checkCursorCloseoutProbe validates FP-2: the qualified-probe paragraph
// once, after the binding paragraphs and before the optional selection.
func checkCursorCloseoutProbe(doc string) error {
	p, err := cursorCloseoutSections(doc)
	if err != nil {
		return err
	}
	if err := cursorPinOnce(doc, p.cursor, cursorProbePin, "qualified-probe paragraph"); err != nil {
		return err
	}
	return cursorInOrder(flatText(p.cursor), cursorBindingParagraphs[2], cursorProbePin, cursorOptionalSelection)
}

// checkCursorCloseoutExercise validates FP-3: the audited-exercise
// paragraph once, after the qualified probe and before the optional
// selection.
func checkCursorCloseoutExercise(doc string) error {
	p, err := cursorCloseoutSections(doc)
	if err != nil {
		return err
	}
	if err := cursorPinOnce(doc, p.cursor, cursorExercisePin, "audited-exercise paragraph"); err != nil {
		return err
	}
	return cursorInOrder(flatText(p.cursor), cursorBindingParagraphs[2], cursorProbePin, cursorExercisePin, cursorOptionalSelection)
}

// checkCursorCloseoutHistory validates FP-4: one superseded-history
// heading bounded by the slice 3a history, holding its paragraph once.
func checkCursorCloseoutHistory(doc string) error {
	p, err := cursorCloseoutSections(doc)
	if err != nil {
		return err
	}
	return cursorPinOnce(doc, p.superseded, cursorSupersededPin, "superseded-history paragraph")
}

// checkCursorCloseoutRule validates FP-5: the completion rule and the full
// invalidation sentence once in the owner procedure, and no replaced
// completion sentence.
func checkCursorCloseoutRule(doc string) error {
	p, err := cursorCloseoutSections(doc)
	if err != nil {
		return err
	}
	if err := cursorPinOnce(doc, p.procedure, cursorCompletionRulePin, "completion rule"); err != nil {
		return err
	}
	if err := cursorPinOnce(doc, p.procedure, cursorInvalidationSentence, "invalidation sentence"); err != nil {
		return err
	}
	if strings.Contains(flatText(doc), "Cursor is complete only when") {
		return fmt.Errorf("the replaced completion sentence remains")
	}
	return nil
}

// checkCursorCloseoutConsistency validates FP-6: exactly one Cursor coder
// status, no stale pending status anywhere, the superseded history holding
// only its paragraph, and the superseded IDs, directories and revision
// nowhere else.
func checkCursorCloseoutConsistency(doc string) error {
	lower := strings.ToLower(flatText(doc))
	if n := strings.Count(lower, "cursor coder status:"); n != 1 {
		return fmt.Errorf("the runbook states %d Cursor coder statuses, not one", n)
	}
	for _, s := range cursorStaleStatus {
		if strings.Contains(lower, strings.ToLower(s)) {
			return fmt.Errorf("the runbook keeps the stale status %q", s)
		}
	}
	p, err := cursorCloseoutSections(doc)
	if err != nil {
		return err
	}
	if flatText(p.superseded) != flatText(cursorSupersededPin) {
		return fmt.Errorf("the superseded history holds more than its paragraph")
	}
	for _, s := range cursorSupersededOnly {
		if strings.Count(doc, s) != strings.Count(p.superseded, s) {
			return fmt.Errorf("%q occurs outside the superseded history", s)
		}
	}
	return nil
}

// checkCursorCloseoutCitations validates FP-7: no Markdown evidence link
// (the existing checkNoEvidenceLink), and every closeout citation an
// exact backticked repository-relative path, cited only that way and as
// often as its section's wording cites it.
func checkCursorCloseoutCitations(doc string) error {
	if err := checkNoEvidenceLink(doc); err != nil {
		return err
	}
	p, err := cursorCloseoutSections(doc)
	if err != nil {
		return err
	}
	parts := map[string]string{"cursor": flatText(p.cursor), "superseded": flatText(p.superseded)}
	for _, c := range cursorCitations {
		sec := parts[c.part]
		if strings.Count(sec, "`"+c.path+"`") != c.count || strings.Count(sec, c.path) != c.count {
			return fmt.Errorf("%s is not cited %d time(s) as a backticked repository-relative path in its section", c.path, c.count)
		}
	}
	return nil
}

// checkCursorCloseout is the closeout's aggregate validator over its eight
// helpers (FP-8's checkCursorCloseoutPaths is in codex_triage_test.go). It
// calls neither checkCursorProcedure (which calls it) nor
// checkCursorBinding.
func checkCursorCloseout(doc string) error {
	for _, check := range []func(string) error{checkCursorCloseoutStatus, checkCursorCloseoutProbe, checkCursorCloseoutExercise,
		checkCursorCloseoutHistory, checkCursorCloseoutRule, checkCursorCloseoutConsistency, checkCursorCloseoutCitations, checkCursorCloseoutPaths} {
		if err := check(doc); err != nil {
			return err
		}
	}
	return nil
}

// cursorRunbook is the one-entry document map mutateExpect reads.
func cursorRunbook(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{"e2e": string(repoFile(t, "docs/real-e2e.md"))}
}

// onCursorRunbook adapts a runbook validator to mutateExpect.
func onCursorRunbook(check func(string) error) func(map[string]string) error {
	return func(docs map[string]string) error { return check(docs["e2e"]) }
}

// cursorEdits are mutations of pin, each replacing its first from with to.
func cursorEdits(pin string, edits ...[2]string) [][2]string {
	out := make([][2]string, len(edits))
	for i, e := range edits {
		out[i] = [2]string{pin, strings.Replace(pin, e[0], e[1], 1)}
	}
	return out
}

// cursorChanged asserts that every assembled mutation changes its target.
func cursorChanged(t *testing.T, muts [][2]string) [][2]string {
	t.Helper()
	for _, m := range muts {
		if m[0] == m[1] {
			t.Fatalf("the mutation of %.80q is a no-op", m[0])
		}
	}
	return muts
}

// cursorRefused requires check to refuse mutated, a composite mutation of
// doc that a single replacement cannot express.
func cursorRefused(t *testing.T, check func(string) error, what, doc, mutated string) {
	t.Helper()
	if mutated == doc {
		t.Fatalf("%s: the mutation is a no-op", what)
	}
	if check(mutated) == nil {
		t.Errorf("%s was accepted", what)
	}
}

// cursorCloseoutPositive validates the shipped runbook with the aggregate
// and the retained procedure and binding validators.
func cursorCloseoutPositive(t *testing.T, doc string) {
	t.Helper()
	for name, check := range map[string]func(string) error{"closeout": checkCursorCloseout, "procedure": checkCursorProcedure, "binding": checkCursorBinding} {
		if err := check(doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestCursorCloseoutFP1(t *testing.T) {
	docs := cursorRunbook(t)
	cursorCloseoutPositive(t, docs["e2e"])
	sentences := []string{
		"Cursor coder status: complete for the recorded binary identity and selection; roadmap 12a slice 3, the optional Cursor coder, was closed by the owner on 2026-10-28.",
		"Completion rests on the fresh QUALIFIED both-scope Cursor probe and audited combined full/light exercise recorded below, all using Cursor `2026.10.22-27dcf5f` after the binding fix in #76.",
		"A2 remains unobserved: no approval-needing action occurred under `--auto-review`.",
		"There is no jail and no confinement claim; activity samples are report-only.",
		"A changed relevant binary identity, version or selection requires owner-authorized requalification before a later live run; this is not a permanent vendor qualification.",
		"The 2026-10-27 slice 3a receipt is not qualification.",
		"Default-selection probe 11 and its combined exercise do not qualify Cursor.",
	}
	if strings.Join(sentences, " ") != cursorStatusPin {
		t.Fatal("the status sentences do not compose the status pin")
	}
	// Each status sentence dropped, then each substituted or with one
	// caveat removed; the navigation paragraph dropped, substituted,
	// weakened or moved out of the introduction.
	var muts [][2]string
	for _, s := range sentences {
		muts = append(muts, [2]string{cursorStatusPin, strings.Replace(cursorStatusPin, s, "", 1)})
	}
	muts = append(muts, cursorEdits(cursorStatusPin,
		[2]string{"complete for the recorded binary identity and selection", "complete"},
		[2]string{"was closed by the owner on 2026-10-28", "was closed automatically"},
		[2]string{"fresh QUALIFIED both-scope Cursor probe", "QUALIFIED full-scope Cursor probe"},
		[2]string{"audited combined full/light exercise", "combined full exercise"},
		[2]string{"`2026.10.22-27dcf5f`", "`2026.10.14-0f9edde`"},
		[2]string{"after the binding fix in #76", "before the binding fix"},
		[2]string{"A2 remains unobserved: no approval-needing action occurred under `--auto-review`. ", ""},
		[2]string{"A2 remains unobserved", "A2 was observed"},
		[2]string{"There is no jail and no confinement claim; ", ""},
		[2]string{"no jail and no confinement claim", "a jail that confines the coder"},
		[2]string{"; activity samples are report-only", ""},
		[2]string{"activity samples are report-only", "activity samples are enforced"},
		[2]string{"A changed relevant binary identity, version or selection requires owner-authorized requalification before a later live run; ", ""},
		[2]string{"requires owner-authorized requalification before a later live run", "needs no requalification"},
		[2]string{"this is not a permanent vendor qualification", "this is a permanent vendor qualification"},
		[2]string{"The 2026-10-27 slice 3a receipt is not qualification.", "The 2026-10-27 slice 3a receipt also qualifies."},
		[2]string{"Default-selection probe 11 and its combined exercise do not qualify Cursor.", "Default-selection probe 11 also qualifies the coder."},
	)...)
	muts = append(muts,
		[2]string{cursorStatusPin + "\n\n", ""},
		[2]string{cursorNavigationPin + "\n\n", ""},
		[2]string{cursorNavigationPin, "Roadmap 12a slice 3 is complete; see [Cursor coder qualification](#cursor-coder-qualification)."},
		[2]string{"\n\n" + cursorNavigationPin + "\n\n## Prerequisites\n", "\n\n## Prerequisites\n\n" + cursorNavigationPin + "\n"},
	)
	muts = append(muts, cursorEdits(cursorNavigationPin,
		[2]string{"remain separate from the optional Cursor coder", "include the optional Cursor coder"},
		[2]string{"is complete for the recorded Cursor binary identity and selection", "is complete for every Cursor installation"},
		[2]string{"(#cursor-coder-qualification)", "(#cursor-owner-procedure)"},
		[2]string{"its 2026-10-28 requalification", "its 2026-10-27 requalification"},
		[2]string{", limits and future requalification procedure", ""},
	)...)
	mutateExpect(t, docs, onCursorRunbook(checkCursorCloseoutStatus), "e2e", cursorChanged(t, muts))
}

func TestCursorCloseoutFP2(t *testing.T) {
	docs := cursorRunbook(t)
	cursorCloseoutPositive(t, docs["e2e"])
	qual1 := strings.NewReplacer("20261028-cursor-qual2/", "20261028-cursor-qual1/", "20261028T201303Z-b2fcc67a", "20261028T065706Z-4f7cb8ea",
		"6e57006ad5a7bbe01fff4e4476650bdff6e1c47e", "167d38f508421035d54db4b72cd5549133db0ba5", "2026.10.22-27dcf5f", "2026.10.14-0f9edde").Replace(cursorProbePin)
	muts := [][2]string{
		{cursorProbePin + "\n\n", ""},
		{cursorProbePin, qual1},
		// Moved before the third binding paragraph.
		{cursorBindingParagraphs[2] + "\n\n" + cursorProbePin, cursorProbePin + "\n\n" + cursorBindingParagraphs[2]},
	}
	muts = append(muts, cursorEdits(cursorProbePin,
		[2]string{"`20261028T201303Z-b2fcc67a`", "`20261028T201303Z-b2fcc67b`"},
		[2]string{"is `QUALIFIED`", "is `BLOCKED`"},
		[2]string{"`--scenario both`", "`--scenario full`"},
		[2]string{"`callsheet-dev-cursor-qualification/v1`", "`callsheet-dev-qualification/v1`"},
		[2]string{"revision `6e57006ad5a7bbe01fff4e4476650bdff6e1c47e`", "revision `167d38f508421035d54db4b72cd5549133db0ba5`"},
		[2]string{"using Cursor `2026.10.22-27dcf5f`", "using Cursor `2026.10.14-0f9edde`"},
		[2]string{"`cursor / grok-4.7 / high`", "`cursor / grok-4.7 / xhigh`"},
		[2]string{"all seven stages passed", "all six stages passed"},
		[2]string{"all seven stages passed", "the stages were observed"},
		[2]string{"`versions/2026.10.22-27dcf5f/cursor-agent`", "`cursor-agent`"},
		[2]string{"`2ccc9a8e167797641448b5e5c936f006ba137a2555f117f38c5eb76a5238a233`", "`2ccc9a8e167797641448b5e5c936f006ba137a2555f117f38c5eb76a5238a234`"},
		[2]string{"equal before and after", "changed after cleanup"},
		[2]string{"`design/real-e2e-runs/20261028-cursor-exercise2/attempts/full/audit-review.md` also records", "`design/real-e2e-runs/20261028-cursor-exercise2/attempts/light/audit-review.md` also records"},
		[2]string{" The external coordinator's record `design/real-e2e-runs/20261028-cursor-exercise2/attempts/full/audit-review.md` also records this probe's offline check as QUALIFIED with codes `[]`.", ""},
		[2]string{"offline check as QUALIFIED", "offline check as BLOCKED"},
		[2]string{"with codes `[]`", "with codes `[cursor_executable_mismatch]`"},
		[2]string{"20261028-cursor-qual2/", "20261028-cursor-qual1/"},
	)...)
	mutateExpect(t, docs, onCursorRunbook(checkCursorCloseoutProbe), "e2e", cursorChanged(t, muts))
}

func TestCursorCloseoutFP3(t *testing.T) {
	docs := cursorRunbook(t)
	cursorCloseoutPositive(t, docs["e2e"])
	first := strings.NewReplacer("20261028-cursor-exercise2/", "20261028-cursor-exercise/", "20261028T203540Z-11107d40", "20261028T074203Z-f6ae17b1",
		"20261028T203547Z-d9d43e67", "20261028T074210Z-70dc1229", "20261028T204730Z-d51eefed", "20261028T075929Z-560a5bdc").Replace(cursorExercisePin)
	muts := [][2]string{
		{cursorExercisePin + "\n\n", ""},
		{cursorExercisePin, first},
		// Swapped before the qualified probe.
		{cursorProbePin + "\n\n" + cursorExercisePin, cursorExercisePin + "\n\n" + cursorProbePin},
	}
	muts = append(muts, cursorEdits(cursorExercisePin,
		[2]string{"`design/real-e2e-runs/20261028-cursor-exercise2/attempts/full/audit-review.md` and ", ""},
		[2]string{" and `design/real-e2e-runs/20261028-cursor-exercise2/attempts/light/audit-review.md`", ""},
		[2]string{"`20261028T203540Z-11107d40`", "`20261028T203540Z-11107d41`"},
		[2]string{"`20261028T203547Z-d9d43e67`", "`20261028T203547Z-d9d43e68`"},
		[2]string{"`20261028T204730Z-d51eefed`", "`20261028T204730Z-d51eefee`"},
		[2]string{"with 17 worker calls", "with 18 worker calls"},
		[2]string{"PASS with 9 calls", "PASS with 10 calls"},
		[2]string{"PASS with 8 calls", "PASS with 7 calls"},
		[2]string{"is `PASS`", "is `FAIL`"},
		[2]string{"`20261028T203547Z-d9d43e67` is mechanical PASS", "`20261028T203547Z-d9d43e67` is mechanical FAIL"},
		[2]string{"`20261028T204730Z-d51eefed` is mechanical PASS", "`20261028T204730Z-d51eefed` is mechanical FAIL"},
		[2]string{"same revision `6e57006ad5a7bbe01fff4e4476650bdff6e1c47e`", "same revision `167d38f508421035d54db4b72cd5549133db0ba5`"},
		[2]string{"Cursor version `2026.10.22-27dcf5f`", "Cursor version `2026.10.14-0f9edde`"},
		[2]string{"carry the same resolved-target binding and equal before/after file hashes", "carry their own bindings"},
		[2]string{"equal before/after file hashes", "changed file hashes"},
		[2]string{"2026-10-28 judgment audits", "2026-10-27 judgment audits"},
		[2]string{"all four judgment rows as correct", "three of four judgment rows as correct"},
		[2]string{"no disputed mechanical row", "one disputed mechanical row"},
		[2]string{"offline checks of the root and both children exited 0", "offline checks of the root exited 0"},
		[2]string{"exited 0 and matched their canonical report.json", "exited 1"},
		[2]string{" Those audit records also state that offline checks of the root and both children exited 0 and matched their canonical report.json.", ""},
		[2]string{"The mechanical reports alone do not approve model judgment", "The mechanical reports approve model judgment"},
		[2]string{"without rewriting the canonical reports", "by rewriting the canonical reports"},
	)...)
	mutateExpect(t, docs, onCursorRunbook(checkCursorCloseoutExercise), "e2e", cursorChanged(t, muts))
	// The status retained while the exercise is removed or replaced fails
	// the aggregate, and the procedure validator that calls it.
	for _, check := range []func(string) error{checkCursorCloseout, checkCursorProcedure} {
		mutateExpect(t, docs, onCursorRunbook(check), "e2e", cursorChanged(t, [][2]string{{cursorExercisePin + "\n\n", ""}, {cursorExercisePin, first}}))
	}
}

func TestCursorCloseoutFP4(t *testing.T) {
	docs := cursorRunbook(t)
	cursorCloseoutPositive(t, docs["e2e"])
	muts := [][2]string{
		{cursorSupersededHeading + "\n\n", ""},
		{cursorSupersededHeading, "### First Cursor qualification and exercise"},
		{cursorSupersededHeading, "#### Superseded first Cursor qualification and exercise"},
		{cursorSupersededPin, ""},
		{"### Historical slice 3a diagnostic observation", "### Cursor coder diagnostic probe (slice 3a)"},
	}
	muts = append(muts, cursorEdits(cursorSupersededPin,
		[2]string{"QUALIFIED for Cursor `2026.10.14-0f9edde`", "QUALIFIED for Cursor `2026.10.22-27dcf5f`"},
		[2]string{"on the qualified `2026.10.14-0f9edde`", "on the qualified `2026.10.22-27dcf5f`"},
		[2]string{"self-updated to `2026.10.22-27dcf5f`", "self-updated to `2026.10.14-0f9edde`"},
		[2]string{"`167d38f508421035d54db4b72cd5549133db0ba5`", "`6e57006ad5a7bbe01fff4e4476650bdff6e1c47e`"},
		[2]string{"`20261028T074203Z-f6ae17b1`", "`20261028T203540Z-11107d40`"},
		[2]string{"Cursor self-updated to", "The owner updated Cursor to"},
		[2]string{"task started 07:52:08Z", "task started 07:42:08Z"},
		[2]string{"the new version directory appeared 07:52:30Z", "the new version directory appeared 07:59:30Z"},
		[2]string{"The light child then ran the unqualified version.", "The light child then ran the qualified version."},
		[2]string{"The former shim-hash binding missed that change because the wrapper bytes were unchanged", "The former shim-hash binding detected that change"},
		[2]string{"#76 fixed that gap", "a later change may fix that gap"},
		[2]string{"`design/real-e2e-runs/20261028-cursor-exercise/attempts/full/audit-review.md`", "`design/real-e2e-runs/20261028-cursor-exercise/attempts/full/audit.md`"},
		[2]string{"`design/real-e2e-runs/20261028-cursor-exercise/attempts/light/audit-review.md`", "`design/real-e2e-runs/20261028-cursor-exercise/attempts/light/audit.md`"},
		[2]string{"The dated 2026-10-28 addenda", "The addenda"},
		[2]string{"record the owner's decision to fix the binding and requalify", "record the owner's acceptance of the drift"},
		[2]string{"but does not complete slice 3", "and completes slice 3"},
		[2]string{" Qual1 and the first exercise are superseded history, never completion evidence.", ""},
		[2]string{"never completion evidence", "completion evidence"},
		[2]string{"does not establish the stronger binding or completion", "establishes completion"},
	)...)
	mutateExpect(t, docs, onCursorRunbook(checkCursorCloseoutHistory), "e2e", cursorChanged(t, muts))
	// The retained slice 3a disclaimer through the existing history
	// mutations of the owner-procedure validator.
	mutateExpect(t, docs, onCursorRunbook(checkCursorProcedure), "e2e", cursorChanged(t, [][2]string{
		{"The 2026-10-27 slice 3a receipt was a diagnostic-only observation, not qualification.", "The 2026-10-27 slice 3a receipt is qualified."},
		{"### Historical slice 3a diagnostic observation", "### Cursor coder diagnostic probe (slice 3a)"},
		{"never rewritten or promoted", "promoted"},
	}))
}

func TestCursorCloseoutFP5(t *testing.T) {
	docs := cursorRunbook(t)
	cursorCloseoutPositive(t, docs["e2e"])
	const notCompletion = "a probe alone, a full child alone or an unaudited exercise is not completion"
	muts := [][2]string{
		{" " + cursorCompletionRulePin, ""},
		{cursorCompletionRulePin, "Cursor is complete only when the fresh qualification, the combined exercise with both children and the root passing, and both external child judgment audits are recorded; " + notCompletion + "."},
		// Moved out of the owner procedure into the superseded history.
		{" " + cursorCompletionRulePin + "\n\n" + cursorSupersededHeading + "\n\n", "\n\n" + cursorSupersededHeading + "\n\n" + cursorCompletionRulePin + " "},
		{" " + cursorInvalidationSentence, ""},
	}
	muts = append(muts, cursorEdits(cursorCompletionRulePin,
		[2]string{"; " + notCompletion, ""},
		[2]string{notCompletion, "a probe alone is completion"},
		[2]string{notCompletion, "a full child alone is completion"},
		[2]string{notCompletion, "an unaudited exercise is completion"},
		[2]string{"the combined exercise with both children and the root passing", "the full child passing"},
		[2]string{", and both external child judgment audits to be recorded", ""},
		[2]string{"both external child judgment audits", "the root audit"},
		[2]string{"For any Cursor closeout, completion requires", "This closeout's completion required"},
		[2]string{"for their recorded binary identity and selection", "for every Cursor version"},
		[2]string{"future requalification follows this same owner procedure", "future requalification is automatic"},
	)...)
	muts = append(muts, cursorEdits(cursorInvalidationSentence,
		[2]string{"binary/version, ", ""},
		[2]string{"manual, ", ""},
		[2]string{"policy, ", ""},
		[2]string{"account context ", ""},
		[2]string{" or selection", ""},
		[2]string{"Any changed relevant binary/version, manual, policy, account context or selection", "Any changed binary identity"},
		[2]string{"invalidates use of this qualification", "keeps this qualification usable"},
	)...)
	mutateExpect(t, docs, onCursorRunbook(checkCursorCloseoutRule), "e2e", cursorChanged(t, muts))
	// Fresh-output safety: either command variable replaced by a recorded
	// bundle (qual2, exercise2, qual1, the first exercise or the slice 3a
	// bundle) is refused by the exact command templates.
	var outputs [][2]string
	for _, v := range []string{`"$CURSOR_PROBE_EVIDENCE"`, `"$CURSOR_EXERCISE_EVIDENCE"`} {
		for _, b := range []string{"20261028-cursor-qual2", "20261028-cursor-exercise2", "20261028-cursor-qual1", "20261028-cursor-exercise", "20261027-cursor-probe1"} {
			outputs = append(outputs, [2]string{v, "/abs/callsheet/design/real-e2e-runs/" + b})
		}
	}
	mutateExpect(t, docs, onCursorRunbook(checkCursorProcedure), "e2e", cursorChanged(t, outputs))
}

func TestCursorCloseoutFP6(t *testing.T) {
	docs := cursorRunbook(t)
	doc := docs["e2e"]
	cursorCloseoutPositive(t, doc)
	p, err := cursorCloseoutSections(doc)
	if err != nil {
		t.Fatal(err)
	}
	// Positive control: the slice 3a paragraph's legitimate old version
	// outside the superseded history stays accepted.
	if _, historical, _ := cursorSections(doc); !strings.Contains(historical, "Cursor `2026.10.14-0f9edde`") || strings.Count(doc, "2026.10.14-0f9edde") <= strings.Count(p.superseded, "2026.10.14-0f9edde") {
		t.Fatal("the positive control (the slice 3a version outside the superseded history) is absent")
	}
	muts := [][2]string{
		{cursorStatusPin, cursorStatusPin + " Cursor coder status: diagnostic observed; fresh qualification, combined full/light execution and external child judgment audits pending."},
		{cursorStatusPin, cursorStatusPin + " Fresh qualification, combined full/light execution and external child judgment audits pending."},
		{cursorBindingParagraphs[2], cursorBindingParagraphs[2] + " Still pending: fresh qualification, combined full/light execution and external child judgment audits pending."},
		{"\n## Sign-off\n", "\nCursor coder status: diagnostic observed.\n\n## Sign-off\n"},
		{cursorStatusPin, cursorStatusPin + " Cursor coder status: complete."},
		{cursorNavigationPin, cursorNavigationPin + " Cursor coder status: complete."},
		{cursorStatusPin, cursorStatusPin + " Cursor coder status: observed."},
		{cursorStatusPin, cursorStatusPin + " Cursor coder status: qualified."},
		// A promotion appended inside the superseded history.
		{cursorSupersededPin, cursorSupersededPin + " The first exercise completes slice 3."},
		{cursorSupersededPin + "\n", cursorSupersededPin + "\n\nQual1 remains the current qualification.\n"},
	}
	for _, s := range cursorSupersededOnly {
		muts = append(muts,
			[2]string{cursorStatusPin, cursorStatusPin + " " + s},
			[2]string{cursorExercisePin, cursorExercisePin + " See `" + s + "`."},
			[2]string{cursorNavigationPin, cursorNavigationPin + " " + s},
			[2]string{"### Historical slice 3a diagnostic observation\n\n", "### Historical slice 3a diagnostic observation\n\n" + s + " "},
		)
	}
	mutateExpect(t, docs, onCursorRunbook(checkCursorCloseoutConsistency), "e2e", cursorChanged(t, muts))
	// Each present superseded string moved out of the superseded history
	// into the current status.
	moved := 0
	for _, s := range cursorSupersededOnly {
		if !strings.Contains(p.superseded, s) {
			continue
		}
		moved++
		m := strings.Replace(doc, s, "", 1)
		m = strings.Replace(m, cursorStatusPin, cursorStatusPin+" "+s, 1)
		cursorRefused(t, checkCursorCloseoutConsistency, "moving "+s+" out of the superseded history", doc, m)
	}
	if moved != 4 {
		t.Fatalf("%d superseded strings are in the superseded history, want 4", moved)
	}
}

func TestCursorCloseoutFP7(t *testing.T) {
	docs := cursorRunbook(t)
	doc := docs["e2e"]
	cursorCloseoutPositive(t, doc)
	check := onCursorRunbook(checkCursorCloseoutCitations)
	// forms are a citation's wrong forms: no backticks, a wrong or private
	// absolute path, an inline evidence link instead, and such a link
	// appended beside the correct citation.
	forms := func(c string) []string {
		return []string{c, "`" + strings.Replace(c, "design/real-e2e-runs/", "design/real-e2e-run/", 1) + "`", "`/home/owner/callsheet/" + c + "`",
			"[" + c + "](../" + c + ")", "`" + c + "` ([record](../" + c + "))"}
	}
	var muts [][2]string
	for _, c := range cursorCitations {
		for _, f := range forms(c.path) {
			muts = append(muts, [2]string{"`" + c.path + "`", f})
		}
	}
	mutateExpect(t, docs, check, "e2e", cursorChanged(t, muts))
	// A citation cited twice: every wrong form at its second occurrence.
	for _, c := range cursorCitations {
		cited := "`" + c.path + "`"
		for n := 1; n < c.count; n++ {
			at := 0
			for k := 0; k <= n; k++ {
				i := strings.Index(doc[at:], cited)
				if i < 0 {
					t.Fatalf("%s is cited fewer than %d times", c.path, n+1)
				}
				at += i
				if k < n {
					at += len(cited)
				}
			}
			for _, f := range forms(c.path) {
				cursorRefused(t, checkCursorCloseoutCitations, fmt.Sprintf("citation %d of %s as %q", n+1, c.path, f), doc, doc[:at]+f+doc[at+len(cited):])
			}
		}
	}
}
