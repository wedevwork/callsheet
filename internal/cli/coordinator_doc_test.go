//go:build unix

package cli

import (
	"bufio"
	"os"
	oexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// UT-5 (design nonblocking-coordinator-waits, FP-5): the coordinator
// guide's single-line wrapper, extracted from docs/coordinator.md and run
// by /bin/sh with a fake callsheet on PATH: a winner publishes the result,
// status 0 and one notification line; an error publishes its status and
// diagnostic and still notifies (no set -e); a wrapper killed before
// publishing leaves no status file, which the harness treats as an
// interrupted watcher. Nothing from the wait itself reaches stdout.

// guideWrapper returns the wrapper script of the guide's runbook example.
func guideWrapper(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "docs", "coordinator.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "\n## Coordinator runbook example\n")
	if i < 0 {
		t.Fatal("no runbook example")
	}
	m := regexp.MustCompile("(?s)```sh\n(callsheet task wait .*?)\n```").FindStringSubmatch(doc[i:])
	if m == nil {
		t.Fatal("no wrapper in the runbook example")
	}
	return m[1]
}

// writeFake writes an executable under syscall.ForkLock (read), so no
// concurrently forked child inherits its write descriptor (ETXTBSY,
// golang/go#22315).
func writeFake(t *testing.T, path, script string) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorWrapper(t *testing.T) {
	wrapper := guideWrapper(t)
	notify := regexp.MustCompile(`^callsheet-wait-ended: `)
	run := func(t *testing.T, fake string, kill bool) (string, string, int) {
		t.Helper()
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		bin, waitdir := filepath.Join(dir, "bin"), filepath.Join(dir, "wait")
		for _, d := range []string{bin, waitdir} {
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		writeFake(t, filepath.Join(bin, "callsheet"), fake)
		cmd := oexec.Command("/bin/sh", "-c", wrapper)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "id1=t_1", "id2=t_2", "plane=https://p", "ca=/ca", "handle=h7", "waitdir=" + waitdir,
			"STARTED=" + filepath.Join(dir, "started")}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if kill {
			// Kill the whole group once the fake (the wait) is running.
			deadline := time.Now().Add(30 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the fake wait never started")
				}
				time.Sleep(5 * time.Millisecond)
			}
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		cmd.Wait()
		status, _ := os.ReadFile(filepath.Join(waitdir, "exit-code"))
		if line != "" && !notify.MatchString(line) {
			t.Fatalf("notification %q", line)
		}
		return line, string(status), cmd.ProcessState.ExitCode()
	}
	t.Run("winner", func(t *testing.T) {
		const answer = `{"version":6,"status":"terminal","winner":"t_2"}`
		line, status, code := run(t, "#!/bin/sh\necho '"+answer+"'\nexit 0\n", false)
		if line != "callsheet-wait-ended: h7 exit=0\n" || status != "0\n" || code != 0 {
			t.Fatalf("%q %q %d", line, status, code)
		}
	})
	t.Run("error", func(t *testing.T) {
		line, status, code := run(t, "#!/bin/sh\necho 'callsheet: not_found: task t_1 does not exist' >&2\nexit 3\n", false)
		if line != "callsheet-wait-ended: h7 exit=3\n" || status != "3\n" || code != 3 {
			t.Fatalf("%q %q %d", line, status, code)
		}
	})
	t.Run("missing-status", func(t *testing.T) {
		line, status, code := run(t, "#!/bin/sh\n: > \"$STARTED\"\nexec sleep 30\n", true)
		if line != "" || status != "" || code != -1 {
			t.Fatalf("a killed wrapper published %q %q (exit %d)", line, status, code)
		}
	})
	t.Run("files", func(t *testing.T) {
		// The result and diagnostic files hold exactly the wait's own
		// streams; the wrapper's stdout holds only the notification.
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		bin, waitdir := filepath.Join(dir, "bin"), filepath.Join(dir, "wait")
		os.Mkdir(bin, 0o755)
		os.Mkdir(waitdir, 0o755)
		writeFake(t, filepath.Join(bin, "callsheet"), "#!/bin/sh\necho \"$*\"\necho diag >&2\nexit 0\n")
		cmd := oexec.Command("/bin/sh", "-c", wrapper)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "id1=t_1", "id2=t_2", "plane=https://p", "ca=/a b/ca", "handle=h8", "waitdir=" + waitdir}
		out, err := cmd.Output()
		res, _ := os.ReadFile(filepath.Join(waitdir, "result.json"))
		diag, _ := os.ReadFile(filepath.Join(waitdir, "error.txt"))
		if err != nil || string(out) != "callsheet-wait-ended: h8 exit=0\n" || string(res) != "task wait t_1 t_2 --until-done --json --plane https://p --ca /a b/ca\n" ||
			string(diag) != "diag\n" {
			t.Fatalf("%v %q %q %q", err, out, res, diag)
		}
	})
}
