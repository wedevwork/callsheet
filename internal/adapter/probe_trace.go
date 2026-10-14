package adapter

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Probe diagnostics for tests of other packages (the sidecar's real-process
// qualification): which instants a failed ready probe crossed. Only tests
// construct a traced fake; no production path does (TestProbeTraceSeam
// guards it), and a traced probe runs the same prober, with the same
// deadline and outcome as NewFake's, on the wall clock or on a clock the
// test injects (the prober's injected-clock seam).

// ProbeTrace records the latest probe of a traced fake. The zero value is
// ready; it is safe for concurrent use.
type ProbeTrace struct {
	mu   sync.Mutex
	last ProbeTiming
}

// ProbeTiming is one probe's observed instants.
type ProbeTiming struct {
	// Deadline is the probe deadline on the probe's clock; it started
	// ProbeTimeout before.
	Deadline time.Time
	// Armed is the wall-clock instant the deadline was armed. Armed,
	// Started and Waited are wall-clock instants whatever the probe's
	// clock.
	Armed time.Time
	// Started is when cmd.Start returned a started child (zero if none).
	Started time.Time
	// Waited is when cmd.Wait returned, just after the completion instant
	// was sampled, and State the child's state then: before the deadline
	// check, which it is compared with (nil if no child was waited for).
	Waited time.Time
	State  *os.ProcessState
}

// Last returns the latest probe's timing.
func (tr *ProbeTrace) Last() ProbeTiming {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.last
}

func (tr *ProbeTrace) update(f func(*ProbeTiming)) {
	tr.mu.Lock()
	f(&tr.last)
	tr.mu.Unlock()
}

// String reports the elapsed wall-clock time from the deadline start
// until cmd.Start returned and until cmd.Wait returned, and whether the
// child had already exited successfully when the deadline check fired.
func (p ProbeTiming) String() string {
	if p.Deadline.IsZero() {
		return "no probe deadline was armed"
	}
	start := p.Armed
	s := "deadline start to cmd.Start returned: "
	if p.Started.IsZero() {
		s += "no child started"
	} else {
		s += p.Started.Sub(start).String()
	}
	if p.State == nil {
		return s + "; no child was waited for"
	}
	return s + fmt.Sprintf("; to cmd.Wait returned: %v (deadline %v); child exited successfully before the deadline check: %t (%v)",
		p.Waited.Sub(start), ProbeTimeout, p.State.Success(), p.State)
}

// tracingClock is a probe clock recording the deadline it arms and the
// wall-clock instant it armed it.
type tracingClock struct {
	clock
	tr *ProbeTrace
}

func (c tracingClock) NewTimerAt(at time.Time) (<-chan time.Time, func() bool) {
	now := time.Now()
	c.tr.update(func(p *ProbeTiming) { *p = ProbeTiming{Deadline: at, Armed: now} })
	return c.clock.NewTimerAt(at)
}

// NewFakeTraced is NewFake whose probes record their timing in tr (tests
// only).
func NewFakeTraced(dir string, tr *ProbeTrace) Adapter {
	return NewFakeTracedClock(dir, tr, realClock{})
}

// NewFakeTracedClock is NewFakeTraced whose probe deadline runs on clk, a
// test's injected clock (tests only): the probe is otherwise NewFake's.
func NewFakeTracedClock(dir string, tr *ProbeTrace, clk interface {
	Now() time.Time
	NewTimerAt(at time.Time) (<-chan time.Time, func() bool)
}) Adapter {
	return &fake{p: &prober{dir: dir, environ: os.Environ, clock: tracingClock{clock: clk, tr: tr},
		started: func(int) {
			now := time.Now()
			tr.update(func(p *ProbeTiming) { p.Started = now })
		},
		waited: func(ps *os.ProcessState) {
			now := time.Now()
			tr.update(func(p *ProbeTiming) { p.Waited, p.State = now, ps })
		}}}
}
