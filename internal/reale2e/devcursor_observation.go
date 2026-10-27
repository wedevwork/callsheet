package reale2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The historical slice 3a diagnostic observation receipt (design
// 12a-3-cursor-worker, slice 3a FP-6). Slice 3a's Cursor probe wrote
// cursor-probe-observation.json and .txt, never qualification.json, and
// always exited 1; slice 3b replaced that live finalization with checked
// qualification (devcursor_check.go). The receipt's schema and its
// offline refusal stay: reale2e check, and LoadDevQualification, refuse a
// directory holding one (diagnostic_not_qualification) before any routing
// or criterion, so no diagnostic receipt, alone or beside other records,
// is ever read as qualification. Historical receipts are never rewritten
// or promoted.

// Receipt names and codes.
const (
	CursorObservationSchema         = "callsheet-cursor-probe-observation/v1"
	cursorObservationFile           = "cursor-probe-observation.json"
	cursorObservationText           = "cursor-probe-observation.txt"
	CodeDiagnosticNotQualification  = "diagnostic_not_qualification"
	ResultObserved                  = "OBSERVED"
	diagnosticNotQualificationError = "reale2e check: " + CodeDiagnosticNotQualification + ": slice 3a observations are not qualification evidence"
)

// CursorProbeObservation is the historical cursor-probe-observation.json:
// diagnostic, never a pass format and never qualification evidence.
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

// CursorProbeCalls is the receipt's bounded call accounting.
type CursorProbeCalls struct {
	WorkerCalls    int `json:"worker_calls"`
	MaxWorkerCalls int `json:"max_worker_calls"`
	Preflights     int `json:"preflights"`
	Coder          int `json:"coder"`
	Coordinator    int `json:"coordinator"`
	Rejected       int `json:"rejected"`
}

// CursorCoderObservation is the receipt's coder stage observation.
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

// CursorEnvObservation is the receipt's strict owner-environment
// comparison.
type CursorEnvObservation struct {
	ProtectedChanges int      `json:"protected_changes"`
	Complete         bool     `json:"complete"`
	Codes            []string `json:"codes"`
}

// CursorCleanupStatus is the receipt's cleanup outcome.
type CursorCleanupStatus struct {
	Complete       bool `json:"complete"`
	RuntimeRemoved bool `json:"runtime_removed"`
}

// CursorActivitySummary names the two sample records and their compared
// markers (report-only, all processes, not attribution to Cursor): the
// historical receipt's, and since slice 3b the Cursor qualification
// report's and Cursor attempt report's recomputed summary.
type CursorActivitySummary struct {
	Before  string           `json:"before"`
	After   string           `json:"after"`
	Changes []ActivityChange `json:"changes"`
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

// diagnosticReceipt reads a resolved evidence root's diagnostic receipt:
// present reports a cursor-probe-observation.json entry; err is nil only
// for a bounded, regular, non-symlink file of the diagnostic schema, and
// otherwise the existing schema error (never a fall-through).
func diagnosticReceipt(resolved string) (present bool, err error) {
	data, err := lstatRegular(resolved, cursorObservationFile)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
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
		return true, err
	}
	return true, nil
}

// checkDiagnosticObservation is CheckEvidence's first decision, on the
// root safeEvidenceRoot resolved and before bundleProtocol, any bundle
// load or criterion (and so before both generic version-list checks): a
// bounded, regular, non-symlink cursor-probe-observation.json with the
// diagnostic schema is refused with diagnostic_not_qualification, exit 2,
// no criterion evaluated. A missing file is not handled (existing
// routing); an unsafe, oversized, unreadable, malformed or unknown-schema
// file is the existing schema error, exit 2, never a fall-through.
func checkDiagnosticObservation(resolved string, out, errOut io.Writer) (int, bool) {
	present, err := diagnosticReceipt(resolved)
	if !present {
		return 0, false
	}
	refuse := func(rep DevReport, msg string) (int, bool) {
		b, _ := encodeJSON(rep)
		out.Write(b)
		fmt.Fprintln(errOut, msg)
		return 2, true
	}
	if err != nil {
		return refuse(devSchemaReport(), fmt.Sprintf("reale2e check: %v", err))
	}
	rep := devSchemaReport()
	rep.Error = CodeDiagnosticNotQualification
	return refuse(rep, diagnosticNotQualificationError)
}
