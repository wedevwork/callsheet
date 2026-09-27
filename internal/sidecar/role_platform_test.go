package sidecar

import (
	"context"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// manualReason returns the check's safe reason ("" for success).
func manualReason(t *testing.T, d *deps, p string) string {
	t.Helper()
	err := d.checkManual(bg, "instruction", p)
	if err == nil {
		return ""
	}
	ce, ok := err.(*contract.Error)
	if !ok {
		t.Fatalf("%s: %v", p, err)
	}
	r, _ := ce.Details["reason"].(string)
	return r
}

// TestRolePlatformContract is UT FP-9 for the worker's manual checks on the
// running host (Linux or macOS); its manuals subtest is delegated from
// tests/function (TestRolePlatform/manuals). Native filesystem behavior is
// decided by the host, never by a mode-bit heuristic: permission and case
// cases compare against the actual open result. Do not rename or skip its
// subtests.
func TestRolePlatformContract(t *testing.T) {
	t.Run("manuals", func(t *testing.T) {
		d := testDeps(nil)
		dir := t.TempDir()
		write := func(rel, content string, mode os.FileMode) string {
			p := filepath.Join(dir, rel)
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(content), mode); err != nil {
				t.Fatal(err)
			}
			return p
		}
		regular := write("manuals/instruction.md", "body", 0o644)
		empty := write("manuals/empty.md", "", 0o644)
		spaced := write("my manuals/run book.md", "x", 0o644)
		unicode := write("手册/说明.md", "x", 0o644)
		link := filepath.Join(dir, "link.md")
		os.Symlink(regular, link)
		dirLink := filepath.Join(dir, "dirlink")
		os.Symlink(filepath.Dir(regular), dirLink)
		broken := filepath.Join(dir, "broken.md")
		os.Symlink(filepath.Join(dir, "gone.md"), broken)
		loop := filepath.Join(dir, "loop.md")
		os.Symlink(loop, loop)
		fifo := filepath.Join(dir, "fifo")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Fatal(err)
		}
		// A short socket path: sun_path is small on both systems.
		sockDir, err := os.MkdirTemp("", "cs")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(sockDir)
		sock := filepath.Join(sockDir, "s")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		locked := write("locked.md", "x", 0o000)
		wantLocked := contract.ReasonManualUnreadable
		if openOracle(locked) {
			wantLocked = ""
		}
		for name, c := range map[string]struct{ path, reason string }{
			"regular":        {regular, ""},
			"empty":          {empty, ""},
			"spaces":         {spaced, ""},
			"unicode":        {unicode, ""},
			"symlink":        {link, ""},
			"parent-symlink": {filepath.Join(dirLink, "instruction.md"), ""},
			"missing":        {filepath.Join(dir, "gone.md"), contract.ReasonManualUnreadable},
			"broken-link":    {broken, contract.ReasonManualUnreadable},
			"link-loop":      {loop, contract.ReasonManualUnreadable},
			"not-dir":        {filepath.Join(regular, "x"), contract.ReasonManualUnreadable},
			"directory":      {dir, contract.ReasonManualNotRegular},
			"fifo":           {fifo, contract.ReasonManualNotRegular},
			"socket":         {sock, contract.ReasonManualNotRegular},
			"device":         {"/dev/null", contract.ReasonManualNotRegular},
			"mode-000":       {locked, wantLocked},
		} {
			if got := manualReason(t, d, c.path); got != c.reason {
				t.Errorf("%s: reason %q, want %q", name, got, c.reason)
			}
		}
		// Case: the volume decides whether an alternate spelling resolves;
		// Callsheet neither folds nor canonicalizes and agrees with it.
		alt := filepath.Join(filepath.Dir(regular), "INSTRUCTION.md")
		_, statErr := os.Stat(alt)
		want := ""
		if statErr != nil {
			want = contract.ReasonManualUnreadable
		}
		if got := manualReason(t, d, alt); got != want {
			t.Fatalf("alternate spelling: %q, want %q (host resolves it: %v)", got, want, statErr == nil)
		}
		// A FIFO swapped in after the stat is opened without blocking and
		// refused on the descriptor's type.
		swapped := *d
		swapped.openManual = func(string) (*os.File, error) { return openManual(fifo) }
		done := make(chan string, 1)
		go func() { done <- manualReason(t, &swapped, regular) }()
		select {
		case r := <-done:
			if r != contract.ReasonManualNotRegular {
				t.Fatalf("swapped FIFO: %q", r)
			}
		case <-time.After(testWait):
			t.Fatal("a swapped FIFO blocked the check")
		}
		// An injected EACCES is a deterministic oracle.
		denied := *d
		denied.openManual = func(p string) (*os.File, error) {
			return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EACCES}
		}
		if got := manualReason(t, &denied, regular); got != contract.ReasonManualUnreadable {
			t.Fatalf("EACCES: %q", got)
		}
		// Manuals changed or deleted after registration: the next cycle
		// reports the role false.
		script := newScript()
		env := roleEnv{adapters: script.registry(), executables: map[string]string{adapter.FakeID: fakeExe}}
		ins := write("r/instruction.md", "x", 0o644)
		run := write("r/runbook.md", "x", 0o644)
		roles := []contract.RoleRecord{{RoleConfig: roleConfig("r", ins, run), RegistrationOrder: 1}}
		if p := d.checkCycle(bg, env, roles); !p[0] {
			t.Fatal("registered manuals not ready")
		}
		os.Remove(run)
		if p := d.checkCycle(bg, env, roles); p[0] {
			t.Fatal("deleted runbook still ready")
		}
		os.Mkdir(run, 0o755)
		if p := d.checkCycle(bg, env, roles); p[0] {
			t.Fatal("runbook replaced by a directory still ready")
		}
		os.Remove(run)
		os.WriteFile(run, []byte("again"), 0o644)
		if p := d.checkCycle(bg, env, roles); !p[0] {
			t.Fatal("restored runbook not ready")
		}
		// A canceled cycle reports nothing ready.
		ctx, cancel := context.WithCancel(bg)
		cancel()
		if p := d.checkCycle(ctx, env, roles); p[0] {
			t.Fatal("canceled cycle passed")
		}
	})
}

// BenchmarkRoleReadyChecks measures one ready-check cycle over 1 and 100
// roles with local regular manuals and an injected fixed-success probe:
// every iteration asserts two manual checks per role, one probe for the
// shared executable, all roles ready, and that nothing but readiness leaves
// the check.
func BenchmarkRoleReadyChecks(b *testing.B) {
	for _, n := range []int{1, 100} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			dir := b.TempDir()
			var roles []contract.RoleRecord
			for i := 1; i <= n; i++ {
				ins := filepath.Join(dir, "i"+strconv.Itoa(i))
				run := filepath.Join(dir, "r"+strconv.Itoa(i))
				os.WriteFile(ins, []byte("instruction body"), 0o644)
				os.WriteFile(run, []byte("runbook body"), 0o644)
				roles = append(roles, contract.RoleRecord{RoleConfig: roleConfig("role-"+strconv.Itoa(i), ins, run), RegistrationOrder: i})
			}
			var manuals, probes atomic.Int64
			d := testDeps(nil)
			d.observeCheck = func(kind, _ string) {
				if kind == "manual" {
					manuals.Add(1)
				} else {
					probes.Add(1)
				}
			}
			script := newScript()
			script.started = make(chan string, 1<<20)
			env := roleEnv{adapters: script.registry(), executables: map[string]string{adapter.FakeID: fakeExe}}
			b.ReportAllocs()
			for b.Loop() {
				manuals.Store(0)
				probes.Store(0)
				passed := d.checkCycle(bg, env, roles)
				if len(passed) != n || manuals.Load() != int64(2*n) || probes.Load() != 1 {
					b.Fatalf("checks: %d manual, %d probes, %d results", manuals.Load(), probes.Load(), len(passed))
				}
				for i, ok := range passed {
					if !ok {
						b.Fatalf("role %d not ready", i)
					}
				}
				for len(script.started) > 0 {
					<-script.started
				}
			}
		})
	}
}
