package mcpqual

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// The capture unit-test world (design decoder-enrollment, UT-1..UT-6):
// injected launches whose streams are in-memory pipes and whose probe
// events the fake writes itself, an injected reaper and a fake clock. No
// process is started, no grace elapses and no decoder runs; every launch
// must name a /fake/ executable.

// capBehavior scripts one fake launch.
type capBehavior struct {
	stdout, stderr string
	exit           int
	startErr       error
	hang           bool // exits only when reaped
	holdStdout     bool // stdout stays open after its content until closed
	holdStderr     bool
	plainProc      bool // no StderrProc: stderr is not captured
	floodStdout    int  // extra stdout padding lines (bytes)
	floodStderr    int
	// events renders the probe events file of a session (nil: none).
	events func(cf CaseFile, caseID string) string
	// writes are files the launch writes before it exits (a path relative
	// to its working directory, or absolute), as a vendor's approval
	// command would; a nil content removes the path.
	writes map[string]*string
}

// fakeStream is an in-memory pipe: its reader gets the written bytes, then
// EOF once the writer closed (even after the read end was closed, as a
// drained OS pipe whose writers are gone), or, with the writer still open,
// an error once the read end is closed.
type fakeStream struct {
	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	wclosed bool
	rclosed bool
}

func newFakeStream() *fakeStream {
	s := &fakeStream{}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *fakeStream) Read(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.buf) == 0 && !s.wclosed && !s.rclosed {
		s.cond.Wait()
	}
	switch {
	case len(s.buf) > 0:
		n := copy(b, s.buf)
		s.buf = s.buf[n:]
		return n, nil
	case s.wclosed:
		return 0, io.EOF
	}
	return 0, io.ErrClosedPipe
}

func (s *fakeStream) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, b...)
	s.cond.Broadcast()
	return len(b), nil
}

func (s *fakeStream) closeWrite() { s.mu.Lock(); s.wclosed = true; s.cond.Broadcast(); s.mu.Unlock() }
func (s *fakeStream) Close() error {
	s.mu.Lock()
	s.rclosed = true
	s.cond.Broadcast()
	s.mu.Unlock()
	return nil
}

// capProc is a fake launched process.
type capProc struct {
	pgid         int
	out, errOut  *fakeStream
	exited       chan struct{}
	exit         *int
	signal       *string
	mu           sync.Mutex
	gone         bool
	closedOut    bool
	closedErrOut bool
}

func (p *capProc) PGID() int               { return p.pgid }
func (p *capProc) Stdout() io.Reader       { return p.out }
func (p *capProc) Stderr() io.Reader       { return p.errOut }
func (p *capProc) Exited() <-chan struct{} { return p.exited }
func (p *capProc) Status() (*int, *string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit, p.signal
}
func (p *capProc) CloseStdout() { p.out.Close(); p.mu.Lock(); p.closedOut = true; p.mu.Unlock() }
func (p *capProc) CloseStderr() { p.errOut.Close(); p.mu.Lock(); p.closedErrOut = true; p.mu.Unlock() }

// plainProc hides StderrProc.
type plainProc struct{ *capProc }

func (p plainProc) PGID() int               { return p.capProc.PGID() }
func (p plainProc) Stdout() io.Reader       { return p.capProc.Stdout() }
func (p plainProc) Exited() <-chan struct{} { return p.capProc.Exited() }
func (p plainProc) Status() (*int, *string) { return p.capProc.Status() }
func (p plainProc) CloseStdout()            { p.capProc.CloseStdout() }

type capWorld struct {
	t        testing.TB
	clock    *testkit.FakeClock
	mu       sync.Mutex
	launches []ProcSpec
	procs    []*capProc
	script   func(spec ProcSpec) capBehavior
	reapFail func(pgid int) bool
	// leaderFirst marks reaps whose leader exited before its group.
	leaderFirst func(spec ProcSpec) bool
	// onReap, when set, runs at the start of each reap (ordering tests:
	// the select of launch has already taken its branch).
	onReap func(spec ProcSpec)
	specs  map[int]ProcSpec
	// started receives each launch's kind (buffered).
	started chan string
}

func newCapWorld(t testing.TB) *capWorld {
	w := &capWorld{t: t, clock: testkit.NewFakeClock(epoch), started: make(chan string, 64)}
	w.script = func(spec ProcSpec) capBehavior { return w.defaults(spec) }
	return w
}

// kind names a launch: version, help, enable (Cursor's approval
// preparation) or session.
func launchKind(spec ProcSpec) string {
	switch {
	case slices.Equal(spec.Args, []string{"--version"}):
		return "version"
	case len(spec.Args) > 0 && spec.Args[len(spec.Args)-1] == "--help":
		return "help"
	case slices.Equal(spec.Args, CursorApprovalArgv()):
		return "enable"
	}
	return "session"
}

// approvedFile is the workspace file the fake Cursor enable writes.
const approvedFile = ".cursor/approved-servers.json"

func strp(s string) *string { return &s }

// clientOf is the fake executable's client (".../fake/<id>").
func clientOf(spec ProcSpec) string { return filepath.Base(spec.Path) }

var capVersions = map[string]string{"claude": "2.1.291 (Claude Code)", "codex": "codex-cli 0.160.0", "grok": "grok 1.0.46 (2765805b9442) [stable]", "cursor": "2026.10.01-e373342"}

// defaults is a well-behaved client: its exact version, a help text, and a
// session that initializes, calls slow once and completes.
func (w *capWorld) defaults(spec ProcSpec) capBehavior {
	switch launchKind(spec) {
	case "version":
		return capBehavior{stdout: capVersions[clientOf(spec)] + "\n"}
	case "help":
		return capBehavior{stdout: "usage: fake [options]\n  -p PROMPT  run one prompt\n"}
	case "enable":
		return capBehavior{stdout: "probe enabled\n", writes: map[string]*string{approvedFile: strp(`{"approved":["probe"]}`)}}
	}
	return capBehavior{stdout: capTranscript, events: goodEvents}
}

// capTranscript is a fake session transcript (never decoded by capture).
const capTranscript = `[{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__probe__slow","input":{"case_id":"x"}}]}},` +
	`{"type":"result","subtype":"success","result":"done"}]` + "\n"

// probeLog renders probe events with sequence numbers.
func probeLog(runID string, evs ...ProbeEvent) string {
	var b bytes.Buffer
	for i, ev := range evs {
		ev.Seq, ev.RunID, ev.OffsetNS = i+1, runID, int64(i)*1000
		line, _ := encodeJSON(ev)
		b.Write(append(line, '\n'))
	}
	return b.String()
}

func goodEvents(cf CaseFile, caseID string) string {
	name, ver := "fake-cli", "1.0.0"
	return probeLog(cf.RunID, ProbeEvent{Kind: EvStart}, ProbeEvent{Kind: EvInitialize, ClientName: &name, ClientVersion: &ver},
		ProbeEvent{Kind: EvReceipt, CaseID: caseID, RequestID: json.RawMessage(`1`)}, ProbeEvent{Kind: EvScheduled, CaseID: caseID, RequestID: json.RawMessage(`1`)},
		ProbeEvent{Kind: EvCompleted, CaseID: caseID, RequestID: json.RawMessage(`1`)}, ProbeEvent{Kind: EvEOF}, ProbeEvent{Kind: EvExit, Reason: "eof"})
}

func (w *capWorld) Start(spec ProcSpec) (Proc, error) {
	w.mu.Lock()
	w.launches = append(w.launches, spec)
	n := len(w.launches)
	w.mu.Unlock()
	if filepath.Base(filepath.Dir(spec.Path)) != "fake" {
		w.t.Errorf("launched a non-fixture executable %s", spec.Path)
	}
	if spec.CaptureStderr && spec.StderrPath != "" {
		return nil, ErrStderrConflict
	}
	w.started <- launchKind(spec)
	b := w.script(spec)
	if b.startErr != nil {
		return nil, b.startErr
	}
	if b.events != nil {
		raw, err := os.ReadFile(filepath.Join(spec.Dir, "case.json"))
		if err != nil {
			w.t.Fatal(err)
		}
		cf, err := ParseCaseFile(raw)
		if err != nil {
			w.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(spec.Dir, "server-events.jsonl"), []byte(b.events(cf, cf.Cases[0].CaseID)), 0o600); err != nil {
			w.t.Fatal(err)
		}
	}
	for rel, content := range b.writes {
		p := rel
		if !filepath.IsAbs(p) {
			p = filepath.Join(spec.Dir, filepath.FromSlash(rel))
		}
		if content == nil {
			if err := os.RemoveAll(p); err != nil {
				w.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			w.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(*content), 0o600); err != nil {
			w.t.Fatal(err)
		}
	}
	p := &capProc{pgid: 3000 + n, exited: make(chan struct{})}
	p.out, p.errOut = newFakeStream(), newFakeStream()
	feed := func(s *fakeStream, content string, flood int, hold bool) {
		io.WriteString(s, content)
		line := strings.Repeat("p", 63) + "\n"
		for ; flood > 0; flood -= len(line) {
			io.WriteString(s, line[:min(len(line), flood)])
		}
		if !hold {
			s.closeWrite()
		}
	}
	feed(p.out, b.stdout, b.floodStdout, b.holdStdout)
	feed(p.errOut, b.stderr, b.floodStderr, b.holdStderr)
	w.mu.Lock()
	w.procs = append(w.procs, p)
	if w.specs == nil {
		w.specs = map[int]ProcSpec{}
	}
	w.specs[p.pgid] = spec
	w.mu.Unlock()
	if !b.hang {
		code := b.exit
		p.exit = &code
		p.gone = true
		close(p.exited)
	}
	if b.plainProc {
		return plainProc{p}, nil
	}
	return p, nil
}

// Reap ends a hanging fake as TERM would and proves nothing more.
func (w *capWorld) Reap(pr Proc) CaseCleanup {
	if w.onReap != nil {
		w.mu.Lock()
		spec := w.specs[pr.PGID()]
		w.mu.Unlock()
		w.onReap(spec)
	}
	var p *capProc
	switch v := pr.(type) {
	case *capProc:
		p = v
	case plainProc:
		p = v.capProc
	}
	w.mu.Lock()
	first := w.leaderFirst != nil && w.leaderFirst(w.specs[pr.PGID()])
	w.mu.Unlock()
	cc := CaseCleanup{GroupGone: true, LeaderExitedFirst: first}
	p.mu.Lock()
	if !p.gone {
		p.gone = true
		sig := "terminated"
		p.signal = &sig
		close(p.exited)
		cc.TermSent = true
	}
	p.mu.Unlock()
	if first {
		cc.TermSent, cc.KillSent = true, true
	}
	if w.reapFail != nil && w.reapFail(pr.PGID()) {
		return CaseCleanup{TermSent: true, KillSent: true, Error: sptr("cleanup: injected")}
	}
	return cc
}

func (w *capWorld) kinds() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, s := range w.launches {
		out = append(out, clientOf(s)+":"+launchKind(s))
	}
	return out
}

func (w *capWorld) sessions() []ProcSpec {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []ProcSpec
	for _, s := range w.launches {
		if launchKind(s) == "session" {
			out = append(out, s)
		}
	}
	return out
}

// capTemplates caches the shipped short plan templates per process.
var capTemplates sync.Map

// capTemplate reads a shipped short plan template.
func capTemplate(t testing.TB, id string) []byte {
	t.Helper()
	if b, ok := capTemplates.Load(id); ok {
		return b.([]byte)
	}
	b, err := os.ReadFile(filepath.Join("testdata", "plans", id+"-short.json"))
	if err != nil {
		t.Fatal(err)
	}
	capTemplates.Store(id, b)
	return b
}

// sharedCapture is a completed capture used only as input (the evidence
// assertions, the corruption matrix and the enrollment fixtures), built
// once per test process in the shared directory TestMain removes.
type sharedCapture struct {
	dir    string
	man    *CaptureManifest
	bundle *CaptureBundle
	runner *CaptureRunner
}

var (
	sharedCaptures = map[string]*sharedCapture{}
	sharedValues   = map[string]any{}
)

// sharedValue returns the per-process value key, built on first use.
func sharedValue[T any](key string, build func() T) T {
	sharedMu.Lock()
	v, ok := sharedValues[key]
	sharedMu.Unlock()
	if !ok {
		v = build()
		sharedMu.Lock()
		sharedValues[key] = v
		sharedMu.Unlock()
	}
	return v.(T)
}

// sharedTempDir is the per-process shared directory TestMain removes.
func sharedTempDir(t testing.TB) string {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedDir == "" {
		d, err := os.MkdirTemp("", "mcpqual-shared-runs-")
		if err != nil {
			t.Fatal(err)
		}
		sharedDir = d
	}
	return sharedDir
}

// cachedCapture returns the shared capture key, building it with build
// (which configures a fresh world and runner) on first use. Callers never
// modify it.
func cachedCapture(t *testing.T, key string, build func(w *capWorld) *CaptureRunner) *sharedCapture {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sc, ok := sharedCaptures[key]; ok {
		return sc
	}
	if sharedDir == "" {
		d, err := os.MkdirTemp("", "mcpqual-shared-runs-")
		if err != nil {
			t.Fatal(err)
		}
		sharedDir = d
	}
	c := build(newCapWorld(t))
	c.OutDir = filepath.Join(sharedDir, "capture-"+key)
	withFixtureRoot(c, sharedDir)
	man, b := validatedCapture(t, c, context.Background())
	sc := &sharedCapture{dir: c.OutDir, man: man, bundle: b, runner: c}
	sharedCaptures[key] = sc
	return sc
}

// onceCapture is one capture run's validated outcome.
type onceCapture struct {
	man *CaptureManifest
	b   *CaptureBundle
}

// capturedOnce runs the capture key once per test process (on its first
// repetition, so in every stress invocation) and returns the manifest and
// bundle each repetition asserts on: a deterministic fake-vendor run that
// is only input to the assertions, not a runner lifecycle under test (those
// tests run every repetition). build configures the fresh world and
// returns its runner; its output directory is moved under the shared
// directory TestMain removes. Callers never modify the result.
func capturedOnce(t *testing.T, key string, build func(w *capWorld) *CaptureRunner) (*CaptureManifest, *CaptureBundle) {
	t.Helper()
	r := sharedValue("capture-once-"+key, func() onceCapture {
		c := build(newCapWorld(t))
		c.OutDir = filepath.Join(sharedTempDir(t), "once-"+key)
		withFixtureRoot(c, sharedTempDir(t))
		man, b := runCapture(t, c, context.Background())
		return onceCapture{man, b}
	})
	return r.man, r.b
}

// templateTrees caches each decoded template; templateTree returns a deep
// copy for one plan.
var templateTrees sync.Map

func templateTree(t testing.TB, id string) map[string]any {
	t.Helper()
	v, ok := templateTrees.Load(id)
	if !ok {
		var p map[string]any
		if err := json.Unmarshal(capTemplate(t, id), &p); err != nil {
			t.Fatal(err)
		}
		templateTrees.Store(id, p)
		v = p
	}
	return cloneTree(v).(map[string]any)
}

func cloneTree(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = cloneTree(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneTree(e)
		}
		return out
	}
	return v
}

// filledPlan is the shipped short template of each id filled with a fake
// executable, the fake version and model M; mutate edits the JSON object.
func filledPlan(t testing.TB, mutate func(c map[string]any), ids ...string) []byte {
	t.Helper()
	var clients []any
	var limits any
	for _, id := range ids {
		p := templateTree(t, id)
		limits = p["limits"]
		c := p["clients"].([]any)[0].(map[string]any)
		c["executable"] = "/fake/" + id
		c["expected_version"] = capVersions[id]
		c["model"] = "fake-model-1"
		argv := c["session"].(map[string]any)["argv"].([]any)
		for i, a := range argv {
			if a == "<model>" {
				argv[i] = "fake-model-1"
			}
		}
		if mutate != nil {
			mutate(c)
		}
		clients = append(clients, c)
	}
	b, err := json.Marshal(map[string]any{"version": 1, "limits": limits, "clients": clients})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// capPlans caches parsed unmutated capture plans per process; a capture
// never modifies its plan.
var capPlans sync.Map

// capPlan is the filled shipped templates of ids, parsed once.
func capPlan(t testing.TB, ids ...string) *Plan {
	t.Helper()
	key := strings.Join(ids, ",")
	if p, ok := capPlans.Load(key); ok {
		return p.(*Plan)
	}
	p := mustCapturePlan(t, filledPlan(t, nil, ids...))
	capPlans.Store(key, p)
	return p
}

func mustCapturePlan(t testing.TB, b []byte) *Plan {
	t.Helper()
	p, err := ParseCapturePlan(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newCapRunner(t *testing.T, w *capWorld, p *Plan) *CaptureRunner {
	view := newTempView(t)
	return &CaptureRunner{Plan: p, OutDir: filepath.Join(t.TempDir(), "out"), GOOS: "linux", GOARCH: "amd64", ServerPath: "/opt/mcpqual",
		BaseEnv: []string{"PATH=/usr/bin", "API_TOKEN=inherited-tok-value", "SHORT_SECRET=Zq9"}, Launcher: w, Reaper: w, Clock: w.clock, Log: io.Discard,
		RunID: "run-cap-1", Nonce: "noncecap1", CapturedAt: epoch, HarnessVersion: "test", Home: "/home/owner", User: "owner",
		HashFile: func(p string) (string, error) {
			if strings.HasPrefix(p, "/fake/") {
				return strings.Repeat("c", 64), nil
			}
			return "", fs.ErrNotExist
		}, GrokPlacementFS: view, approvalFS: view}
}

// tempView is the unit tests' filesystem view for the Grok placement gate
// and the Cursor approval scan, with the design's DW10 boundary at
// test-owned fixture roots (code review B1 round 1, W1: never the host's
// temporary root, which may itself hold a marker): every path at or below
// a fixture root (lexical or resolved) is the real filesystem, so every
// marker or change a test plants is visible; every strict ancestor of a
// fixture root is a clean ordinary directory with no entry; any other path
// does not exist. Neither gate then depends on the host's temporary
// directories or their ancestors being clean.
type tempView struct{ roots []string }

// newTempView is the view of t's fixture root: the directory holding all
// of t's own temporary directories (its output directories and fixture
// homes alike). A runner whose output moves to the shared run directory
// gets that root too (withFixtureRoot).
func newTempView(t testing.TB) tempView {
	return newTempViewAt(filepath.Dir(t.TempDir()))
}

// withFixtureRoot adds root to c's views (the shared run directory its
// output was moved to). It takes no lock: callers may hold sharedMu.
func withFixtureRoot(c *CaptureRunner, root string) {
	if v, ok := c.GrokPlacementFS.(tempView); ok {
		v = newTempViewAt(append(append([]string(nil), v.roots...), root)...)
		c.GrokPlacementFS, c.approvalFS = v, v
	}
}

// newTempViewAt is the view whose fixture roots are roots.
func newTempViewAt(roots ...string) tempView {
	var v tempView
	for _, root := range roots {
		root = filepath.Clean(root)
		v.roots = append(v.roots, root)
		if real, err := filepath.EvalSymlinks(root); err == nil && real != root {
			v.roots = append(v.roots, real)
		}
	}
	return v
}

func (v tempView) inside(p string) bool {
	for _, r := range v.roots {
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (v tempView) above(p string) bool {
	for _, r := range v.roots {
		for d := filepath.Dir(r); ; d = filepath.Dir(d) {
			if d == p {
				return true
			}
			if d == filepath.Dir(d) {
				break
			}
		}
	}
	return false
}

// cleanDir is an ordinary directory of the view.
type cleanDir struct{ name string }

func (d cleanDir) Name() string       { return d.name }
func (d cleanDir) Size() int64        { return 0 }
func (d cleanDir) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (d cleanDir) ModTime() time.Time { return time.Time{} }
func (d cleanDir) IsDir() bool        { return true }
func (d cleanDir) Sys() any           { return nil }

func (v tempView) Lstat(p string) (fs.FileInfo, error) {
	p = filepath.Clean(p)
	switch {
	case v.inside(p):
		return os.Lstat(p)
	case v.above(p):
		return cleanDir{filepath.Base(p)}, nil
	}
	return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
}

func (v tempView) EvalSymlinks(p string) (string, error) {
	p = filepath.Clean(p)
	switch {
	case v.inside(p):
		return filepath.EvalSymlinks(p)
	case v.above(p):
		return p, nil
	}
	return "", &fs.PathError{Op: "evalsymlinks", Path: p, Err: fs.ErrNotExist}
}

func (v tempView) ReadDir(p string) ([]fs.DirEntry, error) {
	if v.inside(filepath.Clean(p)) {
		return os.ReadDir(p)
	}
	return nil, &fs.PathError{Op: "readdir", Path: p, Err: fs.ErrNotExist}
}

func (v tempView) Open(p string) (approvalFile, error) {
	if v.inside(filepath.Clean(p)) {
		return osApprovalFS{}.Open(p)
	}
	return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
}

// runCapture runs c and requires its manifest on disk (finalize has
// already checked the directory against it) and the workspace gone; it
// returns the payloads read back. Full bundle validation runs where the
// bundle is the subject (validatedCapture, the shared captures).
func runCapture(t *testing.T, c *CaptureRunner, ctx context.Context) (*CaptureManifest, *CaptureBundle) {
	t.Helper()
	man, err := c.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.OutDir, workDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the disposable workspace survived the capture")
	}
	mb, err := os.ReadFile(filepath.Join(c.OutDir, CaptureManifestName))
	if err != nil {
		t.Fatal(err)
	}
	b := &CaptureBundle{Manifest: man, ManifestBytes: mb, Files: map[string][]byte{}}
	for _, f := range man.Files {
		data, err := os.ReadFile(filepath.Join(c.OutDir, filepath.FromSlash(f.Path)))
		if err != nil || sha256Hex(data) != f.SHA256 {
			t.Fatalf("%s: %v", f.Path, err)
		}
		b.Files[f.Path] = data
	}
	return man, b
}

// validatedCapture is runCapture with the bundle validated from disk.
func validatedCapture(t *testing.T, c *CaptureRunner, ctx context.Context) (*CaptureManifest, *CaptureBundle) {
	t.Helper()
	man, _ := runCapture(t, c, ctx)
	b, err := ValidateCaptureBundle(os.DirFS(c.OutDir), ".")
	if err != nil {
		t.Fatal(err)
	}
	return man, b
}

func capClient(t *testing.T, m *CaptureManifest, id string) CaptureClient {
	t.Helper()
	for _, c := range m.Clients {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no client %s", id)
	return CaptureClient{}
}

func reasonOf(c CaptureClient) string {
	if c.Reason == nil {
		return ""
	}
	return *c.Reason
}

// awaitStart waits (real time, bounded) for a launch of kind.
func (w *capWorld) awaitStart(kind string) {
	w.t.Helper()
	deadline := time.NewTimer(testWait)
	defer deadline.Stop()
	for {
		select {
		case k := <-w.started:
			if k == kind {
				return
			}
		case <-deadline.C:
			w.t.Fatalf("no %s launch", kind)
		}
	}
}

// awaitTimer waits (real time, bounded) for a fake-clock timer of d.
func awaitTimer(t *testing.T, clock *testkit.FakeClock, d time.Duration) {
	t.Helper()
	if err := clock.AwaitWaiter(testWait, testkit.HasTimer(d)); err != nil {
		t.Fatal(err)
	}
}
