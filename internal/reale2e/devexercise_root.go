package reale2e

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The combined exercise (design 12a-dev-flow-light, Exercise invocation;
// FP-8). reale2e run-dev-exercise asks the owner once for the combined cost
// authorization, then runs one independent full attempt and, unless the
// full attempt ended unsafely, one independent light attempt, each with its
// own run ID, runtime, plane, workspace, seed, counters, preflights,
// deadlines, inventory and environment snapshots, under one closed
// callsheet-dev-exercise/v1 root. The root records both children's
// outcomes and their summed actual accounting; combined PASS requires both
// independent PASSes. Each child's 2m cleanup tail is a shared
// finalization budget for cleanup, collection and its own check (and, for
// the last reached child, the root's aggregation).

// Exercise schemas, files and bounds.
const (
	DevExerciseSchema       = "callsheet-dev-exercise/v1"
	devExerciseReportSchema = "callsheet-dev-exercise-report/v1"
	devExerciseAuditSchema  = "callsheet-dev-exercise-audit/v1"
	devExerciseFile         = "exercise.json"
	exerciseFullDir         = "attempts/full"
	exerciseLightDir        = "attempts/light"
	// maxExerciseRootText bounds the root-only primary and derived text.
	maxExerciseRootText = 1 << 20
	// ExerciseBound is the combined bound after the cost authorization:
	// 90m full work, 2m full finalization, 90m light work, 2m light
	// finalization and root aggregation (184m).
	ExerciseBound = 2 * (AttemptBound + CleanupBound)
	// exerciseMaxCalls and exerciseHappyCalls are the combined worker-call
	// ceiling (full 19 plus light 14) and happy path (full 9 plus light 8).
	exerciseMaxCalls   = 33
	exerciseHappyCalls = 17
)

// Exercise child statuses and authorization kinds.
const (
	childPass     = "PASS"
	childFail     = "FAIL"
	childNotRun   = "NOT_RUN"
	authExercise  = "exercise"
	stopFinalized = "finalization_deadline"
)

// ExerciseBounds is the combined exercise's cost and time disclosure.
const ExerciseBounds = `Cost and time bounds of the combined exercise (policy, not measurements):
  - one full development attempt then one light attempt, independent: separate run IDs,
    planes, workspaces, seeds, coordinator sessions, preflights, deadlines and evidence;
  - at most 33 paid worker calls (full 19 plus light 14, each including its five
    authentication preflights); 17 on the happy path (full 9 plus light 8); the coordinator
    adds vendor-dependent turns in up to four full phases plus one light phase;
  - the cost authorization window at most 20m; then at most 184m: 90m full work, 2m of full
    cleanup, collection and check, 90m light work, 2m of light cleanup, collection, check and
    root aggregation; the full attempt's design decision and the light intake are separate
    owner gates;
  - no automatic retries, no concurrency and no token-price ceiling; monetary cost is unknown.
`

// DevExerciseAuthorization is the typed cost authorization.
type DevExerciseAuthorization struct {
	Prompt   string `json:"prompt_time"`
	Deadline string `json:"deadline"`
	Decision string `json:"decision_time"`
	Kind     string `json:"kind"`
	Response string `json:"response"`
}

// DevExerciseChild is one child attempt's record.
type DevExerciseChild struct {
	Dir            string `json:"dir"`
	Scenario       string `json:"scenario"`
	RunID          string `json:"run_id"`
	ManifestSHA256 string `json:"manifest_sha256"`
	Start          string `json:"start"`
	Deadline       string `json:"deadline"`
	FinalizeBy     string `json:"finalize_by"`
	End            string `json:"end"`
	Status         string `json:"status"`
	Stop           string `json:"stop"`
}

// DevExercise is exercise.json.
type DevExercise struct {
	Schema        string                   `json:"schema"`
	ExerciseID    string                   `json:"exercise_id"`
	Executable    DevExecutableRecord      `json:"executable"`
	FlowSHA256    string                   `json:"flow_sha256"`
	Authorization DevExerciseAuthorization `json:"authorization"`
	Start         string                   `json:"start"`
	End           string                   `json:"end"`
	Children      []DevExerciseChild       `json:"children"`
}

// devExerciseRun is one live combined exercise.
type devExerciseRun struct {
	h    Host
	w    *World
	a    runArgs
	root string
	ex   DevExercise
	// prompted, when set, runs right after the authorization prompt (the
	// offline rig types its fresh answer there).
	prompted func(id string)
	// aggregating, when set, runs during the root's check and writing
	// (tests).
	aggregating func(stage string)
	lines       <-chan ownerInput
	eof         bool
}

// runExerciseLive runs the combined exercise.
func runExerciseLive(h Host, a runArgs) int {
	fmt.Fprint(h.Stdout, ExerciseBounds)
	w, release, err := h.NewWorld(h)
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e run-dev-exercise: %v\n", err)
		return 1
	}
	defer release()
	e := &devExerciseRun{h: h, w: w, a: a, prompted: h.exercisePrompted, aggregating: h.exerciseAggregating}
	return e.run()
}

// run is the whole exercise.
func (e *devExerciseRun) run() int {
	root, err := e.locateRoot()
	if err != nil {
		fmt.Fprintf(e.h.Stderr, "reale2e run-dev-exercise: %v\n", err)
		return 2
	}
	e.root = root
	now := e.w.Clock.Now()
	id, err := newRunID(now, e.w.Rand)
	if err != nil {
		fmt.Fprintf(e.h.Stderr, "reale2e run-dev-exercise: %v\n", err)
		return 1
	}
	e.ex = DevExercise{Schema: DevExerciseSchema, ExerciseID: id, FlowSHA256: e.ex.FlowSHA256, Start: formatTime(now), Children: []DevExerciseChild{
		{Dir: exerciseFullDir, Scenario: scenarioFull, Status: childNotRun}, {Dir: exerciseLightDir, Scenario: scenarioLight, Status: childNotRun}}}
	if b, err := readBounded(e.w.Executable, 1<<30); err == nil {
		e.ex.Executable = DevExecutableRecord{Path: e.w.Executable, Before: sha256Hex(b), After: sha256Hex(b)}
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		fmt.Fprintf(e.h.Stderr, "reale2e run-dev-exercise: %v\n", err)
		return 1
	}
	if err := os.Mkdir(filepath.Join(root, "attempts"), 0o700); err != nil {
		fmt.Fprintf(e.h.Stderr, "reale2e run-dev-exercise: %v\n", err)
		return 1
	}
	e.ex.Authorization = e.authorize(id)
	if e.ex.Authorization.Kind != authExercise {
		for i := range e.ex.Children {
			e.ex.Children[i].Stop = "owner_" + e.ex.Authorization.Kind
		}
		return e.finishRoot(nil)
	}
	full := e.child(0, func(h Host, w *World, a runArgs) *devSupervisor {
		d := newDevSupervisor(h, w, a)
		if h.observeDev != nil {
			h.observeDev(d)
		}
		return d
	})
	if cause := continueCause(full); cause != "" {
		e.ex.Children[1].Stop = cause
		return e.finishRoot(full)
	}
	light := e.child(1, newDevLightSupervisor)
	return e.finishRoot(light)
}

// locateRoot validates the exercise root as run validates an attempt's
// evidence: a new, Git-ignored directory under the checkout's design/.
func (e *devExerciseRun) locateRoot() (string, error) {
	s := &supervisor{w: e.w, args: e.a}
	if err := s.locate(); err != nil {
		return "", err
	}
	// The selected flow: the explicit --flow file, or the shipped one.
	flow := e.a.flow
	if flow == "" {
		flow = filepath.Join(s.checkout, filepath.FromSlash(DevFullDir), FlowFile)
	}
	if b, err := readBounded(flow, maxFlowBytes); err == nil {
		e.ex.FlowSHA256 = sha256Hex(b)
	}
	return s.evidenceDir, nil
}

// authorize asks for the combined cost authorization: exactly
// "exercise <exercise-id>" (or "no <exercise-id>"), a fresh line after the
// prompt within 20m. Wrong IDs and other text never authorize.
func (e *devExerciseRun) authorize(id string) DevExerciseAuthorization {
	at := e.w.Clock.Now()
	deadline := at.Add(OwnerDecisionBound)
	auth := DevExerciseAuthorization{Prompt: formatTime(at), Deadline: formatTime(deadline)}
	t, ok := e.w.Terminal.(windowedTerminal)
	if !ok {
		auth.Kind, auth.Decision = intakeEOF, formatTime(e.w.Clock.Now())
		return auth
	}
	e.lines = t.ActivateWindowed()
	epoch := t.OpenWindow()
	fmt.Fprintf(e.h.Stdout, "Authorize the combined exercise %s (one full then one light attempt, at most %d paid worker calls)? Type exactly \"exercise %s\" or \"no %s\".\n",
		id, exerciseMaxCalls, id, id)
	if e.prompted != nil {
		e.prompted(id)
	}
	for {
		if !e.w.Clock.Now().Before(deadline) {
			auth.Kind, auth.Decision = intakeExpired, formatTime(e.w.Clock.Now())
			return auth
		}
		timer, stop := e.w.Clock.NewTimer(min(e.w.Poll, max(time.Millisecond, deadline.Sub(e.w.Clock.Now()))))
		select {
		case in, ok := <-e.lines:
			stop()
			if !ok {
				auth.Kind, auth.Decision = intakeEOF, formatTime(e.w.Clock.Now())
				return auth
			}
			if in.epoch != epoch || !e.w.Clock.Now().Before(deadline) {
				continue
			}
			switch line := strings.TrimSuffix(in.line, "\r"); line {
			case authExercise + " " + id:
				auth.Kind, auth.Response, auth.Decision = authExercise, line, formatTime(e.w.Clock.Now())
				return auth
			case "no " + id:
				auth.Kind, auth.Response, auth.Decision = intakeNo, line, formatTime(e.w.Clock.Now())
				return auth
			}
			fmt.Fprintf(e.h.Stdout, "not an authorization for exercise %s; the window's deadline is unchanged\n", id)
		case <-timer:
		case <-e.w.Context.Done():
			stop()
			auth.Kind, auth.Decision = intakeEOF, formatTime(e.w.Clock.Now())
			return auth
		}
	}
}

// child runs child k (full or light) in its fixed directory with its own
// supervisor and records it.
func (e *devExerciseRun) child(k int, make func(Host, *World, runArgs) *devSupervisor) *devSupervisor {
	c := &e.ex.Children[k]
	a := e.a
	a.evidence = filepath.Join(e.root, filepath.FromSlash(c.Dir))
	h := e.h
	d := make(h, e.w, a)
	d.exerciseChild = true
	code := d.run()
	c.RunID, c.Start, c.Deadline = d.s.runID, d.manifest.AttemptStart, d.manifest.AttemptDeadline
	c.FinalizeBy, c.End = formatTime(d.s.cleanupBy), formatTime(e.w.Clock.Now())
	if b, err := readBounded(filepath.Join(a.evidence, devManifestFile), maxTextFile); err == nil {
		c.ManifestSHA256 = sha256Hex(b)
	}
	c.Status, c.Stop = childFail, d.stopCode
	switch {
	case d.finalizeExpired:
		c.Stop = stopFinalized
	case code == 0 && d.passed:
		c.Status = childPass
	}
	return d
}

// continueCause is why the light attempt must not start after the full
// one ("" to continue): an owner refusal or stop, an interrupt, unsafe or
// incomplete cleanup, a protected change, an evidence write failure or an
// expired finalization. A workflow or oracle failure continues.
func continueCause(d *devSupervisor) string {
	switch {
	case d.finalizeExpired:
		return stopFinalized
	case d.stopCode == "owner_no" || d.stopCode == "owner_eof" || d.stopCode == "owner_expired":
		return "full_owner_stop"
	case d.stopCode == "interrupted":
		return "interrupted"
	case !d.cleanupOK:
		return "full_cleanup_incomplete"
	case d.protected:
		return "full_protected_change"
	case d.s.ev == nil || d.s.ev.failure() != nil || d.stopCode == "evidence_write_failed":
		return "full_evidence_write_failed"
	}
	return ""
}

// finishRoot aggregates the root and writes exercise.json and the root's
// canonical report and audit (the offline check's own derivation) within
// the last reached child's remaining finalization tail, and returns the
// exit: 0 only for a combined PASS. The record's end is the aggregation's
// own time: when the aggregation or the writing passes the tail, the root
// is aggregated again with its actual end, so the stored record, report
// and audit carry the expiry (finalization_deadline, a FAIL) as the live
// exit does.
func (e *devExerciseRun) finishRoot(last *devSupervisor) int {
	expired := func() bool { return last != nil && !e.w.Clock.Now().Before(last.s.cleanupBy) }
	aggregate := func() (exerciseCheckResult, []byte, error) {
		e.ex.End = formatTime(e.w.Clock.Now())
		b, err := encodeJSON(e.ex)
		if err != nil {
			return exerciseCheckResult{}, nil, err
		}
		if e.aggregating != nil {
			e.aggregating("check")
		}
		res, err := checkExerciseRootData(e.root, b, true)
		return res, b, err
	}
	var written []string
	write := func(res exerciseCheckResult, record []byte) error {
		for _, w := range []struct {
			name string
			data []byte
		}{{devExerciseFile, record}, {"report.json", res.reportJSON}, {"report.txt", []byte(res.reportText)}, {"audit.json", res.auditJSON},
			{"audit.md", []byte(res.auditText)}} {
			if err := writeExclusive(filepath.Join(e.root, w.name), w.data, 0o600); err != nil {
				if w.name == devExerciseFile {
					return fmt.Errorf("the exercise root was not written: %w", err)
				}
				return fmt.Errorf("the root report or audit was not written: %w", err)
			}
			written = append(written, w.name)
			if w.name == devExerciseFile && e.aggregating != nil {
				e.aggregating("write")
			}
		}
		return nil
	}
	res, record, err := aggregate()
	if err == nil && expired() {
		// The aggregation itself passed the tail: its actual end.
		res, record, err = aggregate()
	}
	if err != nil {
		fmt.Fprintf(e.h.Stderr, "reale2e run-dev-exercise: %v\n", err)
		return 1
	}
	late := expired()
	err = write(res, record)
	if err == nil && !late && expired() {
		// The writing passed the tail: the timely record is replaced by one
		// with its actual end (only the files this call wrote).
		late = true
		for _, n := range written {
			os.Remove(filepath.Join(e.root, n))
		}
		written = nil
		if res, record, err = aggregate(); err == nil {
			err = write(res, record)
		}
	}
	if err != nil {
		fmt.Fprintf(e.h.Stderr, "reale2e run-dev-exercise: %v\n", err)
		return 1
	}
	fmt.Fprint(e.h.Stdout, res.reportText)
	if late {
		// Never a PASS: the stored root records the expiry too.
		fmt.Fprintf(e.h.Stderr, "reale2e run-dev-exercise: stopped (%s): the root aggregation passed the finalization deadline\n", stopFinalized)
		return 1
	}
	if res.report.Result == ResultPass {
		fmt.Fprintln(e.h.Stdout, "combined exercise: PASS (both attempts' mechanical and deterministic criteria); "+JudgmentReportLine)
		return 0
	}
	fmt.Fprintln(e.h.Stdout, "combined exercise: FAIL; "+JudgmentReportLine)
	return 1
}

// exerciseRootExists reports an exercise root (exercise.json present).
func exerciseRootExists(resolved string) bool {
	_, err := lstatRegular(resolved, devExerciseFile)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
