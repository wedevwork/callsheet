package reale2e

import (
	"testing"
)

// The combined exercise rig (design 12a-dev-flow-light, FP-8): one fake
// world (one checkout, evidence root, clock and owner terminal) with a
// fresh private plane per child, the full rig driving the first child and
// the light rig the second, and the owner's cost authorization typed as a
// fresh line after its prompt.
type exerciseRig struct {
	t           testing.TB
	full, light *devRig
	// auth is the authorization answer: "exercise", "no", "eof" or ""
	// (expiry).
	auth  string
	plane map[string]*fakePlane
	// aggregating runs during the root's check and writing.
	aggregating func(stage string)
	// prompted, when set, runs right after the authorization answer is
	// typed, before any child starts.
	prompted func()
}

// newExerciseRig is a combined exercise over full and light scripts.
func newExerciseRig(t testing.TB, fullSteps, lightSteps []devAct) *exerciseRig {
	t.Helper()
	full := newDevRig(t, fullSteps)
	full.owner = []string{"yes 1"}
	light := newDevRig(t, lightSteps)
	light.f, light.term, light.intake = full.f, full.term, intakeLight
	full.f.light = true
	return &exerciseRig{t: t, full: full, light: light, auth: authExercise, plane: map[string]*fakePlane{}}
}

// root is the exercise root.
func (x *exerciseRig) root() string { return x.full.f.evidence }

// run runs the combined exercise and returns its exit.
func (x *exerciseRig) run() int {
	f := x.full.f
	h := x.full.host()
	h.Args[0] = "run-dev-exercise"
	h.exerciseAggregating = x.aggregating
	h.exercisePrompted = func(id string) {
		switch x.auth {
		case authExercise:
			x.full.term.typeLine("exercise " + id)
		case intakeNo:
			x.full.term.typeLine("no " + id)
		case intakeEOF:
			x.full.term.close()
		case "wrong":
			// Another exercise's ID never authorizes; the window stays open
			// for the right line.
			x.full.term.typeLine("exercise 20990101T000000Z-00000000")
			x.full.term.typeLine("exercise " + id)
		}
		if x.prompted != nil {
			x.prompted()
		}
	}
	h.observeDev = func(d *devSupervisor) {
		r := x.full
		if d.light != nil {
			r = x.light
			d.light.prompted = r.answerIntake
			// Each child runs its own private plane.
			f.plane = newFakePlane(x.t, f.clock)
		}
		r.d = d
		if r.devHook != nil {
			r.devHook(d)
		}
		x.plane[d.scenario()] = f.plane
		rig := r
		f.plane.onList = func() {
			if rig.d != nil && rig.d.finished && rig.onFinal != nil {
				fn := rig.onFinal
				rig.onFinal = nil
				fn(rig)
			}
			if rig.d == nil || !rig.d.looping || rig.d.finished || rig.d.ticks == rig.lastTick {
				return
			}
			rig.lastTick = rig.d.ticks
			rig.hook()
		}
	}
	code := MainWith(h)
	f.clock.release()
	return code
}

// stdout is the exercise's printed output.
func (x *exerciseRig) stdout() string { return x.full.f.stdout.String() }
