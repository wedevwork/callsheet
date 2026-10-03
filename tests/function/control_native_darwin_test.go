//go:build darwin

package function

import (
	"testing"

	"golang.org/x/sys/unix"
)

// parentPID reads pid's parent through sysctl (0 when unavailable).
func parentPID(pid int) int {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0
	}
	return int(kp.Eproc.Ppid)
}

// wantAdoption requires a descendant whose parent exited to remain a
// member of the guardian's group (macOS has no subreaper: the group's
// process list is the proof's subject).
func wantAdoption(t *testing.T, pid, guardian int) {
	t.Helper()
	if pg, err := unix.Getpgid(pid); err != nil || pg != guardian {
		t.Fatalf("descendant %d in group %d (%v), want %d", pid, pg, err, guardian)
	}
}
