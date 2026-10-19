package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Benchmarks of design 12a-dev-flow-verify: the offline check of the
// bounded histories (valid and rejected), the audit's rows and rendering,
// the reviewer evidence over the largest retained log and receipt, and the
// oracle's pure recomputation. Each iteration asserts its invariants; none
// starts a process.
//
// The fourteen-task and four-phase ceilings are structural: under the
// 2/2/2 counters (at most three coders, every coder after the first
// reserving a code cycle) no completing history reaches both. The bounded
// histories are the largest each way: both loops twice (twelve tasks, two
// phases) and three owner approvals after questions and an escalation (ten
// tasks, four phases, three receipts, both recovery dispatches).

// boundedHistory is one cached bounded-history bundle.
type boundedHistory struct {
	name   string
	steps  []devAct
	owner  []string
	tasks  int
	phases int
	once   sync.Once
	dir    string
	err    error
}

var (
	maxTasksHistory = &boundedHistory{name: "max-tasks", tasks: 12, phases: 2, owner: []string{"yes 3"}, steps: []devAct{
		{marker: markerDesignDraft}, {marker: markerDesignChanges}, {marker: markerDesignDraft}, {marker: markerDesignChanges},
		{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
		{marker: markerFixesComplete}, {marker: markerReviewChanges}, {marker: markerFixesComplete}, {marker: markerReviewApproved}}}
	maxPhasesHistory = &boundedHistory{name: "max-phases", tasks: 10, phases: maxPhases, owner: []string{"yes 1", "yes 2", "yes 3"}, steps: []devAct{
		{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerQuestions}, {marker: markerDesignDraft},
		{marker: markerDesignApproved}, {marker: markerNeedsDesign}, {marker: markerDesignDraft}, {marker: markerDesignApproved},
		{marker: markerImplComplete}, {marker: markerReviewApproved}}}
)

// bundle returns a fresh copy of the history's checked bundle.
func (h *boundedHistory) bundle(tb testing.TB) string {
	tb.Helper()
	h.once.Do(func() {
		r := newDevRig(tb, h.steps)
		r.owner = h.owner
		if code := r.run(); code != 0 {
			h.err = fmt.Errorf("the %s history failed (%d): %s %s", h.name, code, r.d.stopCode, r.d.stopReason)
			return
		}
		h.dir = filepath.Join(bundleCacheRoot, "dev-"+h.name)
		h.err = copyTree(r.f.bundle(), h.dir)
	})
	if h.err != nil {
		tb.Fatal(h.err)
	}
	dst := filepath.Join(tb.TempDir(), h.name)
	if err := copyTree(h.dir, dst); err != nil {
		tb.Fatal(err)
	}
	return dst
}

// bundleBytes is a loaded bundle's text bytes.
func bundleBytes(b *DevBundle) int64 {
	var n int64
	for _, f := range b.Files {
		n += int64(len(f))
	}
	return n
}

func TestDevFullBoundedHistories(t *testing.T) {
	t.Parallel()
	// The benchmarks' bounded histories are themselves checked PASSes.
	for _, h := range []*boundedHistory{maxTasksHistory, maxPhasesHistory} {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			code, rep, errOut := devCheck(t, h.bundle(t))
			if code != 0 || rep.Tasks != h.tasks || rep.Phases != h.phases || rep.WorkerCalls != 5+h.tasks {
				t.Fatalf("exit %d tasks %d phases %d calls %d %v %s", code, rep.Tasks, rep.Phases, rep.WorkerCalls, devCodes(rep), errOut)
			}
		})
	}
	// (One more loop past either history stops at the cap: FP-2's
	// design_cap and code_cap.)
}

func BenchmarkDevFullBundleCheck(b *testing.B) {
	type variant struct {
		dir   string
		pass  bool
		tasks int
	}
	var variants []variant
	var total int64
	for _, h := range []*boundedHistory{maxTasksHistory, maxPhasesHistory} {
		valid, rejected := h.bundle(b), h.bundle(b)
		receipts, _ := filepath.Glob(filepath.Join(rejected, "receipts", "*.json"))
		if len(receipts) == 0 {
			b.Fatal("no receipt")
		}
		editJSON(b, rejected, "receipts/"+filepath.Base(receipts[0]), func(m map[string]any) {
			m["data"].(map[string]any)["design_sha256"] = sha256Hex([]byte("another design"))
		})
		reforge(b, rejected)
		vb, err := LoadDevBundle(valid)
		if err != nil {
			b.Fatal(err)
		}
		total += 2 * bundleBytes(vb)
		variants = append(variants, variant{valid, true, h.tasks}, variant{rejected, false, h.tasks})
	}
	b.SetBytes(total)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, c := range variants {
			bundle, err := LoadDevBundle(c.dir)
			if err != nil {
				b.Fatal(err)
			}
			res := checkDevBundle(bundle, false)
			if (res.report.Result == ResultPass) != c.pass || res.report.Tasks != c.tasks {
				b.Fatalf("%s: %s %v", c.dir, res.report.Result, devCodes(res.report))
			}
			if !c.pass && len(devCriterion(res.report, "owner_receipts").Codes) == 0 {
				b.Fatal("the counterfeit receipt was not rejected")
			}
		}
	}
}

func BenchmarkDevFullAudit(b *testing.B) {
	dir := maxTasksHistory.bundle(b)
	bundle, err := LoadDevBundle(dir)
	if err != nil {
		b.Fatal(err)
	}
	c := newDevChecker(bundle)
	c.run()
	r := c.buildReport()
	want := mustReadTB(b, filepath.Join(dir, "audit.md"))
	b.SetBytes(int64(len(want)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := c.buildAudit(r)
		data, err := encodeJSON(a)
		text := renderDevAudit(a)
		if err != nil || len(a.Rows) < 100 || len(a.JudgmentRows) != maxTasksHistory.tasks || len(data) == 0 || text != string(want) {
			b.Fatalf("audit rows %d judgments %d", len(a.Rows), len(a.JudgmentRows))
		}
		for _, j := range a.JudgmentRows {
			if j.Verdict != VerdictUnobserved {
				b.Fatal("a judgment row was decided")
			}
		}
	}
}

// mustReadTB reads a file for a benchmark.
func mustReadTB(tb testing.TB, p string) []byte { return mustRead(tb, p) }

func BenchmarkDevFullReviewEvidence(b *testing.B) {
	f := newReviewFix(b)
	// The largest receipt the helper can emit: both test commands' outputs
	// at the stream cap.
	inner := f.script.script
	f.launcher = &fakeLauncher{script: func(s ProcSpec) behavior {
		out := inner(s)
		if a := strings.Join(s.Args, " "); a == "test ./..." || strings.HasPrefix(a, "test -coverprofile=") {
			out.stdout = strings.Repeat("o", reviewStreamCap-1) + "\n"
		}
		return out
	}}
	out, _, code := verifyHelperRun(reviewEnv{cwd: f.co, clock: f.clock, launcher: f.launcher, path: "/bin"}, f.argv())
	if code != 0 {
		b.Fatalf("the helper failed: %s", out)
	}
	in := f.evidenceInput(strings.Join(f.argv(), " "), out, 0, markerReviewApproved)
	// The largest retained log: vendor chatter before the helper's records
	// up to the stored-log bound.
	pad := []byte(codexRecord(map[string]any{"type": "item.completed", "item": map[string]any{"id": "m", "type": "agent_message",
		"text": strings.Repeat("r", 4000)}}))
	var log bytes.Buffer
	for log.Len()+len(pad)+len(in.Log) <= maxStoredLog {
		log.Write(pad)
	}
	log.Write(in.Log)
	in.Log = log.Bytes()
	in.Meta = completeMeta(in.Log)
	b.SetBytes(int64(len(in.Log)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev := DeriveDevReviewEvidence(in)
		if ev.Status != evidenceVerified || ev.Coverage == nil || !ev.Coverage.Pass || ev.RunBytes > maxReviewRunBytes {
			b.Fatalf("%s/%s", ev.Status, ev.Code)
		}
	}
}

func BenchmarkDevFullOracleDerivation(b *testing.B) {
	dir := maxTasksHistory.bundle(b)
	bundle, err := LoadDevBundle(dir)
	if err != nil {
		b.Fatal(err)
	}
	c := newDevChecker(bundle)
	c.loadTrees()
	m := bundle.Manifest
	files := map[string][]byte{}
	for p, f := range c.trees[m.FinalCandidate] {
		files[p] = f.Content
	}
	seedMain := c.trees[m.Workspace.SeedCommit]["main.go"].Content
	var cases CasesRecord
	var rq RequirementsRecord
	json.Unmarshal(bundle.Files["validation/cases.json"], &cases)
	json.Unmarshal(bundle.Files["validation/requirements.json"], &rq)
	tests, profile := bundle.Files["validation/tests.jsonl"], bundle.Files["validation/coverage.out"]
	b.SetBytes(int64(len(files["main.go"]) + len(files["main_test.go"]) + len(tests) + len(profile)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := buildRequirements(files, tests, cases.Cases, rq.TestRun, m.FinalCandidate)
		if !r.RunSignature || !r.TableTests || !r.TestCallsRun || !r.TestRunPassed || len(r.MissingTests) != 0 || len(r.Rows) != len(FeatureCases) {
			b.Fatalf("requirements %+v", r)
		}
		for j, fc := range FeatureCases {
			cr := cases.Cases[j]
			if ok, _ := evaluateCase(fc, cr.Exec, cr.Stdout, cr.Stderr); !ok || cr.ID != fc.ID {
				b.Fatalf("case %s", fc.ID)
			}
		}
		cov, err := deriveChangedCoverage(seedMain, files["main.go"], profile, modulePathOf(files["go.mod"]))
		if err != nil || !cov.Pass {
			b.Fatalf("coverage %+v %v", cov, err)
		}
	}
}
