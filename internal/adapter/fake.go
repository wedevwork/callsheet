package adapter

import (
	"context"
	"errors"
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
type prober struct {
	dir     string
	environ func() []string
	clock   clock
	// started and waited, when non-nil, observe the direct child (tests).
	started func(pid int)
	waited  func(*os.ProcessState)
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
	cmd := exec.CommandContext(pctx, executable, ProbeArg)
	cmd.Dir = p.dir
	cmd.Env = filterEnv(p.environ())
	cmd.WaitDelay = probeWaitDelay
	stdout, stderr := &capture{max: probeCapture}, &capture{max: probeCapture}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return probeErr("cannot be executed")
	}
	if p.started != nil {
		p.started(cmd.Process.Pid)
	}
	werr := cmd.Wait()
	done := p.clock.Now()
	if p.waited != nil {
		p.waited(cmd.ProcessState)
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
	case errOut != "":
		return probeErr("wrote to stderr during the probe")
	case out != ProbeOutput:
		return probeErr("is not the callsheet fake adapter (unexpected probe output)")
	}
	return nil
}
