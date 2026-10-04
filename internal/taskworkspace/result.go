package taskworkspace

import (
	"context"
	"errors"
	"syscall"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// SnapshotInput is one result snapshot after every child writer is gone.
type SnapshotInput struct {
	GOOS       string
	TaskDir    string
	Map        *workspacetransfer.PathMap
	RuntimeDir string
}

// SnapshotError is a failed snapshot with its fixed publication code.
type SnapshotError struct {
	Code  string
	Cause error
}

func (e *SnapshotError) Error() string { return "workspace snapshot failed: " + e.Code }
func (e *SnapshotError) Unwrap() error { return e.Cause }

// PublicationCode returns err's fixed publication error code: a nested
// repository is unsupported_repository, a metadata overflow
// workspace_metadata_overflow, a disk or storage failure
// workspace_storage_failed, anything else about the worktree
// workspace_snapshot_failed.
func PublicationCode(err error) string {
	var se *SnapshotError
	if errors.As(err, &se) {
		return se.Code
	}
	switch r := contract.TransferReason(err); {
	case r == contract.ReasonUnsupportedRepository:
		return contract.PubErrUnsupportedRepository
	case r == contract.PubErrMetadataOverflow:
		return contract.PubErrMetadataOverflow
	case r == contract.ReasonStorageFailure, errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return contract.PubErrStorageFailed
	}
	return contract.PubErrSnapshotFailed
}

func snapErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return &SnapshotError{Code: PublicationCode(err), Cause: err}
}

// Snapshot stores the work directory's result tree in the task's trusted
// database (synced) and returns it.
func Snapshot(ctx context.Context, in SnapshotInput) (plumbing.Hash, workspacetransfer.Snapshot, error) {
	db, err := workspacetransfer.OpenTaskDB(taskDB(in.TaskDir))
	if err != nil {
		return plumbing.ZeroHash, workspacetransfer.Snapshot{}, snapErr(ctx, err)
	}
	defer db.Close()
	s, err := workspacetransfer.SnapshotTask(ctx, workspacetransfer.SnapshotOptions{GOOS: in.GOOS, Work: taskWork(in.TaskDir), DB: db, Map: in.Map, RuntimeDir: in.RuntimeDir})
	if err != nil {
		return plumbing.ZeroHash, s, snapErr(ctx, err)
	}
	return s.Tree, s, nil
}

func taskDB(taskDir string) string   { return taskDir + "/" + ObjectsName }
func taskWork(taskDir string) string { return taskDir + "/" + WorkName }

// ---- Result commit ----

// ResultCommit is the deterministic result commit of a task: tree, the
// parent list [base] (none for the empty base), the fixed identity and
// the fixed message of the task, role, effective model and canonical
// terminal state. Every interpolated value must be printable ASCII.
func ResultCommit(taskID, roleID, model, state string, base *plumbing.Hash, tree plumbing.Hash) (*object.Commit, plumbing.Hash, error) {
	for _, v := range []string{taskID, roleID, model, state} {
		if !contract.ValidMessageValue(v) {
			return nil, plumbing.ZeroHash, &SnapshotError{Code: contract.PubErrSnapshotFailed, Cause: errors.New("a commit message value is not printable ASCII")}
		}
	}
	var parent plumbing.Hash
	if base != nil {
		parent = *base
	}
	c := workspacetransfer.TaskResultCommit(tree, parent, contract.TaskResultMessage(taskID, roleID, model, state))
	h, err := workspacetransfer.CommitHash(c)
	return c, h, err
}

// StoreCommit stores c in the task's trusted database and syncs it: the
// commit and its complete parent closure are durable before any push.
func StoreCommit(ctx context.Context, taskDir string, c *object.Commit) (plumbing.Hash, error) {
	db, err := workspacetransfer.OpenTaskDB(taskDB(taskDir))
	if err != nil {
		return plumbing.ZeroHash, &SnapshotError{Code: contract.PubErrStorageFailed, Cause: err}
	}
	defer db.Close()
	h, err := db.WriteObject(ctx, c)
	if err == nil {
		err = db.Sync()
	}
	if err != nil {
		return plumbing.ZeroHash, snapErr(ctx, err)
	}
	if _, _, err := db.Closure(ctx, []plumbing.Hash{h}); err != nil {
		return plumbing.ZeroHash, snapErr(ctx, err)
	}
	return h, nil
}

// PushObjects selects the objects of commit's closure not reachable from
// haves (the plane-advertised base and own ref); with no haves, the
// complete closure.
func PushObjects(s storer.EncodedObjectStorer, commit plumbing.Hash, haves []plumbing.Hash) ([]plumbing.Hash, error) {
	return revlist.Objects(s, []plumbing.Hash{commit}, haves)
}
