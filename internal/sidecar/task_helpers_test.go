package sidecar

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 05 task fixtures. Every new task test launches task children
// only through the counted factory below: injected children are joined
// goroutines on real OS pipes (zero OS processes), and only
// TestTaskExecutionContract/process delegates to the real exec path. The
// launch ledger is asserted per test at cleanup, zero included.

// procLedger is a test's counted process factory.
type procLedger struct {
	mu       sync.Mutex
	real     bool
	osStarts int
	children chan *fakeChild
	startErr error
	// onStart, when set, runs after a real child started (the /process
	// qualification observes its process group there).
	onStart func(pid int)
	nextPID int
	// byPID is every injected child by PID (the group fake finds a child
	// it was never shown, as a real group signal would).
	byPID map[int]*fakeChild
}

func newLedger() *procLedger {
	return &procLedger{children: make(chan *fakeChild, 64), nextPID: 50000}
}

// factory is the deps.taskProcs of every new task test.
func (l *procLedger) factory(spec procSpec) taskProc {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.real {
		return &countedProc{taskProc: newExecProc(spec), l: l}
	}
	l.nextPID++
	c := &fakeChild{spec: spec, pid: l.nextPID, exit: make(chan procExit, 1), stdin: make(chan []byte, 1), done: make(chan struct{}), l: l}
	if l.byPID == nil {
		l.byPID = map[int]*fakeChild{}
	}
	l.byPID[c.pid] = c
	return c
}

func (l *procLedger) starts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.osStarts
}

// countedProc counts real starts.
type countedProc struct {
	taskProc
	l *procLedger
}

func (p *countedProc) Start() error {
	err := p.taskProc.Start()
	if err == nil {
		p.l.mu.Lock()
		p.l.osStarts++
		on := p.l.onStart
		p.l.mu.Unlock()
		if on != nil {
			on(p.PID())
		}
	}
	return err
}

// fakeChild is an injected task child: a goroutine that reads the
// prompt from its real stdin pipe and writes to its real output pipes as
// the test directs; Wait returns the exit the test chooses.
type fakeChild struct {
	spec  procSpec
	pid   int
	l     *procLedger
	exit  chan procExit
	stdin chan []byte
	done  chan struct{}
	once  sync.Once
}

func (c *fakeChild) Start() error {
	c.l.mu.Lock()
	err := c.l.startErr
	c.l.mu.Unlock()
	if err != nil {
		c.spec.stdin.Close()
		c.spec.stdout.Close()
		c.spec.stderr.Close()
		return err
	}
	go func() {
		b, _ := io.ReadAll(c.spec.stdin)
		c.spec.stdin.Close()
		c.stdin <- b
	}()
	c.l.children <- c
	return nil
}

func (c *fakeChild) PID() int { return c.pid }

func (c *fakeChild) Wait() procExit {
	e := <-c.exit
	return e
}

// out and errOut write to the child's pipes (blocking like a child).
func (c *fakeChild) out(b []byte)    { c.spec.stdout.Write(b) }
func (c *fakeChild) errOut(b []byte) { c.spec.stderr.Write(b) }

// finish ends the direct child with e after closing its pipe ends.
func (c *fakeChild) finish(e procExit) {
	c.once.Do(func() {
		c.spec.stdout.Close()
		c.spec.stderr.Close()
		close(c.done)
		c.exit <- e
	})
}

func (c *fakeChild) exitCode(code int)   { c.finish(procExit{code: code}) }
func (c *fakeChild) signaled(sig string) { c.finish(procExit{signal: sig}) }

// exitHolding ends the direct child while a "descendant" keeps its
// output pipes open.
func (c *fakeChild) exitHolding(code int) {
	c.once.Do(func() {
		close(c.done)
		c.exit <- procExit{code: code}
	})
}

// prompt returns the prompt the child read from stdin.
func (c *fakeChild) prompt(t *testing.T) []byte {
	t.Helper()
	select {
	case b := <-c.stdin:
		return b
	case <-time.After(testWait):
		t.Fatal("the child never read its stdin to EOF")
		return nil
	}
}

// fakeGroups is an injected group cleaner: cleanup returns err, and
// terminate ends the known child as SIGTERM would.
type fakeGroups struct {
	mu       sync.Mutex
	err      error
	cleaned  []int
	signaled []int
	children map[int]*fakeChild
	l        *procLedger
}

func (g *fakeGroups) cleanup(pgid int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cleaned = append(g.cleaned, pgid)
	return g.err
}

func (g *fakeGroups) terminate(pgid int, exited <-chan struct{}) {
	g.mu.Lock()
	g.signaled = append(g.signaled, pgid)
	c, l := g.children[pgid], g.l
	g.mu.Unlock()
	if c == nil && l != nil {
		l.mu.Lock()
		c = l.byPID[pgid]
		l.mu.Unlock()
	}
	if c != nil {
		c.signaled("SIGTERM")
	}
	<-exited
}

func (g *fakeGroups) track(c *fakeChild) {
	g.mu.Lock()
	if g.children == nil {
		g.children = map[int]*fakeChild{}
	}
	g.children[c.pid] = c
	g.mu.Unlock()
}

func (g *fakeGroups) signals() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]int(nil), g.signaled...)
}

// taskRun is a sidecar Run for task tests: a fake plane, the fake clock,
// the scripted adapter (never executes a probe), the counted factory and
// the injected group cleaner.
type taskRun struct {
	*fakeRun
	script *scriptAdapter
	dir    string
	ledger *procLedger
	groups *fakeGroups
	tmp    string
	envMu  sync.Mutex
	env    []string
	sup    chan *taskSupervisor
	cur    *taskSupervisor
}

// taskOpts adjust a task run before it starts.
type taskOpts struct {
	adjust   func(d *deps)
	exe      string
	goos     string
	adapters func(dir string) adapter.Registry
	// tmp, when set, replaces the injected temp root.
	tmp string
	// wrap, when set, wraps the counted factory.
	wrap func(procFactory) procFactory
}

func startTaskRun(t *testing.T, fp *fakePlane, o taskOpts) *taskRun {
	t.Helper()
	ledger := newLedger()
	tr := &taskRun{script: newScript(), dir: t.TempDir(), ledger: ledger, groups: &fakeGroups{l: ledger}, tmp: t.TempDir(), sup: make(chan *taskSupervisor, 1),
		env: []string{"PATH=" + os.Getenv("PATH"), "PWD=/elsewhere", "CALLSHEET_FAKE_READY_FD=4", "KEEP=1", "CALLSHEET_FAKE_READY_FD=5"}}
	if o.tmp != "" {
		tr.tmp = o.tmp
	}
	f := &fakeRun{fp: fp, clk: testkit.NewFakeClock(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)), logs: newSyncLog(), root: newRoot(t)}
	f.d = testDeps(f.clk)
	f.d.adapters = func(string) adapter.Registry { return tr.script.registry() }
	if o.adapters != nil {
		f.d.adapters = o.adapters
	}
	f.d.taskProcs = tr.ledger.factory
	if o.wrap != nil {
		f.d.taskProcs = o.wrap(tr.ledger.factory)
	}
	f.d.taskGroups = tr.groups
	f.d.onSupervisor = func(s *taskSupervisor) { tr.sup <- s }
	f.d.taskEnviron = func() []string {
		tr.envMu.Lock()
		defer tr.envMu.Unlock()
		return append([]string(nil), tr.env...)
	}
	f.d.taskTempDir = func() string { return tr.tmp }
	if o.adjust != nil {
		o.adjust(f.d)
	}
	f.ev = observe(f.d)
	fp.ev = f.ev
	writeState(t, f.root, testID, fp.url, fp.caPEM)
	exe := o.exe
	if exe == "" {
		exe = fakeExeFile(t)
	}
	goos := o.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	f.run = startRunOpts(t, f.d, RunOptions{StateDir: f.root, SoftwareVersion: "test-1", Logger: slog.New(slog.NewJSONHandler(f.logs, nil)),
		FakeAdapterPath: exe, GOOS: goos})
	tr.fakeRun = f
	t.Cleanup(func() {
		// Launch ledger: injected task tests start no OS task child (the
		// scripted adapter's probes never execute anything either).
		if !tr.ledger.real && tr.ledger.starts() != 0 {
			t.Errorf("an injected task test started %d OS children, want 0", tr.ledger.starts())
		}
	})
	return tr
}

// scratchRoot is the parent a task's scratch directory must have under
// the injected temp root tmp: tmp as given on Linux, its physical path on
// Darwin, whose seam resolves symlinks (/var is one to /private/var).
func scratchRoot(t *testing.T, goos, tmp string) string {
	t.Helper()
	if goos != "darwin" {
		return tmp
	}
	root, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("resolve the temp root %s: %v", tmp, err)
	}
	return root
}

// setEnv replaces the children's environment source.
func (tr *taskRun) setEnv(env []string) {
	tr.envMu.Lock()
	tr.env = env
	tr.envMu.Unlock()
}

// fakeExeFile is an executable file that is never executed (injected
// children), so the enabled-executable check passes.
func fakeExeFile(t *testing.T) string {
	t.Helper()
	p := t.TempDir() + "/fake-adapter"
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// child waits for the next injected child's start.
func (tr *taskRun) child(t *testing.T) *fakeChild {
	t.Helper()
	select {
	case c := <-tr.ledger.children:
		tr.groups.track(c)
		return c
	case <-time.After(testWait):
		t.Fatal("no task child started")
		return nil
	}
}

// noChild asserts no child started.
func (tr *taskRun) noChild(t *testing.T) {
	t.Helper()
	select {
	case c := <-tr.ledger.children:
		t.Fatalf("an unexpected child %d started", c.pid)
	default:
	}
}

const taskEpoch = "0123456789abcdef0123456789abcdef"

// taskID returns the n-th test task ID.
func taskID(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 32 {
		s = "0" + s
	}
	return "t_" + s
}

// startBody is a task_start for task n on role (registration order
// order) under attachment gen's token.
func startBody(n int, role contract.RoleConfig, order, gen int, goal string) contract.TaskStartBody {
	return contract.TaskStartBody{TaskID: taskID(n), Execution: contract.ExecutionToken{Epoch: taskEpoch, Attachment: gen}, RolesRevision: 1,
		Role: contract.RoleRecord{RoleConfig: role, RegistrationOrder: order},
		Request: contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: role.ID}, Goal: goal, Payload: []string{"p://1"},
			Acceptance: "done", RequestedBy: contract.RequestedBy{Name: "callsheet", Version: "dev", Hostname: "coord"}},
		Effective: contract.TaskEffective{Model: role.Model, Effort: role.Effort, Timeout: role.Timeout}}
}

// sendStart sends task_start rid.
func (c *fakeConn) sendStart(rid string, b contract.TaskStartBody) {
	c.t.Helper()
	c.send(contract.ProtocolVersion, contract.FrameTaskStart, rid, b)
}

// startResult reads task_start_result rid; with c.ev set it also waits
// for the reply's write to return.
func (c *fakeConn) startResult(rid string) contract.TaskStartResult {
	c.t.Helper()
	f := c.expect(contract.FrameTaskStartResult, rid)
	r, err := contract.DecodeTaskStartResult(f.Body)
	if err != nil {
		c.t.Fatalf("start result %s: %v", f.Body, err)
	}
	return r
}

// expectLog reads task_log rid.
func (c *fakeConn) expectLog(rid string) contract.TaskLogBody {
	c.t.Helper()
	f := c.expect(contract.FrameTaskLog, rid)
	b, err := contract.DecodeTaskLog(f.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return b
}

// ackLog acknowledges task_log rid through next.
func (c *fakeConn) ackLog(rid, id string, next int) {
	c.t.Helper()
	c.send(contract.ProtocolVersion, contract.FrameTaskLogAck, rid, contract.TaskLogAckBody{TaskID: id, NextOffset: next})
}

// expectResult reads task_result rid.
func (c *fakeConn) expectResult(rid string) contract.TaskResultBody {
	c.t.Helper()
	f := c.expect(contract.FrameTaskResult, rid)
	b, err := contract.DecodeTaskResult(f.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return b
}

// ackResult acknowledges receipt of task_result rid.
func (c *fakeConn) ackResult(rid, id string) {
	c.t.Helper()
	c.send(contract.ProtocolVersion, contract.FrameTaskResultAck, rid, contract.TaskResultAckBody{TaskID: id, Received: true})
}

// taskSession is one connected session with roles installed.
type taskSession struct {
	c    *fakeConn
	tr   *taskRun
	b    int // next sidecar request number
	p    int // next plane request number
	gen  int
	cfgs []contract.RoleConfig
}

// connect accepts the next session, acknowledges b1 and installs cfgs as
// revision rev (orders 1..n, or orders given by orders when non-nil).
func (tr *taskRun) connect(t *testing.T, gen, rev int, cfgs ...contract.RoleConfig) *taskSession {
	t.Helper()
	c := tr.fp.accept(t)
	c.connect()
	s := &taskSession{c: c, tr: tr, b: 2, p: 1, gen: gen, cfgs: cfgs}
	c.replace("p1", rev, cfgs...)
	c.expectReplaceAck("p1", rev)
	// The sidecar's ack write returned: nothing moves the clock while it
	// is still in flight.
	tr.ev.awaitMatch(t, evAckWritten, func(ev event) bool { return ev.rev == rev })
	s.p = 2
	return s
}

// awaitCollected waits until the supervisor collected task id's worker
// (its goroutine completed; nothing can be sent for it any more),
// without consuming the main event stream.
func (e *events) awaitCollected(t *testing.T, id string) {
	t.Helper()
	deadline := time.After(testWait)
	for !e.gone[id] {
		select {
		case ev := <-e.collected:
			e.gone[ev.id] = true
		case <-deadline:
			t.Fatalf("task %s's worker was never collected", id)
		}
	}
}

// awaitAll consumes events until every predicate has matched one event,
// in any order (for events whose relative order is not determined).
func (e *events) awaitAll(t *testing.T, preds ...func(event) bool) {
	t.Helper()
	left := slices.Clone(preds)
	deadline := time.After(testWait)
	for len(left) > 0 {
		select {
		case ev := <-e.ch:
			left = slices.DeleteFunc(left, func(p func(event) bool) bool { return p(ev) })
		case <-deadline:
			t.Fatalf("%d awaited events never arrived", len(left))
		}
	}
}

// nthKind matches the n-th event of kind it is shown (a counting,
// single-use predicate for awaitAll).
func nthKind(kind eventKind, n int) func(event) bool {
	seen := 0
	return func(ev event) bool {
		if ev.kind == kind {
			seen++
		}
		return seen == n
	}
}

// nextP returns the next plane request ID.
func (s *taskSession) nextP() string {
	rid := "p" + strconv.Itoa(s.p)
	s.p++
	return rid
}

// nextB returns the next sidecar request ID the sidecar will use.
func (s *taskSession) nextB() string {
	rid := "b" + strconv.Itoa(s.b)
	s.b++
	return rid
}

// start sends a start for task n on role index i and returns the reply.
func (s *taskSession) start(t *testing.T, n, i int, goal string) (contract.TaskStartBody, contract.TaskStartResult) {
	t.Helper()
	b := startBody(n, s.cfgs[i], i+1, s.gen, goal)
	rid := s.nextP()
	s.c.sendStart(rid, b)
	r := s.c.startResult(rid)
	return b, r
}

// run starts task n on role i and returns its child, after the start
// reply's write returned (so its output may flow).
func (s *taskSession) run(t *testing.T, n, i int, goal string) (contract.TaskStartBody, *fakeChild) {
	t.Helper()
	b, r := s.start(t, n, i, goal)
	if r.Err != nil {
		t.Fatalf("start %d refused: %v", n, r.Err)
	}
	ch := s.tr.child(t)
	s.tr.ev.awaitMatch(t, evStartReplied, func(ev event) bool { return ev.id == b.TaskID })
	return b, ch
}

// beat acknowledges the next heartbeat (the clock must have made it due).
func (s *taskSession) beat(t *testing.T, rev int) contract.HeartbeatBody {
	t.Helper()
	rid := s.nextB()
	k, _ := strconv.Atoi(rid[1:])
	b := s.c.heartbeatAt(k, rev)
	s.tr.ev.awaitMatch(t, evAck, func(ev event) bool { return ev.acks == k })
	return b
}

// logs reads task_log requests of st until next reaches end, acking each.
func (s *taskSession) logs(t *testing.T, st contract.TaskStartBody, end int) []byte {
	t.Helper()
	var out []byte
	for next := 0; next < end; {
		rid := s.nextB()
		lb := s.c.expectLog(rid)
		if lb.TaskID != st.TaskID || lb.Offset != next || lb.Execution != st.Execution {
			t.Fatalf("log %s = %s at %d, want %s at %d", rid, lb.TaskID, lb.Offset, st.TaskID, next)
		}
		out = append(out, lb.Data...)
		next = lb.Offset + len(lb.Data)
		s.c.ackLog(rid, st.TaskID, next)
	}
	return out
}

// result reads and acknowledges st's task_result.
func (s *taskSession) result(t *testing.T, st contract.TaskStartBody) contract.TaskResultBody {
	t.Helper()
	rid := s.nextB()
	r := s.c.expectResult(rid)
	if r.TaskID != st.TaskID || r.Execution != st.Execution {
		t.Fatalf("result %s for %s", rid, r.TaskID)
	}
	s.c.ackResult(rid, st.TaskID)
	s.tr.ev.awaitMatch(t, evResultAcked, func(ev event) bool { return ev.id == st.TaskID })
	return r
}

// settled waits until task id's child was reaped and its result set.
func (tr *taskRun) settled(t *testing.T, id string) {
	t.Helper()
	tr.ev.awaitMatch(t, evTaskExited, func(ev event) bool { return ev.id == id })
}

// errUnsupported is an injected start failure.
var errUnsupported = errors.New("injected start failure")

// super returns Run's task supervisor.
func (tr *taskRun) super(t *testing.T) *taskSupervisor {
	t.Helper()
	if tr.cur == nil {
		select {
		case tr.cur = <-tr.sup:
		case <-time.After(testWait):
			t.Fatal("no task supervisor")
		}
	}
	return tr.cur
}

// find returns the live worker of task id (nil when forgotten).
func (s *taskSupervisor) find(id string) *taskWorker {
	s.mu.Lock()
	defer s.mu.Unlock()
	for w := range s.workers {
		if w.id() == id {
			return w
		}
	}
	return nil
}

// drain reads and acknowledges st's remaining task_log requests until its
// task_result, which it acknowledges and returns, with the output bytes
// the logs carried (offsets may jump over evicted bytes).
func (s *taskSession) drain(t *testing.T, st contract.TaskStartBody) (contract.TaskResultBody, []byte) {
	t.Helper()
	var out []byte
	for {
		rid := s.nextB()
		f := s.c.recv()
		if f.RequestID != rid {
			t.Fatalf("got %s %s, want request %s", f.Type, f.RequestID, rid)
		}
		switch f.Type {
		case contract.FrameTaskLog:
			lb, err := contract.DecodeTaskLog(f.Body)
			if err != nil || lb.TaskID != st.TaskID {
				t.Fatalf("log %s: %v", f.Body, err)
			}
			out = append(out, lb.Data...)
			s.c.ackLog(rid, st.TaskID, lb.Offset+len(lb.Data))
		case contract.FrameTaskResult:
			r, err := contract.DecodeTaskResult(f.Body)
			if err != nil || r.TaskID != st.TaskID {
				t.Fatalf("result %s: %v", f.Body, err)
			}
			s.c.ackResult(rid, st.TaskID)
			s.tr.ev.awaitMatch(t, evResultAcked, func(ev event) bool { return ev.id == st.TaskID })
			return r, out
		default:
			t.Fatalf("got %s %s while draining", f.Type, rid)
		}
	}
}
