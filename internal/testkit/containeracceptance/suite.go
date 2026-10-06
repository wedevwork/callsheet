package containeracceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
	"github.com/wedevwork/callsheet/internal/testkit/workersmoke"
)

// Bounds. They are safety bounds of bounded polling, never fixed sleeps.
const (
	// cliTimeout bounds one coordinator CLI command.
	cliTimeout = 2 * time.Minute
	// taskBound bounds one task from dispatch to its resolved terminal
	// state.
	taskBound = 2 * time.Minute
	// readyBound bounds a role's readiness and a barrier's appearance.
	readyBound = time.Minute
	// pollEvery is the polling interval of public state.
	pollEvery = 25 * time.Millisecond
	// waitSlice is one task wait call's requested wait (within the CLI's
	// 0 to 5m limit).
	waitSlice = 20 * time.Second
	// processBound bounds the post-close process disappearance check.
	processBound = 20 * time.Second
	// maxErrorText bounds a record's sanitized error.
	maxErrorText = 2048
)

// node is one sidecar of the suite's deployment: its enrolled node ID, its
// private state directory and its current process ID.
type node struct {
	NodeID, State string
	pid           func() int
}

// PID is the sidecar's current process ID.
func (n *node) PID() int { return n.pid() }

// deployment is the suite's process seam: in the container the real plane
// and sidecars (workersmoke); host unit tests inject a fixture so the
// ordinary suite never launches them.
type deployment interface {
	// origin is the plane's https origin and its CA file.
	origin() (url, ca string)
	// startSidecar enrolls and runs the sidecar name with env and args.
	startSidecar(ctx context.Context, name string, env []string, args ...string) (*node, error)
	// restart stops n and runs it again on its state.
	restart(ctx context.Context, n *node) error
	// close stops every sidecar, then the plane.
	close() error
}

// command is one coordinator CLI invocation (argv, never a shell).
// started, when set, is the start acknowledgement: called once the
// process has started (before its exit is awaited).
type command struct {
	bin, dir       string
	args, env      []string
	stdout, stderr io.Writer
	started        func()
}

// seams are the suite's injected command and process operations.
type seams struct {
	// start starts the plane under dir from the callsheet executable bin.
	start func(ctx context.Context, bin, dir string, env []string) (deployment, error)
	// run runs one command to its exit and returns the exit code, or an
	// error when it did not run to an exit (start failure, context end).
	run func(ctx context.Context, c command) (int, error)
	// live lists the live processes running one of targets (or, without
	// a process table, which of pids still exist).
	live func(targets map[string]bool, pids []int) ([]string, error)
}

// processSeams are the real operations: workersmoke's process lifecycle,
// child processes and the /proc table.
func processSeams() seams {
	return seams{start: startSmoke, run: execCommand, live: func(targets map[string]bool, pids []int) ([]string, error) {
		return liveProcesses("/proc", targets, pids)
	}}
}

// smokeDeployment is the real deployment: workersmoke's plane and
// sidecars.
type smokeDeployment struct {
	d        *workersmoke.Deployment
	sidecars map[*node]*workersmoke.Sidecar
}

func startSmoke(ctx context.Context, bin, dir string, env []string) (deployment, error) {
	d, err := workersmoke.Start(ctx, bin, dir, env)
	if err != nil {
		return nil, err
	}
	return &smokeDeployment{d: d, sidecars: map[*node]*workersmoke.Sidecar{}}, nil
}

func (m *smokeDeployment) origin() (string, string) { return m.d.URL, m.d.CA }

func (m *smokeDeployment) startSidecar(ctx context.Context, name string, env []string, args ...string) (*node, error) {
	sc, err := m.d.StartSidecar(ctx, name, env, args...)
	if err != nil {
		return nil, err
	}
	// The process changes on a restart: read it at each call.
	n := &node{NodeID: sc.NodeID, State: sc.State, pid: func() int { return sc.PID() }}
	m.sidecars[n] = sc
	return n, nil
}

func (m *smokeDeployment) restart(ctx context.Context, n *node) error {
	return m.d.Restart(ctx, m.sidecars[n])
}

func (m *smokeDeployment) close() error { return m.d.Close() }

// execCommand runs c as a child process under ctx (a context end kills
// it), acknowledging its start before waiting for its exit.
func execCommand(ctx context.Context, c command) (int, error) {
	cmd := exec.CommandContext(ctx, c.bin, c.args...)
	cmd.Dir, cmd.Env = c.dir, c.env
	cmd.Stdout, cmd.Stderr = c.stdout, c.stderr
	err := cmd.Start()
	if err == nil {
		if c.started != nil {
			c.started()
		}
		err = cmd.Wait()
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee) && ctx.Err() == nil:
		return ee.ExitCode(), nil
	}
	return -1, err
}

// Suite is one container process's acceptance fixture: a real plane and
// two enrolled sidecars (nodes a and b) from the configured binaries, the
// coordinator's run-scoped state, and the ensure-once case results.
type Suite struct {
	cfg   Config
	seams seams
	root  string
	home  string
	// barriers holds the private test-control files (reports and holds).
	barriers string
	cliEnv   []string
	dep      deployment
	url, ca  string
	nodes    map[string]*node

	mu       sync.Mutex
	once     map[string]*onceResult
	cases    map[string]caseResult
	held     []string
	closed   bool
	closeErr error

	// Shared scenario state, written by the ensure-once steps.
	prep    preparation
	answers []observed
	m4      workspaceFixture
	pubA    observed
	contB   observed
	sibling observed
	partial map[string]observed
	// The background-wait proof (design nonblocking-coordinator-waits):
	// its outcome, its ordered harness events and the background commands
	// it started (stopped and reaped by Close at the latest).
	bgWait bgProof
	bg     []*bgChild
}

// bgProof is the stored outcome of the coordinator's background-wait
// proof: whether it ran, its error and its ordered harness events.
type bgProof struct {
	ran    bool
	err    error
	events []string
	// handled are the wake notifications already acted on, by wait
	// handle and winner task ID.
	handled map[string]bool
}

// onceResult is one ensure-once step's cached outcome.
type onceResult struct {
	done bool
	err  error
}

// caseResult is RunCase's cached outcome.
type caseResult struct {
	rec CaseRecord
	err error
}

// New validates cfg, prepares the private fixture under cfg.WorkDir and
// starts the plane and both sidecars (each with its own state directory,
// the fake adapter in workspace mode and no other adapter). A failure
// stops every child already started.
func New(ctx context.Context, cfg Config) (*Suite, error) {
	return newSuite(ctx, cfg, processSeams())
}

// newSuite is New with the given command and process operations.
func newSuite(ctx context.Context, cfg Config, sm seams) (*Suite, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	for _, p := range []string{cfg.CallsheetPath, cfg.FakeAdapterPath} {
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
			return nil, fmt.Errorf("containeracceptance: executable %s is unavailable", p)
		}
	}
	root := filepath.Join(cfg.WorkDir, fmt.Sprintf("run-%d", cfg.Iteration))
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	s := &Suite{cfg: cfg, seams: sm, root: root, home: filepath.Join(root, "home"), barriers: filepath.Join(root, "control"),
		once: map[string]*onceResult{}, cases: map[string]caseResult{}, partial: map[string]observed{}}
	emptyPath := filepath.Join(root, "empty-path")
	for _, d := range []string{s.home, s.barriers, emptyPath} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	s.cliEnv = []string{"PATH=" + emptyPath, "HOME=" + s.home, "TMPDIR=" + tmp}
	dep, err := sm.start(ctx, cfg.CallsheetPath, filepath.Join(root, "deploy"), s.cliEnv)
	if err != nil {
		os.RemoveAll(root)
		return nil, fmt.Errorf("containeracceptance: plane: %w", err)
	}
	s.dep = dep
	s.url, s.ca = dep.origin()
	sidecarEnv := append(append([]string(nil), s.cliEnv...), fakeadapter.EnvTaskMode+"="+fakeadapter.WorkspaceMode)
	s.nodes = map[string]*node{}
	for _, alias := range []string{"a", "b"} {
		sc, err := dep.startSidecar(ctx, "node-"+alias, sidecarEnv, "--fake-adapter", cfg.FakeAdapterPath)
		if err != nil {
			dep.close()
			os.RemoveAll(root)
			return nil, fmt.Errorf("containeracceptance: sidecar %s: %w", alias, err)
		}
		s.nodes[alias] = sc
	}
	return s, nil
}

// common returns the record fields shared by every record.
func (s *Suite) common(outcome string) Common {
	hashes := map[string]string{}
	for k, v := range s.cfg.ExpectedHashes {
		hashes[k] = v
	}
	return Common{RunID: s.cfg.RunID, Iteration: s.cfg.Iteration, Total: s.cfg.Total, SourceRevision: s.cfg.SourceRevision,
		Arch: s.cfg.Architecture, BinaryHashes: hashes, Outcome: outcome}
}

// EndRecord returns the iteration's end record (call it only after Close).
// Its outcome is the overall result: pass only when the test run passed
// (testsErr nil: the caller's m.Run result), no case or phase the suite
// ran failed, and cleanup succeeded. cleanup_ok reports cleanup alone, so
// a failed case with a successful cleanup is outcome fail, cleanup_ok
// true.
func (s *Suite) EndRecord(testsErr, cleanupErr error) EndRecord {
	s.mu.Lock()
	ids := make([]string, 0, len(s.cases))
	for id, c := range s.cases {
		if c.err != nil {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	sort.Strings(ids)
	var errs []error
	if testsErr != nil {
		errs = append(errs, testsErr)
	}
	if len(ids) > 0 {
		errs = append(errs, fmt.Errorf("failed cases: %s", strings.Join(ids, ", ")))
	}
	if cleanupErr != nil {
		errs = append(errs, cleanupErr)
	}
	rec := EndRecord{Common: s.common(OutcomePass), CaseIDs: RequiredCaseIDs(), CleanupOK: cleanupErr == nil}
	if err := errors.Join(errs...); err != nil {
		rec.Outcome, rec.Error = OutcomeFail, sanitizeError(err)
	}
	return rec
}

// ensure runs step once; later calls return its cached outcome, so a
// failed prerequisite fails every dependent.
func (s *Suite) ensure(key string, step func() error) error {
	r := s.once[key]
	if r == nil {
		r = &onceResult{}
		s.once[key] = r
	}
	if !r.done {
		r.done = true
		r.err = step()
		if r.err != nil {
			r.err = fmt.Errorf("%s: %w", key, r.err)
		}
	}
	return r.err
}

// RunCase executes the named case (or a coordinator/continuation phase)
// once, ensuring its prerequisites once, and returns its record after its
// assertions succeeded, or a failed record and the error. Later calls
// return the cached outcome. It emits nothing itself.
func (s *Suite) RunCase(ctx context.Context, caseID string) (CaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return CaseRecord{}, errors.New("containeracceptance: the suite is closed")
	}
	if c, ok := s.cases[caseID]; ok {
		return c.rec, c.err
	}
	run, ok := s.scenarios()[caseID]
	if !ok {
		return CaseRecord{}, fmt.Errorf("containeracceptance: unknown case %q", caseID)
	}
	rec := CaseRecord{Common: s.common(OutcomePass), CaseID: caseID, Subcases: map[string]string{}, Tasks: []Task{}, Boundary: boundaries[caseID]}
	err := run(ctx, &rec)
	if err != nil {
		rec.Outcome, rec.Error = OutcomeFail, sanitizeError(err)
		err = fmt.Errorf("%s: %w", caseID, err)
	}
	s.cases[caseID] = caseResult{rec, err}
	return rec, err
}

// Close releases every held barrier, stops the sidecars and then the
// plane (waiting for each), checks that no Callsheet or fake worker
// process of this suite survives and that no task work directory remains,
// and removes the runtime state. It is idempotent: later calls return the
// first outcome.
func (s *Suite) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	var errs []error
	for _, p := range s.held {
		if err := os.WriteFile(p, nil, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	// A background command never outlives the suite: each is stopped and
	// its process Wait path joined before the deployment stops.
	for _, b := range s.bg {
		b.stop()
	}
	var pids []int
	if s.dep != nil {
		for _, sc := range s.nodes {
			pids = append(pids, sc.PID())
		}
		errs = append(errs, s.dep.close())
	}
	errs = append(errs, s.processesGone(ctx, pids))
	for alias, sc := range s.nodes {
		entries, err := os.ReadDir(filepath.Join(sc.State, "tasks"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "t_") {
				if _, err := os.Stat(filepath.Join(sc.State, "tasks", e.Name(), "work")); err == nil {
					errs = append(errs, fmt.Errorf("node %s keeps the work directory of %s", alias, e.Name()))
				}
			}
		}
	}
	errs = append(errs, os.RemoveAll(s.root))
	s.closeErr = errors.Join(errs...)
	if s.closeErr != nil {
		s.closeErr = fmt.Errorf("containeracceptance: cleanup: %w", s.closeErr)
	}
	return s.closeErr
}

// processesGone waits (bounded) until no live process runs this suite's
// callsheet or fake adapter executable, observed through /proc where it
// exists; elsewhere it checks the stopped sidecars' process IDs.
func (s *Suite) processesGone(ctx context.Context, pids []int) error {
	targets := map[string]bool{s.cfg.CallsheetPath: true, s.cfg.FakeAdapterPath: true}
	deadline := time.Now().Add(processBound)
	for {
		live, err := s.seams.live(targets, pids)
		if err != nil {
			return err
		}
		if len(live) == 0 {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("task or service processes survive the shutdown: %v", live)
		}
		time.Sleep(pollEvery)
	}
}

// liveProcesses lists the live (non-zombie) processes under procRoot
// whose executable is one of targets; without a readable procRoot it
// reports which of pids still exist.
func liveProcesses(procRoot string, targets map[string]bool, pids []int) ([]string, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		var live []string
		for _, pid := range pids {
			if pid > 0 && syscall.Kill(pid, 0) == nil {
				live = append(live, strconv.Itoa(pid))
			}
		}
		return live, nil
	}
	var live []string
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		exe, err := os.Readlink(filepath.Join(procRoot, e.Name(), "exe"))
		if err != nil {
			continue
		}
		exe = strings.TrimSuffix(exe, " (deleted)")
		if !targets[exe] {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue
		}
		if i := bytes.LastIndexByte(stat, ')'); i > 0 && i+2 < len(stat) && stat[i+2] == 'Z' {
			continue
		}
		live = append(live, fmt.Sprintf("%d %s", pid, exe))
	}
	return live, nil
}

// sanitizeError renders err as one bounded line of printable text.
func sanitizeError(err error) string {
	var b strings.Builder
	for _, r := range err.Error() {
		switch {
		case r == '\n' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
		default:
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > maxErrorText {
		s = s[:maxErrorText] + "..."
	}
	return s
}

// ---- the coordinator CLI ----

// cliResult is one CLI command's outcome.
type cliResult struct {
	code           int
	stdout, stderr string
}

// cli runs the built CLI with the plane origin and CA appended, in the
// coordinator's directory and environment, under a child context.
func (s *Suite) cli(ctx context.Context, args ...string) (cliResult, error) {
	return s.cliRaw(ctx, append(append([]string(nil), args...), "--plane", s.url, "--ca", s.ca)...)
}

// cliRaw runs the built CLI with exactly args.
func (s *Suite) cliRaw(ctx context.Context, args ...string) (cliResult, error) {
	cctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	code, err := s.seams.run(cctx, command{bin: s.cfg.CallsheetPath, dir: s.root, args: args, env: s.cliEnv, stdout: &out, stderr: &errOut})
	if err != nil {
		return cliResult{}, fmt.Errorf("callsheet %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return cliResult{code, out.String(), errOut.String()}, nil
}

// bgChild is one background coordinator command: the harness runs it in
// one joined goroutine through the run seam, whose return (the process's
// Wait path) is the exit notification.
type bgChild struct {
	cancel      context.CancelFunc
	done        chan struct{} // closed once code and err are set
	out, errOut *syncBuffer
	code        int
	err         error
}

// syncBuffer is a buffer the child's output copier and the harness share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// startBackground starts the CLI with args (plane and CA appended) as a
// background command and returns after its start acknowledgement (or its
// end, when it never started). The child is registered for Close.
func (s *Suite) startBackground(ctx context.Context, args ...string) *bgChild {
	cctx, cancel := context.WithCancel(ctx)
	b := &bgChild{cancel: cancel, done: make(chan struct{}), out: &syncBuffer{}, errOut: &syncBuffer{}}
	started := make(chan struct{})
	var once sync.Once
	ack := func() { once.Do(func() { close(started) }) }
	full := append(append([]string(nil), args...), "--plane", s.url, "--ca", s.ca)
	go func() {
		code, err := s.seams.run(cctx, command{bin: s.cfg.CallsheetPath, dir: s.root, args: full, env: s.cliEnv, stdout: b.out, stderr: b.errOut, started: ack})
		b.code, b.err = code, err
		ack()
		close(b.done)
	}()
	<-started
	s.bg = append(s.bg, b)
	return b
}

// exited reports, without waiting, whether the exit notification arrived.
func (b *bgChild) exited() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

// await waits (bounded) for the exit notification. Whichever wake came
// first, the child's recorded state decides: an arrived notification is
// never reported as a timeout or a cancellation.
func (b *bgChild) await(ctx context.Context, bound time.Duration) error {
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-b.done:
	case <-t.C:
	case <-ctx.Done():
	}
	switch {
	case b.exited():
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return fmt.Errorf("no exit notification from the background wait within %v", bound)
}

// stop cancels the child (a running process is killed) and joins its
// goroutine; it is idempotent.
func (b *bgChild) stop() {
	b.cancel()
	<-b.done
}

// cliOK runs a command that must exit 0 and returns its stdout without the
// final newline.
func (s *Suite) cliOK(ctx context.Context, args ...string) ([]byte, error) {
	r, err := s.cli(ctx, args...)
	if err != nil {
		return nil, err
	}
	if r.code != 0 {
		return nil, fmt.Errorf("callsheet %s exited %d: %s", strings.Join(args[:min(2, len(args))], " "), r.code, strings.TrimSpace(r.stderr))
	}
	return []byte(strings.TrimSuffix(r.stdout, "\n")), nil
}

// poll checks cond every pollEvery until it holds, fails or bound ends; it
// rechecks once after the bound before failing.
func poll(ctx context.Context, what string, bound time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(bound)
	for {
		ok, err := cond()
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			if ok, err := cond(); err == nil && ok {
				return nil
			}
			return fmt.Errorf("%s: not observed within %v", what, bound)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", what, ctx.Err())
		case <-time.After(pollEvery):
		}
	}
}

// roleView is role show --json.
func (s *Suite) roleView(ctx context.Context, id string) (contract.RoleView, error) {
	b, err := s.cliOK(ctx, "role", "show", "--json", id)
	if err != nil {
		return contract.RoleView{}, err
	}
	return contract.ParseRoleResponse(b, nil)
}

// awaitAccept polls role show until the role can accept a task.
func (s *Suite) awaitAccept(ctx context.Context, id string) error {
	return poll(ctx, "role "+id+" can_accept", readyBound, func() (bool, error) {
		v, err := s.roleView(ctx, id)
		if err != nil {
			return false, err
		}
		return v.CanAccept, nil
	})
}

// dispatch admits goal to role through the CLI after its readiness, with
// extra flags (workspace, base, instance, timeout).
func (s *Suite) dispatch(ctx context.Context, role, goal string, extra ...string) (contract.TaskView, error) {
	if err := s.awaitAccept(ctx, role); err != nil {
		return contract.TaskView{}, err
	}
	b, err := s.cliOK(ctx, append([]string{"dispatch", "--json", "--role-id", role, "--goal", goal, "--acceptance", "the fixed fake answer and the expected files"}, extra...)...)
	if err != nil {
		return contract.TaskView{}, err
	}
	return contract.ParseDispatchResponse(b)
}

// show is task show --json.
func (s *Suite) show(ctx context.Context, id string) (contract.TaskView, error) {
	b, err := s.cliOK(ctx, "task", "show", "--json", "--lines", "0", id)
	if err != nil {
		return contract.TaskView{}, err
	}
	return contract.ParseTaskShowResponse(b)
}

// resolved reports a terminal view whose workspace publication (if any)
// is resolved; a terminal view with a pending publication is not.
func resolved(v contract.TaskView) bool {
	if !contract.TaskTerminal(v.State) || v.CompletionPending || v.Result == nil {
		return false
	}
	if v.WorkspaceBinding != nil {
		return v.Result.Workspace != nil && v.Result.Workspace.Publication != "" && v.WorkspacePhase == nil
	}
	return true
}

// await waits for task id with task wait (bounded slices) and returns its
// task show view once terminal and resolved.
func (s *Suite) await(ctx context.Context, id string) (contract.TaskView, error) {
	var v contract.TaskView
	err := poll(ctx, "task "+id+" resolved", taskBound, func() (bool, error) {
		b, err := s.cliOK(ctx, "task", "wait", "--json", "--wait", waitSlice.String(), id)
		if err != nil {
			return false, err
		}
		if _, err := contract.ParseWaitResponse(b, []string{id}, waitSlice); err != nil {
			return false, err
		}
		if v, err = s.show(ctx, id); err != nil {
			return false, err
		}
		return resolved(v), nil
	})
	return v, err
}

// awaitFile waits for a barrier file, failing early if task id ends first
// (a barrier that does not precede the end fails the scenario).
func (s *Suite) awaitFile(ctx context.Context, path, id string) error {
	return poll(ctx, "barrier "+filepath.Base(path), readyBound, func() (bool, error) {
		if _, err := os.Stat(path); err == nil {
			return true, nil
		}
		v, err := s.show(ctx, id)
		if err != nil {
			return false, err
		}
		if contract.TaskTerminal(v.State) {
			if _, err := os.Stat(path); err == nil {
				return true, nil
			}
			return false, fmt.Errorf("task %s ended %s before the barrier", id, v.State)
		}
		return false, nil
	})
}

// hold names a barrier file and registers it for release at Close.
func (s *Suite) hold(name string) string {
	p := filepath.Join(s.barriers, name)
	s.held = append(s.held, p)
	return p
}

// fileSHA256 is a file's lowercase SHA-256.
func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
