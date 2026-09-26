//go:build linux

package processgroup

import (
	"syscall"
	"time"
)

// prSetChildSubreaper is Linux prctl option PR_SET_CHILD_SUBREAPER.
const prSetChildSubreaper = 36

// setSubreaper makes this process a child subreaper so orphaned descendants
// reparent to it. Raw syscall use is confined to this file.
func setSubreaper() (bool, error) {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		return false, errno
	}
	return true, nil
}

// reapAdopted waits for an adopted descendant with Wait4. It must only run
// after the leader has been reaped (the descendant is then our child), so it
// never races Cmd.Wait for the same pid.
func reapAdopted(pid int, _ <-chan struct{}) ProcStatus {
	var ws syscall.WaitStatus
	for {
		wpid, err := syscall.Wait4(pid, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return ProcStatus{PID: pid, Err: "wait4: " + err.Error()}
		}
		if wpid == pid {
			return statusFromWait(pid, ws, "subreaper")
		}
	}
}

// reapGroup reaps adopted, already-killed members of group pgid without
// blocking, until none remain as our children or limit passes (failure paths
// only, after the leader has been reaped).
func reapGroup(pgid int, limit time.Duration) {
	deadline := time.Now().Add(limit)
	var ws syscall.WaitStatus
	for {
		pid, err := syscall.Wait4(-pgid, &ws, syscall.WNOHANG, nil)
		switch {
		case err == syscall.EINTR:
			continue
		case err != nil:
			return // ECHILD: no children left in the group
		case pid > 0:
			continue
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
