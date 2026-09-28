package contract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

// TaskReason explains a rejection or a loss: a fixed safe code and a
// message of at most MaxReasonMessageBytes.
type TaskReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r TaskReason) validate(state, what string) error {
	if !taskReasonCode(state, r.Code) || !utf8.ValidString(r.Message) || len(r.Message) > MaxReasonMessageBytes {
		return errInvalid("%s reason must carry a known code and a message of at most %d bytes", what, MaxReasonMessageBytes)
	}
	return nil
}

// TaskCandidate is one instance considered for a dispatch, observed at
// the decision instant. Inflight is the plane's held reservations and
// ReconcilingInflight how many of them await reconciliation with their
// worker (iteration 06a).
type TaskCandidate struct {
	RoleID              string `json:"role_id"`
	NodeID              string `json:"node_id"`
	RegistrationOrder   int    `json:"registration_order"`
	NodeLiveness        string `json:"node_liveness"`
	Inflight            int    `json:"inflight"`
	ReconcilingInflight int    `json:"reconciling_inflight"`
	Concurrency         int    `json:"concurrency"`
	CanAccept           bool   `json:"can_accept"`
	Reason              string `json:"reason"`
}

// CandidateReasons are the per-candidate reasons, in precedence order.
var CandidateReasons = []string{ReasonStorageUnconfirmed, ReasonRoleRemoving, ReasonNodeOffline, ReasonNodeDetached, ReasonRoleUnsynced, ReasonWorkerUnready, ReasonFull, ReasonAvailable}

func (c TaskCandidate) validate(what string) error {
	known := false
	for _, r := range CandidateReasons {
		known = known || r == c.Reason
	}
	switch {
	case !ValidSlug(c.RoleID) || !ValidNodeID(c.NodeID):
		return errInvalid("%s candidate must name a role and a node", what)
	case c.RegistrationOrder < 1 || c.RegistrationOrder > MaxSafeInteger:
		return errInvalid("%s candidate registration_order is out of range", what)
	case c.NodeLiveness != LivenessOnline && c.NodeLiveness != LivenessOffline:
		return errInvalid("%s candidate node_liveness is invalid", what)
	case c.Inflight < 0 || c.Inflight > MaxSafeInteger || c.ReconcilingInflight < 0 || c.ReconcilingInflight > c.Inflight:
		return errInvalid("%s candidate inflight counts are invalid", what)
	case c.Concurrency < 1 || c.Concurrency > MaxConcurrency:
		return errInvalid("%s candidate concurrency is out of range", what)
	case !known || len(c.Reason) > MaxCandidateReason:
		return errInvalid("%s candidate reason is not known", what)
	case c.CanAccept != (c.Reason == ReasonAvailable):
		return errInvalid("%s candidate can_accept disagrees with its reason", what)
	}
	return nil
}

func validateCandidates(cs []TaskCandidate, what string) error {
	if len(cs) > MaxCandidates {
		return errInvalid("%s has %d candidates; at most %d are allowed", what, len(cs), MaxCandidates)
	}
	for _, c := range cs {
		if err := c.validate(what); err != nil {
			return err
		}
	}
	return nil
}

// ParseCandidates decodes an error detail's candidate list (as the client
// received it) strictly.
func ParseCandidates(v any) ([]TaskCandidate, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, errInvalid("candidates are not JSON")
	}
	var out []TaskCandidate
	if err := decodeStrict(b, &out, "candidates"); err != nil {
		return nil, err
	}
	return out, validateCandidates(out, "error details")
}

// TaskLog is the persisted retained output: Data is the raw retained tail
// (at most MaxLogRetainedBytes), SourceBytes the highest observed source
// end, ReceivedBytes the unique bytes received over the task's lifetime
// (saturated at MaxSafeInteger).
type TaskLog struct {
	Data            []byte `json:"data"`
	SourceBytes     int    `json:"source_bytes"`
	ReceivedBytes   int    `json:"received_bytes"`
	Incomplete      bool   `json:"incomplete"`
	CounterOverflow bool   `json:"counter_overflow"`
}

// MarshalJSON renders data as base64 (an empty tail as "").
func (l TaskLog) MarshalJSON() ([]byte, error) {
	type wire struct {
		Data            string `json:"data"`
		SourceBytes     int    `json:"source_bytes"`
		ReceivedBytes   int    `json:"received_bytes"`
		Incomplete      bool   `json:"incomplete"`
		CounterOverflow bool   `json:"counter_overflow"`
	}
	return compact(wire{EncodeBase64(l.Data), l.SourceBytes, l.ReceivedBytes, l.Incomplete, l.CounterOverflow})
}

// Validate checks 0 <= len(data) <= received <= source <= MaxSafeInteger;
// bytes missing beyond leading retention eviction require incomplete, as
// does a counter overflow.
func (l TaskLog) Validate() error {
	switch {
	case len(l.Data) > MaxLogRetainedBytes:
		return errInvalid("log data exceeds %d bytes", MaxLogRetainedBytes)
	case len(l.Data) > l.ReceivedBytes || l.ReceivedBytes > l.SourceBytes || l.SourceBytes > MaxSafeInteger:
		return errInvalid("log counters must satisfy len(data) <= received_bytes <= source_bytes <= %d", MaxSafeInteger)
	case l.ReceivedBytes < l.SourceBytes && !l.Incomplete:
		return errInvalid("a log with missing bytes must be marked incomplete")
	case l.CounterOverflow && !l.Incomplete:
		return errInvalid("a log whose counter overflowed must be marked incomplete")
	}
	return nil
}

// TaskLogMeta is the public retained-log metadata.
type TaskLogMeta struct {
	RetainedBytes      int  `json:"retained_bytes"`
	SourceBytes        int  `json:"source_bytes"`
	ReceivedBytes      int  `json:"received_bytes"`
	DroppedBytes       int  `json:"dropped_bytes"`
	Truncated          bool `json:"truncated"`
	Incomplete         bool `json:"incomplete"`
	CounterOverflow    bool `json:"counter_overflow"`
	LogMayBeIncomplete bool `json:"log_may_be_incomplete"`
}

// MetaOf derives the public metadata of l.
func MetaOf(l TaskLog, mayBeIncomplete bool) TaskLogMeta {
	return TaskLogMeta{RetainedBytes: len(l.Data), SourceBytes: l.SourceBytes, ReceivedBytes: l.ReceivedBytes,
		DroppedBytes: max(0, l.SourceBytes-len(l.Data)), Truncated: l.ReceivedBytes > len(l.Data) || l.Incomplete,
		Incomplete: l.Incomplete, CounterOverflow: l.CounterOverflow, LogMayBeIncomplete: mayBeIncomplete}
}

func (m TaskLogMeta) validate(what string) error {
	switch {
	case m.RetainedBytes < 0 || m.RetainedBytes > MaxLogRetainedBytes || m.RetainedBytes > m.ReceivedBytes ||
		m.ReceivedBytes > m.SourceBytes || m.SourceBytes > MaxSafeInteger:
		return errInvalid("%s log counters are inconsistent", what)
	case m.DroppedBytes != max(0, m.SourceBytes-m.RetainedBytes):
		return errInvalid("%s log dropped_bytes is inconsistent", what)
	case m.Truncated != (m.ReceivedBytes > m.RetainedBytes || m.Incomplete):
		return errInvalid("%s log truncated is inconsistent", what)
	case m.CounterOverflow && !m.Incomplete:
		return errInvalid("%s log counter_overflow requires incomplete", what)
	}
	return nil
}

// TaskRole is the public resolved role of a task: never manual paths.
type TaskRole struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Node              string `json:"node"`
	Adapter           string `json:"adapter"`
	RegistrationOrder int    `json:"registration_order"`
}

// PublicRole is rec's public shape.
func PublicRole(rec RoleRecord) TaskRole {
	return TaskRole{ID: rec.ID, Name: rec.Name, Node: rec.Node, Adapter: rec.Adapter, RegistrationOrder: rec.RegistrationOrder}
}

func (r TaskRole) validate(what string) error {
	if !ValidSlug(r.ID) || !ValidSlug(r.Name) || !ValidNodeID(r.Node) || r.Adapter == "" || r.RegistrationOrder < 1 || r.RegistrationOrder > MaxSafeInteger {
		return errInvalid("%s role is invalid", what)
	}
	return nil
}

// TaskResult is a terminal task's immutable result. Workspace fields are
// explicitly unavailable in this build: result_commit and diffstat are
// null and changed_paths is empty, never fabricated.
type TaskResult struct {
	State                 string           `json:"state"`
	ExitCode              *int             `json:"exit_code"`
	Signal                *string          `json:"signal"`
	FinalMessage          *string          `json:"final_message"`
	FinalMessageTruncated bool             `json:"final_message_truncated"`
	LogTail               string           `json:"log_tail"`
	ResultCommit          *json.RawMessage `json:"result_commit"`
	Diffstat              *json.RawMessage `json:"diffstat"`
	ChangedPaths          []string         `json:"changed_paths"`
}

// MarshalJSON renders the result with null workspace fields and [].
func (r TaskResult) MarshalJSON() ([]byte, error) {
	type plain TaskResult
	p := plain(r)
	p.ResultCommit, p.Diffstat, p.ChangedPaths = nil, nil, []string{}
	return compact(p)
}

// TaskView is the single-task operator view (show, dispatch).
type TaskView struct {
	TaskID              string           `json:"task_id"`
	Request             DispatchRequest  `json:"request"`
	Role                TaskRole         `json:"role"`
	Effective           TaskEffective    `json:"effective"`
	TimeoutPolicy       string           `json:"timeout_policy"`
	State               string           `json:"state"`
	StopRequested       bool             `json:"stop_requested"`
	CreatedAt           string           `json:"created_at"`
	StartedAt           *string          `json:"started_at"`
	FinishedAt          *string          `json:"finished_at"`
	ElapsedMS           int              `json:"elapsed_ms"`
	Reconciling         bool             `json:"reconciling"`
	CompletionPending   bool             `json:"completion_pending"`
	PersistenceReason   *string          `json:"persistence_reason"`
	DurabilityConfirmed bool             `json:"durability_confirmed"`
	Reason              *TaskReason      `json:"reason"`
	Candidates          []TaskCandidate  `json:"candidates"`
	Log                 TaskLogMeta      `json:"log"`
	LogTail             string           `json:"log_tail"`
	TailTruncated       bool             `json:"tail_truncated"`
	Result              *TaskResult      `json:"result"`
	LateResult          *TaskLateSummary `json:"late_result"`
}

// MarshalJSON renders the view with an empty (never null) candidate list.
func (v TaskView) MarshalJSON() ([]byte, error) {
	type plain TaskView
	p := plain(v)
	if p.Candidates == nil {
		p.Candidates = []TaskCandidate{}
	}
	return compact(p)
}

// TaskSummary is one task list row: no goal, payload, acceptance, final
// message or log bodies.
type TaskSummary struct {
	TaskID              string        `json:"task_id"`
	Target              TaskTarget    `json:"target"`
	Role                TaskRole      `json:"role"`
	State               string        `json:"state"`
	StopRequested       bool          `json:"stop_requested"`
	CreatedAt           string        `json:"created_at"`
	StartedAt           *string       `json:"started_at"`
	FinishedAt          *string       `json:"finished_at"`
	ElapsedMS           int           `json:"elapsed_ms"`
	Effective           TaskEffective `json:"effective"`
	RequestedBy         RequestedBy   `json:"requested_by"`
	Reconciling         bool          `json:"reconciling"`
	CompletionPending   bool          `json:"completion_pending"`
	PersistenceReason   *string       `json:"persistence_reason"`
	DurabilityConfirmed bool          `json:"durability_confirmed"`
	Reason              *TaskReason   `json:"reason"`
}

// FormatTime renders a task timestamp: UTC RFC3339Nano.
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func validTimes(created string, started, finished *string, what string) error {
	for _, s := range []*string{&created, started, finished} {
		if s != nil {
			if _, ok := ParseTime(*s); !ok {
				return errInvalid("%s timestamps must be UTC RFC3339 timestamps", what)
			}
		}
	}
	return nil
}

// validLifecycle checks the state's timestamp and outcome invariants.
func validLifecycle(state string, started, finished bool, exit *int, signal, final *string, truncated bool, reason bool, candidates int, what string) error {
	bad := func(msg string) error { return errInvalid("%s: a %s task %s", what, state, msg) }
	if !TaskTerminal(state) && (finished || exit != nil || signal != nil || final != nil || truncated) {
		return bad("has no finish time, exit, signal or final message")
	}
	if state != TaskRejected && candidates > 0 {
		return bad("has no candidates")
	}
	if state != TaskRejected && state != TaskLost && state != TaskCancelled && reason {
		return bad("has no reason")
	}
	switch state {
	case TaskPending:
		if started {
			return bad("has not started")
		}
	case TaskRunning:
		if !started {
			return bad("has a start time")
		}
	case TaskSucceeded:
		if !started || !finished || exit == nil || *exit != 0 || signal != nil {
			return bad("has both timestamps and exit code 0")
		}
	case TaskFailed:
		if !started || !finished || (exit == nil) == (signal == nil) || (exit != nil && *exit == 0) {
			return bad("has both timestamps and a nonzero exit code or a signal")
		}
	case TaskRejected:
		if started || !finished || exit != nil || signal != nil || final != nil || truncated || !reason {
			return bad("has a finish time and a reason but no start, exit, signal or final message")
		}
	case TaskLost:
		// Lost: outcome and cleanup unconfirmed. It may lack a start (a
		// pending task), and an exit or signal when no trustworthy
		// observation exists, but never both.
		if !finished || !reason || (exit != nil && signal != nil) {
			return bad("has a finish time and a reason, and at most one of exit code and signal")
		}
	case TaskCancelled:
		// Cancelled (iteration 06b): cleanup confirmed. Its exit or signal
		// (at most one) is the adapter's own during cleanup; without a start
		// it was cancelled before any adapter ran, and says so.
		if !finished || (exit != nil && signal != nil) || started == reason {
			return bad("has a finish time, at most one of exit code and signal, and a start or the reason cancelled_before_start")
		}
	case TaskTimedOut:
		if !started || !finished || (exit != nil && signal != nil) || reason {
			return bad("has both timestamps, at most one of exit code and signal and no reason")
		}
	default:
		return errInvalid("%s state %q is not a state this build produces", what, SafeText(state, 32))
	}
	if exit != nil && (*exit < 0 || *exit > 255) {
		return bad("has an exit code from 0 to 255")
	}
	if signal != nil && !ValidWireSignal(*signal) {
		return bad("has a known signal name")
	}
	return checkFinalMessage(final, truncated, what)
}

func (v TaskView) validate() error {
	const what = "task view"
	if !ValidTaskID(v.TaskID) {
		return errInvalid("%s task_id is not a task ID", what)
	}
	if err := v.Request.Validate(); err != nil {
		return err
	}
	if err := v.Role.validate(what); err != nil {
		return err
	}
	if !ValidTimeoutPolicy(v.TimeoutPolicy) {
		return errInvalid("%s timeout_policy must be %q or %q", what, TimeoutPolicyEnforced, TimeoutPolicyLegacy)
	}
	if v.StopRequested && !stopIntentState(v.State) {
		return errInvalid("%s: a %s task has no stop request", what, v.State)
	}
	if err := validTimes(v.CreatedAt, v.StartedAt, v.FinishedAt, what); err != nil {
		return err
	}
	var exit *int
	var signal, final *string
	var truncated bool
	if v.Result != nil {
		exit, signal, final, truncated = v.Result.ExitCode, v.Result.Signal, v.Result.FinalMessage, v.Result.FinalMessageTruncated
	}
	if err := validLifecycle(v.State, v.StartedAt != nil, v.FinishedAt != nil, exit, signal, final, truncated, v.Reason != nil, len(v.Candidates), what); err != nil {
		return err
	}
	if (v.Result != nil) != TaskTerminal(v.State) {
		return errInvalid("%s result must be present exactly for terminal tasks", what)
	}
	if r := v.Result; r != nil && (r.State != v.State || r.ResultCommit != nil || r.Diffstat != nil || len(r.ChangedPaths) != 0 || r.LogTail != v.LogTail) {
		return errInvalid("%s result is inconsistent with the task", what)
	}
	if v.Reason != nil {
		if err := v.Reason.validate(v.State, what); err != nil {
			return err
		}
	}
	if v.LateResult != nil {
		if !TaskTerminal(v.State) {
			return errInvalid("%s: a %s task has no late result", what, v.State)
		}
		if err := v.LateResult.validate(what); err != nil {
			return err
		}
	}
	if v.Reconciling && TaskTerminal(v.State) {
		return errInvalid("%s: a terminal task is never reconciling", what)
	}
	return v.checkCommon(what)
}

func (v TaskView) checkCommon(what string) error {
	switch {
	case v.ElapsedMS < 0:
		return errInvalid("%s elapsed_ms must not be negative", what)
	case v.PersistenceReason != nil && *v.PersistenceReason != ReasonResultStorageUnconfirmed:
		return errInvalid("%s persistence_reason is not known", what)
	case !utf8.ValidString(v.LogTail) || len(v.LogTail) > MaxTailBytes*3:
		return errInvalid("%s log_tail is invalid", what)
	}
	if err := validateCandidates(v.Candidates, what); err != nil {
		return err
	}
	return v.Log.validate(what)
}

func (s TaskSummary) validate() error {
	const what = "task summary"
	if !ValidTaskID(s.TaskID) || !ValidTaskState(s.State) {
		return errInvalid("%s must name a task and a known state", what)
	}
	if err := s.Role.validate(what); err != nil {
		return err
	}
	if err := s.RequestedBy.Validate(); err != nil {
		return err
	}
	if err := validTimes(s.CreatedAt, s.StartedAt, s.FinishedAt, what); err != nil {
		return err
	}
	if s.StopRequested && !stopIntentState(s.State) {
		return errInvalid("%s: a %s task has no stop request", what, s.State)
	}
	if s.ElapsedMS < 0 || (s.Reconciling && TaskTerminal(s.State)) || (s.PersistenceReason != nil && *s.PersistenceReason != ReasonResultStorageUnconfirmed) {
		return errInvalid("%s derived fields are inconsistent", what)
	}
	if s.Reason != nil {
		return s.Reason.validate(s.State, what)
	}
	return nil
}

// DispatchResponse is 202 {"version":5,"task_id":ID,"task":TaskView,
// "wait_result":null} for an asynchronous dispatch, or (iteration 06b) with
// the task omitted and wait_result the wait union for a dispatch with a
// wait: the admitted view is never repeated beside it.
type DispatchResponse struct {
	Version    int           `json:"version"`
	TaskID     string        `json:"task_id"`
	Task       *TaskView     `json:"task,omitempty"`
	WaitResult *WaitResponse `json:"wait_result"`
}

// TaskListResponse is one page of tasks ordered by task ID.
type TaskListResponse struct {
	Version   int           `json:"version"`
	Tasks     []TaskSummary `json:"tasks"`
	NextAfter *string       `json:"next_after"`
}

// MarshalJSON renders an empty page as [].
func (r TaskListResponse) MarshalJSON() ([]byte, error) {
	type plain TaskListResponse
	p := plain(r)
	if p.Tasks == nil {
		p.Tasks = []TaskSummary{}
	}
	return compact(p)
}

// TaskShowResponse is {"version":4,"task":TaskView}.
type TaskShowResponse struct {
	Version int      `json:"version"`
	Task    TaskView `json:"task"`
}

// TaskLogsResponse is the complete retained output snapshot, byte exact.
type TaskLogsResponse struct {
	Version            int    `json:"version"`
	TaskID             string `json:"task_id"`
	Data               []byte `json:"data"`
	RetainedBytes      int    `json:"retained_bytes"`
	SourceBytes        int    `json:"source_bytes"`
	DroppedBytes       int    `json:"dropped_bytes"`
	Truncated          bool   `json:"truncated"`
	Incomplete         bool   `json:"incomplete"`
	CounterOverflow    bool   `json:"counter_overflow"`
	LogMayBeIncomplete bool   `json:"log_may_be_incomplete"`
}

// MarshalJSON renders data as base64 (empty as "").
func (r TaskLogsResponse) MarshalJSON() ([]byte, error) {
	type wire struct {
		Version            int    `json:"version"`
		TaskID             string `json:"task_id"`
		Data               string `json:"data"`
		RetainedBytes      int    `json:"retained_bytes"`
		SourceBytes        int    `json:"source_bytes"`
		DroppedBytes       int    `json:"dropped_bytes"`
		Truncated          bool   `json:"truncated"`
		Incomplete         bool   `json:"incomplete"`
		CounterOverflow    bool   `json:"counter_overflow"`
		LogMayBeIncomplete bool   `json:"log_may_be_incomplete"`
	}
	meta, err := compact(wire{r.Version, r.TaskID, "", r.RetainedBytes, r.SourceBytes, r.DroppedBytes, r.Truncated, r.Incomplete, r.CounterOverflow, r.LogMayBeIncomplete})
	if err != nil {
		return nil, err
	}
	// Splice the base64 data into one exact-size buffer: a full 10 MiB
	// tail costs one output allocation, not the encoder's copies. The
	// only "data" member is the empty one just rendered (task IDs hold no
	// quotes).
	i := bytes.Index(meta, logsDataKey)
	if i < 0 {
		return nil, errInvalid("task logs response is not encodable")
	}
	at := i + len(logsDataKey) - 1
	out := make([]byte, len(meta)+base64.StdEncoding.EncodedLen(len(r.Data)))
	n := copy(out, meta[:at])
	base64.StdEncoding.Encode(out[n:], r.Data)
	copy(out[n+base64.StdEncoding.EncodedLen(len(r.Data)):], meta[at:])
	return out, nil
}

var logsDataKey = []byte(`"data":""`)

// encodeDirect is Encode's fast path: MarshalJSON's output is already
// compact and unescaped, so the generic encoder's validating copy is
// skipped.
func (r TaskLogsResponse) encodeDirect() ([]byte, error) { return r.MarshalJSON() }

// versioned checks a response envelope's version before anything else.
// A matching envelope is then strictly decoded into v from the same
// object, so the document is scanned once at the top level.
func versioned(data []byte, v any, what string) error {
	o, err := decodeObject(data, what)
	if err != nil {
		return err
	}
	if _, ok := o.raw["version"]; !ok {
		return errInvalid("%s lacks the required field %q", what, "version")
	}
	if err := envelopeVersion(o, what); err != nil {
		return err
	}
	return decodeFields(o, reflect.ValueOf(v).Elem(), what)
}

// ParseDispatchResponse strictly decodes an asynchronous dispatch
// response: its task view and a null wait_result.
func ParseDispatchResponse(data []byte) (TaskView, error) {
	const what = "dispatch response"
	o, err := decodeObject(data, what)
	if err != nil {
		return TaskView{}, err
	}
	if err := o.only(what, []string{"version", "task_id", "task"}, "wait_result"); err != nil {
		return TaskView{}, err
	}
	if err := envelopeVersion(o, what); err != nil {
		return TaskView{}, err
	}
	if raw, ok := o.raw["wait_result"]; !ok || !isNull(raw) {
		return TaskView{}, errInvalid("%s of an asynchronous dispatch has a null wait_result", what)
	}
	var r struct {
		TaskID string   `json:"task_id"`
		Task   TaskView `json:"task"`
	}
	if err := decodeStrict([]byte(`{"task_id":`+string(o.raw["task_id"])+`,"task":`+string(o.raw["task"])+`}`), &r, what); err != nil {
		return TaskView{}, err
	}
	if r.TaskID != r.Task.TaskID {
		return TaskView{}, errInvalid("%s task_id differs from its task", what)
	}
	return r.Task, r.Task.validate()
}

// ParseDispatchWaitResponse strictly decodes a dispatch-with-wait
// response (iteration 06b): its task_id, no task view, and a wait union
// that answers exactly that task within the requested wait.
func ParseDispatchWaitResponse(data []byte, requested time.Duration) (string, WaitResponse, error) {
	const what = "dispatch response"
	o, err := decodeObject(data, what)
	if err != nil {
		return "", WaitResponse{}, err
	}
	if err := o.only(what, []string{"version", "task_id", "wait_result"}); err != nil {
		return "", WaitResponse{}, err
	}
	if err := envelopeVersion(o, what); err != nil {
		return "", WaitResponse{}, err
	}
	id, err := o.str(what, "task_id")
	if err != nil || !ValidTaskID(id) {
		return "", WaitResponse{}, errInvalid("%s task_id is not a task ID", what)
	}
	wr, err := ParseWaitResponse(o.raw["wait_result"], []string{id}, requested)
	if err != nil {
		return "", WaitResponse{}, err
	}
	if len(data)+1 > MaxWaitResponseBytes || (wr.Status == WaitStillRunning && len(data)+1 > MaxStillRunningOneBytes) {
		return "", WaitResponse{}, errInvalid("%s exceeds its bound", what)
	}
	return id, wr, nil
}

// ParseTaskShowResponse strictly decodes a task show response.
func ParseTaskShowResponse(data []byte) (TaskView, error) {
	const what = "task response"
	var r TaskShowResponse
	if err := versioned(data, &r, what); err != nil {
		return TaskView{}, err
	}
	return r.Task, r.Task.validate()
}

// ParseTaskListResponse strictly decodes one list page: at most
// MaxTaskListLimit summaries in strictly ascending task ID order, with
// next_after equal to the last ID when present.
func ParseTaskListResponse(data []byte) ([]TaskSummary, *string, error) {
	const what = "task list response"
	var r TaskListResponse
	if err := versioned(data, &r, what); err != nil {
		return nil, nil, err
	}
	if len(r.Tasks) > MaxTaskListLimit {
		return nil, nil, errInvalid("%s has %d tasks; at most %d are allowed", what, len(r.Tasks), MaxTaskListLimit)
	}
	for i, s := range r.Tasks {
		if err := s.validate(); err != nil {
			return nil, nil, err
		}
		if i > 0 && r.Tasks[i-1].TaskID >= s.TaskID {
			return nil, nil, errInvalid("%s is not sorted by unique task ID", what)
		}
	}
	if r.NextAfter != nil && (len(r.Tasks) == 0 || *r.NextAfter != r.Tasks[len(r.Tasks)-1].TaskID) {
		return nil, nil, errInvalid("%s next_after must be the last task ID of the page", what)
	}
	return r.Tasks, r.NextAfter, nil
}

// ParseTaskLogsResponse strictly decodes a task logs response.
func ParseTaskLogsResponse(data []byte) (TaskLogsResponse, error) {
	const what = "task logs response"
	var r TaskLogsResponse
	if err := versioned(data, &r, what); err != nil {
		return TaskLogsResponse{}, err
	}
	switch {
	case !ValidTaskID(r.TaskID):
		return r, errInvalid("%s task_id is not a task ID", what)
	case r.RetainedBytes != len(r.Data) || r.RetainedBytes > MaxLogRetainedBytes || r.SourceBytes < r.RetainedBytes || r.SourceBytes > MaxSafeInteger:
		return r, errInvalid("%s retained_bytes must equal the data length", what)
	case r.DroppedBytes != max(0, r.SourceBytes-r.RetainedBytes) || (r.CounterOverflow && !r.Incomplete) || (r.Incomplete && !r.Truncated):
		return r, errInvalid("%s metadata is inconsistent", what)
	}
	return r, nil
}

// LogTail selects the display tail of retained output: the final
// MaxTailBytes, then the last lines lines (a trailing LF terminates its
// line without adding a blank one; an unterminated last line counts),
// with invalid UTF-8 replaced. truncated reports that retained bytes were
// omitted by the byte or line selection.
func LogTail(data []byte, lines int) (string, bool) {
	window := data
	cut := false
	if len(window) > MaxTailBytes {
		window, cut = window[len(window)-MaxTailBytes:], true
	}
	if lines <= 0 || len(window) == 0 {
		return "", len(data) > 0
	}
	// Walk back over line terminators: the last byte's LF ends the last
	// line; each earlier LF starts a new line.
	end := len(window)
	start := end
	if window[end-1] == '\n' {
		start = end - 1
	}
	n := 0
	for {
		i := bytes.LastIndexByte(window[:start], '\n')
		n++
		if i < 0 {
			start = 0
			break
		}
		if n == lines {
			start = i + 1
			break
		}
		start = i
	}
	tail := window[start:end]
	return strings.ToValidUTF8(string(tail), "�"), cut || start > 0
}
