//go:build linux || darwin

package sidecar

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
)

// Group teardown bounds (design 05, Execution): one-second TERM grace,
// five-second disappearance bound polled every 5 ms.
const (
	groupGrace    = time.Second
	groupGoneWait = 5 * time.Second
	groupPoll     = 5 * time.Millisecond
)

// execProc is a real child: exec with Setpgid, so its process group ID
// equals its PID and never the supervisor's.
type execProc struct {
	cmd  *exec.Cmd
	spec procSpec
}

func newExecProc(spec procSpec) taskProc {
	cmd := &exec.Cmd{Path: spec.path, Args: append([]string{spec.path}, spec.argv...), Env: spec.env, Dir: spec.dir,
		Stdin: spec.stdin, Stdout: spec.stdout, Stderr: spec.stderr, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	return &execProc{cmd: cmd, spec: spec}
}

func (p *execProc) Start() error {
	err := p.cmd.Start()
	// The child holds its own copies now (or none exists): the parent's
	// child ends close either way, so EOF follows the child's exit.
	p.spec.stdin.Close()
	p.spec.stdout.Close()
	p.spec.stderr.Close()
	return err
}

func (p *execProc) PID() int { return p.cmd.Process.Pid }

func (p *execProc) Wait() procExit {
	err := p.cmd.Wait()
	var ws syscall.WaitStatus
	ok := false
	if p.cmd.ProcessState != nil {
		ws, ok = p.cmd.ProcessState.Sys().(syscall.WaitStatus)
	}
	var ee *exec.ExitError
	return waitOutcome(err, errors.As(err, &ee), ws, ok)
}

// waitOutcome classifies a direct child's wait: a signal death, a normal
// exit code, a wait error (other than the child's own exit status), or
// (never success) an unrecognized status.
func waitOutcome(err error, exitStatus bool, ws syscall.WaitStatus, ok bool) procExit {
	switch {
	case ok && ws.Signaled():
		return procExit{signal: adapter.SignalName(ws.Signal())}
	case ok && ws.Exited():
		return procExit{code: ws.ExitStatus()}
	case err != nil && !exitStatus:
		return procExit{err: err}
	}
	return procExit{err: errors.New("unrecognized wait status")}
}

// realGroups reuses the qualified process-group mechanics of
// internal/spikes/processgroup (Escalate, WaitGone, Existence and the
// kill(2) Signaler): run-owned group teardown only, never its experiment
// helpers or Linux test subreaper.
// Its signaler and clock are kill(2) and real time unless injected (the
// wrapper's own tests drive them without any process).
type realGroups struct {
	sig processgroup.Signaler
	clk processgroup.Clock
}

func (g realGroups) parts() (processgroup.Signaler, processgroup.Clock) {
	sig, clk := g.sig, g.clk
	if sig == nil {
		sig = processgroup.SysSignaler{}
	}
	if clk == nil {
		clk = processgroup.RealClock{}
	}
	return sig, clk
}

func (g realGroups) cleanup(pgid int) error {
	sig, clk := g.parts()
	alive, err := processgroup.Existence(sig, -pgid)
	if err == nil && !alive {
		return nil
	}
	// Members remain, or EPERM (on macOS possibly a zombie-only group,
	// never proof of absence): escalate while polling for disappearance.
	gone := make(chan struct{})
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		if processgroup.WaitGone(sig, clk, groupGrace, groupPoll, -pgid) == nil {
			close(gone)
		}
	}()
	processgroup.Escalate(context.Background(), processgroup.Plan{PGID: pgid, Grace: groupGrace, Sig: sig, Clock: clk, Done: gone})
	<-polled
	return processgroup.WaitGone(sig, clk, groupGoneWait, groupPoll, -pgid)
}

func (g realGroups) terminate(pgid int, exited <-chan struct{}) {
	sig, clk := g.parts()
	processgroup.Escalate(context.Background(), processgroup.Plan{PGID: pgid, Grace: groupGrace, Sig: sig, Clock: clk, Done: exited})
}
