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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
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
	Poll       time.Duration
}

// runArgs are run's absolute paths.
type runArgs struct {
	callsheet, claude, codex, grok, evidence, flow string
}

func (a runArgs) vendor(id string) string {
	switch id {
	case "claude":
		return a.claude
	case "codex":
		return a.codex
	}
	return a.grok
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
	reply chan string
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

	plane      Plane
	puller     Puller
	closePlane func()
	dep        *deployment
	mu         sync.Mutex
	children   []*child
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
	finish     bool
	complete   bool
	finalTests *TestRun
	baseline   *TestRun
	attempt    time.Time
	cancelled  []string
	unsettled  []string
	cleanupBy  time.Time
	// coordinatorReady is set once the launch instructions are offered.
	coordinatorReady bool

	ctl      *controlServer
	reqs     chan ctrlMsg
	lines    chan string
	closing  atomic.Bool
	loopDone chan struct{}
}

func (s *supervisor) say(format string, args ...any) {
	fmt.Fprintf(s.stdout, "reale2e: "+format+"\n", args...)
}

// track records a started child.
func (s *supervisor) track(c *child) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.children = append(s.children, c)
}

// stopped records a child's stop.
func (s *supervisor) stopped(c *child, rec ProcRecord) {
	s.mu.Lock()
	s.stoppedSet[c] = true
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
		s.track(&child{name: "command " + filepath.Base(spec.Path), spec: spec, proc: res.Proc, logs: newLineLog()})
		s.ev.event(Event{Type: EvProcessStarted, Handle: "command " + filepath.Base(spec.Path), Code: "unverified_stop"})
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

// newSessionID returns a random version 4 UUID.
func newSessionID(r io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
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

// locate finds the checkout and validates the evidence destination (design
// 12a-real-e2e, command interface): the bundle directory itself, which
// must not exist yet, whose existing parent resolves (symlinks evaluated)
// inside the checkout's design/ tree. The run creates it exclusively.
func (s *supervisor) locate() error {
	co, err := findCheckout(s.w.Cwd)
	if err != nil {
		return err
	}
	co, err = filepath.EvalSymlinks(co)
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
	s.evidenceDir = filepath.Join(parent, name)
	s.red = Redactor{Home: s.w.Home, Checkout: co}
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
		Gate: GateRecord{OptIn: true, CIAbsent: true, Platform: s.goos}, Versions: []VersionRecord{}, Templates: []FileHash{}, Rendered: []FileHash{}}
	s.ev.event(Event{Type: EvRunStarted})
	s.say("run %s: evidence %s", id, s.ev.root)
	s.attempt = s.w.Clock.Now()
	s.ev.event(Event{Type: EvPreflightStarted})
	if err := s.attemptRun(); err != nil {
		s.fail(codeOf(err), err)
	}
	return s.finishRun()
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

// attemptRun runs the phases up to the end of the feature loop.
func (s *supervisor) attemptRun() error {
	for _, f := range []func() error{s.staticPreflight, s.createRuntime, s.deploy, s.authPreflight, s.prepareSeed, s.prepareCoordinator} {
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
	for _, d := range []string{"coord", "owner", "waits", "collect", "review"} {
		if err := os.Mkdir(filepath.Join(rt, d), 0o700); err != nil {
			return failure("runtime_unavailable", err)
		}
	}
	for i := range Hops {
		if err := os.Mkdir(filepath.Join(rt, "waits", hopDir(i)), 0o700); err != nil {
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
	for _, a := range sidecarAdapters {
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

// featureLoop serves observations, owner input and the task monitor
// until finish is requested or the attempt fails.
func (s *supervisor) featureLoop() error {
	for !s.finish && !s.failed {
		t, stop := s.w.Clock.NewTimer(s.w.Poll)
		select {
		case m := <-s.reqs:
			stop()
			m.reply <- s.handleRequest(m.req)
		case line, ok := <-s.lines:
			stop()
			s.handleLine(line, ok)
		case <-t:
			s.tick()
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

// handleControl forwards a socket request to the loop; while closing,
// finish is acknowledged and every other request refused.
func (s *supervisor) handleControl(req ControlRequest) string {
	if s.closing.Load() {
		if req.Op == opFinish {
			return CodeAccepted
		}
		return CodeClosing
	}
	m := ctrlMsg{req: req, reply: make(chan string, 1)}
	select {
	case s.reqs <- m:
	case <-s.loopDone:
		if req.Op == opFinish {
			return CodeAccepted
		}
		return CodeClosing
	case <-time.After(forwardWait):
		return CodeInternalError
	}
	select {
	case code := <-m.reply:
		return code
	case <-time.After(forwardWait):
		return CodeInternalError
	}
}

// readLines feeds the operator terminal's lines to the loop; EOF closes
// the channel.
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
			if len(buf) > 0 {
				out <- string(buf)
			}
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
