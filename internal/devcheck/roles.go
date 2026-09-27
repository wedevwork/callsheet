package devcheck

import (
	"fmt"
	"sort"
	"strings"
)

// RoleDelegation is one row of design 04's delegation table (Function
// tests): the function subtest that delegates, the package whose compiled
// test binary it runs, the exact single-argv selector (-run for go test,
// -test.run for the binary) and every test name that must show run and
// pass evidence in that package's own output.
type RoleDelegation struct {
	Wrapper  string
	Package  string
	Selector string
	Required []string
}

// roleDelegations is the table, in the design's order.
var roleDelegations = []RoleDelegation{
	{"TestRoleProtocol/duplex", "./internal/plane", "^TestRoleStreamContract$/^duplex$", []string{"TestRoleStreamContract", "TestRoleStreamContract/duplex"}},
	{"TestRoleProtocol/duplex", "./internal/sidecar", "^TestRoleStreamContract$/^duplex$", []string{"TestRoleStreamContract", "TestRoleStreamContract/duplex"}},
	{"TestRoleProtocol/bounds", "./internal/plane", "^TestRoleStreamContract$/^bounds$", []string{"TestRoleStreamContract", "TestRoleStreamContract/bounds"}},
	{"TestRoleProtocol/bounds", "./internal/sidecar", "^TestRoleStreamContract$/^bounds$", []string{"TestRoleStreamContract", "TestRoleStreamContract/bounds"}},
	{"TestRoleReadiness/changes", "./internal/sidecar", "^TestRoleReadinessContract$/^changes$", []string{"TestRoleReadinessContract", "TestRoleReadinessContract/changes"}},
	{"TestRoleReadiness/reconnect", "./internal/plane", "^TestRoleDistributionContract$/^reconnect$", []string{"TestRoleDistributionContract", "TestRoleDistributionContract/reconnect"}},
	{"TestRolePersistence/failures", "./internal/plane", "^TestRoleRegistryContract$/^failures$", []string{"TestRoleRegistryContract", "TestRoleRegistryContract/failures"}},
	{"TestRoleMutation/races", "./internal/plane", "^TestRoleMutationContract$/^races$", []string{"TestRoleMutationContract", "TestRoleMutationContract/races", "TestRoleMutationContract/races/snapshot-in-flight"}},
	{"TestRolePlatform/manuals", "./internal/sidecar", "^TestRolePlatformContract$/^manuals$", []string{"TestRolePlatformContract", "TestRolePlatformContract/manuals"}},
	{"TestRolePlatform/executable", "./internal/adapter", "^TestRolePlatformContract$/^executable$", []string{"TestRolePlatformContract", "TestRolePlatformContract/executable"}},
	{"TestRolePlatform/policy", "./internal/devcheck", "^TestRolePolicy$/^policy$", []string{"TestRolePolicy", "TestRolePolicy/policy"}},
}

// RoleDelegations returns a fresh copy of the delegation table.
func RoleDelegations() []RoleDelegation {
	out := make([]RoleDelegation, len(roleDelegations))
	for i, d := range roleDelegations {
		d.Required = append([]string(nil), d.Required...)
		out[i] = d
	}
	return out
}

// RoleDelegationsFor returns the rows of one wrapper, in table order.
func RoleDelegationsFor(wrapper string) []RoleDelegation {
	var out []RoleDelegation
	for _, d := range RoleDelegations() {
		if d.Wrapper == wrapper {
			out = append(out, d)
		}
	}
	return out
}

// delegationFailures are verbose-output markers that fail a delegation
// even when the binary exited zero: a failed or skipped test, or an empty
// selection.
var delegationFailures = []string{"--- FAIL", "--- SKIP", "no tests to run"}

// CheckDelegationEvidence validates one package invocation's verbose test
// output (-test.v) for row d: no failure, skip or empty-selection marker,
// and both a RUN and a PASS line for every required name.
func CheckDelegationEvidence(d RoleDelegation, output string) error {
	for _, bad := range delegationFailures {
		if strings.Contains(output, bad) {
			return fmt.Errorf("devcheck: %s in %s (%s): %q in its output: %s", d.Wrapper, d.Package, d.Selector, bad, unobserved)
		}
	}
	var missing []string
	for _, name := range d.Required {
		if !strings.Contains(output, "=== RUN   "+name+"\n") {
			missing = append(missing, name+" has no run evidence")
		}
		if !strings.Contains(output, "--- PASS: "+name+" (") {
			missing = append(missing, name+" has no pass evidence")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("devcheck: %s in %s (%s): %s: %s", d.Wrapper, d.Package, d.Selector, strings.Join(missing, "; "), unobserved)
	}
	return nil
}

// CheckRoleWrapperEvidence requires every row of wrapper to pass
// CheckDelegationEvidence against the output of its own package's
// invocation (outputs keyed by package): identical test names in one
// package never satisfy another package's row.
func CheckRoleWrapperEvidence(wrapper string, outputs map[string]string) error {
	rows := RoleDelegationsFor(wrapper)
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
	extra := []string{}
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
