//go:build linux || darwin

// Package procexec is the qualification harness's real process launcher
// (iteration 07b, FP-15): each launch leads its own process group, stdout
// is a pipe the harness owns and stderr a file, so the leader's Wait never
// waits for a descendant that keeps a stream open. It is separate from
// internal/mcpqual so that package's repeated tests stay free of real
// subprocesses; its own tests start short-lived real processes.
package procexec

import (
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/wedevwork/callsheet/internal/mcpqual"
)

// Launcher implements mcpqual.Launcher with os/exec.
type Launcher struct{}

type proc struct {
	cmd    *exec.Cmd
	out    *os.File
	exited chan struct{}
	state  *os.ProcessState
}

// Start implements mcpqual.Launcher.
func (Launcher) Start(s mcpqual.ProcSpec) (mcpqual.Proc, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	var errFile *os.File
	if s.StderrPath != "" {
		if errFile, err = os.OpenFile(s.StderrPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
			r.Close()
			w.Close()
			return nil, err
		}
		defer errFile.Close()
	}
	cmd := &exec.Cmd{Path: s.Path, Args: append([]string{s.Path}, s.Args...), Env: s.Env, Dir: s.Dir, Stdout: w,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	if errFile != nil {
		cmd.Stderr = errFile
	}
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	w.Close()
	p := &proc{cmd: cmd, out: r, exited: make(chan struct{})}
	go func() {
		cmd.Wait()
		p.state = cmd.ProcessState
		close(p.exited)
	}()
	return p, nil
}

func (p *proc) PGID() int               { return p.cmd.Process.Pid }
func (p *proc) Stdout() io.Reader       { return p.out }
func (p *proc) Exited() <-chan struct{} { return p.exited }
func (p *proc) CloseStdout()            { p.out.Close() }

// Status is the exit code, or the signal name when a signal ended the
// leader; both are nil until the leader was reaped.
func (p *proc) Status() (*int, *string) {
	select {
	case <-p.exited:
	default:
		return nil, nil
	}
	if ws, ok := p.state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		name := ws.Signal().String()
		return nil, &name
	}
	code := p.state.ExitCode()
	return &code, nil
}
