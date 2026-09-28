//go:build linux || darwin

package sidecar

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/contract"
)

// guardianFIFO is TestControlCancel/guardian-fifo (review C6): the real
// control boundary of an exec-backed guardian (execGuardian.Control over
// its task directory's FIFO), without a guardian process: a missing FIFO,
// a FIFO nobody reads (no guardian), a non-FIFO in its place and an
// unencodable stop ID fail; with a reader the exact cancel command (task,
// nonce, cause cancelled and the stop ID, one LF-terminated line) arrives.
func guardianFIFO(t *testing.T) {
	dir := t.TempDir()
	st := contract.ExecutionToken{Epoch: strings.Repeat("0", 32), Attachment: 1}
	g := &execGuardian{spec: guardianSpec{inv: contract.GuardianInvocation{TaskID: taskID(1), Execution: st, Nonce: ctlNonce, TaskDir: dir}}}
	stop := strings.Repeat("7", 32)
	fifo := filepath.Join(dir, controlName)
	if err := g.Control(stop); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing FIFO: %v", err)
	}
	if err := g.Control("not-a-stop-id"); err == nil {
		t.Fatal("an invalid stop ID was sent")
	}
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.Control(stop); !errors.Is(err, errNoGuardian) {
		t.Fatalf("FIFO without a reader: %v", err)
	}
	r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := g.Control(stop); err != nil {
		t.Fatalf("delivery: %v", err)
	}
	buf := make([]byte, contract.MaxGuardianCommandBytes)
	n, err := r.Read(buf)
	if err != nil || n == 0 || buf[n-1] != '\n' || bytes.Count(buf[:n], []byte("\n")) != 1 {
		t.Fatalf("command bytes %q %v", buf[:n], err)
	}
	c, err := contract.ParseGuardianCommand(buf[:n-1])
	if err != nil || c.TaskID != taskID(1) || c.Nonce != ctlNonce || c.Command != contract.GuardianStop || c.Cause != contract.CauseCancelled || c.StopID != stop {
		t.Fatalf("command %+v %v", c, err)
	}
	// A regular file where the FIFO belongs is refused before any write.
	r.Close()
	os.Remove(fifo)
	if err := os.WriteFile(fifo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.Control(stop); err == nil || !strings.Contains(err.Error(), "is not a FIFO") {
		t.Fatalf("regular file: %v", err)
	}
	if b, _ := os.ReadFile(fifo); len(b) != 0 {
		t.Fatalf("wrote %q into a regular file", b)
	}
}
