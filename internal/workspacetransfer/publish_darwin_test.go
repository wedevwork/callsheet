//go:build darwin

package workspacetransfer

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// TestFsyncFDFallback drives fsyncFD's fallback: on a pipe F_FULLFSYNC
// fails (it applies to files on a device), so fsyncFD must return exactly
// what a plain fsync of that descriptor returns.
func TestFsyncFDFallback(t *testing.T) {
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(p[0])
	defer unix.Close(p[1])
	if _, err := unix.FcntlInt(uintptr(p[1]), unix.F_FULLFSYNC, 0); err == nil {
		t.Fatal("F_FULLFSYNC succeeded on a pipe: the fallback is not exercised")
	}
	got, want := fsyncFD(p[1]), unix.Fsync(p[1])
	if got != want && !errors.Is(got, want) {
		t.Fatalf("fsyncFD on a pipe = %v, fsync = %v", got, want)
	}
	// A regular file takes the F_FULLFSYNC path and succeeds.
	f := t.TempDir() + "/f"
	fd, err := unix.Open(f, unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := fsyncFD(fd); err != nil {
		t.Fatalf("fsyncFD on a file: %v", err)
	}
}
