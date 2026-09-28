package sidecar

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Task execution on the worker (iterations 05 and 06a). Task workers
// belong to Run, not to a stream session: they survive a reconnect, and
// since 06a they keep a stable execution identity across attachments (a
// later attachment reconciles them and may carry their output and
// result). Each execution is recorded in the durable task journal before
// anything is spawned, its process group is led by an internal guardian,
// and its frozen outcome is an outbox kept until the plane's committed
// acknowledgement. No filesystem call, stdin write, probe or Wait runs in
// the stream owner.
//
// Lock order: the supervisor's lock (s.mu) is never held while taking a
// worker's lock; a worker's lock may be held while taking the supervisor's
// (worker, then supervisor). Journal writes are per-worker serialized
// outside both.
const (
	// prepBudget bounds prelaunch preparation from the start's receipt:
	// the durable directory, prepared journal, guardian spawn, owner record
	// and FIFO, running authorization and release barriers (raised from
	// 2 s to 10 s in 06a; a conservative filesystem-progress budget, not a
	// measured hosted-macOS claim).
	prepBudget = 10 * time.Second
	// outputExchange bounds one task_log or task_result receipt exchange
	// from wire-slot reservation through write and acknowledgement.
	outputExchange = 4 * time.Second
	// inventoryExchange bounds one task_inventory exchange.
	inventoryExchange = 4 * time.Second
	// drainBound bounds inherited-pipe draining after the guardian (the
	// group's anchor) was reaped; lingering handles are then closed and the
	// log is incomplete.
	drainBound = time.Second
	// resultRetry is the minimum spacing of an uncommitted result's
	// retransmissions.
	resultRetry = time.Second
	// journalRetry is the retry interval of a failed journal publication.
	journalRetry = time.Second
	// promptFormat is the stdin envelope's format.
	promptFormat = "callsheet-task-v1"
)

// Local lost reasons (sidecar diagnostics only; the wire result carries
// outcome lost without a reason).
const (
	lostSidecarRestarted = "sidecar_restarted"
	lostSidecarStopped   = "sidecar_stopped"
	lostStopped          = "stop_lost"
	lostUnobserved       = "outcome_unobserved"
)

// instanceKey identifies a role instance: a reused role ID with a new
// registration order is another instance and inherits no activity.
type instanceKey struct {
	id    string
	order int
}

func keyOf(r contract.RoleRecord) instanceKey {
	return instanceKey{id: r.ID, order: r.RegistrationOrder}
}

// startPhase is a task start's authorization state.
type startPhase int

const (
	phasePreparing  startPhase = iota
	phaseAuthorized            // running authorization recorded, launch not yet confirmed
	phaseRefused               // definitely no adapter
	phaseStarted               // an adapter was started
)

// attachTag identifies one stream session (attachment) of this Run.
type attachTag struct{ n int }

// taskWorker is one stable execution's Run-lifetime state. The stream
// session reads and changes the transmission fields under mu; the worker
// goroutine owns the guardian, the adapter's pipes and the extractor.
type taskWorker struct {
	sup      *taskSupervisor
	start    contract.TaskStartBody
	digest   string
	key      instanceKey
	deadline time.Time
	ring     *outputRing
	exitedCh chan struct{}
	stopCh   chan struct{}
	commitCh chan struct{}
	// recovered: loaded from its journal at Run start (a previous Run's
	// execution); owner is its guardian's record, if any.
	recovered bool
	owner     *contract.OwnerRecord

	stopOnce, commitOnce sync.Once

	mu      sync.Mutex
	phase   startPhase
	expired bool
	refusal *contract.Error
	pgid    int
	started *time.Time
	// tag is the attachment that may transmit this execution's output and
	// result (nil: none); replied: its start reply ok was written there,
	// or it was reconciled there; late: its output is a replayed tail
	// tagged with its outcome digest (reconciled send_result/stop_lost).
	tag     *attachTag
	replied bool
	late    bool
	// outcome is the frozen, journaled result (the outbox).
	outcome *contract.TaskResultBody
	// waitAck: a result exchange is outstanding; nextSend: the earliest
	// retransmission after an uncommitted acknowledgement.
	waitAck   bool
	nextSend  time.Time
	committed bool
	// stopReq: a stop was requested (reconciliation's stop_lost or Run
	// shutdown); cleaning: recovery cleanup of a previous Run's group is
	// pending; cleanupOK: the group was proved gone (or never existed);
	// cleanupFailed: its absence could not be proved (node blocked).
	stopReq       bool
	cleaning      bool
	cleanupOK     bool
	cleanupFailed bool
	// journalFailing: a journal publication failed and is retried;
	// pendingLost is a recovered execution's lost outcome awaiting it.
	journalFailing bool
	pendingLost    *contract.TaskResultBody
	// done: the worker goroutine returned; forgotten: the journal is gone
	// (or was never kept), so nothing can be sent for it any more.
	done      bool
	forgotten bool
	// unconfirmed: an attachment ended before the result's receipt.
	unconfirmed bool
}

func (w *taskWorker) id() string { return w.start.TaskID }

// requestStop latches a stop request: the guardian cleans the group up.
// Until the group is proved gone an active execution's unresolved cleanup
// blocks every node start (a removed and re-added role cannot bypass it).
func (w *taskWorker) requestStop() {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopReq = true
		active := w.outcome == nil && !w.recovered && w.phase != phaseRefused && !w.forgotten
		if active {
			w.cleaning = true
		}
		w.mu.Unlock()
		if active {
			w.sup.refreshBlocker(w)
		}
		close(w.stopCh)
	})
}

// markCommitted records the committed acknowledgement (or forget): the
// outbox may be deleted once the group's cleanup is confirmed.
func (w *taskWorker) markCommitted() {
	w.commitOnce.Do(func() {
		w.mu.Lock()
		w.committed = true
		w.mu.Unlock()
		close(w.commitCh)
	})
}

// taskSupervisor owns every task worker of one Run, the local occupied
// slot counts reported in heartbeats and the task journal.
type taskSupervisor struct {
	d         *deps
	env       roleEnv
	plat      taskPlatform
	platErr   error
	logger    *slog.Logger
	guardians guardianFactory
	groups    groupWatcher
	send      commandSender
	environ   func() []string
	tmpRoot   func() string
	exe       func() (string, error)
	jr        journal
	runID     string
	// ownGroup is the sidecar's own process group: a guardian's group must
	// differ from it.
	ownGroup int

	mu       sync.Mutex
	workers  map[*taskWorker]bool
	occupied map[instanceKey]int
	blocked  map[instanceKey]bool
	// blockers are the workers that block every node start: unresolved
	// cleanup or a failing journal (iteration 06a).
	blockers map[*taskWorker]bool
	closing  bool
	stopAll  chan struct{}
	wg       sync.WaitGroup
	// janitor wakes the outbox deletion loop; replay the recovered tail
	// loader.
	janitor chan struct{}
	replay  chan struct{}
	// lookup is the adapter metadata journals are validated with.
	lookup contract.AdapterLookup
	// gcMu guards the active-collection count and the IDs collected while
	// another collection was active (see gc).
	gcMu      sync.Mutex
	gcActive  int
	gcDeleted []string
	// notify is the coalesced work-availability signal to the session: it
	// carries no data.
	notify chan struct{}
}

func (d *deps) newSupervisor(env roleEnv, goos string, logger *slog.Logger, l layout) *taskSupervisor {
	s := &taskSupervisor{d: d, env: env, logger: logger, guardians: d.taskGuardians, groups: d.taskGroups, send: d.taskCommand,
		environ: d.taskEnviron, tmpRoot: d.taskTempDir, exe: d.guardianExe, jr: journal{l: l, d: d},
		workers: map[*taskWorker]bool{}, occupied: map[instanceKey]int{}, blocked: map[instanceKey]bool{}, blockers: map[*taskWorker]bool{},
		stopAll: make(chan struct{}), janitor: make(chan struct{}, 1), replay: make(chan struct{}, 1), notify: make(chan struct{}, 1)}
	s.plat, s.platErr = taskPlatformFor(goos)
	if s.guardians == nil {
		s.guardians = newExecGuardian
	}
	if s.groups == nil {
		s.groups = realGroups{}
	}
	if s.send == nil {
		s.send = sendCommand
	}
	if s.environ == nil {
		s.environ = os.Environ
	}
	if s.tmpRoot == nil {
		s.tmpRoot = os.TempDir
	}
	if s.exe == nil {
		s.exe = os.Executable
	}
	s.ownGroup = d.ownGroup()
	var b [16]byte
	io.ReadFull(d.rand, b[:])
	s.runID = hex.EncodeToString(b[:])
	return s
}

func (s *taskSupervisor) signal() { notify(s.notify) }

func (s *taskSupervisor) event(kind eventKind, w *taskWorker) {
	s.d.emit(event{kind: kind, id: w.id()})
}

// local reports an instance's occupied slots and whether it, or the whole
// node, is locally nonaccepting (an earlier child's group cleanup or a
// journal publication is unresolved).
func (s *taskSupervisor) local(k instanceKey) (int, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.occupied[k], s.blocked[k] || len(s.blockers) > 0
}

// setBlocker adds or removes w from the node-wide blockers.
func (s *taskSupervisor) setBlocker(w *taskWorker, on bool) {
	s.mu.Lock()
	if on {
		s.blockers[w] = true
	} else {
		delete(s.blockers, w)
	}
	s.mu.Unlock()
	s.signal()
}

// nodeBlocked reports whether any worker blocks node starts.
func (s *taskSupervisor) nodeBlocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.blockers) > 0
}

// reserve atomically occupies one local slot of k below concurrency; an
// instance or node with unresolved cleanup accepts nothing.
func (s *taskSupervisor) reserve(k instanceKey, concurrency int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.blocked[k] || len(s.blockers) > 0 || s.occupied[k] >= concurrency {
		return false
	}
	s.occupied[k]++
	return true
}

func (s *taskSupervisor) release(k instanceKey) {
	s.mu.Lock()
	if s.occupied[k]--; s.occupied[k] <= 0 {
		delete(s.occupied, k)
	}
	s.mu.Unlock()
	s.signal()
}

// refuse is a definitive start refusal with a fixed safe reason.
func refuse(reason string) *contract.Error {
	code := contract.CodeUnavailable
	msg := "the worker refused the task start: " + strings.ReplaceAll(reason, "_", " ")
	switch reason {
	case contract.ReasonManualUnreadable, contract.ReasonManualNotRegular, contract.ReasonManualTooLarge, contract.ReasonManualInvalidText:
		code = contract.CodeInvalidArgument
	case contract.ReasonStartFailed:
		code = contract.CodeInternal
	}
	return contract.TaskError(code, "", reason, "%s", msg)
}

// known reports whether task id has a worker (live, recovered or awaiting
// its commit) in this Run.
func (s *taskSupervisor) known(id string) bool {
	return s.find(id) != nil
}

// find returns the worker of task id (nil when forgotten).
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

// begin registers a new worker for a start that passed the session's
// checks and reserved its local slot, bound to tag, and launches its
// preparation.
func (s *taskSupervisor) begin(start contract.TaskStartBody, tag *attachTag, deadline time.Time) *taskWorker {
	ringCap := contract.MaxLogRetainedBytes
	if s.d.taskRingCap > 0 {
		ringCap = s.d.taskRingCap
	}
	w := &taskWorker{sup: s, start: start, digest: start.StartDigestHex(), key: keyOf(start.Role), tag: tag, deadline: deadline,
		ring: newOutputRing(ringCap), exitedCh: make(chan struct{}), stopCh: make(chan struct{}), commitCh: make(chan struct{})}
	s.mu.Lock()
	s.workers[w] = true
	s.mu.Unlock()
	s.wg.Add(1)
	go s.work(w)
	return w
}

// work is a worker goroutine: run, then its completion (done, a wake-up
// and collection, last).
func (s *taskSupervisor) work(w *taskWorker) {
	defer s.wg.Done()
	s.run(w)
	s.complete(w)
}

// complete marks w's goroutine finished and collects what can go.
func (s *taskSupervisor) complete(w *taskWorker) {
	w.mu.Lock()
	w.done = true
	w.mu.Unlock()
	s.signal()
	s.kickJanitor()
	s.gc()
}

// journalOf is w's journal document in phase.
func (s *taskSupervisor) journalOf(w *taskWorker, phase string, nonce *string) contract.ExecutionJournal {
	return contract.ExecutionJournal{TaskID: w.id(), Execution: w.start.Execution, StartDigest: w.digest, Role: w.start.Role,
		Effective: w.start.Effective, TimeoutPolicy: contract.TimeoutPolicyLegacy, OwnerNonce: nonce, Phase: phase}
}

// newNonce returns 32 random bytes in lowercase hex.
func (s *taskSupervisor) newNonce() (string, error) {
	var b [32]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// waitStatus waits for w's guardian's next status message, bounded by
// the preparation deadline when bounded, and by Run shutdown.
func (s *taskSupervisor) waitStatus(w *taskWorker, g guardianProc, bounded bool) (contract.GuardianStatus, bool, bool) {
	var timer <-chan time.Time
	if bounded {
		c, stop := s.d.clock.NewTimerAt(w.deadline)
		defer stop()
		timer = c
	}
	select {
	case m, ok := <-g.Status():
		return m, ok, false
	case <-timer:
		// A message that arrived with the deadline still wins.
		select {
		case m, ok := <-g.Status():
			return m, ok, false
		default:
		}
		return contract.GuardianStatus{}, false, true
	case <-s.stopAll:
		return contract.GuardianStatus{}, false, true
	case <-w.stopCh:
		return contract.GuardianStatus{}, false, true
	}
}

// run prepares, records, spawns, authorizes and releases w's execution,
// then supervises it.
func (s *taskSupervisor) run(w *taskWorker) {
	s.event(evTaskPreparing, w)
	inv, a, exe, reason := s.prepare(w)
	if reason != "" {
		s.refuseStart(w, reason, nil, "", false)
		return
	}
	scratch, err := s.plat.scratch(s.tmpRoot(), w.id())
	if err != nil {
		s.refuseStart(w, contract.ReasonScratchUnavailable, nil, "", false)
		return
	}
	// Barrier 1: the durable task directory and prepared journal, before
	// anything is spawned.
	if err := s.jr.create(s.journalOf(w, contract.JournalPrepared, nil)); err != nil {
		s.logger.Error("task journal unavailable", "task_id", w.id(), "error", err)
		s.refuseStart(w, contract.ReasonStartFailed, nil, scratch, true)
		return
	}
	nonce, err := s.newNonce()
	if err != nil {
		s.refuseStart(w, contract.ReasonStartFailed, nil, scratch, true)
		return
	}
	self, err := s.exe()
	if err != nil {
		s.refuseStart(w, contract.ReasonStartFailed, nil, scratch, true)
		return
	}
	p, err := newPipes()
	if err != nil {
		s.refuseStart(w, contract.ReasonStartFailed, nil, scratch, true)
		return
	}
	spec := guardianSpec{exe: self, inv: contract.GuardianInvocation{Version: contract.GuardianInvocationVersion, TaskID: w.id(),
		Execution: w.start.Execution, StartDigest: w.digest, TaskDir: s.jr.l.path(taskDirRel(w.id())), Nonce: nonce, Path: exe,
		Argv: inv.Argv, Env: childEnv(s.environ(), scratch), Dir: scratch},
		proc: procSpec{path: exe, argv: inv.Argv, env: childEnv(s.environ(), scratch), dir: scratch, stdin: p.stdinR, stdout: p.stdoutW, stderr: p.stderrW}}
	g := s.guardians(spec)
	// Barrier 2: the guardian in its own new group; it cannot start an
	// adapter before the release byte.
	if err := g.Start(); err != nil {
		p.closeParent()
		s.refuseStart(w, contract.ReasonStartFailed, nil, scratch, true)
		return
	}
	pgid := g.PID()
	w.mu.Lock()
	w.pgid = pgid
	w.mu.Unlock()
	abandon := func(reason string) {
		// Never released, so no adapter can exist: revoke, reap the
		// guardian, confirm its group is gone, then refuse definitely.
		g.Revoke()
		g.Wait()
		if err := s.groups.gone(pgid); err != nil {
			s.logger.Error("task process group cleanup unconfirmed", "task_id", w.id(), "role_id", w.key.id, "reason", "cleanup_unconfirmed")
		}
		p.closeParent()
		s.refuseStart(w, reason, nil, scratch, true)
	}
	// Barrier 3: durable ownership (owner.json armed, the FIFO) and ready.
	m, ok, timedOut := s.waitStatus(w, g, true)
	if timedOut {
		abandon(contract.ReasonPreparationTimeout)
		return
	}
	if !ok || m.Type != contract.GuardianReady || m.PID != pgid || m.PGID != pgid || pgid <= 1 || pgid == s.ownGroup {
		abandon(contract.ReasonStartFailed)
		return
	}
	// Barrier 4: the running authorization, decided under the lock that
	// publishes it; a timer can never later reject an authorized start.
	w.mu.Lock()
	if s.d.taskAuthHook != nil {
		s.d.taskAuthHook(w.id())
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if w.expired || closing || !s.d.clock.Now().Before(w.deadline) {
		w.mu.Unlock()
		abandon(contract.ReasonPreparationTimeout)
		return
	}
	w.phase = phaseAuthorized
	w.mu.Unlock()
	s.event(evTaskAuthorized, w)
	if err := s.jr.write(s.journalOf(w, contract.JournalRunning, &nonce)); err != nil {
		// Not released: no adapter can exist.
		s.logger.Error("task journal unavailable", "task_id", w.id(), "error", err)
		abandon(contract.ReasonStartFailed)
		return
	}
	g.Release()
	// Barrier 5: the adapter's launch.
	m, ok, stopped := s.waitStatus(w, g, false)
	if stopped {
		// Run shutdown while the launch is in progress: the guardian is
		// asked to clean up, and its next message (or EOF) decides.
		g.Stop()
		m, ok = <-g.Status()
	}
	switch {
	case ok && m.Type == contract.GuardianStarted:
		t, _ := contract.ParseTime(*m.StartedAt)
		w.mu.Lock()
		w.phase, w.started = phaseStarted, &t
		w.mu.Unlock()
	case ok && m.Type == contract.GuardianError:
		// The guardian proves no adapter started: a definite refusal after
		// its group is gone.
		g.Wait()
		if err := s.groups.gone(pgid); err != nil {
			s.logger.Error("task process group cleanup unconfirmed", "task_id", w.id(), "role_id", w.key.id, "reason", "cleanup_unconfirmed")
		}
		p.closeParent()
		s.refuseStart(w, contract.ReasonStartFailed, nil, scratch, true)
		return
	default:
		// The guardian ended without confirming a launch: an adapter may
		// have run. Never a definite refusal: the outcome is lost.
		s.supervise(w, g, a, inv, p, scratch, nil, true)
		return
	}
	s.event(evChildStarted, w)
	s.signal()
	s.supervise(w, g, a, inv, p, scratch, g.Status(), false)
}

// prepare reads both manuals completely, composes the prompt and resolves
// the enabled executable and invocation. A nonempty reason is a refusal.
func (s *taskSupervisor) prepare(w *taskWorker) (adapter.Invocation, adapter.Adapter, string, string) {
	if s.platErr != nil {
		return adapter.Invocation{}, nil, "", contract.ReasonStartFailed
	}
	role := w.start.Role
	ins, reason := s.readManual(role.Instruction)
	if reason != "" {
		return adapter.Invocation{}, nil, "", reason
	}
	run, reason := s.readManual(role.Runbook)
	if reason != "" {
		return adapter.Invocation{}, nil, "", reason
	}
	prompt, err := composePrompt(ins, run, w.start, contract.MaxPromptBytes)
	if err != nil {
		return adapter.Invocation{}, nil, "", contract.ReasonManualTooLarge
	}
	a, ok := s.env.adapters.Lookup(role.Adapter)
	exe, enabled := s.env.executables[role.Adapter]
	if !ok || !enabled {
		return adapter.Invocation{}, nil, "", contract.ReasonAdapterDisabled
	}
	if fi, err := os.Stat(exe); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return adapter.Invocation{}, nil, "", contract.ReasonExecutableUnavailable
	}
	inv, err := a.Invocation(adapter.TaskInput{TaskID: w.id(), Model: w.start.Effective.Model, Effort: w.start.Effective.Effort, Prompt: prompt})
	if err != nil {
		return adapter.Invocation{}, nil, "", contract.ReasonStartFailed
	}
	return inv, a, exe, ""
}

// refuseStart releases w's local slot (and any prepared pipes, scratch
// directory and journal) before publishing the refusal: definitely no
// adapter ran.
func (s *taskSupervisor) refuseStart(w *taskWorker, reason string, p *pipes, scratch string, journaled bool) {
	if p != nil {
		p.closeAll()
	}
	if scratch != "" {
		removeScratch(scratch)
	}
	if journaled {
		s.removeJournal(w)
	}
	s.release(w.key)
	w.mu.Lock()
	if w.phase != phaseRefused {
		w.phase, w.refusal = phaseRefused, refuse(reason)
	}
	w.forgotten, w.cleaning = true, false
	w.mu.Unlock()
	s.refreshBlocker(w)
	s.event(evTaskRefused, w)
	s.signal()
}

// removeJournal deletes w's journal, retrying every journalRetry until it
// succeeds or Run shuts down (a failing deletion blocks node starts).
func (s *taskSupervisor) removeJournal(w *taskWorker) bool {
	for {
		err := s.jr.remove(w.id())
		if err == nil {
			w.mu.Lock()
			failing := w.journalFailing
			w.journalFailing = false
			w.mu.Unlock()
			if failing {
				s.refreshBlocker(w)
			}
			return true
		}
		s.logger.Error("task journal deletion failed; retrying", "task_id", w.id(), "error", err)
		w.mu.Lock()
		w.journalFailing = true
		w.mu.Unlock()
		s.refreshBlocker(w)
		if !s.pause(journalRetry) {
			return false
		}
	}
}

// pause waits d on the injected clock; false when Run shuts down first.
func (s *taskSupervisor) pause(d time.Duration) bool {
	c, stop := s.d.clock.NewTimer(d)
	defer stop()
	select {
	case <-c:
		return true
	case <-s.stopAll:
		return false
	}
}

// refreshBlocker recomputes whether w blocks node starts.
func (s *taskSupervisor) refreshBlocker(w *taskWorker) {
	w.mu.Lock()
	on := w.cleaning || w.cleanupFailed || w.journalFailing
	w.mu.Unlock()
	s.setBlocker(w, on)
}

// readManual opens a manual with the nonblocking regular-file pattern and
// reads it completely, bounded by MaxManualBytes plus a sentinel byte.
func (s *taskSupervisor) readManual(p string) ([]byte, string) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, contract.ReasonManualUnreadable
	}
	if !fi.Mode().IsRegular() {
		return nil, contract.ReasonManualNotRegular
	}
	f, err := s.d.openManual(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.ELOOP) {
			return nil, contract.ReasonManualUnreadable
		}
		return nil, contract.ReasonManualUnreadable
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, contract.ReasonManualNotRegular
	}
	b, err := io.ReadAll(io.LimitReader(f, contract.MaxManualBytes+1))
	switch {
	case err != nil:
		return nil, contract.ReasonManualUnreadable
	case len(b) > contract.MaxManualBytes:
		return nil, contract.ReasonManualTooLarge
	case !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0:
		return nil, contract.ReasonManualInvalidText
	}
	return b, ""
}

// promptRole is the role in the task envelope.
type promptRole struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type promptTask struct {
	TaskID          string                 `json:"task_id"`
	Target          contract.TaskTarget    `json:"target"`
	Role            promptRole             `json:"role"`
	Goal            string                 `json:"goal"`
	Payload         []string               `json:"payload"`
	Acceptance      string                 `json:"acceptance"`
	Effective       contract.TaskEffective `json:"effective"`
	TimeoutEnforced bool                   `json:"timeout_enforced"`
	RequestedBy     contract.RequestedBy   `json:"requested_by"`
}

type promptEnvelope struct {
	Format      string     `json:"format"`
	Instruction string     `json:"instruction"`
	Runbook     string     `json:"runbook"`
	Task        promptTask `json:"task"`
}

// errPromptTooLarge is a prompt over the cap.
var errPromptTooLarge = errors.New("the encoded prompt exceeds its cap")

// composePrompt renders the stdin prompt: one UTF-8 JSON object in field
// order format, instruction, runbook, task, followed by LF. The encoder
// quotes manual bytes unambiguously; acceptance stays opaque. Encoded
// size above limit fails before launch.
func composePrompt(instruction, runbook []byte, st contract.TaskStartBody, limit int) ([]byte, error) {
	// Each encoded string is at least its raw bytes plus two quotes, so
	// manuals already over the limit are refused before any copy.
	if len(instruction)+len(runbook) > limit {
		return nil, errPromptTooLarge
	}
	payload := st.Request.Payload
	if payload == nil {
		payload = []string{}
	}
	env := promptEnvelope{Format: promptFormat, Instruction: string(instruction), Runbook: string(runbook), Task: promptTask{
		TaskID: st.TaskID, Target: st.Request.Target, Role: promptRole{ID: st.Role.ID, Name: st.Role.Name}, Goal: st.Request.Goal,
		Payload: payload, Acceptance: st.Request.Acceptance, Effective: st.Effective, RequestedBy: st.Request.RequestedBy}}
	var b bytes.Buffer
	b.Grow(len(instruction) + len(runbook) + 4096)
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		return nil, err
	}
	if b.Len() > limit {
		return nil, errPromptTooLarge
	}
	return b.Bytes(), nil
}

// supervise runs a released execution to its frozen outcome: stdin
// delivery, both pipe drains, the adapter's forwarded wait status, the
// guardian's reap (after its own whole-group cleanup), the bounded
// inherited-pipe drain, the group's verified disappearance, scratch
// removal, then the journaled outcome. A stop request or Run shutdown
// closes the guardian's parent-lifetime pipe. The adapter's wait status
// decides a natural outcome; an interrupted or unobserved execution is
// lost. Nothing here retries an execution.
func (s *taskSupervisor) supervise(w *taskWorker, g guardianProc, a adapter.Adapter, inv adapter.Invocation, p *pipes, scratch string,
	status <-chan contract.GuardianStatus, unobserved bool) {
	ext := a.NewFinalExtractor()
	var copier, drains sync.WaitGroup
	if !unobserved {
		copier.Add(1)
		go func() {
			defer copier.Done()
			p.stdinW.Write(inv.Stdin)
			p.stdinW.Close()
		}()
	}
	drains.Add(2)
	go func() { defer drains.Done(); drain(p.stdoutR, w.ring, ext.Feed, s.signal) }()
	go func() { defer drains.Done(); drain(p.stderrR, w.ring, nil, s.signal) }()
	var exit *procExit
	stopCh, stopAll := w.stopCh, s.stopAll
	for status != nil {
		select {
		case m, ok := <-status:
			if !ok {
				status = nil
				break
			}
			if m.Type == contract.GuardianExit {
				ex := procExit{}
				if m.Signal != nil {
					ex.signal = *m.Signal
				} else {
					ex.code = *m.ExitCode
				}
				exit = &ex
				status = nil
			}
		case <-stopCh:
			stopCh = nil
			g.Stop()
		case <-stopAll:
			stopAll = nil
			g.Stop()
		}
	}
	close(w.exitedCh)
	s.event(evChildExited, w)
	g.Wait()
	drained := make(chan struct{})
	go func() { drains.Wait(); close(drained) }()
	timer, stop := s.d.clock.NewTimerAt(s.d.clock.Now().Add(drainBound))
	select {
	case <-drained:
	case <-timer:
		// The bound fired: a drain that completed before it still wins;
		// otherwise an escaped handle holds the pipes and they close.
		select {
		case <-drained:
		default:
			w.ring.markIncomplete()
			p.stdoutR.Close()
			p.stderrR.Close()
			<-drained
		}
	}
	stop()
	p.stdoutR.Close()
	p.stderrR.Close()
	p.stdinW.Close()
	copier.Wait()
	w.mu.Lock()
	pgid := w.pgid
	w.mu.Unlock()
	cleanupErr := s.groups.gone(pgid)
	if cleanupErr != nil {
		s.logger.Error("task process group cleanup unconfirmed", "task_id", w.id(), "role_id", w.key.id, "reason", "cleanup_unconfirmed")
		w.mu.Lock()
		w.cleanupFailed, w.cleaning = true, false
		w.mu.Unlock()
		s.mu.Lock()
		s.blocked[w.key] = true
		s.mu.Unlock()
		s.refreshBlocker(w)
	} else {
		w.mu.Lock()
		w.cleanupOK, w.cleaning = true, false
		w.mu.Unlock()
		s.refreshBlocker(w)
		if scratch != "" {
			if err := removeScratch(scratch); err != nil {
				s.logger.Warn("task scratch cleanup failed", "task_id", w.id(), "path", scratch)
			}
		}
	}
	fm := ext.Finish()
	end, incomplete, overflow := w.ring.totals()
	res := contract.TaskResultBody{TaskID: w.id(), Execution: w.start.Execution, Outcome: contract.OutcomeNatural, FinalMessage: fm.Message,
		FinalMessageTruncated: fm.Truncated, OutputBytes: end, LogIncomplete: incomplete, CounterOverflow: overflow}
	switch {
	case exit == nil:
	case exit.signal != "":
		sig := exit.signal
		res.Signal = &sig
	default:
		code := exit.code
		res.ExitCode = &code
	}
	if res.FinalMessage == nil {
		res.FinalMessageTruncated = false
	}
	w.mu.Lock()
	interrupted := w.stopReq
	w.mu.Unlock()
	s.mu.Lock()
	interrupted = interrupted || s.closing
	s.mu.Unlock()
	if exit == nil || interrupted {
		// Interrupted (a stop, Run shutdown) or unobserved: the outcome is
		// unconfirmed. An observed wait status is kept, never invented.
		res.Outcome = contract.OutcomeLost
		why := lostStopped
		switch {
		case exit == nil:
			why = lostUnobserved
		case s.isClosing():
			why = lostSidecarStopped
		}
		s.logger.Warn("task execution lost", "task_id", w.id(), "reason", why)
	}
	if cleanupErr == nil {
		// Release after the group's verified disappearance, before the
		// outcome is offered (a heartbeat after the result never counts
		// the slot); an unconfirmed cleanup retains the slot and keeps the
		// role locally blocked. While the outcome's journal publication
		// fails the node stays nonaccepting (freeze's blocker).
		s.release(w.key)
	}
	s.freeze(w, res.Sealed())
	s.event(evTaskExited, w)
	s.signal()
}

func (s *taskSupervisor) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// freeze journals w's frozen outcome with the unsent retained tail
// (completed or lost), retrying every journalRetry while the journal
// fails, then offers it (the outbox): a result is never offered before it
// is durable.
func (s *taskSupervisor) freeze(w *taskWorker, res contract.TaskResultBody) {
	phase := contract.JournalCompleted
	if res.Outcome == contract.OutcomeLost {
		phase = contract.JournalLost
	}
	w.mu.Lock()
	started := w.started
	w.mu.Unlock()
	for {
		j := s.journalOf(w, phase, nil)
		j.StartedAt, j.Result, j.Log = started, &res, w.ring.journalLog()
		err := s.jr.write(j)
		if err == nil {
			break
		}
		s.logger.Error("task journal publication failed; retrying", "task_id", w.id(), "error", err)
		w.mu.Lock()
		w.journalFailing = true
		w.mu.Unlock()
		s.refreshBlocker(w)
		if !s.pause(journalRetry) {
			// Run shutdown: the outcome was never journaled, so it is never
			// offered; the next Run reports the execution lost.
			return
		}
	}
	w.mu.Lock()
	failing := w.journalFailing
	w.journalFailing = false
	w.outcome = &res
	w.mu.Unlock()
	if failing {
		s.refreshBlocker(w)
	}
	s.signal()
}

// kickJanitor wakes the outbox deletion loop.
func (s *taskSupervisor) kickJanitor() { notify(s.janitor) }

// janitorLoop deletes the journal of every execution whose outcome was
// committed (or forgotten by reconciliation) and whose group's cleanup is
// confirmed, one at a time, until Run shuts down.
func (s *taskSupervisor) janitorLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.janitor:
		case <-s.stopAll:
			return
		}
		for _, w := range s.snapshot() {
			w.mu.Lock()
			ready := w.committed && w.cleanupOK && !w.forgotten && (w.recovered || w.done)
			w.mu.Unlock()
			if !ready {
				continue
			}
			if !s.removeJournal(w) {
				return
			}
			w.mu.Lock()
			w.forgotten = true
			w.mu.Unlock()
			s.event(evTaskForgotten, w)
		}
		s.gc()
	}
}

// ids returns the task IDs of every execution this Run still holds (an
// attachment's frozen inventory list), sorted.
func (s *taskSupervisor) ids() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, w := range s.snapshot() {
		w.mu.Lock()
		keep := !w.forgotten && w.phase != phaseRefused
		w.mu.Unlock()
		if keep {
			out = append(out, w.id())
		}
	}
	return out
}

// snapshot returns the workers, sorted by task ID.
func (s *taskSupervisor) snapshot() []*taskWorker {
	s.mu.Lock()
	out := make([]*taskWorker, 0, len(s.workers))
	for w := range s.workers {
		out = append(out, w)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].id() < out[j].id() })
	return out
}

// gc forgets workers whose goroutine returned and whose journal is gone
// (refused, or committed and deleted).
//
// Lock order is a worker's lock before the supervisor's, never the
// reverse: collection snapshots the workers under s.mu, inspects each
// under its own lock with s.mu released, and deletes the collectable ones
// afterwards (a collectable worker never becomes live again).
//
// Concurrent collections each hold a snapshot of worker pointers, so a
// worker deleted by one may still be referenced by another's snapshot.
// A collection is therefore reported (evTaskCollected) only when no
// collection that could have snapshotted the worker is still active: the
// dedicated gcMu counts active collections (every snapshot is taken after
// its collection registered) and holds the IDs deleted meanwhile; the
// last collection to finish publishes them. gcMu is never held while
// taking s.mu or a worker's lock, and gc is never called with either held.
func (s *taskSupervisor) gc() {
	s.gcMu.Lock()
	s.gcActive++
	s.gcMu.Unlock()
	ids := s.collect()
	s.gcMu.Lock()
	s.gcDeleted = append(s.gcDeleted, ids...)
	s.gcActive--
	var publish []string
	if s.gcActive == 0 {
		publish, s.gcDeleted = s.gcDeleted, nil
	}
	s.gcMu.Unlock()
	for _, id := range publish {
		s.d.emit(event{kind: evTaskCollected, id: id})
	}
	if len(ids) > 0 {
		s.kickReplay() // a collected replay may free the loader
	}
}

// collect is one collection pass: it deletes the collectable workers and
// returns their IDs (no worker reference outlives it).
func (s *taskSupervisor) collect() []string {
	s.mu.Lock()
	ws := make([]*taskWorker, 0, len(s.workers))
	for w := range s.workers {
		ws = append(ws, w)
	}
	s.mu.Unlock()
	if s.d.taskGCHook != nil {
		s.d.taskGCHook()
	}
	var gone []*taskWorker
	for _, w := range ws {
		w.mu.Lock()
		if (w.done || w.recovered) && w.forgotten {
			gone = append(gone, w)
		}
		w.mu.Unlock()
	}
	var ids []string
	if len(gone) == 0 {
		return nil
	}
	s.mu.Lock()
	for _, w := range gone {
		if s.workers[w] {
			delete(s.workers, w)
			delete(s.blockers, w)
			ids = append(ids, w.id())
		}
	}
	s.mu.Unlock()
	return ids
}

// attached returns the workers bound to tag, sorted by task ID.
func (s *taskSupervisor) attached(tag *attachTag) []*taskWorker {
	var out []*taskWorker
	for _, w := range s.snapshot() {
		w.mu.Lock()
		ok := w.tag == tag
		w.mu.Unlock()
		if ok {
			out = append(out, w)
		}
	}
	return out
}

// fence unbinds every worker of tag when its attachment ends: nothing is
// ever sent for them on it again; the next attachment reconciles them. An
// in-flight chunk is resent there; a result in flight without its
// acknowledgement is resent later (the plane deduplicates by digest).
func (s *taskSupervisor) fence(tag *attachTag) {
	for _, w := range s.attached(tag) {
		w.mu.Lock()
		w.tag, w.replied, w.late = nil, false, false
		if w.waitAck {
			w.waitAck, w.unconfirmed = false, true
			s.logger.Warn("task result receipt unconfirmed", "task_id", w.id(), "reason", "result_receipt_unconfirmed")
		}
		w.mu.Unlock()
		w.ring.unbind()
		s.event(evTaskFenced, w)
	}
	s.gc()
}

// start runs the supervisor's background loops (the janitor and the
// recovered tail replay loader).
func (s *taskSupervisor) startLoops() {
	s.wg.Add(2)
	go s.janitorLoop()
	go s.replayLoop()
}

// shutdown is Run's local supervisor teardown: no new launch is
// authorized, every running execution's guardian is asked to clean its
// group (its parent-lifetime pipe closes), interrupted executions are
// journaled lost (sidecar_stopped) when storage permits, and every
// worker, recovery and janitor loop is joined. It invents no cancel API:
// the plane learns nothing unless a result exchange completes; a later
// Run reports what the journal holds.
func (s *taskSupervisor) shutdown() {
	s.mu.Lock()
	already := s.closing
	s.closing = true
	s.mu.Unlock()
	if !already {
		close(s.stopAll)
	}
	s.d.emit(event{kind: evSupervisorClosing})
	s.wg.Wait()
}
