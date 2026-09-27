package contract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"time"
)

// TaskRecord is one persisted task document (tasks/<task_id>.json): the
// validated request, the immutable role snapshot, the effective settings,
// the execution token, the lifecycle fields and the retained log. Derived
// public fields (elapsed time, recovery and persistence status) are never
// persisted.
type TaskRecord struct {
	TaskID                string
	Request               DispatchRequest
	Role                  RoleRecord
	RolesRevision         int
	Effective             TaskEffective
	Execution             ExecutionToken
	State                 string
	CreatedAt             time.Time
	StartedAt             *time.Time
	FinishedAt            *time.Time
	ExitCode              *int
	Signal                *string
	FinalMessage          *string
	FinalMessageTruncated bool
	Reason                *TaskReason
	Candidates            []TaskCandidate
	Log                   TaskLog
	Revision              int
}

// taskRecordWire is the ordered schema of tasks/<task_id>.json.
type taskRecordWire struct {
	SchemaVersion         int             `json:"schema_version"`
	TaskID                string          `json:"task_id"`
	Request               DispatchRequest `json:"request"`
	Role                  json.RawMessage `json:"role"`
	RolesRevision         int             `json:"roles_revision"`
	Effective             TaskEffective   `json:"effective"`
	TimeoutEnforced       bool            `json:"timeout_enforced"`
	Execution             ExecutionToken  `json:"execution"`
	State                 string          `json:"state"`
	CreatedAt             string          `json:"created_at"`
	StartedAt             *string         `json:"started_at"`
	FinishedAt            *string         `json:"finished_at"`
	ExitCode              *int            `json:"exit_code"`
	Signal                *string         `json:"signal"`
	FinalMessage          *string         `json:"final_message"`
	FinalMessageTruncated bool            `json:"final_message_truncated"`
	Reason                *TaskReason     `json:"reason"`
	Candidates            []TaskCandidate `json:"candidates"`
	Log                   json.RawMessage `json:"log"`
	Revision              int             `json:"revision"`
}

func timePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := FormatTime(*t)
	return &s
}

// logDataKey is the indented log data member EncodeTaskRecord splices the
// tail into: the only "data" key of a document, and quotes inside string
// values are always escaped, so it cannot occur elsewhere.
var logDataKey = []byte(`"data": ""`)

// EncodeTaskRecord renders the exact writer bytes: two-space indentation,
// schema field order, one final LF, no HTML escaping; the log tail is
// base64. The metadata is encoded first with an empty tail and the base64
// tail is then written once into an exact-size buffer, so a full 10 MiB
// tail costs one output buffer, not several copies.
func EncodeTaskRecord(r TaskRecord) ([]byte, error) {
	data := r.Log.Data
	r.Log.Data = nil
	meta, err := encodeTaskMeta(r)
	if err != nil {
		return nil, err
	}
	i := bytes.Index(meta, logDataKey)
	if i < 0 || bytes.Count(meta, logDataKey) != 1 {
		return nil, errInvalid("task record log is not encodable")
	}
	at := i + len(logDataKey) - 1 // between the two quotes
	out := make([]byte, len(meta)+base64.StdEncoding.EncodedLen(len(data)))
	n := copy(out, meta[:at])
	base64.StdEncoding.Encode(out[n:], data)
	n += base64.StdEncoding.EncodedLen(len(data))
	copy(out[n:], meta[at:])
	return out, nil
}

// encodeTaskMeta renders r (its log tail empty) in the writer form.
func encodeTaskMeta(r TaskRecord) ([]byte, error) {
	role, err := compact(r.Role)
	if err != nil {
		return nil, err
	}
	lg, err := compact(r.Log)
	if err != nil {
		return nil, err
	}
	cands := r.Candidates
	if cands == nil {
		cands = []TaskCandidate{}
	}
	w := taskRecordWire{SchemaVersion: TaskRecordSchemaVersion, TaskID: r.TaskID, Request: r.Request, Role: role, RolesRevision: r.RolesRevision,
		Effective: r.Effective, Execution: r.Execution, State: r.State, CreatedAt: FormatTime(r.CreatedAt), StartedAt: timePtr(r.StartedAt),
		FinishedAt: timePtr(r.FinishedAt), ExitCode: r.ExitCode, Signal: r.Signal, FinalMessage: r.FinalMessage,
		FinalMessageTruncated: r.FinalMessageTruncated, Reason: r.Reason, Candidates: cands, Log: lg, Revision: r.Revision}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(w); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ParseTaskRecord strictly decodes and validates one task document:
// schema and exact keys, task ID, request, role record and effective
// settings (against lookup), execution token, timestamps, lifecycle
// invariants, reason and candidates, base64 log and its counters, and the
// revision. Registry references are checked by the plane's loader.
func ParseTaskRecord(data []byte, lookup AdapterLookup) (TaskRecord, error) {
	const what = "task record"
	var w taskRecordWire
	if err := decodeStrict(data, &w, what); err != nil {
		return TaskRecord{}, err
	}
	if w.SchemaVersion != TaskRecordSchemaVersion {
		return TaskRecord{}, errInvalid("%s schema_version %d is not supported (this build supports %d)", what, w.SchemaVersion, TaskRecordSchemaVersion)
	}
	if !ValidTaskID(w.TaskID) {
		return TaskRecord{}, errInvalid("%s task_id is not a task ID", what)
	}
	role, err := ParseRoleRecord(w.Role, lookup)
	if err != nil {
		return TaskRecord{}, err
	}
	if err := CheckEffort(role.Adapter, w.Effective.Effort, lookup); err != nil {
		return TaskRecord{}, err
	}
	if w.RolesRevision < 0 || w.RolesRevision > MaxSafeInteger || w.Revision < 1 || w.Revision > MaxSafeInteger {
		return TaskRecord{}, errInvalid("%s revisions are out of range", what)
	}
	if w.TimeoutEnforced {
		return TaskRecord{}, errInvalid("%s timeout_enforced must be false in this build", what)
	}
	if err := validTimes(w.CreatedAt, w.StartedAt, w.FinishedAt, what); err != nil {
		return TaskRecord{}, err
	}
	if err := validLifecycle(w.State, w.StartedAt != nil, w.FinishedAt != nil, w.ExitCode, w.Signal, w.FinalMessage, w.FinalMessageTruncated, w.Reason != nil, len(w.Candidates), what); err != nil {
		return TaskRecord{}, err
	}
	if w.Reason != nil {
		if err := w.Reason.validate(what); err != nil {
			return TaskRecord{}, err
		}
	}
	if err := validateCandidates(w.Candidates, what); err != nil {
		return TaskRecord{}, err
	}
	var lg TaskLog
	if err := decodeStrict(w.Log, &lg, what+" log"); err != nil {
		return TaskRecord{}, err
	}
	if err := lg.Validate(); err != nil {
		return TaskRecord{}, err
	}
	parse := func(s *string) *time.Time {
		if s == nil {
			return nil
		}
		t, _ := ParseTime(*s)
		return &t
	}
	created, _ := ParseTime(w.CreatedAt)
	return TaskRecord{TaskID: w.TaskID, Request: w.Request, Role: role, RolesRevision: w.RolesRevision, Effective: w.Effective,
		Execution: w.Execution, State: w.State, CreatedAt: created, StartedAt: parse(w.StartedAt), FinishedAt: parse(w.FinishedAt),
		ExitCode: w.ExitCode, Signal: w.Signal, FinalMessage: w.FinalMessage, FinalMessageTruncated: w.FinalMessageTruncated,
		Reason: w.Reason, Candidates: w.Candidates, Log: lg, Revision: w.Revision}, nil
}
