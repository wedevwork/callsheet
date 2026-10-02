//go:build darwin

package sidecar

import (
	"syscall"
	"testing"
	"unsafe"
)

// TestGroupAloneDarwin is UT-A2 of iteration 10a for the macOS primitive
// through its injected Syscall6 (the real group enumeration runs in
// tests/function's TestTaskFastGroupCleanup on macOS).
func TestGroupAloneDarwin(t *testing.T) {
	t.Parallel()
	if err := enableGuardianSubreaper(); err != nil {
		t.Fatalf("subreap %v", err)
	}
	const self, pgid = 4242, 4242
	for name, c := range map[string]struct {
		pids  []int32
		r1    uintptr
		errno syscall.Errno
		want  groupState
		err   bool
	}{
		"alone":          {[]int32{self}, 4, 0, groupAlone, false},
		"another-sole":   {[]int32{77}, 4, 0, groupUnknown, true},
		"two":            {[]int32{self, 77}, 8, 0, groupBusy, false},
		"zero":           {nil, 0, 0, groupUnknown, true},
		"malformed":      {[]int32{self}, 6, 0, groupUnknown, true},
		"oversized":      {[]int32{self, 77}, 12, 0, groupUnknown, true},
		"errno":          {nil, 0, syscall.EPERM, groupUnknown, true},
		"errno-and-size": {[]int32{self}, 4, syscall.ESRCH, groupUnknown, true},
	} {
		calls := 0
		sys := func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
			calls++
			if trap != syscall.SYS_PROC_INFO || syscall.SYS_PROC_INFO != 336 || a1 != procInfoCallListPIDs || procInfoCallListPIDs != 1 || a2 != procPgrpOnly || procPgrpOnly != 2 ||
				a3 != pgid || a4 != 0 || a6 != 8 || a5 == 0 {
				t.Fatalf("%s: syscall(%d, %d, %d, %d, %d, %#x, %d)", name, trap, a1, a2, a3, a4, a5, a6)
			}
			// The buffer the probe passed (reinterpreted, not converted
			// from an integer: it is live for the whole call).
			buf := *(**[2]int32)(unsafe.Pointer(&a5))
			copy(buf[:], c.pids)
			return c.r1, 0, c.errno
		}
		st, err := listGroup(sys, self, pgid)
		if st != c.want || (err != nil) != c.err || calls != 1 {
			t.Fatalf("%s: %v %v (%d calls), want %v (error %v)", name, st, err, calls, c.want, c.err)
		}
	}
	for _, g := range []int{0, 1, -3} {
		if st, err := listGroup(func(uintptr, uintptr, uintptr, uintptr, uintptr, uintptr, uintptr) (uintptr, uintptr, syscall.Errno) {
			t.Fatal("probed an invalid group")
			return 0, 0, 0
		}, self, g); st != groupUnknown || err == nil {
			t.Fatalf("pgid %d: %v %v", g, st, err)
		}
	}
	if st, err := probeGuardianGroup(1); st != groupUnknown || err == nil {
		t.Fatalf("native default on pgid 1: %v %v", st, err)
	}
	// The buffer stays valid across the injected boundary however the
	// callee grows the stack (a fresh goroutine starts small): its write
	// through the passed address is the one listGroup reads.
	done := make(chan groupState, 1)
	go func() {
		st, _ := listGroup(func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
			growStack(4096)
			buf := *(**[2]int32)(unsafe.Pointer(&a5))
			buf[0] = self
			return 4, 0, 0
		}, self, pgid)
		done <- st
	}()
	if st := <-done; st != groupAlone {
		t.Fatalf("after the callee grew its stack the buffer read %v, want alone", st)
	}
}

// growStack recurses with a large frame, forcing the goroutine's stack to
// grow (and move).
//
//go:noinline
func growStack(n int) int {
	var pad [256]byte
	if n == 0 {
		return int(pad[0])
	}
	return growStack(n-1) + int(pad[n%256])
}
