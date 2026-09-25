//go:build linux || darwin

package devcheck

import (
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// A Go source name that is not a regular file (here a named pipe) is
// rejected with a path diagnostic and is never opened: reading a FIFO would
// block. Runs on both supported hosts (mkfifo exists on linux and darwin).
func TestPlatformGuardNonRegularSource(t *testing.T) {
	root := policyTree(t, nil)
	fifo := filepath.Join(root, "internal", "x", "pipe.go")
	if err := syscall.Mkdir(filepath.Dir(fifo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatalf("mkfifo on %s: %v", runtime.GOOS, err)
	}
	requireViolations(t, "fifo on "+runtime.GOOS, guardViolations(t, root),
		"internal/x/pipe.go: Go source is not a regular file")
}
