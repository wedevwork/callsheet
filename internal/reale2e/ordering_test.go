package reale2e

import (
	"slices"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// tasksOf lists the fake plane's workspace tasks of role.
func tasksOf(p *fakePlane, role string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, id := range p.order {
		if v := p.tasks[id]; v.Role.ID == role && v.WorkspaceBinding != nil {
			out = append(out, id)
		}
	}
	return out
}

// TestSuccessorArrival (C1 of the round-1 review): a correctly bound next
// hop dispatched in the same instant its predecessor's result lands,
// before any further supervisor poll, is admitted on the predecessor's
// authoritative state, whether the coordinator's observation or the
// supervisor's own scan sees it first. No coordinator sleep is involved.
func TestSuccessorArrival(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"observe", "monitor"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			f.coord.eager = path
			if code := f.attempt(); code != 0 {
				t.Fatalf("attempt = %d %q codes %v\n%s", code, f.sup.failCode, f.coord.codes, f.stderr.String())
			}
			if want := []string{CodeAccepted, CodeAccepted}; !slices.Equal(f.coord.codes[:2], want) {
				t.Fatalf("observe codes %v", f.coord.codes)
			}
			if r := f.report(); r.Result != ResultPass {
				t.Fatalf("report %v", failedCodes(r))
			}
		})
	}
}

// TestDeadlinesStopWork (C2 of the round-1 review): an expired bound
// stops further paid work at once. A success landing late is not
// accepted (by the watchdog or by the plane's own times), a successor
// reported or admitted after a bound passed is refused and cancelled, a
// feature bound runs from the task's authoritative creation, a yes that
// arrives together with or after the 20m timeout records no decision and
// admits no coder, and a late preflight answer is refused.
func TestDeadlinesStopWork(t *testing.T) {
	t.Parallel()
	type result struct {
		code     string
		noHop    string
		noCommit int
	}
	cases := map[string]struct {
		setup func(f *fakeWorld)
		want  result
	}{
		"late success": {func(f *fakeWorld) {
			f.coord.before = func(c *coordScript) bool {
				if c.step == 1 {
					f.clock.advance(11 * time.Minute)
				}
				return false
			}
		}, result{"feature_deadline_exceeded", HopDesignReviewer, 0}},
		"late success and an eager successor": {func(f *fakeWorld) {
			f.coord.eager = "observe"
			f.coord.before = func(c *coordScript) bool {
				if c.step == 1 {
					f.clock.advance(11 * time.Minute)
				}
				return false
			}
		}, result{"feature_deadline_exceeded", "", 0}},
		"late by the plane's times": {func(f *fakeWorld) {
			f.coord.before = func(c *coordScript) bool {
				if c.step == 1 {
					f.plane.finishSkew = 11 * time.Minute
				}
				return false
			}
		}, result{"feature_deadline_exceeded", HopDesignReviewer, 0}},
		"admitted long before it was seen": {func(f *fakeWorld) {
			f.coord.before = func(c *coordScript) bool {
				if c.step == 0 && c.tasks[0] == "" {
					c.files[0], c.commit[0] = c.s.seed.Files, c.s.seed.Commit
					c.dispatchOnly(0)
					f.clock.advance(11 * time.Minute)
					return true
				}
				return c.step == 0
			}
		}, result{"feature_deadline_exceeded", HopDesignReviewer, 0}},
		"observation after the attempt bound": {func(f *fakeWorld) {
			f.coord.before = func(c *coordScript) bool {
				if c.step == 0 {
					f.clock.advance(91 * time.Minute)
				}
				return false
			}
		}, result{"attempt_deadline_exceeded", HopDesignReviewer, 0}},
		"yes after the owner timeout": {func(f *fakeWorld) {
			f.coord.ownerInput = func(c *coordScript) {
				f.clock.advance(21 * time.Minute)
				c.s.handleLine("yes "+c.s.runID, true)
			}
		}, result{"owner_decision_timeout", HopCoder, 2}},
		"yes and the timeout ready together": {func(f *fakeWorld) {
			f.coord.ownerInput = func(c *coordScript) {
				f.clock.advance(21 * time.Minute)
				typeLines(f, "yes "+c.s.runID)(c)
			}
			f.coord.before = holdForDecision(f)
		}, result{"owner_decision_timeout", HopCoder, 2}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			tc.setup(f)
			if code := f.attempt(); code != 1 || f.sup.failCode != tc.want.code {
				t.Fatalf("attempt = %d %q, want %s (codes %v)", code, f.sup.failCode, tc.want.code, f.coord.codes)
			}
			if tc.want.noHop != "" && len(tasksOf(f.plane, tc.want.noHop)) != 0 {
				t.Fatalf("a %s task was dispatched after the bound", tc.want.noHop)
			}
			for i := tc.want.noCommit; i < len(Hops); i++ {
				if !f.sup.hops[i].commit.IsZero() {
					t.Fatalf("hop %d's late result was accepted", i+1)
				}
			}
			if f.sup.decision != nil {
				t.Fatal("a decision was recorded after the owner timeout")
			}
			// Every task of the run's roles was stopped.
			for _, h := range Hops {
				for _, id := range tasksOf(f.plane, h) {
					if v, _ := f.plane.ShowTask(t.Context(), id, 0); !contract.TaskTerminal(v.State) {
						t.Fatalf("task %s of %s still runs", id, h)
					}
				}
			}
			if name == "late success and an eager successor" {
				if !slices.Contains(f.coord.codes, CodeClosing) || !slices.Contains(f.plane.cancels, f.coord.tasks[1]) {
					t.Fatalf("the eager successor was not refused and cancelled: %v %v", f.coord.codes, f.plane.cancels)
				}
			}
		})
	}
	t.Run("late preflight answer", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.plane.finishSkew = 3 * time.Minute
		if code := f.attempt(); code != 1 || f.sup.failCode != "deadline_exceeded" || len(preflightDispatches(f.plane)) != 1 {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
	})
}

// TestPreflightDeadlineOrigin (C2 of the round-2 review): a preflight's 2m
// bound runs from the task's authoritative creation, not from the arrival
// of the dispatch answer: a response delayed by 8s, or a lost answer whose
// task is adopted, still cancels the task within 2m of its creation, and
// every poll carries a deadline.
func TestPreflightDeadlineOrigin(t *testing.T) {
	t.Parallel()
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "delayed answer", true: "adopted after a lost answer"}[lost], func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			f.plane.preflight = func(i int) (string, *string, bool, bool) {
				return contract.TaskSucceeded, ptr(AuthToken), false, i != 0
			}
			f.plane.dispatchLost = lost
			f.plane.afterDispatch = func(req contract.DispatchRequest) {
				if len(req.Payload) > 0 && req.Payload[0] == markerFor(f.sup.runID, preflightHop(0)) {
					f.clock.advance(8 * time.Second)
				}
			}
			if code := f.attempt(); code != 1 || f.sup.failCode != "deadline_exceeded" {
				t.Fatalf("attempt = %d %q", code, f.sup.failCode)
			}
			v := readTask(t, f.bundle(), "preflight/01")
			if d, ok := duration(v); !ok || v.State != contract.TaskCancelled || d > PreflightBound {
				t.Fatalf("the first preflight ran %s (%s) before its cancellation; bound %s", d, v.State, PreflightBound)
			}
			if f.plane.unbounded != 0 {
				t.Fatalf("%d task polls carried no deadline", f.plane.unbounded)
			}
		})
	}
}
