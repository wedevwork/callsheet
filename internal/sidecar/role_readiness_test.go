package sidecar

import (
	"os"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// awaitCycle waits for a published cycle result for rev.
func (rr *roleRun) awaitCycle(t *testing.T, rev int) []bool {
	t.Helper()
	return rr.ev.awaitMatch(t, evCycleDone, func(ev event) bool { return ev.rev == rev }).passed
}

// TestRoleReadinessContract is UT FP-5 on the worker; its changes subtest
// is delegated from tests/function (TestRoleReadiness/changes). Cycles run
// on the fake clock and are observed through explicit cycle events; the
// clock is moved only in steps whose outcome is fixed. Do not rename or
// skip its subtests.
func TestRoleReadinessContract(t *testing.T) {
	t.Run("changes", func(t *testing.T) {
		t.Parallel()
		// Since iteration 10a every changed status snapshot is reported at
		// once by an ordinary heartbeat; an identical one travels only on
		// the periodic heartbeat.
		fp := startFakePlane(t)
		rr := startRoleRun(t, fp, true)
		c := fp.accept(t)
		c.helloOK(testID)
		rr.beat(c, 1, 0) // T0; b2 due at T0+5s
		c.reconcileEmpty()
		insA, runA := manuals(t, rr.dir, "a", "x")
		insB, runB := manuals(t, rr.dir, "b", "")
		a, b := roleConfig("a", insA, runA), roleConfig("b", insB, runB)
		// Installed at T0+4s while its first cycle is held: the new
		// revision is reported at once, both false (first false).
		block := make(chan struct{})
		rr.script.set(nil, block)
		rr.clk.Advance(4 * time.Second)
		c.replace("p1", 1, a, b)
		c.expectReplaceAck("p1", 1)
		rr.ev.awaitMatch(t, evCycleStarted, func(ev event) bool { return ev.rev == 1 })
		rr.script.awaitProbe(t)
		if s := statuses(rr.beat(c, 2, 1)); s["a"] || s["b"] || len(s) != 2 {
			t.Fatalf("first heartbeat = %v", s)
		}
		// The cycle completes within its budget: then true, reported at
		// once (b3 at T0+4s; the next periodic heartbeat is due at T0+9s).
		close(block)
		if p := rr.awaitCycle(t, 1); !p[0] || !p[1] {
			t.Fatalf("cycle = %v", p)
		}
		if s := statuses(rr.beat(c, 3, 1)); !s["a"] || !s["b"] {
			t.Fatalf("ready heartbeat = %v", s)
		}
		rr.script.set(nil, nil)
		// Next cycle starts at T0+9s (5 s after the previous start), with
		// the periodic heartbeat due then: the repeated identical result
		// is no report, only the periodic backstop (b4).
		rr.clk.Advance(5 * time.Second) // T0+9s
		rr.awaitCycle(t, 1)
		if s := statuses(rr.beat(c, 4, 1)); !s["a"] || !s["b"] {
			t.Fatalf("periodic heartbeat = %v", s)
		}
		// Deletion: a's runbook disappears; the next cycle (T0+14s) marks
		// only a false, reported by the heartbeat due at that instant or at
		// once after it.
		os.Remove(runA)
		rr.clk.Advance(5 * time.Second)
		if p := rr.awaitCycle(t, 1); p[0] || !p[1] {
			t.Fatalf("after deletion = %v", p)
		}
		rr.report(c, 1, func(s map[string]bool) bool { return !s["a"] && s["b"] })
		// Recovery at the following cycle (T0+19s).
		os.WriteFile(runA, []byte("x"), 0o644)
		rr.clk.Advance(5 * time.Second)
		if p := rr.awaitCycle(t, 1); !p[0] || !p[1] {
			t.Fatalf("after recovery = %v", p)
		}
		rr.report(c, 1, func(s map[string]bool) bool { return s["a"] && s["b"] })
		// set: a changed configuration (concurrency) replaces the snapshot;
		// readiness restarts from its own first cycle.
		a.Concurrency = 5
		c.replace("p2", 2, a, b)
		c.expectReplaceAck("p2", 2)
		rr.awaitCycle(t, 2)
		hb := rr.report(c, 2, func(s map[string]bool) bool { return s["a"] && s["b"] }).body
		if hb.Roles[0].Concurrency != 5 {
			t.Fatalf("after set = %+v", hb)
		}
		// rm: the snapshot without a; its status disappears.
		c.replace("p3", 3, b)
		c.expectReplaceAck("p3", 3)
		rr.awaitCycle(t, 3)
		if hb := rr.report(c, 3, func(s map[string]bool) bool { return s["b"] }).body; len(hb.Roles) != 1 || hb.Roles[0].RoleID != "b" {
			t.Fatalf("after rm = %+v", hb)
		}
		// rm of the last role: no roles, no cycle, no probe.
		probes := rr.script.count()
		c.replace("p4", 4)
		c.expectReplaceAck("p4", 4)
		if hb := rr.report(c, 4, func(s map[string]bool) bool { return len(s) == 0 }).body; len(hb.Roles) != 0 || rr.script.count() != probes {
			t.Fatalf("empty snapshot = %+v, probes %d->%d", hb, probes, rr.script.count())
		}
	})
	t.Run("shared-probe", func(t *testing.T) {
		t.Parallel()
		// One probe per cycle for the one enabled adapter executable,
		// whatever the number of roles; two manual checks per role.
		checks := map[string]int{}
		var mu = make(chan struct{}, 1)
		mu <- struct{}{}
		fp := startFakePlane(t)
		rr := startRoleRunWith(t, fp, true, func(d *deps) {
			d.observeCheck = func(kind, name string) {
				<-mu
				checks[kind]++
				mu <- struct{}{}
			}
		})
		c := fp.accept(t)
		c.connect()
		var roles []contract.RoleConfig
		for _, id := range []string{"a", "b", "c"} {
			ins, run := manuals(t, rr.dir, id, "x")
			roles = append(roles, roleConfig(id, ins, run))
		}
		c.replace("p1", 1, roles...)
		c.expectReplaceAck("p1", 1)
		rr.awaitCycle(t, 1)
		<-mu
		got := checks["probe"] == 1 && checks["manual"] == 6
		mu <- struct{}{}
		if !got || rr.script.count() != 1 {
			t.Fatalf("checks %v probes %d", checks, rr.script.count())
		}
	})
	t.Run("failure", func(t *testing.T) {
		t.Parallel()
		// A failing executable makes every role using it false; the
		// session and its heartbeats carry on.
		fp := startFakePlane(t)
		rr := startRoleRun(t, fp, true)
		c := fp.accept(t)
		c.helloOK(testID)
		rr.beat(c, 1, 0)
		c.reconcileEmpty()
		rr.script.set(&adapter.ProbeError{Reason: "exited unsuccessfully"}, nil)
		ins, run := manuals(t, rr.dir, "a", "x")
		c.replace("p1", 1, roleConfig("a", ins, run))
		c.expectReplaceAck("p1", 1)
		if p := rr.awaitCycle(t, 1); p[0] {
			t.Fatalf("cycle = %v", p)
		}
		// The new revision's immediate report, then the periodic ones.
		for k := 2; k <= 4; k++ {
			if k > 2 {
				rr.clk.Advance(heartbeatInterval)
			}
			if s := statuses(rr.beat(c, k, 1)); s["a"] {
				t.Fatalf("b%d = %v", k, s)
			}
		}
		// A disabled adapter is the same: false, never probed.
		fp2 := startFakePlane(t)
		off := startRoleRun(t, fp2, false)
		c2 := fp2.accept(t)
		c2.helloOK(testID)
		off.beat(c2, 1, 0)
		c2.reconcileEmpty()
		c2.replace("p1", 1, roleConfig("a", ins, run))
		c2.expectReplaceAck("p1", 1)
		if p := off.awaitCycle(t, 1); p[0] || off.script.count() != 0 {
			t.Fatalf("disabled cycle = %v, probes %d", p, off.script.count())
		}
	})
	t.Run("slow-worker", func(t *testing.T) {
		t.Parallel()
		// A cycle over its 2 s budget publishes all-false, keeps its slot
		// until the work returns and discards the late result; ticks during
		// the work leave one pending cycle, never a queue, and cycles never
		// overlap.
		fp := startFakePlane(t)
		rr := startRoleRun(t, fp, true)
		c := fp.accept(t)
		c.helloOK(testID)
		rr.beat(c, 1, 0)
		c.reconcileEmpty()
		ins, run := manuals(t, rr.dir, "a", "x")
		block := make(chan struct{})
		rr.script.set(nil, block)
		rr.script.setStuck(true)
		c.replace("p1", 1, roleConfig("a", ins, run))
		c.expectReplaceAck("p1", 1)
		rr.ev.awaitMatch(t, evCycleStarted, func(ev event) bool { return ev.rev == 1 })
		rr.script.awaitProbe(t)
		rr.beat(c, 2, 1)            // the new revision's immediate report
		rr.clk.Advance(cycleBudget) // T0+2s
		if p := rr.awaitCycle(t, 1); p[0] {
			t.Fatalf("expired cycle = %v", p)
		}
		rr.clk.Advance(3 * time.Second) // T0+5s: a tick, slot still held
		rr.beat(c, 3, 1)
		rr.clk.Advance(heartbeatInterval) // T0+10s: another tick
		rr.beat(c, 4, 1)
		if n := rr.script.count(); n != 1 {
			t.Fatalf("%d probes while the worker was stuck", n)
		}
		rr.script.setStuck(false)
		rr.script.set(nil, nil)
		close(block)
		// Exactly one pending cycle runs once the slot frees.
		rr.ev.awaitMatch(t, evCycleStarted, func(ev event) bool { return ev.rev == 1 })
		if p := rr.awaitCycle(t, 1); !p[0] {
			t.Fatalf("pending cycle = %v", p)
		}
		if n := rr.script.count(); n != 2 {
			t.Fatalf("%d probes, want 2", n)
		}
	})
	t.Run("interrupted-revision", func(t *testing.T) {
		t.Parallel()
		// A snapshot replaced while its cycle runs: the old result is never
		// published; the new revision's cycle starts after the worker
		// returns.
		fp := startFakePlane(t)
		rr := startRoleRun(t, fp, true)
		c := fp.accept(t)
		c.helloOK(testID)
		rr.beat(c, 1, 0)
		c.reconcileEmpty()
		ins, run := manuals(t, rr.dir, "a", "x")
		block := make(chan struct{})
		rr.script.set(nil, block)
		rr.script.setStuck(true)
		c.replace("p1", 1, roleConfig("a", ins, run))
		c.expectReplaceAck("p1", 1)
		rr.script.awaitProbe(t)
		c.replace("p2", 2, roleConfig("a", ins, run))
		c.expectReplaceAck("p2", 2)
		rr.script.setStuck(false)
		rr.script.set(nil, nil)
		close(block)
		if p := rr.awaitCycle(t, 2); !p[0] {
			t.Fatalf("revision 2 cycle = %v", p)
		}
		// Reported at once; the abandoned revision-1 result never was.
		rr.report(c, 2, func(s map[string]bool) bool { return s["a"] })
	})
	t.Run("freshness", func(t *testing.T) {
		t.Parallel()
		// can_accept is computed when the heartbeat is prepared: a passed
		// cycle is fresh strictly before its start + 10 s; a mismatched
		// revision is false.
		start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
		rs := &roleSession{inst: snapshot{any: true, rev: 3, roles: []contract.RoleRecord{{RoleConfig: roleConfig("a", "/i", "/r"), RegistrationOrder: 1}}}, reconciled: true}
		rs.last = &cycleResult{rev: 3, start: start, passed: []bool{true}}
		if !rs.statuses(start.Add(freshness - time.Nanosecond))[0].CanAccept {
			t.Fatal("stale before start+10s")
		}
		if rs.statuses(start.Add(freshness))[0].CanAccept {
			t.Fatal("fresh at start+10s")
		}
		rs.last.rev = 2
		if rs.statuses(start)[0].CanAccept {
			t.Fatal("another revision's result used")
		}
		rs.last = nil
		if st := rs.statuses(start); st[0].CanAccept || st[0].Inflight != 0 || st[0].Concurrency != 2 {
			t.Fatalf("no result = %+v", st)
		}
	})
}
