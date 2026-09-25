//go:build linux || darwin

package function

import (
	"context"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestFP6ProcessGroups runs the cooperative, TERM-resistant and
// leader-exits-first experiments inside a dedicated helper process (the
// compiled process-group test binary, a Linux child subreaper) and asserts
// the recorded evidence. On Darwin the same assertions run minus the adopted
// wait statuses, which launchd owns.
func TestFP6ProcessGroups(t *testing.T) {
	fake := testkit.BuildBinary(t, "./cmd/fake-adapter", "fake-adapter")
	helper := testkit.BuildTestBinary(t, "./internal/spikes/processgroup", "processgroup")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := processgroup.RunExperiment(ctx, []string{helper}, os.Environ(), fake, t.TempDir())
	if err != nil {
		t.Fatalf("experiment: %v\n%+v", err, rep)
	}
	linux := runtime.GOOS == "linux"
	if linux && !rep.Subreaper {
		t.Fatal("helper was not a child subreaper")
	}
	if len(rep.Cases) != 3 {
		t.Fatalf("cases = %d", len(rep.Cases))
	}
	own := syscall.Getpgrp()
	for _, c := range rep.Cases {
		t.Run(c.Case.Name, func(t *testing.T) {
			if !c.Pass || len(c.Errors) != 0 {
				t.Fatalf("helper verdict: %v", c.Errors)
			}
			if c.PGID <= 1 || c.PGID == own || c.LeaderPGID != c.PGID || c.DescendantPGID != c.PGID || c.DescendantPID == 0 {
				t.Fatalf("groups: pgid=%d leader=%d descendant=%d own=%d", c.PGID, c.LeaderPGID, c.DescendantPGID, own)
			}
			if !contains(c.LeaderSignals, "SIGTERM") || !contains(c.DescendantSignals, "SIGTERM") {
				t.Fatalf("both must receive TERM: %v %v", c.LeaderSignals, c.DescendantSignals)
			}
			if !c.LeaderGoneESRCH || !c.DescendantGoneESRCH || !c.GroupGoneESRCH || c.EmergencyKill {
				t.Fatalf("teardown: %+v", c)
			}
			pre, hasPre := c.Observation("pre-deadline")
			after, hasAfter := c.Observation("after-leader-exit")
			switch c.Case.Name {
			case "cooperative":
				if c.KillSent || !c.Leader.Exited || c.Leader.ExitCode != 0 {
					t.Fatalf("cooperative: kill=%v leader=%+v", c.KillSent, c.Leader)
				}
				if linux && (!c.Descendant.Exited || c.Descendant.ExitCode != 0) {
					t.Fatalf("descendant = %+v", c.Descendant)
				}
			case "resistant":
				if !c.KillSent || c.KillSentAt.Before(c.Deadline) {
					t.Fatalf("KILL at %v, deadline %v", c.KillSentAt, c.Deadline)
				}
				if !hasPre || pre.LeaderExited || !pre.LeaderAlive || !pre.DescendantAlive || !pre.LifetimeOpen || !pre.At.Before(c.Deadline) {
					t.Fatalf("liveness before deadline not proven: %+v", pre)
				}
				if c.Leader.Signal != "SIGKILL" || (linux && c.Descendant.Signal != "SIGKILL") {
					t.Fatalf("statuses leader=%+v descendant=%+v", c.Leader, c.Descendant)
				}
			case "leader-exits-first":
				if !c.Leader.Exited || c.Leader.ExitCode != 0 {
					t.Fatalf("leader = %+v", c.Leader)
				}
				if !hasAfter || !after.LeaderExited || !after.DescendantAlive || !after.LifetimeOpen {
					t.Fatalf("descendant not alive after leader exit: %+v", after)
				}
				if !hasPre || !pre.LeaderExited || !pre.DescendantAlive || !pre.LifetimeOpen || !pre.At.Before(c.Deadline) || pre.At.Before(after.At) {
					t.Fatalf("descendant not alive immediately before grace expiry: %+v", pre)
				}
				if !c.LeaderExitedAt.Before(pre.At) {
					t.Fatal("leader did not exit before the pre-deadline observation")
				}
				if !c.KillSent || c.KillSentAt.Before(c.Deadline) || c.LifetimeClosedAt.Before(c.KillSentAt) {
					t.Fatalf("group KILL did not end the descendant: kill=%v at %v, lifetime closed %v", c.KillSent, c.KillSentAt, c.LifetimeClosedAt)
				}
				if linux && (c.Descendant.Signal != "SIGKILL" || c.Descendant.ReapedBy != "subreaper") {
					t.Fatalf("adopted descendant = %+v", c.Descendant)
				}
			}
			t.Logf("%s: kill=%v grace=%.1fms leader=%+v descendant=%+v", c.Case.Name, c.KillSent, c.ObservedGraceMillis, c.Leader, c.Descendant)
		})
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
