package sidecar

import (
	"os"
)

// The task child process boundary (iteration 05): a sidecar-private
// interface following the existing Runner argv/env/dir/stdout/stderr
// convention, plus Start, PID and Wait. Production uses exec with a new
// process group (task_process_unix.go); tests inject handles whose
// "child" is a joined goroutine on real OS pipes.

// procSpec is one child to start. The three *os.File are the child's pipe
// ends; the process owns them from Start on and closes them once the
// child holds its own copies (or, for an injected child, when it ends).
type procSpec struct {
	path   string
	argv   []string // arguments only
	env    []string
	dir    string
	stdin  *os.File
	stdout *os.File
	stderr *os.File
}

// procExit is a reaped child's outcome: its exit code, or the wire name
// of the signal that ended it, or a wait error.
type procExit struct {
	code   int
	signal string
	err    error
}

// taskProc is one task child.
type taskProc interface {
	// Start starts the child in its own process group (PGID = PID) and
	// hands its pipe ends over; an error means no child exists.
	Start() error
	// PID is the child's process ID (and process group ID) after Start.
	PID() int
	// Wait blocks until the direct child exits and returns its outcome.
	Wait() procExit
}

// procFactory creates a child for spec without starting it.
type procFactory func(spec procSpec) taskProc

// groupCleaner tears down a known run-owned task process group.
type groupCleaner interface {
	// cleanup runs after the direct child was reaped: when the group
	// still has members it sends TERM, allows the grace, sends KILL and
	// waits (bounded) for disappearance. nil proves the group is gone.
	cleanup(pgid int) error
	// terminate ends a still-running child's group at Run shutdown: TERM,
	// the grace, then KILL unless exited closes first.
	terminate(pgid int, exited <-chan struct{})
}
