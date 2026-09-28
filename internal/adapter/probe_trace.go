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
// guards it), and a traced probe runs the same prober, on the same wall
// clock, with the same deadline and outcome as NewFake's.

// ProbeTrace records the latest probe of a traced fake. The zero value is
// ready; it is safe for concurrent use.
type ProbeTrace struct {
	mu   sync.Mutex
	last ProbeTiming
}

// ProbeTiming is one probe's observed instants.
type ProbeTiming struct {
	// Deadline is the probe deadline; it started ProbeTimeout before.
	Deadline time.Time
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

// String reports the elapsed time from the deadline start until
// cmd.Start returned and until cmd.Wait returned, and whether the child
// had already exited successfully when the deadline check fired.
func (p ProbeTiming) String() string {
	if p.Deadline.IsZero() {
		return "no probe deadline was armed"
	}
	start := p.Deadline.Add(-ProbeTimeout)
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

// tracingClock is the real probe clock, recording the deadline it arms.
type tracingClock struct {
	realClock
	tr *ProbeTrace
}

func (c tracingClock) NewTimerAt(at time.Time) (<-chan time.Time, func() bool) {
	c.tr.update(func(p *ProbeTiming) { *p = ProbeTiming{Deadline: at} })
	return c.realClock.NewTimerAt(at)
}

// NewFakeTraced is NewFake whose probes record their timing in tr (tests
// only).
func NewFakeTraced(dir string, tr *ProbeTrace) Adapter {
	return &fake{p: &prober{dir: dir, environ: os.Environ, clock: tracingClock{tr: tr},
		started: func(int) {
			now := time.Now()
			tr.update(func(p *ProbeTiming) { p.Started = now })
		},
		waited: func(ps *os.ProcessState) {
			now := time.Now()
			tr.update(func(p *ProbeTiming) { p.Waited, p.State = now, ps })
		}}}
}
