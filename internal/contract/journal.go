package contract

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The sidecar's task journal and guardian codecs (iteration 06a,
// resilience.md "Recoverable launch"): private local documents and
// messages, strictly decoded with the contract's machinery. They never
// cross the network and never carry prompts or manual content (the
// guardian invocation carries the adapter's argv and environment on a
// private inherited pipe only).
const (
	// ExecutionJournalSchemaVersion is execution.json's schema (iteration
	// 10b: the journal-owned work directory, the workspace binding and the
	// owned runtime directory); ExecutionJournalSchema2 is 06b's (the stop
	// intent and control outcomes) and LegacyExecutionJournalSchemaVersion
	// 06a's, both still decoded strictly and normalized. OwnerSchemaVersion
	// is owner.json's, unchanged.
	ExecutionJournalSchemaVersion       = 3
	ExecutionJournalSchema2             = 2
	LegacyExecutionJournalSchemaVersion = 1
	OwnerSchemaVersion                  = 1
	// MaxExecutionJournalBytes bounds execution.json (the retained tail
	// dominates); MaxOwnerBytes bounds owner.json.
	MaxExecutionJournalBytes = 16 << 20
	MaxOwnerBytes            = 8 << 10
	// MaxGuardianInvocationBytes bounds the guardian's invocation; its
	// status messages are each at most MaxGuardianStatusBytes; a control
	// command is at most MaxGuardianCommandBytes (below PIPE_BUF).
	MaxGuardianInvocationBytes = 1 << 20
	MaxGuardianStatusBytes     = 8 << 10
	MaxGuardianCommandBytes    = 512
	// GuardianInvocationVersion is the invocation format (iteration 06b:
	// version 2 adds the timeout and its policy).
	GuardianInvocationVersion = 2
)

// Execution journal phases.
const (
	JournalPrepared  = "prepared"
	JournalRunning   = "running"
	JournalCompleted = "completed"
	JournalLost      = "lost"
)

// Owner record phases.
const (
	OwnerArmed    = "armed"
	OwnerReleased = "released"
)

// Guardian status message types.
const (
	GuardianReady   = "ready"
	GuardianStarted = "started"
	GuardianExit    = "exit"
	GuardianError   = "error"
	// GuardianStopping (iteration 06b) reports the guardian's latched
	// cleanup cause before its deliberate group KILL.
	GuardianStopping = "stopping"
)

// Guardian cleanup causes of a stopping status.
const (
	CauseCancelled = "cancelled"
	CauseTimedOut  = "timed_out"
	CauseLost      = "lost"
)

// GuardianStop is the only control command.
const GuardianStop = "stop"

var nonceRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidNonce reports whether n is a guardian nonce: 32 bytes in
// lowercase hex.
func ValidNonce(n string) bool { return nonceRE.MatchString(n) }

// ExecutionJournal is tasks/<task_id>/execution.json: the sidecar's
// durable record of one stable execution, its launch barriers and its
// frozen outcome (the outbox), with the unsent retained output tail
// ending at the log's source count.
type ExecutionJournal struct {
	TaskID        string
	Execution     ExecutionToken
	StartDigest   string
	Role          RoleRecord
	Effective     TaskEffective
	TimeoutPolicy string
	OwnerNonce    *string
	Phase         string
	// StopIntent is the plane's stop intent latched by this execution
	// (iteration 06b; nil for none and for a guardian's own timeout).
	StopIntent *StopIntent
	StartedAt  *time.Time
	Result     *TaskResultBody
	Log        TaskLog
	// Work (schema 3) reports the journal-owned work directory
	// tasks/<task_id>/work (the child's cwd); Workspace is the immutable
	// binding of a workspace task and RuntimeDir its owned final-output
	// directory's name inside work (null for none).
	Work       bool
	Workspace  *WorkspaceBinding
	RuntimeDir *string
	// Schema is the decoded schema (1 to 3); the encoder writes 3.
	Schema int
}

type executionWire struct {
	SchemaVersion int               `json:"schema_version"`
	TaskID        string            `json:"task_id"`
	Execution     ExecutionToken    `json:"execution"`
	StartDigest   string            `json:"start_digest"`
	Role          json.RawMessage   `json:"role"`
	Effective     TaskEffective     `json:"effective"`
	TimeoutPolicy string            `json:"timeout_policy"`
	OwnerNonce    *string           `json:"owner_nonce"`
	Phase         string            `json:"phase"`
	StopIntent    *StopIntent       `json:"stop_intent"`
	Work          bool              `json:"work"`
	Workspace     *WorkspaceBinding `json:"workspace"`
	RuntimeDir    *string           `json:"runtime_dir"`
	StartedAt     *string           `json:"started_at"`
	Result        *TaskResultBody   `json:"result"`
	Log           json.RawMessage   `json:"log"`
}

// executionWireV2 is iteration 06b's exact schema-2 journal.
type executionWireV2 struct {
	SchemaVersion int             `json:"schema_version"`
	TaskID        string          `json:"task_id"`
	Execution     ExecutionToken  `json:"execution"`
	StartDigest   string          `json:"start_digest"`
	Role          json.RawMessage `json:"role"`
	Effective     TaskEffective   `json:"effective"`
	TimeoutPolicy string          `json:"timeout_policy"`
	OwnerNonce    *string         `json:"owner_nonce"`
	Phase         string          `json:"phase"`
	StopIntent    *StopIntent     `json:"stop_intent"`
	StartedAt     *string         `json:"started_at"`
	Result        *TaskResultBody `json:"result"`
	Log           json.RawMessage `json:"log"`
}

// RuntimeDirPrefix starts an owned final-output directory's name inside a
// workspace task's work directory (128 random bits follow in hex).
const RuntimeDirPrefix = ".callsheet-runtime-"

// ValidRuntimeDir reports .callsheet-runtime- plus 32 lowercase hex.
func ValidRuntimeDir(s string) bool {
	tok, ok := strings.CutPrefix(s, RuntimeDirPrefix)
	return ok && ValidWorkspaceToken(tok)
}

// executionWireV1 is iteration 06a's exact schema-1 journal: no stop
// intent, and a result without stop_id (its digest is the same one a null
// stop_id seals).
type executionWireV1 struct {
	SchemaVersion int             `json:"schema_version"`
	TaskID        string          `json:"task_id"`
	Execution     ExecutionToken  `json:"execution"`
	StartDigest   string          `json:"start_digest"`
	Role          json.RawMessage `json:"role"`
	Effective     TaskEffective   `json:"effective"`
	TimeoutPolicy string          `json:"timeout_policy"`
	OwnerNonce    *string         `json:"owner_nonce"`
	Phase         string          `json:"phase"`
	StartedAt     *string         `json:"started_at"`
	Result        *legacyResultV1 `json:"result"`
	Log           json.RawMessage `json:"log"`
}

// legacyResultV1 is a protocol 4 result (no stop_id).
type legacyResultV1 struct {
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
	Digest                string         `json:"digest"`
}

// EncodeExecutionJournal renders j in its two-space indented writer form
// with one final LF; the log tail is base64, spliced into one buffer.
func EncodeExecutionJournal(j ExecutionJournal) ([]byte, error) {
	data := j.Log.Data
	j.Log.Data = nil
	role, err := compact(j.Role)
	if err != nil {
		return nil, err
	}
	lg, err := compact(j.Log)
	if err != nil {
		return nil, err
	}
	policy := j.TimeoutPolicy
	if policy == "" {
		policy = TimeoutPolicyLegacy
	}
	w := executionWire{SchemaVersion: ExecutionJournalSchemaVersion, TaskID: j.TaskID, Execution: j.Execution, StartDigest: j.StartDigest, Role: role,
		Effective: j.Effective, TimeoutPolicy: policy, OwnerNonce: j.OwnerNonce, Phase: j.Phase, StopIntent: j.StopIntent, Work: j.Work,
		Workspace: j.Workspace, RuntimeDir: j.RuntimeDir, StartedAt: timePtr(j.StartedAt), Result: j.Result, Log: lg}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(w); err != nil {
		return nil, err
	}
	meta := b.Bytes()
	if bytes.Count(meta, logDataKey) != 1 {
		return nil, errInvalid("execution journal log is not encodable")
	}
	i := bytes.Index(meta, logDataKey) + len(logDataKey) - 1
	// The bound is checked before the tail is encoded.
	if size := len(meta) + base64.StdEncoding.EncodedLen(len(data)); size > MaxExecutionJournalBytes {
		return nil, errInvalid("execution journal would be %d bytes, above %d", size, MaxExecutionJournalBytes)
	}
	out := make([]byte, len(meta)+base64.StdEncoding.EncodedLen(len(data)))
	n := copy(out, meta[:i])
	base64.StdEncoding.Encode(out[n:], data)
	copy(out[n+base64.StdEncoding.EncodedLen(len(data)):], meta[i:])
	return out, nil
}

// ParseExecutionJournal strictly decodes and validates one execution
// journal against lookup: schema 2, or 06a's schema 1 with its exact wire
// struct normalized in memory (null intent, legacy policy, the original
// frozen result and digest). It checks identity, role and effort, timeout
// policy, the phase invariants (a prepared record has no owner nonce,
// start or result; a running one has its owner nonce; completed holds a
// natural or control result and lost a lost one, both sealed and naming
// this execution; a timed_out result has its start) and the log counters.
func ParseExecutionJournal(data []byte, lookup AdapterLookup) (ExecutionJournal, error) {
	const what = "execution journal"
	if len(data) > MaxExecutionJournalBytes {
		return ExecutionJournal{}, errInvalid("%s is larger than %d bytes", what, MaxExecutionJournalBytes)
	}
	var probe struct {
		SchemaVersion json.RawMessage `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.SchemaVersion == nil {
		return ExecutionJournal{}, errInvalid("%s is not a JSON object with a schema_version", what)
	}
	var w executionWire
	switch v, _ := ParseInteger(string(bytes.TrimSpace(probe.SchemaVersion))); v {
	case LegacyExecutionJournalSchemaVersion:
		var o executionWireV1
		if err := decodeStrict(data, &o, what); err != nil {
			return ExecutionJournal{}, err
		}
		if o.TimeoutPolicy != TimeoutPolicyLegacy {
			return ExecutionJournal{}, errInvalid("%s timeout_policy must be %q in schema 1", what, TimeoutPolicyLegacy)
		}
		w = executionWire{SchemaVersion: o.SchemaVersion, TaskID: o.TaskID, Execution: o.Execution, StartDigest: o.StartDigest, Role: o.Role,
			Effective: o.Effective, TimeoutPolicy: o.TimeoutPolicy, OwnerNonce: o.OwnerNonce, Phase: o.Phase, StartedAt: o.StartedAt, Log: o.Log}
		if r := o.Result; r != nil {
			if r.Outcome != OutcomeNatural && r.Outcome != OutcomeLost {
				return ExecutionJournal{}, errInvalid("%s schema 1 result outcome must be natural or lost", what)
			}
			w.Result = &TaskResultBody{TaskID: r.TaskID, Execution: r.Execution, Outcome: r.Outcome, ExitCode: r.ExitCode, Signal: r.Signal,
				FinalMessage: r.FinalMessage, FinalMessageTruncated: r.FinalMessageTruncated, OutputBytes: r.OutputBytes,
				LogIncomplete: r.LogIncomplete, CounterOverflow: r.CounterOverflow, Digest: r.Digest}
		}
	case ExecutionJournalSchema2:
		var o executionWireV2
		if err := decodeStrict(data, &o, what); err != nil {
			return ExecutionJournal{}, err
		}
		if !ValidTimeoutPolicy(o.TimeoutPolicy) {
			return ExecutionJournal{}, errInvalid("%s timeout_policy must be %q or %q", what, TimeoutPolicyEnforced, TimeoutPolicyLegacy)
		}
		if o.Result != nil && o.Result.Workspace != nil {
			return ExecutionJournal{}, errInvalid("%s: a schema 2 result has no workspace", what)
		}
		w = executionWire{SchemaVersion: o.SchemaVersion, TaskID: o.TaskID, Execution: o.Execution, StartDigest: o.StartDigest, Role: o.Role,
			Effective: o.Effective, TimeoutPolicy: o.TimeoutPolicy, OwnerNonce: o.OwnerNonce, Phase: o.Phase, StopIntent: o.StopIntent,
			StartedAt: o.StartedAt, Result: o.Result, Log: o.Log}
	case ExecutionJournalSchemaVersion:
		if err := decodeStrict(data, &w, what); err != nil {
			return ExecutionJournal{}, err
		}
		if !ValidTimeoutPolicy(w.TimeoutPolicy) {
			return ExecutionJournal{}, errInvalid("%s timeout_policy must be %q or %q", what, TimeoutPolicyEnforced, TimeoutPolicyLegacy)
		}
		switch {
		case w.Workspace != nil && !w.Work:
			// work=false is an execution migrated from schema 1 or 2, whose
			// scratch directory was never journal-owned.
			return ExecutionJournal{}, errInvalid("%s: a workspace execution owns its work directory", what)
		case w.RuntimeDir != nil && (w.Workspace == nil || !ValidRuntimeDir(*w.RuntimeDir)):
			return ExecutionJournal{}, errInvalid("%s runtime_dir is an owned .callsheet-runtime- directory of a workspace task", what)
		case w.Result != nil && (w.Result.Workspace != nil) != (w.Workspace != nil):
			return ExecutionJournal{}, errInvalid("%s: a result carries its workspace result exactly for a workspace task", what)
		}
		if w.Workspace != nil {
			if err := w.Workspace.Validate(); err != nil {
				return ExecutionJournal{}, err
			}
			if r := w.Result; r != nil && (r.Workspace.Name != w.Workspace.Name || r.Workspace.Instance != w.Workspace.Instance ||
				!sameHash(r.Workspace.BaseCommit, w.Workspace.BaseCommit)) {
				return ExecutionJournal{}, errInvalid("%s result's workspace result does not match the binding", what)
			}
		}
	default:
		return ExecutionJournal{}, errInvalid("%s schema_version %s is not supported (this build supports %d, %d and %d)", what, SafeText(string(probe.SchemaVersion), 32),
			LegacyExecutionJournalSchemaVersion, ExecutionJournalSchema2, ExecutionJournalSchemaVersion)
	}
	switch {
	case !ValidTaskID(w.TaskID):
		return ExecutionJournal{}, errInvalid("%s task_id is not a task ID", what)
	case !ValidDigest(w.StartDigest):
		return ExecutionJournal{}, errInvalid("%s start_digest must be 64 lowercase hex digits", what)
	case w.OwnerNonce != nil && !ValidNonce(*w.OwnerNonce):
		return ExecutionJournal{}, errInvalid("%s owner_nonce must be null or 64 lowercase hex digits", what)
	case w.StartedAt != nil && !validTime(*w.StartedAt):
		return ExecutionJournal{}, errInvalid("%s started_at must be a UTC RFC3339 timestamp", what)
	}
	role, err := ParseRoleRecord(w.Role, lookup)
	if err != nil {
		return ExecutionJournal{}, err
	}
	if err := CheckEffort(role.Adapter, w.Effective.Effort, lookup); err != nil {
		return ExecutionJournal{}, err
	}
	switch w.Phase {
	case JournalPrepared:
		if w.OwnerNonce != nil || w.StartedAt != nil || w.Result != nil {
			return ExecutionJournal{}, errInvalid("%s: a prepared execution has no owner nonce, start or result", what)
		}
	case JournalRunning:
		if w.OwnerNonce == nil || w.Result != nil {
			return ExecutionJournal{}, errInvalid("%s: a running execution has its owner nonce and no result", what)
		}
	case JournalCompleted, JournalLost:
		ok := w.Result != nil && w.Result.Outcome == OutcomeLost
		if w.Phase == JournalCompleted {
			ok = w.Result != nil && w.Result.Outcome != OutcomeLost
		}
		if !ok {
			return ExecutionJournal{}, errInvalid("%s: a %s execution holds a matching result", what, w.Phase)
		}
		if w.Result.TaskID != w.TaskID || w.Result.Execution != w.Execution {
			return ExecutionJournal{}, errInvalid("%s result names another execution", what)
		}
		if err := w.Result.Validate(); err != nil {
			return ExecutionJournal{}, err
		}
		if w.Result.Outcome == OutcomeTimedOut && w.StartedAt == nil {
			return ExecutionJournal{}, errInvalid("%s: a timed_out execution has its start", what)
		}
		if id := w.Result.StopID; id != nil && (w.StopIntent == nil || w.StopIntent.ID != *id) {
			return ExecutionJournal{}, errInvalid("%s result stop_id names no latched stop intent", what)
		}
	default:
		return ExecutionJournal{}, errInvalid("%s phase must be prepared, running, completed or lost", what)
	}
	var lg TaskLog
	if err := decodeStrict(w.Log, &lg, what+" log"); err != nil {
		return ExecutionJournal{}, err
	}
	if err := lg.Validate(); err != nil {
		return ExecutionJournal{}, err
	}
	j := ExecutionJournal{TaskID: w.TaskID, Execution: w.Execution, StartDigest: w.StartDigest, Role: role, Effective: w.Effective,
		TimeoutPolicy: w.TimeoutPolicy, OwnerNonce: w.OwnerNonce, Phase: w.Phase, StopIntent: w.StopIntent, Result: w.Result, Log: lg,
		Work: w.Work, Workspace: w.Workspace, RuntimeDir: w.RuntimeDir, Schema: w.SchemaVersion}
	if w.StartedAt != nil {
		t, _ := ParseTime(*w.StartedAt)
		j.StartedAt = &t
	}
	return j, nil
}

// OwnerRecord is tasks/<task_id>/owner.json, written only by the task's
// guardian: its own process group (PGID equals its PID) and the local
// execution-correlation nonce. It is never exposed through the plane.
type OwnerRecord struct {
	SchemaVersion int            `json:"schema_version"`
	TaskID        string         `json:"task_id"`
	Execution     ExecutionToken `json:"execution"`
	Nonce         string         `json:"nonce"`
	PGID          int            `json:"pgid"`
	GuardianPID   int            `json:"guardian_pid"`
	Phase         string         `json:"phase"`
}

// EncodeOwner renders an owner record (indented, final LF).
func EncodeOwner(o OwnerRecord) ([]byte, error) {
	o.SchemaVersion = OwnerSchemaVersion
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ParseOwner strictly decodes an owner record: schema 1, identity, nonce,
// guardian PID equal to its PGID above 1, and phase armed or released.
func ParseOwner(data []byte) (OwnerRecord, error) {
	const what = "owner record"
	var o OwnerRecord
	if len(data) > MaxOwnerBytes {
		return o, errInvalid("%s is larger than %d bytes", what, MaxOwnerBytes)
	}
	if err := decodeStrict(data, &o, what); err != nil {
		return o, err
	}
	switch {
	case o.SchemaVersion != OwnerSchemaVersion:
		return o, errInvalid("%s schema_version %d is not supported", what, o.SchemaVersion)
	case !ValidTaskID(o.TaskID):
		return o, errInvalid("%s task_id is not a task ID", what)
	case !ValidNonce(o.Nonce):
		return o, errInvalid("%s nonce must be 64 lowercase hex digits", what)
	case o.PGID <= 1 || o.GuardianPID != o.PGID || o.PGID > MaxSafeInteger:
		return o, errInvalid("%s guardian_pid must equal its pgid, above 1", what)
	case o.Phase != OwnerArmed && o.Phase != OwnerReleased:
		return o, errInvalid("%s phase must be armed or released", what)
	}
	return o, nil
}

// GuardianInvocation is the guardian's input on its fd 3: the validated
// task identity, its private task directory and nonce, and how to run the
// adapter (no shell parsing: an absolute path, arguments, environment and
// working directory).
type GuardianInvocation struct {
	Version     int            `json:"version"`
	TaskID      string         `json:"task_id"`
	Execution   ExecutionToken `json:"execution"`
	StartDigest string         `json:"start_digest"`
	TaskDir     string         `json:"task_dir"`
	Nonce       string         `json:"nonce"`
	Path        string         `json:"path"`
	Argv        []string       `json:"argv"`
	Env         []string       `json:"env"`
	Dir         string         `json:"dir"`
	// Timeout and TimeoutPolicy (version 2): the canonical effective
	// execution duration ("0s" unlimited) the guardian enforces from its
	// adapter's Start authorization when the policy is enforced.
	Timeout       string `json:"timeout"`
	TimeoutPolicy string `json:"timeout_policy"`
}

// TimeoutDuration is the invocation's parsed timeout (0: none).
func (g GuardianInvocation) TimeoutDuration() time.Duration {
	d, _ := ParseRoleTimeout(g.Timeout)
	return d
}

// Validate checks the invocation's identity and shape.
func (g GuardianInvocation) Validate() error {
	const what = "guardian invocation"
	switch {
	case g.Version != GuardianInvocationVersion:
		return errInvalid("%s version must be %d", what, GuardianInvocationVersion)
	case !ValidTaskID(g.TaskID):
		return errInvalid("%s task_id is not a task ID", what)
	case !ValidEpoch(g.Execution.Epoch) || g.Execution.Attachment < 1:
		return errInvalid("%s execution is invalid", what)
	case !ValidDigest(g.StartDigest):
		return errInvalid("%s start_digest must be 64 lowercase hex digits", what)
	case !ValidNonce(g.Nonce):
		return errInvalid("%s nonce must be 64 lowercase hex digits", what)
	case !filepath.IsAbs(g.TaskDir) || filepath.Clean(g.TaskDir) != g.TaskDir || filepath.Base(g.TaskDir) != g.TaskID ||
		filepath.Base(filepath.Dir(g.TaskDir)) != "tasks":
		return errInvalid("%s task_dir must be the absolute private tasks/<task_id> directory", what)
	case !filepath.IsAbs(g.Path) || !filepath.IsAbs(g.Dir):
		return errInvalid("%s path and dir must be absolute", what)
	case !ValidTimeoutPolicy(g.TimeoutPolicy):
		return errInvalid("%s timeout_policy must be %q or %q", what, TimeoutPolicyEnforced, TimeoutPolicyLegacy)
	}
	if d, err := ParseRoleTimeout(g.Timeout); err != nil || d.String() != g.Timeout {
		return errInvalid("%s timeout must be a canonical nonnegative Go duration", what)
	}
	for _, s := range append(append([]string{}, g.Argv...), g.Env...) {
		if bytes.IndexByte([]byte(s), 0) >= 0 {
			return errInvalid("%s arguments and environment must not contain NUL", what)
		}
	}
	return nil
}

// GuardianStatus is one status message on the guardian's fd 6: ready
// (durable ownership: its pid and pgid), started (the adapter's pid and
// launch timestamp), exit (the adapter's exact wait status) or error (a
// safe reason; no adapter was started).
type GuardianStatus struct {
	Type      string  `json:"type"`
	TaskID    string  `json:"task_id"`
	Nonce     string  `json:"nonce"`
	PID       int     `json:"pid"`
	PGID      int     `json:"pgid"`
	StartedAt *string `json:"started_at"`
	ExitCode  *int    `json:"exit_code"`
	Signal    *string `json:"signal"`
	Reason    *string `json:"reason"`
	// Cause and StopID (iteration 06b) belong to stopping only.
	Cause  *string `json:"cause"`
	StopID *string `json:"stop_id"`
}

// Validate checks a status message's type-specific shape.
func (s GuardianStatus) Validate() error {
	const what = "guardian status"
	if !ValidTaskID(s.TaskID) || !ValidNonce(s.Nonce) {
		return errInvalid("%s must name a task and its nonce", what)
	}
	if s.Type != GuardianStopping && (s.Cause != nil || s.StopID != nil) {
		return errInvalid("%s: only stopping carries a cause and stop_id", what)
	}
	switch s.Type {
	case GuardianStopping:
		c := ""
		if s.Cause != nil {
			c = *s.Cause
		}
		switch {
		case c != CauseCancelled && c != CauseTimedOut && c != CauseLost:
			return errInvalid("%s stopping cause must be cancelled, timed_out or lost", what)
		case (c == CauseCancelled) != (s.StopID != nil), s.StopID != nil && !ValidStopID(*s.StopID):
			return errInvalid("%s stopping carries a stop_id exactly for a cancelled cause", what)
		case s.PID != 0 || s.PGID != 0 || s.StartedAt != nil || s.ExitCode != nil || s.Signal != nil || s.Reason != nil:
			return errInvalid("%s stopping carries only its cause and stop_id", what)
		}
	case GuardianReady:
		if s.PID <= 1 || s.PGID != s.PID || s.StartedAt != nil || s.ExitCode != nil || s.Signal != nil || s.Reason != nil {
			return errInvalid("%s ready carries the guardian's pid equal to its pgid, above 1", what)
		}
	case GuardianStarted:
		if s.PID <= 1 || s.StartedAt == nil || !validTime(*s.StartedAt) || s.ExitCode != nil || s.Signal != nil || s.Reason != nil {
			return errInvalid("%s started carries the adapter pid and its launch time", what)
		}
	case GuardianExit:
		if (s.ExitCode == nil) == (s.Signal == nil) || (s.ExitCode != nil && (*s.ExitCode < 0 || *s.ExitCode > 255)) ||
			(s.Signal != nil && !ValidWireSignal(*s.Signal)) || s.Reason != nil || s.StartedAt != nil {
			return errInvalid("%s exit carries exactly one of exit_code and signal", what)
		}
	case GuardianError:
		if s.Reason == nil || len(*s.Reason) == 0 || len(*s.Reason) > 64 || s.ExitCode != nil || s.Signal != nil || s.StartedAt != nil {
			return errInvalid("%s error carries a short reason", what)
		}
	default:
		return errInvalid("%s type must be ready, started, stopping, exit or error", what)
	}
	return nil
}

// WriteFramed writes b with a 4-byte big-endian length prefix.
func WriteFramed(w io.Writer, b []byte) error {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	if _, err := w.Write(append(n[:], b...)); err != nil {
		return err
	}
	return nil
}

// ReadFramed reads one length-prefixed message of at most limit bytes; a
// larger announced length is rejected before anything else is read.
func ReadFramed(r io.Reader, limit int) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if int64(size) > int64(limit) {
		return nil, errInvalid("framed message of %d bytes exceeds %d", size, limit)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return b, nil
}

// EncodeGuardianStatus renders a validated status message.
func EncodeGuardianStatus(s GuardianStatus) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return compact(s)
}

// ParseGuardianStatus strictly decodes and validates a status message.
func ParseGuardianStatus(b []byte) (GuardianStatus, error) {
	var s GuardianStatus
	if len(b) > MaxGuardianStatusBytes {
		return s, errInvalid("guardian status exceeds %d bytes", MaxGuardianStatusBytes)
	}
	if err := decodeStrict(b, &s, "guardian status"); err != nil {
		return s, err
	}
	return s, s.Validate()
}

// ParseGuardianInvocation strictly decodes and validates an invocation.
func ParseGuardianInvocation(b []byte) (GuardianInvocation, error) {
	var g GuardianInvocation
	if len(b) > MaxGuardianInvocationBytes {
		return g, errInvalid("guardian invocation exceeds %d bytes", MaxGuardianInvocationBytes)
	}
	if err := decodeStrict(b, &g, "guardian invocation"); err != nil {
		return g, err
	}
	return g, g.Validate()
}

// GuardianCommand is one control FIFO command: a request, never a PID
// signalling channel. The guardian verifies every identity and signals
// only its own still-owned group.
type GuardianCommand struct {
	TaskID    string         `json:"task_id"`
	Execution ExecutionToken `json:"execution"`
	Nonce     string         `json:"nonce"`
	Command   string         `json:"command"`
	// Cause and StopID (iteration 06b, optional): cancelled with its plane
	// stop intent. A command without a cause is 06a's stop (recovery), the
	// only form sent to a recovered, possibly older, guardian.
	Cause  string `json:"cause,omitempty"`
	StopID string `json:"stop_id,omitempty"`
}

// EncodeGuardianCommand renders one command line (JSON and LF) of at most
// MaxGuardianCommandBytes.
func EncodeGuardianCommand(c GuardianCommand) ([]byte, error) {
	if (c.Cause != "" || c.StopID != "") && (c.Cause != CauseCancelled || !ValidStopID(c.StopID)) {
		return nil, errInvalid("guardian command cause must be absent or cancelled with its stop_id")
	}
	b, err := compact(c)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if len(b) > MaxGuardianCommandBytes {
		return nil, errInvalid("guardian command exceeds %d bytes", MaxGuardianCommandBytes)
	}
	return b, nil
}

// ParseGuardianCommand strictly decodes one command line (without LF).
func ParseGuardianCommand(b []byte) (GuardianCommand, error) {
	var c GuardianCommand
	if len(b) >= MaxGuardianCommandBytes {
		return c, errInvalid("guardian command exceeds %d bytes", MaxGuardianCommandBytes)
	}
	if err := decodeStrict(b, &c, "guardian command"); err != nil {
		return c, err
	}
	if !ValidTaskID(c.TaskID) || !ValidNonce(c.Nonce) || c.Command != GuardianStop {
		return c, errInvalid("guardian command must name a task, its nonce and stop")
	}
	switch {
	case c.Cause == "" && c.StopID == "":
	case c.Cause == CauseCancelled && ValidStopID(c.StopID):
	default:
		return c, errInvalid("guardian command cause must be absent or cancelled with its stop_id")
	}
	return c, nil
}

// FormatInt renders n in decimal (for diagnostics).
func FormatInt(n int) string { return strconv.Itoa(n) }

// EncodeGuardianInvocation renders a validated invocation (framed by the
// caller with WriteFramed).
func EncodeGuardianInvocation(g GuardianInvocation) ([]byte, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	if g.Argv == nil {
		g.Argv = []string{}
	}
	if g.Env == nil {
		g.Env = []string{}
	}
	b, err := compact(g)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxGuardianInvocationBytes {
		return nil, errInvalid("guardian invocation exceeds %d bytes", MaxGuardianInvocationBytes)
	}
	return b, nil
}
