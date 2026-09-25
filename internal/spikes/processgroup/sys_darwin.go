//go:build darwin

package processgroup

import (
	"time"
)

// setSubreaper: macOS has no prctl/subreaper; orphans reparent to launchd.
func setSubreaper() (bool, error) { return false, nil }

// reapAdopted waits for the descendant-owned lifetime pipe to close and then
// polls for nonexistence (ESRCH) up to ReapLimit while launchd reaps it. The
// wait status is unavailable on Darwin.
func reapAdopted(pid int, lifetimeClosed <-chan struct{}) ProcStatus {
	<-lifetimeClosed
	if err := WaitGone(SysSignaler{}, RealClock{}, ReapLimit, 5*time.Millisecond, pid); err != nil {
		return ProcStatus{PID: pid, Err: err.Error(), ReapedBy: "launchd"}
	}
	return ProcStatus{PID: pid, ReapedBy: "launchd"}
}

// reapGroup is a no-op on Darwin: launchd reaps orphans.
func reapGroup(int, time.Duration) {}
