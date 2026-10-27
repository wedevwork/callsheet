package reale2e

import (
	"bytes"
	"errors"
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
