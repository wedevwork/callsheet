package reale2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Unit tests of design 12a-3-cursor-worker, slice 3a (UT-A1..UT-A6; the
// shallow observer's are devcursor_activity_test.go).

func TestCursorSelectionUnit(t *testing.T) {
	t.Parallel()
	// UT-A1: exactly Claude or Cursor for the coder, the fixed adapters
	// elsewhere, and the selected adapter's own model/effort validation.
	base := func(edit func(rs []FlowRole)) DevFullFlow {
		f := DefaultDevFullFlow()
		f.Roles = append([]FlowRole{}, f.Roles...)
		edit(f.Roles)
		return f
	}
	for name, c := range map[string]struct {
		flow DevFullFlow
		err  string
	}{
		"default": {DefaultDevFullFlow(), ""},
		"cursor_coder": {base(func(rs []FlowRole) {
			rs[2] = FlowRole{ID: HopCoder, Adapter: "cursor", Model: "grok-4.7", Effort: "high"}
		}), ""},
		"cursor_max": {base(func(rs []FlowRole) {
			rs[2] = FlowRole{ID: HopCoder, Adapter: "cursor", Model: "grok-4.7", Effort: "max"}
		}), ""},
		"cursor_bad_eff": {base(func(rs []FlowRole) {
			rs[2] = FlowRole{ID: HopCoder, Adapter: "cursor", Model: "grok-4.7", Effort: "turbo"}
		}), "role coder"},
		"grok_coder": {base(func(rs []FlowRole) {
			rs[2] = FlowRole{ID: HopCoder, Adapter: "grok", Model: "grok-4.7", Effort: "high"}
		}), "must use adapter claude or cursor"},
		"codex_coder": {base(func(rs []FlowRole) { rs[2].Adapter = "codex" }), "must use adapter claude or cursor"},
		"cursor_designer": {base(func(rs []FlowRole) {
			rs[0] = FlowRole{ID: HopDesigner, Adapter: "cursor", Model: "grok-4.7", Effort: "high"}
		}), "role designer must use adapter codex"},
		"cursor_reviewer": {base(func(rs []FlowRole) {
			rs[3] = FlowRole{ID: HopCodeReviewer, Adapter: "cursor", Model: "grok-4.7", Effort: "high"}
		}), "role code-reviewer must use adapter codex"},
		"cursor_triage": {base(func(rs []FlowRole) {
			rs[4] = FlowRole{ID: "triage", Adapter: "cursor", Model: "grok-4.7", Effort: "high"}
		}), "role triage must use adapter codex"},
		"cursor_dreviewer": {base(func(rs []FlowRole) {
			rs[1] = FlowRole{ID: HopDesignReviewer, Adapter: "cursor", Model: "grok-4.7", Effort: "low"}
		}), "must use adapter claude"},
	} {
		err := c.flow.Validate()
		if (err == nil) != (c.err == "") || err != nil && !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !base(func(rs []FlowRole) { rs[2].Adapter = "cursor"; rs[2].Model = "grok-4.7" }).CursorCoder() || (DevFullFlow{}).CursorCoder() {
		t.Error("CursorCoder")
	}
	if CursorProbeBoundsFor(probeScopeLight) != ProbeCursorLightBounds || CursorProbeBoundsFor(probeScopeFull) != ProbeCursorBounds ||
		!strings.Contains(ProbeCursorLightBounds, "twelve") || !strings.Contains(ProbeCursorBounds, "ten paid") {
		t.Error("bounds")
	}
	if cursorProbeMaxCalls(probeScopeFull) != 10 || cursorProbeMaxCalls(probeScopeBoth) != 12 || cursorProbeMaxCalls(probeScopeLight) != 12 {
		t.Error("call bounds")
	}
	if strings.Join(cursorProbeStagesFor(probeScopeFull), ",") != "models,coder_evidence,resume_wake,print_notification,reviewer_evidence" ||
		len(cursorProbeStagesFor(probeScopeBoth)) != 7 || len(probeStagesFor(probeScopeBoth)) != 6 || len(probeStages) != 4 {
		t.Error("stage coverage")
	}
}

func TestCursorBindingUnit(t *testing.T) {
	t.Parallel()
	// UT-A2: the read-only selection peek, the flag shape and the bound
	// selection's recheck.
	dir := t.TempDir()
	cursorFlow := filepath.Join(dir, "cursor.json")
	os.WriteFile(cursorFlow, cursorFlowBytes(t), 0o600)
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"schema":"x"}`), 0o600)
	sel, err := peekCursorSelection(cursorFlow)
	none, nerr := peekCursorSelection("")
	if !sel || err != nil || none || nerr != nil {
		t.Fatal("peek")
	}
	for _, p := range []string{bad, filepath.Join(dir, "missing")} {
		if s, err := peekCursorSelection(p); s || err == nil || !strings.HasPrefix(err.Error(), "flow: ") {
			t.Fatalf("%s: %v %v", p, s, err)
		}
	}
	for args, want := range map[string]bool{"--cursor /x": true, "--cursor=/x": true, "-cursor /x": true, "-cursor=/x": true, "--cursorx": false, "--flow /c": false} {
		if cursorArgPresent(strings.Fields(args)) != want {
			t.Errorf("%s", args)
		}
	}
	f := &onceAbsFlag{}
	if f.Set("/a") != nil || f.Set("/b") == nil || !f.bad || f.val != "/a" || f.String() != "" {
		t.Fatal("once")
	}
	for _, v := range []string{"", "rel"} {
		if g := (&onceAbsFlag{}); g.Set(v) == nil || !g.bad {
			t.Errorf("value %q accepted", v)
		}
	}
	cursor, err := ParseDevFullFlow(cursorFlowBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	plain := DefaultDevFullFlow()
	maxed := DefaultDevFullFlow()
	maxed.Roles = append([]FlowRole{}, maxed.Roles...)
	maxed.Roles[2] = FlowRole{ID: HopCoder, Adapter: adapter.CursorID, Model: "grok-4.7", Effort: "max"}
	for name, c := range map[string]struct {
		d    *devSupervisor
		flow DevFullFlow
		code string
	}{
		// Slice 3b: an attempt binds a Cursor flow as the probe does.
		"attempt_nobind": {&devSupervisor{}, cursor, CodeCursorAdapterRequired},
		"attempt_ok":     {&devSupervisor{cursor: &cursorRun{}}, cursor, ""},
		"probe_nobind":   {&devSupervisor{probe: &devProbe{}}, cursor, CodeCursorAdapterRequired},
		"probe_unused":   {&devSupervisor{cursor: &cursorRun{}, probe: &devProbe{cursor: &cursorProbe{}}}, plain, CodeCursorAdapterUnused},
		"probe_ok":       {&devSupervisor{cursor: &cursorRun{}, probe: &devProbe{cursor: &cursorProbe{}}}, cursor, ""},
		"not_shipped":    {&devSupervisor{cursor: &cursorRun{}}, maxed, CodeCursorSelectionMismatch},
		"default_ok":     {&devSupervisor{}, plain, ""},
	} {
		err := c.d.cursorSelectionBinding(c.flow)
		if c.code == "" && err != nil || c.code != "" && codeOf(err) != c.code {
			t.Errorf("%s: %v", name, err)
		}
	}
	if v := (&devSupervisor{}).versionVendors(); strings.Join(v, ",") != "claude,codex,grok" {
		t.Error(v)
	}
	if v := (&devSupervisor{cursor: &cursorRun{}}).versionVendors(); strings.Join(v, ",") != "claude,codex,grok,cursor" {
		t.Error(v)
	}
	if (&supervisor{}).sidecarList()[2] != "grok" || len((&supervisor{sidecars: cursorSidecars}).sidecarList()) != 4 {
		t.Error("sidecar lists")
	}
	if (runArgs{cursor: "/c", grok: "/g"}).vendor("cursor") != "/c" || (runArgs{grok: "/g"}).vendor("grok") != "/g" {
		t.Error("vendor lookup")
	}
	// The live push needs the verified client: any other plane is an
	// error, never a panic.
	_, err = pushFor("linux", nil)(context.Background(), &fakePlane{}, workspacetransfer.PushRequest{})
	if err == nil || !strings.Contains(err.Error(), "live plane client") {
		t.Fatalf("push over a fake plane: %v", err)
	}
	if err := probeVendor(context.Background(), "unknown", "/x", t.TempDir()); err == nil {
		t.Fatal("an unknown vendor probed")
	}
	// A new world's Cursor run samples the three named roots.
	c := newCursorRun(&World{Home: "/home/u"}, "/c")
	if len(c.roots) != 3 || c.roots[0].Path != "/home/u/.cache" || c.roots[0].Token != "$HOME/.cache" || c.roots[1].Path != "/tmp" ||
		c.roots[2].Path != "/var/tmp" || c.roots[2].Scope != activityScopeVarTmp {
		t.Fatalf("roots %+v", c.roots)
	}
}

func TestCursorProbeCacheTokenSymlinkHome(t *testing.T) {
	t.Parallel()
	// A home reached through a symlink still tokens its cache root as
	// $HOME/.cache (the macOS runner's /home is a symlink): an ancestor
	// symlink above a missing user, and an existing home that is itself a
	// symlink. The sampled Path stays the spelled one.
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "Data", "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "Data", "home"), filepath.Join(base, "home")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "real-user", ".cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real-user"), filepath.Join(base, "link-user")); err != nil {
		t.Fatal(err)
	}
	for _, home := range []string{filepath.Join(base, "home", "u"), filepath.Join(base, "link-user")} {
		c := newCursorRun(&World{Home: home}, "/c")
		if len(c.roots) != 3 {
			t.Fatalf("%s: roots %+v", home, c.roots)
		}
		r := c.roots[0]
		if r.Scope != activityScopeCache || r.Path != filepath.Join(home, ".cache") || r.Token != "$HOME/.cache" || strings.Contains(r.Token, base) {
			t.Errorf("%s: cache root %+v", home, r)
		}
		if c.roots[1].Token != "/tmp" || c.roots[2].Token != "/var/tmp" || c.roots[1].Path != "/tmp" || c.roots[2].Path != "/var/tmp" {
			t.Errorf("%s: tmp roots %+v", home, c.roots[1:])
		}
	}
}

func TestCursorDispatchOwnPayloadless(t *testing.T) {
	t.Parallel()
	// UT-A4: a lost answer of the payload-less coder dispatch is adopted by
	// its exact run-unique goal and acceptance; anything else is ambiguous.
	f := newFakeWorld(t)
	s := &supervisor{w: f.world(), ctx: context.Background(), known: map[string]string{}, plane: f.plane}
	f.plane.AddRole(context.Background(), contract.RoleConfig{ID: HopCoder, Name: HopCoder, Node: hexID("n_", 4), Adapter: "cursor", Instruction: "/m/i.md",
		Runbook: "/m/r.md", Model: "grok-4.7", Effort: "high", Concurrency: RoleConcurrency, Timeout: FeatureTaskBound, HasTimeout: true})
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: HopCoder}, Goal: "goal run-1", Acceptance: cursorProbeAcceptance,
		RequestedBy: requester}
	f.plane.dispatchLost = true
	id, err := s.dispatchOwn(req, f.clock.Now().Add(FeatureTaskBound))
	if err != nil || id != f.plane.ids()[0] {
		t.Fatalf("adopted %s %v", id, err)
	}
	other := req
	other.Goal = "goal run-2"
	f.plane.Dispatch(context.Background(), other)
	f.plane.Dispatch(context.Background(), req)
	f.plane.dispatchLost = true
	s.known = map[string]string{}
	if _, err := s.dispatchOwn(req, f.clock.Now().Add(FeatureTaskBound)); codeOf(err) != "dispatch_lost_ambiguous" {
		t.Fatalf("two matches adopted: %v", err)
	}
	if !sameOwnRequest(contract.DispatchRequest{Payload: []string{"m"}}, contract.DispatchRequest{Payload: []string{"m"}}) ||
		sameOwnRequest(contract.DispatchRequest{Goal: "g", Payload: []string{"m"}}, contract.DispatchRequest{Goal: "g"}) ||
		sameOwnRequest(contract.DispatchRequest{}, contract.DispatchRequest{Payload: []string{"m"}}) {
		t.Fatal("sameOwnRequest")
	}
}

func TestCursorCoderFixtureUnit(t *testing.T) {
	t.Parallel()
	// UT-A4: the exact fixture replacement, the envelope's pins and the
	// pre-dispatch prompt bound with escape-heavy text.
	good := mustRead(t, filepath.Join(repoRoot(t), filepath.FromSlash(devProbeFixtureDir), "main.go"))
	in, err := probeCoderInputMain(good)
	if err != nil || bytes.Count(in, []byte(`"Pending, %s!\n"`)) != 1 || len(in) != len(good)+2 || bytes.Contains(in, []byte(`"Hello, `)) {
		t.Fatalf("input %v", err)
	}
	if _, err := probeCoderInputMain(append(append([]byte{}, good...), good...)); err == nil {
		t.Fatal("two greeting lines accepted")
	}
	if _, err := probeCoderInputMain(in); err == nil {
		t.Fatal("no greeting line accepted")
	}
	input := map[string][]byte{}
	for _, n := range probeCoderFixtureFiles {
		input[n] = []byte(n + "\n")
	}
	goal := cursorProbeEnvelope("run-1", WorkspaceRecord{Name: "e2e-probe-coder-1", Instance: "i"}, "base", input)
	for _, s := range []string{"run_id: run-1", "workspace: e2e-probe-coder-1 instance i", "input_commit: base", cursorProbeAuthority,
		"- main.go " + sha256Hex([]byte("main.go\n")), `the greeting line is fmt.Fprintf(stdout, "Pending, %s!\n", *name); replace exactly its "Pending,  prefix with "Hello, `,
		"main.go only", "100% pass"} {
		if !strings.Contains(goal, s) {
			t.Fatalf("goal lacks %q:\n%s", s, goal)
		}
	}
	if strings.Contains(cursorProbeAcceptance, "main_test.go and") || !strings.Contains(cursorProbeAcceptance, "Publish only main.go") ||
		cursorProbeAcceptance == devAcceptance(&TaskPlan{Role: HopCoder, Mode: modeImplement}) {
		t.Fatal("acceptance")
	}
	instr, runbook := bytes.Repeat([]byte("i"), 1000), bytes.Repeat([]byte("r"), 1000)
	if err := checkCursorProbeBudget(instr, runbook, goal, cursorProbeAcceptance); err != nil {
		t.Fatal(err)
	}
	// Raw size at the bound passes; one more byte, or an escape-heavy
	// goal whose encoding passes the bound, is refused (never truncated).
	fixed := 2000 + len(cursorProbeAcceptance) + devPromptReserve
	room := adapter.MaxCursorPromptBytes - fixed
	if room > contract.MaxGoalBytes {
		// The goal's own bound is reached first: exercise both bounds.
		if checkCursorProbeBudget(instr, runbook, strings.Repeat("g", contract.MaxGoalBytes), cursorProbeAcceptance) != nil ||
			checkCursorProbeBudget(instr, runbook, strings.Repeat("g", contract.MaxGoalBytes+1), cursorProbeAcceptance) == nil {
			t.Fatal("goal bound")
		}
		instr = bytes.Repeat([]byte("i"), 1000+room-contract.MaxGoalBytes)
		room = contract.MaxGoalBytes
	}
	if err := checkCursorProbeBudget(instr, runbook, strings.Repeat("g", room), cursorProbeAcceptance); err != nil {
		t.Fatalf("at the raw bound: %v", err)
	}
	if err := checkCursorProbeBudget(append(instr, 'x'), runbook, strings.Repeat("g", room), cursorProbeAcceptance); !errors.Is(err, errPromptBudget) {
		t.Fatalf("one byte over: %v", err)
	}
	if err := checkCursorProbeBudget(instr, runbook, strings.Repeat(`"`, room/2+1), cursorProbeAcceptance); !errors.Is(err, errPromptBudget) ||
		!strings.Contains(err.Error(), "encoded") {
		t.Fatalf("escape-heavy goal: %v", err)
	}
	if err := checkCursorProbeBudget(instr, runbook, goal, string([]byte{0xff})); !errors.Is(err, errPromptBudget) {
		t.Fatalf("invalid acceptance: %v", err)
	}
	if err := checkCursorProbeBudget([]byte{0xff}, runbook, goal, cursorProbeAcceptance); err == nil {
		t.Fatal("invalid manual")
	}
}

func TestCursorValidationVerdictUnit(t *testing.T) {
	t.Parallel()
	// UT-A4: the independent oracle's every refusal.
	pass := func() (finalValidation, DevCoverageRecord) {
		v := finalValidation{Test: TestRun{Toolchain: "go1"}, Cases: CasesRecord{Build: ExecRecord{Started: true}},
			Reqs: RequirementsRecord{RunSignature: true, TableTests: true, TestCallsRun: true, TestRunPassed: true}, Cover: &ExecRecord{Started: true}}
		for range FeatureCases {
			v.Cases.Cases = append(v.Cases.Cases, CaseResult{Pass: true})
		}
		return v, DevCoverageRecord{Coverage: &DevCoverage{Pass: true}}
	}
	if v, c := pass(); coderValidationVerdict(v, c) != nil {
		t.Fatal("a passing validation refused")
	}
	for name, c := range map[string]struct {
		code string
		edit func(v *finalValidation, c *DevCoverageRecord)
	}{
		"tests":         {"final_tests_failed", func(v *finalValidation, c *DevCoverageRecord) { v.Test.ExitCode = 1 }},
		"case":          {CodeCaseFailed, func(v *finalValidation, c *DevCoverageRecord) { v.Cases.Cases[3].Pass = false }},
		"mapping":       {CodeTestMapping, func(v *finalValidation, c *DevCoverageRecord) { v.Reqs.MissingTests = []string{"TestRun/name"} }},
		"json":          {CodeValidationFail, func(v *finalValidation, c *DevCoverageRecord) { v.Reqs.TestRunPassed = false }},
		"no_coverage":   {CodeCoverageUnobserved, func(v *finalValidation, c *DevCoverageRecord) { c.Coverage = nil }},
		"below":         {CodeCoverageBelow, func(v *finalValidation, c *DevCoverageRecord) { c.Coverage.Pass = false }},
		"no_cover_exec": {CodeCoverageUnobserved, func(v *finalValidation, c *DevCoverageRecord) { v.Cover = nil }},
	} {
		v, cov := pass()
		c.edit(&v, &cov)
		if err := coderValidationVerdict(v, cov); err == nil || !strings.HasPrefix(err.Error(), c.code) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCursorLayoutUnit(t *testing.T) {
	t.Parallel()
	// UT-A6: the diagnostic collector's closed layout admits exactly the
	// coder records, captures, repository and samples.
	for _, scope := range []string{probeScopeFull, probeScopeBoth} {
		l := cursorProbeLayout(scope)
		for _, rel := range []string{probeCoderDir + "/task.json", probeCoderDir + "/verdict.json", probeCoderDir + "/validation/coverage.json",
			capturedPath(probeCoderDir + "/input/main.go"), capturedPath(probeCoderDir + "/expected/main.go"), cursorActivityFiles[0], cursorActivityFiles[1],
			"probe/wake/01/task.json"} {
			if !l.file(rel) {
				t.Errorf("%s: %s refused", scope, rel)
			}
		}
		for _, rel := range []string{probeCoderDir + "/notes.txt", probeCoderDir + "/validation/other.json", capturedPath(probeCoderDir + "/input/x.go"),
			"environment/cursor-activity-extra.json", probeCoderDir + "/main.go", devQualificationFile + ".bak"} {
			if l.file(rel) {
				t.Errorf("%s: %s admitted", scope, rel)
			}
		}
		for _, rel := range []string{"probe/coder", probeCoderDir, probeCoderDir + "/validation", "workspace/coder", probeCoderRepoDir, "_captured/probe/coder/01/input"} {
			if !l.dir(rel) {
				t.Errorf("%s: dir %s refused", scope, rel)
			}
		}
		if !l.isRepoDir(probeCoderRepoDir) || l.dir("probe/coder/02") {
			t.Errorf("%s: repositories", scope)
		}
	}
	// The default probe's layout is untouched.
	if probeLayoutFor(devBundleV2, probeScopeFull).file(probeCoderDir + "/task.json") {
		t.Fatal("the default qualification layout admits the coder stage")
	}
}

func TestCursorObservationUnit(t *testing.T) {
	t.Parallel()
	// UT-A6: the offline refusal's precedence and its schema-error
	// behavior; the receipt's text and exit.
	write := func(t *testing.T, data []byte) string {
		dir := t.TempDir()
		if data != nil {
			os.WriteFile(filepath.Join(dir, cursorObservationFile), data, 0o600)
		}
		return dir
	}
	run := func(dir string) (int, DevReport, string) {
		var out, errb bytes.Buffer
		code := CheckEvidence(dir, &out, &errb)
		var rep DevReport
		json.Unmarshal(out.Bytes(), &rep)
		return code, rep, errb.String()
	}
	valid := []byte(`{"schema":"` + CursorObservationSchema + `","result":"OBSERVED"}`)
	if code, rep, errOut := run(write(t, valid)); code != 2 || rep.Error != CodeDiagnosticNotQualification || errOut != diagnosticNotQualificationError+"\n" {
		t.Fatalf("diagnostic %d %s %s", code, rep.Error, errOut)
	}
	for name, data := range map[string][]byte{
		"malformed":   []byte(`{"schema":`),
		"unknown":     []byte(`{"schema":"callsheet-cursor-probe-observation/v2"}`),
		"duplicate":   []byte(`{"schema":"a","schema":"b"}`),
		"not_utf8":    {0xff},
		"oversized":   append([]byte(`{"schema":"`+CursorObservationSchema+`","x":"`), bytes.Repeat([]byte("x"), maxTextFile)...),
		"schema_type": []byte(`{"schema":7}`),
	} {
		code, rep, errOut := run(write(t, data))
		if code != 2 || rep.Error != "evidence_schema_error" || !strings.Contains(errOut, "evidence schema error") ||
			strings.Contains(errOut, CodeDiagnosticNotQualification) {
			t.Errorf("%s: %d %s %s", name, code, rep.Error, errOut)
		}
	}
	// A symlink or directory in its place is unsafe: a schema error.
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "x.json")
	os.WriteFile(target, valid, 0o600)
	os.Symlink(target, filepath.Join(dir, cursorObservationFile))
	if code, rep, _ := run(dir); code != 2 || rep.Error != "evidence_schema_error" {
		t.Fatalf("symlink %d %s", code, rep.Error)
	}
	dir2 := t.TempDir()
	os.Mkdir(filepath.Join(dir2, cursorObservationFile), 0o700)
	if code, rep, _ := run(dir2); code != 2 || rep.Error != "evidence_schema_error" {
		t.Fatalf("directory %d %s", code, rep.Error)
	}
	// A missing observation keeps the existing routing (here: the legacy
	// checker's missing manifest), and an unsafe root its existing error.
	var out, errb bytes.Buffer
	if code := CheckEvidence(t.TempDir(), &out, &errb); code == 2 && strings.Contains(errb.String(), CodeDiagnosticNotQualification) {
		t.Fatal("an empty bundle was refused as diagnostic")
	}
	if code, rep, errOut := run(filepath.Join(t.TempDir(), "missing")); code != 2 || rep.Error != "evidence_schema_error" || !strings.Contains(errOut, "evidence root") {
		t.Fatalf("unsafe root %d %s", code, errOut)
	}
	// A safely resolved root reached through a symlinked ancestor works.
	link := filepath.Join(t.TempDir(), "l")
	real := write(t, valid)
	os.Symlink(real, link)
	if code, rep, _ := run(link); code != 2 || rep.Error != CodeDiagnosticNotQualification {
		t.Fatalf("linked root %d %s", code, rep.Error)
	}
	// A Cursor probe's exit lines (slice 3b replaced the diagnostic ones):
	// BLOCKED names its primary live cause first, then the independent
	// check; QUALIFIED only for a passed, checked record.
	for name, c := range map[string]struct {
		d    *devSupervisor
		rep  *DevCursorQualificationReport
		code int
		want []string
	}{
		"stopped":    {&devSupervisor{stopCode: CodeProbeCoderUnqualified}, &DevCursorQualificationReport{Result: ResultBlocked, Codes: []string{"not_qualified"}}, 1, []string{"qualification probe: BLOCKED (probe_coder_unqualified)", "independent check: BLOCKED (not_qualified)"}},
		"protected":  {&devSupervisor{protected: true, cleanupOK: true}, &DevCursorQualificationReport{Result: ResultBlocked, Codes: []string{"x"}}, 1, []string{"BLOCKED (" + CodeEnvModified + ")"}},
		"cleanup":    {&devSupervisor{}, &DevCursorQualificationReport{Result: ResultBlocked, Codes: []string{"x"}}, 1, []string{"BLOCKED (cleanup_incomplete)"}},
		"no_report":  {&devSupervisor{cleanupOK: true}, nil, 1, []string{"BLOCKED (evidence_collection_failed)"}},
		"check_only": {&devSupervisor{cleanupOK: true}, &DevCursorQualificationReport{Result: ResultBlocked, Codes: []string{CodeCursorFixtureMismatch}}, 1, []string{"BLOCKED (" + CodeCursorFixtureMismatch + ")"}},
		"schema":     {&devSupervisor{cleanupOK: true}, &DevCursorQualificationReport{Result: ResultBlocked, Codes: []string{}, Error: "evidence_schema_error"}, 1, []string{"BLOCKED (evidence_schema_error)"}},
		"unchecked":  {&devSupervisor{cleanupOK: true, passed: true}, &DevCursorQualificationReport{Result: ResultBlocked, Codes: []string{"x"}}, 1, []string{"BLOCKED (x)"}},
		"qualified":  {&devSupervisor{cleanupOK: true, passed: true}, &DevCursorQualificationReport{Result: ResultQualified, Codes: []string{}}, 0, []string{"qualification probe: QUALIFIED"}},
	} {
		var stdout bytes.Buffer
		c.d.s = &supervisor{stdout: &stdout}
		c.d.probe = &devProbe{cursor: &cursorProbe{report: c.rep}}
		if got := c.d.cursorQualificationExit(); got != c.code {
			t.Errorf("%s: exit %d", name, got)
		}
		for _, s := range c.want {
			if !strings.Contains(stdout.String(), s) {
				t.Errorf("%s: %q lacks %q", name, stdout.String(), s)
			}
		}
	}
	if firstCode(nil) != "" || firstCode([]string{"a", "b"}) != "a" || hashOrEmpty([20]byte{}) != "" {
		t.Fatal("helpers")
	}
}
