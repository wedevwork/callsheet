package reale2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The Cursor probe's diagnostic observation receipt (design
// 12a-3-cursor-worker, slice 3a FP-6). Its finalization is factored out
// before the qualified-record finalizer: it writes
// cursor-probe-observation.json and .txt, never qualification.json, never
// sets d.passed and never calls CheckDevQualification. Every Cursor probe
// ends BLOCKED with exit 1: cursor_qualification_pending when every live
// stage and the strict environment and cleanup checks were observed (the
// independent Cursor evidence checker is slice 3b), otherwise the primary
// cause. reale2e check refuses such a receipt (diagnostic_not_qualification)
// before any routing or criterion.

// Receipt names and codes.
const (
	CursorObservationSchema         = "callsheet-cursor-probe-observation/v1"
	cursorObservationFile           = "cursor-probe-observation.json"
	cursorObservationText           = "cursor-probe-observation.txt"
	CodeDiagnosticNotQualification  = "diagnostic_not_qualification"
	ResultObserved                  = "OBSERVED"
	cursorQualificationPendingLine  = "qualification probe: BLOCKED (" + CodeCursorQualificationPending + ": independent Cursor evidence checking is deferred to slice 3b)"
	cursorObservationsLinePrefix    = "Cursor probe observations: "
	diagnosticNotQualificationError = "reale2e check: " + CodeDiagnosticNotQualification + ": slice 3a observations are not qualification evidence"
)

// CursorProbeObservation is cursor-probe-observation.json: diagnostic,
// never a pass format and never qualification evidence.
type CursorProbeObservation struct {
	Schema           string                 `json:"schema"`
	RunID            string                 `json:"run_id"`
	Slice            string                 `json:"slice"`
	Result           string                 `json:"result"`
	Qualification    bool                   `json:"qualification"`
	Blocker          string                 `json:"blocker"`
	Revision         string                 `json:"revision"`
	CallsheetVersion string                 `json:"callsheet_version"`
	GOOS             string                 `json:"goos"`
	GOARCH           string                 `json:"goarch"`
	Versions         []VersionRecord        `json:"versions"`
	Executable       DevExecutableRecord    `json:"executable"`
	FlowLabel        string                 `json:"flow_label"`
	SelectionSHA256  string                 `json:"selection_sha256"`
	Roles            []FlowRole             `json:"roles"`
	Scope            string                 `json:"scope"`
	StageCoverage    []string               `json:"stage_coverage"`
	Stages           []DevProbeStage        `json:"stages"`
	Calls            CursorProbeCalls       `json:"calls"`
	Phases           int                    `json:"phases"`
	AttemptStart     string                 `json:"attempt_start"`
	AttemptDeadline  string                 `json:"attempt_deadline"`
	StopCode         *string                `json:"stop_code"`
	StopSeq          *int                   `json:"stop_seq"`
	Coder            CursorCoderObservation `json:"coder"`
	Environment      CursorEnvObservation   `json:"environment"`
	Cleanup          CursorCleanupStatus    `json:"cleanup"`
	Activity         CursorActivitySummary  `json:"activity"`
	Artifacts        []string               `json:"artifacts"`
}

// CursorProbeCalls is the receipt's bounded call accounting: every
// admission (failed calls included) by kind, and the scope's bound.
type CursorProbeCalls struct {
	WorkerCalls    int `json:"worker_calls"`
	MaxWorkerCalls int `json:"max_worker_calls"`
	Preflights     int `json:"preflights"`
	Coder          int `json:"coder"`
	Coordinator    int `json:"coordinator"`
	Rejected       int `json:"rejected"`
}

// CursorCoderObservation is the coder stage's observation.
type CursorCoderObservation struct {
	Task                string `json:"task"`
	Workspace           string `json:"workspace"`
	Instance            string `json:"instance"`
	InputCommit         string `json:"input_commit"`
	ResultCommit        string `json:"result_commit"`
	Marker              string `json:"marker"`
	PublicationAdmitted bool   `json:"publication_admitted"`
	ValidationPassed    bool   `json:"validation_passed"`
	ChangedCoverage     string `json:"changed_coverage"`
}

// CursorEnvObservation is the strict owner-environment comparison,
// unchanged by the report-only samples.
type CursorEnvObservation struct {
	ProtectedChanges int      `json:"protected_changes"`
	Complete         bool     `json:"complete"`
	Codes            []string `json:"codes"`
}

// CursorCleanupStatus is the cleanup outcome.
type CursorCleanupStatus struct {
	Complete       bool `json:"complete"`
	RuntimeRemoved bool `json:"runtime_removed"`
}

// CursorActivitySummary names the two sample records and their compared
// markers (report-only, all processes, not attribution to Cursor).
type CursorActivitySummary struct {
	Before  string           `json:"before"`
	After   string           `json:"after"`
	Changes []ActivityChange `json:"changes"`
}

// ---- Activity phases ----

// cursorObserve takes one bounded shallow sample: the 5s budget clipped
// to the existing deadline, the run's evidence and runtime excluded.
func (d *devSupervisor) cursorObserve(phase string, clip time.Time) *ActivityAggregate {
	c := d.probe.cursor
	var exclude []string
	for _, x := range []string{d.s.evidenceDir, d.s.runtime} {
		if x == "" {
			continue
		}
		if cx, err := canonicalPath(x); err == nil {
			x = cx
		}
		exclude = append(exclude, x)
	}
	o := activityObserver{fs: c.fs, clock: d.s.w.Clock, uid: c.uid, exclude: exclude, output: c.output}
	deadline := minTime(d.s.w.Clock.Now().Add(activityScanBudget), clip)
	return o.observe(d.s.runID, phase, c.roots, deadline)
}

// cursorActivityBefore samples the roots right after the baseline
// fingerprint. Sample quality never fails the probe; only an evidence
// write failure does (the existing evidence failure).
func (d *devSupervisor) cursorActivityBefore() error {
	c := d.probe.cursor
	c.before = d.cursorObserve("before", d.attemptDeadline())
	if !d.writeActivity(cursorActivityFiles[0], c.before) {
		return d.halt()
	}
	return nil
}

// cursorEnvironment samples the roots again alongside the after-cleanup
// fingerprint (when the before sample exists) and records the strict
// comparison's outcome for the receipt.
func (d *devSupervisor) cursorEnvironment(diff *EnvDiff) {
	if d.probe == nil || d.probe.cursor == nil {
		return
	}
	c := d.probe.cursor
	if diff != nil {
		c.envProtected, c.envComplete, c.envCodes = len(diff.Protected), diff.Complete, slices.Clone(diff.Codes)
	}
	if c.before == nil {
		return
	}
	c.after = d.cursorObserve("after", d.s.cleanupBy)
	d.writeActivity(cursorActivityFiles[1], c.after)
}

// writeActivity writes one sample record compactly (its rows were bounded
// by their compact size); an encoding or write failure is the existing
// evidence failure, never a silently dropped record.
func (d *devSupervisor) writeActivity(rel string, agg *ActivityAggregate) bool {
	b, err := json.Marshal(agg)
	if err != nil {
		d.stop("evidence_write_failed", fmt.Sprintf("%s: %v", rel, err))
		return false
	}
	return d.writeEvidence(rel, append(b, '\n'))
}

// ---- Finalization ----

// cursorStages completes the stage list in scope order: the first stage
// a stop left unpassed failed, its successors were not reached.
func (d *devSupervisor) cursorStages() []DevProbeStage {
	p := d.probe
	names := cursorProbeStagesFor(p.scope)
	p.completeStages(names)
	return orderedStages(p.stages, names)
}

// collectCursorProbe writes the cost record, then the receipt and its
// text, and decides the observation (never a qualification).
func (d *devSupervisor) collectCursorProbe() {
	p, c := d.probe, d.probe.cursor
	stages := d.cursorStages()
	names := cursorProbeStagesFor(p.scope)
	calls := CursorProbeCalls{MaxWorkerCalls: cursorProbeMaxCalls(p.scope)}
	collectErr := ""
	b, err := loadDevLayout(d.s.ev.root, cursorProbeLayout(p.scope))
	if err == nil {
		cost := deriveDevCost(b, d.s.runID)
		err = d.s.ev.writeJSON(devCostFile, cost)
		calls.WorkerCalls, calls.Preflights, calls.Rejected = cost.WorkerCalls, len(cost.PreflightTasks), len(cost.RejectedTasks)
		for _, t := range cost.WorkflowTasks {
			if d.s.known[t] == probeCoderLabel {
				calls.Coder++
			} else {
				calls.Coordinator++
			}
		}
	}
	if err != nil {
		collectErr = "evidence_collection_failed"
		fmt.Fprintf(d.s.stderr, "reale2e probe-dev-full: evidence collection failed: %v\n", err)
	}
	blocker := ""
	switch {
	case d.stopCode != "":
		blocker = d.stopCode
	case d.protected:
		blocker = CodeEnvModified
		if c.envProtected == 0 {
			blocker = cmpOr(firstOf(c.envCodes), CodeEnvUnobserved)
		}
	case !d.cleanupOK:
		blocker = "cleanup_incomplete"
	case collectErr != "":
		blocker = collectErr
	case d.s.ev.failure() != nil:
		blocker = "evidence_write_failed"
	case !d.executed || !qualifiedStages(stages, names):
		blocker = "stage_unobserved"
	case calls.WorkerCalls > calls.MaxWorkerCalls:
		blocker = "cost_bound_exceeded"
	default:
		blocker = CodeCursorQualificationPending
	}
	c.blocker, c.result = blocker, ResultBlocked
	if blocker == CodeCursorQualificationPending {
		c.result = ResultObserved
	}
	obs := d.cursorReceipt(stages, calls)
	data, err := encodeJSON(obs)
	if err == nil {
		err = d.s.ev.writeFile(cursorObservationFile, data)
	}
	if err == nil {
		err = d.s.ev.writeFile(cursorObservationText, []byte(renderCursorObservation(obs, c.before, c.after)))
	}
	if err != nil {
		// The receipt itself could not be written: the existing evidence
		// failure, still BLOCKED.
		fmt.Fprintf(d.s.stderr, "reale2e probe-dev-full: %s not written: %v\n", cursorObservationFile, err)
		if c.result == ResultObserved {
			c.result, c.blocker = ResultBlocked, "evidence_write_failed"
		}
	}
	for _, s := range stages {
		d.s.say("probe stage %s: %s %s", s.Name, s.Result, s.Code)
	}
}

// firstOf is the first element ("" none).
func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// cursorReceipt assembles the receipt from the live state.
func (d *devSupervisor) cursorReceipt(stages []DevProbeStage, calls CursorProbeCalls) CursorProbeObservation {
	p, c := d.probe, d.probe.cursor
	obs := CursorProbeObservation{Schema: CursorObservationSchema, RunID: d.s.runID, Slice: "3a", Result: c.result, Qualification: false,
		Blocker: c.blocker, Revision: d.manifest.Revision, CallsheetVersion: d.manifest.CallsheetVersion, GOOS: d.s.goos, GOARCH: d.s.goarch,
		Versions: append([]VersionRecord{}, d.manifest.Versions...), Executable: d.manifest.Executable, FlowLabel: d.flowLabel,
		SelectionSHA256: d.manifest.SelectionSHA256, Roles: append([]FlowRole{}, d.manifest.Roles...), Scope: p.scope,
		StageCoverage: cursorProbeStagesFor(p.scope), Stages: stages, Calls: calls, Phases: len(d.phases), AttemptStart: d.manifest.AttemptStart,
		AttemptDeadline: d.manifest.AttemptDeadline,
		Coder: CursorCoderObservation{Task: c.task, Workspace: c.ws.Name, Instance: c.ws.Instance, InputCommit: hashOrEmpty(c.inputCommit),
			ResultCommit: c.resultCommit, Marker: c.marker, PublicationAdmitted: c.admitted, ValidationPassed: c.validated},
		Environment: CursorEnvObservation{ProtectedChanges: c.envProtected, Complete: c.envComplete && !d.protected, Codes: append([]string{}, c.envCodes...)},
		Cleanup:     CursorCleanupStatus{Complete: d.cleanupOK, RuntimeRemoved: d.cleanupOK && d.s.runtime != ""},
		Activity:    CursorActivitySummary{Changes: compareActivity(c.before, c.after)}, Artifacts: []string{}}
	if c.coverage != nil {
		obs.Coder.ChangedCoverage = fmt.Sprintf("%s%% (%d/%d)", c.coverage.Percent, c.coverage.Covered, c.coverage.Total)
	}
	if c.before != nil {
		obs.Activity.Before = cursorActivityFiles[0]
	}
	if c.after != nil {
		obs.Activity.After = cursorActivityFiles[1]
	}
	if d.stopCode != "" {
		code, seq := d.stopCode, d.stopSeq
		obs.StopCode, obs.StopSeq = &code, &seq
	}
	if b, err := readBounded(d.s.w.Executable, 1<<30); err == nil {
		obs.Executable.After = sha256Hex(b)
	}
	if inv, err := diskInventory(d.s.ev.root); err == nil {
		for _, e := range inv {
			obs.Artifacts = append(obs.Artifacts, e.Path)
		}
	}
	return obs
}

// cursorProbeExit prints the observation's two final lines and exits 1:
// a Cursor probe is never QUALIFIED.
func (d *devSupervisor) cursorProbeExit() int {
	c := d.probe.cursor
	if c.result == ResultObserved {
		fmt.Fprintln(d.s.stdout, cursorObservationsLinePrefix+ResultObserved)
		fmt.Fprintln(d.s.stdout, cursorQualificationPendingLine)
		return 1
	}
	code := cmpOr(c.blocker, cmpOr(d.stopCode, "stage_unobserved"))
	fmt.Fprintf(d.s.stdout, "%s%s (%s)\n", cursorObservationsLinePrefix, ResultBlocked, code)
	fmt.Fprintf(d.s.stdout, "qualification probe: %s (%s)\n", ResultBlocked, code)
	return 1
}

// renderCursorObservation is cursor-probe-observation.txt.
func renderCursorObservation(o CursorProbeObservation, before, after *ActivityAggregate) string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	if o.Result == ResultObserved {
		line("%s%s", cursorObservationsLinePrefix, ResultObserved)
		line("%s", cursorQualificationPendingLine)
	} else {
		line("%s%s (%s)", cursorObservationsLinePrefix, ResultBlocked, o.Blocker)
		line("qualification probe: %s (%s)", ResultBlocked, o.Blocker)
	}
	line("")
	line("Run %s, slice %s diagnostic probe, scope %s. qualification: false. This receipt is not qualification evidence: reale2e check refuses it (%s), and slice 3b needs a fresh probe.",
		o.RunID, o.Slice, o.Scope, CodeDiagnosticNotQualification)
	line("Selection %s (selection sha256 %s); revision %s; callsheet %s; %s/%s.", o.FlowLabel, o.SelectionSHA256, o.Revision, o.CallsheetVersion, o.GOOS, o.GOARCH)
	for _, r := range o.Roles {
		line("  role %s: %s %s/%s", r.ID, r.Adapter, r.Model, r.Effort)
	}
	for _, v := range o.Versions {
		line("  version %s: %s (%s, eligible %t)", v.Tool, v.Version, v.Path, v.Eligible)
	}
	line("Stages:")
	for _, s := range o.Stages {
		line("  %s: %s %s", s.Name, s.Result, s.Code)
	}
	line("Worker calls: %d of at most %d (preflights %d, coder %d, coordinator tasks %d, rejected %d); coordinator phases %d.", o.Calls.WorkerCalls,
		o.Calls.MaxWorkerCalls, o.Calls.Preflights, o.Calls.Coder, o.Calls.Coordinator, o.Calls.Rejected, o.Phases)
	line("Coder task %s: input %s, result %s, marker %s, publication admitted %t, independent validation passed %t, changed-code coverage %s.",
		cmpOr(o.Coder.Task, "-"), cmpOr(o.Coder.InputCommit, "-"), cmpOr(o.Coder.ResultCommit, "-"), cmpOr(o.Coder.Marker, "-"), o.Coder.PublicationAdmitted,
		o.Coder.ValidationPassed, cmpOr(o.Coder.ChangedCoverage, "-"))
	line("The validation proves the published edit and its independent validation, not that Cursor executed every command it reported.")
	line("Owner environment: %d protected changes, comparison complete %t %v; cleanup complete %t.", o.Environment.ProtectedChanges, o.Environment.Complete,
		o.Environment.Codes, o.Cleanup.Complete)
	line("")
	line("Cache/tmp activity (report-only shallow samples of all processes, not attribution to Cursor; sampling %s, depth %d, %d top-level entries per root,",
		activitySampling, activityMaxDepth, activityMaxEntries)
	line("%d children per sampled directory, %d bytes and %s per phase; no recursive total, no complete diff, no confinement claim):",
		activityMaxEntries, activityMaxOutput, activityScanBudget)
	for _, phase := range []*ActivityAggregate{before, after} {
		if phase == nil {
			continue
		}
		for _, r := range phase.Roots {
			line("  %s %s %s: status %s, exhaustive %t (top-level %t); sampled direct %s bytes %d; sampled children %s bytes %d; rows %d; flags %s",
				phase.Phase, r.Scope, r.Path, r.Status, r.Exhaustive, r.TopLevelExhaustive, countsText(r.ObservedCounts.Direct), r.ObservedFileBytes.Direct,
				countsText(r.ObservedCounts.Children), r.ObservedFileBytes.Children, len(r.Rows), flagsText(r.Flags))
			// Every retained row with its own child-sample quality flags
			// (a directory over 64 children is entry_limited there).
			for _, row := range r.Rows {
				line("    %s %s %s row %s: type %s, status %s; sampled children %s bytes %d; children exhaustive %t; flags %s", phase.Phase, r.Scope,
					r.Path, row.Name, row.Type, row.Status, countsText(row.ObservedCounts), row.ObservedFileBytes, row.ChildrenExhaustive, flagsText(row.Flags))
			}
		}
	}
	if len(o.Activity.Changes) == 0 {
		line("  compared rows: no marker")
	}
	for _, ch := range o.Activity.Changes {
		line("  %s %s: %s", ch.Scope, ch.Name, ch.Mark)
	}
	line("Not covered: deep or transient writes, other home paths (.local/bin, .npm), reads and network.")
	line("Approval-required branch (A2): not classified by the harness; read the coder task's state, exit, logs and final text.")
	return b.String()
}

// countsText renders sampled counts.
func countsText(c ActivityCounts) string {
	return fmt.Sprintf("file=%d directory=%d symlink=%d other=%d unknown=%d", c.File, c.Directory, c.Symlink, c.Other, c.Unknown)
}

// flagsText renders the set quality flags ("none" when clear).
func flagsText(f ActivityFlags) string {
	var out []string
	for _, x := range []struct {
		name string
		on   bool
	}{{"entry_limited", f.EntryLimited}, {"depth_limited", f.DepthLimited}, {"output_limited", f.OutputLimited}, {"time_limited", f.TimeLimited},
		{"numeric_overflow", f.NumericOverflow}} {
		if x.on {
			out = append(out, x.name)
		}
	}
	for _, x := range []struct {
		name string
		n    uint64
	}{{"foreign_entries", f.ForeignEntries}, {"unreadable_entries", f.UnreadableEntries}, {"raced_entries", f.RacedEntries},
		{"name_unrecorded", f.NameUnrecorded}, {"excluded_entries", f.ExcludedEntries}} {
		if x.n > 0 {
			out = append(out, fmt.Sprintf("%s=%d", x.name, x.n))
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ",")
}

// ---- Offline refusal ----

// checkDiagnosticObservation is CheckEvidence's first decision, on the
// root safeEvidenceRoot resolved and before bundleProtocol, any bundle
// load or criterion (and so before both generic version-list checks): a
// bounded, regular, non-symlink cursor-probe-observation.json with the
// diagnostic schema is refused with diagnostic_not_qualification, exit 2,
// no criterion evaluated. A missing file is not handled (existing
// routing); an unsafe, oversized, unreadable, malformed or unknown-schema
// file is the existing schema error, exit 2, never a fall-through.
func checkDiagnosticObservation(resolved string, out, errOut io.Writer) (int, bool) {
	data, err := lstatRegular(resolved, cursorObservationFile)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false
	}
	refuse := func(rep DevReport, msg string) (int, bool) {
		b, _ := encodeJSON(rep)
		out.Write(b)
		fmt.Fprintln(errOut, msg)
		return 2, true
	}
	if err == nil {
		var schema string
		if schema, err = manifestSchema(data); err == nil && schema != CursorObservationSchema {
			err = fmt.Errorf("unsupported schema %q", contract.SafeText(schema, 80))
		}
	}
	if err != nil {
		var se *SchemaError
		if !errors.As(err, &se) {
			err = schemaErr("%s: %v", cursorObservationFile, err)
		}
		return refuse(devSchemaReport(), fmt.Sprintf("reale2e check: %v", err))
	}
	rep := devSchemaReport()
	rep.Error = CodeDiagnosticNotQualification
	return refuse(rep, diagnosticNotQualificationError)
}
