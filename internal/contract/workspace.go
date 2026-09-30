package contract

import (
	"encoding/base64"
	"strings"
	"time"
	"unicode/utf8"
)

// Workspace hub contracts (iteration 09a): the plane is a durable, named
// git hub. The control API lives under PathWorkspaces; the smart-HTTP git
// transport under WorkspaceGitPrefix. Responses carry no commit messages,
// author names, patches, blob bytes, repository configuration or arbitrary
// error text: only names, tokens, full hashes, timestamps, modes and sizes.
const (
	// PathWorkspaces is the workspace collection; one workspace is
	// PathWorkspaces + "/" + name.
	PathWorkspaces = "/api/v1/workspaces"
	// WorkspaceGitPrefix starts every smart-HTTP route: /ws/<name>.git/...
	WorkspaceGitPrefix = "/ws/"
	// WorkspaceInstanceHeader carries the workspace instance on a
	// receive-pack request (the value workspace show returns).
	WorkspaceInstanceHeader = "X-Callsheet-Workspace-Instance"

	// MaxWorkspaceBody bounds every workspace JSON request body.
	MaxWorkspaceBody = 64 << 10
	// MaxWorkspaceResponse bounds every encoded workspace success object;
	// a page stops before its next row would cross it.
	MaxWorkspaceResponse = 1 << 20
	// DefaultWorkspaceLimit and MaxWorkspaceLimit bound list, status and
	// diff pages.
	DefaultWorkspaceLimit = 100
	MaxWorkspaceLimit     = 100

	// DefaultBranchRef is every workspace's default branch, whether or not
	// it exists (HEAD is never retargeted).
	DefaultBranchRef = "refs/heads/main"
	// BranchPrefix and TaskRefPrefix are the only ref namespaces.
	BranchPrefix  = "refs/heads/"
	TaskRefPrefix = "refs/callsheet/tasks/"

	// MaxBranchComponent bounds one slash-separated branch component.
	MaxBranchComponent = 200
	// MaxFullRef bounds a canonical full branch ref, its prefix included.
	MaxFullRef = 512

	// SelectorEmpty is the empty-tree diff base; ExpectedAbsent is the
	// ref-set CAS value of a ref that must not exist.
	SelectorEmpty  = "empty"
	ExpectedAbsent = "absent"

	// Diff row kinds.
	ChangeAdded    = "added"
	ChangeDeleted  = "deleted"
	ChangeModified = "modified"
)

// WorkspaceRetention is the fixed retention policy of this release: no
// automatic pruning and no quota.
type WorkspaceRetention struct {
	AutomaticPrune bool   `json:"automatic_prune"`
	QuotaBytes     *int64 `json:"quota_bytes"`
}

// WorkspaceSummary is one list row.
type WorkspaceSummary struct {
	Name      string `json:"name"`
	Instance  string `json:"instance"`
	CreatedAt string `json:"created_at"`
}

// WorkspaceView is a workspace's identity, current generation, default
// branch, owned storage size and retention.
type WorkspaceView struct {
	Name          string             `json:"name"`
	Instance      string             `json:"instance"`
	CreatedAt     string             `json:"created_at"`
	Generation    string             `json:"generation"`
	DefaultBranch string             `json:"default_branch"`
	SizeBytes     int64              `json:"size_bytes"`
	Retention     WorkspaceRetention `json:"retention"`
}

// WorkspaceRef is one stored ref: a branch (published_at null) or a task
// ref with its plane publication time.
type WorkspaceRef struct {
	Name        string  `json:"name"`
	Commit      string  `json:"commit"`
	PublishedAt *string `json:"published_at"`
}

// WorkspaceChange is one changed path of a diff: the raw path bytes as
// standard base64, never file contents.
type WorkspaceChange struct {
	PathBase64 string  `json:"path_base64"`
	Kind       string  `json:"kind"`
	OldMode    *string `json:"old_mode"`
	NewMode    *string `json:"new_mode"`
	OldBytes   *int64  `json:"old_bytes"`
	NewBytes   *int64  `json:"new_bytes"`
}

// Requests.
type (
	WorkspaceCreateRequest struct {
		Name string `json:"name"`
	}
	WorkspaceRemoveRequest struct {
		Instance string `json:"instance"`
	}
	WorkspacePruneRequest struct {
		Instance string `json:"instance"`
		Before   string `json:"before"`
	}
	// WorkspaceRefSetRequest is ref set's body (the name is in the path).
	WorkspaceRefSetRequest struct {
		Instance string  `json:"instance"`
		Branch   string  `json:"branch"`
		Expected string  `json:"expected"`
		Target   *string `json:"target,omitempty"`
		Delete   *bool   `json:"delete,omitempty"`
	}
)

// Responses.
type (
	WorkspaceListResponse struct {
		Workspaces []WorkspaceSummary `json:"workspaces"`
		NextAfter  *string            `json:"next_after"`
	}
	WorkspaceRemoveResponse struct {
		Name     string `json:"name"`
		Instance string `json:"instance"`
		Removed  bool   `json:"removed"`
	}
	WorkspacePruneResponse struct {
		Name            string `json:"name"`
		Instance        string `json:"instance"`
		Generation      string `json:"generation"`
		RemovedTaskRefs int64  `json:"removed_task_refs"`
		ReclaimedBytes  int64  `json:"reclaimed_bytes"`
		SizeBytes       int64  `json:"size_bytes"`
	}
	WorkspaceRefSetResponse struct {
		Name       string  `json:"name"`
		Instance   string  `json:"instance"`
		Generation string  `json:"generation"`
		Ref        string  `json:"ref"`
		OldCommit  *string `json:"old_commit"`
		NewCommit  *string `json:"new_commit"`
	}
	WorkspaceStatusResponse struct {
		Name          string         `json:"name"`
		Instance      string         `json:"instance"`
		Generation    string         `json:"generation"`
		DefaultBranch string         `json:"default_branch"`
		Refs          []WorkspaceRef `json:"refs"`
		NextAfter     *string        `json:"next_after"`
	}
	WorkspaceDiffResponse struct {
		Name         string            `json:"name"`
		Instance     string            `json:"instance"`
		Generation   string            `json:"generation"`
		BaseCommit   *string           `json:"base_commit"`
		TargetCommit string            `json:"target_commit"`
		Changes      []WorkspaceChange `json:"changes"`
		NextAfter    *string           `json:"next_after"`
	}
)

// NewRetention is the only retention value of this release.
func NewRetention() WorkspaceRetention { return WorkspaceRetention{} }

// ---- Grammars ----

// ValidWorkspaceName is the slug grammar: 1-63 lowercase ASCII letters,
// digits and internal hyphens.
func ValidWorkspaceName(s string) bool { return ValidSlug(s) }

// InvalidWorkspaceName is the invalid_argument for a malformed name.
func InvalidWorkspaceName() *Error {
	return errInvalid("invalid workspace name; want a 1-63 character slug of lowercase letters, digits and internal hyphens")
}

func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ValidWorkspaceToken reports a 128-bit lowercase hex instance or
// generation token.
func ValidWorkspaceToken(s string) bool { return lowerHex(s, 32) }

// ValidCommitHash reports a full 40-character lowercase SHA-1.
func ValidCommitHash(s string) bool { return lowerHex(s, 40) }

// validComponent is one portable branch component: [a-z0-9][a-z0-9._-]*,
// 1-200 bytes, no "..", no ".lock" suffix.
func validComponent(c string) bool {
	if len(c) == 0 || len(c) > MaxBranchComponent {
		return false
	}
	for i := 0; i < len(c); i++ {
		b := c[i]
		switch {
		case b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		case i > 0 && (b == '.' || b == '_' || b == '-'):
		default:
			return false
		}
	}
	return !strings.Contains(c, "..") && !strings.HasSuffix(c, ".lock")
}

// ValidBranchRef reports whether full is a canonical portable branch ref:
// refs/heads/ followed by one or more portable components, at most 512
// bytes, valid UTF-8 (thus ASCII) and not ending in ".".
func ValidBranchRef(full string) bool {
	if len(full) > MaxFullRef || !utf8.ValidString(full) || strings.HasSuffix(full, ".") {
		return false
	}
	short, ok := strings.CutPrefix(full, BranchPrefix)
	if !ok || short == "" {
		return false
	}
	for _, c := range strings.Split(short, "/") {
		if !validComponent(c) {
			return false
		}
	}
	return true
}

// ValidTaskRef reports refs/callsheet/tasks/<task ID>.
func ValidTaskRef(full string) bool {
	id, ok := strings.CutPrefix(full, TaskRefPrefix)
	return ok && ValidTaskID(id)
}

// invalidBranch is the invalid_argument for a nonportable branch name. It
// never echoes the input.
func invalidBranch(field string) *Error {
	return errInvalid("invalid %s: a branch is refs/heads/ or a short name of slash-separated components, each 1-%d bytes of lowercase ASCII letters, digits, '.', '_' and '-' (starting with a letter or digit, no '..', no .lock suffix, not ending in '.'), at most %d bytes as a full ref; names are case- and byte-exact and never repaired",
		field, MaxBranchComponent, MaxFullRef)
}

// NormalizeBranch returns the canonical full ref of a short or full branch
// name under the portable branch-name contract. Adding refs/heads/ is the
// only normalization: nothing is lowercased, Unicode-normalized, trimmed
// or truncated. An input that starts with "refs/" is full and must be in
// refs/heads/.
func NormalizeBranch(s, field string) (string, error) {
	full := s
	if !strings.HasPrefix(s, "refs/") {
		full = BranchPrefix + s
	}
	if !ValidBranchRef(full) {
		return "", invalidBranch(field)
	}
	return full, nil
}

// Selector kinds.
const (
	SelectorKindHash   = "hash"
	SelectorKindBranch = "branch"
	SelectorKindTask   = "task"
	SelectorKindEmpty  = "empty"
)

// Selector is a parsed commit selector: a full hash, a branch ref, a task
// ref or (diff base only) the empty tree.
type Selector struct {
	Kind  string
	Value string
}

// ParseSelector parses a target/base selector: a full 40-hex commit hash
// (a 40-hex string always means a hash; such a branch is selected by its
// full ref), a short or full branch, a complete task ref, or, when
// allowEmpty, the literal "empty". No revision expressions, short hashes,
// reflog selectors or task-ID shorthand.
func ParseSelector(s, field string, allowEmpty bool) (Selector, error) {
	switch {
	case allowEmpty && s == SelectorEmpty:
		return Selector{Kind: SelectorKindEmpty}, nil
	case ValidCommitHash(s):
		return Selector{Kind: SelectorKindHash, Value: s}, nil
	case strings.HasPrefix(s, TaskRefPrefix):
		if !ValidTaskRef(s) {
			return Selector{}, errInvalid("invalid %s: a task ref is refs/callsheet/tasks/t_ followed by 32 lowercase hex digits", field)
		}
		return Selector{Kind: SelectorKindTask, Value: s}, nil
	case s == "":
		return Selector{}, errInvalid("%s must not be empty", field)
	}
	full, err := NormalizeBranch(s, field)
	if err != nil {
		return Selector{}, errInvalid("invalid %s: want a full 40-hex commit hash, a branch (short or refs/heads/...) or a complete refs/callsheet/tasks/ ref%s", field, map[bool]string{true: ", or empty", false: ""}[allowEmpty])
	}
	return Selector{Kind: SelectorKindBranch, Value: full}, nil
}

// ValidStatusCursor reports a complete branch or task ref (status after).
func ValidStatusCursor(s string) bool { return ValidBranchRef(s) || ValidTaskRef(s) }

// ParseCutoff parses a prune cutoff: RFC3339 with fractional seconds
// allowed and a mandatory zone, normalized to UTC.
func ParseCutoff(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, errInvalid("before must be an RFC3339 timestamp with a zone, e.g. 2026-09-30T12:00:00Z or 2026-09-30T14:00:00+02:00")
	}
	return t.UTC(), nil
}

// ParsePathCursor decodes a diff cursor: nonempty canonical standard
// padded base64 of the last emitted raw path.
func ParsePathCursor(s string) ([]byte, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if s == "" || err != nil || len(b) == 0 || base64.StdEncoding.EncodeToString(b) != s {
		return nil, errInvalid("after must be the nonempty standard base64 path of a previous diff page's next_after")
	}
	return b, nil
}

// ValidMode reports a six-digit git octal mode string.
func ValidMode(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < 6; i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

// ---- Strict response parsers (the client's verification) ----

func decodeWorkspace(b []byte, v any, what string) error {
	if len(b) > MaxWorkspaceResponse {
		return errInvalid("%s exceeds %d bytes", what, MaxWorkspaceResponse)
	}
	return decodeStrict(b, v, what)
}

func (s WorkspaceSummary) validate(what string) error {
	if !ValidWorkspaceName(s.Name) || !ValidWorkspaceToken(s.Instance) || !validTime(s.CreatedAt) {
		return errInvalid("%s has an invalid workspace name, instance or created_at", what)
	}
	return nil
}

func (v WorkspaceView) validate(what string) error {
	if err := (WorkspaceSummary{Name: v.Name, Instance: v.Instance, CreatedAt: v.CreatedAt}).validate(what); err != nil {
		return err
	}
	switch {
	case !ValidWorkspaceToken(v.Generation):
		return errInvalid("%s has an invalid generation", what)
	case v.DefaultBranch != DefaultBranchRef:
		return errInvalid("%s default_branch must be %s", what, DefaultBranchRef)
	case v.SizeBytes < 0:
		return errInvalid("%s size_bytes must not be negative", what)
	case v.Retention.AutomaticPrune || v.Retention.QuotaBytes != nil:
		return errInvalid("%s retention must be {automatic_prune:false, quota_bytes:null}", what)
	}
	return nil
}

// ParseWorkspaceView strictly decodes and validates a workspace view.
func ParseWorkspaceView(b []byte) (WorkspaceView, error) {
	const what = "workspace view"
	var v WorkspaceView
	if err := decodeWorkspace(b, &v, what); err != nil {
		return v, err
	}
	return v, v.validate(what)
}

// ParseWorkspaceList strictly decodes a list page: sorted unique names,
// at most limit rows, and next_after equal to the last row when present.
func ParseWorkspaceList(b []byte, after string, limit int) (WorkspaceListResponse, error) {
	const what = "workspace list response"
	var r WorkspaceListResponse
	if err := decodeWorkspace(b, &r, what); err != nil {
		return r, err
	}
	if len(r.Workspaces) > limit {
		return r, errInvalid("%s has more rows than the requested limit", what)
	}
	prev := after
	for i, s := range r.Workspaces {
		if err := s.validate(what); err != nil {
			return r, err
		}
		if (i > 0 || after != "") && s.Name <= prev {
			return r, errInvalid("%s is not sorted after its cursor", what)
		}
		prev = s.Name
	}
	if r.NextAfter != nil && (len(r.Workspaces) == 0 || *r.NextAfter != prev) {
		return r, errInvalid("%s next_after must be the last name of the page", what)
	}
	return r, nil
}

// ParseWorkspaceRemove strictly decodes a removal answer.
func ParseWorkspaceRemove(b []byte) (WorkspaceRemoveResponse, error) {
	const what = "workspace removal response"
	var r WorkspaceRemoveResponse
	if err := decodeWorkspace(b, &r, what); err != nil {
		return r, err
	}
	if !ValidWorkspaceName(r.Name) || !ValidWorkspaceToken(r.Instance) || !r.Removed {
		return r, errInvalid("%s is invalid", what)
	}
	return r, nil
}

// ParseWorkspacePrune strictly decodes a prune answer.
func ParseWorkspacePrune(b []byte) (WorkspacePruneResponse, error) {
	const what = "workspace prune response"
	var r WorkspacePruneResponse
	if err := decodeWorkspace(b, &r, what); err != nil {
		return r, err
	}
	if !ValidWorkspaceName(r.Name) || !ValidWorkspaceToken(r.Instance) || !ValidWorkspaceToken(r.Generation) ||
		r.RemovedTaskRefs < 0 || r.ReclaimedBytes < 0 || r.SizeBytes < 0 {
		return r, errInvalid("%s is invalid", what)
	}
	return r, nil
}

func optHash(p *string) bool { return p == nil || ValidCommitHash(*p) }

// ParseWorkspaceRefSet strictly decodes a ref set answer.
func ParseWorkspaceRefSet(b []byte) (WorkspaceRefSetResponse, error) {
	const what = "workspace ref set response"
	var r WorkspaceRefSetResponse
	if err := decodeWorkspace(b, &r, what); err != nil {
		return r, err
	}
	if !ValidWorkspaceName(r.Name) || !ValidWorkspaceToken(r.Instance) || !ValidWorkspaceToken(r.Generation) ||
		!ValidBranchRef(r.Ref) || !optHash(r.OldCommit) || !optHash(r.NewCommit) {
		return r, errInvalid("%s is invalid", what)
	}
	return r, nil
}

// ParseWorkspaceStatus strictly decodes a status page: refs sorted by raw
// bytes after the cursor, valid names, hashes and publication times.
func ParseWorkspaceStatus(b []byte, after string, limit int) (WorkspaceStatusResponse, error) {
	const what = "workspace status response"
	var r WorkspaceStatusResponse
	if err := decodeWorkspace(b, &r, what); err != nil {
		return r, err
	}
	if !ValidWorkspaceName(r.Name) || !ValidWorkspaceToken(r.Instance) || !ValidWorkspaceToken(r.Generation) || r.DefaultBranch != DefaultBranchRef {
		return r, errInvalid("%s has an invalid header", what)
	}
	if len(r.Refs) > limit {
		return r, errInvalid("%s has more rows than the requested limit", what)
	}
	prev := after
	for i, ref := range r.Refs {
		switch {
		case ValidBranchRef(ref.Name):
			if ref.PublishedAt != nil {
				return r, errInvalid("%s branch published_at must be null", what)
			}
		case ValidTaskRef(ref.Name):
			if ref.PublishedAt == nil || !validTime(*ref.PublishedAt) {
				return r, errInvalid("%s task ref needs a UTC published_at", what)
			}
		default:
			return r, errInvalid("%s has an invalid ref name", what)
		}
		if !ValidCommitHash(ref.Commit) {
			return r, errInvalid("%s has an invalid commit", what)
		}
		if (i > 0 || after != "") && ref.Name <= prev {
			return r, errInvalid("%s is not sorted after its cursor", what)
		}
		prev = ref.Name
	}
	if r.NextAfter != nil && (len(r.Refs) == 0 || *r.NextAfter != prev) {
		return r, errInvalid("%s next_after must be the last ref of the page", what)
	}
	return r, nil
}

// ParseWorkspaceDiff strictly decodes a diff page: rows sorted by raw path
// bytes after the cursor with consistent kinds, modes and sizes.
func ParseWorkspaceDiff(b []byte, after []byte, limit int) (WorkspaceDiffResponse, error) {
	const what = "workspace diff response"
	var r WorkspaceDiffResponse
	if err := decodeWorkspace(b, &r, what); err != nil {
		return r, err
	}
	if !ValidWorkspaceName(r.Name) || !ValidWorkspaceToken(r.Instance) || !ValidWorkspaceToken(r.Generation) ||
		!optHash(r.BaseCommit) || !ValidCommitHash(r.TargetCommit) {
		return r, errInvalid("%s has an invalid header", what)
	}
	if len(r.Changes) > limit {
		return r, errInvalid("%s has more rows than the requested limit", what)
	}
	prev := after
	var last string
	for i, c := range r.Changes {
		p, err := ParsePathCursor(c.PathBase64)
		if err != nil {
			return r, errInvalid("%s has an invalid path_base64", what)
		}
		if (i > 0 || after != nil) && string(p) <= string(prev) {
			return r, errInvalid("%s is not sorted by raw path after its cursor", what)
		}
		if err := c.validate(); err != nil {
			return r, errInvalid("%s: %s", what, err.Message)
		}
		prev, last = p, c.PathBase64
	}
	if r.NextAfter != nil && (len(r.Changes) == 0 || *r.NextAfter != last) {
		return r, errInvalid("%s next_after must be the last path of the page", what)
	}
	return r, nil
}

func (c WorkspaceChange) validate() *Error {
	okMode := func(m *string) bool { return m == nil || ValidMode(*m) }
	okSize := func(n *int64) bool { return n == nil || *n >= 0 }
	if !okMode(c.OldMode) || !okMode(c.NewMode) || !okSize(c.OldBytes) || !okSize(c.NewBytes) ||
		(c.OldMode == nil) != (c.OldBytes == nil) || (c.NewMode == nil) != (c.NewBytes == nil) {
		return errInvalid("a change row has inconsistent modes or sizes")
	}
	switch c.Kind {
	case ChangeAdded:
		if c.OldMode != nil || c.NewMode == nil {
			return errInvalid("an added row must have only new metadata")
		}
	case ChangeDeleted:
		if c.OldMode == nil || c.NewMode != nil {
			return errInvalid("a deleted row must have only old metadata")
		}
	case ChangeModified:
		if c.OldMode == nil || c.NewMode == nil {
			return errInvalid("a modified row must have old and new metadata")
		}
	default:
		return errInvalid("a change row has an unknown kind")
	}
	return nil
}

// ---- Strict request parsers (the plane's) ----

func decodeRequest(b []byte, v any, what string) error {
	if len(b) > MaxWorkspaceBody {
		return errInvalid("%s exceeds %d bytes", what, MaxWorkspaceBody)
	}
	return decodeStrict(b, v, what)
}

// ParseWorkspaceCreateRequest strictly decodes {name}.
func ParseWorkspaceCreateRequest(b []byte) (WorkspaceCreateRequest, error) {
	var r WorkspaceCreateRequest
	if err := decodeRequest(b, &r, "workspace create request"); err != nil {
		return r, err
	}
	if !ValidWorkspaceName(r.Name) {
		return r, InvalidWorkspaceName()
	}
	return r, nil
}

// ValidateInstance is the invalid_argument check of an instance argument.
func ValidateInstance(s string) error {
	if !ValidWorkspaceToken(s) {
		return errInvalid("instance must be the workspace's 32-hex instance token (see workspace show)")
	}
	return nil
}

// ParseWorkspaceRemoveRequest strictly decodes {instance}.
func ParseWorkspaceRemoveRequest(b []byte) (WorkspaceRemoveRequest, error) {
	var r WorkspaceRemoveRequest
	if err := decodeRequest(b, &r, "workspace removal request"); err != nil {
		return r, err
	}
	return r, ValidateInstance(r.Instance)
}

// ParseWorkspacePruneRequest strictly decodes {instance,before}.
func ParseWorkspacePruneRequest(b []byte) (WorkspacePruneRequest, time.Time, error) {
	var r WorkspacePruneRequest
	if err := decodeRequest(b, &r, "workspace prune request"); err != nil {
		return r, time.Time{}, err
	}
	if err := ValidateInstance(r.Instance); err != nil {
		return r, time.Time{}, err
	}
	t, err := ParseCutoff(r.Before)
	return r, t, err
}

// RefSetIntent is a validated ref set request.
type RefSetIntent struct {
	Instance string
	// Ref is the canonical full branch ref.
	Ref string
	// Expected is a full hash or "" for absent.
	Expected string
	// Target is the parsed selector; zero for a delete.
	Target Selector
	Delete bool
}

// ValidateRefSet checks a ref set request: a portable branch, expected a
// full hash or absent, exactly one of target and delete=true (delete=false
// is omitted), delete never with expected absent, and no HEAD or task
// destination.
func ValidateRefSet(r WorkspaceRefSetRequest) (RefSetIntent, error) {
	var in RefSetIntent
	if err := ValidateInstance(r.Instance); err != nil {
		return in, err
	}
	full, err := NormalizeBranch(r.Branch, "branch")
	if err != nil {
		return in, err
	}
	in.Instance, in.Ref = r.Instance, full
	switch {
	case r.Expected == ExpectedAbsent:
	case ValidCommitHash(r.Expected):
		in.Expected = r.Expected
	default:
		return in, errInvalid("expected must be a full 40-hex commit hash or absent")
	}
	del := r.Delete != nil && *r.Delete
	switch {
	case del && r.Target != nil:
		return in, errInvalid("give exactly one of a target and delete")
	case !del && r.Target == nil:
		return in, errInvalid("give exactly one of a target and delete")
	case del && in.Expected == "":
		return in, errInvalid("a delete needs expected as the branch's current full hash, not absent")
	}
	in.Delete = del
	if !del {
		if in.Target, err = ParseSelector(*r.Target, "target", false); err != nil {
			return in, err
		}
	}
	return in, nil
}

// ParseWorkspaceRefSetRequest strictly decodes and validates ref set.
func ParseWorkspaceRefSetRequest(b []byte) (RefSetIntent, error) {
	var r WorkspaceRefSetRequest
	if err := decodeRequest(b, &r, "workspace ref set request"); err != nil {
		return RefSetIntent{}, err
	}
	return ValidateRefSet(r)
}
