package workspacetransfer

import (
	"context"
	"errors"
	"syscall"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Safe transfer errors: fixed messages, never a local path, file
// content, commit message or raw library error (the cause is kept for
// local diagnosis only and is never serialized).

func errUnsupported(what string) error {
	return contract.TransferError(contract.CodeInvalidArgument, contract.ReasonUnsupportedRepository,
		"unsupported repository: "+what+"; Callsheet transfers only ordinary non-bare repositories at their root and plain folders")
}

func errInsideRepository() error {
	return contract.TransferError(contract.CodeInvalidArgument, contract.ReasonUnsupportedRepository,
		"the path is inside a git repository: use the repository root (a subdirectory is never transferred as a plain folder)")
}

func errDirty() error {
	return contract.TransferError(contract.CodeConflict, contract.ReasonDirtySource, contract.DirtyMessage)
}

func errUnborn() error {
	return contract.TransferError(contract.CodeConflict, contract.ReasonDirtySource, "the repository has no commit (HEAD is unborn); commit first")
}

func errSourceChanged() error {
	return contract.TransferError(contract.CodeConflict, contract.ReasonSourceChanged,
		"the source changed while it was being read; nothing was pushed: keep the source quiescent and push again")
}

func errNonFastForward() error {
	return contract.TransferError(contract.CodeConflict, contract.ReasonNonFastForward, contract.NonFastForwardMessage)
}

func errUnsafeTree(what string) error {
	return contract.TransferError(contract.CodeInvalidArgument, contract.ReasonUnsafeTree, what)
}

func errNotEmpty() error {
	return contract.TransferError(contract.CodeConflict, contract.ReasonDestinationNotEmpty,
		"destination must be new or empty; nothing was written")
}

func errLocalRef(what string) error {
	return contract.TransferError(contract.CodeConflict, contract.ReasonLocalRefConflict, what)
}

// errStorage is a local I/O failure. Disk exhaustion says so; nothing
// else about the cause is shown.
func errStorage(what string, cause error) error {
	msg := what
	if errors.Is(cause, syscall.ENOSPC) || errors.Is(cause, syscall.EDQUOT) {
		msg += ": the disk is full"
	} else if errors.Is(cause, syscall.EACCES) || errors.Is(cause, syscall.EPERM) {
		msg += ": permission denied"
	}
	return &contract.Error{Code: contract.CodeInternal, Message: msg, Details: map[string]any{"reason": contract.ReasonStorageFailure}, Cause: cause}
}

func errIntegrity(what string) error {
	return contract.New(contract.CodeInternal, what)
}

// ctxOr returns ctx's error when it is done, else err.
func ctxOr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

// isContract reports whether err already is a contract error or a
// cancellation (to be returned unchanged).
func isContract(err error) bool {
	var ce *contract.Error
	return errors.As(err, &ce) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// orStorage returns err unchanged when it is a contract error or a
// cancellation, else a storage failure describing what.
func orStorage(err error, what string) error {
	if err == nil || isContract(err) {
		return err
	}
	return errStorage(what, err)
}
