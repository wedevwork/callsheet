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
	"time"
)

// The sidecar's task journal and guardian codecs (iteration 06a,
// resilience.md "Recoverable launch"): private local documents and
// messages, strictly decoded with the contract's machinery. They never
// cross the network and never carry prompts or manual content (the
// guardian invocation carries the adapter's argv and environment on a
// private inherited pipe only).
const (
	// JournalSchemaVersion is execution.json's and owner.json's schema.
	JournalSchemaVersion = 1
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
	// GuardianInvocationVersion is the invocation format.
	GuardianInvocationVersion = 1
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
	StartedAt     *time.Time
	Result        *TaskResultBody
	Log           TaskLog
}

type executionWire struct {
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
	Result        *TaskResultBody `json:"result"`
	Log           json.RawMessage `json:"log"`
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
	w := executionWire{SchemaVersion: JournalSchemaVersion, TaskID: j.TaskID, Execution: j.Execution, StartDigest: j.StartDigest, Role: role,
		Effective: j.Effective, TimeoutPolicy: policy, OwnerNonce: j.OwnerNonce, Phase: j.Phase, StartedAt: timePtr(j.StartedAt), Result: j.Result, Log: lg}
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
// journal against lookup: identity, role and effort, timeout policy, the
// phase invariants (a prepared record has no owner nonce, start or
// result; a running one has its owner nonce; completed holds a natural
// result and lost a lost one, both sealed and naming this execution) and
// the log counters.
func ParseExecutionJournal(data []byte, lookup AdapterLookup) (ExecutionJournal, error) {
	const what = "execution journal"
	if len(data) > MaxExecutionJournalBytes {
		return ExecutionJournal{}, errInvalid("%s is larger than %d bytes", what, MaxExecutionJournalBytes)
	}
	var w executionWire
	if err := decodeStrict(data, &w, what); err != nil {
		return ExecutionJournal{}, err
	}
	switch {
	case w.SchemaVersion != JournalSchemaVersion:
		return ExecutionJournal{}, errInvalid("%s schema_version %d is not supported (this build supports %d)", what, w.SchemaVersion, JournalSchemaVersion)
	case !ValidTaskID(w.TaskID):
		return ExecutionJournal{}, errInvalid("%s task_id is not a task ID", what)
	case !ValidDigest(w.StartDigest):
		return ExecutionJournal{}, errInvalid("%s start_digest must be 64 lowercase hex digits", what)
	case w.TimeoutPolicy != TimeoutPolicyLegacy:
		return ExecutionJournal{}, errInvalid("%s timeout_policy must be %q", what, TimeoutPolicyLegacy)
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
		want := OutcomeNatural
		if w.Phase == JournalLost {
			want = OutcomeLost
		}
		if w.Result == nil || w.Result.Outcome != want {
			return ExecutionJournal{}, errInvalid("%s: a %s execution holds a %s result", what, w.Phase, want)
		}
		if w.Result.TaskID != w.TaskID || w.Result.Execution != w.Execution {
			return ExecutionJournal{}, errInvalid("%s result names another execution", what)
		}
		if err := w.Result.Validate(); err != nil {
			return ExecutionJournal{}, err
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
		TimeoutPolicy: w.TimeoutPolicy, OwnerNonce: w.OwnerNonce, Phase: w.Phase, Result: w.Result, Log: lg}
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
	o.SchemaVersion = JournalSchemaVersion
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
	case o.SchemaVersion != JournalSchemaVersion:
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
}

// Validate checks a status message's type-specific shape.
func (s GuardianStatus) Validate() error {
	const what = "guardian status"
	if !ValidTaskID(s.TaskID) || !ValidNonce(s.Nonce) {
		return errInvalid("%s must name a task and its nonce", what)
	}
	switch s.Type {
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
		return errInvalid("%s type must be ready, started, exit or error", what)
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
}

// EncodeGuardianCommand renders one command line (JSON and LF) of at most
// MaxGuardianCommandBytes.
func EncodeGuardianCommand(c GuardianCommand) ([]byte, error) {
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
