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
}

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
	g := &guardian{env: env, fds: fds, inv: inv, pid: pid, diag: stderr, stop: make(chan struct{}), parentGone: make(chan struct{})}
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
}

// requestStop latches a stop request (idempotent: a cleanup already under
// way keeps its grace).
func (g *guardian) requestStop() { g.stopOnce.Do(func() { close(g.stop) }) }

func (g *guardian) status(s contract.GuardianStatus) error {
	s.TaskID, s.Nonce = g.inv.TaskID, g.inv.Nonce
	b, err := contract.EncodeGuardianStatus(s)
	if err != nil {
		return err
	}
	return contract.WriteFramed(g.fds.status, b)
}

func (g *guardian) fail(reason string, err error) int {
	diagf(g.diag, "%s: %v", reason, err)
	r := reason
	g.status(contract.GuardianStatus{Type: contract.GuardianError, Reason: &r})
	return 1
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
	go func() {
		io.Copy(io.Discard, g.fds.life)
		close(g.parentGone)
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
	go g.readCommands(ff)
	if err := g.writeOwner(contract.OwnerArmed); err != nil {
		return g.fail(guardianOwnerFailed, err)
	}
	if err := g.status(contract.GuardianStatus{Type: contract.GuardianReady, PID: g.pid, PGID: g.pid}); err != nil {
		diagf(g.diag, "the parent is gone before ready: %v", err)
		return 0
	}
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
	run, err := g.env.startAdapter(g.inv, g.fds.stdin, g.fds.stdout, g.fds.stderr)
	g.fds.stdin.Close()
	g.fds.stdout.Close()
	g.fds.stderr.Close()
	if err != nil {
		return g.fail(guardianStartFailed, err)
	}
	at := contract.FormatTime(g.env.now())
	g.status(contract.GuardianStatus{Type: contract.GuardianStarted, PID: run.PID(), StartedAt: &at})
	exited := make(chan procExit, 1)
	go func() { exited <- run.Wait() }()
	cleaned := make(chan struct{})
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			go func() {
				defer close(cleaned)
				// TERM to its own group, the grace, then KILL to that same
				// group, the guardian included: no descendant-enumeration
				// fast path; the adapter's wait status, not this KILL,
				// decides the outcome.
				processgroup.Escalate(context.Background(), processgroup.Plan{PGID: g.pid, Grace: g.env.grace, Sig: g.env.sig, Clock: g.env.clock})
			}()
		})
	}
	parentGone, stop := g.parentGone, g.stop
	for {
		select {
		case ex := <-exited:
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
			g.status(s)
			cleanup()
			<-cleaned
			return 0
		case <-parentGone:
			parentGone = nil
			cleanup()
		case <-stop:
			stop = nil
			cleanup()
		}
	}
}
