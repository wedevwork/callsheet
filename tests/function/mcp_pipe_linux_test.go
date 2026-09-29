//go:build linux

package function

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// pipeCapacity is the kernel capacity of the pipe behind f (F_GETPIPE_SZ).
func pipeCapacity(t *testing.T, f *os.File) int {
	t.Helper()
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	n, ferr := 0, error(nil)
	if err := rc.Control(func(fd uintptr) { n, ferr = unix.FcntlInt(fd, unix.F_GETPIPE_SZ, 0) }); err != nil || ferr != nil {
		t.Fatalf("pipe capacity: %v %v", err, ferr)
	}
	return n
}
