package adapter

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestProbeTraceSeam covers the probe diagnostics seam: a traced fake
// records the deadline, cmd.Start's return and the child's state at the
// deadline check without changing the probe's outcome, and no production
// source constructs one.
func TestProbeTraceSeam(t *testing.T) {
	t.Run("unreachable-from-production", func(t *testing.T) {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		self := filepath.Join(root, "internal", "adapter", "probe_trace.go")
		var users []string
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if n := d.Name(); p != root && (strings.HasPrefix(n, ".") || n == "testdata") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || p == self {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), "NewFakeTraced") {
				users = append(users, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != 0 {
			t.Fatalf("production sources construct a traced fake: %v", users)
		}
	})
	t.Run("passed", func(t *testing.T) {
		dir := t.TempDir()
		tr := &ProbeTrace{}
		before := time.Now()
		if err := NewFakeTraced(dir, tr).Probe(bg, script(t, dir, "fake-adapter", `echo callsheet-fake-probe-v1`)); err != nil {
			t.Fatal(err)
		}
		p := tr.Last()
		start := p.Deadline.Add(-ProbeTimeout)
		if start.Before(before) || p.Armed.Before(start) || p.Started.Before(p.Armed) || p.Waited.Before(p.Started) || p.State == nil || !p.State.Success() {
			t.Fatalf("timing %+v", p)
		}
		s := p.String()
		for _, want := range []string{"deadline start to cmd.Start returned: ", "; to cmd.Wait returned: ", "(deadline 1s)", "child exited successfully before the deadline check: true (exit status 0)"} {
			if !strings.Contains(s, want) {
				t.Errorf("%q lacks %q", s, want)
			}
		}
	})
	t.Run("injected-clock", func(t *testing.T) {
		// The deadline runs on the test's clock: armed at its instant, it
		// never fires while that clock stands still, and a probe that
		// completes before it passes however long it ran in real time. The
		// reported elapsed times stay wall-clock.
		dir := t.TempDir()
		tr := &ProbeTrace{}
		clk := testkit.NewFakeClock(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
		before := time.Now()
		if err := NewFakeTracedClock(dir, tr, clk).Probe(bg, script(t, dir, "fake-adapter", `echo callsheet-fake-probe-v1`)); err != nil {
			t.Fatal(err)
		}
		p := tr.Last()
		if !p.Deadline.Equal(clk.Now().Add(ProbeTimeout)) || p.Armed.Before(before) || p.Started.Before(p.Armed) || p.Waited.Before(p.Started) ||
			p.Waited.Sub(p.Armed) > testWait || p.State == nil || !p.State.Success() {
			t.Fatalf("timing %+v", p)
		}
		if ws := clk.Waiters(); len(ws) != 0 {
			t.Fatalf("timer left: %v", ws)
		}
		if s := p.String(); !strings.Contains(s, "child exited successfully before the deadline check: true (exit status 0)") {
			t.Fatalf("%q", s)
		}
	})
	t.Run("unsuccessful", func(t *testing.T) {
		dir := t.TempDir()
		tr := &ProbeTrace{}
		wantProbeErr(t, NewFakeTraced(dir, tr).Probe(bg, script(t, dir, "fails", `exit 3`)), "exited unsuccessfully")
		if s := tr.Last().String(); !strings.Contains(s, "child exited successfully before the deadline check: false (exit status 3)") {
			t.Fatalf("%q", s)
		}
	})
	t.Run("not-started", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "garbage")
		if err := os.WriteFile(p, []byte("not an executable format\x00\x01"), 0o755); err != nil {
			t.Fatal(err)
		}
		tr := &ProbeTrace{}
		wantProbeErr(t, NewFakeTraced(dir, tr).Probe(bg, p), "cannot be executed")
		if s := tr.Last().String(); s != "deadline start to cmd.Start returned: no child started; no child was waited for" {
			t.Fatalf("%q", s)
		}
	})
	t.Run("no-probe", func(t *testing.T) {
		tr := &ProbeTrace{}
		if s := tr.Last().String(); s != "no probe deadline was armed" {
			t.Fatalf("%q", s)
		}
	})
}
