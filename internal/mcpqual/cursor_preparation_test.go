package mcpqual

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Design decoder-enrollment B3, UT-24 (FP-24): the per-case Cursor
// preparation of qualify, in process on the capture world's injected
// launches, reaper and clock (no process, no vendor, no network), with the
// fixture home and workspaces in a short real directory so the residue
// parent slug stays within its bound: production nil/zero wiring, the
// preparer's defaults and clamps, ordering and fresh workspaces, the exact
// permission bytes and mode, each refusal and each permission read, the
// budget, cleanup and output failures, and the strict report schema.

// shortRoot is a fresh directory directly under the resolved /tmp (a short,
// link-free path whose components the slug derivation accepts).
func shortRoot(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	d, err := os.MkdirTemp(base, "cq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// cuTranscript is the reviewed real Cursor success shape for caseID with
// the probe's result nonce, plus a prose echo.
func cuTranscript(caseID, nonce string) string {
	start := cuCall("started", cuID, map[string]any{"args": cuArgs(caseID, cuID)}, nil)
	done := cuCall("completed", cuID, map[string]any{"args": cuArgs(caseID, cuID), "result": cuSuccess(cuPayload(caseID, nonce))}, nil)
	return strings.Join([]string{cuInit, cuUser, cuThink, cuThinkEnd, start, done, cuProse(), cuEnd}, "\n") + "\n"
}

var promptCaseRe = regexp.MustCompile(`"case_id": "([^"]+)"`)

// cursorQualPlan is the filled short Cursor plan of the pinned version and
// its enrolled fixture (mutate edits the client).
func cursorQualPlan(t *testing.T, mutate func(c map[string]any)) (*Plan, []byte) {
	t.Helper()
	b := filledPlan(t, func(c map[string]any) {
		c["decoder_fixture"] = EnrolledFixtureID("cursor-jsonl", CursorRealVersion, EnrolledRealPlatform)
		if mutate != nil {
			mutate(c)
		}
	}, "cursor")
	p, err := ParsePlan(b, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return p, b
}

// cqRun is one in-process Cursor qualification.
type cqRun struct {
	ctx      context.Context // the run's context (nil: background)
	w        *capWorld
	r        *Runner
	rep      *Report
	err      error
	home     string
	mu       sync.Mutex
	sessions []string // each session's working directory
	seen     []string // what each session saw of its .cursor/cli.json
}

func (q *cqRun) kinds() []string {
	q.w.mu.Lock()
	defer q.w.mu.Unlock()
	var out []string
	for _, s := range q.w.launches {
		out = append(out, launchKind(s))
	}
	return out
}

// cursorQualify runs the pinned short Cursor plan with a well-behaved fake
// (workspace-only approval, the real success shape) unless setup changes
// it; outFor (nil: "out") names the output relative to a short root.
func cursorQualify(t *testing.T, outFor func(root string) string, setup func(q *cqRun)) *cqRun {
	t.Helper()
	root := shortRoot(t)
	out := "out"
	if outFor != nil {
		out = outFor(root)
	}
	plan, raw := cursorQualPlan(t, nil)
	q := &cqRun{w: newCapWorld(t), home: filepath.Join(root, "h")}
	os.MkdirAll(filepath.Join(q.home, ".cursor"), 0o700)
	const nonce = "noncecq1"
	q.w.script = func(spec ProcSpec) capBehavior {
		b := q.w.defaults(spec)
		if launchKind(spec) == "session" {
			caseID := ""
			for _, a := range spec.Args {
				if m := promptCaseRe.FindStringSubmatch(a); m != nil {
					caseID = m[1]
				}
			}
			info, err := os.Lstat(filepath.Join(spec.Dir, ".cursor", "cli.json"))
			got := "<absent>"
			if err == nil {
				c, _ := os.ReadFile(filepath.Join(spec.Dir, ".cursor", "cli.json"))
				got = info.Mode().Perm().String() + " " + string(c)
			}
			q.mu.Lock()
			q.sessions, q.seen = append(q.sessions, spec.Dir), append(q.seen, got)
			q.mu.Unlock()
			b.stdout = cuTranscript(caseID, nonce)
		}
		return b
	}
	q.r = &Runner{Plan: plan, PlanSHA256: sha256Hex(raw), OutDir: filepath.Join(root, out), AllowModelCalls: true, GOOS: "linux", GOARCH: "amd64",
		Hostname: "host-1", ServerPath: "/opt/mcpqual", BaseEnv: []string{"PATH=/usr/bin"}, Launcher: q.w, Reaper: q.w, Clock: q.w.clock,
		Registry: DefaultRegistry(), Log: io.Discard, RunID: "run-cq-1", Nonce: nonce, CaptureDate: "2026-10-09", HarnessVersion: "test", Home: q.home,
		HashFile: func(string) (string, error) { return strings.Repeat("c", 64), nil }}
	if setup != nil {
		setup(q)
	}
	ctx := q.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	q.rep, q.err = q.r.Run(ctx)
	return q
}

// cqCases are the report's cases of the cursor client.
func cqCases(rep *Report) []CaseReport {
	var out []CaseReport
	for _, c := range rep.Clients {
		for _, ph := range c.Phases {
			out = append(out, ph.Cases...)
		}
	}
	return out
}

func TestCursorPreparerConstructor(t *testing.T) {
	p := newCursorPreparer(nil, ApprovalScanLimits{}, "linux", "amd64", CursorRealVersion, "/h")
	if _, ok := p.fsys.(osApprovalFS); !ok || p.limits != DefaultApprovalScanLimits() || !p.applies() {
		t.Fatalf("defaults %+v", p)
	}
	d := DefaultApprovalScanLimits()
	p = newCursorPreparer(newMemFS(), ApprovalScanLimits{Entries: d.Entries + 1, Bytes: 5, FileBytes: d.FileBytes * 2}, "linux", "amd64", CursorRealVersion, "/h")
	if p.limits != (ApprovalScanLimits{Entries: d.Entries, Bytes: 5, FileBytes: d.FileBytes}) {
		t.Fatalf("clamped %+v", p.limits)
	}
	for _, id := range [][3]string{{"darwin", "arm64", CursorRealVersion}, {"linux", "arm64", CursorRealVersion}, {"linux", "amd64", "2026.10.02-0000000"}} {
		if newCursorPreparer(nil, ApprovalScanLimits{}, id[0], id[1], id[2], "/h").applies() {
			t.Fatalf("%v applies", id)
		}
	}
}

// Production wiring pin (design decoder-enrollment B3, FP-24, DW10
// precedent): the qualify runner the CLI builds leaves the preparation
// filesystem nil and the limits zero, and Env has no field that could carry
// an approval filesystem.
func TestQualifyProductionWiring(t *testing.T) {
	w := newCapWorld(t)
	env := capEnv(t, w)
	plan, raw := cursorQualPlan(t, nil)
	r := newQualifyRunner(env, plan, raw, "/abs/out", true, CleanupPolicy{Grace: CleanupGrace, Limit: CleanupLimit, Poll: CleanupPoll}, bytes.Repeat([]byte{7}, 22))
	if r.approvalFS != nil || r.ApprovalLimits != (ApprovalScanLimits{}) || r.Home != env.Home || r.GOOS != env.GOOS || r.OutDir != "/abs/out" || !r.AllowModelCalls {
		t.Fatalf("production runner %+v", r)
	}
	scan := reflect.TypeOf((*approvalFS)(nil)).Elem()
	et := reflect.TypeOf(Env{})
	for i := 0; i < et.NumField(); i++ {
		if f := et.Field(i); f.Type.Implements(scan) || strings.Contains(strings.ToLower(f.Name), "approval") {
			t.Fatalf("Env field %s could carry the preparation filesystem", f.Name)
		}
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(Plan{}), reflect.TypeOf(PlanClient{})} {
		for i := 0; i < typ.NumField(); i++ {
			if n := strings.ToLower(typ.Field(i).Name); strings.Contains(n, "approval") || strings.Contains(n, "residue") {
				t.Fatalf("%s.%s reaches the preparation", typ.Name(), typ.Field(i).Name)
			}
		}
	}
}

func TestCursorQualifyPreparationFlow(t *testing.T) {
	q := cursorQualify(t, nil, nil)
	if q.err != nil {
		t.Fatal(q.err)
	}
	cases := cqCases(q.rep)
	if q.rep.Outcome != StatusConclusive || len(cases) != 3 || !slices.Equal(q.kinds(), []string{"version", "help", "enable", "session", "enable", "session", "enable", "session"}) {
		t.Fatalf("flow %s %d %v", q.rep.Outcome, len(cases), q.kinds())
	}
	dirs := map[string]bool{}
	for i, cs := range cases {
		p := cs.CursorPreparation
		if cs.Outcome != KindToolResult || p == nil || p.State != PreparationVerified || p.Reason != nil || p.ToolPermission == nil ||
			p.ToolPermission.State != PermissionVerified || p.PermissionChecks != (PermissionChecks{true, true, true}) || p.Approval.Scope != ScopeWorkspaceOnly ||
			p.ApprovalStdout == nil || *p.ApprovalStdout != "cases/"+cs.CaseID+"/approval-stdout.txt" || p.Residue.State != ResidueVerified || len(p.Residue.Entries) != 0 {
			t.Fatalf("case %d %+v %+v", i, cs, p)
		}
		// The exact bytes, 0600, in each case's own fresh workspace; the
		// session saw them, never a home or another case's file.
		if q.seen[i] != "-rw------- "+CursorToolPermissionContent || dirs[q.sessions[i]] || filepath.Base(q.sessions[i]) != cs.CaseID {
			t.Fatalf("session %d saw %q in %s", i, q.seen[i], q.sessions[i])
		}
		dirs[q.sessions[i]] = true
	}
	b, _ := os.ReadFile(filepath.Join(q.r.OutDir, "report.json"))
	rep, err := ParseReport(b)
	if err != nil || rep.CheckEvidence(q.r.OutDir) != nil {
		t.Fatalf("report roundtrip: %v", err)
	}
	filepath.WalkDir(q.home, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "cli.json" {
			t.Fatalf("a home permission file %s", p)
		}
		return nil
	})
}

// openHookFS is the operating system's approval view that runs mutate on
// the nth open of a path ending in suffix (the scanner's digests and the
// permission reads both open the file, in a fixed order).
type openHookFS struct {
	osApprovalFS
	suffix string
	n      int
	mutate func(p string)
	mu     sync.Mutex
	opens  int
}

func (f *openHookFS) Open(p string) (approvalFile, error) {
	if strings.HasSuffix(p, f.suffix) {
		f.mu.Lock()
		f.opens++
		hit := f.opens == f.n
		f.mu.Unlock()
		if hit {
			f.mutate(p)
		}
	}
	return f.osApprovalFS.Open(p)
}

func TestCursorQualifyPreparationRefusals(t *testing.T) {
	enable := func(b capBehavior) func(q *cqRun) {
		return func(q *cqRun) {
			def := q.w.script
			q.w.script = func(spec ProcSpec) capBehavior {
				if launchKind(spec) == "enable" {
					return b
				}
				return def(spec)
			}
		}
	}
	for name, tc := range map[string]struct {
		setup          func(q *cqRun)
		want           string
		enables, model int
	}{
		"existing-cli": {func(q *cqRun) {
			def := q.w.script
			q.w.script = func(spec ProcSpec) capBehavior {
				b := def(spec)
				if launchKind(spec) == "help" {
					b.writes = map[string]*string{"../cursor-setup/.cursor/cli.json": strp(`{"permissions":{"deny":["Shell(rm)"]}}`)}
				}
				return b
			}
		}, "a cli.json already exists", 0, 0},
		"no-change": {enable(capBehavior{stdout: "probe enabled\n"}), "no change gives positive evidence", 1, 0},
		"enable-rewrites-permission": {enable(capBehavior{stdout: "probe enabled\n",
			writes: map[string]*string{approvedFile: strp(`{}`), ".cursor/cli.json": strp(`{"permissions":{"allow":["Mcp(*:*)"],"deny":[]}}`)}}), "changed during the enable command", 1, 0},
		"enable-fails": {enable(capBehavior{stderr: "error\n", exit: 1, writes: map[string]*string{approvedFile: strp(`{}`)}}), ReasonCursorApprovalFailed, 1, 0},
		"outside":      {enable(capBehavior{stdout: "ok\n", writes: map[string]*string{"../.cursor/x.json": strp(`{}`)}}), ReasonCursorOutside, 1, 0},
		"config-removed": {enable(capBehavior{stdout: "ok\n", writes: map[string]*string{approvedFile: strp(`{}`), ".cursor/mcp.json": nil}}),
			ReasonConfigFailure + ": the generated .cursor/mcp.json cannot be re-read", 1, 0},
		"output-withheld": {enable(capBehavior{stdout: "Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789\n", writes: map[string]*string{approvedFile: strp(`{}`)}}),
			"cannot be retained faithfully", 1, 0},
		"approval-cleanup": {func(q *cqRun) {
			q.w.reapFail = func(pgid int) bool { return q.w.kindOf(pgid) == "enable" }
		}, ReasonCleanupFailed, 1, 0},
		"budget": {func(q *cqRun) {
			q.w.onReap = func(spec ProcSpec) {
				if launchKind(spec) == "enable" {
					q.w.clock.Advance(121 * 1e9)
				}
			}
		}, ReasonBudget + ": max_case_ms", 1, 0},
		"mutated-after-enable": {func(q *cqRun) {
			q.r.approvalFS = &openHookFS{suffix: "/.cursor/cli.json", n: 3, mutate: func(p string) { os.WriteFile(p, []byte(`{"permissions":{"allow":[],"deny":[]}}`), 0o600) }}
		}, permissionChanged, 1, 0},
		"mutated-before-session": {func(q *cqRun) {
			q.r.approvalFS = &openHookFS{suffix: "/.cursor/cli.json", n: 4, mutate: func(p string) { os.WriteFile(p, []byte(`{"permissions":{"allow":[],"deny":[]}}`), 0o600) }}
		}, permissionChanged, 1, 0},
		"mutated-during-session": {func(q *cqRun) {
			q.r.approvalFS = &openHookFS{suffix: "/.cursor/cli.json", n: 5, mutate: func(p string) { os.Remove(p) }}
		}, permissionChanged, 1, 1},
	} {
		q := cursorQualify(t, nil, tc.setup)
		if q.err != nil {
			t.Fatalf("%s: %v", name, q.err)
		}
		cases := cqCases(q.rep)
		kinds := q.kinds()
		count := func(k string) int {
			n := 0
			for _, x := range kinds {
				if x == k {
					n++
				}
			}
			return n
		}
		if len(cases) == 0 {
			t.Fatalf("%s: no case", name)
		}
		cs := cases[0]
		p := cs.CursorPreparation
		if q.rep.Outcome == StatusConclusive || cs.Outcome != OutcomeInconclusive || p == nil || p.State != PreparationUnverified || p.Reason == nil ||
			!strings.Contains(*p.Reason, tc.want) || !strings.Contains(deref(cs.Reason), tc.want) || count("enable") != tc.enables || count("session") != tc.model || len(cases) != 1 {
			t.Errorf("%s: %s %+v %+v %v", name, q.rep.Outcome, cs, p, kinds)
			continue
		}
		// No positive approval from the harness file; a written but
		// unverified permission keeps its record.
		if tp := p.ToolPermission; tp != nil && tp.State == PermissionVerified {
			t.Errorf("%s: verified permission %+v", name, tp)
		}
		b, _ := os.ReadFile(filepath.Join(q.r.OutDir, "report.json"))
		if _, err := ParseReport(b); err != nil {
			t.Errorf("%s: report %v", name, err)
		}
	}
}

// kindOf is the launch kind of process group pgid.
func (w *capWorld) kindOf(pgid int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s, ok := w.specs[pgid]; ok {
		return launchKind(s)
	}
	return ""
}

// The report schema (design decoder-enrollment B3, Report evidence and
// validation): strict members, nulls, types, cross-field rules, evidence
// path forgery, conclusive pinned cases without verified preparation
// (also after fixture, version or platform label changes), and legacy or
// other-client reports unchanged.
func TestCursorPreparationReport(t *testing.T) {
	q := cursorQualify(t, nil, nil)
	base, err := os.ReadFile(filepath.Join(q.r.OutDir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	edit := func(f func(rep, client, cs, prep map[string]any)) []byte {
		var m map[string]any
		json.Unmarshal(base, &m)
		c := m["clients"].([]any)[0].(map[string]any)
		cs := c["phases"].([]any)[0].(map[string]any)["cases"].([]any)[0].(map[string]any)
		f(m, c, cs, cs["cursor_preparation"].(map[string]any))
		b, _ := json.Marshal(m)
		return b
	}
	if _, err := ParseReport(base); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		b    []byte
		want string
	}{
		"null":            {edit(func(_, _, cs, _ map[string]any) { cs["cursor_preparation"] = nil }), "explicit null"},
		"unknown-member":  {edit(func(_, _, _, p map[string]any) { p["extra"] = 1 }), "unknown field"},
		"missing-member":  {edit(func(_, _, _, p map[string]any) { delete(p, "approval_stderr") }), "members"},
		"null-checks":     {edit(func(_, _, _, p map[string]any) { p["permission_checks"] = nil }), "permission_checks is null"},
		"check-string":    {edit(func(_, _, _, p map[string]any) { p["permission_checks"].(map[string]any)["after_enable"] = "true" }), "cannot unmarshal"},
		"check-missing":   {edit(func(_, _, _, p map[string]any) { delete(p["permission_checks"].(map[string]any), "after_cleanup") }), "members"},
		"residue-null":    {edit(func(_, _, _, p map[string]any) { p["residue"] = nil }), "residue is null"},
		"approval-policy": {edit(func(_, _, _, p map[string]any) { delete(p["approval"].(map[string]any), "inventory_policy") }), "inventory_policy"},
		"state":           {edit(func(_, _, _, p map[string]any) { p["state"] = "ok" }), "state"},
		"verified-reason": {edit(func(_, _, _, p map[string]any) { p["reason"] = "x" }), "reason exactly"},
		"unverified-null": {edit(func(_, _, _, p map[string]any) { p["state"] = "unverified" }), "reason exactly"},
		"perm-null-checks": {edit(func(_, _, _, p map[string]any) { p["tool_permission"] = nil }),
			"permission checks without"},
		"perm-content": {edit(func(_, _, _, p map[string]any) {
			p["tool_permission"].(map[string]any)["content"] = `{"permissions":{"allow":["Mcp(*:*)"],"deny":[]}}`
		}), "exact file"},
		"perm-unchecked": {edit(func(_, _, _, p map[string]any) { p["permission_checks"].(map[string]any)["after_cleanup"] = false }), "all three checks"},
		"checks-order": {edit(func(_, _, _, p map[string]any) {
			p["permission_checks"].(map[string]any)["after_enable"] = false
		}), "out of order"},
		"output-forged":  {edit(func(_, _, _, p map[string]any) { p["approval_stdout"] = "cases/other/approval-stdout.txt" }), "not the case's listed"},
		"output-escape":  {edit(func(_, _, _, p map[string]any) { p["approval_stdout"] = "../approval-stdout.txt" }), "not the case's listed"},
		"output-missing": {edit(func(_, _, _, p map[string]any) { p["approval_stdout"] = nil }), "exactly when the enable command launched"},
		"residue-policy": {edit(func(_, _, _, p map[string]any) { p["residue"].(map[string]any)["policy"] = "other" }), "residue: policy"},
		"residue-state":  {edit(func(_, _, _, p map[string]any) { p["residue"].(map[string]any)["state"] = "not_checked" }), "reason exactly"},
		"residue-future": {edit(func(_, _, _, p map[string]any) {
			p["residue"].(map[string]any)["entries"] = []any{map[string]any{"case_id": "cursor-default-repeat", "path": residuePath(1), "origin": ResidueOrigin,
				"prefix_verified": true, "suffix_verified": true, "socket_type_verified": true, "sole_entry_verified": true, "identity_verified": true}}
		}), "has not run"},
		"residue-flag": {edit(func(_, _, cs, p map[string]any) {
			p["residue"].(map[string]any)["entries"] = []any{map[string]any{"case_id": cs["case_id"], "path": residuePath(1), "origin": ResidueOrigin,
				"prefix_verified": true, "suffix_verified": false, "socket_type_verified": true, "sole_entry_verified": true, "identity_verified": true}}
		}), "verified with a failed check"},
		"residue-raw-path": {edit(func(_, _, cs, p map[string]any) {
			p["residue"].(map[string]any)["entries"] = []any{map[string]any{"case_id": cs["case_id"], "path": "<home>/.cursor/projects/tmp-x-work-cu-1b81996/worker.sock",
				"origin": ResidueOrigin, "prefix_verified": true, "suffix_verified": true, "socket_type_verified": true, "sole_entry_verified": true, "identity_verified": true}}
		}), "normalized residue path"},
		"unverified-success": {edit(func(_, _, _, p map[string]any) {
			p["state"], p["reason"] = "unverified", "x"
		}), "without verified cursor_preparation"},
		"absent-success": {edit(func(_, _, cs, _ map[string]any) { delete(cs, "cursor_preparation") }), "without verified cursor_preparation"},
		"darwin":         {edit(func(rep, _, _, _ map[string]any) { rep["os"] = "darwin" }), "only a case of the"},
		"other-fixture": {edit(func(_, c, cs, _ map[string]any) {
			c["expected_version"] = "2026.10.02-0000000"
			delete(cs, "cursor_preparation")
		}), "without verified cursor_preparation"},
		"verified-dirty-session": {edit(func(_, _, cs, _ map[string]any) {
			cs["exit"] = 1
			cs["probe_observation"].(map[string]any)["clean_session"] = false
		}), "clean session"},
	} {
		if _, err := ParseReport(tc.b); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A legacy report (no preparation) of a non-pinned Cursor version and a
	// pinned Cursor case that never became conclusive both parse.
	for name, b := range map[string][]byte{
		"legacy-synthetic": edit(func(_, c, _, _ map[string]any) {
			c["expected_version"], c["observed_version"], c["decoder_version"] = "2026.09.23-86fc751", "2026.09.23-86fc751", nil
			for _, ph := range c["phases"].([]any) {
				for _, x := range ph.(map[string]any)["cases"].([]any) {
					delete(x.(map[string]any), "cursor_preparation")
				}
			}
		}),
		"inconclusive-without": edit(func(_, _, cs, _ map[string]any) {
			delete(cs, "cursor_preparation")
			cs["outcome"], cs["reason"] = OutcomeInconclusive, "x"
		}),
	} {
		if _, err := ParseReport(b); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Design decoder-enrollment B3, UT-26 (FP-26): the confirmation's bounds
// and arithmetic (B stays 10 s; L = 15 s gives 12 s < 15 s), the schedule's
// three sessions within 120 s per case and 360 s per client including
// preparation, the parent-slug precondition at 54 (admitted) and 55
// (refused before any enable or model launch with the exact reason, the
// preparation evidence retained), and no session beyond the plan.
func TestCursorConfirmationBounds(t *testing.T) {
	if ShortPollBudget != 10e9 || !ShortPollCompatible(ShortPollBudget, 15e9) || ShortPollBudget+ShortPollMargin(15e9) != 12e9 || ShortPollCompatible(ShortPollBudget, 12e9) {
		t.Fatal("the short-poll bar")
	}
	plan, _ := cursorQualPlan(t, nil)
	lim := plan.EffectiveLimits()
	r := &Runner{Plan: plan, AllowModelCalls: true}
	if lim.MaxSessionsPerClient != 3 || lim.MaxCaseMS != 120000 || lim.MaxClientMS != 360000 || r.plannedBound().Sessions != 3 || r.plannedBound().WallMS != 360000 {
		t.Fatalf("limits %+v bound %+v", lim, r.plannedBound())
	}
	// P is the slug of <out>/.work: the root's components, the output name
	// and "work", joined by '-'.
	outOfP := func(n int) func(root string) string {
		return func(root string) string {
			rootSlug := strings.ReplaceAll(strings.TrimPrefix(root, "/"), "/", "-")
			return strings.Repeat("o", n-len(rootSlug)-len("-")-len("-work"))
		}
	}
	q := cursorQualify(t, outOfP(54), nil)
	if p := strings.ReplaceAll(strings.TrimPrefix(q.r.OutDir, "/"), "/", "-") + "-work"; len(p) != 54 {
		t.Fatalf("P %q", p)
	}
	if q.rep.Outcome != StatusConclusive || len(cqCases(q.rep)) != 3 {
		t.Fatalf("P=54: %s", q.rep.Outcome)
	}
	q = cursorQualify(t, outOfP(55), nil)
	cases := cqCases(q.rep)
	if len(cases) != 1 || deref(cases[0].Reason) != ReasonResidueParentSlug || cases[0].CursorPreparation == nil ||
		deref(cases[0].CursorPreparation.Reason) != ReasonResidueParentSlug || len(q.kinds()) != 2 || q.rep.Outcome == StatusConclusive {
		t.Fatalf("P=55: %+v %v", cases, q.kinds())
	}
	// The setup is not conclusive, so the 15 s case never runs; nothing
	// was written into the workspace before the refusal.
	if setup := q.rep.Clients[0].Phases[0]; setup.Status == StatusConclusive || q.rep.Clients[0].Phases[1].Status != StatusNotRun {
		t.Fatalf("phases %+v", q.rep.Clients[0].Phases)
	}
}

// Code review B3 round 1, C2: the report's evidence (the approval outputs
// and every other listed file, the input of ProposePatch and Publish) is
// read only through the confined, bounded, no-follow reader: a symbolic
// link to identical bytes outside the report, a linked parent directory, a
// special file in place of a file and oversized evidence are all refused
// before any hash is trusted; an untouched copy still passes.
func TestCheckEvidenceConfined(t *testing.T) {
	q := cursorQualify(t, nil, nil)
	if q.err != nil || q.rep.Outcome != StatusConclusive {
		t.Fatalf("run %v %s", q.err, q.rep.Outcome)
	}
	if err := q.rep.CheckEvidence(q.r.OutDir); err != nil {
		t.Fatal(err)
	}
	stdout := filepath.Join("cases", "cursor-setup", FileApprovalStdout)
	for name, tc := range map[string]struct {
		mutate func(out, outside string)
		want   string
	}{
		"file-symlink": {func(out, outside string) {
			p := filepath.Join(out, stdout)
			b, _ := os.ReadFile(p)
			os.WriteFile(filepath.Join(outside, "same.txt"), b, 0o600)
			os.Remove(p)
			os.Symlink(filepath.Join(outside, "same.txt"), p)
		}, "symbolic link"},
		"parent-symlink": {func(out, outside string) {
			dir := filepath.Join(out, "cases", "cursor-setup")
			copyDirForTest(t, dir, filepath.Join(outside, "cursor-setup"))
			os.RemoveAll(dir)
			os.Symlink(filepath.Join(outside, "cursor-setup"), dir)
		}, "symbolic link"},
		"relative-escape": {func(out, outside string) {
			p := filepath.Join(out, stdout)
			b, _ := os.ReadFile(p)
			os.WriteFile(filepath.Join(outside, "same.txt"), b, 0o600)
			os.Remove(p)
			rel, _ := filepath.Rel(filepath.Dir(p), filepath.Join(outside, "same.txt"))
			os.Symlink(rel, p)
		}, "evidence"},
		"fifo": {func(out, _ string) {
			p := filepath.Join(out, stdout)
			os.Remove(p)
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		"oversized": {func(out, _ string) {
			f, _ := os.OpenFile(filepath.Join(out, "cases", "cursor-setup", "vendor-events.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
			f.Write(bytes.Repeat([]byte("x"), 1<<20))
			f.Close()
		}, "exceeds"},
	} {
		cp := filepath.Join(realDir(t), "copy")
		outside := realDir(t)
		copyDirForTest(t, q.r.OutDir, cp)
		if err := q.rep.CheckEvidence(cp); err != nil {
			t.Fatalf("%s: an untouched copy: %v", name, err)
		}
		tc.mutate(cp, outside)
		done := make(chan error, 1)
		go func() { done <- q.rep.CheckEvidence(cp) }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: %v", name, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: the evidence check blocked", name)
		}
	}
}

// dirHookFS is the operating system's approval view that runs hook on the
// nth listing of the directory dir.
type dirHookFS struct {
	osApprovalFS
	dir   string
	n     int
	hook  func()
	mu    sync.Mutex
	lists int
}

func (f *dirHookFS) ReadDir(p string) ([]fs.DirEntry, error) {
	if p == f.dir {
		f.mu.Lock()
		f.lists++
		hit := f.lists == f.n
		f.mu.Unlock()
		if hit {
			f.hook()
		}
	}
	return f.osApprovalFS.ReadDir(p)
}

// Code review B3 round 1, C1: the case budget is checked after every
// preparation, residue and final-verification operation, and the model's
// allowance is computed only after them. A filesystem operation that
// exhausts the case's 120 s (or the client's 360 s) through the existing
// seam (the fake clock advanced inside the injected filesystem view) stops
// before the model launch, or, during the final verification, leaves the
// case inconclusive: never a model launch after expiry, never a Qualified
// result.
func TestCursorQualifyPreparationDeadline(t *testing.T) {
	const caseBudget, clientBudget = 121e9, 361e9
	for name, tc := range map[string]struct {
		view     func(q *cqRun, advance func()) approvalFS
		skip     float64
		want     string
		sessions int
	}{
		"after-enable-read": {func(q *cqRun, advance func()) approvalFS {
			return &openHookFS{suffix: "/.cursor/cli.json", n: 3, mutate: func(string) { advance() }}
		}, caseBudget, ReasonBudget + ": max_case_ms", 0},
		"before-session-read": {func(q *cqRun, advance func()) approvalFS {
			return &openHookFS{suffix: "/.cursor/cli.json", n: 4, mutate: func(string) { advance() }}
		}, caseBudget, ReasonBudget + ": max_case_ms", 0},
		"before-session-client": {func(q *cqRun, advance func()) approvalFS {
			return &openHookFS{suffix: "/.cursor/cli.json", n: 4, mutate: func(string) { advance() }}
		}, clientBudget, ReasonClientBudget, 0},
		"residue-baseline": {func(q *cqRun, advance func()) approvalFS {
			// The projects root is listed by the approval inventories before
			// and after the enable, then by the residue baseline.
			return &dirHookFS{dir: filepath.Join(q.home, ".cursor", "projects"), n: 3, hook: advance}
		}, caseBudget, ReasonBudget + ": max_case_ms", 0},
		"after-cleanup-read": {func(q *cqRun, advance func()) approvalFS {
			return &openHookFS{suffix: "/.cursor/cli.json", n: 5, mutate: func(string) { advance() }}
		}, caseBudget, ReasonBudget + ": max_case_ms", 1},
	} {
		q := cursorQualify(t, nil, func(q *cqRun) {
			os.MkdirAll(filepath.Join(q.home, ".cursor", "projects"), 0o700)
			q.r.approvalFS = tc.view(q, func() { q.w.clock.Advance(time.Duration(tc.skip)) })
		})
		if q.err != nil {
			t.Fatalf("%s: %v", name, q.err)
		}
		sessions := 0
		for _, k := range q.kinds() {
			if k == "session" {
				sessions++
			}
		}
		cases := cqCases(q.rep)
		if q.rep.Outcome == StatusConclusive || sessions != tc.sessions || len(cases) != 1 {
			t.Errorf("%s: %s, %d model launches, %d cases: %v", name, q.rep.Outcome, sessions, len(cases), q.kinds())
			continue
		}
		cs := cases[0]
		p := cs.CursorPreparation
		if cs.Outcome != OutcomeInconclusive || deref(cs.Reason) != tc.want || p == nil || p.State != PreparationUnverified || deref(p.Reason) != tc.want {
			t.Errorf("%s: case %s %q, preparation %+v", name, cs.Outcome, deref(cs.Reason), p)
		}
		if tc.sessions == 1 && p.Residue.State != ResidueNotChecked {
			t.Errorf("%s: residue checked after the budget expired: %+v", name, p.Residue)
		}
		b, _ := os.ReadFile(filepath.Join(q.r.OutDir, "report.json"))
		if _, err := ParseReport(b); err != nil {
			t.Errorf("%s: report %v", name, err)
		}
	}
	// A cancellation between operations is an interruption, launching
	// nothing further.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := cursorQualify(t, nil, func(q *cqRun) {
		q.ctx = ctx
		q.r.approvalFS = &openHookFS{suffix: "/.cursor/cli.json", n: 4, mutate: func(string) { cancel() }}
	})
	cases := cqCases(q.rep)
	if q.err != nil || len(cases) != 1 || cases[0].Outcome != OutcomeInterrupted || deref(cases[0].CursorPreparation.Reason) != ReasonInterrupted ||
		slices.Contains(q.kinds(), "session") {
		t.Fatalf("cancellation: %v %+v %v", q.err, cases, q.kinds())
	}
}
