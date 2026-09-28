package function

import (
	"os"
	"testing"

	"github.com/wedevwork/callsheet/internal/devcheck"
)

// Iteration 06a and 06b function tests: exactly one top-level TestControl*
// per FP, in FP order. Each delegates to the package contract(s) named in
// devcheck's control delegation table, through the precompiled package
// test binaries, and requires every listed package's own RUN and PASS
// evidence (no failure, skip or empty selection). TestControlNativeGroups
// (control_native_test.go) is the direct real-binary qualification.

// controlDelegate runs every delegation row of wrapper in its package's
// compiled test binary and requires the wrapper's complete table.
func controlDelegate(t *testing.T, wrapper string) {
	t.Helper()
	env := append(os.Environ(), nodeCLIEnv+"="+nodeBinary(t))
	outputs := map[string]string{}
	rows := devcheck.ControlDelegationsFor(wrapper)
	if len(rows) == 0 {
		t.Fatalf("%s has no delegation rows", wrapper)
	}
	for _, d := range rows {
		out := contractRun(t, contractBinary(t, d.Package), d.Package, d.Selector, env, d.Required...)
		if err := devcheck.CheckDelegationEvidence(d, out); err != nil {
			t.Fatal(err)
		}
		outputs[d.Package] = out
	}
	if err := devcheck.CheckControlWrapperEvidence(wrapper, outputs); err != nil {
		t.Fatal(err)
	}
}

// FP-1: durable lifecycle (the plane's commit matrix).
func TestControlDurability(t *testing.T) { controlDelegate(t, "TestControlDurability") }

// FP-2: the reconnection protocol (contract, plane and sidecar).
func TestControlReconnect(t *testing.T) { controlDelegate(t, "TestControlReconnect") }

// FP-3: lease loss (plane and sidecar).
func TestControlNodeLoss(t *testing.T) { controlDelegate(t, "TestControlNodeLoss") }

// FP-4: recoverable launch (the sidecar's barriers and guardian).
func TestControlLaunchSafety(t *testing.T) { controlDelegate(t, "TestControlLaunchSafety") }

// FP-5: sidecar restart recovery.
func TestControlWorkerRecovery(t *testing.T) { controlDelegate(t, "TestControlWorkerRecovery") }

// FP-6: plane restart (plane and sidecar).
func TestControlPlaneRecovery(t *testing.T) { controlDelegate(t, "TestControlPlaneRecovery") }

// FP-7: late results (plane and sidecar).
func TestControlLateResult(t *testing.T) { controlDelegate(t, "TestControlLateResult") }

// FP-8: legacy records (contract and plane).
func TestControlLegacy(t *testing.T) { controlDelegate(t, "TestControlLegacy") }

// Iteration 06b (task controls), each wrapper the conjunction of its
// packages' contracts with every mandatory subcase.

// 06b FP-1: cancellation (plane, sidecar, client and cli).
func TestControlCancellation(t *testing.T) { controlDelegate(t, "TestControlCancellation") }

// 06b FP-2: the guardian-enforced execution timeout (sidecar and contract).
func TestControlExecutionTimeout(t *testing.T) { controlDelegate(t, "TestControlExecutionTimeout") }

// 06b FP-3: bounded wait (plane, client, cli and contract).
func TestControlBoundedWait(t *testing.T) { controlDelegate(t, "TestControlBoundedWait") }

// 06b FP-4: forced role removal (plane, sidecar and cli).
func TestControlForceRemove(t *testing.T) { controlDelegate(t, "TestControlForceRemove") }
