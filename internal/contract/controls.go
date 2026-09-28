package contract

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Iteration 06b task controls (protocol 5): the plane's stop intent, the
// task_cancel control frame and its receipt, the reconcile stop_control
// disposition, and the cancel and bounded wait APIs with their strict
// codecs and byte bounds.

// Control frames and bounds.
const (
	FrameTaskCancel    = "task_cancel"
	FrameTaskCancelAck = "task_cancel_ack"
	// MaxControlBody bounds a task_cancel and its acknowledgement.
	MaxControlBody = 1 << 10

	// ActionStopControl latches a durable plane stop intent on a held
	// preparing or running execution (reconcile, protocol 5).
	ActionStopControl = "stop_control"

	// StopKindCancelled is the only plane stop intent kind; timed_out is
	// the guardian's own cause and never travels in a plane control.
	StopKindCancelled = "cancelled"
)

// StopIntent is a task's single frozen plane stop intent: its random ID,
// kind and request instant.
type StopIntent struct {
	ID          string
	Kind        string
	RequestedAt time.Time
}

type stopIntentWire struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	RequestedAt string `json:"requested_at"`
}

// MarshalJSON renders {"id","kind","requested_at"}.
func (s StopIntent) MarshalJSON() ([]byte, error) {
	return compact(stopIntentWire{ID: s.ID, Kind: s.Kind, RequestedAt: FormatTime(s.RequestedAt)})
}

func (s *StopIntent) unmarshalStrict(raw json.RawMessage, what string) error {
	var w stopIntentWire
	if err := decodeStrict(raw, &w, what); err != nil {
		return err
	}
	at, ok := ParseTime(w.RequestedAt)
	if !ok {
		return errInvalid("%s requested_at must be a UTC RFC3339 timestamp", what)
	}
	v := StopIntent{ID: w.ID, Kind: w.Kind, RequestedAt: at}
	if err := v.Validate(); err != nil {
		return err
	}
	*s = v
	return nil
}

// Validate checks the intent's ID and kind.
func (s StopIntent) Validate() error {
	if !ValidStopID(s.ID) || s.Kind != StopKindCancelled {
		return errInvalid("stop intent must carry a 32-hex id and kind %q", StopKindCancelled)
	}
	return nil
}

// ---- Control frames ----

// TaskCancelBody is the plane's task_cancel request.
type TaskCancelBody struct {
	TaskID    string         `json:"task_id"`
	Execution ExecutionToken `json:"execution"`
	StopID    string         `json:"stop_id"`
	Kind      string         `json:"kind"`
}

// DecodeTaskCancel strictly decodes a task_cancel body: kind must be
// cancelled (timed_out is the guardian's cause and is rejected here).
func DecodeTaskCancel(body json.RawMessage) (TaskCancelBody, error) {
	const what = "task_cancel body"
	var b TaskCancelBody
	if len(body) > MaxControlBody {
		return b, errInvalid("%s exceeds %d bytes", what, MaxControlBody)
	}
	if err := decodeStrict(body, &b, what); err != nil {
		return b, err
	}
	switch {
	case !ValidTaskID(b.TaskID):
		return b, errInvalid("%s task_id is not a task ID", what)
	case !ValidStopID(b.StopID):
		return b, errInvalid("%s stop_id must be 32 lowercase hex digits", what)
	case b.Kind == OutcomeTimedOut:
		return b, errInvalid("%s kind timed_out is reserved for the guardian's own deadline", what)
	case b.Kind != StopKindCancelled:
		return b, errInvalid("%s kind must be %q", what, StopKindCancelled)
	}
	return b, nil
}

// TaskCancelAckBody acknowledges receipt of a task_cancel (never cleanup).
type TaskCancelAckBody struct {
	TaskID    string         `json:"task_id"`
	Execution ExecutionToken `json:"execution"`
	StopID    string         `json:"stop_id"`
	Received  bool           `json:"received"`
}

// DecodeTaskCancelAck strictly decodes a task_cancel_ack body.
func DecodeTaskCancelAck(body json.RawMessage) (TaskCancelAckBody, error) {
	const what = "task_cancel_ack body"
	var b TaskCancelAckBody
	if len(body) > MaxControlBody {
		return b, errInvalid("%s exceeds %d bytes", what, MaxControlBody)
	}
	if err := decodeStrict(body, &b, what); err != nil {
		return b, err
	}
	if !ValidTaskID(b.TaskID) || !ValidStopID(b.StopID) || !b.Received {
		return b, errInvalid("%s must name a task and a stop_id with received=true", what)
	}
	return b, nil
}

// ---- Cancel API ----

// CancelResponse is POST /api/v1/tasks/{id}/cancel's result: 202 with
// accepted=true once a (new or existing) stop intent is durable, 200 with
// accepted=false for an already durably terminal task.
type CancelResponse struct {
	Version  int      `json:"version"`
	TaskID   string   `json:"task_id"`
	Accepted bool     `json:"accepted"`
	Task     TaskView `json:"task"`
}

// ParseCancelRequest requires exactly the empty JSON object.
func ParseCancelRequest(data []byte) error {
	o, err := decodeObject(data, "cancel request")
	if err != nil {
		return err
	}
	return o.only("cancel request", nil)
}

// ParseCancelResponse strictly decodes a cancel response for task id
// received with HTTP status.
func ParseCancelResponse(data []byte, status int, id string) (CancelResponse, error) {
	const what = "cancel response"
	var r CancelResponse
	if err := versioned(data, &r, what); err != nil {
		return r, err
	}
	if err := r.Task.validate(); err != nil {
		return r, err
	}
	switch {
	case r.TaskID != id || r.Task.TaskID != id:
		return r, errInvalid("%s names another task", what)
	case r.Accepted && status != 202, !r.Accepted && status != 200:
		return r, errInvalid("%s accepted=%t does not match HTTP %d", what, r.Accepted, status)
	case r.Accepted && !r.Task.StopRequested && !TaskTerminal(r.Task.State):
		return r, errInvalid("%s: an accepted cancel's task shows no stop request", what)
	case !r.Accepted && (!TaskTerminal(r.Task.State) || !r.Task.DurabilityConfirmed):
		return r, errInvalid("%s: an unaccepted cancel names a task that is not durably terminal", what)
	}
	return r, nil
}

// ---- Bounded wait ----

// Wait bounds (iteration 06b, FP-3). The CLI default and the plane's
// runtime cap are operator choices, never an MCP-safe claim.
const (
	PathTaskWait = PathTasks + "/wait"

	MaxWait            = 5 * time.Minute
	DefaultTaskWait    = 5 * time.Second
	DefaultMaxTaskWait = 30 * time.Second
	MinMaxTaskWait     = 100 * time.Millisecond

	MaxWaitIDs          = 16
	MaxWaitRequestBytes = 2 << 10
	// A still_running response, final newline included, is at most
	// MaxStillRunningBytes (MaxStillRunningOneBytes for one id).
	MaxStillRunningBytes    = 16384
	MaxStillRunningOneBytes = 2048
	MaxLastLogLineBytes     = 96
	// MaxWaitResponseBytes bounds a terminal wait or a dispatch-with-wait
	// response: a task view plus its envelope.
	MaxWaitResponseBytes = MaxTaskViewBytes + 1<<10

	WaitTerminal     = "terminal"
	WaitStillRunning = "still_running"
)

// ParseWaitDuration parses a wait: a strict Go duration from 0 (an
// immediate snapshot) to MaxWait inclusive; negative, blank, malformed or
// larger values are invalid (never clamped).
func ParseWaitDuration(s string) (time.Duration, error) {
	bad := func(msg string) (time.Duration, error) {
		return 0, taskErr(CodeInvalidArgument, "wait", "", "%s", msg)
	}
	if strings.TrimSpace(s) == "" || strings.TrimSpace(s) != s {
		return bad("wait must be a Go duration such as 5s or 2m, from 0 to 5m")
	}
	d, err := time.ParseDuration(s)
	switch {
	case err != nil:
		return bad("wait must be a Go duration such as 5s or 2m, from 0 to 5m")
	case d < 0:
		return bad("wait must not be negative")
	case d > MaxWait:
		return bad("wait must be at most 5m")
	}
	return d, nil
}

// ValidateWaitIDs checks a wait's task IDs: 1 to MaxWaitIDs canonical,
// unique IDs.
func ValidateWaitIDs(ids []string) error {
	if len(ids) == 0 || len(ids) > MaxWaitIDs {
		return taskErr(CodeInvalidArgument, "task_ids", "", "a wait names 1 to %d task IDs", MaxWaitIDs)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !ValidTaskID(id) {
			return taskErr(CodeInvalidArgument, "task_ids", "", "invalid task ID; want t_ followed by 32 lowercase hex digits")
		}
		if seen[id] {
			return taskErr(CodeInvalidArgument, "task_ids", "", "task %s is named more than once", id)
		}
		seen[id] = true
	}
	return nil
}

// TaskWaitRequest is POST /api/v1/tasks/wait: {"task_ids":[...],"wait":D}.
type TaskWaitRequest struct {
	TaskIDs []string
	Wait    time.Duration
}

// MarshalJSON renders the canonical request.
func (r TaskWaitRequest) MarshalJSON() ([]byte, error) {
	ids := r.TaskIDs
	if ids == nil {
		ids = []string{}
	}
	return compact(struct {
		TaskIDs []string `json:"task_ids"`
		Wait    string   `json:"wait"`
	}{ids, r.Wait.String()})
}

// ParseTaskWaitRequest strictly decodes a wait request of at most
// MaxWaitRequestBytes.
func ParseTaskWaitRequest(data []byte) (TaskWaitRequest, error) {
	const what = "wait request"
	var r TaskWaitRequest
	if len(data) > MaxWaitRequestBytes {
		return r, taskErr(CodeInvalidArgument, "", "", "the wait request is larger than %d bytes", MaxWaitRequestBytes)
	}
	var w struct {
		TaskIDs []string `json:"task_ids"`
		Wait    string   `json:"wait"`
	}
	if err := decodeStrict(data, &w, what); err != nil {
		return r, err
	}
	if err := ValidateWaitIDs(w.TaskIDs); err != nil {
		return r, err
	}
	d, err := ParseWaitDuration(w.Wait)
	if err != nil {
		return r, err
	}
	return TaskWaitRequest{TaskIDs: w.TaskIDs, Wait: d}, nil
}

// WaitRow is one still_running row.
type WaitRow struct {
	TaskID              string `json:"task_id"`
	State               string `json:"state"`
	ElapsedMS           int    `json:"elapsed_ms"`
	LastLogLine         string `json:"last_log_line"`
	LogTruncated        bool   `json:"log_truncated"`
	DurabilityConfirmed bool   `json:"durability_confirmed"`
}

// WaitResponse is the wait union: terminal (the winner and its bounded
// view) or still_running (one compact row per requested ID, caller order).
type WaitResponse struct {
	Version         int
	Status          string
	EffectiveWaitMS int
	Winner          string
	Task            *TaskView
	Tasks           []WaitRow
}

type waitTerminalWire struct {
	Version         int      `json:"version"`
	Status          string   `json:"status"`
	EffectiveWaitMS int      `json:"effective_wait_ms"`
	Winner          string   `json:"winner"`
	Task            TaskView `json:"task"`
}

type waitRunningWire struct {
	Version         int       `json:"version"`
	Status          string    `json:"status"`
	EffectiveWaitMS int       `json:"effective_wait_ms"`
	Tasks           []WaitRow `json:"tasks"`
}

// MarshalJSON renders the variant selected by Status.
func (r WaitResponse) MarshalJSON() ([]byte, error) {
	if r.Status == WaitTerminal {
		if r.Task == nil {
			return nil, errInvalid("a terminal wait response needs its task")
		}
		return compact(waitTerminalWire{r.Version, r.Status, r.EffectiveWaitMS, r.Winner, *r.Task})
	}
	rows := r.Tasks
	if rows == nil {
		rows = []WaitRow{}
	}
	return compact(waitRunningWire{r.Version, r.Status, r.EffectiveWaitMS, rows})
}

// StillRunningLimit is the byte bound of a still_running answer for n ids
// (final newline included).
func StillRunningLimit(n int) int {
	if n == 1 {
		return MaxStillRunningOneBytes
	}
	return MaxStillRunningBytes
}

// EncodeWaitResponse renders r and asserts its bound (a still_running
// answer's compact bound, a terminal one's view bound) before anything is
// written; exceeding it is an internal error, never a partial answer.
func EncodeWaitResponse(r WaitResponse) ([]byte, error) {
	b, err := compact(r)
	if err != nil {
		return nil, err
	}
	limit := MaxWaitResponseBytes
	if r.Status == WaitStillRunning {
		limit = StillRunningLimit(len(r.Tasks))
	}
	if len(b)+1 > limit {
		return nil, New(CodeInternal, "the wait response of "+strconv.Itoa(len(b)+1)+" bytes exceeds its "+strconv.Itoa(limit)+"-byte bound")
	}
	return b, nil
}

// waitStatus reads a wait object's status.
func waitStatus(o object, what string) (string, error) {
	if raw, ok := o.raw["status"]; !ok || isNull(raw) {
		return "", errInvalid("%s lacks the required field %q", what, "status")
	}
	s, err := o.str(what, "status")
	if err != nil || (s != WaitTerminal && s != WaitStillRunning) {
		return "", errInvalid("%s status must be %q or %q", what, WaitTerminal, WaitStillRunning)
	}
	return s, nil
}

// ParseWaitResponse strictly decodes a wait union answering ids in
// caller order with the requested wait: the winner is one of ids and its
// task is durably terminal; still_running rows match ids exactly, in
// order; effective_wait_ms is at most the request rounded up to ms; the
// still_running byte bound holds.
func ParseWaitResponse(data []byte, ids []string, requested time.Duration) (WaitResponse, error) {
	const what = "wait response"
	o, err := decodeObject(data, what)
	if err != nil {
		return WaitResponse{}, err
	}
	if _, ok := o.raw["version"]; !ok {
		return WaitResponse{}, errInvalid("%s lacks the required field %q", what, "version")
	}
	if err := envelopeVersion(o, what); err != nil {
		return WaitResponse{}, err
	}
	status, err := waitStatus(o, what)
	if err != nil {
		return WaitResponse{}, err
	}
	var r WaitResponse
	maxMS := int((requested + time.Millisecond - 1) / time.Millisecond)
	if status == WaitTerminal {
		var w waitTerminalWire
		if err := decodeFields(o, reflectValue(&w), what); err != nil {
			return r, err
		}
		if err := w.Task.validate(); err != nil {
			return r, err
		}
		r = WaitResponse{Version: w.Version, Status: w.Status, EffectiveWaitMS: w.EffectiveWaitMS, Winner: w.Winner, Task: &w.Task}
		found := false
		for _, id := range ids {
			found = found || id == w.Winner
		}
		switch {
		case !found || w.Task.TaskID != w.Winner:
			return r, errInvalid("%s winner is not one of the requested tasks", what)
		case !TaskTerminal(w.Task.State) || !w.Task.DurabilityConfirmed:
			return r, errInvalid("%s winner is not durably terminal", what)
		}
	} else {
		if len(data)+1 > StillRunningLimit(len(ids)) {
			return r, errInvalid("%s exceeds its compact bound", what)
		}
		var w waitRunningWire
		if err := decodeFields(o, reflectValue(&w), what); err != nil {
			return r, err
		}
		r = WaitResponse{Version: w.Version, Status: w.Status, EffectiveWaitMS: w.EffectiveWaitMS, Tasks: w.Tasks}
		if len(w.Tasks) != len(ids) {
			return r, errInvalid("%s has %d rows for %d requested tasks", what, len(w.Tasks), len(ids))
		}
		for i, row := range w.Tasks {
			switch {
			case row.TaskID != ids[i]:
				return r, errInvalid("%s rows are not the requested tasks in order", what)
			case !ValidTaskState(row.State) || row.ElapsedMS < 0 || row.ElapsedMS > MaxSafeInteger:
				return r, errInvalid("%s row %s is invalid", what, row.TaskID)
			case TaskTerminal(row.State) && row.DurabilityConfirmed:
				return r, errInvalid("%s row %s is durably terminal but did not win", what, row.TaskID)
			case len(row.LastLogLine) > MaxLastLogLineBytes || !utf8.ValidString(row.LastLogLine):
				return r, errInvalid("%s row %s last_log_line exceeds %d bytes", what, row.TaskID, MaxLastLogLineBytes)
			}
		}
	}
	if r.EffectiveWaitMS < 0 || r.EffectiveWaitMS > maxMS {
		return r, errInvalid("%s effective_wait_ms %d exceeds the requested %d", what, r.EffectiveWaitMS, maxMS)
	}
	return r, nil
}

// LastLogLine derives a still_running row's log fragment from the tail of
// retained output: the final line (a trailing LF ends it) or unterminated
// fragment, invalid UTF-8 replaced, then its last at most
// MaxLastLogLineBytes bytes on rune boundaries. cut reports that bytes of
// the line were omitted.
func LastLogLine(tail []byte) (line string, cut bool) {
	end := len(tail)
	if end > 0 && tail[end-1] == '\n' {
		end--
	}
	start := bytes.LastIndexByte(tail[:end], '\n') + 1
	s := strings.ToValidUTF8(string(tail[start:end]), "�")
	if len(s) <= MaxLastLogLineBytes {
		return s, false
	}
	i := len(s) - MaxLastLogLineBytes
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:], true
}

// WaitRowHeader is the text still_running header.
var WaitRowHeader = []string{"TASK_ID", "STATE", "ELAPSED_MS", "LAST_LOG_LINE", "LOG_TRUNCATED", "DURABILITY_CONFIRMED"}

// EscapeTerminal renders s for one tab-separated terminal field: every
// control byte (TAB, LF, CR and ESC included), DEL and C1 control is a
// printable escape; the backslash itself is doubled.
func EscapeTerminal(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x`)
			b.WriteString(strconv.FormatInt(int64(r)+0x100, 16)[1:])
		case r >= 0x80 && r <= 0x9f:
			b.WriteString(`\u00`)
			b.WriteString(strconv.FormatInt(int64(r), 16))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RenderWaitRows renders the still_running text: the header, then one
// tab-separated row per task in caller order, newline terminated.
func RenderWaitRows(rows []WaitRow) string {
	var b strings.Builder
	b.WriteString(strings.Join(WaitRowHeader, "\t") + "\n")
	for _, r := range rows {
		b.WriteString(strings.Join([]string{r.TaskID, r.State, strconv.Itoa(r.ElapsedMS), EscapeTerminal(r.LastLogLine),
			strconv.FormatBool(r.LogTruncated), strconv.FormatBool(r.DurabilityConfirmed)}, "\t") + "\n")
	}
	return b.String()
}
