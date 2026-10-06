//go:build linux || darwin

package function

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/build/constraint"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/sidecar"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
	"github.com/wedevwork/callsheet/internal/testkit/workersmoke"
)

// Iteration 11 function tests (real adapters, wave 2: Grok and Cursor;
// FP-1..FP-9): exactly one top-level TestWave2* per FP, each subtest
// running and passing on Linux and macOS. They drive real Callsheet
// processes against the replay stub (testdata/worker-replay, now also
// impersonating grok and cursor-agent) enabled through the product's
// --grok-adapter and --cursor-adapter options, with an isolated HOME and a
// PATH of launch-recording grok, cursor-agent, agent, claude and codex
// traps: no installed vendor CLI, model or network is used. The wave-2
// deployment is its own plane (started once per test process, stopped by
// TestMain), so its role registrations never disturb iteration 08's rig.
// Grok's execution path runs on Linux; on macOS the native refusal is
// asserted and the identical Linux task logic runs in the tagged local
// contract (TestRealAdapterLocal/wave2-*) through roleEnv.goos, never by
// faking a real vendor run's OS.

const (
	// wave2CapturesRel is iteration 11's checked-in evidence root.
	wave2CapturesRel = "tests/testdata/real-adapters/linux-2026-10-04"
	// wave2ProbeEnv selects TestWave2Probe's direct-probe child half.
	wave2ProbeEnv = "CALLSHEET_WAVE2_DIRECT_PROBE"
	wave2ExeEnv   = "CALLSHEET_WAVE2_DIRECT_EXE"
	// The exact posture refusals and the Grok failure message.
	wave2GrokRefusal   = "grok worker execution is not qualified on this OS; consult the support catalog"
	wave2CursorRefusal = "cursor worker execution is refused: no qualified unattended recipe preserves the operator posture; consult the support catalog"
	wave2GrokFailure   = `Couldn't set model 'does-not-exist': Invalid params: "unknown model id". Run 'grok models' to see available models.`
)

// wave2Linux reports whether this host executes Grok (the production
// posture of the native OS).
func wave2Linux() bool { return adapter.ValidateWorkerPosture("grok", runtime.GOOS) == nil }

func wave2File(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), filepath.FromSlash(wave2CapturesRel), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wave2Versions are the captured versions (host.txt), keyed by vendor.
func wave2Versions(root string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(wave2CapturesRel), "host.txt"))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(b), "\n") {
		for _, id := range []string{"grok", "cursor"} {
			if v, ok := strings.CutPrefix(line, id+" "); ok {
				out[id] = v
			}
		}
	}
	return out, nil
}

// wave2Kit is a scenario directory with grok and cursor-agent links to the
// replay stub (in a directory whose name holds a space) and a PATH of
// launch-recording traps for grok, cursor-agent, agent, claude and codex.
type wave2Kit struct {
	*replayKit
	grok, cursor string
}

var wave2Traps = []string{"grok", "cursor-agent", "agent", "claude", "codex"}

func newWave2Kit(root, stub string, versions map[string]string) (*wave2Kit, error) {
	k := &wave2Kit{replayKit: &replayKit{dir: filepath.Join(root, "replay"), traps: filepath.Join(root, "traps"), trapLog: filepath.Join(root, "traps.log")}}
	bin := filepath.Join(root, "replay bin")
	for _, d := range []string{k.dir, filepath.Join(k.dir, "scenarios"), filepath.Join(k.dir, "launches"), bin, k.traps} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	k.grok, k.cursor = filepath.Join(bin, "grok"), filepath.Join(bin, "cursor-agent")
	for id, p := range map[string]string{"grok": k.grok, "cursor": k.cursor} {
		if err := os.Symlink(stub, p); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(k.dir, id+".version"), []byte(versions[id]+"\n"), 0o644); err != nil {
			return nil, err
		}
	}
	for _, name := range wave2Traps {
		trap := "#!/bin/sh\necho \"" + name + " $*\" >> '" + k.trapLog + "'\nexit 127\n"
		if err := writeExecutable(filepath.Join(k.traps, name), []byte(trap)); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// grokArgv is Grok's production argv with the composed prompt replaced by
// placeholder.
func grokArgv(placeholder string) []string {
	inv, _ := adapterFor("grok").Invocation(adapter.TaskInput{TaskID: "t_" + strings.Repeat("0", 32), Model: "grok-4.7", Effort: "low", Prompt: []byte("P")})
	out := slices.Clone(inv.Argv)
	out[len(out)-1] = placeholder
	return out
}

func strPtr(s string) *string { return &s }

// grokScenario replays capture run (runs/<run>) for goal under the valid
// qualified Grok invocation: the capture's own argv is evidence only.
func grokScenario(t testing.TB, goal, run string) scenario {
	src := strings.Split(strings.TrimSuffix(string(wave2File(t, "runs/"+run+"/argv.txt")), "\n"), "\n")
	return scenario{Goal: goal, Vendor: "grok", SourceArgv: src, ExpectedArgv: grokArgv("{prompt}"), ExpectedStdin: strPtr(""),
		Capture: filepath.Join(testkit.MustRepoRoot(t), filepath.FromSlash(wave2CapturesRel), "runs", run),
		Normalization: "the valid production argv of the qualified pair (grok-4.7, low) with the full composed task envelope as the -p value replays this " +
			"capture's output and exit bytes; the capture's own argv (its model, prompt text or missing -p value) is evidence only"}
}

// wave2Rig is the shared wave-2 deployment: a plane and worker G enabling
// grok and cursor with the replay stubs; on Linux, worker G's grok roles
// wg (ordinary manuals) and wbig (a composed prompt over 32 KiB).
type wave2Rig struct {
	*realRig
	kit *wave2Kit
}

var (
	wave2Once sync.Once
	theWave2  *wave2Rig
	wave2Err  error
)

func sharedWave2Rig(t *testing.T) *wave2Rig {
	t.Helper()
	bin, stub := nodeBinary(t), replayBinary(t)
	wave2Once.Do(func() { theWave2, wave2Err = startWave2Rig(bin, stub) })
	if wave2Err != nil {
		t.Fatal(wave2Err)
	}
	return theWave2
}

func startWave2Rig(bin, stub string) (*wave2Rig, error) {
	root, err := os.MkdirTemp("", "callsheet-wave2-")
	if err != nil {
		return nil, err
	}
	r := &wave2Rig{realRig: &realRig{root: root}}
	fixtureMu.Lock()
	afterSuite = append(afterSuite, r.close)
	fixtureMu.Unlock()
	versions, err := wave2Versions(repoRootOrEmpty())
	if err != nil {
		return nil, err
	}
	if r.kit, err = newWave2Kit(root, stub, versions); err != nil {
		return nil, err
	}
	r.realRig.kit = r.kit.replayKit
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o700)
	r.env = []string{"PATH=" + r.kit.traps, "HOME=" + home}
	ctx, cancel := context.WithTimeout(context.Background(), rigWait)
	defer cancel()
	if r.dep, err = workersmoke.Start(ctx, bin, root, r.env); err != nil {
		return nil, err
	}
	if r.a, err = r.dep.StartSidecar(ctx, "worker-g", r.sidecarEnv(r.kit.replayKit), "--grok-adapter", r.kit.grok, "--cursor-adapter", r.kit.cursor); err != nil {
		return nil, err
	}
	r.insTxt = "Wave-2 replay instruction: answer the goal. Quotes \" \\ and é 😀 stay bytes.\n"
	r.runTxt = "Wave-2 replay runbook.\n"
	if r.ins, r.run, err = r.dep.Manuals("wave2", r.insTxt, r.runTxt); err != nil {
		return nil, err
	}
	if !wave2Linux() {
		return r, nil
	}
	// Every successful role mutation happens here, before any dispatch
	// (iteration 08's known roles-revision behavior).
	bigIns, bigRun, err := r.dep.Manuals("wave2-big", strings.Repeat("i", 33<<10), "runbook\n")
	if err != nil {
		return nil, err
	}
	for _, rc := range []contract.RoleConfig{
		{ID: "wbig", Name: "wbig", Node: r.a.NodeID, Adapter: "grok", Instruction: bigIns, Runbook: bigRun, Model: "grok-4.7", Effort: "low", Concurrency: 2},
		{ID: "wg", Name: "wg", Node: r.a.NodeID, Adapter: "grok", Instruction: r.ins, Runbook: r.run, Model: "grok-4.7", Effort: "low", Concurrency: 8},
	} {
		if _, err := r.dep.Client.AddRole(ctx, rc); err != nil {
			return nil, err
		}
	}
	return r, r.ready(ctx, "wg", "wbig")
}

// grokPrompt is the exact composed prompt the worker passes for req on
// role wg (the task ID a placeholder).
func (r *wave2Rig) grokPrompt(t *testing.T, req contract.DispatchRequest) string {
	t.Helper()
	type role struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	type task struct {
		TaskID          string                 `json:"task_id"`
		Target          contract.TaskTarget    `json:"target"`
		Role            role                   `json:"role"`
		Goal            string                 `json:"goal"`
		Payload         []string               `json:"payload"`
		Acceptance      string                 `json:"acceptance"`
		Effective       contract.TaskEffective `json:"effective"`
		TimeoutEnforced bool                   `json:"timeout_enforced"`
		RequestedBy     contract.RequestedBy   `json:"requested_by"`
	}
	env := struct {
		Format      string `json:"format"`
		Instruction string `json:"instruction"`
		Runbook     string `json:"runbook"`
		Task        task   `json:"task"`
	}{"callsheet-task-v1", r.insTxt, r.runTxt, task{TaskID: "{task_id}", Target: req.Target, Role: role{"wg", "wg"}, Goal: req.Goal, Payload: req.Payload,
		Acceptance: req.Acceptance, Effective: contract.TaskEffective{Model: "grok-4.7", Effort: "low", Timeout: contract.DefaultRoleTimeout}, RequestedBy: req.RequestedBy}}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// refuse registers rc and requires the node's exact posture refusal.
func (r *wave2Rig) refuse(t *testing.T, rc contract.RoleConfig, posture string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rigWait)
	defer cancel()
	_, err := r.addRole(ctx, rc)
	var ce *contract.Error
	if !errors.As(err, &ce) || ce.Code != contract.CodeInvalidArgument || ce.Details["field"] != "adapter" || ce.Details["reason"] != contract.ReasonProbeFailed ||
		ce.Message != "node "+rc.Node+" rejected role "+rc.ID+": "+posture {
		t.Fatalf("%s role %s: %v", rc.Adapter, rc.ID, err)
	}
	if _, err := r.dep.Client.ShowRole(ctx, rc.ID); contract.CodeOf(err) != contract.CodeNotFound {
		t.Fatalf("refused role %s recorded: %v", rc.ID, err)
	}
}

// role is a wave-2 role config on worker G with the known pair.
func (r *wave2Rig) role(id, vendor string) contract.RoleConfig {
	return contract.RoleConfig{ID: id, Name: id, Node: r.a.NodeID, Adapter: vendor, Instruction: r.ins, Runbook: r.run, Model: "grok-4.7", Effort: "low", Concurrency: 1}
}

// noCursorTask requires that no kit ever recorded a Cursor task launch.
func noCursorTask(t *testing.T, kits ...*replayKit) {
	t.Helper()
	for _, k := range kits {
		if ls := launches(t, k, func(l launch) bool { return l.Vendor == "cursor" && l.Mode != "version" }); len(ls) != 0 {
			t.Fatalf("a cursor task was launched: %+v", ls)
		}
	}
}

// FP-1: the grok and cursor flags, metadata and exact pairs, and the
// posture refusals of Cursor everywhere and Grok on macOS.
func TestWave2Registration(t *testing.T) {
	t.Parallel()
	r := sharedWave2Rig(t)
	ctx := context.Background()
	t.Run("registry", func(t *testing.T) {
		delegate(t, contractBinary(t, "./internal/adapter"), "./internal/adapter", "^TestWave2RegistrySelection$", "TestWave2RegistrySelection")
		for _, id := range []string{"grok", "cursor"} {
			info, ok := adapter.Lookup()(id)
			if !ok || info.TestOnly || !slices.Equal(info.Efforts, []string{"low"}) {
				t.Fatalf("%s metadata %+v %v", id, info, ok)
			}
			// The plane refuses efforts outside the descriptor before the node.
			bad := r.role("reg-"+id+"-effort", id)
			bad.Effort = "medium"
			if _, err := r.addRole(ctx, bad); contract.CodeOf(err) != contract.CodeInvalidArgument || !strings.Contains(err.Error(), "allowed: low") {
				t.Fatalf("%s medium: %v", id, err)
			}
		}
		if wave2Linux() {
			v, err := r.dep.Client.ShowRole(ctx, "wg")
			if err != nil || v.Adapter != "grok" || v.AdapterTestOnly || v.Model != "grok-4.7" || v.Effort != "low" || !v.CanAccept {
				t.Fatalf("role wg = %+v %v", v, err)
			}
		}
		// Worker G's startup warnings: Grok's (with the macOS suffix on
		// darwin) and Cursor's once each; never the Claude/Codex one.
		grokWarning := sidecar.GrokVendorWarning
		if runtime.GOOS == "darwin" {
			grokWarning += " " + sidecar.GrokDarwinWarningSuffix
		}
		if logRecords(r.a.Logs.String(), grokWarning) != 1 || logRecords(r.a.Logs.String(), sidecar.CursorVendorWarning) != 1 ||
			logRecords(r.a.Logs.String(), sidecar.DarwinVendorWarning) != 0 {
			t.Fatalf("worker G warnings:\n%s", r.a.Logs.String())
		}
	})
	t.Run("paths", func(t *testing.T) {
		delegate(t, contractBinary(t, "./internal/cli"), "./internal/cli", "^TestRealAdapterCLIOptions$", "TestRealAdapterCLIOptions")
		if !strings.Contains(r.kit.grok, " ") || !strings.Contains(r.kit.cursor, " ") {
			t.Fatal("the rig's paths hold no space")
		}
		p := newNodeCLI(t)
		state := t.TempDir()
		for _, args := range [][]string{{"--grok-adapter", "grok"}, {"--cursor-adapter", ""}, {"--grok-adapter", "/a", "--grok-adapter", "/b"},
			{"--cursor-adapter", "cursor-agent"}, {"--cursor-adapter", "agent"}, {"--grok-adapter", "./bin/grok"}} {
			res := runBin(t, p.bin, p.cwd, p.env, append([]string{"sidecar", "run", "--state-dir", state}, args...)...)
			if res.code != 2 || !strings.Contains(res.stderr, "Usage:") || !strings.Contains(res.stderr, "adapter") {
				t.Fatalf("sidecar run %v = %+v", args, res)
			}
		}
		if entries, _ := os.ReadDir(state); len(entries) != 0 {
			t.Fatalf("a usage error touched the state: %v", entries)
		}
		// An omitted flag disables that adapter whatever PATH holds.
		b, err := r.dep.StartSidecar(ctx, r.unique("worker-grok-only"), r.sidecarEnv(r.kit.replayKit), "--grok-adapter", r.kit.grok)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Stop() })
		rc := r.role("paths-cursor", "cursor")
		rc.Node = b.NodeID
		if _, err := r.addRole(ctx, rc); contract.CodeOf(err) != contract.CodeInvalidArgument ||
			!strings.Contains(err.Error(), "cursor adapter is disabled on node; start sidecar with --cursor-adapter ABSOLUTE_PATH") {
			t.Fatalf("omitted cursor: %v", err)
		}
		if logRecords(b.Logs.String(), sidecar.CursorVendorWarning) != 0 {
			t.Fatal("a sidecar without --cursor-adapter warned about cursor")
		}
	})
	t.Run("selection", func(t *testing.T) {
		// Exact pairs only, refused field by field without echoing the
		// value, before any probe (the selection check comes first).
		for _, c := range []struct{ vendor, model, field string }{{"grok", "grok-4.7-build", "model"}, {"grok", "grok-4.7-low", "model"},
			{"cursor", "grok-4.7-low", "model"}, {"cursor", "auto", "model"}} {
			rc := r.role("sel-"+c.vendor, c.vendor)
			rc.Model = c.model
			_, err := r.addRole(ctx, rc)
			var ce *contract.Error
			if !errors.As(err, &ce) || ce.Code != contract.CodeInvalidArgument || ce.Details["field"] != c.field || ce.Details["reason"] != contract.ReasonProbeFailed ||
				!strings.Contains(ce.Message, "the "+c.vendor+" model/effort selection is not qualified; supported: model grok-4.7, effort low") ||
				strings.Contains(ce.Message, c.model+"\"") {
				t.Fatalf("%s %s: %v", c.vendor, c.model, err)
			}
		}
		if !wave2Linux() {
			return
		}
		// An unqualified per-task override is refused start_failed before
		// any launch.
		goal := r.goal("grok override")
		writeScenario(t, r.kit.replayKit, grokScenario(t, goal, "grok-stdin-success"))
		req := requestFor("wg", goal)
		m := "grok-4.7-build"
		req.Override = &contract.TaskOverride{Model: &m}
		f := r.dispatch(t, req)
		if f.view.State == contract.TaskSucceeded || f.view.Reason == nil || f.view.Reason.Code != contract.ReasonStartFailed {
			t.Fatalf("override %s", f)
		}
		if ls := launches(t, r.kit.replayKit, func(l launch) bool { return l.TaskID == f.view.TaskID }); len(ls) != 0 {
			t.Fatalf("an unqualified override launched %+v", ls)
		}
	})
	t.Run("posture", func(t *testing.T) {
		r.refuse(t, r.role("posture-cursor", "cursor"), wave2CursorRefusal)
		if !wave2Linux() {
			r.refuse(t, r.role("posture-grok", "grok"), wave2GrokRefusal)
		}
		delegate(t, contractBinary(t, "./internal/adapter"), "./internal/adapter", "^TestWave2Posture$", "TestWave2Posture")
		delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^wave2-posture$", "TestRealAdapterLocal", "TestRealAdapterLocal/wave2-posture")
		noCursorTask(t, r.kit.replayKit)
	})
}

// wave2DirectProbe is TestWave2Probe's child half: the production
// Adapter.Probe of the replay stub at the explicit path, in a fresh state
// root (the stub finds its scenario directory in this process's
// environment).
func wave2DirectProbe(t *testing.T) {
	vendor, exe := os.Getenv(wave2ProbeEnv), os.Getenv(wave2ExeEnv)
	a, ok := adapter.Builtin(t.TempDir()).Lookup(vendor)
	if !ok {
		t.Fatalf("no adapter %s", vendor)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Probe(ctx, exe); err != nil {
		t.Fatalf("direct %s probe: %v", vendor, err)
	}
	fmt.Println("wave2 direct probe passed: " + vendor)
}

// directProbe runs the child half for vendor's stub at exe.
func directProbe(t *testing.T, k *replayKit, vendor, exe string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rigWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWave2Probe$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), wave2ProbeEnv+"="+vendor, wave2ExeEnv+"="+exe, replayDirEnv+"="+k.dir,
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "wave2 direct probe passed: "+vendor) {
		t.Fatalf("direct %s probe child: %v\n%s", vendor, err, out)
	}
}

// FP-2: bounded --version probes of the explicit replay executables; a
// matching version never enables Cursor; wrong versions and paths fail.
func TestWave2Probe(t *testing.T) {
	if os.Getenv(wave2ProbeEnv) != "" {
		wave2DirectProbe(t)
		return
	}
	t.Parallel()
	r := sharedWave2Rig(t)
	versions, err := wave2Versions(testkit.MustRepoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, vendor, exe string) {
		t.Helper()
		if versions[vendor] != qualification(vendor).Version {
			t.Fatalf("captured %s version %q is not the known %q", vendor, versions[vendor], qualification(vendor).Version)
		}
		state := r.a.State
		real, _ := filepath.EvalSymlinks(state)
		if ls := launches(t, r.kit.replayKit, func(l launch) bool { return l.Argv0 == exe && (l.Cwd == state || l.Cwd == real) && l.Mode == "version" }); len(ls) == 0 {
			t.Fatalf("no %s version probe in worker G's state root", vendor)
		}
		// Every probe of the path is exactly a version call (Grok's task
		// launches are the other parents' replays; Cursor has none).
		for _, l := range launches(t, r.kit.replayKit, func(l launch) bool { return l.Argv0 == exe && (l.Mode == "version" || vendor == "cursor") }) {
			if l.Mode != "version" || !l.OK || !slices.Equal(l.Argv, []string{"--version"}) || l.Vendor != vendor || l.TaskID != "" {
				t.Fatalf("%s launch %+v", vendor, l)
			}
		}
		directProbe(t, r.kit.replayKit, vendor, exe)
	}
	t.Run("grok", func(t *testing.T) {
		if !wave2Linux() {
			// The refused registration still probes first (probe precedence).
			r.refuse(t, r.role("probe-grok", "grok"), wave2GrokRefusal)
		}
		check(t, "grok", r.kit.grok)
	})
	t.Run("cursor", func(t *testing.T) {
		// A matching version alone never enables Cursor: the probe runs,
		// then the posture refuses.
		r.refuse(t, r.role("probe-cursor", "cursor"), wave2CursorRefusal)
		check(t, "cursor", r.kit.cursor)
		noCursorTask(t, r.kit.replayKit)
	})
	t.Run("refusal", func(t *testing.T) {
		delegate(t, contractBinary(t, "./internal/adapter"), "./internal/adapter", "^TestWave2VendorProbe$", "TestWave2VendorProbe")
		// A second kit: another grok version, cursor without its shape.
		k, err := newWave2Kit(t.TempDir(), replayBinary(t), map[string]string{"grok": "grok 1.0.45 (1111111aaaaa) [stable]", "cursor": "cursor-agent " + versions["cursor"]})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		c, err := r.dep.StartSidecar(ctx, r.unique("worker-wrong"), r.sidecarEnv(k.replayKit), "--grok-adapter", k.grok, "--cursor-adapter", k.cursor)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Stop() })
		for vendor, want := range map[string]string{
			"grok":   "has an unqualified grok version; expected grok 1.0.46 (2765805b9442) [stable]",
			"cursor": "is not the cursor CLI (unexpected version output)",
		} {
			rc := r.role("wrong-"+vendor, vendor)
			rc.Node = c.NodeID
			_, err := r.addRole(ctx, rc)
			var ce *contract.Error
			// The probe error wins over (and is never mistaken for) the posture.
			if !errors.As(err, &ce) || ce.Details["reason"] != contract.ReasonProbeFailed || !strings.Contains(ce.Message, want) || strings.Contains(ce.Message, "consult") {
				t.Fatalf("%s: %v", vendor, err)
			}
		}
		if ls := launches(t, k.replayKit, func(l launch) bool { return l.Mode != "version" || !l.OK }); len(ls) != 0 {
			t.Fatalf("refused executables were called for tasks: %+v", ls)
		}
		if len(launches(t, k.replayKit, func(launch) bool { return true })) == 0 {
			t.Fatal("the refused executables were never probed")
		}
		p := newNodeCLI(t)
		for _, args := range [][]string{{"--grok-adapter", "grok"}, {"--cursor-adapter", "cursor-agent"}} {
			res := runBin(t, p.bin, p.cwd, append(slices.Clone(r.env), replayDirEnv+"="+k.dir), append([]string{"sidecar", "run", "--state-dir", t.TempDir()}, args...)...)
			if res.code != 2 || !strings.Contains(res.stderr, args[0]+" must be an absolute path") {
				t.Fatalf("relative %v = %+v", args, res)
			}
		}
		if b, err := os.ReadFile(k.trapLog); err == nil && len(b) > 0 {
			t.Fatalf("PATH traps were called: %s", b)
		}
	})
}

// FP-3: Grok's exact recorded argv with the composed prompt as its -p
// value and empty stdin, the size/encoding bounds, and zero Cursor task
// launches.
func TestWave2Invocation(t *testing.T) {
	t.Parallel()
	r := sharedWave2Rig(t)
	forbidden := []string{"--sandbox", "--always-approve", "--force", "--trust", "--yolo", "--auto-review", "--resume", "--continue", "--worktree",
		"--prompt-file", "-", "--dangerously-skip-permissions", "--approve-mcps"}
	check := func(t *testing.T, goal string) {
		t.Helper()
		req := requestFor("wg", goal)
		prompt := r.grokPrompt(t, req)
		sc := grokScenario(t, goal, "grok-stdin-success")
		sc.ExpectedPrompt = &prompt
		writeScenario(t, r.kit.replayKit, sc)
		f := r.dispatch(t, req)
		if f.view.State != contract.TaskSucceeded || finalOf(f) != "pong" {
			t.Fatalf("grok task %s", f)
		}
		l := taskLaunch(t, r.kit.replayKit, f.view.TaskID)
		want := grokArgv(strings.ReplaceAll(prompt, "{task_id}", f.view.TaskID))
		if !slices.Equal(l.Argv, want) || l.Argv0 != r.kit.grok || len(l.Stdin) != 0 {
			t.Fatalf("grok argv %q (argv0 %s, stdin %q), want %q", l.Argv, l.Argv0, l.Stdin, want)
		}
		for _, a := range l.Argv[:len(l.Argv)-1] {
			if slices.Contains(forbidden, a) || strings.Contains(a, goal) {
				t.Fatalf("grok argv carries %q", a)
			}
		}
		if l.CwdMode != "0700" || filepath.Base(l.Cwd) != "work" || filepath.Base(filepath.Dir(l.Cwd)) != f.view.TaskID {
			t.Fatalf("grok cwd %s %s", l.Cwd, l.CwdMode)
		}
		if strings.Contains(r.a.Logs.String(), goal) {
			t.Fatal("the prompt reached the sidecar log")
		}
	}
	t.Run("grok", func(t *testing.T) {
		delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^wave2-invocation$", "TestRealAdapterLocal", "TestRealAdapterLocal/wave2-invocation")
		if !wave2Linux() {
			r.refuse(t, r.role("inv-grok", "grok"), wave2GrokRefusal)
			return
		}
		check(t, r.goal("invocation grok"))
	})
	t.Run("cursor-refused", func(t *testing.T) {
		delegate(t, contractBinary(t, "./internal/adapter"), "./internal/adapter", "^TestWave2VendorInvocation$", "TestWave2VendorInvocation")
		r.refuse(t, r.role("inv-cursor", "cursor"), wave2CursorRefusal)
		if inv, err := adapterFor("cursor").Invocation(adapter.TaskInput{TaskID: "t_" + strings.Repeat("0", 32), Model: "grok-4.7", Effort: "low", Prompt: []byte("p")}); err == nil || inv.Argv != nil {
			t.Fatalf("cursor invocation %+v %v", inv, err)
		}
		noCursorTask(t, r.kit.replayKit)
	})
	t.Run("prompt", func(t *testing.T) {
		if !wave2Linux() {
			delegate(t, contractBinary(t, "./internal/adapter"), "./internal/adapter", "^TestWave2VendorInvocation$", "TestWave2VendorInvocation")
			return
		}
		// Arbitrary goal bytes reach the -p value exactly (no shell, no
		// trimming), valid UTF-8 without NUL.
		check(t, r.goal("prompt: \"quoted\" 'single' $(echo no) `tick` \\ back\nline two\ttab é 😀 -- --model other  "))
		// A composed prompt over 32 KiB is a definite start refusal before
		// any launch.
		goal := r.goal("prompt over 32 KiB")
		writeScenario(t, r.kit.replayKit, grokScenario(t, goal, "grok-stdin-success"))
		f := r.dispatch(t, requestFor("wbig", goal))
		if f.view.State == contract.TaskSucceeded || f.view.Reason == nil || f.view.Reason.Code != contract.ReasonStartFailed {
			t.Fatalf("over-32-KiB prompt %s", f)
		}
		if ls := launches(t, r.kit.replayKit, func(l launch) bool { return l.TaskID == f.view.TaskID }); len(ls) != 0 {
			t.Fatalf("an oversized prompt launched %+v", ls)
		}
		for _, l := range launches(t, r.kit.replayKit, func(l launch) bool { return l.Vendor == "grok" && l.Mode == "task" }) {
			if !utf8.Valid(l.Prompt) || bytes.IndexByte(l.Prompt, 0) >= 0 || len(l.Prompt) > adapter.MaxGrokPromptBytes || len(l.Stdin) != 0 {
				t.Fatalf("grok launch %s prompt %d bytes", l.TaskID, len(l.Prompt))
			}
		}
	})
}

// grokReplay dispatches run's capture to wg (on Linux).
func (r *wave2Rig) grokReplay(t *testing.T, prefix, run string) finished {
	t.Helper()
	return r.replay(t, "wg", grokScenario(t, r.goal(prefix), run))
}

// grokOffline feeds out to Grok's public extractor in 7-byte chunks.
func grokOffline(vendor string, out []byte) adapter.FinalMessage {
	e := adapterFor(vendor).NewFinalExtractor()
	for i := 0; i < len(out); i += 7 {
		e.Feed(out[i:min(i+7, len(out))])
	}
	return e.Finish()
}

func msgOrNil(m adapter.FinalMessage) string {
	if m.Message == nil {
		return "<null>"
	}
	return *m.Message
}

// FP-4: Grok's captured text and error, cancelled output as an ordinary
// answer with the process exit kept, and malformed output's diagnostic.
func TestWave2GrokFinal(t *testing.T) {
	t.Parallel()
	r := sharedWave2Rig(t)
	var cancelled struct{ Text, StopReason string }
	json.Unmarshal(wave2File(t, "runs/grok-shell-home/stdout.bin"), &cancelled)
	offline := func(t *testing.T, run, want string) {
		t.Helper()
		if got := grokOffline("grok", wave2File(t, "runs/"+run+"/stdout.bin")); msgOrNil(got) != want {
			t.Fatalf("%s offline %+v", run, got)
		}
	}
	t.Run("success", func(t *testing.T) {
		offline(t, "grok-stdin-success", "pong")
		if !wave2Linux() {
			delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^wave2-outcomes$", "TestRealAdapterLocal", "TestRealAdapterLocal/wave2-outcomes")
			return
		}
		f := r.grokReplay(t, "grok success", "grok-stdin-success")
		if f.view.State != contract.TaskSucceeded || exitOf(f) != 0 || finalOf(f) != "pong" || !bytes.Equal(f.logs, wave2File(t, "runs/grok-stdin-success/stdout.bin")) {
			t.Fatalf("success %s", f)
		}
	})
	t.Run("error", func(t *testing.T) {
		offline(t, "grok-stdin-fail", wave2GrokFailure)
		if !wave2Linux() {
			return
		}
		f := r.grokReplay(t, "grok error", "grok-stdin-fail")
		if f.view.State != contract.TaskFailed || exitOf(f) != 1 || finalOf(f) != wave2GrokFailure ||
			!interleaves(f.logs, wave2File(t, "runs/grok-stdin-fail/stdout.bin"), wave2File(t, "runs/grok-stdin-fail/stderr.bin")) {
			t.Fatalf("error %s", f)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		if cancelled.StopReason != "cancelled" || cancelled.Text == "" {
			t.Fatalf("capture %+v", cancelled)
		}
		offline(t, "grok-shell-home", cancelled.Text)
		if !wave2Linux() {
			return
		}
		// The vendor's cancelled tool call is not a Callsheet cancellation:
		// exit 0, succeeded, the exact text, no stop.
		f := r.grokReplay(t, "grok cancelled", "grok-shell-home")
		if f.view.State != contract.TaskSucceeded || exitOf(f) != 0 || finalOf(f) != cancelled.Text || f.view.Reason != nil || f.view.Result.Signal != nil {
			t.Fatalf("cancelled %s", f)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		delegate(t, contractBinary(t, "./internal/adapter"), "./internal/adapter", "^TestGrokFinal$", "TestGrokFinal")
		if got := grokOffline("grok", []byte("not json {\n")); got.Message != nil || got.Error != adapter.FinalInvalid {
			t.Fatalf("offline malformed %+v", got)
		}
		if !wave2Linux() {
			return
		}
		out, zero := "not json {\n", 0
		sc := scenario{Goal: r.goal("grok malformed"), Vendor: "grok", ExpectedArgv: grokArgv("{prompt}"), ExpectedStdin: strPtr(""), Synthetic: true,
			Stdout: &out, Exit: &zero, Normalization: "synthetic malformed stdout (robustness, not qualification evidence)"}
		f := r.replay(t, "wg", sc)
		if f.view.State != contract.TaskSucceeded || exitOf(f) != 0 || finalOf(f) != "<null>" || string(f.logs) != out+diagnosticLine("grok", adapter.FinalInvalid) {
			t.Fatalf("malformed %s", f)
		}
		// The stdin-missing capture's output (exit 2, empty stdout) replayed
		// under a valid invocation: failed, null, one diagnostic.
		f = r.grokReplay(t, "grok stdin missing", "grok-stdin-missing")
		want := append(wave2File(t, "runs/grok-stdin-missing/stderr.bin"), diagnosticLine("grok", adapter.FinalInvalid)...)
		if f.view.State != contract.TaskFailed || exitOf(f) != 2 || finalOf(f) != "<null>" || !bytes.Equal(f.logs, want) {
			t.Fatalf("stdin missing %s", f)
		}
	})
}

// FP-5: Cursor's captured json results through the bounded public
// extractor, absent output as null, and the production refusal.
func TestWave2CursorFinal(t *testing.T) {
	t.Parallel()
	r := sharedWave2Rig(t)
	t.Run("success", func(t *testing.T) {
		if got := grokOffline("cursor", wave2File(t, "runs/cursor-stdin-success/stdout.bin")); msgOrNil(got) != "pong" || got.Truncated || got.Error != "" {
			t.Fatalf("cursor success %+v", got)
		}
		for _, run := range []string{"cursor-shell-home", "cursor-edit-permitted"} {
			var doc struct{ Result string }
			out := wave2File(t, "runs/"+run+"/stdout.bin")
			json.Unmarshal(out, &doc)
			if got := grokOffline("cursor", out); msgOrNil(got) != doc.Result || doc.Result == "" {
				t.Fatalf("%s %+v", run, got)
			}
		}
	})
	t.Run("absent", func(t *testing.T) {
		for _, run := range []string{"cursor-stdin-fail", "cursor-baseline-home", "cursor-sandbox-home"} {
			out := wave2File(t, "runs/"+run+"/stdout.bin")
			if got := grokOffline("cursor", out); len(out) != 0 || got.Message != nil || got.Error != adapter.FinalInvalid {
				t.Fatalf("%s %+v", run, got)
			}
		}
	})
	t.Run("malformed", func(t *testing.T) {
		delegate(t, contractBinary(t, "./internal/adapter"), "./internal/adapter", "^TestCursorFinal$", "TestCursorFinal")
		for _, out := range []string{`{"text":"pong","stopReason":"end_turn"}`, "pong\n", `{"type":"result","is_error":false,"result":"a"}{}`} {
			if got := grokOffline("cursor", []byte(out)); got.Message != nil || got.Error != adapter.FinalInvalid {
				t.Fatalf("%q %+v", out, got)
			}
		}
	})
	t.Run("blocked", func(t *testing.T) {
		r.refuse(t, r.role("final-cursor", "cursor"), wave2CursorRefusal)
		if _, err := adapterFor("cursor").Invocation(adapter.TaskInput{TaskID: "t_" + strings.Repeat("0", 32), Model: "grok-4.7", Effort: "low", Prompt: []byte("p")}); err == nil ||
			err.Error() != wave2CursorRefusal {
			t.Fatalf("cursor invocation %v", err)
		}
		noCursorTask(t, r.kit.replayKit)
	})
}

// FP-6: process exits decide outcomes, controls keep their precedence,
// refusals launch nothing and sealed results are immutable.
func TestWave2Outcomes(t *testing.T) {
	t.Parallel()
	r := sharedWave2Rig(t)
	t.Run("exits", func(t *testing.T) {
		if !wave2Linux() {
			r.refuse(t, r.role("exit-grok", "grok"), wave2GrokRefusal)
			delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^wave2-outcomes$", "TestRealAdapterLocal", "TestRealAdapterLocal/wave2-outcomes")
			return
		}
		for _, c := range []struct {
			run   string
			exit  int
			state string
		}{{"grok-stdin-fail", 1, contract.TaskFailed}, {"grok-edit-permitted", 0, contract.TaskSucceeded}, {"grok-stdin-missing", 2, contract.TaskFailed}} {
			f := r.grokReplay(t, "exit "+c.run, c.run)
			if f.view.State != c.state || exitOf(f) != c.exit || f.view.Result.Signal != nil || f.view.Result.State != c.state {
				t.Fatalf("%s %s", c.run, f)
			}
		}
	})
	t.Run("controls", func(t *testing.T) {
		delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^wave2-outcomes$", "TestRealAdapterLocal", "TestRealAdapterLocal/wave2-outcomes")
	})
	t.Run("refusal", func(t *testing.T) {
		r.refuse(t, r.role("outcome-cursor", "cursor"), wave2CursorRefusal)
		delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^wave2-posture$", "TestRealAdapterLocal", "TestRealAdapterLocal/wave2-posture")
		noCursorTask(t, r.kit.replayKit)
	})
	t.Run("retry", func(t *testing.T) {
		delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^wave2-retry$", "TestRealAdapterLocal", "TestRealAdapterLocal/wave2-retry")
		if !wave2Linux() {
			return
		}
		f := r.grokReplay(t, "grok sealed", "grok-stdin-success")
		again, err := r.dep.Client.ShowTask(context.Background(), f.view.TaskID, contract.DefaultTailLines)
		a, _ := json.Marshal(f.view.Result)
		b, _ := json.Marshal(again.Result)
		if err != nil || !bytes.Equal(a, b) || finalOf(f) != "pong" {
			t.Fatalf("sealed result changed: %s then %s (%v)", a, b, err)
		}
	})
}

// Frozen against the base fixtures (1d9d663's catalog): the twelve 07b
// timeout facts and Grok's/Cursor's mcp_config and runbook facts.
const (
	wave2TimeoutsSHA256 = "1a9150027290e4e4bbd90dbae839308b501a73768016774a3d040fe54099936b"
	wave2CoordSHA256    = "ec7e65c3abafe812ae450bfce0d00ab704b842fd49737035ce6aebbc2c5b0ea5"
)

// FP-7: the Grok recipe, Cursor's explicitly non-runnable candidate, the
// exact evidence hashes and every frozen fact.
func TestWave2Catalog(t *testing.T) {
	t.Parallel()
	root := testkit.MustRepoRoot(t)
	entries, err := catalog.Load(filepath.Join(root, "tests", "testdata", "support-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]catalog.Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	doc := string(repoFile(t, "docs/support-catalog.md"))
	t.Run("recipes", func(t *testing.T) {
		value := byID["grok"].Facts["headless"].Value
		start := strings.Index(value, "`grok ")
		end := strings.Index(value[start+1:], "`")
		recipe := strings.Fields(value[start+1 : start+1+end])
		if want := append([]string{"grok"}, grokArgv("<composed-prompt>")...); !slices.Equal(recipe, want) {
			t.Fatalf("grok catalog recipe %q, want %q", recipe, want)
		}
		captured := strings.Split(strings.TrimSuffix(string(wave2File(t, "runs/grok-stdin-success/argv.txt")), "\n"), "\n")
		if len(captured) != 11 || !slices.Equal(captured[1:10], grokArgv("")[:9]) || captured[10] != strings.TrimSpace(string(wave2File(t, "runs/grok-stdin-missing/stdin.txt"))) {
			t.Fatalf("grok captured argv %q", captured)
		}
		cvalue := byID["cursor"].Facts["headless"].Value
		start = strings.Index(cvalue, "`cursor-agent ")
		end = strings.Index(cvalue[start+1:], "`")
		candidate := strings.Fields(cvalue[start+1 : start+1+end])
		ccaptured := strings.Split(strings.TrimSuffix(string(wave2File(t, "runs/cursor-stdin-success/argv.txt")), "\n"), "\n")
		ccaptured[0] = "cursor-agent"
		if !slices.Equal(candidate, ccaptured) || !strings.Contains(cvalue, "rejected candidate, not production argv") {
			t.Fatalf("cursor candidate %q (capture %q)", candidate, ccaptured)
		}
		if _, err := adapterFor("cursor").Invocation(adapter.TaskInput{TaskID: "t_" + strings.Repeat("0", 32), Model: "grok-4.7", Effort: "low", Prompt: []byte("p")}); err == nil {
			t.Fatal("cursor became callable")
		}
		for _, s := range []string{
			"Qualified worker recipe (Linux only): `grok --output-format json --model grok-4.7 --reasoning-effort low --permission-mode dontAsk -p <composed-prompt>`",
			"Rejected candidate, recorded for comparison only: `cursor-agent -p --output-format json --model grok-4.7-low --force --trust`",
			"Qualified worker recipe (Linux): `claude -p --model sonnet --effort low --permission-prompts none --output-format json`",
		} {
			if !strings.Contains(doc, s) {
				t.Fatalf("docs/support-catalog.md lacks %q", s)
			}
		}
	})
	t.Run("evidence", func(t *testing.T) {
		var m struct {
			Provenance string `json:"provenance"`
			SourceRoot string `json:"source_root"`
			Files      []struct {
				Destination, Source, SHA256 string
				Bytes                       int
			} `json:"files"`
			Absent []string `json:"absent"`
			Errata []string `json:"errata"`
		}
		if err := json.Unmarshal(repoFile(t, wave2CapturesRel+"/manifest.json"), &m); err != nil {
			t.Fatal(err)
		}
		listed := map[string]bool{}
		for _, f := range m.Files {
			b := repoFile(t, f.Destination)
			sum := sha256.Sum256(b)
			if len(b) != f.Bytes || hex.EncodeToString(sum[:]) != f.SHA256 || !strings.HasPrefix(f.Destination, wave2CapturesRel+"/") {
				t.Fatalf("manifest entry %+v does not match its copy (%d bytes)", f, len(b))
			}
			listed[f.Destination] = true
		}
		for _, a := range m.Absent {
			if _, err := os.Lstat(filepath.Join(root, wave2CapturesRel, a)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s is listed absent but exists", a)
			}
		}
		filepath.WalkDir(filepath.Join(root, wave2CapturesRel), func(p string, d os.DirEntry, err error) error {
			rel, _ := filepath.Rel(root, p)
			if err == nil && !d.IsDir() && filepath.Base(p) != "manifest.json" && !listed[filepath.ToSlash(rel)] {
				t.Errorf("unlisted evidence file %s", rel)
			}
			return nil
		})
		if len(m.Files) != 169 || len(m.Absent) != 57 || !strings.Contains(m.Provenance, "not a new run") || len(m.Errata) != 1 ||
			!strings.Contains(m.Errata[0], "`runs/grok-stdin-fail/stdout.bin` is authoritative") {
			t.Fatalf("manifest: %d files, %d absent, %q %q", len(m.Files), len(m.Absent), m.Provenance, m.Errata)
		}
		versions, _ := wave2Versions(root)
		for _, vendor := range []string{"grok", "cursor"} {
			e := byID[vendor]
			if versions[vendor] != e.Version || e.Version != qualification(vendor).Version {
				t.Fatalf("%s version %q (host %q)", vendor, e.Version, versions[vendor])
			}
			for key, f := range e.Facts {
				for _, ev := range f.Evidence {
					if strings.HasPrefix(ev, wave2CapturesRel+"/") && !listed[ev] {
						t.Fatalf("%s.%s evidence %s is not a manifest copy", vendor, key, ev)
					}
				}
				if f.Status == catalog.Verified && (strings.Contains(strings.ToLower(f.Value), "unknown") || strings.Contains(strings.ToLower(f.Value), "unverified")) {
					t.Fatalf("%s.%s VERIFIED value mentions an unknown: %s", vendor, key, f.Value)
				}
			}
			for _, key := range []string{"sandbox_linux", "sandbox_macos", "exit_codes"} {
				if e.Facts[key].Status != catalog.Unverified {
					t.Fatalf("%s.%s promoted", vendor, key)
				}
			}
		}
		if byID["cursor"].Facts["approval"].Status != catalog.Unverified {
			t.Fatal("cursor approval promoted")
		}
		delegateCatalog(t)
	})
	t.Run("ownership", func(t *testing.T) {
		if err := catalog.Validate(root, entries); err != nil {
			t.Fatal(err)
		}
		for _, vendor := range []string{"grok", "cursor"} {
			for _, key := range catalog.RequiredFacts {
				want := "11"
				if strings.HasPrefix(key, "mcp_timeout") || key == "mcp_progress_extension" {
					want = "07b"
				}
				if got := byID[vendor].Facts[key].VerificationIteration; got != want || catalog.Owner(vendor, key) != want {
					t.Fatalf("%s.%s owner %s, want %s", vendor, key, got, want)
				}
			}
		}
		var timeouts, coord []catalog.Fact
		for _, vendor := range []string{"claude", "codex", "grok", "cursor"} {
			for _, key := range []string{"mcp_timeout", "mcp_timeout_override", "mcp_progress_extension"} {
				timeouts = append(timeouts, byID[vendor].Facts[key])
			}
		}
		for _, vendor := range []string{"grok", "cursor"} {
			for _, key := range []string{"mcp_config", "runbook"} {
				coord = append(coord, byID[vendor].Facts[key])
			}
		}
		// Apart from the short-poll policy that replaced each mcp_timeout's
		// retired interim suffix (design nonblocking-coordinator-waits).
		timeouts, restored := preShortPoll(timeouts)
		tb, _ := json.Marshal(timeouts)
		cb, _ := json.Marshal(coord)
		if ts, cs := sha256.Sum256(tb), sha256.Sum256(cb); hex.EncodeToString(ts[:]) != wave2TimeoutsSHA256 || hex.EncodeToString(cs[:]) != wave2CoordSHA256 || restored != 4 {
			t.Fatal("a frozen 07b timeout or coordinator fact changed")
		}
		for _, s := range []string{"## Grok Build", "## Cursor Agent", `<a id="interim-mcp-wait-exception"></a>`, "`07b` for the three coordinator timeout facts"} {
			if !strings.Contains(doc, s) {
				t.Fatalf("docs/support-catalog.md lost %q", s)
			}
		}
		ra := string(repoFile(t, "docs/real-adapters.md"))
		requireTerms(t, "docs/real-adapters.md", ra, "## Wave 2: Grok and Cursor", sidecar.CursorVendorWarning[:60], sidecar.GrokVendorWarning,
			"--grok-adapter", "--cursor-adapter", "dedicated OS user", "32 KiB", "process inspection", workersmoke.Wave2Command, workersmoke.Command,
			"M3 has not been demonstrated", "fail-closed boundary")
		for _, d := range []string{"real-adapters.md", "support-catalog.md"} {
			if err := catalog.CheckDocLinks(filepath.Join(root, "docs", d)); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// delegateCatalog runs the catalog package's evidence contract.
func delegateCatalog(t *testing.T) {
	t.Helper()
	delegate(t, contractBinary(t, "./internal/testkit/catalog"), "./internal/testkit/catalog", "^TestRealAdapterEvidence$", "TestRealAdapterEvidence")
}

// FP-8: real stdio MCP dispatch through a real plane to the enrolled
// worker running the Grok replay; Cursor refused with no launch; the PATH
// traps prove no installed vendor was used.
func TestWave2Dispatch(t *testing.T) {
	t.Parallel()
	r := sharedWave2Rig(t)
	m := startMCP(t, "--plane", r.dep.URL, "--ca", r.dep.CA)
	p := newNodeCLI(t)
	// cliAdd runs the real role add, retrying the documented transient
	// refusal while a parallel parent's role change holds the plane.
	cliAdd := func(t *testing.T, id, vendor string) result {
		t.Helper()
		deadline := time.Now().Add(rigWait)
		for {
			res := runBin(t, p.bin, p.cwd, p.env, "role", "add", id, "--name", id, "--node", r.a.NodeID, "--adapter", vendor, "--instruction", r.ins,
				"--runbook", r.run, "--model", "grok-4.7", "--effort", "low", "--concurrency", "1", "--plane", r.dep.URL, "--ca", r.dep.CA)
			if !strings.Contains(res.stderr, "unavailable:") || !strings.Contains(res.stderr, "retry") || time.Now().After(deadline) {
				return res
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Run("grok", func(t *testing.T) {
		if !wave2Linux() {
			if res := cliAdd(t, "mcp-grok", "grok"); res.code == 0 || !strings.Contains(res.stderr, wave2GrokRefusal) {
				t.Fatalf("grok role add on %s: %+v", runtime.GOOS, res)
			}
			delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^wave2-invocation$", "TestRealAdapterLocal", "TestRealAdapterLocal/wave2-invocation")
			return
		}
		sc := grokScenario(t, r.goal("mcp grok"), "grok-stdin-success")
		writeScenario(t, r.kit.replayKit, sc)
		var id string
		deadline := time.Now().Add(rigWait)
		for {
			res := m.call("dispatch", dispatchArgs("id", "wg", sc.Goal, nil))
			if !res.isError {
				id = taskIDOf(t, res.text)
				break
			}
			if e, err := contract.ParseErrorBody([]byte(res.text)); err != nil || !retryable(e) || time.Now().After(deadline) {
				t.Fatalf("dispatch: %s", res.text)
			}
			time.Sleep(10 * time.Millisecond)
		}
		for {
			text := m.ok("task_wait", map[string]any{"task_ids": []string{id}, "wait": "5s"})
			if strings.Contains(text, `"status":"terminal"`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("task_wait never terminal: %s", text)
			}
		}
		v, err := contract.ParseTaskShowResponse([]byte(m.ok("task_show", map[string]any{"id": id})))
		if err != nil || v.State != contract.TaskSucceeded || v.Result == nil || v.Result.FinalMessage == nil || *v.Result.FinalMessage != "pong" {
			t.Fatalf("task_show %+v %v", v, err)
		}
		lr, err := contract.ParseTaskLogsResponse([]byte(m.ok("task_logs", map[string]any{"id": id})))
		if err != nil || !bytes.Equal(lr.Data, wave2File(t, "runs/grok-stdin-success/stdout.bin")) {
			t.Fatalf("task_logs %q %v", lr.Data, err)
		}
		l := taskLaunch(t, r.kit.replayKit, id)
		if l.Argv0 != r.kit.grok || !strings.Contains(string(l.Prompt), `"goal":"`+sc.Goal+`"`) || len(l.Stdin) != 0 {
			t.Fatalf("grok launch %+v", l)
		}
		// The task's scratch directory was cleaned up.
		if _, err := os.Stat(l.Cwd); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("scratch %s kept: %v", l.Cwd, err)
		}
	})
	t.Run("cursor-refused", func(t *testing.T) {
		if res := cliAdd(t, "mcp-cursor", "cursor"); res.code == 0 || !strings.Contains(res.stderr, wave2CursorRefusal) {
			t.Fatalf("cursor role add: %+v", res)
		}
		if res := m.call("dispatch", dispatchArgs("id", "mcp-cursor", "never", nil)); !res.isError {
			t.Fatalf("a dispatch to the refused cursor role: %s", res.text)
		}
		noCursorTask(t, r.kit.replayKit)
	})
	t.Run("no-vendors", func(t *testing.T) {
		for _, l := range launches(t, r.kit.replayKit, func(launch) bool { return true }) {
			if l.Argv0 != r.kit.grok && l.Argv0 != r.kit.cursor {
				t.Fatalf("a launch outside the explicit paths: %+v", l)
			}
		}
		if b, err := os.ReadFile(r.kit.trapLog); err == nil && len(b) > 0 {
			t.Fatalf("PATH traps were called:\n%s", b)
		}
		// Every trap would have recorded a lookup.
		k, err := newWave2Kit(t.TempDir(), replayBinary(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range wave2Traps {
			cmd := &exec.Cmd{Path: filepath.Join(k.traps, name), Args: []string{name, "--version"}, Env: []string{"PATH=" + k.traps}}
			var ee *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &ee) || ee.ExitCode() != 127 {
				t.Fatalf("the %s trap did not run and refuse: %v", name, err)
			}
		}
		if b, _ := os.ReadFile(k.trapLog); strings.Count(string(b), " --version\n") != len(wave2Traps) {
			t.Fatalf("traps record %q", b)
		}
		if strings.Contains(strings.Join(r.env, " "), "/usr") || r.env[0] != "PATH="+r.kit.traps {
			t.Fatalf("the sidecar environment %q reaches installed binaries", r.env)
		}
	})
}

// FP-9: the smoke gate's decisions with injected environment, stat and OS
// values (no skip), the stub-backed Grok smoke or macOS refusal and the
// Cursor refusal through the full deployment, and the build-tag exclusion.
func TestWave2SmokeGate(t *testing.T) {
	t.Parallel()
	statted := 0
	gate := func(env map[string]string, files map[string]os.FileMode) workersmoke.Gate {
		return workersmoke.Gate{Getenv: func(k string) string { return env[k] }, Stat: func(p string) (os.FileInfo, error) {
			statted++
			if mode, ok := files[p]; ok {
				return fakeInfo{mode: mode}, nil
			}
			return nil, os.ErrNotExist
		}}
	}
	t.Run("default-off", func(t *testing.T) {
		statted = 0
		for _, env := range []map[string]string{{}, {"CALLSHEET_GROK_PATH": "/x", "CALLSHEET_CURSOR_PATH": "/y"}, {workersmoke.EnvOptIn: "yes", "CALLSHEET_GROK_PATH": "/x"}} {
			if d := gate(env, nil).Smoke(); d.Run || !strings.Contains(d.Reason, "opt-in") {
				t.Fatalf("%v: %+v", env, d)
			}
		}
		if statted != 0 {
			t.Fatal("the default-off decision touched the filesystem")
		}
	})
	t.Run("ci-off", func(t *testing.T) {
		statted = 0
		env := map[string]string{workersmoke.EnvOptIn: "1", "CI": "1", "CALLSHEET_GROK_PATH": "/x", "CALLSHEET_CURSOR_PATH": "/y"}
		if d := gate(env, nil).Smoke(); d.Run || !strings.Contains(d.Reason, "CI") || statted != 0 {
			t.Fatalf("ci: %+v (%d stats)", d, statted)
		}
	})
	t.Run("absent", func(t *testing.T) {
		env := map[string]string{workersmoke.EnvOptIn: "1", "CALLSHEET_CURSOR_PATH": "/nonexistent/cursor-agent"}
		g := gate(env, map[string]os.FileMode{"/opt/noexec/grok": 0o644, "/opt/dir": os.ModeDir | 0o755, "/opt/ok/cursor-agent": 0o755})
		if d := g.Vendor("grok"); d.Run || d.Fail != "" || !strings.Contains(d.Skip, "CALLSHEET_GROK_PATH is unset") {
			t.Fatalf("unset grok: %+v", d)
		}
		if d := g.Vendor("cursor"); d.Run || d.Fail != "" || !strings.Contains(d.Skip, "absent") {
			t.Fatalf("absent cursor: %+v", d)
		}
		for path, want := range map[string]string{"/opt/noexec/grok": "no executable permission bit", "/opt/dir": "not a regular file", "grok": "absolute path"} {
			env["CALLSHEET_GROK_PATH"] = path
			if d := g.Vendor("grok"); d.Run || d.Skip != "" || !strings.Contains(d.Fail, want) {
				t.Fatalf("%s: %+v", path, d)
			}
		}
		env["CALLSHEET_CURSOR_PATH"] = "/opt/ok/cursor-agent"
		if d := g.Vendor("cursor"); !d.Run || d.Path != "/opt/ok/cursor-agent" {
			t.Fatalf("present cursor: %+v", d)
		}
		if d := g.Vendor("agent"); d.Fail == "" {
			t.Fatalf("the agent alias is a vendor: %+v", d)
		}
	})
	t.Run("enabled", func(t *testing.T) {
		stub := replayBinary(t)
		root := t.TempDir()
		versions, _ := wave2Versions(testkit.MustRepoRoot(t))
		k, err := newWave2Kit(root, stub, versions)
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{workersmoke.EnvOptIn: "1", "CALLSHEET_GROK_PATH": k.grok, "CALLSHEET_CURSOR_PATH": k.cursor}
		g := workersmoke.Gate{Getenv: func(key string) string { return env[key] }, Stat: os.Stat}
		if d := g.Smoke(); !d.Run || !g.Vendor("grok").Run || !g.Vendor("cursor").Run {
			t.Fatalf("enabled smoke: %+v", d)
		}
		goal := strings.TrimSpace(string(wave2File(t, "runs/cursor-stdin-success/stdin.txt")))
		writeScenario(t, k.replayKit, grokScenario(t, goal, "grok-stdin-success"))
		home := filepath.Join(root, "home")
		os.MkdirAll(home, 0o700)
		base := []string{"PATH=" + k.traps, "HOME=" + home}
		sidecarEnv := append(slices.Clone(base), replayDirEnv+"="+k.dir)
		bin := nodeBinary(t)
		var wg sync.WaitGroup
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), workersmoke.OuterBound)
			defer cancel()
			if workersmoke.Refused("grok", runtime.GOOS) {
				ref, err := workersmoke.Refuse(ctx, bin, filepath.Join(root, "smoke-grok"), base, sidecarEnv, "grok", g.Vendor("grok").Path, runtime.GOOS)
				if err != nil || ref.Message != wave2GrokRefusal {
					t.Errorf("grok refusal smoke %+v %v", ref, err)
				}
				return
			}
			res, err := workersmoke.Run(ctx, bin, filepath.Join(root, "smoke-grok"), base, sidecarEnv, "grok", g.Vendor("grok").Path, goal)
			if err != nil || res.View.State != contract.TaskSucceeded || res.View.Result == nil || *res.View.Result.ExitCode != 0 ||
				res.View.Result.FinalMessage == nil || *res.View.Result.FinalMessage != "pong" {
				t.Errorf("grok smoke %+v %v", res.View, err)
			}
		})
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), workersmoke.OuterBound)
			defer cancel()
			ref, err := workersmoke.Refuse(ctx, bin, filepath.Join(root, "smoke-cursor"), base, sidecarEnv, "cursor", g.Vendor("cursor").Path, runtime.GOOS)
			if err != nil || ref.Message != wave2CursorRefusal {
				t.Errorf("cursor refusal smoke %+v %v", ref, err)
			}
		})
		wg.Wait()
		noCursorTask(t, k.replayKit)
		if b, err := os.ReadFile(k.trapLog); err == nil && len(b) > 0 {
			t.Fatalf("PATH traps were called: %s", b)
		}
		// The real smoke exists only with its tag.
		files, _ := filepath.Glob(filepath.Join(testkit.MustRepoRoot(t), "tests", "smoke", "*.go"))
		if len(files) == 0 {
			t.Fatal("tests/smoke has no Go file")
		}
		for _, f := range files {
			line, _, _ := strings.Cut(string(repoFile(t, "tests/smoke/"+filepath.Base(f))), "\n")
			expr, err := constraint.Parse(line)
			if err != nil {
				t.Fatalf("%s: first line %q is not a build constraint: %v", f, line, err)
			}
			for tags, want := range map[string]bool{"linux amd64": false, "darwin arm64 realadaptercheck": false, "linux amd64 realadaptersmoke": true} {
				set := strings.Fields(tags)
				if expr.Eval(func(tag string) bool { return slices.Contains(set, tag) }) != want {
					t.Fatalf("%s with tags %q: included %v", f, tags, !want)
				}
			}
		}
		smoke := string(repoFile(t, "tests/smoke/real_worker_smoke_test.go"))
		if !strings.Contains(workersmoke.Wave2Command, "-tags="+workersmoke.Tag+" ./tests/smoke -run '^TestRealWorkerSmoke$'") ||
			!strings.Contains(string(repoFile(t, "docs/real-adapters.md")), workersmoke.Wave2Command) || !strings.Contains(smoke, `"grok", "cursor"`) ||
			!strings.Contains(smoke, "workersmoke.Refused(vendor, runtime.GOOS)") {
			t.Fatal("the documented wave-2 smoke command or subtests changed")
		}
	})
	t.Run("posture", func(t *testing.T) {
		// Both OS values decide on either host, before any process activity.
		for _, c := range []struct {
			vendor, goos string
			refused      bool
		}{{"grok", "linux", false}, {"grok", "darwin", true}, {"cursor", "linux", true}, {"cursor", "darwin", true}} {
			if workersmoke.Refused(c.vendor, c.goos) != c.refused {
				t.Fatalf("Refused(%s, %s) = %v", c.vendor, c.goos, !c.refused)
			}
		}
		// The refusal helper accepts only the exact posture refusal.
		r := sharedWave2Rig(t)
		ctx := context.Background()
		if err := r.dep.RefuseVendorRole(ctx, "smoke-cursor-posture", "cursor", r.a.NodeID, r.ins, r.run, runtime.GOOS); err != nil {
			t.Fatalf("cursor refusal helper: %v", err)
		}
		if err := r.dep.RefuseVendorRole(ctx, "smoke-grok-linux", "grok", r.a.NodeID, r.ins, r.run, "linux"); !errors.Is(err, workersmoke.ErrNotRefused) {
			t.Fatalf("an eligible grok counted as refused: %v", err)
		}
		if !wave2Linux() {
			if err := r.dep.RefuseVendorRole(ctx, "smoke-grok-posture", "grok", r.a.NodeID, r.ins, r.run, runtime.GOOS); err != nil {
				t.Fatalf("grok refusal helper on %s: %v", runtime.GOOS, err)
			}
		}
		noCursorTask(t, r.kit.replayKit)
	})
}
