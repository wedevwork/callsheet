package reale2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/storage"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// The Cursor diagnostic probe's offline rig (design 12a-3-cursor-worker,
// 3a.md Function tests): the existing probe rig with the alternate
// cursor-coder flow and --cursor, an injected World.Push into the fake
// plane's hub, a scripted Cursor coder worker answering the supervisor's
// coder dispatch, and hermetic shallow-sample roots. One small rig is
// reused by every 3a function test; no attempt or exercise rig is added.

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
	r.extraArgs = []string{"--cursor", fakeCursor}
	if scope != "" {
		r.extraArgs = append(r.extraArgs, "--scenario", scope)
	}
	r.f.versions[fakeCursor] = fakeCursorVersion
	cr := &cursorRig{devRig: r}
	base := shortTemp(t)
	cache, tmp := filepath.Join(r.f.home, ".cache"), filepath.Join(base, "tmp")
	for _, d := range []string{filepath.Join(cache, "vendor"), filepath.Join(tmp, "build")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(cache, "vendor", "state.bin"), []byte("x"), 0o600)
	cr.roots = []activityRootSpec{{Scope: activityScopeCache, Path: cache, Token: "$HOME/.cache"}, {Scope: activityScopeTmp, Path: tmp, Token: tmp},
		{Scope: activityScopeVarTmp, Path: filepath.Join(base, "var-tmp"), Token: filepath.Join(base, "var-tmp")}}
	r.worldHook = func(w *World) { w.Push = cr.push }
	r.probeHook = func(p *devProbe) {
		if p.cursor != nil {
			p.cursor.roots = cr.roots
			p.cursor.output = cr.output
			if cr.activityFS != nil {
				p.cursor.fs = cr.activityFS
			}
		}
	}
	r.f.plane.afterDispatch = cr.afterDispatch
	return cr
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

// observation reads the probe's receipt.
func (cr *cursorRig) observation() CursorProbeObservation {
	cr.t.Helper()
	var o CursorProbeObservation
	if err := json.Unmarshal(cr.evidence(cursorObservationFile), &o); err != nil {
		cr.t.Fatal(err)
	}
	return o
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

// stage is the receipt's named stage.
func stageNamed(o CursorProbeObservation, name string) DevProbeStage {
	for _, s := range o.Stages {
		if s.Name == name {
			return s
		}
	}
	return DevProbeStage{}
}

// noQualification fails when the probe wrote a qualification record.
func (cr *cursorRig) noQualification() {
	cr.t.Helper()
	if _, err := os.Stat(filepath.Join(cr.f.evidence, devQualificationFile)); err == nil || cr.d.passed {
		cr.t.Fatalf("a Cursor probe wrote %s or passed", devQualificationFile)
	}
}

// coderStopped asserts a coder-stage stop: code, no coordinator phase,
// one coder dispatch (no fallback), retained task artifacts, complete
// cleanup and a BLOCKED receipt naming the primary code.
func (cr *cursorRig) coderStopped(code string, artifacts ...string) CursorProbeObservation {
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
	o := cr.observation()
	if o.Result != ResultBlocked || o.Blocker != code || o.Qualification || stageNamed(o, stageCoderEvidence).Result != stageFail ||
		stageNamed(o, stageCoderEvidence).Code != code || stageNamed(o, "resume_wake").Result != stageUnreached || !o.Cleanup.Complete {
		cr.t.Fatalf("receipt %+v", o)
	}
	out := cr.f.stdout.String()
	if !strings.Contains(out, "Cursor probe observations: BLOCKED ("+code+")") || strings.Contains(out, "qualification probe: "+ResultQualified) {
		cr.t.Fatalf("stdout %s", out)
	}
	cr.noQualification()
	return o
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
