package adapter

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The fake adapter (iteration 04): a test/demo adapter that never calls a
// model. Production code knows only its probe argv and output; it does not
// import the fixture package.
const (
	// FakeID is the fake adapter's ID.
	FakeID = "fake"
	// ProbeArg is the fake's exclusive probe mode argument.
	ProbeArg = "--callsheet-probe"
	// ProbeOutput is the exact stdout of a successful probe.
	ProbeOutput = "callsheet-fake-probe-v1\n"

	// ProbeTimeout bounds one probe on the injected clock.
	ProbeTimeout = time.Second
	// probeCapture bounds each of stdout and stderr.
	probeCapture = 4 << 10
	// probeWaitDelay closes inherited output pipes a misconfigured
	// executable leaves open after it exits.
	probeWaitDelay = 100 * time.Millisecond
)

// fakeEfforts are the fake's allowed efforts, in order.
var fakeEfforts = []string{"low", "medium", "high"}

// scrubbedEnv are the fixture file-descriptor variables removed from every
// probe environment (exact keys, duplicates included).
var scrubbedEnv = []string{"CALLSHEET_FAKE_DESCENDANT_LIFETIME_FD", "CALLSHEET_FAKE_READY_FD"}

// ProbeError is a probe failure with a fixed, safe reason: never child
// output or file contents.
type ProbeError struct {
	Reason string
}

func (e *ProbeError) Error() string { return "the adapter executable " + e.Reason }

func probeErr(reason string) error { return &ProbeError{Reason: reason} }

// clock is the probe deadline's time source (injectable for tests).
type clock interface {
	Now() time.Time
	NewTimerAt(at time.Time) (<-chan time.Time, func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimerAt(at time.Time) (<-chan time.Time, func() bool) {
	t := time.NewTimer(time.Until(at))
	return t.C, t.Stop
}

// prober runs one executable probe: private, injectable dependencies.
// args and check generalize the fake's probe (iteration 08): nil args is
// the fake's ProbeArg and nil check its exact stdout/no-stderr predicate.
type prober struct {
	dir     string
	environ func() []string
	clock   clock
	// started and waited, when non-nil, observe the direct child (tests).
	started func(pid int)
	waited  func(*os.ProcessState)
	// runner, when non-nil, replaces the real process start (vendor probe
	// tests only; the fake keeps its real-process coverage).
	runner procRunner
	// args is the probe argv after the executable; check decides the
	// captured output of a zero exit, returning a fixed failure reason or
	// "".
	args  []string
	check func(stdout, stderr string) string
}

// procRunner starts one probe child (injectable for vendor probe tests).
type procRunner interface {
	start(ctx context.Context, exe string, args []string, dir string, env []string, stdout, stderr io.Writer, waitDelay time.Duration) (probeProc, error)
}

// probeProc is a started probe child: Wait is called exactly once.
type probeProc interface {
	pid() int
	wait() (error, *os.ProcessState)
}

// execRunner is the real procRunner (exec.CommandContext, no shell).
type execRunner struct{}

type execProc struct{ cmd *exec.Cmd }

func (execRunner) start(ctx context.Context, exe string, args []string, dir string, env []string, stdout, stderr io.Writer, waitDelay time.Duration) (probeProc, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = waitDelay
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return execProc{cmd: cmd}, nil
}

func (p execProc) pid() int { return p.cmd.Process.Pid }

func (p execProc) wait() (error, *os.ProcessState) {
	err := p.cmd.Wait()
	return err, p.cmd.ProcessState
}

// fakeCheck is the fake's exact probe predicate: nothing on stderr and
// exactly ProbeOutput on stdout.
func fakeCheck(out, errOut string) string {
	switch {
	case errOut != "":
		return "wrote to stderr during the probe"
	case out != ProbeOutput:
		return "is not the callsheet fake adapter (unexpected probe output)"
	}
	return ""
}

type fake struct{ p *prober }

// NewFake returns the fake adapter whose probes run in dir with the
// process environment (minus the fixture variables).
func NewFake(dir string) Adapter {
	return &fake{p: &prober{dir: dir, environ: os.Environ, clock: realClock{}}}
}

func (f *fake) Descriptor() Descriptor {
	return Descriptor{ID: FakeID, Efforts: append([]string(nil), fakeEfforts...), TestOnly: true}
}

// Probe checks that executable, an absolute path used as given (symlinks
// followed natively, no PATH lookup), names a regular file with an
// executable mode bit, then actually runs "executable --callsheet-probe"
// (direct argv, no shell) within ProbeTimeout: it must exit 0 with exactly
// ProbeOutput on stdout and nothing on stderr. The direct child is killed
// and waited for on cancellation or timeout.
func (f *fake) Probe(ctx context.Context, executable string) error {
	return f.p.run(ctx, executable)
}

// filterEnv removes every entry whose key is exactly one of scrubbedEnv.
func filterEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, s := range scrubbedEnv {
			if key == s {
				drop = true
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// capture keeps at most max bytes, drains the rest and records overflow.
type capture struct {
	mu       sync.Mutex
	max      int
	b        []byte
	overflow bool
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - len(c.b); len(p) > room {
		c.b = append(c.b, p[:max(room, 0)]...)
		c.overflow = true
	} else {
		c.b = append(c.b, p...)
	}
	return len(p), nil
}

func (c *capture) result() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.b), c.overflow
}

func (p *prober) run(ctx context.Context, executable string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(executable) {
		return probeErr("path is not absolute")
	}
	fi, err := os.Stat(executable)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return probeErr("does not exist (or is a broken symbolic link)")
	case errors.Is(err, fs.ErrPermission):
		return probeErr("cannot be accessed: permission denied")
	case err != nil:
		return probeErr("cannot be inspected")
	case !fi.Mode().IsRegular():
		return probeErr("is not a regular file")
	case fi.Mode().Perm()&0o111 == 0:
		return probeErr("has no executable permission bit")
	}
	// The deadline starts before the child does; the watcher only kills
	// the child. Success is decided by the completion instant sampled when
	// Wait returns (strictly before the deadline), never by which of the
	// timer and the exit was observed first.
	deadline := p.clock.Now().Add(ProbeTimeout)
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer, stop := p.clock.NewTimerAt(deadline)
	defer stop()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-timer:
			cancel()
		case <-pctx.Done():
		}
	}()
	defer func() { cancel(); <-watchDone }()
	args, check, run := p.args, p.check, p.runner
	if args == nil {
		args = []string{ProbeArg}
	}
	if check == nil {
		check = fakeCheck
	}
	if run == nil {
		run = execRunner{}
	}
	stdout, stderr := &capture{max: probeCapture}, &capture{max: probeCapture}
	proc, err := run.start(pctx, executable, append([]string(nil), args...), p.dir, filterEnv(p.environ()), stdout, stderr, probeWaitDelay)
	if err != nil {
		return probeErr("cannot be executed")
	}
	if p.started != nil {
		p.started(proc.pid())
	}
	werr, state := proc.wait()
	done := p.clock.Now()
	if p.waited != nil {
		p.waited(state)
	}
	// Precedence: the caller's cancellation, then the probe deadline, then
	// the child's own result.
	if err := ctx.Err(); err != nil {
		return err
	}
	if !done.Before(deadline) {
		return probeErr("did not answer within " + ProbeTimeout.String())
	}
	out, outOver := stdout.result()
	errOut, errOver := stderr.result()
	switch {
	case errors.Is(werr, exec.ErrWaitDelay):
		return probeErr("left its output open after exiting")
	case werr != nil:
		return probeErr("exited unsuccessfully")
	case outOver || errOver:
		return probeErr("wrote more output than a probe allows")
	}
	if reason := check(out, errOut); reason != "" {
		return probeErr(reason)
	}
	return nil
}

// ChildEnv returns env without the fixture file-descriptor variables
// (every exact occurrence), the filter probes use; task children get the
// same (iteration 05).
func ChildEnv(env []string) []string { return filterEnv(env) }
