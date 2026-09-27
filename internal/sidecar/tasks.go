package sidecar

import (
	"bytes"
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

// Task execution on the worker (iteration 05). Task workers belong to
// Run, not to a stream session: they survive a reconnect, drain and reap
// their child, and never send an old attachment's output or result on a
// new one. No filesystem call, stdin write, probe or Wait runs in the
// stream owner.
const (
	// prepBudget bounds prelaunch preparation from the start's receipt.
	prepBudget = 2 * time.Second
	// outputExchange bounds one task_log or task_result receipt exchange
	// from wire-slot reservation through write and acknowledgement.
	outputExchange = 4 * time.Second
	// drainBound bounds inherited-pipe draining after the direct child
	// exits; lingering handles are then closed and the log is incomplete.
	drainBound = time.Second
	// promptFormat is the stdin envelope's format.
	promptFormat = "callsheet-task-v1"
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
	phaseAuthorized            // Start authorized, its result not yet known
	phaseRefused               // definitely no child
	phaseStarted               // a child exists
)

// attachTag identifies one stream session (attachment) of this Run.
type attachTag struct{ n int }

// taskWorker is one task's run-lifetime state. The stream session reads
// and changes the transmission fields under mu; the worker goroutine owns
// the child, its pipes and the extractor.
type taskWorker struct {
	sup      *taskSupervisor
	start    contract.TaskStartBody
	key      instanceKey
	tag      *attachTag
	deadline time.Time
	ring     *outputRing
	exitedCh chan struct{}

	mu      sync.Mutex
	phase   startPhase
	expired bool
	refusal *contract.Error
	pid     int
	// replied: the start reply ok:true was written on tag's attachment,
	// so output and result may flow there.
	replied bool
	// fenced: tag's attachment ended; nothing is ever sent again.
	fenced bool
	// exited: the child was reaped and its pipes drained; result is set.
	exited bool
	result contract.TaskResultBody
	// resultSent: the result exchange began; acked: its receipt was
	// acknowledged; unconfirmed: the attachment ended before the ack.
	resultSent, acked, unconfirmed bool
	// done: the worker goroutine returned.
	done bool
	// termSent: Run shutdown's group termination was started (once).
	termSent bool
}

func (w *taskWorker) id() string { return w.start.TaskID }

// taskSupervisor owns every task worker of one Run and the local
// occupied-slot counts reported in heartbeats.
type taskSupervisor struct {
	d       *deps
	env     roleEnv
	plat    taskPlatform
	platErr error
	logger  *slog.Logger
	procs   procFactory
	groups  groupCleaner
	environ func() []string
	tmpRoot func() string

	mu       sync.Mutex
	workers  map[*taskWorker]bool
	occupied map[instanceKey]int
	blocked  map[instanceKey]bool
	closing  bool
	wg       sync.WaitGroup
	// gcMu guards the active-collection count and the IDs collected while
	// another collection was active (see gc).
	gcMu      sync.Mutex
	gcActive  int
	gcDeleted []string
	// notify is the coalesced work-availability signal to the session: it
	// carries no data.
	notify chan struct{}
}

func (d *deps) newSupervisor(env roleEnv, goos string, logger *slog.Logger) *taskSupervisor {
	s := &taskSupervisor{d: d, env: env, logger: logger, procs: d.taskProcs, groups: d.taskGroups, environ: d.taskEnviron, tmpRoot: d.taskTempDir,
		workers: map[*taskWorker]bool{}, occupied: map[instanceKey]int{}, blocked: map[instanceKey]bool{}, notify: make(chan struct{}, 1)}
	s.plat, s.platErr = taskPlatformFor(goos)
	if s.procs == nil {
		s.procs = newExecProc
	}
	if s.groups == nil {
		s.groups = realGroups{}
	}
	if s.environ == nil {
		s.environ = os.Environ
	}
	if s.tmpRoot == nil {
		s.tmpRoot = os.TempDir
	}
	return s
}

func (s *taskSupervisor) signal() { notify(s.notify) }

func (s *taskSupervisor) event(kind eventKind, w *taskWorker) {
	s.d.emit(event{kind: kind, id: w.id()})
}

// local reports an instance's occupied slots and whether cleanup of an
// earlier child could not be confirmed (locally nonaccepting).
func (s *taskSupervisor) local(k instanceKey) (int, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.occupied[k], s.blocked[k]
}

// reserve atomically occupies one local slot of k below concurrency.
func (s *taskSupervisor) reserve(k instanceKey, concurrency int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.blocked[k] || s.occupied[k] >= concurrency {
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

// begin registers a new worker for a start that passed the session's
// checks and reserved its local slot, and launches its preparation.
func (s *taskSupervisor) begin(start contract.TaskStartBody, tag *attachTag, deadline time.Time) *taskWorker {
	ringCap := contract.MaxLogRetainedBytes
	if s.d.taskRingCap > 0 {
		ringCap = s.d.taskRingCap
	}
	w := &taskWorker{sup: s, start: start, key: keyOf(start.Role), tag: tag, deadline: deadline, ring: newOutputRing(ringCap),
		exitedCh: make(chan struct{})}
	s.mu.Lock()
	s.workers[w] = true
	s.mu.Unlock()
	s.wg.Add(1)
	go s.work(w)
	return w
}

// work is a worker goroutine: run, then its completion (done, a wake-up
// and collection, last). Nothing here refers to w after completion, so a
// collected worker is unreachable once evTaskCollected is observed.
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
	s.gc()
}

// run prepares, authorizes and starts w's child, then supervises it.
func (s *taskSupervisor) run(w *taskWorker) {
	s.event(evTaskPreparing, w)
	inv, a, exe, reason := s.prepare(w)
	if reason != "" {
		s.refuseStart(w, reason, nil, "")
		return
	}
	scratch, err := s.plat.scratch(s.tmpRoot(), w.id())
	if err != nil {
		s.refuseStart(w, contract.ReasonScratchUnavailable, nil, "")
		return
	}
	p, err := newPipes()
	if err != nil {
		s.refuseStart(w, contract.ReasonStartFailed, nil, scratch)
		return
	}
	proc := s.procs(procSpec{path: exe, argv: inv.Argv, env: childEnv(s.environ(), scratch), dir: scratch,
		stdin: p.stdinR, stdout: p.stdoutW, stderr: p.stderrW})
	// The authorization point: the deadline, the session's expiry
	// decision and Run shutdown are checked under the same lock that
	// publishes the launch decision. A timer can never later reject a
	// child whose Start was authorized.
	w.mu.Lock()
	if s.d.taskAuthHook != nil {
		s.d.taskAuthHook(w.id())
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if w.expired || closing || !s.d.clock.Now().Before(w.deadline) {
		w.mu.Unlock()
		s.refuseStart(w, contract.ReasonPreparationTimeout, p, scratch)
		return
	}
	w.phase = phaseAuthorized
	w.mu.Unlock()
	s.event(evTaskAuthorized, w)
	if err := proc.Start(); err != nil {
		s.refuseStart(w, contract.ReasonStartFailed, p, scratch)
		return
	}
	// Publish the started child and, in the same decision, learn whether
	// shutdown began while Start was in progress (its scan may have seen
	// this worker only as authorized): then this goroutine terminates the
	// group itself. termSent makes the termination exactly once.
	pid := proc.PID()
	w.mu.Lock()
	s.mu.Lock()
	closing = s.closing
	s.mu.Unlock()
	w.phase, w.pid = phaseStarted, pid
	term := closing && !w.termSent
	w.termSent = w.termSent || term
	w.mu.Unlock()
	if term {
		s.wg.Add(1) // this worker is counted, so Wait has not returned
		go func() {
			defer s.wg.Done()
			s.groups.terminate(pid, w.exitedCh)
		}()
	}
	s.event(evChildStarted, w)
	s.signal()
	s.supervise(w, proc, a, inv, p, scratch)
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

// refuseStart releases w's local slot (and any prepared pipes and
// scratch directory) before publishing the refusal.
func (s *taskSupervisor) refuseStart(w *taskWorker, reason string, p *pipes, scratch string) {
	if p != nil {
		p.closeAll()
	}
	if scratch != "" {
		removeScratch(scratch)
	}
	s.release(w.key)
	w.mu.Lock()
	if w.phase != phaseRefused {
		w.phase, w.refusal = phaseRefused, refuse(reason)
	}
	w.mu.Unlock()
	s.event(evTaskRefused, w)
	s.signal()
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

// supervise runs a started child to completion: stdin delivery, both
// pipe drains, Wait, the bounded inherited-pipe drain, group cleanup,
// scratch removal and the result. The direct child's exit decides the
// outcome; nothing here retries.
func (s *taskSupervisor) supervise(w *taskWorker, proc taskProc, a adapter.Adapter, inv adapter.Invocation, p *pipes, scratch string) {
	ext := a.NewFinalExtractor()
	var copier, drains sync.WaitGroup
	copier.Add(1)
	go func() {
		defer copier.Done()
		p.stdinW.Write(inv.Stdin)
		p.stdinW.Close()
	}()
	drains.Add(2)
	go func() { defer drains.Done(); drain(p.stdoutR, w.ring, ext.Feed, s.signal) }()
	go func() { defer drains.Done(); drain(p.stderrR, w.ring, nil, s.signal) }()
	exit := proc.Wait()
	close(w.exitedCh)
	s.event(evChildExited, w)
	drained := make(chan struct{})
	go func() { drains.Wait(); close(drained) }()
	timer, stop := s.d.clock.NewTimerAt(s.d.clock.Now().Add(drainBound))
	select {
	case <-drained:
	case <-timer:
		// The bound fired: a drain that completed before it still wins;
		// otherwise descendants hold the pipes and their handles close.
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
	cleanupErr := s.groups.cleanup(w.pid)
	if cleanupErr != nil {
		s.logger.Error("task process group cleanup unconfirmed", "task_id", w.id(), "role_id", w.key.id, "reason", "cleanup_unconfirmed")
		s.mu.Lock()
		s.blocked[w.key] = true
		s.mu.Unlock()
	} else if scratch != "" {
		if err := removeScratch(scratch); err != nil {
			s.logger.Warn("task scratch cleanup failed", "task_id", w.id(), "path", scratch)
		}
	}
	fm := ext.Finish()
	end, incomplete, overflow := w.ring.totals()
	res := contract.TaskResultBody{TaskID: w.id(), Execution: w.start.Execution, FinalMessage: fm.Message, FinalMessageTruncated: fm.Truncated,
		OutputBytes: end, LogIncomplete: incomplete, CounterOverflow: overflow}
	switch {
	case exit.signal != "":
		sig := exit.signal
		res.Signal = &sig
	case exit.err != nil:
		// An unrecognized wait outcome is a failure, never a success.
		sig := contract.SignalUnknown
		res.Signal = &sig
	default:
		code := exit.code
		res.ExitCode = &code
	}
	if res.FinalMessage == nil {
		res.FinalMessageTruncated = false
	}
	w.mu.Lock()
	w.exited, w.result = true, res
	w.mu.Unlock()
	if cleanupErr == nil {
		// Release after the actual exit and pipe drain; an unconfirmed
		// cleanup retains the slot and keeps the role locally blocked.
		s.release(w.key)
	}
	s.event(evTaskExited, w)
	s.signal()
}

// gc forgets workers whose goroutine returned and whose result can no
// longer be sent (acknowledged, or fenced).
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
		if w.done && (w.phase == phaseRefused || w.acked || w.fenced) {
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
			ids = append(ids, w.id())
		}
	}
	s.mu.Unlock()
	return ids
}

// attached returns the workers started on tag, sorted by task ID.
func (s *taskSupervisor) attached(tag *attachTag) []*taskWorker {
	s.mu.Lock()
	out := make([]*taskWorker, 0, len(s.workers))
	for w := range s.workers {
		if w.tag == tag {
			out = append(out, w)
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].id() < out[j].id() })
	return out
}

// fence ends transmission for every worker of tag, permanently: a result
// in flight without its receipt acknowledgement is recorded locally as
// result_receipt_unconfirmed and is never resent.
func (s *taskSupervisor) fence(tag *attachTag) {
	for _, w := range s.attached(tag) {
		w.mu.Lock()
		w.fenced = true
		if w.resultSent && !w.acked {
			w.unconfirmed = true
			s.logger.Warn("task result receipt unconfirmed", "task_id", w.id(), "reason", "result_receipt_unconfirmed")
		}
		w.mu.Unlock()
		s.event(evTaskFenced, w)
	}
	s.gc()
}

// shutdown is Run's local supervisor teardown: no new launch is
// authorized, every running child's group gets TERM, the grace and KILL
// if needed, and every worker (Wait and group-disappearance
// verification included) is joined. It invents no cancel API: the plane
// learns nothing unless a result exchange completed.
func (s *taskSupervisor) shutdown() {
	s.mu.Lock()
	s.closing = true
	var running []*taskWorker
	for w := range s.workers {
		running = append(running, w)
	}
	s.mu.Unlock()
	var term sync.WaitGroup
	for _, w := range running {
		exited := false
		select {
		case <-w.exitedCh:
			exited = true
		default:
		}
		// A worker still authorized here publishes its child after this
		// scan and, seeing closing, terminates it itself (run).
		w.mu.Lock()
		pid, started := w.pid, w.phase == phaseStarted && !w.termSent && !exited
		w.termSent = w.termSent || started
		w.mu.Unlock()
		if started {
			term.Add(1)
			go func() {
				defer term.Done()
				s.groups.terminate(pid, w.exitedCh)
			}()
		}
	}
	s.d.emit(event{kind: evSupervisorClosing})
	term.Wait()
	s.wg.Wait()
}
