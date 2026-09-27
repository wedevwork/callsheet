package plane

import (
	"context"
	"crypto/sha256"
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

// Task lifecycle and admission on the plane (iteration 05). The plane is
// the only authority for admission and public task state. Dispatch
// admission shares one nonblocking mutation gate with role add/set/rm;
// child state and output use per-task serialization under the task
// observation lock, never the gate. Lock order: gate, task observation
// lock (taskService.mu), node observation lock (nodeRegistry.mu); no
// disk or network I/O is performed under either lock.
const (
	// admissionTimeout bounds one dispatch, persistence included.
	admissionTimeout = 6 * time.Second
	// startControl is a start's deadline from its pending record's first
	// visible publication: queue waiting, write and reply included; it
	// never resets.
	startControl = 4 * time.Second
	// maxQueuedStarts bounds the not-yet-sent starts per node.
	maxQueuedStarts = 100
	// commitWatchdog bounds how long a captured result's terminal commit
	// may take before its storage condition becomes visible.
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

// heldCount is an instance's held reservations and how many of them are
// recovery-required.
type heldCount struct{ held, recovery int }

// sendState is a task start's transport state.
type sendState int

const (
	sendNone    sendState = iota // not submitted (durability pending, or loaded)
	sendQueued                   // queued on its attachment, never written
	sendSending                  // its write began
	sendReplied                  // the worker answered
	sendUnsent                   // definitely never written
)

// completion is a captured result: the frozen terminal record (retained
// log included) and its acceptance instant.
type completion struct {
	rec contract.TaskRecord
	at  time.Time
}

// taskEntry is one task's plane state. rec is the visible record; for a
// terminal task its log data is dropped from memory after durable
// publication (show and logs read the document).
type taskEntry struct {
	rec      contract.TaskRecord
	retained int
	key      instanceKey
	// confirmed: the visible publication's directory sync succeeded.
	confirmed bool
	// fault: a publication failed before or after visibility; retryAt is
	// the next coalesced retry.
	fault   bool
	retryAt time.Time
	// loaded: read from disk at startup (another plane epoch).
	loaded bool
	// Live execution state of this plane run.
	gen       uint64
	send      sendState
	started   bool
	startedAt time.Time
	recovery  string
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
	mailbox        *completion
	// accepted is the digest of the result accepted on the task's
	// attachment: an identical resubmission there is acknowledged again,
	// before or after the commit.
	accepted     [32]byte
	watchdog     bool
	commitFailed bool
	needRunning  bool
	reject       *contract.TaskReason
	// wake signals the task's persistence writer; writer reports it runs.
	wake   chan struct{}
	writer bool
	// contribution to counts and to the storage-blocked set.
	contribHeld, contribRecovery bool
}

func (e *taskEntry) terminal() bool { return contract.TaskTerminal(e.rec.State) }

// held reports whether the entry holds its instance's reservation:
// nonterminal records, and terminal ones whose durability is unconfirmed.
func (e *taskEntry) held() bool { return !e.terminal() || !e.confirmed }

// recoveryReason is the derived recovery reason ("" when none): a
// nonterminal task whose attachment or plane run ended, whose start was
// ambiguous or whose instance was removed, unless the plane already
// captured a valid result.
func (e *taskEntry) recoveryReason(removed bool) string {
	if e.terminal() || e.mailbox != nil {
		return ""
	}
	switch {
	case removed:
		return contract.RecoveryRoleRemoved
	case e.loaded:
		return contract.RecoveryPlaneRestarted
	}
	return e.recovery
}

// taskService owns tasks: admission, the start dispatcher callbacks,
// receipt of output and results, persistence writers and reads.
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
}

func newTaskService(st *taskStore, reg *nodeRegistry, roles *roleRegistry, gate *atomic.Bool, d *deps, logger *slog.Logger, loaded []loadedTask) (*taskService, error) {
	ts := &taskService{st: st, reg: reg, roles: roles, clock: d.nodeClock, logger: logger, lookup: roleLookup, gate: gate, rand: d.rand,
		tasks: map[string]*taskEntry{}, counts: map[instanceKey]*heldCount{}, blocked: map[*taskEntry]bool{}, stop: make(chan struct{})}
	var b [16]byte
	if _, err := io.ReadFull(d.rand, b[:]); err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot generate the plane run epoch: %v", err)
	}
	ts.epoch = hex.EncodeToString(b[:])
	for _, lt := range loaded {
		rec := lt.rec
		e := &taskEntry{rec: rec, retained: lt.retained, key: keyOf(rec.Role), confirmed: true, loaded: true}
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
	if ts.events != nil {
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
	rec := held && e.recoveryReason(false) != ""
	c := ts.count(e.key)
	if e.contribHeld {
		c.held--
	}
	if e.contribRecovery {
		c.recovery--
	}
	if held {
		c.held++
	}
	if rec {
		c.recovery++
	}
	e.contribHeld, e.contribRecovery = held, rec
	if c.held == 0 && c.recovery == 0 {
		delete(ts.counts, e.key)
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

// wakeLocked signals e's persistence writer.
func (ts *taskService) wakeLocked(e *taskEntry) {
	if e.wake != nil {
		notifyChan(e.wake)
	}
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
		if c.RecoveryInflight > 0 {
			msg += "; " + contract.RecoveryNotice
			break
		}
	}
	return &contract.Error{Code: code, Message: msg, Details: d}
}

func candidatesDetail(cs []contract.TaskCandidate) []any {
	out := make([]any, 0, len(cs))
	for _, c := range cs {
		out = append(out, map[string]any{"role_id": c.RoleID, "node_id": c.NodeID, "registration_order": c.RegistrationOrder,
			"node_liveness": c.NodeLiveness, "inflight": c.Inflight, "recovery_inflight": c.RecoveryInflight, "concurrency": c.Concurrency,
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
		Execution: contract.ExecutionToken{Epoch: ts.epoch, Attachment: int(gen)}, State: contract.TaskPending, CreatedAt: now, Revision: 1}
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
	go ts.writerLoop(e, st)
	return view, e, nil
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
	submit := e.rec.State == contract.TaskPending && e.send == sendNone && !e.loaded
	ts.refreshLocked(e)
	ts.wakeLocked(e)
	ts.mu.Unlock()
	ts.event("task-confirmed " + e.rec.TaskID)
	if submit {
		ts.submit(e)
	}
}

// submit queues e's sole start attempt on its captured attachment, or
// rejects it as definitely unsent when the attachment is gone or its
// deadline passed before anything was written.
func (ts *taskService) submit(e *taskEntry) {
	ts.mu.Lock()
	if e.send != sendNone || e.rec.State != contract.TaskPending {
		ts.mu.Unlock()
		return
	}
	expired := !ts.clock.Now().Before(e.deadline)
	e.send = sendQueued
	body := contract.TaskStartBody{TaskID: e.rec.TaskID, Execution: e.rec.Execution, RolesRevision: e.rec.RolesRevision, Role: e.rec.Role,
		Request: e.rec.Request, Effective: e.rec.Effective}
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
// begins; a start that is no longer queued (already rejected) is dropped.
func (ts *taskService) claimStart(id string) bool {
	if ts == nil {
		return false
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e := ts.tasks[id]
	if e == nil || e.send != sendQueued || e.rec.State != contract.TaskPending {
		return false
	}
	e.send = sendSending
	return true
}

// startUnsent rejects a start the plane can prove was never written.
func (ts *taskService) startUnsent(id string) {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e := ts.tasks[id]
	if e == nil || e.rec.State != contract.TaskPending || e.send == sendSending || e.send == sendReplied || e.reject != nil {
		return
	}
	e.send = sendUnsent
	e.reject = &contract.TaskReason{Code: contract.ReasonStartNotSent, Message: "the start was never sent to the worker (its stream ended or its start deadline passed first)"}
	ts.wakeLocked(e)
	ts.event("start-unsent " + id)
}

// startUncertain records an ambiguous start: its write began and no
// conclusive reply arrived. The task stays pending and holds its slot.
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
	if e.recovery == "" {
		e.recovery = contract.RecoveryStartUnconfirmed
	}
	ts.refreshLocked(e)
	ts.event("start-uncertain " + id)
}

// startReplied applies the worker's answer: ok records the observed start
// (running is published durably before any terminal record); a refusal
// becomes rejected with its safe reason.
func (ts *taskService) startReplied(id string, refusal *contract.Error) {
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
		e.reject = &contract.TaskReason{Code: reason, Message: sanitizeReason(refusal.Message)}
		ts.event("start-refused " + id + " " + reason)
	} else {
		e.started, e.needRunning = true, true
		e.startedAt = ts.clock.Now().UTC()
		ts.event("task-started " + id)
	}
	ts.wakeLocked(e)
}

// detached latches the interruption of every task started on the ended
// attachment gen of node without a captured result.
func (ts *taskService) detached(node string, gen uint64) {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, e := range ts.tasks {
		if e.gen == gen && e.rec.Role.Node == node && !e.terminal() && e.mailbox == nil && e.started && e.recovery == "" {
			e.recovery = contract.RecoveryAttachmentLost
			ts.refreshLocked(e)
		}
	}
}

// liveTaskLocked returns the task a sidecar request names when it may
// report on attachment gen of node: this plane run's epoch and the
// attachment the start was sent on, still the node's current live
// attachment at this instant (a frame queued before a detach or lease
// eviction is refused), and an observed start. The caller holds the task
// lock, so acceptance and the attachment check are one decision; a
// result captured before the attachment ended stays captured.
func (ts *taskService) liveTaskLocked(node string, gen uint64, id string, tok contract.ExecutionToken) (*taskEntry, error) {
	e := ts.tasks[id]
	switch {
	case !ts.reg.currentAttachment(node, gen):
		return nil, errf(contract.CodeInvalidArgument, "attachment %d is no longer node %s's live attachment", gen, node)
	case e == nil:
		return nil, errf(contract.CodeInvalidArgument, "task %s is not known to this plane", id)
	case e.rec.Role.Node != node:
		return nil, errf(contract.CodeInvalidArgument, "task %s belongs to another node", id)
	case tok.Epoch != ts.epoch || uint64(tok.Attachment) != gen || e.gen != gen || e.rec.Execution != tok:
		return nil, errf(contract.CodeInvalidArgument, "task %s's execution token is not this attachment's", id)
	case !e.started:
		return nil, errf(contract.CodeInvalidArgument, "task %s has no observed start on this attachment", id)
	}
	return e, nil
}

// receiveLog accepts one output chunk into bounded memory and returns
// the acknowledged next offset. It never waits for disk.
func (ts *taskService) receiveLog(node string, gen uint64, b contract.TaskLogBody) (int, error) {
	if ts == nil {
		return 0, errNoTasks
	}
	ts.at("log-received", b.TaskID, context.Background())
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, err := ts.liveTaskLocked(node, gen, b.TaskID, b.Execution)
	if err != nil {
		return 0, err
	}
	if e.mailbox != nil || e.terminal() || e.ring == nil {
		return 0, errf(contract.CodeInvalidArgument, "task %s already reported its result; no further output is accepted", b.TaskID)
	}
	next, err := e.ring.append(b.Offset, b.Data)
	if err != nil {
		return 0, err
	}
	e.logDirty = true
	return next, nil
}

// receiveResult captures a result into the task's completion mailbox:
// the acceptance instant. It derives the state from the exit, freezes the
// retained log, sets completion_pending and schedules the terminal
// commit; receipt never publishes a terminal state or releases a slot.
// An identical resubmission on the same attachment is acknowledged again.
func (ts *taskService) receiveResult(node string, gen uint64, b contract.TaskResultBody) error {
	if ts == nil {
		return errNoTasks
	}
	ts.at("result-received", b.TaskID, context.Background())
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, err := ts.liveTaskLocked(node, gen, b.TaskID, b.Execution)
	if err != nil {
		return err
	}
	digest := resultDigest(b)
	if e.mailbox != nil || e.terminal() {
		if e.accepted != ([32]byte{}) && e.accepted == digest {
			return nil
		}
		return errf(contract.CodeInvalidArgument, "task %s's result differs from the one already received, or it is already final", b.TaskID)
	}
	if e.ring == nil {
		return errf(contract.CodeInvalidArgument, "task %s is already final", b.TaskID)
	}
	if b.OutputBytes < e.ring.next {
		return errf(contract.CodeInvalidArgument, "task %s reports %d output bytes, fewer than the %d received", b.TaskID, b.OutputBytes, e.ring.next)
	}
	now := ts.clock.Now()
	lg := e.ring.finalize(b.OutputBytes, b.LogIncomplete, b.CounterOverflow)
	rec := e.rec
	t, started := now.UTC(), e.startedAt
	rec.StartedAt = &started
	rec.FinishedAt, rec.ExitCode, rec.Signal, rec.FinalMessage, rec.FinalMessageTruncated, rec.Log = &t, b.ExitCode, b.Signal, b.FinalMessage, b.FinalMessageTruncated, lg
	rec.State = contract.TaskFailed
	if b.ExitCode != nil && *b.ExitCode == 0 {
		rec.State = contract.TaskSucceeded
	}
	e.mailbox = &completion{rec: rec, at: now}
	e.accepted = digest
	ts.refreshLocked(e)
	ts.wakeLocked(e)
	ts.event("result-captured " + b.TaskID)
	return nil
}

// resultDigest is the SHA-256 of a result's canonical encoding.
func resultDigest(b contract.TaskResultBody) [32]byte {
	x, err := contract.Encode(b)
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256(x)
}

// tick is the sweep's once-per-second storage duty: it expires commit
// watchdogs and wakes writers whose retry or checkpoint is due.
func (ts *taskService) tick() {
	if ts == nil {
		return
	}
	now := ts.clock.Now()
	var resync bool
	ts.mu.Lock()
	for _, e := range ts.tasks {
		if e.mailbox != nil && !e.watchdog && !now.Before(e.mailbox.at.Add(commitWatchdog)) {
			e.watchdog = true
			ts.refreshLocked(e)
			ts.logger.Error("task result commit unconfirmed", "task_id", e.rec.TaskID, "reason", contract.ReasonResultStorageUnconfirmed)
			ts.event("commit-watchdog " + e.rec.TaskID)
		}
		if !e.confirmed && !now.Before(e.retryAt) {
			resync = true
		}
		if e.writer && ((e.fault && !now.Before(e.retryAt)) || (e.logDirty && !now.Before(e.lastCheckpoint.Add(checkpointEvery)))) {
			ts.wakeLocked(e)
		}
	}
	ts.mu.Unlock()
	if resync {
		ts.resyncStore()
	}
}

// ---- Persistence writer ----

// publication is one prepared document write.
type publication struct {
	kind string // running, checkpoint, terminal, rejected
	rec  contract.TaskRecord
	live int // retained bytes of rec (for the in-memory metadata)
}

// nextJobLocked selects e's next publication: nothing while its visible
// record is unconfirmed or a retry is not due; then running before any
// terminal record; then the terminal (captured result or rejection);
// then a due checkpoint.
func (ts *taskService) nextJobLocked(e *taskEntry, now time.Time) (*publication, bool) {
	if e.terminal() && e.confirmed {
		return nil, true
	}
	if !e.confirmed || (e.fault && now.Before(e.retryAt)) || e.rec.Revision >= contract.MaxSafeInteger {
		return nil, false
	}
	next := e.rec
	next.Revision++
	switch {
	case e.needRunning:
		started := e.startedAt
		next.State, next.StartedAt = contract.TaskRunning, &started
		next.Log = e.ring.snapshot()
		return &publication{kind: "running", rec: next, live: len(next.Log.Data)}, false
	case e.mailbox != nil:
		rec := e.mailbox.rec
		rec.Revision = next.Revision
		return &publication{kind: "terminal", rec: rec, live: len(rec.Log.Data)}, false
	case e.reject != nil:
		t := ts.clock.Now().UTC()
		next.State, next.FinishedAt, next.Reason = contract.TaskRejected, &t, e.reject
		next.Log = contract.TaskLog{}
		next.Candidates = ts.observeLocked(e.rec.Request.Target).cands
		return &publication{kind: "rejected", rec: next}, false
	case e.logDirty && e.rec.State == contract.TaskRunning && !now.Before(e.lastCheckpoint.Add(checkpointEvery)):
		next.Log = e.ring.snapshot()
		e.logDirty = false
		e.lastCheckpoint = now
		return &publication{kind: "checkpoint", rec: next, live: len(next.Log.Data)}, false
	}
	return nil, false
}

// writerLoop is e's task-specific persistence writer: it serializes the
// running publication, checkpoints and the terminal commit, one document
// at a time, without holding the gate or a node lock.
func (ts *taskService) writerLoop(e *taskEntry, st *nodeStream) {
	defer ts.wg.Done()
	for {
		now := ts.clock.Now()
		ts.mu.Lock()
		job, done := ts.nextJobLocked(e, now)
		if done {
			e.writer = false
			ts.mu.Unlock()
			return
		}
		ts.mu.Unlock()
		if job == nil {
			select {
			case <-e.wake:
				continue
			case <-ts.stop:
				ts.mu.Lock()
				e.writer = false
				ts.mu.Unlock()
				return
			}
		}
		// Named events before any syscall of the write (tests hold them).
		ts.at(job.kind+"-queued", e.rec.TaskID, context.Background())
		if job.kind == "terminal" || job.kind == "rejected" {
			ts.at("terminal-commit-queued", e.rec.TaskID, context.Background())
		}
		err := ts.st.update(job.rec)
		ts.mu.Lock()
		ts.applyLocked(e, job, err)
		ts.mu.Unlock()
	}
}

// applyLocked publishes a write's outcome atomically for readers: a
// confirmed document and its derived reservation change together; a
// visible but unconfirmed one is adopted with its reservation held and
// admissions blocked; a failure before visibility leaves the old record
// authoritative and schedules a coalesced retry.
func (ts *taskService) applyLocked(e *taskEntry, job *publication, err error) {
	now := ts.clock.Now()
	switch {
	case err == nil || errors.Is(err, errTaskUnconfirmed):
		e.rec = job.rec
		e.retained = job.live
		e.confirmed = err == nil
		e.fault = err != nil
		if err != nil {
			e.retryAt = now.Add(storageRetry)
			ts.logger.Error("task durability unconfirmed", "task_id", e.rec.TaskID, "error", err)
		}
		switch job.kind {
		case "running":
			e.needRunning = false
		case "terminal", "rejected":
			e.reject = nil
		}
		if e.confirmed {
			ts.finishPublicationLocked(e)
		}
		ts.event("published " + job.kind + " " + e.rec.TaskID)
	default:
		e.fault, e.retryAt = true, now.Add(storageRetry)
		if job.kind == "terminal" {
			e.commitFailed = true
		}
		if job.kind == "checkpoint" {
			e.logDirty = true
		}
		ts.logger.Error("task publication failed", "task_id", e.rec.TaskID, "kind", job.kind, "error", err)
		ts.event("publish-failed " + job.kind + " " + e.rec.TaskID)
	}
	ts.refreshLocked(e)
}

// finishPublicationLocked completes a confirmed publication: a terminal
// record drops the live ring and the captured mailbox and releases the
// reservation (refreshLocked) in the same critical section; the log data
// of any record leaves memory (the ring or the document holds it).
func (ts *taskService) finishPublicationLocked(e *taskEntry) {
	if e.terminal() {
		e.mailbox, e.ring, e.watchdog, e.commitFailed = nil, nil, false, false
		e.rec.Log.Data = nil
		ts.event("terminal-committed " + e.rec.TaskID)
	} else if e.ring != nil {
		e.rec.Log.Data = nil
	}
}

// close stops the writers and waits for them (plane shutdown); a writer
// inside a filesystem call finishes that call first.
func (ts *taskService) close() {
	ts.mu.Lock()
	if !ts.closed {
		ts.closed = true
		close(ts.stop)
	}
	ts.mu.Unlock()
	ts.wg.Wait()
}

// ---- Reads ----

// logOf returns e's current log (data may be nil for a terminal record
// whose tail is on disk) and its retained byte count.
func (ts *taskService) logOfLocked(e *taskEntry) (contract.TaskLog, int) {
	switch {
	case e.mailbox != nil:
		return e.mailbox.rec.Log, len(e.mailbox.rec.Log.Data)
	case e.ring != nil:
		return e.ring.meta()
	}
	return e.rec.Log, e.retained
}

// removedLocked reports whether e's instance no longer exists.
func removed(doc *roleDoc, k instanceKey) bool {
	r, _, ok := doc.find(k.id)
	return !ok || r.RegistrationOrder != k.order
}

// viewLocked builds e's TaskView at now; lines selects the tail. A
// terminal task's tail is read from its document by the caller when its
// data is not in memory (fillTail).
func (ts *taskService) viewLocked(e *taskEntry, lines int, doc *roleDoc, now time.Time) contract.TaskView {
	// A captured result is published only once durable: the view keeps
	// the visible nonterminal state, with completion_pending.
	rec := e.rec
	v := contract.TaskView{TaskID: rec.TaskID, Request: rec.Request, Role: contract.PublicRole(rec.Role), Effective: rec.Effective,
		State: rec.State, CreatedAt: contract.FormatTime(rec.CreatedAt), DurabilityConfirmed: e.confirmed, Reason: rec.Reason,
		Candidates: rec.Candidates, CompletionPending: e.mailbox != nil}
	if rec.StartedAt != nil {
		s := contract.FormatTime(*rec.StartedAt)
		v.StartedAt = &s
	}
	end := now
	if rec.FinishedAt != nil {
		s := contract.FormatTime(*rec.FinishedAt)
		v.FinishedAt = &s
		end = *rec.FinishedAt
	}
	v.ElapsedMS = int(max(0, end.Sub(rec.CreatedAt).Milliseconds()))
	if r := e.recoveryReason(removed(doc, e.key)); r != "" {
		v.RecoveryRequired, v.RecoveryReason = true, &r
	}
	if e.mailbox != nil && (e.watchdog || e.commitFailed) {
		r := contract.ReasonResultStorageUnconfirmed
		v.PersistenceReason = &r
	}
	lg, retained := ts.logOfLocked(e)
	meta := contract.MetaOf(contract.TaskLog{SourceBytes: lg.SourceBytes, ReceivedBytes: lg.ReceivedBytes, Incomplete: lg.Incomplete, CounterOverflow: lg.CounterOverflow}, v.RecoveryRequired)
	meta.RetainedBytes = retained
	meta.DroppedBytes = max(0, lg.SourceBytes-retained)
	meta.Truncated = lg.ReceivedBytes > retained || lg.Incomplete
	v.Log = meta
	switch {
	case e.ring != nil && e.mailbox == nil:
		v.LogTail, v.TailTruncated = contract.LogTail(e.ring.tail(contract.MaxTailBytes), lines)
		v.TailTruncated = v.TailTruncated || (lines > 0 && retained > contract.MaxTailBytes)
	case lg.Data != nil || retained == 0:
		v.LogTail, v.TailTruncated = contract.LogTail(lg.Data, lines)
	}
	if v.RecoveryRequired {
		v.Log.LogMayBeIncomplete = true
	}
	if contract.TaskTerminal(rec.State) {
		v.Result = &contract.TaskResult{State: rec.State, ExitCode: rec.ExitCode, Signal: rec.Signal, FinalMessage: rec.FinalMessage,
			FinalMessageTruncated: rec.FinalMessageTruncated, LogTail: v.LogTail}
	}
	return v
}

// needsFileTail reports that e's tail must come from its document.
func (ts *taskService) needsFileTailLocked(e *taskEntry) bool {
	lg, retained := ts.logOfLocked(e)
	return lg.Data == nil && retained > 0 && e.ring == nil
}

// show returns id's view (tail lines), reading a terminal tail from its
// validated document outside the locks.
func (ts *taskService) show(id string, lines int) (contract.TaskView, error) {
	ts.mu.Lock()
	e := ts.tasks[id]
	if e == nil {
		ts.mu.Unlock()
		return contract.TaskView{}, errf(contract.CodeNotFound, "task %s does not exist", id)
	}
	v := ts.viewLocked(e, lines, ts.roles.load().visible, ts.clock.Now())
	file := ts.needsFileTailLocked(e)
	ts.mu.Unlock()
	if file {
		rec, err := ts.st.l.readTaskFile(id, ts.lookup)
		if err != nil {
			return contract.TaskView{}, err
		}
		v.LogTail, v.TailTruncated = contract.LogTail(rec.Log.Data, lines)
		if v.Result != nil {
			v.Result.LogTail = v.LogTail
		}
	}
	return v, nil
}

// logs returns id's complete retained output snapshot.
func (ts *taskService) logs(id string) (contract.TaskLogsResponse, error) {
	ts.mu.Lock()
	e := ts.tasks[id]
	if e == nil {
		ts.mu.Unlock()
		return contract.TaskLogsResponse{}, errf(contract.CodeNotFound, "task %s does not exist", id)
	}
	lg, retained := ts.logOfLocked(e)
	var data []byte
	switch {
	case e.ring != nil && e.mailbox == nil:
		data = e.ring.tail(contract.MaxLogRetainedBytes)
	case lg.Data != nil:
		data = append([]byte(nil), lg.Data...)
	}
	file := data == nil && retained > 0
	rr := e.recoveryReason(removed(ts.roles.load().visible, e.key)) != ""
	ts.mu.Unlock()
	if file {
		rec, err := ts.st.l.readTaskFile(id, ts.lookup)
		if err != nil {
			return contract.TaskLogsResponse{}, err
		}
		data, lg = rec.Log.Data, rec.Log
	}
	if data == nil {
		data = []byte{}
	}
	m := contract.MetaOf(contract.TaskLog{Data: data, SourceBytes: lg.SourceBytes, ReceivedBytes: lg.ReceivedBytes, Incomplete: lg.Incomplete, CounterOverflow: lg.CounterOverflow}, rr)
	return contract.TaskLogsResponse{Version: contract.ProtocolVersion, TaskID: id, Data: data, RetainedBytes: m.RetainedBytes, SourceBytes: m.SourceBytes,
		DroppedBytes: m.DroppedBytes, Truncated: m.Truncated, Incomplete: m.Incomplete, CounterOverflow: m.CounterOverflow, LogMayBeIncomplete: rr}, nil
}

// list returns up to limit summaries after the exclusive cursor, ordered
// by task ID, with next_after when more rows exist in this snapshot.
func (ts *taskService) list(after string, limit int) ([]contract.TaskSummary, *string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	doc, now := ts.roles.load().visible, ts.clock.Now()
	i := sort.SearchStrings(ts.ids, after)
	if i < len(ts.ids) && ts.ids[i] == after {
		i++
	}
	var out []contract.TaskSummary
	for ; i < len(ts.ids) && len(out) < limit; i++ {
		e := ts.tasks[ts.ids[i]]
		v := ts.viewLocked(e, 0, doc, now)
		out = append(out, contract.TaskSummary{TaskID: v.TaskID, Target: e.rec.Request.Target, Role: v.Role, State: v.State, CreatedAt: v.CreatedAt,
			StartedAt: v.StartedAt, FinishedAt: v.FinishedAt, ElapsedMS: v.ElapsedMS, Effective: v.Effective, RequestedBy: e.rec.Request.RequestedBy,
			RecoveryRequired: v.RecoveryRequired, RecoveryReason: v.RecoveryReason, CompletionPending: v.CompletionPending,
			PersistenceReason: v.PersistenceReason, DurabilityConfirmed: v.DurabilityConfirmed, Reason: v.Reason})
	}
	if i < len(ts.ids) && len(out) > 0 {
		next := out[len(out)-1].TaskID
		return out, &next
	}
	return out, nil
}

// removalCheckLocked is role rm's reservation predicate for instance k:
// none held, or every held reservation recovery-required, allows removal.
func (ts *taskService) removalCheck(k instanceKey, force bool) error {
	ts.mu.Lock()
	c := ts.heldLocked(k)
	ts.mu.Unlock()
	if c.held == 0 || c.recovery == c.held {
		return nil
	}
	details := map[string]any{"role_id": k.id, "inflight": c.held, "recovery_inflight": c.recovery}
	if force {
		details["reason"] = contract.ReasonForceNotSupported
		return &contract.Error{Code: contract.CodeConflict, Details: details,
			Message: "role " + k.id + " has tasks in flight; --force cannot cancel them in this build (cancellation arrives in iteration 06); nothing was removed"}
	}
	details["reason"] = contract.ReasonTasksInflight
	return &contract.Error{Code: contract.CodeConflict, Details: details,
		Message: "role " + k.id + " has tasks in flight; wait for them to finish (see callsheet task ls); nothing was removed"}
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
