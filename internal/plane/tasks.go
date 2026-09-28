package plane

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Task lifecycle and admission on the plane (iterations 05 and 06a). The
// plane is the only authority for admission and public task state.
// Dispatch admission shares one nonblocking mutation gate with role
// add/set/rm; child state and output use per-task serialization under the
// task observation lock, never the gate.
//
// Lock order: mutation gate, task observation lock (taskService.mu), node
// observation lock (nodeRegistry.mu), then a node stream's lock
// (nodeStream.mu) and the loss queue lock (taskService.lossMu, innermost);
// the removal coordinator's lock (removalCoordinator.mu, iteration 06b) is
// never held while taking any of them. No disk or network I/O is performed
// under any of them; a per-task writer performs every document write
// outside the locks and installs its outcome under the task lock after
// revalidating the task's revision. Lease and startup-grace expiry
// enqueue loss facts under the node lock; they are applied under the task
// lock by the loss worker (or first by an inventory), never in reverse
// lock order. A task completion writer never acquires the mutation gate.
const (
	// admissionTimeout bounds one dispatch, persistence included.
	admissionTimeout = 6 * time.Second
	// startControl is a start's deadline from its pending record's first
	// visible publication: queue waiting, write and reply included; it
	// never resets. 06a raised it from 4 s to 15 s: the worker's durable
	// launch barriers take up to its 10 s preparation budget.
	startControl = 15 * time.Second
	// maxQueuedStarts bounds the not-yet-sent starts per node.
	maxQueuedStarts = 100
	// commitWatchdog bounds how long a captured terminal candidate's
	// commit may take before its storage condition becomes visible.
	commitWatchdog = 30 * time.Second
	// checkpointEvery is the ceiling on log checkpoint frequency.
	checkpointEvery = time.Second
	// storageRetry is the coalesced retry interval of failed publications.
	storageRetry = time.Second
)

// instanceKey is one role instance: reservations are keyed by role ID and
// registration order, so a re-added role is another instance.
type instanceKey struct {
	id    string
	order int
}

func keyOf(r contract.RoleRecord) instanceKey {
	return instanceKey{id: r.ID, order: r.RegistrationOrder}
}

// heldCount is an instance's held reservations and how many of them await
// reconciliation with their worker.
type heldCount struct{ held, reconciling int }

// sendState is a task start's transport state.
type sendState int

const (
	sendNone    sendState = iota // not submitted (durability pending, or loaded)
	sendQueued                   // queued on its attachment, never written
	sendSending                  // its write began
	sendReplied                  // the worker answered (or reconciled it)
	sendUnsent                   // definitely never written
)

// candKind is a terminal candidate's kind: the 06a writer's candidate set
// is natural result, lost decision and definite refusal; iteration 06b
// adds the control terminal (cancelled or timed_out).
type candKind int

const (
	candNatural candKind = iota + 1 // a worker's natural outcome
	candLost                        // a lost decision (plane's, or a worker's lost outcome)
	candRefusal                     // a definite refusal (no adapter ever ran)
	candControl                     // a control terminal: cancelled (under a stop intent or DW7) or timed_out
)

// terminalCand is the one latched terminal candidate of a task: its
// frozen terminal outcome (retained log included), its arrival instant at
// the plane and the worker's result digest ("" for a plane decision).
// Once latched it owns the commit attempt and is retried, never replaced.
type terminalCand struct {
	kind    candKind
	outcome terminalOutcome
	at      time.Time
	digest  string
}

// lateCand is a worker outcome received after a different terminal
// decision, awaiting its durable late-evidence write.
type lateCand struct {
	result contract.TaskResultBody
	at     time.Time
	log    contract.TaskLog
}

// taskEntry is one task's plane state. rec is the visible record; for a
// terminal task its log data is dropped from memory after durable
// publication (show and logs read the document).
type taskEntry struct {
	rec      contract.TaskRecord
	retained int
	// lateRetained is the retained length of the late evidence's tail
	// (its data lives in the document once published).
	lateRetained int
	key          instanceKey
	// confirmed: the visible publication's directory sync succeeded.
	confirmed bool
	// released: a terminal record of this task was confirmed, so its
	// reservation was released; it is never reacquired (a later
	// late-evidence write that fails holds admissions, not the slot).
	released bool
	// fault: a publication failed before or after visibility; retryAt is
	// the next coalesced retry.
	fault   bool
	retryAt time.Time
	// loaded: read from disk at startup (another plane epoch).
	loaded bool
	// reconciling: the execution's outcome awaits reconciliation with its
	// worker (loaded, or its attachment ended or its start was ambiguous).
	reconciling bool
	// Live execution state of this plane run: gen is the admission
	// attachment; rgen the attachment currently authorized to report on
	// this stable execution (its start reply, or a reconciliation), with
	// lateOK when that reconciliation authorized late evidence; lateBound
	// is the digest bound to the late staging ring; seen stamps the
	// reconciliation (attachment) whose inventory reported the task.
	gen, rgen uint64
	seq       uint64
	lateOK    bool
	lateBound string
	seen      uint64
	send      sendState
	started   bool
	startedAt time.Time
	visible   time.Time
	deadline  time.Time
	// res is the start's transport token (nil for a loaded task): owned
	// by the entry until the start is queued with it, and released
	// exactly once by whichever path ends the start.
	res *startReservation
	// ring is the received output of a live task; logDirty and
	// lastCheckpoint coalesce checkpoints.
	ring           *planeLog
	logDirty       bool
	lastCheckpoint time.Time
	// cand is the latched terminal candidate; late the pending late
	// evidence and lateRing its staged tail.
	cand     *terminalCand
	late     *lateCand
	lateRing *planeLog
	watchdog bool
	// commitFailed: the latched candidate's write failed at least once.
	commitFailed bool
	needRunning  bool
	// wake signals the task's persistence writer; writer reports it runs.
	wake   chan struct{}
	writer bool
	// contribution to counts and to the storage-blocked set.
	contribHeld, contribRec bool

	// Iteration 06b controls. pendingIntent is the selected stop intent not
	// yet visible (the writer's stop-intent publication owns it);
	// intentDurable: the visible intent's publication was confirmed (every
	// later record carries it). ctlDue is the earliest (re)delivery of the
	// control to the reporting attachment and ctlInflight its outstanding
	// exchange. waiters are cancel responses woken at each confirmed
	// publication; waitRegs the bounded wait registrations naming this
	// task.
	pendingIntent *contract.StopIntent
	intentDurable bool
	ctlDue        time.Time
	ctlInflight   bool
	waiters       []chan struct{}
	waitRegs      []*waitReg
}

// intentLocked is e's selected stop intent (pending or visible), or nil.
func (e *taskEntry) intentLocked() *contract.StopIntent {
	if e.pendingIntent != nil {
		return e.pendingIntent
	}
	return e.rec.StopIntent
}

func (e *taskEntry) terminal() bool { return contract.TaskTerminal(e.rec.State) }

// held reports whether the entry holds its instance's reservation:
// nonterminal records, and terminal ones never confirmed.
func (e *taskEntry) held() bool { return !e.terminal() || !e.released }

// isReconciling is the derived public flag: a nonterminal task without a
// latched terminal candidate whose execution awaits reconciliation.
func (e *taskEntry) isReconciling() bool { return !e.terminal() && e.cand == nil && e.reconciling }

// taskService owns tasks: admission, the start dispatcher callbacks,
// reconciliation, receipt of output and results, persistence writers,
// loss decisions and reads.
type taskService struct {
	st     *taskStore
	reg    *nodeRegistry
	roles  *roleRegistry
	clock  nodeClock
	logger *slog.Logger
	lookup contract.AdapterLookup
	gate   *atomic.Bool
	epoch  string
	rand   io.Reader
	events func(string)
	// hook, when non-nil, pauses at named stages with a key (a dispatch's
	// goal or a task ID) and the operation's context (tests only).
	hook func(stage, key string, ctx context.Context)

	mu      sync.Mutex
	tasks   map[string]*taskEntry
	ids     []string
	counts  map[instanceKey]*heldCount
	blocked map[*taskEntry]bool
	closed  bool
	stop    chan struct{}
	wg      sync.WaitGroup

	// lossMu guards the loss queue (innermost lock); lossKick wakes the
	// loss worker. admitted is the admission sequence: a loss fact never
	// affects a task admitted after it was captured.
	// logCap, when positive, replaces the 10 MiB combined tail budget of
	// late evidence (tests of the budget's arithmetic).
	logCap int

	lossMu   sync.Mutex
	losses   []lossFact
	lossKick chan struct{}
	admitted atomic.Uint64

	// Iteration 06b: the bounded wait registrations (plus the capacity
	// reserved by dispatches with a wait), the plane's per-call wait cap,
	// the nodes with a pending control delivery, and onRelease, called
	// (under mu, nonblocking) when an instance's reservation is released.
	waits       map[*waitReg]bool
	waitReserve int
	maxWait     time.Duration
	ctlNodes    map[string]map[string]bool
	onRelease   func(instanceKey)
}

func newTaskService(st *taskStore, reg *nodeRegistry, roles *roleRegistry, gate *atomic.Bool, d *deps, logger *slog.Logger, loaded []loadedTask) (*taskService, error) {
	ts := &taskService{st: st, reg: reg, roles: roles, clock: d.nodeClock, logger: logger, lookup: roleLookup, gate: gate, rand: d.rand,
		tasks: map[string]*taskEntry{}, counts: map[instanceKey]*heldCount{}, blocked: map[*taskEntry]bool{}, stop: make(chan struct{}),
		lossKick: make(chan struct{}, 1), waits: map[*waitReg]bool{}, maxWait: contract.DefaultMaxTaskWait, ctlNodes: map[string]map[string]bool{}}
	var b [16]byte
	if _, err := io.ReadFull(d.rand, b[:]); err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot generate the plane run epoch: %v", err)
	}
	ts.epoch = hex.EncodeToString(b[:])
	for _, lt := range loaded {
		rec := lt.rec
		e := &taskEntry{rec: rec, retained: lt.retained, lateRetained: lt.lateRetained, key: keyOf(rec.Role), confirmed: true, loaded: true,
			wake: make(chan struct{}, 1)}
		// A complete surviving file loaded on a new Run is the recovered
		// authority: a terminal one is confirmed and holds nothing.
		e.released = e.terminal()
		e.reconciling = !e.terminal()
		e.intentDurable = rec.StopIntent != nil
		ts.tasks[rec.TaskID] = e
		ts.ids = append(ts.ids, rec.TaskID)
		if e.held() {
			c := ts.count(e.key)
			if c.held >= contract.MaxSafeInteger {
				return nil, errf(contract.CodeConflict, "the task records hold more reservations for role %s than can be counted", e.key.id)
			}
		}
		ts.refreshLocked(e)
	}
	st.exists.Store(len(loaded) > 0)
	sort.Strings(ts.ids)
	return ts, nil
}

func (ts *taskService) event(e string) {
	if ts != nil && ts.events != nil {
		ts.events(e)
	}
}

func (ts *taskService) at(stage, key string, ctx context.Context) {
	if ts.hook != nil {
		ts.hook(stage, key, ctx)
	}
}

func (ts *taskService) count(k instanceKey) *heldCount {
	c := ts.counts[k]
	if c == nil {
		c = &heldCount{}
		ts.counts[k] = c
	}
	return c
}

// refreshLocked republishes e's contribution to the reservation counts
// and to the storage-blocked set, in the same critical section as the
// state change that caused it.
func (ts *taskService) refreshLocked(e *taskEntry) {
	held := e.held()
	rec := held && e.isReconciling()
	c := ts.count(e.key)
	if e.contribHeld {
		c.held--
	}
	if e.contribRec {
		c.reconciling--
	}
	if held {
		c.held++
	}
	if rec {
		c.reconciling++
	}
	released := e.contribHeld && !held
	e.contribHeld, e.contribRec = held, rec
	if c.held == 0 && c.reconciling == 0 {
		delete(ts.counts, e.key)
	}
	if released && ts.onRelease != nil {
		ts.onRelease(e.key)
	}
	if !e.confirmed || e.fault || e.watchdog {
		ts.blocked[e] = true
	} else {
		delete(ts.blocked, e)
	}
}

// heldLocked returns k's reservation counts.
func (ts *taskService) heldLocked(k instanceKey) heldCount {
	if c := ts.counts[k]; c != nil {
		return *c
	}
	return heldCount{}
}

// storageBlockedLocked reports that a task publication's durability is
// unconfirmed or failed: admissions are refused until it clears.
func (ts *taskService) storageBlockedLocked() bool { return len(ts.blocked) > 0 }

// wakeLocked signals e's persistence writer, starting one when none runs
// (a loaded task, or a terminal task receiving late evidence, has none).
func (ts *taskService) wakeLocked(e *taskEntry) {
	if e.wake == nil {
		e.wake = make(chan struct{}, 1)
	}
	if !e.writer && !ts.closed {
		e.writer = true
		ts.wg.Add(1)
		go ts.writerLoop(e)
	}
	notifyChan(e.wake)
}

func notifyChan(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// ---- Admission ----

// admissionError is an admission failure with its candidate snapshot.
func admissionError(code contract.Code, reason, msg string, cands []contract.TaskCandidate) error {
	d := map[string]any{"reason": reason, "candidates": candidatesDetail(cands)}
	for _, c := range cands {
		if c.ReconcilingInflight > 0 {
			msg += "; " + contract.ReconcilingNotice
			break
		}
	}
	return &contract.Error{Code: code, Message: msg, Details: d}
}

func candidatesDetail(cs []contract.TaskCandidate) []any {
	out := make([]any, 0, len(cs))
	for _, c := range cs {
		out = append(out, map[string]any{"role_id": c.RoleID, "node_id": c.NodeID, "registration_order": c.RegistrationOrder,
			"node_liveness": c.NodeLiveness, "inflight": c.Inflight, "reconciling_inflight": c.ReconcilingInflight, "concurrency": c.Concurrency,
			"can_accept": c.CanAccept, "reason": c.Reason})
	}
	return out
}

// targets returns the target's role records in registration order: by ID
// only that role; by name its exact matches.
func targets(doc *roleDoc, t contract.TaskTarget) []contract.RoleRecord {
	var out []contract.RoleRecord
	for _, r := range doc.roles {
		if (t.Kind == contract.TargetID && r.ID == t.Value) || (t.Kind == contract.TargetName && r.Name == t.Value) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RegistrationOrder < out[j].RegistrationOrder })
	if len(out) > contract.MaxCandidates {
		out = out[:contract.MaxCandidates]
	}
	return out
}

// observation is one coherent registry, node and task snapshot of a
// target's candidates.
type observation struct {
	roles   *roleState
	recs    []contract.RoleRecord
	cands   []contract.TaskCandidate
	streams []*nodeStream
	gens    []uint64
}

// observeLocked samples the target's candidates under the task lock (the
// caller holds it) and the node lock, at one clock instant.
func (ts *taskService) observeLocked(t contract.TaskTarget) observation {
	rs := ts.roles.load()
	ob := observation{roles: rs, recs: targets(rs.visible, t)}
	blocked := rs.blocked || ts.storageBlockedLocked()
	r := ts.reg
	r.mu.Lock()
	closers := r.expireLocked(r.clock.Now(), 0)
	for _, rec := range ob.recs {
		c, st, gen := r.candidateLocked(rs, rec, ts.heldLocked(keyOf(rec)), blocked)
		ob.cands = append(ob.cands, c)
		ob.streams = append(ob.streams, st)
		ob.gens = append(ob.gens, gen)
	}
	r.mu.Unlock()
	runAll(closers)
	return ob
}

// observe is observeLocked taking the task lock.
func (ts *taskService) observe(t contract.TaskTarget) observation {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.observeLocked(t)
}

// newTaskID returns t_ plus 32 random lowercase hex digits.
func (ts *taskService) newTaskID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(ts.rand, b[:]); err != nil {
		return "", err
	}
	return "t_" + hex.EncodeToString(b[:]), nil
}

// dispatch admits req atomically or fails at once with the candidates:
// no queue, no reroute and no retry. It returns the committed task's view
// (HTTP 202); the start is scheduled on the captured attachment after
// commit and the dispatch never waits for it.
func (ts *taskService) dispatch(ctx context.Context, req contract.DispatchRequest) (contract.TaskView, error) {
	goal := req.Goal
	deadline := ts.clock.Now().Add(admissionTimeout)
	mctx, cancel := clockDeadline(ctx, ts.clock, deadline)
	defer cancel()
	ts.at("before-gate-CAS", goal, mctx)
	if !ts.gate.CompareAndSwap(false, true) {
		ob := ts.observe(req.Target)
		ts.event("gate-busy " + goal)
		return contract.TaskView{}, admissionError(contract.CodeUnavailable, contract.ReasonBusy, "another dispatch or role change is in progress; nothing was dispatched, retry", ob.cands)
	}
	ts.event("gate-acquired " + goal)
	view, e, err := ts.admit(mctx, deadline, req)
	ts.gate.Store(false)
	ts.event("gate-released " + goal)
	if err != nil {
		return contract.TaskView{}, err
	}
	ts.afterCommit(e)
	return view, nil
}

// live reports whether the admission may still publish.
func (ts *taskService) live(ctx context.Context, deadline time.Time) error {
	if ctx.Err() != nil || !ts.clock.Now().Before(deadline) {
		return contract.TaskError(contract.CodeUnavailable, "", contract.ReasonValidationTimeout, "the dispatch was canceled or ran out of time before publication; nothing was dispatched, retry")
	}
	return nil
}

// admit runs with the gate held.
func (ts *taskService) admit(ctx context.Context, deadline time.Time, req contract.DispatchRequest) (contract.TaskView, *taskEntry, error) {
	goal := req.Goal
	if _, resynced, err := ts.roles.resync(); err != nil {
		return contract.TaskView{}, nil, err
	} else if resynced {
		ts.logger.Info("role registry durability confirmed")
	}
	ts.resyncStore()
	ts.mu.Lock()
	ob := ts.observeLocked(req.Target)
	ts.mu.Unlock()
	if len(ob.recs) == 0 {
		what := "role " + req.Target.Value
		if req.Target.Kind == contract.TargetName {
			what = "no role named " + req.Target.Value
			return contract.TaskView{}, nil, admissionError(contract.CodeNotFound, "", what+" exists; nothing was dispatched", nil)
		}
		return contract.TaskView{}, nil, admissionError(contract.CodeNotFound, "", what+" does not exist; nothing was dispatched", nil)
	}
	pick := -1
	for i, c := range ob.cands {
		if c.CanAccept {
			pick = i
			break
		}
	}
	if pick < 0 {
		return contract.TaskView{}, nil, admissionError(contract.CodeUnavailable, contract.ReasonNoCapacity, "no candidate role can accept the task now; nothing was dispatched (no queue, no reroute)", ob.cands)
	}
	role, st, gen := ob.recs[pick], ob.streams[pick], ob.gens[pick]
	eff, err := contract.ResolveEffective(role, req.Override, ts.lookup)
	if err != nil {
		return contract.TaskView{}, nil, err
	}
	res := st.reserveStart()
	if res == nil {
		return contract.TaskView{}, nil, admissionError(contract.CodeUnavailable, contract.ReasonBusy, "the node's start queue is full or its stream ended; nothing was dispatched, retry", ob.cands)
	}
	release := res.release
	now := ts.clock.Now().UTC()
	rec := contract.TaskRecord{Request: req, Role: role, RolesRevision: ob.roles.visible.revision, Effective: eff,
		Execution: contract.ExecutionToken{Epoch: ts.epoch, Attachment: int(gen)}, State: contract.TaskPending, CreatedAt: now, Revision: 1,
		TimeoutPolicy: contract.TimeoutPolicyEnforced, Schema: contract.TaskRecordSchemaVersion}
	ts.at("before-task-publication", goal, ctx)
	// recheck is the authorization check: caller liveness, the registry
	// revision, the attachment generation and lease, the capacity and the
	// store's durability, under the short locks.
	recheck := func() error {
		if err := ts.live(ctx, deadline); err != nil {
			return err
		}
		ts.mu.Lock()
		defer ts.mu.Unlock()
		again := ts.observeLocked(contract.TaskTarget{Kind: contract.TargetID, Value: role.ID})
		if again.roles.visible.revision != ob.roles.visible.revision || len(again.recs) != 1 || again.recs[0].RegistrationOrder != role.RegistrationOrder ||
			!again.cands[0].CanAccept || again.gens[0] != gen {
			return admissionError(contract.CodeUnavailable, contract.ReasonBusy, "the role, its node or its capacity changed during the dispatch; nothing was dispatched, retry", again.cands)
		}
		return nil
	}
	var e *taskEntry
	for attempt := 0; ; attempt++ {
		id, err := ts.newTaskID()
		if err != nil {
			release()
			return contract.TaskView{}, nil, wrapf(contract.CodeInternal, err, "cannot generate a task ID: %v", err)
		}
		rec.TaskID = id
		// The stable execution identity: the canonical digest of the start
		// this record will send (it never changes).
		sd := startBody(rec).StartDigestHex()
		rec.StartDigest = &sd
		if err := recheck(); err != nil {
			release()
			return contract.TaskView{}, nil, err
		}
		tmp, err := ts.st.prepare(rec)
		if err != nil {
			release()
			return contract.TaskView{}, nil, wrapf(contract.CodeInternal, err, "cannot write the task record: %v; nothing was dispatched, retry", err)
		}
		if err := recheck(); err != nil {
			removeFile(tmp)
			release()
			return contract.TaskView{}, nil, err
		}
		visible, perr := ts.st.publishFirst(tmp, id)
		if errors.Is(perr, fs.ErrExist) && attempt < 3 {
			continue // an ID collision: another ID before anything is sent
		}
		if perr != nil && !errors.Is(perr, errTaskUnconfirmed) {
			release()
			return contract.TaskView{}, nil, wrapf(contract.CodeInternal, perr, "cannot publish the task record: %v; nothing was dispatched, retry", perr)
		}
		e = &taskEntry{rec: rec, key: keyOf(role), confirmed: perr == nil, gen: gen, visible: visible, deadline: visible.Add(startControl),
			ring: newPlaneLog(), wake: make(chan struct{}, 1), res: res}
		if perr != nil {
			e.fault, e.retryAt = true, visible.Add(storageRetry)
			ts.logger.Error("task durability unconfirmed", "task_id", id, "error", perr)
		}
		break
	}
	ts.mu.Lock()
	e.seq = ts.admitted.Add(1)
	ts.tasks[rec.TaskID] = e
	i := sort.SearchStrings(ts.ids, rec.TaskID)
	ts.ids = append(ts.ids, "")
	copy(ts.ids[i+1:], ts.ids[i:])
	ts.ids[i] = rec.TaskID
	ts.refreshLocked(e)
	e.writer = true
	ts.wg.Add(1)
	view := ts.viewLocked(e, contract.DefaultTailLines, ts.roles.load().visible, ts.clock.Now())
	ts.mu.Unlock()
	ts.logger.Info("task admitted", "task_id", rec.TaskID, "role_id", role.ID, "node_id", role.Node, "registration_order", role.RegistrationOrder)
	ts.event("task-published " + goal)
	go ts.writerLoop(e)
	return view, e, nil
}

// startBody is the task_start rec's execution sends: its digest is the
// record's stable start digest.
func startBody(rec contract.TaskRecord) contract.TaskStartBody {
	return contract.TaskStartBody{TaskID: rec.TaskID, Execution: rec.Execution, RolesRevision: rec.RolesRevision, Role: rec.Role,
		Request: rec.Request, Effective: rec.Effective}
}

// afterCommit transfers the committed task to the dispatcher: a confirmed
// pending record's start is queued on its captured attachment now; an
// unconfirmed one waits for its directory sync (the writer submits it).
func (ts *taskService) afterCommit(e *taskEntry) {
	ts.mu.Lock()
	ok := e.confirmed
	ts.mu.Unlock()
	if ok {
		ts.submit(e)
	}
}

// resyncStore retries the task directory sync that confirms every
// visible but unconfirmed publication; each confirmed entry then
// proceeds (a pending start is submitted, a terminal slot released).
//
// A sync proves only the publications that were visible before it began,
// so each entry is captured with its visible revision and confirmed only
// if that revision is still the visible one: an overlapping retry whose
// sync preceded a newer publication never confirms it.
func (ts *taskService) resyncStore() {
	ts.mu.Lock()
	var pending []pendingSync
	for e := range ts.blocked {
		if !e.confirmed {
			pending = append(pending, pendingSync{e: e, revision: e.rec.Revision})
		}
	}
	ts.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	if err := ts.st.resync(); err != nil {
		ts.event("resync-failed")
		return
	}
	ts.at("resync-synced", "store", context.Background())
	for _, p := range pending {
		ts.confirm(p.e, p.revision)
	}
}

// pendingSync is a visible, unconfirmed publication a sync covers: the
// entry and the revision that was visible when the sync was scheduled.
type pendingSync struct {
	e        *taskEntry
	revision int
}

// confirm marks e's visible publication of revision durable and
// continues it; a newer publication is not covered and stays unconfirmed.
func (ts *taskService) confirm(e *taskEntry, revision int) {
	ts.mu.Lock()
	if e.confirmed || e.rec.Revision != revision {
		ts.mu.Unlock()
		return
	}
	e.confirmed, e.fault = true, false
	ts.finishPublicationLocked(e)
	submit := e.rec.State == contract.TaskPending && e.send == sendNone && !e.loaded && e.cand == nil
	ts.refreshLocked(e)
	ts.wakeLocked(e)
	id := e.rec.TaskID
	ts.mu.Unlock()
	ts.event("task-confirmed " + id)
	if submit {
		ts.submit(e)
	}
}

// submit queues e's sole start attempt on its captured attachment, or
// rejects it as definitely unsent when the attachment is gone or its
// deadline passed before anything was written. A task already resolved
// (a loss latched at expiry) is never sent.
func (ts *taskService) submit(e *taskEntry) {
	ts.mu.Lock()
	if e.send != sendNone || e.rec.State != contract.TaskPending || e.cand != nil {
		ts.mu.Unlock()
		return
	}
	expired := !ts.clock.Now().Before(e.deadline)
	e.send = sendQueued
	body := startBody(e.rec)
	res := e.res
	ts.mu.Unlock()
	if expired || !ts.reg.enqueueStart(e.rec.Role.Node, e.gen, &startItem{id: e.rec.TaskID, body: body, deadline: e.deadline, res: res}) {
		// Never queued: the token is returned here, where the start ends.
		res.release()
		ts.startUnsent(e.rec.TaskID)
	}
}

// ---- Dispatcher callbacks (called by the node stream session) ----

// errNoTasks refuses task frames on a node service without a task service.
var errNoTasks = errf(contract.CodeInvalidArgument, "this plane serves no tasks")

// startItem is one queued start: its body and its never-reset deadline.
type startItem struct {
	id       string
	body     contract.TaskStartBody
	deadline time.Time
	res      *startReservation
}

// claimStart moves a queued start to sending immediately before its write
// begins; a start that is no longer queued (already rejected or resolved)
// is dropped.
func (ts *taskService) claimStart(id string) bool {
	if ts == nil {
		return false
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e := ts.tasks[id]
	if e == nil || e.send != sendQueued || e.rec.State != contract.TaskPending || e.cand != nil || e.intentLocked() != nil {
		return false
	}
	e.send = sendSending
	return true
}

// startUnsent rejects a start the plane can prove was never written: only
// this Run's in-memory transport state is such proof (a loaded task never
// takes this path).
func (ts *taskService) startUnsent(id string) {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e := ts.tasks[id]
	if e == nil || e.loaded || e.rec.State != contract.TaskPending || e.send == sendSending || e.send == sendReplied || e.cand != nil {
		return
	}
	e.send = sendUnsent
	ts.latchRefusalLocked(e, &contract.TaskReason{Code: contract.ReasonStartNotSent,
		Message: "the start was never sent to the worker (its stream ended or its start deadline passed first)"})
	ts.event("start-unsent " + id)
}

// startUncertain records an ambiguous start: its write began and no
// conclusive reply arrived. The task stays pending, holds its slot and
// awaits reconciliation (or its node's lease expiry).
func (ts *taskService) startUncertain(id string) {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e := ts.tasks[id]
	if e == nil || e.terminal() {
		return
	}
	e.reconciling = true
	ts.refreshLocked(e)
	ts.event("start-uncertain " + id)
}

// startReplied applies the worker's answer: ok records the observed start
// (running is published durably before any terminal record) and
// authorizes the attachment to report; a refusal becomes rejected with
// its safe reason.
func (ts *taskService) startReplied(id string, gen uint64, refusal *contract.Error) {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e := ts.tasks[id]
	if e == nil || e.send != sendSending {
		return
	}
	e.send = sendReplied
	if refusal != nil {
		reason, _ := refusal.Details["reason"].(string)
		ts.latchRefusalLocked(e, &contract.TaskReason{Code: reason, Message: sanitizeReason(refusal.Message)})
		ts.event("start-refused " + id + " " + reason)
		return
	}
	if e.cand == nil && !e.terminal() {
		e.started, e.needRunning = true, true
		e.startedAt = ts.clock.Now().UTC()
		e.rgen, e.lateOK, e.reconciling = gen, false, false
		ts.refreshLocked(e)
		// A durable stop intent selected while the start was being written
		// is delivered now, on the attachment that answered it.
		ts.wantControlLocked(e)
	}
	ts.event("task-started " + id)
	ts.wakeLocked(e)
}

// latchRefusalLocked latches a definite refusal (rejected) as e's terminal
// candidate unless one is already latched. Under a selected stop intent
// the definite no-start is a cancellation before start (iteration 06b).
func (ts *taskService) latchRefusalLocked(e *taskEntry, reason *contract.TaskReason) {
	if e.terminal() || e.cand != nil {
		return
	}
	if e.intentLocked() != nil {
		ts.latchCancelledBeforeStartLocked(e)
		return
	}
	t := ts.clock.Now().UTC()
	e.cand = &terminalCand{kind: candRefusal, at: t, outcome: terminalOutcome{state: contract.TaskRejected, finished: t, reason: reason}}
	e.needRunning = false
	ts.refreshLocked(e)
	ts.wakeLocked(e)
}

// detached marks every nonterminal task reporting on (or ambiguously
// started on) the ended attachment gen of node as awaiting
// reconciliation. Closing a stream alone never declares a task lost.
func (ts *taskService) detached(node string, gen uint64) {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, e := range ts.tasks {
		if e.rec.Role.Node != node || e.terminal() || e.cand != nil {
			continue
		}
		if e.rgen == gen || (e.gen == gen && e.send == sendSending) {
			e.reconciling = true
			ts.refreshLocked(e)
		}
	}
}

// authorizedLocked returns the task a sidecar request names when it may
// report on attachment gen of node: gen is the node's current live
// attachment at this instant (a frame queued before a detach or lease
// eviction is refused), the stable execution identity matches the stored
// task's node and token, and this attachment is authorized to report on it
// (its start reply, or reconciliation). The caller holds the task lock, so
// acceptance and the attachment check are one decision.
func (ts *taskService) authorizedLocked(node string, gen uint64, id string, tok contract.ExecutionToken) (*taskEntry, error) {
	e := ts.tasks[id]
	switch {
	case !ts.reg.currentAttachment(node, gen):
		return nil, errf(contract.CodeInvalidArgument, "attachment %d is no longer node %s's live attachment", gen, node)
	case e == nil:
		return nil, errf(contract.CodeInvalidArgument, "task %s is not known to this plane", id)
	case e.rec.Role.Node != node:
		return nil, errf(contract.CodeInvalidArgument, "task %s belongs to another node", id)
	case e.rec.Execution != tok:
		return nil, errf(contract.CodeInvalidArgument, "task %s's execution token is not its stable execution identity", id)
	case e.rgen != gen:
		return nil, errf(contract.CodeInvalidArgument, "task %s is not authorized on this attachment (no start reply or reconciliation)", id)
	}
	return e, nil
}

// removed reports whether e's instance no longer exists.
func removed(doc *roleDoc, k instanceKey) bool {
	r, _, ok := doc.find(k.id)
	return !ok || r.RegistrationOrder != k.order
}

// close stops the writers and the loss worker and waits for them (plane
// shutdown); a writer inside a filesystem call finishes that call first.
func (ts *taskService) close() {
	ts.mu.Lock()
	if !ts.closed {
		ts.closed = true
		close(ts.stop)
	}
	ts.mu.Unlock()
	ts.wg.Wait()
}

// removalCheck is ordinary role rm's reservation predicate for instance
// k: only an instance holding no reservation may be removed (iteration
// 06a removed 05's recovery-only exception; a forced removal cancels the
// held tasks first and deletes only after they are durably resolved).
func (ts *taskService) removalCheck(k instanceKey) error {
	ts.mu.Lock()
	c := ts.heldLocked(k)
	ts.mu.Unlock()
	if c.held == 0 {
		return nil
	}
	details := map[string]any{"role_id": k.id, "inflight": c.held, "reconciling_inflight": c.reconciling, "reason": contract.ReasonTasksInflight}
	msg := "role " + k.id + " has tasks in flight; wait for them to finish (see callsheet task ls), or cancel them with role rm --force; nothing was removed"
	if c.reconciling > 0 {
		msg = "role " + k.id + " has tasks in flight, some awaiting reconciliation with their worker; they are resolved when the worker reconnects or its lease expires (see callsheet task ls), or role rm --force cancels them; nothing was removed"
	}
	return &contract.Error{Code: contract.CodeConflict, Details: details, Message: msg}
}

// removeFile removes an unpublished temporary.
func removeFile(p string) { os.Remove(p) }

// taskNotFound reports an unknown task ID.
func taskNotFound(id string) error {
	return errf(contract.CodeNotFound, "task %s does not exist", id)
}

// sanitizeReason keeps a sidecar message safe for a record.
func sanitizeReason(s string) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) > contract.MaxReasonMessageBytes {
		s = s[:contract.MaxReasonMessageBytes]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}
