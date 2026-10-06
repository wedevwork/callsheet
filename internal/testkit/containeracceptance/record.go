package containeracceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// EvidencePrefix starts every evidence line; the rest of the line is one
// compact JSON object.
const EvidencePrefix = "CALLSHEET_E2E_V1 "

// The 14 required case IDs: the eleven container test parents and the
// three partial-result subcases.
const (
	CaseRuntime            = "TestContainerRuntime"
	CaseCoordinator        = "TestContainerCoordinator"
	CasePublication        = "TestContainerPublication"
	CaseContinuation       = "TestContainerContinuation"
	CasePartialResults     = "TestContainerPartialResults"
	CasePartialFailed      = "TestContainerPartialResults/failed"
	CasePartialCancelled   = "TestContainerPartialResults/cancelled"
	CasePartialTimedOut    = "TestContainerPartialResults/timed_out"
	CaseLost               = "TestContainerLost"
	CasePublicationRestart = "TestContainerPublicationRestart"
	CaseDirtyPull          = "TestContainerDirtyPull"
	CaseClaims             = "TestContainerClaims"
	CaseEvidence           = "TestContainerEvidence"
	CaseSampleFlow         = "TestContainerSampleFlow"
)

// Phase IDs: the nested subtests of the coordinator and continuation
// parents, executable through RunCase but never emitted as records.
const (
	PhasePrepare    = CaseCoordinator + "/prepare"
	PhaseGoalAnswer = CaseCoordinator + "/goal_answer"
	// PhaseBackgroundWait (design nonblocking-coordinator-waits) verifies
	// goal_answer's stored background-wait proof; it dispatches nothing.
	PhaseBackgroundWait = CaseCoordinator + "/background_wait"
	PhaseSibling        = CaseContinuation + "/sibling"
	PhaseContinuation   = CaseContinuation + "/continuation"
)

// RequiredCaseIDs returns the exact 14 required case IDs.
func RequiredCaseIDs() []string {
	return []string{CaseRuntime, CaseCoordinator, CasePublication, CaseContinuation, CasePartialResults,
		CasePartialFailed, CasePartialCancelled, CasePartialTimedOut, CaseLost, CasePublicationRestart,
		CaseDirtyPull, CaseClaims, CaseEvidence, CaseSampleFlow}
}

// Outcomes.
const (
	OutcomePass = "pass"
	OutcomeFail = "fail"
)

// Task is one task observation in a case record. Nullable fields are nil
// when inapplicable to the task.
type Task struct {
	Label             string  `json:"label"`
	RoleID            string  `json:"role_id"`
	NodeID            string  `json:"node_id"`
	TaskID            string  `json:"task_id"`
	WorkspaceInstance *string `json:"workspace_instance"`
	BaseCommit        *string `json:"base_commit"`
	ResultCommit      *string `json:"result_commit"`
	ResultRef         *string `json:"result_ref"`
	TerminalState     string  `json:"terminal_state"`
	PublicationStatus *string `json:"publication_status"`
	ExitCode          *int    `json:"exit_code"`
	FinalMessage      *string `json:"final_message"`
}

// Common holds the fields every record carries.
type Common struct {
	RunID          string
	Iteration      int
	Total          int
	SourceRevision string
	Arch           string
	BinaryHashes   map[string]string
	Outcome        string
}

// CaseRecord is the case variant of Record. NodeBindings is set only on
// TestContainerRuntime's record.
type CaseRecord struct {
	Common
	CaseID       string
	Subcases     map[string]string
	Tasks        []Task
	Boundary     string
	Error        string
	NodeBindings map[string]string
}

// EndRecord is the end variant of Record: one per iteration, after
// cleanup.
type EndRecord struct {
	Common
	CaseIDs   []string
	CleanupOK bool
	Error     string
}

// Record is the tagged union of the two variants: exactly one is set.
type Record struct {
	Case *CaseRecord
	End  *EndRecord
}

// commonWire is the common fields in wire order.
func commonWire(kind string, c Common) map[string]any {
	hashes := c.BinaryHashes
	if hashes == nil {
		hashes = map[string]string{}
	}
	return map[string]any{"schema_version": 1, "kind": kind, "run_id": c.RunID, "iteration": c.Iteration, "total": c.Total,
		"source_revision": c.SourceRevision, "os": "linux", "arch": c.Arch, "binary_hashes": hashes, "adapter": "fake", "outcome": c.Outcome}
}

// MarshalJSON renders the case record; node_bindings is absent (never
// null) unless set.
func (r CaseRecord) MarshalJSON() ([]byte, error) {
	m := commonWire("case", r.Common)
	sub := r.Subcases
	if sub == nil {
		sub = map[string]string{}
	}
	tasks := r.Tasks
	if tasks == nil {
		tasks = []Task{}
	}
	m["case_id"], m["subcases"], m["tasks"], m["boundary"], m["error"] = r.CaseID, sub, tasks, r.Boundary, r.Error
	if r.NodeBindings != nil {
		m["node_bindings"] = r.NodeBindings
	}
	return json.Marshal(m)
}

// MarshalJSON renders the end record with its sorted case IDs.
func (r EndRecord) MarshalJSON() ([]byte, error) {
	m := commonWire("end", r.Common)
	ids := append([]string{}, r.CaseIDs...)
	sort.Strings(ids)
	m["case_ids"], m["cleanup_ok"], m["error"] = ids, r.CleanupOK, r.Error
	return json.Marshal(m)
}

// WriteRecord writes one evidence line: the prefix, one compact JSON
// object and a newline, in a single write.
func WriteRecord(w io.Writer, record Record) error {
	var v any
	switch {
	case (record.Case == nil) == (record.End == nil):
		return errors.New("containeracceptance: a record is exactly one of case and end")
	case record.Case != nil:
		v = *record.Case
	default:
		v = *record.End
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, b); err != nil {
		return err
	}
	line := append(append([]byte(EvidencePrefix), compact.Bytes()...), '\n')
	n, err := w.Write(line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("containeracceptance: writing a record: %w", err)
	}
	return nil
}
