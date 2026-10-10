package reale2e

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Process supervision (design 12a-real-e2e, Failure and lifecycle): every
// child is started from an argv slice (no shell), leads its own process
// group so the operator terminal's signals reach only the supervisor, and
// is stopped by its captured handle only: SIGTERM, then a bounded SIGKILL,
// each followed by an exit observation. Nothing is ever signalled by a
// name pattern or by a PID read back from evidence.

// Clock is the supervisor's time source; tests inject a fake clock whose
// timers fire only when it is advanced.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) (<-chan time.Time, func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// ProcSpec is one child's argv, complete environment, directory and
// output writers (nil discards).
type ProcSpec struct {
	Path   string
	Args   []string
	Env    []string
	Dir    string
	Stdout io.Writer
	Stderr io.Writer
}

// Proc is a started child leading its own process group: its captured
// PID (also the group ID), a channel closed once the leader was reaped,
// its exit (valid after Done), signal delivery to the whole captured group
// and whether any process of that group remains.
type Proc interface {
	PID() int
	Done() <-chan struct{}
	Exit() (code int, signal string)
	Signal(sig syscall.Signal) error
	GroupGone() bool
}

// Launcher starts children.
type Launcher interface {
	Start(s ProcSpec) (Proc, error)
}

// ExecLauncher is the live Launcher over os/exec.
type ExecLauncher struct{}

type execProc struct {
	cmd   *exec.Cmd
	done  chan struct{}
	state *os.ProcessState
}

// Start starts s with Setpgid; its streams are copied to the writers, and
// WaitDelay bounds a descendant that keeps them open.
func (ExecLauncher) Start(s ProcSpec) (Proc, error) {
	cmd := &exec.Cmd{Path: s.Path, Args: append([]string{s.Path}, s.Args...), Env: s.Env, Dir: s.Dir,
		Stdout: s.Stdout, Stderr: s.Stderr, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}, WaitDelay: 2 * time.Second}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		p.state = cmd.ProcessState
		close(p.done)
	}()
	return p, nil
}

func (p *execProc) PID() int              { return p.cmd.Process.Pid }
func (p *execProc) Done() <-chan struct{} { return p.done }

// Signal delivers sig to the captured process group (the leader and every
// descendant that stayed in it), also after the leader exited; it reports
// os.ErrProcessDone once no process of the group remains.
func (p *execProc) Signal(sig syscall.Signal) error {
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

// GroupGone reports whether the leader was reaped and no process of its
// group remains (kill(-pgid, 0) finds none).
func (p *execProc) GroupGone() bool {
	select {
	case <-p.done:
	default:
		return false
	}
	return errors.Is(syscall.Kill(-p.cmd.Process.Pid, 0), syscall.ESRCH)
}

func (p *execProc) Exit() (int, string) {
	select {
	case <-p.done:
	default:
		return -1, ""
	}
	if ws, ok := p.state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -1, ws.Signal().String()
	}
	return p.state.ExitCode(), ""
}

// ProcRecord is one owned process's cleanup record.
type ProcRecord struct {
	Name         string `json:"name"`
	PID          int    `json:"pid"`
	StoppedBy    string `json:"stopped_by"`
	ExitVerified bool   `json:"exit_verified"`
	Exit         string `json:"exit"`
}

// Stop methods recorded in ProcRecord.StoppedBy.
const (
	StoppedExited = "exited"
	StoppedTerm   = "SIGTERM"
	StoppedKill   = "SIGKILL"
)

// exitText renders a reaped child's exit.
func exitText(p Proc) string {
	code, sig := p.Exit()
	if sig != "" {
		return "signal " + sig
	}
	return "exit " + strconv.Itoa(code)
}

// awaitDone reports whether the leader p was reaped within d.
func awaitDone(p Proc, clock Clock, d time.Duration) bool {
	select {
	case <-p.Done():
		return true
	default:
	}
	t, stop := clock.NewTimer(d)
	defer stop()
	select {
	case <-p.Done():
		return true
	case <-t:
		return false
	}
}

// groupPoll is the poll period for a process group's last members (the
// kernel gives no wake-up for another process's descendants).
const groupPoll = 50 * time.Millisecond

// awaitGone reports whether p's whole group was gone within d: the
// leader's reap is awaited directly, its remaining group members by
// polling.
func awaitGone(p Proc, clock Clock, d time.Duration) bool {
	deadline := clock.Now().Add(d)
	for {
		done := false
		select {
		case <-p.Done():
			done = true
		default:
		}
		if done && p.GroupGone() {
			return true
		}
		left := deadline.Sub(clock.Now())
		if left <= 0 {
			return false
		}
		t, stop := clock.NewTimer(min(left, groupPoll))
		if done {
			<-t
			continue
		}
		select {
		case <-p.Done():
			stop()
		case <-t:
		}
	}
}

// stopProc stops p's captured process group by its handle: nothing if the
// leader was reaped and no group member remains; else SIGTERM to the group
// and up to grace for all of it to be gone, then SIGKILL to the group and
// up to grace again. A leader that exited first while descendants stay in
// its group is still stopped through the group. ExitVerified is true only
// when the leader's reap and the group's end were both observed.
func stopProc(name string, p Proc, clock Clock, grace time.Duration) ProcRecord {
	rec := ProcRecord{Name: name, PID: p.PID()}
	if awaitGone(p, clock, 0) {
		rec.StoppedBy, rec.ExitVerified, rec.Exit = StoppedExited, true, exitText(p)
		return rec
	}
	rec.StoppedBy = StoppedTerm
	p.Signal(syscall.SIGTERM)
	if awaitGone(p, clock, grace) {
		rec.ExitVerified, rec.Exit = true, exitText(p)
		return rec
	}
	rec.StoppedBy = StoppedKill
	p.Signal(syscall.SIGKILL)
	if awaitGone(p, clock, grace) {
		rec.ExitVerified, rec.Exit = true, exitText(p)
	}
	return rec
}

// limitBuffer keeps the first limit bytes written and counts the rest; it
// is safe for the copying goroutines of os/exec.
type limitBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	total     int64
	truncated bool
}

func newLimitBuffer(limit int) *limitBuffer { return &limitBuffer{limit: limit} }

func (b *limitBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += int64(len(p))
	room := b.limit - b.buf.Len()
	if room < len(p) {
		b.truncated = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *limitBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func (b *limitBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// CommandResult is one bounded short command's outcome.
type CommandResult struct {
	Stdout, Stderr []byte
	Truncated      bool
	ExitCode       int
	Signal         string
	TimedOut       bool
	Duration       time.Duration
	Stop           ProcRecord
	// Proc is the captured handle (kept for cleanup when Stop is
	// unverified).
	Proc Proc
}

// errCommandStart wraps a start failure.
var errCommandStart = errors.New("the command could not be started")

// errUnverifiedStop is a command whose process group could not be proved
// gone: the command failed, and its handle must be kept for cleanup.
var errUnverifiedStop = errors.New("the command's process group could not be proved gone")

// runCommand runs a short command with output bounded to limit bytes per
// stream and a timeout measured from before its start, after which its
// process group is stopped by its handle. Even a command that ended on
// its own has its group stopped; an unverified stop is an error.
func runCommand(l Launcher, clock Clock, s ProcSpec, timeout time.Duration, limit int) (CommandResult, error) {
	out, errOut := newLimitBuffer(limit), newLimitBuffer(limit)
	s.Stdout, s.Stderr = out, errOut
	start := clock.Now()
	p, err := l.Start(s)
	if err != nil {
		return CommandResult{}, errors.Join(errCommandStart, err)
	}
	res := CommandResult{Proc: p}
	if !awaitDone(p, clock, max(0, timeout-clock.Now().Sub(start))) {
		res.TimedOut = true
	}
	res.Stop = stopProc(s.Path, p, clock, 5*time.Second)
	res.Duration = clock.Now().Sub(start)
	res.ExitCode, res.Signal = p.Exit()
	res.Stdout, res.Stderr = out.Bytes(), errOut.Bytes()
	res.Truncated = out.Truncated() || errOut.Truncated()
	if !res.Stop.ExitVerified {
		return res, errUnverifiedStop
	}
	return res, nil
}
