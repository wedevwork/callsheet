package contract

import (
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"
)

// Protocol 4 reconciliation (iteration 06a, lifecycle.md "Reconnection
// protocol"): after hello the worker reports its stable executions in
// bounded task_inventory pages, then the plane answers with bounded
// task_reconcile pages of dispositions. Inventory carries metadata only,
// never prompts or output.
const (
	FrameTaskInventory    = "task_inventory"
	FrameTaskInventoryAck = "task_inventory_ack"
	FrameTaskReconcile    = "task_reconcile"
	FrameTaskReconcileAck = "task_reconcile_ack"

	// Body bounds (JSON whitespace included).
	MaxInventoryBody = 64 << 10
	MaxReconcileBody = 64 << 10
	MaxAckBody       = 1 << 10
	// Entry bounds per page.
	MaxInventoryEntries = 64
	MaxReconcileEntries = 64
)

// Inventory phases.
const (
	PhasePreparing = "preparing"
	PhaseRunning   = "running"
	PhaseResult    = "result"
	PhaseLost      = "lost"
)

// Reconcile actions.
const (
	ActionContinue   = "continue"
	ActionStopLost   = "stop_lost"
	ActionSendResult = "send_result"
	ActionForget     = "forget"
	// ActionStopControl (protocol 5) is defined with the controls.
)

var (
	digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	runIDRE  = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ValidDigest reports whether d is a SHA-256 digest in lowercase hex.
func ValidDigest(d string) bool { return digestRE.MatchString(d) }

// ValidRunID reports whether id is a sidecar Run ID: 32 lowercase hex.
func ValidRunID(id string) bool { return runIDRE.MatchString(id) }

// HexDigest renders a SHA-256 sum as 64 lowercase hex digits.
func HexDigest(sum [32]byte) string { return hex.EncodeToString(sum[:]) }

// StartDigestHex is the canonical start digest of b in lowercase hex.
func (b TaskStartBody) StartDigestHex() string { return HexDigest(b.Digest()) }

// TaskInventoryEntry is one stable execution a worker holds.
type TaskInventoryEntry struct {
	TaskID       string         `json:"task_id"`
	Execution    ExecutionToken `json:"execution"`
	StartDigest  string         `json:"start_digest"`
	Phase        string         `json:"phase"`
	StartedAt    *string        `json:"started_at"`
	ResultDigest *string        `json:"result_digest"`
}

func (e TaskInventoryEntry) validate(what string) error {
	switch {
	case !ValidTaskID(e.TaskID):
		return errInvalid("%s entry task_id is not a task ID", what)
	case !ValidDigest(e.StartDigest):
		return errInvalid("%s entry start_digest must be 64 lowercase hex digits", what)
	case e.StartedAt != nil && !validTime(*e.StartedAt):
		return errInvalid("%s entry started_at must be a UTC RFC3339 timestamp", what)
	case e.ResultDigest != nil && !ValidDigest(*e.ResultDigest):
		return errInvalid("%s entry result_digest must be 64 lowercase hex digits", what)
	}
	switch e.Phase {
	case PhasePreparing:
		if e.StartedAt != nil || e.ResultDigest != nil {
			return errInvalid("%s entry in phase preparing has no start time or result", what)
		}
	case PhaseRunning:
		if e.ResultDigest != nil {
			return errInvalid("%s entry in phase running has no result", what)
		}
	case PhaseResult, PhaseLost:
		if e.ResultDigest == nil {
			return errInvalid("%s entry in phase %s needs its result_digest", what, e.Phase)
		}
	default:
		return errInvalid("%s entry phase must be preparing, running, result or lost", what)
	}
	return nil
}

func validTime(s string) bool {
	_, ok := ParseTime(s)
	return ok
}

// TaskInventoryBody is one task_inventory page.
type TaskInventoryBody struct {
	RunID   string               `json:"run_id"`
	Page    int                  `json:"page"`
	Final   bool                 `json:"final"`
	Entries []TaskInventoryEntry `json:"entries"`
}

// MarshalJSON renders an empty page's entries as [].
func (b TaskInventoryBody) MarshalJSON() ([]byte, error) {
	type wire TaskInventoryBody
	w := wire(b)
	if w.Entries == nil {
		w.Entries = []TaskInventoryEntry{}
	}
	return compact(w)
}

// DecodeTaskInventory strictly decodes one inventory page: a Run ID, a
// page number, at most MaxInventoryEntries valid entries with unique task
// IDs.
func DecodeTaskInventory(body json.RawMessage) (TaskInventoryBody, error) {
	const what = "task_inventory body"
	var b TaskInventoryBody
	if len(body) > MaxInventoryBody {
		return b, errInvalid("%s exceeds %d bytes", what, MaxInventoryBody)
	}
	if err := decodeStrict(body, &b, what); err != nil {
		return b, err
	}
	switch {
	case !ValidRunID(b.RunID):
		return b, errInvalid("%s run_id must be 32 lowercase hex digits", what)
	case b.Page < 0 || b.Page > MaxSafeInteger:
		return b, errInvalid("%s page must be an integer from 0 to %d", what, MaxSafeInteger)
	case len(b.Entries) > MaxInventoryEntries:
		return b, errInvalid("%s has %d entries; at most %d are allowed", what, len(b.Entries), MaxInventoryEntries)
	}
	seen := map[string]bool{}
	for _, e := range b.Entries {
		if err := e.validate(what); err != nil {
			return b, err
		}
		if seen[e.TaskID] {
			return b, errInvalid("%s repeats task %s", what, e.TaskID)
		}
		seen[e.TaskID] = true
	}
	return b, nil
}

// TaskInventoryAckBody acknowledges receipt of an inventory page, not
// reconciliation.
type TaskInventoryAckBody struct {
	Page     int  `json:"page"`
	Received bool `json:"received"`
}

// DecodeTaskInventoryAck decodes a task_inventory_ack body.
func DecodeTaskInventoryAck(body json.RawMessage) (TaskInventoryAckBody, error) {
	const what = "task_inventory_ack body"
	var b TaskInventoryAckBody
	if err := decodeStrict(body, &b, what); err != nil {
		return b, err
	}
	if b.Page < 0 || b.Page > MaxSafeInteger || !b.Received {
		return b, errInvalid("%s must name a page with received=true", what)
	}
	return b, nil
}

// TaskReconcileEntry is one disposition of the plane. Stop (protocol 5)
// is the durable plane stop intent a stop_control latches; it is null for
// every other action.
type TaskReconcileEntry struct {
	TaskID    string         `json:"task_id"`
	Execution ExecutionToken `json:"execution"`
	Action    string         `json:"action"`
	Stop      *StopIntent    `json:"stop"`
}

// TaskReconcileBody is one task_reconcile page.
type TaskReconcileBody struct {
	Entries []TaskReconcileEntry `json:"entries"`
	Final   bool                 `json:"final"`
}

// MarshalJSON renders an empty page's entries as [].
func (b TaskReconcileBody) MarshalJSON() ([]byte, error) {
	type wire TaskReconcileBody
	w := wire(b)
	if w.Entries == nil {
		w.Entries = []TaskReconcileEntry{}
	}
	return compact(w)
}

// DecodeTaskReconcile strictly decodes one reconcile page.
func DecodeTaskReconcile(body json.RawMessage) (TaskReconcileBody, error) {
	const what = "task_reconcile body"
	var b TaskReconcileBody
	if len(body) > MaxReconcileBody {
		return b, errInvalid("%s exceeds %d bytes", what, MaxReconcileBody)
	}
	if err := decodeStrict(body, &b, what); err != nil {
		return b, err
	}
	if len(b.Entries) > MaxReconcileEntries {
		return b, errInvalid("%s has %d entries; at most %d are allowed", what, len(b.Entries), MaxReconcileEntries)
	}
	seen := map[string]bool{}
	for _, e := range b.Entries {
		switch {
		case !ValidTaskID(e.TaskID):
			return b, errInvalid("%s entry task_id is not a task ID", what)
		case e.Action != ActionContinue && e.Action != ActionStopLost && e.Action != ActionSendResult && e.Action != ActionForget && e.Action != ActionStopControl:
			return b, errInvalid("%s entry action must be continue, stop_lost, send_result, forget or stop_control", what)
		case (e.Action == ActionStopControl) != (e.Stop != nil):
			return b, errInvalid("%s entry stop is required exactly for stop_control", what)
		case seen[e.TaskID]:
			return b, errInvalid("%s repeats task %s", what, e.TaskID)
		}
		seen[e.TaskID] = true
	}
	return b, nil
}

// TaskReconcileAckBody acknowledges a reconcile page.
type TaskReconcileAckBody struct {
	Received bool `json:"received"`
}

// DecodeTaskReconcileAck decodes a task_reconcile_ack body.
func DecodeTaskReconcileAck(body json.RawMessage) (TaskReconcileAckBody, error) {
	const what = "task_reconcile_ack body"
	var b TaskReconcileAckBody
	if err := decodeStrict(body, &b, what); err != nil {
		return b, err
	}
	if !b.Received {
		return b, errInvalid("%s must carry received=true", what)
	}
	return b, nil
}

// LateResult is a terminal task's single late-evidence slot (FP-7): a
// worker outcome recorded after a different terminal decision, with its
// own bounded log tail. It never changes the terminal state.
type LateResult struct {
	Digest                string
	ReceivedAt            time.Time
	Outcome               string
	ExitCode              *int
	Signal                *string
	FinalMessage          *string
	FinalMessageTruncated bool
	OutputBytes           int
	LogIncomplete         bool
	CounterOverflow       bool
	Log                   TaskLog
}

// lateWire is a late result's persisted form.
type lateWire struct {
	Digest                string          `json:"digest"`
	ReceivedAt            string          `json:"received_at"`
	Outcome               string          `json:"outcome"`
	ExitCode              *int            `json:"exit_code"`
	Signal                *string         `json:"signal"`
	FinalMessage          *string         `json:"final_message"`
	FinalMessageTruncated bool            `json:"final_message_truncated"`
	OutputBytes           int             `json:"output_bytes"`
	LogIncomplete         bool            `json:"log_incomplete"`
	CounterOverflow       bool            `json:"counter_overflow"`
	Log                   json.RawMessage `json:"log"`
}

// LateFrom builds the late evidence of result b received at t.
func LateFrom(b TaskResultBody, at time.Time, lg TaskLog) LateResult {
	return LateResult{Digest: b.Digest, ReceivedAt: at.UTC(), Outcome: b.Outcome, ExitCode: b.ExitCode, Signal: b.Signal,
		FinalMessage: b.FinalMessage, FinalMessageTruncated: b.FinalMessageTruncated, OutputBytes: b.OutputBytes,
		LogIncomplete: b.LogIncomplete, CounterOverflow: b.CounterOverflow, Log: lg}
}

func (l LateResult) validate(what string) error {
	switch {
	case !ValidDigest(l.Digest):
		return errInvalid("%s late_result digest must be 64 lowercase hex digits", what)
	case !validOutcome(l.Outcome):
		return errInvalid("%s late_result outcome must be natural, lost, cancelled or timed_out", what)
	case l.Outcome == OutcomeNatural && (l.ExitCode == nil) == (l.Signal == nil):
		return errInvalid("%s late_result needs exactly one of exit_code and signal", what)
	case l.ExitCode != nil && l.Signal != nil:
		return errInvalid("%s late_result has both exit_code and signal", what)
	case l.ExitCode != nil && (*l.ExitCode < 0 || *l.ExitCode > 255):
		return errInvalid("%s late_result exit_code must be an integer from 0 to 255", what)
	case l.Signal != nil && !ValidWireSignal(*l.Signal):
		return errInvalid("%s late_result signal is not a known signal name", what)
	case l.OutputBytes < 0 || l.OutputBytes > MaxSafeInteger:
		return errInvalid("%s late_result output_bytes is out of range", what)
	}
	if err := checkFinalMessage(l.FinalMessage, l.FinalMessageTruncated, what+" late_result"); err != nil {
		return err
	}
	return l.Log.Validate()
}

// TaskLateSummary is the public late_result summary of task show: the
// outcome, digest, bounded final message and log metadata; its tail is
// read with task logs --late.
type TaskLateSummary struct {
	Digest                string      `json:"digest"`
	ReceivedAt            string      `json:"received_at"`
	Outcome               string      `json:"outcome"`
	ExitCode              *int        `json:"exit_code"`
	Signal                *string     `json:"signal"`
	FinalMessage          *string     `json:"final_message"`
	FinalMessageTruncated bool        `json:"final_message_truncated"`
	OutputBytes           int         `json:"output_bytes"`
	Log                   TaskLogMeta `json:"log"`
}

// SummaryOf is l's public summary.
func SummaryOf(l LateResult) *TaskLateSummary {
	return &TaskLateSummary{Digest: l.Digest, ReceivedAt: FormatTime(l.ReceivedAt), Outcome: l.Outcome, ExitCode: l.ExitCode, Signal: l.Signal,
		FinalMessage: l.FinalMessage, FinalMessageTruncated: l.FinalMessageTruncated, OutputBytes: l.OutputBytes, Log: MetaOf(l.Log, false)}
}

func (s TaskLateSummary) validate(what string) error {
	l := LateResult{Digest: s.Digest, Outcome: s.Outcome, ExitCode: s.ExitCode, Signal: s.Signal, FinalMessage: s.FinalMessage,
		FinalMessageTruncated: s.FinalMessageTruncated, OutputBytes: s.OutputBytes}
	if !validTime(s.ReceivedAt) {
		return errInvalid("%s late_result received_at must be a UTC RFC3339 timestamp", what)
	}
	if err := l.validate(what); err != nil {
		return err
	}
	return s.Log.validate(what + " late_result")
}
