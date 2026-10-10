package reale2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// indexOf returns the first argv entry containing sub, or -1.
func indexOf(argv []string, sub string, from int) int {
	for i := from; i < len(argv); i++ {
		if strings.Contains(argv[i], sub) {
			return i
		}
	}
	return -1
}

// TestDeploymentPreparation (FP-2): end-to-end preparation with injected
// processes and plane: a short private runtime, a loopback plane, one
// enrolled sidecar per adapter with only its explicit path, the four roles
// with the specified defaults registered before any task, every sidecar
// reconnected, all roles ready with a recorded snapshot, and the seed
// repository with its manifest and baseline tests, all before the
// coordinator is given anything.
func TestDeploymentPreparation(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	if code := f.attempt(); code != 0 {
		t.Fatalf("attempt = %d %s", code, f.stderr.String())
	}
	s := f.sup
	// Runtime: short, private, a new ce-* directory of the temporary root.
	if filepath.Dir(s.runtime) != f.tempRoot || !strings.HasPrefix(filepath.Base(s.runtime), "ce-") {
		t.Fatalf("runtime %s", s.runtime)
	}
	if p, err := socketPath(s.runtime); err != nil || len(p) > 90 {
		t.Fatalf("socket path %q %v", p, err)
	}
	argv := f.launcher.argv()
	order := []string{"callsheet plane init --state-dir " + s.runtime + "/p --bind 127.0.0.1:0 --san 127.0.0.1", "callsheet plane run"}
	for _, a := range sidecarAdapters {
		order = append(order, "sidecar enroll --plane https://127.0.0.1:43210 --ca "+s.runtime+"/p/pki/ca.crt --state-dir "+s.runtime+"/n-"+a,
			"sidecar run --state-dir "+s.runtime+"/n-"+a+" --"+a+"-adapter /fake/bin/"+a)
	}
	for _, a := range sidecarAdapters {
		order = append(order, "sidecar run --state-dir "+s.runtime+"/n-"+a+" --"+a+"-adapter /fake/bin/"+a)
	}
	at := 0
	for _, want := range order {
		i := indexOf(argv, want, at)
		if i < 0 {
			t.Fatalf("missing %q after %d in\n%s", want, at, strings.Join(argv, "\n"))
		}
		at = i + 1
	}
	for _, a := range argv {
		if strings.Contains(a, "sidecar run") && strings.Count(a, "-adapter") != 1 {
			t.Fatalf("a sidecar enables more than its own adapter: %s", a)
		}
	}
	// Roles: the defaults, registered before any task.
	if len(f.plane.roles) != 4 {
		t.Fatalf("roles %v", f.plane.roles)
	}
	for i, r := range f.plane.roles {
		d := DefaultFlow().Roles[i]
		if r.ID != d.ID || r.Adapter != d.Adapter || r.Model != d.Model || r.Effort != d.Effort || r.Concurrency != 1 || r.Timeout != RoleTimeout ||
			!r.HasTimeout || r.Node != hexID("n_", slices.Index(sidecarAdapters, d.Adapter)+1) {
			t.Fatalf("role %d = %+v", i, r.RoleConfig)
		}
		for _, p := range []string{r.Instruction, r.Runbook} {
			if !strings.HasPrefix(p, s.runtime+"/m/") {
				t.Fatalf("manual outside the runtime: %s", p)
			}
		}
	}
	types := eventTypesOf(t, f.bundle())
	seq := []string{EvRunStarted, EvPreflightStarted, EvVersionsChecked, EvRuntimeCreated, EvProcessStarted, EvRolesRegistered,
		EvSidecarsReconnected, EvRolesReady, EvTaskAdmitted, EvSeedCreated, EvBaselineTests, EvCoordinatorReady}
	at = 0
	for _, w := range seq {
		i := slices.Index(types[at:], w)
		if i < 0 {
			t.Fatalf("event %s out of order in %v", w, types)
		}
		at += i + 1
	}
	var roles RolesRecord
	readDoc(t, filepath.Join(f.bundle(), "setup", "roles.json"), &roles)
	if len(roles.Roles) != 4 || roles.Roles[3].Adapter != "grok" || !roles.Roles[3].CanAccept {
		t.Fatalf("roles snapshot %+v", roles)
	}
	// The seed: a deterministic commit of the four files, recorded.
	var m Manifest
	readDoc(t, filepath.Join(f.bundle(), "manifest.json"), &m)
	if m.Workspace.SeedCommit != s.seed.Commit.String() || m.Workspace.Name != s.workspace.Name || m.Workspace.Instance != f.plane.ws.Instance {
		t.Fatalf("workspace %+v", m.Workspace)
	}
	if !slices.Equal(sortedKeys(s.seed.Files), []string{"FEATURE.md", "go.mod", "main.go", "main_test.go"}) ||
		string(s.seed.Files["go.mod"]) != "module example.com/greeting\n\ngo 1.26.0\n" {
		t.Fatalf("seed files %v", sortedKeys(s.seed.Files))
	}
	var base TestRun
	readDoc(t, filepath.Join(f.bundle(), "validation", "baseline.json"), &base)
	if base.Commit != m.Workspace.SeedCommit || base.ExitCode != 0 || base.Toolchain == "" {
		t.Fatalf("baseline %+v", base)
	}
	// Evidence is private.
	st, _ := os.Stat(f.bundle())
	fst, _ := os.Stat(filepath.Join(f.bundle(), "manifest.json"))
	if st.Mode().Perm() != 0o700 || fst.Mode().Perm() != 0o600 {
		t.Fatalf("evidence modes %v %v", st.Mode(), fst.Mode())
	}
}

// TestDeploymentUnits (UT-2): ordering failures unwind every owned child,
// readiness is awaited, the documented transient refusal is retried
// boundedly, long socket paths and non-loopback planes are refused, and
// the seed is deterministic.
func TestDeploymentUnits(t *testing.T) {
	t.Parallel()
	stoppedAll := func(t *testing.T, f *fakeWorld, code string) {
		t.Helper()
		if f.sup.failCode != code {
			t.Fatalf("failure %q, want %q\n%s", f.sup.failCode, code, f.stderr.String())
		}
		running := 0
		for _, p := range f.launcher.procs {
			if !isDone(p) {
				running++
			}
		}
		var c CleanupRecord
		readDoc(t, filepath.Join(f.bundle(), "cleanup.json"), &c)
		// Every child is stopped, or cleanup says it is not.
		if (running > 0) == c.Complete {
			t.Fatalf("%d children left running, cleanup complete %v", running, c.Complete)
		}
		if _, err := os.Stat(filepath.Join(f.bundle(), "cleanup.json")); err != nil {
			t.Fatal("no cleanup record")
		}
	}
	child := func(match func(a []string) bool, b behavior) func(ProcSpec) (behavior, bool) {
		return func(s ProcSpec) (behavior, bool) {
			if s.Path == fakeCallsheet && match(s.Args) {
				return b, true
			}
			return behavior{}, false
		}
	}
	isRun := func(kind string) func([]string) bool {
		return func(a []string) bool { return len(a) > 1 && a[0] == kind && a[1] == "run" }
	}
	cases := []struct {
		name  string
		setup func(f *fakeWorld)
		code  string
	}{
		{"plane init fails", func(f *fakeWorld) {
			f.override = child(func(a []string) bool { return a[0] == "plane" && a[1] == "init" }, behavior{exit: 1})
		}, "plane_start_failed"},
		{"plane exits", func(f *fakeWorld) { f.override = child(isRun("plane"), behavior{exit: 1}) }, "plane_start_failed"},
		{"plane not loopback", func(f *fakeWorld) {
			f.override = child(isRun("plane"), behavior{long: true, stderr: `{"msg":"listening","bind":"0.0.0.0:1"}` + "\n"})
		}, "plane_start_failed"},
		{"plane start error", func(f *fakeWorld) { f.override = child(isRun("plane"), behavior{startErr: errors.New("no exec")}) }, "plane_start_failed"},
		{"connect fails", func(f *fakeWorld) { f.connectErr = errors.New("bad trust") }, "plane_connect_failed"},
		{"enroll fails", func(f *fakeWorld) {
			f.override = child(func(a []string) bool { return a[0] == "sidecar" && a[1] == "enroll" }, behavior{stdout: "error\n"})
		}, "sidecar_start_failed"},
		{"sidecar exits", func(f *fakeWorld) { f.override = child(isRun("sidecar"), behavior{exit: 1}) }, "sidecar_start_failed"},
		{"sidecar start error", func(f *fakeWorld) { f.override = child(isRun("sidecar"), behavior{startErr: errors.New("x")}) }, "sidecar_start_failed"},
		{"role refused", func(f *fakeWorld) {
			f.plane.addRoleErrs = []error{nil, contract.New(contract.CodeInvalidArgument, "node rejected role")}
		}, "role_registration_failed"},
		{"sidecar does not stop", func(f *fakeWorld) {
			f.override = child(isRun("sidecar"), behavior{long: true, ignoreTerm: true, ignoreKill: true, stderr: `{"msg":"heartbeat acknowledged"}` + "\n"})
		}, "sidecar_reconnect_failed"},
		{"roles never ready", func(f *fakeWorld) { f.plane.notReadyCalls = 1 << 20 }, "roles_not_ready"},
		{"workspace refused", func(f *fakeWorld) { f.plane.createWSErr = contract.New(contract.CodeUnavailable, "full") }, "workspace_create_failed"},
		{"baseline fails", func(f *fakeWorld) {
			f.override = func(s ProcSpec) (behavior, bool) {
				if s.Path == fakeGo && s.Args[0] == "test" {
					return behavior{exit: 1, stdout: "FAIL\n"}, true
				}
				return behavior{}, false
			}
		}, "baseline_tests_failed"},
		{"checkout go.mod unreadable", func(f *fakeWorld) {
			f.override = func(s ProcSpec) (behavior, bool) {
				if s.Path == fakeCallsheet && s.Args[0] == "sidecar" && s.Args[1] == "enroll" && strings.HasSuffix(s.Args[len(s.Args)-1], "n-grok") {
					os.WriteFile(filepath.Join(f.checkout, "go.mod"), []byte("module "+modulePath+"\n"), 0o644)
				}
				return behavior{}, false
			}
		}, "checkout_unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			tc.setup(f)
			if code := f.attempt(); code != 1 {
				t.Fatalf("attempt = %d", code)
			}
			stoppedAll(t, f, tc.code)
		})
	}
	t.Run("transient role refusal", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		busy := contract.New(contract.CodeUnavailable, "another role change holds the registry; retry")
		f.plane.addRoleErrs = []error{busy, busy, nil}
		f.plane.notReadyCalls = 3
		if code := f.attempt(); code != 0 {
			t.Fatalf("attempt = %d %s", code, f.sup.failCode)
		}
	})
	t.Run("socket path bound", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		long := filepath.Join(f.tempRoot, strings.Repeat("d", 80))
		os.MkdirAll(long, 0o700)
		f.tempRoot = long
		if code := f.attempt(); code != 1 || f.sup.failCode != "socket_path_too_long" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
	})
	t.Run("seed determinism", func(t *testing.T) {
		t.Parallel()
		a, err := createSeed(t.TempDir(), seedFiles("1.26.0"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := createSeed(t.TempDir(), seedFiles("1.26.0"))
		if err != nil || a.Commit != b.Commit || a.Tree != b.Tree {
			t.Fatalf("seeds differ: %v %v %v", a.Commit, b.Commit, err)
		}
		if _, err := createSeed(filepath.Join(t.TempDir(), "x", "\x00"), seedFiles("1.26.0")); err == nil {
			t.Fatal("an invalid seed path passed")
		}
		if _, err := goDirective([]byte("module x\n")); err == nil {
			t.Fatal("no go directive passed")
		}
	})
	t.Run("checkout and evidence", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.evidence = t.TempDir()
		if code := f.attempt(); code != 2 || !strings.Contains(f.stderr.String(), "inside the checkout's ignored design/ tree") {
			t.Fatalf("outside evidence = %d %q", code, f.stderr.String())
		}
		f = newFakeWorld(t)
		f.checkout = t.TempDir()
		if code := f.attempt(); code != 2 || !strings.Contains(f.stderr.String(), "Callsheet checkout") {
			t.Fatalf("no checkout = %d %q", code, f.stderr.String())
		}
		for name, dest := range map[string]func(f *fakeWorld) string{
			"missing parent": func(f *fakeWorld) string { return filepath.Join(f.checkout, "design", "missing", "run") },
			"existing":       func(f *fakeWorld) string { return filepath.Join(f.checkout, "design", "real-e2e-runs") },
			"the design dir": func(f *fakeWorld) string { return filepath.Join(f.checkout, "design") },
			"root":           func(*fakeWorld) string { return "/" },
			"parent is a file": func(f *fakeWorld) string {
				os.WriteFile(filepath.Join(f.checkout, "design", "file"), nil, 0o600)
				return filepath.Join(f.checkout, "design", "file", "run")
			},
		} {
			f = newFakeWorld(t)
			f.evidence = dest(f)
			if code := f.attempt(); code != 2 || !strings.Contains(f.stderr.String(), "must not exist yet") {
				t.Fatalf("%s = %d %q", name, code, f.stderr.String())
			}
		}
		// Through a symlinked parent inside design/, a new destination is fine.
		f = newFakeWorld(t)
		link := filepath.Join(f.checkout, "design", "runs-link")
		os.Symlink(filepath.Join(f.checkout, "design", "real-e2e-runs"), link)
		f.evidence = filepath.Join(link, "run-1")
		if code := f.attempt(); code != 0 {
			t.Fatalf("symlinked parent = %d %q", code, f.stderr.String())
		}
		if _, err := os.Stat(filepath.Join(f.checkout, "design", "real-e2e-runs", "run-1", "report.json")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("random source", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		w := f.world()
		w.Rand = failingReader{}
		s := &supervisor{w: w, stdout: f.stdout, stderr: f.stderr, args: runArgs{evidence: f.evidence}, ctx: context.Background()}
		if code := s.run(); code != 1 {
			t.Fatalf("run = %d", code)
		}
	})
}

// failingReader fails every read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }
