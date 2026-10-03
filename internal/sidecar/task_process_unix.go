//go:build linux || darwin

package sidecar

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
)

// Group teardown bounds (design 05, Execution; 06a keeps them): one-second
// TERM grace in the guardian, five-second disappearance bound polled every
// 5 ms by the sidecar.
const (
	groupGrace    = time.Second
	groupGoneWait = 5 * time.Second
	groupPoll     = 5 * time.Millisecond
	// commandDeadline bounds one control FIFO request.
	commandDeadline = time.Second
)

// execGuardian is a real guardian child: the callsheet binary with
// GuardianToken, Setpgid (PGID = PID), no setsid, and exactly the fds 3-9
// of its descriptor table.
type execGuardian struct {
	spec  guardianSpec
	cmd   *exec.Cmd
	invW  *os.File
	lifeW *os.File
	relW  *os.File
	statR *os.File
	msgs  chan contract.GuardianStatus
	diag  *tailWriter

	lifeOnce, relOnce sync.Once
}

func newExecGuardian(spec guardianSpec) guardianProc {
	return &execGuardian{spec: spec, msgs: make(chan contract.GuardianStatus, 8), diag: &tailWriter{max: maxGuardianDiag * 4}}
}

// tailWriter keeps the last max bytes written (bounded diagnostics).
type tailWriter struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (g *execGuardian) Start() error {
	inv, err := contract.EncodeGuardianInvocation(g.spec.inv)
	if err != nil {
		g.spec.proc.closeEnds()
		return err
	}
	var child []*os.File
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		return r, w, err
	}
	invR, invW, err1 := pipe()
	lifeR, lifeW, err2 := pipe()
	relR, relW, err3 := pipe()
	statR, statW, err4 := pipe()
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		for _, f := range []*os.File{invR, invW, lifeR, lifeW, relR, relW, statR, statW} {
			if f != nil {
				f.Close()
			}
		}
		g.spec.proc.closeEnds()
		return err
	}
	child = []*os.File{invR, lifeR, relR, statW, g.spec.proc.stdin, g.spec.proc.stdout, g.spec.proc.stderr}
	g.invW, g.lifeW, g.relW, g.statR = invW, lifeW, relW, statR
	g.cmd = &exec.Cmd{Path: g.spec.exe, Args: []string{g.spec.exe, GuardianToken}, Env: []string{}, Stderr: g.diag,
		ExtraFiles: child, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	err = g.cmd.Start()
	// The child holds its own copies now (or none exists): the parent's
	// child ends close either way.
	for _, f := range child {
		f.Close()
	}
	if err != nil {
		for _, f := range []*os.File{invW, lifeW, relW, statR} {
			f.Close()
		}
		close(g.msgs)
		return err
	}
	go func() {
		contract.WriteFramed(invW, inv)
		invW.Close()
	}()
	go func() {
		defer close(g.msgs)
		for {
			b, err := contract.ReadFramed(statR, contract.MaxGuardianStatusBytes)
			if err != nil {
				return
			}
			s, err := contract.ParseGuardianStatus(b)
			if err != nil || s.TaskID != g.spec.inv.TaskID || s.Nonce != g.spec.inv.Nonce {
				return
			}
			g.msgs <- s
		}
	}()
	return nil
}

func (g *execGuardian) PID() int                               { return g.cmd.Process.Pid }
func (g *execGuardian) Status() <-chan contract.GuardianStatus { return g.msgs }

func (g *execGuardian) Release() {
	g.relOnce.Do(func() {
		g.relW.Write([]byte{0x01})
		g.relW.Close()
	})
}

func (g *execGuardian) Revoke() { g.relOnce.Do(func() { g.relW.Close() }) }

// Control asks the live guardian to cancel through its control FIFO: the
// version-2 command with cause cancelled and the plane's stop ID.
func (g *execGuardian) Control(stopID string) error {
	cmd, err := contract.EncodeGuardianCommand(contract.GuardianCommand{TaskID: g.spec.inv.TaskID, Execution: g.spec.inv.Execution,
		Nonce: g.spec.inv.Nonce, Command: contract.GuardianStop, Cause: contract.CauseCancelled, StopID: stopID})
	if err != nil {
		return err
	}
	return sendCommand(filepath.Join(g.spec.inv.TaskDir, controlName), cmd)
}
func (g *execGuardian) Stop() { g.lifeOnce.Do(func() { g.lifeW.Close() }) }

func (g *execGuardian) Wait() procExit {
	err := g.cmd.Wait()
	g.Stop()
	g.Revoke()
	g.statR.Close()
	var ws syscall.WaitStatus
	ok := false
	if g.cmd.ProcessState != nil {
		ws, ok = g.cmd.ProcessState.Sys().(syscall.WaitStatus)
	}
	var ee *exec.ExitError
	return waitOutcome(err, errors.As(err, &ee), ws, ok)
}

// closeEnds closes an adapter spec's pipe ends (a launch that failed
// before handing them over).
func (p procSpec) closeEnds() {
	for _, f := range []*os.File{p.stdin, p.stdout, p.stderr} {
		if f != nil {
			f.Close()
		}
	}
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

// realGroups observes run-owned task groups with the qualified
// process-group mechanics of internal/spikes/processgroup (WaitGone and
// Existence over kill(2) with signal 0): never TERM or KILL against a raw
// PGID. Its signaler and clock are kill(2) and real time unless injected.
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

func (g realGroups) gone(pgid int) error {
	if pgid <= 1 {
		return processgroup.ErrInvalidGroup
	}
	sig, clk := g.parts()
	return processgroup.WaitGone(sig, clk, groupGoneWait, groupPoll, -pgid)
}

func (g realGroups) exists(pgid int) (bool, error) {
	if pgid <= 1 {
		return false, processgroup.ErrInvalidGroup
	}
	sig, _ := g.parts()
	return processgroup.Existence(sig, -pgid)
}

// sendCommand writes one control command to fifo: a nonblocking open
// (ENXIO: no reader, so no guardian) and one write below PIPE_BUF within
// commandDeadline.
func sendCommand(fifo string, cmd []byte) error {
	fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENXIO) {
		return errNoGuardian
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), fifo)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		return fmt.Errorf("%s is not a FIFO", fifo)
	}
	f.SetWriteDeadline(time.Now().Add(commandDeadline))
	_, err = f.Write(cmd)
	return err
}

// defaultGuardianEnv is the guardian's production OS seam.
func defaultGuardianEnv() guardianEnv {
	return guardianEnv{
		fds:     inheritedFDs,
		getpid:  os.Getpid,
		getpgrp: unix.Getpgrp,
		mkfifo:  func(p string) error { return unix.Mkfifo(p, 0o600) },
		startAdapter: func(inv contract.GuardianInvocation, stdin, stdout, stderr *os.File) (adapterRun, error) {
			// The adapter inherits the guardian's group: no Setpgid, no
			// setsid; only its three stdio descriptors are passed.
			cmd := &exec.Cmd{Path: inv.Path, Args: append([]string{inv.Path}, inv.Argv...), Env: inv.Env, Dir: inv.Dir,
				Stdin: stdin, Stdout: stdout, Stderr: stderr}
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return execRun{cmd}, nil
		},
		notifyTerm: func(c chan<- os.Signal) func() {
			signal.Notify(c, syscall.SIGTERM)
			return func() { signal.Stop(c) }
		},
		sig:    processgroup.SysSignaler{},
		clock:  processgroup.RealClock{},
		grace:  groupGrace,
		now:    func() time.Time { return time.Now().UTC() },
		lookup: adapter.Lookup(),
		timerAt: func(at time.Time) (<-chan time.Time, func() bool) {
			t := time.NewTimer(time.Until(at))
			return t.C, t.Stop
		},
		// Iteration 10a: the build-selected native group-completion proof
		// (group_alone_linux.go, group_alone_darwin.go).
		subreap:    enableGuardianSubreaper,
		groupAlone: probeGuardianGroup,
	}
}

// execRun is a started adapter.
type execRun struct{ cmd *exec.Cmd }

func (r execRun) PID() int { return r.cmd.Process.Pid }

func (r execRun) Wait() procExit {
	err := r.cmd.Wait()
	var ws syscall.WaitStatus
	ok := false
	if r.cmd.ProcessState != nil {
		ws, ok = r.cmd.ProcessState.Sys().(syscall.WaitStatus)
	}
	var ee *exec.ExitError
	return waitOutcome(err, errors.As(err, &ee), ws, ok)
}

// guardianFDLayout is the fixed descriptor table: fd, direction (true:
// the guardian's write end) and name.
var guardianFDLayout = []struct {
	fd    int
	write bool
	name  string
}{{3, false, "invocation"}, {4, false, "lifetime"}, {5, false, "release"}, {6, true, "status"}, {7, false, "adapter stdin"},
	{8, true, "adapter stdout"}, {9, true, "adapter stderr"}}

// inheritedFDs validates and adopts fds 3-9: each open, a pipe, of the
// right direction (the native access mode), and no two the same
// underlying pipe; every one is made close-on-exec so the adapter inherits
// only its explicit stdio.
func inheritedFDs() (guardianFDs, error) {
	adopt, blocking := lifetimeAdopter(lifetimeFD, unix.SetNonblock)
	fds, err := checkFDs(unix.FcntlInt, unix.Fstat, unix.CloseOnExec, adopt)
	fds.lifeBlocking = blocking()
	return fds, err
}

// lifetimeFD is the parent-lifetime pipe's descriptor (guardianFDLayout).
const lifetimeFD = 4

// lifetimeAdopter returns checkFDs' adoption of each validated descriptor
// and a report of its one possible failure. The lifetime pipe's read end
// (lifetime) is the guardian's alone (its parent closed its copy; the
// adapter inherits only its stdio): it is made nonblocking first, so the
// runtime poller serves it and the guardian's ordinary return (iteration
// 10a) can close it to join its reader. If that fails the descriptor stays
// blocking, which the report says: the guardian then disables its early
// completion (its cleanup ends by its group KILL, as before). Every other
// descriptor is adopted as inherited.
func lifetimeAdopter(lifetime uintptr, setNonblock func(fd int, nonblocking bool) error) (func(fd uintptr, name string) *os.File, func() bool) {
	failed := false
	return func(fd uintptr, name string) *os.File {
		if fd == lifetime && setNonblock(int(fd), true) != nil {
			failed = true
		}
		return os.NewFile(fd, name)
	}, func() bool { return failed }
}

// checkFDs is inheritedFDs over injectable primitives.
func checkFDs(fcntl func(fd uintptr, cmd, arg int) (int, error), fstat func(fd int, st *unix.Stat_t) error, cloexec func(fd int),
	newFile func(fd uintptr, name string) *os.File) (guardianFDs, error) {
	seen := map[[2]uint64]bool{}
	files := make([]*os.File, 0, len(guardianFDLayout))
	for _, want := range guardianFDLayout {
		flags, err := fcntl(uintptr(want.fd), unix.F_GETFL, 0)
		if err != nil {
			return guardianFDs{}, fmt.Errorf("fd %d (%s) is missing", want.fd, want.name)
		}
		mode := flags & unix.O_ACCMODE
		if (want.write && mode != unix.O_WRONLY) || (!want.write && mode != unix.O_RDONLY) {
			return guardianFDs{}, fmt.Errorf("fd %d (%s) is the wrong end of its pipe", want.fd, want.name)
		}
		var st unix.Stat_t
		if err := fstat(want.fd, &st); err != nil {
			return guardianFDs{}, fmt.Errorf("fd %d (%s) cannot be inspected", want.fd, want.name)
		}
		if uint32(st.Mode)&unix.S_IFMT != unix.S_IFIFO {
			return guardianFDs{}, fmt.Errorf("fd %d (%s) is not a pipe", want.fd, want.name)
		}
		key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
		if seen[key] {
			return guardianFDs{}, fmt.Errorf("fd %d (%s) duplicates another descriptor's pipe", want.fd, want.name)
		}
		seen[key] = true
		cloexec(want.fd)
		files = append(files, newFile(uintptr(want.fd), want.name))
	}
	return guardianFDs{inv: files[0], life: files[1], release: files[2], status: files[3], stdin: files[4], stdout: files[5], stderr: files[6]}, nil
}
