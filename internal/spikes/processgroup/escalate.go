//go:build linux || darwin

package processgroup

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

const (
	// Grace is this spike's TERM-to-KILL grace period. It is not the
	// production cancel default (iteration 06).
	Grace = 200 * time.Millisecond
	// ProbeLead is how long before the deadline liveness is observed.
	ProbeLead = 25 * time.Millisecond
)

// Signaler sends a signal; a negative pid addresses a process group.
type Signaler interface {
	Signal(pid int, sig syscall.Signal) error
}

// Clock is the time seam.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// SysSignaler uses kill(2).
type SysSignaler struct{}

// Signal implements Signaler with syscall.Kill.
func (SysSignaler) Signal(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// RealClock uses the time package.
type RealClock struct{}

// Now returns time.Now.
func (RealClock) Now() time.Time { return time.Now() }

// After returns time.After.
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Plan describes one TERM->KILL escalation against a process group.
type Plan struct {
	PGID      int
	Grace     time.Duration
	ProbeLead time.Duration
	Sig       Signaler
	Clock     Clock
	// Done is closed once every tracked member has exited and been reaped.
	Done <-chan struct{}
	// BeforeDeadline observes liveness immediately before the deadline; it
	// runs only if Done has not closed by then.
	BeforeDeadline func() error
}

// Outcome records what the escalation did and when.
type Outcome struct {
	TermSentAt    time.Time
	Deadline      time.Time
	Probed        bool
	ProbedAt      time.Time
	ProbeErr      error
	KillSent      bool
	KillSentAt    time.Time
	AlreadyExited bool
}

// ErrInvalidGroup rejects group ids that would address the caller, every
// process, or init.
var ErrInvalidGroup = errors.New("processgroup: refusing to signal pgid <= 1")

// Escalate sends TERM to the group, waits up to Grace for Done, observes
// liveness just before the deadline, and sends KILL to the group at or after
// the deadline. ESRCH on TERM means the group is already gone; ESRCH on KILL
// means it vanished at the deadline. Any other signal error is returned.
// If ctx ends first the group is KILLed and ctx's error returned.
func Escalate(ctx context.Context, p Plan) (Outcome, error) {
	var out Outcome
	if p.PGID <= 1 {
		return out, ErrInvalidGroup
	}
	if p.Grace <= 0 {
		p.Grace = Grace
	}
	if p.ProbeLead <= 0 || p.ProbeLead >= p.Grace {
		p.ProbeLead = min(ProbeLead, p.Grace/2)
	}
	// Stamp before sending: every effect of the TERM is at or after TermSentAt.
	termAt := p.Clock.Now()
	if err := p.Sig.Signal(-p.PGID, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			out.AlreadyExited = true
			return out, nil
		}
		return out, fmt.Errorf("processgroup: TERM group %d: %w", p.PGID, err)
	}
	out.TermSentAt = termAt
	out.Deadline = out.TermSentAt.Add(p.Grace)

	select {
	case <-p.Done:
		return out, nil
	case <-ctx.Done():
		return out, kill(&out, p, ctx.Err())
	case <-p.Clock.After(p.Grace - p.ProbeLead):
	}
	select {
	case <-p.Done:
		return out, nil
	default:
	}
	if p.BeforeDeadline != nil {
		out.Probed = true
		out.ProbedAt = p.Clock.Now()
		out.ProbeErr = p.BeforeDeadline()
	}
	for {
		remaining := out.Deadline.Sub(p.Clock.Now())
		if remaining <= 0 {
			break
		}
		select {
		case <-p.Done:
			return out, nil
		case <-ctx.Done():
			return out, kill(&out, p, ctx.Err())
		case <-p.Clock.After(remaining):
		}
	}
	return out, kill(&out, p, nil)
}

func kill(out *Outcome, p Plan, cause error) error {
	// Stamp before sending: the KILL's effects (members dying, the lifetime
	// pipe closing) can land before a post-send stamp would.
	out.KillSentAt = p.Clock.Now()
	err := p.Sig.Signal(-p.PGID, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return errors.Join(cause, fmt.Errorf("processgroup: KILL group %d: %w", p.PGID, err))
	}
	out.KillSent = err == nil
	return cause
}

// Existence reports whether kill(target, 0) shows a live target: nil error
// means it exists, ESRCH means it does not; anything else (EPERM included) is
// an error, never proof of absence.
func Existence(sig Signaler, target int) (bool, error) {
	err := sig.Signal(target, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

// WaitGone polls until every target reports ESRCH or the limit passes.
func WaitGone(sig Signaler, clock Clock, limit, every time.Duration, targets ...int) error {
	deadline := clock.Now().Add(limit)
	for {
		var alive []int
		for _, t := range targets {
			ok, err := Existence(sig, t)
			if err != nil {
				return fmt.Errorf("processgroup: probe %d: %w", t, err)
			}
			if ok {
				alive = append(alive, t)
			}
		}
		if len(alive) == 0 {
			return nil
		}
		if !clock.Now().Before(deadline) {
			return fmt.Errorf("processgroup: still present after %v: %v", limit, alive)
		}
		<-clock.After(every)
	}
}
