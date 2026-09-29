//go:build linux || darwin

package mcpqual

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
)

// SysSignaler signals with kill(2).
type SysSignaler = processgroup.SysSignaler

// GroupReaper reaps one process group through processgroup's exported
// Escalate, WaitGone and Existence with an injected Signaler and Clock.
type GroupReaper struct {
	Sig    Signaler
	Clock  Clock
	Policy CleanupPolicy
}

// Reap stops p's group: TERM, the grace, KILL, then Wait for the leader and
// ESRCH for the group. Its context is fresh, independent of the canceled
// run. Done closes only after the leader was reaped and absence proven;
// a probe error or a persistent EPERM ends the escalation with KILL and is
// reported as a cleanup failure, never as absence.
func (g GroupReaper) Reap(p Proc) CaseCleanup {
	var cc CaseCleanup
	pgid := p.PGID()
	if pgid <= 1 {
		return CaseCleanup{Error: sptr("cleanup: " + processgroup.ErrInvalidGroup.Error())}
	}
	pc := pgClock{g.Clock}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	absent := make(chan error, 1)
	go func() {
		tc, stop := g.Clock.NewTimer(g.Policy.Grace + g.Policy.Limit)
		defer stop()
		select {
		case <-p.Exited():
		case <-tc:
			absent <- fmt.Errorf("leader %d was not reaped within %v", pgid, g.Policy.Grace+g.Policy.Limit)
			cancel()
			return
		}
		if alive, err := processgroup.Existence(g.Sig, -pgid); err == nil && alive {
			cc.LeaderExitedFirst = true
		}
		if err := processgroup.WaitGone(g.Sig, pc, g.Policy.Limit, g.Policy.Poll, -pgid); err != nil {
			absent <- err
			cancel()
			return
		}
		close(done)
		absent <- nil
	}()
	out, err := processgroup.Escalate(ctx, processgroup.Plan{PGID: pgid, Grace: g.Policy.Grace, Sig: g.Sig, Clock: pc, Done: done})
	aerr := <-absent
	cc.TermSent = !out.TermSentAt.IsZero()
	cc.KillSent = out.KillSent
	switch {
	case aerr != nil:
		cc.Error = sptr("cleanup: " + aerr.Error())
	case err != nil && !errors.Is(err, context.Canceled):
		cc.Error = sptr("cleanup: " + err.Error())
	default:
		cc.GroupGone = true
	}
	return cc
}

// pgClock adapts a Clock to processgroup.Clock.
type pgClock struct{ c Clock }

func (p pgClock) Now() time.Time { return p.c.Now() }

func (p pgClock) After(d time.Duration) <-chan time.Time {
	c, _ := p.c.NewTimer(d)
	return c
}

var _ processgroup.Clock = pgClock{}
