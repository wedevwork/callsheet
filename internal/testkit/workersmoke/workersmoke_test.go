package workersmoke

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

type info struct{ mode fs.FileMode }

func (i info) Name() string       { return "x" }
func (i info) Size() int64        { return 1 }
func (i info) Mode() fs.FileMode  { return i.mode }
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return i.mode.IsDir() }
func (i info) Sys() any           { return nil }

func gate(env map[string]string, files map[string]fs.FileMode, statErr error) (Gate, *int) {
	n := 0
	return Gate{Getenv: func(k string) string { return env[k] }, Stat: func(p string) (fs.FileInfo, error) {
		n++
		if statErr != nil {
			return nil, statErr
		}
		if m, ok := files[p]; ok {
			return info{m}, nil
		}
		return nil, fs.ErrNotExist
	}}, &n
}

// TestGate covers every smoke and vendor decision on an injected
// environment and stat.
func TestGate(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		run  bool
		want string
	}{
		{map[string]string{}, false, "opt-in"},
		{map[string]string{EnvOptIn: "yes"}, false, "opt-in"},
		{map[string]string{EnvOptIn: "1", EnvCI: "1"}, false, "CI is set"},
		{map[string]string{EnvOptIn: "1", EnvCI: "true"}, false, "never runs in CI"},
		{map[string]string{EnvOptIn: "1"}, true, ""},
	} {
		g, n := gate(c.env, nil, nil)
		if d := g.Smoke(); d.Run != c.run || !strings.Contains(d.Reason, c.want) || *n != 0 {
			t.Fatalf("%v: %+v (%d stats)", c.env, d, *n)
		}
	}
	if PathEnv("claude") != "CALLSHEET_CLAUDE_PATH" || PathEnv("codex") != "CALLSHEET_CODEX_PATH" || PathEnv("grok") != "" {
		t.Fatal("path variables")
	}
	files := map[string]fs.FileMode{"/bin/claude": 0o755, "/bin/plain": 0o644, "/bin/dir": fs.ModeDir | 0o755}
	for _, c := range []struct {
		id, path          string
		run               bool
		skip, fail, statE string
	}{
		{"claude", "", false, "is unset", "", ""},
		{"claude", "claude", false, "", "must be an absolute path", ""},
		{"claude", "/bin/claude", true, "", "", ""},
		{"codex", "/bin/missing", false, "absent on this machine", "", ""},
		{"codex", "/bin/plain", false, "", "no executable permission bit", ""},
		{"codex", "/bin/dir", false, "", "not a regular file", ""},
		{"codex", "/bin/claude", false, "", "cannot be inspected", "boom"},
		{"grok", "/bin/claude", false, "", "unknown vendor", ""},
	} {
		var serr error
		if c.statE != "" {
			serr = errors.New(c.statE)
		}
		g, _ := gate(map[string]string{PathEnv(c.id): c.path}, files, serr)
		d := g.Vendor(c.id)
		if d.Run != c.run || (c.skip == "") != (d.Skip == "") || !strings.Contains(d.Skip, c.skip) || (c.fail == "") != (d.Fail == "") || !strings.Contains(d.Fail, c.fail) {
			t.Fatalf("%s %q: %+v", c.id, c.path, d)
		}
	}
	if g := OSGate(); g.Getenv == nil || g.Stat == nil {
		t.Fatal("OSGate")
	}
	if !strings.Contains(Command, "-tags="+Tag+" ./tests/smoke") || OuterBound != 2*time.Minute {
		t.Fatal("the documented command or bound changed")
	}
}

// fakeClaude is a shell stand-in for the claude CLI: the qualified version
// line, and for a task one JSON result object ("pong"), or a hang when the
// composed prompt asks for one.
const fakeClaude = `#!/bin/sh
if [ "$1" = --version ]; then echo '2.1.285 (Claude Code)'; exit 0; fi
if grep -q hang-forever; then exec sleep 30; fi
printf '{"type":"result","is_error":false,"result":"pong"}'
`

// TestDeployment runs the harness end to end with the stand-in: a real
// plane and sidecar from the built callsheet binary, the qualified role,
// a completed task, and a task cancelled at its outer bound.
func TestDeployment(t *testing.T) {
	dir := t.TempDir()
	bin, err := testkit.BuildBinaryAt(dir, "./cmd/callsheet", "callsheet")
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "vendor bin", "claude")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	if err := os.WriteFile(exe, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := Run(ctx, bin, filepath.Join(dir, "run"), env, env, "claude", exe, "reply pong")
	if err != nil || res.View.State != contract.TaskSucceeded || res.View.Result.FinalMessage == nil || *res.View.Result.FinalMessage != "pong" ||
		string(res.Logs) != `{"type":"result","is_error":false,"result":"pong"}` {
		t.Fatalf("run %+v %q %v", res.View, res.Logs, err)
	}
	// The outer bound: the task is cancelled and awaited.
	d, err := Start(ctx, bin, filepath.Join(dir, "bound"), env)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s, err := d.StartSidecar(ctx, "worker", env, "--claude-adapter", exe)
	if err != nil {
		t.Fatal(err)
	}
	if s.PID() <= 0 || s.NodeID == "" || !strings.HasSuffix(s.State, "worker") {
		t.Fatalf("sidecar %+v", s)
	}
	// A restart keeps the node and its state, with a new process.
	old := s.PID()
	if err := d.Restart(ctx, s); err != nil || s.PID() == old || !strings.Contains(s.Logs.String(), "heartbeat acknowledged") {
		t.Fatalf("restart: %v (pid %d, was %d)", err, s.PID(), old)
	}
	ins, run, err := d.Manuals("claude", SmokeInstruction, SmokeRunbook)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AddVendorRole(ctx, "held", "claude", s.NodeID, ins, run, 1); err != nil {
		t.Fatal(err)
	}
	res, err = d.Dispatch(ctx, "held", "hang-forever", time.Second)
	if !errors.Is(err, ErrOuterBound) || res.View.State != contract.TaskCancelled {
		t.Fatalf("bounded %+v %v", res.View, err)
	}
	// Refusals: an unqualified vendor, a missing role, a broken binary.
	if err := d.AddVendorRole(ctx, "x", "grok", s.NodeID, ins, run, 1); err == nil {
		t.Fatal("an unqualified vendor was registered")
	}
	if _, err := d.Dispatch(ctx, "nobody", "g", time.Second); err == nil {
		t.Fatal("a dispatch to a missing role succeeded")
	}
	if _, err := d.StartSidecar(ctx, "bad", env, "--claude-adapter", "relative"); err == nil {
		t.Fatal("a sidecar with a relative adapter path started")
	}
	if _, err := Start(ctx, filepath.Join(dir, "missing"), filepath.Join(dir, "none"), env); err == nil {
		t.Fatal("a missing binary started")
	}
	if _, err := Run(ctx, bin, filepath.Join(dir, "unready"), env, env, "codex", exe, "g"); err == nil {
		t.Fatal("a codex smoke through a claude stand-in passed")
	}
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	if _, err := d.Dispatch(cctx, "held", "g", time.Second); err == nil {
		t.Fatal("a canceled dispatch succeeded")
	}
	if !strings.Contains(s.Logs.String(), "heartbeat acknowledged") {
		t.Fatal("no sidecar logs captured")
	}
	// Stop is idempotent (Close stops it again).
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
}

// TestDeploymentConcurrentStart starts sidecars on one deployment from
// concurrent goroutines, as the function suite's parallel parents do: every
// started sidecar is retained, and Close stops every one of them (run it
// under -race: the child list is shared).
func TestDeploymentConcurrentStart(t *testing.T) {
	const n = 8
	dir := t.TempDir()
	bin, err := testkit.BuildBinaryAt(dir, "./cmd/callsheet", "callsheet")
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "claude")
	if err := os.WriteFile(exe, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	d, err := Start(ctx, bin, filepath.Join(dir, "run"), env)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			d.Close()
		}
	}()
	started := make([]*Sidecar, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			started[i], errs[i] = d.StartSidecar(ctx, fmt.Sprintf("worker-%d", i), env, "--claude-adapter", exe)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("sidecar %d: %v", i, err)
		}
	}
	d.mu.Lock()
	retained := len(d.sidecars)
	d.mu.Unlock()
	if retained != n {
		t.Fatalf("retained %d of %d started sidecars", retained, n)
	}
	closed = true
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	for i, s := range started {
		select {
		case <-s.done:
		default:
			t.Fatalf("sidecar %d (pid %d) still running after Close", i, s.PID())
		}
	}
}
