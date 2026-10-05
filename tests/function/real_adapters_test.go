//go:build linux || darwin

package function

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/build/constraint"
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
	"github.com/wedevwork/callsheet/internal/sidecar"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
	"github.com/wedevwork/callsheet/internal/testkit/workersmoke"
)

// Iteration 08 function tests (real adapters, FP-1..FP-9): exactly one
// top-level TestRealAdapter* per FP. They drive real Callsheet processes
// (the built callsheet binary as plane, sidecars and guardians; the real
// callsheet mcp for FP-8) against the replay stub testdata/worker-replay,
// enabled through the product's --claude-adapter and --codex-adapter
// options, with an isolated HOME and a PATH holding only launch-recording
// claude/codex traps: no installed vendor CLI, model or network is ever
// used. Package contracts are delegated with their own run/pass evidence
// (the tagged sidecar contract through its prebuilt realadaptercheck test
// binary). The shared rig is started once per test process and stopped by
// TestMain.

const (
	// replayDirEnv is the stub's scenario directory (sidecar environment
	// only; no product code reads it).
	replayDirEnv = "CALLSHEET_WORKER_REPLAY_DIR"
	// capturesRel is the checked-in evidence root.
	capturesRel = "tests/testdata/real-adapters/linux-2026-09-30"
	// rigWait bounds rig setup and every task wait.
	rigWait = 60 * time.Second
)

// replayBinary is the once-built replay stub.
func replayBinary(t *testing.T) string {
	return fixture(t, "worker-replay", func(dir string) (string, error) {
		return testkit.BuildBinaryAt(dir, "./tests/function/testdata/worker-replay", "worker-replay")
	})
}

// taggedSidecarBinary is the prebuilt realadaptercheck sidecar test binary.
func taggedSidecarBinary(t *testing.T) string {
	return fixture(t, "sidecar-realadaptercheck", func(dir string) (string, error) {
		return testkit.BuildTaggedTestBinaryAt(dir, "./internal/sidecar", "internal-sidecar-realadaptercheck", "realadaptercheck")
	})
}

// delegate runs one package contract selector in pkg's test binary bin and
// requires RUN and PASS evidence for every name.
func delegate(t *testing.T, bin, pkg, run string, names ...string) {
	t.Helper()
	out := contractRun(t, bin, pkg, run, append(os.Environ(), nodeCLIEnv+"="+nodeBinary(t)), names...)
	for _, n := range names {
		if !strings.Contains(out, "=== RUN   "+n+"\n") {
			t.Fatalf("%s: no RUN evidence for %s", pkg, n)
		}
	}
}

// captureDir is a capture's absolute directory.
func captureDir(t testing.TB, rel string) string {
	return filepath.Join(testkit.MustRepoRoot(t), filepath.FromSlash(capturesRel), filepath.FromSlash(rel))
}

func captureFile(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(captureDir(t, ""), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// hostVersions are the captured versions (host.txt), keyed by vendor.
func hostVersions(root string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(capturesRel), "host.txt"))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		for _, id := range []string{"claude", "codex"} {
			if v, ok := strings.CutPrefix(line, id+": "); ok {
				out[id] = v
			}
		}
	}
	return out, nil
}

// replayKit is a scenario directory with claude and codex links to the
// replay stub (in a directory whose name holds a space) and a PATH of
// launch-recording traps.
type replayKit struct {
	dir, claude, codex string
	traps, trapLog     string
}

// newReplayKit prepares a kit under root whose stubs print versions.
func newReplayKit(root, stub string, versions map[string]string) (*replayKit, error) {
	k := &replayKit{dir: filepath.Join(root, "replay"), traps: filepath.Join(root, "traps"), trapLog: filepath.Join(root, "traps.log")}
	bin := filepath.Join(root, "replay bin")
	for _, d := range []string{k.dir, filepath.Join(k.dir, "scenarios"), filepath.Join(k.dir, "launches"), bin, k.traps} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	k.claude, k.codex = filepath.Join(bin, "claude"), filepath.Join(bin, "codex")
	for id, p := range map[string]string{"claude": k.claude, "codex": k.codex} {
		if err := os.Symlink(stub, p); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(k.dir, id+".version"), []byte(versions[id]+"\n"), 0o644); err != nil {
			return nil, err
		}
		// A PATH trap: any lookup of the vendor name is recorded.
		trap := "#!/bin/sh\necho \"" + id + " $*\" >> '" + k.trapLog + "'\nexit 127\n"
		if err := writeExecutable(filepath.Join(k.traps, id), []byte(trap)); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// scenario mirrors the stub's scenario file.
type scenario struct {
	Goal          string   `json:"goal"`
	Vendor        string   `json:"vendor"`
	SourceArgv    []string `json:"source_argv"`
	ExpectedArgv  []string `json:"expected_argv"`
	Normalization string   `json:"normalization"`
	ExpectedStdin *string  `json:"expected_stdin"`
	// ExpectedPrompt (iteration 11) is Grok's exact -p value.
	ExpectedPrompt *string `json:"expected_prompt"`
	Capture        string  `json:"capture"`
	Synthetic      bool    `json:"synthetic"`
	Stdout         *string `json:"stdout"`
	Stderr         *string `json:"stderr"`
	Exit           *int    `json:"exit"`
	FinalMode      string  `json:"final_mode"`
	Final          string  `json:"final"`
	Hold           bool    `json:"hold"`
}

// productionArgv is the vendor's qualified argv, the final path a
// placeholder.
func productionArgv(vendor string) []string {
	inv, _ := adapterFor(vendor).Invocation(adapter.TaskInput{TaskID: "t_" + strings.Repeat("0", 32), Model: qualification(vendor).Model,
		Effort: qualification(vendor).Effort, ScratchDir: "/scratch"})
	out := slices.Clone(inv.Argv)
	for i, a := range out {
		if a == "/scratch/"+adapter.CodexFinalName {
			out[i] = "{final}"
		}
	}
	return out
}

func adapterFor(vendor string) adapter.Adapter {
	a, _ := adapter.Builtin("").Lookup(vendor)
	return a
}

func qualification(vendor string) adapter.Qualification {
	for _, q := range adapter.Qualifications() {
		if q.ID == vendor {
			return q
		}
	}
	return adapter.Qualification{}
}

// tokens splits a captured shell-form argv.txt (backslash escapes, no
// quotes in the captures) into its words; it never executes anything.
func tokens(s string) []string {
	var out []string
	var cur strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			in = true
		case c == ' ' || c == '\n' || c == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// captureScenario replays capture rel (runs/ or runs-scratch/) for goal:
// the production argv, the capture's argv recorded as source evidence and
// the normalization between them stated.
func captureScenario(t testing.TB, vendor, goal, rel string) scenario {
	src := tokens(string(captureFile(t, rel+"/argv.txt")))
	return scenario{Goal: goal, Vendor: vendor, SourceArgv: src, ExpectedArgv: productionArgv(vendor), Capture: captureDir(t, rel),
		Normalization: "the production argv of the qualified pair replays this capture's recorded bytes; the capture's own argv (its model, positional prompt or " +
			"flags) and final path are evidence only, the final path being this task's scratch file"}
}

// realRig is the shared deployment: a plane and worker A enabling both
// replay vendors, with roles rc (claude) and rx (codex).
type realRig struct {
	root   string
	kit    *replayKit
	dep    *workersmoke.Deployment
	a      *workersmoke.Sidecar
	b      *workersmoke.Sidecar
	h      *workersmoke.Sidecar
	fakeB  contract.RoleView
	env    []string
	ins    string
	run    string
	insTxt string
	runTxt string
	mu     sync.Mutex
	n      int
}

var (
	rigOnce sync.Once
	theRig  *realRig
	rigErr  error
)

// sharedRig starts the rig once per test process; TestMain stops it.
func sharedRig(t *testing.T) *realRig {
	t.Helper()
	bin, stub, fake := nodeBinary(t), replayBinary(t), fakeAdapterBinary(t)
	rigOnce.Do(func() { theRig, rigErr = startRealRig(bin, stub, fake) })
	if rigErr != nil {
		t.Fatal(rigErr)
	}
	return theRig
}

func startRealRig(bin, stub, fakeAdapterPath string) (*realRig, error) {
	root, err := os.MkdirTemp("", "callsheet-real-adapters-")
	if err != nil {
		return nil, err
	}
	r := &realRig{root: root}
	fixtureMu.Lock()
	afterSuite = append(afterSuite, r.close)
	fixtureMu.Unlock()
	versions, err := hostVersions(repoRootOrEmpty())
	if err != nil {
		return nil, err
	}
	if r.kit, err = newReplayKit(root, stub, versions); err != nil {
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
	if r.a, err = r.dep.StartSidecar(ctx, "worker-a", r.sidecarEnv(r.kit), "--claude-adapter", r.kit.claude, "--codex-adapter", r.kit.codex); err != nil {
		return nil, err
	}
	r.insTxt = "Replay instruction: answer the goal. Quotes \" \\ and é 😀 stay bytes.\n"
	r.runTxt = "Replay runbook.\n"
	if r.ins, r.run, err = r.dep.Manuals("replay", r.insTxt, r.runTxt); err != nil {
		return nil, err
	}
	// Every successful role mutation happens here, before any parent
	// dispatches, and worker A's roles come last: the plane stamps a start
	// with the registry-wide roles revision but sends a new snapshot only to
	// the mutated role's node, so after a later mutation on another node
	// worker A would refuse every start as role_changed (pre-existing
	// plane/sidecar behavior, reported with iteration 08). Worker B enables
	// claude and the fake explicitly (no codex); its fake role is FP-1's
	// "fake only with its own flag".
	if r.b, err = r.dep.StartSidecar(ctx, "worker-b", r.sidecarEnv(r.kit), "--claude-adapter", r.kit.claude, "--fake-adapter", fakeAdapterPath); err != nil {
		return nil, err
	}
	if r.fakeB, err = r.dep.Client.AddRole(ctx, contract.RoleConfig{ID: "reg-fake-b", Name: "reg", Node: r.b.NodeID, Adapter: "fake", Instruction: r.ins,
		Runbook: r.run, Model: "example model", Effort: "medium", Concurrency: 1}); err != nil {
		return nil, err
	}
	// Worker H (codex only) hosts FP-6's cancellation: an execution's
	// unresolved group cleanup blocks every start on its node (06b), which
	// must not hold worker A's starts while the other parents dispatch.
	if r.h, err = r.dep.StartSidecar(ctx, "worker-h", r.sidecarEnv(r.kit), "--codex-adapter", r.kit.codex); err != nil {
		return nil, err
	}
	roles := []contract.RoleConfig{{ID: "rh", Name: "rh", Node: r.h.NodeID, Adapter: "codex", Concurrency: 1}}
	for _, id := range []string{"rc", "rx"} {
		roles = append(roles, contract.RoleConfig{ID: id, Name: id, Node: r.a.NodeID, Adapter: map[string]string{"rc": "claude", "rx": "codex"}[id], Concurrency: 8})
	}
	for _, rc := range roles {
		q := qualification(rc.Adapter)
		rc.Instruction, rc.Runbook, rc.Model, rc.Effort = r.ins, r.run, q.Model, q.Effort
		if _, err := r.dep.Client.AddRole(ctx, rc); err != nil {
			return nil, err
		}
	}
	// Worker A had the last role mutation; worker H's reattachment installs
	// the current complete snapshot, so starts reach both.
	if err := r.dep.Restart(ctx, r.h); err != nil {
		return nil, err
	}
	return r, r.ready(ctx, "rc", "rx", "rh")
}

// repoRootOrEmpty is the repository root (rig setup has no testing.T).
func repoRootOrEmpty() string {
	root, _ := testkit.RepoRoot()
	return root
}

func (r *realRig) close() {
	if r.dep != nil {
		r.dep.Close()
	}
	os.RemoveAll(r.root)
}

// sidecarEnv is a sidecar's complete environment with kit's scenarios.
func (r *realRig) sidecarEnv(k *replayKit) []string {
	return append(slices.Clone(r.env), replayDirEnv+"="+k.dir)
}

// ready waits until every role can accept work.
func (r *realRig) ready(ctx context.Context, ids ...string) error {
	for {
		ok := true
		for _, id := range ids {
			v, err := r.dep.Client.ShowRole(ctx, id)
			ok = ok && err == nil && v.CanAccept
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("roles " + strings.Join(ids, ",") + " never became ready")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// goal returns a unique task goal with prefix.
func (r *realRig) goal(prefix string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return prefix + " #" + itoa(r.n)
}

// unique returns prefix with a rig-unique suffix (repeated runs reuse the
// rig: node state directories must not collide).
func (r *realRig) unique(prefix string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return prefix + "-" + itoa(r.n)
}

// scenario writes sc into kit's scenario directory.
func writeScenario(t *testing.T, k *replayKit, sc scenario) {
	t.Helper()
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(sc.Goal))
	if err := os.WriteFile(filepath.Join(k.dir, "scenarios", hex.EncodeToString(sum[:8])+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// requestFor is a dispatch request of goal to role id.
func requestFor(id, goal string) contract.DispatchRequest {
	return contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: id}, Goal: goal, Payload: []string{"repo://none", "ü"},
		Acceptance: "the replayed answer", RequestedBy: contract.RequestedBy{Name: "callsheet", Version: "function", Hostname: "replay-host"}}
}

// composed is the exact stdin the worker composes for req on role (the
// task ID a placeholder): the callsheet-task-v1 envelope of iteration 05.
func (r *realRig) composed(t *testing.T, req contract.DispatchRequest, roleID, vendor string) string {
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
	q := qualification(vendor)
	env := struct {
		Format      string `json:"format"`
		Instruction string `json:"instruction"`
		Runbook     string `json:"runbook"`
		Task        task   `json:"task"`
	}{"callsheet-task-v1", r.insTxt, r.runTxt, task{TaskID: "{task_id}", Target: req.Target, Role: role{roleID, roleID}, Goal: req.Goal, Payload: req.Payload,
		Acceptance: req.Acceptance, Effective: contract.TaskEffective{Model: q.Model, Effort: q.Effort, Timeout: contract.DefaultRoleTimeout}, RequestedBy: req.RequestedBy}}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// launch mirrors the stub's launch record.
type launch struct {
	Mode     string   `json:"mode"`
	Vendor   string   `json:"vendor"`
	Argv0    string   `json:"argv0"`
	Argv     []string `json:"argv"`
	Cwd      string   `json:"cwd"`
	CwdMode  string   `json:"cwd_mode"`
	Stdin    []byte   `json:"stdin"`
	Prompt   []byte   `json:"prompt"`
	TaskID   string   `json:"task_id"`
	Scenario string   `json:"scenario"`
	Final    string   `json:"final"`
	OK       bool     `json:"ok"`
	Error    string   `json:"error"`
}

// launches returns kit's launch records matching keep.
func launches(t *testing.T, k *replayKit, keep func(launch) bool) []launch {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(k.dir, "launches", "*.json"))
	var out []launch
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), ".") {
			continue // a record still being written (renamed when complete)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var l launch
		if err := json.Unmarshal(b, &l); err != nil {
			t.Fatal(err)
		}
		if keep(l) {
			out = append(out, l)
		}
	}
	return out
}

// taskLaunch returns the one task launch of id.
func taskLaunch(t *testing.T, k *replayKit, id string) launch {
	t.Helper()
	ls := launches(t, k, func(l launch) bool { return l.Mode == "task" && l.TaskID == id })
	if len(ls) != 1 {
		t.Fatalf("task %s launched %d times: %+v", id, len(ls), ls)
	}
	if !ls[0].OK {
		t.Fatalf("task %s: replay fixture failure: %s", id, ls[0].Error)
	}
	return ls[0]
}

// finished is a terminal task with its complete logs.
type finished struct {
	view contract.TaskView
	logs []byte
}

// dispatch sends req and waits for the terminal task and its logs.
func (r *realRig) dispatch(t *testing.T, req contract.DispatchRequest) finished {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rigWait)
	defer cancel()
	v, err := r.send(ctx, req)
	if err != nil {
		var lines []string
		for _, l := range strings.Split(r.a.Logs.String(), "\n") {
			if strings.Contains(l, "ready") || strings.Contains(l, "probe") || strings.Contains(l, "ERROR") || strings.Contains(l, "cleanup") {
				lines = append(lines, l)
			}
		}
		views, _ := r.dep.Client.ListRoles(context.Background())
		var vs []string
		for _, v := range views {
			vs = append(vs, v.ID+" inflight="+itoa(v.Inflight)+" can_accept="+map[bool]string{true: "true", false: "false"}[v.CanAccept])
		}
		n, _ := r.dep.Client.ShowNode(context.Background(), r.a.NodeID)
		nb, _ := json.Marshal(n)
		t.Fatalf("dispatch %q: %v\nroles: %v\nnode A: %s\nworker A's readiness log lines:\n%s", req.Goal, err, vs, nb, strings.Join(lines[max(0, len(lines)-5):], "\n"))
	}
	return r.await(t, ctx, v.TaskID)
}

// send dispatches req, retrying the plane's documented "nothing was
// dispatched, retry" refusal while another test's dispatch or role change
// holds the plane's admission (the parents run in parallel on one rig).
func (r *realRig) send(ctx context.Context, req contract.DispatchRequest) (contract.TaskView, error) {
	for {
		v, err := r.dep.Client.Dispatch(ctx, req)
		if err == nil || contract.CodeOf(err) != contract.CodeUnavailable || !strings.Contains(err.Error(), "nothing was dispatched, retry") {
			return v, err
		}
		select {
		case <-ctx.Done():
			return v, err
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// retryable is the plane's or node's documented transient refusal of a
// mutation (another dispatch or role change in progress, a busy
// validation slot): nothing was changed, and the caller retries.
func retryable(err error) bool {
	return contract.CodeOf(err) == contract.CodeUnavailable && strings.Contains(err.Error(), "retry")
}

// addRole registers rc, retrying transient refusals.
func (r *realRig) addRole(ctx context.Context, rc contract.RoleConfig) (contract.RoleView, error) {
	for {
		v, err := r.dep.Client.AddRole(ctx, rc)
		if err == nil || !retryable(err) || ctx.Err() != nil {
			return v, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// String summarizes a finished task for failure messages.
func (f finished) String() string {
	s := "state " + f.view.State
	if f.view.Reason != nil {
		s += " reason " + f.view.Reason.Code + " (" + f.view.Reason.Message + ")"
	}
	if f.view.Result != nil {
		b, _ := json.Marshal(f.view.Result)
		s += " result " + string(b)
	}
	return s + " logs " + strconvQuote(f.logs)
}

func strconvQuote(b []byte) string {
	q, _ := json.Marshal(string(b))
	return string(q)
}

func (r *realRig) await(t *testing.T, ctx context.Context, id string) finished {
	t.Helper()
	for {
		v, err := r.dep.Client.ShowTask(ctx, id, contract.DefaultTailLines)
		if err == nil && contract.TaskTerminal(v.State) {
			lg, err := r.dep.Client.TaskLogs(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			return finished{view: v, logs: lg.Data}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("task %s did not finish: %+v %v\n%s", id, v.State, err, r.a.Logs.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// replay dispatches sc (written first) to role id.
func (r *realRig) replay(t *testing.T, roleID string, sc scenario) finished {
	t.Helper()
	writeScenario(t, r.kit, sc)
	return r.dispatch(t, requestFor(roleID, sc.Goal))
}

// finalOf renders a result's final message (null shown as <null>).
func finalOf(f finished) string {
	if f.view.Result == nil || f.view.Result.FinalMessage == nil {
		return "<null>"
	}
	return *f.view.Result.FinalMessage
}

func exitOf(f finished) int {
	if f.view.Result == nil || f.view.Result.ExitCode == nil {
		return -1
	}
	return *f.view.Result.ExitCode
}

// interleaves reports whether log is an interleaving of a and b that keeps
// each stream's own byte order (pipes are drained concurrently, so only
// each stream's order is a captured promise).
func interleaves(log, a, b []byte) bool {
	if len(log) != len(a)+len(b) {
		return false
	}
	// ok[j] is whether log[:i+j] interleaves a[:i] and b[:j].
	ok := make([]bool, len(b)+1)
	ok[0] = true
	for j := 1; j <= len(b); j++ {
		ok[j] = ok[j-1] && log[j-1] == b[j-1]
	}
	for i := 1; i <= len(a); i++ {
		ok[0] = ok[0] && log[i-1] == a[i-1]
		for j := 1; j <= len(b); j++ {
			ok[j] = (ok[j] && log[i+j-1] == a[i-1]) || (ok[j-1] && log[i+j-1] == b[j-1])
		}
	}
	return ok[len(b)]
}

// diagnosticLine is the fixed task-log line of a failed extraction.
func diagnosticLine(vendor, code string) string {
	return "callsheet: " + vendor + " final-message extraction failed (" + code + ")\n"
}

// logRecords counts the JSON log records in logs whose msg is msg.
func logRecords(logs, msg string) int {
	n := 0
	for line := range strings.SplitSeq(logs, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			n++
		}
	}
	return n
}

// FP-1: explicit sidecar flags enable the vendor roles; unqualified pairs
// and disabled vendors are refused; the fake stays test-only and needs its
// own flag.
func TestRealAdapterRegistration(t *testing.T) {
	t.Parallel()
	r := sharedRig(t)
	ctx := context.Background()
	t.Run("registry", func(t *testing.T) {
		delegate(t, contractBinary(t, "./internal/adapter"), "./internal/adapter", "^TestRealAdapterRegistry$", "TestRealAdapterRegistry")
		for id, vendor := range map[string]string{"rc": "claude", "rx": "codex"} {
			v, err := r.dep.Client.ShowRole(ctx, id)
			if err != nil || v.Adapter != vendor || v.AdapterTestOnly || v.Effort != "low" || v.Model != qualification(vendor).Model {
				t.Fatalf("role %s = %+v %v", id, v, err)
			}
		}
		// Worker A has no --fake-adapter: the fake is refused there; a worker
		// started with it (and claude only) accepts a fake role, test-only.
		fake := contract.RoleConfig{ID: "reg-fake-a", Name: "reg", Node: r.a.NodeID, Adapter: "fake", Instruction: r.ins, Runbook: r.run,
			Model: "example model", Effort: "medium", Concurrency: 1}
		if _, err := r.addRole(ctx, fake); contract.CodeOf(err) != contract.CodeInvalidArgument ||
			!strings.Contains(err.Error(), "fake adapter is disabled on node; start sidecar with --fake-adapter ABSOLUTE_PATH") {
			t.Fatalf("fake on worker A: %v", err)
		}
		// (The rig registered worker B's fake role before any dispatch.)
		if v := r.fakeB; v.Node != r.b.NodeID || v.Adapter != "fake" || !v.AdapterTestOnly {
			t.Fatalf("fake on worker B: %+v", v)
		}
		if _, ok := r.b.Logs.Record(`fake adapter enabled: test/demo adapter; never calls a model`); !ok || logRecords(r.b.Logs.String(), sidecar.FakeAdapterWarning) != 1 {
			t.Fatalf("worker B lacks the fake warning (or repeats it):\n%s", r.b.Logs.String())
		}
		// Worker B has no --codex-adapter: codex is disabled there.
		cx := contract.RoleConfig{ID: "reg-codex-b", Name: "reg", Node: r.b.NodeID, Adapter: "codex", Instruction: r.ins, Runbook: r.run,
			Model: "gpt-6.1-sol", Effort: "low", Concurrency: 1}
		if _, err := r.addRole(ctx, cx); contract.CodeOf(err) != contract.CodeInvalidArgument ||
			!strings.Contains(err.Error(), "codex adapter is disabled on node; start sidecar with --codex-adapter ABSOLUTE_PATH") {
			t.Fatalf("codex on worker B: %v", err)
		}
		if strings.Contains(r.a.Logs.String(), "fake adapter enabled") {
			t.Fatalf("worker A warned about the fake:\n%s", r.a.Logs.String())
		}
		// Worker A enables real vendors: the Darwin qualification warning
		// is logged exactly once on macOS and never elsewhere.
		darwin := 0
		if runtime.GOOS == "darwin" {
			darwin = 1
		}
		if n := logRecords(r.a.Logs.String(), sidecar.DarwinVendorWarning); n != darwin || darwin == 0 && strings.Contains(r.a.Logs.String(), "macOS") {
			t.Fatalf("worker A logged the Darwin warning %d times on %s (want %d):\n%s", n, runtime.GOOS, darwin, r.a.Logs.String())
		}
		// The plane refuses effort outside the vendor's descriptor.
		bad := contract.RoleConfig{ID: "reg-effort", Name: "reg", Node: r.a.NodeID, Adapter: "claude", Instruction: r.ins, Runbook: r.run,
			Model: "sonnet", Effort: "medium", Concurrency: 1}
		if _, err := r.addRole(ctx, bad); contract.CodeOf(err) != contract.CodeInvalidArgument || !strings.Contains(err.Error(), "allowed: low") {
			t.Fatalf("claude medium: %v", err)
		}
	})
	t.Run("paths", func(t *testing.T) {
		delegate(t, contractBinary(t, "./internal/cli"), "./internal/cli", "^TestRealAdapterCLIOptions$", "TestRealAdapterCLIOptions")
		// Explicit paths are used literally, spaces included (worker A's
		// roles were validated through them); a relative, empty or repeated
		// flag is a usage error of the real CLI before any state access.
		if !strings.Contains(r.kit.claude, " ") {
			t.Fatal("the rig's claude path has no space")
		}
		p := newNodeCLI(t)
		state := t.TempDir()
		for _, args := range [][]string{{"--claude-adapter", "claude"}, {"--codex-adapter", ""}, {"--codex-adapter", "/a", "--codex-adapter", "/b"},
			{"--claude-adapter", "./bin/claude"}} {
			res := runBin(t, p.bin, p.cwd, p.env, append([]string{"sidecar", "run", "--state-dir", state}, args...)...)
			if res.code != 2 || !strings.Contains(res.stderr, "Usage:") || !strings.Contains(res.stderr, "adapter") {
				t.Fatalf("sidecar run %v = %+v", args, res)
			}
		}
		if entries, _ := os.ReadDir(state); len(entries) != 0 {
			t.Fatalf("a usage error touched the state: %v", entries)
		}
		// An omitted flag disables that vendor, whatever PATH holds.
		b, err := r.dep.StartSidecar(ctx, r.unique("worker-paths"), r.sidecarEnv(r.kit), "--claude-adapter", r.kit.claude)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Stop() })
		cx := contract.RoleConfig{ID: "paths-codex", Name: "paths", Node: b.NodeID, Adapter: "codex", Instruction: r.ins, Runbook: r.run,
			Model: "gpt-6.1-sol", Effort: "low", Concurrency: 1}
		if _, err := r.addRole(ctx, cx); contract.CodeOf(err) != contract.CodeInvalidArgument ||
			!strings.Contains(err.Error(), "codex adapter is disabled on node; start sidecar with --codex-adapter ABSOLUTE_PATH") {
			t.Fatalf("omitted codex: %v", err)
		}
	})
	t.Run("selection", func(t *testing.T) {
		delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^selection$", "TestRealAdapterLocal", "TestRealAdapterLocal/selection")
		for vendor, model := range map[string]string{"claude": "opus[1m]", "codex": "gpt-5-codex"} {
			rc := contract.RoleConfig{ID: "sel-" + vendor, Name: "sel", Node: r.a.NodeID, Adapter: vendor, Instruction: r.ins, Runbook: r.run,
				Model: model, Effort: "low", Concurrency: 1}
			_, err := r.addRole(ctx, rc)
			var ce *contract.Error
			q := qualification(vendor)
			if !errors.As(err, &ce) || ce.Code != contract.CodeInvalidArgument || ce.Details["field"] != "model" || ce.Details["reason"] != contract.ReasonProbeFailed ||
				!strings.Contains(ce.Message, "the "+vendor+" model/effort selection is not qualified; supported: model "+q.Model+", effort low") ||
				strings.Contains(ce.Message, model) {
				t.Fatalf("%s %s: %v", vendor, model, err)
			}
		}
		// An unqualified per-task override is refused by the worker
		// (start_failed) before any launch.
		goal := r.goal("override")
		writeScenario(t, r.kit, captureScenario(t, "claude", goal, "runs-scratch/claude-stdin-success"))
		req := requestFor("rc", goal)
		m := "claude-sonnet-5-5"
		req.Override = &contract.TaskOverride{Model: &m}
		f := r.dispatch(t, req)
		if f.view.State == contract.TaskSucceeded || f.view.Reason == nil || f.view.Reason.Code != contract.ReasonStartFailed {
			t.Fatalf("override %s", f)
		}
		if ls := launches(t, r.kit, func(l launch) bool { return l.TaskID == f.view.TaskID }); len(ls) != 0 {
			t.Fatalf("an unqualified override launched %+v", ls)
		}
	})
}

// FP-2: the bounded --version probe of the explicit executable accepts
// the captured versions and refuses wrong versions, wrong vendors and
// relative paths without any task call.
func TestRealAdapterProbe(t *testing.T) {
	t.Parallel()
	r := sharedRig(t)
	ctx := context.Background()
	stateA := r.a.State
	versions, err := hostVersions(testkit.MustRepoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, vendor := range []string{"claude", "codex"} {
		t.Run(vendor, func(t *testing.T) {
			exe := map[string]string{"claude": r.kit.claude, "codex": r.kit.codex}[vendor]
			// Worker A's probes of this vendor ran "exe --version" in its
			// state root; every probe of the path (any worker) is a version
			// call, never a task.
			real, _ := filepath.EvalSymlinks(stateA)
			ls := launches(t, r.kit, func(l launch) bool {
				return l.Argv0 == exe && (l.Cwd == stateA || l.Cwd == real) && l.Mode == "version"
			})
			if len(ls) == 0 {
				t.Fatalf("no %s version probe recorded in worker A's state root", vendor)
			}
			for _, l := range launches(t, r.kit, func(l launch) bool { return l.Argv0 == exe && l.Mode == "version" }) {
				if !l.OK || !slices.Equal(l.Argv, []string{"--version"}) || l.Vendor != vendor || l.TaskID != "" {
					t.Fatalf("%s probe %+v", vendor, l)
				}
			}
			if versions[vendor] != qualification(vendor).Version {
				t.Fatalf("captured %s version %q is not the qualified %q", vendor, versions[vendor], qualification(vendor).Version)
			}
		})
	}
	t.Run("refusal", func(t *testing.T) {
		delegate(t, contractBinary(t, "./internal/adapter"), "./internal/adapter", "^TestVendorProbe$", "TestVendorProbe")
		// A second kit: claude reports another version, codex claude's.
		k, err := newReplayKit(t.TempDir(), replayBinary(t), map[string]string{"claude": "2.1.284 (Claude Code)", "codex": versions["claude"]})
		if err != nil {
			t.Fatal(err)
		}
		c, err := r.dep.StartSidecar(ctx, r.unique("worker-refusal"), r.sidecarEnv(k), "--claude-adapter", k.claude, "--codex-adapter", k.codex)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Stop() })
		for vendor, want := range map[string]string{
			"claude": "has an unqualified claude version; expected 2.1.285 (Claude Code)",
			"codex":  "is not the codex CLI (unexpected version output)",
		} {
			q := qualification(vendor)
			_, err := r.addRole(ctx, contract.RoleConfig{ID: "probe-" + vendor, Name: "probe", Node: c.NodeID, Adapter: vendor, Instruction: r.ins,
				Runbook: r.run, Model: q.Model, Effort: q.Effort, Concurrency: 1})
			var ce *contract.Error
			if !errors.As(err, &ce) || ce.Details["reason"] != contract.ReasonProbeFailed || !strings.Contains(ce.Message, want) {
				t.Fatalf("%s: %v", vendor, err)
			}
		}
		// Only version probes ran: no task call.
		if ls := launches(t, k, func(l launch) bool { return l.Mode != "version" || !l.OK }); len(ls) != 0 {
			t.Fatalf("refused executables were called for tasks: %+v", ls)
		}
		if len(launches(t, k, func(l launch) bool { return true })) == 0 {
			t.Fatal("the refused executables were never probed")
		}
		// A relative path is refused before any probe or PATH lookup.
		p := newNodeCLI(t)
		res := runBin(t, p.bin, p.cwd, append(slices.Clone(r.env), replayDirEnv+"="+k.dir), "sidecar", "run", "--state-dir", t.TempDir(), "--claude-adapter", "claude")
		if res.code != 2 || !strings.Contains(res.stderr, "--claude-adapter must be an absolute path") {
			t.Fatalf("relative = %+v", res)
		}
		if b, err := os.ReadFile(k.trapLog); err == nil && len(b) > 0 {
			t.Fatalf("PATH traps were called: %s", b)
		}
	})
}

// FP-3: the stub records the exact argv, the complete composed stdin, a
// private non-git cwd and the explicit qualified pair, with no prohibited
// flag.
func TestRealAdapterInvocation(t *testing.T) {
	t.Parallel()
	r := sharedRig(t)
	forbidden := []string{"--dangerously-skip-permissions", "--dangerously-bypass-approvals-and-sandbox", "--sandbox", "--ignore-user-config", "--resume",
		"--continue", "--permission-mode", "--approve-for-me", "--full-auto", "--yolo"}
	check := func(t *testing.T, vendor, roleID, goal string) {
		t.Helper()
		sc := captureScenario(t, vendor, goal, map[string]string{"claude": "runs-scratch/claude-stdin-success", "codex": "runs-scratch/codex-skip-success"}[vendor])
		req := requestFor(roleID, goal)
		stdin := r.composed(t, req, roleID, vendor)
		sc.ExpectedStdin = &stdin
		writeScenario(t, r.kit, sc)
		f := r.dispatch(t, req)
		if f.view.State != contract.TaskSucceeded {
			t.Fatalf("%s task %s", vendor, f)
		}
		l := taskLaunch(t, r.kit, f.view.TaskID)
		want := productionArgv(vendor)
		if len(l.Argv) != len(want) {
			t.Fatalf("argv %q", l.Argv)
		}
		for i, a := range want {
			if a == "{final}" {
				want[i] = filepath.Join(l.Cwd, adapter.CodexFinalName)
			}
		}
		if !slices.Equal(l.Argv, want) || l.Argv0 != map[string]string{"claude": r.kit.claude, "codex": r.kit.codex}[vendor] {
			t.Fatalf("%s argv %q (argv0 %s), want %q", vendor, l.Argv, l.Argv0, want)
		}
		q := qualification(vendor)
		if !slices.Contains(l.Argv, q.Model) || (vendor == "claude" && !slices.Contains(l.Argv, "low")) ||
			(vendor == "codex" && !slices.Contains(l.Argv, `model_reasoning_effort="low"`)) {
			t.Fatalf("%s argv lacks the explicit pair: %q", vendor, l.Argv)
		}
		for _, a := range l.Argv {
			if slices.Contains(forbidden, a) || strings.Contains(a, goal) {
				t.Fatalf("%s argv carries %q", vendor, a)
			}
		}
		if got, wantIn := string(l.Stdin), strings.ReplaceAll(stdin, "{task_id}", f.view.TaskID); got != wantIn {
			t.Fatalf("%s stdin %q, want %q", vendor, got, wantIn)
		}
		if l.CwdMode != "0700" || filepath.Base(l.Cwd) != "work" || filepath.Base(filepath.Dir(l.Cwd)) != f.view.TaskID {
			t.Fatalf("%s cwd %s %s", vendor, l.Cwd, l.CwdMode)
		}
	}
	t.Run("claude", func(t *testing.T) { check(t, "claude", "rc", r.goal("invocation claude")) })
	t.Run("codex", func(t *testing.T) { check(t, "codex", "rx", r.goal("invocation codex")) })
	t.Run("stdin", func(t *testing.T) {
		delegate(t, contractBinary(t, "./internal/adapter"), "./internal/adapter", "^TestVendorInvocation$", "TestVendorInvocation")
		// Arbitrary goal bytes within the contract reach stdin exactly (no
		// shell, no argv copy, no added newline).
		check(t, "claude", "rc", r.goal("stdin: \"quoted\" 'single' $(echo no) `tick` \\ back\nline two\ttab <html>&amp; é 😀  "))
		check(t, "codex", "rx", r.goal("stdin codex: -- --model other; rm -rf / \x7f"))
	})
}

// FP-4: Claude's captured success and failure results and exits survive
// exactly; malformed JSON gives a null answer and the fixed diagnostic.
func TestRealAdapterClaudeFinal(t *testing.T) {
	t.Parallel()
	r := sharedRig(t)
	t.Run("success", func(t *testing.T) {
		f := r.replay(t, "rc", captureScenario(t, "claude", r.goal("claude success"), "runs-scratch/claude-stdin-success"))
		if f.view.State != contract.TaskSucceeded || exitOf(f) != 0 || finalOf(f) != "pong" ||
			!bytes.Equal(f.logs, captureFile(t, "runs-scratch/claude-stdin-success/stdout.bin")) {
			t.Fatalf("success %s", f)
		}
	})
	t.Run("failure", func(t *testing.T) {
		f := r.replay(t, "rc", captureScenario(t, "claude", r.goal("claude failure"), "runs-scratch/claude-stdin-fail"))
		var doc struct{ Result string }
		json.Unmarshal(captureFile(t, "runs-scratch/claude-stdin-fail/stdout.bin"), &doc)
		if f.view.State != contract.TaskFailed || exitOf(f) != 1 || finalOf(f) != doc.Result || doc.Result == "" ||
			!interleaves(f.logs, captureFile(t, "runs-scratch/claude-stdin-fail/stdout.bin"), captureFile(t, "runs-scratch/claude-stdin-fail/stderr.bin")) {
			t.Fatalf("failure %s", f)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		delegate(t, contractBinary(t, "./internal/adapter"), "./internal/adapter", "^TestClaudeFinal$", "TestClaudeFinal")
		out, zero := "not json {\n", 0
		f := r.replay(t, "rc", scenario{Goal: r.goal("claude malformed"), Vendor: "claude", ExpectedArgv: productionArgv("claude"), Synthetic: true,
			Stdout: &out, Exit: &zero, Normalization: "synthetic malformed stdout (robustness, not qualification evidence)"})
		if f.view.State != contract.TaskSucceeded || exitOf(f) != 0 || finalOf(f) != "<null>" ||
			string(f.logs) != out+diagnosticLine("claude", adapter.FinalInvalid) || f.view.Result.FinalMessageTruncated {
			t.Fatalf("malformed %s", f)
		}
	})
}

// FP-5: Codex's exact final file survives scratch cleanup; missing or
// unsafe files never become empty answers; two tasks never exchange files.
func TestRealAdapterCodexFinal(t *testing.T) {
	t.Parallel()
	r := sharedRig(t)
	t.Run("success", func(t *testing.T) {
		f := r.replay(t, "rx", captureScenario(t, "codex", r.goal("codex success"), "runs-scratch/codex-skip-success"))
		l := taskLaunch(t, r.kit, f.view.TaskID)
		if f.view.State != contract.TaskSucceeded || exitOf(f) != 0 || finalOf(f) != "pong" || len(finalOf(f)) != 4 ||
			!bytes.Equal(f.logs, captureFile(t, "runs-scratch/codex-skip-success/stdout.bin")) {
			t.Fatalf("success %s", f)
		}
		if _, err := os.Stat(l.Cwd); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("scratch %s kept after cleanup: %v", l.Cwd, err)
		}
	})
	t.Run("absent", func(t *testing.T) {
		f := r.replay(t, "rx", captureScenario(t, "codex", r.goal("codex absent"), "runs-scratch/codex-skip-fail"))
		want := append(captureFile(t, "runs-scratch/codex-skip-fail/stdout.bin"), diagnosticLine("codex", adapter.FinalMissing)...)
		if f.view.State != contract.TaskFailed || exitOf(f) != 1 || f.view.Result.FinalMessage != nil || !bytes.Equal(f.logs, want) {
			t.Fatalf("absent %s", f)
		}
	})
	t.Run("unsafe", func(t *testing.T) {
		delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^file$", "TestRealAdapterLocal", "TestRealAdapterLocal/file")
		for mode, code := range map[string]string{"symlink": adapter.FinalUnsafe, "fifo": adapter.FinalUnsafe, "directory": adapter.FinalUnsafe,
			"invalid-utf8": adapter.FinalInvalid} {
			sc := captureScenario(t, "codex", r.goal("codex unsafe "+mode), "runs-scratch/codex-skip-success")
			sc.FinalMode, sc.Normalization = mode, "synthetic final-file fault "+mode+" (robustness, not qualification evidence)"
			f := r.replay(t, "rx", sc)
			want := append(captureFile(t, "runs-scratch/codex-skip-success/stdout.bin"), diagnosticLine("codex", code)...)
			if f.view.State != contract.TaskSucceeded || f.view.Result.FinalMessage != nil || !bytes.Equal(f.logs, want) {
				t.Fatalf("%s %s", mode, f)
			}
		}
	})
	t.Run("cleanup", func(t *testing.T) {
		delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^ordering$", "TestRealAdapterLocal", "TestRealAdapterLocal/ordering")
		// Both are dispatched before either is awaited (a dispatch returns at
		// admission), so their executions overlap on worker A.
		ctx, cancel := context.WithTimeout(context.Background(), rigWait)
		defer cancel()
		answers := []string{"first answer\n", "second answer ✓"}
		var ids []string
		for i := range answers {
			sc := captureScenario(t, "codex", r.goal("codex isolation"), "runs-scratch/codex-skip-success")
			sc.FinalMode, sc.Final, sc.Normalization = "custom", answers[i], "synthetic distinct final bytes per task (isolation robustness)"
			writeScenario(t, r.kit, sc)
			v, err := r.send(ctx, requestFor("rx", sc.Goal))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, v.TaskID)
		}
		for i, id := range ids {
			f := r.await(t, ctx, id)
			l := taskLaunch(t, r.kit, f.view.TaskID)
			if finalOf(f) != answers[i] || l.Final != filepath.Join(l.Cwd, adapter.CodexFinalName) {
				t.Fatalf("task %d final %q (file %s)", i, finalOf(f), l.Final)
			}
			if _, err := os.Stat(l.Cwd); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch %s kept: %v", l.Cwd, err)
			}
		}
	})
}

// FP-6: observed exits, signals and controls decide outcomes; denial prose
// and hook warnings do not; replayed bytes reach the logs unchanged.
func TestRealAdapterOutcomes(t *testing.T) {
	t.Parallel()
	r := sharedRig(t)
	t.Run("exits", func(t *testing.T) {
		for _, c := range []struct{ vendor, role, rel string }{{"claude", "rc", "runs-scratch/claude-stdin-fail"}, {"codex", "rx", "runs-scratch/codex-skip-fail"}} {
			f := r.replay(t, c.role, captureScenario(t, c.vendor, r.goal("exit "+c.vendor), c.rel))
			if f.view.State != contract.TaskFailed || exitOf(f) != 1 || f.view.Result.Signal != nil {
				t.Fatalf("%s failure %+v", c.vendor, f.view.Result)
			}
		}
	})
	t.Run("denials", func(t *testing.T) {
		var doc struct{ Result string }
		json.Unmarshal(captureFile(t, "runs/claude-forbidden-home/stdout.bin"), &doc)
		for _, c := range []struct{ vendor, role, rel, want string }{
			{"claude", "rc", "runs/claude-forbidden-home", doc.Result},
			{"codex", "rx", "runs-scratch/codex-skip-forbidden-home", string(captureFile(t, "runs-scratch/codex-skip-forbidden-home/final.saved.txt"))},
		} {
			f := r.replay(t, c.role, captureScenario(t, c.vendor, r.goal("denial "+c.vendor), c.rel))
			if f.view.State != contract.TaskSucceeded || exitOf(f) != 0 || finalOf(f) != c.want || !strings.Contains(c.want, "Read-only file system") {
				t.Fatalf("%s denial %s", c.vendor, f)
			}
		}
	})
	t.Run("controls", func(t *testing.T) {
		delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^restart$", "TestRealAdapterLocal", "TestRealAdapterLocal/restart")
		// A cancelled task keeps its control outcome; its safe final file,
		// present once the group is gone, is its partial answer. It runs on
		// worker H: its cleanup blocks that node's starts until the group is
		// gone (06b), never worker A's.
		sc := captureScenario(t, "codex", r.goal("codex hold"), "runs-scratch/codex-skip-success")
		sc.FinalMode, sc.Final, sc.Hold, sc.Normalization = "custom", "partial answer", true, "synthetic hold until cancelled (control robustness)"
		writeScenario(t, r.kit, sc)
		ctx, cancel := context.WithTimeout(context.Background(), rigWait)
		defer cancel()
		v, err := r.send(ctx, requestFor("rh", sc.Goal))
		if err != nil {
			t.Fatal(err)
		}
		for len(launches(t, r.kit, func(l launch) bool { return l.TaskID == v.TaskID })) == 0 {
			if ctx.Err() != nil {
				t.Fatal("the held task never launched")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := r.dep.Client.CancelTask(ctx, v.TaskID); err != nil {
			t.Fatal(err)
		}
		f := r.await(t, ctx, v.TaskID)
		if f.view.State != contract.TaskCancelled || f.view.Result == nil || f.view.Result.Signal == nil || *f.view.Result.Signal != "SIGTERM" ||
			finalOf(f) != "partial answer" {
			t.Fatalf("cancelled %s", f)
		}
	})
	t.Run("replay", func(t *testing.T) {
		// Captured bytes replay unchanged per stream; the success capture's
		// item.type=error hook warnings do not fail it.
		for _, c := range []struct{ vendor, role, rel, final string }{
			{"claude", "rc", "runs-scratch/claude-stdin-success", "pong"},
			{"codex", "rx", "runs-scratch/codex-skip-success", "pong"},
			{"codex", "rx", "runs-scratch/codex-skip-forbidden-tmp", string(captureFile(t, "runs-scratch/codex-skip-forbidden-tmp/final.saved.txt"))},
		} {
			f := r.replay(t, c.role, captureScenario(t, c.vendor, r.goal("replay "+c.rel), c.rel))
			if f.view.State != contract.TaskSucceeded || finalOf(f) != c.final ||
				!interleaves(f.logs, captureFile(t, c.rel+"/stdout.bin"), captureFile(t, c.rel+"/stderr.bin")) {
				t.Fatalf("%s %s", c.rel, f)
			}
		}
		if !bytes.Contains(captureFile(t, "runs-scratch/codex-skip-success/stdout.bin"), []byte(`"type":"error"`)) {
			t.Fatal("the success capture carries no hook warning")
		}
		delegate(t, taggedSidecarBinary(t), "./internal/sidecar", "^TestRealAdapterLocal$/^diagnostic$", "TestRealAdapterLocal", "TestRealAdapterLocal/diagnostic")
	})
}

// timeoutFactsSHA256 pins the six Claude/Codex coordinator timeout facts
// (iteration 07b's) exactly as they were before iteration 08.
const timeoutFactsSHA256 = "5ab2e59f99171699dbcccdec739cfa3b9053be832264de9cb81271cb987a3558"

// FP-7: the catalog's recipes agree with the invocations and captures, its
// evidence resolves to the checked-in byte-identical copies, and the
// ownership, macOS and timeout facts keep their rules.
func TestRealAdapterCatalog(t *testing.T) {
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
		for vendor, capture := range map[string]string{"claude": "runs-scratch/claude-stdin-success", "codex": "runs-scratch/codex-skip-success"} {
			// The catalog's backticked recipe, tokenized, is exactly the
			// executable and the adapter's argv (the final path the only
			// variable); so is the capture's own argv.
			value := byID[vendor].Facts["headless"].Value
			start := strings.Index(value, "`"+vendor+" ")
			end := strings.Index(value[start+1:], "`")
			recipe := strings.Fields(strings.NewReplacer(`\"`, `"`).Replace(value[start+1 : start+1+end]))
			for i, tok := range recipe {
				if strings.HasPrefix(tok, `model_reasoning_effort=`) {
					recipe[i] = strings.Trim(tok, "'")
				}
			}
			want := append([]string{vendor}, productionArgv(vendor)...)
			norm := func(argv []string) []string {
				out := slices.Clone(argv)
				for i := range out {
					if i > 0 && out[i-1] == "--output-last-message" {
						out[i] = "{final}"
					}
				}
				return out
			}
			if got := norm(recipe); !slices.Equal(got, want) {
				t.Fatalf("%s catalog recipe %q, want %q", vendor, got, want)
			}
			if got := norm(tokens(string(captureFile(t, capture+"/argv.txt")))); !slices.Equal(got, want) {
				t.Fatalf("%s captured argv %q, want %q", vendor, got, want)
			}
			if strings.TrimSpace(byID[vendor].Version) != qualification(vendor).Version {
				t.Fatalf("%s catalog version %q", vendor, byID[vendor].Version)
			}
		}
		// The human-readable recipes (shell-quoted for Codex).
		for _, recipe := range []string{
			"Qualified worker recipe (Linux): `claude -p --model sonnet --effort low --permission-prompts none --output-format json`",
			"Qualified worker recipe (Linux): `codex -a never exec --model gpt-6.1-sol -c 'model_reasoning_effort=\"low\"' --json --skip-git-repo-check --output-last-message <scratch>/callsheet-final.txt -`",
		} {
			if !strings.Contains(doc, recipe) {
				t.Fatalf("docs/support-catalog.md lacks %q", recipe)
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
		}
		if err := json.Unmarshal(repoFile(t, capturesRel+"/manifest.json"), &m); err != nil {
			t.Fatal(err)
		}
		listed := map[string]bool{}
		for _, f := range m.Files {
			b := repoFile(t, f.Destination)
			sum := sha256.Sum256(b)
			if f.Destination != capturesRel+"/"+f.Source || len(b) != f.Bytes || hex.EncodeToString(sum[:]) != f.SHA256 {
				t.Fatalf("manifest entry %+v does not match its copy (%d bytes)", f, len(b))
			}
			listed[f.Destination] = true
		}
		for _, a := range m.Absent {
			if _, err := os.Lstat(filepath.Join(root, capturesRel, a)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s is listed absent but exists", a)
			}
		}
		if len(m.Files) < 200 || !strings.Contains(m.Provenance, "not a new run") {
			t.Fatalf("manifest: %d files, provenance %q", len(m.Files), m.Provenance)
		}
		// No credential-bearing snapshot was copied.
		filepath.WalkDir(filepath.Join(root, capturesRel), func(p string, d os.DirEntry, err error) error {
			rel, _ := filepath.Rel(root, p)
			if !d.IsDir() && filepath.Base(p) != "manifest.json" && !listed[filepath.ToSlash(rel)] {
				t.Errorf("unlisted evidence file %s", rel)
			}
			return nil
		})
		versions, _ := hostVersions(root)
		for _, vendor := range []string{"claude", "codex"} {
			e := byID[vendor]
			if versions[vendor] != e.Version {
				t.Fatalf("%s version %q is not the host capture's %q", vendor, e.Version, versions[vendor])
			}
			for key, f := range e.Facts {
				for _, ev := range f.Evidence {
					if strings.HasPrefix(ev, capturesRel+"/") && !listed[ev] {
						t.Fatalf("%s.%s evidence %s is not a manifest copy", vendor, key, ev)
					}
				}
				switch {
				case f.Status == catalog.Verified && (strings.Contains(strings.ToLower(f.Value), "unknown") || strings.Contains(strings.ToLower(f.Value), "unverified")):
					t.Fatalf("%s.%s VERIFIED value mentions an unknown: %s", vendor, key, f.Value)
				case f.Status == catalog.Unverified && f.VerificationIteration == "08" &&
					(!strings.Contains(f.Value, "iteration 08") || !strings.Contains(f.Value, "did not measure")):
					t.Fatalf("%s.%s UNVERIFIED value does not say iteration 08 did not measure it: %s", vendor, key, f.Value)
				}
			}
			if f := e.Facts["sandbox_macos"]; f.Status != catalog.Unverified {
				t.Fatalf("%s sandbox_macos promoted: %+v", vendor, f)
			}
			if f := e.Facts["exit_codes"]; f.Status != catalog.Unverified {
				t.Fatalf("%s exit mapping promoted: %+v", vendor, f)
			}
			for _, key := range []string{"headless", "model", "effort", "approval", "sandbox_linux", "final_message"} {
				if e.Facts[key].Status != catalog.Verified {
					t.Fatalf("%s.%s is %s", vendor, key, e.Facts[key].Status)
				}
			}
		}
		// The 07b timeout facts are unchanged, byte for byte.
		var timeouts []catalog.Fact
		for _, vendor := range []string{"claude", "codex"} {
			for _, key := range []string{"mcp_timeout", "mcp_timeout_override", "mcp_progress_extension"} {
				timeouts = append(timeouts, byID[vendor].Facts[key])
			}
		}
		b, _ := json.Marshal(timeouts)
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != timeoutFactsSHA256 {
			t.Fatalf("the Claude/Codex timeout facts changed: %s", hex.EncodeToString(sum[:]))
		}
		// The operator guide describes the manual remote procedure and does
		// not claim M3 has happened.
		ra := string(repoFile(t, "docs/real-adapters.md"))
		requireTerms(t, "docs/real-adapters.md", ra, "## Remote acceptance (M3)", "M3 has not been demonstrated", "task_wait", "task_show", "task_logs",
			"unqualified-selection refusal", workersmoke.Command, "--claude-adapter", "--codex-adapter")
		for _, claim := range []string{"M3 was demonstrated", "M3 is demonstrated", "M3 has been demonstrated", "M3 demonstrated on", "M3 passed"} {
			if strings.Contains(ra, claim) {
				t.Fatalf("docs/real-adapters.md claims %q", claim)
			}
		}
		if err := catalog.CheckDocLinks(filepath.Join(root, "docs", "real-adapters.md")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ownership", func(t *testing.T) {
		if err := catalog.Validate(root, entries); err != nil {
			t.Fatal(err)
		}
		for _, vendor := range []string{"claude", "codex"} {
			for _, key := range catalog.RequiredFacts {
				want := "08"
				if strings.HasPrefix(key, "mcp_timeout") || key == "mcp_progress_extension" {
					want = "07b"
				}
				if got := byID[vendor].Facts[key].VerificationIteration; got != want || catalog.Owner(vendor, key) != want {
					t.Fatalf("%s.%s owner %s, want %s", vendor, key, got, want)
				}
			}
		}
		for _, s := range []string{"`07b` for the three coordinator timeout facts", `<a id="interim-mcp-wait-exception"></a>`, "Coordinator stdio registration syntax is VERIFIED",
			"Coordinator registration syntax is VERIFIED: `codex mcp add callsheet"} {
			if !strings.Contains(doc, s) {
				t.Fatalf("docs/support-catalog.md lost %q", s)
			}
		}
	})
}

// FP-8: real stdio MCP dispatch through a real plane to an enrolled
// sidecar running the replay stubs, observed through task_wait, task_show
// and task_logs; the PATH traps prove no installed vendor was used.
func TestRealAdapterDispatch(t *testing.T) {
	t.Parallel()
	r := sharedRig(t)
	m := startMCP(t, "--plane", r.dep.URL, "--ca", r.dep.CA)
	run := func(t *testing.T, vendor, roleID, rel string) {
		t.Helper()
		sc := captureScenario(t, vendor, r.goal("mcp "+vendor), rel)
		writeScenario(t, r.kit, sc)
		// A parallel parent's dispatch or role change may hold the plane's
		// admission: the documented refusal changes nothing and is retried.
		var id string
		deadline := time.Now().Add(rigWait)
		for {
			res := m.call("dispatch", dispatchArgs("id", roleID, sc.Goal, nil))
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
			t.Fatalf("%s task_show %+v %v", vendor, v, err)
		}
		lr, err := contract.ParseTaskLogsResponse([]byte(m.ok("task_logs", map[string]any{"id": id})))
		if err != nil || !bytes.Equal(lr.Data, captureFile(t, rel+"/stdout.bin")) {
			t.Fatalf("%s task_logs %q %v", vendor, lr.Data, err)
		}
		l := taskLaunch(t, r.kit, id)
		if l.Argv0 != map[string]string{"claude": r.kit.claude, "codex": r.kit.codex}[vendor] || !strings.Contains(string(l.Stdin), `"goal":"`+sc.Goal+`"`) {
			t.Fatalf("%s launch %+v", vendor, l)
		}
	}
	t.Run("claude", func(t *testing.T) { run(t, "claude", "rc", "runs-scratch/claude-stdin-success") })
	t.Run("codex", func(t *testing.T) { run(t, "codex", "rx", "runs-scratch/codex-skip-success") })
	t.Run("no-vendors", func(t *testing.T) {
		// Every launch came from the explicit replay paths; the PATH traps
		// (the sidecar's whole PATH) were never called, although a PATH
		// lookup would have reached them.
		for _, l := range launches(t, r.kit, func(launch) bool { return true }) {
			if l.Argv0 != r.kit.claude && l.Argv0 != r.kit.codex {
				t.Fatalf("a launch outside the explicit paths: %+v", l)
			}
		}
		if b, err := os.ReadFile(r.kit.trapLog); err == nil && len(b) > 0 {
			t.Fatalf("PATH traps were called:\n%s", b)
		}
		k, err := newReplayKit(t.TempDir(), replayBinary(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		runTrap(t, k)
		if strings.Contains(strings.Join(r.env, " "), "/usr") || !strings.HasPrefix(r.env[0], "PATH="+r.kit.traps) {
			t.Fatalf("the sidecar environment %q reaches installed binaries", r.env)
		}
	})
}

// FP-9: the smoke gate decides every case without calling t.Skip; the
// stub-backed harness runs the real deployment path; the real smoke's
// files are all excluded without the realadaptersmoke tag.
func TestRealAdapterSmokeGate(t *testing.T) {
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
		for _, env := range []map[string]string{{}, {workersmoke.EnvOptIn: "true"}, {workersmoke.EnvOptIn: "0"}, {"CALLSHEET_CLAUDE_PATH": "/x"}} {
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
		if d := gate(map[string]string{workersmoke.EnvOptIn: "1", "CI": "true"}, nil).Smoke(); d.Run || !strings.Contains(d.Reason, "CI") {
			t.Fatalf("ci: %+v", d)
		}
		if statted != 0 {
			t.Fatal("the CI decision touched the filesystem")
		}
	})
	t.Run("absent", func(t *testing.T) {
		env := map[string]string{workersmoke.EnvOptIn: "1", "CALLSHEET_CODEX_PATH": "/nonexistent/codex"}
		g := gate(env, map[string]os.FileMode{"/opt/noexec/claude": 0o644, "/opt/dir": os.ModeDir | 0o755})
		if d := g.Smoke(); !d.Run {
			t.Fatalf("opted in: %+v", d)
		}
		if d := g.Vendor("claude"); d.Run || d.Fail != "" || !strings.Contains(d.Skip, "unset") {
			t.Fatalf("unset claude: %+v", d)
		}
		if d := g.Vendor("codex"); d.Run || d.Fail != "" || !strings.Contains(d.Skip, "absent") {
			t.Fatalf("absent codex: %+v", d)
		}
		for path, want := range map[string]string{"/opt/noexec/claude": "no executable permission bit", "/opt/dir": "not a regular file", "claude": "absolute path"} {
			env["CALLSHEET_CLAUDE_PATH"] = path
			if d := g.Vendor("claude"); d.Run || d.Skip != "" || !strings.Contains(d.Fail, want) {
				t.Fatalf("%s: %+v", path, d)
			}
		}
		if d := g.Vendor("unknown-vendor"); d.Fail == "" || !strings.Contains(d.Fail, "unknown vendor") {
			t.Fatalf("an unknown vendor: %+v", d)
		}
	})
	t.Run("enabled", func(t *testing.T) {
		stub := replayBinary(t)
		root := t.TempDir()
		versions, _ := hostVersions(testkit.MustRepoRoot(t))
		k, err := newReplayKit(root, stub, versions)
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{workersmoke.EnvOptIn: "1", "CALLSHEET_CLAUDE_PATH": k.claude, "CALLSHEET_CODEX_PATH": k.codex}
		g := workersmoke.Gate{Getenv: func(key string) string { return env[key] }, Stat: os.Stat}
		if d := g.Smoke(); !d.Run {
			t.Fatalf("enabled smoke: %+v", d)
		}
		goal := strings.TrimSpace(string(captureFile(t, "runs-scratch/claude-stdin-success/stdin.txt")))
		if goal != strings.TrimSpace(string(captureFile(t, "runs-scratch/codex-skip-success/stdin.txt"))) {
			t.Fatal("the two captures' prompts differ")
		}
		for vendor, rel := range map[string]string{"claude": "runs-scratch/claude-stdin-success", "codex": "runs-scratch/codex-skip-success"} {
			d := g.Vendor(vendor)
			if !d.Run || d.Path != env[workersmoke.PathEnv(vendor)] {
				t.Fatalf("%s: %+v", vendor, d)
			}
			writeScenario(t, k, captureScenario(t, vendor, goal+" ("+vendor+")", rel))
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
				}
			}()
		}
		wg.Wait()
		// The real smoke exists only with its tag: every Go file in
		// tests/smoke carries the constraint, which the ordinary and the
		// tagged native builds never satisfy.
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
		if !strings.Contains(workersmoke.Command, "-tags="+workersmoke.Tag+" ./tests/smoke -run '^TestRealWorkerSmoke$'") ||
			!strings.Contains(string(repoFile(t, "docs/real-adapters.md")), workersmoke.Command) {
			t.Fatal("the documented smoke command does not enable its tag")
		}
		if b, err := os.ReadFile(k.trapLog); err == nil && len(b) > 0 {
			t.Fatalf("PATH traps were called: %s", b)
		}
	})
}

// fakeInfo is a stat result of a given mode.
type fakeInfo struct{ mode os.FileMode }

func (f fakeInfo) Name() string       { return "fake" }
func (f fakeInfo) Size() int64        { return 1 }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }
