//go:build linux

package sidecar

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// TestGroupAloneLinux is UT-A2 of iteration 10a for the Linux primitive
// through its injected prctl and wait4 (no real subreaper or reap runs in
// this test process: either would change other tests' children; the real
// fork and adoption run in tests/function's TestTaskFastGroupCleanup).
func TestGroupAloneLinux(t *testing.T) {
	t.Parallel()
	t.Run("subreaper", func(t *testing.T) {
		t.Parallel()
		var got []uintptr
		var opt int
		ok := func(option int, a2, a3, a4, a5 uintptr) error {
			opt, got = option, []uintptr{a2, a3, a4, a5}
			return nil
		}
		if err := setChildSubreaper(ok); err != nil || opt != unix.PR_SET_CHILD_SUBREAPER || unix.PR_SET_CHILD_SUBREAPER != 36 || got[0] != 1 || got[1]|got[2]|got[3] != 0 {
			t.Fatalf("prctl(%d, %v): %v", opt, got, err)
		}
		fail := func(int, uintptr, uintptr, uintptr, uintptr) error { return unix.EINVAL }
		if err := setChildSubreaper(fail); !errors.Is(err, unix.EINVAL) {
			t.Fatalf("failure %v", err)
		}
	})
	t.Run("probe", func(t *testing.T) {
		t.Parallel()
		type step struct {
			pid int
			err error
		}
		for name, c := range map[string]struct {
			steps []step
			want  groupState
			err   bool
			calls int
		}{
			"no-child":     {[]step{{0, unix.ECHILD}}, groupAlone, false, 1},
			"live-child":   {[]step{{0, nil}}, groupBusy, false, 1},
			"reaped-then":  {[]step{{901, nil}, {902, nil}, {0, unix.ECHILD}}, groupAlone, false, 3},
			"reaped-live":  {[]step{{901, nil}, {0, nil}}, groupBusy, false, 2},
			"interrupted":  {[]step{{0, unix.EINTR}, {0, unix.EINTR}, {0, unix.ECHILD}}, groupAlone, false, 3},
			"always-eintr": {[]step{{0, unix.EINTR}}, groupUnknown, true, maxProbeRetries + 1},
			"other-error":  {[]step{{0, unix.EINVAL}}, groupUnknown, true, 1},
			"invalid-pid":  {[]step{{-7, nil}}, groupUnknown, true, 1},
		} {
			calls := 0
			wait4 := func(pid int, ws *unix.WaitStatus, options int, ru *unix.Rusage) (int, error) {
				if pid != -1 || options != unix.WNOHANG || ws == nil || ru != nil {
					t.Fatalf("%s: wait4(%d, %v, %d, %v)", name, pid, ws, options, ru)
				}
				s := c.steps[min(calls, len(c.steps)-1)]
				calls++
				return s.pid, s.err
			}
			st, err := reapAndProbe(wait4, 4242)
			if st != c.want || (err != nil) != c.err || calls != c.calls {
				t.Fatalf("%s: %v %v after %d calls, want %v (error %v) after %d", name, st, err, calls, c.want, c.err, c.calls)
			}
		}
		// An invalid group is never probed (the native default included).
		for _, pgid := range []int{0, 1, -5} {
			if st, err := reapAndProbe(func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) {
				t.Fatal("probed an invalid group")
				return 0, nil
			}, pgid); st != groupUnknown || err == nil {
				t.Fatalf("pgid %d: %v %v", pgid, st, err)
			}
		}
		if st, err := probeGuardianGroup(1); st != groupUnknown || err == nil {
			t.Fatalf("native default on pgid 1: %v %v", st, err)
		}
	})
}
