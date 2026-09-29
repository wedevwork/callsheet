package mcpqual

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// ProcSpec is one launch: an absolute executable (never a PATH lookup),
// its arguments, its complete environment, working directory and a file
// for its stderr. Stdin is the null device.
type ProcSpec struct {
	Path       string
	Args       []string
	Env        []string
	Dir        string
	StderrPath string
}

// Proc is a launched process leading its own process group.
type Proc interface {
	// PGID is the run-specific process group ID (the leader's PID).
	PGID() int
	// Stdout is the leader's stdout; descendants may hold it open.
	Stdout() io.Reader
	// Exited is closed once the leader has been waited for (reaped).
	Exited() <-chan struct{}
	// Status is the leader's exit code or signal name, valid after Exited.
	Status() (exit *int, signal *string)
	// CloseStdout closes the read end (after cleanup).
	CloseStdout()
}

// Launcher starts processes; tests inject fakes, production uses the exec
// launcher of the linux/darwin build.
type Launcher interface {
	Start(ProcSpec) (Proc, error)
}

// Reaper stops and reaps one launched process group, proving its absence.
type Reaper interface {
	Reap(Proc) CaseCleanup
}

// Cleanup policy (design 07b, Process cleanup): TERM, a 1 s grace, KILL,
// then Wait for the tracked leader and prove group absence (ESRCH) within
// 5 s, polling every 10 ms. The default processgroup ProbeLead (25 ms)
// applies and no BeforeDeadline callback is installed.
const (
	CleanupGrace = time.Second
	CleanupLimit = 5 * time.Second
	CleanupPoll  = 10 * time.Millisecond
)

// CleanupPolicy is the per-OS cleanup policy. linux and darwin share the
// same numbers and the same rule that a negative-group EPERM is never
// absence: it is polled until ESRCH or the limit, then a cleanup failure
// (on darwin this covers a zombie-only group).
type CleanupPolicy struct {
	Grace, Limit, Poll time.Duration
	EPERMIsAbsence     bool
}

// PolicyFor returns goos's cleanup policy; other systems are unsupported.
func PolicyFor(goos string) (CleanupPolicy, bool) {
	switch goos {
	case "linux", "darwin":
		return CleanupPolicy{Grace: CleanupGrace, Limit: CleanupLimit, Poll: CleanupPoll}, true
	}
	return CleanupPolicy{}, false
}

// Signaler sends a signal; a negative pid addresses a process group. It
// has processgroup.Signaler's method set.
type Signaler interface {
	Signal(pid int, sig syscall.Signal) error
}

// ErrInjectedProbe is the deterministic failure of the developer fault
// seam's group-existence probes.
var ErrInjectedProbe = errors.New("mcpqual: injected group-existence probe failure (MCPQUAL_TEST_FAULT=probe-error)")

// FaultSignaler fails every existence probe (signal 0) with
// ErrInjectedProbe and forwards TERM and KILL to Next unchanged.
type FaultSignaler struct{ Next Signaler }

// Signal implements Signaler.
func (f FaultSignaler) Signal(pid int, sig syscall.Signal) error {
	if sig == 0 {
		return ErrInjectedProbe
	}
	return f.Next.Signal(pid, sig)
}

// FaultValue is the only supported developer fault.
const FaultValue = "probe-error"

// SignalerFor selects the cleanup Signaler for the developer fault
// variable's value, read by cmd/mcpqual only: empty selects real, the
// supported value wraps real in a FaultSignaler, anything else is an
// invalid_argument.
func SignalerFor(fault string, real Signaler) (Signaler, error) {
	switch fault {
	case "":
		return real, nil
	case FaultValue:
		return FaultSignaler{Next: real}, nil
	}
	return nil, contract.New(contract.CodeInvalidArgument, fmt.Sprintf("unsupported MCPQUAL_TEST_FAULT %q (the only fault is %q)", fault, FaultValue))
}
