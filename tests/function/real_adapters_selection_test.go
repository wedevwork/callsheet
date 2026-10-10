//go:build linux || darwin

package function

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/sidecar"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
	"github.com/wedevwork/callsheet/internal/testkit/workersmoke"
)

// Design 12a-worker-selection's function tests: distinct subtests under
// the existing RealAdapter parents (no new native inventory name). Every
// expectation is a literal (the effort unions, argv arrays and versions
// below), never derived from the production tables. Alternate selections
// replay recorded bytes under explicitly synthetic normalization: they
// prove Callsheet's routing of the selected model and effort, never a real
// execution of that pair.

// The effort unions (design 12a-worker-selection, Selection policy).
var effortUnions = map[string][]string{
	"claude": {"low", "medium", "high", "xhigh", "max"},
	"codex":  {"low", "medium", "high", "xhigh", "max", "ultra"},
	"grok":   {"low", "medium", "high", "xhigh"},
	"cursor": {"none", "minimal", "low", "medium", "high", "xhigh", "max"},
}

// newerHostVersions are the newer host versions the coordinator recorded
// on 2026-10-10 (Claude and Codex update themselves): above the minimums.
var newerHostVersions = map[string]string{"claude": "2.1.292 (Claude Code)", "codex": "codex-cli 0.160.0"}

// The literal recipes of a selection ({final} for Codex's final path).
func claudeRecipe(model, effort string) []string {
	return []string{"-p", "--model", model, "--effort", effort, "--permission-prompts", "none", "--output-format", "json"}
}

func codexRecipe(model, effort string) []string {
	return []string{"-a", "never", "exec", "--model", model, "-c", "model_reasoning_effort=\"" + effort + "\"", "--json", "--skip-git-repo-check",
		"--output-last-message", "{final}", "-"}
}

// selectionScenario replays capture rel for goal under the literal argv of
// a requested selection.
func selectionScenario(t testing.TB, vendor, goal, rel string, argv []string) scenario {
	sc := captureScenario(t, vendor, goal, rel)
	sc.ExpectedArgv, sc.Synthetic = argv, true
	sc.Normalization = "synthetic selection routing: the production argv of the requested selection replays this capture's recorded bytes; " +
		"it proves Callsheet passed the selected model and effort, never a real execution of that pair"
	return sc
}

// withFinal is argv with the placeholder replaced by the launch's final
// path.
func withFinal(argv []string, l launch) []string {
	out := slices.Clone(argv)
	for i, a := range out {
		if a == "{final}" {
			out[i] = filepath.Join(l.Cwd, adapter.CodexFinalName)
		}
	}
	return out
}

// taggedAdapterBinary is the prebuilt realadaptercheck adapter test binary
// (the selection, invocation and version matrices).
func taggedAdapterBinary(t *testing.T) string {
	return fixture(t, "adapter-realadaptercheck", func(dir string) (string, error) {
		return testkit.BuildTaggedTestBinaryAt(dir, "./internal/adapter", "internal-adapter-realadaptercheck", "realadaptercheck")
	})
}

// delegateAdapter runs selector in the tagged adapter binary with RUN and
// PASS evidence for every name.
func delegateAdapter(t *testing.T, selector string, names ...string) {
	t.Helper()
	delegate(t, taggedAdapterBinary(t), "./internal/adapter", selector, names...)
}

// selectionRig is design 12a-worker-selection's role-mutation deployment:
// its own plane and worker S enabling the claude and codex replay stubs,
// which print the newer recorded host versions, with role sel-codex
// (codex, the observed pair) registered at start. Role changes happen only
// here, never on the shared rig: after a role change the plane's other
// workers would refuse starts as role_changed (iteration 08's known
// issue), so the shared rig changes no role once dispatching starts.
// serial orders this deployment's role changes and dispatches (each
// sequence waits for the changed role's readiness, so no start can carry
// a revision worker S has not installed).
type selectionRig struct {
	*realRig
	serial sync.Mutex
}

var (
	selOnce sync.Once
	theSel  *selectionRig
	selErr  error
)

func sharedSelectionRig(t *testing.T) *selectionRig {
	t.Helper()
	bin, stub := nodeBinary(t), replayBinary(t)
	selOnce.Do(func() { theSel, selErr = startSelectionRig(bin, stub) })
	if selErr != nil {
		t.Fatal(selErr)
	}
	return theSel
}

func startSelectionRig(bin, stub string) (*selectionRig, error) {
	root, err := os.MkdirTemp("", "callsheet-selection-")
	if err != nil {
		return nil, err
	}
	r := &selectionRig{realRig: &realRig{root: root}}
	fixtureMu.Lock()
	afterSuite = append(afterSuite, r.close)
	fixtureMu.Unlock()
	if r.kit, err = newReplayKit(root, stub, newerHostVersions); err != nil {
		return nil, err
	}
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o700)
	r.env = []string{"PATH=" + r.kit.traps, "HOME=" + home}
	ctx, cancel := context.WithTimeout(context.Background(), rigWait)
	defer cancel()
	if r.dep, err = workersmoke.Start(ctx, bin, root, r.env); err != nil {
		return nil, err
	}
	if r.a, err = r.dep.StartSidecar(ctx, "worker-s", r.sidecarEnv(r.kit), "--claude-adapter", r.kit.claude, "--codex-adapter", r.kit.codex); err != nil {
		return nil, err
	}
	r.insTxt, r.runTxt = "Selection instruction: answer the goal.\n", "Selection runbook.\n"
	if r.ins, r.run, err = r.dep.Manuals("selection", r.insTxt, r.runTxt); err != nil {
		return nil, err
	}
	if _, err := r.dep.Client.AddRole(ctx, contract.RoleConfig{ID: "sel-codex", Name: "sel-codex", Node: r.a.NodeID, Adapter: "codex", Instruction: r.ins,
		Runbook: r.run, Model: "gpt-6.1-sol", Effort: "low", Concurrency: 4}); err != nil {
		return nil, err
	}
	return r, r.ready(ctx, "sel-codex")
}

// role is a selection-rig role config of vendor with an explicit pair.
func (r *selectionRig) role(id, vendor, model, effort string) contract.RoleConfig {
	return contract.RoleConfig{ID: id, Name: id, Node: r.a.NodeID, Adapter: vendor, Instruction: r.ins, Runbook: r.run, Model: model, Effort: effort, Concurrency: 2}
}

// removeRoles removes the roles a serialized sequence added (deferred
// while serial is held, so repeated runs never reach the plane's role
// limit and no removal overlaps another sequence's dispatch). Forced
// removal also ends any task a failed sequence left behind.
func (r *selectionRig) removeRoles(t *testing.T, ids ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rigWait)
	defer cancel()
	for _, id := range ids {
		for {
			_, err := r.dep.Client.RemoveRole(ctx, id, true, "")
			if err == nil || contract.CodeOf(err) == contract.CodeNotFound {
				break
			}
			if !retryable(err) || ctx.Err() != nil {
				t.Errorf("remove role %s: %v", id, err)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// await2 is ready within a fresh rig bound.
func (r *selectionRig) await2(t *testing.T, ids ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rigWait)
	defer cancel()
	if err := r.ready(ctx, ids...); err != nil {
		t.Fatalf("%v\n%s", err, r.a.Logs.String())
	}
}

// taskCount is the plane's number of tasks.
func taskCount(t *testing.T, c interface {
	ListTasks(context.Context, string, int) ([]contract.TaskSummary, *string, error)
}) int {
	t.Helper()
	n, after := 0, ""
	for {
		page, next, err := c.ListTasks(context.Background(), after, 0)
		if err != nil {
			t.Fatal(err)
		}
		n += len(page)
		if next == nil {
			return n
		}
		after = *next
	}
}

// mcpDispatch dispatches args through m, retrying the plane's documented
// transient refusal; it returns the task ID or the tool error.
func mcpDispatch(t *testing.T, m *mcpProc, args map[string]any) (string, *contract.Error) {
	t.Helper()
	deadline := time.Now().Add(rigWait)
	for {
		res := m.call("dispatch", args)
		if !res.isError {
			return taskIDOf(t, res.text), nil
		}
		e, err := contract.ParseErrorBody([]byte(res.text))
		if err != nil {
			t.Fatalf("dispatch: %s", res.text)
		}
		if !retryable(e) || time.Now().After(deadline) {
			return "", e
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mcpTerminal waits through task_wait until id is terminal and returns its
// task_show view.
func mcpTerminal(t *testing.T, m *mcpProc, id string) contract.TaskView {
	t.Helper()
	deadline := time.Now().Add(rigWait)
	for !strings.Contains(m.ok("task_wait", map[string]any{"task_ids": []string{id}, "wait": "5s"}), `"status":"terminal"`) {
		if time.Now().After(deadline) {
			t.Fatalf("task %s never terminal", id)
		}
	}
	v, err := contract.ParseTaskShowResponse([]byte(m.ok("task_show", map[string]any{"id": id})))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// unionPolicy is TestRealAdapterRegistration/selection/union-policy (FP-1).
func unionPolicy(t *testing.T) {
	ids := []string{"claude", "codex", "cursor", "grok"}
	t.Run("free-model", func(t *testing.T) {
		delegateAdapter(t, "^TestWorkerSelectionPolicy$/^free-model$", "TestWorkerSelectionPolicy", "TestWorkerSelectionPolicy/free-model")
		for _, id := range ids {
			for _, m := range []string{"claude-opus-6-0", "gpt-7-nova", "grok-5", "a future model", "-x", strings.Repeat("m", 1024)} {
				if err := adapter.ValidateSelection(id, m, "low"); err != nil {
					t.Fatalf("%s %q: %v", id, m, err)
				}
			}
			for _, m := range []string{"", " ", "\t", "m\n", "\xff", strings.Repeat("m", 1025)} {
				var se *adapter.SelectionError
				if err := adapter.ValidateSelection(id, m, "low"); !errors.As(err, &se) || se.Field != "model" || err.Error() != "adapter: invalid model" {
					t.Fatalf("%s %q: %v", id, m, err)
				}
			}
		}
	})
	t.Run("effort-union", func(t *testing.T) {
		delegateAdapter(t, "^TestWorkerSelectionPolicy$/^(effort-union|precedence)$", "TestWorkerSelectionPolicy", "TestWorkerSelectionPolicy/effort-union",
			"TestWorkerSelectionPolicy/precedence")
		for _, id := range ids {
			info, ok := adapter.Lookup()(id)
			if !ok || info.TestOnly || !slices.Equal(info.Efforts, effortUnions[id]) {
				t.Fatalf("%s metadata %+v", id, info)
			}
			for _, e := range effortUnions[id] {
				if err := adapter.ValidateSelection(id, "any-model", e); err != nil {
					t.Fatalf("%s %s: %v", id, e, err)
				}
			}
			for _, e := range []string{"", "LOW", "fast", "extra-high", "turbo"} {
				var se *adapter.SelectionError
				if err := adapter.ValidateSelection(id, "any-model", e); !errors.As(err, &se) || se.Field != "effort" ||
					err.Error() != "adapter: effort is not allowed for adapter "+id+"; allowed: "+strings.Join(effortUnions[id], ", ") {
					t.Fatalf("%s %q: %v", id, e, err)
				}
			}
		}
		// Cursor's selection is validated independently of its posture: a
		// valid selection is still never eligible on any OS.
		for _, goos := range []string{"linux", "darwin"} {
			if adapter.ValidateSelection("cursor", "grok-4.7", "max") != nil || adapter.ValidateWorkerPosture("cursor", goos) == nil {
				t.Fatalf("cursor selection or posture on %s", goos)
			}
		}
	})
	t.Run("descriptor-copies", func(t *testing.T) {
		delegateAdapter(t, "^TestWorkerSelectionPolicy$/^descriptor-copies$", "TestWorkerSelectionPolicy", "TestWorkerSelectionPolicy/descriptor-copies")
		r := adapter.Builtin("")
		ds := r.Descriptors()
		for _, d := range ds {
			if d.ID != "fake" && !slices.Equal(d.Efforts, effortUnions[d.ID]) {
				t.Fatalf("descriptor %+v", d)
			}
			d.Efforts[0] = "mutated"
		}
		for _, id := range ids {
			a, _ := r.Lookup(id)
			if !slices.Equal(a.Descriptor().Efforts, effortUnions[id]) {
				t.Fatalf("%s descriptor mutated", id)
			}
			info, _ := adapter.Lookup()(id)
			info.Efforts[0] = "mutated"
			if again, _ := adapter.Lookup()(id); !slices.Equal(again.Efforts, effortUnions[id]) {
				t.Fatalf("%s lookup mutated", id)
			}
		}
	})
	t.Run("qualification-evidence", func(t *testing.T) {
		delegateAdapter(t, "^TestWorkerSelectionPolicy$/^qualification-evidence$", "TestWorkerSelectionPolicy", "TestWorkerSelectionPolicy/qualification-evidence")
		want := []adapter.Qualification{{ID: "claude", Version: "2.1.285 (Claude Code)", Model: "sonnet", Effort: "low"},
			{ID: "codex", Version: "codex-cli 0.159.0", Model: "gpt-6.1-sol", Effort: "low"}, {ID: "cursor", Version: "2026.10.01-e373342", Model: "grok-4.7", Effort: "low"},
			{ID: "grok", Version: "grok 1.0.46 (2765805b9442) [stable]", Model: "grok-4.7", Effort: "low"}}
		qs := adapter.Qualifications()
		if !slices.Equal(qs, want) {
			t.Fatalf("observations %+v", qs)
		}
		qs[0].Model = "mutated"
		if !slices.Equal(adapter.Qualifications(), want) {
			t.Fatal("the observations are shared")
		}
		// The observed pair restricts nothing: other models and efforts pass.
		for _, q := range want {
			for _, e := range effortUnions[q.ID] {
				if adapter.ValidateSelection(q.ID, q.Model, e) != nil || adapter.ValidateSelection(q.ID, "not-"+q.Model, e) != nil {
					t.Fatalf("%s %s refused", q.ID, e)
				}
			}
		}
	})
}

// roleLifecycle is TestRealAdapterRegistration/selection/role-lifecycle
// (FP-4): on the selection deployment, alternate valid roles are added and
// changed and become ready through worker S's real version probes of the
// newer banners; an invalid change is refused and leaves the role as it
// was. The tagged sidecar contract proves repeated checks, version
// transitions and independent failures.
func roleLifecycle(t *testing.T) {
	delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^selection$", "TestRealAdapterLocal", "TestRealAdapterLocal/selection")
	s := sharedSelectionRig(t)
	s.serial.Lock()
	defer s.serial.Unlock()
	ctx := context.Background()
	id := s.unique("life-claude")
	defer s.removeRoles(t, id)
	if _, err := s.addRole(ctx, s.role(id, "claude", "claude-opus-5-5", "high")); err != nil {
		t.Fatal(err)
	}
	s.await2(t, id)
	set := func(model, effort string) (contract.RoleView, error) {
		for {
			v, err := s.dep.Client.SetRole(ctx, id, contract.RolePatch{Model: &model, Effort: &effort})
			if err == nil || !retryable(err) {
				return v, err
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if _, err := set("claude-fable-5-1", "low"); err != nil {
		t.Fatal(err)
	}
	s.await2(t, id)
	v, err := s.dep.Client.ShowRole(ctx, id)
	if err != nil || v.Model != "claude-fable-5-1" || v.Effort != "low" || !v.CanAccept || v.AdapterTestOnly {
		t.Fatalf("changed role %+v %v", v, err)
	}
	// An effort outside the union is refused by the plane; the role keeps
	// its selection and readiness.
	if _, err := set("claude-fable-5-1", "ultra"); contract.CodeOf(err) != contract.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "effort is not allowed for adapter claude; allowed: low, medium, high, xhigh, max") {
		t.Fatalf("out-of-union change: %v", err)
	}
	if v, err := s.dep.Client.ShowRole(ctx, id); err != nil || v.Model != "claude-fable-5-1" || v.Effort != "low" || !v.CanAccept {
		t.Fatalf("role after a refused change %+v %v", v, err)
	}
	// Every eligible probe ran against the newer banners.
	for _, vendor := range []string{"claude", "codex"} {
		exe := map[string]string{"claude": s.kit.claude, "codex": s.kit.codex}[vendor]
		if ls := launches(t, s.kit, func(l launch) bool { return l.Argv0 == exe && l.Mode == "version" && l.OK }); len(ls) == 0 {
			t.Fatalf("no %s version probe on worker S", vendor)
		}
	}
}

// explicitSelection is TestRealAdapterInvocation/claude/explicit-selection
// (FP-2): a per-task override reaches Claude's recipe exactly; the other
// callable recipes and Cursor's refusal for the same kind of alternate
// selections; the tagged invocation matrix.
func explicitSelection(t *testing.T, r *realRig) {
	goal := r.goal("explicit selection claude")
	req := requestFor("rc", goal)
	m, e := "claude-opus-5-5", "high"
	req.Override = &contract.TaskOverride{Model: &m, Effort: &e}
	sc := selectionScenario(t, "claude", goal, "runs-scratch/claude-stdin-success", claudeRecipe(m, e))
	stdin := r.composedWith(t, req, "rc", contract.TaskEffective{Model: m, Effort: e, Timeout: contract.DefaultRoleTimeout})
	sc.ExpectedStdin = &stdin
	writeScenario(t, r.kit, sc)
	f := r.dispatch(t, req)
	if f.view.State != contract.TaskSucceeded || finalOf(f) != "pong" || f.view.Effective.Model != m || f.view.Effective.Effort != e {
		t.Fatalf("explicit selection %s (effective %+v)", f, f.view.Effective)
	}
	l := taskLaunch(t, r.kit, f.view.TaskID)
	if !slices.Equal(l.Argv, claudeRecipe(m, e)) || l.Argv0 != r.kit.claude || string(l.Stdin) != strings.ReplaceAll(stdin, "{task_id}", f.view.TaskID) {
		t.Fatalf("claude launch %q", l.Argv)
	}
	// The other callable recipes with alternate selections, and Cursor's
	// zero invocation for valid ones (no process).
	tid := "t_" + strings.Repeat("0", 32)
	codex, _ := adapter.Builtin("").Lookup("codex")
	inv, err := codex.Invocation(adapter.TaskInput{TaskID: tid, Model: "gpt-6-astra", Effort: "ultra", Prompt: []byte("p"), ScratchDir: "/scratch"})
	if want := withFinal(codexRecipe("gpt-6-astra", "ultra"), launch{Cwd: "/scratch"}); err != nil || !slices.Equal(inv.Argv, want) {
		t.Fatalf("codex alternate %q %v", inv.Argv, err)
	}
	grok, _ := adapter.Builtin("").Lookup("grok")
	inv, err = grok.Invocation(adapter.TaskInput{TaskID: tid, Model: "grok-4.7-build-fast", Effort: "low", Prompt: []byte("P")})
	if err != nil || !slices.Equal(inv.Argv, []string{"--output-format", "json", "--model", "grok-4.7-build-fast", "--reasoning-effort", "low", "--permission-mode",
		"dontAsk", "-p", "P"}) || inv.Stdin != nil {
		t.Fatalf("grok fast %q %v", inv.Argv, err)
	}
	cursor, _ := adapter.Builtin("").Lookup("cursor")
	for _, sel := range [][2]string{{"grok-4.7", "xhigh"}, {"grok-4.7-low", "low"}, {"gpt-5.6-sol", "none"}} {
		if inv, err := cursor.Invocation(adapter.TaskInput{TaskID: tid, Model: sel[0], Effort: sel[1], Prompt: []byte("p")}); err == nil ||
			err.Error() != adapter.ValidateWorkerPosture("cursor", "linux").Error() || inv.Argv != nil {
			t.Fatalf("cursor %v: %+v %v", sel, inv, err)
		}
	}
	delegateAdapter(t, "^TestWorkerSelectionInvocation$", "TestWorkerSelectionInvocation")
}

// probeBanner probes vendor's production adapter against a stand-in that
// prints banner exactly (a real process; the stand-in is not a vendor).
func probeBanner(t *testing.T, vendor, banner string) error {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "banner"), []byte(banner), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, vendor+" stand-in")
	if err := writeExecutable(exe, []byte("#!/bin/sh\nexec /bin/cat '"+filepath.Join(dir, "banner")+"'\n")); err != nil {
		t.Fatal(err)
	}
	a, _ := adapter.Builtin(t.TempDir()).Lookup(vendor)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return a.Probe(ctx, exe)
}

// wantBanner requires probeBanner's outcome: success for "", otherwise the
// probe error with exactly reason, never echoing the banner.
func wantBanner(t *testing.T, vendor, banner, reason string) {
	t.Helper()
	err := probeBanner(t, vendor, banner)
	var pe *adapter.ProbeError
	switch {
	case reason == "" && err != nil:
		t.Fatalf("%s %q refused: %v", vendor, banner, err)
	case reason != "" && (!errors.As(err, &pe) || pe.Reason != reason || strings.Contains(err.Error(), "update available")):
		t.Fatalf("%s %q: %v, want %q", vendor, banner, err, reason)
	}
}

// minimumVersionPolicy is TestRealAdapterProbe/refusal/
// minimum-version-policy (FP-3): the production probes of stand-ins with
// the installed and older, malformed and foreign banners, the selection
// deployment's ready worker on the newer banners without a version-drift
// warning, and the tagged parser matrix.
func minimumVersionPolicy(t *testing.T) {
	t.Run("minimum-and-newer", func(t *testing.T) {
		delegateAdapter(t, "^TestWorkerVersionPolicy$/^(minimum-and-newer|ordering|baseline)$", "TestWorkerVersionPolicy",
			"TestWorkerVersionPolicy/minimum-and-newer", "TestWorkerVersionPolicy/ordering", "TestWorkerVersionPolicy/baseline")
		for vendor, banners := range map[string][]string{
			"claude": {"2.1.285 (Claude Code)\n", "2.1.292 (Claude Code)\n", "2.1.292 (Claude Code)\r\n", "3.0.0 (Claude Code)"},
			"codex":  {"codex-cli 0.159.0\n", "codex-cli 0.160.0\n", "codex-cli 0.160.0-rc.1\n", "codex-cli 0.159.10\n"},
		} {
			for _, b := range banners {
				wantBanner(t, vendor, b, "")
			}
		}
		// Worker S runs the newer banners: its role is ready, and no
		// warning about versions is logged (the macOS platform warning is the
		// existing one, logged once on darwin only).
		s := sharedSelectionRig(t)
		s.await2(t, "sel-codex")
		for _, line := range strings.Split(s.a.Logs.String(), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) != nil || rec["level"] != "WARN" {
				continue
			}
			msg, _ := rec["msg"].(string)
			if msg == sidecar.DarwinVendorWarning && runtime.GOOS != "darwin" || strings.Contains(strings.ToLower(line), "version") && msg != sidecar.DarwinVendorWarning {
				t.Fatalf("worker S warned: %s", line)
			}
		}
		if n := logRecords(s.a.Logs.String(), sidecar.DarwinVendorWarning); n != map[bool]int{true: 1, false: 0}[runtime.GOOS == "darwin"] {
			t.Fatalf("worker S logged the macOS warning %d times", n)
		}
	})
	t.Run("older", func(t *testing.T) {
		delegateAdapter(t, "^TestWorkerVersionPolicy$/^older$", "TestWorkerVersionPolicy", "TestWorkerVersionPolicy/older")
		wantBanner(t, "claude", "2.1.284 (Claude Code)\n", "has an older claude version; minimum 2.1.285 (Claude Code)")
		wantBanner(t, "claude", "2.1.285-rc.1 (Claude Code)\n", "has an older claude version; minimum 2.1.285 (Claude Code)")
		wantBanner(t, "codex", "codex-cli 0.158.9\n", "has an older codex version; minimum codex-cli 0.159.0")
		wantBanner(t, "codex", "codex-cli 0.159.0-rc.1\n", "has an older codex version; minimum codex-cli 0.159.0")
	})
	t.Run("malformed", func(t *testing.T) {
		delegateAdapter(t, "^TestWorkerVersionPolicy$/^malformed$", "TestWorkerVersionPolicy", "TestWorkerVersionPolicy/malformed")
		wantBanner(t, "claude", "2.1.292 (Claude Code)\nupdate available\n", "has an invalid claude version; expected an orderable version at or above 2.1.285 (Claude Code)")
		wantBanner(t, "claude", "v2.1.292 (Claude Code)\n", "has an invalid claude version; expected an orderable version at or above 2.1.285 (Claude Code)")
		wantBanner(t, "codex", "codex-cli 0.160\n", "has an invalid codex version; expected an orderable version at or above codex-cli 0.159.0")
		wantBanner(t, "codex", "codex-cli 0.160.0\n\n", "has an invalid codex version; expected an orderable version at or above codex-cli 0.159.0")
	})
	t.Run("wrong-vendor", func(t *testing.T) {
		delegateAdapter(t, "^TestWorkerVersionPolicy$/^wrong-vendor$", "TestWorkerVersionPolicy", "TestWorkerVersionPolicy/wrong-vendor")
		wantBanner(t, "claude", "codex-cli 0.160.0\n", "is not the claude CLI (unexpected version output)")
		wantBanner(t, "claude", "update available\n2.1.292 (Claude Code)\n", "is not the claude CLI (unexpected version output)")
		wantBanner(t, "codex", "2.1.292 (Claude Code)\n", "is not the codex CLI (unexpected version output)")
		wantBanner(t, "codex", "", "is not the codex CLI (unexpected version output)")
	})
}

// overridePolicy is TestRealAdapterDispatch/codex/override-policy (FP-5)
// on the selection deployment's sel-codex role through a real MCP server.
func overridePolicy(t *testing.T) {
	s := sharedSelectionRig(t)
	m := startMCP(t, "--plane", s.dep.URL, "--ca", s.dep.CA)
	s.serial.Lock()
	defer s.serial.Unlock()
	s.await2(t, "sel-codex")
	// run dispatches goal with override through MCP and returns the
	// terminal view and launch.
	run := func(t *testing.T, goal string, override map[string]any, sc scenario) (contract.TaskView, launch) {
		t.Helper()
		writeScenario(t, s.kit, sc)
		extra := map[string]any{}
		if override != nil {
			extra["override"] = override
		}
		id, e := mcpDispatch(t, m, dispatchArgs("id", "sel-codex", goal, extra))
		if e != nil {
			t.Fatalf("dispatch %q: %v", goal, e)
		}
		return mcpTerminal(t, m, id), taskLaunch(t, s.kit, id)
	}
	t.Run("admission-union", func(t *testing.T) {
		before := taskCount(t, s.dep.Client)
		for _, c := range []struct {
			override map[string]any
			want     string
		}{
			{map[string]any{"effort": "minimal"}, "effort is not allowed for adapter codex; allowed: low, medium, high, xhigh, max, ultra"},
			{map[string]any{"effort": "High"}, "effort is not allowed for adapter codex"},
			{map[string]any{"model": "gpt-6-astra", "effort": "none"}, "effort is not allowed for adapter codex"},
			{map[string]any{"model": ""}, "model"},
			{map[string]any{"model": " "}, "model must be nonblank text"},
		} {
			goal := s.goal("refused override")
			_, e := mcpDispatch(t, m, dispatchArgs("id", "sel-codex", goal, map[string]any{"override": c.override}))
			if e == nil || e.Code != contract.CodeInvalidArgument || !strings.Contains(e.Message, c.want) {
				t.Fatalf("override %v: %+v", c.override, e)
			}
			if ls := launches(t, s.kit, func(l launch) bool { return strings.Contains(string(l.Stdin), goal) }); len(ls) != 0 {
				t.Fatalf("a refused override launched %+v", ls)
			}
		}
		if after := taskCount(t, s.dep.Client); after != before {
			t.Fatalf("refused overrides created %d tasks", after-before)
		}
		// Any union effort is admitted for the role's model.
		goal := s.goal("union effort")
		v, l := run(t, goal, map[string]any{"effort": "max"}, selectionScenario(t, "codex", goal, "runs-scratch/codex-skip-success", codexRecipe("gpt-6.1-sol", "max")))
		if v.State != contract.TaskSucceeded || v.Effective.Model != "gpt-6.1-sol" || v.Effective.Effort != "max" || !slices.Equal(l.Argv, withFinal(codexRecipe("gpt-6.1-sol", "max"), l)) {
			t.Fatalf("union effort %+v argv %q", v.Effective, l.Argv)
		}
	})
	t.Run("one-task-only", func(t *testing.T) {
		goal := s.goal("one task override")
		v, l := run(t, goal, map[string]any{"model": "gpt-6-astra", "effort": "xhigh"},
			selectionScenario(t, "codex", goal, "runs-scratch/codex-skip-success", codexRecipe("gpt-6-astra", "xhigh")))
		if v.State != contract.TaskSucceeded || finalOf(finished{view: v}) != "pong" || v.Effective.Model != "gpt-6-astra" || v.Effective.Effort != "xhigh" ||
			v.Effective.Timeout != contract.DefaultRoleTimeout || !slices.Equal(l.Argv, withFinal(codexRecipe("gpt-6-astra", "xhigh"), l)) {
			t.Fatalf("override task %+v argv %q", v.Effective, l.Argv)
		}
		// The next dispatch without an override runs the role's own values;
		// the stored role never changed.
		goal = s.goal("no override")
		v, l = run(t, goal, nil, selectionScenario(t, "codex", goal, "runs-scratch/codex-skip-success", codexRecipe("gpt-6.1-sol", "low")))
		if v.State != contract.TaskSucceeded || v.Effective.Model != "gpt-6.1-sol" || v.Effective.Effort != "low" || !slices.Equal(l.Argv, withFinal(codexRecipe("gpt-6.1-sol", "low"), l)) {
			t.Fatalf("role task %+v argv %q", v.Effective, l.Argv)
		}
		if role := roleOf(t, m.ok("role_show", map[string]any{"id": "sel-codex"})); role.Model != "gpt-6.1-sol" || role.Effort != "low" {
			t.Fatalf("an override changed the role %+v", role)
		}
	})
	t.Run("vendor-rejection", func(t *testing.T) {
		// A syntactically valid model the vendor rejects reaches the vendor
		// (the recorded rejection of this model, replayed) and keeps the
		// existing failed-task result: exit 1, no final file, the diagnostic.
		goal := s.goal("vendor rejection")
		sc := selectionScenario(t, "codex", goal, "runs-scratch/codex-skip-fail", codexRecipe("callsheet-no-such-model", "low"))
		v, l := run(t, goal, map[string]any{"model": "callsheet-no-such-model"}, sc)
		logs, err := s.dep.Client.TaskLogs(context.Background(), v.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		want := append(captureFile(t, "runs-scratch/codex-skip-fail/stdout.bin"), diagnosticLine("codex", adapter.FinalMissing)...)
		if v.State != contract.TaskFailed || v.Result == nil || v.Result.ExitCode == nil || *v.Result.ExitCode != 1 || v.Result.FinalMessage != nil ||
			v.Effective.Model != "callsheet-no-such-model" || string(logs.Data) != string(want) || !slices.Equal(l.Argv, withFinal(sc.ExpectedArgv, l)) {
			t.Fatalf("vendor rejection %+v logs %q", v, logs.Data)
		}
	})
}

// selectionSurfaces is TestRealAdapterDispatch/claude/selection-surfaces
// (FP-6): the real CLI and MCP add, change, show and list an alternate
// selection exactly (a model with spaces, quotes and non-ASCII text), a
// task shows its effective selection, and help and schemas describe the
// policy.
func selectionSurfaces(t *testing.T) {
	s := sharedSelectionRig(t)
	m := startMCP(t, "--plane", s.dep.URL, "--ca", s.dep.CA)
	cli := newNodeCLI(t)
	trust := []string{"--plane", s.dep.URL, "--ca", s.dep.CA}
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	// Help and schemas.
	addHelp := cli.run(t, "role", "add", "--help")
	runHelp := cli.run(t, "sidecar", "run", "--help")
	for _, w := range []string{"required model name, free text passed unchanged to the adapter", "no model list is kept", "a vendor refusal is the task's result",
		"claude low, medium, high, xhigh, max; codex low, medium, high, xhigh, max, ultra; grok low, medium, high, xhigh; cursor none, minimal, low, medium, high, xhigh, max",
		"grok (Grok Build; Linux workers only)", "its roles are refused on every OS"} {
		if !strings.Contains(flat(addHelp.stdout), w) {
			t.Fatalf("role add help lacks %q", w)
		}
	}
	for _, w := range []string{"(minimum version 2.1.285 (Claude Code))", "(minimum version codex-cli 0.159.0)", "no PATH lookup", "Linux only",
		"at or above its minimum, a newer one without a warning", "are refused on every OS"} {
		if !strings.Contains(flat(runHelp.stdout), w) {
			t.Fatalf("sidecar run help lacks %q", w)
		}
	}
	var list struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if r := m.request("tools/list", nil); r.Error != nil || json.Unmarshal(r.Result, &list) != nil {
		t.Fatalf("tools/list %s", r.raw)
	}
	for _, tl := range list.Tools {
		if tl.Name != "role_add" && tl.Name != "role_set" && tl.Name != "dispatch" {
			continue
		}
		var sch struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		json.Unmarshal(tl.InputSchema, &sch)
		props := sch.Properties
		if tl.Name == "dispatch" {
			var o struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			json.Unmarshal(props["override"], &o)
			props = o.Properties
		}
		var model, effort struct {
			Type        string `json:"type"`
			Enum        []any  `json:"enum"`
			Description string `json:"description"`
		}
		if json.Unmarshal(props["model"], &model) != nil || json.Unmarshal(props["effort"], &effort) != nil || model.Type != "string" || model.Enum != nil ||
			effort.Type != "string" || effort.Enum != nil || !strings.Contains(model.Description, "free text passed unchanged to the adapter") ||
			!strings.Contains(effort.Description, "codex low, medium, high, xhigh, max, ultra") {
			t.Fatalf("%s schema %s", tl.Name, tl.InputSchema)
		}
	}
	s.serial.Lock()
	defer s.serial.Unlock()
	// The CLI adds a role with an alternate selection; text and JSON views
	// keep the exact model (a JSON string literal in text).
	model := `opus "1m" é`
	cid, xid := s.unique("surf-claude"), s.unique("surf-codex")
	defer s.removeRoles(t, cid, xid)
	add := cli.run(t, append([]string{"role", "add", cid, "--name", cid, "--node", s.a.NodeID, "--adapter", "claude", "--instruction", s.ins, "--runbook", s.run,
		"--model", model, "--effort", "max", "--concurrency", "2", "--json"}, trust...)...)
	if add.code != 0 {
		t.Fatalf("role add %+v", add)
	}
	if v := roleOf(t, add.stdout); v.Model != model || v.Effort != "max" {
		t.Fatalf("role add view %+v", v)
	}
	s.await2(t, cid)
	quoted, _ := json.Marshal(model)
	show := cli.run(t, append([]string{"role", "show", cid}, trust...)...)
	if !strings.Contains(show.stdout, "\nmodel: "+string(quoted)+"\neffort: max\n") {
		t.Fatalf("role show text %q", show.stdout)
	}
	ls := cli.run(t, append([]string{"role", "ls"}, trust...)...)
	if !strings.Contains(ls.stdout, "\tclaude\t"+string(quoted)+"\tmax\t") {
		t.Fatalf("role ls text %q", ls.stdout)
	}
	sameJSON(t, m.ok("role_show", map[string]any{"id": cid}), cli.run(t, append([]string{"role", "show", cid, "--json"}, trust...)...).stdout)
	sameJSON(t, m.ok("role_ls", nil), cli.run(t, append([]string{"role", "ls", "--json"}, trust...)...).stdout)
	// MCP changes it to another alternate selection and adds a codex role.
	if v := roleOf(t, m.ok("role_set", map[string]any{"id": cid, "model": "claude-fable-5-1", "effort": "low"})); v.Model != "claude-fable-5-1" || v.Effort != "low" {
		t.Fatalf("role_set %+v", v)
	}
	if v := roleOf(t, m.ok("role_add", map[string]any{"id": xid, "name": xid, "node": s.a.NodeID, "adapter": "codex", "instruction": s.ins, "runbook": s.run,
		"model": "gpt-6-astra", "effort": "ultra", "concurrency": 1})); v.Model != "gpt-6-astra" || v.Effort != "ultra" {
		t.Fatalf("role_add %+v", v)
	}
	s.await2(t, cid, xid)
	if v := roleOf(t, cli.run(t, append([]string{"role", "show", cid, "--json"}, trust...)...).stdout); v.Model != "claude-fable-5-1" || v.Effort != "low" {
		t.Fatalf("CLI view after role_set %+v", v)
	}
	// A task with an override shows its exact effective selection in MCP
	// and the CLI; the role keeps its own.
	goal := s.goal("surfaces override")
	writeScenario(t, s.kit, selectionScenario(t, "claude", goal, "runs-scratch/claude-stdin-success", claudeRecipe(model, "xhigh")))
	id, e := mcpDispatch(t, m, dispatchArgs("id", cid, goal, map[string]any{"override": map[string]any{"model": model, "effort": "xhigh"}}))
	if e != nil {
		t.Fatal(e)
	}
	v := mcpTerminal(t, m, id)
	if v.State != contract.TaskSucceeded || v.Effective.Model != model || v.Effective.Effort != "xhigh" {
		t.Fatalf("task %+v", v)
	}
	if l := taskLaunch(t, s.kit, id); !slices.Equal(l.Argv, claudeRecipe(model, "xhigh")) {
		t.Fatalf("launch %q", l.Argv)
	}
	tshow := cli.run(t, append([]string{"task", "show", id}, trust...)...)
	if !strings.Contains(tshow.stdout, "model: "+string(quoted)+"\n") || !strings.Contains(tshow.stdout, "effort: xhigh\n") {
		t.Fatalf("task show text %q", tshow.stdout)
	}
	tj, err := contract.ParseTaskShowResponse([]byte(cli.run(t, append([]string{"task", "show", id, "--json"}, trust...)...).stdout))
	if err != nil || tj.Effective != v.Effective {
		t.Fatalf("task show json %+v %v", tj.Effective, err)
	}
	if r := roleOf(t, m.ok("role_show", map[string]any{"id": cid})); r.Model != "claude-fable-5-1" || r.Effort != "low" {
		t.Fatalf("the override changed the role %+v", r)
	}
}

// selectionEvidence is TestRealAdapterCatalog/ownership/selection-evidence
// (FP-7): the catalog keeps the observed versions and pairs with the
// revised nonexclusive prose; the projection, publication baseline,
// receipts and enrolled fixtures are unchanged; the guides carry the
// policy.
func selectionEvidence(t *testing.T, root string, entries []catalog.Entry) {
	for _, e := range entries {
		q := qualification(e.ID)
		if e.Version != q.Version {
			t.Fatalf("%s catalog version %q, observation %q", e.ID, e.Version, q.Version)
		}
		model, effort := e.Facts["model"].Value, e.Facts["effort"].Value
		wantModel := map[bool]string{true: "The observed pair is not a selection allowlist.", false: "This is an observed model, not a model allowlist."}[e.ID == "cursor"]
		wantEffort := map[bool]string{true: "All Cursor task execution remains refused.", false: "This is an observed effort, not the adapter's complete allowed effort set."}[e.ID == "cursor"]
		observedArg := map[bool]string{true: "`grok-4.7-low`", false: q.Model + "`"}[e.ID == "cursor"]
		if !strings.Contains(model, observedArg) || !strings.Contains(model, wantModel) || !strings.Contains(effort, "low") || !strings.Contains(effort, wantEffort) {
			t.Fatalf("%s model/effort prose %q / %q", e.ID, model, effort)
		}
		for _, v := range []string{model, effort} {
			if strings.Contains(v, "Callsheet passes") || strings.Contains(v, "are refused.") || strings.Contains(v, "accepted selection") {
				t.Fatalf("%s keeps an exclusive claim: %q", e.ID, v)
			}
		}
	}
	delegate(t, contractBinary(t, "./internal/testkit/catalog"), "./internal/testkit/catalog", "^(TestWorkerCatalogProjection|TestWorkerCatalogSelectionProse)$",
		"TestWorkerCatalogProjection", "TestWorkerCatalogSelectionProse")
	if err := checkPublicationBaseline(entries); err != nil {
		t.Fatal(err)
	}
	if err := validatePublishedCatalog(t, root, entries); err != nil {
		t.Fatalf("the publisher-managed facts: %v", err)
	}
	idx, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{Root: root, Registry: mcpqual.DefaultRegistry()})
	if err != nil || len(idx.Entries) != 4 {
		t.Fatalf("enrolled fixtures: %v", err)
	}
	sc := string(repoFile(t, "docs/support-catalog.md"))
	ra := string(repoFile(t, "docs/real-adapters.md"))
	requireTerms(t, "docs/support-catalog.md", sc, "## Worker selection policy", "codex `low`, `medium`, `high`, `xhigh`, `max`, `ultra`",
		"a same-date version with another hash being equal and accepted", "the worker's minimum version stays 2.1.285", "the worker's minimum version stays 0.159.0")
	requireTerms(t, "docs/real-adapters.md", ra, "## Selection and versions", "## Captured recipes (historical observations)", "out-of-union effort",
		"Codex `gpt-6-astra` with `low`", "Claude `claude-fable-5-1` with `low`", "Claude `claude-opus-5-5` with `high`", "Grok `grok-4.7-build-fast` with `low`",
		"requested selections, not observed successful executions or defaults")
	for _, stale := range []string{"the worker still runs only", "unqualified-selection refusal", "selection_not_qualified"} {
		if strings.Contains(sc+ra, stale) {
			t.Fatalf("a guide keeps %q", stale)
		}
	}
}

// newerVersionSmoke is TestRealAdapterSmokeGate/enabled/newer-version
// (FP-8): the stub-backed smoke of each vendor through a fresh deployment
// with banners newer than the minimum and the observed smoke pair; it
// completes, and its task's scratch directory is gone.
func newerVersionSmoke(t *testing.T, k *replayKit, root, goal string, g workersmoke.Gate) {
	for vendor, v := range newerHostVersions {
		if b, err := os.ReadFile(filepath.Join(k.dir, vendor+".version")); err != nil || string(b) != v+"\n" || v == qualification(vendor).Version {
			t.Fatalf("%s banner %q %v", vendor, b, err)
		}
	}
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o700)
	base := []string{"PATH=" + k.traps, "HOME=" + home}
	bin := nodeBinary(t) // built here, never inside the goroutines
	var wg sync.WaitGroup
	for _, vendor := range []string{"claude", "codex"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir := filepath.Join(root, "smoke-"+vendor)
			os.MkdirAll(dir, 0o755)
			ctx, cancel := context.WithTimeout(context.Background(), workersmoke.OuterBound)
			defer cancel()
			res, err := workersmoke.Run(ctx, bin, dir, base, append(slices.Clone(base), replayDirEnv+"="+k.dir), vendor, g.Vendor(vendor).Path, goal+" ("+vendor+")")
			if err != nil || res.View.State != contract.TaskSucceeded || res.View.Result == nil || *res.View.Result.ExitCode != 0 ||
				res.View.Result.FinalMessage == nil || *res.View.Result.FinalMessage != "pong" {
				t.Errorf("%s smoke %+v %v", vendor, res.View, err)
				return
			}
			q := qualification(vendor)
			if res.View.Effective.Model != q.Model || res.View.Effective.Effort != q.Effort {
				t.Errorf("%s smoke selection %+v, want the observed smoke pair", vendor, res.View.Effective)
			}
		}()
	}
	wg.Wait()
	for _, l := range launches(t, k, func(launch) bool { return true }) {
		if !l.OK {
			t.Fatalf("smoke launch failed %+v", l)
		}
		if l.Mode == "task" {
			if _, err := os.Stat(l.Cwd); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("smoke scratch %s kept: %v", l.Cwd, err)
			}
		}
	}
	if n := len(launches(t, k, func(l launch) bool { return l.Mode == "version" })); n == 0 {
		t.Fatal("no version probe of the newer banners")
	}
}
