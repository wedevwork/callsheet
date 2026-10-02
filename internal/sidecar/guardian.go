//go:build linux || darwin

package sidecar

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
)

// The internal task guardian (iteration 06a, resilience.md "Guardian
// entrypoint and descriptors"): the callsheet binary re-executed with the
// reserved argv[1] GuardianToken becomes one task's process-group leader
// (Setpgid by its parent, never setsid). It validates its private fd
// layout and invocation before any mutation, records its ownership
// durably (owner.json armed, then the control FIFO and ready), launches
// the adapter only after the single release byte (owner.json released
// first), forwards the adapter's exact wait status, and always cleans its
// whole group itself: TERM to its own group (it survives its own TERM),
// one second of grace, then KILL to that same group, itself included.
// Only the guardian signals its group after a sidecar restart; a stop
// command on its FIFO or its parent-lifetime pipe's EOF requests that
// cleanup. It opens no network listener and is no public command.
//
// Iteration 06b: the guardian is the sole execution-deadline authority.
// Under one arbitration mutex it samples the monotonic authorization
// instant immediately before starting the adapter and arms the timeout
// (invocation version 2; zero arms none) before invoking Start, which runs
// outside the mutex. One cause latch decides the outcome: an adapter exit
// observed strictly before the deadline is natural, at or after it
// timed_out; a cancel command (cause cancelled with the plane's stop ID)
// or the parent's EOF (lost) latched first keeps its cause. The latched
// control cause is reported as a stopping status before the deliberate
// group KILL, through one bounded, ordered status writer.
//
// Iteration 10a: the grace ends early, without the KILL, only on positive
// native proof that the guardian is the last member of its group (Linux:
// a child subreaper whose WNOHANG wait4 reports ECHILD; macOS: the
// kernel's process-group PID list naming only the guardian). Anything
// else keeps the full grace and the KILL.

// maxGuardianDiag bounds the guardian's stderr diagnostics.
const maxGuardianDiag = 512

// Guardian error reasons (status "error"): each means no adapter was
// started.
const (
	guardianFIFOFailed  = "fifo_failed"
	guardianOwnerFailed = "owner_failed"
	guardianStartFailed = "start_failed"
)

// guardianFDs are the guardian's validated inherited descriptors, in the
// fixed order 3 (invocation), 4 (parent lifetime), 5 (release barrier), 6
// (status), 7-9 (the adapter's stdin, stdout and stderr).
type guardianFDs struct {
	inv, life, release, status, stdin, stdout, stderr *os.File
	// lifeBlocking reports a lifetime pipe that could not be made
	// nonblocking at adoption (iteration 10a): closing it would not unblock
	// its reader, so the guardian neither completes early nor joins it.
	lifeBlocking bool
}

// adapterRun is a started adapter the guardian waits for.
type adapterRun interface {
	PID() int
	Wait() procExit
}

// guardianEnv is the guardian's OS seam: the injected tests run it in
// process with pipes and fakes (no child, no real signal).
type guardianEnv struct {
	fds          func() (guardianFDs, error)
	getpid       func() int
	getpgrp      func() int
	mkfifo       func(path string) error
	startAdapter func(inv contract.GuardianInvocation, stdin, stdout, stderr *os.File) (adapterRun, error)
	notifyTerm   func(c chan<- os.Signal) func()
	sig          processgroup.Signaler
	clock        processgroup.Clock
	grace        time.Duration
	now          func() time.Time
	lookup       contract.AdapterLookup
	// timerAt arms a stoppable timer at an absolute instant of clock (the
	// same clock: its Now samples every instant); events, when non-nil,
	// observes the arbitration (tests only).
	timerAt func(at time.Time) (<-chan time.Time, func() bool)
	events  func(string)
	// subreap and groupAlone are iteration 10a's early-completion
	// primitives (build-selected natively: group_alone_linux.go,
	// group_alone_darwin.go). subreap runs once before any adapter Start;
	// groupAlone observes the guardian's group after the adapter's Wait
	// joined. A nil field disables the fast path (unknown), never proves
	// alone; an error always means unknown, whatever state it returns.
	subreap    func() error
	groupAlone func(pgid int) (groupState, error)
}

// groupState is one native observation of the guardian's own group
// (iteration 10a): only groupAlone, returned without an error, is the
// positive proof that the guardian is the group's last member.
type groupState int

const (
	groupUnknown groupState = iota
	groupBusy
	groupAlone
)

// maxProbeRetries bounds a native observation's EINTR retries: an
// interrupted probe is retried within the same cleanup deadline, then
// counts as unknown.
const maxProbeRetries = 8

// statusDeadline bounds one status delivery on the injected clock: a
// blocked status pipe is abandoned, never waited for.
const statusDeadline = 100 * time.Millisecond

// Status queue slots, in lifecycle order: at most one record each.
const (
	slotReady = iota
	slotStarted
	slotStopping
	slotExit
	slotCount
)

// RunTaskGuardian is the guardian entrypoint: args are os.Args[1:] and
// must be exactly [GuardianToken]. cmd/callsheet dispatches it before any
// signal handling or CLI parsing, so the public SIGTERM handler never
// terminates the group anchor. It is not authentication: an invalid
// invocation exits 2 with a bounded stderr diagnostic before any spawn,
// journal mutation or group signal.
func RunTaskGuardian(args []string, stderr io.Writer) int {
	return runGuardian(args, stderr, defaultGuardianEnv())
}

// diagf writes one bounded, sanitized diagnostic line.
func diagf(w io.Writer, format string, args ...any) {
	if w != nil {
		io.WriteString(w, "callsheet task guardian: "+contract.SafeText(fmt.Sprintf(format, args...), maxGuardianDiag)+"\n")
	}
}

func runGuardian(args []string, stderr io.Writer, env guardianEnv) int {
	if len(args) != 1 || args[0] != GuardianToken {
		diagf(stderr, "invalid invocation: the internal guardian takes exactly its reserved token")
		return 2
	}
	fds, err := env.fds()
	if err != nil {
		diagf(stderr, "invalid descriptors: %v", err)
		return 2
	}
	pid := env.getpid()
	if pid <= 1 || env.getpgrp() != pid {
		diagf(stderr, "invalid process group: the guardian must lead its own process group")
		return 2
	}
	inv, err := readInvocation(fds.inv)
	fds.inv.Close()
	if err != nil {
		diagf(stderr, "invalid invocation: %v", err)
		return 2
	}
	if err := checkTaskDir(inv, env.lookup); err != nil {
		diagf(stderr, "invalid task directory: %v", err)
		return 2
	}
	g := &guardian{env: env, fds: fds, inv: inv, pid: pid, diag: stderr, stop: make(chan struct{}), parentGone: make(chan struct{}),
		exitCh: make(chan struct{}), done: make(chan struct{})}
	g.sq = newStatusQueue(g)
	defer g.sq.close()
	defer close(g.done)
	return g.run()
}

// readInvocation reads the length-prefixed invocation, then EOF.
func readInvocation(f *os.File) (contract.GuardianInvocation, error) {
	b, err := contract.ReadFramed(f, contract.MaxGuardianInvocationBytes)
	if err != nil {
		return contract.GuardianInvocation{}, err
	}
	var extra [1]byte
	if n, err := f.Read(extra[:]); n != 0 || err != io.EOF {
		return contract.GuardianInvocation{}, errors.New("data after the invocation")
	}
	return contract.ParseGuardianInvocation(b)
}

// checkTaskDir validates the invocation against the prepared journal
// before any mutation: a private real task directory, a prepared
// execution of exactly this identity without an owner, and no owner
// record yet.
func checkTaskDir(inv contract.GuardianInvocation, lookup contract.AdapterLookup) error {
	fi, err := os.Lstat(inv.TaskDir)
	if err != nil {
		return err
	}
	if err := checkDir(inv.TaskDir, fi); err != nil {
		return err
	}
	b, err := readPrivate(filepath.Join(inv.TaskDir, executionName), contract.MaxExecutionJournalBytes)
	if err != nil {
		return err
	}
	j, err := contract.ParseExecutionJournal(b, lookup)
	if err != nil {
		return err
	}
	switch {
	case j.TaskID != inv.TaskID || j.Execution != inv.Execution || j.StartDigest != inv.StartDigest:
		return errors.New("the journal names another execution")
	case j.Phase != contract.JournalPrepared || j.OwnerNonce != nil:
		return errors.New("the execution is not prepared for a guardian")
	}
	if _, err := os.Lstat(filepath.Join(inv.TaskDir, ownerName)); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("the task already has an owner record")
	}
	return nil
}

// guardian is one running guardian.
type guardian struct {
	env        guardianEnv
	fds        guardianFDs
	inv        contract.GuardianInvocation
	pid        int
	diag       io.Writer
	stopOnce   sync.Once
	stop       chan struct{}
	parentGone chan struct{}
	sq         *statusQueue
	// arb is the arbitration mutex and its cause latch: the first cause
	// latched governs the outcome and is never replaced.
	arb      sync.Mutex
	cause    string
	stopID   string
	deadline time.Time
	armed    bool
	// exitCh closes once the adapter's exit fact is published; done when
	// the guardian returns (injected tests).
	exitCh    chan struct{}
	exitOnce  sync.Once
	done      chan struct{}
	cleanOnce sync.Once
	cleaned   chan struct{}
	// fast reports that the early-completion primitives are installed
	// (iteration 10a: both present and the subreaper, where one exists,
	// set before the adapter's Start). fin makes one decision between a
	// positive group proof and the escalation's deadline: once the
	// deadline's final flush (or its KILL) claimed the cleanup, no proof is
	// accepted; a proof accepted first ends the cleanup and withholds the
	// KILL (killGate).
	fast    bool
	fin     sync.Mutex
	claimed bool
	proved  bool
}

func (g *guardian) event(e string) {
	if g.env.events != nil {
		g.env.events(e)
	}
}

// requestStop latches a stop request (idempotent: a cleanup already under
// way keeps its grace).
func (g *guardian) requestStop() { g.stopOnce.Do(func() { close(g.stop) }) }

// latch sets the cause if none is latched yet (under the arbitration
// mutex) and reports whether it did.
func (g *guardian) latch(cause, stopID string) bool {
	g.arb.Lock()
	defer g.arb.Unlock()
	if g.cause != "" {
		return false
	}
	g.cause, g.stopID = cause, stopID
	return true
}

// stopping enqueues the latched control cause's stopping status.
func (g *guardian) stopping() {
	g.arb.Lock()
	cause, stopID := g.cause, g.stopID
	g.arb.Unlock()
	if cause == "" || cause == "natural" {
		return
	}
	s := contract.GuardianStatus{Type: contract.GuardianStopping, Cause: &cause}
	if stopID != "" {
		id := stopID
		s.StopID = &id
	}
	g.sq.put(slotStopping, s)
	g.event("stopping " + cause)
}

func (g *guardian) status(s contract.GuardianStatus) error {
	s.TaskID, s.Nonce = g.inv.TaskID, g.inv.Nonce
	b, err := contract.EncodeGuardianStatus(s)
	if err != nil {
		return err
	}
	return contract.WriteFramed(g.fds.status, b)
}

// after is a bounded wait of d on the injected clock's stoppable timer
// (nil without one: unbounded; the injected tests without a timer seam).
func (g *guardian) after(d time.Duration) (<-chan time.Time, func() bool) {
	if g.env.timerAt == nil {
		return nil, func() bool { return false }
	}
	return g.env.timerAt(g.env.clock.Now().Add(d))
}

func (g *guardian) fail(reason string, err error) int {
	diagf(g.diag, "%s: %v", reason, err)
	r := reason
	g.sq.skipSlot(slotReady)
	g.sq.put(slotStarted, contract.GuardianStatus{Type: contract.GuardianError, Reason: &r})
	g.sq.skipFrom(slotStopping)
	g.sq.drain()
	return 1
}

// statusQueue is the guardian's one bounded, ordered status writer: at
// most one record per lifecycle slot (ready, started or error, stopping,
// exit), written in slot order by one goroutine, each delivery bounded by
// statusDeadline on the injected clock. A delivery that fails or times
// out abandons the pipe (closed; nothing later is written) and records no
// success. It never blocks the escalation: callers only enqueue.
type statusQueue struct {
	g       *guardian
	mu      sync.Mutex
	slots   [slotCount]*contract.GuardianStatus
	skip    [slotCount]bool
	written int // slots handled (written, skipped or abandoned)
	closed  bool
	dead    bool
	wake    chan struct{}
	idle    chan struct{} // signaled whenever written advances
	joined  chan struct{}
}

func newStatusQueue(g *guardian) *statusQueue {
	q := &statusQueue{g: g, wake: make(chan struct{}, 1), idle: make(chan struct{}, 1), joined: make(chan struct{})}
	go q.loop()
	return q
}

func (q *statusQueue) put(slot int, s contract.GuardianStatus) {
	q.mu.Lock()
	if q.slots[slot] == nil && !q.skip[slot] {
		q.slots[slot] = &s
	}
	q.mu.Unlock()
	notify(q.wake)
}

// skipFrom marks every unfilled slot from slot on as never written.
func (q *statusQueue) skipFrom(slot int) {
	q.mu.Lock()
	for i := slot; i < slotCount; i++ {
		if q.slots[i] == nil {
			q.skip[i] = true
		}
	}
	q.mu.Unlock()
	notify(q.wake)
}

func (q *statusQueue) skipSlot(slot int) {
	q.mu.Lock()
	if q.slots[slot] == nil {
		q.skip[slot] = true
	}
	q.mu.Unlock()
	notify(q.wake)
}

func (q *statusQueue) loop() {
	defer close(q.joined)
	for i := 0; i < slotCount; {
		q.mu.Lock()
		s, skip, closed, dead := q.slots[i], q.skip[i], q.closed, q.dead
		q.mu.Unlock()
		switch {
		case dead || (closed && s == nil):
			return
		case skip:
			i++
			q.advance(i)
			continue
		case s == nil:
			<-q.wake
			continue
		}
		if !q.deliver(*s) {
			q.mu.Lock()
			q.dead = true
			q.mu.Unlock()
			q.g.fds.status.Close()
			q.advance(slotCount)
			return
		}
		// The delivery returned: its bound timer is stopped.
		q.g.event("status-delivered " + s.Type)
		i++
		q.advance(i)
	}
}

func (q *statusQueue) advance(n int) {
	q.mu.Lock()
	q.written = n
	q.mu.Unlock()
	notify(q.idle)
}

// deliver writes one status within statusDeadline.
func (q *statusQueue) deliver(s contract.GuardianStatus) bool {
	res := make(chan error, 1)
	go func() { res <- q.g.status(s) }()
	timeout, stop := q.g.after(statusDeadline)
	defer stop()
	select {
	case err := <-res:
		return err == nil
	case <-timeout:
		select {
		case err := <-res:
			return err == nil
		default:
		}
		q.g.fds.status.Close() // unblocks the write
		<-res
		return false
	}
}

// flush waits (bounded by statusDeadline) until every deliverable
// enqueued record (none of its predecessors still awaited) is delivered or
// abandoned: called immediately before the deliberate KILL.
func (q *statusQueue) flush() {
	timeout, stop := q.g.after(statusDeadline)
	defer stop()
	for {
		q.mu.Lock()
		pending := false
		for i := q.written; i < slotCount; i++ {
			if q.slots[i] != nil {
				pending = true
				continue
			}
			if !q.skip[i] {
				break
			}
		}
		dead := q.dead
		q.mu.Unlock()
		if !pending || dead {
			return
		}
		select {
		case <-q.idle:
		case <-timeout:
			return
		}
	}
}

// drain waits (bounded) for the enqueued records before a return.
func (q *statusQueue) drain() { q.flush() }

// close stops the writer and joins it (the guardian's return).
func (q *statusQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	notify(q.wake)
	<-q.joined
}

// writeOwner publishes owner.json in phase durably: a synced private
// temporary, a rename and the task directory's sync.
func (g *guardian) writeOwner(phase string) error {
	b, err := contract.EncodeOwner(contract.OwnerRecord{TaskID: g.inv.TaskID, Execution: g.inv.Execution, Nonce: g.inv.Nonce, PGID: g.pid,
		GuardianPID: g.pid, Phase: phase})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(g.inv.TaskDir, tempPrefix+ownerName+"-")
	if err != nil {
		return err
	}
	name := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(name)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(name)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, filepath.Join(g.inv.TaskDir, ownerName)); err != nil {
		os.Remove(name)
		return err
	}
	return syncPath(g.inv.TaskDir)
}

// syncPath fsyncs a directory by path.
func syncPath(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if err != nil && (errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTTY)) {
		err = rawFsync(f)
	}
	return errors.Join(err, f.Close())
}

// readCommands serves the control FIFO: bounded lines, each a command
// whose task, execution and nonce must be this guardian's; a valid stop
// latches the stop request, anything else is drained and reported, never
// executed.
func (g *guardian) readCommands(f *os.File) {
	r := bufio.NewReaderSize(f, contract.MaxGuardianCommandBytes)
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// An overlong command: drain it to its LF without growing memory.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			diagf(g.diag, "an overlong control command was ignored")
			if err != nil {
				return
			}
			continue
		}
		if err != nil {
			return
		}
		c, perr := contract.ParseGuardianCommand(line[:len(line)-1])
		if perr != nil || c.TaskID != g.inv.TaskID || c.Execution != g.inv.Execution || c.Nonce != g.inv.Nonce {
			diagf(g.diag, "a control command for another execution was ignored")
			continue
		}
		// A command without a cause is the recovery stop (lost); a cancel
		// carries the plane's stop ID. A cause latched earlier (a natural
		// exit, the deadline, an earlier stop) is kept.
		cause := contract.CauseLost
		if c.Cause == contract.CauseCancelled {
			cause = contract.CauseCancelled
		}
		if g.latch(cause, c.StopID) {
			g.event("cause " + cause)
			g.stopping()
		}
		g.requestStop()
	}
}

// readRelease waits for the release barrier: exactly one byte 0x01, then
// EOF. Premature EOF, another byte or extra data means never launch.
func readRelease(f *os.File) bool {
	var b [2]byte
	if n, err := io.ReadFull(f, b[:1]); n != 1 || err != nil || b[0] != 0x01 {
		return false
	}
	n, err := f.Read(b[1:])
	return n == 0 && err == io.EOF
}

func closed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// run is the guardian after its validation.
func (g *guardian) run() int {
	term := make(chan os.Signal, 4)
	stopTerm := g.env.notifyTerm(term)
	go func() {
		for range term {
			// The guardian survives its own group's TERM: it anchors the
			// group through the grace.
		}
	}()
	defer func() {
		stopTerm() // no delivery after this, so the channel may close
		close(term)
	}()
	// The lifetime reader ends at the parent's EOF (parent gone), or when
	// the guardian's ordinary return closes its descriptor and joins it.
	lifeDone := make(chan struct{})
	go func() {
		defer close(lifeDone)
		if _, err := io.Copy(io.Discard, g.fds.life); !errors.Is(err, os.ErrClosed) {
			close(g.parentGone)
		}
	}()
	fifo := filepath.Join(g.inv.TaskDir, controlName)
	if err := g.env.mkfifo(fifo); err != nil {
		return g.fail(guardianFIFOFailed, err)
	}
	ff, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		return g.fail(guardianFIFOFailed, err)
	}
	defer ff.Close()
	if err := syncPath(g.inv.TaskDir); err != nil {
		return g.fail(guardianFIFOFailed, err)
	}
	cmdDone := make(chan struct{})
	go func() {
		defer close(cmdDone)
		g.readCommands(ff)
	}()
	if err := g.writeOwner(contract.OwnerArmed); err != nil {
		return g.fail(guardianOwnerFailed, err)
	}
	if err := g.status(contract.GuardianStatus{Type: contract.GuardianReady, PID: g.pid, PGID: g.pid}); err != nil {
		diagf(g.diag, "the parent is gone before ready: %v", err)
		return 0
	}
	g.sq.skipSlot(slotReady) // written synchronously above
	release := make(chan bool, 1)
	go func() { release <- readRelease(g.fds.release) }()
	select {
	case ok := <-release:
		if !ok {
			return 0 // revoked: never launch
		}
	case <-g.parentGone:
		return 0
	case <-g.stop:
		return 0
	}
	// The release, the parent's liveness and the stop latch are checked
	// together before the durable released record and the launch.
	if closed(g.parentGone) || closed(g.stop) {
		return 0
	}
	if err := g.writeOwner(contract.OwnerReleased); err != nil {
		return g.fail(guardianOwnerFailed, err)
	}
	g.cleaned = make(chan struct{})
	// Iteration 10a: the early-completion primitives are installed before
	// any adapter Start. The subreaper (Linux) must exist before the launch
	// so an orphaned descendant is adopted before its parent can be fully
	// reaped; its failure only disables the early completion (the full
	// grace remains), never the task.
	if g.fast = g.env.subreap != nil && g.env.groupAlone != nil; g.fast && g.fds.lifeBlocking {
		g.fast = false
		diagf(g.diag, "early group completion disabled: the lifetime pipe stayed blocking")
		g.event("lifetime-blocking")
	}
	if g.fast {
		if err := g.env.subreap(); err != nil {
			g.fast = false
			diagf(g.diag, "early group completion disabled: %v", err)
			g.event("subreap-failed")
		}
	}
	// Start authorization: the monotonic instant is sampled and the
	// deadline armed under the arbitration mutex, immediately before Start;
	// Start runs outside it, so a blocked Start never prevents the timeout.
	g.arb.Lock()
	authAt, authWall := g.env.clock.Now(), g.env.now()
	var timer <-chan time.Time
	stopTimer := func() bool { return false }
	if d := g.inv.TimeoutDuration(); g.inv.TimeoutPolicy == contract.TimeoutPolicyEnforced && d > 0 && g.env.timerAt != nil {
		g.deadline, g.armed = authAt.Add(d), true
		timer, stopTimer = g.env.timerAt(g.deadline)
		g.event("timeout-armed")
	}
	g.arb.Unlock()
	defer stopTimer()
	if timer != nil {
		go g.watchDeadline(timer)
	}
	run, err := g.env.startAdapter(g.inv, g.fds.stdin, g.fds.stdout, g.fds.stderr)
	g.fds.stdin.Close()
	g.fds.stdout.Close()
	g.fds.stderr.Close()
	if err != nil {
		// A definite Start failure: no adapter, so no execution timeout
		// result (a latched deadline's cleanup still runs on the group).
		code := g.fail(guardianStartFailed, err)
		g.arb.Lock()
		began := g.cause != ""
		g.arb.Unlock()
		if began {
			<-g.cleaned
		}
		return code
	}
	at := contract.FormatTime(authWall)
	g.sq.put(slotStarted, contract.GuardianStatus{Type: contract.GuardianStarted, PID: run.PID(), StartedAt: &at})
	g.arb.Lock()
	late := g.cause != "" && g.cause != "natural"
	g.arb.Unlock()
	if late {
		// The deadline (or a control) fired while Start was outstanding: no
		// fresh duration, the group is cleaned up at once; the adapter,
		// started after the first TERM, gets its own.
		g.env.sig.Signal(-g.pid, syscall.SIGTERM)
	}
	go func() {
		ex := run.Wait()
		g.publishExit(ex)
	}()
	parentGone, stop := g.parentGone, g.stop
	for {
		select {
		case <-g.exitCh:
			g.cleanup()
			<-g.cleaned
			// An ordinary return (iteration 10a: the group was proved to
			// hold only this guardian; before, only an unkillable injected
			// group returned): close the command and lifetime readers'
			// descriptors and join them (the lifetime reader only when its
			// descriptor is pollable, so its Close unblocks it).
			ff.Close()
			<-cmdDone
			if !g.fds.lifeBlocking {
				g.fds.life.Close()
				<-lifeDone
			}
			return 0
		case <-parentGone:
			parentGone = nil
			if g.latch(contract.CauseLost, "") {
				g.event("cause " + contract.CauseLost)
				g.stopping()
			}
			g.cleanup()
		case <-stop:
			stop = nil
			g.cleanup()
		}
	}
}

// watchDeadline is the timer's own goroutine: at the deadline, absent an
// earlier cause (an exit observed strictly before it is natural), it
// latches timed_out and starts the cleanup at once, independently of
// status, journal I/O or a Start still outstanding.
func (g *guardian) watchDeadline(timer <-chan time.Time) {
	select {
	case <-timer:
	case <-g.done:
		return
	}
	if g.latch(contract.CauseTimedOut, "") {
		g.event("cause " + contract.CauseTimedOut)
		g.stopping()
		g.cleanup()
	}
}

// publishExit publishes the adapter's exit fact with its monotonic
// observation instant under the arbitration mutex, before waking the
// loop: strictly before the deadline an unlatched cause becomes natural,
// at or after it timed_out. Its exact wait status is forwarded either way.
func (g *guardian) publishExit(ex procExit) {
	g.arb.Lock()
	at := g.env.clock.Now()
	timedOut := false
	if g.cause == "" {
		if g.armed && !at.Before(g.deadline) {
			g.cause, timedOut = contract.CauseTimedOut, true
		} else {
			g.cause = "natural"
		}
	}
	natural := g.cause == "natural"
	g.arb.Unlock()
	g.event("adapter-exit-observed")
	if timedOut {
		g.event("cause " + contract.CauseTimedOut)
		g.stopping()
	}
	if natural {
		g.sq.skipSlot(slotStopping)
	}
	s := contract.GuardianStatus{Type: contract.GuardianExit}
	switch {
	case ex.signal != "":
		sig := ex.signal
		s.Signal = &sig
	case ex.err != nil:
		sig := contract.SignalUnknown
		s.Signal = &sig
	default:
		code := ex.code
		s.ExitCode = &code
	}
	g.sq.put(slotExit, s)
	g.exitOnce.Do(func() { close(g.exitCh) })
}

// cleanup starts the group cleanup once: TERM to its own group, the
// grace, then KILL to that same group, the guardian included. The
// adapter's wait status, not this KILL, decides a natural outcome; the
// enqueued status records are flushed (bounded) immediately before the
// KILL.
//
// Iteration 10a: when the fast path is installed, a separate observer
// (observe) looks for positive native proof that the guardian is its
// group's last member; a proof accepted before the deadline's final flush
// claimed the cleanup ends it without the KILL: the status queue is
// flushed (bounded) and the guardian returns normally, still having
// anchored the group until then. Busy, unknown or a probe error never ends
// the grace early, and a slow probe never delays the escalation, which
// keeps its armed deadline. The sidecar's own ESRCH observation, not this
// proof, remains the group's final absence.
func (g *guardian) cleanup() {
	g.cleanOnce.Do(func() {
		go func() {
			defer close(g.cleaned)
			alone, stop, observed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			if g.fast {
				go func() {
					defer close(observed)
					g.observe(alone, stop)
				}()
			} else {
				close(observed)
			}
			processgroup.Escalate(context.Background(), processgroup.Plan{PGID: g.pid, Grace: g.env.grace, Sig: killGate{g}, Clock: g.env.clock,
				Done: alone, BeforeDeadline: func() error { g.claim(); g.sq.flush(); return nil }})
			g.event("escalation-ended")
			close(stop)
			<-observed
			g.fin.Lock()
			proved := g.proved
			g.fin.Unlock()
			if proved {
				g.sq.flush()
				g.event("group-alone")
			}
		}()
	})
}

// observe is the cleanup's group observer: once the adapter's Start
// returned and its Wait joined (the published exit fact; a Start in
// progress or a direct child's exit alone is never eligible), it probes
// the group at once and then every groupPoll until a proof is accepted,
// the deadline claimed the cleanup, or the cleanup ends.
func (g *guardian) observe(alone chan<- struct{}, stop <-chan struct{}) {
	select {
	case <-g.exitCh:
	case <-stop:
		return
	}
	for {
		st, err := g.env.groupAlone(g.pid)
		if err != nil {
			st = groupUnknown
		}
		g.event("probe " + st.String())
		if st == groupAlone {
			if g.accept() {
				g.event("proof-accepted")
				close(alone)
			}
			return
		}
		tick, cancel := g.after(groupPoll)
		select {
		case <-stop:
			cancel()
			return
		case <-tick:
		}
	}
}

// accept records a positive proof unless the deadline already claimed the
// cleanup (under fin: one order between the two).
func (g *guardian) accept() bool {
	g.fin.Lock()
	defer g.fin.Unlock()
	if g.claimed {
		return false
	}
	g.proved = true
	return true
}

// claim is the deadline's final step before its flush and KILL: unless a
// proof was accepted first, no proof is accepted afterwards.
func (g *guardian) claim() bool {
	g.fin.Lock()
	ok := !g.proved
	if ok {
		g.claimed = true
	}
	g.fin.Unlock()
	if ok {
		g.event("deadline-claimed")
	}
	return ok
}

// killGate is the escalation's signaler: every signal but the KILL passes
// to the guardian's own; the KILL is sent only if claim, under the same
// mutex as accept, still authorizes it. A proof accepted first therefore
// withholds the KILL however the escalation's wake-ups are ordered (the
// group then holds only the guardian; the escalation reads the withheld
// KILL as a group already gone).
type killGate struct{ g *guardian }

func (k killGate) Signal(pid int, sig syscall.Signal) error {
	if sig == syscall.SIGKILL && !k.g.claim() {
		k.g.event("kill-withheld")
		return syscall.ESRCH
	}
	return k.g.env.sig.Signal(pid, sig)
}

func (s groupState) String() string {
	switch s {
	case groupAlone:
		return "alone"
	case groupBusy:
		return "busy"
	}
	return "unknown"
}
