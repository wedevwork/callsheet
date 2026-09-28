package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Task wire, API and limit constants (iteration 05).
const (
	// PathTasks is the task collection; one task is PathTasks + "/" + ID
	// and its retained output PathTasks + "/" + ID + "/logs".
	PathTasks = "/api/v1/tasks"

	// Protocol 3 task frames.
	FrameTaskStart       = "task_start"
	FrameTaskStartResult = "task_start_result"
	FrameTaskLog         = "task_log"
	FrameTaskLogAck      = "task_log_ack"
	FrameTaskResult      = "task_result"
	FrameTaskResultAck   = "task_result_ack"

	// Per-type task body limits; the other task frames use MaxOtherBody.
	MaxTaskStartBody  = 512 << 10
	MaxTaskLogBody    = 32 << 10
	MaxTaskResultBody = 512 << 10

	// Dispatch request limits (UTF-8 bytes).
	MaxGoalBytes            = 16 << 10
	MaxAcceptanceBytes      = 16 << 10
	MaxPayloadPointers      = 64
	MaxPointerBytes         = 2 << 10
	MaxPayloadBytes         = 64 << 10
	MaxDispatchRequestBytes = 256 << 10
	MaxRequesterBytes       = 128

	// Output and result limits.
	MaxLogChunkBytes      = 16 << 10
	MaxLogRetainedBytes   = 10 << 20
	MaxFinalMessageBytes  = 64 << 10
	MaxReasonMessageBytes = 256
	MaxCandidates         = 100
	MaxCandidateReason    = 128
	MaxPromptBytes        = 16 << 20
	MaxManualBytes        = 1 << 20

	// HTTP response limits (client decode bounds).
	MaxTaskViewBytes = 2 << 20
	MaxTaskListBytes = 1 << 20
	MaxTaskLogsBytes = 16 << 20

	// Read parameters.
	DefaultTailLines     = 20
	MaxTailLines         = 200
	MaxTailBytes         = 64 << 10
	DefaultTaskListLimit = 100
	MaxTaskListLimit     = 100

	// TaskRecordSchemaVersion is tasks/<id>.json's schema written by this
	// build (iteration 06b: stop intent and enforced timeout policy);
	// TaskRecordSchema2 is iteration 06a's and LegacyTaskRecordSchemaVersion
	// iteration 05's, both still decoded strictly with their exact wire
	// structs and normalized in memory (never rewritten by a read).
	TaskRecordSchemaVersion       = 3
	TaskRecordSchema2             = 2
	LegacyTaskRecordSchemaVersion = 1

	// TimeoutPolicyLegacy is 06a's (and earlier) history: the recorded
	// timeout was never enforced and never acquires an execution timer.
	// TimeoutPolicyEnforced (iteration 06b) is every new dispatch's: the
	// task's guardian enforces its effective timeout (0: unlimited).
	TimeoutPolicyLegacy   = "legacy_unenforced"
	TimeoutPolicyEnforced = "enforced"

	// ReconcilingNotice is the fixed text for tasks whose execution awaits
	// reconciliation with its worker (iteration 06a).
	ReconcilingNotice = "the task's execution awaits reconciliation with its worker; its reservation is held until it is resolved"
	// TimeoutNotice is the fixed text about a historical task's recorded,
	// unenforced timeout (timeout_policy legacy_unenforced).
	TimeoutNotice = "this historical task predates timeout enforcement: its recorded timeout is not enforced"
)

// ValidTimeoutPolicy reports whether p is a known timeout policy.
func ValidTimeoutPolicy(p string) bool { return p == TimeoutPolicyLegacy || p == TimeoutPolicyEnforced }

// Task states. Iteration 06b adds the control states cancelled and
// timed_out (protocol 5), both terminal.
const (
	TaskPending   = "pending"
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
	TaskRejected  = "rejected"
	TaskLost      = "lost"
	TaskCancelled = "cancelled"
	TaskTimedOut  = "timed_out"
)

// ValidTaskState reports whether s is a state this build produces.
func ValidTaskState(s string) bool {
	switch s {
	case TaskPending, TaskRunning, TaskSucceeded, TaskFailed, TaskRejected, TaskLost, TaskCancelled, TaskTimedOut:
		return true
	}
	return false
}

// TaskTerminal reports whether s is a terminal state.
func TaskTerminal(s string) bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskRejected, TaskLost, TaskCancelled, TaskTimedOut:
		return true
	}
	return false
}

// Task error and diagnostic reasons (safe, fixed codes).
const (
	ReasonWorkspaceNotSupported = "workspace_not_supported"
	ReasonNoCapacity            = "no_capacity"
	ReasonTasksInflight         = "tasks_inflight"
	ReasonStartNotSent          = "start_not_sent"

	// Iteration 06b controls: a task cancelled before any start was sent
	// or launched (the reason of a cancelled record without started_at);
	// the plane-wide wait registration bound (unavailable details); a
	// fenced role instance (candidate reason and conflict detail); a force
	// removal token naming no fence on the role's current instance.
	ReasonCancelledBeforeStart     = "cancelled_before_start"
	ReasonWaitCapacity             = "wait_capacity"
	ReasonRoleRemoving             = "role_removing"
	ReasonRemovalOperationMismatch = "removal_operation_mismatch"

	// Sidecar start refusals.
	ReasonRoleMissing           = "role_missing"
	ReasonRoleChanged           = "role_changed"
	ReasonLocalFull             = "local_full"
	ReasonManualTooLarge        = "manual_too_large"
	ReasonManualInvalidText     = "manual_invalid_text"
	ReasonExecutableUnavailable = "executable_unavailable"
	ReasonPreparationTimeout    = "preparation_timeout"
	ReasonScratchUnavailable    = "scratch_unavailable"
	ReasonStartFailed           = "start_failed"

	// Candidate reasons, in precedence order.
	ReasonStorageUnconfirmed = "storage_unconfirmed"
	ReasonNodeDetached       = "node_detached"
	ReasonRoleUnsynced       = "role_unsynced"
	ReasonWorkerUnready      = "worker_unready"
	ReasonFull               = "full"
	ReasonAvailable          = "available"

	ReasonResultStorageUnconfirmed = "result_storage_unconfirmed"

	// Lost reasons (iteration 06a): why the plane resolved an execution
	// lost. Lost means outcome and cleanup unconfirmed, never success.
	ReasonLeaseExpired          = "lease_expired"
	ReasonStartupGraceExpired   = "startup_grace_expired"
	ReasonExecutionMissing      = "execution_missing"
	ReasonLegacyUnrecoverable   = "legacy_execution_unrecoverable"
	ReasonWorkerLost            = "worker_lost"
	ReasonNoLateResult          = "no_late_result"
	ReasonPreparationOverrun    = "preparation_overrun"
	ReasonResultConflict        = "result_conflict"
	ReasonUnknownExecution      = "unknown_execution"
	ReasonReconciliationPending = "reconciliation_pending"
)

// LostReasons are the codes a lost record's reason may carry.
var LostReasons = []string{ReasonLeaseExpired, ReasonStartupGraceExpired, ReasonExecutionMissing, ReasonLegacyUnrecoverable, ReasonWorkerLost}

func knownLost(r string) bool {
	for _, s := range LostReasons {
		if s == r {
			return true
		}
	}
	return false
}

// StartRefusalReasons are the safe reasons a sidecar may give in a
// task_start_result refusal.
var StartRefusalReasons = []string{
	ReasonRoleMissing, ReasonRoleChanged, ReasonLocalFull, ReasonManualUnreadable, ReasonManualNotRegular,
	ReasonManualTooLarge, ReasonManualInvalidText, ReasonAdapterDisabled, ReasonExecutableUnavailable,
	ReasonPreparationTimeout, ReasonScratchUnavailable, ReasonStartFailed,
}

func knownRefusal(r string) bool {
	for _, s := range StartRefusalReasons {
		if s == r {
			return true
		}
	}
	return false
}

// taskReasonCode reports whether c may appear as a record's reason code
// in state (a rejection's, a loss's, or a cancellation's before start).
func taskReasonCode(state, c string) bool {
	switch state {
	case TaskLost:
		return knownLost(c)
	case TaskCancelled:
		return c == ReasonCancelledBeforeStart
	}
	return c == ReasonStartNotSent || knownRefusal(c)
}

var (
	taskIDRE   = regexp.MustCompile(`^t_[0-9a-f]{32}$`)
	epochRE    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hostnameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// ValidTaskID reports whether id is t_ plus 32 lowercase hex digits.
func ValidTaskID(id string) bool { return taskIDRE.MatchString(id) }

// ValidEpoch reports whether e is a plane-run epoch: 32 lowercase hex.
func ValidEpoch(e string) bool { return epochRE.MatchString(e) }

// ValidHostname reports whether h is a requester hostname:
// [A-Za-z0-9][A-Za-z0-9._-]{0,127}, no DNS resolution or normalization.
func ValidHostname(h string) bool { return hostnameRE.MatchString(h) }

// taskErr is a task error with safe field and reason details.
func taskErr(code Code, field, reason, format string, args ...any) *Error {
	d := map[string]any{}
	if field != "" {
		d["field"] = field
	}
	if reason != "" {
		d["reason"] = reason
	}
	if len(d) == 0 {
		d = nil
	}
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Details: d}
}

// TaskError returns a task error of code with the given safe field and
// reason details (empty values are omitted).
func TaskError(code Code, field, reason, format string, args ...any) *Error {
	return taskErr(code, field, reason, format, args...)
}

// TaskTarget selects a role instance: exactly one kind, never inferred.
type TaskTarget struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Target kinds.
const (
	TargetID   = "id"
	TargetName = "name"
)

func (t *TaskTarget) unmarshalStrict(raw json.RawMessage, what string) error {
	type plain TaskTarget
	var p plain
	if err := decodeStrict(raw, &p, what); err != nil {
		return taskErr(CodeInvalidArgument, "target", "", "%s", err.(*Error).Message)
	}
	if p.Kind != TargetID && p.Kind != TargetName {
		return taskErr(CodeInvalidArgument, "target", "", "target kind must be %q or %q", TargetID, TargetName)
	}
	if !ValidSlug(p.Value) {
		return taskErr(CodeInvalidArgument, "target", "", "target value must be a 1-63 character role slug of lowercase letters, digits and internal hyphens")
	}
	*t = TaskTarget(p)
	return nil
}

// RequestedBy is self-reported audit context, never authorization.
type RequestedBy struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
}

// Validate checks the requester grammar.
func (r RequestedBy) Validate() error {
	switch {
	case !ValidSoftwareVersion(r.Name):
		return taskErr(CodeInvalidArgument, "requested_by", "", "requested_by name must be 1-128 printable ASCII bytes without spaces")
	case !ValidSoftwareVersion(r.Version):
		return taskErr(CodeInvalidArgument, "requested_by", "", "requested_by version must be 1-128 printable ASCII bytes without spaces")
	case !ValidHostname(r.Hostname):
		return taskErr(CodeInvalidArgument, "requested_by", "", "requested_by hostname must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
	}
	return nil
}

func (r *RequestedBy) unmarshalStrict(raw json.RawMessage, what string) error {
	type plain RequestedBy
	var p plain
	if err := decodeStrict(raw, &p, what); err != nil {
		return taskErr(CodeInvalidArgument, "requested_by", "", "%s", err.(*Error).Message)
	}
	if err := RequestedBy(p).Validate(); err != nil {
		return err
	}
	*r = RequestedBy(p)
	return nil
}

// TaskOverride holds the per-task settings a dispatch replaces; nil
// fields inherit the selected role's.
type TaskOverride struct {
	Model   *string
	Effort  *string
	Timeout *time.Duration
}

// MarshalJSON renders the present fields; timeout canonical.
func (o TaskOverride) MarshalJSON() ([]byte, error) {
	type wire struct {
		Model   *string `json:"model,omitempty"`
		Effort  *string `json:"effort,omitempty"`
		Timeout *string `json:"timeout,omitempty"`
	}
	w := wire{Model: o.Model, Effort: o.Effort}
	if o.Timeout != nil {
		s := o.Timeout.String()
		w.Timeout = &s
	}
	return compact(w)
}

func (o *TaskOverride) unmarshalStrict(raw json.RawMessage, what string) error {
	ob, err := decodeObject(raw, "override")
	if err != nil {
		return taskErr(CodeInvalidArgument, "override", "", "%s", err.(*Error).Message)
	}
	if err := rejectUnknown(ob, "override", []string{"model", "effort", "timeout"}); err != nil {
		return err
	}
	if len(ob.keys) == 0 {
		return taskErr(CodeInvalidArgument, "override", "", "override must set at least one of model, effort and timeout")
	}
	var out TaskOverride
	for _, k := range ob.keys {
		v := ob.raw[k]
		if isNull(v) {
			return taskErr(CodeInvalidArgument, k, "", "override field %q must not be null", k)
		}
		s, err := strictString(bytes.TrimSpace(v), "override field "+strconvQuote(k))
		if err != nil {
			return taskErr(CodeInvalidArgument, k, "", "%s", err.(*Error).Message)
		}
		switch k {
		case "model":
			if !ValidModel(s) {
				return taskErr(CodeInvalidArgument, "model", "", "model must be nonblank text of 1-1024 bytes of valid UTF-8 without control characters")
			}
			out.Model = &s
		case "effort":
			if s == "" {
				return taskErr(CodeInvalidArgument, "effort", "", "effort must not be empty")
			}
			out.Effort = &s
		case "timeout":
			d, err := ParseRoleTimeout(s)
			if err != nil {
				return err
			}
			out.Timeout = &d
		}
	}
	*o = out
	return nil
}

// DispatchRequest is POST /api/v1/tasks: what a coordinator asks for.
// It never carries manual bodies, argv or environment.
type DispatchRequest struct {
	Target      TaskTarget
	Goal        string
	Payload     []string
	Acceptance  string
	RequestedBy RequestedBy
	Override    *TaskOverride
}

// MarshalJSON renders the canonical request in field order; override is
// omitted when nil.
func (r DispatchRequest) MarshalJSON() ([]byte, error) {
	type wire struct {
		Target      TaskTarget    `json:"target"`
		Goal        string        `json:"goal"`
		Payload     []string      `json:"payload"`
		Acceptance  string        `json:"acceptance"`
		RequestedBy RequestedBy   `json:"requested_by"`
		Override    *TaskOverride `json:"override,omitempty"`
	}
	w := wire{Target: r.Target, Goal: r.Goal, Payload: r.Payload, Acceptance: r.Acceptance, RequestedBy: r.RequestedBy, Override: r.Override}
	if w.Payload == nil {
		w.Payload = []string{}
	}
	return compact(w)
}

// Validate checks every field constraint of an already decoded request
// (the client uses it before sending).
func (r DispatchRequest) Validate() error {
	if r.Target.Kind != TargetID && r.Target.Kind != TargetName {
		return taskErr(CodeInvalidArgument, "target", "", "target kind must be %q or %q", TargetID, TargetName)
	}
	if !ValidSlug(r.Target.Value) {
		return taskErr(CodeInvalidArgument, "target", "", "target value must be a 1-63 character role slug of lowercase letters, digits and internal hyphens")
	}
	if err := checkTaskText("goal", r.Goal, MaxGoalBytes); err != nil {
		return err
	}
	if err := checkTaskText("acceptance", r.Acceptance, MaxAcceptanceBytes); err != nil {
		return err
	}
	if err := checkPayload(r.Payload); err != nil {
		return err
	}
	if err := r.RequestedBy.Validate(); err != nil {
		return err
	}
	if o := r.Override; o != nil {
		if o.Model == nil && o.Effort == nil && o.Timeout == nil {
			return taskErr(CodeInvalidArgument, "override", "", "override must set at least one of model, effort and timeout")
		}
		if o.Model != nil && !ValidModel(*o.Model) {
			return taskErr(CodeInvalidArgument, "model", "", "model must be nonblank text of 1-1024 bytes of valid UTF-8 without control characters")
		}
		if o.Effort != nil && *o.Effort == "" {
			return taskErr(CodeInvalidArgument, "effort", "", "effort must not be empty")
		}
		if o.Timeout != nil && *o.Timeout < 0 {
			return taskErr(CodeInvalidArgument, "timeout", "", "timeout must not be negative")
		}
	}
	b, err := compact(r)
	if err != nil || len(b) > MaxDispatchRequestBytes {
		return taskErr(CodeInvalidArgument, "", "", "the encoded dispatch request exceeds %d bytes", MaxDispatchRequestBytes)
	}
	return nil
}

// checkTaskText validates goal and acceptance: nonblank valid UTF-8 of at
// most max bytes without NUL; preserved verbatim otherwise.
func checkTaskText(field, s string, max int) error {
	switch {
	case !utf8.ValidString(s):
		return taskErr(CodeInvalidArgument, field, "", "%s is not valid UTF-8", field)
	case strings.TrimSpace(s) == "":
		return taskErr(CodeInvalidArgument, field, "", "%s must not be empty or whitespace only", field)
	case len(s) > max:
		return taskErr(CodeInvalidArgument, field, "", "%s is %d bytes; at most %d are allowed", field, len(s), max)
	case strings.IndexByte(s, 0) >= 0:
		return taskErr(CodeInvalidArgument, field, "", "%s must not contain NUL", field)
	}
	return nil
}

// checkPayload validates the opaque pointer list.
func checkPayload(p []string) error {
	if len(p) > MaxPayloadPointers {
		return taskErr(CodeInvalidArgument, "payload", "", "payload has %d pointers; at most %d are allowed", len(p), MaxPayloadPointers)
	}
	total := 0
	for _, s := range p {
		switch {
		case s == "":
			return taskErr(CodeInvalidArgument, "payload", "", "payload pointers must not be empty")
		case !utf8.ValidString(s):
			return taskErr(CodeInvalidArgument, "payload", "", "payload pointers must be valid UTF-8")
		case len(s) > MaxPointerBytes:
			return taskErr(CodeInvalidArgument, "payload", "", "a payload pointer is %d bytes; at most %d are allowed", len(s), MaxPointerBytes)
		case strings.IndexByte(s, 0) >= 0:
			return taskErr(CodeInvalidArgument, "payload", "", "payload pointers must not contain NUL")
		}
		total += len(s)
	}
	if total > MaxPayloadBytes {
		return taskErr(CodeInvalidArgument, "payload", "", "payload pointers total %d bytes; at most %d are allowed", total, MaxPayloadBytes)
	}
	return nil
}

var dispatchFields = []string{"target", "goal", "payload", "acceptance", "requested_by"}

// ParseDispatchRequest strictly decodes a dispatch request body: the
// encoded size bound, then workspace/base and wait refusals, then exact
// keys (no unknown, duplicate or null field), then every field in order.
func ParseDispatchRequest(data []byte) (DispatchRequest, error) {
	if len(data) > MaxDispatchRequestBytes {
		return DispatchRequest{}, taskErr(CodeInvalidArgument, "", "", "the dispatch request is larger than %d bytes", MaxDispatchRequestBytes)
	}
	return parseDispatch(data, "dispatch request")
}

func (r *DispatchRequest) unmarshalStrict(raw json.RawMessage, what string) error {
	if len(raw) > MaxDispatchRequestBytes {
		return taskErr(CodeInvalidArgument, "", "", "%s is larger than %d bytes", what, MaxDispatchRequestBytes)
	}
	p, err := parseDispatch(raw, what)
	if err != nil {
		return err
	}
	*r = p
	return nil
}

func parseDispatch(data []byte, what string) (DispatchRequest, error) {
	o, err := decodeObject(data, what)
	if err != nil {
		return DispatchRequest{}, err
	}
	return parseDispatchObject(o, what)
}

// MaxDispatchEnvelopeBytes bounds a dispatch transport envelope: the
// request plus its optional wait option (iteration 06b).
const MaxDispatchEnvelopeBytes = MaxDispatchRequestBytes + 1<<10

// ParseDispatchEnvelope strictly decodes POST /api/v1/tasks (iteration
// 06b): a dispatch request plus the optional transport member "wait" (a Go
// duration of at most MaxWait). The wait is a transport option, validated
// before admission and never part of the stored request or its start
// digest; nil means an ordinary asynchronous dispatch.
func ParseDispatchEnvelope(data []byte) (DispatchRequest, *time.Duration, error) {
	const what = "dispatch request"
	if len(data) > MaxDispatchEnvelopeBytes {
		return DispatchRequest{}, nil, taskErr(CodeInvalidArgument, "", "", "the dispatch request is larger than %d bytes", MaxDispatchEnvelopeBytes)
	}
	o, err := decodeObject(data, what)
	if err != nil {
		return DispatchRequest{}, nil, err
	}
	var wait *time.Duration
	if v, ok := o.raw["wait"]; ok {
		if isNull(v) {
			return DispatchRequest{}, nil, taskErr(CodeInvalidArgument, "wait", "", "%s field %q must not be null", what, "wait")
		}
		s, err := strictString(bytes.TrimSpace(v), what+" field \"wait\"")
		if err != nil {
			return DispatchRequest{}, nil, taskErr(CodeInvalidArgument, "wait", "", "%s", err.(*Error).Message)
		}
		d, err := ParseWaitDuration(s)
		if err != nil {
			return DispatchRequest{}, nil, err
		}
		wait = &d
		delete(o.raw, "wait")
		keys := o.keys[:0:0]
		for _, k := range o.keys {
			if k != "wait" {
				keys = append(keys, k)
			}
		}
		o.keys = keys
	}
	r, err := parseDispatchObject(o, what)
	if err != nil {
		return r, nil, err
	}
	if err := r.Validate(); err != nil {
		return r, nil, err
	}
	return r, wait, nil
}

// parseDispatchObject decodes an already split request object.
func parseDispatchObject(o object, what string) (DispatchRequest, error) {
	var r DispatchRequest
	var err error
	for _, k := range []string{"workspace", "base"} {
		if _, ok := o.raw[k]; ok {
			return r, taskErr(CodeInvalidArgument, k, ReasonWorkspaceNotSupported, "workspace and base are not supported in this build (iteration 10 adds workspaces); nothing was dispatched")
		}
	}
	if err := rejectUnknown(o, what, append(append([]string{}, dispatchFields...), "override")); err != nil {
		return r, err
	}
	for _, k := range dispatchFields {
		v, ok := o.raw[k]
		if !ok {
			return r, taskErr(CodeInvalidArgument, k, "", "%s lacks the required field %q", what, k)
		}
		if isNull(v) {
			return r, taskErr(CodeInvalidArgument, k, "", "%s field %q must not be null", what, k)
		}
		v = bytes.TrimSpace(v)
		switch k {
		case "target":
			err = r.Target.unmarshalStrict(v, "target")
		case "goal", "acceptance":
			var s string
			if s, err = strictString(v, what+" field "+strconvQuote(k)); err != nil {
				return r, taskErr(CodeInvalidArgument, k, "", "%s", err.(*Error).Message)
			}
			if k == "goal" {
				r.Goal, err = s, checkTaskText(k, s, MaxGoalBytes)
			} else {
				r.Acceptance, err = s, checkTaskText(k, s, MaxAcceptanceBytes)
			}
		case "payload":
			var elems []json.RawMessage
			if elems, err = array(v, "payload"); err != nil {
				return r, taskErr(CodeInvalidArgument, "payload", "", "payload must be an array of strings")
			}
			if len(elems) > MaxPayloadPointers {
				return r, taskErr(CodeInvalidArgument, "payload", "", "payload has %d pointers; at most %d are allowed", len(elems), MaxPayloadPointers)
			}
			r.Payload = make([]string, 0, len(elems))
			for _, e := range elems {
				s, err := strictString(bytes.TrimSpace(e), "payload pointer")
				if err != nil {
					return r, taskErr(CodeInvalidArgument, "payload", "", "%s", err.(*Error).Message)
				}
				r.Payload = append(r.Payload, s)
			}
			err = checkPayload(r.Payload)
		case "requested_by":
			err = r.RequestedBy.unmarshalStrict(v, "requested_by")
		}
		if err != nil {
			return r, err
		}
	}
	if v, ok := o.raw["override"]; ok {
		if isNull(v) {
			return r, taskErr(CodeInvalidArgument, "override", "", "%s field %q must not be null", what, "override")
		}
		r.Override = &TaskOverride{}
		if err := r.Override.unmarshalStrict(bytes.TrimSpace(v), "override"); err != nil {
			return r, err
		}
	}
	return r, nil
}

// TaskEffective is the settings a task runs with: the role's, replaced
// by the request's override. Timeout 0 means unlimited; it is recorded,
// never enforced in this build.
type TaskEffective struct {
	Model   string
	Effort  string
	Timeout time.Duration
}

// MarshalJSON renders model, effort and the canonical timeout.
func (e TaskEffective) MarshalJSON() ([]byte, error) {
	return compact(struct {
		Model   string `json:"model"`
		Effort  string `json:"effort"`
		Timeout string `json:"timeout"`
	}{e.Model, e.Effort, e.Timeout.String()})
}

func (e *TaskEffective) unmarshalStrict(raw json.RawMessage, what string) error {
	var w struct {
		Model   string `json:"model"`
		Effort  string `json:"effort"`
		Timeout string `json:"timeout"`
	}
	if err := decodeStrict(raw, &w, what); err != nil {
		return err
	}
	if !ValidModel(w.Model) || w.Effort == "" {
		return errInvalid("%s model and effort must be valid", what)
	}
	d, err := ParseRoleTimeout(w.Timeout)
	if err != nil || d.String() != w.Timeout {
		return errInvalid("%s timeout must be a canonical nonnegative Go duration", what)
	}
	*e = TaskEffective{Model: w.Model, Effort: w.Effort, Timeout: d}
	return nil
}

// CheckEffort validates effort against the role's adapter metadata.
func CheckEffort(adapterID, effort string, lookup AdapterLookup) error {
	if lookup == nil {
		return nil
	}
	info, ok := lookup(adapterID)
	if !ok {
		return fieldErr("adapter", "unknown adapter; registered adapters: fake")
	}
	for _, e := range info.Efforts {
		if e == effort {
			return nil
		}
	}
	return fieldErr("effort", "effort is not allowed for adapter %s; allowed: %s", adapterID, strings.Join(info.Efforts, ", "))
}

// ResolveEffective applies o to role's resolved settings and validates
// the result against the role's adapter: an incompatible override is
// invalid_argument for this role (no other instance is tried).
func ResolveEffective(role RoleRecord, o *TaskOverride, lookup AdapterLookup) (TaskEffective, error) {
	r := role.Resolved()
	e := TaskEffective{Model: r.Model, Effort: r.Effort, Timeout: r.Timeout}
	if o != nil {
		if o.Model != nil {
			e.Model = *o.Model
		}
		if o.Effort != nil {
			e.Effort = *o.Effort
		}
		if o.Timeout != nil {
			e.Timeout = *o.Timeout
		}
	}
	if !ValidModel(e.Model) {
		return e, fieldErr("model", "model must be nonblank text of 1-1024 bytes of valid UTF-8 without control characters")
	}
	if e.Timeout < 0 {
		return e, fieldErr("timeout", "timeout must not be negative")
	}
	return e, CheckEffort(r.Adapter, e.Effort, lookup)
}

// ExecutionToken correlates a task with the plane run (epoch) and the
// node attachment generation that admitted it. It fences, never
// authenticates.
type ExecutionToken struct {
	Epoch      string `json:"epoch"`
	Attachment int    `json:"attachment"`
}

func (t *ExecutionToken) unmarshalStrict(raw json.RawMessage, what string) error {
	type plain ExecutionToken
	var p plain
	if err := decodeStrict(raw, &p, what); err != nil {
		return err
	}
	if !ValidEpoch(p.Epoch) || p.Attachment < 1 || p.Attachment > MaxSafeInteger {
		return errInvalid("%s must be a 32-hex epoch and a positive attachment generation", what)
	}
	*t = ExecutionToken(p)
	return nil
}

// ---- Protocol 3 task frames ----

// TaskStartBody is the plane's task_start request: the admitted role
// snapshot, the validated request and the effective settings.
type TaskStartBody struct {
	TaskID        string          `json:"task_id"`
	Execution     ExecutionToken  `json:"execution"`
	RolesRevision int             `json:"roles_revision"`
	Role          RoleRecord      `json:"role"`
	Request       DispatchRequest `json:"request"`
	Effective     TaskEffective   `json:"effective"`
}

// Digest is the SHA-256 of the canonical encoding (start deduplication).
func (b TaskStartBody) Digest() [32]byte {
	enc, err := compact(b)
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256(enc)
}

// DecodeTaskStart strictly decodes a task_start body; the role record is
// validated against lookup and the effective effort against its adapter.
func DecodeTaskStart(body json.RawMessage, lookup AdapterLookup) (TaskStartBody, error) {
	const what = "task_start body"
	var w struct {
		TaskID        string          `json:"task_id"`
		Execution     ExecutionToken  `json:"execution"`
		RolesRevision int             `json:"roles_revision"`
		Role          json.RawMessage `json:"role"`
		Request       DispatchRequest `json:"request"`
		Effective     TaskEffective   `json:"effective"`
	}
	if err := decodeStrict(body, &w, what); err != nil {
		return TaskStartBody{}, err
	}
	if !ValidTaskID(w.TaskID) {
		return TaskStartBody{}, errInvalid("%s task_id is not a task ID", what)
	}
	if w.RolesRevision < 0 || w.RolesRevision > MaxSafeInteger {
		return TaskStartBody{}, errInvalid("%s roles_revision must be an integer from 0 to %d", what, MaxSafeInteger)
	}
	role, err := ParseRoleRecord(w.Role, lookup)
	if err != nil {
		return TaskStartBody{}, err
	}
	if err := CheckEffort(role.Adapter, w.Effective.Effort, lookup); err != nil {
		return TaskStartBody{}, err
	}
	return TaskStartBody{TaskID: w.TaskID, Execution: w.Execution, RolesRevision: w.RolesRevision, Role: role, Request: w.Request, Effective: w.Effective}, nil
}

// TaskStartResult is the sidecar's task_start_result: {task_id, ok:true}
// (a child exists) or {task_id, ok:false, error} (definitely no child).
type TaskStartResult struct {
	TaskID string
	Err    *Error
}

// MarshalJSON renders the exact success or refusal shape.
func (r TaskStartResult) MarshalJSON() ([]byte, error) {
	id, _ := compact(r.TaskID)
	if r.Err == nil {
		return []byte(`{"task_id":` + string(id) + `,"ok":true}`), nil
	}
	e, err := r.Err.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return append([]byte(`{"task_id":`+string(id)+`,"ok":false,`), e[1:]...), nil
}

// DecodeTaskStartResult decodes a task_start_result body. A refusal's code
// is invalid_argument, unavailable or internal and its reason one of
// StartRefusalReasons.
func DecodeTaskStartResult(body json.RawMessage) (TaskStartResult, error) {
	const what = "task_start_result body"
	o, err := decodeObject(body, what)
	if err != nil {
		return TaskStartResult{}, err
	}
	if raw, ok := o.raw["ok"]; !ok || isNull(raw) {
		return TaskStartResult{}, errInvalid("%s lacks the required field %q", what, "ok")
	}
	okv, err := o.boolean(what, "ok")
	if err != nil {
		return TaskStartResult{}, err
	}
	var r TaskStartResult
	if okv {
		err = o.only(what, []string{"task_id", "ok"})
	} else {
		err = o.only(what, []string{"task_id", "ok", "error"})
	}
	if err != nil {
		return r, err
	}
	if r.TaskID, err = strictString(bytes.TrimSpace(o.raw["task_id"]), what+" task_id"); err != nil || !ValidTaskID(r.TaskID) {
		return r, errInvalid("%s task_id is not a task ID", what)
	}
	if okv {
		return r, nil
	}
	e, err := ParseErrorBody(append(append([]byte(`{"error":`), o.raw["error"]...), '}'))
	if err != nil {
		return r, err
	}
	reason, _ := e.Details["reason"].(string)
	if !validationResultCode(e.Code) || !knownRefusal(reason) {
		return r, errInvalid("%s refusal must carry an allowed code and reason", what)
	}
	r.Err = e
	return r, nil
}

// TaskLogBody is one task_log request: at most MaxLogChunkBytes of raw
// output at an absolute merged offset. LateDigest (protocol 4) is null
// for primary output, or the digest of the frozen outcome whose retained
// tail a reconciled worker replays as late evidence.
type TaskLogBody struct {
	TaskID     string         `json:"task_id"`
	Execution  ExecutionToken `json:"execution"`
	Offset     int            `json:"offset"`
	Data       []byte         `json:"data"`
	LateDigest *string        `json:"late_digest"`
}

// DecodeTaskLog decodes a task_log body.
func DecodeTaskLog(body json.RawMessage) (TaskLogBody, error) {
	const what = "task_log body"
	var b TaskLogBody
	if err := decodeStrict(body, &b, what); err != nil {
		return b, err
	}
	switch {
	case !ValidTaskID(b.TaskID):
		return b, errInvalid("%s task_id is not a task ID", what)
	case len(b.Data) < 1 || len(b.Data) > MaxLogChunkBytes:
		return b, errInvalid("%s data must hold 1 to %d bytes", what, MaxLogChunkBytes)
	case b.Offset < 0 || b.Offset > MaxSafeInteger-len(b.Data):
		return b, errInvalid("%s offset plus length must be at most %d", what, MaxSafeInteger)
	case b.LateDigest != nil && !ValidDigest(*b.LateDigest):
		return b, errInvalid("%s late_digest must be null or 64 lowercase hex digits", what)
	}
	return b, nil
}

// TaskLogAckBody acknowledges receipt of output into bounded memory.
type TaskLogAckBody struct {
	TaskID     string `json:"task_id"`
	NextOffset int    `json:"next_offset"`
}

// DecodeTaskLogAck decodes a task_log_ack body.
func DecodeTaskLogAck(body json.RawMessage) (TaskLogAckBody, error) {
	const what = "task_log_ack body"
	var b TaskLogAckBody
	if err := decodeStrict(body, &b, what); err != nil {
		return b, err
	}
	if !ValidTaskID(b.TaskID) || b.NextOffset < 0 || b.NextOffset > MaxSafeInteger {
		return b, errInvalid("%s must name a task and an offset from 0 to %d", what, MaxSafeInteger)
	}
	return b, nil
}

// Result outcomes (protocol 4): natural is the adapter's own exit; lost
// is a worker that cannot report a trustworthy outcome (it restarted or
// stopped while the execution was active, or no adapter ever ran).
// Protocol 5 (iteration 06b) adds the control outcomes: cancelled (a
// cleanup-confirmed control cause) and timed_out (the guardian's execution
// deadline).
const (
	OutcomeNatural   = "natural"
	OutcomeLost      = "lost"
	OutcomeCancelled = "cancelled"
	OutcomeTimedOut  = "timed_out"
)

// validOutcome reports whether o is a result outcome.
func validOutcome(o string) bool {
	return o == OutcomeNatural || o == OutcomeLost || o == OutcomeCancelled || o == OutcomeTimedOut
}

// ValidStopID reports whether id is a stop intent ID: 32 lowercase hex.
func ValidStopID(id string) bool { return runIDRE.MatchString(id) }

// TaskResultBody reports an execution's frozen outcome. The sidecar sends
// no state: the plane derives the terminal state. Digest is the SHA-256
// (lowercase hex) of the canonical encoding of every other field
// (ResultDigest); a retry repeats the same body and digest. StopID
// (protocol 5) names the plane stop intent a control outcome answers
// (null otherwise); a null stop_id keeps the exact protocol 4 digest.
type TaskResultBody struct {
	TaskID                string         `json:"task_id"`
	Execution             ExecutionToken `json:"execution"`
	Outcome               string         `json:"outcome"`
	ExitCode              *int           `json:"exit_code"`
	Signal                *string        `json:"signal"`
	FinalMessage          *string        `json:"final_message"`
	FinalMessageTruncated bool           `json:"final_message_truncated"`
	OutputBytes           int            `json:"output_bytes"`
	LogIncomplete         bool           `json:"log_incomplete"`
	CounterOverflow       bool           `json:"counter_overflow"`
	StopID                *string        `json:"stop_id"`
	Digest                string         `json:"digest"`
}

// resultDigestBody is TaskResultBody without its digest and stop_id, in
// wire order: the exact protocol 4 field sequence.
type resultDigestBody struct {
	TaskID                string         `json:"task_id"`
	Execution             ExecutionToken `json:"execution"`
	Outcome               string         `json:"outcome"`
	ExitCode              *int           `json:"exit_code"`
	Signal                *string        `json:"signal"`
	FinalMessage          *string        `json:"final_message"`
	FinalMessageTruncated bool           `json:"final_message_truncated"`
	OutputBytes           int            `json:"output_bytes"`
	LogIncomplete         bool           `json:"log_incomplete"`
	CounterOverflow       bool           `json:"counter_overflow"`
}

// resultDigestStop is resultDigestBody with a nonnull stop_id.
type resultDigestStop struct {
	resultDigestBody
	StopID string `json:"stop_id"`
}

// ResultDigest is the canonical digest of b's fields other than Digest: a
// null stop_id is omitted (the old field sequence), a nonnull one is
// included last.
func (b TaskResultBody) ResultDigest() string {
	base := resultDigestBody{b.TaskID, b.Execution, b.Outcome, b.ExitCode, b.Signal, b.FinalMessage, b.FinalMessageTruncated,
		b.OutputBytes, b.LogIncomplete, b.CounterOverflow}
	var enc []byte
	var err error
	if b.StopID == nil {
		enc, err = compact(base)
	} else {
		enc, err = compact(resultDigestStop{base, *b.StopID})
	}
	if err != nil {
		return ""
	}
	return HexDigest(sha256.Sum256(enc))
}

// Sealed returns b with its Digest set.
func (b TaskResultBody) Sealed() TaskResultBody {
	b.Digest = b.ResultDigest()
	return b
}

// Validate checks the outcome, exit, stop, final message and counter
// invariants and that the digest covers the other fields. A natural
// outcome has exactly one of exit_code and signal and no stop_id; lost and
// control outcomes carry at most one; timed_out is the guardian's cause
// and never names a plane stop intent.
func (b TaskResultBody) Validate() error {
	const what = "task_result body"
	switch {
	case !ValidTaskID(b.TaskID):
		return errInvalid("%s task_id is not a task ID", what)
	case !validOutcome(b.Outcome):
		return errInvalid("%s outcome must be %q, %q, %q or %q", what, OutcomeNatural, OutcomeLost, OutcomeCancelled, OutcomeTimedOut)
	case b.Outcome == OutcomeNatural && (b.ExitCode == nil) == (b.Signal == nil):
		return errInvalid("%s needs exactly one of exit_code and signal", what)
	case b.ExitCode != nil && b.Signal != nil:
		return errInvalid("%s has both exit_code and signal", what)
	case b.ExitCode != nil && (*b.ExitCode < 0 || *b.ExitCode > 255):
		return errInvalid("%s exit_code must be an integer from 0 to 255", what)
	case b.Signal != nil && !ValidWireSignal(*b.Signal):
		return errInvalid("%s signal is not a known signal name", what)
	case b.StopID != nil && !ValidStopID(*b.StopID):
		return errInvalid("%s stop_id must be null or 32 lowercase hex digits", what)
	case b.StopID != nil && (b.Outcome == OutcomeNatural || b.Outcome == OutcomeTimedOut):
		return errInvalid("%s: a %s outcome has no stop_id", what, b.Outcome)
	case b.OutputBytes < 0 || b.OutputBytes > MaxSafeInteger:
		return errInvalid("%s output_bytes must be an integer from 0 to %d", what, MaxSafeInteger)
	case !ValidDigest(b.Digest):
		return errInvalid("%s digest must be 64 lowercase hex digits", what)
	case b.Digest != b.ResultDigest():
		return errInvalid("%s digest does not cover its fields", what)
	}
	return checkFinalMessage(b.FinalMessage, b.FinalMessageTruncated, what)
}

func checkFinalMessage(m *string, truncated bool, what string) error {
	if m == nil {
		if truncated {
			return errInvalid("%s: a null final_message requires final_message_truncated=false", what)
		}
		return nil
	}
	if !utf8.ValidString(*m) || len(*m) > MaxFinalMessageBytes {
		return errInvalid("%s final_message must be valid UTF-8 of at most %d bytes", what, MaxFinalMessageBytes)
	}
	return nil
}

// DecodeTaskResult decodes a task_result body.
func DecodeTaskResult(body json.RawMessage) (TaskResultBody, error) {
	var b TaskResultBody
	if err := decodeStrict(body, &b, "task_result body"); err != nil {
		return b, err
	}
	return b, b.Validate()
}

// TaskResultAckBody acknowledges a result (protocol 4): received is
// receipt into the plane's bounded memory; committed=true means the
// outcome with this digest is durable (as the terminal result or as late
// evidence), the only acknowledgement that authorizes the worker to
// delete its outbox. committed=false means retry the same result later.
type TaskResultAckBody struct {
	TaskID    string `json:"task_id"`
	Digest    string `json:"digest"`
	Received  bool   `json:"received"`
	Committed bool   `json:"committed"`
}

// DecodeTaskResultAck decodes a task_result_ack body: received must be true.
func DecodeTaskResultAck(body json.RawMessage) (TaskResultAckBody, error) {
	const what = "task_result_ack body"
	var b TaskResultAckBody
	if err := decodeStrict(body, &b, what); err != nil {
		return b, err
	}
	if !ValidTaskID(b.TaskID) || !b.Received || !ValidDigest(b.Digest) {
		return b, errInvalid("%s must name a task and a digest with received=true", what)
	}
	return b, nil
}

// ---- Signals ----

// WireSignals is the closed set of signal names a task_result may carry
// (on every platform). SIGUNKNOWN means an unmapped native signal.
var WireSignals = []string{
	"SIGHUP", "SIGINT", "SIGQUIT", "SIGILL", "SIGTRAP", "SIGABRT", "SIGBUS", "SIGFPE", "SIGKILL", "SIGUSR1",
	"SIGSEGV", "SIGUSR2", "SIGPIPE", "SIGALRM", "SIGTERM", "SIGCHLD", "SIGCONT", "SIGSTOP", "SIGTSTP", "SIGTTIN",
	"SIGTTOU", "SIGURG", "SIGXCPU", "SIGXFSZ", "SIGVTALRM", "SIGPROF", "SIGWINCH", "SIGIO", "SIGSYS",
	"SIGEMT", "SIGINFO", "SIGSTKFLT", "SIGPWR", "SIGUNKNOWN",
}

// SignalUnknown is the fallback for an unmapped native signal.
const SignalUnknown = "SIGUNKNOWN"

// ValidWireSignal reports whether s is in WireSignals (aliases such as
// SIGIOT are not accepted).
func ValidWireSignal(s string) bool {
	for _, w := range WireSignals {
		if w == s {
			return true
		}
	}
	return false
}

// EncodeBase64 renders b as standard padded base64.
func EncodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
