package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Workspace execution and durable results (iteration 10b, protocol 6). A
// workspace task binds an immutable workspace instance and base commit at
// admission; its worker prepares a private checkout before its adapter is
// released (task_prepared), and publishes the remaining changes as a
// create-once task ref through a plane-owned publication transaction. Every
// value here is a name, token, full hash, mode, size or fixed code: no file
// contents, patches, commit messages or library diagnostics.
const (
	// FrameTaskPrepared (sidecar request, b sequence) reports a workspace
	// start's preparation outcome; FrameTaskPreparedAck is the plane's
	// release decision.
	FrameTaskPrepared    = "task_prepared"
	FrameTaskPreparedAck = "task_prepared_ack"
	// MaxTaskPreparedBody bounds both preparation frames.
	MaxTaskPreparedBody = 8 << 10

	// WorkspacePrepareTimeout bounds a workspace start's preparation from
	// the plane's receipt of its preparing acknowledgement (and the
	// worker's own preparation context); WorkspaceFinalizeTimeout bounds
	// the worker's local snapshot and Begin after confirmed group absence;
	// PublicationExpiry bounds an authorized publication intent from its
	// durable authorization.
	WorkspacePrepareTimeout  = 5 * time.Minute
	WorkspaceFinalizeTimeout = 5 * time.Minute
	PublicationExpiry        = 5 * time.Minute

	// PathNodeWorkspaces starts the node transfer namespace:
	// /api/v1/node-workspaces/<task_id>.git/{info/refs,git-upload-pack,git-receive-pack}.
	PathNodeWorkspaces = "/api/v1/node-workspaces/"

	// Node transfer and publication headers. These are correctness checks
	// against the durable task, never credentials.
	NodeIDHeader              = "X-Callsheet-Node-ID"
	ExecutionEpochHeader      = "X-Callsheet-Execution-Epoch"
	ExecutionAttachmentHeader = "X-Callsheet-Execution-Attachment"
	StartDigestHeader         = "X-Callsheet-Start-Digest"
	PublicationIDHeader       = "X-Callsheet-Publication-ID"

	// PublicationSuffix follows /api/v1/tasks/<id> for the publication
	// endpoints; FinishSuffix follows its publication ID.
	PublicationSuffix = "/workspace-publication"
	FinishSuffix      = "/finish"

	// MaxPublicationBeginBody bounds a Begin request; MaxPublicationReply a
	// Begin, Finish or observation reply.
	MaxPublicationBeginBody = 640 << 10
	MaxPublicationReply     = MaxTaskResultBody + 8<<10
	MaxPublicationFinish    = 1 << 10

	// MaxWorkspaceChanges and MaxWorkspaceDTOBytes bound a result's
	// workspace DTO: at most 100 change rows and 32 KiB of canonical
	// compact JSON for the whole DTO.
	MaxWorkspaceChanges  = 100
	MaxWorkspaceDTOBytes = 32 << 10

	// TaskResultCommitName and TaskResultCommitEmail are a result commit's
	// fixed author and committer identity.
	TaskResultCommitName  = "Callsheet"
	TaskResultCommitEmail = "workspace@callsheet.invalid"
)

// Preparation refusal reasons (strict start/refusal enums).
const (
	ReasonWorkspaceUnavailable     = "workspace_unavailable"
	ReasonWorkspaceBaseUnavailable = "workspace_base_unavailable"
	ReasonWorkspaceCheckoutFailed  = "workspace_checkout_failed"
	ReasonWorkspacePrepareTimeout  = "workspace_prepare_timeout"
)

// Node transfer refusal reasons (409 conflict).
const (
	ReasonTaskAssignmentMismatch    = "task_assignment_mismatch"
	ReasonWorkspaceInstanceMismatch = "workspace_instance_mismatch"
	ReasonTaskPublicationClosed     = "task_publication_closed"
	ReasonTaskRefExists             = "task_ref_exists"
)

// Closed publication error enum.
const (
	PubErrWorkspaceUnavailable     = "workspace_unavailable"
	PubErrInstanceMismatch         = "workspace_instance_mismatch"
	PubErrBaseUnavailable          = "workspace_base_unavailable"
	PubErrUnsupportedRepository    = "unsupported_repository"
	PubErrSnapshotFailed           = "workspace_snapshot_failed"
	PubErrMetadataOverflow         = "workspace_metadata_overflow"
	PubErrStorageFailed            = "workspace_storage_failed"
	PubErrTaskRefExists            = "task_ref_exists"
	PubErrAssignmentMismatch       = "task_assignment_mismatch"
	PubErrPublicationClosed        = "task_publication_closed"
	PubErrPublicationTimeout       = "publication_timeout"
	PubErrPublicationInterrupted   = "publication_interrupted"
	PubErrPublicationTransportFail = "publication_transport_failed"
)

// PublicationErrors is the closed publication error enum, in order.
var PublicationErrors = []string{
	PubErrWorkspaceUnavailable, PubErrInstanceMismatch, PubErrBaseUnavailable, PubErrUnsupportedRepository, PubErrSnapshotFailed,
	PubErrMetadataOverflow, PubErrStorageFailed, PubErrTaskRefExists, PubErrAssignmentMismatch, PubErrPublicationClosed,
	PubErrPublicationTimeout, PubErrPublicationInterrupted, PubErrPublicationTransportFail,
}

// ValidPublicationError reports a member of the closed publication enum.
func ValidPublicationError(s string) bool {
	for _, e := range PublicationErrors {
		if e == s {
			return true
		}
	}
	return false
}

// Workspace publication statuses of a result's workspace DTO.
const (
	PublicationPublished     = "published"
	PublicationFailed        = "failed"
	PublicationNotStarted    = "not_started"
	PublicationNotApplicable = "not_applicable"
)

// Public workspace phases of a nonterminal workspace task.
const (
	WorkspacePhasePreparing  = "preparing"
	WorkspacePhaseExecuting  = "executing"
	WorkspacePhasePublishing = "publishing"
)

// Publication intent phases (plane records and observation replies).
const (
	PubPhaseNone       = "none"
	PubPhaseAuthorized = "authorized"
	PubPhaseSettled    = "settled"
)

// ---- Dispatch selection ----

// WorkspaceBinding is a task's immutable workspace binding, resolved once
// at admission: the workspace name and instance, the canonical base
// selector and the base commit (null for the explicit empty base). Later
// branch movement never changes it.
type WorkspaceBinding struct {
	Name         string  `json:"name"`
	Instance     string  `json:"instance"`
	BaseSelector string  `json:"base_selector"`
	BaseCommit   *string `json:"base_commit"`
}

// Validate checks the binding's grammar and its selector/commit pairing.
func (b WorkspaceBinding) Validate() error {
	const what = "workspace binding"
	switch {
	case !ValidWorkspaceName(b.Name):
		return errInvalid("%s name is not a workspace name", what)
	case !ValidWorkspaceToken(b.Instance):
		return errInvalid("%s instance must be a 32-hex token", what)
	case b.BaseCommit != nil && !ValidCommitHash(*b.BaseCommit):
		return errInvalid("%s base_commit must be null or a full 40-hex hash", what)
	}
	sel, err := ParseSelector(b.BaseSelector, "base_selector", true)
	if err != nil || (sel.Kind == SelectorKindBranch && sel.Value != b.BaseSelector) {
		return errInvalid("%s base_selector must be canonical", what)
	}
	switch {
	case sel.Kind == SelectorKindEmpty && b.BaseCommit != nil, sel.Kind != SelectorKindEmpty && b.BaseCommit == nil:
		return errInvalid("%s base_commit is null exactly for the empty base", what)
	case sel.Kind == SelectorKindHash && *b.BaseCommit != sel.Value:
		return errInvalid("%s base_commit must equal its hash selector", what)
	}
	return nil
}

func (b *WorkspaceBinding) unmarshalStrict(raw json.RawMessage, what string) error {
	type plain WorkspaceBinding
	var p plain
	if err := decodeStrict(raw, &p, what); err != nil {
		return err
	}
	if err := WorkspaceBinding(p).Validate(); err != nil {
		return err
	}
	*b = WorkspaceBinding(p)
	return nil
}

// Empty reports the explicit empty base.
func (b WorkspaceBinding) Empty() bool { return b.BaseSelector == SelectorEmpty }

// ParseTaskBase parses a dispatch's base selector: omitted (nil) selects
// refs/heads/main; "empty" the empty tree; a canonical bare task ID its
// task ref; otherwise ParseSelector's full hash, short or full branch or
// full task ref. No trimming, case repair or expressions.
func ParseTaskBase(base *string) (Selector, error) {
	if base == nil {
		return Selector{Kind: SelectorKindBranch, Value: DefaultBranchRef}, nil
	}
	s := *base
	if ValidTaskID(s) {
		return Selector{Kind: SelectorKindTask, Value: TaskRefPrefix + s}, nil
	}
	sel, err := ParseSelector(s, "base", true)
	if err != nil {
		return Selector{}, TaskError(CodeInvalidArgument, "base", "", "invalid base: want empty, a full 40-hex commit hash, a branch (short or refs/heads/...), a complete task ref or a task ID")
	}
	return sel, nil
}

// SelectorString renders a parsed selector canonically (the binding's
// base_selector).
func SelectorString(s Selector) string {
	if s.Kind == SelectorKindEmpty {
		return SelectorEmpty
	}
	return s.Value
}

// checkWorkspaceFields validates a dispatch's optional workspace
// selection: an omitted workspace means none; a present one is a valid
// name; base and workspace_instance require it.
func checkWorkspaceFields(ws, base, inst *string) error {
	if ws == nil {
		switch {
		case base != nil:
			return TaskError(CodeInvalidArgument, "base", "", "base requires workspace; nothing was dispatched")
		case inst != nil:
			return TaskError(CodeInvalidArgument, "workspace_instance", "", "workspace_instance requires workspace; nothing was dispatched")
		}
		return nil
	}
	if !ValidWorkspaceName(*ws) {
		return TaskError(CodeInvalidArgument, "workspace", "", "invalid workspace name; want a 1-63 character slug of lowercase letters, digits and internal hyphens")
	}
	if inst != nil && !ValidWorkspaceToken(*inst) {
		return TaskError(CodeInvalidArgument, "workspace_instance", "", "workspace_instance must be the workspace's 32-hex instance token (see workspace show)")
	}
	if _, err := ParseTaskBase(base); err != nil {
		return err
	}
	return nil
}

// ---- Preparation frames ----

// TaskPreparedBody is a workspace start's preparation outcome: ok=true
// (error null) means a private checkout and an armed guardian exist with
// the adapter release barrier still closed; ok=false carries a bounded
// error with a fixed preparation reason (definitely no adapter).
type TaskPreparedBody struct {
	TaskID      string         `json:"task_id"`
	Execution   ExecutionToken `json:"execution"`
	StartDigest string         `json:"start_digest"`
	OK          bool           `json:"ok"`
	Error       *Error         `json:"-"`
}

// MarshalJSON renders {task_id,execution,start_digest,ok,error}.
func (b TaskPreparedBody) MarshalJSON() ([]byte, error) {
	type wire struct {
		TaskID      string          `json:"task_id"`
		Execution   ExecutionToken  `json:"execution"`
		StartDigest string          `json:"start_digest"`
		OK          bool            `json:"ok"`
		Error       json.RawMessage `json:"error"`
	}
	w := wire{TaskID: b.TaskID, Execution: b.Execution, StartDigest: b.StartDigest, OK: b.OK, Error: json.RawMessage("null")}
	if b.Error != nil {
		e, err := b.Error.MarshalJSON()
		if err != nil {
			return nil, err
		}
		var env struct {
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(e, &env); err != nil {
			return nil, err
		}
		w.Error = env.Error
	}
	return compact(w)
}

// PreparationReasons are the fixed reasons of a failed preparation.
var PreparationReasons = []string{ReasonWorkspaceUnavailable, ReasonWorkspaceBaseUnavailable, ReasonWorkspaceCheckoutFailed, ReasonWorkspacePrepareTimeout}

func knownPreparation(r string) bool {
	for _, s := range PreparationReasons {
		if s == r {
			return true
		}
	}
	return false
}

// DecodeTaskPrepared strictly decodes a task_prepared body.
func DecodeTaskPrepared(body json.RawMessage) (TaskPreparedBody, error) {
	const what = "task_prepared body"
	var b TaskPreparedBody
	if len(body) > MaxTaskPreparedBody {
		return b, errInvalid("%s exceeds %d bytes", what, MaxTaskPreparedBody)
	}
	o, err := decodeObject(body, what)
	if err != nil {
		return b, err
	}
	if err := o.only(what, []string{"task_id", "execution", "start_digest", "ok"}, "error"); err != nil {
		return b, err
	}
	raw, ok := o.raw["error"]
	if !ok {
		return b, errInvalid("%s lacks the required field %q", what, "error")
	}
	var w struct {
		TaskID      string         `json:"task_id"`
		Execution   ExecutionToken `json:"execution"`
		StartDigest string         `json:"start_digest"`
		OK          bool           `json:"ok"`
	}
	if err := decodeStrict([]byte(`{"task_id":`+string(o.raw["task_id"])+`,"execution":`+string(o.raw["execution"])+`,"start_digest":`+
		string(o.raw["start_digest"])+`,"ok":`+string(o.raw["ok"])+`}`), &w, what); err != nil {
		return b, err
	}
	switch {
	case !ValidTaskID(w.TaskID):
		return b, errInvalid("%s task_id is not a task ID", what)
	case !ValidDigest(w.StartDigest):
		return b, errInvalid("%s start_digest must be 64 lowercase hex digits", what)
	case w.OK && !isNull(raw):
		return b, errInvalid("%s: ok=true has a null error", what)
	case !w.OK && isNull(raw):
		return b, errInvalid("%s: ok=false carries its error", what)
	}
	b = TaskPreparedBody{TaskID: w.TaskID, Execution: w.Execution, StartDigest: w.StartDigest, OK: w.OK}
	if !w.OK {
		e, err := ParseErrorBody(append(append([]byte(`{"error":`), raw...), '}'))
		if err != nil {
			return b, err
		}
		reason, _ := e.Details["reason"].(string)
		if !validationResultCode(e.Code) || !knownPreparation(reason) || len(e.Message) > MaxReasonMessageBytes {
			return b, errInvalid("%s error must carry an allowed code, a fixed preparation reason and a bounded message", what)
		}
		b.Error = e
	}
	return b, nil
}

// TaskPreparedAckBody is the plane's release decision for one prepared
// execution: release=true only after the start is durably authorized.
type TaskPreparedAckBody struct {
	TaskID    string         `json:"task_id"`
	Execution ExecutionToken `json:"execution"`
	Release   bool           `json:"release"`
}

// DecodeTaskPreparedAck strictly decodes a task_prepared_ack body.
func DecodeTaskPreparedAck(body json.RawMessage) (TaskPreparedAckBody, error) {
	const what = "task_prepared_ack body"
	var b TaskPreparedAckBody
	if len(body) > MaxTaskPreparedBody {
		return b, errInvalid("%s exceeds %d bytes", what, MaxTaskPreparedBody)
	}
	if err := decodeStrict(body, &b, what); err != nil {
		return b, err
	}
	if !ValidTaskID(b.TaskID) {
		return b, errInvalid("%s task_id is not a task ID", what)
	}
	return b, nil
}

// ---- Result workspace DTO ----

// TaskDiffstat is tree-metadata change totals against the admitted base:
// added, modified and deleted blob paths and the sums of their old and
// new blob sizes. Never line counts.
type TaskDiffstat struct {
	Added    int64 `json:"added"`
	Modified int64 `json:"modified"`
	Deleted  int64 `json:"deleted"`
	OldBytes int64 `json:"old_bytes"`
	NewBytes int64 `json:"new_bytes"`
}

func (d TaskDiffstat) validate(what string) error {
	for _, n := range []int64{d.Added, d.Modified, d.Deleted, d.OldBytes, d.NewBytes} {
		if n < 0 || n > MaxSafeInteger {
			return errInvalid("%s diffstat counters must be integers from 0 to %d", what, MaxSafeInteger)
		}
	}
	return nil
}

// TaskWorkspaceResult is a terminal workspace task's publication DTO (all
// keys always present): publication published (commit, full task ref,
// complete totals and bounded sorted changes), failed (a fixed error, no
// hash, ref or metadata), not_started (cancelled before release) or
// not_applicable (lost or rejected). It never carries contents.
type TaskWorkspaceResult struct {
	Name             string            `json:"name"`
	Instance         string            `json:"instance"`
	BaseCommit       *string           `json:"base_commit"`
	Publication      string            `json:"publication"`
	Commit           *string           `json:"commit"`
	Ref              *string           `json:"ref"`
	Error            *string           `json:"error"`
	Diffstat         *TaskDiffstat     `json:"diffstat"`
	Changes          []WorkspaceChange `json:"changes"`
	ChangesTruncated bool              `json:"changes_truncated"`
	NextAfter        *string           `json:"next_after"`
}

// MarshalJSON renders the DTO with an empty (never null) change list.
func (r TaskWorkspaceResult) MarshalJSON() ([]byte, error) {
	type plain TaskWorkspaceResult
	p := plain(r)
	if p.Changes == nil {
		p.Changes = []WorkspaceChange{}
	}
	return compact(p)
}

func (r *TaskWorkspaceResult) unmarshalStrict(raw json.RawMessage, what string) error {
	type plain TaskWorkspaceResult
	var p plain
	if len(raw) > MaxWorkspaceDTOBytes*2 {
		return errInvalid("%s is too large", what)
	}
	if err := decodeStrict(raw, &p, what); err != nil {
		return err
	}
	v := TaskWorkspaceResult(p)
	if err := v.Validate(); err != nil {
		return err
	}
	*r = v
	return nil
}

// NewNotApplicable is the DTO of a lost or rejected task (no child ran or
// its outcome is unconfirmed): nothing is claimed.
func NewNotApplicable(b WorkspaceBinding) TaskWorkspaceResult {
	return TaskWorkspaceResult{Name: b.Name, Instance: b.Instance, BaseCommit: b.BaseCommit, Publication: PublicationNotApplicable, Changes: []WorkspaceChange{}}
}

// NewNotStarted is the DTO of a task cancelled before its adapter's
// release.
func NewNotStarted(b WorkspaceBinding) TaskWorkspaceResult {
	r := NewNotApplicable(b)
	r.Publication = PublicationNotStarted
	return r
}

// NewPublicationFailed is a failed publication's DTO with a fixed error.
func NewPublicationFailed(b WorkspaceBinding, code string) TaskWorkspaceResult {
	r := NewNotApplicable(b)
	r.Publication = PublicationFailed
	r.Error = &code
	return r
}

// Validate checks the DTO's per-publication invariants, sorted unique
// change rows, the truncation marker and the 100-row/32 KiB bounds.
func (r TaskWorkspaceResult) Validate() error {
	const what = "workspace result"
	switch {
	case !ValidWorkspaceName(r.Name) || !ValidWorkspaceToken(r.Instance):
		return errInvalid("%s must name its workspace and instance", what)
	case r.BaseCommit != nil && !ValidCommitHash(*r.BaseCommit):
		return errInvalid("%s base_commit must be null or a full hash", what)
	case len(r.Changes) > MaxWorkspaceChanges:
		return errInvalid("%s has %d change rows; at most %d are allowed", what, len(r.Changes), MaxWorkspaceChanges)
	}
	empty := r.Commit == nil && r.Ref == nil && r.Diffstat == nil && len(r.Changes) == 0 && !r.ChangesTruncated && r.NextAfter == nil
	switch r.Publication {
	case PublicationPublished:
		if r.Commit == nil || !ValidCommitHash(*r.Commit) || r.Ref == nil || r.Error != nil || r.Diffstat == nil {
			return errInvalid("%s: a published result has its commit, task ref and totals and no error", what)
		}
		if !ValidTaskRef(*r.Ref) {
			return errInvalid("%s ref must be a full task ref", what)
		}
		if err := r.Diffstat.validate(what); err != nil {
			return err
		}
	case PublicationFailed:
		if !empty || r.Error == nil || !ValidPublicationError(*r.Error) {
			return errInvalid("%s: a failed publication carries only its fixed error", what)
		}
	case PublicationNotStarted, PublicationNotApplicable:
		if !empty || r.Error != nil {
			return errInvalid("%s: a %s result carries no hash, ref, error or metadata", what, r.Publication)
		}
	default:
		return errInvalid("%s publication must be published, failed, not_started or not_applicable", what)
	}
	var prev []byte
	var last string
	for i, c := range r.Changes {
		p, err := ParsePathCursor(c.PathBase64)
		if err != nil {
			return errInvalid("%s has an invalid path_base64", what)
		}
		if i > 0 && string(p) <= string(prev) {
			return errInvalid("%s changes are not sorted by unique raw path", what)
		}
		if e := c.validate(); e != nil {
			return errInvalid("%s: %s", what, e.Message)
		}
		prev, last = p, c.PathBase64
	}
	switch {
	case !r.ChangesTruncated && r.NextAfter != nil:
		return errInvalid("%s next_after is set only when truncated", what)
	case r.ChangesTruncated && len(r.Changes) == 0 && r.NextAfter != nil, r.ChangesTruncated && len(r.Changes) > 0 && (r.NextAfter == nil || *r.NextAfter != last):
		return errInvalid("%s next_after must be the last emitted path (null when no row fits)", what)
	}
	enc, err := compact(r)
	if err != nil || len(enc) > MaxWorkspaceDTOBytes {
		return errInvalid("%s exceeds %d bytes", what, MaxWorkspaceDTOBytes)
	}
	return nil
}

// ChangeRow is one unbounded changed path of a result's tree metadata.
type ChangeRow struct {
	Path     []byte
	Kind     string
	OldMode  *string
	NewMode  *string
	OldBytes *int64
	NewBytes *int64
}

// BoundChanges fills r's change rows from rows (sorted by raw path bytes)
// up to MaxWorkspaceChanges rows and MaxWorkspaceDTOBytes of the whole
// DTO's canonical compact JSON, stopping before the next row would exceed
// either: truncation sets changes_truncated and next_after (the last
// emitted path, or null when no row fits). Totals are never touched.
func BoundChanges(r *TaskWorkspaceResult, rows []ChangeRow) error {
	r.Changes, r.ChangesTruncated, r.NextAfter = []WorkspaceChange{}, false, nil
	base, err := compact(*r)
	if err != nil {
		return err
	}
	size := len(base)
	for i, row := range rows {
		c := WorkspaceChange{PathBase64: EncodeBase64(row.Path), Kind: row.Kind, OldMode: row.OldMode, NewMode: row.NewMode, OldBytes: row.OldBytes, NewBytes: row.NewBytes}
		b, err := compact(c)
		if err != nil {
			return err
		}
		add := len(b)
		if i > 0 {
			add++
		}
		// Room for changes_truncated=true and next_after naming this row,
		// unless it is the last row (nothing can be truncated after it).
		reserve := 0
		if i < len(rows)-1 {
			reserve = len(`true`) - len(`false`) + len(`"`+c.PathBase64+`"`) - len(`null`)
		}
		if i == MaxWorkspaceChanges || size+add+reserve > MaxWorkspaceDTOBytes {
			r.ChangesTruncated = true
			if len(r.Changes) > 0 {
				last := r.Changes[len(r.Changes)-1].PathBase64
				r.NextAfter = &last
			}
			return nil
		}
		size += add
		r.Changes = append(r.Changes, c)
	}
	return nil
}

// ---- Publication API ----

// PublicationBeginRequest is POST /api/v1/tasks/<id>/workspace-publication:
// the sealed execution outcome candidate (no workspace extension) and the
// result tree.
type PublicationBeginRequest struct {
	Candidate TaskResultBody `json:"candidate"`
	Tree      string         `json:"tree"`
}

// ParsePublicationBegin strictly decodes and validates a Begin request.
func ParsePublicationBegin(b []byte) (PublicationBeginRequest, error) {
	const what = "workspace publication request"
	var r PublicationBeginRequest
	if len(b) > MaxPublicationBeginBody {
		return r, errInvalid("%s exceeds %d bytes", what, MaxPublicationBeginBody)
	}
	o, err := decodeObject(b, what)
	if err != nil {
		return r, err
	}
	if err := o.only(what, []string{"candidate", "tree"}); err != nil {
		return r, err
	}
	if len(o.raw["candidate"]) > MaxTaskResultBody {
		return r, errInvalid("%s candidate exceeds %d bytes", what, MaxTaskResultBody)
	}
	cand, err := DecodeTaskResult(o.raw["candidate"])
	if err != nil {
		return r, err
	}
	if cand.Workspace != nil {
		return r, errInvalid("%s candidate carries no workspace extension", what)
	}
	tree, err := strictString(bytes.TrimSpace(o.raw["tree"]), what+" tree")
	if err != nil || !ValidCommitHash(tree) {
		return r, errInvalid("%s tree must be a full 40-hex tree hash", what)
	}
	return PublicationBeginRequest{Candidate: cand, Tree: tree}, nil
}

// PublicationBeginResponse is an authorized intent: its correlation ID,
// the canonical terminal state its commit carries and its expiry.
type PublicationBeginResponse struct {
	PublicationID string `json:"publication_id"`
	State         string `json:"state"`
	ExpiresAt     string `json:"expires_at"`
}

// ParsePublicationBeginResponse strictly decodes a Begin reply.
func ParsePublicationBeginResponse(b []byte) (PublicationBeginResponse, error) {
	const what = "workspace publication response"
	var r PublicationBeginResponse
	if len(b) > MaxPublicationReply {
		return r, errInvalid("%s exceeds %d bytes", what, MaxPublicationReply)
	}
	if err := decodeStrict(b, &r, what); err != nil {
		return r, err
	}
	switch {
	case !ValidWorkspaceToken(r.PublicationID):
		return r, errInvalid("%s publication_id must be 32 lowercase hex digits", what)
	case !publishableState(r.State):
		return r, errInvalid("%s state must be a publishable terminal state", what)
	case !validTime(r.ExpiresAt):
		return r, errInvalid("%s expires_at must be a UTC RFC3339 timestamp", what)
	}
	return r, nil
}

// publishableState reports a terminal state whose result is published.
func publishableState(s string) bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskCancelled || s == TaskTimedOut
}

// PublishableState reports whether s is a terminal state whose workspace
// result is published (succeeded, failed, cancelled, timed_out).
func PublishableState(s string) bool { return publishableState(s) }

// PublicationFinishRequest reports a definitive push refusal or local
// failure after Begin.
type PublicationFinishRequest struct {
	Error string `json:"error"`
}

// ParsePublicationFinish strictly decodes a Finish request.
func ParsePublicationFinish(b []byte) (PublicationFinishRequest, error) {
	const what = "workspace publication finish request"
	var r PublicationFinishRequest
	if len(b) > MaxPublicationFinish {
		return r, errInvalid("%s exceeds %d bytes", what, MaxPublicationFinish)
	}
	if err := decodeStrict(b, &r, what); err != nil {
		return r, err
	}
	if !ValidPublicationError(r.Error) {
		return r, errInvalid("%s error must be a fixed publication error code", what)
	}
	return r, nil
}

// PublicationStatus is the settlement envelope of the Finish reply and the
// read-only observations: phase (none only without an ID), the settled
// canonical sealed result (null until settled), the task's current state
// and whether its terminal record is durably committed. PublicationID is
// rendered only by the lookup form (WithID).
type PublicationStatus struct {
	PublicationID *string
	WithID        bool
	Phase         string
	Result        *TaskResultBody
	TaskState     string
	Committed     bool
}

// MarshalJSON renders {[publication_id,]phase,result,task_state,committed}.
func (s PublicationStatus) MarshalJSON() ([]byte, error) {
	type byID struct {
		Phase     string          `json:"phase"`
		Result    *TaskResultBody `json:"result"`
		TaskState string          `json:"task_state"`
		Committed bool            `json:"committed"`
	}
	type lookup struct {
		PublicationID *string         `json:"publication_id"`
		Phase         string          `json:"phase"`
		Result        *TaskResultBody `json:"result"`
		TaskState     string          `json:"task_state"`
		Committed     bool            `json:"committed"`
	}
	if s.WithID {
		return compact(lookup{s.PublicationID, s.Phase, s.Result, s.TaskState, s.Committed})
	}
	return compact(byID{s.Phase, s.Result, s.TaskState, s.Committed})
}

// ParsePublicationStatus strictly decodes a settlement envelope: withID
// selects the lookup form.
func ParsePublicationStatus(b []byte, withID bool) (PublicationStatus, error) {
	const what = "workspace publication status"
	var s PublicationStatus
	if len(b) > MaxPublicationReply {
		return s, errInvalid("%s exceeds %d bytes", what, MaxPublicationReply)
	}
	var w struct {
		PublicationID *string         `json:"publication_id,omitempty"`
		Phase         string          `json:"phase"`
		Result        *TaskResultBody `json:"result"`
		TaskState     string          `json:"task_state"`
		Committed     bool            `json:"committed"`
	}
	o, err := decodeObject(b, what)
	if err != nil {
		return s, err
	}
	if _, ok := o.raw["publication_id"]; ok != withID {
		return s, errInvalid("%s publication_id is present exactly in the lookup form", what)
	}
	if err := decodeStrict(b, &w, what); err != nil {
		return s, err
	}
	s = PublicationStatus{PublicationID: w.PublicationID, WithID: withID, Phase: w.Phase, Result: w.Result, TaskState: w.TaskState, Committed: w.Committed}
	switch {
	case !ValidTaskState(s.TaskState):
		return s, errInvalid("%s task_state is not a known state", what)
	case s.PublicationID != nil && !ValidWorkspaceToken(*s.PublicationID):
		return s, errInvalid("%s publication_id must be null or 32 lowercase hex digits", what)
	}
	switch s.Phase {
	case PubPhaseNone:
		if !withID || s.PublicationID != nil || s.Result != nil {
			return s, errInvalid("%s phase none has no publication or result", what)
		}
	case PubPhaseAuthorized:
		if s.Result != nil || (withID && s.PublicationID == nil) {
			return s, errInvalid("%s: an authorized publication has no result yet", what)
		}
	case PubPhaseSettled:
		if s.Result == nil || s.Result.Workspace == nil || (withID && s.PublicationID == nil) {
			return s, errInvalid("%s: a settled publication carries its sealed result with its workspace DTO", what)
		}
	default:
		return s, errInvalid("%s phase must be none, authorized or settled", what)
	}
	if s.Committed && !TaskTerminal(s.TaskState) {
		return s, errInvalid("%s: only a terminal task is committed", what)
	}
	return s, nil
}

// ---- Durable publication intent (task record schema 4) ----

// TaskPublication is a workspace task's durable publication intent and
// settlement: the correlation ID, the canonical terminal state, the result
// tree and expected commit, the bound instance, the expiry, the phase, the
// arbitrated candidate and (settled) the final status and error.
type TaskPublication struct {
	ID        string         `json:"publication_id"`
	State     string         `json:"state"`
	Tree      string         `json:"tree"`
	Commit    string         `json:"commit"`
	Instance  string         `json:"instance"`
	Deadline  time.Time      `json:"-"`
	Phase     string         `json:"phase"`
	Candidate TaskResultBody `json:"candidate"`
	Status    *string        `json:"status"`
	Error     *string        `json:"error"`
}

type taskPublicationWire struct {
	ID        string         `json:"publication_id"`
	State     string         `json:"state"`
	Tree      string         `json:"tree"`
	Commit    string         `json:"commit"`
	Instance  string         `json:"instance"`
	Deadline  string         `json:"deadline"`
	Phase     string         `json:"phase"`
	Candidate TaskResultBody `json:"candidate"`
	Status    *string        `json:"status"`
	Error     *string        `json:"error"`
}

// MarshalJSON renders the intent in its record order.
func (p TaskPublication) MarshalJSON() ([]byte, error) {
	return compact(taskPublicationWire{p.ID, p.State, p.Tree, p.Commit, p.Instance, FormatTime(p.Deadline), p.Phase, p.Candidate, p.Status, p.Error})
}

func (p *TaskPublication) unmarshalStrict(raw json.RawMessage, what string) error {
	var w taskPublicationWire
	if err := decodeStrict(raw, &w, what); err != nil {
		return err
	}
	at, ok := ParseTime(w.Deadline)
	if !ok {
		return errInvalid("%s deadline must be a UTC RFC3339 timestamp", what)
	}
	v := TaskPublication{w.ID, w.State, w.Tree, w.Commit, w.Instance, at, w.Phase, w.Candidate, w.Status, w.Error}
	if err := v.Validate(); err != nil {
		return err
	}
	*p = v
	return nil
}

// Validate checks the intent's grammar and phase invariants.
func (p TaskPublication) Validate() error {
	const what = "publication intent"
	switch {
	case !ValidWorkspaceToken(p.ID) || !ValidWorkspaceToken(p.Instance):
		return errInvalid("%s publication_id and instance must be 32-hex tokens", what)
	case !publishableState(p.State):
		return errInvalid("%s state must be a publishable terminal state", what)
	case !ValidCommitHash(p.Tree) || !ValidCommitHash(p.Commit):
		return errInvalid("%s tree and commit must be full hashes", what)
	case p.Candidate.Workspace != nil:
		return errInvalid("%s candidate carries no workspace extension", what)
	}
	if err := p.Candidate.Validate(); err != nil {
		return err
	}
	switch p.Phase {
	case PubPhaseAuthorized:
		if p.Status != nil || p.Error != nil {
			return errInvalid("%s: an authorized intent has no status yet", what)
		}
	case PubPhaseSettled:
		switch {
		case p.Status == nil || (*p.Status != PublicationPublished && *p.Status != PublicationFailed):
			return errInvalid("%s: a settled intent is published or failed", what)
		case (*p.Status == PublicationFailed) != (p.Error != nil), p.Error != nil && !ValidPublicationError(*p.Error):
			return errInvalid("%s: a failed settlement carries exactly its fixed error", what)
		}
	default:
		return errInvalid("%s phase must be authorized or settled", what)
	}
	return nil
}

// ---- Result digest (protocol 6) ----

// resultDigestDomain separates protocol 6 workspace result digests from the
// protocol 4/5 field sequence.
const resultDigestDomain = "callsheet task result v6 workspace\n"

// workspaceDigestBody is a workspace result's digested fields in wire
// order, stop_id and workspace included.
type workspaceDigestBody struct {
	resultDigestBody
	StopID    *string             `json:"stop_id"`
	Workspace TaskWorkspaceResult `json:"workspace"`
}

func workspaceDigest(b TaskResultBody) string {
	base := resultDigestBody{b.TaskID, b.Execution, b.Outcome, b.ExitCode, b.Signal, b.FinalMessage, b.FinalMessageTruncated,
		b.OutputBytes, b.LogIncomplete, b.CounterOverflow}
	enc, err := compact(workspaceDigestBody{base, b.StopID, *b.Workspace})
	if err != nil {
		return ""
	}
	return HexDigest(sha256.Sum256(append([]byte(resultDigestDomain), enc...)))
}

// ---- Result commit message ----

// TaskResultMessage is a result commit's fixed UTF-8/LF message: every
// interpolated value already satisfies its contract ASCII grammar (never
// child output or a display name).
func TaskResultMessage(taskID, roleID, model, state string) string {
	return "Callsheet task result\ntask: " + taskID + "\nrole: " + roleID + "\nmodel: " + model + "\nstate: " + state + "\n"
}

// ValidMessageValue reports a value that may be interpolated into a result
// commit message: nonempty printable ASCII without control characters.
func ValidMessageValue(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// ---- Public mirrors ----

// mirrorsOf derives a TaskResult's public mirrors of its workspace DTO.
func mirrorsOf(w *TaskWorkspaceResult) (*string, *TaskDiffstat, []string, bool, *string) {
	paths := []string{}
	if w == nil || w.Publication != PublicationPublished {
		return nil, nil, paths, false, nil
	}
	for _, c := range w.Changes {
		paths = append(paths, c.PathBase64)
	}
	commit := *w.Commit
	ds := *w.Diffstat
	var next *string
	if w.NextAfter != nil {
		n := *w.NextAfter
		next = &n
	}
	return &commit, &ds, paths, w.ChangesTruncated, next
}

// WithWorkspace returns r with the workspace DTO w and its derived public
// mirrors (result_commit, diffstat, changed_paths and its truncation).
func (r TaskResult) WithWorkspace(w *TaskWorkspaceResult) TaskResult {
	r.Workspace = w
	r.ResultCommit, r.Diffstat, r.ChangedPaths, r.ChangedPathsTruncated, r.ChangedPathsNextAfter = mirrorsOf(w)
	return r
}

// checkMirrors rejects mirrors inconsistent with the workspace DTO.
func (r TaskResult) checkMirrors(what string) error {
	c, d, p, t, n := mirrorsOf(r.Workspace)
	eqStr := func(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	eqDS := func(a, b *TaskDiffstat) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	if !eqStr(c, r.ResultCommit) || !eqDS(d, r.Diffstat) || len(p) != len(r.ChangedPaths) || t != r.ChangedPathsTruncated || !eqStr(n, r.ChangedPathsNextAfter) {
		return errInvalid("%s result mirrors are inconsistent with its workspace result", what)
	}
	for i := range p {
		if p[i] != r.ChangedPaths[i] {
			return errInvalid("%s result changed_paths are inconsistent with its workspace result", what)
		}
	}
	if r.Workspace != nil {
		return r.Workspace.Validate()
	}
	return nil
}

// sameHash reports equal nullable hashes.
func sameHash(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// WorkspacePhaseOf derives a task's public workspace phase.
func WorkspacePhaseOf(b *WorkspaceBinding, state string, publishing bool) *string {
	if b == nil || TaskTerminal(state) {
		return nil
	}
	p := WorkspacePhasePreparing
	switch {
	case publishing:
		p = WorkspacePhasePublishing
	case state == TaskRunning:
		p = WorkspacePhaseExecuting
	}
	return &p
}

// ---- Node transfer helpers ----

// NodeAssignment is a node transfer's claimed assignment, parsed from its
// headers (checked against the durable task, never trusted).
type NodeAssignment struct {
	NodeID      string
	Execution   ExecutionToken
	StartDigest string
	Instance    string
}

// Headers renders the assignment's transfer headers.
func (a NodeAssignment) Headers() map[string]string {
	return map[string]string{NodeIDHeader: a.NodeID, ExecutionEpochHeader: a.Execution.Epoch,
		ExecutionAttachmentHeader: strconv.Itoa(a.Execution.Attachment), StartDigestHeader: a.StartDigest, WorkspaceInstanceHeader: a.Instance}
}

// ParseNodeAssignment strictly parses the assignment headers: each exactly
// once and valid (header values are given as Values per name).
func ParseNodeAssignment(values func(string) []string) (NodeAssignment, error) {
	one := func(h string) (string, bool) {
		v := values(h)
		if len(v) != 1 {
			return "", false
		}
		return v[0], true
	}
	var a NodeAssignment
	var ok bool
	bad := func(h string) error {
		return TaskError(CodeInvalidArgument, "", "", "the request needs exactly one valid %s header", h)
	}
	if a.NodeID, ok = one(NodeIDHeader); !ok || !ValidNodeID(a.NodeID) {
		return a, bad(NodeIDHeader)
	}
	if a.Execution.Epoch, ok = one(ExecutionEpochHeader); !ok || !ValidEpoch(a.Execution.Epoch) {
		return a, bad(ExecutionEpochHeader)
	}
	att, ok := one(ExecutionAttachmentHeader)
	n, valid := ParseInteger(att)
	if !ok || !valid || n < 1 || n > MaxSafeInteger || strconv.Itoa(n) != att {
		return a, bad(ExecutionAttachmentHeader)
	}
	a.Execution.Attachment = n
	if a.StartDigest, ok = one(StartDigestHeader); !ok || !ValidDigest(a.StartDigest) {
		return a, bad(StartDigestHeader)
	}
	if a.Instance, ok = one(WorkspaceInstanceHeader); !ok || !ValidWorkspaceToken(a.Instance) {
		return a, bad(WorkspaceInstanceHeader)
	}
	return a, nil
}

// NodeWorkspacePath is the node transfer route prefix of a task:
// /api/v1/node-workspaces/<task_id>.git/
func NodeWorkspacePath(taskID string) string { return PathNodeWorkspaces + taskID + ".git/" }

// PublicationPath is a task's publication endpoint, with an optional
// publication ID and the finish suffix.
func PublicationPath(taskID, pubID string, finish bool) string {
	p := PathTasks + "/" + taskID + PublicationSuffix
	if pubID != "" {
		p += "/" + pubID
		if finish {
			p += FinishSuffix
		}
	}
	return p
}

// SplitPublicationPath parses the remainder after /api/v1/tasks/<id>:
// "/workspace-publication" (pub ""), ".../<pub>" or ".../<pub>/finish".
func SplitPublicationPath(rest string) (pub string, finish, ok bool) {
	r, found := strings.CutPrefix(rest, PublicationSuffix)
	if !found {
		return "", false, false
	}
	if r == "" {
		return "", false, true
	}
	r, found = strings.CutPrefix(r, "/")
	if !found {
		return "", false, false
	}
	pub, tail, _ := strings.Cut(r, "/")
	switch {
	case !ValidWorkspaceToken(pub):
		return "", false, false
	case tail == "":
		return pub, false, !strings.HasSuffix(r, "/")
	case tail == strings.TrimPrefix(FinishSuffix, "/"):
		return pub, true, true
	}
	return "", false, false
}

// EncodePath renders raw path bytes as path_base64.
func EncodePath(p []byte) string { return base64.StdEncoding.EncodeToString(p) }

// ---- Coordinator delivery (iteration 10c) ----

// Task-result door errors of the coordinator's task-ID forms (ws pull,
// ws status and ws diff TASK_ID): a bound task without a terminal result
// yet (conflict), a terminal task without a published, still exactly
// present result (conflict), and a task without a workspace binding
// (invalid_argument). The landed ReasonWorkspaceInstanceMismatch reports a
// removed or recreated bound workspace.
const (
	ReasonTaskResultPending     = "task_result_pending"
	ReasonTaskResultUnavailable = "task_result_unavailable"
	ReasonNoTaskWorkspace       = "no_task_workspace"
)

// TaskWorkspaceSuffix follows /api/v1/tasks/<id> for the task workspace
// status endpoint (matched exactly, never by prefix).
const TaskWorkspaceSuffix = "/workspace"

// TaskWorkspacePath is GET /api/v1/tasks/<id>/workspace.
func TaskWorkspacePath(taskID string) string { return PathTasks + "/" + taskID + TaskWorkspaceSuffix }

// ParseTaskSelector parses a selector at a task-aware door: a canonical
// bare task ID selects its full task ref; everything else is
// ParseSelector's grammar (an actual branch named like a task ID is
// refs/heads/TASK_ID).
func ParseTaskSelector(s, field string, allowEmpty bool) (Selector, error) {
	if ValidTaskID(s) {
		return Selector{Kind: SelectorKindTask, Value: TaskRefPrefix + s}, nil
	}
	return ParseSelector(s, field, allowEmpty)
}

// NoTaskWorkspace is the error of a task-ID form naming a task without a
// workspace binding.
func NoTaskWorkspace(id string) error {
	return TaskError(CodeInvalidArgument, "task_id", ReasonNoTaskWorkspace, "task %s has no workspace (no_task_workspace): a scratch task has no result commit", id)
}

// TaskResultPending is the error of a bound task that is not terminal yet.
func TaskResultPending(id string) error {
	return TaskError(CodeConflict, "task_id", ReasonTaskResultPending, "task %s has no workspace result yet (task_result_pending); wait for it to end (task wait) and retry", id)
}

// TaskResultUnavailable is the error of a terminal task whose result is not
// published, or whose task ref names another commit.
func TaskResultUnavailable(id, why string) error {
	return TaskError(CodeConflict, "task_id", ReasonTaskResultUnavailable, "task %s has no available workspace result (task_result_unavailable): %s", id, why)
}

// WorkspaceInstanceMismatch is the error of a task-ID form whose bound
// workspace was removed or recreated: never a fallback to a new workspace
// of the same name.
func WorkspaceInstanceMismatch(name string) error {
	return TaskError(CodeConflict, "", ReasonWorkspaceInstanceMismatch,
		"workspace %s was removed or recreated since the task's admission (workspace_instance_mismatch); its result is not in the current workspace", name)
}

// TaskWorkspaceStatus is a task's workspace status (the payload of GET
// /api/v1/tasks/<id>/workspace): its state and nonterminal phase, the
// immutable binding, the bounded terminal result (null while nonterminal)
// and whether the current matching instance holds the exact recorded task
// ref and hash, a fresh observation and not a retention lease. It never
// carries file contents, patches, commit messages or task output.
type TaskWorkspaceStatus struct {
	TaskID         string               `json:"task_id"`
	State          string               `json:"state"`
	WorkspacePhase *string              `json:"workspace_phase"`
	Binding        WorkspaceBinding     `json:"binding"`
	Result         *TaskWorkspaceResult `json:"result"`
	Available      bool                 `json:"available"`
}

// Validate checks the payload's grammar and its state, phase, binding,
// result and availability consistency.
func (s TaskWorkspaceStatus) Validate() error {
	const what = "task workspace status"
	switch {
	case !ValidTaskID(s.TaskID):
		return errInvalid("%s task_id is not a task ID", what)
	case !ValidTaskState(s.State):
		return errInvalid("%s state is not a known state", what)
	}
	if err := s.Binding.Validate(); err != nil {
		return err
	}
	term := TaskTerminal(s.State)
	switch p := s.WorkspacePhase; {
	case term && p != nil, !term && p == nil:
		return errInvalid("%s workspace_phase is null exactly when terminal", what)
	case p != nil && *p != WorkspacePhasePreparing && *p != WorkspacePhaseExecuting && *p != WorkspacePhasePublishing:
		return errInvalid("%s workspace_phase must be preparing, executing or publishing", what)
	case term != (s.Result != nil):
		return errInvalid("%s result is present exactly when terminal", what)
	}
	if r := s.Result; r != nil {
		if err := r.Validate(); err != nil {
			return err
		}
		switch {
		case r.Name != s.Binding.Name || r.Instance != s.Binding.Instance || !sameHash(r.BaseCommit, s.Binding.BaseCommit):
			return errInvalid("%s result does not match the task's binding", what)
		case r.Ref != nil && *r.Ref != TaskRefPrefix+s.TaskID:
			return errInvalid("%s result ref is not the task's own ref", what)
		}
	}
	if s.Available && (s.Result == nil || s.Result.Publication != PublicationPublished) {
		return errInvalid("%s: only a published result is available", what)
	}
	return nil
}

// TaskWorkspaceStatusResponse is the versioned task workspace status
// envelope {"version":6,"workspace":{...}}, shared by the HTTP endpoint,
// CLI JSON and MCP structuredContent.
type TaskWorkspaceStatusResponse struct {
	Version   int
	Workspace TaskWorkspaceStatus
}

// MarshalJSON renders the envelope with the protocol version and every
// payload key, nulls included.
func (r TaskWorkspaceStatusResponse) MarshalJSON() ([]byte, error) {
	type wire struct {
		Version   int                 `json:"version"`
		Workspace TaskWorkspaceStatus `json:"workspace"`
	}
	return compact(wire{ProtocolVersion, r.Workspace})
}

// ParseTaskWorkspaceStatusResponse strictly decodes the envelope: its
// MaxWorkspaceResponse bound, the protocol version, exact keys (no unknown,
// missing, null or duplicate member) and the payload's consistency.
func ParseTaskWorkspaceStatusResponse(b []byte) (TaskWorkspaceStatusResponse, error) {
	const what = "task workspace status response"
	var r TaskWorkspaceStatusResponse
	if len(b) > MaxWorkspaceResponse {
		return r, errInvalid("%s exceeds %d bytes", what, MaxWorkspaceResponse)
	}
	var w struct {
		Version   int             `json:"version"`
		Workspace json.RawMessage `json:"workspace"`
	}
	if err := decodeStrict(b, &w, what); err != nil {
		return r, err
	}
	if w.Version != ProtocolVersion {
		return r, errInvalid("%s version must be %d", what, ProtocolVersion)
	}
	var s TaskWorkspaceStatus
	if err := decodeStrict(w.Workspace, &s, what+" workspace"); err != nil {
		return r, err
	}
	if err := s.Validate(); err != nil {
		return r, err
	}
	return TaskWorkspaceStatusResponse{Version: w.Version, Workspace: s}, nil
}
