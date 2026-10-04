// Package taskhub is a test fixture (iteration 10b): an in-process
// plane-side workspace stack for the taskworkspace and taskpublication
// tests and benchmarks. It runs the real workspace hub (both git route
// families over verified TLS) and the real publication service on a
// durable file-backed task authority (Store) that applies the plane's
// arbitration essentials. Restart reopens every store from disk and runs
// the service's startup replay, so crash boundaries between the task and
// workspace stores are observed with real persistence. It is not a plane:
// no streams, roles or task lifecycle beyond publication.
package taskhub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/taskpublication"
	"github.com/wedevwork/callsheet/internal/taskworkspace"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/workspace"
)

// Clock is the service clock (a testkit.FakeClock or the real one).
type Clock = taskpublication.Clock

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) NewTimerAt(at time.Time) (<-chan time.Time, func() bool) {
	t := time.NewTimer(time.Until(at))
	return t.C, t.Stop
}

// Options configures Start.
type Options struct {
	// Dir holds the plane state root and the task store (a restart
	// reuses it); default a fresh temporary directory.
	Dir   string
	Clock Clock
	// Hook observes the service's named stages.
	Hook func(stage, taskID string)
	// DirSyncFault, when non-nil, can fail the hub's directory sync of a
	// named directory (workspace.OpenOptions).
	DirSyncFault func(dir string) error
}

// Hub is the running stack.
type Hub struct {
	Dir    string
	URL    string
	CAPEM  []byte
	M      *workspace.Manager
	Svc    *taskpublication.Service
	Tasks  *Store
	Client *client.Client
	Git    testkit.GitRemote
	Clock  Clock

	ca     *testkit.FixtureCA
	opts   Options
	srv    *http.Server
	served chan struct{}
	ln     net.Listener
}

// Start runs the stack and its startup replay (a replay error fails t).
func Start(t testing.TB, o Options) *Hub {
	t.Helper()
	h, err := start(t, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func start(t testing.TB, o Options, ca *testkit.FixtureCA) (*Hub, error) {
	if o.Dir == "" {
		o.Dir = t.TempDir()
	}
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	var err error
	if ca == nil {
		if ca, err = testkit.NewFixtureCA(); err != nil {
			return nil, err
		}
	}
	h := &Hub{Dir: o.Dir, CAPEM: ca.CertPEM, Clock: o.Clock, ca: ca, opts: o}
	root := filepath.Join(o.Dir, "plane")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if h.M, err = workspace.OpenWith(context.Background(), root, workspace.OpenOptions{DirSyncFault: o.DirSyncFault}); err != nil {
		return nil, err
	}
	if h.Tasks, err = OpenStore(filepath.Join(o.Dir, "tasks")); err != nil {
		h.M.Close()
		return nil, err
	}
	h.Svc = taskpublication.New(taskpublication.Options{Tasks: h.Tasks, Workspaces: h.M, Clock: o.Clock, Hook: o.Hook})
	replayErr := h.Svc.Replay(context.Background())
	cert, err := ca.ServerCertificate()
	if err != nil {
		return nil, err
	}
	addr := "127.0.0.1:0"
	if h.URL != "" {
		addr = strings.TrimPrefix(h.URL, "https://")
	}
	if h.ln, err = net.Listen("tcp", addr); err != nil {
		return nil, err
	}
	h.URL = "https://" + h.ln.Addr().String()
	h.srv = &http.Server{ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case strings.HasPrefix(p, contract.WorkspaceGitPrefix):
			h.M.ServeGit(w, r)
		case strings.HasPrefix(p, contract.PathNodeWorkspaces):
			h.M.ServeTaskGit(w, r, h.Svc)
		default:
			http.NotFound(w, r)
		}
	})}
	h.served = make(chan struct{})
	go func() {
		h.srv.Serve(tls.NewListener(h.ln, &tls.Config{Certificates: []tls.Certificate{cert}}))
		close(h.served)
	}()
	if h.Client, err = client.New(h.URL, client.Trust{CAPEM: h.CAPEM}); err != nil {
		h.Close()
		return nil, err
	}
	hc, err := testkit.GitHTTPClient(h.CAPEM)
	if err != nil {
		h.Close()
		return nil, err
	}
	h.Git = testkit.GitRemote{HTTP: hc}
	t.Cleanup(h.Close)
	return h, replayErr
}

// Close stops the server, the service and the hub (idempotent).
func (h *Hub) Close() {
	if h.srv != nil {
		h.srv.Close()
		<-h.served
		h.srv = nil
	}
	if h.Client != nil {
		h.Client.Close()
	}
	if h.Svc != nil {
		h.Svc.Close()
	}
	if h.M != nil {
		h.M.Close()
	}
}

// Restart closes the stack and starts it again on the same directory and
// trust: the startup replay's result is returned (the stack runs either
// way).
func (h *Hub) Restart(t testing.TB) (*Hub, error) {
	t.Helper()
	h.Close()
	return start(t, h.opts, h.ca)
}

// Workspace creates workspace name and returns its instance.
func (h *Hub) Workspace(t testing.TB, name string) string {
	t.Helper()
	v, err := h.M.Create(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return v.Instance
}

// Remote is the coordinator git endpoint of a workspace.
func (h *Hub) Remote(name, instance string) testkit.GitRemote {
	g := h.Git
	g.URL = h.URL + contract.WorkspaceGitPrefix + name + ".git"
	g.Instance = instance
	return g
}

// Seed commits files (parents as given) into a fresh store and pushes it
// as branch; it returns the commit.
func (h *Hub) Seed(t testing.TB, name, instance, branch string, old plumbing.Hash, files map[string]testkit.FileSpec, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	s := testkit.NewMemoryStore()
	for _, p := range parents {
		h.copyHistory(t, name, instance, s, p)
	}
	c, err := testkit.CommitFiles(s, files, parents, "seed")
	if err != nil {
		t.Fatal(err)
	}
	h.Push(t, name, instance, s, branch, old, c)
	return c
}

func (h *Hub) copyHistory(t testing.TB, name, instance string, s *memory.Storage, c plumbing.Hash) {
	t.Helper()
	if err := h.Remote(name, instance).Fetch(context.Background(), s, []plumbing.Hash{c}, nil); err != nil {
		t.Fatal(err)
	}
}

// Push pushes branch old -> new from s through the coordinator route.
func (h *Hub) Push(t testing.TB, name, instance string, s *memory.Storage, branch string, old, new plumbing.Hash) {
	t.Helper()
	// Window 0: fixture blobs are incompressible, a delta search only
	// costs time.
	res := h.Remote(name, instance).Push(context.Background(), s, "refs/heads/"+branch, old, new, testkit.PackWindow(0))
	if !res.OK() {
		t.Fatalf("push %s: %v %+v", branch, res.Err, res.Report)
	}
}

// Binding resolves a selector (nil: main) into the task binding.
func (h *Hub) Binding(t testing.TB, name, instance string, base *string) contract.WorkspaceBinding {
	t.Helper()
	sel, err := contract.ParseTaskBase(base)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.M.ResolveTaskBase(context.Background(), name, instance, sel)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// NewTask registers a live workspace task of binding b and returns it with
// its assignment.
func (h *Hub) NewTask(t testing.TB, n int, b contract.WorkspaceBinding) (taskpublication.Task, contract.NodeAssignment) {
	t.Helper()
	tk := taskpublication.Task{TaskID: TaskID(n), Node: "n_" + strings.Repeat("7", 32), Execution: contract.ExecutionToken{Epoch: strings.Repeat("e", 32), Attachment: 1},
		StartDigest: strings.Repeat("d", 64), RoleID: "worker-a", Model: "example-model", Binding: b, State: contract.TaskRunning, Live: true}
	if err := h.Tasks.Add(tk); err != nil {
		t.Fatal(err)
	}
	return tk, Assignment(tk)
}

// TaskID is the n-th fixture task ID.
func TaskID(n int) string { return fmt.Sprintf("t_%032d", n) }

// Assignment is a task's own assignment.
func Assignment(tk taskpublication.Task) contract.NodeAssignment {
	return contract.NodeAssignment{NodeID: tk.Node, Execution: tk.Execution, StartDigest: tk.StartDigest, Instance: tk.Binding.Instance}
}

// Remote is a task's node transfer session (Close it).
func (h *Hub) NodeRemote(t testing.TB, taskID string, a contract.NodeAssignment) *client.WorkspaceGit {
	t.Helper()
	g, err := h.Client.NodeTaskGit(taskID, a, "")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// API is a task's publication endpoints, served by the service directly
// (the plane's HTTP mapping is the plane's), with the real node receive
// session.
func (h *Hub) API(taskID string, a contract.NodeAssignment) *API {
	return &API{h: h, id: taskID, a: a}
}

// API implements taskworkspace.PlaneAPI. Fail, when set, is consulted
// before each call (op "begin", "finish", "observe", "pusher"); a
// non-nil error is returned instead of calling the service, and Lose makes
// a call run but its answer be lost (an unavailable error).
type API struct {
	h    *Hub
	id   string
	a    contract.NodeAssignment
	mu   sync.Mutex
	Fail func(op string) error
	Lose func(op string) bool
}

func (p *API) gate(op string) (error, bool) {
	p.mu.Lock()
	f, l := p.Fail, p.Lose
	p.mu.Unlock()
	if f != nil {
		if err := f(op); err != nil {
			return err, false
		}
	}
	return nil, l != nil && l(op)
}

var errLost = contract.New(contract.CodeUnavailable, "the answer was lost (fixture)")

func (p *API) Begin(ctx context.Context, req contract.PublicationBeginRequest) (contract.PublicationBeginResponse, error) {
	err, lose := p.gate("begin")
	if err != nil {
		return contract.PublicationBeginResponse{}, err
	}
	r, err := p.h.Svc.Begin(ctx, p.id, p.a, req)
	if lose {
		return contract.PublicationBeginResponse{}, errLost
	}
	return r, err
}

func (p *API) Finish(ctx context.Context, pubID, code string) (contract.PublicationStatus, error) {
	err, lose := p.gate("finish")
	if err != nil {
		return contract.PublicationStatus{}, err
	}
	r, err := p.h.Svc.Finish(ctx, p.id, p.a, pubID, code)
	if lose {
		return contract.PublicationStatus{}, errLost
	}
	return r, err
}

func (p *API) Observe(ctx context.Context, pubID string) (contract.PublicationStatus, error) {
	err, lose := p.gate("observe")
	if err != nil {
		return contract.PublicationStatus{}, err
	}
	r, err := p.h.Svc.Observe(p.id, p.a, pubID)
	if lose {
		return contract.PublicationStatus{}, errLost
	}
	return r, err
}

func (p *API) Pusher(pubID string) (taskworkspace.Pusher, error) {
	if err, _ := p.gate("pusher"); err != nil {
		return nil, err
	}
	g, err := p.h.Client.NodeTaskPusher(p.id, p.a, pubID)
	if err != nil {
		return nil, err
	}
	return g, nil
}

// ---- The node side ----

// RealClock is the wall clock for the node side's publication retries.
type RealClock struct{}

// NewTimer is time.NewTimer.
func (RealClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// Worker is a node-side fixture: a state root with the shared cache.
type Worker struct {
	Root  string
	Cache *taskworkspace.Manager
	GOOS  string
}

// NewWorker opens a worker's cache for hub h (target 0: the default) on
// the explicit platform goos (the checkout seam; tests pass runtime.GOOS).
func NewWorker(t testing.TB, h *Hub, target int64, goos string) *Worker {
	t.Helper()
	root := t.TempDir()
	c, err := taskworkspace.OpenCache(context.Background(), taskworkspace.CacheOptions{Root: root, Origin: h.URL, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	return &Worker{Root: root, Cache: c, GOOS: goos}
}

// TaskDir is task id's private directory (created).
func (w *Worker) TaskDir(t testing.TB, id string) string {
	t.Helper()
	d := filepath.Join(w.Root, "tasks", id)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// Prepare prepares tk's private checkout through the node route.
func (w *Worker) Prepare(ctx context.Context, t testing.TB, h *Hub, tk taskpublication.Task) (string, *taskworkspace.Prepared, error) {
	t.Helper()
	dir := w.TaskDir(t, tk.TaskID)
	if err := os.Mkdir(filepath.Join(dir, taskworkspace.WorkName), 0o700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	g := h.NodeRemote(t, tk.TaskID, Assignment(tk))
	defer g.Close()
	p, err := taskworkspace.Prepare(ctx, taskworkspace.PrepareInput{GOOS: w.GOOS, TaskDir: dir, Binding: tk.Binding, Remote: g, Cache: w.Cache})
	return dir, p, err
}

// Candidate is tk's natural sealed candidate with exit code exit.
func Candidate(tk taskpublication.Task, exit int) contract.TaskResultBody {
	return contract.TaskResultBody{TaskID: tk.TaskID, Execution: tk.Execution, Outcome: contract.OutcomeNatural, ExitCode: &exit}.Sealed()
}

// Seal snapshots the work directory and returns the sealed checkpoint.
func (w *Worker) Seal(ctx context.Context, t testing.TB, dir string, p *taskworkspace.Prepared, cand contract.TaskResultBody) contract.PublicationCheckpoint {
	t.Helper()
	tree, _, err := taskworkspace.Snapshot(ctx, taskworkspace.SnapshotInput{GOOS: w.GOOS, TaskDir: dir, Map: p.Map, RuntimeDir: p.RuntimeDir})
	if err != nil {
		t.Fatal(err)
	}
	return contract.PublicationCheckpoint{TaskID: cand.TaskID, Execution: cand.Execution, Phase: contract.CheckpointSealed, Candidate: cand, Tree: tree.String()}
}

// Checkpoints records every saved checkpoint (the node's journal).
type Checkpoints struct {
	mu   sync.Mutex
	List []contract.PublicationCheckpoint
	// Fail, when set, fails a save of the given phase.
	Fail func(c contract.PublicationCheckpoint) error
}

// Save implements taskworkspace.Saver.
func (c *Checkpoints) Save(cp contract.PublicationCheckpoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Fail != nil {
		if err := c.Fail(cp); err != nil {
			return err
		}
	}
	if _, err := contract.EncodePublicationCheckpoint(cp); err != nil {
		return err
	}
	c.List = append(c.List, cp)
	return nil
}

// Last is the latest saved checkpoint.
func (c *Checkpoints) Last() contract.PublicationCheckpoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.List[len(c.List)-1]
}

// Input is tk's publication input from cp (API and checkpoint journal as
// given; the wall clock and 10 ms retries).
func Input(tk taskpublication.Task, dir string, cp contract.PublicationCheckpoint, api taskworkspace.PlaneAPI, j *Checkpoints) taskworkspace.PublishInput {
	return taskworkspace.PublishInput{TaskID: tk.TaskID, RoleID: tk.RoleID, Model: tk.Model, TaskDir: dir, Binding: tk.Binding, Checkpoint: cp,
		API: api, Save: j.Save, Clock: RealClock{}, Retry: 10 * time.Millisecond}
}

// ---- The durable task authority ----

// Store is a file-backed taskpublication.Tasks: one JSON document per task
// written with temporary/sync/rename/directory-sync. It applies the
// plane's arbitration essentials: a durable stop request makes a natural
// candidate cancelled, a lost or rejected task refuses, a duplicate Begin
// returns the original intent. FailWrite, when set, can fail a durable
// write (op "authorize" or "settle") before it happens.
type Store struct {
	dir       string
	mu        sync.Mutex
	tasks     map[string]*entry
	changed   chan struct{}
	FailWrite func(op, taskID string) error
}

type entry struct {
	T        taskpublication.Task
	Deadline time.Time
	Stop     bool
}

// docWire is a task document (the intent's deadline kept apart: the
// contract's intent encoding is not a json.Unmarshaler).
type docWire struct {
	Task     taskpublication.Task `json:"task"`
	Deadline time.Time            `json:"deadline"`
	Stop     bool                 `json:"stop"`
}

// OpenStore loads every task document in dir (creating it).
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, tasks: map[string]*entry{}, changed: make(chan struct{})}
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var d docWire
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("task document %s: %w", id, err)
		}
		if d.Task.Publication != nil {
			d.Task.Publication.Deadline = d.Deadline
		}
		// A restarted plane has no attachment: nothing is live.
		d.Task.Live = false
		s.tasks[id] = &entry{T: d.Task, Deadline: d.Deadline, Stop: d.Stop}
	}
	return s, nil
}

func (s *Store) writeLocked(e *entry) error {
	if e.T.Publication != nil {
		e.Deadline = e.T.Publication.Deadline
	}
	b, err := json.Marshal(docWire{Task: e.T, Deadline: e.Deadline, Stop: e.Stop})
	if err != nil {
		return err
	}
	p := filepath.Join(s.dir, e.T.TaskID+".json")
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err == nil {
		if d, derr := os.Open(s.dir); derr == nil {
			d.Sync()
			d.Close()
		}
	}
	return err
}

func (s *Store) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Add stores a new task.
func (s *Store) Add(t taskpublication.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &entry{T: t}
	s.tasks[t.TaskID] = e
	return s.writeLocked(e)
}

// Update changes a task durably (stop requests, decided outcomes, liveness).
func (s *Store) Update(id string, f func(t *taskpublication.Task, stop *bool)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.tasks[id]
	if e == nil {
		return errors.New("no such task")
	}
	f(&e.T, &e.Stop)
	defer s.notifyLocked()
	return s.writeLocked(e)
}

func (s *Store) get(id string) (*entry, error) {
	e := s.tasks[id]
	if e == nil {
		return nil, contract.TaskError(contract.CodeNotFound, "", "", "no such workspace task")
	}
	return e, nil
}

func view(e *entry) taskpublication.Task {
	t := e.T
	if t.Publication != nil {
		p := *t.Publication
		t.Publication = &p
	}
	return t
}

// Check implements taskpublication.Tasks.
func (s *Store) Check(id string, a contract.NodeAssignment) (taskpublication.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.get(id)
	if err != nil {
		return taskpublication.Task{}, err
	}
	switch {
	case a.NodeID != e.T.Node || a.Execution != e.T.Execution || a.StartDigest != e.T.StartDigest:
		return taskpublication.Task{}, contract.TaskError(contract.CodeConflict, "", contract.ReasonTaskAssignmentMismatch, "assignment mismatch")
	case a.Instance != e.T.Binding.Instance:
		return taskpublication.Task{}, contract.TaskError(contract.CodeConflict, "", contract.ReasonWorkspaceInstanceMismatch, "instance mismatch")
	}
	return view(e), nil
}

// AwaitStart implements taskpublication.Tasks: the store's tasks carry
// their liveness directly (no start reply is ever in flight).
func (s *Store) AwaitStart(context.Context, string, contract.NodeAssignment) error { return nil }

// Get implements taskpublication.Tasks.
func (s *Store) Get(id string) (taskpublication.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.get(id)
	if err != nil {
		return taskpublication.Task{}, err
	}
	return view(e), nil
}

func closed(msg string) error {
	return contract.TaskError(contract.CodeConflict, "", contract.ReasonTaskPublicationClosed, "%s", msg)
}

// outcomeState is the plane's canonical terminal state of a candidate.
func outcomeState(c contract.TaskResultBody, stop bool) string {
	switch {
	case c.Outcome == contract.OutcomeTimedOut:
		return contract.TaskTimedOut
	case c.Outcome == contract.OutcomeCancelled || stop:
		return contract.TaskCancelled
	case c.ExitCode != nil && *c.ExitCode == 0:
		return contract.TaskSucceeded
	}
	return contract.TaskFailed
}

// Authorize implements taskpublication.Tasks.
func (s *Store) Authorize(ctx context.Context, id string, a contract.NodeAssignment, cand contract.TaskResultBody, tree string,
	build func(t taskpublication.Task, state string) (contract.TaskPublication, error)) (contract.TaskPublication, error) {
	if _, err := s.Check(id, a); err != nil {
		return contract.TaskPublication{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.tasks[id]
	if p := e.T.Publication; p != nil {
		if p.Tree != tree || !taskpublication.SameCandidate(p.Candidate, cand) {
			return contract.TaskPublication{}, closed("the task has another publication intent")
		}
		return *p, nil
	}
	switch {
	case contract.TaskTerminal(e.T.State):
		return contract.TaskPublication{}, closed("the task is already decided")
	case !e.T.Live:
		return contract.TaskPublication{}, contract.TaskError(contract.CodeConflict, "", contract.ReasonTaskAssignmentMismatch, "stale attachment")
	case cand.Outcome == contract.OutcomeLost:
		return contract.TaskPublication{}, closed("a lost outcome publishes nothing")
	}
	in, err := build(view(e), outcomeState(cand, e.Stop))
	if err != nil {
		return contract.TaskPublication{}, err
	}
	if s.FailWrite != nil {
		if err := s.FailWrite("authorize", id); err != nil {
			return contract.TaskPublication{}, err
		}
	}
	e.T.Publication = &in
	if err := s.writeLocked(e); err != nil {
		e.T.Publication = nil
		return contract.TaskPublication{}, err
	}
	s.notifyLocked()
	return in, nil
}

// Settle implements taskpublication.Tasks.
func (s *Store) Settle(ctx context.Context, id string, st taskpublication.Settlement) (contract.TaskResultBody, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.get(id)
	if err != nil {
		return contract.TaskResultBody{}, err
	}
	p := e.T.Publication
	if p == nil || p.ID != st.PubID {
		return contract.TaskResultBody{}, contract.TaskError(contract.CodeNotFound, "", "", "no such publication")
	}
	if p.Phase == contract.PubPhaseSettled {
		return *e.T.Result, nil
	}
	if s.FailWrite != nil {
		if err := s.FailWrite("settle", id); err != nil {
			return contract.TaskResultBody{}, err
		}
	}
	next := e.T
	np := *p
	status := st.Status
	np.Phase, np.Status = contract.PubPhaseSettled, &status
	if st.Error != "" {
		code := st.Error
		np.Error = &code
	}
	r := np.Candidate
	switch np.State {
	case contract.TaskCancelled:
		r.Outcome = contract.OutcomeCancelled
	case contract.TaskTimedOut:
		r.Outcome = contract.OutcomeTimedOut
	}
	w := st.Workspace
	r.Workspace = &w
	r = r.Sealed()
	next.Publication, next.Result, next.State, next.Live, next.Committed = &np, &r, np.State, false, true
	old := e.T
	e.T = next
	if err := s.writeLocked(e); err != nil {
		e.T = old
		return contract.TaskResultBody{}, err
	}
	s.notifyLocked()
	return r, nil
}

// AwaitDurable implements taskpublication.Tasks.
func (s *Store) AwaitDurable(ctx context.Context, id string) error {
	for {
		s.mu.Lock()
		e, err := s.get(id)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		ch := s.changed
		done := e.T.Committed
		s.mu.Unlock()
		if done {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Authorized implements taskpublication.Tasks.
func (s *Store) Authorized() []taskpublication.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []taskpublication.Task
	for _, e := range s.tasks {
		if p := e.T.Publication; p != nil && p.Phase == contract.PubPhaseAuthorized {
			out = append(out, view(e))
		}
	}
	return out
}
