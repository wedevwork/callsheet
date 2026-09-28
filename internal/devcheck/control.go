package devcheck

import (
	"fmt"
	"sort"
	"strings"
)

// Iteration 06a (resilient execution) and 06b (task controls): the single
// delegation table of the twelve control function tests (06a's eight and
// 06b's four), one top-level function test per FP. Each
// row is the function wrapper, one package whose compiled test binary it
// runs, the exact single-argv selector and the name that must show RUN
// and PASS evidence in that package's own output. A multi-package wrapper
// is a conjunction: a same-named test in one package never satisfies
// another package's row. Wrappers never run devcheck or the function
// suite recursively.
var controlDelegations = []RoleDelegation{
	{"TestControlDurability", "./internal/plane", "^TestControlCommit$", []string{"TestControlCommit"}},
	{"TestControlReconnect", "./internal/contract", "^TestControlProtocol$", []string{"TestControlProtocol"}},
	{"TestControlReconnect", "./internal/plane", "^TestControlProtocol$", []string{"TestControlProtocol"}},
	{"TestControlReconnect", "./internal/sidecar", "^TestControlProtocol$", []string{"TestControlProtocol"}},
	{"TestControlNodeLoss", "./internal/plane", "^TestControlLease$", []string{"TestControlLease"}},
	{"TestControlNodeLoss", "./internal/sidecar", "^TestControlLease$", []string{"TestControlLease"}},
	{"TestControlLaunchSafety", "./internal/sidecar", "^TestControlLaunch$", []string{"TestControlLaunch"}},
	{"TestControlWorkerRecovery", "./internal/sidecar", "^TestControlRestart$", []string{"TestControlRestart"}},
	{"TestControlPlaneRecovery", "./internal/plane", "^TestControlPlaneRestart$", []string{"TestControlPlaneRestart"}},
	{"TestControlPlaneRecovery", "./internal/sidecar", "^TestControlPlaneRestart$", []string{"TestControlPlaneRestart"}},
	{"TestControlLateResult", "./internal/plane", "^TestControlLate$", []string{"TestControlLate"}},
	{"TestControlLateResult", "./internal/sidecar", "^TestControlLate$", []string{"TestControlLate"}},
	{"TestControlLegacy", "./internal/contract", "^TestControlMigration$", []string{"TestControlMigration"}},
	{"TestControlLegacy", "./internal/plane", "^TestControlMigration$", []string{"TestControlMigration"}},
	// Iteration 06b (task controls): four function parents, one per FP.
	// Each row requires its package contract's parent and every mandatory
	// subcase (design 06b, Mandatory test subcases), so a vacuous parent
	// never qualifies.
	{"TestControlCancellation", "./internal/plane", "^TestControlCancel$", []string{"TestControlCancel", "TestControlCancel/unsent", "TestControlCancel/loaded-pending", "TestControlCancel/durable-order", "TestControlCancel/storage-retry", "TestControlCancel/offline", "TestControlCancel/duplicate"}},
	{"TestControlCancellation", "./internal/sidecar", "^TestControlCancel$", []string{"TestControlCancel", "TestControlCancel/preparing", "TestControlCancel/guardian-control", "TestControlCancel/partial-output", "TestControlCancel/cleanup-unconfirmed", "TestControlCancel/duplicate"}},
	{"TestControlCancellation", "./internal/client", "^TestControlCancel$", []string{"TestControlCancel", "TestControlCancel/accepted", "TestControlCancel/terminal", "TestControlCancel/errors"}},
	{"TestControlCancellation", "./internal/cli", "^TestControlCancel$", []string{"TestControlCancel", "TestControlCancel/accepted", "TestControlCancel/terminal", "TestControlCancel/errors"}},
	{"TestControlExecutionTimeout", "./internal/sidecar", "^TestControlTimeout$", []string{"TestControlTimeout", "TestControlTimeout/default-override-zero", "TestControlTimeout/deadline-tie", "TestControlTimeout/slow-start", "TestControlTimeout/plane-outage", "TestControlTimeout/restart", "TestControlTimeout/status-failure"}},
	{"TestControlExecutionTimeout", "./internal/contract", "^TestControlTimeout$", []string{"TestControlTimeout", "TestControlTimeout/policy", "TestControlTimeout/outcome", "TestControlTimeout/migration"}},
	{"TestControlBoundedWait", "./internal/plane", "^TestControlWait$", []string{"TestControlWait", "TestControlWait/register-race", "TestControlWait/any-of", "TestControlWait/deadline", "TestControlWait/capacity", "TestControlWait/shutdown", "TestControlWait/dispatch"}},
	{"TestControlBoundedWait", "./internal/client", "^TestControlWait$", []string{"TestControlWait", "TestControlWait/restart-budget", "TestControlWait/no-dispatch-retry", "TestControlWait/deadline", "TestControlWait/correlation"}},
	{"TestControlBoundedWait", "./internal/cli", "^TestControlWait$", []string{"TestControlWait", "TestControlWait/text", "TestControlWait/json", "TestControlWait/exit"}},
	{"TestControlBoundedWait", "./internal/contract", "^TestControlWait$", []string{"TestControlWait", "TestControlWait/bounds", "TestControlWait/validation"}},
	{"TestControlForceRemove", "./internal/plane", "^TestControlRemove$", []string{"TestControlRemove", "TestControlRemove/fence", "TestControlRemove/drain", "TestControlRemove/restart", "TestControlRemove/storage-retry", "TestControlRemove/instance-reuse"}},
	{"TestControlForceRemove", "./internal/sidecar", "^TestControlRemove$", []string{"TestControlRemove", "TestControlRemove/removed-instance-cleanup"}},
	{"TestControlForceRemove", "./internal/cli", "^TestControlRemove$", []string{"TestControlRemove", "TestControlRemove/pending", "TestControlRemove/completed", "TestControlRemove/retry"}},
}

// ControlDelegations returns a fresh copy of the control delegation table.
func ControlDelegations() []RoleDelegation {
	out := make([]RoleDelegation, len(controlDelegations))
	for i, d := range controlDelegations {
		d.Required = append([]string(nil), d.Required...)
		out[i] = d
	}
	return out
}

// ControlDelegationsFor returns the rows of one control wrapper, in table
// order.
func ControlDelegationsFor(wrapper string) []RoleDelegation {
	var out []RoleDelegation
	for _, d := range ControlDelegations() {
		if d.Wrapper == wrapper {
			out = append(out, d)
		}
	}
	return out
}

// CheckControlWrapperEvidence requires every row of a control wrapper to
// pass CheckDelegationEvidence against its own package's invocation
// output (outputs keyed by package), and no invocation outside the
// wrapper's rows: a missing, failed, skipped or empty package invocation
// is not qualification.
func CheckControlWrapperEvidence(wrapper string, outputs map[string]string) error {
	rows := ControlDelegationsFor(wrapper)
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
