package reale2e

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Offline doubles of the supervisor's world (design 12a-real-e2e, FP-9,
// amendment A3): an auto-advancing clock, scripted processes, an in-memory
// plane with a go-git hub, a fake headless Claude coordinator that emits
// print-mode stream records and writes its transcript under a t.TempDir()
// home, a fake terminal that delivers input only once the decision prompt
// is open, and a fake read-only Git runner. No vendor, network or real
// owner file is ever used by them.

// autoClock fires every timer at once, advancing its time by the timer's
// duration: a single-goroutine supervisor run is then deterministic and
// instantaneous in real time.
type autoClock struct {
	mu  sync.Mutex
	now time.Time
	// skip is the number of next timers that fire without advancing the
	// time (a test holds the time for one tick; it never moves back).
	skip int
	// held, when set, supplies the timer for a duration it accepts (ok):
	// the test fires it explicitly and the time does not move.
	held func(d time.Duration) (<-chan time.Time, func() bool, bool)
	// gate, when set, holds every timer of duration gateFor: it fires
	// without advancing the time, and only once gate or closing is closed
	// (an in-flight exchange answered, or the supervisor is closing). A
	// loop selecting on its own events then waits for them instead of
	// spinning through timers that fire at once.
	gate, closing <-chan struct{}
	gateFor       time.Duration
}

// holdNext makes the next timer fire without advancing the time.
func (c *autoClock) holdNext() {
	c.mu.Lock()
	c.skip = 1
	c.mu.Unlock()
}

// holdUntil gates every timer of duration d until done or closing is
// closed; release ends the gate.
func (c *autoClock) holdUntil(d time.Duration, done, closing <-chan struct{}) {
	c.mu.Lock()
	c.gate, c.closing, c.gateFor = done, closing, d
	c.mu.Unlock()
}

// release ends a holdUntil gate.
func (c *autoClock) release() {
	c.mu.Lock()
	c.gate, c.closing, c.gateFor = nil, nil, 0
	c.mu.Unlock()
}

// gated is a timer that fires at the time it fires, unmoved, once done or
// closing is closed; its stop reports whether it stopped it first.
func (c *autoClock) gated(done, closing <-chan struct{}) (<-chan time.Time, func() bool) {
	ch := make(chan time.Time, 1)
	stopped := make(chan struct{})
	// state: 0 pending, 1 fired, 2 stopped; the first transition wins.
	var state atomic.Int32
	go func() {
		select {
		case <-done:
		case <-closing:
		case <-stopped:
			return
		}
		if state.CompareAndSwap(0, 1) {
			ch <- c.Now()
		}
	}()
	return ch, func() bool {
		if !state.CompareAndSwap(0, 2) {
			return false
		}
		close(stopped)
		return true
	}
}

func newAutoClock() *autoClock {
	return &autoClock{now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
}

func (c *autoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *autoClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	if c.held != nil {
		if ch, stop, ok := c.held(d); ok {
			c.mu.Unlock()
			return ch, stop
		}
	}
	if c.gate != nil && d == c.gateFor {
		select {
		case <-c.closing:
			// The supervisor is closing: its cleanup timers run unheld.
			c.gate, c.closing, c.gateFor = nil, nil, 0
		default:
			done, closing := c.gate, c.closing
			c.mu.Unlock()
			return c.gated(done, closing)
		}
	}
	switch {
	case c.skip > 0:
		c.skip--
	case d > 0:
		c.now = c.now.Add(d)
	}
	t := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- t
	return ch, func() bool { return false }
}

func (c *autoClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeProc is a scripted child.
type fakeProc struct {
	pid        int
	done       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	code       int
	sig        string
	ignoreTerm bool
	ignoreKill bool
	signals    []syscall.Signal
	// lingering is a descendant still in the group (it outlives the
	// leader until a group signal it does not ignore).
	lingering bool
}

func (p *fakeProc) PID() int { return p.pid }

func (p *fakeProc) GroupGone() bool {
	if !isDone(p) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.lingering
}
func (p *fakeProc) Done() <-chan struct{} { return p.done }

func (p *fakeProc) exit(code int, sig string) {
	p.once.Do(func() {
		p.mu.Lock()
		p.code, p.sig = code, sig
		p.mu.Unlock()
		close(p.done)
	})
}

func (p *fakeProc) Exit() (int, string) {
	select {
	case <-p.done:
	default:
		return -1, ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.sig
}

func (p *fakeProc) Signal(sig syscall.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	ignore := (sig == syscall.SIGTERM && p.ignoreTerm) || (sig == syscall.SIGKILL && p.ignoreKill)
	if !ignore {
		p.lingering = false
	}
	p.mu.Unlock()
	if !ignore {
		p.exit(-1, sig.String())
	}
	return nil
}

// behavior is a scripted child's conduct.
type behavior struct {
	stdout     string
	stderr     string
	exit       int
	long       bool
	startErr   error
	ignoreTerm bool
	ignoreKill bool
	after      func()
	// onStart receives the started process (a long-running scripted child
	// the test ends itself).
	onStart func(p *fakeProc)
	// lingering leaves a descendant in the short command's group after
	// the leader exits.
	lingering bool
}

// fakeLauncher starts scripted children; a spec real accepts runs as a
// real process instead (the explicitly named real-toolchain tests).
type fakeLauncher struct {
	mu      sync.Mutex
	pid     int
	script  func(ProcSpec) behavior
	real    func(ProcSpec) bool
	started []ProcSpec
	procs   []*fakeProc
}

func (l *fakeLauncher) Start(s ProcSpec) (Proc, error) {
	if l.real != nil && l.real(s) {
		l.mu.Lock()
		l.started = append(l.started, s)
		l.mu.Unlock()
		// A real process under the fake clock runs to its end inside Start:
		// the caller's timers fire at once, so it must find the process
		// already reaped (its output pipes closed with its group).
		p, err := ExecLauncher{}.Start(s)
		if err == nil {
			<-p.Done()
		}
		return p, err
	}
	b := l.script(s)
	if b.startErr != nil {
		return nil, b.startErr
	}
	l.mu.Lock()
	l.pid++
	p := &fakeProc{pid: 1000 + l.pid, done: make(chan struct{}), ignoreTerm: b.ignoreTerm, ignoreKill: b.ignoreKill, lingering: b.lingering}
	l.started = append(l.started, s)
	l.procs = append(l.procs, p)
	l.mu.Unlock()
	if s.Stdout != nil && b.stdout != "" {
		io.WriteString(s.Stdout, b.stdout)
	}
	if s.Stderr != nil && b.stderr != "" {
		io.WriteString(s.Stderr, b.stderr)
	}
	if b.after != nil {
		b.after()
	}
	if b.onStart != nil {
		b.onStart(p)
	}
	if !b.long {
		p.exit(b.exit, "")
	}
	return p, nil
}

// argv returns every started argv joined by spaces (path first).
func (l *fakeLauncher) argv() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.started {
		out = append(out, strings.Join(append([]string{s.Path}, s.Args...), " "))
	}
	return out
}

// phaseSpecs returns the started coordinator phases' specs.
func (l *fakeLauncher) phaseSpecs() []ProcSpec {
	var out []ProcSpec
	for _, s := range l.specsOf(fakeClaude) {
		if len(s.Args) > 0 && s.Args[0] == "-p" {
			out = append(out, s)
		}
	}
	return out
}

// specsOf returns the started specs of path.
func (l *fakeLauncher) specsOf(path string) []ProcSpec {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []ProcSpec
	for _, s := range l.started {
		if s.Path == path {
			out = append(out, s)
		}
	}
	return out
}

// Fake executable paths.
const (
	fakeCallsheet = "/fake/bin/callsheet"
	fakeClaude    = "/fake/bin/claude"
	fakeCodex     = "/fake/bin/codex"
	fakeGrok      = "/fake/bin/grok"
	fakeGo        = "/fake/bin/go"
	fakeGit       = "/fake/bin/git"
	fakeReale2e   = "/fake/bin/reale2e"
)

// hexID returns a deterministic 32-hex identifier for n.
func hexID(prefix string, n int) string { return fmt.Sprintf("%s%032x", prefix, n) }

// fakeView builds a valid task view for req at now.
func fakeView(id string, req contract.DispatchRequest, role contract.RoleView, now time.Time) contract.TaskView {
	started := contract.FormatTime(now)
	v := contract.TaskView{TaskID: id, Request: req, Role: contract.PublicRole(role.RoleRecord),
		Effective:     contract.TaskEffective{Model: role.Model, Effort: role.Effort, Timeout: role.Resolved().Timeout},
		TimeoutPolicy: contract.TimeoutPolicyEnforced, State: contract.TaskRunning, CreatedAt: started, StartedAt: &started, Candidates: []contract.TaskCandidate{}}
	if req.Override != nil {
		if req.Override.Model != nil {
			v.Effective.Model = *req.Override.Model
		}
		if req.Override.Effort != nil {
			v.Effective.Effort = *req.Override.Effort
		}
	}
	return v
}

// fakePlane is an in-memory plane with a go-git hub.
type fakePlane struct {
	mu      sync.Mutex
	t       testing.TB
	clock   Clock
	roles   []contract.RoleView
	tasks   map[string]*contract.TaskView
	order   []string
	n       int
	hub     *memory.Storage
	ws      *contract.WorkspaceView
	logs    map[string][]byte
	cancels []string
	// Fault and behavior injection.
	addRoleErrs    []error
	notReadyCalls  int
	dispatchErr    func(req contract.DispatchRequest) error
	dispatchLost   bool
	effectiveModel string
	// wsRefs are the workspace's branch refs (a seed push sets main);
	// statusErr fails workspace status reads.
	wsRefs map[string]string
	// others are further workspaces (a light qualification's second one)
	// with their own main refs; a task ref belongs to its bound workspace.
	others        []*contract.WorkspaceView
	otherRefs     map[string]map[string]string
	statusErr     error
	listErr       error
	showErr       func(id string) error
	createWSErr   error
	preflight     func(i int) (state string, final *string, truncated bool, finish bool)
	cancelIgnored bool
	// finishSkew moves every finish time (a result whose plane times say
	// it ran longer than the supervisor saw).
	finishSkew time.Duration
	// afterDispatch runs before a dispatch answer is returned (a delayed
	// response advances the clock there).
	afterDispatch func(req contract.DispatchRequest)
	// unbounded counts task polls made without a context deadline.
	unbounded int
	// shows counts every task fetch.
	shows int
	// onList runs on every ListTasks call (the scripted coordinator).
	onList func()
}

func newFakePlane(t testing.TB, clock Clock) *fakePlane {
	return &fakePlane{t: t, clock: clock, tasks: map[string]*contract.TaskView{}, hub: memory.NewStorage(), logs: map[string][]byte{}}
}

// check validates v as the real strict parser would read it back.
func (p *fakePlane) check(v contract.TaskView) {
	b, err := json.Marshal(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v})
	if err == nil {
		_, err = contract.ParseTaskShowResponse(b)
	}
	if err != nil {
		p.t.Errorf("fake plane produced an invalid task view: %v", err)
	}
}

func (p *fakePlane) AddRole(_ context.Context, rc contract.RoleConfig) (contract.RoleView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.addRoleErrs) > 0 {
		err := p.addRoleErrs[0]
		p.addRoleErrs = p.addRoleErrs[1:]
		if err != nil {
			return contract.RoleView{}, err
		}
	}
	v := contract.RoleView{RoleRecord: contract.RoleRecord{RoleConfig: rc, RegistrationOrder: len(p.roles) + 1}, CanAccept: true, NodeLiveness: "online"}
	p.roles = append(p.roles, v)
	return v, nil
}

func (p *fakePlane) ShowRole(_ context.Context, id string) (contract.RoleView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.notReadyCalls > 0 {
		p.notReadyCalls--
		return contract.RoleView{}, contract.New(contract.CodeUnavailable, "not ready")
	}
	for _, r := range p.roles {
		if r.ID == id {
			return r, nil
		}
	}
	return contract.RoleView{}, contract.New(contract.CodeNotFound, "no role")
}

func (p *fakePlane) ListRoles(context.Context) ([]contract.RoleView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.roles), nil
}

func (p *fakePlane) role(id string) (contract.RoleView, bool) {
	for _, r := range p.roles {
		if r.ID == id {
			return r, true
		}
	}
	return contract.RoleView{}, false
}

func (p *fakePlane) Dispatch(_ context.Context, req contract.DispatchRequest) (contract.TaskView, error) {
	if p.dispatchErr != nil {
		if err := p.dispatchErr(req); err != nil {
			return contract.TaskView{}, err
		}
	}
	p.mu.Lock()
	r, ok := p.role(req.Target.Value)
	if !ok {
		p.mu.Unlock()
		return contract.TaskView{}, contract.New(contract.CodeNotFound, "no role")
	}
	p.n++
	id := hexID("t_", p.n)
	v := fakeView(id, req, r, p.clock.Now())
	if req.Workspace != nil {
		sel, err := contract.ParseTaskBase(req.Base)
		if err != nil {
			p.mu.Unlock()
			return contract.TaskView{}, err
		}
		b := &contract.WorkspaceBinding{Name: *req.Workspace, Instance: p.instanceOf(*req.Workspace), BaseSelector: contract.SelectorString(sel)}
		if req.WorkspaceInstance != nil {
			b.Instance = *req.WorkspaceInstance
		}
		if req.Base != nil {
			base := *req.Base
			b.BaseCommit = &base
		}
		v.WorkspaceBinding = b
		phase := contract.WorkspacePhaseExecuting
		v.WorkspacePhase = &phase
	}
	p.check(v)
	p.tasks[id] = &v
	p.order = append(p.order, id)
	lost := p.dispatchLost
	p.dispatchLost = false
	if p.effectiveModel != "" {
		v.Effective.Model = p.effectiveModel
	}
	preflight := p.preflight
	p.mu.Unlock()
	if len(req.Payload) > 0 && strings.Contains(req.Payload[0], " hop=preflight-") {
		i := int(req.Payload[0][len(req.Payload[0])-1]-'0') - 1
		state, final, trunc, finish := contract.TaskSucceeded, ptr(AuthToken+"\n"), false, true
		if preflight != nil {
			state, final, trunc, finish = preflight(i)
		}
		if finish {
			p.finish(id, state, final, trunc, nil)
		}
	}
	if p.afterDispatch != nil {
		p.afterDispatch(req)
	}
	if lost {
		return contract.TaskView{}, contract.New(contract.CodeUnavailable, "the answer was lost")
	}
	return v, nil
}

func ptr[T any](v T) *T { return &v }

// finish ends task id in state with final; a workspace task publishes
// commit (nil: the publication failed).
func (p *fakePlane) finish(id, state string, final *string, truncated bool, commit *plumbing.Hash) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v := p.tasks[id]
	now := contract.FormatTime(p.clock.Now().Add(p.finishSkew))
	v.State, v.FinishedAt, v.WorkspacePhase, v.DurabilityConfirmed = state, &now, nil, true
	exit := 0
	r := contract.TaskResult{State: state, ExitCode: &exit, FinalMessage: final, FinalMessageTruncated: truncated, ChangedPaths: []string{}}
	switch state {
	case contract.TaskFailed:
		exit = 1
	case contract.TaskCancelled:
		r.ExitCode = nil
		sig := "SIGTERM"
		r.Signal = &sig
	}
	if b := v.WorkspaceBinding; b != nil {
		w := &contract.TaskWorkspaceResult{Name: b.Name, Instance: b.Instance, BaseCommit: b.BaseCommit}
		if commit != nil {
			c, ref := commit.String(), contract.TaskRefPrefix+id
			w.Publication, w.Commit, w.Ref, w.Diffstat = contract.PublicationPublished, &c, &ref, &contract.TaskDiffstat{}
		} else if state == contract.TaskCancelled {
			w.Publication = contract.PublicationNotStarted
		} else {
			e := contract.PubErrPublicationTimeout
			w.Publication, w.Error = contract.PublicationFailed, &e
		}
		r = r.WithWorkspace(w)
	}
	v.Result = &r
	p.check(*v)
	p.logs[id] = []byte("log of " + id + "\n")
}

// ShowTask answers task id as the plane renders it for lines (design
// 12a-dev-flow-verify r0.12): its retained log's contract.LogTail display
// tail, mirrored in a terminal result, on a copied view and result (the
// stored task and earlier answers are never changed).
func (p *fakePlane) ShowTask(ctx context.Context, id string, lines int) (contract.TaskView, error) {
	p.mu.Lock()
	p.shows++
	if _, ok := ctx.Deadline(); !ok {
		p.unbounded++
	}
	p.mu.Unlock()
	if p.showErr != nil {
		if err := p.showErr(id); err != nil {
			return contract.TaskView{}, err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.tasks[id]
	if !ok {
		return contract.TaskView{}, contract.New(contract.CodeNotFound, "no task")
	}
	out := *v
	out.LogTail, out.TailTruncated = contract.LogTail(p.logs[id], lines)
	if v.Result != nil {
		r := *v.Result
		r.LogTail = out.LogTail
		out.Result = &r
	}
	return out, nil
}

func (p *fakePlane) ListTasks(_ context.Context, after string, limit int) ([]contract.TaskSummary, *string, error) {
	if p.onList != nil {
		p.onList()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listErr != nil {
		return nil, nil, p.listErr
	}
	ids := slices.Clone(p.order)
	sort.Strings(ids)
	var out []contract.TaskSummary
	for _, id := range ids {
		if id <= after {
			continue
		}
		v := p.tasks[id]
		// The plane's complete summary of the task's view.
		out = append(out, contract.TaskSummary{TaskID: id, Target: v.Request.Target, Role: v.Role, State: v.State, StopRequested: v.StopRequested,
			CreatedAt: v.CreatedAt, StartedAt: v.StartedAt, FinishedAt: v.FinishedAt, ElapsedMS: v.ElapsedMS, Effective: v.Effective,
			RequestedBy: v.Request.RequestedBy, Reconciling: v.Reconciling, CompletionPending: v.CompletionPending,
			PersistenceReason: v.PersistenceReason, DurabilityConfirmed: v.DurabilityConfirmed, Reason: v.Reason})
		if len(out) == 2 {
			// Small pages exercise pagination.
			last := id
			if len(ids) > 0 && ids[len(ids)-1] != id {
				return out, &last, nil
			}
		}
	}
	return out, nil, nil
}

func (p *fakePlane) CancelTask(_ context.Context, id string) (contract.CancelResponse, error) {
	p.mu.Lock()
	p.cancels = append(p.cancels, id)
	v, ok := p.tasks[id]
	ignored := p.cancelIgnored
	p.mu.Unlock()
	if ok && !contract.TaskTerminal(v.State) && !ignored {
		p.finish(id, contract.TaskCancelled, nil, false, nil)
	}
	return contract.CancelResponse{}, nil
}

// setLog replaces task id's retained log (a scripted vendor stream).
func (p *fakePlane) setLog(id string, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logs[id] = data
}

func (p *fakePlane) TaskLogs(_ context.Context, id string) (contract.TaskLogsResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.logs[id]
	if !ok {
		return contract.TaskLogsResponse{}, contract.New(contract.CodeNotFound, "no logs")
	}
	return contract.TaskLogsResponse{Version: contract.ProtocolVersion, TaskID: id, Data: d, SourceBytes: len(d), RetainedBytes: len(d)}, nil
}

func (p *fakePlane) CreateWorkspace(_ context.Context, name string) (contract.WorkspaceView, error) {
	if p.createWSErr != nil {
		return contract.WorkspaceView{}, p.createWSErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ws != nil || strings.HasPrefix(name, "e2e-"+probeCoderNamespace+"-") {
		// A further workspace has its own instance and refs (a Cursor
		// probe's coder workspace, created before the probe fixture's, is
		// never the primary one).
		w := &contract.WorkspaceView{Name: name, Instance: hexID("", 0xabc+len(p.others)+1)}
		p.others = append(p.others, w)
		if p.otherRefs == nil {
			p.otherRefs = map[string]map[string]string{}
		}
		p.otherRefs[name] = map[string]string{}
		return *w, nil
	}
	p.ws = &contract.WorkspaceView{Name: name, Instance: hexID("", 0xabc)}
	return *p.ws, nil
}

// workspaceLocked is the named workspace (nil: none).
func (p *fakePlane) workspaceLocked(name string) *contract.WorkspaceView {
	if p.ws != nil && p.ws.Name == name {
		return p.ws
	}
	for _, w := range p.others {
		if w.Name == name {
			return w
		}
	}
	return nil
}

// instanceOf is the named workspace's instance (the primary's when none).
func (p *fakePlane) instanceOf(name string) string {
	if w := p.workspaceLocked(name); w != nil {
		return w.Instance
	}
	return p.ws.Instance
}

// showCount is the number of task fetches so far.
func (p *fakePlane) ids() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.order...)
}

func (p *fakePlane) showCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shows
}

// resultCommit returns task id's published commit.
func (p *fakePlane) resultCommit(id string) (plumbing.Hash, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.tasks[id]
	if !ok || v.Result == nil || v.Result.Workspace == nil || v.Result.Workspace.Commit == nil {
		return plumbing.ZeroHash, false
	}
	return plumbing.NewHash(*v.Result.Workspace.Commit), true
}

// fakePuller installs a task's published result from the hub.
type fakePuller struct {
	p   *fakePlane
	err error
}

func (f *fakePuller) PullResult(_ context.Context, task, repo string) error {
	if f.err != nil {
		return f.err
	}
	c, ok := f.p.resultCommit(task)
	if !ok {
		return errors.New("no published result")
	}
	r, err := git.PlainOpen(repo)
	if err != nil {
		return err
	}
	return copyObjects(f.p.hub, r.Storer, []plumbing.Hash{c})
}

// writeCommit stores a commit of files (all at the tree root) with parent
// into st and returns it.
func writeCommit(t testing.TB, st *memory.Storage, files map[string][]byte, parents ...plumbing.Hash) (plumbing.Hash, plumbing.Hash) {
	t.Helper()
	return writeCommitMode(t, st, files, nil, parents...)
}

// writeCommitMode is writeCommit with per-file modes (default regular).
func writeCommitMode(t testing.TB, st *memory.Storage, files map[string][]byte, modes map[string]filemode.FileMode, parents ...plumbing.Hash) (plumbing.Hash, plumbing.Hash) {
	t.Helper()
	tree := &object.Tree{}
	for _, name := range sortedKeys(files) {
		o := st.NewEncodedObject()
		o.SetType(plumbing.BlobObject)
		w, _ := o.Writer()
		w.Write(files[name])
		w.Close()
		h, err := st.SetEncodedObject(o)
		if err != nil {
			t.Fatal(err)
		}
		mode := filemode.Regular
		if m, ok := modes[name]; ok {
			mode = m
		}
		tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: mode, Hash: h})
	}
	to := st.NewEncodedObject()
	if err := tree.Encode(to); err != nil {
		t.Fatal(err)
	}
	th, err := st.SetEncodedObject(to)
	if err != nil {
		t.Fatal(err)
	}
	sig := object.Signature{Name: "Callsheet", Email: "callsheet@invalid", When: time.Unix(0, 0).UTC()}
	c := &object.Commit{Author: sig, Committer: sig, Message: "result\n", TreeHash: th, ParentHashes: parents}
	co := st.NewEncodedObject()
	if err := c.Encode(co); err != nil {
		t.Fatal(err)
	}
	ch, err := st.SetEncodedObject(co)
	if err != nil {
		t.Fatal(err)
	}
	return ch, th
}

// withFiles returns base with changes applied (nil deletes).
func withFiles(base map[string][]byte, changes map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range changes {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

// fixtureMain and fixtureTest are an actually conforming implementation of
// the feature: run with the required signature and the ten TestRun table
// subtests named by the fixed case IDs.
const fixtureMain = `// Command greeting prints a greeting.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// run is the testable command; it returns the exit status.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("greeting", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "world", "the name to greet")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "greeting: unexpected argument", fs.Arg(0))
		return 2
	}
	if *name == "" {
		fmt.Fprintln(stderr, "greeting: --name must not be empty")
		return 2
	}
	fmt.Fprintf(stdout, "Hello, %s!\n", *name)
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
`

const fixtureTest = `package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		id     string
		args   []string
		code   int
		stdout string
		stderr bool
	}{
		{"no_args", nil, 0, "Hello, world!\n", false},
		{"name", []string{"--name", "Ada"}, 0, "Hello, Ada!\n", false},
		{"spaces", []string{"--name", "Ada Lovelace"}, 0, "Hello, Ada Lovelace!\n", false},
		{"unicode", []string{"--name", "世界"}, 0, "Hello, 世界!\n", false},
		{"whitespace", []string{"--name", " "}, 0, "Hello,  !\n", false},
		{"empty_name", []string{"--name", ""}, 2, "", true},
		{"unknown_flag", []string{"--unknown"}, 2, "", true},
		{"missing_value", []string{"--name"}, 2, "", true},
		{"positional", []string{"Ada"}, 2, "", true},
		{"help", []string{"--help"}, 0, "", true},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(c.args, &stdout, &stderr); got != c.code {
				t.Fatalf("run(%q) = %d, want %d", c.args, got, c.code)
			}
			if stdout.String() != c.stdout || (stderr.Len() > 0) != c.stderr {
				t.Fatalf("run(%q) wrote %q and %q", c.args, stdout.String(), stderr.String())
			}
			if c.id == "help" && !strings.Contains(stderr.String(), "name") {
				t.Fatalf("usage %q does not mention name", stderr.String())
			}
		})
	}
}
`

// Hop contents of the scripted happy path.
var (
	designDoc     = []byte("# Design\n\nAdd --name with flag parsing in run(args, stdout, stderr); TestRun covers every case ID.\n\nSTATUS: DESIGN_DRAFT\n")
	designReview  = []byte("# Review\n\nThe design covers every case.\n\nSTATUS: DESIGN_REVIEW_APPROVED\n")
	codedMain     = []byte(fixtureMain)
	codedMainTest = []byte(fixtureTest)
)

// testSession is the fake coordinator's session ID.
const testSession = "0f1e2d3c-4b5a-4968-8786-a5b4c3d2e1f0"

// fakeSession is the fake headless coordinator's output: each phase's
// print-mode stream records (written to the phase's stdout) and the
// session transcript (rewritten whole under the fake Claude home, as the
// vendor appends to it).
type fakeSession struct {
	t          testing.TB
	home       string
	sid        string
	cwd        string
	transcript []string
	stream     [2]io.Writer
	n          int
	// path is the transcript's location below the fake home.
	path string
}

// rec renders one JSON record.
func rec(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// emit writes a record to phase p's stream (with session_id).
func (s *fakeSession) emit(p int, v map[string]any) {
	if s.stream[p-1] == nil {
		// The phase is not running: only the transcript records it.
		return
	}
	v["session_id"] = s.sid
	io.WriteString(s.stream[p-1], rec(v)+"\n")
}

// log appends a record to the transcript (with sessionId and cwd).
func (s *fakeSession) log(v map[string]any) {
	v["sessionId"], v["cwd"] = s.sid, s.cwd
	s.transcript = append(s.transcript, rec(v))
	s.flush()
}

// flush rewrites the transcript file.
func (s *fakeSession) flush() {
	if s.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		s.t.Error(err)
	}
	if err := os.WriteFile(s.path, []byte(strings.Join(s.transcript, "\n")+"\n"), 0o600); err != nil {
		s.t.Error(err)
	}
}

// init emits an init record of phase p (each turn opens with one), with
// edits applied.
func (s *fakeSession) init(p int, sid string, edits ...func(v map[string]any)) {
	v := map[string]any{"type": "system", "subtype": "init", "session_id": sid, "cwd": s.cwd, "model": "claude-opus-5-5",
		"permissionMode": autoModeFlag, "tools": []string{"Bash", "Read", toolDispatch, toolTaskShow}, "mcp_servers": []map[string]string{{"name": "callsheet", "status": "connected"}}}
	for _, e := range edits {
		e(v)
	}
	io.WriteString(s.stream[p-1], rec(v)+"\n")
}

// prompt logs phase p's user prompt.
func (s *fakeSession) prompt(text string) {
	s.log(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
}

// use logs and emits one tool use and its result; it returns the tool
// use ID.
func (s *fakeSession) use(p int, name string, input map[string]any, result string, isError bool) string {
	s.n++
	id := fmt.Sprintf("toolu_%04d", s.n)
	use := func() map[string]any {
		return map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant",
			"content": []map[string]any{{"type": "tool_use", "id": id, "name": name, "input": input}}}}
	}
	res := func() map[string]any {
		return map[string]any{"type": "user", "message": map[string]any{"role": "user",
			"content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": result, "is_error": isError}}}}
	}
	s.emit(p, use())
	s.log(use())
	s.emit(p, res())
	s.log(res())
	return id
}

// say logs and emits assistant text.
func (s *fakeSession) say(p int, text string) {
	m := func() map[string]any {
		return map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": text}}}}
	}
	s.emit(p, m())
	s.log(m())
}

// user logs an ordinary user-role text message.
func (s *fakeSession) user(text string) {
	s.log(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
}

// notify logs the background completion notification of a handle.
func (s *fakeSession) notify(handle, toolUse string) {
	s.user("<task-notification>\n<task-id>" + handle + "</task-id>\n<tool-use-id>" + toolUse + "</tool-use-id>\n<status>completed</status>\n" +
		"<summary>Background command completed (exit code 0)</summary>\n</task-notification>")
}

// result emits phase p's terminal result record.
func (s *fakeSession) result(p int, ok bool, denials []map[string]string) {
	sub := "success"
	if !ok {
		sub = "error_during_execution"
	}
	v := map[string]any{"type": "result", "subtype": sub, "is_error": !ok, "result": "Phase done."}
	if denials != nil {
		v["permission_denials"] = denials
	}
	s.emit(p, v)
}

// coordScript is the scripted headless coordinator: each step runs on one
// ListTasks call of the feature loop, on the supervisor's goroutine, while
// its phase process is running.
type coordScript struct {
	t      testing.TB
	f      *fakeWorld
	s      *supervisor
	p      *fakePlane
	step   int
	codes  []string
	tasks  [4]string
	files  [5]map[string][]byte
	commit [5]plumbing.Hash
	// The two phase processes and the session they write.
	procs   [2]*fakeProc
	specs   [2]ProcSpec
	sess    *fakeSession
	handles [4]string
	waitUse [4]string
	// Variations.
	finals             [4]string
	hopFiles           [4]map[string][]byte
	parentOf           func(i int) plumbing.Hash
	dispatchEarlyCoder bool
	stopAfter          int
	goalOf             func(i int, goal string) string
	baseOf             func(i int, base string) string
	failHop            int
	extraAt            int
	waitExit           string
	overrideHop        int
	nilFinal           int
	unpublished        int
	reviewerMode       bool
	instanceOf         func(i int) string
	// eager dispatches the design reviewer in the same instant the
	// designer's result lands: "observe" reports it at once, "monitor"
	// leaves it to the supervisor's scan and reports it a step later.
	eager          string
	pendingObserve bool
	before         func(c *coordScript) bool
	// Session variations.
	noWake      [4]bool
	userBetween int
	phaseExit   [2]int
	phase2SID   string
	rewrite     bool
	badWake     bool
	longPoll    bool
	denied      bool
	firstDenied bool
	// deniedBeforeWait dispatches and observes the designer, then a denied
	// Bash command ends phase 1 before its wait starts (A3.8 review C1).
	deniedBeforeWait bool
	toolFailed       bool
	earlyResult      bool
	waitCommand      func(i int, cmd string) string
	lingering        [2]bool
	stubborn         [2]bool
	noTranscript     bool
	afterPhase       func(c *coordScript, p int)
	failResult       [2]bool
	// perTurn ends the turn after each hop's background wait starts and
	// opens the wake's turn with a new init, edited by turnInit when set
	// (headless claude -p, A3.9).
	perTurn  bool
	turnInit func(i int, v map[string]any)
}

func newCoordScript(t testing.TB) *coordScript {
	c := &coordScript{t: t, failHop: -1, extraAt: -1, stopAfter: -1, waitExit: "0", overrideHop: -1, nilFinal: -1, unpublished: -1, userBetween: -1}
	c.finals = [4]string{"Design written.\nSTATUS: DESIGN_DRAFT\n", "Approved.\nSTATUS: DESIGN_REVIEW_APPROVED\n",
		"Implemented.\nSTATUS: IMPL_COMPLETE\n", "Looks right.\nSTATUS: REVIEW_APPROVED\n"}
	c.hopFiles = [4]map[string][]byte{{"design.md": designDoc}, {"design-review.md": designReview},
		{"main.go": codedMain, "main_test.go": codedMainTest}, {}}
	return c
}

// started records a coordinator phase process (the fake launcher's
// claude behavior).
func (c *coordScript) started(spec ProcSpec, p *fakeProc) {
	k := 0
	if slices.Contains(spec.Args, "--resume") {
		k = 1
	}
	c.procs[k], c.specs[k] = p, spec
	p.mu.Lock()
	p.lingering = c.lingering[k]
	p.ignoreTerm, p.ignoreKill = c.stubborn[k], c.stubborn[k]
	p.mu.Unlock()
	if c.sess == nil {
		c.sess = &fakeSession{t: c.t, home: c.f.home, sid: testSession, cwd: spec.Dir}
		if !c.noTranscript {
			c.sess.path = filepath.Join(c.f.home, ".claude", "projects", strings.ReplaceAll(spec.Dir, string(filepath.Separator), "-"), testSession+".jsonl")
		}
	}
	c.sess.stream[k] = spec.Stdout
}

// promptOf is a launch's -p argument.
func promptOf(spec ProcSpec) string {
	for i, a := range spec.Args {
		if a == "-p" && i+1 < len(spec.Args) {
			return spec.Args[i+1]
		}
	}
	return ""
}

// goal is hop i's goal text.
func (c *coordScript) goal(i int) string {
	g := "Do hop " + Hops[i] + "."
	if i == 3 {
		g = "Review the implementation.\n" + string(c.reviewMaterial())
	}
	if c.goalOf != nil {
		g = c.goalOf(i, g)
	}
	return g
}

// reviewMaterial is the review-material helper's output as the
// coordinator appends it: the pulled coder files in coord/coder, admitted
// and hashed by the supervisor. A refused request falls back to the
// coordinator's own blocks so a scenario can still reach its check.
func (c *coordScript) reviewMaterial() []byte {
	dir := filepath.Join(c.s.runtime, "coord", "coder")
	os.MkdirAll(dir, 0o700)
	for _, p := range inlinePaths {
		os.WriteFile(filepath.Join(dir, p), c.files[3][p], 0o600)
	}
	if resp := c.s.serveRequest(ControlRequest{Run: c.s.runID, Op: opReview}); resp.OK {
		out, err := reviewOutput(c.s.runtime, resp.Files)
		if err != nil {
			c.t.Errorf("review-material: %v", err)
		}
		return out
	}
	var b bytes.Buffer
	for _, p := range inlinePaths {
		b.WriteString(fileBlockBegin(p, sha256Hex(c.files[3][p])))
		b.Write(c.files[3][p])
		b.WriteString(fileBlockEnd(p))
	}
	return b.Bytes()
}

// phaseOf is the running phase (1 or 2) for hop i.
func (c *coordScript) phaseOf(i int) int { return hopPhase(i) }

// dispatch dispatches hop i as the coordinator would, observes it and
// starts its background wait.
func (c *coordScript) dispatch(i int) {
	c.dispatchOnly(i)
	c.observe(i)
	c.startWait(i)
}

// observe reports hop i's task as the coordinator's observe helper would.
func (c *coordScript) observe(i int) {
	code := c.s.handleRequest(ControlRequest{Run: c.s.runID, Op: opObserve, Task: c.tasks[i], Hop: Hops[i]})
	c.codes = append(c.codes, code)
	if c.sess != nil && c.sess.stream[c.phaseOf(i)-1] != nil {
		c.sess.use(c.phaseOf(i), toolBash, map[string]any{"command": fakeReale2e + " observe --run " + c.s.runtime + " --task " + c.tasks[i] + " --hop " + Hops[i],
			"description": "observe"}, code, false)
	}
}

// startWait starts hop i's background wait (the rendered template).
func (c *coordScript) startWait(i int) {
	cmd := strings.ReplaceAll(c.s.waitTemplate(i), "TASK_ID", c.tasks[i])
	if c.waitCommand != nil {
		cmd = c.waitCommand(i, cmd)
	}
	c.handles[i] = fmt.Sprintf("b%d", i+1)
	c.waitUse[i] = c.sess.use(c.phaseOf(i), toolBash, map[string]any{"command": cmd, "run_in_background": true, "description": "wait"},
		"Command running in background with ID: "+c.handles[i]+". Output is being written to: /tmp/x", false)
}

// dispatchOnly dispatches hop i without observing it.
func (c *coordScript) dispatchOnly(i int) {
	base := c.commit[i].String()
	if c.baseOf != nil {
		base = c.baseOf(i, base)
	}
	ws, inst := c.s.workspace.Name, c.s.workspace.Instance
	if c.instanceOf != nil && c.instanceOf(i) != "" {
		inst = c.instanceOf(i)
	}
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: Hops[i]}, Goal: c.goal(i), Payload: []string{markerFor(c.s.runID, Hops[i])},
		Acceptance: "done", RequestedBy: contract.RequestedBy{Name: "claude-code", Version: "2.1.292", Hostname: "localhost"}, Workspace: &ws, Base: &base, WorkspaceInstance: &inst}
	if c.overrideHop == i {
		req.Override = &contract.TaskOverride{Effort: ptr("max")}
	}
	v, err := c.p.Dispatch(context.Background(), req)
	if err != nil {
		c.t.Fatalf("coordinator dispatch %s: %v", Hops[i], err)
	}
	c.tasks[i] = v.TaskID
	if c.sess != nil && c.sess.stream[c.phaseOf(i)-1] != nil {
		out, _ := json.Marshal(map[string]any{"task_id": v.TaskID, "state": v.State})
		c.sess.use(c.phaseOf(i), toolDispatch, map[string]any{"target": map[string]string{"kind": "id", "value": Hops[i]}, "goal": req.Goal,
			"acceptance": req.Acceptance, "payload": req.Payload, "workspace": ws, "workspace_instance": inst, "base": base}, string(out), false)
	}
}

// complete finishes hop i's task with its commit and wait files, and the
// coordinator consumes the completion.
func (c *coordScript) complete(i int) {
	files := withFiles(c.files[i], c.hopFiles[i])
	parent := c.commit[i]
	if c.parentOf != nil {
		parent = c.parentOf(i)
	}
	var h plumbing.Hash
	if c.reviewerMode && i == 3 {
		h, _ = writeCommitMode(c.t, c.p.hub, files, map[string]filemode.FileMode{"main.go": filemode.Executable}, parent)
	} else {
		h, _ = writeCommit(c.t, c.p.hub, files, parent)
	}
	c.files[i+1], c.commit[i+1] = files, h
	state, final := contract.TaskSucceeded, &c.finals[i]
	if c.failHop == i {
		state = contract.TaskFailed
	}
	if c.nilFinal == i {
		final = nil
	}
	commit := &h
	if c.unpublished == i {
		commit = nil
	}
	c.p.finish(c.tasks[i], state, final, false, commit)
	v, _ := c.p.ShowTask(context.Background(), c.tasks[i], contract.DefaultTailLines)
	wr := contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitTerminal, EffectiveWaitMS: 30000, Winner: c.tasks[i], Task: &v}
	b, err := json.Marshal(wr)
	if err != nil {
		c.t.Fatal(err)
	}
	// The rendered wait: the wait-task helper's admission, the CLI wait's
	// output in the hop's files, then the helper's capture.
	dir := waitDir(c.s.runtime, i)
	if got := c.s.handleRequest(ControlRequest{Run: c.s.runID, Op: opWaitTask, Task: c.tasks[i], Hop: Hops[i]}); got != CodeAccepted {
		c.t.Errorf("wait-task %s = %s", Hops[i], got)
	}
	os.WriteFile(filepath.Join(dir, "result.json"), append(b, '\n'), 0o600)
	os.WriteFile(filepath.Join(dir, "error.txt"), nil, 0o600)
	if c.waitExit != "" {
		code, _ := strconv.Atoi(c.waitExit)
		if err := captureFiles(c.s.runtime, i, code, io.Discard, io.Discard); err != nil {
			c.t.Errorf("wait-task %s: %v", Hops[i], err)
		}
	}
	p := c.phaseOf(i)
	if c.perTurn {
		c.sess.result(p, true, nil)
		c.sess.init(p, c.sess.sid, func(v map[string]any) {
			if c.turnInit != nil {
				c.turnInit(i, v)
			}
		})
	}
	switch {
	case c.badWake && i == 0:
		c.sess.user("<task-notification><task-id>" + c.handles[i] + "</task-id></task-notification> and more")
	case !c.noWake[i]:
		c.sess.notify(c.handles[i], c.waitUse[i])
	}
	if c.userBetween == i {
		c.sess.user("are you done yet?")
	}
	if c.longPoll && i == 0 {
		c.sess.use(p, toolTaskWait, map[string]any{"task_ids": []string{c.tasks[i]}, "wait": "5m"}, `{"status":"terminal"}`, false)
	}
	v, _ = c.p.ShowTask(context.Background(), c.tasks[i], contract.DefaultTailLines)
	show, _ := json.Marshal(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v})
	c.sess.use(p, toolTaskShow, map[string]any{"id": c.tasks[i]}, string(show), false)
}

// endPhase ends phase p with its result and exit.
func (c *coordScript) endPhase(p int) {
	var denials []map[string]string
	if c.denied && p == 1 {
		c.sess.use(p, toolBash, map[string]any{"command": "rm -rf /tmp/whatever"}, "Permission to use Bash with command rm -rf /tmp/whatever has been denied.", true)
		denials = []map[string]string{{"tool_name": "Bash", "tool_use_id": fmt.Sprintf("toolu_%04d", c.sess.n)}}
	}
	if c.toolFailed && p == 2 {
		c.sess.use(p, "Read", map[string]any{"file_path": "/nonexistent"}, "File does not exist.", true)
	}
	c.sess.say(p, "Phase "+fmt.Sprint(p)+" done.")
	c.sess.result(p, !c.failResult[p-1], denials)
	if c.afterPhase != nil {
		c.afterPhase(c, p)
	}
	c.procs[p-1].exit(c.phaseExit[p-1], "")
}

// beginPhase writes phase p's init and prompt.
func (c *coordScript) beginPhase(p int) {
	sid := c.sess.sid
	if p == 2 && c.phase2SID != "" {
		sid = c.phase2SID
	}
	c.sess.init(p, sid)
	if p == 2 && c.rewrite {
		// The vendor rewrote (compacted) the transcript on resume.
		c.sess.transcript[0] += " "
	}
	c.sess.prompt(promptOf(c.specs[p-1]))
}

// run is the ListTasks hook.
func (c *coordScript) run() {
	s := c.s
	if s.ctl == nil || s.failed || c.step == c.stopAfter {
		return
	}
	if c.before != nil && (c.before(c) || s.failed) {
		return
	}
	switch c.step {
	case 0:
		if c.procs[0] == nil {
			return
		}
		c.beginPhase(1)
		if c.firstDenied {
			// The first-run shape: the first tool call is denied and
			// phase 1 ends at once with a result and exit 0.
			c.sess.use(1, "mcp__callsheet__ws_push", map[string]any{"name": s.workspace.Name, "path": s.seed.Path},
				"Permission to use mcp__callsheet__ws_push has been denied.", true)
			denials := []map[string]string{{"tool_name": "mcp__callsheet__ws_push", "tool_use_id": fmt.Sprintf("toolu_%04d", c.sess.n)}}
			c.sess.say(1, "I cannot continue.")
			c.sess.result(1, true, denials)
			c.procs[0].exit(0, "")
			c.step = 99
			return
		}
		seed, err := git.PlainOpen(s.seed.Path)
		if err != nil {
			c.t.Fatal(err)
		}
		if err := copyObjects(seed.Storer, c.p.hub, []plumbing.Hash{s.seed.Commit}); err != nil {
			c.t.Fatal(err)
		}
		c.sess.use(1, "mcp__callsheet__ws_push", map[string]any{"name": s.workspace.Name, "path": s.seed.Path}, `{"commit":"`+s.seed.Commit.String()+`"}`, false)
		c.files[0], c.commit[0] = s.seed.Files, s.seed.Commit
		if c.extraAt == 0 {
			c.extra(0)
		}
		if c.deniedBeforeWait {
			c.dispatchOnly(0)
			c.observe(0)
			c.sess.use(1, toolBash, map[string]any{"command": "rm -rf /tmp/whatever"}, "Permission to use Bash with command rm -rf /tmp/whatever has been denied.", true)
			denials := []map[string]string{{"tool_name": "Bash", "tool_use_id": fmt.Sprintf("toolu_%04d", c.sess.n)}}
			c.sess.say(1, "I cannot continue.")
			c.sess.result(1, true, denials)
			c.procs[0].exit(0, "")
			c.step = 99
			return
		}
		c.dispatch(0)
	case 1:
		c.complete(0)
		if c.earlyResult {
			c.endPhase(1)
			c.step = 99
			return
		}
		switch c.eager {
		case "observe":
			c.dispatch(1)
			c.step = 2
		case "monitor":
			c.dispatchOnly(1)
			c.pendingObserve, c.step = true, 2
		}
	case 2:
		if !s.hops[0].terminal {
			return
		}
		c.dispatch(1)
	case 3:
		if c.pendingObserve {
			c.pendingObserve = false
			c.observe(1)
			c.startWait(1)
			return
		}
		c.complete(1)
	case 4:
		c.endPhase(1)
	case 5:
		if c.dispatchEarlyCoder && s.owner == ownerAwaiting {
			c.dispatch(2)
			return
		}
		if c.procs[1] == nil {
			return
		}
		c.beginPhase(2)
		if c.extraAt == 2 {
			c.extra(2)
			return
		}
		c.dispatch(2)
	case 6:
		c.complete(2)
	case 7:
		if !s.hops[2].terminal {
			return
		}
		c.dispatch(3)
	case 8:
		c.complete(3)
	case 9:
		if !s.complete {
			return
		}
		c.endPhase(2)
	default:
		return
	}
	c.step++
}

// extra dispatches an unobserved second task of hop i.
func (c *coordScript) extra(i int) {
	base := c.commit[i].String()
	ws, inst := c.s.workspace.Name, c.s.workspace.Instance
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: Hops[i]}, Goal: "extra", Payload: []string{markerFor(c.s.runID, Hops[i])},
		Acceptance: "done", RequestedBy: contract.RequestedBy{Name: "claude-code", Version: "2.1.292", Hostname: "localhost"}, Workspace: &ws, Base: &base, WorkspaceInstance: &inst}
	if _, err := c.p.Dispatch(context.Background(), req); err != nil {
		c.t.Fatal(err)
	}
}

// fakeTerminal is the operator terminal: its scripted lines are delivered
// only once the decision prompt activated it; input typed before the
// prompt is never delivered.
type fakeTerminal struct {
	mu        sync.Mutex
	lines     []string
	eof       bool
	early     []string
	activated int
	// onActivate runs when the prompt opens (a test moves the time).
	onActivate func()
}

func (t *fakeTerminal) Activate() <-chan string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.onActivate != nil {
		t.onActivate()
	}
	t.activated++
	ch := make(chan string, len(t.lines)+1)
	for _, l := range t.lines {
		ch <- l
	}
	if t.eof {
		close(ch)
	}
	return ch
}

// drainedTerminal runs the production line reader over a finite input to
// its end before the prompt receives the channel, so the outcome never
// depends on the reader goroutine racing the auto clock's timers.
type drainedTerminal struct{ r io.Reader }

func (d drainedTerminal) Activate() <-chan string {
	ch := make(chan string, 16)
	readLines(d.r, ch)
	return ch
}

// typeEarly records input typed before the prompt (discarded).
func (t *fakeTerminal) typeEarly(l string) {
	t.mu.Lock()
	t.early = append(t.early, l)
	t.mu.Unlock()
}

// fakeWorld is one offline attempt's world.
type fakeWorld struct {
	t        testing.TB
	clock    *autoClock
	launcher *fakeLauncher
	plane    *fakePlane
	puller   *fakePuller
	coord    *coordScript
	term     *fakeTerminal
	// liveTerm, when set, replaces term with a production Terminal.
	liveTerm   Terminal
	checkout   string
	evidence   string
	tempRoot   string
	home       string
	stdout     *syncBuffer
	stderr     *syncBuffer
	stdin      io.Reader
	probeErr   map[string]error
	versions   map[string]string
	connectErr error
	ctx        context.Context
	lookErr    map[string]error
	randSeed   byte
	// script overrides individual commands (nil: default).
	override func(ProcSpec) (behavior, bool)
	flow     string
	sup      *supervisor
	// executable replaces the helper's path (empty: fakeReale2e).
	executable string
	// roots are the supplied --protect-root values; git is the read-only
	// Git runner (default: the fake one, which knows only the evidence
	// checks).
	roots []string
	depth string
	git   GitRunner
	// testActions sets a TestRun/<case> action in go test -json ("" omits
	// the subtest; default pass); cases overrides a CLI case's output.
	testActions map[string]string
	cases       map[string]behavior
	// jsonExtra is appended to the go test -json stream, which exits
	// jsonExit.
	jsonExtra string
	jsonExit  int
	// build is the go build's behavior (zero: success).
	build behavior
	env   map[string]string
	// goPath replaces the go executable's path (empty: fakeGo); profile,
	// when set, replaces the final coverage run's synthesized profile.
	goPath  string
	profile func(module string, main []byte) []byte
	// light runs the semantic light-recipe Go (devlight_fakes_test.go)
	// for every light command and built binary.
	light bool
}

// goBin is the world's go executable path.
func (f *fakeWorld) goBin() string { return cmp.Or(f.goPath, fakeGo) }

// mustRead reads a file.
func mustRead(t testing.TB, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// syncBuffer is a goroutine-safe buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// repoRootOnce caches the real checkout root for template copies.
var repoRootOnce struct {
	sync.Once
	root string
}

// legacyExampleDir is the retained legacy four-hop exercise's exact bytes
// (design 12a-dev-flow-verify, Default and documentation migration): an
// offline compatibility fixture, no longer shipped documentation.
const legacyExampleDir = "internal/reale2e/testdata/legacy-example"

// newCheckout creates a minimal checkout: the Callsheet go.mod, the
// legacy templates (copied from the legacy fixture to the location the
// retained legacy supervisor reads) and design/.
func newCheckout(t testing.TB) string {
	t.Helper()
	repoRootOnce.Do(func() { repoRootOnce.root = testkit.MustRepoRoot(t) })
	co := t.TempDir()
	if err := os.WriteFile(filepath.Join(co, "go.mod"), []byte("module "+modulePath+"\n\ngo 1.26.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(co, ".gitignore"), []byte("/design/real-e2e-runs/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(co, filepath.FromSlash(ExamplesDir))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range TemplateNames() {
		b, err := os.ReadFile(filepath.Join(repoRootOnce.root, filepath.FromSlash(legacyExampleDir), n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(co, "design", "real-e2e-runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real HEAD gives the run its revision.
	r, err := git.PlainInit(co, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	wt.Add("go.mod")
	wt.Add(".gitignore")
	sig := object.Signature{Name: "t", Email: "t@invalid", When: time.Unix(0, 0)}
	if _, err := wt.Commit("checkout\n", &git.CommitOptions{Author: &sig, Committer: &sig}); err != nil {
		t.Fatal(err)
	}
	return co
}

// newHome creates a fake owner home with owner configuration in each
// vendor home (Cursor absent).
func newHome(t testing.TB) string {
	t.Helper()
	h := t.TempDir()
	for rel, content := range map[string]string{
		".claude/CLAUDE.md":          "# my instructions\n",
		".claude/settings.json":      `{"permissions":{"allow":["Bash(callsheet:*)"]}}` + "\n",
		".claude/history.jsonl":      `{"display":"old"}` + "\n",
		".claude/.credentials.json":  `{"token":"old"}` + "\n",
		".claude/skills/a/SKILL.md":  "skill\n",
		".claude/projects/-old/x.md": "memory\n",
		".codex/config.toml":         "model = \"x\"\n",
		".codex/AGENTS.md":           "# codex instructions\n",
		".codex/auth.json":           "{}\n",
		".grok/settings.json":        "{}\n",
	} {
		p := filepath.Join(h, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// shortRoot is the short parent of every socket-bearing fixture.
const shortRoot = "/tmp"

// runFileJSON is a version 2 run file for run whose wait runs callsheet.
func runFileJSON(run, callsheet string) string {
	return `{"schema":"` + RunFileSchema + `","run_id":"` + run + `","callsheet":"` + callsheet + `","plane":"https://127.0.0.1:1","ca":"/rt/ca.crt"}` + "\n"
}

// shortTemp returns a short private temporary root (socket paths stay
// within their bound whatever TMPDIR is).
func shortTemp(t testing.TB) string {
	t.Helper()
	// Explicitly under /tmp, never $TMPDIR: an inherited TMPDIR may be
	// arbitrarily long, and the runtime's Unix socket must stay within
	// the 90-byte production bound (/tmp/reNNNNNNNNNN/ce-NNNNNNNNNN/s.sock).
	d, err := os.MkdirTemp(shortRoot, "re")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	// Canonical, as the supervisor's runtime is (macOS /var is a link).
	if d, err = filepath.EvalSymlinks(d); err != nil {
		t.Fatal(err)
	}
	return d
}

// fakeGitRunner answers the evidence destination's checks (ignored, no
// tracked path) and fails any repository probe (no protected root).
func fakeGitRunner(t testing.TB) GitRunner {
	return func(dir string, _ io.Writer, args ...string) ([]byte, int, error) {
		switch {
		case len(args) > 0 && args[0] == "check-ignore":
			return nil, 0, nil
		case len(args) > 0 && args[0] == "ls-files":
			return nil, 0, nil
		}
		t.Errorf("unexpected git probe in %s: %v", dir, args)
		return nil, 128, errors.New("unexpected git probe")
	}
}

func newFakeWorld(t testing.TB) *fakeWorld {
	f := &fakeWorld{t: t, clock: newAutoClock(), stdout: &syncBuffer{}, stderr: &syncBuffer{}, probeErr: map[string]error{}, lookErr: map[string]error{},
		ctx: context.Background(), versions: map[string]string{fakeClaude: "2.1.290 (Claude Code)", fakeCodex: "codex-cli 0.160.0", fakeGrok: "grok 1.0.47 (abcdef0) [stable]"},
		testActions: map[string]string{}, cases: map[string]behavior{}, env: map[string]string{EnvOptIn: "1"}}
	f.checkout = newCheckout(t)
	// The bundle directory itself: new, under the checkout's design/ tree.
	f.evidence = filepath.Join(f.checkout, "design", "real-e2e-runs", "attempt")
	f.tempRoot = shortTemp(t)
	f.home = newHome(t)
	f.plane = newFakePlane(t, f.clock)
	f.puller = &fakePuller{p: f.plane}
	f.launcher = &fakeLauncher{script: f.script}
	f.coord = newCoordScript(t)
	f.coord.f = f
	f.term = &fakeTerminal{lines: []string{"yes"}}
	f.git = fakeGitRunner(t)
	f.stdin = strings.NewReader("")
	return f
}

// script is the default behavior of every child.
func (f *fakeWorld) script(s ProcSpec) behavior {
	if f.override != nil {
		if b, ok := f.override(s); ok {
			return b
		}
	}
	return f.base(s)
}

// conformingCase is a conforming program's output for case c.
func conformingCase(c FeatureCase) behavior {
	switch c.Kind {
	case "ok":
		return behavior{stdout: c.Stdout}
	case "error":
		return behavior{stderr: "greeting: invalid arguments\n", exit: 2}
	}
	return behavior{stderr: "Usage of greeting:\n  -name string\n    \tthe name to greet\n"}
}

// testJSON is a go test -json stream for the ten TestRun subtests with the
// world's actions.
func (f *fakeWorld) testJSON() string {
	var b strings.Builder
	b.WriteString(`{"Action":"start","Package":"example.com/greeting"}` + "\n")
	b.WriteString(`{"Action":"run","Package":"example.com/greeting","Test":"TestRun"}` + "\n")
	for _, c := range FeatureCases {
		action, set := f.testActions[c.ID]
		if !set {
			action = "pass"
		}
		if action == "" {
			continue
		}
		fmt.Fprintf(&b, `{"Action":"run","Package":"example.com/greeting","Test":"TestRun/%s"}`+"\n", c.ID)
		fmt.Fprintf(&b, `{"Action":"%s","Package":"example.com/greeting","Test":"TestRun/%s","Elapsed":0}`+"\n", action, c.ID)
	}
	b.WriteString(`{"Action":"pass","Package":"example.com/greeting","Test":"TestRun","Elapsed":0}` + "\n")
	b.WriteString(`{"Action":"pass","Package":"example.com/greeting","Elapsed":0.01}` + "\n")
	return b.String()
}

// base is a child's default behavior.
func (f *fakeWorld) base(s ProcSpec) behavior {
	a := s.Args
	if f.light && (s.Path == f.goBin() || filepath.Base(s.Path) == "greeting") {
		if b, ok := lightGoBehavior(s); ok {
			return b
		}
	}
	switch {
	case s.Path == fakeCallsheet && len(a) == 1 && a[0] == "version":
		return behavior{stdout: "callsheet dev protocol=6\n"}
	case len(a) == 1 && a[0] == "--version" && f.versions[s.Path] != "":
		return behavior{stdout: f.versions[s.Path] + "\n"}
	case s.Path == fakeClaude && len(a) > 1 && a[0] == "-p":
		spec := s
		return behavior{long: true, onStart: func(p *fakeProc) { f.coord.started(spec, p) }}
	case s.Path == f.goBin() && len(a) == 1 && a[0] == "version":
		return behavior{stdout: "go version go1.26.4 linux/amd64\n"}
	case s.Path == f.goBin() && strings.Join(a, " ") == "test -buildvcs=false -count=1 ./...":
		return behavior{stdout: "ok  \texample.com/greeting\t0.01s\n"}
	case s.Path == f.goBin() && strings.Join(a, " ") == "test -json -buildvcs=false -count=1 ./...":
		return behavior{stdout: f.testJSON() + f.jsonExtra, exit: f.jsonExit}
	case s.Path == f.goBin() && len(a) == 5 && strings.Join(a[:3], " ") == "build -buildvcs=false -o" && a[4] == ".":
		return f.build
	case s.Path == f.goBin() && len(a) == 6 && strings.Join(a[:4], " ") == "test -buildvcs=false -count=1 -covermode=set" &&
		strings.HasPrefix(a[4], "-coverprofile=") && a[5] == "./...":
		dir, out := s.Dir, strings.TrimPrefix(a[4], "-coverprofile=")
		return behavior{stdout: "ok  \texample.com/greeting\t0.01s\tcoverage: 88.9% of statements\n", after: func() {
			main, _ := os.ReadFile(filepath.Join(dir, "main.go"))
			gomod, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
			profile := synthProfile(modulePathOf(gomod), main)
			if f.profile != nil {
				profile = f.profile(modulePathOf(gomod), main)
			}
			os.WriteFile(out, profile, 0o600)
		}}
	case filepath.Base(s.Path) == "greeting":
		for _, c := range FeatureCases {
			if slices.Equal(c.Args, a) {
				if b, ok := f.cases[c.ID]; ok {
					return b
				}
				return conformingCase(c)
			}
		}
	case s.Path == fakeGit:
		return behavior{stdout: "git version 2.43.0\n"}
	case s.Path == fakeCallsheet && len(a) > 2 && a[0] == "plane" && a[1] == "init":
		state := a[3]
		return behavior{after: func() {
			os.MkdirAll(filepath.Join(state, "pki"), 0o700)
			os.WriteFile(filepath.Join(state, "pki", "ca.crt"), []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"), 0o600)
		}}
	case s.Path == fakeCallsheet && len(a) > 1 && a[0] == "plane" && a[1] == "run":
		return behavior{long: true, stderr: `{"time":"t","level":"INFO","msg":"listening","bind":"127.0.0.1:43210"}` + "\n"}
	case s.Path == fakeCallsheet && len(a) > 1 && a[0] == "sidecar" && a[1] == "enroll":
		n := map[string]int{"n-codex": 1, "n-claude": 2, "n-grok": 3, "n-cursor": 4}[filepath.Base(a[len(a)-1])]
		return behavior{stdout: "node_id: " + hexID("n_", n) + "\n"}
	case s.Path == fakeCallsheet && len(a) > 1 && a[0] == "sidecar" && a[1] == "run":
		return behavior{long: true, stderr: `{"msg":"heartbeat acknowledged","session":1}` + "\n"}
	}
	f.t.Errorf("unexpected command %s %v", s.Path, a)
	return behavior{exit: 127}
}

// world returns the supervisor's world.
func (f *fakeWorld) world() *World {
	var term Terminal = f.term
	if f.liveTerm != nil {
		term = f.liveTerm
	}
	return &World{Context: f.ctx, Launcher: f.launcher, Clock: f.clock, Environ: []string{"PATH=/fake/bin", "HOME=" + f.home}, Cwd: f.checkout,
		Executable: cmp.Or(f.executable, fakeReale2e), Home: f.home, TempRoot: f.tempRoot, Rand: &countingRand{n: f.randSeed},
		LookPath: func(n string) (string, error) {
			if err := f.lookErr[n]; err != nil {
				return "", err
			}
			if n == "go" {
				return f.goBin(), nil
			}
			return "/fake/bin/" + n, nil
		},
		Probe: func(_ context.Context, vendor, exe, dir string) error { return f.probeErr[vendor] },
		Connect: func(url string, ca []byte) (Plane, Puller, func(), error) {
			if f.connectErr != nil {
				return nil, nil, nil, f.connectErr
			}
			return f.plane, f.puller, func() {}, nil
		},
		Poll: 2 * time.Second, Terminal: term, Git: f.git}
}

// countingRand is a deterministic random source.
type countingRand struct{ n byte }

func (r *countingRand) Read(p []byte) (int, error) {
	for i := range p {
		r.n++
		p[i] = r.n
	}
	return len(p), nil
}

// args is run's command line.
func (f *fakeWorld) args() []string {
	args := []string{"run", "--callsheet", fakeCallsheet, "--claude", fakeClaude, "--codex", fakeCodex, "--grok", fakeGrok, "--evidence", f.evidence}
	if f.flow != "" {
		args = append(args, "--flow", f.flow)
	}
	for _, r := range f.roots {
		args = append(args, "--protect-root", r)
	}
	if f.depth != "" {
		args = append(args, "--protect-depth", f.depth)
	}
	return args
}

// host returns run's host inputs with the opt-in set; the scripted
// coordinator sees the new supervisor through its observe seam.
func (f *fakeWorld) host() Host {
	return Host{GOOS: "linux", GOARCH: "amd64", Args: f.args(), legacy: true,
		Getenv: func(k string) string { return f.env[k] },
		LookupEnv: func(k string) (string, bool) {
			v, ok := f.env[k]
			return v, ok
		},
		Stdin: f.stdin, Stdout: f.stdout, Stderr: f.stderr,
		NewWorld: func(Host) (*World, func(), error) {
			w := f.world()
			return w, func() {}, nil
		},
		observe: func(s *supervisor) {
			f.sup = s
			f.coord.s = s
		}}
}

// attempt runs one offline attempt through MainWith; the scripted
// coordinator acts inside the plane's ListTasks calls.
func (f *fakeWorld) attempt() int {
	f.plane.onList = func() {
		if f.coord.s != nil {
			f.coord.run()
		}
	}
	f.coord.p = f.plane
	return MainWith(f.host())
}

// bundle returns the attempt's evidence directory.
func (f *fakeWorld) bundle() string {
	if st, err := os.Stat(f.evidence); err != nil || !st.IsDir() {
		f.t.Fatalf("no evidence bundle at %s: %v", f.evidence, err)
	}
	// Canonical, as the supervisor reports it (TMPDIR or macOS /var may be a link).
	b, err := filepath.EvalSymlinks(f.evidence)
	if err != nil {
		f.t.Fatalf("resolve evidence bundle %s: %v", f.evidence, err)
	}
	return b
}

// report reads the attempt's report.json.
func (f *fakeWorld) report() Report {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.bundle(), "report.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// failedCodes returns every failing criterion code of r.
func failedCodes(r Report) []string {
	var out []string
	for _, c := range r.Criteria {
		out = append(out, c.Codes...)
	}
	return out
}

// eventTypesOf reads the bundle's event types.
func eventTypesOf(t *testing.T, bundle string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(bundle, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e Event
		json.Unmarshal([]byte(l), &e)
		out = append(out, e.Type+codeSuffix(e.Code))
	}
	return out
}

func codeSuffix(c string) string {
	if c == "" {
		return ""
	}
	return ":" + c
}
