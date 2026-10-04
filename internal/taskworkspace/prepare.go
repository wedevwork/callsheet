package taskworkspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Task storage inside the sidecar's private journal directory
// tasks/<task_id>/: work/ (the child's cwd), objects/ (the trusted bare
// selected-history and result database) and publication.json (the
// publication checkpoint).
const (
	WorkName        = "work"
	ObjectsName     = "objects"
	PublicationName = "publication.json"
)

// Remote is one task's node transfer session on the plane's assignment-
// guarded route: the upload advertisement (the synthetic base ref, none
// for the empty base) and the authorized fetch.
type Remote interface {
	Fetcher
	UploadRefs(ctx context.Context) (map[string]plumbing.Hash, error)
}

// PrepareInput is one workspace task's preparation.
type PrepareInput struct {
	GOOS    string
	TaskDir string
	Binding contract.WorkspaceBinding
	Remote  Remote
	Cache   *Manager
	// RuntimeDir allocates the owned final-output directory inside work.
	RuntimeDir bool
}

// Prepared is a ready private checkout.
type Prepared struct {
	Work       string
	Map        *workspacetransfer.PathMap
	RuntimeDir string
	Stats      FetchStats
	Files      int64
	Bytes      int64
}

// PrepareError is a definite preparation failure with its fixed reason.
type PrepareError struct {
	Reason string
	Cause  error
}

func (e *PrepareError) Error() string { return "workspace preparation failed: " + e.Reason }
func (e *PrepareError) Unwrap() error { return e.Cause }

// PrepareReason returns err's fixed preparation reason: the deadline is
// workspace_prepare_timeout, a remote refusal or transport failure
// workspace_unavailable or (the base no longer reachable)
// workspace_base_unavailable, anything local workspace_checkout_failed.
func PrepareReason(err error) string {
	var pe *PrepareError
	switch {
	case errors.As(err, &pe):
		return pe.Reason
	case errors.Is(err, context.DeadlineExceeded):
		return contract.ReasonWorkspacePrepareTimeout
	}
	return contract.ReasonWorkspaceCheckoutFailed
}

// remoteReason classifies a remote fetch or advertisement error.
func remoteReason(err error) string {
	if contract.TransferReason(err) == contract.ReasonWorkspaceBaseUnavailable {
		return contract.ReasonWorkspaceBaseUnavailable
	}
	var ce *contract.Error
	if errors.As(err, &ce) {
		if r, _ := ce.Details["reason"].(string); r == contract.ReasonWorkspaceBaseUnavailable {
			return r
		}
		switch ce.Code {
		case contract.CodeNotFound, contract.CodeConflict, contract.CodeUnavailable, contract.CodeInvalidArgument, contract.CodeProtocolMismatch, contract.CodeTrustFailed:
			return contract.ReasonWorkspaceUnavailable
		}
	}
	return ""
}

// Prepare builds the task's trusted database and private checkout in
// TaskDir: the selected base's closure (through the cache, under its
// instance lock only for the fetch) is copied into objects/, materialized
// into the empty work/ with its independent child repository, and an
// owned runtime directory is allocated when asked. The empty base only
// checks the bound instance through the upload advertisement.
func Prepare(ctx context.Context, in PrepareInput) (*Prepared, error) {
	work := filepath.Join(in.TaskDir, WorkName)
	p := &Prepared{Work: work}
	db, err := workspacetransfer.CreateTaskDB(filepath.Join(in.TaskDir, ObjectsName))
	if err != nil {
		return nil, local(ctx, err)
	}
	defer db.Close()
	var base plumbing.Hash
	if in.Binding.BaseCommit == nil {
		refs, err := in.Remote.UploadRefs(ctx)
		if err != nil {
			return nil, remote(ctx, err)
		}
		if len(refs) != 0 {
			return nil, &PrepareError{Reason: contract.ReasonWorkspaceUnavailable, Cause: errors.New("the empty base's advertisement names refs")}
		}
	} else {
		base = plumbing.NewHash(*in.Binding.BaseCommit)
		st, err := in.Cache.Fetch(ctx, in.Binding.Name, in.Binding.Instance, base, in.Remote, db)
		p.Stats = st
		if err != nil {
			return nil, remote(ctx, err)
		}
	}
	co, err := workspacetransfer.CheckoutTask(ctx, workspacetransfer.CheckoutOptions{GOOS: in.GOOS, Work: work, DB: db, Commit: base})
	if err != nil {
		return nil, local(ctx, err)
	}
	p.Map, p.Files, p.Bytes = co.Map, co.Files, co.Bytes
	if in.RuntimeDir {
		if p.RuntimeDir, err = allocRuntime(work); err != nil {
			return nil, local(ctx, err)
		}
	}
	return p, nil
}

// remote wraps a remote or cache error with its reason (cancellation and
// the deadline unchanged; a corrupt fetched closure is a checkout
// failure).
func remote(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if r := remoteReason(err); r != "" {
		return &PrepareError{Reason: r, Cause: err}
	}
	return &PrepareError{Reason: contract.ReasonWorkspaceCheckoutFailed, Cause: err}
}

func local(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return &PrepareError{Reason: contract.ReasonWorkspaceCheckoutFailed, Cause: err}
}

// allocRuntime creates an exclusive random owned runtime directory
// (.callsheet-runtime-<128-bit token>, 0700) inside work, retrying a
// collision without touching the existing path.
func allocRuntime(work string) (string, error) {
	for i := 0; i < 8; i++ {
		n := contract.RuntimeDirPrefix + token()
		err := os.Mkdir(filepath.Join(work, n), 0o700)
		if err == nil {
			return n, syncDir(work)
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("cannot allocate a runtime directory")
}
