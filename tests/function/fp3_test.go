package function

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestFP3FakeWorker invokes the built fake with quoted/Unicode argv,
// stdout/stderr, an edit and a nonzero exit, and checks exact bytes, the
// duration bound measured from ready acknowledgment, and containment.
func TestFP3FakeWorker(t *testing.T) {
	fake := testkit.BuildBinary(t, "./cmd/fake-adapter", "fake-adapter")
	env := os.Environ()

	t.Run("output edit duration exit", func(t *testing.T) {
		parent := t.TempDir()
		work := mkdir(t, filepath.Join(parent, "work"))
		content := "結果 \"exact\"\nline two\n"
		args := []string{
			"--echo-argv", "--stdout=你好", "--stderr=diagnostic", "--duration=100ms", "--exit-code=17",
			"--edit=sub/result.txt", "--content=" + content, "--ready-file=ready.json",
			"--model", "model with spaces", "--effort=high",
			"--", `an argument with spaces and "quotes"`, "ünïcødé ✓", "",
		}
		cmd := exec.Command(fake, args...)
		cmd.Dir = work
		cmd.Env = env
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		ready := filepath.Join(work, "ready.json")
		var readyAt time.Time
		deadline := time.Now().Add(10 * time.Second)
		for readyAt.IsZero() {
			if _, err := os.Stat(ready); err == nil {
				readyAt = time.Now()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("fake never published readiness")
			}
			time.Sleep(100 * time.Microsecond)
		}
		err := <-exited
		exitAt := time.Now()
		// Measure from the ready acknowledgment itself: the ready file's
		// mtime is set when the fake writes it (the kernel's coarse clock
		// never runs ahead), and the fake starts its duration only after
		// publishing it, so polling latency cannot shorten the measurement.
		st, serr := os.Stat(ready)
		if serr != nil {
			t.Fatal(serr)
		}
		if st.ModTime().After(readyAt) {
			t.Fatalf("ready mtime %v after observation %v", st.ModTime(), readyAt)
		}
		elapsed := exitAt.Sub(st.ModTime())
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 17 {
			t.Fatalf("exit = %v", err)
		}
		if elapsed < 100*time.Millisecond || elapsed >= 5*time.Second {
			t.Fatalf("ready-to-exit = %v, want [100ms, 5s)", elapsed)
		}
		var argvJSON bytes.Buffer
		enc := json.NewEncoder(&argvJSON)
		enc.SetEscapeHTML(false)
		enc.Encode(map[string][]string{"argv": args})
		if want := argvJSON.String() + "你好\n"; out.String() != want {
			t.Fatalf("stdout = %q\nwant   %q", out.String(), want)
		}
		var echoed struct{ Argv []string }
		json.Unmarshal([]byte(strings.SplitN(out.String(), "\n", 2)[0]), &echoed)
		if len(echoed.Argv) != len(args) || echoed.Argv[len(args)-3] != `an argument with spaces and "quotes"` || echoed.Argv[len(args)-1] != "" {
			t.Fatalf("argv boundaries lost: %q", echoed.Argv)
		}
		if errOut.String() != "diagnostic\n" {
			t.Fatalf("stderr = %q", errOut.String())
		}
		b, err := os.ReadFile(filepath.Join(work, "sub", "result.txt"))
		if err != nil || string(b) != content {
			t.Fatalf("edit = %q %v", b, err)
		}
		var info struct {
			PID           int `json:"pid"`
			DescendantPID int `json:"descendant_pid"`
		}
		rb, _ := os.ReadFile(ready)
		if json.Unmarshal(rb, &info) != nil || info.PID != cmd.Process.Pid || info.DescendantPID != 0 {
			t.Fatalf("ready = %s", rb)
		}
		if got := strings.Join(listTree(t, parent), ","); got != strings.Join([]string{"work", filepath.Join("work", "ready.json"), filepath.Join("work", "sub"), filepath.Join("work", "sub", "result.txt")}, ",") {
			t.Fatalf("files = %s", got)
		}
	})

	t.Run("traversal rejected", func(t *testing.T) {
		parent := t.TempDir()
		work := mkdir(t, filepath.Join(parent, "work"))
		for _, p := range []string{"../escape.txt", "sub/../../escape.txt", filepath.Join(parent, "abs.txt")} {
			r := runBin(t, fake, work, env, "--edit="+p, "--content=x")
			if r.code != 2 || r.stdout != "" {
				t.Fatalf("%s = %+v", p, r)
			}
		}
		if got := listTree(t, parent); len(got) != 1 {
			t.Fatalf("traversal wrote files: %v", got)
		}
	})

	t.Run("symlink escape rejected", func(t *testing.T) {
		work := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(work, "link")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		r := runBin(t, fake, work, env, "--edit=link/escaped.txt", "--content=x")
		if r.code != 1 || !strings.Contains(r.stderr, "fake-adapter: edit") {
			t.Fatalf("symlink escape = %+v", r)
		}
		if got := listTree(t, outside); len(got) != 0 {
			t.Fatalf("wrote outside cwd: %v", got)
		}
	})

	t.Run("zero duration", func(t *testing.T) {
		start := time.Now()
		r := runBin(t, fake, t.TempDir(), env, "--duration=0s", "--exit-code=3", "--stdout=done")
		if r.code != 3 || r.stdout != "done\n" || time.Since(start) >= 5*time.Second {
			t.Fatalf("zero duration = %+v", r)
		}
	})

	t.Run("option error without side effects", func(t *testing.T) {
		work := t.TempDir()
		for _, args := range [][]string{
			{"--exit-code=126", "--edit=f.txt", "--content=x", "--stdout=x"},
			{"--duration=soon", "--edit=f.txt"},
			{"--term-mode=stop", "--edit=f.txt"},
			{"--unknown-flag", "--edit=f.txt"},
			{"--edit"},
		} {
			r := runBin(t, fake, work, env, args...)
			if r.code != 2 || r.stdout != "" || !strings.HasPrefix(r.stderr, "fake-adapter: ") {
				t.Fatalf("%v = %+v", args, r)
			}
		}
		if got := listTree(t, work); len(got) != 0 {
			t.Fatalf("option errors had side effects: %v", got)
		}
	})
}
