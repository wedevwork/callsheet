package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
)

const testWait = 30 * time.Second

var bg = context.Background()

// The real fake-adapter binary, built at most once per test process.
var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
	fakeDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeDir != "" {
		os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}

func fakeBinary(t testing.TB) string {
	t.Helper()
	fakeOnce.Do(func() {
		fakeDir, fakeErr = os.MkdirTemp("", "callsheet-adapter-fixture-")
		if fakeErr == nil {
			fakePath, fakeErr = testkit.BuildBinaryAt(fakeDir, "./cmd/fake-adapter", "fake-adapter")
		}
	})
	if fakeErr != nil {
		t.Fatalf("building the fake adapter: %v", fakeErr)
	}
	return fakePath
}

// stub is a minimal adapter for registry tests.
type stub struct{ d Descriptor }

func (s stub) Descriptor() Descriptor                   { return s.d }
func (s stub) Probe(context.Context, string) error      { return nil }
func (s stub) Invocation(TaskInput) (Invocation, error) { return Invocation{}, nil }
func (s stub) NewFinalExtractor() FinalExtractor        { return &markerExtractor{} }

// script writes an executable /bin/sh script.
func script(t testing.TB, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// testProber is a prober with a fake clock and child observation.
type testProber struct {
	*prober
	clk     *testkit.FakeClock
	started chan int
	waited  chan *os.ProcessState
}

func newTestProber(t *testing.T, dir string, env []string) *testProber {
	tp := &testProber{clk: testkit.NewFakeClock(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)), started: make(chan int, 4), waited: make(chan *os.ProcessState, 4)}
	tp.prober = &prober{dir: dir, environ: func() []string { return env }, clock: tp.clk,
		started: func(pid int) { tp.started <- pid }, waited: func(ps *os.ProcessState) { tp.waited <- ps }}
	return tp
}

func (tp *testProber) reaped(t *testing.T) *os.ProcessState {
	t.Helper()
	select {
	case ps := <-tp.waited:
		if ps == nil {
			t.Fatal("the child was not waited for")
		}
		return ps
	case <-time.After(testWait):
		t.Fatal("no wait")
		return nil
	}
}

func wantProbeErr(t *testing.T, err error, reason string) {
	t.Helper()
	var pe *ProbeError
	if !errors.As(err, &pe) || !strings.Contains(pe.Reason, reason) {
		t.Fatalf("probe = %v, want %q", err, reason)
	}
}

// TestAdapterContract is UT FP-2: the immutable registry, the fake's
// metadata, explicit-path probing with its bounds, and the environment
// filter.
func TestAdapterContract(t *testing.T) {
	t.Run("registry", func(t *testing.T) {
		good := Descriptor{ID: "codex", Efforts: []string{"low", "high"}}
		for name, ads := range map[string][]Adapter{
			"nil":             {nil},
			"empty id":        {stub{Descriptor{Efforts: []string{"low"}}}},
			"uppercase id":    {stub{Descriptor{ID: "Fake", Efforts: []string{"low"}}}},
			"long id":         {stub{Descriptor{ID: strings.Repeat("a", 64), Efforts: []string{"low"}}}},
			"dotted id":       {stub{Descriptor{ID: "a.b", Efforts: []string{"low"}}}},
			"duplicate id":    {stub{good}, stub{good}},
			"no efforts":      {stub{Descriptor{ID: "x"}}},
			"bad effort":      {stub{Descriptor{ID: "x", Efforts: []string{"Low"}}}},
			"empty effort":    {stub{Descriptor{ID: "x", Efforts: []string{""}}}},
			"repeated effort": {stub{Descriptor{ID: "x", Efforts: []string{"low", "low"}}}},
		} {
			if _, err := NewRegistry(ads...); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		r, err := NewRegistry(stub{good}, NewFake(""))
		if err != nil {
			t.Fatal(err)
		}
		ds := r.Descriptors()
		if len(ds) != 2 || ds[0].ID != "codex" || ds[1].ID != "fake" {
			t.Fatalf("descriptors = %+v", ds)
		}
		// Returned slices are copies: the registry is immutable.
		ds[0].Efforts[0] = "mutated"
		ds[0].ID = "mutated"
		if again := r.Descriptors(); again[0].ID != "codex" || again[0].Efforts[0] != "low" {
			t.Fatalf("registry mutated through a returned slice: %+v", again)
		}
		if _, ok := r.Lookup("FAKE"); ok {
			t.Fatal("lookup is case-insensitive")
		}
		a, ok := r.Lookup("fake")
		if !ok {
			t.Fatal("fake missing")
		}
		fd := a.Descriptor()
		fd.Efforts[0] = "mutated"
		if a.Descriptor().Efforts[0] != "low" {
			t.Fatal("fake descriptor mutable")
		}
		// Concurrent readers are safe.
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 100 {
					r.Lookup("fake")
					r.Descriptors()
				}
			}()
		}
		wg.Wait()
	})
	t.Run("fake", func(t *testing.T) {
		// The fake is unchanged beside the iteration 08 vendors: all three
		// efforts, test-only, no model list.
		ds := Builtin("").Descriptors()
		if len(ds) != 3 || ds[2].ID != FakeID || !slices.Equal(ds[2].Efforts, []string{"low", "medium", "high"}) || !ds[2].TestOnly {
			t.Fatalf("builtin = %+v", ds)
		}
		look := Lookup()
		info, ok := look("fake")
		if !ok || !info.TestOnly || len(info.Efforts) != 3 {
			t.Fatalf("lookup = %+v %v", info, ok)
		}
		if _, ok := look("grok"); ok {
			t.Fatal("unknown adapter found")
		}
		// No model allowlist: any valid model text passes with every effort.
		for _, effort := range []string{"low", "medium", "high"} {
			for _, model := range []string{"gpt-6-sol", "some future model", "-x", "模型"} {
				c := contract.RoleConfig{ID: "a", Name: "a", Node: "n_0123456789abcdef0123456789abcdef", Adapter: "fake", Instruction: "/i", Runbook: "/r", Model: model, Effort: effort, Concurrency: 1}
				if err := contract.ValidateRoleConfig(c, look); err != nil {
					t.Fatalf("%s/%s: %v", model, effort, err)
				}
			}
		}
		// The product and the fixture agree on the probe's argv and output;
		// production never imports the fixture.
		if ProbeArg != fakeadapter.ProbeFlag || ProbeOutput != fakeadapter.ProbeOutput {
			t.Fatal("probe protocol differs from the fixture")
		}
	})
	t.Run("explicit-path", func(t *testing.T) {
		// Only an absolute path is used, as given: a bare name never
		// consults PATH, even when PATH holds a matching executable.
		dir := t.TempDir()
		script(t, dir, "fake-adapter", `echo callsheet-fake-probe-v1`)
		tp := newTestProber(t, dir, []string{"PATH=" + dir})
		for _, p := range []string{"fake-adapter", "./fake-adapter", "", "bin/fake-adapter"} {
			wantProbeErr(t, tp.run(bg, p), "not absolute")
		}
		for name, c := range map[string]struct {
			prep   func() string
			reason string
		}{
			"missing":   {func() string { return filepath.Join(dir, "absent") }, "does not exist"},
			"directory": {func() string { return dir }, "not a regular file"},
			"no x bit": {func() string {
				p := filepath.Join(dir, "plain")
				os.WriteFile(p, []byte("#!/bin/sh\necho callsheet-fake-probe-v1\n"), 0o644)
				return p
			}, "no executable permission bit"},
			"broken link": {func() string {
				p := filepath.Join(dir, "broken")
				os.Symlink(filepath.Join(dir, "gone"), p)
				return p
			}, "does not exist"},
			"bad interpreter": {func() string {
				p := filepath.Join(dir, "badint")
				os.WriteFile(p, []byte("#!/nonexistent/interpreter\n"), 0o755)
				return p
			}, "cannot be executed"},
		} {
			wantProbeErr(t, tp.run(bg, c.prep()), c.reason)
			_ = name
		}
		ctx, cancel := context.WithCancel(bg)
		cancel()
		if err := tp.run(ctx, filepath.Join(dir, "fake-adapter")); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled before start = %v", err)
		}
	})
	t.Run("outcomes", func(t *testing.T) {
		dir := t.TempDir()
		big := strings.Repeat("x", probeCapture+1)
		for name, c := range map[string]struct {
			body, reason string
		}{
			"success":         {`printf 'callsheet-fake-probe-v1\n'`, ""},
			"wrong output":    {`echo callsheet-fake-probe-v2`, "unexpected probe output"},
			"no newline":      {`printf 'callsheet-fake-probe-v1'`, "unexpected probe output"},
			"extra output":    {`printf 'callsheet-fake-probe-v1\nmore\n'`, "unexpected probe output"},
			"stderr":          {`echo callsheet-fake-probe-v1; echo SECRET-STDERR >&2`, "wrote to stderr"},
			"exit code":       {`echo callsheet-fake-probe-v1; exit 3`, "exited unsuccessfully"},
			"stdout overflow": {`printf '` + big + `'`, "more output"},
			"stderr overflow": {`echo callsheet-fake-probe-v1; printf '` + big + `' >&2`, "more output"},
			"argv":            {`[ "$#" = 1 ] && [ "$1" = --callsheet-probe ] && echo callsheet-fake-probe-v1`, ""},
		} {
			tp := newTestProber(t, dir, os.Environ())
			err := tp.run(bg, script(t, dir, strings.ReplaceAll(name, " ", "-"), c.body))
			if c.reason == "" {
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			} else {
				wantProbeErr(t, err, c.reason)
				if strings.Contains(err.Error(), "SECRET") {
					t.Fatalf("%s: child output leaked: %v", name, err)
				}
			}
			tp.reaped(t)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		// The 1 s deadline runs on the injected clock from before the
		// child starts; at expiry the child is killed and waited for.
		dir := t.TempDir()
		tp := newTestProber(t, dir, os.Environ())
		done := make(chan error, 1)
		go func() { done <- tp.run(bg, script(t, dir, "slow", `exec sleep 30`)) }()
		select {
		case <-tp.started:
		case <-time.After(testWait):
			t.Fatal("no child")
		}
		tp.clk.Advance(ProbeTimeout)
		wantProbeErr(t, <-done, "did not answer within 1s")
		if ps := tp.reaped(t); ps.Success() {
			t.Fatalf("killed child state %v", ps)
		}
		if ws := tp.clk.Waiters(); len(ws) != 0 {
			t.Fatalf("timer left: %v", ws)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		dir := t.TempDir()
		tp := newTestProber(t, dir, os.Environ())
		ctx, cancel := context.WithCancel(bg)
		done := make(chan error, 1)
		go func() { done <- tp.run(ctx, script(t, dir, "slow", `exec sleep 30`)) }()
		<-tp.started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled = %v", err)
		}
		tp.reaped(t)
	})
	t.Run("wait-delay", func(t *testing.T) {
		// A child that exits while a descendant keeps its stdout open: the
		// pipes are closed after the 100 ms WaitDelay and the probe fails;
		// it is not a process-tree guarantee, so the test reaps the
		// descendant itself.
		dir := t.TempDir()
		pidFile := filepath.Join(dir, "descendant.pid")
		tp := newTestProber(t, dir, os.Environ())
		start := time.Now()
		err := tp.run(bg, script(t, dir, "leaky", `sleep 30 & echo $! > `+pidFile+`; echo callsheet-fake-probe-v1`))
		wantProbeErr(t, err, "left its output open")
		if el := time.Since(start); el > 10*time.Second {
			t.Fatalf("WaitDelay did not bound the probe: %v", el)
		}
		tp.reaped(t)
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	t.Run("environment", func(t *testing.T) {
		// Exactly CALLSHEET_FAKE_DESCENDANT_LIFETIME_FD and
		// CALLSHEET_FAKE_READY_FD are removed, duplicates included; similar
		// keys and unrelated entries are kept. The child also runs in the
		// configured directory.
		env := []string{
			"CALLSHEET_FAKE_DESCENDANT_LIFETIME_FD=3", "CALLSHEET_FAKE_READY_FD=4", "CALLSHEET_FAKE_READY_FD=5",
			"CALLSHEET_FAKE_READY_FD2=keep", "CALLSHEET_FAKE_READY_FDX=", "X_CALLSHEET_FAKE_READY_FD=keep", "callsheet_fake_ready_fd=keep",
			"CALLSHEET_FAKE_DESCENDANT_LIFETIME_FD_OLD=keep", "PATH=" + os.Getenv("PATH"), "NOEQUALS", "EMPTY=",
		}
		want := []string{"CALLSHEET_FAKE_READY_FD2=keep", "CALLSHEET_FAKE_READY_FDX=", "X_CALLSHEET_FAKE_READY_FD=keep", "callsheet_fake_ready_fd=keep",
			"CALLSHEET_FAKE_DESCENDANT_LIFETIME_FD_OLD=keep", "PATH=" + os.Getenv("PATH"), "NOEQUALS", "EMPTY="}
		if got := filterEnv(env); !slices.Equal(got, want) {
			t.Fatalf("filtered = %q", got)
		}
		dir := t.TempDir()
		tp := newTestProber(t, dir, env)
		exe := script(t, t.TempDir(), "envdump", `env > seen.env; pwd > seen.pwd; echo callsheet-fake-probe-v1`)
		if err := tp.run(bg, exe); err != nil {
			t.Fatal(err)
		}
		tp.reaped(t)
		seen, _ := os.ReadFile(filepath.Join(dir, "seen.env"))
		for _, bad := range []string{"CALLSHEET_FAKE_DESCENDANT_LIFETIME_FD=", "CALLSHEET_FAKE_READY_FD="} {
			for _, line := range strings.Split(string(seen), "\n") {
				if strings.HasPrefix(line, bad) {
					t.Fatalf("child saw %s", line)
				}
			}
		}
		if !strings.Contains(string(seen), "CALLSHEET_FAKE_READY_FD2=keep") || !strings.Contains(string(seen), "X_CALLSHEET_FAKE_READY_FD=keep") {
			t.Fatalf("child environment lost unrelated keys:\n%s", seen)
		}
		pwd, _ := os.ReadFile(filepath.Join(dir, "seen.pwd"))
		real, _ := filepath.EvalSymlinks(dir)
		if got := strings.TrimSpace(string(pwd)); got != dir && got != real {
			t.Fatalf("probe cwd %q, want %q", got, dir)
		}
	})
}

// TestRolePlatformContract is UT FP-9 for the adapter on the running host;
// its executable subtest is delegated from tests/function
// (TestRolePlatform/executable): the real built fixture through the
// production probe, with native symlink, space, permission, format and
// deletion cases. Do not rename or skip its subtests.
func TestRolePlatformContract(t *testing.T) {
	t.Run("executable", func(t *testing.T) {
		bin := fakeBinary(t)
		dir := t.TempDir()
		copyExe := func(name string, mode os.FileMode) string {
			b, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, name)
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, b, mode); err != nil {
				t.Fatal(err)
			}
			return p
		}
		spaced := copyExe("with space/fake adapter", 0o755)
		link := filepath.Join(dir, "link")
		os.Symlink(spaced, link)
		dirLink := filepath.Join(dir, "dirlink")
		os.Symlink(filepath.Dir(spaced), dirLink)
		noX := copyExe("nox", 0o644)
		broken := filepath.Join(dir, "broken")
		os.Symlink(filepath.Join(dir, "gone"), broken)
		garbage := filepath.Join(dir, "garbage")
		os.WriteFile(garbage, []byte("not an executable format\x00\x01"), 0o755)
		wrong := script(t, dir, "wrong", `echo I am not the fake`)
		deleted := copyExe("deleted", 0o755)
		// The process environment may lack PATH entirely: no lookup.
		a := &fake{p: &prober{dir: t.TempDir(), environ: func() []string { return []string{"PATH="} }, clock: realClock{}}}
		for name, c := range map[string]struct{ path, reason string }{
			"built":          {bin, ""},
			"spaces":         {spaced, ""},
			"symlink":        {link, ""},
			"parent-symlink": {filepath.Join(dirLink, "fake adapter"), ""},
			"no-x-bit":       {noX, "no executable permission bit"},
			"broken-link":    {broken, "does not exist"},
			"wrong-format":   {garbage, "cannot be executed"},
			"wrong-binary":   {wrong, "unexpected probe output"},
			"directory":      {dir, "not a regular file"},
		} {
			err := a.Probe(bg, c.path)
			if c.reason == "" {
				if err != nil {
					t.Errorf("%s: %v", name, err)
				}
				continue
			}
			var pe *ProbeError
			if !errors.As(err, &pe) || !strings.Contains(pe.Reason, c.reason) {
				t.Errorf("%s: %v, want %q", name, err, c.reason)
			}
		}
		// Changed or deleted after registration: the next probe fails.
		if err := a.Probe(bg, deleted); err != nil {
			t.Fatal(err)
		}
		os.Remove(deleted)
		if err := a.Probe(bg, deleted); err == nil {
			t.Fatal("a deleted executable passed")
		}
		// The probe leaves nothing in its working directory.
		if entries, _ := os.ReadDir(a.p.dir); len(entries) != 0 {
			t.Fatalf("probe wrote %v", entries)
		}
	})
}

// BenchmarkFakeProbe measures one probe of the real, once-built fixture
// (process start, bounded capture, wait) and checks its exact success and
// the joined child every iteration.
func BenchmarkFakeProbe(b *testing.B) {
	bin := fakeBinary(b)
	waited := 0
	p := &prober{dir: b.TempDir(), environ: os.Environ, clock: realClock{}, waited: func(ps *os.ProcessState) {
		if ps != nil && ps.Exited() {
			waited++
		}
	}}
	b.ReportAllocs()
	n := 0
	for b.Loop() {
		n++
		if err := p.run(bg, bin); err != nil {
			b.Fatal(err)
		}
		if waited != n {
			b.Fatalf("child not joined: %d of %d", waited, n)
		}
	}
}
