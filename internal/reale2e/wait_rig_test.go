package reale2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The wait relay's offline rig (design 12a-real-e2e, amendment A2): a
// supervisor with a live control socket and its real feature loop over an
// in-memory plane, injected clocks for the loop and the helper, and the
// public wait command run through MainWith. The loop's clock never fires,
// so the loop only serves requests; the helper's clock fires every poll
// timer at once (after an optional hook). Real time is used only by the
// socket's own deadlines.

// rigRun is the rig's run ID.
const rigRun = "20261010T120000Z-0a0b0c0d"

// rigBases are hop i's base commits: the seed, then each predecessor's
// result.
var rigBases = [4]string{strings.Repeat("a", 40), fmt.Sprintf("%040x", 1), fmt.Sprintf("%040x", 2), fmt.Sprintf("%040x", 3)}

// manualClock fires no timer on its own; its time moves only when set. A
// wait exchange's work-budget timer (waitHandoff) is announced on work
// (when the clock has one) so the test can fire it; its stops are counted.
type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	work    chan chan time.Time
	created int
	stopped int
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	ch := make(chan time.Time, 1)
	if d != waitHandoff {
		return ch, func() bool { return true }
	}
	c.mu.Lock()
	c.created++
	work := c.work
	c.mu.Unlock()
	if work != nil {
		work <- ch
	}
	return ch, func() bool {
		c.mu.Lock()
		c.stopped++
		c.mu.Unlock()
		return true
	}
}

// announce sets (nil: clears) the channel announcing work timers.
func (c *manualClock) announce(work chan chan time.Time) {
	c.mu.Lock()
	c.work = work
	c.mu.Unlock()
}

// workTimers returns how many work-budget timers were created and
// stopped.
func (c *manualClock) workTimers() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.created, c.stopped
}

func (c *manualClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// pollClock is the helper's clock: every poll timer fires at once, after
// onPoll(n) for the n-th poll (from 1). Its time advances 250ms per poll,
// and jumps 91m past its start from the jumpAfter-th Now call on (0:
// never).
type pollClock struct {
	mu        sync.Mutex
	start     time.Time
	polls     int
	nows      int
	jumpAfter int
	onPoll    func(n int)
}

func (c *pollClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nows++
	if c.jumpAfter > 0 && c.nows >= c.jumpAfter {
		return c.start.Add(91 * time.Minute)
	}
	return c.start.Add(time.Duration(c.polls) * waitPoll)
}

func (c *pollClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	c.polls++
	n, hook := c.polls, c.onPoll
	c.mu.Unlock()
	if d != waitPoll {
		panic("the helper's timer is not its poll interval")
	}
	if hook != nil {
		hook(n)
	}
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch, func() bool { return false }
}

func (c *pollClock) pollCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.polls
}

// rigPlane is the rig's plane: the in-memory plane, with served views,
// fetch failures, blocking fetches and a fetch counter.
type rigPlane struct {
	*fakePlane
	mu       sync.Mutex
	views    map[string]contract.TaskView
	fetchErr error
	block    bool
	started  chan struct{}
	blocked  chan error
	calls    int
	// onFetch runs at every fetch, before it answers.
	onFetch func()
}

func (p *rigPlane) ShowTask(ctx context.Context, id string, lines int) (contract.TaskView, error) {
	p.mu.Lock()
	p.calls++
	v, served := p.views[id]
	err, block, hook := p.fetchErr, p.block, p.onFetch
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	switch {
	case block:
		select {
		case p.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		select {
		case p.blocked <- context.Cause(ctx):
		default:
		}
		return contract.TaskView{}, ctx.Err()
	case err != nil:
		return contract.TaskView{}, err
	case served:
		return v, nil
	}
	return p.fakePlane.ShowTask(ctx, id, lines)
}

func (p *rigPlane) fetches() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// edit changes task id's view in the in-memory plane.
func (p *rigPlane) edit(id string, f func(v *contract.TaskView)) {
	p.fakePlane.mu.Lock()
	defer p.fakePlane.mu.Unlock()
	f(p.fakePlane.tasks[id])
}

// waitRig is one supervisor serving its control socket.
type waitRig struct {
	t      *testing.T
	s      *supervisor
	p      *rigPlane
	clock  *manualClock
	rt     string
	runID  string
	tasks  [4]string
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	// looping is set once the loop was started.
	looping bool
	// serverOpts adjust the control server at construction.
	serverOpts []func(*controlServer)
}

// newWaitRig builds the rig's supervisor with one dispatched task per hop
// (none observed yet); serve starts it.
func newWaitRig(t *testing.T) *waitRig {
	t.Helper()
	clock := &manualClock{now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	fp := newFakePlane(t, clock)
	p := &rigPlane{fakePlane: fp, views: map[string]contract.TaskView{}, started: make(chan struct{}, 1), blocked: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	flow := DefaultFlow()
	for i, r := range flow.Roles {
		if _, err := fp.AddRole(ctx, contract.RoleConfig{ID: r.ID, Name: r.ID, Node: hexID("n_", i+1), Adapter: r.Adapter, Instruction: "instruction",
			Runbook: "runbook", Model: r.Model, Effort: r.Effort, Concurrency: RoleConcurrency, Timeout: RoleTimeout, HasTimeout: true}); err != nil {
			t.Fatal(err)
		}
	}
	ws, _ := fp.CreateWorkspace(ctx, "e2e-rig")
	ev, err := newEvidenceWriter(filepath.Join(t.TempDir(), "evidence"), clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ev.close() })
	rt := shortTemp(t)
	if err := os.Mkdir(filepath.Join(rt, "waits"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &supervisor{stdout: io.Discard, stderr: io.Discard, w: &World{Clock: clock, Poll: time.Hour}, ctx: ctx, runID: rigRun, runtime: rt, ev: ev,
		flow: flow, workspace: WorkspaceRecord{Name: ws.Name, Instance: ws.Instance, SeedCommit: rigBases[0]}, known: map[string]string{},
		rendered: map[string][]byte{}, stoppedSet: map[*child]bool{}, reqs: make(chan ctrlMsg), loopDone: make(chan struct{}), attempt: clock.Now(),
		life: ctx}
	r := &waitRig{t: t, s: s, p: p, clock: clock, rt: rt, runID: rigRun, cancel: cancel, done: make(chan struct{})}
	s.plane = p
	for i := range Hops {
		req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: Hops[i]}, Goal: "Do hop " + Hops[i] + ".",
			Payload: []string{markerFor(rigRun, Hops[i])}, Acceptance: "done", RequestedBy: contract.RequestedBy{Name: "claude-code", Version: "2.1.292", Hostname: "localhost"},
			Workspace: &ws.Name, Base: &rigBases[i], WorkspaceInstance: &ws.Instance}
		v, err := fp.Dispatch(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		r.tasks[i] = v.TaskID
		if err := os.Mkdir(filepath.Join(rt, "waits", hopDir(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(r.shutdown)
	return r
}

// observe records hop i's task as observed (and its predecessors'
// results as accepted), as the loop would have; call before serve.
func (r *waitRig) observe(i int) {
	s := r.s
	h := &s.hops[i]
	h.task, h.observed, h.admittedAt = r.tasks[i], true, r.clock.Now()
	s.known[r.tasks[i]] = Hops[i]
	for j := 0; j < i; j++ {
		s.hops[j].terminal, s.hops[j].commit = true, plumbing.NewHash(rigBases[j+1])
	}
}

// adopt makes the rig the supervisor of another run whose designer task
// is v (served unchanged by its plane): an evidence bundle's own task.
func (r *waitRig) adopt(runID string, ws WorkspaceRecord, v contract.TaskView) {
	r.runID, r.s.runID, r.s.workspace, r.tasks[0] = runID, runID, ws, v.TaskID
	r.p.views[v.TaskID] = v
}

// writeRunFile writes the runtime's run file.
func (r *waitRig) writeRunFile() {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.rt, RunFileName), []byte(`{"schema":"`+RunFileSchema+`","run_id":"`+r.runID+`"}`+"\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// serve writes the run file, opens the socket and starts the loop.
func (r *waitRig) serve() {
	r.t.Helper()
	r.writeRunFile()
	r.listen()
	r.startLoop()
}

// listen opens the rig's socket (server run ID rigRun or the adopted
// one) without starting the loop: a wait's handoff is then never taken,
// as with a loop busy elsewhere.
func (r *waitRig) listen() {
	r.t.Helper()
	runID := rigRun
	if r.s.runID != rigRun {
		runID = r.s.runID
	}
	ctl, err := serveControl(r.rt, runID, r.clock, r.s.handleControl, r.s.handleWaitControl, r.serverOpts...)
	if err != nil {
		r.t.Fatal(err)
	}
	r.s.ctl = ctl
}

// startLoop starts the loop of a listening rig.
func (r *waitRig) startLoop() {
	r.looping = true
	go func() {
		defer close(r.done)
		r.s.featureLoop()
		close(r.s.loopDone)
	}()
}

// shutdown closes the socket (cancelling any in-flight fetch) and stops
// the loop (idempotent).
func (r *waitRig) shutdown() {
	r.once.Do(func() {
		if r.s.ctl != nil {
			r.s.ctl.close()
		}
		r.cancel()
		if r.looping {
			<-r.done
		}
	})
}

// finish ends task id durably with commit c.
func (r *waitRig) finish(i int, state string) {
	c := plumbing.NewHash(fmt.Sprintf("%040x", i+1))
	r.p.finish(r.tasks[i], state, ptr("Done.\nSTATUS: DESIGN_DRAFT\n"), false, &c)
}

// eventCount is the number of events the rig's supervisor recorded.
func (r *waitRig) eventCount() int {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.s.ev.root, "events.jsonl"))
	if err != nil {
		r.t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}

// waitRun is one public wait command run: its exit code, stdout and
// stderr, which the coordinator's shell redirect stores as result.json,
// error.txt and exit-code.
type waitRun struct {
	code        int
	out, errOut string
}

// exitCodeFile is the redirect's exit-code record.
func (w waitRun) exitCodeFile() string { return fmt.Sprintf("%d\n", w.code) }

// runWait runs reale2e wait through MainWith with the given world.
func runWait(t *testing.T, ww WaitWorld, args ...string) waitRun {
	t.Helper()
	var out, errOut bytes.Buffer
	code := MainWith(Host{GOOS: "linux", GOARCH: "amd64", Args: append([]string{"wait"}, args...), Getenv: func(string) string { return "" },
		LookupEnv: func(string) (string, bool) { return "", true }, Stdout: &out, Stderr: &errOut,
		NewWorld:     func(Host) (*World, func(), error) { t.Fatal("wait built a supervisor world"); return nil, nil, nil },
		NewWaitWorld: func() (WaitWorld, func()) { return ww, func() {} }})
	return waitRun{code: code, out: out.String(), errOut: errOut.String()}
}

// helperWorld is a wait world with a poll clock.
func helperWorld(c *pollClock) WaitWorld {
	if c.start.IsZero() {
		c.start = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	}
	return WaitWorld{Context: context.Background(), Clock: c, Exchange: controlExchange}
}

// wait runs the wait command for hop i's task.
func (r *waitRig) wait(c *pollClock, i int) waitRun {
	return runWait(r.t, helperWorld(c), "--run", r.rt, "--task", r.tasks[i], "--hop", Hops[i])
}

// requireWaitError requires a failed wait: exit 1, a fixed diagnostic
// containing want, and nothing on stdout (an empty result.json).
func requireWaitError(t *testing.T, name string, w waitRun, want string) {
	t.Helper()
	if w.code != 1 || w.out != "" || w.exitCodeFile() != "1\n" || !strings.HasPrefix(w.errOut, "reale2e wait: ") || !strings.Contains(w.errOut, want) {
		t.Errorf("%s: exit %d stdout %q stderr %q, want exit 1, no stdout and %q", name, w.code, w.out, w.errOut, want)
	}
}

// fakeWaitServer serves a runtime whose supervisor answers each
// connection n (from 1) with answer(n, request); a nil answer holds the
// connection open until the test ends.
func fakeWaitServer(t *testing.T, answer func(n int, req []byte) []byte) string {
	t.Helper()
	rt := shortTemp(t)
	if err := os.WriteFile(filepath.Join(rt, RunFileName), []byte(`{"schema":"`+RunFileSchema+`","run_id":"`+rigRun+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(rt, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() {
		ln.Close()
		close(release)
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 1; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				defer c.Close()
				c.SetDeadline(time.Now().Add(30 * time.Second))
				req, _ := io.ReadAll(c)
				a := answer(n, req)
				if a == nil {
					<-release
					return
				}
				c.Write(a)
			}(n)
		}
	}()
	return rt
}
