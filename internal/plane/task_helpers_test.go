package plane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 05 task fixtures: keyed stage hooks, a plane with an
// initial role registry, and a worker peer that speaks the task frames.
// Every product timer runs on the fake node clock; tests synchronize on
// named events and paused stages, never on sleeps.

// taskHooks pauses named task stages for one key (a dispatch's goal or a
// task ID): an armed (stage, key) pauses its next caller until released.
type taskHooks struct {
	mu    sync.Mutex
	armed map[string]chan hookCall
}

func newTaskHooks() *taskHooks { return &taskHooks{armed: map[string]chan hookCall{}} }

func hookKey(stage, key string) string { return stage + "\x00" + key }

// arm makes the next (stage, key) caller pause; its call arrives on the
// returned channel.
func (h *taskHooks) arm(stage, key string) chan hookCall {
	c := make(chan hookCall, 1)
	h.mu.Lock()
	h.armed[hookKey(stage, key)] = c
	h.mu.Unlock()
	return c
}

func (h *taskHooks) fn(stage, key string, ctx context.Context) {
	h.mu.Lock()
	c := h.armed[hookKey(stage, key)]
	delete(h.armed, hookKey(stage, key))
	h.mu.Unlock()
	if c == nil {
		return
	}
	call := hookCall{ctx: ctx, release: make(chan struct{})}
	c <- call
	<-call.release
}

// paused waits for the armed stage's caller.
func paused(t *testing.T, c chan hookCall, what string) hookCall {
	t.Helper()
	select {
	case call := <-c:
		// A failing assertion must not strand the paused stage (and with
		// it the plane's shutdown): cleanup releases it unless the test
		// did. Cleanups run after the test body, never concurrently.
		t.Cleanup(func() {
			select {
			case <-call.release:
			default:
				close(call.release)
			}
		})
		return call
	case <-time.After(testWait):
		t.Fatalf("nothing reached %s", what)
		return hookCall{}
	}
}

// prefixInjector fails (op, name) boundaries whose name has a prefix, on
// demand; concurrency-safe.
type prefixInjector struct {
	mu     sync.Mutex
	op     string
	prefix string
	count  int
	// syncOnce holds, per directory (tasks/ or roles/), a function run in
	// place of that directory's next sync's injection check (a slow, held
	// or failing sync); it is cleared before it runs, so it may install
	// the next one.
	syncOnce map[string]func() error
}

// onNextSync makes the next tasks/ directory sync run f first and
// return its error.
func (i *prefixInjector) onNextSync(f func() error) { i.onNextSyncOf(tasksName, f) }

// onNextSyncOf makes the next sync of directory dir run f first and
// return its error.
func (i *prefixInjector) onNextSyncOf(dir string, f func() error) {
	i.mu.Lock()
	if i.syncOnce == nil {
		i.syncOnce = map[string]func() error{}
	}
	i.syncOnce[dir] = f
	i.mu.Unlock()
}

func (i *prefixInjector) set(op, prefix string) {
	i.mu.Lock()
	i.op, i.prefix, i.count = op, prefix, 0
	i.mu.Unlock()
}

func (i *prefixInjector) fail(op, name string) error {
	i.mu.Lock()
	if f := i.syncOnce[name]; f != nil && op == "dirsync" {
		delete(i.syncOnce, name)
		i.mu.Unlock()
		return f()
	}
	defer i.mu.Unlock()
	if i.op != "" && op == i.op && strings.HasPrefix(name, i.prefix) {
		i.count++
		return fmt.Errorf("injected %s failure at %s", op, name)
	}
	return nil
}

func (i *prefixInjector) hits() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.count
}

// awaitOnce waits (bounded, real time) for an event that occurs at most
// once in a run (it names one task), wherever it is in the log: events of
// independent paths (a detach and a commit, say) are ordered by nothing,
// so a cursor already past it must not hide it. The cursor only moves
// forward, past the event when it lies ahead.
func (l *eventLog) awaitOnce(t *testing.T, want string) {
	t.Helper()
	at := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		for i, e := range l.events {
			if e == want {
				l.cursor = max(l.cursor, i+1)
				return i
			}
		}
		return -1
	}
	deadline := time.After(testWait)
	for at() < 0 {
		select {
		case <-l.sig:
		case <-deadline:
			// The event and the deadline may be ready together: recheck.
			if at() >= 0 {
				return
			}
			t.Fatalf("no event %q in %v", want, l.all())
		}
	}
}

// taskPlane is a served plane for task tests.
type taskPlane struct {
	*rolePlane
	th  *taskHooks
	inj *prefixInjector
	ts  chan *taskService
	cur *taskService
	// workers are every peer connected so far; restart closes them first
	// so shutdown never waits a close grace for a peer that is not
	// reading.
	workers []*worker
}

// startTaskPlane serves a plane whose registry holds recs (orders as
// given) for nodes ids, with fast syncs unless d is given.
func startTaskPlane(t *testing.T, d *deps, recs []contract.RoleRecord, ids ...string) *taskPlane {
	t.Helper()
	if d == nil {
		d = fast(testDeps(t))
	}
	tp := &taskPlane{th: newTaskHooks(), inj: &prefixInjector{}, ts: make(chan *taskService, 4)}
	d.taskHook = tp.th.fn
	d.fail = tp.inj.fail
	d.onTasks = func(ts *taskService) { tp.ts <- ts }
	next := 1
	for _, r := range recs {
		next = max(next, r.RegistrationOrder+1)
	}
	var doc *roleDoc
	if len(recs) > 0 {
		doc = docOf(1, next, recs...)
	}
	tp.rolePlane = startRolePlaneDoc(t, d, doc, ids...)
	return tp
}

// svc is the served task service (of the latest run).
func (tp *taskPlane) svc(t *testing.T) *taskService {
	t.Helper()
	for {
		select {
		case ts := <-tp.ts:
			tp.cur = ts
			continue
		default:
		}
		break
	}
	if tp.cur == nil {
		t.Fatal("no task service")
	}
	return tp.cur
}

// taskCfg is a role configuration of concurrency conc on node.
func taskCfg(id, name, node string, conc int) contract.RoleConfig {
	c := roleCfg(id, name, node)
	c.Concurrency = conc
	return c
}

// worker is a node peer speaking the task frames: b is its next sidecar
// request number (heartbeats, task_log and task_result share it).
type worker struct {
	*peer
	b int
	// ev is the plane's event log and node this worker's node: helpers
	// wait for the plane's own post-write event of each exchange, so no
	// test moves the clock while the plane's write is still in flight.
	ev   *eventLog
	node string
	// marks are the log positions before each sidecar request was sent.
	marks map[string]int
	// dialed is the log position before this worker's stream was dialed
	// (set by worker and workerInv): its session's detach follows it.
	dialed int
}

// detached waits until the plane detached this worker's session. A close
// the worker observed is sent while the session is still ending: the
// session's own state changes (an uncertain start, the detach) follow
// it, and a new stream of the node is refused until the detach.
func (w *worker) detached() {
	w.t.Helper()
	w.ev.awaitFrom(w.t, w.dialed, "detached "+w.node)
}

// online connects id's worker: hello, the initial snapshot acknowledged,
// every role ready, and one heartbeat.
func (tp *taskPlane) worker(t *testing.T, id string) *worker {
	t.Helper()
	dialed := tp.log.mark()
	p := tp.dial(t)
	p.ready = map[string]bool{}
	p.helloOK(id)
	b := p.ackReplace("p1")
	for _, r := range b.Roles {
		p.ready[r.ID] = true
	}
	w := &worker{peer: p, b: 1, ev: tp.log, node: id, marks: map[string]int{}, dialed: dialed}
	tp.workers = append(tp.workers, w)
	w.beat()
	return w
}

// beat sends the next heartbeat (describing the acknowledged snapshot,
// with the peer's inflight observations), reads its ack and waits until
// the plane's ack write returned.
func (w *worker) beat() {
	w.t.Helper()
	rid := "b" + strconv.Itoa(w.b)
	from := w.ev.mark()
	w.heartbeat(w.b)
	w.b++
	w.ev.awaitFrom(w.t, from, "ack "+w.node+" "+rid)
}

// isPlaneRequest reports frames the plane initiates.
func isPlaneRequest(typ string) bool {
	return typ == contract.FrameRolesReplace || typ == contract.FrameRoleValidate || typ == contract.FrameTaskStart || typ == contract.FrameTaskReconcile ||
		typ == contract.FrameTaskCancel
}

// reply reads the plane's reply rid of type typ, holding plane requests
// that arrive first for later reads, in order.
func (w *worker) reply(typ, rid string) contract.NodeFrame {
	w.t.Helper()
	var skipped []contract.NodeFrame
	for {
		f := w.recv()
		if isPlaneRequest(f.Type) {
			skipped = append(skipped, f)
			continue
		}
		if f.Type != typ || f.RequestID != rid {
			w.t.Fatalf("got %s %s (%s), want %s %s", f.Type, f.RequestID, f.Body, typ, rid)
		}
		w.held = append(skipped, w.held...)
		return f
	}
}

// start reads task_start pid and decodes it.
func (w *worker) start(pid string) contract.TaskStartBody {
	w.t.Helper()
	f := w.expect(contract.FrameTaskStart, pid)
	b, err := contract.DecodeTaskStart(f.Body, roleLookup)
	if err != nil {
		w.t.Fatalf("task_start %s: %v", f.Body, err)
	}
	// A task's start is written once: its post-write event is unique.
	w.ev.awaitOnce(w.t, "start-sent "+w.node+" "+pid+" "+b.TaskID)
	return b
}

// answer replies to start pid: nil is ok:true, else a refusal.
func (w *worker) answer(pid, id string, refusal *contract.Error) {
	w.t.Helper()
	w.send(contract.ProtocolVersion, contract.FrameTaskStartResult, pid, contract.TaskStartResult{TaskID: id, Err: refusal})
}

// nextID assigns the next sidecar request ID.
func (w *worker) nextID() string {
	rid := "b" + strconv.Itoa(w.b)
	w.b++
	return rid
}

// sendLog sends one task_log and returns its request ID.
func (w *worker) sendLog(st contract.TaskStartBody, off int, data []byte) string {
	w.t.Helper()
	rid := w.nextID()
	w.marks[rid] = w.ev.mark()
	w.send(contract.ProtocolVersion, contract.FrameTaskLog, rid, contract.TaskLogBody{TaskID: st.TaskID, Execution: st.Execution, Offset: off, Data: data})
	return rid
}

// logAck reads the task_log_ack of rid and returns its next offset.
func (w *worker) logAck(rid string) int {
	w.t.Helper()
	f := w.reply(contract.FrameTaskLogAck, rid)
	a, err := contract.DecodeTaskLogAck(f.Body)
	if err != nil {
		w.t.Fatal(err)
	}
	w.ev.awaitFrom(w.t, w.marks[rid], "log-acked "+w.node+" "+rid)
	return a.NextOffset
}

// log sends data at off and returns the acknowledged next offset.
func (w *worker) log(st contract.TaskStartBody, off int, data []byte) int {
	w.t.Helper()
	return w.logAck(w.sendLog(st, off, data))
}

// result builds a sealed natural result body for st.
func result(st contract.TaskStartBody, exit int, out int, final *string) contract.TaskResultBody {
	return contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: &exit, FinalMessage: final,
		OutputBytes: out}.Sealed()
}

// sendResult sends a task_result and returns its request ID.
func (w *worker) sendResult(b contract.TaskResultBody) string {
	w.t.Helper()
	rid := w.nextID()
	w.marks[rid] = w.ev.mark()
	w.send(contract.ProtocolVersion, contract.FrameTaskResult, rid, b)
	return rid
}

// resultAck reads the acknowledgement of rid and reports whether it was
// committed.
func (w *worker) resultAck(rid, id string) bool {
	w.t.Helper()
	f := w.reply(contract.FrameTaskResultAck, rid)
	a, err := contract.DecodeTaskResultAck(f.Body)
	if err != nil || a.TaskID != id {
		w.t.Fatalf("result ack %s: %v", f.Body, err)
	}
	w.ev.awaitFrom(w.t, w.marks[rid], "result-acked "+w.node+" "+rid)
	return a.Committed
}

// taskReq is a valid dispatch request for a target.
func taskReq(kind, value, goal string) contract.DispatchRequest {
	return contract.DispatchRequest{Target: contract.TaskTarget{Kind: kind, Value: value}, Goal: goal, Payload: []string{},
		Acceptance: "tests pass", RequestedBy: contract.RequestedBy{Name: "callsheet", Version: "test", Hostname: "coord"}}
}

// dispatchAsync runs a dispatch through the verified client.
func (tp *taskPlane) dispatchAsync(r contract.DispatchRequest) *call[contract.TaskView] {
	return async(func() (contract.TaskView, error) { return tp.cl.Dispatch(bg, r) })
}

// admit dispatches r and requires admission.
func (tp *taskPlane) admit(t *testing.T, r contract.DispatchRequest) contract.TaskView {
	t.Helper()
	v, err := tp.cl.Dispatch(bg, r)
	if err != nil {
		t.Fatalf("dispatch %q: %v", r.Goal, err)
	}
	return v
}

// run admits a task to role id, has w start it on pid, and waits until
// the plane observed the start and published running durably.
func (tp *taskPlane) run(t *testing.T, w *worker, role, goal, pid string) contract.TaskStartBody {
	t.Helper()
	v := tp.admit(t, taskReq(contract.TargetID, role, goal))
	st := w.start(pid)
	if st.TaskID != v.TaskID {
		t.Fatalf("started %s, admitted %s", st.TaskID, v.TaskID)
	}
	w.answer(pid, st.TaskID, nil)
	tp.log.awaitOnce(t, "published running "+st.TaskID)
	return st
}

// runHeld is run with the running publication held before its write:
// the start is observed, so output and a result are accepted, but
// running is not published until release, which lets the write proceed
// and waits for published running. Nothing orders the writer's pass
// after a publication against the test's next frames, and a task's
// first checkpoint is due at once, so output acknowledged after
// published running may be checkpointed by that pass, unobserved and
// before any clock advance; output acknowledged before release is
// dirty when that pass runs, which then deterministically selects the
// captured result's terminal commit, or else the first checkpoint.
func (tp *taskPlane) runHeld(t *testing.T, w *worker, role, goal, pid string) (contract.TaskStartBody, func()) {
	t.Helper()
	v := tp.admit(t, taskReq(contract.TargetID, role, goal))
	st := w.start(pid)
	if st.TaskID != v.TaskID {
		t.Fatalf("started %s, admitted %s", st.TaskID, v.TaskID)
	}
	hold := tp.th.arm("running-queued", st.TaskID)
	w.answer(pid, st.TaskID, nil)
	call := paused(t, hold, "running-queued")
	return st, func() {
		t.Helper()
		close(call.release)
		tp.log.awaitOnce(t, "published running "+st.TaskID)
	}
}

// finish reports st's exit after out output bytes and waits for its
// durable terminal commit.
func (tp *taskPlane) finish(t *testing.T, w *worker, st contract.TaskStartBody, exit, out int) {
	t.Helper()
	w.resultAck(w.sendResult(result(st, exit, out, nil)), st.TaskID)
	tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
}

// startDeadline is task id's never-reset start deadline.
func (tp *taskPlane) startDeadline(t *testing.T, id string) time.Time {
	t.Helper()
	ts := tp.svc(t)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e := ts.tasks[id]
	if e == nil {
		t.Fatalf("no task %s", id)
	}
	return e.deadline
}

// startTokens is the transport tokens node's live attachment holds.
func (tp *taskPlane) startTokens(t *testing.T, node string) int {
	t.Helper()
	r := tp.svc(t).reg
	r.mu.Lock()
	st := r.nodes[node]
	if st == nil || st.att == nil || st.att.stream == nil {
		r.mu.Unlock()
		t.Fatalf("node %s has no live attachment", node)
	}
	sn := st.att.stream
	r.mu.Unlock()
	sn.mu.Lock()
	defer sn.mu.Unlock()
	return sn.tokens
}

// show reads task id through the client.
func (tp *taskPlane) show(t *testing.T, id string) contract.TaskView {
	t.Helper()
	v, err := tp.cl.ShowTask(bg, id, contract.DefaultTailLines)
	if err != nil {
		t.Fatalf("show %s: %v", id, err)
	}
	return v
}

// taskFile reads and strictly parses id's document.
func (tp *taskPlane) taskFile(t *testing.T, id string) contract.TaskRecord {
	t.Helper()
	rec, err := layout{root: tp.root}.readTaskFile(id, roleLookup)
	if err != nil {
		t.Fatalf("task file %s: %v", id, err)
	}
	return rec
}

// taskBytes reads id's document bytes.
func taskBytes(t *testing.T, root, id string) []byte {
	t.Helper()
	b, err := os.ReadFile(layout{root: root}.path(taskRel(id)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// candidates decodes an admission error's candidate snapshot.
func candidatesOf(t *testing.T, err error) []contract.TaskCandidate {
	t.Helper()
	ce, ok := err.(*contract.Error)
	if !ok || ce.Details == nil {
		t.Fatalf("no candidate details in %v", err)
	}
	cs, perr := contract.ParseCandidates(ce.Details["candidates"])
	if perr != nil {
		t.Fatalf("candidates of %v: %v", err, perr)
	}
	return cs
}

// roleInflight reads role id's held reservations through role show.
func (tp *taskPlane) roleInflight(t *testing.T, id string) int {
	t.Helper()
	return tp.view(t, id).Inflight
}

func mustJSONOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// entry is st's inventory entry in phase: started (running, launched),
// digest (result or lost phases).
func entry(st contract.TaskStartBody, phase string, started *time.Time, digest *string) contract.TaskInventoryEntry {
	e := contract.TaskInventoryEntry{TaskID: st.TaskID, Execution: st.Execution, StartDigest: st.StartDigestHex(), Phase: phase, ResultDigest: digest}
	if started != nil {
		s := contract.FormatTime(*started)
		e.StartedAt = &s
	}
	return e
}

// reconcile reports entries as one final inventory page i1 and returns
// the plane's dispositions (every reconcile page acknowledged), by task.
func (p *peer) reconcile(entries ...contract.TaskInventoryEntry) map[string]string {
	p.t.Helper()
	p.send(contract.ProtocolVersion, contract.FrameTaskInventory, "i1", contract.TaskInventoryBody{RunID: peerRunID, Final: true, Entries: entries})
	p.expect(contract.FrameTaskInventoryAck, "i1")
	out := map[string]string{}
	for n := 1; ; n++ {
		rid := "r" + strconv.Itoa(n)
		f := p.expect(contract.FrameTaskReconcile, rid)
		b, err := contract.DecodeTaskReconcile(f.Body)
		if err != nil {
			p.t.Fatalf("reconcile %s: %v", f.Body, err)
		}
		for _, e := range b.Entries {
			out[e.TaskID] = e.Action
		}
		p.send(contract.ProtocolVersion, contract.FrameTaskReconcileAck, rid, contract.TaskReconcileAckBody{Received: true})
		if b.Final {
			return out
		}
	}
}

// workerInv connects id's worker like worker, reporting entries in its
// inventory and requiring the dispositions want.
func (tp *taskPlane) workerInv(t *testing.T, id string, want map[string]string, entries ...contract.TaskInventoryEntry) *worker {
	t.Helper()
	dialed := tp.log.mark()
	p := tp.dial(t)
	p.ready = map[string]bool{}
	p.hello(id)
	p.expect(contract.FrameHelloOK, "h1")
	if got := p.reconcile(entries...); len(got) != len(want) {
		t.Fatalf("dispositions %v, want %v", got, want)
	} else {
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("dispositions %v, want %v", got, want)
			}
		}
	}
	tp.log.await(t, "reconciled "+id)
	b := p.ackReplace("p1")
	for _, r := range b.Roles {
		p.ready[r.ID] = true
	}
	w := &worker{peer: p, b: 1, ev: tp.log, node: id, marks: map[string]int{}, dialed: dialed}
	tp.workers = append(tp.workers, w)
	w.beat()
	return w
}

// sendLateLog sends one task_log tagged with digest (a replayed tail).
func (w *worker) sendLateLog(st contract.TaskStartBody, off int, data []byte, digest string) string {
	w.t.Helper()
	rid := w.nextID()
	w.marks[rid] = w.ev.mark()
	w.send(contract.ProtocolVersion, contract.FrameTaskLog, rid, contract.TaskLogBody{TaskID: st.TaskID, Execution: st.Execution, Offset: off, Data: data, LateDigest: &digest})
	return rid
}

// lostResult builds a sealed lost outcome for st.
func lostResult(st contract.TaskStartBody, out int) contract.TaskResultBody {
	return contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeLost, OutputBytes: out}.Sealed()
}
