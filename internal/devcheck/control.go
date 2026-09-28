package devcheck

import (
	"fmt"
	"sort"
	"strings"
)

// Iteration 06a (resilient execution): the single delegation table of the
// eight control function tests, one top-level function test per FP. Each
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
