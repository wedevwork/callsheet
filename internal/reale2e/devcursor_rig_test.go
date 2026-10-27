package reale2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/storage"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// The Cursor probe's offline rig (design 12a-3-cursor-worker, 3a.md
// Function tests, reused by slice 3b): the existing probe rig with the
// alternate cursor-coder flow and --cursor, an injected World.Push into
// the fake plane's hub, a scripted Cursor coder worker answering the
// supervisor's coder dispatch, and hermetic shallow-sample roots.

const (
	fakeCursor        = "/fake/bin/cursor-agent"
	fakeCursorVersion = "2026.10.14-0f9edde"
)

// cursorAct scripts the coder worker: its terminal state, final answer
// and publication.
type cursorAct struct {
	// final replaces the default "...STATUS: IMPL_COMPLETE" answer; nofinal
	// sends none and truncated marks it truncated.
	final     string
	nofinal   bool
	truncated bool
	// state is the terminal state ("" succeeded); pending never finishes
	// (the 10m bound cancels it).
	state   string
	pending bool
	// edit changes the published files (nil: the exact repair); nopublish
	// publishes nothing; parent replaces the input parent; mode publishes
	// main.go with that file mode.
	edit      func(files map[string][]byte)
	nopublish bool
	parent    plumbing.Hash
	mode      filemode.FileMode
	// during runs while the worker runs (an owner change, a clutter).
	during func(cr *cursorRig)
}

// cursorRig drives one offline Cursor diagnostic probe.
type cursorRig struct {
	*devRig
	act cursorAct
	// coderTasks are the coder dispatches seen; pushes the supervisor's
	// pushes; pushErr fails them; pushEdit alters the answer.
	coderTasks []string
	coderReqs  []contract.DispatchRequest
	pushes     []workspacetransfer.PushRequest
	pushErr    error
	pushEdit   func(res *contract.WorkspacePushResult)
	// roots are the hermetic sample roots: cache under the fake home, a
	// private tmp and an absent var-tmp.
	roots      []activityRootSpec
	activityFS activityFS
	output     int
	// uid, when set, is the observer's own UID (a synthetic filesystem's).
	uid *uint32
	// exe is the rig's stub Cursor executable.
	exe string
}

// newCursorRig is the Cursor probe of scope ("" full) on the probe rig.
func newCursorRig(t testing.TB, scope string) *cursorRig {
	steps := []devAct{{}, {}, {}, {marker: markerReviewApproved}}
	if probeLightScope(scope) {
		steps = append(steps, devAct{marker: markerTriageConf, designRequired: designRequiredNo}, devAct{marker: markerReviewApproved})
	}
	r := newDevRig(t, steps)
	r.command = "probe-dev-full"
	r.owner = []string{"continue 1", "continue 2"}
	r.f.flow = filepath.Join(r.f.checkout, filepath.FromSlash(DevFullDir), "flow-cursor-coder.json")
	// A real stub file: the run hashes its explicit Cursor executable
	// before any vendor and after cleanup (its --version stays scripted).
	exe := stubFile(t, t.TempDir(), "cursor-agent")
	r.extraArgs = []string{"--cursor", exe}
	if scope != "" {
		r.extraArgs = append(r.extraArgs, "--scenario", scope)
	}
	r.f.versions[exe] = fakeCursorVersion
	eligibleVersions(r.f)
	cr := &cursorRig{devRig: r, exe: exe}
	base := shortTemp(t)
	cache, tmp := filepath.Join(r.f.home, ".cache"), filepath.Join(base, "tmp")
	for _, d := range []string{filepath.Join(cache, "vendor"), filepath.Join(tmp, "build")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(cache, "vendor", "state.bin"), []byte("x"), 0o600)
	// Hermetic sample roots under the live token spellings the checker
	// requires (a token is a label, never opened).
	cr.roots = []activityRootSpec{{Scope: activityScopeCache, Path: cache, Token: "$HOME/.cache"}, {Scope: activityScopeTmp, Path: tmp, Token: "/tmp"},
		{Scope: activityScopeVarTmp, Path: filepath.Join(base, "var-tmp"), Token: "/var/tmp"}}
	r.worldHook = func(w *World) { w.Push = cr.push }
	r.devHook = cr.hookRun
	r.f.plane.afterDispatch = cr.afterDispatch
	return cr
}

// hookRun points a Cursor run's observer at the rig's hermetic roots.
func (cr *cursorRig) hookRun(d *devSupervisor) {
	if d.cursor == nil {
		return
	}
	d.cursor.roots, d.cursor.output = cr.roots, cr.output
	if cr.activityFS != nil {
		d.cursor.fs = cr.activityFS
	}
	if cr.uid != nil {
		d.cursor.uid = *cr.uid
	}
}

// push is the injected supervisor push: the source repository's HEAD
// closure into the hub and the named workspace's main branch.
func (cr *cursorRig) push(_ context.Context, plane Plane, req workspacetransfer.PushRequest) (contract.WorkspacePushResult, error) {
	cr.pushes = append(cr.pushes, req)
	if plane != Plane(cr.f.plane) {
		return contract.WorkspacePushResult{}, errors.New("not the connected plane")
	}
	if cr.pushErr != nil {
		return contract.WorkspacePushResult{}, cr.pushErr
	}
	repo, err := git.PlainOpen(req.Path)
	if err != nil {
		return contract.WorkspacePushResult{}, err
	}
	head, err := repo.Head()
	if err != nil {
		return contract.WorkspacePushResult{}, err
	}
	if err := copyObjects(repo.Storer, cr.f.plane.hub, []plumbing.Hash{head.Hash()}); err != nil {
		return contract.WorkspacePushResult{}, err
	}
	cr.f.plane.pushTo(req.Name, head.Hash())
	res := contract.WorkspacePushResult{Name: req.Name, Instance: req.Instance, Branch: contract.DefaultBranchRef, Commit: head.Hash().String(),
		SourceKind: contract.KindGit, Changed: true}
	if cr.pushEdit != nil {
		cr.pushEdit(&res)
	}
	return res, nil
}

// afterDispatch is the scripted Cursor coder: it answers only the
// supervisor's coder fixture dispatch.
func (cr *cursorRig) afterDispatch(req contract.DispatchRequest) {
	if req.Target.Value != HopCoder || req.Workspace == nil || !strings.HasPrefix(*req.Workspace, "e2e-"+probeCoderNamespace+"-") {
		return
	}
	ids := cr.f.plane.ids()
	id := ids[len(ids)-1]
	cr.coderTasks = append(cr.coderTasks, id)
	cr.coderReqs = append(cr.coderReqs, req)
	act := cr.act
	if act.during != nil {
		act.during(cr)
	}
	if act.pending {
		return
	}
	c := cr.d.probe.cursor
	final := "Repaired the FP-1 greeting prefix; go test ./... passed and coverage is above 80%.\nSTATUS: IMPL_COMPLETE\n"
	if act.final != "" {
		final = act.final
	}
	fp := &final
	if act.nofinal {
		fp = nil
	}
	state := cmpOr(act.state, contract.TaskSucceeded)
	// The scripted vendor stream replaces the plane's default log.
	vendorLog := []byte("{\"type\":\"result\",\"is_error\":false}\n")
	if act.nopublish {
		cr.f.plane.finish(id, state, fp, act.truncated, nil)
		cr.f.plane.setLog(id, vendorLog)
		return
	}
	files := withFiles(c.input, map[string][]byte{"main.go": c.expected["main.go"]})
	if act.edit != nil {
		act.edit(files)
	}
	parent := c.inputCommit
	if !act.parent.IsZero() {
		parent = act.parent
	}
	var modes map[string]filemode.FileMode
	if act.mode != 0 {
		modes = map[string]filemode.FileMode{"main.go": act.mode}
	}
	h, _ := writeCommitMode(cr.t, cr.f.plane.hub, files, modes, parent)
	cr.f.plane.finish(id, state, fp, act.truncated, &h)
	cr.f.plane.setLog(id, vendorLog)
}

// errInjectedStore is failingCollect's write error.
var errInjectedStore = errors.New("injected: the evidence object store refused a write")

// failingStore is a storer whose first object write fails (later writes,
// such as the probe fixture's, succeed: only the result copy is lost).
type failingStore struct {
	storage.Storer
	failed *bool
}

func (s failingStore) SetEncodedObject(o plumbing.EncodedObject) (plumbing.Hash, error) {
	if !*s.failed {
		*s.failed = true
		return plumbing.ZeroHash, errInjectedStore
	}
	return s.Storer.SetEncodedObject(o)
}

// failingCollect is the collection repository whose next object write
// fails once.
func failingCollect(t testing.TB, r *git.Repository) *git.Repository {
	t.Helper()
	f, err := git.Open(failingStore{Storer: r.Storer, failed: new(bool)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// cursorQual reads the probe's Cursor qualification record.
func (cr *cursorRig) cursorQual() DevQualification {
	cr.t.Helper()
	var q DevQualification
	if err := json.Unmarshal(cr.evidence(devQualificationFile), &q); err != nil {
		cr.t.Fatal(err)
	}
	if q.Schema != DevCursorQualificationSchema || q.Cursor == nil {
		cr.t.Fatalf("not a Cursor qualification record: %s", q.Schema)
	}
	return q
}

// cursorCost reads the probe's cost record.
func (cr *cursorRig) cursorCost() DevCost {
	cr.t.Helper()
	var c DevCost
	if err := json.Unmarshal(cr.evidence(devCostFile), &c); err != nil {
		cr.t.Fatal(err)
	}
	return c
}

// coderVerdict reads the coder stage's stored verdict.
func (cr *cursorRig) coderVerdict() cursorCoderVerdict {
	cr.t.Helper()
	var v cursorCoderVerdict
	if err := cr.data(probeCoderDir+"/verdict.json", &v); err != nil {
		cr.t.Fatal(err)
	}
	return v
}

// stageOf is the record's named stage.
func stageOf(q DevQualification, name string) DevProbeStage {
	for _, s := range q.Stages {
		if s.Name == name {
			return s
		}
	}
	return DevProbeStage{}
}

// qualifiedCheck runs the offline check of the rig's evidence: its exit,
// report and stderr.
func (cr *cursorRig) qualifiedCheck() (int, DevCursorQualificationReport, string) {
	cr.t.Helper()
	var out, errb bytes.Buffer
	code := CheckEvidence(cr.f.bundle(), &out, &errb)
	var rep DevCursorQualificationReport
	json.Unmarshal(out.Bytes(), &rep)
	return code, rep, errb.String()
}

// activity reads one phase's shallow sample.
func (cr *cursorRig) activity(phase string) ActivityAggregate {
	cr.t.Helper()
	var a ActivityAggregate
	rel := cursorActivityFiles[0]
	if phase == "after" {
		rel = cursorActivityFiles[1]
	}
	if err := json.Unmarshal(cr.evidence(rel), &a); err != nil {
		cr.t.Fatal(err)
	}
	return a
}

// blocked asserts a BLOCKED Cursor probe: never passed or QUALIFIED, its
// printed cause, and its record's independent check BLOCKED too (the
// command and the collector agree). It returns the record.
func (cr *cursorRig) blocked(cause string) DevQualification {
	cr.t.Helper()
	out := cr.f.stdout.String()
	if cr.d.passed || strings.Contains(out, "qualification probe: "+ResultQualified) ||
		!strings.Contains(out, "qualification probe: "+ResultBlocked+" ("+cause) {
		cr.t.Fatalf("a blocked Cursor probe printed %s", out)
	}
	q := cr.cursorQual()
	if code, rep, _ := cr.qualifiedCheck(); code != 1 || rep.Result != ResultBlocked || len(rep.Codes) == 0 {
		cr.t.Fatalf("offline check of a blocked record %d %+v", code, rep)
	}
	return q
}

// coderStopped asserts a coder-stage stop: code, no coordinator phase,
// one coder dispatch (no fallback), retained task artifacts, complete
// cleanup and a BLOCKED record naming the primary code.
func (cr *cursorRig) coderStopped(code string, artifacts ...string) DevQualification {
	cr.t.Helper()
	cr.expect(1, code)
	if n := len(cr.f.launcher.phaseSpecs()); n != 0 {
		cr.t.Fatalf("%d coordinator phases started after a failed coder stage", n)
	}
	if len(cr.coderTasks) != 1 {
		cr.t.Fatalf("coder dispatches %v: a retry or fallback", cr.coderTasks)
	}
	for _, a := range artifacts {
		cr.evidence(probeCoderDir + "/" + a)
	}
	q := cr.blocked(code)
	if q.Result != ResultBlocked || q.StopCode == nil || *q.StopCode != code || stageOf(q, stageCoderEvidence).Result != stageFail ||
		stageOf(q, stageCoderEvidence).Code != code || stageOf(q, "resume_wake").Result != stageUnreached || !cr.cleanupRecord().Complete {
		cr.t.Fatalf("record %+v", q)
	}
	return q
}

// ---- Slice 3b: attempts, the combined exercise and cached bundles ----

// cursorAttempt makes r a Cursor-selected attempt of the rig's own world:
// the shipped Cursor flow, a stub --cursor executable with its scripted
// version, and hermetic sample roots under the live token spellings. It
// returns the executable.
func cursorAttempt(t testing.TB, r *devRig) string {
	t.Helper()
	r.f.flow = filepath.Join(r.f.checkout, filepath.FromSlash(DevFullDir), "flow-cursor-coder.json")
	exe := stubFile(t, t.TempDir(), "cursor-agent")
	r.extraArgs = append(r.extraArgs, "--cursor", exe)
	r.f.versions[exe] = fakeCursorVersion
	eligibleVersions(r.f)
	base := shortTemp(t)
	cache := filepath.Join(r.f.home, ".cache")
	for _, d := range []string{filepath.Join(cache, "vendor"), filepath.Join(base, "tmp", "build")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	roots := []activityRootSpec{{Scope: activityScopeCache, Path: cache, Token: "$HOME/.cache"}, {Scope: activityScopeTmp, Path: filepath.Join(base, "tmp"), Token: "/tmp"},
		{Scope: activityScopeVarTmp, Path: filepath.Join(base, "var-tmp"), Token: "/var/tmp"}}
	prev := r.devHook
	r.devHook = func(d *devSupervisor) {
		if prev != nil {
			prev(d)
		}
		if d.cursor != nil {
			d.cursor.roots = roots
		}
	}
	return exe
}

// eligibleVersions scripts vendor versions at the adapters' current
// minimums: a Cursor format's checker re-applies the version policy to the
// recorded lines offline.
func eligibleVersions(f *fakeWorld) {
	f.versions[fakeClaude], f.versions[fakeCodex], f.versions[fakeGrok] = "2.1.299 (Claude Code)", "codex-cli 0.162.1",
		"grok 1.0.52 (0920c8169e27) [stable]"
}

// newCursorFullRig is a Cursor full attempt over steps.
func newCursorFullRig(t testing.TB, steps []devAct) *devRig {
	t.Helper()
	r := newDevRig(t, steps)
	r.owner = []string{"yes 1"}
	cursorAttempt(t, r)
	return r
}

// newCursorLightRig is a Cursor light attempt over steps.
func newCursorLightRig(t testing.TB, steps []devAct) *devRig {
	t.Helper()
	r := newLightRig(t, steps)
	cursorAttempt(t, r)
	return r
}

// newCursorExerciseRig is a Cursor combined exercise over full and light
// scripts: both children share the root's frozen Cursor selection and
// bound executable. It returns the rig and the executable.
func newCursorExerciseRig(t testing.TB, fullSteps, lightSteps []devAct) (*exerciseRig, string) {
	t.Helper()
	x := newExerciseRig(t, fullSteps, lightSteps)
	exe := cursorAttempt(t, x.full)
	x.light.devHook = x.full.devHook
	return x, exe
}

// cursorCache is one cached immutable Cursor bundle: a qualified probe of
// a scope or a passing attempt, produced once per test binary by the live
// collector and copied for every mutation.
type cursorCache struct {
	sync.Once
	dir string
	err error
}

var (
	cursorQualifiedCache = map[string]*cursorCache{probeScopeFull: {}, probeScopeBoth: {}}
	cursorAttemptCache   = map[string]*cursorCache{scenarioFull: {}, scenarioLight: {}}
)

// cachedCopy builds c once with build and returns a fresh copy.
func (c *cursorCache) cachedCopy(t testing.TB, name string, build func() (string, error)) string {
	t.Helper()
	c.Do(func() {
		src, err := build()
		if err != nil {
			c.err = err
			return
		}
		c.dir = filepath.Join(bundleCacheRoot, name)
		c.err = copyTree(src, c.dir)
	})
	if c.err != nil {
		t.Fatal(c.err)
	}
	dst := filepath.Join(t.TempDir(), "bundle")
	if err := copyTree(c.dir, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// cursorQualifiedBundle is a fresh copy of the cached QUALIFIED Cursor
// probe of scope.
func cursorQualifiedBundle(t testing.TB, scope string) string {
	t.Helper()
	return cursorQualifiedCache[scope].cachedCopy(t, "cursor-qualified-"+scope, func() (string, error) {
		cr := newCursorRig(t, scope)
		if code := cr.run(); code != 0 || !cr.d.passed {
			return "", fmt.Errorf("the shared Cursor probe failed (%d): %s\n%s\n%s", code, cr.d.stopCode, cr.f.stderr.String(), cr.f.stdout.String())
		}
		return cr.f.bundle(), nil
	})
}

// cursorAttemptBundle is a fresh copy of the cached passing Cursor attempt
// of scenario.
func cursorAttemptBundle(t testing.TB, scenario string) string {
	t.Helper()
	return cursorAttemptCache[scenario].cachedCopy(t, "cursor-attempt-"+scenario, func() (string, error) {
		r := newCursorFullRig(t, happy())
		if scenario == scenarioLight {
			r = newCursorLightRig(t, lightHappy())
		}
		if code := r.run(); code != 0 || !r.d.passed {
			detail, _ := os.ReadFile(filepath.Join(r.f.evidence, "report.txt"))
			return "", fmt.Errorf("the shared Cursor %s attempt failed (%d): %s\n%s\n%s", scenario, code, r.d.stopCode, r.f.stderr.String(), detail)
		}
		return r.f.bundle(), nil
	})
}

// cursorExerciseCache is the cached passing Cursor combined exercise.
var cursorExerciseCache = &cursorCache{}

// cursorExerciseRoot is a fresh copy of the cached passing Cursor
// combined exercise root.
func cursorExerciseRoot(t testing.TB) string {
	t.Helper()
	return cursorExerciseCache.cachedCopy(t, "cursor-exercise", func() (string, error) {
		x, _ := newCursorExerciseRig(t, happy(), lightHappy())
		if code := x.run(); code != 0 {
			return "", fmt.Errorf("the shared Cursor exercise failed (%d): %s", code, x.stdout())
		}
		return x.root(), nil
	})
}

// cursorCheck runs the offline Cursor qualification check of dir.
func cursorCheck(t testing.TB, dir string) (int, DevCursorQualificationReport, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := CheckEvidence(dir, &out, &errb)
	var rep DevCursorQualificationReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("check printed no report: %v\n%s", err, out.String())
	}
	return code, rep, errb.String()
}

// roleOnNode reports the registered role's node and adapter.
func (cr *cursorRig) roleOnNode(id string) (string, string) {
	for _, r := range cr.f.plane.roles {
		if r.ID == id {
			return r.Node, r.Adapter
		}
	}
	return "", ""
}

// sidecarArgs are the started sidecar run argv.
func sidecarArgs(f *fakeWorld) [][]string {
	var out [][]string
	for _, s := range f.launcher.specsOf(fakeCallsheet) {
		if len(s.Args) > 1 && s.Args[0] == "sidecar" && s.Args[1] == "run" && !slices.ContainsFunc(out, func(a []string) bool { return slices.Equal(a, s.Args) }) {
			out = append(out, s.Args)
		}
	}
	return out
}
