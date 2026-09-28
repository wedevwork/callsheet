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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 05 task fixtures, guardian-backed since iteration 06a. Every
// task test launches task guardians only through the counted factory
// below: injected guardians and their adapters are joined goroutines on
// real OS pipes (zero OS processes), and only
// TestTaskExecutionContract/process delegates to the real exec path. The
// launch ledger is asserted per test at cleanup, zero included.

// procLedger is a test's counted guardian factory.
type procLedger struct {
	mu       sync.Mutex
	real     bool
	osStarts int
	children chan *fakeChild
	// startErr fails an injected guardian's Start (no guardian); adapterErr
	// makes an injected guardian report that its adapter failed to start.
	startErr   error
	adapterErr error
	// holdReady, when set, delays every injected guardian's ready message
	// until it is closed (a preparation that does not complete).
	holdReady chan struct{}
	// badReady makes an injected guardian's ready message name another
	// group ("pgid"); "own" leaves it valid (the test makes it the
	// sidecar's own group through deps.ownGroup).
	badReady string
	// onStart, when set, runs after a real adapter started with its
	// guardian's and its own PID (the /process qualification observes
	// the group there).
	onStart   func(guardian, adapter int)
	nextPID   int
	guardians []*fakeGuardian
	groups    *fakeGroups
	// manualControl makes an injected guardian only record a cancel
	// command (iteration 06b): the test drives its stopping status and the
	// child's exit; controls counts every recorded command.
	manualControl bool
	controls      chan string
	// controlErrs fails that many next cancel deliveries (a FIFO failure;
	// each attempt is still recorded on controls).
	controlErrs int
	// spawned receives the task ID of every injected guardian created (the
	// worker passed its pre-spawn checks).
	spawned chan string
}

func newLedger() *procLedger {
	return &procLedger{children: make(chan *fakeChild, 64), nextPID: 50000, controls: make(chan string, 64), spawned: make(chan string, 64)}
}

// factory is the deps.taskGuardians of every task test.
func (l *procLedger) factory(spec guardianSpec) guardianProc {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.real {
		return newCountedGuardian(newExecGuardian(spec), l)
	}
	l.nextPID += 2
	g := &fakeGuardian{l: l, spec: spec, pid: l.nextPID, msgs: make(chan contract.GuardianStatus, 8), done: make(chan struct{})}
	g.child = &fakeChild{spec: spec.proc, pid: l.nextPID + 1, gpid: l.nextPID, exit: make(chan procExit, 1), stdin: make(chan []byte, 1),
		done: make(chan struct{}), l: l}
	l.guardians = append(l.guardians, g)
	select {
	case l.spawned <- spec.inv.TaskID:
	default:
	}
	return g
}

func (l *procLedger) starts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.osStarts
}

// countedGuardian counts a real guardian's start and its adapter's (from
// its started message), forwarding its status messages.
type countedGuardian struct {
	guardianProc
	l    *procLedger
	msgs chan contract.GuardianStatus
}

func newCountedGuardian(g guardianProc, l *procLedger) *countedGuardian {
	return &countedGuardian{guardianProc: g, l: l, msgs: make(chan contract.GuardianStatus, 8)}
}

func (p *countedGuardian) Start() error {
	if err := p.guardianProc.Start(); err != nil {
		close(p.msgs)
		return err
	}
	p.l.mu.Lock()
	p.l.osStarts++
	p.l.mu.Unlock()
	go func() {
		defer close(p.msgs)
		for m := range p.guardianProc.Status() {
			if m.Type == contract.GuardianStarted {
				p.l.mu.Lock()
				p.l.osStarts++
				on := p.l.onStart
				p.l.mu.Unlock()
				if on != nil {
					on(p.PID(), m.PID)
				}
			}
			p.msgs <- m
		}
	}()
	return nil
}

func (p *countedGuardian) Status() <-chan contract.GuardianStatus { return p.msgs }

// fakeGuardian is an injected guardian: ready at once (or when the
// ledger's hold is released), its adapter a fakeChild started by the
// release byte, its exit forwarded; it "ends" (its status channel closes)
// after its adapter's exit or without an adapter.
type fakeGuardian struct {
	l     *procLedger
	spec  guardianSpec
	pid   int
	child *fakeChild
	msgs  chan contract.GuardianStatus
	done  chan struct{}

	mu                        sync.Mutex
	released, stopped, exited bool
	relOnce, stopOnce, once   sync.Once
}

func (g *fakeGuardian) send(m contract.GuardianStatus) {
	m.TaskID, m.Nonce = g.spec.inv.TaskID, g.spec.inv.Nonce
	g.msgs <- m
}

func (g *fakeGuardian) Start() error {
	g.l.mu.Lock()
	err, hold, bad := g.l.startErr, g.l.holdReady, g.l.badReady
	g.l.mu.Unlock()
	if err != nil {
		g.spec.proc.closeEnds()
		close(g.msgs)
		close(g.done)
		return err
	}
	go func() {
		if hold != nil {
			select {
			case <-hold:
			case <-g.done:
				return
			}
		}
		g.mu.Lock()
		if !g.exited {
			pgid := g.pid
			if bad == "pgid" {
				pgid = g.pid + 100
			}
			g.send(contract.GuardianStatus{Type: contract.GuardianReady, PID: g.pid, PGID: pgid})
		}
		g.mu.Unlock()
	}()
	return nil
}

func (g *fakeGuardian) PID() int                               { return g.pid }
func (g *fakeGuardian) Status() <-chan contract.GuardianStatus { return g.msgs }

// exit ends the guardian once: a never-released adapter's pipe ends
// close with it.
func (g *fakeGuardian) exit() {
	g.once.Do(func() {
		g.mu.Lock()
		g.exited = true
		released := g.released
		close(g.msgs)
		g.mu.Unlock()
		if !released {
			g.spec.proc.closeEnds()
		}
		close(g.done)
	})
}

func (g *fakeGuardian) Release() {
	g.relOnce.Do(func() {
		g.mu.Lock()
		if g.stopped || g.exited {
			g.mu.Unlock()
			g.exit()
			return
		}
		g.l.mu.Lock()
		aerr := g.l.adapterErr
		g.l.mu.Unlock()
		if aerr != nil {
			r := guardianStartFailed
			g.send(contract.GuardianStatus{Type: contract.GuardianError, Reason: &r})
			g.mu.Unlock()
			g.exit()
			return
		}
		g.released = true
		g.mu.Unlock()
		if err := g.child.Start(); err != nil {
			r := guardianStartFailed
			g.mu.Lock()
			g.send(contract.GuardianStatus{Type: contract.GuardianError, Reason: &r})
			g.mu.Unlock()
			g.exit()
			return
		}
		at := contract.FormatTime(time.Now().UTC())
		g.mu.Lock()
		g.send(contract.GuardianStatus{Type: contract.GuardianStarted, PID: g.child.pid, StartedAt: &at})
		g.mu.Unlock()
		go func() {
			e := g.child.Wait()
			s := contract.GuardianStatus{Type: contract.GuardianExit}
			switch {
			case e.signal != "":
				sig := e.signal
				s.Signal = &sig
			case e.err != nil:
				sig := contract.SignalUnknown
				s.Signal = &sig
			default:
				code := e.code
				s.ExitCode = &code
			}
			g.mu.Lock()
			g.send(s)
			g.mu.Unlock()
			g.exit()
		}()
	})
}

func (g *fakeGuardian) Revoke() { g.relOnce.Do(g.exit) }

// Stop is the parent-lifetime pipe's close: a running adapter gets TERM
// (it exits signaled); a guardian that never released ends at once.
func (g *fakeGuardian) Stop() {
	g.stopOnce.Do(func() {
		if gs := g.l.groups; gs != nil {
			gs.mu.Lock()
			gs.signaled = append(gs.signaled, g.pid)
			ch := gs.stopped
			gs.mu.Unlock()
			if ch != nil {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
		g.mu.Lock()
		g.stopped = true
		released := g.released
		g.mu.Unlock()
		if released {
			g.child.signaled("SIGTERM")
		} else {
			g.relOnce.Do(g.exit)
		}
	})
}

// Control is the injected guardian's cancel command: it is recorded;
// unless the ledger drives it manually, a released guardian latches
// cancelled (its stopping status) and its adapter exits on TERM, and an
// unreleased one ends without an adapter.
func (g *fakeGuardian) Control(stopID string) error {
	g.l.mu.Lock()
	manual := g.l.manualControl
	fail := g.l.controlErrs > 0
	if fail {
		g.l.controlErrs--
	}
	g.l.mu.Unlock()
	g.l.controls <- stopID
	if fail {
		return errors.New("injected control FIFO failure")
	}
	if manual {
		return nil
	}
	g.mu.Lock()
	released, exited := g.released, g.exited
	if released && !exited {
		c, id := contract.CauseCancelled, stopID
		g.send(contract.GuardianStatus{Type: contract.GuardianStopping, Cause: &c, StopID: &id})
	}
	g.mu.Unlock()
	if released {
		g.child.signaled("SIGTERM")
	} else {
		g.relOnce.Do(g.exit)
	}
	return nil
}

// stopping sends a stopping status with cause (and stop ID) now.
func (g *fakeGuardian) stopping(cause string, stopID *string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.exited {
		c := cause
		g.send(contract.GuardianStatus{Type: contract.GuardianStopping, Cause: &c, StopID: stopID})
	}
}

func (g *fakeGuardian) Wait() procExit {
	<-g.done
	return procExit{signal: "SIGKILL"}
}

// fakeChild is an injected adapter: a goroutine that reads the prompt
// from its real stdin pipe and writes to its real output pipes as the
// test directs; Wait returns the exit the test chooses. gpid is its
// guardian's PID (its process group).
type fakeChild struct {
	spec  procSpec
	pid   int
	gpid  int
	l     *procLedger
	exit  chan procExit
	stdin chan []byte
	done  chan struct{}
	once  sync.Once
}

func (c *fakeChild) Start() error {
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

// fakeGroups is an injected group watcher: gone returns err (and records
// the group), exists reports alive (by group) or existsErr; signaled
// records the guardians whose cleanup was requested (Stop).
type fakeGroups struct {
	mu        sync.Mutex
	err       error
	cleaned   []int
	signaled  []int
	alive     map[int]bool
	existsErr error
	gate      chan struct{}
	l         *procLedger
	// stopped is notified on every recorded Stop (awaitSignal).
	stopped chan struct{}
}

func (g *fakeGroups) gone(pgid int) error {
	g.mu.Lock()
	g.cleaned = append(g.cleaned, pgid)
	gate := g.gate
	g.mu.Unlock()
	if gate != nil {
		<-gate
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

// hold makes group disappearance checks wait until the returned release
// runs (a cleanup still in progress).
func (g *fakeGroups) hold() func() {
	c := make(chan struct{})
	g.mu.Lock()
	g.gate = c
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.gate = nil
			g.mu.Unlock()
			close(c)
		})
	}
}

func (g *fakeGroups) exists(pgid int) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.alive[pgid], g.existsErr
}

func (g *fakeGroups) track(c *fakeChild) {}

// awaitSignal waits (bounded) until the guardian of group pgid was asked
// to clean up (its Stop): stop requests reach the guardian from the
// worker's goroutine, after the disposition that requested them.
func (g *fakeGroups) awaitSignal(t *testing.T, pgid int) []int {
	t.Helper()
	deadline := time.After(testWait)
	for {
		g.mu.Lock()
		sig, ch := slices.Clone(g.signaled), g.stopped
		if ch == nil {
			ch = make(chan struct{}, 1)
			g.stopped = ch
		}
		g.mu.Unlock()
		if slices.Contains(sig, pgid) {
			return sig
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("the guardian of group %d was never stopped (stopped %v)", pgid, sig)
		}
	}
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
	script   *scriptAdapter
	dir      string
	ledger   *procLedger
	groups   *fakeGroups
	commands *fakeCommands
	tmp      string
	envMu    sync.Mutex
	env      []string
	sup      chan *taskSupervisor
	cur      *taskSupervisor
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
	wrap func(guardianFactory) guardianFactory
	// root, when set, is the state root (a restart reuses one).
	root string
}

func startTaskRun(t *testing.T, fp *fakePlane, o taskOpts) *taskRun {
	t.Helper()
	ledger := newLedger()
	tr := &taskRun{script: newScript(), dir: t.TempDir(), ledger: ledger, groups: &fakeGroups{l: ledger}, tmp: t.TempDir(), sup: make(chan *taskSupervisor, 1),
		commands: &fakeCommands{},
		env:      []string{"PATH=" + os.Getenv("PATH"), "PWD=/elsewhere", "CALLSHEET_FAKE_READY_FD=4", "KEEP=1", "CALLSHEET_FAKE_READY_FD=5"}}
	if o.tmp != "" {
		tr.tmp = o.tmp
	}
	f := &fakeRun{fp: fp, clk: testkit.NewFakeClock(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)), logs: newSyncLog(), root: newRoot(t)}
	if o.root != "" {
		f.root = o.root
	}
	f.d = testDeps(f.clk)
	f.d.adapters = func(string) adapter.Registry { return tr.script.registry() }
	if o.adapters != nil {
		f.d.adapters = o.adapters
	}
	ledger.groups = tr.groups
	f.d.taskGuardians = tr.ledger.factory
	if o.wrap != nil {
		f.d.taskGuardians = o.wrap(tr.ledger.factory)
	}
	f.d.taskGroups = tr.groups
	f.d.taskCommand = tr.commands.send
	f.d.guardianExe = func() (string, error) { return "/injected/guardian", nil }
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
	if o.root == "" {
		writeState(t, f.root, testID, fp.url, fp.caPEM)
	}
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

// ackResult acknowledges task_result rid (digest d) as committed or only
// received.
func (c *fakeConn) ackResult(rid, id, d string, committed bool) {
	c.t.Helper()
	c.send(contract.ProtocolVersion, contract.FrameTaskResultAck, rid, contract.TaskResultAckBody{TaskID: id, Digest: d, Received: true, Committed: committed})
}

// inventory reads the sidecar's inventory pages i1, i2, ... until the
// final one, acknowledging each, and returns their entries.
func (c *fakeConn) inventory() []contract.TaskInventoryEntry {
	c.t.Helper()
	var out []contract.TaskInventoryEntry
	for n := 1; ; n++ {
		rid := "i" + strconv.Itoa(n)
		f := c.expect(contract.FrameTaskInventory, rid)
		b, err := contract.DecodeTaskInventory(f.Body)
		if err != nil || b.Page != n-1 {
			c.t.Fatalf("inventory %s: %v", f.Body, err)
		}
		out = append(out, b.Entries...)
		c.send(contract.ProtocolVersion, contract.FrameTaskInventoryAck, rid, contract.TaskInventoryAckBody{Page: b.Page, Received: true})
		if b.Final {
			return out
		}
	}
}

// reconcile answers the inventory with one final reconcile page r1 of
// actions (task ID to action, for inventoried executions) and waits for
// the sidecar's acknowledgement write.
func (c *fakeConn) reconcile(entries []contract.TaskInventoryEntry, actions map[string]string) {
	c.t.Helper()
	c.reconcileStops(entries, actions, nil)
}

// reconcileStops is reconcile with the durable stop intents of its
// stop_control dispositions (iteration 06b).
func (c *fakeConn) reconcileStops(entries []contract.TaskInventoryEntry, actions map[string]string, stops map[string]contract.StopIntent) {
	c.t.Helper()
	body := contract.TaskReconcileBody{Final: true}
	for _, e := range entries {
		if a, ok := actions[e.TaskID]; ok {
			ent := contract.TaskReconcileEntry{TaskID: e.TaskID, Execution: e.Execution, Action: a}
			if in, ok := stops[e.TaskID]; ok {
				ent.Stop = &in
			}
			body.Entries = append(body.Entries, ent)
		}
	}
	c.send(contract.ProtocolVersion, contract.FrameTaskReconcile, "r1", body)
	c.expect(contract.FrameTaskReconcileAck, "r1")
	if c.ev != nil {
		c.ev.awaitWritten(c.t, evReplied, "r1")
	}
}

// reconcileEmpty requires an empty inventory and reconciles it.
func (c *fakeConn) reconcileEmpty() {
	c.t.Helper()
	if es := c.inventory(); len(es) != 0 {
		c.t.Fatalf("inventory %+v, want none", es)
	}
	c.reconcile(nil, nil)
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

// connect accepts the next session, acknowledges b1, reconciles an empty
// inventory and installs cfgs as revision rev (orders 1..n).
func (tr *taskRun) connect(t *testing.T, gen, rev int, cfgs ...contract.RoleConfig) *taskSession {
	t.Helper()
	s, entries := tr.reconnect(t, gen, rev, nil, cfgs...)
	if len(entries) != 0 {
		t.Fatalf("inventory %+v, want none", entries)
	}
	return s
}

// reconnect is connect for a worker holding executions: it returns the
// inventory, reconciled with actions.
func (tr *taskRun) reconnect(t *testing.T, gen, rev int, actions map[string]string, cfgs ...contract.RoleConfig) (*taskSession, []contract.TaskInventoryEntry) {
	t.Helper()
	return tr.reconnectStops(t, gen, rev, actions, nil, cfgs...)
}

// reconnectStops is reconnect with stop_control intents.
func (tr *taskRun) reconnectStops(t *testing.T, gen, rev int, actions map[string]string, stops map[string]contract.StopIntent,
	cfgs ...contract.RoleConfig) (*taskSession, []contract.TaskInventoryEntry) {
	t.Helper()
	c := tr.fp.accept(t)
	c.helloOK(testID)
	c.heartbeatAt(1, 0)
	entries := c.inventory()
	c.reconcileStops(entries, actions, stops)
	s := &taskSession{c: c, tr: tr, b: 2, p: 1, gen: gen, cfgs: cfgs}
	c.replace("p1", rev, cfgs...)
	c.expectReplaceAck("p1", rev)
	// The sidecar's ack write returned: nothing moves the clock while it
	// is still in flight.
	tr.ev.awaitMatch(t, evAckWritten, func(ev event) bool { return ev.rev == rev })
	s.p = 2
	return s, entries
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

// result reads st's task_result and acknowledges it as committed.
func (s *taskSession) result(t *testing.T, st contract.TaskStartBody) contract.TaskResultBody {
	t.Helper()
	return s.resultAck(t, st, true)
}

// resultAck reads st's task_result and acknowledges it (committed or
// received only).
func (s *taskSession) resultAck(t *testing.T, st contract.TaskStartBody, committed bool) contract.TaskResultBody {
	t.Helper()
	rid := s.nextB()
	r := s.c.expectResult(rid)
	if r.TaskID != st.TaskID || r.Execution != st.Execution {
		t.Fatalf("result %s for %s", rid, r.TaskID)
	}
	s.c.ackResult(rid, st.TaskID, r.Digest, committed)
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
			s.c.ackResult(rid, st.TaskID, r.Digest, true)
			s.tr.ev.awaitMatch(t, evResultAcked, func(ev event) bool { return ev.id == st.TaskID })
			return r, out
		default:
			t.Fatalf("got %s %s while draining", f.Type, rid)
		}
	}
}

// fakeCommands is the injected control FIFO sender: it records every
// command and returns err (errNoGuardian models no reader), or delivers
// the stop to deliver (a live guardian's cleanup).
type fakeCommands struct {
	mu      sync.Mutex
	sent    []string
	err     error
	deliver func(fifo string)
}

func (c *fakeCommands) send(fifo string, cmd []byte) error {
	c.mu.Lock()
	c.sent = append(c.sent, fifo+" "+strings.TrimSpace(string(cmd)))
	err, d := c.err, c.deliver
	c.mu.Unlock()
	if err == nil && d != nil {
		d(fifo)
	}
	return err
}

func (c *fakeCommands) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}

// lookup is the run's adapter metadata (the scripted registry).
func (tr *taskRun) lookup() contract.AdapterLookup {
	return adapter.ContractLookup(tr.script.registry())
}
