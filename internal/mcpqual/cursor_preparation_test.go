package mcpqual

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
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
			// A3.1: the walk around the inventory after the enable is the first
			// to observe the expiry and records its fixed reason.
		}, ReasonForeignDeadline, 1, 0},
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
			p["residue"].(map[string]any)["entries"] = []any{cqEntry("cursor-default-repeat", residuePath(1), nil)}
		}), "has not run"},
		"residue-flag": {edit(func(_, _, cs, p map[string]any) {
			p["residue"].(map[string]any)["entries"] = []any{cqEntry(cs["case_id"], residuePath(1), func(e map[string]any) { e["layout_verified"] = false })}
		}), "verified with a failed check"},
		"residue-raw-path": {edit(func(_, _, cs, p map[string]any) {
			p["residue"].(map[string]any)["entries"] = []any{cqEntry(cs["case_id"], "<home>/.cursor/projects/tmp-x-work-cu-1b81996/worker.sock", nil)}
		}), "normalized residue path"},
		"residue-v1-members": {edit(func(_, _, cs, p map[string]any) {
			p["residue"].(map[string]any)["entries"] = []any{map[string]any{"case_id": cs["case_id"], "path": residuePath(1), "origin": ResidueOrigin,
				"prefix_verified": true, "suffix_verified": true, "socket_type_verified": true, "sole_entry_verified": true, "identity_verified": true}}
		}), "members 8"},
		"residue-v1-in-place": {edit(func(_, _, cs, p map[string]any) {
			p["residue"].(map[string]any)["policy"] = WorkerResiduePolicy
			p["residue"].(map[string]any)["entries"] = []any{cqEntry(cs["case_id"], residuePath(1), nil)}
		}), "members 9"},
		"residue-in-place-workspace-only": {edit(func(_, _, cs, p map[string]any) {
			p["residue"].(map[string]any)["entries"] = []any{cqEntry(cs["case_id"], residuePath(1), func(e map[string]any) {
				e["layout"], e["approval_unchanged"] = ResidueLayoutInPlace, true
				e["items"] = jsonAny(rsInPlaceEntry("x", 1).Items)
			})}
		}), "no project_scoped approval"},
		"residue-item-member": {edit(func(_, _, cs, p map[string]any) {
			p["residue"].(map[string]any)["entries"] = []any{cqEntry(cs["case_id"], residuePath(1), func(e map[string]any) {
				e["items"].([]any)[0].(map[string]any)["inode"] = 7
			})}
		}), "unknown field"},
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

// Design decoder-enrollment B3, UT-26 (FP-26), as amended by A3: the
// confirmation's bounds and arithmetic (B stays 10 s; L = 15 s gives 12 s <
// 15 s), the schedule's three sessions within 120 s per case and 360 s per
// client including preparation, and no session beyond the plan. A3 replaces
// the old unconditional pre-launch parent-slug refusal: a long P prepares
// and runs normally; only an observed hashed candidate is capped (P=54
// admitted, P=55 refused after the session's cleanup with the exact
// reason), and an in_place tree or no residue needs no cap. A3.1: at P=54
// the first case's admitted hashed directory is the next case's possible
// hashed-candidate directory, refused before that case's enable.
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
	residue := func(layout string) func(q *cqRun) {
		return func(q *cqRun) { cqResidue(t, q, func(string) string { return layout }) }
	}
	for _, tc := range []struct {
		n          int
		layout     string
		conclusive bool
	}{
		{54, "", true}, {55, "", true}, {55, "in_place", true}, {80, "in_place", true}, {80, "", true},
		{55, "hashed", false}, {54, "hashed", false},
	} {
		q := cursorQualify(t, outOfP(tc.n), residue(tc.layout))
		if p := strings.ReplaceAll(strings.TrimPrefix(q.r.OutDir, "/"), "/", "-") + "-work"; len(p) != tc.n {
			t.Fatalf("P %q", p)
		}
		cases := cqCases(q.rep)
		label := fmt.Sprintf("P=%d %q", tc.n, tc.layout)
		if q.err != nil || (q.rep.Outcome == StatusConclusive) != tc.conclusive {
			t.Fatalf("%s: %v %s %v", label, q.err, q.rep.Outcome, q.kinds())
		}
		if !tc.conclusive && tc.n == 54 {
			// A3.1: the first case's hashed residue is admitted (P=54), but
			// its name is the second case's possible hashed-candidate
			// directory (S[:57]-hex7, the ledger never excuses it): the second
			// case is refused before its enable with the exact reason.
			if len(cases) != 2 || cases[0].CursorPreparation.State != PreparationVerified ||
				cases[0].CursorPreparation.Residue.Entries[0].Layout != ResidueLayoutHashed || deref(cases[1].Reason) != ReasonHashedCandidateExists ||
				deref(cases[1].CursorPreparation.Approval.Reason) != ReasonHashedCandidateExists ||
				!slices.Equal(q.kinds(), []string{"version", "help", "enable", "session"}) {
				t.Fatalf("%s: %+v %v", label, cases, q.kinds())
			}
			continue
		}
		if !tc.conclusive {
			// Refused only after the session's cleanup (never before enable or
			// the model), with the verbatim reason; the 15 s case never runs.
			cs := cases[0]
			if len(cases) != 1 || deref(cs.Reason) != ReasonResidueParentSlug || deref(cs.CursorPreparation.Reason) != ReasonResidueParentSlug ||
				deref(cs.CursorPreparation.Residue.Reason) != ReasonResidueParentSlug || !slices.Equal(q.kinds(), []string{"version", "help", "enable", "session"}) ||
				q.rep.Clients[0].Phases[1].Status != StatusNotRun {
				t.Fatalf("%s: %+v %v", label, cs, q.kinds())
			}
			continue
		}
		if len(cases) != 3 || !slices.Equal(q.kinds(), []string{"version", "help", "enable", "session", "enable", "session", "enable", "session"}) {
			t.Fatalf("%s: %d cases %v", label, len(cases), q.kinds())
		}
		for i, cs := range cases {
			res := cs.CursorPreparation.Residue
			want := 0
			if tc.layout != "" {
				want = i + 1
			}
			if res.State != ResidueVerified || len(res.Entries) != want || want > 0 && res.Entries[i].Layout != map[string]string{"hashed": ResidueLayoutHashed, "in_place": ResidueLayoutInPlace}[tc.layout] {
				t.Fatalf("%s case %d: %+v", label, i, res)
			}
		}
	}
}

// A3 across the confirmation (in process): three freshly prepared cases
// with in_place residue, the report's cross-case residue rules (a path
// keeps its owner, layout and items, never reappears), the in_place owner's
// project_scoped provenance, and mixed, partial and changed-approval
// sessions failing closed with nothing of the session files in evidence.
func TestCursorPreparationResidueAcrossCases(t *testing.T) {
	// The real operating-system view, counting every open: the later
	// cases' enable inventories (before and after) and safety scans never
	// open a session file; the approvals are read.
	spy := &openCountFS{}
	q := cursorQualify(t, nil, func(q *cqRun) {
		cqResidue(t, q, func(string) string { return "in_place" })
		q.r.approvalFS = spy
	})
	if q.err != nil || q.rep.Outcome != StatusConclusive {
		t.Fatalf("in_place run: %v %s", q.err, q.rep.Outcome)
	}
	if spy.session != 0 || spy.approval == 0 {
		t.Fatalf("opens: %d session files, %d approvals", spy.session, spy.approval)
	}
	base, _ := os.ReadFile(filepath.Join(q.r.OutDir, "report.json"))
	for _, raw := range []string{rsCanary, rsUUID, "-work-cursor-setup"} {
		if bytes.Contains(base, []byte(raw)) {
			t.Fatalf("%q in the report", raw)
		}
	}
	if _, err := ParseReport(base); err != nil {
		t.Fatal(err)
	}
	edit := func(f func(cases []map[string]any)) []byte {
		var m map[string]any
		json.Unmarshal(base, &m)
		var cases []map[string]any
		for _, ph := range m["clients"].([]any)[0].(map[string]any)["phases"].([]any) {
			for _, c := range ph.(map[string]any)["cases"].([]any) {
				cases = append(cases, c.(map[string]any))
			}
		}
		f(cases)
		b, _ := json.Marshal(m)
		return b
	}
	entries := func(c map[string]any) []any {
		return c["cursor_preparation"].(map[string]any)["residue"].(map[string]any)["entries"].([]any)
	}
	for name, tc := range map[string]struct {
		b    []byte
		want string
	}{
		"owner": {edit(func(cs []map[string]any) { entries(cs[1])[0].(map[string]any)["case_id"] = "cursor-default-1" }), "owns more than one"},
		"owner-later": {edit(func(cs []map[string]any) {
			e := entries(cs[2])
			e[0].(map[string]any)["case_id"], e[1].(map[string]any)["case_id"] = "cursor-default-1", "cursor-setup"
		}), "changes its owning case"},
		"layout": {edit(func(cs []map[string]any) {
			e := entries(cs[1])[0].(map[string]any)
			e["layout"], e["approval_unchanged"], e["items"] = ResidueLayoutHashed, nil, jsonAny(rsHashedEntry("x", 1).Items)
		}), "changes its layout or items"},
		"items": {edit(func(cs []map[string]any) {
			entries(cs[2])[0].(map[string]any)["items"].([]any)[6].(map[string]any)["size"] = 1
		}), "changes its layout or items"},
		"reappears": {edit(func(cs []map[string]any) {
			r := cs[1]["cursor_preparation"].(map[string]any)["residue"].(map[string]any)
			r["entries"] = []any{}
			r["state"], r["reason"] = ResidueUnverified, "x"
			cs[1]["cursor_preparation"].(map[string]any)["state"], cs[1]["cursor_preparation"].(map[string]any)["reason"] = PreparationUnverified, "x"
			cs[1]["outcome"], cs[1]["reason"] = OutcomeInconclusive, "x"
		}), "reappears"},
		"v1-in-place": {edit(func(cs []map[string]any) {
			cs[0]["cursor_preparation"].(map[string]any)["residue"].(map[string]any)["policy"] = WorkerResiduePolicy
		}), "members 9"},
	} {
		if _, err := ParseReport(tc.b); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A hashed variant: three cases, one entry each, P within the cap.
	q = cursorQualify(t, nil, func(q *cqRun) { cqResidue(t, q, func(string) string { return "hashed" }) })
	if q.err != nil || q.rep.Outcome != StatusConclusive || len(cqCases(q.rep)[2].CursorPreparation.Residue.Entries) != 3 {
		t.Fatalf("hashed run: %v %s", q.err, q.rep.Outcome)
	}
	// Mixed and partial sessions, and an approval changed during the
	// session, fail closed after the session: inconclusive, nothing admitted,
	// nothing of the session files in evidence, no further case.
	for name, tc := range map[string]struct {
		layout string
		change func(q *cqRun)
		want   string
	}{
		"mixed":   {"mixed", nil, "both residue layouts"},
		"partial": {"partial", nil, "not the exact in-place layout"},
		"approval-changed": {"in_place", func(q *cqRun) {
			prev := q.w.onReap
			q.w.onReap = func(spec ProcSpec) {
				prev(spec)
				if launchKind(spec) == "session" {
					slug, _ := cursorProjectSlug(osApprovalFS{}, "linux", "amd64", CursorRealVersion, spec.Dir)
					os.WriteFile(filepath.Join(q.home, ".cursor", "projects", slug, cursorApprovalsFile), []byte(`["probe-6e58c4b6c129cbd1"]`), 0o600)
				}
			}
		}, "approval file"},
	} {
		q := cursorQualify(t, nil, func(q *cqRun) {
			cqResidue(t, q, func(string) string { return tc.layout })
			if tc.change != nil {
				tc.change(q)
			}
		})
		cases := cqCases(q.rep)
		if q.err != nil || q.rep.Outcome == StatusConclusive || len(cases) != 1 || !strings.Contains(deref(cases[0].Reason), tc.want) ||
			len(cases[0].CursorPreparation.Residue.Entries) != 0 || cases[0].CursorPreparation.Residue.State != ResidueUnverified {
			t.Errorf("%s: %v %s %+v", name, q.err, q.rep.Outcome, cases)
			continue
		}
		b, _ := os.ReadFile(filepath.Join(q.r.OutDir, "report.json"))
		if _, err := ParseReport(b); err != nil || bytes.Contains(b, []byte(rsCanary)) || bytes.Contains(b, []byte(rsUUID)) {
			t.Errorf("%s: report %v", name, err)
		}
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

// openCountFS is the operating system's approval view counting opens of
// session files (by name, anywhere) and of approval files.
type openCountFS struct {
	osApprovalFS
	mu                sync.Mutex
	session, approval int
}

func (f *openCountFS) Open(p string) (approvalFile, error) {
	f.mu.Lock()
	switch base := filepath.Base(p); {
	case base == cursorApprovalsFile:
		f.approval++
	case slices.Contains(sessionArtifactNames, base) || strings.HasSuffix(base, ".jsonl") && strings.Contains(p, "/"+inPlaceTranscripts+"/"):
		f.session++
	}
	f.mu.Unlock()
	return f.osApprovalFS.Open(p)
}

// cqEntry is a verified v2 hashed residue entry as report JSON (edit
// changes it).
func cqEntry(caseID any, path string, edit func(e map[string]any)) map[string]any {
	e := map[string]any{"case_id": caseID, "path": path, "origin": ResidueOrigin, "layout": ResidueLayoutHashed, "identity_verified": true,
		"socket_type_verified": true, "layout_verified": true, "approval_unchanged": nil,
		"items": []any{map[string]any{"path": cursorWorkerSocket, "type": ResidueItemSocket, "size": 0}}}
	if edit != nil {
		edit(e)
	}
	return e
}

// jsonAny is v as generic JSON.
func jsonAny(v any) any {
	b, _ := json.Marshal(v)
	var out any
	json.Unmarshal(b, &out)
	return out
}

// bindSocketEnv names the socket a helper run of this test binary binds
// in its working directory.
const bindSocketEnv = "MCPQUAL_TEST_BIND_SOCKET"

// bindSocketHere binds the Unix socket name (relative: the working
// directory may exceed the platform's socket path limit) in the working
// directory and closes it without unlinking: a socket no process holds.
func bindSocketHere(name string) int {
	l, err := net.Listen("unix", name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ul := l.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	ul.Close()
	return 0
}

// cqSocketPortable selects cqSocket's non-Linux path (a variable so that
// its cost can be measured on Linux too).
var cqSocketPortable = runtime.GOOS != "linux"

// maxSocketPath is the longest socket path bound directly (sun_path holds
// 104 bytes with its terminator on darwin, 108 on Linux).
const maxSocketPath = 100

// cqSocket creates a Unix socket that no process holds at p, never renaming
// it across devices (a separately mounted temporary directory works) and
// never changing this process's working directory. On Linux it is bound in
// process through p's directory's descriptor (/proc/self/fd/<fd>/<name>, a
// short path whatever p's length). Elsewhere it is bound directly when p
// fits the socket path limit; otherwise in a fresh short directory under the
// resolved /tmp and renamed into place, only when that directory is on p's
// device; only as a last resort by a helper run of this test binary whose
// working directory is p's directory (code review B3 CI: a helper run per
// socket is slow under the race detector).
func cqSocket(t *testing.T, p string) {
	dir, name := filepath.Dir(p), filepath.Base(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Error(err)
		return
	}
	bind := func(path string) error {
		l, err := net.Listen("unix", path)
		if err != nil {
			return err
		}
		ul := l.(*net.UnixListener)
		ul.SetUnlinkOnClose(false)
		return ul.Close()
	}
	if !cqSocketPortable {
		d, err := os.Open(dir)
		if err != nil {
			t.Error(err)
			return
		}
		defer d.Close()
		if err := bind(fmt.Sprintf("/proc/self/fd/%d/%s", d.Fd(), name)); err != nil {
			t.Errorf("binding %s: %v", name, err)
		}
		return
	}
	if len(p) <= maxSocketPath {
		if err := bind(p); err != nil {
			t.Errorf("binding %s: %v", name, err)
		}
		return
	}
	if base, err := filepath.EvalSymlinks("/tmp"); err == nil && sameDevice(base, dir) {
		short, err := os.MkdirTemp(base, "sk")
		if err == nil {
			defer os.RemoveAll(short)
			if err := bind(filepath.Join(short, "s")); err != nil {
				t.Errorf("binding %s: %v", name, err)
				return
			}
			if err := os.Rename(filepath.Join(short, "s"), p); err != nil {
				t.Errorf("placing %s: %v", name, err)
			}
			return
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Dir, cmd.Env = dir, append(os.Environ(), bindSocketEnv+"="+name)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("binding %s: %v %s", name, err, out)
	}
}

// sameDevice reports two existing paths on one device (a rename between
// them never crosses filesystems).
func sameDevice(a, b string) bool {
	ia, err1 := os.Stat(a)
	ib, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		return false
	}
	sa, ok1 := ia.Sys().(*syscall.Stat_t)
	sb, ok2 := ib.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && sa.Dev == sb.Dev
}

// cqResidue makes the fake Cursor's enable approve project_scoped (the
// case's project directory with exactly its approval file) and leave
// layoutFor(case workspace)'s residue after each session: "in_place" (the
// exact tree in that directory), "hashed" (a k=2 truncated directory holding
// only worker.sock), "mixed" (both), "partial" (a socket and log only) or ""
// (none). The session files carry a canary.
func cqResidue(t *testing.T, q *cqRun, layoutFor func(ws string) string) {
	residueWorld(t, q.w, q.home, layoutFor)
}

// residueWorld is cqResidue on any fake world w with the owner home home.
func residueWorld(t *testing.T, w *capWorld, home string, layoutFor func(ws string) string) {
	def := w.script
	if def == nil {
		def = w.defaults
	}
	projects := filepath.Join(home, ".cursor", "projects")
	// An owner's projects directory exists (creating it is outside change).
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	slugOf := func(ws string) string {
		s, err := cursorProjectSlug(osApprovalFS{}, "linux", "amd64", CursorRealVersion, ws)
		if err != nil {
			t.Error(err)
		}
		return s
	}
	w.script = func(spec ProcSpec) capBehavior {
		b := def(spec)
		if launchKind(spec) == "enable" {
			b.writes = approvalAt(filepath.Join(projects, slugOf(spec.Dir)), `["probe-6e58c4b6c129cbd0"]`)
		}
		return b
	}
	w.onReap = func(spec ProcSpec) {
		if launchKind(spec) != "session" {
			return
		}
		slug := slugOf(spec.Dir)
		base := filepath.Base(spec.Dir)
		layout := layoutFor(spec.Dir)
		if layout == "hashed" || layout == "mixed" {
			// The suffix differs per workspace (never computed by the harness).
			suffix := sha256Hex([]byte(slug))[:7]
			cqSocket(t, filepath.Join(projects, strings.TrimSuffix(slug, base)+base[:2]+"-"+suffix, cursorWorkerSocket))
		}
		if layout == "" || layout == "hashed" {
			return
		}
		d := filepath.Join(projects, slug)
		cqSocket(t, filepath.Join(d, cursorWorkerSocket))
		files := map[string]string{inPlaceLog: rsLog}
		if layout != "partial" {
			files[inPlaceRepo], files[inPlaceTrusted] = rsRepo, rsTrusted
			files[filepath.Join(inPlaceTranscripts, rsUUID, rsUUID+".jsonl")] = rsTranscript
		}
		for rel, data := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(d, rel)), 0o700)
			if err := os.WriteFile(filepath.Join(d, rel), []byte(data), 0o600); err != nil {
				t.Error(err)
			}
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
			// The projects root is listed by the foreign baseline's two
			// initial walks, the inventory and the walk after it, then by the
			// three walks around the inventory after the enable (A3.1); the
			// eighth listing is the residue baseline's, and the foreign
			// revalidation that follows it records the A3.1 deadline reason.
			return &dirHookFS{dir: filepath.Join(q.home, ".cursor", "projects"), n: 8, hook: advance}
		}, caseBudget, ReasonForeignDeadline, 0},
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

// Design decoder-enrollment A3.1 on the operating system's filesystem:
// device/inode identity (a same-name replacement with identical size, mode
// and modification time is a change), directory size and modification time
// never used as content identity, a socket replaced by a FIFO and a
// recorded directory replaced by a link (never followed) are changes.
func TestForeignBaselineFilesystem(t *testing.T) {
	root := shortRoot(t)
	home := filepath.Join(root, "h")
	dev := filepath.Join(home, ".cursor", "projects", "dev")
	logPath := filepath.Join(dev, inPlaceLog)
	setup := func() *cursorPreparer {
		os.RemoveAll(home)
		if err := os.MkdirAll(filepath.Join(dev, inPlaceTranscripts, "u"), 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(logPath, []byte("log"), 0o600)
		os.WriteFile(filepath.Join(dev, inPlaceTranscripts, "u", "u.jsonl"), []byte("t"), 0o600)
		cqSocket(t, filepath.Join(dev, cursorWorkerSocket))
		p := newCursorPreparer(nil, ApprovalScanLimits{}, "linux", "amd64", CursorRealVersion, home)
		if err := p.preflight(nil, ""); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, tc := range map[string]struct {
		change func()
		want   string
	}{
		"inode": {func() {
			info, _ := os.Lstat(logPath)
			tmp := filepath.Join(dev, "tmp")
			os.WriteFile(tmp, []byte("log"), 0o600)
			os.Chtimes(tmp, info.ModTime(), info.ModTime())
			os.Rename(tmp, logPath)
			if now, _ := os.Lstat(logPath); now.Size() != info.Size() || !now.ModTime().Equal(info.ModTime()) || now.Mode() != info.Mode() {
				t.Fatal("the replacement differs in metadata")
			}
		}, ReasonForeignChanged},
		"unrelated-file": {func() { os.WriteFile(filepath.Join(dev, "README.md"), []byte("x"), 0o600) }, ""},
		"dir-mtime":      {func() { os.Chtimes(dev, time.Now().Add(time.Hour), time.Now().Add(time.Hour)) }, ""},
		"append": {func() {
			f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
			f.WriteString("x")
			f.Close()
		}, ReasonForeignChanged},
		"socket-to-fifo": {func() {
			os.Remove(filepath.Join(dev, cursorWorkerSocket))
			syscall.Mkfifo(filepath.Join(dev, cursorWorkerSocket), 0o600)
		}, ReasonForeignChanged},
		"dir-to-link": {func() {
			os.RemoveAll(filepath.Join(dev, inPlaceTranscripts))
			os.Symlink(dev, filepath.Join(dev, inPlaceTranscripts))
		}, ReasonForeignChanged},
		"ancestor-replaced": {func() {
			os.Rename(dev, dev+".old")
			os.MkdirAll(filepath.Join(dev, inPlaceTranscripts, "u"), 0o700)
			os.WriteFile(logPath, []byte("log"), 0o600)
		}, ReasonForeignChanged},
		"removed": {func() { os.Remove(logPath) }, ReasonForeignRemoved},
	} {
		p := setup()
		tc.change()
		err := p.preflight(nil, "")
		if got := ""; err != nil {
			got = err.Error()
			if got != tc.want {
				t.Errorf("%s: %q", name, got)
			}
		} else if tc.want != "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

// foreignHome adds pre-existing foreign session artifacts to home's
// projects directory (with a real stale socket) and returns the file to
// mutate.
func foreignHome(t *testing.T, home string) string {
	dev := filepath.Join(home, ".cursor", "projects", "home-o-dev-flow")
	for rel, data := range map[string]string{inPlaceLog: "dev " + rsCanary, inPlaceRepo: "{}", filepath.Join(inPlaceTranscripts, "u1", "u1.jsonl"): rsCanary,
		filepath.Join(inPlaceTranscripts, "u1", "sub", "notes.txt"): rsCanary, cursorApprovalsFile: `["probe-aaaaaaaaaaaaaaaa"]`} {
		os.MkdirAll(filepath.Dir(filepath.Join(dev, rel)), 0o700)
		if err := os.WriteFile(filepath.Join(dev, rel), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cqSocket(t, filepath.Join(dev, cursorWorkerSocket))
	return filepath.Join(dev, inPlaceLog)
}

// A3.1's fixed reasons propagate verbatim and unwrapped through approve,
// beforeSession and checkResidue in qualify and capture; unchanged foreign
// artifacts survive a conclusive run with no session-content open and no
// foreign name in the report, the bundle or the log.
func TestForeignBaselineFlows(t *testing.T) {
	// Qualify: tolerated across three cases.
	spy := &openCountFS{}
	var log bytes.Buffer
	q := cursorQualify(t, nil, func(q *cqRun) {
		cqResidue(t, q, func(string) string { return "in_place" })
		foreignHome(t, q.home)
		q.r.approvalFS, q.r.Log = spy, &log
	})
	if q.err != nil || q.rep.Outcome != StatusConclusive || spy.session != 0 || spy.approval == 0 {
		t.Fatalf("tolerated: %v %s %+v (%d session opens)", q.err, q.rep.Outcome, cqCases(q.rep), spy.session)
	}
	b, _ := os.ReadFile(filepath.Join(q.r.OutDir, "report.json"))
	for _, raw := range []string{"dev-flow", rsCanary, "u1.jsonl", "foreign"} {
		if bytes.Contains(b, []byte(raw)) || strings.Contains(log.String(), raw) {
			t.Fatalf("%q in the report or log", raw)
		}
	}
	// Qualify: each consumer records the fixed reason verbatim.
	for name, tc := range map[string]struct {
		setup    func(q *cqRun, logPath string)
		want     string
		sessions int
	}{
		"approve": {func(q *cqRun, logPath string) {
			prev := q.w.onReap
			q.w.onReap = func(spec ProcSpec) {
				prev(spec)
				if launchKind(spec) == "enable" {
					os.WriteFile(logPath, []byte("dev grown "+rsCanary), 0o600)
				}
			}
		}, ReasonForeignChanged, 0},
		"before-session": {func(q *cqRun, logPath string) {
			q.r.approvalFS = &openHookFS{suffix: "/.cursor/cli.json", n: 4, mutate: func(string) { os.Remove(logPath) }}
		}, ReasonForeignRemoved, 0},
		"check-residue": {func(q *cqRun, logPath string) {
			prev := q.w.onReap
			q.w.onReap = func(spec ProcSpec) {
				prev(spec)
				if launchKind(spec) == "session" {
					os.WriteFile(filepath.Join(filepath.Dir(logPath), inPlaceTranscripts, "u1", "u2.jsonl"), []byte("x"), 0o600)
				}
			}
		}, ReasonForeignNew, 1},
	} {
		q := cursorQualify(t, nil, func(q *cqRun) {
			cqResidue(t, q, func(string) string { return "in_place" })
			tc.setup(q, foreignHome(t, q.home))
		})
		cases := cqCases(q.rep)
		sessions := 0
		for _, k := range q.kinds() {
			if k == "session" {
				sessions++
			}
		}
		if q.err != nil || q.rep.Outcome == StatusConclusive || len(cases) != 1 || sessions != tc.sessions {
			t.Errorf("qualify %s: %v %s %v", name, q.err, q.rep.Outcome, q.kinds())
			continue
		}
		p := cases[0].CursorPreparation
		if deref(cases[0].Reason) != tc.want || deref(p.Reason) != tc.want {
			t.Errorf("qualify %s: case %q preparation %q", name, deref(cases[0].Reason), deref(p.Reason))
		}
		switch name {
		case "approve":
			if deref(p.Approval.Reason) != tc.want || len(p.Approval.Changes) != 0 {
				t.Errorf("qualify %s: approval %+v", name, p.Approval)
			}
		default:
			if deref(p.Residue.Reason) != tc.want {
				t.Errorf("qualify %s: residue %+v", name, p.Residue)
			}
		}
		b, _ := os.ReadFile(filepath.Join(q.r.OutDir, "report.json"))
		if _, err := ParseReport(b); err != nil || bytes.Contains(b, []byte("dev-flow")) {
			t.Errorf("qualify %s: report %v", name, err)
		}
	}
	// Capture: the same three consumers.
	for name, tc := range map[string]struct {
		hook   func(logPath string) *hookFS
		reap   func(logPath string, spec ProcSpec)
		want   string
		kinds  []string
		record func(cc CaptureClient) string
	}{
		"approve": {nil, func(logPath string, spec ProcSpec) {
			if launchKind(spec) == "enable" {
				os.WriteFile(logPath, []byte("dev grown "+rsCanary), 0o600)
			}
		}, ReasonForeignChanged, []string{"version", "help", "enable"}, func(cc CaptureClient) string { return deref(cc.Approval.Reason) }},
		"before-session": {func(logPath string) *hookFS {
			return &hookFS{suffix: "/.cursor/cli.json", n: 4, onOpen: func() { os.Remove(logPath) }}
		}, nil, ReasonForeignRemoved, []string{"version", "help", "enable"}, func(cc CaptureClient) string { return deref(cc.WorkerResidue.Reason) }},
		"check-residue": {nil, func(logPath string, spec ProcSpec) {
			if launchKind(spec) == "session" {
				os.WriteFile(filepath.Join(filepath.Dir(logPath), inPlaceTranscripts, "u1", "u2.jsonl"), []byte("x"), 0o600)
			}
		}, ReasonForeignNew, []string{"version", "help", "enable", "session"}, func(cc CaptureClient) string { return deref(cc.WorkerResidue.Reason) }},
	} {
		var logPath string
		var hook func(c *CaptureRunner, w *capWorld, home string) *hookFS
		if tc.hook != nil {
			hook = func(c *CaptureRunner, w *capWorld, home string) *hookFS {
				logPath = foreignHome(t, home)
				return tc.hook(logPath)
			}
		}
		man, cc, kinds := cursorCapture(t, context.Background(), hook, func(c *CaptureRunner, w *capWorld) {
			if logPath == "" {
				logPath = foreignHome(t, c.Home)
			}
			residueWorld(t, w, c.Home, func(string) string { return "in_place" })
			if tc.reap != nil {
				prev := w.onReap
				w.onReap = func(spec ProcSpec) {
					prev(spec)
					tc.reap(logPath, spec)
				}
			}
		})
		if man.State == CaptureComplete || deref(cc.Reason) != tc.want || tc.record(cc) != tc.want || !slices.Equal(kinds, tc.kinds) {
			t.Errorf("capture %s: %s %q %q %v", name, man.State, deref(cc.Reason), tc.record(cc), kinds)
		}
		mb, _ := json.Marshal(man)
		if bytes.Contains(mb, []byte("dev-flow")) || bytes.Contains(mb, []byte(rsCanary)) {
			t.Errorf("capture %s: a foreign name in the manifest", name)
		}
	}
}

// boundaryFS is the operating system's view that, once ready (after the
// session), changes the foreign log's modification time right after the
// final revalidation's lookup of it: armed by the safety scan's read of the
// foreign approval file, the scan's own lookup of the log passes, and the
// next one is the final revalidation's.
type boundaryFS struct {
	osApprovalFS
	logPath, approvalPath string
	mu                    sync.Mutex
	ready, armed, fired   bool
	lookups               int
}

func (f *boundaryFS) Open(p string) (approvalFile, error) {
	f.mu.Lock()
	if f.ready && p == f.approvalPath {
		f.armed = true
	}
	f.mu.Unlock()
	return f.osApprovalFS.Open(p)
}

func (f *boundaryFS) Lstat(p string) (fs.FileInfo, error) {
	info, err := f.osApprovalFS.Lstat(p)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.armed && !f.fired && p == f.logPath {
		if f.lookups++; f.lookups == 2 {
			f.fired = true
			later := time.Now().Add(time.Hour)
			os.Chtimes(p, later, later)
		}
	}
	return info, err
}

// Code review A3 round 1, C1 and C3: through qualify and capture, a foreign
// project replaced during the session, a new project with session files
// during the session and a foreign change at the final acceptance boundary
// each record A3.1's exact reason (also where A3's classification fails
// too), with no admission and no foreign name in evidence.
func TestForeignBaselineFlowsBoundary(t *testing.T) {
	type vector struct {
		reap func(logPath string)
		fsys func(logPath string) *boundaryFS
		want string
	}
	vectors := map[string]vector{
		"project-replaced": {reap: func(logPath string) {
			dev := filepath.Dir(logPath)
			os.Rename(dev, filepath.Join(filepath.Dir(filepath.Dir(dev)), "moved-away"))
			foreignHome(t, filepath.Dir(filepath.Dir(filepath.Dir(dev))))
		}, want: ReasonForeignChanged},
		"new-project": {reap: func(logPath string) {
			p := filepath.Join(filepath.Dir(filepath.Dir(logPath)), "new-project", inPlaceLog)
			os.MkdirAll(filepath.Dir(p), 0o700)
			os.WriteFile(p, []byte("x"), 0o600)
		}, want: ReasonForeignNew},
		"final-boundary": {fsys: func(logPath string) *boundaryFS {
			return &boundaryFS{logPath: logPath, approvalPath: filepath.Join(filepath.Dir(logPath), cursorApprovalsFile)}
		}, want: ReasonForeignUnverifiable},
	}
	for name, tc := range vectors {
		// Qualify.
		var bfs *boundaryFS
		q := cursorQualify(t, nil, func(q *cqRun) {
			cqResidue(t, q, func(string) string { return "in_place" })
			logPath := foreignHome(t, q.home)
			if tc.fsys != nil {
				bfs = tc.fsys(logPath)
				q.r.approvalFS = bfs
			}
			prev := q.w.onReap
			q.w.onReap = func(spec ProcSpec) {
				prev(spec)
				if launchKind(spec) != "session" {
					return
				}
				if bfs != nil {
					bfs.mu.Lock()
					bfs.ready = true
					bfs.mu.Unlock()
				}
				if tc.reap != nil {
					tc.reap(logPath)
				}
			}
		})
		cases := cqCases(q.rep)
		if q.err != nil || len(cases) != 1 || deref(cases[0].Reason) != tc.want || deref(cases[0].CursorPreparation.Reason) != tc.want ||
			deref(cases[0].CursorPreparation.Residue.Reason) != tc.want || len(cases[0].CursorPreparation.Residue.Entries) != 0 {
			t.Errorf("qualify %s: %v %+v", name, q.err, cases)
		}
		if bfs != nil && !bfs.fired {
			t.Errorf("qualify %s: the change never fired", name)
		}
		b, _ := os.ReadFile(filepath.Join(q.r.OutDir, "report.json"))
		if _, err := ParseReport(b); err != nil || bytes.Contains(b, []byte("dev-flow")) || bytes.Contains(b, []byte(rsCanary)) {
			t.Errorf("qualify %s: report %v", name, err)
		}
		// Capture.
		bfs = nil
		man, cc, kinds := cursorCapture(t, context.Background(), nil, func(c *CaptureRunner, w *capWorld) {
			logPath := foreignHome(t, c.Home)
			residueWorld(t, w, c.Home, func(string) string { return "in_place" })
			if tc.fsys != nil {
				bfs = tc.fsys(logPath)
				c.approvalFS = bfs
			}
			prev := w.onReap
			w.onReap = func(spec ProcSpec) {
				prev(spec)
				if launchKind(spec) != "session" {
					return
				}
				if bfs != nil {
					bfs.mu.Lock()
					bfs.ready = true
					bfs.mu.Unlock()
				}
				if tc.reap != nil {
					tc.reap(logPath)
				}
			}
		})
		if man.State == CaptureComplete || deref(cc.Reason) != tc.want || cc.WorkerResidue == nil || deref(cc.WorkerResidue.Reason) != tc.want ||
			len(cc.WorkerResidue.Entries) != 0 || !slices.Equal(kinds, []string{"version", "help", "enable", "session"}) {
			t.Errorf("capture %s: %s %q %+v %v", name, man.State, deref(cc.Reason), cc.WorkerResidue, kinds)
		}
		if bfs != nil && !bfs.fired {
			t.Errorf("capture %s: the change never fired", name)
		}
		mb, _ := json.Marshal(man)
		if bytes.Contains(mb, []byte("dev-flow")) || bytes.Contains(mb, []byte(rsCanary)) {
			t.Errorf("capture %s: a foreign name in the manifest", name)
		}
	}
}

// Code review A3 round 1, C2: on the operating system's filesystem, the
// approval and residue directories are compared by identity, type and full
// mode (a 0700 to 0755 change is refused at the pre-launch recheck, at
// admission and at ledger revalidation), never by their size or
// modification time (the session's legitimate additions are admitted).
func TestResidueDirectoryModes(t *testing.T) {
	type world struct {
		p        *cursorPreparer
		ws, slug string
		projects string
		b        *cursorApprovalBaseline
	}
	setup := func(approval bool) world {
		root := shortRoot(t)
		home := filepath.Join(root, "h")
		ws := filepath.Join(root, "out", ".work", "cursor-setup")
		os.MkdirAll(filepath.Join(ws, ".cursor"), 0o700)
		os.WriteFile(filepath.Join(ws, ".cursor", "mcp.json"), []byte("{}"), 0o600)
		projects := filepath.Join(home, ".cursor", "projects")
		os.MkdirAll(projects, 0o700)
		w := world{p: newCursorPreparer(nil, ApprovalScanLimits{}, "linux", "amd64", CursorRealVersion, home), ws: ws, projects: projects}
		slug, err := cursorProjectSlug(osApprovalFS{}, "linux", "amd64", CursorRealVersion, ws)
		if err != nil {
			t.Fatal(err)
		}
		w.slug = slug
		if approval {
			os.MkdirAll(filepath.Join(projects, slug), 0o700)
			os.WriteFile(filepath.Join(projects, slug, cursorApprovalsFile), []byte(rsApproval), 0o600)
			sc, _ := w.p.scanner()
			info, data, why := sc.projectApprovalFile(filepath.Join(projects, slug, cursorApprovalsFile))
			if why != "" {
				t.Fatal(why)
			}
			if w.b, err = w.p.approvalBaseline(slug, info, data); err != nil {
				t.Fatal(err)
			}
		}
		return w
	}
	inPlace := func(w world) {
		d := filepath.Join(w.projects, w.slug)
		for rel, data := range map[string]string{inPlaceLog: rsLog, inPlaceRepo: rsRepo, inPlaceTrusted: rsTrusted,
			filepath.Join(inPlaceTranscripts, rsUUID, rsUUID+".jsonl"): rsTranscript} {
			os.MkdirAll(filepath.Dir(filepath.Join(d, rel)), 0o700)
			os.WriteFile(filepath.Join(d, rel), []byte(data), 0o600)
		}
		cqSocket(t, filepath.Join(d, cursorWorkerSocket))
	}
	// The pre-launch recheck.
	w := setup(true)
	if _, err := w.p.sessionBaseline(w.b); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(w.projects, w.slug), 0o755)
	if _, err := w.p.sessionBaseline(w.b); err == nil {
		t.Fatal("the pre-launch recheck accepted a mode change of the approval directory")
	}
	// Admission: the approval directory's mode changed during the session.
	w = setup(true)
	pre, err := w.p.sessionBaseline(w.b)
	if err != nil {
		t.Fatal(err)
	}
	inPlace(w)
	os.Chmod(filepath.Join(w.projects, w.slug), 0o755)
	if r := w.p.checkResidue("cursor-setup", w.ws, pre); r.State != ResidueUnverified || len(w.p.ledger.entries) != 0 {
		t.Fatalf("admission accepted a mode change: %+v", r)
	}
	// Ledger revalidation: the admitted in-place directory and a
	// subdirectory; the session's additions themselves were admitted.
	for _, rel := range []string{"", inPlaceTranscripts} {
		w = setup(true)
		pre, err := w.p.sessionBaseline(w.b)
		if err != nil {
			t.Fatal(err)
		}
		inPlace(w)
		if r := w.p.checkResidue("cursor-setup", w.ws, pre); r.State != ResidueVerified || len(r.Entries) != 1 {
			t.Fatalf("in-place admission: %+v %s", r, deref(r.Reason))
		}
		os.Chmod(filepath.Join(w.projects, w.slug, rel), 0o755)
		if err := w.p.revalidate(); err == nil {
			t.Fatalf("the ledger accepted a mode change of %q", rel)
		}
	}
	// The hashed ledger directory.
	w = setup(false)
	pre, err = w.p.sessionBaseline(nil)
	if err != nil {
		t.Fatal(err)
	}
	parent, _, _ := w.p.parentSlug(w.ws)
	hashed := filepath.Join(w.projects, parent+"-cu-1b81996")
	cqSocket(t, filepath.Join(hashed, cursorWorkerSocket))
	os.Chmod(hashed, 0o700)
	if r := w.p.checkResidue("cursor-setup", w.ws, pre); r.State != ResidueVerified || len(r.Entries) != 1 {
		t.Fatalf("hashed admission: %+v %s", r, deref(r.Reason))
	}
	os.Chmod(hashed, 0o755)
	if err := w.p.revalidate(); err == nil {
		t.Fatal("the ledger accepted a mode change of the hashed directory")
	}
}

// Code review A3 round 2, C1: an ordinary snapshot failure never preempts
// A3.1's fixed reason. Through qualify and capture, before the session (the
// pre-launch baseline) and after it (the residue check): the projects root,
// a recorded ancestor, replaced by a symbolic link is exactly "changed"; an
// unreadable projects root is exactly "unverifiable".
func TestForeignBaselineEarlyFailures(t *testing.T) {
	type change func(projects string)
	toLink := func(projects string) {
		os.Rename(projects, projects+".real")
		os.Symlink(projects+".real", projects)
	}
	unreadable := func(projects string) {
		os.Chmod(projects, 0o300)
		t.Cleanup(func() { os.Chmod(projects, 0o700) })
	}
	for name, tc := range map[string]struct {
		change change
		before bool // before the session (else after its cleanup)
		want   string
	}{
		"root-link-before":       {toLink, true, ReasonForeignChanged},
		"root-link-after":        {toLink, false, ReasonForeignChanged},
		"root-unreadable-before": {unreadable, true, ReasonForeignUnverifiable},
		"root-unreadable-after":  {unreadable, false, ReasonForeignUnverifiable},
	} {
		// Qualify.
		q := cursorQualify(t, nil, func(q *cqRun) {
			cqResidue(t, q, func(string) string { return "in_place" })
			foreignHome(t, q.home)
			projects := filepath.Join(q.home, ".cursor", "projects")
			if tc.before {
				q.r.approvalFS = &openHookFS{suffix: "/.cursor/cli.json", n: 4, mutate: func(string) { tc.change(projects) }}
				return
			}
			prev := q.w.onReap
			q.w.onReap = func(spec ProcSpec) {
				prev(spec)
				if launchKind(spec) == "session" {
					tc.change(projects)
				}
			}
		})
		cases := cqCases(q.rep)
		if q.err != nil || len(cases) != 1 || deref(cases[0].Reason) != tc.want || deref(cases[0].CursorPreparation.Residue.Reason) != tc.want {
			t.Errorf("qualify %s: %v %+v", name, q.err, cases)
		}
		// Capture.
		var hook func(c *CaptureRunner, w *capWorld, home string) *hookFS
		if tc.before {
			hook = func(c *CaptureRunner, w *capWorld, home string) *hookFS {
				projects := filepath.Join(home, ".cursor", "projects")
				return &hookFS{suffix: "/.cursor/cli.json", n: 4, onOpen: func() { tc.change(projects) }}
			}
		}
		man, cc, _ := cursorCapture(t, context.Background(), hook, func(c *CaptureRunner, w *capWorld) {
			foreignHome(t, c.Home)
			residueWorld(t, w, c.Home, func(string) string { return "in_place" })
			if !tc.before {
				projects := filepath.Join(c.Home, ".cursor", "projects")
				prev := w.onReap
				w.onReap = func(spec ProcSpec) {
					prev(spec)
					if launchKind(spec) == "session" {
						tc.change(projects)
					}
				}
			}
		})
		if man.State == CaptureComplete || deref(cc.Reason) != tc.want || cc.WorkerResidue == nil || deref(cc.WorkerResidue.Reason) != tc.want {
			t.Errorf("capture %s: %s %q %+v", name, man.State, deref(cc.Reason), cc.WorkerResidue)
		}
	}
}

// Code review A3 round 3, C1: the enable's outcome and a simultaneous
// foreign change, through qualify and capture. Interruption keeps its
// precedence; an enable killed by its watchdog keeps its own failure (the
// narrow causality rule: the expiry is not a foreign change); an enable
// that merely exits 1 yields A3.1's fixed reason verbatim.
func TestForeignBaselineEnablePrecedence(t *testing.T) {
	ownFailure := ReasonCursorApprovalFailed + ": the enable command did not exit 0 by itself"
	for name, want := range map[string]string{"exit-1": ReasonForeignChanged, "watchdog": ownFailure, "interrupted": ReasonInterrupted} {
		grow := func(logPath string) {
			f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
			if err == nil {
				f.WriteString("grown\n")
				f.Close()
			}
		}
		script := func(w *capWorld, def func(ProcSpec) capBehavior) func(ProcSpec) capBehavior {
			return func(spec ProcSpec) capBehavior {
				b := def(spec)
				if launchKind(spec) == "enable" {
					if name == "exit-1" {
						b.exit = 1
					} else {
						b = capBehavior{hang: true}
					}
				}
				return b
			}
		}
		// drive changes the foreign log during the enable and ends it as the
		// vector requires.
		drive := func(w *capWorld, logPath string, cancel func()) {
			switch name {
			case "exit-1":
				prev := w.onReap
				w.onReap = func(spec ProcSpec) {
					if prev != nil {
						prev(spec)
					}
					if launchKind(spec) == "enable" {
						grow(logPath)
					}
				}
			case "watchdog":
				go func() {
					w.awaitStart("enable")
					grow(logPath)
					awaitTimer(t, w.clock, cursorApprovalWatchdog)
					w.clock.Advance(cursorApprovalWatchdog)
				}()
			default:
				go func() {
					w.awaitStart("enable")
					grow(logPath)
					cancel()
				}()
			}
		}
		// Qualify.
		ctx, cancel := context.WithCancel(context.Background())
		q := cursorQualify(t, nil, func(q *cqRun) {
			logPath := foreignHome(t, q.home)
			q.ctx = ctx
			q.w.script = script(q.w, q.w.script)
			drive(q.w, logPath, cancel)
		})
		cancel()
		cases := cqCases(q.rep)
		if q.err != nil || len(cases) != 1 || deref(cases[0].Reason) != want || deref(cases[0].CursorPreparation.Approval.Reason) != want ||
			slices.Contains(q.kinds(), "session") {
			t.Errorf("qualify %s: %v %+v %v", name, q.err, cases, q.kinds())
		}
		// Capture.
		ctx, cancel = context.WithCancel(context.Background())
		man, cc, kinds := cursorCapture(t, ctx, nil, func(c *CaptureRunner, w *capWorld) {
			logPath := foreignHome(t, c.Home)
			def := w.script
			if def == nil {
				def = w.defaults
			}
			w.script = script(w, def)
			drive(w, logPath, cancel)
		})
		cancel()
		if man.State == CaptureComplete || deref(cc.Reason) != want || deref(cc.Approval.Reason) != want || slices.Contains(kinds, "session") {
			t.Errorf("capture %s: %s %q %q %v", name, man.State, deref(cc.Reason), deref(cc.Approval.Reason), kinds)
		}
	}
}
