package devcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Literal oracles for iteration 05's verification policy (design 05,
// Testing plan), compared against the executable tables, never derived
// from them.

// wantTaskDelegations is the design's 41-row four-column table.
var wantTaskDelegations = []string{
	"TestTaskModel/envelope|./internal/contract|^TestTaskContract$/^envelope$|TestTaskContract,TestTaskContract/envelope",
	"TestTaskModel/states|./internal/plane|^TestTaskStore$/^states$|TestTaskStore,TestTaskStore/states",
	"TestTaskDispatch/selection|./internal/plane|^TestTaskAdmission$/^selection$|TestTaskAdmission,TestTaskAdmission/selection",
	"TestTaskDispatch/gate-race|./internal/plane|^TestTaskAdmission$/^gate-race$|TestTaskAdmission,TestTaskAdmission/gate-race",
	"TestTaskDispatch/reserved-slot|./internal/plane|^TestTaskAdmission$/^reserved-slot$|TestTaskAdmission,TestTaskAdmission/reserved-slot",
	"TestTaskProtocol/duplex|./internal/plane|^TestTaskStream$/^duplex$|TestTaskStream,TestTaskStream/duplex",
	"TestTaskProtocol/duplex|./internal/sidecar|^TestTaskStream$/^duplex$|TestTaskStream,TestTaskStream/duplex",
	"TestTaskProtocol/bounds|./internal/plane|^TestTaskStream$/^bounds$|TestTaskStream,TestTaskStream/bounds",
	"TestTaskProtocol/bounds|./internal/sidecar|^TestTaskStream$/^bounds$|TestTaskStream,TestTaskStream/bounds",
	"TestTaskProtocol/result-receipt|./internal/plane|^TestTaskStream$/^result-receipt$|TestTaskStream,TestTaskStream/result-receipt",
	"TestTaskProtocol/result-receipt|./internal/sidecar|^TestTaskStream$/^result-receipt$|TestTaskStream,TestTaskStream/result-receipt",
	"TestTaskProtocol/result-ack-loss|./internal/plane|^TestTaskStream$/^result-ack-loss$|TestTaskStream,TestTaskStream/result-ack-loss",
	"TestTaskProtocol/result-ack-loss|./internal/sidecar|^TestTaskStream$/^result-ack-loss$|TestTaskStream,TestTaskStream/result-ack-loss",
	"TestTaskExecution/compose|./internal/sidecar|^TestTaskExecutionContract$/^compose$|TestTaskExecutionContract,TestTaskExecutionContract/compose",
	"TestTaskExecution/invoke|./internal/adapter|^TestTaskAdapter$/^(invocation|extraction|signals)$|TestTaskAdapter,TestTaskAdapter/invocation,TestTaskAdapter/extraction,TestTaskAdapter/signals",
	"TestTaskExecution/exit|./internal/sidecar|^TestTaskExecutionContract$/^exit$|TestTaskExecutionContract,TestTaskExecutionContract/exit",
	"TestTaskExecution/process|./internal/sidecar|^TestTaskExecutionContract$/^process$|TestTaskExecutionContract,TestTaskExecutionContract/process",
	"TestTaskExecution/platform|./internal/sidecar|^TestTaskPlatform$/^platforms$|TestTaskPlatform,TestTaskPlatform/platforms",
	"TestTaskExecution/policy|./internal/devcheck|^TestTaskPolicy$/^policy$|TestTaskPolicy,TestTaskPolicy/policy",
	"TestTaskLogs/retention|./internal/plane|^TestTaskOutput$/^retention$|TestTaskOutput,TestTaskOutput/retention",
	"TestTaskLogs/retention|./internal/sidecar|^TestTaskOutput$/^retention$|TestTaskOutput,TestTaskOutput/retention",
	"TestTaskLogs/backpressure|./internal/plane|^TestTaskOutput$/^backpressure$|TestTaskOutput,TestTaskOutput/backpressure",
	"TestTaskLogs/backpressure|./internal/sidecar|^TestTaskOutput$/^backpressure$|TestTaskOutput,TestTaskOutput/backpressure",
	"TestTaskLogs/final|./internal/plane|^TestTaskOutput$/^final$|TestTaskOutput,TestTaskOutput/final",
	"TestTaskLogs/final|./internal/sidecar|^TestTaskOutput$/^final$|TestTaskOutput,TestTaskOutput/final",
	"TestTaskPersistence/durability|./internal/plane|^TestTaskStore$/^durability$|TestTaskStore,TestTaskStore/durability",
	"TestTaskPersistence/restore|./internal/plane|^TestTaskStore$/^restore$|TestTaskStore,TestTaskStore/restore",
	"TestTaskRoles/counts|./internal/plane|^TestTaskRoleIntegration$/^counts$|TestTaskRoleIntegration,TestTaskRoleIntegration/counts",
	"TestTaskRoles/counts|./internal/sidecar|^TestTaskRoleIntegration$/^counts$|TestTaskRoleIntegration,TestTaskRoleIntegration/counts",
	"TestTaskRoles/mutation|./internal/plane|^TestTaskRoleIntegration$/^mutation$|TestTaskRoleIntegration,TestTaskRoleIntegration/mutation",
	"TestTaskRoles/mutation|./internal/sidecar|^TestTaskRoleIntegration$/^mutation$|TestTaskRoleIntegration,TestTaskRoleIntegration/mutation",
	"TestTaskRoles/remaining-capacity|./internal/plane|^TestTaskRoleIntegration$/^remaining-capacity$|TestTaskRoleIntegration,TestTaskRoleIntegration/remaining-capacity",
	"TestTaskRoles/remaining-capacity|./internal/sidecar|^TestTaskRoleIntegration$/^remaining-capacity$|TestTaskRoleIntegration,TestTaskRoleIntegration/remaining-capacity",
	"TestTaskRoles/recovery-remove|./internal/plane|^TestTaskRoleIntegration$/^recovery-remove$|TestTaskRoleIntegration,TestTaskRoleIntegration/recovery-remove",
	"TestTaskRoles/recovery-remove|./internal/sidecar|^TestTaskRoleIntegration$/^recovery-remove$|TestTaskRoleIntegration,TestTaskRoleIntegration/recovery-remove",
	"TestTaskRecoveryBoundary/disconnect|./internal/plane|^TestTaskInterruption$/^disconnect$|TestTaskInterruption,TestTaskInterruption/disconnect",
	"TestTaskRecoveryBoundary/disconnect|./internal/sidecar|^TestTaskInterruption$/^disconnect$|TestTaskInterruption,TestTaskInterruption/disconnect",
	"TestTaskRecoveryBoundary/remaining-capacity|./internal/plane|^TestTaskInterruption$/^remaining-capacity$|TestTaskInterruption,TestTaskInterruption/remaining-capacity",
	"TestTaskRecoveryBoundary/remaining-capacity|./internal/sidecar|^TestTaskInterruption$/^remaining-capacity$|TestTaskInterruption,TestTaskInterruption/remaining-capacity",
	"TestTaskRecoveryBoundary/recovery-remove|./internal/plane|^TestTaskInterruption$/^recovery-remove$|TestTaskInterruption,TestTaskInterruption/recovery-remove",
	"TestTaskRecoveryBoundary/recovery-remove|./internal/sidecar|^TestTaskInterruption$/^recovery-remove$|TestTaskInterruption,TestTaskInterruption/recovery-remove",
}

// wantTaskSuffix is the design's literal 41-name native suffix.
var wantTaskSuffix = []string{
	"TestTaskModel", "TestTaskModel/envelope", "TestTaskModel/states",
	"TestTaskDispatch", "TestTaskDispatch/selection", "TestTaskDispatch/gate-race", "TestTaskDispatch/reserved-slot",
	"TestTaskProtocol", "TestTaskProtocol/duplex", "TestTaskProtocol/bounds", "TestTaskProtocol/result-receipt", "TestTaskProtocol/result-ack-loss",
	"TestTaskExecution", "TestTaskExecution/compose", "TestTaskExecution/invoke", "TestTaskExecution/exit", "TestTaskExecution/process",
	"TestTaskExecution/platform", "TestTaskExecution/policy",
	"TestTaskLogs", "TestTaskLogs/retention", "TestTaskLogs/backpressure", "TestTaskLogs/final",
	"TestTaskPersistence", "TestTaskPersistence/durability", "TestTaskPersistence/restore",
	"TestTaskCommands", "TestTaskCommands/dispatch", "TestTaskCommands/ls", "TestTaskCommands/show", "TestTaskCommands/logs", "TestTaskCommands/trust",
	"TestTaskRoles", "TestTaskRoles/counts", "TestTaskRoles/mutation", "TestTaskRoles/remaining-capacity", "TestTaskRoles/recovery-remove",
	"TestTaskRecoveryBoundary", "TestTaskRecoveryBoundary/disconnect", "TestTaskRecoveryBoundary/remaining-capacity", "TestTaskRecoveryBoundary/recovery-remove",
}

// wantLaunchLedger is the design's per-test incremental task/probe-child
// launch budget per stress repetition of the sidecar package (the packages
// shard until design 05b's sidecar follow-up, the sidecar shard since):
// exactly five OS children since iteration 06a (one probe plus two
// sequential guardian/adapter pairs), all in the sidecar's process
// qualification.
var wantLaunchLedger = []string{
	"sidecar TestTaskExecutionContract/process|5",
	"sidecar TestTaskExecutionContract/compose,/exit|0",
	"adapter TestTaskAdapter/invocation,/extraction,/signals|0",
	"sidecar TestTaskPlatform/platforms|0",
	"plane and sidecar TestTaskOutput/retention,backpressure,final|0",
	"plane TestTaskAdmission,TestTaskStore; plane/sidecar TestTaskStream,TestTaskRoleIntegration,TestTaskInterruption; contract TestTaskContract; client TestTaskClient|0",
	"fakeadapter TestTaskMode; cli TestTaskCLI; devcheck TestTaskPolicy/policy|0",
}

// taskTestFiles are the new test files the launch guard covers.
var taskTestFiles = []string{
	"internal/contract/task_test.go",
	"internal/adapter/task_test.go",
	"internal/testkit/fakeadapter/task_test.go",
	"internal/plane/task_admission_test.go", "internal/plane/task_stream_test.go", "internal/plane/task_store_test.go",
	"internal/plane/task_output_test.go", "internal/plane/task_roles_test.go", "internal/plane/task_interruption_test.go",
	"internal/plane/task_helpers_test.go", "internal/plane/task_bench_test.go",
	"internal/sidecar/task_helpers_test.go", "internal/sidecar/task_stream_test.go", "internal/sidecar/task_execution_test.go",
	"internal/sidecar/task_output_test.go", "internal/sidecar/task_roles_test.go", "internal/sidecar/task_interruption_test.go",
	"internal/sidecar/task_process_unix_test.go", "internal/sidecar/task_process_other_test.go",
	"internal/client/tasks_test.go", "internal/cli/task_test.go", "internal/devcheck/task_policy_test.go",
	"tests/function/tasks_test.go",
}

// TestTaskPolicy is delegated from tests/function (TestTaskExecution/policy).
// It checks iteration 05's delegation table, its evidence rule and
// negative fixtures, the native suffix and sidecar tuple, the unchanged
// stress and bench plans, the launch ledger and its source guard, and the
// production import guards. Do not rename or skip its subtests.
func TestTaskPolicy(t *testing.T) {
	t.Run("policy", func(t *testing.T) {
		// The table, literally; selectors are single argv values.
		rows := TaskDelegations()
		if len(rows) != 41 || len(wantTaskDelegations) != 41 {
			t.Fatalf("%d rows", len(rows))
		}
		wrappers := map[string]bool{}
		for i, d := range rows {
			if got := d.Wrapper + "|" + d.Package + "|" + d.Selector + "|" + strings.Join(d.Required, ","); got != wantTaskDelegations[i] || strings.ContainsAny(d.Selector, " \t") {
				t.Fatalf("row %d = %s", i, got)
			}
			wrappers[d.Wrapper] = true
		}
		if len(wrappers) != 27 { // 32 mandatory subtests minus TestTaskCommands' five direct cases
			t.Fatalf("%d delegating function subtests", len(wrappers))
		}
		rows[0].Required[0] = "mutated"
		if TaskDelegations()[0].Required[0] != "TestTaskContract" {
			t.Fatal("the table is mutable through a copy")
		}
		// Evidence: every row needs its own package's RUN and PASS lines.
		for w := range wrappers {
			good := map[string]string{}
			for _, d := range TaskDelegationsFor(w) {
				good[d.Package] = passing(d.Required...)
			}
			if err := CheckTaskWrapperEvidence(w, good); err != nil {
				t.Fatalf("%s complete: %v", w, err)
			}
			for _, d := range TaskDelegationsFor(w) {
				for _, name := range d.Required {
					for _, cut := range []string{"=== RUN   " + name + "\n", "--- PASS: " + name + " ("} {
						bad := copyOutputs(good)
						bad[d.Package] = strings.Replace(bad[d.Package], cut, "", 1)
						if err := CheckTaskWrapperEvidence(w, bad); err == nil || !strings.Contains(err.Error(), unobserved) {
							t.Fatalf("%s without %q in %s: %v", w, cut, d.Package, err)
						}
					}
				}
				for name, out := range map[string]string{
					"skipped child": good[d.Package] + "    --- SKIP: " + d.Required[len(d.Required)-1] + "/x (0.00s)\n",
					"empty":         "testing: warning: no tests to run\nPASS\n",
					"blank":         "",
				} {
					bad := copyOutputs(good)
					bad[d.Package] = out
					if err := CheckTaskWrapperEvidence(w, bad); err == nil {
						t.Fatalf("%s with %s %s evidence passed", w, name, d.Package)
					}
				}
			}
			// Substituting the other package's output fails both ways.
			if rs := TaskDelegationsFor(w); len(rs) == 2 {
				swapped := map[string]string{rs[0].Package: good[rs[0].Package]}
				if err := CheckTaskWrapperEvidence(w, swapped); err == nil || !strings.Contains(err.Error(), "has no invocation of "+rs[1].Package) {
					t.Fatalf("%s with one package: %v", w, err)
				}
				extra := copyOutputs(good)
				extra["./internal/client"] = good[rs[0].Package]
				if err := CheckTaskWrapperEvidence(w, extra); err == nil {
					t.Fatalf("%s accepted an extra package", w)
				}
			}
		}
		// The process qualification's child is mandatory.
		proc := TaskDelegationsFor("TestTaskExecution/process")
		if len(proc) != 1 || CheckDelegationEvidence(proc[0], passing("TestTaskExecutionContract")) == nil ||
			CheckDelegationEvidence(proc[0], passing("TestTaskExecutionContract", "TestTaskExecutionContract/compose")) == nil {
			t.Fatal("process evidence must include TestTaskExecutionContract/process")
		}
		if err := CheckTaskWrapperEvidence("TestTaskNothing", nil); err == nil {
			t.Fatal("an unknown wrapper passed")
		}
		// Native: the 87 earlier names first and unchanged, then the literal
		// 41-name task suffix (iteration 06a's 13 control names follow it);
		// plus the separate sidecar package tuple.
		req := NativeRequiredTests()
		if len(req) != 145+len(mcpNames())+len(qualNames())+len(realNames())+len(wsNames())+len(trNames())+len(latNames())+len(wsTaskNames())+len(wsDoorNames())+len(wave2Names())+len(nbwNames())+len(dceNames())+len(b1Names())+len(b15Names())+len(b2Names())+len(b3Names()) || strings.Join(req[87:128], ",") != strings.Join(wantTaskSuffix, ",") || strings.Join(req[58:87], ",") != strings.Join(roleNames(), ",") ||
			strings.Join(taskNames(), ",") != strings.Join(wantTaskSuffix, ",") {
			t.Fatalf("native required = %v", req[87:128])
		}
		if NativeTaskProcessPackage != "github.com/wedevwork/callsheet/internal/sidecar" ||
			strings.Join(NativeTaskProcessTests()[:2], ",") != "TestTaskExecutionContract,TestTaskExecutionContract/process" ||
			strings.Join(NativeTaskProcessTests()[2:], ",") != strings.Join(realLocal, ",") {
			t.Fatal("the sidecar native tuple changed")
		}
		q := qualification()
		if err := check(stream(q...)); err != nil {
			t.Fatalf("complete evidence: %v", err)
		}
		for _, name := range wantTaskSuffix {
			mustFail(t, "missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event", unobserved)
			mustFail(t, "skipped "+name, stream(replacing(q, "pass", name, ev("skip", NativePackage, name))...), "skipped: "+unobserved)
		}
		for _, name := range NativeTaskProcessTests() {
			mustFail(t, "sidecar "+name, stream(without(without(q, "run", name), "pass", name)...), name+" in "+NativeTaskProcessPackage+" has no run event")
		}
		// The process tuple is satisfied only by the sidecar's package:
		// the same names in another package do not count.
		var moved []evt
		for _, e := range q {
			c := evt{}
			for k, v := range e {
				c[k] = v
			}
			if c["Package"] == NativeTaskProcessPackage {
				c["Package"] = "github.com/wedevwork/callsheet/internal/plane"
			}
			moved = append(moved, c)
		}
		mustFail(t, "tuple in another package", stream(moved...), "in "+NativeTaskProcessPackage+" has no run event")
		// Unchanged stress selection (iteration 05 adds no shard, selector
		// or package; iteration 05b moved ./internal/plane, and its sidecar
		// follow-up ./internal/sidecar, to their own shards without changing
		// the selection) and bench plan, the launch ledger's budget and the
		// guards. The task children of TestTaskExecutionContract/process
		// stay in the sidecar package, hence since the follow-up in the
		// sidecar shards (single-CPU invocations: CPU1 alone in sidecar-cpu1
		// since design 06a-perf, CPU2 and CPU4 concurrent in sidecar; the
		// same 60 repetitions), and none remain in the packages shard.
		for _, goos := range []string{"linux", "darwin"} {
			shards, err := StressShards(goos)
			if err != nil || len(shards) != 8 || strings.Join(shards[0].Steps[0].Argv, " ") != wantRoleStressPackages ||
				strings.Contains(strings.Join(shards[0].Steps[0].Argv, " "), "./internal/sidecar") || shards[0].CPUGroups != nil || shards[1].Name != "packages-cpu" ||
				strings.Join(argvOf(slices.Concat(shards[1].CPUGroups...)), "|") != strings.Join(slices.Concat(groupContract, groupMcpqual, groupWorkspace), "|") ||
				joinedArgv(shards, 2, 3) != wantStressPlane1+"|"+wantStressPlane2+"|"+wantStressPlane4 ||
				joinedArgv(shards, 2) != wantStressPlane1 || joinedArgv(shards, 3) != wantStressPlane2+"|"+wantStressPlane4 ||
				shards[4].Name != "sidecar-cpu1" || shards[5].Name != "sidecar" ||
				joinedArgv(shards, 4, 5) != wantStressSidecar1+"|"+wantStressSidecar2+"|"+wantStressSidecar4 ||
				joinedArgv(shards, 4) != wantStressSidecar1 || joinedArgv(shards, 5) != wantStressSidecar2+"|"+wantStressSidecar4 ||
				joinedArgv(shards, 6) != wantStressPG1+"|"+wantStressPG2+"|"+wantStressPG4 ||
				joinedArgv(shards, 7) != wantStressFunction+"|"+wantStressPlaneFunction+"|"+wantNodeStressFunction {
				t.Fatalf("%s stress plan changed: %+v %v", goos, shards, err)
			}
		}
		if got := strings.Join(argvOf(BenchSteps()), "|"); got != wantRoleBenchPlan {
			t.Fatalf("bench plan = %s", got)
		}
		if len(wantLaunchLedger) != 7 || !strings.HasSuffix(wantLaunchLedger[0], "|5") {
			t.Fatal("the launch ledger changed")
		}
		root := repoRoot(t)
		launch := regexp.MustCompile(`exec\.Command|os\.StartProcess|syscall\.ForkExec|"os/exec"`)
		for _, f := range taskTestFiles {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
			if err != nil {
				t.Fatalf("task test file %s: %v", f, err)
			}
			src := string(b)
			if f == "internal/devcheck/task_policy_test.go" {
				continue // this file names the forbidden calls
			}
			if m := launch.FindString(src); m != "" {
				t.Fatalf("%s launches processes directly (%s): use the counted factory", f, m)
			}
			if strings.Contains(src, "newExecProc(") && f != "internal/sidecar/task_helpers_test.go" {
				t.Fatalf("%s reaches the real process factory outside the counted ledger", f)
			}
			if strings.Contains(src, "BuildBinaryAt(") && f != "internal/sidecar/task_execution_test.go" && f != "tests/function/tasks_test.go" {
				t.Fatalf("%s builds an executable", f)
			}
			if strings.Contains(src, "ledger.real = true") && f != "internal/sidecar/task_execution_test.go" {
				t.Fatalf("%s enables real task children", f)
			}
		}
		exe, _ := os.ReadFile(filepath.Join(root, "internal", "sidecar", "task_execution_test.go"))
		if strings.Count(string(exe), "ledger.real = true") != 1 || !strings.Contains(string(exe), "want exactly 1 and 4") {
			t.Fatal("the process qualification lost its single real-child ledger assertion")
		}
		helpers, _ := os.ReadFile(filepath.Join(root, "internal", "sidecar", "task_helpers_test.go"))
		if !strings.Contains(string(helpers), "an injected task test started %d OS children, want 0") {
			t.Fatal("the injected tests lost their zero-launch assertion")
		}
		// Production never depends on testkit or devcheck; only the sidecar
		// imports the qualified process-group mechanics; the platform
		// guard's exception list is unchanged.
		if err := CheckPlatformSources(root); err != nil {
			t.Fatalf("platform guard: %v", err)
		}
		if len(platformGuardPolicy.wrappers) != 7 || len(platformGuardPolicy.exemptions) != 6 {
			t.Fatal("the platform guard's exception list grew")
		}
		const mod = "github.com/wedevwork/callsheet/"
		for _, pkg := range []string{"plane", "sidecar", "adapter", "client", "cli", "contract"} {
			for file, imps := range importsOf(t, filepath.Join(root, "internal", pkg)) {
				for _, imp := range imps {
					if strings.HasPrefix(imp, mod+"internal/testkit") || strings.HasPrefix(imp, mod+"internal/devcheck") {
						t.Fatalf("internal/%s/%s imports %s", pkg, file, imp)
					}
					if strings.HasPrefix(imp, mod+"internal/spikes") && (pkg != "sidecar" || imp != mod+"internal/spikes/processgroup") {
						t.Fatalf("internal/%s/%s imports %s", pkg, file, imp)
					}
				}
			}
		}
	})
}

func copyOutputs(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
