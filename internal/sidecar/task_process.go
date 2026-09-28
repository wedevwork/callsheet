package sidecar

import (
	"errors"
	"os"
	"syscall"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The task process boundary. Iteration 05 started the adapter itself with
// Setpgid; iteration 06a starts an internal guardian instead (the
// callsheet binary re-executed with GuardianToken) as the task's process
// group leader, and the guardian starts the adapter in its group after the
// release barrier. Production uses exec (task_process_unix.go); tests
// inject guardians whose "adapter" is a joined goroutine on real OS
// pipes, and count every real child.

// GuardianToken is the reserved argv[1] of the internal task guardian
// entrypoint (RunTaskGuardian): cmd/callsheet dispatches it before signal
// handling and the CLI, and the public CLI rejects it.
const GuardianToken = "__callsheet_task_guardian_v1"

// procSpec is the adapter to run: its absolute path, arguments (without
// the program name), environment and working directory, and its three
// pipe ends. The launcher owns the pipe ends from Start on and closes
// them once the child side holds its copies.
type procSpec struct {
	path   string
	argv   []string // arguments only
	env    []string
	dir    string
	stdin  *os.File
	stdout *os.File
	stderr *os.File
}

// procExit is a reaped process's outcome: its exit code, or the wire name
// of the signal that ended it, or a wait error.
type procExit struct {
	code   int
	signal string
	err    error
}

// guardianSpec is one task's guardian launch: the callsheet binary that
// serves the guardian entrypoint, the invocation delivered on its fd 3,
// and the adapter (whose pipe ends become the guardian's fds 7-9).
type guardianSpec struct {
	exe  string
	inv  contract.GuardianInvocation
	proc procSpec
}

// guardianProc is one launched guardian, seen from its parent sidecar.
type guardianProc interface {
	// Start spawns the guardian in its own process group (PGID = PID,
	// never the sidecar's), hands it its descriptors and writes the
	// invocation; an error means no guardian exists.
	Start() error
	// PID is the guardian's process ID (and process group ID).
	PID() int
	// Status delivers the guardian's decoded status messages in order and
	// is closed at the status pipe's EOF (or on a malformed message).
	Status() <-chan contract.GuardianStatus
	// Release writes the single release byte and closes the barrier;
	// Revoke closes it without the byte (never launch). Either once.
	Release()
	Revoke()
	// Stop closes the parent-lifetime pipe: the guardian cleans its
	// group up (TERM, grace, KILL). Idempotent.
	Stop()
	// Wait reaps the guardian (it normally ends by its own group KILL).
	Wait() procExit
}

// guardianFactory creates a guardian for spec without starting it.
type guardianFactory func(spec guardianSpec) guardianProc

// groupWatcher observes a task's process group after its guardian was
// reaped. It never signals: only a guardian signals its own group.
type groupWatcher interface {
	// gone waits (bounded) until the group has no member; nil proves its
	// disappearance (ESRCH). EPERM, including macOS zombie-only EPERM, is
	// never absence.
	gone(pgid int) error
	// exists probes the group once: ESRCH is (false, nil); EPERM or any
	// other error is returned, never proof of absence.
	exists(pgid int) (bool, error)
}

// errNoGuardian reports a control FIFO without a reader: its guardian is
// gone (or not the FIFO's owner any more).
var errNoGuardian = errors.New("no guardian serves the control FIFO")

// commandSender delivers one control command to a task's FIFO: a
// nonblocking open (errNoGuardian when nobody reads it) and one write,
// bounded by a one-second request deadline.
type commandSender func(fifo string, cmd []byte) error

// errPermission is EPERM: never proof of a group's absence.
var errPermission = syscall.EPERM
