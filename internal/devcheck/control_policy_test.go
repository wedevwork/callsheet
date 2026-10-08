package devcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// wantControlDelegations is iterations 06a's and 06b's delegation table,
// literally:
// wrapper | package | selector | required names.
var wantControlDelegations = []string{
	"TestControlDurability|./internal/plane|^TestControlCommit$|TestControlCommit",
	"TestControlReconnect|./internal/contract|^TestControlProtocol$|TestControlProtocol",
	"TestControlReconnect|./internal/plane|^TestControlProtocol$|TestControlProtocol",
	"TestControlReconnect|./internal/sidecar|^TestControlProtocol$|TestControlProtocol",
	"TestControlNodeLoss|./internal/plane|^TestControlLease$|TestControlLease",
	"TestControlNodeLoss|./internal/sidecar|^TestControlLease$|TestControlLease",
	"TestControlLaunchSafety|./internal/sidecar|^TestControlLaunch$|TestControlLaunch",
	"TestControlWorkerRecovery|./internal/sidecar|^TestControlRestart$|TestControlRestart",
	"TestControlPlaneRecovery|./internal/plane|^TestControlPlaneRestart$|TestControlPlaneRestart",
	"TestControlPlaneRecovery|./internal/sidecar|^TestControlPlaneRestart$|TestControlPlaneRestart",
	"TestControlLateResult|./internal/plane|^TestControlLate$|TestControlLate",
	"TestControlLateResult|./internal/sidecar|^TestControlLate$|TestControlLate",
	"TestControlLegacy|./internal/contract|^TestControlMigration$|TestControlMigration",
	"TestControlLegacy|./internal/plane|^TestControlMigration$|TestControlMigration",
	// Iteration 06b: 13 rows for the four task-control wrappers.
	"TestControlCancellation|./internal/plane|^TestControlCancel$|TestControlCancel,TestControlCancel/unsent,TestControlCancel/loaded-pending,TestControlCancel/durable-order,TestControlCancel/storage-retry,TestControlCancel/offline,TestControlCancel/duplicate",
	"TestControlCancellation|./internal/sidecar|^TestControlCancel$|TestControlCancel,TestControlCancel/preparing,TestControlCancel/guardian-control,TestControlCancel/partial-output,TestControlCancel/cleanup-unconfirmed,TestControlCancel/duplicate",
	"TestControlCancellation|./internal/client|^TestControlCancel$|TestControlCancel,TestControlCancel/accepted,TestControlCancel/terminal,TestControlCancel/errors",
	"TestControlCancellation|./internal/cli|^TestControlCancel$|TestControlCancel,TestControlCancel/accepted,TestControlCancel/terminal,TestControlCancel/errors",
	"TestControlExecutionTimeout|./internal/sidecar|^TestControlTimeout$|TestControlTimeout,TestControlTimeout/default-override-zero,TestControlTimeout/deadline-tie,TestControlTimeout/slow-start,TestControlTimeout/plane-outage,TestControlTimeout/restart,TestControlTimeout/status-failure",
	"TestControlExecutionTimeout|./internal/contract|^TestControlTimeout$|TestControlTimeout,TestControlTimeout/policy,TestControlTimeout/outcome,TestControlTimeout/migration",
	"TestControlBoundedWait|./internal/plane|^TestControlWait$|TestControlWait,TestControlWait/register-race,TestControlWait/any-of,TestControlWait/deadline,TestControlWait/capacity,TestControlWait/shutdown,TestControlWait/dispatch",
	"TestControlBoundedWait|./internal/client|^TestControlWait$|TestControlWait,TestControlWait/restart-budget,TestControlWait/no-dispatch-retry,TestControlWait/deadline,TestControlWait/correlation",
	"TestControlBoundedWait|./internal/cli|^TestControlWait$|TestControlWait,TestControlWait/text,TestControlWait/json,TestControlWait/exit",
	"TestControlBoundedWait|./internal/contract|^TestControlWait$|TestControlWait,TestControlWait/bounds,TestControlWait/validation",
	"TestControlForceRemove|./internal/plane|^TestControlRemove$|TestControlRemove,TestControlRemove/fence,TestControlRemove/drain,TestControlRemove/restart,TestControlRemove/storage-retry,TestControlRemove/instance-reuse",
	"TestControlForceRemove|./internal/sidecar|^TestControlRemove$|TestControlRemove,TestControlRemove/removed-instance-cleanup",
	"TestControlForceRemove|./internal/cli|^TestControlRemove$|TestControlRemove,TestControlRemove/pending,TestControlRemove/completed,TestControlRemove/retry",
}

// wantControlNative is the exact 06a suffix of the native required names.
var wantControlNative = []string{
	"TestControlDurability", "TestControlReconnect", "TestControlNodeLoss", "TestControlLaunchSafety", "TestControlWorkerRecovery",
	"TestControlPlaneRecovery", "TestControlLateResult", "TestControlLegacy",
	"TestControlNativeGroups", "TestControlNativeGroups/cooperative", "TestControlNativeGroups/resistant",
	"TestControlNativeGroups/orphan-restart", "TestControlNativeGroups/plane-restart",
}

// wantTaskControlNative is the exact 06b suffix of the native required
// names: the four task-control function parents, one per FP.
var wantTaskControlNative = []string{"TestControlCancellation", "TestControlExecutionTimeout", "TestControlBoundedWait", "TestControlForceRemove"}

// controlWrappers is every control wrapper in table order.
func controlWrappers() []string {
	return append(append([]string(nil), wantControlNative[:8]...), wantTaskControlNative...)
}

// controlTestFiles are the 06a test files the launch guard covers: none
// starts an OS process (the native group qualification in tests/function
// is the only new real-child test, through its counted fixture).
var controlTestFiles = []string{
	"internal/contract/control_test.go",
	"internal/plane/control_commit_test.go", "internal/plane/control_lease_test.go", "internal/plane/control_restart_test.go",
	"internal/sidecar/control_test.go", "internal/sidecar/control_unix_test.go", "internal/sidecar/control_other_test.go",
	"internal/sidecar/control_mode_linux_test.go", "internal/sidecar/control_mode_darwin_test.go",
	"internal/sidecar/fixture_unix_test.go", "internal/sidecar/fixture_other_test.go",
	"internal/devcheck/control_policy_test.go",
	// Iteration 06b: the task-control families (all injected; zero real
	// children).
	"internal/contract/control_ctl_test.go",
	"internal/plane/control_cancel_test.go", "internal/plane/control_wait_test.go", "internal/plane/control_remove_test.go",
	"internal/plane/control_ext_test.go",
	"internal/sidecar/control_cancel_test.go", "internal/sidecar/control_timeout_test.go", "internal/sidecar/control_timeout_unix_test.go",
	"internal/sidecar/control_migrate_test.go", "internal/sidecar/control_fifo_unix_test.go",
	"internal/client/control_test.go", "internal/cli/control_test.go",
}

// TestControlPolicy is UT FP-1-8 for devcheck (iteration 06a): the exact
// function delegation table and its evidence rule (a failed, skipped,
// empty, missing or foreign package invocation is not qualification), the
// exact native suffix, the OS seam guard, the stress routing of the new
// contracts, the real-child counters and the hosted-budget policy, and
// the benchmark selections. Do not rename or skip it.
func TestControlPolicy(t *testing.T) {
	t.Run("table", func(t *testing.T) {
		rows := ControlDelegations()
		if len(rows) != len(wantControlDelegations) {
			t.Fatalf("%d rows", len(rows))
		}
		var wrappers []string
		for i, d := range rows {
			if got := d.Wrapper + "|" + d.Package + "|" + d.Selector + "|" + strings.Join(d.Required, ","); got != wantControlDelegations[i] || strings.ContainsAny(d.Selector, " \t") {
				t.Fatalf("row %d = %s", i, got)
			}
			if !slices.Contains(wrappers, d.Wrapper) {
				wrappers = append(wrappers, d.Wrapper)
			}
		}
		if strings.Join(wrappers, ",") != strings.Join(controlWrappers(), ",") {
			t.Fatalf("wrappers %v are not the twelve FPs in order", wrappers)
		}
		// Every 06b row requires its parent and at least one mandatory
		// subcase of that parent: a vacuous parent never qualifies.
		for _, d := range rows[14:] {
			if len(d.Required) < 2 || "^"+d.Required[0]+"$" != d.Selector {
				t.Fatalf("06b row %+v", d)
			}
			for _, r := range d.Required[1:] {
				if !strings.HasPrefix(r, d.Required[0]+"/") {
					t.Fatalf("06b row %s requires a foreign name %s", d.Wrapper, r)
				}
			}
		}
		rows[0].Required[0] = "mutated"
		if ControlDelegations()[0].Required[0] != "TestControlCommit" {
			t.Fatal("the table is mutable through a copy")
		}
	})
	t.Run("evidence", func(t *testing.T) {
		for _, w := range controlWrappers() {
			good := map[string]string{}
			rows := ControlDelegationsFor(w)
			for _, d := range rows {
				good[d.Package] = passing(d.Required...)
			}
			if err := CheckControlWrapperEvidence(w, good); err != nil {
				t.Fatalf("%s complete: %v", w, err)
			}
			for _, d := range rows {
				for _, cut := range []string{"=== RUN   " + d.Required[0] + "\n", "--- PASS: " + d.Required[0] + " ("} {
					bad := copyOutputs(good)
					bad[d.Package] = strings.Replace(bad[d.Package], cut, "", 1)
					if err := CheckControlWrapperEvidence(w, bad); err == nil || !strings.Contains(err.Error(), unobserved) {
						t.Fatalf("%s without %q in %s: %v", w, cut, d.Package, err)
					}
				}
				for name, out := range map[string]string{
					"failed child":  good[d.Package] + "    --- FAIL: " + d.Required[0] + "/x (0.00s)\n",
					"skipped child": good[d.Package] + "    --- SKIP: " + d.Required[0] + "/x (0.00s)\n",
					"empty":         "testing: warning: no tests to run\nPASS\n",
					"blank":         "",
				} {
					bad := copyOutputs(good)
					bad[d.Package] = out
					if err := CheckControlWrapperEvidence(w, bad); err == nil {
						t.Fatalf("%s with %s %s evidence passed", w, name, d.Package)
					}
				}
				// A missing package invocation is unobserved.
				missing := copyOutputs(good)
				delete(missing, d.Package)
				if err := CheckControlWrapperEvidence(w, missing); err == nil || !strings.Contains(err.Error(), "has no invocation of "+d.Package) {
					t.Fatalf("%s without %s: %v", w, d.Package, err)
				}
			}
			extra := copyOutputs(good)
			extra["./internal/adapter"] = passing(rows[0].Required...)
			if err := CheckControlWrapperEvidence(w, extra); err == nil || !strings.Contains(err.Error(), "outside its table rows") {
				t.Fatalf("%s accepted a foreign package: %v", w, err)
			}
		}
		if err := CheckControlWrapperEvidence("TestControlNothing", nil); err == nil {
			t.Fatal("an unknown wrapper passed")
		}
	})
	t.Run("native", func(t *testing.T) {
		// The 128 earlier names first and unchanged, then exactly the 06a
		// suffix, then exactly the four 06b parents (145), then only the
		// 07a MCP names (222), then only the 07b setup and qualification
		// names; no 06b name before them; the sidecar process tuple is
		// unchanged.
		req := NativeRequiredTests()
		real := len(realNames())
		if len(req) != 145+len(mcpNames())+len(qualNames())+real+len(wsNames())+len(trNames())+len(latNames())+len(wsTaskNames())+len(wsDoorNames())+len(wave2Names())+len(nbwNames())+len(dceNames())+len(b1Names())+len(b15Names())+len(b2Names()) || strings.Join(req[128:141], ",") != strings.Join(wantControlNative, ",") ||
			strings.Join(req[87:128], ",") != strings.Join(wantTaskSuffix, ",") || strings.Join(req[141:145], ",") != strings.Join(wantTaskControlNative, ",") ||
			strings.Join(req[145:222], ",") != strings.Join(mcpNames(), ",") || strings.Join(req[222:273], ",") != strings.Join(qualNames(), ",") ||
			strings.Join(req[273:273+real], ",") != strings.Join(realNames(), ",") || strings.Join(req[273+real:273+real+len(wsNames())], ",") != strings.Join(wsNames(), ",") ||
			strings.Join(req[273+real+len(wsNames()):273+real+len(wsNames())+len(trNames())], ",") != strings.Join(trNames(), ",") ||
			strings.Join(req[273+real+len(wsNames())+len(trNames()):273+real+len(wsNames())+len(trNames())+len(latNames())], ",") != strings.Join(latNames(), ",") ||
			strings.Join(req[273+real+len(wsNames())+len(trNames())+len(latNames()):273+real+len(wsNames())+len(trNames())+len(latNames())+len(wsTaskNames())], ",") != strings.Join(wsTaskNames(), ",") ||
			strings.Join(req[273+real+len(wsNames())+len(trNames())+len(latNames())+len(wsTaskNames()):], ",") != strings.Join(append(append(append(append(append(append(wsDoorNames(), wave2Names()...), nbwNames()...), dceNames()...), b1Names()...), b15Names()...), b2Names()...), ",") {
			t.Fatalf("native suffix %v", req[128:])
		}
		for _, n := range req[:141] {
			if strings.Contains(n, "Cancel") || strings.Contains(n, "Wait") || strings.Contains(n, "Timeout") || strings.Contains(n, "ForceRemove") {
				t.Fatalf("a 06b name %s is required before the 06b suffix", n)
			}
		}
		// The sidecar tuple keeps its iteration 05 pair first; iteration 08
		// appends only its tagged contract's six names and iteration 11 that
		// contract's four wave-2 subtests.
		if strings.Join(NativeTaskProcessTests(), ",") != "TestTaskExecutionContract,TestTaskExecutionContract/process,"+strings.Join(realLocal, ",") {
			t.Fatal("the sidecar native tuple changed")
		}
		// Missing, failed or skipped native control evidence fails the
		// native check.
		for _, name := range []string{"TestControlLegacy", "TestControlNativeGroups/plane-restart", "TestControlCancellation", "TestControlExecutionTimeout",
			"TestControlBoundedWait", "TestControlForceRemove"} {
			if err := check(stream(without(qualification(), "pass", name)...)); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("native without %s's pass: %v", name, err)
			}
			if err := check(stream(append(qualification(), ev("skip", NativePackage, name))...)); err == nil {
				t.Fatalf("native with %s skipped passed", name)
			}
		}
		if err := check(stream(qualification()...)); err != nil {
			t.Fatalf("complete native evidence: %v", err)
		}
	})
	t.Run("guards", func(t *testing.T) {
		root := repoRoot(t)
		// The OS seam guard is unchanged and still holds with the guardian.
		if err := CheckPlatformSources(root); err != nil {
			t.Fatalf("platform guard: %v", err)
		}
		if len(platformGuardPolicy.wrappers) != 6 || len(platformGuardPolicy.exemptions) != 6 {
			t.Fatal("the platform guard's exception list grew")
		}
		// No new package contract starts an OS process or enables real
		// children; the only real-child change is the process
		// qualification's ledger (five per repetition).
		launch := regexp.MustCompile(`exec\.Command|os\.StartProcess|syscall\.ForkExec|"os/exec"|BuildBinaryAt\(|ledger\.real = true`)
		for _, f := range controlTestFiles {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
			if err != nil {
				t.Fatalf("control test file %s: %v", f, err)
			}
			if f == "internal/devcheck/control_policy_test.go" {
				continue // this file names the forbidden calls
			}
			if m := launch.FindString(string(b)); m != "" {
				t.Fatalf("%s launches processes (%s)", f, m)
			}
		}
		if len(wantLaunchLedger) != 7 || wantLaunchLedger[0] != "sidecar TestTaskExecutionContract/process|5" {
			t.Fatal("the launch ledger changed")
		}
		exe, _ := os.ReadFile(filepath.Join(root, "internal", "sidecar", "task_execution_test.go"))
		if !strings.Contains(string(exe), "want exactly 1 and 4") {
			t.Fatal("the process qualification lost its guardian ledger assertion")
		}
		// Production never imports testkit; the guardian dispatch sits in
		// the callsheet binary's run, before signal handling and the CLI.
		main, _ := os.ReadFile(filepath.Join(root, "cmd", "callsheet", "main.go"))
		m := string(main)
		if i, j := strings.Index(m, "sidecar.GuardianToken"), strings.Index(m, "signal.NotifyContext"); i < 0 || j < 0 || i > j {
			t.Fatal("the guardian is not dispatched before signal handling")
		}
	})
	t.Run("stress", func(t *testing.T) {
		// Routing: the new contracts run in their packages' existing
		// shards (plane and sidecar with their three CPU settings
		// concurrent, contract in the packages shard, since the contract
		// headroom fix as its per-CPU group, in the packages-cpu shard
		// since the stress worker rebalance); the function wrappers and
		// native groups stay out of stress-functions.
		if stressPlanePackage != "./internal/plane" || stressSidecarPackage != "./internal/sidecar" || !slices.Contains(stressSplitPackages, "./internal/contract") {
			t.Fatal("a control contract's package left its shard")
		}
		for _, sel := range append(append(append([]string{}, stressFunctionTests...), stressPlaneFunctionTests...), stressNodeFunctionTests...) {
			if strings.HasPrefix(sel, "TestControl") {
				t.Fatalf("%s entered stress-functions", sel)
			}
		}
		for _, goos := range []string{"linux", "darwin"} {
			shards, err := StressShards(goos)
			if err != nil || len(shards) != 8 {
				t.Fatalf("%s shards %v %v", goos, shards, err)
			}
			for _, sh := range shards {
				for _, st := range slices.Concat(append([][]Step{sh.Steps}, sh.CPUGroups...)...) {
					a := strings.Join(st.Argv, " ")
					if !strings.Contains(a, "-race") || !strings.Contains(a, "-count=20") || !strings.Contains(a, "-timeout=6m") {
						t.Fatalf("%s %s: %s", goos, sh.Name, a)
					}
				}
				// Design 06a-perf: the CPU1 singletons, the CPU2/CPU4 pairs
				// and processgroup's three are Parallel shards.
				if want, ok := map[string]int{"plane-cpu1": 1, "plane": 2, "sidecar-cpu1": 1, "sidecar": 2, "processgroup": 3}[sh.Name]; ok && (!sh.Parallel || len(sh.Steps) != want) {
					t.Fatalf("%s %s is not %d concurrent invocations", goos, sh.Name, want)
				}
			}
		}
		// Hosted budget policy: the count, the per-binary timeout and the
		// concurrency are fixed; no plane fallback wave is applied.
		if StressCount != 20 || stressTestTimeout != "6m" || maxConcurrentCPU != 3 || len(stressWaves) != 0 {
			t.Fatal("the stress budget policy changed")
		}
		// The complete suite (with tests/function) keeps its shared 180 s
		// bound; iteration 08's tagged step runs internal/sidecar's one
		// contract only.
		for _, st := range TestSteps("linux") {
			if slices.Contains(st.Argv, "./...") && !slices.Contains(st.Argv, "-timeout=180s") {
				t.Fatalf("the function package timeout changed: %v", st.Argv)
			}
			if !slices.Contains(st.Argv, "./...") && !slices.Equal(st.Argv[len(st.Argv)-3:], []string{"./internal/sidecar", "-run=^TestRealAdapterLocal$", "-count=1"}) {
				t.Fatalf("a partial test step other than the tagged sidecar contract: %v", st.Argv)
			}
		}
	})
	t.Run("bench", func(t *testing.T) {
		// The new benchmarks exist and their packages are selected with
		// -bench=. (an empty selection would pass unobserved).
		root := repoRoot(t)
		for _, pn := range [][2]string{{"internal/contract", "BenchmarkControlReplay"}, {"internal/sidecar", "BenchmarkControlReplay"},
			{"internal/plane", "BenchmarkControlCommit"}, {"internal/plane", "BenchmarkControlWait"}} {
			pkg, name := pn[0], pn[1]
			found := false
			entries, _ := os.ReadDir(filepath.Join(root, filepath.FromSlash(pkg)))
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), "_test.go") {
					b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(pkg), e.Name()))
					found = found || strings.Contains(string(b), "func "+name+"(b *testing.B)")
				}
			}
			if !found {
				t.Fatalf("%s has no %s", pkg, name)
			}
			selected := false
			for _, st := range BenchSteps() {
				selected = selected || (slices.Contains(st.Argv, "./"+pkg) && slices.Contains(st.Argv, "-bench=."))
			}
			if !selected {
				t.Fatalf("%s's benchmarks are not selected", pkg)
			}
		}
	})
}
