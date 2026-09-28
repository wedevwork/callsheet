package contract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"time"
)

// TaskRecord is one persisted task document (tasks/<task_id>.json): the
// validated request, the immutable role snapshot, the effective settings,
// the original execution token and start digest, the lifecycle fields,
// the retained log and (iteration 06a) the late-evidence slot. Derived
// public fields (elapsed time, reconciliation and persistence status) are
// never persisted.
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
	// StartDigest is the canonical SHA-256 of the original task_start
	// body (lowercase hex); nil only for history migrated from schema 1.
	StartDigest *string
	// TimeoutPolicy is TimeoutPolicyLegacy in 06a.
	TimeoutPolicy string
	// ResultDigest is the committed worker outcome's digest, when a worker
	// result decided the terminal state.
	ResultDigest *string
	// Late is the single late-evidence slot (terminal records only).
	Late *LateResult
	// Schema is the on-disk schema the record was decoded from (1 or 2);
	// EncodeTaskRecord always writes schema 2.
	Schema int
}

// taskRecordWire is the ordered schema-2 form of tasks/<task_id>.json.
type taskRecordWire struct {
	SchemaVersion         int             `json:"schema_version"`
	TaskID                string          `json:"task_id"`
	Request               DispatchRequest `json:"request"`
	Role                  json.RawMessage `json:"role"`
	RolesRevision         int             `json:"roles_revision"`
	Effective             TaskEffective   `json:"effective"`
	TimeoutEnforced       bool            `json:"timeout_enforced"`
	TimeoutPolicy         string          `json:"timeout_policy"`
	Execution             ExecutionToken  `json:"execution"`
	StartDigest           *string         `json:"start_digest"`
	State                 string          `json:"state"`
	CreatedAt             string          `json:"created_at"`
	StartedAt             *string         `json:"started_at"`
	FinishedAt            *string         `json:"finished_at"`
	ExitCode              *int            `json:"exit_code"`
	Signal                *string         `json:"signal"`
	FinalMessage          *string         `json:"final_message"`
	FinalMessageTruncated bool            `json:"final_message_truncated"`
	ResultDigest          *string         `json:"result_digest"`
	Reason                *TaskReason     `json:"reason"`
	Candidates            []TaskCandidate `json:"candidates"`
	Log                   json.RawMessage `json:"log"`
	LateResult            *lateWire       `json:"late_result"`
	Revision              int             `json:"revision"`
}

// legacyTaskRecordWire is iteration 05's exact schema-1 document.
type legacyTaskRecordWire struct {
	SchemaVersion         int               `json:"schema_version"`
	TaskID                string            `json:"task_id"`
	Request               DispatchRequest   `json:"request"`
	Role                  json.RawMessage   `json:"role"`
	RolesRevision         int               `json:"roles_revision"`
	Effective             TaskEffective     `json:"effective"`
	TimeoutEnforced       bool              `json:"timeout_enforced"`
	Execution             ExecutionToken    `json:"execution"`
	State                 string            `json:"state"`
	CreatedAt             string            `json:"created_at"`
	StartedAt             *string           `json:"started_at"`
	FinishedAt            *string           `json:"finished_at"`
	ExitCode              *int              `json:"exit_code"`
	Signal                *string           `json:"signal"`
	FinalMessage          *string           `json:"final_message"`
	FinalMessageTruncated bool              `json:"final_message_truncated"`
	Reason                *TaskReason       `json:"reason"`
	Candidates            []legacyCandidate `json:"candidates"`
	Log                   json.RawMessage   `json:"log"`
	Revision              int               `json:"revision"`
}

// legacyCandidate is iteration 05's candidate sample, whose uncertain
// reservation count was recovery_inflight; it converts to the schema-2
// reconciling_inflight count it became.
type legacyCandidate struct {
	RoleID            string `json:"role_id"`
	NodeID            string `json:"node_id"`
	RegistrationOrder int    `json:"registration_order"`
	NodeLiveness      string `json:"node_liveness"`
	Inflight          int    `json:"inflight"`
	RecoveryInflight  int    `json:"recovery_inflight"`
	Concurrency       int    `json:"concurrency"`
	CanAccept         bool   `json:"can_accept"`
	Reason            string `json:"reason"`
}

func timePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := FormatTime(*t)
	return &s
}

// logDataKey is the indented log data member EncodeTaskRecord splices a
// tail into. Quotes inside string values are always escaped, so it occurs
// only as the data member of the primary log and, when present, of the
// late result's log, in that order.
var logDataKey = []byte(`"data": ""`)

// EncodeTaskRecord renders the exact schema-2 writer bytes: two-space
// indentation, schema field order, one final LF, no HTML escaping; the log
// tails are base64. The metadata is encoded first with empty tails and
// each base64 tail is then written once into an exact-size buffer, so a
// full 10 MiB tail costs one output buffer, not several copies.
func EncodeTaskRecord(r TaskRecord) ([]byte, error) {
	tails := [][]byte{r.Log.Data}
	r.Log.Data = nil
	if r.Late != nil {
		late := *r.Late
		tails = append(tails, late.Log.Data)
		late.Log.Data = nil
		r.Late = &late
	}
	meta, err := encodeTaskMeta(r)
	if err != nil {
		return nil, err
	}
	if bytes.Count(meta, logDataKey) != len(tails) {
		return nil, errInvalid("task record log is not encodable")
	}
	size := len(meta)
	for _, t := range tails {
		size += base64.StdEncoding.EncodedLen(len(t))
	}
	out := make([]byte, 0, size)
	rest := meta
	for _, t := range tails {
		i := bytes.Index(rest, logDataKey)
		at := i + len(logDataKey) - 1 // between the two quotes
		out = append(out, rest[:at]...)
		n := len(out)
		out = out[:n+base64.StdEncoding.EncodedLen(len(t))]
		base64.StdEncoding.Encode(out[n:], t)
		rest = rest[at:]
	}
	return append(out, rest...), nil
}

// encodeTaskMeta renders r (its log tails empty) in the writer form.
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
	policy := r.TimeoutPolicy
	if policy == "" {
		policy = TimeoutPolicyLegacy
	}
	w := taskRecordWire{SchemaVersion: TaskRecordSchemaVersion, TaskID: r.TaskID, Request: r.Request, Role: role, RolesRevision: r.RolesRevision,
		Effective: r.Effective, TimeoutPolicy: policy, Execution: r.Execution, StartDigest: r.StartDigest, State: r.State,
		CreatedAt: FormatTime(r.CreatedAt), StartedAt: timePtr(r.StartedAt), FinishedAt: timePtr(r.FinishedAt), ExitCode: r.ExitCode,
		Signal: r.Signal, FinalMessage: r.FinalMessage, FinalMessageTruncated: r.FinalMessageTruncated, ResultDigest: r.ResultDigest,
		Reason: r.Reason, Candidates: cands, Log: lg, Revision: r.Revision}
	if l := r.Late; l != nil {
		llg, err := compact(l.Log)
		if err != nil {
			return nil, err
		}
		w.LateResult = &lateWire{Digest: l.Digest, ReceivedAt: FormatTime(l.ReceivedAt), Outcome: l.Outcome, ExitCode: l.ExitCode, Signal: l.Signal,
			FinalMessage: l.FinalMessage, FinalMessageTruncated: l.FinalMessageTruncated, OutputBytes: l.OutputBytes,
			LogIncomplete: l.LogIncomplete, CounterOverflow: l.CounterOverflow, Log: llg}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(w); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ParseTaskRecord strictly decodes and validates one task document of
// schema 2, or of schema 1 with iteration 05's exact validation converted
// in memory (timeout_policy legacy_unenforced, null start and result
// digests, no late result; Schema reports 1). It checks the task ID,
// request, role record and effective settings (against lookup), the
// execution token, timestamps, lifecycle invariants, reason and
// candidates, the base64 logs and their counters, the combined tail bound
// and the revision. Registry references are checked by the plane's loader.
func ParseTaskRecord(data []byte, lookup AdapterLookup) (TaskRecord, error) {
	const what = "task record"
	// A light probe of the schema (one scan, no copy of the tails); the
	// schema's strict decoder then validates every key, duplicates
	// included.
	var probe struct {
		SchemaVersion json.RawMessage `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return TaskRecord{}, errInvalid("%s is not a JSON object", what)
	}
	raw := probe.SchemaVersion
	if raw == nil {
		return TaskRecord{}, errInvalid("%s lacks the required field %q", what, "schema_version")
	}
	switch v, _ := ParseInteger(string(bytes.TrimSpace(raw))); v {
	case LegacyTaskRecordSchemaVersion:
		return parseLegacyTaskRecord(data, lookup)
	case TaskRecordSchemaVersion:
	default:
		return TaskRecord{}, errInvalid("%s schema_version %s is not supported (this build supports %d and %d)", what, SafeText(string(raw), 32),
			LegacyTaskRecordSchemaVersion, TaskRecordSchemaVersion)
	}
	var w taskRecordWire
	if err := decodeStrict(data, &w, what); err != nil {
		return TaskRecord{}, err
	}
	rec, err := commonRecord(w.TaskID, w.Role, w.Effective, w.RolesRevision, w.Revision, w.TimeoutEnforced, w.CreatedAt, w.StartedAt,
		w.FinishedAt, w.State, w.ExitCode, w.Signal, w.FinalMessage, w.FinalMessageTruncated, w.Reason, w.Candidates, w.Log, lookup, what)
	if err != nil {
		return TaskRecord{}, err
	}
	switch {
	case w.TimeoutPolicy != TimeoutPolicyLegacy:
		return TaskRecord{}, errInvalid("%s timeout_policy must be %q in this build", what, TimeoutPolicyLegacy)
	case w.StartDigest != nil && !ValidDigest(*w.StartDigest):
		return TaskRecord{}, errInvalid("%s start_digest must be null or 64 lowercase hex digits", what)
	case w.ResultDigest != nil && !ValidDigest(*w.ResultDigest):
		return TaskRecord{}, errInvalid("%s result_digest must be null or 64 lowercase hex digits", what)
	case w.ResultDigest != nil && !TaskTerminal(w.State):
		return TaskRecord{}, errInvalid("%s: a %s task has no result digest", what, w.State)
	case w.LateResult != nil && !TaskTerminal(w.State):
		return TaskRecord{}, errInvalid("%s: a %s task has no late result", what, w.State)
	}
	rec.Request, rec.Execution, rec.StartDigest, rec.ResultDigest = w.Request, w.Execution, w.StartDigest, w.ResultDigest
	rec.TimeoutPolicy, rec.Schema = w.TimeoutPolicy, TaskRecordSchemaVersion
	if lw := w.LateResult; lw != nil {
		at, ok := ParseTime(lw.ReceivedAt)
		if !ok {
			return TaskRecord{}, errInvalid("%s late_result received_at must be a UTC RFC3339 timestamp", what)
		}
		var lg TaskLog
		if err := decodeStrict(lw.Log, &lg, what+" late_result log"); err != nil {
			return TaskRecord{}, err
		}
		l := LateResult{Digest: lw.Digest, ReceivedAt: at, Outcome: lw.Outcome, ExitCode: lw.ExitCode, Signal: lw.Signal, FinalMessage: lw.FinalMessage,
			FinalMessageTruncated: lw.FinalMessageTruncated, OutputBytes: lw.OutputBytes, LogIncomplete: lw.LogIncomplete,
			CounterOverflow: lw.CounterOverflow, Log: lg}
		if err := l.validate(what); err != nil {
			return TaskRecord{}, err
		}
		if err := checkTails(len(rec.Log.Data), len(lg.Data), what); err != nil {
			return TaskRecord{}, err
		}
		rec.Late = &l
	}
	return rec, nil
}

// parseLegacyTaskRecord decodes a schema-1 document with iteration 05's
// exact validation (no lost state, no digests) and converts it.
func parseLegacyTaskRecord(data []byte, lookup AdapterLookup) (TaskRecord, error) {
	const what = "task record"
	var w legacyTaskRecordWire
	if err := decodeStrict(data, &w, what); err != nil {
		return TaskRecord{}, err
	}
	if w.State == TaskLost {
		return TaskRecord{}, errInvalid("%s state %q is not a schema 1 state", what, w.State)
	}
	var cands []TaskCandidate
	for _, c := range w.Candidates {
		cands = append(cands, TaskCandidate{RoleID: c.RoleID, NodeID: c.NodeID, RegistrationOrder: c.RegistrationOrder, NodeLiveness: c.NodeLiveness,
			Inflight: c.Inflight, ReconcilingInflight: c.RecoveryInflight, Concurrency: c.Concurrency, CanAccept: c.CanAccept, Reason: c.Reason})
	}
	rec, err := commonRecord(w.TaskID, w.Role, w.Effective, w.RolesRevision, w.Revision, w.TimeoutEnforced, w.CreatedAt, w.StartedAt,
		w.FinishedAt, w.State, w.ExitCode, w.Signal, w.FinalMessage, w.FinalMessageTruncated, w.Reason, cands, w.Log, lookup, what)
	if err != nil {
		return TaskRecord{}, err
	}
	rec.Request, rec.Execution = w.Request, w.Execution
	rec.TimeoutPolicy, rec.Schema = TimeoutPolicyLegacy, LegacyTaskRecordSchemaVersion
	return rec, nil
}

// commonRecord validates the fields both schemas share.
func commonRecord(id string, roleRaw json.RawMessage, eff TaskEffective, rolesRev, rev int, enforced bool, created string, started, finished *string,
	state string, exit *int, signal, final *string, truncated bool, reason *TaskReason, cands []TaskCandidate, logRaw json.RawMessage,
	lookup AdapterLookup, what string) (TaskRecord, error) {
	if !ValidTaskID(id) {
		return TaskRecord{}, errInvalid("%s task_id is not a task ID", what)
	}
	role, err := ParseRoleRecord(roleRaw, lookup)
	if err != nil {
		return TaskRecord{}, err
	}
	if err := CheckEffort(role.Adapter, eff.Effort, lookup); err != nil {
		return TaskRecord{}, err
	}
	if rolesRev < 0 || rolesRev > MaxSafeInteger || rev < 1 || rev > MaxSafeInteger {
		return TaskRecord{}, errInvalid("%s revisions are out of range", what)
	}
	if enforced {
		return TaskRecord{}, errInvalid("%s timeout_enforced must be false in this build", what)
	}
	if err := validTimes(created, started, finished, what); err != nil {
		return TaskRecord{}, err
	}
	if err := validLifecycle(state, started != nil, finished != nil, exit, signal, final, truncated, reason != nil, len(cands), what); err != nil {
		return TaskRecord{}, err
	}
	if reason != nil {
		if err := reason.validate(state, what); err != nil {
			return TaskRecord{}, err
		}
	}
	if err := validateCandidates(cands, what); err != nil {
		return TaskRecord{}, err
	}
	var lg TaskLog
	if err := decodeStrict(logRaw, &lg, what+" log"); err != nil {
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
	c, _ := ParseTime(created)
	return TaskRecord{TaskID: id, Role: role, RolesRevision: rolesRev, Effective: eff, State: state, CreatedAt: c, StartedAt: parse(started),
		FinishedAt: parse(finished), ExitCode: exit, Signal: signal, FinalMessage: final, FinalMessageTruncated: truncated, Reason: reason,
		Candidates: cands, Log: lg, Revision: rev}, nil
}

// checkTails enforces the combined decoded tail bound of a record with
// late evidence: primary and late tails together at most 10 MiB.
func checkTails(primary, late int, what string) error {
	if primary+late > MaxLogRetainedBytes {
		return errInvalid("%s primary and late log tails exceed %d bytes together", what, MaxLogRetainedBytes)
	}
	return nil
}
