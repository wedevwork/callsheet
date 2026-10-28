package reale2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// The live supervisor (design 12a-real-e2e): a foreground process with
// the operator terminal. It prepares and supervises the infrastructure,
// renders the coordinator's setup, observes the coordinator's four
// dispatches, takes the owner's decision, collects bounded evidence,
// stops what it started by handle and finally judges the bundle with the
// same offline checker. It never dispatches a feature hop itself.

// Plane is the plane access the supervisor uses; *client.Client
// implements it.
type Plane interface {
	AddRole(ctx context.Context, rc contract.RoleConfig) (contract.RoleView, error)
	ShowRole(ctx context.Context, id string) (contract.RoleView, error)
	ListRoles(ctx context.Context) ([]contract.RoleView, error)
	Dispatch(ctx context.Context, req contract.DispatchRequest) (contract.TaskView, error)
	ShowTask(ctx context.Context, id string, lines int) (contract.TaskView, error)
	ListTasks(ctx context.Context, after string, limit int) ([]contract.TaskSummary, *string, error)
	CancelTask(ctx context.Context, id string) (contract.CancelResponse, error)
	TaskLogs(ctx context.Context, id string) (contract.TaskLogsResponse, error)
	CreateWorkspace(ctx context.Context, name string) (contract.WorkspaceView, error)
	// WorkspaceStatus reads a workspace's refs (the development path
	// verifies the coordinator's seed push with it).
	WorkspaceStatus(ctx context.Context, name string, p client.StatusPage) (contract.WorkspaceStatusResponse, error)
}

// Puller installs a task's published result commit, with its ancestry,
// into the git repository at repo.
type Puller interface {
	PullResult(ctx context.Context, taskID, repo string) error
}

// World is the supervisor's side-effect capabilities, built only after
// the startup gate passed.
type World struct {
	Context    context.Context
	Launcher   Launcher
	Clock      Clock
	Environ    []string
	Cwd        string
	Executable string
	Home       string
	TempRoot   string
	Rand       io.Reader
	LookPath   func(string) (string, error)
	Probe      func(ctx context.Context, vendor, exe, dir string) error
	Connect    func(url string, caPEM []byte) (Plane, Puller, func(), error)
	// Push is the supervisor's own workspace push (design
	// 12a-3-cursor-worker, DS3: the Cursor probe's coder input), over the
	// plane Connect returned; the live world requires that plane to be the
	// verified client. Tests inject it.
	Push func(ctx context.Context, plane Plane, req workspacetransfer.PushRequest) (contract.WorkspacePushResult, error)
	Poll time.Duration
	// Terminal is the operator's decision terminal (read only while the
	// owner prompt is open); Git runs the environment's read-only probes.
	Terminal Terminal
	Git      GitRunner
}

// Terminal is the operator's decision terminal: Activate starts reading
// it (once; nothing is read before) and returns its lines, closed at EOF.
type Terminal interface {
	Activate() <-chan string
}

// readerTerminal is a Terminal over a reader (the live stdin). The
// development path activates it windowed instead (devfull_owner.go): a
// terminal is activated one way, never both.
type readerTerminal struct {
	r    io.Reader
	once sync.Once
	ch   chan string
	// epoch, wonce and wch are the windowed activation's.
	epoch atomic.Uint64
	wonce sync.Once
	wch   chan ownerInput
}

func (t *readerTerminal) Activate() <-chan string {
	t.once.Do(func() {
		t.ch = make(chan string, 16)
		go readLines(t.r, t.ch)
	})
	return t.ch
}

// runArgs are run's absolute paths and supplied protected roots.
type runArgs struct {
	callsheet, claude, codex, grok, evidence, flow string
	protectRoots                                   []string
	// protectDepth is --protect-depth (A3.2).
	protectDepth int
	// probeScope is probe-dev-full's --scenario (design 12a-dev-flow-light;
	// "" is the historical full scope).
	probeScope string
	// cursor is the --cursor executable of any live command (design
	// 12a-3-cursor-worker, slice 3b FP-3; "" none): exactly a Cursor-coder
	// selection takes it.
	cursor string
	// frozenFlow, when set, is a combined exercise's frozen selection bytes
	// (read once before the owner's authorization): its children capture
	// these bytes instead of reading the --flow path again.
	frozenFlow []byte
	// cursorExpect, when set, is a combined exercise's root identity of the
	// Cursor executable, frozen before the owner's authorization (design
	// 12a-3c-cursor-binding): its symlink-resolved absolute target and that
	// file's SHA-256, passed by value to both children, whose pre-vendor
	// identity must equal both components. Unset (the zero value) outside a
	// Cursor exercise.
	cursorExpect cursorIdentity
}

func (a runArgs) vendor(id string) string {
	switch id {
	case "claude":
		return a.claude
	case "codex":
		return a.codex
	case "cursor":
		return a.cursor
	}
	return a.grok
}

// sidecarList is this run's sidecar list in start order.
func (s *supervisor) sidecarList() []string {
	if s.sidecars != nil {
		return s.sidecars
	}
	return sidecarAdapters
}

// codedError is a failure with its fixed evidence code.
type codedError struct {
	code string
	err  error
}

func (e *codedError) Error() string { return e.code + ": " + e.err.Error() }

func failure(code string, err error) error { return &codedError{code: code, err: err} }

// codeOf is err's fixed code ("internal_error" when it has none).
func codeOf(err error) string {
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	return CodeInternalError
}

// ctrlMsg is a control request forwarded to the loop.
type ctrlMsg struct {
	req   ControlRequest
	reply chan controlResponse
}

// hopState is one feature hop's live state.
type hopState struct {
	task       string
	admittedAt time.Time
	observed   bool
	dispatch   DispatchRecord
	terminal   bool
	view       *contract.TaskView
	commit     plumbing.Hash
	tree       plumbing.Hash
	files      map[string][]byte
}

// Owner gate states.
const (
	ownerNone = iota
	ownerAwaiting
	ownerDecided
)

// supervisor is one attempt.
type supervisor struct {
	stdout, stderr io.Writer
	goos, goarch   string
	w              *World
	args           runArgs
	ctx            context.Context

	runID       string
	checkout    string
	runtime     string
	evidenceDir string
	ev          *evidenceWriter
	manifest    Manifest
	flow        Flow
	flowBytes   []byte
	templates   map[string][]byte
	rendered    map[string][]byte
	sessionID   string
	goBin       string
	pathEnv     string
	red         Redactor
	// roots are the canonical protected roots; before is the baseline
	// environment snapshot.
	roots  []string
	before *Snapshot
	// envPolicy is the ownership policy of the environment comparison:
	// legacy for the v3 attempt, the development policy (design
	// 12a-dev-flow-verify r0.8, FP-6) for the full flow and qualification.
	envPolicy envPolicy
	// mcpPath, phases and launches are the coordinator's configuration
	// and its two headless phases; done is set once phase 2 completed.
	mcpPath     string
	phases      [2]*phaseRun
	launches    [2]*LaunchRecord
	transcripts [2][]byte
	done        bool
	validation  *finalValidation

	plane      Plane
	puller     Puller
	closePlane func()
	dep        *deployment
	// sidecars is this run's immutable sidecar list in start order (nil:
	// the package's three sidecarAdapters; a Cursor probe's four, design
	// 12a-3-cursor-worker FP-2). The package list is never mutated.
	sidecars   []string
	mu         sync.Mutex
	children   []*child
	started    []StartedProc
	records    []ProcRecord
	stoppedSet map[*child]bool

	seed        *seedRepo
	collectPath string
	collect     *git.Repository
	workspace   WorkspaceRecord
	known       map[string]string
	preflight   []string
	hops        [4]hopState

	owner         int
	ownerDeadline time.Time
	decision      *DecisionRecord
	reviewSums    [2]string

	failed     bool
	failCode   string
	complete   bool
	finalTests *TestRun
	baseline   *TestRun
	attempt    time.Time
	cancelled  []string
	unsettled  []string
	cleanupBy  time.Time

	ctl *controlServer
	// lines are the owner's terminal lines, read only once the decision
	// prompt is open (nil before and after).
	reqs     chan ctrlMsg
	lines    <-chan string
	closing  atomic.Bool
	loopDone chan struct{}
}

func (s *supervisor) say(format string, args ...any) {
	fmt.Fprintf(s.stdout, "reale2e: "+format+"\n", args...)
}

// track records a successfully started child (A3.6): its start event of
// type typ with the captured PID, and the started-instance inventory
// entry cleanup accounts for. It returns the event's sequence.
func (s *supervisor) track(c *child, typ string, code ...string) int {
	ev := Event{Type: typ, Handle: c.name, PID: c.proc.PID()}
	if len(code) > 0 {
		ev.Code = code[0]
	}
	ev = s.ev.event(ev)
	s.mu.Lock()
	defer s.mu.Unlock()
	c.startedSeq = ev.Seq
	s.children = append(s.children, c)
	s.started = append(s.started, StartedProc{Name: c.name, PID: ev.PID, StartedSeq: ev.Seq})
	return ev.Seq
}

// stopped records a child's stop.
func (s *supervisor) stopped(c *child, rec ProcRecord) {
	s.mu.Lock()
	s.stoppedSet[c] = true
	rec.StartedSeq = c.startedSeq
	s.records = append(s.records, rec)
	s.mu.Unlock()
	s.ev.event(Event{Type: EvProcessStopped, Handle: c.name, Code: rec.StoppedBy})
}

// command runs one short owned command (runCommand). A command whose
// process group could not be proved gone fails, and its captured handle is
// kept so cleanup stops it again and, failing that, keeps the runtime.
func (s *supervisor) command(spec ProcSpec, timeout time.Duration, limit int) (CommandResult, error) {
	res, err := runCommand(s.w.Launcher, s.w.Clock, spec, timeout, limit)
	if errors.Is(err, errUnverifiedStop) {
		s.track(&child{name: "command " + filepath.Base(spec.Path), spec: spec, proc: res.Proc, logs: newLineLog()}, EvProcessStarted, "unverified_stop")
	}
	return res, err
}

// sleep waits d on the injected clock.
func (s *supervisor) sleep(d time.Duration) {
	t, stop := s.w.Clock.NewTimer(d)
	defer stop()
	select {
	case <-t:
	case <-s.ctx.Done():
	}
}

// overdue reports whether the attempt passed its bound.
func (s *supervisor) overdue() bool { return s.w.Clock.Now().Sub(s.attempt) > AttemptBound }

// step runs one phase unless the attempt is already interrupted or late.
func (s *supervisor) step(f func() error) error {
	switch {
	case s.ctx.Err() != nil:
		return failure("interrupted", s.ctx.Err())
	case s.overdue():
		return failure("attempt_deadline_exceeded", errors.New("the attempt passed its 90m bound"))
	}
	return f()
}

// newRunID returns a run ID for now.
func newRunID(now time.Time, r io.Reader) (string, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]), nil
}

// modulePath is the checkout's module.
const modulePath = "github.com/wedevwork/callsheet"

// findCheckout returns the nearest ancestor of cwd holding the Callsheet
// go.mod.
func findCheckout(cwd string) (string, error) {
	dir := filepath.Clean(cwd)
	for {
		if b, err := readBounded(filepath.Join(dir, "go.mod"), 64<<10); err == nil && strings.HasPrefix(string(b), "module "+modulePath+"\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run from inside the Callsheet checkout (no go.mod of " + modulePath + " above the working directory)")
		}
		dir = parent
	}
}

// within reports whether p is root or inside it (both clean, absolute).
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// errEvidencePath is an unusable --evidence destination.
var errEvidencePath = errors.New("--evidence must be a new directory (it must not exist yet) whose existing parent is inside the checkout's ignored design/ tree, for example design/real-e2e-runs/<date>")

// errEvidenceIgnored is an evidence destination Git does not ignore.
var errEvidenceIgnored = errors.New("--evidence must be Git-ignored in the checkout and contain no tracked path (design/ is ignored)")

// locate finds the checkout and validates the evidence destination (design
// 12a-real-e2e, command interface): the bundle directory itself, which
// must not exist yet, whose existing parent resolves (symlinks evaluated)
// inside the checkout's design/ tree. The run creates it exclusively.
func (s *supervisor) locate() error {
	raw, err := findCheckout(s.w.Cwd)
	if err != nil {
		return err
	}
	co, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return err
	}
	s.checkout = co
	dest := filepath.Clean(s.args.evidence)
	name := filepath.Base(dest)
	parent, err := filepath.EvalSymlinks(filepath.Dir(dest))
	if err != nil || name == "." || name == ".." || name == string(filepath.Separator) {
		return errEvidencePath
	}
	design := filepath.Join(co, "design")
	if st, err := os.Stat(parent); err != nil || !st.IsDir() || !within(parent, design) {
		return errEvidencePath
	}
	if _, err := os.Lstat(filepath.Join(parent, name)); !errors.Is(err, fs.ErrNotExist) {
		return errEvidencePath
	}
	// A3: the evidence subtree must be Git-ignored and hold no tracked
	// path before it is created (it is the environment's exact exclusion).
	rel, _ := filepath.Rel(co, filepath.Join(parent, name))
	rel = filepath.ToSlash(rel)
	if _, exit, err := s.w.Git(co, nil, "check-ignore", "-q", "--", rel); err != nil || exit != 0 {
		return errEvidenceIgnored
	}
	if out, exit, err := s.w.Git(co, nil, "ls-files", "-z", "--", rel); err != nil || exit != 0 || len(out) != 0 {
		return errEvidenceIgnored
	}
	s.evidenceDir = filepath.Join(parent, name)
	s.red = Redactor{Home: s.w.Home, Checkout: co, CheckoutAlias: raw}
	if home, err := canonicalPath(s.w.Home); err == nil {
		s.red.HomeAlias = home
	}
	return nil
}

// tok tokenizes a path for evidence.
func (s *supervisor) tok(p string) string {
	r := s.red
	r.Run = s.runtime
	return r.Redact(p)
}

// run is the whole attempt; its exit code is the final report's.
func (s *supervisor) run() int {
	fmt.Fprint(s.stdout, Bounds)
	if err := s.locate(); err != nil {
		fmt.Fprintf(s.stderr, "reale2e run: %v\n", err)
		return 2
	}
	now := s.w.Clock.Now()
	id, err := newRunID(now, s.w.Rand)
	if err != nil {
		fmt.Fprintf(s.stderr, "reale2e run: %v\n", err)
		return 1
	}
	s.runID = id
	if s.ev, err = newEvidenceWriter(s.evidenceDir, s.w.Clock); err != nil {
		fmt.Fprintf(s.stderr, "reale2e run: %v\n", err)
		return 1
	}
	s.manifest = Manifest{Schema: ManifestSchema, RunID: id, GOOS: s.goos, GOARCH: s.goarch, StartedAt: formatTime(now),
		Gate: GateRecord{OptIn: true, CIAbsent: true, Platform: s.goos}, Versions: []VersionRecord{}, Templates: []FileHash{}, Rendered: []FileHash{},
		ProtectedRoots: []string{}}
	s.ev.event(Event{Type: EvRunStarted})
	s.say("run %s: evidence %s", id, s.ev.root)
	s.attempt = s.w.Clock.Now()
	s.ev.event(Event{Type: EvPreflightStarted})
	if err := s.attemptRun(); err != nil {
		s.fail(codeOf(err), err)
	}
	return s.finishRun()
}

// envTokenizer renders environment paths ($CHECKOUT, $HOME) from their
// canonical prefixes, as the roots are canonical.
func (s *supervisor) envTokenizer() pathTokenizer {
	return pathTokenizer{home: s.w.Home, checkout: s.checkout}.canonical()
}

// protectedRoots validates the supplied roots against the evidence
// destination (A3, FP-11), before any vendor work.
func (s *supervisor) protectedRoots() error {
	roots, err := validateRoots(s.args.protectRoots, s.evidenceDir, "")
	if err != nil {
		return failure(CodeInvalidRoot, err)
	}
	s.roots = roots
	s.manifest.ProtectDepth = s.args.protectDepth
	s.say("repositories are discovered at most %d directory levels below each protected root", s.args.protectDepth)
	// The roots are spelled in the snapshots' root frame (rootFrame).
	tok := s.envTokenizer().rootFrame(roots)
	for _, r := range roots {
		s.manifest.ProtectedRoots = append(s.manifest.ProtectedRoots, tok.token(r))
	}
	if len(roots) == 0 {
		s.say("No repository root was protected (only the four vendor homes are fingerprinted)")
	}
	return nil
}

// snapshot takes and records the environment snapshot of phase, within
// the attempt (before) or cleanup (after) deadline. Only the compact
// records are written; the full records stay private in this process
// (A3.4) for the comparison's diff.json detail.
func (s *supervisor) snapshot(phase string, deadline time.Time) *Snapshot {
	exclude := []string{s.evidenceDir}
	if s.runtime != "" {
		exclude = append(exclude, s.runtime)
	}
	snap := takeSnapshotFor(s.envPolicy, phase, s.w.Home, s.roots, exclude, s.w.Git, s.envTokenizer(), s.w.Clock, deadline, s.args.protectDepth)
	shards, err := encodeSnapshot(snap)
	if err != nil {
		snap.Complete, snap.Code, snap.Records = false, CodeEnvLimit, nil
		shards, _ = encodeSnapshot(snap)
	}
	for i, name := range envFiles(phase) {
		s.ev.writeFile(name, shards[i])
	}
	s.ev.event(Event{Type: EvSnapshotTaken, Code: phase})
	return snap
}

// baselineSnapshot fingerprints the owner environment before any probe.
func (s *supervisor) baselineSnapshot() error {
	s.before = s.snapshot("before", s.attempt.Add(AttemptBound))
	if !s.before.Complete {
		return failure(cmpOr(s.before.Code, CodeEnvUnobserved), errors.New("the owner environment could not be fingerprinted completely"))
	}
	return nil
}

// cmpOr returns a, or b when a is empty.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// fail records the attempt's first failure.
func (s *supervisor) fail(code string, err error) {
	if s.failed {
		return
	}
	s.failed, s.failCode = true, code
	s.ev.event(Event{Type: EvAttemptFailed, Code: code})
	fmt.Fprintf(s.stderr, "reale2e: attempt failed (%s): %v\n", code, err)
}

// attemptRun runs the phases up to the end of the feature loop: root
// validation and the baseline fingerprint precede every executable probe.
func (s *supervisor) attemptRun() error {
	for _, f := range []func() error{s.protectedRoots, s.baselineSnapshot, s.printPermissionTemplate, s.staticPreflight, s.createRuntime,
		s.deploy, s.renderPermissions, s.authPreflight, s.prepareSeed, s.prepareCoordinator,
		func() error { return s.launchPhase(1, s.rendered[PhasePrompt1]) }} {
		if err := s.step(f); err != nil {
			return err
		}
	}
	return s.featureLoop()
}

// createRuntime creates the private runtime with its ownership marker.
func (s *supervisor) createRuntime() error {
	// The canonical root keeps the runtime path canonical (macOS /tmp and
	// /var are symlinks), which removeRuntime requires.
	root, err := filepath.EvalSymlinks(s.w.TempRoot)
	if err != nil {
		return failure("runtime_unavailable", err)
	}
	rt, err := os.MkdirTemp(root, "ce-")
	if err != nil {
		return failure("runtime_unavailable", err)
	}
	if err := os.Chmod(rt, 0o700); err != nil {
		return failure("runtime_unavailable", err)
	}
	s.runtime = rt
	if err := os.WriteFile(filepath.Join(rt, ownerMarker), []byte(s.runID+"\n"), 0o600); err != nil {
		return failure("runtime_unavailable", err)
	}
	if _, err := socketPath(rt); err != nil {
		return failure("socket_path_too_long", err)
	}
	// The supplied roots are rechecked against the actual runtime.
	if _, err := validateRoots(s.roots, s.evidenceDir, rt); err != nil {
		return failure(CodeInvalidRoot, err)
	}
	for _, d := range []string{"coord", filepath.Join("coord", "waits"), "collect", "review"} {
		if err := os.Mkdir(filepath.Join(rt, d), 0o700); err != nil {
			return failure("runtime_unavailable", err)
		}
	}
	for i := range Hops {
		if err := os.Mkdir(waitDir(rt, i), 0o700); err != nil {
			return failure("runtime_unavailable", err)
		}
	}
	s.collectPath = filepath.Join(rt, "collect")
	if s.collect, err = git.PlainInit(s.collectPath, false); err != nil {
		return failure("runtime_unavailable", err)
	}
	s.ev.event(Event{Type: EvRuntimeCreated})
	s.say("runtime %s", rt)
	return nil
}

// ownerMarker is the runtime's ownership marker file.
const ownerMarker = ".callsheet-real-e2e-owner"

// deploy starts the plane and sidecars, registers and readies the roles.
func (s *supervisor) deploy() error {
	s.dep = &deployment{s: s}
	if err := s.dep.startPlane(); err != nil {
		return failure("plane_start_failed", err)
	}
	pl, pu, closer, err := s.w.Connect(s.dep.url, s.dep.caPEM)
	if err != nil {
		return failure("plane_connect_failed", err)
	}
	s.plane, s.puller, s.closePlane = pl, pu, closer
	if err := s.dep.startSidecars(); err != nil {
		return failure("sidecar_start_failed", err)
	}
	if err := s.dep.writeManuals(s.rendered); err != nil {
		return failure("runtime_unavailable", err)
	}
	if err := s.dep.registerRoles(s.ctx); err != nil {
		return failure("role_registration_failed", err)
	}
	s.ev.event(Event{Type: EvRolesRegistered})
	for _, a := range s.sidecarList() {
		if err := s.dep.restart(a); err != nil {
			return failure("sidecar_reconnect_failed", err)
		}
	}
	s.ev.event(Event{Type: EvSidecarsReconnected})
	rec, err := s.dep.awaitReady(s.ctx, 2*time.Minute)
	if err != nil {
		return failure("roles_not_ready", err)
	}
	s.ev.writeJSON("setup/roles.json", rec)
	s.ev.event(Event{Type: EvRolesReady})
	s.say("plane %s, sidecars and four roles ready", s.dep.url)
	return nil
}

// featureLoop serves observations, the coordinator phases' exits, the
// owner's prompt input and the task monitor until phase 2 completed or
// the attempt fails. Authoritative admissions are polled throughout,
// while either phase runs and while the owner decides.
func (s *supervisor) featureLoop() error {
	for !s.done && !s.failed {
		t, stop := s.w.Clock.NewTimer(s.w.Poll)
		select {
		case m := <-s.reqs:
			stop()
			m.reply <- s.serveRequest(m.req)
		case line, ok := <-s.lines:
			stop()
			s.handleLine(line, ok)
		case <-s.phaseDone():
			stop()
			s.phaseExited()
		case <-t:
			s.tick()
			s.checkStreams()
		case <-s.ctx.Done():
			stop()
			return failure("interrupted", s.ctx.Err())
		}
	}
	return nil
}

// forwardWait bounds a request's hand-off to, and answer from, the loop
// within the socket's 5s exchange.
var forwardWait = controlExchange - time.Second

// handleControl forwards a socket request to the loop; while closing
// every request is refused.
func (s *supervisor) handleControl(req ControlRequest) controlResponse {
	if s.closing.Load() {
		return codeResponse(CodeClosing)
	}
	m := ctrlMsg{req: req, reply: make(chan controlResponse, 1)}
	select {
	case s.reqs <- m:
	case <-s.loopDone:
		return codeResponse(CodeClosing)
	case <-time.After(forwardWait):
		return codeResponse(CodeInternalError)
	}
	select {
	case resp := <-m.reply:
		return resp
	case <-time.After(forwardWait):
		return codeResponse(CodeInternalError)
	}
}

// current is the running phase, or nil.
func (s *supervisor) current() *phaseRun {
	for _, p := range s.phases {
		if p != nil && !p.handled {
			return p
		}
	}
	return nil
}

// phaseDone is the running phase's exit channel (nil: none).
func (s *supervisor) phaseDone() <-chan struct{} {
	if p := s.current(); p != nil {
		return p.proc.Done()
	}
	return nil
}

// checkStreams fails the attempt when a running phase's output passed its
// bound (the phase is stopped by cleanup rather than blocking).
func (s *supervisor) checkStreams() {
	if p := s.current(); p != nil && (p.stdout.Truncated() || p.stderr.Truncated()) {
		s.fail("coordinator_stream_limit", fmt.Errorf("coordinator phase %d output passed its bound", p.phase))
	}
}

// groupBound bounds the wait for an exited phase's whole group (its MCP
// child and background commands) to be gone.
const groupBound = 30 * time.Second

// observePhase records an exited phase's process observation; its group
// must be gone within bound.
func (s *supervisor) observePhase(p *phaseRun, stoppedBy string, bound time.Duration) {
	p.handled = true
	code, sig := p.proc.Exit()
	p.rec = PhaseProcess{Phase: p.phase, PID: p.proc.PID(), Signal: sig, StoppedBy: stoppedBy, StartedSeq: p.startedSeq}
	select {
	case <-p.proc.Done():
		p.rec.Reaped = true
	default:
	}
	if sig == "" && p.rec.Reaped {
		p.rec.ExitCode = &code
	}
	p.rec.GroupGone = p.rec.Reaped && awaitGone(p.proc, s.w.Clock, bound)
	ev := s.ev.event(Event{Type: EvPhaseExited, Handle: phaseHandle(p.phase), Code: exitText(p.proc)})
	p.rec.ExitedSeq = ev.Seq
	s.ev.writeFile(phaseStreamFiles[p.phase-1], p.stdout.Bytes())
	s.ev.writeFile(phaseStderrFiles[p.phase-1], []byte(toValidUTF8(p.stderr.Bytes())))
	s.captureTranscript(p.phase)
}

// captureTranscript copies the session's transcript read-only once phase
// p exited: the unique <session-id>.jsonl below the protected Claude home,
// absent from the baseline snapshot, copied byte for byte.
func (s *supervisor) captureTranscript(p int) {
	if s.sessionID == "" {
		s.sessionID = streamSession(s.phases[0].stdout.Bytes())
	}
	if !sessionIDRE.MatchString(s.sessionID) {
		return
	}
	path, rel, err := findTranscript(s.w.Home, s.sessionID)
	if err != nil {
		return
	}
	if s.before != nil {
		for _, r := range s.before.Records {
			if r.Kind == RecEntry && r.Scope == "claude" && r.Path == encodePath(rel) {
				return
			}
		}
	}
	data, err := readBounded(path, maxTextFile)
	if err != nil {
		return
	}
	s.transcripts[p-1] = data
	s.ev.writeFile(transcriptFiles[p-1], data)
	s.ev.event(Event{Type: EvTranscriptCaptured, Handle: phaseHandle(p)})
}

// sessionInput is the derivation input from the live state.
func (s *supervisor) sessionInput() SessionInput {
	in := SessionInput{RunID: s.runID, Launches: s.launches, Events: s.ev.snapshot()}
	for i, p := range s.phases {
		if p != nil && p.handled {
			in.Streams[i] = p.stdout.Bytes()
		}
		in.Transcripts[i] = s.transcripts[i]
	}
	return in
}

// fatalFindings are derivation findings that fail a phase.
// (the specific causes first: a phase ending early or unobserved evidence
// is often their consequence).
var fatalFindings = []string{CodeToolDenied, CodeToolFailed, CodeLongPoll, CodeWaitMismatch, CodeDispatchMismatch, CodeUserInput, CodeWakeUnsupported,
	CodeSessionMismatch, CodeSessionMalformed, CodeSessionUnobserved, CodePhaseEarly}

// phaseExited handles the running phase's exit: its group must be gone,
// its exit 0, its init and result present for the same session, its hops
// accepted and their waits consumed; phase 1 then opens the owner gate
// and phase 2 completes the attempt. Nothing is retried or resumed after
// an errored phase.
func (s *supervisor) phaseExited() {
	p := s.current()
	s.observePhase(p, StoppedExited, groupBound)
	// The phase's hops are judged on their authoritative state now.
	for i := range s.hops {
		s.refreshHop(i)
	}
	if s.failed {
		return
	}
	hops := []int{0, 1}
	if p.phase == 2 {
		hops = []int{2, 3}
	}
	em := DeriveSession(s.sessionInput())
	codes := findingCodes(em)
	for _, c := range fatalFindings {
		if slices.Contains(codes, c) {
			s.fail(c, fmt.Errorf("coordinator phase %d: %s", p.phase, c))
			return
		}
	}
	switch {
	case !p.rec.GroupGone:
		s.fail("coordinator_group_alive", fmt.Errorf("coordinator phase %d's process group did not end", p.phase))
		return
	case p.rec.ExitCode == nil || *p.rec.ExitCode != 0:
		s.fail("coordinator_exit_nonzero", fmt.Errorf("coordinator phase %d exited %s", p.phase, exitText(p.proc)))
		return
	case len(em.Init) < p.phase:
		s.fail(CodeSessionUnobserved, fmt.Errorf("coordinator phase %d has no init record", p.phase))
		return
	}
	_, resultOK := streamResult(p.stdout.Bytes())
	for _, i := range hops {
		if !s.hops[i].terminal || s.hops[i].commit.IsZero() || len(entriesOf(em, MapWaitConsumed, Hops[i])) != 1 || !s.waitSucceeded(i) {
			s.fail(CodePhaseEarly, fmt.Errorf("coordinator phase %d ended before hop %s was complete and its wait consumed", p.phase, Hops[i]))
			return
		}
	}
	if !resultOK {
		s.fail(CodeSessionUnobserved, fmt.Errorf("coordinator phase %d has no successful result", p.phase))
		return
	}
	if p.phase == 1 {
		s.awaitOwner()
		return
	}
	if !s.complete {
		s.fail("final_tests_failed", errors.New("the final validation did not pass"))
		return
	}
	s.done = true
	s.say("coordinator phase 2 completed; collecting evidence")
}

// waitSucceeded reports hop i's wait files: exit 0 and a terminal result
// for its task.
func (s *supervisor) waitSucceeded(i int) bool {
	w := s.waitRecord(i)
	return w.ExitCode != nil && *w.ExitCode == 0 && w.Winner == s.hops[i].task && w.Winner != ""
}

// readLines feeds the operator terminal's newline-terminated lines to the
// loop; EOF or a read error closes the channel and discards an
// unterminated partial line, which is never a decision.
func readLines(r io.Reader, out chan<- string) {
	buf := make([]byte, 0, 512)
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				out <- string(buf)
				buf = buf[:0]
			} else if len(buf) < maxControlLine {
				buf = append(buf, one[0])
			}
		}
		if err != nil {
			close(out)
			return
		}
	}
}

// randomHex returns n random bytes as hex from the world's source.
func (s *supervisor) randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(s.w.Rand, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// liveRand is the live random source.
var liveRand io.Reader = rand.Reader
