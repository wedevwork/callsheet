//go:build linux

package function

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// parentPID reads pid's parent from /proc (0 when unavailable).
func parentPID(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	// The command name is parenthesized and may contain spaces.
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0
	}
	pp, _ := strconv.Atoi(f[1])
	return pp
}

// wantAdoption requires a descendant whose parent exited to be the child
// of the guardian's subreaper and still a member of its group.
func wantAdoption(t *testing.T, pid, guardian int) {
	t.Helper()
	if pp := parentPID(pid); pp != guardian {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		t.Fatalf("descendant %d has parent %d, want the guardian subreaper %d (%q %v)", pid, pp, guardian, b, err)
	}
	if pg, err := unix.Getpgid(pid); err != nil || pg != guardian {
		t.Fatalf("descendant %d in group %d (%v), want %d", pid, pg, err, guardian)
	}
}
