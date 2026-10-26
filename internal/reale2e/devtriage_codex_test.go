package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Tests of design 12a-codex-triage: every shipped triage dispatch runs on
// Codex gpt-6.1-sol at high (FP-1); the Grok installation, its idle
// sidecar and the five preflights stay harness prerequisites (FP-2a);
// bindings regenerate under unchanged schemas and the obsolete Grok-triage
// flow is refused (FP-2b); the scratch contract is unchanged (FP-3a); and
// the triage budget names the product cap (FP-3b). The expected values are
// independent literals, never derived from the production constants. No
// test reads design/, owner configuration or a vendor.

const (
	wantTriageAdapter = "codex"
	wantTriageModel   = "gpt-6.1-sol"
	wantTriageEffort  = "high"
	// oldDefaultSelectionSHA256 is the Grok-triage default selection's
	// digest (baseline 25a7707); it must never identify the new flow.
	oldDefaultSelectionSHA256 = "05161747afe28b37ef4666f32de5da47b2522d0b172ea61f97950860c56b0d1f"
	// wantPlatformRefusal is the live gate's platform refusal.
	wantPlatformRefusal = "reale2e run refused: live execution is linux only (the harness requires Linux-qualified vendor installations and sidecars)"
)

// The fake sidecars' enrolled nodes (fakes_test.go's enroll script).
var (
	fakeCodexNode = hexID("n_", 1)
	fakeGrokNode  = hexID("n_", 3)
)

// checkTriageRoles checks a plane's registrations: triage on the Codex
// sidecar with sol/high, beside the code reviewer, and no Grok role.
func checkTriageRoles(t *testing.T, p *fakePlane) {
	t.Helper()
	roles, _ := p.ListRoles(t.Context())
	if len(roles) != 5 {
		t.Fatalf("%d roles registered", len(roles))
	}
	byID := map[string]contract.RoleView{}
	for _, r := range roles {
		byID[r.ID] = r
		if r.Adapter == adapter.GrokID || r.Node == fakeGrokNode {
			t.Errorf("role %s is registered on the Grok sidecar (%s on %s)", r.ID, r.Adapter, r.Node)
		}
	}
	tr := byID["triage"]
	if tr.Adapter != wantTriageAdapter || tr.Model != wantTriageModel || tr.Effort != wantTriageEffort || tr.Node != fakeCodexNode ||
		byID["code-reviewer"].Node != fakeCodexNode || tr.Concurrency != 1 || tr.Timeout != FeatureTaskBound {
		t.Fatalf("triage registration %+v", tr.RoleConfig)
	}
}

// storedView is the part of a stored task view these tests read.
type storedView struct {
	TaskID    string            `json:"task_id"`
	Role      contract.TaskRole `json:"role"`
	Effective struct {
		Model   string `json:"model"`
		Effort  string `json:"effort"`
		Timeout string `json:"timeout"`
	} `json:"effective"`
}

// triageViews are every task view a bundle (or exercise root) stores, by
// path: each task.json and each wait-result.json's terminal task.
func triageViews(t *testing.T, root string) map[string]storedView {
	t.Helper()
	out := map[string]storedView{}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() && (e.Name() == "_captured" || e.Name() == "repository") {
			return fs.SkipDir
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if e.Name() != "task.json" && e.Name() != "wait-result.json" {
			return nil
		}
		var v struct {
			Task *storedView `json:"task"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(mustRead(t, p)), &v); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if v.Task != nil {
			out[rel] = *v.Task
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// checkTriageTasks checks every stored task view: each triage dispatch and
// wait ran on the Codex sidecar with effective sol/high, and no task ran
// on Grok. It returns the counts of triage workflow dispatches (task.json
// outside preflight) and triage waits.
func checkTriageTasks(t *testing.T, root string) (dispatched, waited int) {
	t.Helper()
	for rel, v := range triageViews(t, root) {
		if v.Role.Adapter == adapter.GrokID || v.Role.Node == fakeGrokNode {
			t.Errorf("%s: a %s task ran on Grok", rel, v.Role.ID)
		}
		if v.Role.ID != "triage" {
			continue
		}
		if v.Role.Adapter != wantTriageAdapter || v.Role.Node != fakeCodexNode || v.Effective.Model != wantTriageModel ||
			v.Effective.Effort != wantTriageEffort || v.Effective.Timeout != "10m0s" {
			t.Errorf("%s: triage ran as %+v / %+v", rel, v.Role, v.Effective)
		}
		switch {
		case strings.HasSuffix(rel, "wait-result.json"):
			waited++
		case !strings.Contains(rel, "preflight/"):
			dispatched++
		}
	}
	return dispatched, waited
}

// checkPlaneTasks checks the plane's own task records (what dispatch
// returned and every wait read): no Grok task.
func checkPlaneTasks(t *testing.T, p *fakePlane) {
	t.Helper()
	for _, id := range p.ids() {
		v, err := p.ShowTask(t.Context(), id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if v.Role.Adapter == adapter.GrokID {
			t.Errorf("task %s (%s) ran on Grok", id, v.Role.ID)
		}
		if v.Role.ID == "triage" && (v.Effective.Model != wantTriageModel || v.Effective.Effort != wantTriageEffort) {
			t.Errorf("triage task %s effective %+v", id, v.Effective)
		}
	}
}

func TestCodexTriageFP1(t *testing.T) {
	t.Parallel()
	conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
	impl, approve := devAct{marker: markerImplComplete}, devAct{marker: markerReviewApproved}
	t.Run("shipped_selection", func(t *testing.T) {
		t.Parallel()
		flow, err := ParseDevFullFlow(mustRead(t, filepath.Join(repoRoot(t), filepath.FromSlash(DevFullDir), FlowFile)))
		if err != nil {
			t.Fatal(err)
		}
		tr, ok := flow.Role("triage")
		if !ok || tr != (FlowRole{ID: "triage", Adapter: wantTriageAdapter, Model: wantTriageModel, Effort: wantTriageEffort}) || flow.Kind() != FlowDefault {
			t.Fatalf("the shipped triage row is %+v", tr)
		}
		for _, r := range flow.Roles {
			if r.Adapter == adapter.GrokID {
				t.Fatalf("the five-role example still selects Grok for %s", r.ID)
			}
		}
	})
	t.Run("full_recovery", func(t *testing.T) {
		t.Parallel()
		// Full-flow recovery triage uses the same (now Codex) role.
		r := newDevRig(t, []devAct{
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, impl, {marker: markerReviewChanges},
			{marker: markerNeedsTriage}, {marker: markerTriageProb, designRequired: designRequiredNo}, {marker: markerFixesComplete}, approve,
		})
		r.owner = []string{"yes 1"}
		r.expect(0, "")
		checkTriageRoles(t, r.f.plane)
		checkPlaneTasks(t, r.f.plane)
		if d, w := checkTriageTasks(t, r.f.bundle()); d != 1 || w != 1 || r.d.state.Tasks[5].Role != "triage" || r.d.state.Recovery != 1 {
			t.Fatalf("full recovery triage: %d dispatched, %d waited", d, w)
		}
		if code, rep := r.offline(); code != 0 {
			t.Fatalf("offline %d %v", code, devCodes(rep))
		}
	})
	t.Run("light_initial_recovery", func(t *testing.T) {
		t.Parallel()
		r := newLightRig(t, []devAct{conf, {marker: markerQuestions}, conf, impl, approve})
		r.expect(0, "")
		checkTriageRoles(t, r.f.plane)
		checkPlaneTasks(t, r.f.plane)
		if d, w := checkTriageTasks(t, r.f.bundle()); d != 2 || w != 2 {
			t.Fatalf("light triage: %d dispatched, %d waited", d, w)
		}
		if code, rep := r.offline(); code != 0 {
			t.Fatalf("offline %d %v", code, devCodes(rep))
		}
	})
	t.Run("exercise", func(t *testing.T) {
		t.Parallel()
		x := newExerciseRig(t, happy(), lightHappy())
		if code := x.run(); code != 0 {
			t.Fatalf("exit %d: %s", code, x.full.f.stderr.String())
		}
		if len(x.plane) != 2 {
			t.Fatalf("%d child planes", len(x.plane))
		}
		for _, p := range x.plane {
			checkTriageRoles(t, p)
			checkPlaneTasks(t, p)
		}
		if d, w := checkTriageTasks(t, x.root()); d != 1 || w != 1 {
			t.Fatalf("exercise triage: %d dispatched, %d waited", d, w)
		}
		var out, errb bytes.Buffer
		if code := CheckEvidence(x.root(), &out, &errb); code != 0 {
			t.Fatalf("root offline %d %s", code, errb.String())
		}
	})
	t.Run("probe", func(t *testing.T) {
		t.Parallel()
		r := probeLightRig(t, probeScopeBoth)
		r.expect(0, "")
		checkTriageRoles(t, r.f.plane)
		checkPlaneTasks(t, r.f.plane)
		if d, w := checkTriageTasks(t, r.f.bundle()); d != 1 || w != 1 {
			t.Fatalf("probe triage: %d dispatched, %d waited", d, w)
		}
		v := triageViews(t, r.f.bundle())["probe/triage/01/task.json"]
		if v.Role.ID != "triage" || v.Effective.Model != wantTriageModel {
			t.Fatalf("the probe's triage task is %+v", v.Role)
		}
		if codes := qualificationCodes(t, r.f.bundle()); len(codes) != 0 {
			t.Fatalf("codes %v", codes)
		}
	})
}

func TestCodexTriageFP2a(t *testing.T) {
	t.Parallel()
	on := func(k string) string { return map[string]string{EnvOptIn: "1"}[k] }
	none := func(string) (string, bool) { return "", false }
	t.Run("required_grok", func(t *testing.T) {
		t.Parallel()
		// --grok stays required and absolute for every live command.
		for _, cmd := range []string{"run", "run-dev-full", "run-dev-light", "run-dev-exercise", "probe-dev-full"} {
			for name, grok := range map[string][]string{"missing": nil, "relative": {"--grok", "grok"}} {
				args := append([]string{cmd, "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--evidence", "/e"}, grok...)
				var out, errOut bytes.Buffer
				h := Host{GOOS: "linux", Args: args, Getenv: on, LookupEnv: none, Stdout: &out, Stderr: &errOut,
					NewWorld: func(Host) (*World, func(), error) { t.Fatal("world built"); return nil, nil, nil }}
				if code := MainWith(h); code != 2 || !strings.Contains(errOut.String(), "usage:") {
					t.Errorf("%s %s --grok = %d %q", cmd, name, code, errOut.String())
				}
			}
		}
	})
	versionRefused := func(t *testing.T, r *devRig) {
		t.Helper()
		r.expect(1, "version_preflight_failed")
		if len(r.f.plane.ids()) != 0 {
			t.Fatalf("paid work after a Grok version refusal: %v", r.f.plane.ids())
		}
		for _, a := range r.f.launcher.argv() {
			if strings.Contains(a, " sidecar ") || strings.Contains(a, " plane ") {
				t.Fatalf("deployment started: %s", a)
			}
		}
		vs := r.manifest().Versions
		if len(vs) != 3 || vs[2].Tool != "grok" || vs[2].Eligible {
			t.Fatalf("versions %+v", vs)
		}
	}
	t.Run("grok_too_old", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.f.probeErr["grok"] = errors.New("grok 1.0.47 is older than the runtime minimum 1.0.52")
		versionRefused(t, r)
	})
	t.Run("grok_version_missing", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		prev := r.f.override
		r.f.override = func(s ProcSpec) (behavior, bool) {
			if s.Path == fakeGrok && slices.Equal(s.Args, []string{adapter.VersionArg}) {
				return behavior{exit: 1, stderr: "grok: not installed\n"}, true
			}
			return prev(s)
		}
		versionRefused(t, r)
	})
	t.Run("preflights_and_sidecars", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.owner = []string{"no 1"}
		r.expect(1, "owner_no")
		// Five paid per-role preflights: triage and the code reviewer
		// both on the Codex sidecar, under their own role IDs.
		if len(r.d.s.preflight) != 5 {
			t.Fatalf("%d preflights", len(r.d.s.preflight))
		}
		views := triageViews(t, r.f.bundle())
		ids := []string{"designer", "design-reviewer", "coder", "code-reviewer", "triage"}
		for i, id := range ids {
			var obs DevPreflightRecord
			if err := r.data("preflight/"+hopDir(i)+"/observation.json", &obs); err != nil || obs.Role != id || !obs.Answered {
				t.Fatalf("preflight %d %+v %v", i, obs, err)
			}
			v := views["preflight/"+hopDir(i)+"/task.json"]
			if v.Role.ID != id || v.TaskID != r.d.s.preflight[i] {
				t.Fatalf("preflight %d task %+v", i, v.Role)
			}
		}
		cr, tr := views["preflight/04/task.json"], views["preflight/05/task.json"]
		if cr.Role.Node != fakeCodexNode || tr.Role.Node != fakeCodexNode || cr.TaskID == tr.TaskID || tr.Effective.Model != wantTriageModel ||
			tr.Effective.Effort != wantTriageEffort {
			t.Fatalf("the Codex preflights %+v %+v", cr, tr)
		}
		// Three deployed sidecars, the idle Grok one included, with its
		// explicit adapter path; no task ever reaches the Grok node.
		argv := r.f.launcher.argv()
		for _, a := range []string{"codex", "claude", "grok"} {
			enrolled, ran := false, false
			for _, l := range argv {
				enrolled = enrolled || strings.HasPrefix(l, fakeCallsheet+" sidecar enroll ") && strings.HasSuffix(l, "n-"+a)
				ran = ran || strings.HasPrefix(l, fakeCallsheet+" sidecar run ") && strings.Contains(l, " --"+a+"-adapter "+r.f.args()[slices.Index(r.f.args(), "--"+a)+1])
			}
			if !enrolled || !ran {
				t.Fatalf("sidecar %s enrolled %t ran %t", a, enrolled, ran)
			}
		}
		for _, v := range views {
			if v.Role.Node == fakeGrokNode {
				t.Fatalf("a task ran on the idle Grok sidecar: %+v", v.Role)
			}
		}
		// Its cleanup is verified like the others'.
		cl := r.cleanupRecord()
		stopped := map[string]int{}
		for _, p := range cl.Processes {
			if strings.HasPrefix(p.Name, "sidecar-") && p.ExitVerified {
				stopped[p.Name]++
			}
		}
		if !cl.Complete || stopped["sidecar-grok"] == 0 || stopped["sidecar-codex"] == 0 || stopped["sidecar-claude"] == 0 {
			t.Fatalf("sidecar cleanup %v complete %t", stopped, cl.Complete)
		}
		var tools []string
		for _, v := range r.manifest().Versions {
			tools = append(tools, v.Tool)
		}
		if !slices.Equal(tools, []string{"claude", "codex", "grok", "go", "git"}) {
			t.Fatalf("version records %v", tools)
		}
	})
	t.Run("live_darwin_refusal", func(t *testing.T) {
		t.Parallel()
		// The selected roles' Darwin posture does not bypass the live gate.
		for _, args := range [][]string{
			{"probe-dev-full", "--scenario", "both", "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", "/e"},
			{"run-dev-light", "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", "/e"},
		} {
			var out, errOut bytes.Buffer
			h := Host{GOOS: "darwin", GOARCH: "arm64", Args: args, Getenv: on, LookupEnv: none, Stdout: &out, Stderr: &errOut,
				NewWorld: func(Host) (*World, func(), error) { t.Fatal("world built"); return nil, nil, nil }}
			if code := MainWith(h); code != 2 || !strings.Contains(errOut.String(), wantPlatformRefusal) || out.Len() != 0 {
				t.Fatalf("%s = %d %q", args[0], code, errOut.String())
			}
		}
		if err := DefaultDevFullFlow().CheckDevPosture("darwin"); err != nil {
			t.Fatalf("the selected roles are not Darwin-eligible: %v", err)
		}
	})
}

// grokTriageFlow is the obsolete shipped flow: the baseline's Grok triage
// row in the current schema.
const grokTriageFlow = `{
  "schema": "callsheet-dev-full-flow/v1",
  "roles": [
    {"id": "designer", "adapter": "codex", "model": "gpt-6-astra", "effort": "low"},
    {"id": "design-reviewer", "adapter": "claude", "model": "claude-fable-5-1", "effort": "low"},
    {"id": "coder", "adapter": "claude", "model": "claude-opus-5-5", "effort": "high"},
    {"id": "code-reviewer", "adapter": "codex", "model": "gpt-6.1-sol", "effort": "high"},
    {"id": "triage", "adapter": "grok", "model": "grok-4.7", "effort": "xhigh"}
  ]
}
`

func TestCodexTriageFP2b(t *testing.T) {
	t.Parallel()
	def, err := DefaultDevFullFlow().SelectionCanonical()
	if err != nil {
		t.Fatal(err)
	}
	newDigest := sha256Hex(def)
	t.Run("digests", func(t *testing.T) {
		t.Parallel()
		// The old digest is the baseline selection's canonical encoding
		// (the same encoder, without the now-failing validation).
		var old DevFullFlow
		if err := json.Unmarshal([]byte(grokTriageFlow), &old); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(old); err != nil {
			t.Fatal(err)
		}
		if got := sha256Hex(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))); got != oldDefaultSelectionSHA256 {
			t.Fatalf("the baseline selection digest is %s", got)
		}
		if newDigest == oldDefaultSelectionSHA256 || len(newDigest) != 64 {
			t.Fatalf("the new selection digest %s", newDigest)
		}
		if _, err := old.SelectionCanonical(); err == nil {
			t.Fatal("the obsolete selection still canonicalizes")
		}
		// Insignificant whitespace changes the flow bytes, not the
		// selection digest.
		raw := mustRead(t, filepath.Join(repoRoot(t), filepath.FromSlash(DevFullDir), FlowFile))
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			t.Fatal(err)
		}
		spaced := bytes.ReplaceAll(compact.Bytes(), []byte(","), []byte(",\n\t "))
		for _, b := range [][]byte{raw, compact.Bytes(), spaced} {
			f, err := ParseDevFullFlow(b)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := f.SelectionCanonical()
			if sha256Hex(got) != newDigest || f.Kind() != FlowDefault {
				t.Fatalf("whitespace changed the selection: %s", got)
			}
		}
		if sha256Hex(raw) == sha256Hex(spaced) {
			t.Fatal("the whitespace fixture is the shipped bytes")
		}
	})
	t.Run("old_grok_flow_refused", func(t *testing.T) {
		t.Parallel()
		// An old local override with the Grok triage row is a fixed-adapter
		// mismatch, refused before any deployment or paid work.
		if _, err := ParseDevFullFlow([]byte(grokTriageFlow)); err == nil || !strings.Contains(err.Error(), "role triage must use adapter codex") {
			t.Fatalf("the obsolete flow parsed: %v", err)
		}
		path := filepath.Join(t.TempDir(), "flow.json")
		if err := os.WriteFile(path, []byte(grokTriageFlow), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, cmd := range []string{"", "run-dev-light", "probe-dev-full"} {
			r := newDevRig(t, ownerNoSteps())
			r.command, r.f.flow = cmd, path
			if cmd == "probe-dev-full" {
				r.extraArgs = []string{"--scenario", probeScopeBoth}
			}
			code := r.run()
			if r.d == nil || r.d.stopCode != "flow_invalid" || code == 0 {
				t.Fatalf("%q: exit %d stop %v", cmd, code, r.d)
			}
			if len(r.f.plane.ids()) != 0 || len(r.procs) != 0 {
				t.Fatalf("%q: paid work after the refusal", cmd)
			}
			for _, a := range r.f.launcher.argv() {
				if strings.Contains(a, " sidecar ") {
					t.Fatalf("%q: deployment started: %s", cmd, a)
				}
			}
		}
	})
	t.Run("probe_round_trip", func(t *testing.T) {
		t.Parallel()
		// The scripted both-scope probe binds the new digest and roles
		// under the unchanged qualification and bundle schemas.
		dir := probeLightBundle(t)
		b, err := LoadDevQualification(dir)
		if err != nil {
			t.Fatal(err)
		}
		if codes := CheckDevQualification(b); len(codes) != 0 {
			t.Fatalf("codes %v", codes)
		}
		q := doc[DevQualification](b, devQualificationFile)
		if q == nil || q.Schema != "callsheet-dev-qualification/v2" || q.Result != ResultQualified || q.Scope != probeScopeBoth ||
			q.SelectionSHA256 != newDigest || q.SelectionSHA256 == oldDefaultSelectionSHA256 || q.FlowLabel != FlowDefault {
			t.Fatalf("qualification %+v", q)
		}
		if !slices.Equal(q.Roles, DefaultDevFullFlow().Roles) || q.Roles[4].Adapter != wantTriageAdapter {
			t.Fatalf("captured roles %+v", q.Roles)
		}
		var rr RolesRecord
		if err := json.Unmarshal(mustRead(t, filepath.Join(dir, "setup", "roles.json")), &rr); err != nil || len(rr.Roles) != 5 {
			t.Fatalf("registered roles %+v %v", rr, err)
		}
		for i, r := range rr.Roles {
			want := DefaultDevFullFlow().Roles[i]
			if r.ID != want.ID || r.Adapter != want.Adapter || r.Model != want.Model || r.Effort != want.Effort {
				t.Fatalf("registered role %d %+v", i, r)
			}
		}
	})
	t.Run("selection_tampering", func(t *testing.T) {
		t.Parallel()
		// A capable forger rewriting the selection digest, or the triage
		// row back to Grok, everywhere it is recorded: the offline check
		// refuses both.
		for name, pairs := range map[string][]string{
			"digest": {newDigest, oldDefaultSelectionSHA256},
			"grok_row": {`"id":"triage","adapter":"codex","model":"gpt-6.1-sol","effort":"high"`, `"id":"triage","adapter":"grok","model":"grok-4.7","effort":"xhigh"`,
				`{"id": "triage", "adapter": "codex", "model": "gpt-6.1-sol", "effort": "high"}`, `{"id": "triage", "adapter": "grok", "model": "grok-4.7", "effort": "xhigh"}`},
		} {
			for kind, bundle := range map[string]func(*testing.T) string{"full": devPassingBundle, "light": lightPassingBundle} {
				t.Run(name+"_"+kind, func(t *testing.T) {
					t.Parallel()
					dir := bundle(t)
					if code, rep, _ := devCheck(t, dir); code != 0 {
						t.Fatalf("the untouched bundle fails: %v", devCodes(rep))
					}
					var m DevManifest
					if err := json.Unmarshal(mustRead(t, filepath.Join(dir, devManifestFile)), &m); err != nil || m.Schema != "callsheet-dev-evidence/v2" ||
						m.Scenario != kind || m.SelectionSHA256 != newDigest || !slices.Equal(m.Roles, DefaultDevFullFlow().Roles) {
						t.Fatalf("manifest %s %s %s %+v %v", m.Schema, m.Scenario, m.SelectionSHA256, m.Roles, err)
					}
					replaceAll(t, dir, pairs...)
					if code, rep := reforge(t, dir); code == 0 || rep.Result == ResultPass {
						t.Fatalf("the %s forgery passed", name)
					}
				})
			}
		}
	})
}

func TestCodexTriageFP3a(t *testing.T) {
	t.Parallel()
	conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
	impl, approve := devAct{marker: markerImplComplete}, devAct{marker: markerReviewApproved}
	t.Run("literal_fields", func(t *testing.T) {
		t.Parallel()
		// The new selection's light and probe triage dispatches carry the
		// unchanged literal scratch fields; other roles carry none.
		r := newLightRig(t, []devAct{conf, impl, {marker: markerReviewChanges}, {marker: markerNeedsTriage}, conf, {marker: markerFixesComplete}, approve})
		r.expect(0, "")
		if kinds := checkRigScratch(t, r.docs, r.f.plane); !slices.Equal(kinds, []string{"initial", "recovery"}) {
			t.Fatalf("triage kinds %v", kinds)
		}
		checkTriageRoles(t, r.f.plane)
		p := probeLightRig(t, probeScopeBoth)
		p.expect(0, "")
		if kinds := checkRigScratch(t, p.docs, p.f.plane); !slices.Equal(kinds, []string{"probe"}) {
			t.Fatalf("probe triage kinds %v", kinds)
		}
		for _, d := range append(slices.Clone(r.docs), p.docs...) {
			if d.Role == "triage" && goalField(d.Goal, "scratch_policy") != wantScratchRecipe {
				t.Fatalf("ordinal %d: scratch_policy %q", d.Ordinal, goalField(d.Goal, "scratch_policy"))
			}
		}
	})
	t.Run("report_only_control", func(t *testing.T) {
		t.Parallel()
		r := newLightRig(t, lightHappy())
		r.expect(0, "")
		if code, rep := r.offline(); code != 0 {
			t.Fatalf("offline %d %v", code, devCodes(rep))
		}
		if d, _ := checkTriageTasks(t, r.f.bundle()); d != 1 {
			t.Fatalf("%d triage dispatches", d)
		}
	})
	t.Run("populated_scratch_light", func(t *testing.T) {
		t.Parallel()
		triage := conf
		triage.alter = "scratch-left"
		r := newLightRig(t, []devAct{triage, impl, approve})
		r.expect(1, "publication_rejected")
		for _, d := range r.docs {
			if d.Role != "triage" {
				t.Fatalf("a %s was dispatched after the rejected Codex triage publication", d.Role)
			}
		}
		if d, _ := checkTriageTasks(t, r.f.bundle()); d != 1 {
			t.Fatalf("%d triage dispatches", d)
		}
	})
	t.Run("populated_scratch_probe", func(t *testing.T) {
		t.Parallel()
		r := probeLightRig(t, probeScopeBoth)
		r.steps[4].alter = "scratch-left"
		r.expect(1, "publication_rejected")
		q := r.qualification()
		got := map[string]string{}
		for _, s := range q.Stages {
			got[s.Name] = s.Result
		}
		if q.Result != ResultBlocked || got["triage_evidence"] != stageFail || got["light_reviewer_evidence"] != stageUnreached {
			t.Fatalf("%+v", q.Stages)
		}
		var roles []string
		for _, d := range r.docs {
			roles = append(roles, d.Role)
		}
		if !slices.Equal(roles, []string{"designer", "designer", "designer", "code-reviewer", "triage"}) {
			t.Fatalf("roles %v: the light reviewer was reached", roles)
		}
	})
}

func TestCodexTriageFP3b(t *testing.T) {
	t.Parallel()
	t.Run("product_cap", func(t *testing.T) {
		t.Parallel()
		if triagePromptCap != 32768 || triageManualCap != 20480 || triageTaskCeiling != 8192 || devPromptReserve != 4096 {
			t.Fatalf("limits %d %d %d %d", triagePromptCap, triageManualCap, triageTaskCeiling, devPromptReserve)
		}
		// The budget source names the product cap and no vendor adapter.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(repoRoot(t), "internal", "reale2e", "devfull_budget.go"), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range f.Imports {
			if strings.Contains(im.Path.Value, "internal/adapter") {
				t.Fatal("devfull_budget.go imports the adapter package")
			}
		}
		uses := 0
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				switch id.Name {
				case "MaxGrokPromptBytes":
					t.Error("devfull_budget.go names the Grok adapter cap")
				case "triagePromptCap":
					uses++
				}
			}
			return true
		})
		if uses != 3 {
			t.Fatalf("triagePromptCap appears %d times (one declaration, two comparisons)", uses)
		}
		// The shipped manuals: 9228 raw / 9301 encoded, 21516 / 21589 with
		// the task ceiling and reserve, under the cap (design
		// 12a-triage-scratch-guard-wording, Unchanged budgets and measured
		// headroom).
		loaded, err := LoadDevMethod(repoRoot(t))
		if err != nil {
			t.Fatal(err)
		}
		in, rb, err := loaded.ManualPair("triage")
		if err != nil {
			t.Fatal(err)
		}
		el, _ := jsonContentLen(in)
		er, _ := jsonContentLen(rb)
		if raw := len(in) + len(rb); raw != 9228 || el+er != 9301 || raw+8192+4096 != 21516 || el+er+8192+4096 != 21589 {
			t.Fatalf("the triage manuals are %d raw / %d encoded", raw, el+er)
		}
		if err := checkTriageManualBudget(in, rb); err != nil {
			t.Fatal(err)
		}
		// The cap's raw boundary (20480 + 8192 + 4096 = 32768) is admitted;
		// an encoded overflow of raw-admissible manuals is refused.
		half := bytes.Repeat([]byte("a"), 10240)
		if err := checkTriageManualBudget(half, half); err != nil {
			t.Fatalf("manuals at the cap: %v", err)
		}
		if err := checkTriageManualBudget(append(half, 'a'), half); err == nil {
			t.Fatal("manuals over 20480 were accepted")
		}
		esc := bytes.Repeat([]byte(`\`), 10000)
		if err := checkTriageManualBudget(esc, esc[:2000]); err == nil || !errors.Is(err, errPromptBudget) || !strings.Contains(err.Error(), "encoded triage envelope") {
			t.Fatalf("escape-heavy manuals: %v", err)
		}
		// The representative composition with the new effective selection
		// fits the product cap, not the Grok adapter's.
		goal, acc := strings.Repeat("g", 6000), strings.Repeat("a", 2192)
		env, _ := json.Marshal(map[string]any{"format": "callsheet-task-v1", "instruction": string(in), "runbook": string(rb),
			"task": map[string]any{"task_id": hexID("t_", 1), "role": map[string]string{"id": "triage", "name": "triage"}, "goal": goal, "acceptance": acc,
				"payload": []string{}, "effective": contract.TaskEffective{Model: wantTriageModel, Effort: wantTriageEffort, Timeout: FeatureTaskBound}}})
		if len(env) > triagePromptCap {
			t.Fatalf("the representative triage prompt is %d bytes", len(env))
		}
	})
	t.Run("fixed_fixtures", func(t *testing.T) {
		t.Parallel()
		// Unchanged by the selection: worst 7879 raw / 7938 encoded, the
		// 8133 / 8192 boundary and its refusals.
		checkScratchBudgets(t)
	})
	t.Run("oversized_recovery", func(t *testing.T) {
		t.Parallel()
		// Findings that cannot fit a recovery triage envelope stop the
		// attempt before the triage is exposed or dispatched, in both
		// flows.
		conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
		for name, c := range map[string]struct {
			steps []devAct
			light bool
			tasks int
		}{
			"light": {[]devAct{conf, {marker: markerQuestions, alter: "long-final"}}, true, 2},
			"full": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
				{marker: markerNeedsTriage, alter: "long-final"}}, false, 5},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var r *devRig
				if c.light {
					r = newLightRig(t, c.steps)
				} else {
					r = newDevRig(t, c.steps)
					r.owner = []string{"yes 1"}
				}
				r.expect(1, CodePromptBudget)
				if len(r.workflowTasks()) != c.tasks || len(r.docs) != c.tasks {
					t.Fatalf("%d tasks, %d documents after the refusal", len(r.workflowTasks()), len(r.docs))
				}
				triages := 0
				for _, d := range r.docs {
					if d.Role == "triage" {
						triages++
					}
				}
				if want := map[bool]int{true: 1, false: 0}[c.light]; triages != want {
					t.Fatalf("%d triage dispatches", triages)
				}
			})
		}
	})
}
