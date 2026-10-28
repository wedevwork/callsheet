package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Unit tests of design 12a-3-cursor-worker, slice 3b FP-3 (UT-B3): every
// live entrypoint binds --cursor before any world exists, and the combined
// exercise freezes the Cursor selection and executable for both children.

// cursorCommands are every live command taking --cursor, the deprecated
// alias included.
var cursorCommands = []string{"run", "run-dev-full", "run-dev-light", "run-dev-exercise", "probe-dev-full"}

func TestCursorExecutionBindingUnit(t *testing.T) {
	t.Parallel()
	type refusal struct {
		flow    string // "cursor", "default", "malformed", "missing" or "" (no --flow)
		args    []string
		env     map[string]string
		token   string
		reached bool
	}
	cases := map[string]refusal{
		"missing_cursor":    {flow: "cursor", token: CodeCursorAdapterRequired},
		"unused_cursor":     {flow: "default", args: []string{"--cursor", fakeCursor}, token: CodeCursorAdapterUnused},
		"unused_no_flow":    {args: []string{"--cursor", fakeCursor}, token: CodeCursorAdapterUnused},
		"malformed_flow":    {flow: "malformed", args: []string{"--cursor", fakeCursor}, token: "refused: flow_invalid: "},
		"missing_flow":      {flow: "missing", args: []string{"--cursor", fakeCursor}, token: "refused: flow_invalid: "},
		"relative_cursor":   {flow: "cursor", args: []string{"--cursor", "cursor-agent"}, token: "takes at most one --cursor"},
		"empty_cursor":      {flow: "cursor", args: []string{"--cursor="}, token: "takes at most one --cursor"},
		"repeated_cursor":   {flow: "cursor", args: []string{"--cursor", fakeCursor, "--cursor", fakeCursor}, token: "takes at most one --cursor"},
		"cursor_config":     {flow: "cursor", args: []string{"--cursor", fakeCursor}, env: map[string]string{EnvCursorConfigDir: ""}, token: CodeCursorConfigDir},
		"xdg_config":        {flow: "cursor", args: []string{"--cursor", fakeCursor}, env: map[string]string{EnvXDGConfigHome: "/secret/value"}, token: CodeCursorConfigDir},
		"bound_reaches":     {flow: "cursor", args: []string{"--cursor", fakeCursor}, reached: true},
		"default_reaches":   {flow: "default", reached: true},
		"malformed_plain":   {flow: "malformed", reached: true},
		"claude_config_1st": {flow: "cursor", env: map[string]string{EnvClaudeConfigDir: "", EnvCursorConfigDir: ""}, token: "claude_config_dir_unsupported"},
	}
	for _, cmd := range cursorCommands {
		for name, c := range cases {
			t.Run(cmd+"/"+name, func(t *testing.T) {
				t.Parallel()
				f := newFakeWorld(t)
				copyDevDocs(t, f.checkout)
				switch c.flow {
				case "cursor":
					f.flow = filepath.Join(f.checkout, filepath.FromSlash(DevFullDir), "flow-cursor-coder.json")
				case "default":
					f.flow = filepath.Join(f.checkout, filepath.FromSlash(DevFullDir), FlowFile)
				case "malformed":
					f.flow = filepath.Join(f.checkout, "bad-flow.json")
					os.WriteFile(f.flow, []byte(`{"schema":"callsheet-dev-full-flow/v1","roles":[`), 0o600)
				case "missing":
					f.flow = filepath.Join(f.checkout, "missing-flow.json")
				}
				for k, v := range c.env {
					f.env[k] = v
				}
				h := f.host()
				h.legacy = false
				h.Args = append(f.args(), c.args...)
				h.Args[0] = cmd
				built := false
				h.NewWorld = func(Host) (*World, func(), error) {
					built = true
					return nil, nil, errors.New("stop after binding")
				}
				code := MainWith(h)
				switch {
				case c.reached && (!built || code != 1 && code != 2 || strings.Contains(f.stderr.String(), "refused")):
					t.Fatalf("a bound command did not reach its world: %d %s", code, f.stderr.String())
				case !c.reached && (built || code != 2 || !strings.Contains(f.stderr.String(), c.token)):
					t.Fatalf("exit %d built %t stderr %s", code, built, f.stderr.String())
				}
				if strings.Contains(f.stderr.String(), "/secret/value") || len(f.launcher.argv()) != 0 || strings.Contains(f.stderr.String(), CodeCursorPending3b) {
					t.Fatal("a refused command leaked a value, started a process or used the retired refusal")
				}
			})
		}
	}
}

func TestCursorExecutionExercise(t *testing.T) {
	t.Parallel()
	t.Run("frozen_flow", func(t *testing.T) {
		t.Parallel()
		// The selection file changes on disk while the full child runs: both
		// children capture the bytes the root froze before authorization.
		x, _ := newCursorExerciseRig(t, happy(), lightHappy())
		frozen := mustRead(t, x.full.f.flow)
		changed := false
		x.full.onTick = func(r *devRig) {
			if !changed {
				changed = true
				write(t, r.f.flow, strings.Replace(string(frozen), `"high"`, `"max"`, 1))
			}
		}
		if code := x.run(); code != 0 {
			t.Fatalf("exit %d\n%s", code, x.stdout())
		}
		if !changed || bytes.Equal(mustRead(t, x.full.f.flow), frozen) {
			t.Fatal("the selection file never changed while the full child ran")
		}
		ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
		for _, dir := range []string{exerciseFullDir, exerciseLightDir} {
			got := mustRead(t, filepath.Join(x.root(), filepath.FromSlash(dir), "setup", "flow.json"))
			if !bytes.Equal(got, frozen) || bundleDoc[DevManifest](t, filepath.Join(x.root(), filepath.FromSlash(dir)), devManifestFile).FlowSHA256 != ex.FlowSHA256 {
				t.Fatalf("%s captured another selection", dir)
			}
		}
		if ex.FlowSHA256 != sha256Hex(frozen) {
			t.Fatal("the root did not freeze the selection")
		}
	})
	t.Run("executable_changed_between_children", func(t *testing.T) {
		t.Parallel()
		x, exe := newCursorExerciseRig(t, happy(), lightHappy())
		x.full.onFinal = func(r *devRig) { write(t, exe, "a replaced Cursor executable\n") }
		if code := x.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
		if ex.Children[1].Status != childNotRun || ex.Children[1].Stop != CodeCursorExecutableMismatch || exists(x.root(), exerciseLightDir) ||
			x.plane[scenarioLight] != nil || ex.Cursor == nil || ex.Cursor.AfterSHA256 == ex.Cursor.BeforeSHA256 {
			t.Fatalf("children %+v cursor %+v", ex.Children, ex.Cursor)
		}
		var out, errb strings.Builder
		if code := CheckExercise(x.root(), &out, &errb); code != 1 || !strings.Contains(out.String(), CodeCursorExecutableMismatch) {
			t.Fatalf("offline %d %s", code, out.String())
		}
	})
	t.Run("executable_changed_before_full_child", func(t *testing.T) {
		t.Parallel()
		// C1 (review round 1): the executable is replaced after the root
		// bound it and before the full child starts: the full child's own
		// pre-vendor binding refuses it, so no vendor runs and no task is
		// admitted in either child.
		x, exe := newCursorExerciseRig(t, happy(), lightHappy())
		x.prompted = func() { write(t, exe, "a replaced Cursor executable\n") }
		if code := x.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
		if ex.Children[0].Status != childFail || ex.Children[0].Stop != CodeCursorExecutableMismatch || ex.Children[1].Status != childNotRun ||
			ex.Children[1].Stop != CodeCursorExecutableMismatch {
			t.Fatalf("children %+v", ex.Children)
		}
		for _, p := range x.plane {
			if len(p.ids()) != 0 {
				t.Fatalf("tasks admitted with a replaced executable: %v", p.ids())
			}
		}
		for _, s := range x.full.f.launcher.argv() {
			if strings.Contains(s, "--version") || strings.Contains(s, "sidecar run") {
				t.Fatalf("a vendor or sidecar started: %s", s)
			}
		}
	})
	t.Run("executable_unreadable_at_root", func(t *testing.T) {
		t.Parallel()
		x, exe := newCursorExerciseRig(t, happy(), lightHappy())
		if err := os.Remove(exe); err != nil {
			t.Fatal(err)
		}
		if code := x.run(); code != 2 || exists(x.root(), devExerciseFile) || len(x.full.f.launcher.argv()) != 0 ||
			!strings.Contains(x.full.f.stderr.String(), CodeCursorExecutableMismatch) {
			t.Fatalf("exit %d stderr %s", code, x.full.f.stderr.String())
		}
	})
	t.Run("owner_gate_reached", func(t *testing.T) {
		t.Parallel()
		x, _ := newCursorExerciseRig(t, happy(), lightHappy())
		x.full.owner = []string{"no 1"}
		if code := x.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
		if ex.Children[0].Status != childFail || ex.Children[0].Stop != "owner_no" || ex.Children[1].Stop != "full_owner_stop" {
			t.Fatalf("children %+v", ex.Children)
		}
	})
	t.Run("full_failure_continues", func(t *testing.T) {
		t.Parallel()
		x, _ := newCursorExerciseRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete, alter: "bad-final"}},
			lightHappy())
		if code := x.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
		if ex.Children[0].Status != childFail || ex.Children[1].Status != childPass {
			t.Fatalf("children %+v", ex.Children)
		}
	})
	t.Run("cursor_sidecar_start_failure", func(t *testing.T) {
		t.Parallel()
		// The fourth sidecar cannot start: no preflight, no fallback to
		// another node, complete cleanup.
		x, _ := newCursorExerciseRig(t, happy(), lightHappy())
		prev := x.full.f.override
		x.full.f.override = func(s ProcSpec) (behavior, bool) {
			if slices.Contains(s.Args, "--cursor-adapter") {
				return behavior{startErr: errors.New("injected: the Cursor sidecar did not start")}, true
			}
			return prev(s)
		}
		if code := x.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
		if ex.Children[0].Stop != "sidecar_start_failed" {
			t.Fatalf("children %+v", ex.Children)
		}
		for _, p := range x.plane {
			if len(p.ids()) != 0 {
				t.Fatalf("a task was dispatched without the Cursor sidecar: %v", p.ids())
			}
		}
		cl := bundleDoc[devEnvelope[DevCleanup]](t, filepath.Join(x.root(), filepath.FromSlash(exerciseFullDir)), devCleanupFile)
		if !cl.Data.Complete {
			t.Fatalf("cleanup %+v", cl.Data)
		}
	})
}

func TestCursorExerciseRootRefusals(t *testing.T) {
	t.Parallel()
	x, _ := newCursorExerciseRig(t, happy(), lightHappy())
	if code := x.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	base := x.root()
	copyRoot := func(t *testing.T) string {
		dst := filepath.Join(t.TempDir(), "root")
		if err := copyTree(base, dst); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	check := func(root string) (int, string) {
		var out, errb strings.Builder
		code := CheckExercise(root, &out, &errb)
		return code, out.String() + errb.String()
	}
	t.Run("control", func(t *testing.T) {
		if code, out := check(base); code != 0 || !strings.Contains(out, devCursorExerciseReportSchema) {
			t.Fatalf("%d %s", code, out)
		}
		if a := bundleDoc[DevExerciseAudit](t, base, "audit.json"); a.Schema != devCursorExerciseAuditSchema {
			t.Fatalf("audit %s", a.Schema)
		}
	})
	for name, c := range map[string]struct {
		edit func(t *testing.T, root string)
		code int
		want string
	}{
		"root_binding": {func(t *testing.T, root string) {
			editJSON(t, root, devExerciseFile, func(m map[string]any) { jobj(m, "cursor")["after_sha256"] = zeros })
		}, 1, CodeCursorExecutableMismatch},
		"child_binding": {func(t *testing.T, root string) {
			child := filepath.Join(root, filepath.FromSlash(exerciseLightDir))
			editJSON(t, child, devManifestFile, func(m map[string]any) {
				jobj(m, "cursor")["before_sha256"], jobj(m, "cursor")["after_sha256"] = zeros, zeros
			})
			reforge(t, child)
		}, 1, CodeCursorExecutableMismatch},
		"default_child": {func(t *testing.T, root string) {
			child := filepath.Join(root, filepath.FromSlash(exerciseLightDir))
			os.RemoveAll(child)
			if err := copyTree(lightPassingBundle(t), child); err != nil {
				t.Fatal(err)
			}
		}, 1, CodeCursorSelectionMismatch},
		"relabeled_default": {func(t *testing.T, root string) {
			editJSON(t, root, devExerciseFile, func(m map[string]any) {
				m["schema"] = DevExerciseSchema
				delete(m, "cursor")
			})
		}, 1, CodeCursorSelectionMismatch},
		"cursor_without_field": {func(t *testing.T, root string) {
			editJSON(t, root, devExerciseFile, func(m map[string]any) { delete(m, "cursor") })
		}, 2, ""},
		"default_with_field": {func(t *testing.T, root string) {
			editJSON(t, root, devExerciseFile, func(m map[string]any) { m["schema"] = DevExerciseSchema })
		}, 2, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := copyRoot(t)
			c.edit(t, root)
			if c.code == 1 {
				rewriteExerciseDerived(t, root)
			}
			if code, out := check(root); code != c.code || c.want != "" && !strings.Contains(out, c.want) {
				t.Fatalf("exit %d want %d %q: %s", code, c.code, c.want, out)
			}
		})
	}
}

// ---- Design 12a-3c-cursor-binding: function tests, one per FP ----
//
// Every expected identity and tokenized path derives from
// filepath.EvalSymlinks of the fixture (cursorExeFixture.resolved), never
// from the temporary path as created. No paid call, real vendor or sleep.

// exerciseCheckOut runs the offline root check: its exit and output.
func exerciseCheckOut(root string) (int, string) {
	var out, errb strings.Builder
	code := CheckExercise(root, &out, &errb)
	return code, out.String() + errb.String()
}

// noVendorStarted fails when f started a vendor version probe, a sidecar
// or any plane task.
func noVendorStarted(t *testing.T, f *fakeWorld, planes ...*fakePlane) {
	t.Helper()
	for _, s := range f.launcher.argv() {
		if strings.Contains(s, "--version") || strings.Contains(s, "sidecar run") {
			t.Fatalf("a vendor or sidecar started: %s", s)
		}
	}
	for _, p := range append(planes, f.plane) {
		if p != nil && len(p.ids()) != 0 {
			t.Fatalf("tasks admitted: %v", p.ids())
		}
	}
}

// cursorVersionRecords are a version list's Cursor entries.
func cursorVersionRecords(vs []VersionRecord) []VersionRecord {
	var out []VersionRecord
	for _, v := range vs {
		if v.Tool == "cursor" {
			out = append(out, v)
		}
	}
	return out
}

// wantBinding fails unless got is exactly want.
func wantBinding(t *testing.T, what string, got *DevCursorBinding, want DevCursorBinding) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s binding %+v, want %+v", what, got, want)
	}
}

func TestCursorBindingFP1(t *testing.T) {
	t.Parallel()
	t.Run("symlinked_probe_qualifies_on_resolved_target", func(t *testing.T) {
		t.Parallel()
		cr := newCursorRigWith(t, "", cursorExeLinked)
		cr.expect(0, "")
		q := cr.cursorQual()
		a := cr.fx.identity(cr.fx.a)
		want := cr.d.s.tok(a.path)
		if want == cr.d.s.tok(cr.fx.exe) {
			t.Fatal("the fixture's resolved target is spelled like its link")
		}
		wantBinding(t, "qualification", &q.Cursor.Binding, DevCursorBinding{Path: want, BeforeSHA256: a.sha256, AfterSHA256: a.sha256})
		if a.sha256 != sha256Hex(mustRead(t, cr.fx.a)) || cr.d.cursor.frozen != a {
			t.Fatalf("frozen %+v want %+v", cr.d.cursor.frozen, a)
		}
		vs := cursorVersionRecords(q.Versions)
		if len(vs) != 1 || vs[0].Path != want || vs[0].Version != fakeCursorVersion || !vs[0].Eligible {
			t.Fatalf("cursor version records %+v", vs)
		}
		// The version probe still invokes the explicit --cursor path.
		invoked := false
		for _, s := range cr.f.launcher.specsOf(cr.fx.exe) {
			invoked = invoked || slices.Equal(s.Args, []string{"--version"})
		}
		if !invoked || len(cr.f.launcher.specsOf(a.path)) != 0 {
			t.Fatal("the version probe did not invoke the original --cursor path")
		}
		if code, rep, errOut := cr.qualifiedCheck(); code != 0 || rep.Result != ResultQualified {
			t.Fatalf("offline %d %+v %s", code, rep, errOut)
		}
	})
	t.Run("direct_path_control_qualifies", func(t *testing.T) {
		t.Parallel()
		cr := newCursorRig(t, "")
		cr.expect(0, "")
		q := cr.cursorQual()
		id := cr.fx.identity(cr.exe)
		want := cr.d.s.tok(id.path)
		wantBinding(t, "qualification", &q.Cursor.Binding, DevCursorBinding{Path: want, BeforeSHA256: id.sha256, AfterSHA256: id.sha256})
		if vs := cursorVersionRecords(q.Versions); len(vs) != 1 || vs[0].Path != want {
			t.Fatalf("cursor version records %+v", vs)
		}
		if code, rep, errOut := cr.qualifiedCheck(); code != 0 || rep.Result != ResultQualified {
			t.Fatalf("offline %d %+v %s", code, rep, errOut)
		}
	})
}

func TestCursorBindingFP2a(t *testing.T) {
	t.Parallel()
	// break makes the linked fixture's identity unresolvable (a dangling
	// link) or unreadable (a target that is a directory).
	breaks := map[string]func(fx cursorExeFixture){
		"dangling":   func(fx cursorExeFixture) { os.Remove(fx.a) },
		"unreadable": func(fx cursorExeFixture) { fx.retarget(filepath.Dir(fx.a)) },
	}
	for name, brk := range breaks {
		t.Run("exercise_root_"+name, func(t *testing.T) {
			t.Parallel()
			// The root's own freeze refuses before the authorization: exit
			// 2, nothing authorized, no child and no root record.
			x, fx := newCursorExerciseRigWith(t, happy(), lightHappy(), cursorExeLinked)
			brk(fx)
			if code := x.run(); code != 2 || exists(x.root(), devExerciseFile) || len(x.full.f.launcher.argv()) != 0 ||
				!strings.Contains(x.full.f.stderr.String(), "refused: "+CodeCursorExecutableMismatch) || strings.Contains(x.stdout(), "Authorize the combined exercise") {
				t.Fatalf("exit %d stderr %s", code, x.full.f.stderr.String())
			}
		})
		t.Run("probe_"+name, func(t *testing.T) {
			t.Parallel()
			cr := newCursorRigWith(t, "", cursorExeLinked)
			brk(cr.fx)
			cr.expect(1, CodeCursorExecutableMismatch)
			noVendorStarted(t, cr.f)
			if !strings.Contains(cr.f.stdout.String(), "qualification probe: "+ResultBlocked+" ("+CodeCursorExecutableMismatch) {
				t.Fatalf("stdout %s", cr.f.stdout.String())
			}
			if q := cr.cursorQual(); q.Result != ResultBlocked || q.Cursor.Binding.BeforeSHA256 != "" || q.Cursor.Binding.AfterSHA256 != "" {
				t.Fatalf("record %+v", q.Cursor)
			}
		})
		for _, scenario := range []string{scenarioFull, scenarioLight} {
			t.Run(scenario+"_"+name, func(t *testing.T) {
				t.Parallel()
				r, fx := newCursorFullRigWith(t, happy(), cursorExeLinked)
				if scenario == scenarioLight {
					r, fx = newCursorLightRigWith(t, lightHappy(), cursorExeLinked)
				}
				brk(fx)
				r.expect(1, CodeCursorExecutableMismatch)
				noVendorStarted(t, r.f)
				if m := r.manifest(); m.Cursor == nil || m.Cursor.BeforeSHA256 != "" || m.Cursor.AfterSHA256 != "" {
					t.Fatalf("manifest binding %+v", m.Cursor)
				}
				if code, _, _ := devCheck(t, r.f.bundle()); code != 1 {
					t.Fatalf("offline %d", code)
				}
			})
		}
	}
	t.Run("exercise_child_root_expectation", func(t *testing.T) {
		t.Parallel()
		// The executable_changed_before_full_child pattern: retargeted to a
		// byte-identical wrapper after the root froze its identity and
		// before the full child's own pre-vendor binding.
		x, fx := newCursorExerciseRigWith(t, happy(), lightHappy(), cursorExeLinked)
		x.prompted = func() { fx.retarget(fx.b) }
		if code := x.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
		if ex.Children[0].Status != childFail || ex.Children[0].Stop != CodeCursorExecutableMismatch || ex.Children[1].Status != childNotRun ||
			ex.Children[1].Stop != CodeCursorExecutableMismatch || exists(x.root(), exerciseLightDir) {
			t.Fatalf("children %+v", ex.Children)
		}
		var planes []*fakePlane
		for _, p := range x.plane {
			planes = append(planes, p)
		}
		noVendorStarted(t, x.full.f, planes...)
		a, b := fx.identity(fx.a), fx.identity(fx.b)
		tok := x.full.d.s.tok
		wantBinding(t, "root", ex.Cursor, DevCursorBinding{Path: tok(a.path), BeforeSHA256: a.sha256})
		// The child keeps its own resolved identity, so the root comparison
		// exposes the disagreement; its latched failure leaves no after.
		m := bundleDoc[DevManifest](t, filepath.Join(x.root(), filepath.FromSlash(exerciseFullDir)), devManifestFile)
		wantBinding(t, "full child", m.Cursor, DevCursorBinding{Path: tok(b.path), BeforeSHA256: b.sha256})
		if code, out := exerciseCheckOut(x.root()); code != 1 || !strings.Contains(out, CodeCursorExecutableMismatch) {
			t.Fatalf("offline %d %s", code, out)
		}
	})
}

func TestCursorBindingFP2b(t *testing.T) {
	t.Parallel()
	t.Run("probe", func(t *testing.T) {
		t.Parallel()
		cr := newCursorRigWith(t, "", cursorExeLinked)
		cr.act.during = func(cr *cursorRig) { cr.fx.retarget(cr.fx.b) }
		cr.expect(1, "")
		a := cr.fx.identity(cr.fx.a)
		q := cr.cursorQual()
		wantBinding(t, "qualification", &q.Cursor.Binding, DevCursorBinding{Path: cr.d.s.tok(a.path), BeforeSHA256: a.sha256})
		if q.Result != ResultBlocked || !cr.cleanupRecord().Complete || !cr.d.cursor.failed {
			t.Fatalf("record %s cleanup %+v", q.Result, cr.cleanupRecord())
		}
		code, rep, _ := cr.qualifiedCheck()
		if code != 1 || rep.Result != ResultBlocked || !slices.Contains(rep.Codes, CodeCursorExecutableMismatch) ||
			!strings.Contains(cr.f.stdout.String(), "independent check: "+ResultBlocked) {
			t.Fatalf("offline %d %+v", code, rep)
		}
	})
	for _, scenario := range []string{scenarioFull, scenarioLight} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			r, fx := newCursorFullRigWith(t, happy(), cursorExeLinked)
			if scenario == scenarioLight {
				r, fx = newCursorLightRigWith(t, lightHappy(), cursorExeLinked)
			}
			done := false
			r.onTick = func(*devRig) {
				if !done {
					done = true
					fx.retarget(fx.b)
				}
			}
			r.expect(1, "")
			a := fx.identity(fx.a)
			wantBinding(t, "manifest", r.manifest().Cursor, DevCursorBinding{Path: r.d.s.tok(a.path), BeforeSHA256: a.sha256})
			if !done || !r.cleanupRecord().Complete || r.d.passed || !slices.Contains(devCodes(r.report())["manifest"], CodeCursorExecutableMismatch) {
				t.Fatalf("retargeted %t cleanup %t codes %v", done, r.cleanupRecord().Complete, devCodes(r.report()))
			}
			if code, rep, _ := devCheck(t, r.f.bundle()); code != 1 || !slices.Contains(devCodes(rep)["manifest"], CodeCursorExecutableMismatch) {
				t.Fatalf("offline %d %v", code, devCodes(rep))
			}
		})
	}
	t.Run("exercise_full_child", func(t *testing.T) {
		t.Parallel()
		x, fx := newCursorExerciseRigWith(t, happy(), lightHappy(), cursorExeLinked)
		done := false
		x.full.onTick = func(*devRig) {
			if !done {
				done = true
				fx.retarget(fx.b)
			}
		}
		if code := x.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
		full := filepath.Join(x.root(), filepath.FromSlash(exerciseFullDir))
		a := fx.identity(fx.a)
		wantBinding(t, "full child", bundleDoc[DevManifest](t, full, devManifestFile).Cursor,
			DevCursorBinding{Path: x.full.d.s.tok(a.path), BeforeSHA256: a.sha256})
		cl := bundleDoc[devEnvelope[DevCleanup]](t, full, devCleanupFile)
		if !done || ex.Children[0].Status != childFail || ex.Children[1].Status != childNotRun || ex.Children[1].Stop != CodeCursorExecutableMismatch ||
			!cl.Data.Complete {
			t.Fatalf("children %+v cleanup %+v", ex.Children, cl.Data)
		}
		if code, rep, _ := devCheck(t, full); code != 1 || !slices.Contains(devCodes(rep)["manifest"], CodeCursorExecutableMismatch) {
			t.Fatalf("full child offline %d %v", code, devCodes(rep))
		}
	})
}

func TestCursorBindingFP2c(t *testing.T) {
	t.Parallel()
	// refused asserts the regression's outcome: exit 1, the light child
	// not run with cursor_executable_mismatch, no light directory or plane,
	// the root binding with no after digest, and the offline check FAIL
	// with the code.
	refused := func(t *testing.T, x *exerciseRig, fx cursorExeFixture, code int) DevExercise {
		t.Helper()
		if code != 1 {
			t.Fatalf("exit %d\n%s", code, x.stdout())
		}
		ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
		if ex.Children[1].Status != childNotRun || ex.Children[1].Stop != CodeCursorExecutableMismatch || exists(x.root(), exerciseLightDir) ||
			x.plane[scenarioLight] != nil {
			t.Fatalf("the light child ran: %+v", ex.Children)
		}
		a := fx.identity(fx.a)
		wantBinding(t, "root", ex.Cursor, DevCursorBinding{Path: x.full.d.s.tok(a.path), BeforeSHA256: a.sha256})
		if code, out := exerciseCheckOut(x.root()); code != 1 || !strings.Contains(out, CodeCursorExecutableMismatch) {
			t.Fatalf("offline %d %s", code, out)
		}
		return ex
	}
	t.Run("identical_wrapper_retarget", func(t *testing.T) {
		t.Parallel()
		x, fx := newCursorExerciseRigWith(t, happy(), lightHappy(), cursorExeLinked)
		if sha256Hex(mustRead(t, fx.a)) != sha256Hex(mustRead(t, fx.b)) {
			t.Fatal("the two version trees' wrappers differ")
		}
		x.full.onFinal = func(*devRig) { fx.retarget(fx.b) }
		refused(t, x, fx, x.run())
	})
	t.Run("sticky_after_restore", func(t *testing.T) {
		t.Parallel()
		// The full child observes the retarget after its cleanup; the link
		// is restored before the root's between-children gate, which still
		// refuses the light child.
		x, fx := newCursorExerciseRigWith(t, happy(), lightHappy(), cursorExeLinked)
		x.full.onFinal = func(*devRig) { fx.retarget(fx.b) }
		restored := false
		x.childDone = func(scenario string) {
			if scenario == scenarioFull {
				restored = true
				fx.retarget(fx.a)
			}
		}
		refused(t, x, fx, x.run())
		if id, err := readCursorIdentity(fx.exe); !restored || err != nil || !id.equal(fx.identity(fx.a)) {
			t.Fatalf("restored %t: %+v %v", restored, id, err)
		}
		full := bundleDoc[DevManifest](t, filepath.Join(x.root(), filepath.FromSlash(exerciseFullDir)), devManifestFile)
		if full.Cursor == nil || full.Cursor.AfterSHA256 != "" {
			t.Fatalf("full child binding %+v", full.Cursor)
		}
	})
	t.Run("higher_priority_stop_kept", func(t *testing.T) {
		t.Parallel()
		// An owner stop keeps its continuation cause; the observed binding
		// failure still fails the root binding.
		x, fx := newCursorExerciseRigWith(t, happy(), lightHappy(), cursorExeLinked)
		x.full.owner = []string{"no 1"}
		x.full.onFinal = func(*devRig) { fx.retarget(fx.b) }
		if code := x.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
		if ex.Children[0].Stop != "owner_no" || ex.Children[1].Status != childNotRun || ex.Children[1].Stop != "full_owner_stop" ||
			ex.Cursor == nil || ex.Cursor.AfterSHA256 != "" {
			t.Fatalf("children %+v cursor %+v", ex.Children, ex.Cursor)
		}
		if code, out := exerciseCheckOut(x.root()); code != 1 || !strings.Contains(out, CodeCursorExecutableMismatch) {
			t.Fatalf("offline %d %s", code, out)
		}
	})
}

// cursorLabel is a historical-style unresolved executable label: inert,
// never opened (it does not exist here).
const cursorLabel = "/nonexistent/historical/.local/bin/cursor-agent"

// relabelCursor sets a Cursor record's binding path (at keys) and its
// Cursor version record's path to label.
func relabelCursor(label string, keys ...string) func(m map[string]any) {
	return func(m map[string]any) {
		jobj(m, keys...)["path"] = label
		for _, v := range jarr(m, "versions") {
			if v := v.(map[string]any); v["tool"] == "cursor" {
				v["path"] = label
			}
		}
	}
}

// editExerciseChild edits child k's manifest of root with f, reforges the
// child (inventory, report and audit) and binds its new manifest digest in
// the root record; the root's derived files are left to the caller.
func editExerciseChild(t testing.TB, root string, k int, f func(m map[string]any)) {
	t.Helper()
	rel := []string{exerciseFullDir, exerciseLightDir}[k]
	dir := filepath.Join(root, filepath.FromSlash(rel))
	editJSON(t, dir, devManifestFile, f)
	reforge(t, dir)
	sum := sha256Hex(mustRead(t, filepath.Join(dir, devManifestFile)))
	editJSON(t, root, devExerciseFile, func(ex map[string]any) { ex["children"].([]any)[k].(map[string]any)["manifest_sha256"] = sum })
}

func TestCursorBindingFP3(t *testing.T) {
	t.Parallel()
	// serialized checks the raw JSON binding object at keys and the Cursor
	// version record's path of record.
	serialized := func(t *testing.T, record []byte, want DevCursorBinding, keys ...string) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(record, &m); err != nil {
			t.Fatal(err)
		}
		b := jobj(m, keys...)
		if b["path"] != want.Path || b["before_sha256"] != want.BeforeSHA256 || b["after_sha256"] != want.AfterSHA256 || len(b) != 3 {
			t.Fatalf("serialized binding %v want %+v", b, want)
		}
		n := 0
		for _, v := range jarr(m, "versions") {
			if v := v.(map[string]any); v["tool"] == "cursor" {
				n++
				if v["path"] != want.Path {
					t.Fatalf("cursor version path %v want %s", v["path"], want.Path)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%d cursor version records", n)
		}
	}
	t.Run("probe", func(t *testing.T) {
		t.Parallel()
		for _, fail := range []bool{false, true} {
			cr := newCursorRigWith(t, "", cursorExeLinked)
			a := cr.fx.identity(cr.fx.a)
			want, code := DevCursorBinding{Path: "", BeforeSHA256: a.sha256, AfterSHA256: a.sha256}, 0
			if fail {
				cr.act.during = func(cr *cursorRig) { cr.fx.retarget(cr.fx.b) }
				want.AfterSHA256, code = "", 1
			}
			cr.expect(code, "")
			want.Path = cr.d.s.tok(a.path)
			serialized(t, cr.evidence(devQualificationFile), want, "cursor", "binding")
		}
	})
	for _, scenario := range []string{scenarioFull, scenarioLight} {
		for _, c := range []string{"pass", "retarget", "replace"} {
			t.Run(scenario+"_"+c, func(t *testing.T) {
				t.Parallel()
				r, fx := newCursorFullRigWith(t, happy(), cursorExeLinked)
				if scenario == scenarioLight {
					r, fx = newCursorLightRigWith(t, lightHappy(), cursorExeLinked)
				}
				a := fx.identity(fx.a)
				want, code := DevCursorBinding{BeforeSHA256: a.sha256, AfterSHA256: a.sha256}, 0
				done := false
				r.onTick = func(*devRig) {
					if done || c == "pass" {
						return
					}
					done = true
					if c == "retarget" {
						fx.retarget(fx.b)
					} else {
						// A same-path byte replacement keeps its unequal digest.
						os.WriteFile(fx.a, []byte("a replaced Cursor wrapper\n"), 0o700)
					}
				}
				switch c {
				case "retarget":
					want.AfterSHA256, code = "", 1
				case "replace":
					want.AfterSHA256, code = sha256Hex([]byte("a replaced Cursor wrapper\n")), 1
				}
				r.expect(code, "")
				want.Path = r.d.s.tok(a.path)
				serialized(t, r.evidence(devManifestFile), want, "cursor")
			})
		}
	}
	t.Run("historical_labels", func(t *testing.T) {
		t.Parallel()
		// Consistent unresolved labels (an older writer's) still check;
		// only test-owned copies are edited.
		q := cursorQualifiedBundle(t, probeScopeBoth)
		editJSON(t, q, devQualificationFile, relabelCursor(cursorLabel, "cursor", "binding"))
		requalify(t, q)
		if code, rep, errOut := cursorCheck(t, q); code != 0 || rep.Result != ResultQualified {
			t.Fatalf("qualification %d %+v %s", code, rep.Codes, errOut)
		}
		for _, scenario := range []string{scenarioFull, scenarioLight} {
			dir := cursorAttemptBundle(t, scenario)
			editJSON(t, dir, devManifestFile, relabelCursor(cursorLabel, "cursor"))
			if code, rep := reforge(t, dir); code != 0 || rep.Result != ResultPass {
				t.Fatalf("%s attempt %d %v", scenario, code, devCodes(rep))
			}
		}
		root := cursorExerciseRoot(t)
		editJSON(t, root, devExerciseFile, func(m map[string]any) { jobj(m, "cursor")["path"] = cursorLabel })
		for k := range 2 {
			editExerciseChild(t, root, k, relabelCursor(cursorLabel, "cursor"))
		}
		rewriteExerciseDerived(t, root)
		if code, out := exerciseCheckOut(root); code != 0 {
			t.Fatalf("exercise %d %s", code, out)
		}
		if _, err := os.Lstat(cursorLabel); err == nil {
			t.Fatal("the label exists")
		}
	})
	t.Run("non_cursor_records_unchanged", func(t *testing.T) {
		t.Parallel()
		// The other vendors' records keep their invocation paths; a default
		// manifest carries no binding.
		m := bundleDoc[DevManifest](t, cursorAttemptBundle(t, scenarioFull), devManifestFile)
		d := bundleDoc[DevManifest](t, devPassingBundle(t), devManifestFile)
		for _, vs := range [][]VersionRecord{m.Versions, d.Versions} {
			for _, v := range vs {
				if p := map[string]string{"claude": fakeClaude, "codex": fakeCodex, "grok": fakeGrok}[v.Tool]; p != "" && v.Path != p {
					t.Fatalf("%s version path %s", v.Tool, v.Path)
				}
			}
		}
		if d.Cursor != nil || len(cursorVersionRecords(d.Versions)) != 0 || len(d.Versions) != 5 || len(m.Versions) != 6 {
			t.Fatalf("default manifest %+v %+v", d.Cursor, d.Versions)
		}
	})
}

func TestCursorBindingFP4a(t *testing.T) {
	t.Parallel()
	root := func(m map[string]any) map[string]any { return jobj(m, "cursor") }
	for name, c := range map[string]func(t *testing.T, dir string){
		"root_path": func(t *testing.T, dir string) {
			editJSON(t, dir, devExerciseFile, func(m map[string]any) { root(m)["path"] = cursorLabel })
		},
		"root_digests": func(t *testing.T, dir string) {
			editJSON(t, dir, devExerciseFile, func(m map[string]any) { root(m)["before_sha256"], root(m)["after_sha256"] = zeros, zeros })
		},
		"root_after_empty": func(t *testing.T, dir string) {
			editJSON(t, dir, devExerciseFile, func(m map[string]any) { root(m)["after_sha256"] = "" })
		},
		"child_path": func(t *testing.T, dir string) {
			// Consistent inside the light child (its own check passes), but
			// not the root's target.
			editExerciseChild(t, dir, 1, relabelCursor(cursorLabel, "cursor"))
		},
		"child_digests": func(t *testing.T, dir string) {
			editExerciseChild(t, dir, 0, func(m map[string]any) {
				jobj(m, "cursor")["before_sha256"], jobj(m, "cursor")["after_sha256"] = zeros, zeros
			})
		},
		"child_after_empty": func(t *testing.T, dir string) {
			editExerciseChild(t, dir, 0, func(m map[string]any) { jobj(m, "cursor")["after_sha256"] = "" })
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := cursorExerciseRoot(t)
			c(t, dir)
			rewriteExerciseDerived(t, dir)
			if code, out := exerciseCheckOut(dir); code != 1 || !strings.Contains(out, CodeCursorExecutableMismatch) {
				t.Fatalf("exit %d %s", code, out)
			}
		})
	}
	t.Run("consistent_nonexistent_labels", func(t *testing.T) {
		t.Parallel()
		dir := cursorExerciseRoot(t)
		editJSON(t, dir, devExerciseFile, func(m map[string]any) { root(m)["path"] = cursorLabel })
		for k := range 2 {
			editExerciseChild(t, dir, k, relabelCursor(cursorLabel, "cursor"))
		}
		rewriteExerciseDerived(t, dir)
		before := treeFingerprint(t, dir)
		if code, out := exerciseCheckOut(dir); code != 0 || strings.Contains(out, CodeCursorExecutableMismatch) {
			t.Fatalf("exit %d %s", code, out)
		}
		if _, err := os.Lstat(cursorLabel); err == nil || treeFingerprint(t, dir) != before {
			t.Fatal("the label exists or the check changed the evidence")
		}
	})
}

func TestCursorBindingFP4b(t *testing.T) {
	t.Parallel()
	// version edits the light child's Cursor version records.
	version := func(f func(vs []any) []any) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			editExerciseChild(t, dir, 1, func(m map[string]any) { m["versions"] = f(jarr(m, "versions")) })
		}
	}
	set := func(v string) func(t *testing.T, dir string) {
		return version(func(vs []any) []any {
			for _, x := range vs {
				if x := x.(map[string]any); x["tool"] == "cursor" {
					x["version"] = v
				}
			}
			return vs
		})
	}
	for name, c := range map[string]struct {
		edit func(t *testing.T, dir string)
		code int
	}{
		"equal_control":   {func(*testing.T, string) {}, 0},
		"different_date":  {set("2026.10.22-27dcf5f"), 1},
		"suffix_only":     {set("2026.10.14-27dcf5f"), 1},
		"trailing_detail": {set(fakeCursorVersion + " (build 2)"), 1},
		"empty":           {set(""), 1},
		"missing": {version(func(vs []any) []any {
			return slices.DeleteFunc(vs, func(x any) bool { return x.(map[string]any)["tool"] == "cursor" })
		}), 1},
		"duplicate": {version(func(vs []any) []any {
			for _, x := range vs {
				if x := x.(map[string]any); x["tool"] == "cursor" {
					return append(vs, x)
				}
			}
			return vs
		}), 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := cursorExerciseRoot(t)
			c.edit(t, dir)
			rewriteExerciseDerived(t, dir)
			code, out := exerciseCheckOut(dir)
			if code != c.code || (c.code == 1) != strings.Contains(out, CodeCursorExecutableMismatch) {
				t.Fatalf("exit %d want %d: %s", code, c.code, out)
			}
		})
	}
	t.Run("suffix_only_child_passes_alone", func(t *testing.T) {
		t.Parallel()
		// The child-level minimum check alone admits a suffix-only drift:
		// only the root's equality criterion rejects it.
		dir := cursorExerciseRoot(t)
		set("2026.10.14-27dcf5f")(t, dir)
		if code, rep, _ := devCheck(t, filepath.Join(dir, filepath.FromSlash(exerciseLightDir))); code != 0 || rep.Result != ResultPass {
			t.Fatalf("light child %d %v", code, devCodes(rep))
		}
	})
	// earlyStop runs an exercise whose children fail their version probes
	// (claude refused before the Cursor record and versions_checked) while
	// failing is set; the light child's start clears it when lightOK.
	earlyStop := func(t *testing.T, lightOK bool) (*exerciseRig, DevExerciseReport, string) {
		t.Helper()
		x, _ := newCursorExerciseRigWith(t, happy(), lightHappy(), cursorExeLinked)
		x.full.f.probeErr["claude"] = errors.New("injected: claude is older than its minimum")
		prev := x.light.devHook
		x.light.devHook = func(d *devSupervisor) {
			if lightOK {
				delete(x.full.f.probeErr, "claude")
			}
			prev(d)
		}
		if code := x.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		code, out := exerciseCheckOut(x.root())
		var rep DevExerciseReport
		if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 1 {
			t.Fatalf("offline %d %v %s", code, err, out)
		}
		return x, rep, out
	}
	for _, lightOK := range []bool{false, true} {
		t.Run(fmt.Sprintf("early_stop_participants_%d", map[bool]int{false: 0, true: 1}[lightOK]), func(t *testing.T) {
			t.Parallel()
			x, rep, out := earlyStop(t, lightOK)
			ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
			full := filepath.Join(x.root(), filepath.FromSlash(exerciseFullDir))
			b, err := LoadDevBundle(full)
			if err != nil || countEvents(b, func(e Event) bool { return e.Type == EvVersionsChecked }) != 0 || len(cursorVersionRecords(b.Manifest.Versions)) != 0 {
				t.Fatalf("the full child reached its versions: %v", err)
			}
			wantLight := childFail
			if lightOK {
				wantLight = childPass
			}
			if ex.Children[0].Status != childFail || ex.Children[0].Stop != "version_preflight_failed" || ex.Children[1].Status != wantLight ||
				rep.Children[0].Mechanical != ResultFail || rep.Result != ResultFail {
				t.Fatalf("children %+v report %+v", ex.Children, rep.Children)
			}
			// The early stop keeps its own codes and gains no version code.
			if slices.Contains(rep.Codes, CodeCursorExecutableMismatch) || strings.Contains(out, CodeCursorExecutableMismatch) {
				t.Fatalf("codes %v", rep.Codes)
			}
			if code, crep, _ := devCheck(t, full); code != 1 || crep.Result == ResultPass || crep.Stop != "version_preflight_failed" ||
				strings.Contains(fmt.Sprint(devCodes(crep)), CodeCursorExecutableMismatch) {
				t.Fatalf("full child %d %s %v", code, crep.Stop, devCodes(crep))
			}
			if !lightOK {
				return
			}
			// The one participating child is still validated: an empty
			// stored version is a mismatch.
			dir := filepath.Join(t.TempDir(), "root")
			if err := copyTree(x.root(), dir); err != nil {
				t.Fatal(err)
			}
			set("")(t, dir)
			rewriteExerciseDerived(t, dir)
			if code, out := exerciseCheckOut(dir); code != 1 || !strings.Contains(out, CodeCursorExecutableMismatch) {
				t.Fatalf("offline %d %s", code, out)
			}
		})
	}
}
