//go:build linux

package sidecar

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Iteration 10a's Linux group-completion proof. The guardian becomes a
// child subreaper before it starts the adapter, so a descendant orphaned
// by its parent is adopted by the guardian before that parent can be fully
// reaped: while any task descendant remains, the guardian has a child.
// After the adapter's own Wait joined (never concurrently with it), a
// WNOHANG wait4(-1) loop reaps the adopted descendants that already ended;
// only ECHILD proves that no descendant remains. A descendant that left the
// group still counts (conservative).

// prctlFunc is unix.Prctl's signature; wait4Func is unix.Wait4's (tests
// inject both).
type (
	prctlFunc func(option int, arg2, arg3, arg4, arg5 uintptr) error
	wait4Func func(pid int, wstatus *unix.WaitStatus, options int, rusage *unix.Rusage) (int, error)
)

// enableGuardianSubreaper is the guardian's native subreap: the behavior
// of internal/spikes/processgroup's setSubreaper, through unix.Prctl.
func enableGuardianSubreaper() error { return setChildSubreaper(unix.Prctl) }

// probeGuardianGroup is the guardian's native groupAlone.
func probeGuardianGroup(pgid int) (groupState, error) { return reapAndProbe(unix.Wait4, pgid) }

// setChildSubreaper makes the calling process a child subreaper.
func setChildSubreaper(prctl prctlFunc) error {
	return prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

// reapAndProbe reaps every ended child without blocking and reports the
// group alone only at ECHILD (no child at all), busy at a live child, and
// unknown at any other error; EINTR is retried a bounded number of times.
// pgid is the guardian's own group, which every remaining child of the
// guardian would keep anchored; the proof does not depend on it.
func reapAndProbe(wait4 wait4Func, pgid int) (groupState, error) {
	if pgid <= 1 {
		return groupUnknown, errors.New("invalid process group")
	}
	interrupted := 0
	for {
		var ws unix.WaitStatus
		pid, err := wait4(-1, &ws, unix.WNOHANG, nil)
		switch {
		case errors.Is(err, unix.ECHILD):
			return groupAlone, nil
		case errors.Is(err, unix.EINTR):
			if interrupted++; interrupted > maxProbeRetries {
				return groupUnknown, err
			}
		case err != nil:
			return groupUnknown, err
		case pid == 0:
			return groupBusy, nil
		case pid < 0:
			return groupUnknown, errors.New("wait4 returned an invalid process ID")
		}
		// pid > 0: an adopted descendant (or another ended child) was
		// reaped; look again.
	}
}
