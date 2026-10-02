package function

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// TestReplayTrapExecUnderForks: the replay kit's PATH traps can be
// executed as soon as they are written while other goroutines fork (as
// the package's parallel tests do, FP-8's host cross build among them): a
// child forked while a trap's write descriptor is open would hold it and
// make the exec fail with ETXTBSY (golang/go#22315). Every exec must
// reach the trap, which records its launch and exits 127.
func TestReplayTrapExecUnderForks(t *testing.T) {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					exec.Command("/bin/sh", "-c", ":").Run()
				}
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 200; i++ {
		k, err := newReplayKit(t.TempDir(), "/bin/sh", nil)
		if err != nil {
			t.Fatal(err)
		}
		runTrap(t, k)
	}
}

// writeExecutable writes an executable that may be run at once: it is
// created, written and closed while holding syscall.ForkLock for reading
// (forks hold it for writing), so no concurrently forked child can
// inherit its write descriptor, which would make an exec of it fail with
// ETXTBSY until that child execs (golang/go#22315).
func writeExecutable(path string, data []byte) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(path, data, 0o755)
}

// runTrap executes the kit's claude trap and requires its own exit (127)
// and its recorded launch.
func runTrap(t *testing.T, k *replayKit) {
	t.Helper()
	cmd := &exec.Cmd{Path: filepath.Join(k.traps, "claude"), Args: []string{"claude", "--version"}, Env: []string{"PATH=" + k.traps}}
	var ee *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &ee) || ee.ExitCode() != 127 {
		t.Fatalf("the trap did not run and refuse (exit 127): %v", err)
	}
	if b, _ := os.ReadFile(k.trapLog); string(b) != "claude --version\n" {
		t.Fatalf("a trap does not record a PATH launch: %q", b)
	}
}
