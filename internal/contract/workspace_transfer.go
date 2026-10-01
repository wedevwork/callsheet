package contract

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Workspace local transfers (iteration 09b): ws push and ws pull move git
// objects between a local directory on the machine running the CLI or
// the MCP server and a workspace on the plane. Only paths, selectors and
// the result metadata below cross the CLI and MCP boundaries: never file
// contents, file lists, commit messages, absolute local paths, credentials
// or pack diagnostics.

// Transfer kinds.
const (
	// KindGit is a git repository source or destination.
	KindGit = "git"
	// KindFolder is a plain folder source or destination.
	KindFolder = "folder"
)

// Transfer detail reasons (details.reason of a transfer error).
const (
	ReasonDirtySource           = "dirty_source"
	ReasonUnsupportedRepository = "unsupported_repository"
	ReasonNonFastForward        = "non_fast_forward"
	ReasonSourceChanged         = "source_changed"
	ReasonDestinationNotEmpty   = "destination_not_empty"
	ReasonUnsafeTree            = "unsafe_tree"
	ReasonLocalRefConflict      = "local_ref_conflict"
	ReasonStorageFailure        = "storage_failure"
)

// transferReasons is the closed set of transfer detail reasons.
var transferReasons = map[string]bool{
	ReasonDirtySource: true, ReasonUnsupportedRepository: true, ReasonNonFastForward: true, ReasonSourceChanged: true,
	ReasonDestinationNotEmpty: true, ReasonUnsafeTree: true, ReasonLocalRefConflict: true, ReasonStorageFailure: true,
}

// ValidTransferReason reports one of the transfer detail reasons.
func ValidTransferReason(r string) bool { return transferReasons[r] }

// Fixed guidance of the transfer refusals and ambiguous outcomes.
const (
	// DirtyMessage refuses a git source with local changes.
	DirtyMessage = "source has uncommitted or untracked non-ignored changes; commit first"
	// NonFastForwardMessage refuses a push that would replace history.
	NonFastForwardMessage = "push would replace history; push to a new branch, then explicitly move the target with ws ref set"
	// PushAmbiguity follows a push whose answer was lost.
	PushAmbiguity = "the push may have succeeded: inspect the workspace with ws status or ws show before repeating it (never retried)"
	// PullAmbiguity follows a pull whose local publication or answer was
	// lost.
	PullAmbiguity = "the pull may have been published locally: inspect the destination folder or the Callsheet ref before repeating it (nothing is rolled back)"
)

// TransferError is a transfer error with an optional detail reason.
func TransferError(code Code, reason, message string) *Error {
	e := New(code, message)
	if reason != "" {
		e.Details = map[string]any{"reason": reason}
	}
	return e
}

// TransferReason returns err's details.reason, or "".
func TransferReason(err error) string {
	var ce *Error
	if !errors.As(err, &ce) || ce.Details == nil {
		return ""
	}
	r, _ := ce.Details["reason"].(string)
	return r
}

// WorkspacePushResult is ws push's result. Branch is the full target
// ref; OldCommit is null for a created branch; SourceKind is git or
// folder; Changed reports that the branch moved (a same-value CAS is
// false).
type WorkspacePushResult struct {
	Name       string  `json:"name"`
	Instance   string  `json:"instance"`
	Branch     string  `json:"branch"`
	OldCommit  *string `json:"old_commit"`
	Commit     string  `json:"commit"`
	SourceKind string  `json:"source_kind"`
	Changed    bool    `json:"changed"`
}

// WorkspacePullResult is ws pull's result. Selector is the canonical full
// ref or full hash; DestinationKind is git or folder; LocalRef and
// OldCommit identify the Callsheet ref of a git destination (both null
// for a folder export); Changed reports that the ref moved or the folder
// was published.
type WorkspacePullResult struct {
	Name            string  `json:"name"`
	Instance        string  `json:"instance"`
	Selector        string  `json:"selector"`
	Commit          string  `json:"commit"`
	DestinationKind string  `json:"destination_kind"`
	LocalRef        *string `json:"local_ref"`
	OldCommit       *string `json:"old_commit"`
	Changed         bool    `json:"changed"`
}

// Validate checks a push result's grammars.
func (r WorkspacePushResult) Validate() error {
	switch {
	case !ValidWorkspaceName(r.Name) || !ValidWorkspaceToken(r.Instance):
		return errInvalid("push result has an invalid name or instance")
	case !ValidBranchRef(r.Branch):
		return errInvalid("push result has an invalid branch")
	case !optHash(r.OldCommit) || !ValidCommitHash(r.Commit):
		return errInvalid("push result has an invalid commit")
	case r.SourceKind != KindGit && r.SourceKind != KindFolder:
		return errInvalid("push result has an invalid source_kind")
	case r.OldCommit == nil && !r.Changed, r.OldCommit != nil && (*r.OldCommit == r.Commit) == r.Changed:
		return errInvalid("push result changed does not match its commits")
	}
	return nil
}

// Validate checks a pull result's grammars and kind consistency.
func (r WorkspacePullResult) Validate() error {
	switch {
	case !ValidWorkspaceName(r.Name) || !ValidWorkspaceToken(r.Instance):
		return errInvalid("pull result has an invalid name or instance")
	case !ValidCommitHash(r.Selector) && !ValidBranchRef(r.Selector) && !ValidTaskRef(r.Selector):
		return errInvalid("pull result has an invalid selector")
	case !ValidCommitHash(r.Commit) || !optHash(r.OldCommit):
		return errInvalid("pull result has an invalid commit")
	}
	switch r.DestinationKind {
	case KindFolder:
		if r.LocalRef != nil || r.OldCommit != nil || !r.Changed {
			return errInvalid("a folder pull result has no local ref or old commit and is always changed")
		}
	case KindGit:
		if r.LocalRef == nil || LocalRefFor(r.Name, r.Selector) != *r.LocalRef {
			return errInvalid("a git pull result needs the selector's Callsheet ref")
		}
		if (r.OldCommit == nil && !r.Changed) || (r.OldCommit != nil && (*r.OldCommit == r.Commit) == r.Changed) {
			return errInvalid("pull result changed does not match its commits")
		}
	default:
		return errInvalid("pull result has an invalid destination_kind")
	}
	return nil
}

// CallsheetRefPrefix starts every local observation ref a pull writes.
const CallsheetRefPrefix = "refs/callsheet/"

// LocalRefFor maps a workspace name and a canonical selector to its one
// local observation ref: refs/callsheet/NAME/heads/BRANCH,
// refs/callsheet/NAME/tasks/TASK_ID or refs/callsheet/NAME/commits/HASH.
// It returns "" for an invalid input.
func LocalRefFor(name, selector string) string {
	if !ValidWorkspaceName(name) {
		return ""
	}
	base := CallsheetRefPrefix + name + "/"
	switch {
	case ValidCommitHash(selector):
		return base + "commits/" + selector
	case ValidBranchRef(selector):
		return base + "heads/" + strings.TrimPrefix(selector, BranchPrefix)
	case ValidTaskRef(selector):
		return base + "tasks/" + strings.TrimPrefix(selector, TaskRefPrefix)
	}
	return ""
}

// Native pathname limits: the byte length of a complete path argument
// (PATH_MAX without its terminating NUL) and of one component (NAME_MAX).
const (
	MaxPathLinux    = 4095
	MaxPathDarwin   = 1023
	MaxPathArgument = MaxPathLinux
	MaxNameBytes    = 255
)

// MaxPathFor returns the native path byte limit of goos (0: unsupported).
func MaxPathFor(goos string) int {
	switch goos {
	case "linux":
		return MaxPathLinux
	case "darwin":
		return MaxPathDarwin
	}
	return 0
}

// ValidateLocalPath checks a PATH argument before any resolution: it must
// be nonempty, valid UTF-8 without NUL and within goos's native pathname
// limit. The message never echoes the path.
func ValidateLocalPath(goos, p string) error {
	switch {
	case p == "":
		return errInvalid("path must not be empty; omit it to use the current directory")
	case !utf8.ValidString(p) || strings.IndexByte(p, 0) >= 0:
		return errInvalid("path must be valid UTF-8 without NUL bytes")
	case len(p) > MaxPathFor(goos):
		return errInvalid("path exceeds the %d-byte native pathname limit", MaxPathFor(goos))
	}
	return nil
}

// PushIntent is a validated push request.
type PushIntent struct {
	Name, Instance string
	// Branch is the canonical full target ref (default refs/heads/main).
	Branch string
}

// ValidatePush checks a push's name, instance and optional branch (""
// selects main).
func ValidatePush(name, instance, branch string) (PushIntent, error) {
	if !ValidWorkspaceName(name) {
		return PushIntent{}, InvalidWorkspaceName()
	}
	if err := ValidateInstance(instance); err != nil {
		return PushIntent{}, err
	}
	full := DefaultBranchRef
	if branch != "" {
		var err error
		if full, err = NormalizeBranch(branch, "branch"); err != nil {
			return PushIntent{}, err
		}
	}
	return PushIntent{Name: name, Instance: instance, Branch: full}, nil
}

// PullIntent is a validated pull request.
type PullIntent struct {
	Name string
	// Selector is the parsed REF; Canonical its full ref or hash.
	Selector  Selector
	Canonical string
}

// ValidatePull checks a pull's name and REF: a short or full branch, a
// full 40-hex commit hash or a complete task ref (no task-ID shorthand:
// t_... alone is a branch name).
func ValidatePull(name, ref string) (PullIntent, error) {
	if !ValidWorkspaceName(name) {
		return PullIntent{}, InvalidWorkspaceName()
	}
	sel, err := ParseSelector(ref, "ref", false)
	if err != nil {
		return PullIntent{}, err
	}
	return PullIntent{Name: name, Selector: sel, Canonical: sel.Value}, nil
}

// ParseWorkspacePushResult strictly decodes and validates a push result.
func ParseWorkspacePushResult(b []byte) (WorkspacePushResult, error) {
	var r WorkspacePushResult
	if err := decodeStrict(b, &r, "workspace push result"); err != nil {
		return r, err
	}
	return r, r.Validate()
}

// ParseWorkspacePullResult strictly decodes and validates a pull result.
func ParseWorkspacePullResult(b []byte) (WorkspacePullResult, error) {
	var r WorkspacePullResult
	if err := decodeStrict(b, &r, "workspace pull result"); err != nil {
		return r, err
	}
	return r, r.Validate()
}
