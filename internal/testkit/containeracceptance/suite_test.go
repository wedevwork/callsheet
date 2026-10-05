package containeracceptance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
)

// Short names for the decoded views the checks inspect.
type (
	contractView = contract.TaskView
	binding      = contract.WorkspaceBinding
	result       = contract.TaskResult
	wsResult     = contract.TaskWorkspaceResult
)

// The host unit tests never build or launch Callsheet, a sidecar or a
// worker and never run the acceptance scenarios for real (design
// m3-m4-container-e2e, Container execution: the acceptance run belongs to
// the tagged container package alone). The suite's command and process
// seams reach fakePlane (fixture_test.go) instead, and the real child
// process path (execCommand) runs this test binary as a helper command.

// helperEnv makes this test binary a helper command (TestMain).
const helperEnv = "CALLSHEET_CA_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(helperCommand(mode))
	}
	code := m.Run()
	if bundle.dir != "" {
		os.RemoveAll(bundle.dir)
	}
	os.Exit(code)
}

// helperCommand is the helper child: "exit=N" prints its arguments and a
// stderr line and exits N; "hang" blocks until killed.
func helperCommand(mode string) int {
	if mode == "hang" {
		time.Sleep(time.Minute)
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(mode, "exit="))
	wd, _ := os.Getwd()
	fmt.Printf("args=%s cwd=%s\n", strings.Join(os.Args[1:], ","), wd)
	fmt.Fprintln(os.Stderr, "helper stderr")
	return n
}

// bundle is the read-only fixture bundle shared by every test of this
// binary (removed by TestMain): plain synthetic executables (never run:
// fakePlane answers for them), the manuals and milestone docs copied from
// the working tree, and the expected hashes.
var bundle struct {
	once          sync.Once
	dir, cs, fake string
	fixtures      string
	hashes        map[string]string
	err           error
}

func makeBundle() error {
	dir, err := os.MkdirTemp("", "cs-ca-bundle-")
	if err != nil {
		return err
	}
	bundle.dir = dir
	repo, err := testkit.RepoRoot()
	if err != nil {
		return err
	}
	bin := filepath.Join(dir, "bin")
	bundle.fixtures = filepath.Join(dir, "fixtures")
	for _, d := range []string{bin, filepath.Join(bundle.fixtures, "docs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	bundle.cs, bundle.fake = filepath.Join(bin, "callsheet"), filepath.Join(bin, "fake-adapter")
	for p, body := range map[string]string{bundle.cs: "synthetic callsheet\n", bundle.fake: "synthetic fake adapter\n"} {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			return err
		}
	}
	if err := testkit.CopyTree(filepath.Join(bundle.fixtures, "manuals"), filepath.Join(repo, "tests", "container", "fixtures", "manuals")); err != nil {
		return err
	}
	for _, d := range claimDocs {
		b, err := os.ReadFile(filepath.Join(repo, "docs", d))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(bundle.fixtures, "docs", d), b, 0o644); err != nil {
			return err
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	bundle.hashes = map[string]string{}
	for k, p := range map[string]string{HashCallsheet: bundle.cs, HashFakeAdapter: bundle.fake, HashAcceptanceTest: self} {
		if bundle.hashes[k], err = fileSHA256(p); err != nil {
			return err
		}
	}
	return nil
}

// fixtureConfig returns a complete configuration over the shared bundle
// with a fresh temporary work root, and the fixture that answers for it.
func fixtureConfig(t *testing.T) (Config, *fakePlane) {
	t.Helper()
	bundle.once.Do(func() { bundle.err = makeBundle() })
	if bundle.err != nil {
		t.Fatal(bundle.err)
	}
	hashes := map[string]string{}
	for k, v := range bundle.hashes {
		hashes[k] = v
	}
	return Config{RunID: "host-run", SourceRevision: strings.Repeat("c", 40), Architecture: runtime.GOARCH, Iteration: 1, Total: 1,
			ExpectedHashes: hashes, CallsheetPath: bundle.cs, FakeAdapterPath: bundle.fake, FixtureDir: bundle.fixtures, WorkDir: filepath.Join(t.TempDir(), "work")},
		newFakePlane(bundle.cs)
}

// runOrder is every case and phase in the tagged wrappers' order.
var runOrder = []string{CaseRuntime, PhasePrepare, PhaseGoalAnswer, CaseCoordinator, CaseSampleFlow, CasePublication, PhaseSibling, PhaseContinuation,
	CaseContinuation, CasePartialFailed, CasePartialCancelled, CasePartialTimedOut, CasePartialResults, CaseLost, CasePublicationRestart,
	CaseDirtyPull, CaseClaims, CaseEvidence}

// TestSuiteFixtureRun runs every case and phase in the tagged wrappers'
// order against the fixture: the phase order, the ensure-once caching, the
// ledger associations and the checked cleanup, with the records audited by
// the host gate against its static catalog (the scenario expectations stay
// separate from the observations they check). It proves the coordinator
// logic and the record shapes, never the product: that is the container's.
func TestSuiteFixtureRun(t *testing.T) {
	cfg, f := fixtureConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := newSuite(ctx, cfg, f.seams())
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			s.Close(ctx)
		}
	}()
	var stream bytes.Buffer
	for _, id := range runOrder {
		stream.WriteString("=== RUN   " + id + "\n")
		rec, err := s.RunCase(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if rec.Outcome != OutcomePass || rec.CaseID != id {
			t.Fatalf("%s record %+v", id, rec)
		}
		if !strings.Contains(id, "/") || strings.HasPrefix(id, CasePartialResults+"/") {
			WriteRecord(&stream, Record{Case: &rec})
		}
		stream.WriteString(strings.Repeat("    ", strings.Count(id, "/")) + "--- PASS: " + id + " (0.01s)\n")
	}
	// Preparation applied the registry-revision restart of node a before
	// the first dispatch.
	if first := indexOf(f.calls, "dispatch --json"); first < 0 || f.nodes["a"].synced != f.revision || f.nodes["a"].pid == 4100 {
		t.Fatalf("no registry-revision restart before dispatch %d: %+v", first, f.nodes["a"])
	}
	// Cached: the same record again, no rerun.
	calls := len(f.calls)
	again, err := s.RunCase(ctx, CasePublication)
	first, _ := s.RunCase(ctx, CasePublication)
	if err != nil || again.Tasks[0].TaskID != first.Tasks[0].TaskID || len(f.calls) != calls {
		t.Fatal("RunCase is not ensure-once")
	}
	if _, err := s.RunCase(ctx, "TestContainerNothing"); err == nil || !strings.Contains(err.Error(), "unknown case") {
		t.Fatalf("unknown case: %v", err)
	}
	negativeChecks(t, ctx, s)
	closed = true
	cleanupErr := s.Close(ctx)
	if cleanupErr != nil || !f.closed {
		t.Fatalf("cleanup: %v (closed %v)", cleanupErr, f.closed)
	}
	if s.Close(ctx) != nil {
		t.Fatal("Close is not idempotent")
	}
	if _, err := s.RunCase(ctx, CaseClaims); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("after close: %v", err)
	}
	if _, err := os.Stat(s.root); !os.IsNotExist(err) {
		t.Fatal("runtime state not removed")
	}
	stream.WriteString("PASS\n")
	end := s.EndRecord(nil, cleanupErr)
	if end.Outcome != OutcomePass || !end.CleanupOK || end.Error != "" {
		t.Fatalf("end %+v", end)
	}
	WriteRecord(&stream, Record{End: &end})
	meta := devcheck.ContainerMetadata{RunID: cfg.RunID, SourceRevision: cfg.SourceRevision, Architecture: cfg.Architecture, Iteration: 1, Total: 1,
		BinaryHashes: cfg.ExpectedHashes}
	if err := devcheck.ValidateContainerEvidence(&stream, devcheck.ContainerExpectation{Metadata: meta, CaseIDs: devcheck.ContainerCaseIDs(),
		Subcases: devcheck.ContainerSubcases()}); err != nil {
		t.Fatalf("the host gate rejects the records: %v", err)
	}
	// C3: the end record's outcome is the overall result; cleanup_ok is
	// cleanup alone. A failed test with a successful cleanup fails.
	for name, c := range map[string]struct {
		tests, cleanup error
		ok             bool
		text           string
	}{
		"failed test, cleanup ok": {errors.New("the acceptance tests exited 1"), nil, true, "the acceptance tests exited 1"},
		"cleanup failed":          {nil, errors.New("x\ny"), false, "x y"},
		"both":                    {errors.New("tests"), errors.New("cleanup"), false, "tests cleanup"},
	} {
		if e := s.EndRecord(c.tests, c.cleanup); e.Outcome != OutcomeFail || e.CleanupOK != c.ok || e.Error != c.text {
			t.Fatalf("%s: end %+v", name, e)
		}
	}
}

func indexOf(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

// negativeChecks runs the scenario checks against the fixture with
// mismatched expectations: each must reject an observation that does not
// match (a wrong role, report, diff, tree, ref, base, instance or
// barrier), so a passing case cannot hide a mismatch.
func negativeChecks(t *testing.T, ctx context.Context, s *Suite) {
	t.Helper()
	ws, a := s.m4, s.pubA.view
	commitA := *a.Result.Workspace.Commit
	zero40, zero32 := strings.Repeat("0", 40), strings.Repeat("0", 32)
	withResult := func(f func(*contract.TaskResult)) contract.TaskView {
		v := a
		r := *a.Result
		f(&r)
		v.Result = &r
		return v
	}
	failedState := a
	failedState.State = contract.TaskFailed
	for name, err := range map[string]error{
		"assignment": s.assignment(a, "proof-b", "a"),
		"role":       s.checkPublished(ctx, ws, a, ws.base, "", nil, nil, "proof-b", "b", "a.txt"),
		"answer":     s.checkPublished(ctx, ws, withResult(func(r *contract.TaskResult) { r.FinalMessage = nil }), ws.base, "", nil, nil, "proof-a", "a"),
		"state":      s.checkPublished(ctx, ws, failedState, ws.base, "", nil, nil, "proof-a", "a"),
		"report":     s.checkPublished(ctx, ws, a, ws.base, "a-report.json", map[string]string{"x": "y"}, nil, "proof-a", "a", "a.txt"),
		"diff":       s.checkPublished(ctx, ws, a, ws.base, "", nil, nil, "proof-a", "a", "zzz.txt"),
		"diff count": s.checkPublished(ctx, ws, a, ws.base, "", nil, nil, "proof-a", "a"),
		"tree":       s.checkPublished(ctx, ws, a, ws.base, "", nil, map[string]string{}, "proof-a", "a", "a.txt"),
		"status":     s.inspect(ctx, a, zero40, commitA, "a.txt"),
		"main":       s.mainUnmoved(ctx, workspaceFixture{name: ws.name, instance: ws.instance, base: commitA}),
		"barrier":    s.awaitFile(ctx, filepath.Join(s.barriers, "never-made"), a.TaskID),
	} {
		if err == nil {
			t.Fatalf("%s mismatch accepted", name)
		}
	}
	if _, _, err := s.pullCommit(ctx, ws, a.TaskID, zero40); err == nil {
		t.Fatal("pull mismatch accepted")
	}
	if _, _, err := s.refsOf(ctx, workspaceFixture{name: ws.name, instance: zero32}); err == nil {
		t.Fatal("instance mismatch accepted")
	}
	if _, err := s.pushWorkspace(ctx, "Bad Name", nil); err == nil {
		t.Fatal("invalid workspace created")
	}
	if _, err := s.pushWorkspace(ctx, ws.name, map[string]string{"x": "y"}); err == nil {
		t.Fatal("existing workspace created again")
	}
	if _, err := s.wsDispatch(ctx, "proof-a", workspaceFixture{name: ws.name, instance: zero32, base: ws.base}, ws.base, "x"); err == nil {
		t.Fatal("a stale instance was dispatched")
	}
	if _, err := s.dispatch(ctx, "no-such-role", "x"); err == nil {
		t.Fatal("an unknown role was dispatched")
	}
	if _, err := s.await(ctx, "t_"+zero32); err == nil {
		t.Fatal("an unknown task resolved")
	}
	// The sidecar's commit identity and single parent.
	c, _, err := s.pullCommit(ctx, ws, a.TaskID, commitA)
	if err != nil {
		t.Fatal(err)
	}
	if err := sidecarCommit(c, a, zero40); err == nil {
		t.Fatal("wrong parent accepted")
	}
	forged := *c
	forged.Author.Email = "fixture@example.invalid"
	if err := sidecarCommit(&forged, a, ws.base); err == nil {
		t.Fatal("a fixture-made commit accepted")
	}
	if _, err := fixtureRepo(filepath.Join(s.barriers, "a-report.json")); err == nil {
		t.Fatal("a repository inside a file")
	}
}

// TestSuiteCommandFailures is the command fault sweep: in a fresh suite,
// the n-th coordinator command of a complete run exits 1, for every n.
// Every such failure fails at least one case (a failure is never masked
// by a later step, a retry or a cached prerequisite), never panics, and
// leaves cleanup intact, so the end record is outcome fail with cleanup_ok
// true (C3). JSON commands are then swept again with malformed output.
func TestSuiteCommandFailures(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, f *fakePlane, cfg Config) (failed []string, end EndRecord) {
		s, err := newSuite(ctx, cfg, f.seams())
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range runOrder {
			if _, err := s.RunCase(ctx, id); err != nil {
				failed = append(failed, id)
			}
		}
		cleanupErr := s.Close(ctx)
		return failed, s.EndRecord(nil, cleanupErr)
	}
	cfg, f := fixtureConfig(t)
	if failed, end := run(t, f, cfg); len(failed) != 0 || end.Outcome != OutcomePass {
		t.Fatalf("the complete run failed: %v %+v", failed, end)
	}
	calls := append([]string(nil), f.calls...)
	t.Logf("%d coordinator commands in a complete run", len(calls))
	// A second complete run: the same command sequence (the sweep's
	// positions are deterministic), recording which commands are --json.
	jsonCall := map[int]bool{}
	cfg, f = fixtureConfig(t)
	f.mutate = func(args []string, _ *fakeAnswer) {
		for _, a := range args {
			if a == "--json" {
				jsonCall[len(f.calls)] = true
			}
		}
	}
	run(t, f, cfg)
	if strings.Join(f.calls, "\n") != strings.Join(calls, "\n") {
		t.Fatal("the command sequence of a complete run is not deterministic")
	}
	// Each position runs in its own fresh suite and fixture, in parallel.
	for _, malformed := range []bool{false, true} {
		for n := 1; n <= len(calls); n++ {
			if malformed && !jsonCall[n] {
				continue
			}
			t.Run(fmt.Sprintf("malformed=%v/%d", malformed, n), func(t *testing.T) {
				t.Parallel()
				checkCommandFailure(t, run, n, calls[n-1], malformed)
			})
		}
	}
}

// checkCommandFailure runs one sweep position: command n exits 1, or
// answers malformed JSON.
func checkCommandFailure(t *testing.T, run func(*testing.T, *fakePlane, Config) ([]string, EndRecord), n int, call string, malformed bool) {
	cfg, f := fixtureConfig(t)
	if malformed {
		f.mutate = func(_ []string, a *fakeAnswer) {
			if len(f.calls) == n {
				a.stdout = `{"version":6,`
			}
		}
	} else {
		f.fail = n
	}
	failed, end := run(t, f, cfg)
	if len(failed) == 0 {
		t.Fatalf("command %d (%s, malformed %v) failed but every case passed", n, call, malformed)
	}
	if end.Outcome != OutcomeFail || !end.CleanupOK || !strings.Contains(end.Error, "failed cases: ") {
		t.Fatalf("command %d (%s, malformed %v): end %+v", n, call, malformed, end)
	}
}

// TestSuiteFailures: setup refusals, a failed prerequisite failing its
// dependents (never skipping them), a broken coordinator CLI failing every
// case with a failed record, cleanup of a failed setup and every checked
// cleanup failure.
func TestSuiteFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, _ := fixtureConfig(t)
	bad := cfg
	bad.RunID = ""
	if _, err := New(ctx, bad); err == nil {
		t.Fatal("invalid config accepted")
	}
	bad = cfg
	bad.CallsheetPath = filepath.Join(cfg.WorkDir, "missing")
	if _, err := New(ctx, bad); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing executable: %v", err)
	}
	// The plane does not start.
	cfg, f := fixtureConfig(t)
	f.startErr = errors.New("plane init failed")
	if _, err := newSuite(ctx, cfg, f.seams()); err == nil || !strings.Contains(err.Error(), "plane: plane init failed") {
		t.Fatalf("plane: %v", err)
	}
	// A sidecar refuses its adapter or fails: the started plane is stopped
	// again and the runtime state removed.
	for _, name := range []string{"node-a", "node-b"} {
		cfg, f := fixtureConfig(t)
		f.sidecarErr[name] = errors.New("the sidecar refused its fake adapter")
		if _, err := newSuite(ctx, cfg, f.seams()); err == nil || !strings.Contains(err.Error(), "sidecar") || !f.closed {
			t.Fatalf("sidecar failure: %v (plane stopped %v)", err, f.closed)
		}
		if _, err := os.Stat(filepath.Join(cfg.WorkDir, "run-1")); !os.IsNotExist(err) {
			t.Fatal("a failed setup left its runtime state")
		}
	}
	// Missing manuals: preparation fails and every dependent fails with it.
	cfg, f = fixtureConfig(t)
	cfg.FixtureDir = filepath.Join(cfg.WorkDir, "empty-fixtures")
	s, err := newSuite(ctx, cfg, f.seams())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range runOrder[:len(runOrder)-1] {
		rec, err := s.RunCase(ctx, id)
		if err == nil || rec.Outcome != OutcomeFail || rec.Error == "" {
			t.Fatalf("%s passed without its prerequisites: %+v", id, rec)
		}
	}
	if rec, _ := s.RunCase(ctx, CaseCoordinator); rec.Subcases["prepare"] != OutcomeFail || rec.Subcases["goal_answer"] != OutcomeFail {
		t.Fatalf("coordinator subcases %v", rec.Subcases)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if end := s.EndRecord(nil, nil); end.Outcome != OutcomeFail || !end.CleanupOK || !strings.Contains(end.Error, CasePublication) {
		t.Fatalf("failed cases with a clean cleanup: end %+v", end)
	}
	// The registry-revision restart fails: preparation fails.
	cfg, f = fixtureConfig(t)
	f.restartErr = errors.New("sidecar did not come back")
	if s, err = newSuite(ctx, cfg, f.seams()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunCase(ctx, PhasePrepare); err == nil || !strings.Contains(err.Error(), "registry-revision restart") {
		t.Fatalf("restart failure: %v", err)
	}
	s.Close(ctx)
	// A broken coordinator CLI after preparation: every coordinator
	// command fails, so every case records its failure.
	cfg, f = fixtureConfig(t)
	if s, err = newSuite(ctx, cfg, f.seams()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunCase(ctx, PhasePrepare); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureM4(ctx); err != nil {
		t.Fatal(err)
	}
	s.cfg.CallsheetPath = s.cfg.FakeAdapterPath
	for _, id := range []string{CaseRuntime, PhaseGoalAnswer, CaseSampleFlow, CasePublication, CaseContinuation, CasePartialFailed, CasePartialCancelled,
		CasePartialTimedOut, CasePartialResults, CaseLost, CasePublicationRestart, CaseDirtyPull} {
		if rec, err := s.RunCase(ctx, id); err == nil || rec.Outcome != OutcomeFail {
			t.Fatalf("%s passed with a broken CLI", id)
		}
	}
	s.Close(ctx)
	// A coordinator command that cannot run to an exit.
	cfg, f = fixtureConfig(t)
	if s, err = newSuite(ctx, cfg, f.seams()); err != nil {
		t.Fatal(err)
	}
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	if _, err := s.cliOK(cctx, "node", "ls"); err == nil || !strings.Contains(err.Error(), "callsheet node ls") {
		t.Fatalf("cancelled command: %v", err)
	}
	s.Close(ctx)
	// Cleanup failures: each is reported, Close stays idempotent and the
	// end record fails with cleanup_ok false.
	cases := []struct {
		name string
		set  func(*fakePlane, *Suite) context.Context
		want string
	}{
		{"stop", func(f *fakePlane, _ *Suite) context.Context {
			f.closeErr = errors.New("plane ignored SIGTERM")
			return ctx
		}, "plane ignored SIGTERM"},
		{"survivor", func(f *fakePlane, _ *Suite) context.Context {
			f.survivors = []string{"4100 /bin/callsheet"}
			c, cancel := context.WithCancel(ctx)
			cancel()
			return c
		}, "survive the shutdown"},
		{"work", func(f *fakePlane, s *Suite) context.Context {
			f.keepWork = true
			s.cliOK(ctx, "dispatch", "--role-id", "proof-a", "--goal", s.wsScript("", fakeadapterWait(s)), "--workspace", s.m4.name, "--base", s.m4.base,
				"--workspace-instance", s.m4.instance)
			return ctx
		}, "keeps the work directory"},
		{"held", func(_ *fakePlane, s *Suite) context.Context {
			plain := filepath.Join(s.barriers, "plain-file")
			os.WriteFile(plain, nil, 0o600)
			s.held = append(s.held, filepath.Join(plain, "release"))
			return ctx
		}, "not a directory"},
	}
	for _, c := range cases {
		cfg, f := fixtureConfig(t)
		s, err := newSuite(ctx, cfg, f.seams())
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ensureM4(ctx); err != nil {
			t.Fatal(err)
		}
		cerr := s.Close(c.set(f, s))
		if cerr == nil || !strings.Contains(cerr.Error(), c.want) || s.Close(ctx) != cerr {
			t.Fatalf("%s: cleanup %v", c.name, cerr)
		}
		if end := s.EndRecord(nil, cerr); end.Outcome != OutcomeFail || end.CleanupOK || !strings.Contains(end.Error, c.want) {
			t.Fatalf("%s: end %+v", c.name, end)
		}
	}
	// The process check waits for survivors to disappear, and fails on an
	// unreadable process table.
	cfg, f = fixtureConfig(t)
	sm := f.seams()
	n := 0
	sm.live = func(map[string]bool, []int) ([]string, error) {
		if n++; n < 3 {
			return []string{"4100 /bin/callsheet"}, nil
		}
		return nil, nil
	}
	if s, err = newSuite(ctx, cfg, sm); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil || n != 3 {
		t.Fatalf("survivors that exit: %v after %d checks", err, n)
	}
	cfg, f = fixtureConfig(t)
	sm = f.seams()
	sm.live = func(map[string]bool, []int) ([]string, error) { return nil, errors.New("no process table") }
	if s, err = newSuite(ctx, cfg, sm); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err == nil || !strings.Contains(err.Error(), "no process table") {
		t.Fatalf("unreadable process table: %v", err)
	}
}

// fakeadapterWait is a worker op that holds until the suite releases it.
func fakeadapterWait(s *Suite) fakeadapter.WorkspaceOp {
	return fakeadapter.WorkspaceOp{WaitFor: s.hold("never-released")}
}

// TestProcessSeams covers the real operations without a Callsheet
// process: a child command (this test binary as a helper) and its exit
// code, output, directory and environment; a missing executable; a
// context end; the real process table; and a plane that cannot start.
func TestProcessSeams(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx := context.Background()
	for _, code := range []int{0, 3} {
		var out, errOut bytes.Buffer
		got, err := execCommand(ctx, command{bin: self, dir: dir, args: []string{"node", "ls"}, env: []string{helperEnv + "=exit=" + strconv.Itoa(code)},
			stdout: &out, stderr: &errOut})
		// (A coverage build's child may append its own GOCOVERDIR warning.)
		if err != nil || got != code || out.String() != "args=node,ls cwd="+dir+"\n" || !strings.HasPrefix(errOut.String(), "helper stderr\n") {
			t.Fatalf("exit %d: %d %v %q %q", code, got, err, out.String(), errOut.String())
		}
	}
	if _, err := execCommand(ctx, command{bin: filepath.Join(dir, "missing"), dir: dir, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}); err == nil {
		t.Fatal("a missing executable ran")
	}
	cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := execCommand(cctx, command{bin: self, dir: dir, env: []string{helperEnv + "=hang"}, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}); err == nil {
		t.Fatal("a command outlived its context")
	}
	// The suite's command path through the real child process.
	cfg, f := fixtureConfig(t)
	sm := f.seams()
	sm.run = execCommand
	s, err := newSuite(ctx, cfg, sm)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.CallsheetPath = self
	s.cliEnv = append(s.cliEnv, helperEnv+"=exit=4")
	r, err := s.cli(ctx, "task", "show")
	if err != nil || r.code != 4 || !strings.HasPrefix(r.stdout, "args=task,show,--plane,"+s.url+",--ca,"+s.ca+" ") {
		t.Fatalf("suite command: %+v %v", r, err)
	}
	if _, err := s.cliOK(ctx, "task", "show"); err == nil || !strings.Contains(err.Error(), "exited 4: helper stderr") {
		t.Fatalf("nonzero exit: %v", err)
	}
	s.Close(ctx)
	// The real seams: the process table and a plane whose executable does
	// not exist (nothing starts).
	real := processSeams()
	if live, err := real.live(map[string]bool{filepath.Join(dir, "none"): true}, nil); err != nil || len(live) != 0 {
		t.Fatalf("process table: %v %v", live, err)
	}
	if _, err := real.start(ctx, filepath.Join(dir, "missing"), filepath.Join(dir, "deploy"), nil); err == nil {
		t.Fatal("a plane started from a missing executable")
	}
}

// TestScenarioChecks covers the observation checks against malformed and
// mismatched observations: a rejected or publication-pending task never
// passes.
func TestScenarioChecks(t *testing.T) {
	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	ws := workspaceFixture{name: "w", instance: strings.Repeat("1", 32), base: strings.Repeat("b", 40)}
	id := "t_" + strings.Repeat("a", 32)
	good := func() contractView {
		return contractView{TaskID: id, State: "succeeded", WorkspaceBinding: &binding{Name: "w", Instance: ws.instance, BaseSelector: ws.base, BaseCommit: str(ws.base)},
			Result: &result{ExitCode: num(0), FinalMessage: str(FinalMessage), ResultCommit: str(strings.Repeat("c", 40)),
				Workspace: &wsResult{Publication: "published", Commit: str(strings.Repeat("c", 40)), Ref: str("refs/callsheet/tasks/" + id)}}}
	}
	if _, err := published(good(), "succeeded", ws, ws.base); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*contractView){
		"state":          func(v *contractView) { v.State = "failed" },
		"no result":      func(v *contractView) { v.Result = nil },
		"pending":        func(v *contractView) { v.Result.Workspace.Publication = "not_started" },
		"rejected":       func(v *contractView) { v.State = "rejected" },
		"other ref":      func(v *contractView) { v.Result.Workspace.Ref = str("refs/callsheet/tasks/t_other") },
		"mirror":         func(v *contractView) { v.Result.ResultCommit = str(strings.Repeat("d", 40)) },
		"no binding":     func(v *contractView) { v.WorkspaceBinding = nil },
		"wrong base":     func(v *contractView) { v.WorkspaceBinding.BaseCommit = str(strings.Repeat("e", 40)) },
		"wrong instance": func(v *contractView) { v.WorkspaceBinding.Instance = strings.Repeat("2", 32) },
	} {
		v := good()
		mut(&v)
		if _, err := published(v, "succeeded", ws, ws.base); err == nil {
			t.Fatalf("%s passed", name)
		}
	}
	if err := answered(good()); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*contractView){
		"exit":    func(v *contractView) { v.Result.ExitCode = num(1) },
		"message": func(v *contractView) { v.Result.FinalMessage = str("pong") },
		"nil":     func(v *contractView) { v.Result = nil },
	} {
		v := good()
		mut(&v)
		if answered(v) == nil {
			t.Fatalf("%s answered", name)
		}
	}
	// resolved: terminal with its resolved publication only.
	v := good()
	if !resolved(v) {
		t.Fatal("published view not resolved")
	}
	v.CompletionPending = true
	if resolved(v) {
		t.Fatal("completion-pending view resolved")
	}
	v = good()
	v.Result.Workspace = nil
	if resolved(v) {
		t.Fatal("missing publication resolved")
	}
	v = good()
	v.State = "running"
	if resolved(v) {
		t.Fatal("running view resolved")
	}
	v = good()
	v.WorkspaceBinding, v.Result.Workspace = nil, nil
	if !resolved(v) {
		t.Fatal("scratch terminal view not resolved")
	}
	// The evidence task is the actual decoded view; only the fixed answer
	// may enter final_message.
	o := observed{"x", good()}
	tk := o.task()
	if tk.Label != "x" || *tk.WorkspaceInstance != ws.instance || *tk.BaseCommit != ws.base || *tk.ResultRef != "refs/callsheet/tasks/"+id ||
		*tk.PublicationStatus != "published" || *tk.ExitCode != 0 || *tk.FinalMessage != FinalMessage {
		t.Fatalf("task %+v", tk)
	}
	o.view.Result.FinalMessage = str("secret answer text")
	if tk := o.task(); *tk.FinalMessage == "secret answer text" {
		t.Fatal("an arbitrary answer entered the evidence")
	}
	o.view.WorkspaceBinding, o.view.Result = nil, nil
	if tk := o.task(); tk.WorkspaceInstance != nil || tk.PublicationStatus != nil || tk.ExitCode != nil {
		t.Fatalf("scratch task %+v", tk)
	}
	// Trees, reports and helpers.
	if err := sameTree(nil, map[string]string{"a": "b"}); err == nil {
		t.Fatal("tree mismatch accepted")
	}
	if got := reportOf(map[string]string{"a/b/c.txt": "x"}); len(got) != 3 || got["a"] != "d 0755 "+sum("") || got["a/b/c.txt"] != "f 0644 "+sum("x") {
		t.Fatalf("report %v", got)
	}
	s := &Suite{barriers: t.TempDir()}
	if err := s.checkReport("missing.json", nil); err == nil {
		t.Fatal("missing report accepted")
	}
	os.WriteFile(filepath.Join(s.barriers, "bad.json"), []byte("{"), 0o600)
	if err := s.checkReport("bad.json", nil); err == nil {
		t.Fatal("malformed report accepted")
	}
	os.WriteFile(filepath.Join(s.barriers, "other.json"), []byte(`{"has_git":true,"files":{"x":"f 0644 0"}}`), 0o600)
	if err := s.checkReport("other.json", map[string]string{"y": ""}); err == nil {
		t.Fatal("mismatched report accepted")
	}
	if got := sanitizeError(errors.New("a\nb\x01" + strings.Repeat("z", 3000))); strings.ContainsAny(got, "\n\x01") || len(got) != maxErrorText+3 {
		t.Fatalf("sanitized %q", got[:20])
	}
	if outcome(nil) != OutcomePass || outcome(errors.New("x")) != OutcomeFail {
		t.Fatal("outcome")
	}
	// Bounded polling: success, a failing condition, the bound and a
	// cancelled context.
	ctx := context.Background()
	if err := poll(ctx, "ok", time.Second, func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if err := poll(ctx, "err", time.Second, func() (bool, error) { return false, errors.New("boom") }); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatal(err)
	}
	if err := poll(ctx, "never", 30*time.Millisecond, func() (bool, error) { return false, nil }); err == nil || !strings.Contains(err.Error(), "not observed") {
		t.Fatal(err)
	}
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	if err := poll(cctx, "cancelled", time.Minute, func() (bool, error) { return false, nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// The process check through a synthetic proc tree: live, zombie,
	// foreign and unreadable entries.
	proc := t.TempDir()
	mk := func(pid, exe, state string) {
		d := filepath.Join(proc, pid)
		os.MkdirAll(d, 0o755)
		if exe != "" {
			os.Symlink(exe, filepath.Join(d, "exe"))
		}
		if state != "" {
			os.WriteFile(filepath.Join(d, "stat"), []byte(pid+" (callsheet) "+state+" 1 2"), 0o644)
		}
	}
	mk("10", "/x/callsheet", "S")
	mk("11", "/x/callsheet (deleted)", "R")
	mk("12", "/x/callsheet", "Z")
	mk("13", "/x/other", "S")
	mk("14", "", "")
	mk("15", "/x/fake", "")
	mk("self", "/x/callsheet", "S")
	live, err := liveProcesses(proc, map[string]bool{"/x/callsheet": true, "/x/fake": true}, nil)
	if err != nil || len(live) != 2 || !strings.HasPrefix(live[0], "10 ") || !strings.HasPrefix(live[1], "11 ") {
		t.Fatalf("live = %v %v", live, err)
	}
	live, _ = liveProcesses(filepath.Join(proc, "none"), nil, []int{os.Getpid(), 0})
	if len(live) != 1 {
		t.Fatalf("pid fallback = %v", live)
	}
}

// TestConfigAndRecords covers the environment parser, the configuration
// rules and the record writer.
func TestConfigAndRecords(t *testing.T) {
	hashes := `{"acceptance_test":"` + strings.Repeat("a", 64) + `","callsheet":"` + strings.Repeat("b", 64) + `","fake_adapter":"` + strings.Repeat("c", 64) + `"}`
	env := map[string]string{EnvRunID: "run-1", EnvIteration: "2", EnvTotal: "20", EnvRevision: strings.Repeat("d", 40), EnvArch: "arm64", EnvHashes: hashes,
		"HOME": "/ignored"}
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cfg, err := ConfigFromEnv(get(env))
	if err != nil || cfg.Iteration != 2 || cfg.Total != 20 || cfg.CallsheetPath != ImageCallsheet || cfg.FakeAdapterPath != ImageFakeAdapter ||
		cfg.FixtureDir != ImageFixtures || cfg.WorkDir != ImageWorkRoot || len(cfg.ExpectedHashes) != 3 {
		t.Fatalf("config %+v %v", cfg, err)
	}
	for name, mut := range map[string][2]string{
		"run id":         {EnvRunID, "Bad Run"},
		"iteration":      {EnvIteration, "x"},
		"iteration sign": {EnvIteration, "+2"},
		"iteration pad":  {EnvIteration, "02"},
		"iteration zero": {EnvIteration, "0"},
		"total":          {EnvTotal, "21"},
		"total word":     {EnvTotal, "twenty"},
		"over total":     {EnvIteration, "21"},
		"revision":       {EnvRevision, "abc"},
		"arch":           {EnvArch, "386"},
		"hashes":         {EnvHashes, "[]"},
		"hashes empty":   {EnvHashes, ""},
		"hash key":       {EnvHashes, `{"acceptance_test":"` + strings.Repeat("a", 64) + `","callsheet":"` + strings.Repeat("b", 64) + `","other":"` + strings.Repeat("c", 64) + `"}`},
		"hash upper":     {EnvHashes, strings.Replace(hashes, strings.Repeat("a", 64), strings.Repeat("A", 64), 1)},
		"hash extra":     {EnvHashes, strings.TrimSuffix(hashes, "}") + `,"x":"` + strings.Repeat("e", 64) + `"}`},
	} {
		m := map[string]string{}
		for k, v := range env {
			m[k] = v
		}
		m[mut[0]] = mut[1]
		if _, err := ConfigFromEnv(get(m)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	rel := cfg
	rel.WorkDir = "relative"
	if rel.Validate() == nil {
		t.Fatal("relative work dir accepted")
	}
	// Records: exactly one variant; node_bindings only when set; sorted end
	// IDs; one write per line.
	var b bytes.Buffer
	if err := WriteRecord(&b, Record{}); err == nil {
		t.Fatal("empty record written")
	}
	c := CaseRecord{CaseID: CaseClaims}
	e := EndRecord{CaseIDs: []string{"b", "a"}}
	if err := WriteRecord(&b, Record{Case: &c, End: &e}); err == nil {
		t.Fatal("double record written")
	}
	if err := WriteRecord(&b, Record{Case: &c}); err != nil {
		t.Fatal(err)
	}
	line := b.String()
	if !strings.HasPrefix(line, EvidencePrefix+"{") || strings.Count(line, "\n") != 1 || strings.Contains(line, "node_bindings") ||
		!strings.Contains(line, `"tasks":[]`) || !strings.Contains(line, `"subcases":{}`) || !strings.Contains(line, `"binary_hashes":{}`) ||
		!strings.Contains(line, `"os":"linux"`) || !strings.Contains(line, `"adapter":"fake"`) {
		t.Fatalf("case line %q", line)
	}
	b.Reset()
	c.NodeBindings = map[string]string{"a": "n_1", "b": "n_2"}
	WriteRecord(&b, Record{Case: &c})
	if !strings.Contains(b.String(), `"node_bindings":{"a":"n_1","b":"n_2"}`) {
		t.Fatalf("bindings %q", b.String())
	}
	b.Reset()
	WriteRecord(&b, Record{End: &e})
	if !strings.Contains(b.String(), `"case_ids":["a","b"]`) || !strings.Contains(b.String(), `"kind":"end"`) {
		t.Fatalf("end %q", b.String())
	}
	if err := WriteRecord(shortWriter{}, Record{End: &e}); err == nil {
		t.Fatal("short write accepted")
	}
	if err := WriteRecord(errWriter{}, Record{End: &e}); err == nil {
		t.Fatal("write error accepted")
	}
	if got := strings.Join(RequiredCaseIDs(), ","); got != strings.Join(devcheck.ContainerCaseIDs(), ",") {
		t.Fatalf("manifest %s differs from the host's", got)
	}
	for id, want := range boundaries {
		if strings.Contains(id, "/") && !strings.HasPrefix(id, CasePartialResults) {
			continue
		}
		if devcheck.ContainerBoundary(id) != want {
			t.Fatalf("boundary of %s differs from the host's", id)
		}
	}
	// The synthetic stream passes the host gate; a mutated one fails.
	meta := devcheck.ContainerMetadata{RunID: "r", SourceRevision: strings.Repeat("d", 40), Architecture: "amd64", Iteration: 1, Total: 1,
		BinaryHashes: cfg.ExpectedHashes}
	exp := devcheck.ContainerExpectation{Metadata: meta, CaseIDs: devcheck.ContainerCaseIDs(), Subcases: devcheck.ContainerSubcases()}
	stream := SyntheticStream(Common{RunID: "r", Iteration: 1, Total: 1, SourceRevision: meta.SourceRevision, Arch: "amd64", BinaryHashes: meta.BinaryHashes})
	if err := devcheck.ValidateContainerEvidence(bytes.NewReader(stream), exp); err != nil {
		t.Fatal(err)
	}
	if err := devcheck.ValidateContainerEvidence(bytes.NewReader(bytes.Replace(stream, []byte(`"cleanup_ok":true`), []byte(`"cleanup_ok":false`), 1)), exp); err == nil {
		t.Fatal("failed cleanup accepted")
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// TestClaims checks the working tree's milestone documents and rejects
// each removed mapping, statement or bundle file and each superseded
// claim.
func TestClaims(t *testing.T) {
	repo := testkit.MustRepoRoot(t)
	docs := map[string][]byte{}
	for _, d := range claimDocs {
		b, err := os.ReadFile(filepath.Join(repo, "docs", d))
		if err != nil {
			t.Fatal(err)
		}
		docs[d] = b
	}
	if err := CheckClaims(docs); err != nil {
		t.Fatalf("working tree docs: %v", err)
	}
	mutate := func(doc, old, repl, want string) {
		t.Helper()
		m := map[string][]byte{}
		for k, v := range docs {
			m[k] = v
		}
		if !bytes.Contains(m[doc], []byte(old)) {
			t.Fatalf("%s lacks %q", doc, old)
		}
		m[doc] = bytes.ReplaceAll(m[doc], []byte(old), []byte(repl))
		if err := CheckClaims(m); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s %q -> %q: %v, want %q", doc, old, repl, err, want)
		}
	}
	mutate("workspaces.md", "| FP-7 | `TestContainerPublicationRestart` |", "| FP-6 | `TestContainerPublicationRestart` |", "does not map M4-8")
	mutate("workspaces.md", "| optional, not gated | none |", "| FP-3 | `TestContainerPublication` |", "does not map M4-10")
	mutate("workspaces.md", "| **M4-5 Parallel sibling:**", "| **M4-4 Second hop:**", "maps M4-4 Second hop twice")
	mutate("workspaces.md", "`TestContainerSampleFlow`", "TestContainerSampleFlow", "lacks \"`TestContainerSampleFlow`\"")
	mutate("workspaces.md", "this slice does not restart the plane", "the plane restarts too", "this slice does not restart the plane")
	mutate("workspaces.md", "No macOS container proof is required", "macOS proof pending", "No macOS container proof is required")
	mutate("workspaces.md", "Never record file contents or credentials.", "Never record file contents or credentials. These are manual checks for the two-machine M3+M4 acceptance session.",
		"superseded claim")
	mutate("workspaces.md", "## Manual M4 checks", "## Manual checks", "no Manual M4 checks section")
	mutate("real-adapters.md", "`goal_answer`", "goal answer", "`goal_answer`")
	mutate("real-adapters.md", "M3 has not been\ndemonstrated.", "M3 is demonstrated.", "M3 has not been demonstrated")
	mutate("real-adapters.md", "## Remote acceptance (M3)", "## Remote acceptance", "no Remote acceptance (M3) section")
	mutate("ci.md", "Publish container E2E evidence", "Publish evidence", "Publish container E2E evidence")
	m := map[string][]byte{"ci.md": docs["ci.md"]}
	if err := CheckClaims(m); err == nil || !strings.Contains(err.Error(), "missing from the bundle") {
		t.Fatalf("missing docs: %v", err)
	}
}
