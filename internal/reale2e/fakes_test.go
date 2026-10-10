package reale2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
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

// Offline doubles of the supervisor's world (design 12a-real-e2e, FP-9):
// an auto-advancing clock, scripted processes, an in-memory plane with a
// go-git hub and a scripted coordinator. No vendor, network or real child
// is ever used by them.

// autoClock fires every timer at once, advancing its time by the timer's
// duration: a single-goroutine supervisor run is then deterministic and
// instantaneous in real time.
type autoClock struct {
	mu  sync.Mutex
	now time.Time
	// skip is the number of next timers that fire without advancing the
	// time (a test holds the time for one tick; it never moves back).
	skip int
}

// holdNext makes the next timer fire without advancing the time.
func (c *autoClock) holdNext() {
	c.mu.Lock()
	c.skip = 1
	c.mu.Unlock()
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
	// lingering leaves a descendant in the short command's group after
	// the leader exits.
	lingering bool
}

// fakeLauncher starts scripted children.
type fakeLauncher struct {
	mu      sync.Mutex
	pid     int
	script  func(ProcSpec) behavior
	started []ProcSpec
	procs   []*fakeProc
}

func (l *fakeLauncher) Start(s ProcSpec) (Proc, error) {
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
	t       *testing.T
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
	addRoleErrs   []error
	notReadyCalls int
	dispatchErr   func(req contract.DispatchRequest) error
	dispatchLost  bool
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
	// onList runs on every ListTasks call (the scripted coordinator).
	onList func()
}

func newFakePlane(t *testing.T, clock Clock) *fakePlane {
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
		b := &contract.WorkspaceBinding{Name: *req.Workspace, Instance: p.ws.Instance, BaseSelector: contract.SelectorString(sel)}
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

func (p *fakePlane) ShowTask(ctx context.Context, id string, _ int) (contract.TaskView, error) {
	if _, ok := ctx.Deadline(); !ok {
		p.mu.Lock()
		p.unbounded++
		p.mu.Unlock()
	}
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
	return *v, nil
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
		out = append(out, contract.TaskSummary{TaskID: id, Role: v.Role, State: v.State, CreatedAt: v.CreatedAt, RequestedBy: v.Request.RequestedBy})
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

func (p *fakePlane) TaskLogs(_ context.Context, id string) (contract.TaskLogsResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.logs[id]
	if !ok {
		return contract.TaskLogsResponse{}, contract.New(contract.CodeNotFound, "no logs")
	}
	return contract.TaskLogsResponse{TaskID: id, Data: d, SourceBytes: len(d), RetainedBytes: len(d)}, nil
}

func (p *fakePlane) CreateWorkspace(_ context.Context, name string) (contract.WorkspaceView, error) {
	if p.createWSErr != nil {
		return contract.WorkspaceView{}, p.createWSErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ws = &contract.WorkspaceView{Name: name, Instance: hexID("", 0xabc)}
	return *p.ws, nil
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
func writeCommit(t *testing.T, st *memory.Storage, files map[string][]byte, parents ...plumbing.Hash) (plumbing.Hash, plumbing.Hash) {
	t.Helper()
	return writeCommitMode(t, st, files, nil, parents...)
}

// writeCommitMode is writeCommit with per-file modes (default regular).
func writeCommitMode(t *testing.T, st *memory.Storage, files map[string][]byte, modes map[string]filemode.FileMode, parents ...plumbing.Hash) (plumbing.Hash, plumbing.Hash) {
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

// Hop contents of the scripted happy path.
var (
	designDoc     = []byte("# Design\n\nAdd --name with flag parsing in run(args, stdout, stderr).\n\nSTATUS: DESIGN_DRAFT\n")
	designReview  = []byte("# Review\n\nThe design covers every case.\n\nSTATUS: DESIGN_REVIEW_APPROVED\n")
	codedMain     = []byte("package main\n\n// run is the CLI.\nfunc run() int { return 0 }\n\nfunc greeting() string { return \"Hello, world!\\n\" }\n\nfunc main() {}\n")
	codedMainTest = []byte("package main\n\nimport \"testing\"\n\nfunc TestRun(t *testing.T) { _ = run() }\n")
)

// coordScript is the scripted coordinator: each step runs on one
// ListTasks call of the feature loop, on the supervisor's goroutine.
type coordScript struct {
	t      *testing.T
	s      *supervisor
	p      *fakePlane
	step   int
	codes  []string
	tasks  [4]string
	files  [5]map[string][]byte
	commit [5]plumbing.Hash
	// Variations.
	finals             [4]string
	hopFiles           [4]map[string][]byte
	parentOf           func(i int) plumbing.Hash
	skipOwner          bool
	ownerLine          string
	dispatchEarlyCoder bool
	stopAfter          int
	noOwnerFiles       bool
	mutateOwner        func(dir string)
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
	ownerInput     func(c *coordScript)
}

func newCoordScript(t *testing.T) *coordScript {
	c := &coordScript{t: t, failHop: -1, extraAt: -1, stopAfter: -1, waitExit: "0", overrideHop: -1, nilFinal: -1, unpublished: -1}
	c.finals = [4]string{"Design written.\nSTATUS: DESIGN_DRAFT\n", "Approved.\nSTATUS: DESIGN_REVIEW_APPROVED\n",
		"Implemented.\nSTATUS: IMPL_COMPLETE\n", "Looks right.\nSTATUS: REVIEW_APPROVED\n"}
	c.hopFiles = [4]map[string][]byte{{"design.md": designDoc}, {"design-review.md": designReview},
		{"main.go": codedMain, "main_test.go": codedMainTest}, {}}
	return c
}

// goal is hop i's goal text.
func (c *coordScript) goal(i int) string {
	g := "Do hop " + Hops[i] + "."
	if i == 3 {
		var b strings.Builder
		b.WriteString("Review the implementation.\n")
		for _, p := range inlinePaths {
			b.WriteString(fileBlockBegin(p, sha256Hex(c.files[3][p])))
			b.Write(c.files[3][p])
			b.WriteString(fileBlockEnd(p))
		}
		g = b.String()
	}
	if c.goalOf != nil {
		g = c.goalOf(i, g)
	}
	return g
}

// dispatch dispatches hop i as the coordinator would and observes it.
func (c *coordScript) dispatch(i int) {
	c.dispatchOnly(i)
	c.observe(i)
}

// observe reports hop i's task as the coordinator's observe helper would.
func (c *coordScript) observe(i int) {
	c.codes = append(c.codes, c.s.handleRequest(ControlRequest{Run: c.s.runID, Op: opObserve, Task: c.tasks[i], Hop: Hops[i]}))
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
}

// complete finishes hop i's task with its commit and wait record.
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
	v, _ := c.p.ShowTask(context.Background(), c.tasks[i], 0)
	wr := contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitTerminal, EffectiveWaitMS: 30000, Winner: c.tasks[i], Task: &v}
	b, err := json.Marshal(wr)
	if err != nil {
		c.t.Fatal(err)
	}
	dir := filepath.Join(c.s.runtime, "waits", hopDir(i))
	os.WriteFile(filepath.Join(dir, "result.json"), append(b, '\n'), 0o600)
	os.WriteFile(filepath.Join(dir, "error.txt"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "exit-code"), []byte(c.waitExit+"\n"), 0o600)
}

// run is the ListTasks hook.
func (c *coordScript) run() {
	s := c.s
	if s.ctl == nil || s.failed || s.finish || c.step == c.stopAfter {
		return
	}
	if c.before != nil && (c.before(c) || s.failed) {
		return
	}
	switch c.step {
	case 0:
		seed, err := git.PlainOpen(s.seed.Path)
		if err != nil {
			c.t.Fatal(err)
		}
		if err := copyObjects(seed.Storer, c.p.hub, []plumbing.Hash{s.seed.Commit}); err != nil {
			c.t.Fatal(err)
		}
		c.files[0], c.commit[0] = s.seed.Files, s.seed.Commit
		if c.extraAt == 0 {
			c.extra(0)
		}
		c.dispatch(0)
	case 1:
		c.complete(0)
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
			return
		}
		c.complete(1)
	case 4:
		if s.owner != ownerAwaiting {
			return
		}
		if c.dispatchEarlyCoder {
			c.dispatch(2)
			return
		}
		if c.ownerInput != nil {
			c.ownerInput(c)
			break
		}
		if c.skipOwner {
			return
		}
		line := "yes " + s.runID
		if c.ownerLine != "" {
			line = c.ownerLine
		}
		s.handleLine(line, true)
	case 5:
		if s.decision == nil {
			return
		}
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
		if !c.noOwnerFiles {
			writeOwnerFiles(c.t, s, c.tasks)
		}
		if c.mutateOwner != nil {
			c.mutateOwner(filepath.Join(s.runtime, "owner"))
		}
		c.codes = append(c.codes, s.handleRequest(ControlRequest{Run: s.runID, Op: opFinish}))
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

// transcriptAndMap returns a synthetic transcript and its owner-attested
// event map for the four tasks.
func transcriptAndMap(runID, sessionID string, tasks [4]string) ([]byte, EventMap) {
	var lines []string
	add := func(s string) int {
		lines = append(lines, fmt.Sprintf(`{"type":"assistant","n":%d,"text":%q}`, len(lines)+1, s))
		return len(lines)
	}
	em := EventMap{Schema: EventMapSchema, RunID: runID, SessionID: sessionID, AttestedByOwner: true}
	span := func(k, hop, task, handle string) MapEvent {
		n := add(k + " " + hop)
		return MapEvent{Kind: k, Hop: hop, Task: task, Handle: handle, Lines: []int{n, n}}
	}
	em.Events = append(em.Events, span(MapSessionLaunch, "", "", ""))
	for i, h := range Hops {
		if i == 2 {
			em.Events = append(em.Events, span(MapOwnerPause, "", "", ""), span(MapOwnerResume, "", "", ""))
		}
		em.Events = append(em.Events, span(MapDispatch, h, tasks[i], ""))
		ws := span(MapWaitStart, h, tasks[i], waitHandle(i))
		ws.Background = true
		wc := span(MapWaitConsumed, h, tasks[i], waitHandle(i))
		wc.AutomaticWake = true
		em.Events = append(em.Events, ws, wc)
	}
	em.Events = append(em.Events, span(MapFinalResult, "", "", ""))
	data := []byte(strings.Join(lines, "\n") + "\n")
	em.TranscriptSHA256 = sha256Hex(data)
	return data, em
}

// writeOwnerFiles writes the owner's session files into the runtime.
func writeOwnerFiles(t *testing.T, s *supervisor, tasks [4]string) {
	t.Helper()
	dir := filepath.Join(s.runtime, "owner")
	tr, em := transcriptAndMap(s.runID, s.sessionID, tasks)
	att := SetupAttestation{Schema: AttestationSchema, RunID: s.runID, SessionID: s.sessionID, ClaudeVersion: "2.1.292 (Claude Code)", FreshSession: true,
		EmptyCoordinatorDir: true, NoPriorConversation: true, NoExtraPrompt: true, NoUnrelatedInstructions: true, InstructionSourcesChecked: true,
		SetupInputs: slices.Clone(allowedSetupInputs), TranscriptExport: "session JSONL file"}
	ce := CoordinatorExit{Schema: CoordExitSchema, SessionClosed: true, MCPChildExited: true}
	for i := range Hops {
		ce.WaitHandles = append(ce.WaitHandles, WaitHandleExit{Handle: waitHandle(i)})
	}
	for name, v := range map[string]any{"event-map.json": em, "setup-attestation.json": att, "coordinator-exit.json": ce} {
		b, _ := encodeJSON(v)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), tr, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeWorld is one offline attempt's world.
type fakeWorld struct {
	t          *testing.T
	clock      *autoClock
	launcher   *fakeLauncher
	plane      *fakePlane
	puller     *fakePuller
	coord      *coordScript
	checkout   string
	evidence   string
	tempRoot   string
	stdout     *syncBuffer
	stderr     *syncBuffer
	stdin      io.Reader
	probeErr   map[string]error
	versions   map[string]string
	connectErr error
	ctx        context.Context
	lookErr    map[string]error
	randSeed   byte
	stdinW     *io.PipeWriter
	// script overrides individual commands (nil: default).
	override func(ProcSpec) (behavior, bool)
	flow     string
	sup      *supervisor
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

// newCheckout creates a minimal checkout: the Callsheet go.mod, the
// shipped templates (copied from this repository's docs) and design/.
func newCheckout(t *testing.T) string {
	t.Helper()
	repoRootOnce.Do(func() { repoRootOnce.root = testkit.MustRepoRoot(t) })
	co := t.TempDir()
	if err := os.WriteFile(filepath.Join(co, "go.mod"), []byte("module "+modulePath+"\n\ngo 1.26.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(co, filepath.FromSlash(ExamplesDir))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range TemplateNames() {
		b, err := os.ReadFile(filepath.Join(repoRootOnce.root, filepath.FromSlash(ExamplesDir), n))
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
	sig := object.Signature{Name: "t", Email: "t@invalid", When: time.Unix(0, 0)}
	if _, err := wt.Commit("checkout\n", &git.CommitOptions{Author: &sig, Committer: &sig}); err != nil {
		t.Fatal(err)
	}
	return co
}

// shortRoot is the short parent of every socket-bearing fixture.
const shortRoot = "/tmp"

// shortTemp returns a short private temporary root (socket paths stay
// within their bound whatever TMPDIR is).
func shortTemp(t *testing.T) string {
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

func newFakeWorld(t *testing.T) *fakeWorld {
	f := &fakeWorld{t: t, clock: newAutoClock(), stdout: &syncBuffer{}, stderr: &syncBuffer{}, probeErr: map[string]error{}, lookErr: map[string]error{},
		ctx: context.Background(), versions: map[string]string{fakeClaude: "2.1.290 (Claude Code)", fakeCodex: "codex-cli 0.160.0", fakeGrok: "grok 1.0.47 (abcdef0) [stable]"}}
	f.checkout = newCheckout(t)
	// The bundle directory itself: new, under the checkout's design/ tree.
	f.evidence = filepath.Join(f.checkout, "design", "real-e2e-runs", "attempt")
	f.tempRoot = shortTemp(t)
	f.plane = newFakePlane(t, f.clock)
	f.puller = &fakePuller{p: f.plane}
	f.launcher = &fakeLauncher{script: f.script}
	f.coord = newCoordScript(t)
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	f.stdin, f.stdinW = pr, pw
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

// base is a child's default behavior.
func (f *fakeWorld) base(s ProcSpec) behavior {
	a := s.Args
	switch {
	case s.Path == fakeCallsheet && len(a) == 1 && a[0] == "version":
		return behavior{stdout: "callsheet dev protocol=6\n"}
	case len(a) == 1 && a[0] == "--version" && f.versions[s.Path] != "":
		return behavior{stdout: f.versions[s.Path] + "\n"}
	case s.Path == fakeGo && len(a) == 1 && a[0] == "version":
		return behavior{stdout: "go version go1.26.4 linux/amd64\n"}
	case s.Path == fakeGo && strings.Join(a, " ") == "test -count=1 ./...":
		return behavior{stdout: "ok  \texample.com/greeting\t0.01s\n"}
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
		n := map[string]int{"n-codex": 1, "n-claude": 2, "n-grok": 3}[filepath.Base(a[len(a)-1])]
		return behavior{stdout: "node_id: " + hexID("n_", n) + "\n"}
	case s.Path == fakeCallsheet && len(a) > 1 && a[0] == "sidecar" && a[1] == "run":
		return behavior{long: true, stderr: `{"msg":"heartbeat acknowledged","session":1}` + "\n"}
	}
	f.t.Errorf("unexpected command %s %v", s.Path, a)
	return behavior{exit: 127}
}

// world returns the supervisor's world.
func (f *fakeWorld) world() *World {
	return &World{Context: f.ctx, Launcher: f.launcher, Clock: f.clock, Environ: []string{"PATH=/fake/bin", "HOME=/home/owner"}, Cwd: f.checkout,
		Executable: fakeReale2e, Home: "/home/owner", TempRoot: f.tempRoot, Rand: &countingRand{n: f.randSeed},
		LookPath: func(n string) (string, error) {
			if err := f.lookErr[n]; err != nil {
				return "", err
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
		Poll: 2 * time.Second}
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

// host returns run's host inputs with the opt-in set.
func (f *fakeWorld) host() Host {
	args := []string{"run", "--callsheet", fakeCallsheet, "--claude", fakeClaude, "--codex", fakeCodex, "--grok", fakeGrok, "--evidence", f.evidence}
	if f.flow != "" {
		args = append(args, "--flow", f.flow)
	}
	return Host{GOOS: "linux", GOARCH: "amd64", Args: args,
		Getenv:    func(k string) string { return map[string]string{EnvOptIn: "1"}[k] },
		LookupEnv: func(string) (string, bool) { return "", false },
		Stdin:     f.stdin, Stdout: f.stdout, Stderr: f.stderr,
		NewWorld: func(Host) (*World, func(), error) {
			w := f.world()
			return w, func() {}, nil
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
	a := runArgs{callsheet: fakeCallsheet, claude: fakeClaude, codex: fakeCodex, grok: fakeGrok, evidence: f.evidence, flow: f.flow}
	return runWith(f.host(), a, func(s *supervisor) {
		f.sup = s
		f.coord.s = s
	})
}

// bundle returns the attempt's evidence directory.
func (f *fakeWorld) bundle() string {
	if st, err := os.Stat(f.evidence); err != nil || !st.IsDir() {
		f.t.Fatalf("no evidence bundle at %s: %v", f.evidence, err)
	}
	return f.evidence
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
