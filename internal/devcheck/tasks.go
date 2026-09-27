package devcheck

import (
	"fmt"
	"sort"
	"strings"
)

// Iteration 05 (tasks): the design's four-column delegation contract.
// Each row is one function subtest, the package whose compiled test
// binary it runs, the exact single-argv selector and every name that must
// show RUN and PASS evidence in that package's own output. A two-package
// wrapper is a conjunction: a same-named test in one package never
// satisfies the other's row.
var taskDelegations = []RoleDelegation{
	{"TestTaskModel/envelope", "./internal/contract", "^TestTaskContract$/^envelope$", []string{"TestTaskContract", "TestTaskContract/envelope"}},
	{"TestTaskModel/states", "./internal/plane", "^TestTaskStore$/^states$", []string{"TestTaskStore", "TestTaskStore/states"}},
	{"TestTaskDispatch/selection", "./internal/plane", "^TestTaskAdmission$/^selection$", []string{"TestTaskAdmission", "TestTaskAdmission/selection"}},
	{"TestTaskDispatch/gate-race", "./internal/plane", "^TestTaskAdmission$/^gate-race$", []string{"TestTaskAdmission", "TestTaskAdmission/gate-race"}},
	{"TestTaskDispatch/reserved-slot", "./internal/plane", "^TestTaskAdmission$/^reserved-slot$", []string{"TestTaskAdmission", "TestTaskAdmission/reserved-slot"}},
	{"TestTaskProtocol/duplex", "./internal/plane", "^TestTaskStream$/^duplex$", []string{"TestTaskStream", "TestTaskStream/duplex"}},
	{"TestTaskProtocol/duplex", "./internal/sidecar", "^TestTaskStream$/^duplex$", []string{"TestTaskStream", "TestTaskStream/duplex"}},
	{"TestTaskProtocol/bounds", "./internal/plane", "^TestTaskStream$/^bounds$", []string{"TestTaskStream", "TestTaskStream/bounds"}},
	{"TestTaskProtocol/bounds", "./internal/sidecar", "^TestTaskStream$/^bounds$", []string{"TestTaskStream", "TestTaskStream/bounds"}},
	{"TestTaskProtocol/result-receipt", "./internal/plane", "^TestTaskStream$/^result-receipt$", []string{"TestTaskStream", "TestTaskStream/result-receipt"}},
	{"TestTaskProtocol/result-receipt", "./internal/sidecar", "^TestTaskStream$/^result-receipt$", []string{"TestTaskStream", "TestTaskStream/result-receipt"}},
	{"TestTaskProtocol/result-ack-loss", "./internal/plane", "^TestTaskStream$/^result-ack-loss$", []string{"TestTaskStream", "TestTaskStream/result-ack-loss"}},
	{"TestTaskProtocol/result-ack-loss", "./internal/sidecar", "^TestTaskStream$/^result-ack-loss$", []string{"TestTaskStream", "TestTaskStream/result-ack-loss"}},
	{"TestTaskExecution/compose", "./internal/sidecar", "^TestTaskExecutionContract$/^compose$", []string{"TestTaskExecutionContract", "TestTaskExecutionContract/compose"}},
	{"TestTaskExecution/invoke", "./internal/adapter", "^TestTaskAdapter$/^(invocation|extraction|signals)$", []string{"TestTaskAdapter", "TestTaskAdapter/invocation", "TestTaskAdapter/extraction", "TestTaskAdapter/signals"}},
	{"TestTaskExecution/exit", "./internal/sidecar", "^TestTaskExecutionContract$/^exit$", []string{"TestTaskExecutionContract", "TestTaskExecutionContract/exit"}},
	{"TestTaskExecution/process", "./internal/sidecar", "^TestTaskExecutionContract$/^process$", []string{"TestTaskExecutionContract", "TestTaskExecutionContract/process"}},
	{"TestTaskExecution/platform", "./internal/sidecar", "^TestTaskPlatform$/^platforms$", []string{"TestTaskPlatform", "TestTaskPlatform/platforms"}},
	{"TestTaskExecution/policy", "./internal/devcheck", "^TestTaskPolicy$/^policy$", []string{"TestTaskPolicy", "TestTaskPolicy/policy"}},
	{"TestTaskLogs/retention", "./internal/plane", "^TestTaskOutput$/^retention$", []string{"TestTaskOutput", "TestTaskOutput/retention"}},
	{"TestTaskLogs/retention", "./internal/sidecar", "^TestTaskOutput$/^retention$", []string{"TestTaskOutput", "TestTaskOutput/retention"}},
	{"TestTaskLogs/backpressure", "./internal/plane", "^TestTaskOutput$/^backpressure$", []string{"TestTaskOutput", "TestTaskOutput/backpressure"}},
	{"TestTaskLogs/backpressure", "./internal/sidecar", "^TestTaskOutput$/^backpressure$", []string{"TestTaskOutput", "TestTaskOutput/backpressure"}},
	{"TestTaskLogs/final", "./internal/plane", "^TestTaskOutput$/^final$", []string{"TestTaskOutput", "TestTaskOutput/final"}},
	{"TestTaskLogs/final", "./internal/sidecar", "^TestTaskOutput$/^final$", []string{"TestTaskOutput", "TestTaskOutput/final"}},
	{"TestTaskPersistence/durability", "./internal/plane", "^TestTaskStore$/^durability$", []string{"TestTaskStore", "TestTaskStore/durability"}},
	{"TestTaskPersistence/restore", "./internal/plane", "^TestTaskStore$/^restore$", []string{"TestTaskStore", "TestTaskStore/restore"}},
	{"TestTaskRoles/counts", "./internal/plane", "^TestTaskRoleIntegration$/^counts$", []string{"TestTaskRoleIntegration", "TestTaskRoleIntegration/counts"}},
	{"TestTaskRoles/counts", "./internal/sidecar", "^TestTaskRoleIntegration$/^counts$", []string{"TestTaskRoleIntegration", "TestTaskRoleIntegration/counts"}},
	{"TestTaskRoles/mutation", "./internal/plane", "^TestTaskRoleIntegration$/^mutation$", []string{"TestTaskRoleIntegration", "TestTaskRoleIntegration/mutation"}},
	{"TestTaskRoles/mutation", "./internal/sidecar", "^TestTaskRoleIntegration$/^mutation$", []string{"TestTaskRoleIntegration", "TestTaskRoleIntegration/mutation"}},
	{"TestTaskRoles/remaining-capacity", "./internal/plane", "^TestTaskRoleIntegration$/^remaining-capacity$", []string{"TestTaskRoleIntegration", "TestTaskRoleIntegration/remaining-capacity"}},
	{"TestTaskRoles/remaining-capacity", "./internal/sidecar", "^TestTaskRoleIntegration$/^remaining-capacity$", []string{"TestTaskRoleIntegration", "TestTaskRoleIntegration/remaining-capacity"}},
	{"TestTaskRoles/recovery-remove", "./internal/plane", "^TestTaskRoleIntegration$/^recovery-remove$", []string{"TestTaskRoleIntegration", "TestTaskRoleIntegration/recovery-remove"}},
	{"TestTaskRoles/recovery-remove", "./internal/sidecar", "^TestTaskRoleIntegration$/^recovery-remove$", []string{"TestTaskRoleIntegration", "TestTaskRoleIntegration/recovery-remove"}},
	{"TestTaskRecoveryBoundary/disconnect", "./internal/plane", "^TestTaskInterruption$/^disconnect$", []string{"TestTaskInterruption", "TestTaskInterruption/disconnect"}},
	{"TestTaskRecoveryBoundary/disconnect", "./internal/sidecar", "^TestTaskInterruption$/^disconnect$", []string{"TestTaskInterruption", "TestTaskInterruption/disconnect"}},
	{"TestTaskRecoveryBoundary/remaining-capacity", "./internal/plane", "^TestTaskInterruption$/^remaining-capacity$", []string{"TestTaskInterruption", "TestTaskInterruption/remaining-capacity"}},
	{"TestTaskRecoveryBoundary/remaining-capacity", "./internal/sidecar", "^TestTaskInterruption$/^remaining-capacity$", []string{"TestTaskInterruption", "TestTaskInterruption/remaining-capacity"}},
	{"TestTaskRecoveryBoundary/recovery-remove", "./internal/plane", "^TestTaskInterruption$/^recovery-remove$", []string{"TestTaskInterruption", "TestTaskInterruption/recovery-remove"}},
	{"TestTaskRecoveryBoundary/recovery-remove", "./internal/sidecar", "^TestTaskInterruption$/^recovery-remove$", []string{"TestTaskInterruption", "TestTaskInterruption/recovery-remove"}},
}

// TaskDelegations returns a fresh copy of the task delegation table.
func TaskDelegations() []RoleDelegation {
	out := make([]RoleDelegation, len(taskDelegations))
	for i, d := range taskDelegations {
		d.Required = append([]string(nil), d.Required...)
		out[i] = d
	}
	return out
}

// TaskDelegationsFor returns the rows of one wrapper, in table order.
func TaskDelegationsFor(wrapper string) []RoleDelegation {
	var out []RoleDelegation
	for _, d := range TaskDelegations() {
		if d.Wrapper == wrapper {
			out = append(out, d)
		}
	}
	return out
}

// CheckTaskWrapperEvidence requires every row of a task wrapper to pass
// CheckDelegationEvidence against its own package's invocation output
// (outputs keyed by package), and no invocation outside the wrapper's
// rows.
func CheckTaskWrapperEvidence(wrapper string, outputs map[string]string) error {
	rows := TaskDelegationsFor(wrapper)
	if len(rows) == 0 {
		return fmt.Errorf("devcheck: %s delegates nothing", wrapper)
	}
	var errs []string
	for _, d := range rows {
		out, ok := outputs[d.Package]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s has no invocation of %s: %s", wrapper, d.Package, unobserved))
			continue
		}
		if err := CheckDelegationEvidence(d, out); err != nil {
			errs = append(errs, err.Error())
		}
	}
	var extra []string
	for pkg := range outputs {
		found := false
		for _, d := range rows {
			found = found || d.Package == pkg
		}
		if !found {
			extra = append(extra, pkg)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		errs = append(errs, fmt.Sprintf("%s has invocations outside its table rows: %s", wrapper, strings.Join(extra, ", ")))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return nil
}
