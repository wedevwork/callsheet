package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The full-flow checker (design 12a-dev-flow-verify, Bundle and checker,
// Replay and audit). CheckDevBundle is a pure derivation from captured
// bytes and bounded objects: no network, vendor, shell, test execution,
// configuration read or write to the evidence. It replays the policy from
// the seed, re-derives every session fact, publication, reviewer receipt,
// final-candidate oracle, accounting and environment comparison, and
// compares the stored derived report and audit byte for byte with its
// canonical output (the external judgment appendix aside). Mechanical
// PASS never means a model's design, diagnosis or review was judged.

// Full-flow criteria, in report order.
var devCriteria = []string{"manifest", "provenance", "roles", "preflight", "coordinator_setup", "admissions", "transitions", "waits",
	"publications", "owner_receipts", "reviewer_execution", "baseline", "final_tests", "coverage", "feature_oracle", "cleanup",
	"owner_environment", "deadlines", "cost"}

// DevCriterion is one criterion's result.
type DevCriterion struct {
	ID       string   `json:"id"`
	Result   string   `json:"result"`
	Codes    []string `json:"codes"`
	Cause    string   `json:"cause"`
	CauseSeq int      `json:"cause_seq"`
	Sources  []string `json:"sources"`
}

// DevReport is report.json.
type DevReport struct {
	Schema          string           `json:"schema"`
	RunID           string           `json:"run_id"`
	Result          string           `json:"result"`
	Scope           string           `json:"scope"`
	FlowLabel       string           `json:"flow_label"`
	Claim           string           `json:"claim"`
	Judgments       string           `json:"judgments"`
	Revision        string           `json:"revision"`
	Platform        string           `json:"platform"`
	Versions        []VersionRecord  `json:"versions"`
	ProtectedRoots  []string         `json:"protected_roots"`
	ProtectDepth    int              `json:"protect_depth"`
	Criteria        []DevCriterion   `json:"criteria"`
	Protected       []EnvChange      `json:"protected_changes"`
	OversizedConfig []string         `json:"oversized_config"`
	Tasks           int              `json:"workflow_tasks"`
	Phases          int              `json:"coordinator_phases"`
	WorkerCalls     int              `json:"worker_calls"`
	FinalCandidate  string           `json:"final_candidate"`
	FinalReview     string           `json:"final_review"`
	Stop            string           `json:"stop"`
	StopSeq         int              `json:"stop_seq"`
	Artifacts       []InventoryEntry `json:"artifacts"`
	Error           string           `json:"error"`
	// Activity is a Cursor report's recomputed shallow-sample summary
	// (design 12a-3-cursor-worker, slice 3b): required on
	// callsheet-dev-cursor-report/v1 only, never emitted otherwise.
	Activity *CursorActivitySummary `json:"activity,omitempty"`
	tool     toolReport
	// activity are the samples' per-root lines for report.txt.
	activity []string
}

// Dev report scope and statements.
const (
	devScope       = "full-flow-mechanics-and-deterministic-results"
	CodeDevMissing = "evidence_missing"
)

// devClaim states what a result demonstrates.
func devClaim(result, label string) string {
	switch {
	case result != ResultPass:
		return "not demonstrated"
	case label == FlowDefault:
		return "the default full development flow's recorded mechanics, publications, reviewer execution and the final candidate's deterministic oracle passed on this installation; no model judgment is claimed"
	}
	return "an override flow's recorded mechanics passed; the owner's default pairs are not demonstrated"
}

// devCheckResult is one check's canonical outputs.
type devCheckResult struct {
	report     DevReport
	reportJSON []byte
	reportText string
	audit      DevAudit
	auditJSON  []byte
	auditText  string
}

// devChecker accumulates one check.
type devChecker struct {
	b       *DevBundle
	m       *DevManifest
	runID   string
	flow    DevFullFlow
	flowOK  bool
	codes   map[string][]string
	srcs    map[string][]string
	unreach map[string]bool
	rows    []DevAuditRow
	judge   []DevAuditRow
	// stop is the verified stop cause (nil: none verified).
	stop *devStop
	// derived facts
	trees    map[string]map[string]TreeFile
	treeHash map[string]string
	views    map[int]*contract.TaskView
	finals   map[string]string
	rp       *devReplay
	eventMap DevEventMap
	rowSeq   map[string]int
}

// devStop is a verified stop: the manifest's stop code at its recorded
// attempt_failed event.
type devStop struct {
	code string
	seq  int
}

// newDevChecker starts one check of b.
func newDevChecker(b *DevBundle) *devChecker {
	return &devChecker{b: b, m: b.Manifest, codes: map[string][]string{}, srcs: map[string][]string{}, unreach: map[string]bool{},
		trees: map[string]map[string]TreeFile{}, treeHash: map[string]string{}, views: map[int]*contract.TaskView{}, finals: map[string]string{},
		rowSeq: map[string]int{}}
}

// CheckDevBundle checks a loaded full-flow bundle and returns its
// canonical report and audit.
func CheckDevBundle(b *DevBundle) (DevReport, DevAudit) {
	res := checkDevBundle(b, false)
	return res.report, res.audit
}

// checkDevBundle is CheckDevBundle; initial is the collector's private
// initial-generation form, run before the derived files exist (an offline
// check requires them and compares them byte for byte).
func checkDevBundle(b *DevBundle, initial bool) devCheckResult {
	c := newDevChecker(b)
	c.run()
	r := c.buildReport()
	a := c.buildAudit(r)
	res := devCheckResult{report: r, audit: a}
	res.reportJSON, _ = encodeJSON(r)
	res.reportText = renderDevReport(r)
	res.auditJSON, _ = encodeJSON(a)
	res.auditText = renderDevAudit(a)
	if initial {
		return res
	}
	// The stored derived files must equal their canonical recomputation;
	// a mismatch (or a missing file) fails, without its diagnostics
	// entering the canonical report it was compared with.
	stored := [][2][]byte{{b.Files["report.json"], res.reportJSON}, {b.Files["report.txt"], []byte(res.reportText)},
		{b.Files["audit.json"], res.auditJSON}, {b.Files["audit.md"], []byte(res.auditText)}}
	mismatch := false
	for i, s := range stored {
		name := []string{"report.json", "report.txt", "audit.json", "audit.md"}[i]
		if _, ok := b.Files[name]; !ok || !bytes.Equal(s[0], s[1]) {
			mismatch = true
		}
	}
	if mismatch {
		out := r
		out.Criteria = slices.Clone(r.Criteria)
		for i := range out.Criteria {
			if out.Criteria[i].ID == "manifest" {
				cr := out.Criteria[i]
				cr.Codes = append(slices.Clone(cr.Codes), CodeDerivedMismatch)
				sort.Strings(cr.Codes)
				cr.Result, cr.Cause, cr.CauseSeq = ResultCriterionFail, "", 0
				out.Criteria[i] = cr
			}
		}
		out.Result, out.Claim = ResultFail, devClaim(ResultFail, out.FlowLabel)
		if c.isLight() {
			out.Claim = lightClaim(ResultFail, out.FlowLabel)
		}
		res.report = out
		res.reportJSON, _ = encodeJSON(out)
		res.reportText = renderDevReport(out)
	}
	return res
}

// fail records code under crit.
func (c *devChecker) fail(crit, code string) {
	if !slices.Contains(c.codes[crit], code) {
		c.codes[crit] = append(c.codes[crit], code)
	}
}

// source records a consulted bundle file under crit.
func (c *devChecker) source(crit string, files ...string) {
	for _, f := range files {
		if _, ok := c.b.Files[f]; ok && !slices.Contains(c.srcs[crit], f) {
			c.srcs[crit] = append(c.srcs[crit], f)
		}
	}
}

// absent records missing evidence for crit: not reached when the attempt
// verifiably stopped before its producing step (reached false), else a
// failure.
func (c *devChecker) absent(crit, code string, reached bool) bool {
	if !reached && c.stop != nil {
		c.unreach[crit] = true
		return false
	}
	c.fail(crit, code)
	return true
}

// buildReport derives the report from the criteria.
func (c *devChecker) buildReport() DevReport {
	r := DevReport{Schema: c.b.reportSchema(), Result: ResultFail, Scope: c.scope(), Judgments: PendingReview, Versions: []VersionRecord{},
		ProtectedRoots: []string{}, Criteria: []DevCriterion{}, Protected: []EnvChange{}, OversizedConfig: []string{}, Artifacts: []InventoryEntry{}}
	red := Redactor{}
	pass := true
	for _, id := range c.criteria() {
		cr := DevCriterion{ID: id, Result: ResultCriterionPass, Codes: []string{}, Sources: append([]string{}, c.srcs[id]...)}
		sort.Strings(cr.Sources)
		codes := slices.Clone(c.codes[id])
		sort.Strings(codes)
		switch {
		case len(codes) > 0:
			cr.Result, cr.Codes = ResultCriterionFail, codes
		case c.unreach[id] && c.stop != nil:
			cr.Result, cr.Cause, cr.CauseSeq = ResultCriterionNotReached, c.stop.code, c.stop.seq
		}
		pass = pass && cr.Result == ResultCriterionPass
		r.Criteria = append(r.Criteria, cr)
	}
	complete := c.rp != nil && c.rp.complete && c.m != nil && c.m.StopCode == nil
	if m := c.m; m != nil {
		if ValidRunID(m.RunID) {
			r.RunID = m.RunID
		}
		if m.FlowLabel == FlowDefault || m.FlowLabel == FlowOverride {
			r.FlowLabel = m.FlowLabel
		}
		if hex40RE.MatchString(m.Revision) {
			r.Revision = m.Revision
		}
		r.Platform = red.Redact(m.GOOS + "/" + m.GOARCH)
		for _, v := range m.Versions {
			r.Versions = append(r.Versions, VersionRecord{Tool: red.Redact(v.Tool), Path: red.Redact(v.Path), Version: red.Redact(v.Version), Eligible: v.Eligible})
		}
		r.ProtectDepth = m.ProtectDepth
		for _, root := range m.ProtectedRoots {
			r.ProtectedRoots = append(r.ProtectedRoots, red.Redact(root))
		}
		if m.StopCode != nil && stopCodeRE.MatchString(*m.StopCode) {
			r.Stop = *m.StopCode
		}
		if m.StopSeq != nil {
			r.StopSeq = *m.StopSeq
		}
		if hex40RE.MatchString(m.FinalCandidate) && hex40RE.MatchString(m.FinalReview) {
			r.FinalCandidate, r.FinalReview = m.FinalCandidate, m.FinalReview
		}
	}
	if d := doc[EnvDiff](c.b, "environment/diff.json"); d != nil {
		for _, ch := range d.Protected {
			r.Protected = append(r.Protected, sanitizeChange(ch))
		}
		for _, n := range d.OversizedConfig {
			r.OversizedConfig = append(r.OversizedConfig, red.Redact(n))
		}
		r.tool = toolReport{detail: d.ToolDetailCount, omitted: d.ToolOmittedCount, present: true}
		for _, row := range d.ToolSummary {
			row.Scope, row.TopLevel = red.Redact(row.Scope), red.Redact(row.TopLevel)
			r.tool.rows = append(r.tool.rows, row)
		}
	}
	// The counts are the derived accounting's (admissions and settlements,
	// failed and interrupted calls included), never the stored summary's.
	cost := deriveDevCost(c.b, c.runID)
	r.Tasks = len(cost.WorkflowTasks)
	r.Phases = len(c.phaseNumbers())
	r.WorkerCalls = cost.WorkerCalls
	r.Artifacts = c.b.inventory()
	if c.b.cursor {
		// A Cursor report's recomputed report-only activity summary (never on
		// a historical or default report).
		f := cursorActivityOf(c.b, c.runID)
		r.Activity, r.activity = &f.summary, f.lines
	}
	if pass && complete {
		r.Result = ResultPass
	}
	if c.m == nil {
		r.Error = CodeDevMissing
	}
	r.Claim = devClaim(r.Result, r.FlowLabel)
	if c.isLight() {
		r.Claim = lightClaim(r.Result, r.FlowLabel)
	}
	return r
}

// renderDevReport is report.txt.
func renderDevReport(r DevReport) string {
	var b strings.Builder
	flow := "full"
	if r.Scope == devLightScope {
		flow = "light"
	}
	fmt.Fprintf(&b, "result: %s (%s development flow mechanics and deterministic criteria)\n%s\n", r.Result, flow, JudgmentReportLine)
	fmt.Fprintf(&b, "run: %s\nflow: %s\nclaim: %s\nrevision: %s\nplatform: %s\n", r.RunID, r.FlowLabel, r.Claim, r.Revision, r.Platform)
	if r.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", r.Error)
	}
	for _, v := range r.Versions {
		fmt.Fprintf(&b, "version %s: %s\n", v.Tool, v.Version)
	}
	fmt.Fprintf(&b, "workflow tasks: %d; coordinator phases: %d; worker calls: %d\n", r.Tasks, r.Phases, r.WorkerCalls)
	if r.FinalCandidate != "" {
		fmt.Fprintf(&b, "final candidate: %s; approving review: %s\n", r.FinalCandidate, r.FinalReview)
	}
	if r.Stop != "" {
		fmt.Fprintf(&b, "stopped: %s (event %d)\n", r.Stop, r.StopSeq)
	}
	if len(r.ProtectedRoots) == 0 {
		b.WriteString("protected roots: No repository root was protected (only the four vendor homes)\n")
	}
	for _, root := range r.ProtectedRoots {
		fmt.Fprintf(&b, "protected root: %s\n", root)
	}
	fmt.Fprintf(&b, "protected depth: repositories at most %d directory levels below each protected root; deeper repositories are not protected\n", r.ProtectDepth)
	for _, l := range append(slices.Clone(ClaudeStateExclusions), OwnerAcceptedExclusions...) {
		b.WriteString(l + "\n")
	}
	b.WriteString("not covered: " + NestedRepoLimitation + "\n")
	for _, c := range r.Criteria {
		switch {
		case c.Result == ResultCriterionNotReached:
			fmt.Fprintf(&b, "criterion %s: %s (not reached: %s, attempt stopped at event %d)\n", c.ID, c.Result, c.Cause, c.CauseSeq)
		case len(c.Codes) == 0:
			fmt.Fprintf(&b, "criterion %s: %s\n", c.ID, c.Result)
		default:
			fmt.Fprintf(&b, "criterion %s: %s (%s)\n", c.ID, c.Result, strings.Join(c.Codes, ", "))
		}
	}
	for _, c := range r.Protected {
		fmt.Fprintf(&b, "protected change: %s %s %s [%s]\n", c.Scope, c.Path, c.Kind, strings.Join(c.Fields, ","))
	}
	for _, n := range r.OversizedConfig {
		fmt.Fprintf(&b, "configuration file over 1 MiB, fingerprinted by metadata only: %s\n", n)
	}
	if t := r.tool; t.present {
		fmt.Fprintf(&b, "tool-maintained changes (report-only): %d (%d detailed in diff.json, %d omitted)\n", t.detail+t.omitted, t.detail, t.omitted)
		for _, row := range t.rows {
			fmt.Fprintf(&b, "  tool-maintained %s %s: %d added, %d changed, %d deleted, %d total\n", row.Scope, row.TopLevel, row.Added, row.Changed,
				row.Deleted, row.Total)
		}
	}
	if a := r.Activity; a != nil {
		b.WriteString("cursor activity (report-only shallow samples of all processes at depth 2, not attribution to Cursor; no recursive total, " +
			"no complete diff and no confinement claim; quality flags are observations, never a gate):\n")
		for _, l := range r.activity {
			fmt.Fprintf(&b, "  %s\n", l)
		}
		if len(a.Changes) == 0 {
			b.WriteString("  compared rows: no marker\n")
		}
		for _, ch := range a.Changes {
			fmt.Fprintf(&b, "  compared %s %s: %s\n", ch.Scope, ch.Name, ch.Mark)
		}
		b.WriteString("cursor activity not covered: deep or transient writes, other home paths (.local/bin, .npm), reads and network\n")
	}
	fmt.Fprintf(&b, "artifacts: %d files (names and SHA-256 in report.json)\n", len(r.Artifacts))
	return b.String()
}

// devSchemaReport is the report of a full-flow bundle that could not be
// validated at all.
func devSchemaReport() DevReport {
	return DevReport{Schema: devKind("report"), Result: ResultFail, Scope: devScope, Claim: devClaim(ResultFail, ""), Judgments: PendingReview,
		Versions: []VersionRecord{}, ProtectedRoots: []string{}, Criteria: []DevCriterion{}, Protected: []EnvChange{}, OversizedConfig: []string{},
		Artifacts: []InventoryEntry{}, Error: "evidence_schema_error"}
}

// Bundle protocols, by their manifest discriminator.
const (
	protocolLegacy    = "legacy"
	protocolDevFull   = "dev-full"
	protocolExecution = "execution-only"
	protocolExercise  = "exercise"
	// protocolCursorQualification is a callsheet-dev-cursor-qualification/v1
	// record's independent check (design 12a-3-cursor-worker, slice 3b).
	protocolCursorQualification = "cursor-qualification"
)

// bundleProtocol decides a bundle's protocol from a safely read, strictly
// decoded manifest discriminator only: the legacy v3 manifest, the
// full-flow evidence/v1 manifest, or an error (exit 2) for an
// execution-only bundle (even with a second manifest), a qualification
// record, legacy v1/v2 or an unknown version. Evidence is never migrated
// and its protocol is never inferred from file names.
func bundleProtocol(root string) (string, string, error) {
	resolved, err := safeEvidenceRoot(root)
	if err != nil {
		return "", "", err
	}
	if _, err := lstatRegular(resolved, DevExecutionFile); err == nil {
		return protocolExecution, resolved, schemaErr("%s: an execution-only bundle (%s) is unsupported", DevExecutionFile, DevExecutionSchema)
	}
	if data, err := lstatRegular(resolved, devQualificationFile); err == nil {
		// Only the new Cursor qualification format has an offline check
		// route (design 12a-3-cursor-worker, slice 3b); every other
		// qualification record is refused as before.
		if schema, serr := manifestSchema(data); serr == nil && schema == DevCursorQualificationSchema {
			return protocolCursorQualification, resolved, nil
		}
		return "", resolved, schemaErr("%s is a qualification record, not a full-flow run", devQualificationFile)
	}
	if exerciseRootExists(resolved) {
		return protocolExercise, resolved, nil
	}
	data, err := lstatRegular(resolved, devManifestFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No manifest: the legacy loader judges it (a missing legacy
			// manifest is failed evidence there).
			return protocolLegacy, resolved, nil
		}
		return "", resolved, schemaErr("manifest.json: %v", err)
	}
	schema, err := manifestSchema(data)
	if err != nil {
		return "", resolved, schemaErr("manifest.json: %v", err)
	}
	switch schema {
	case ManifestSchema:
		return protocolLegacy, resolved, nil
	case DevBundleSchema, DevBundleSchemaV1, DevCursorBundleSchema:
		return protocolDevFull, resolved, nil
	}
	return "", resolved, schemaErr("manifest.json: unsupported schema %q", contract.SafeText(schema, 80))
}

// CheckEvidence runs the offline check of root and writes the JSON report
// to out: 0 for PASS, 1 for failed or incomplete evidence, 2 for schema
// errors. It routes by the manifest discriminator: a legacy
// callsheet-real-e2e/v3 bundle to the unchanged legacy checker (its PASS
// means only legacy PASS), a callsheet-dev-full-evidence/v1 bundle to the
// full-flow checker. It writes nothing to the bundle and executes nothing.
func CheckEvidence(root string, out, errOut io.Writer) int {
	// Design 12a-3-cursor-worker (FP-6, post-final DS1): on the root
	// safeEvidenceRoot resolves, before bundleProtocol and every version
	// list check, a Cursor diagnostic observation is refused
	// (diagnostic_not_qualification). An unsafe root keeps its existing
	// error, which bundleProtocol reports.
	if resolved, err := safeEvidenceRoot(root); err == nil {
		if code, handled := checkDiagnosticObservation(resolved, out, errOut); handled {
			return code
		}
	}
	proto, _, err := bundleProtocol(root)
	if err != nil {
		data, _ := encodeJSON(devSchemaReport())
		out.Write(data)
		fmt.Fprintf(errOut, "reale2e check: %v\n", err)
		return 2
	}
	switch proto {
	case protocolLegacy:
		return checkLegacyEvidence(root, out, errOut)
	case protocolExercise:
		return CheckExercise(root, out, errOut)
	case protocolCursorQualification:
		return checkCursorQualificationEvidence(root, out, errOut)
	}
	b, err := LoadDevBundle(root)
	if err != nil {
		data, _ := encodeJSON(devSchemaReport())
		out.Write(data)
		fmt.Fprintf(errOut, "reale2e check: %v\n", err)
		return 2
	}
	res := checkDevBundle(b, false)
	out.Write(res.reportJSON)
	if res.report.Result == ResultPass {
		return 0
	}
	return 1
}

// phaseNumbers are the phases whose launch or stream exists.
func (c *devChecker) phaseNumbers() []int {
	var out []int
	for p := 1; p <= maxPhases; p++ {
		_, launch := c.b.Files[fmt.Sprintf("setup/launch-phase-%d.json", p)]
		_, stream := c.b.Files[devStreamFile(p)]
		if launch || stream {
			out = append(out, p)
		}
	}
	return out
}

// eventsOf returns the events of type typ.
func (c *devChecker) eventsOf(typ string) []Event {
	var out []Event
	for _, e := range c.b.Events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// event returns the first event matching pred.
func (c *devChecker) event(pred func(Event) bool) (Event, bool) {
	for _, e := range c.b.Events {
		if pred(e) {
			return e, true
		}
	}
	return Event{}, false
}

// eventAt is the event of sequence seq.
func (c *devChecker) eventAt(seq int) *Event {
	if seq < 1 || seq > len(c.b.Events) || c.b.Events[seq-1].Seq != seq {
		return nil
	}
	return &c.b.Events[seq-1]
}

// seqOf is the sequence of the first event matching pred (0: none).
func (c *devChecker) seqOf(pred func(Event) bool) int {
	e, ok := c.event(pred)
	if !ok {
		return 0
	}
	return e.Seq
}

// run evaluates everything, in dependency order.
func (c *devChecker) run() {
	c.checkEvents()
	c.checkManifest()
	if c.m == nil {
		for _, id := range c.criteria()[1:] {
			c.fail(id, "manifest_missing")
		}
		c.setupRow("manifest", "The full-flow manifest exists and validates", []string{"manifest_missing"}, nil)
		return
	}
	c.loadTrees()
	if flow, err := ParseDevFullFlow(c.b.Files["setup/flow.json"]); err == nil && slices.Equal(flow.Roles, c.m.Roles) {
		c.flow, c.flowOK = flow, true
	} else if _, ok := c.b.Files["setup/flow.json"]; ok {
		c.fail("provenance", "flow_mismatch")
	}
	if d := c.b.derived; d != nil && d.goos == c.m.GOOS && d.reale2e == c.m.Render["REALE2E"] && d.runtime == c.m.Render["RUNTIME"] {
		// The collector derived it from these very bytes and values.
		c.eventMap = d.em
	} else {
		c.eventMap = deriveDevEventMap(sessionMapInput(c.b, c.runID, c.m.GOOS, c.m.Render["REALE2E"], c.m.Render["RUNTIME"]))
	}
	c.checkProvenance()
	c.checkRoles()
	c.checkPreflight()
	if c.isLight() {
		c.checkLightIntake()
	}
	c.replay()
	c.checkRejections()
	c.checkSession()
	c.checkAdmissions()
	if c.isLight() {
		c.checkLightValidation()
		c.lightJudgmentRows()
	} else {
		c.checkReceipts()
		c.checkValidation()
	}
	c.checkCleanup()
	c.checkEnvironment()
	c.checkDeadlines()
	c.checkCost()
	if !c.isLight() {
		c.judgmentRows()
	}
}

// checkEvents validates the event log's own order and finds the verified
// stop.
func (c *devChecker) checkEvents() {
	evs := c.b.Events
	if len(evs) == 0 {
		c.fail("manifest", "events_missing")
		return
	}
	var last time.Time
	for i, e := range evs {
		t, ok := parseTime(e.Time)
		switch {
		case e.Seq != i+1:
			c.fail("manifest", "sequence_not_increasing")
		case !ok:
			c.fail("manifest", "time_invalid")
		case t.Before(last):
			c.fail("manifest", "time_not_monotonic")
		}
		last = t
	}
	if evs[0].Type != EvRunStarted {
		c.fail("manifest", "run_started_not_first")
	}
	c.source("manifest", "events.jsonl")
	m := c.b.Manifest
	failures := c.eventsOf(EvAttemptFailed)
	switch {
	case m == nil:
	case m.StopCode == nil && len(failures) == 0:
	case m.StopCode == nil || m.StopSeq == nil || len(failures) == 0:
		c.fail("manifest", "stop_unverified")
	default:
		first := failures[0]
		if first.Seq != *m.StopSeq || first.Code != *m.StopCode || !stopCodeRE.MatchString(first.Code) {
			c.fail("manifest", "stop_unverified")
		} else {
			c.stop = &devStop{code: first.Code, seq: first.Seq}
		}
	}
}

// checkManifest validates the manifest's own fields and the inventory.
func (c *devChecker) checkManifest() {
	m := c.m
	if m == nil {
		c.fail("manifest", "manifest_missing")
		return
	}
	c.runID = m.RunID
	c.source("manifest", devManifestFile)
	switch {
	case !ValidRunID(m.RunID):
		c.fail("manifest", "run_id_invalid")
	case m.Scenario != c.b.Scenario || c.b.ver == devBundleV1 && m.Scenario != scenarioFull:
		c.fail("manifest", "scenario_invalid")
	case m.GOOS != "linux":
		c.fail("manifest", "platform_not_linux")
	}
	if !m.Gate.OptIn || !m.Gate.CIAbsent || m.Gate.Platform != "linux" {
		c.fail("manifest", "gate_not_passed")
	}
	if c.seqOf(func(e Event) bool { return e.Type == EvVersionsChecked }) > 0 {
		if !hex40RE.MatchString(m.Revision) || strings.TrimSpace(m.CallsheetVersion) == "" {
			c.fail("manifest", "revision_missing")
		}
		var tools []string
		for _, v := range m.Versions {
			tools = append(tools, v.Tool)
			if !v.Eligible || strings.TrimSpace(v.Version) == "" {
				c.fail("manifest", "version_not_eligible")
			}
		}
		want := []string{"claude", "codex", "grok", "go", "git"}
		if c.b.cursor {
			// Only the recognized Cursor format carries the cursor tool.
			want = cursorVersionTools
		}
		if !slices.Equal(tools, want) {
			c.fail("manifest", "versions_incomplete")
		}
		if c.b.cursor {
			// A Cursor attempt's recorded vendor versions are re-checked
			// against the adapter minimums offline (historical and default
			// admission is unchanged).
			for _, code := range cursorVersionCodes(m.Versions) {
				c.fail("manifest", code)
			}
		}
	}
	if c.b.cursor {
		// The vendor binding: hashed before any vendor execution and after
		// cleanup, unchanged, at the Cursor version record's path.
		bind := m.Cursor
		switch {
		case bind == nil || !hex64RE.MatchString(bind.BeforeSHA256) || bind.BeforeSHA256 != bind.AfterSHA256:
			c.fail("manifest", CodeCursorExecutableMismatch)
		case c.seqOf(func(e Event) bool { return e.Type == EvVersionsChecked }) > 0 && bind.Path != cursorVersionPath(m.Versions):
			c.fail("manifest", CodeCursorExecutableMismatch)
		}
	}
	start, ok1 := parseTime(m.AttemptStart)
	deadline, ok2 := parseTime(m.AttemptDeadline)
	if !ok1 || !ok2 || !deadline.Equal(start.Add(AttemptBound)) {
		c.fail("manifest", "attempt_window_invalid")
	}
	if _, ok := parseTime(m.CollectedAt); !ok {
		c.fail("manifest", "collected_at_invalid")
	}
	if !hex64RE.MatchString(m.Executable.Before) || m.Executable.Before != m.Executable.After {
		c.fail("manifest", "executable_changed")
	}
	if inv := c.b.inventory(); !bytes.Equal(mustJSON(inv), mustJSON(m.Inventory)) {
		c.fail("manifest", "inventory_mismatch")
	}
	if m.Render["RUN_ID"] != "" && m.Render["RUN_ID"] != m.RunID {
		c.fail("manifest", "run_id_mismatch")
	}
	if m.Helper.Reale2e != "" && (m.Helper.Reale2e != m.Executable.Path || m.Helper.Deadline != formatHelperDeadline(deadline)) {
		c.fail("manifest", "helper_recipe_mismatch")
	}
	// Every envelope names this run.
	for rel, v := range c.b.docs {
		if id, has := runIDOf(v); has && id != m.RunID {
			c.fail("manifest", "run_id_mismatch")
			c.source("manifest", rel)
		}
	}
	codes := slices.Clone(c.codes["manifest"])
	c.setupRow("manifest", "The manifest is the run's complete, consistent, inventory-bearing record and the event log is ordered", codes, []string{devManifestFile, "events.jsonl"})
}

// runIDOf reads a decoded record's run_id; has is false only for a record
// type without a run_id field (an envelope's empty run_id is a mismatch,
// never "no run").
func runIDOf(v any) (id string, has bool) {
	switch x := v.(type) {
	case interface{ runIDOf() string }:
		return x.runIDOf(), true
	}
	return "", false
}

func (e *devEnvelope[T]) runIDOf() string    { return e.RunID }
func (m *DevAdmissions) runIDOf() string     { return m.RunID }
func (m *DevCost) runIDOf() string           { return m.RunID }
func (m *DevFlowLabel) runIDOf() string      { return m.RunID }
func (m *DevFeatureRecord) runIDOf() string  { return m.RunID }
func (m *DevSeedPush) runIDOf() string       { return m.RunID }
func (m *DevEventMap) runIDOf() string       { return m.RunID }
func (m *DevCoverageRecord) runIDOf() string { return m.RunID }
func (m *DevObservation) runIDOf() string    { return m.RunID }
func (m *DevLogRecord) runIDOf() string      { return m.RunID }
func (m *DevAdmission) runIDOf() string      { return m.RunID }
func (m *DevReviewRecord) runIDOf() string   { return m.RunID }

// loadTrees reads the seed and every task ref's commit and tree from the
// validated evidence repository.
func (c *devChecker) loadTrees() {
	r := c.b.Repo
	if r == nil {
		return
	}
	for name, h := range r.refs {
		cm, err := r.repo.CommitObject(h)
		if err != nil {
			continue
		}
		files, err := readTree(r.repo, cm, name)
		if err != nil {
			continue
		}
		m := map[string]TreeFile{}
		for _, f := range files {
			m[f.Path] = f
		}
		c.trees[h.String()] = m
		c.treeHash[h.String()] = cm.TreeHash.String()
	}
}

// commitParent is a repository commit's single parent ("" otherwise).
func (c *devChecker) commitParent(commit string) (string, bool) {
	if c.b.Repo == nil {
		return "", false
	}
	cm, err := c.b.Repo.repo.CommitObject(plumbing.NewHash(commit))
	if err != nil || len(cm.ParentHashes) != 1 {
		return "", false
	}
	return cm.ParentHashes[0].String(), true
}

// manifestSchema decodes a manifest's schema discriminator: exactly one
// JSON object of valid UTF-8 without a duplicate key, whose schema is a
// string.
func manifestSchema(data []byte) (string, error) {
	if !utf8.Valid(data) {
		return "", errors.New("not valid UTF-8")
	}
	if err := checkDuplicateKeys(data); err != nil {
		return "", err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return "", errors.New("not one JSON object")
	}
	var s string
	if err := json.Unmarshal(raw["schema"], &s); err != nil {
		return "", errors.New("no string schema")
	}
	return s, nil
}

// safeEvidenceRoot resolves root once (an ancestor symlink is fine) and
// requires a real directory.
func safeEvidenceRoot(root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", schemaErr("evidence root: %v", err)
	}
	st, err := os.Lstat(resolved)
	if err != nil || !st.IsDir() {
		return "", schemaErr("evidence root is not a directory")
	}
	return resolved, nil
}

// lstatRegular reads dir/name when it is a regular file (no symlink),
// within the text bound.
func lstatRegular(dir, name string) ([]byte, error) {
	p := filepath.Join(dir, name)
	st, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, schemaErr("%s is not a regular file", name)
	}
	return readBounded(p, maxTextFile)
}
