package devcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// evt is one go test -json event. Synthetic events carry a "Synthetic"
// field (an unknown field the parser must accept) so they can never be
// mistaken for recorded evidence.
type evt map[string]any

func ev(action, pkg, test string) evt {
	e := evt{"Synthetic": "UT-3 synthetic event"}
	if action != "" {
		e["Action"] = action
	}
	if pkg != "" {
		e["Package"] = pkg
	}
	if test != "" {
		e["Test"] = test
	}
	return e
}

func (e evt) with(k string, v any) evt { e[k] = v; return e }

func stream(evs ...evt) string {
	var b strings.Builder
	for _, e := range evs {
		j, err := json.Marshal(e)
		if err != nil {
			panic(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

const fp6 = "TestFP6ProcessGroups"

var scenarios = []string{"cooperative", "resistant", "leader-exits-first"}

// planeRequired are the iteration 02 plane trust tests and mandatory
// compound-FP subtests (design 02, CI plan), with the iteration 02b
// process/contracts boundaries, in FP order.
var planeRequired = []struct {
	test string
	subs []string
}{
	{"TestPlaneCommands", nil},
	{"TestPlaneState", []string{"paths", "persistence", "locking", "validation", "contracts"}},
	{"TestPlaneBind", nil},
	{"TestPlaneInit", []string{"issuance", "fingerprint", "restart-invariance"}},
	{"TestPlaneTLS", []string{"https-only", "prelisten-validation", "bounded-shutdown", "contracts"}},
	{"TestPlaneReissue", []string{"process", "contracts"}},
	{"TestPlaneStatus", []string{"inspection", "expiry-warnings"}},
	{"TestPlanePlatform", nil},
}

// nodeRequired are the iteration 03 node function tests and their
// mandatory subtests (design 03, Function tests), in FP order.
var nodeRequired = []struct {
	test string
	subs []string
}{
	{"TestNodeTrust", []string{"ca", "pin", "rejections"}},
	{"TestNodeEnrollment", []string{"identity", "recovery", "locking"}},
	{"TestNodeProtocol", []string{"hello", "limits"}},
	{"TestNodeReconnect", []string{"restart", "disconnect", "shutdown"}},
	{"TestNodeLease", []string{"expiry", "return"}},
	{"TestNodeRegistry", []string{"restore", "validation"}},
	{"TestNodeDiscovery", []string{"text", "json", "errors"}},
	{"TestNodePlatform", []string{"paths", "native-state", "policy", "sticky-write"}},
}

// roleRequired are the iteration 04 role function tests and their
// mandatory subtests (design 04, Function tests), in FP order.
var roleRequired = []struct {
	test string
	subs []string
}{
	{"TestRoleConfiguration", []string{"fields", "order"}},
	{"TestRoleAdapter", []string{"disabled", "probe"}},
	{"TestRoleValidation", []string{"remote", "rejections"}},
	{"TestRoleProtocol", []string{"duplex", "bounds"}},
	{"TestRoleReadiness", []string{"changes", "reconnect"}},
	{"TestRolePersistence", []string{"restore", "failures"}},
	{"TestRoleCommands", []string{"text", "json", "trust"}},
	{"TestRoleMutation", []string{"races", "remove"}},
	{"TestRolePlatform", []string{"manuals", "executable", "policy"}},
}

// taskRequired are the iteration 05 task function tests and their
// mandatory subtests (design 05, Function tests), in FP order.
var taskRequired = []struct {
	test string
	subs []string
}{
	{"TestTaskModel", []string{"envelope", "states"}},
	{"TestTaskDispatch", []string{"selection", "gate-race", "reserved-slot"}},
	{"TestTaskProtocol", []string{"duplex", "bounds", "result-receipt", "result-ack-loss"}},
	{"TestTaskExecution", []string{"compose", "invoke", "exit", "process", "platform", "policy"}},
	{"TestTaskLogs", []string{"retention", "backpressure", "final"}},
	{"TestTaskPersistence", []string{"durability", "restore"}},
	{"TestTaskCommands", []string{"dispatch", "ls", "show", "logs", "trust"}},
	{"TestTaskRoles", []string{"counts", "mutation", "remaining-capacity", "recovery-remove"}},
	{"TestTaskRecoveryBoundary", []string{"disconnect", "remaining-capacity", "recovery-remove"}},
}

// taskNames lists every required task name, parents before subtests.
func taskNames() []string { return requiredNames(taskRequired) }

// controlRequired are iteration 06a's native names (the eight control
// function parents and the native group scenarios) and 06b's four
// task-control function parents.
var controlRequired = []struct {
	test string
	subs []string
}{
	{"TestControlDurability", nil}, {"TestControlReconnect", nil}, {"TestControlNodeLoss", nil}, {"TestControlLaunchSafety", nil},
	{"TestControlWorkerRecovery", nil}, {"TestControlPlaneRecovery", nil}, {"TestControlLateResult", nil}, {"TestControlLegacy", nil},
	{"TestControlNativeGroups", []string{"cooperative", "resistant", "orphan-restart", "plane-restart"}},
	{"TestControlCancellation", nil}, {"TestControlExecutionTimeout", nil}, {"TestControlBoundedWait", nil}, {"TestControlForceRemove", nil},
}

// controlNames lists every required control name, parents before
// subtests.
func controlNames() []string { return requiredNames(controlRequired) }

// mcpRequired are iteration 07a's eight MCP function parents and their
// mandatory direct children (design 07a, Function tests), in FP order.
var mcpRequired = []struct {
	test string
	subs []string
}{
	{"TestMCPProtocol", []string{"initialize", "version", "discovery", "framing", "concurrency", "cancellation"}},
	{"TestMCPRelay", []string{"ca", "pin", "trust-errors", "protocol-mismatch", "contract-errors", "no-cache", "recovery"}},
	{"TestMCPNodes", []string{"node-ls", "node-show", "shared-roster"}},
	{"TestMCPRoles", []string{"role-add", "role-set", "role-ls", "role-show", "role-rm", "global-slots", "force-pending", "force-completed", "operation-rejoin", "instance-reuse"}},
	{"TestMCPDispatch", []string{"target-id", "target-name", "overrides", "async", "attribution", "invalid-attribution", "last-slot", "wait-fast", "wait-slow", "lost-response"}},
	{"TestMCPTaskReads", []string{"task-ls", "task-show", "task-logs", "pagination", "tails", "binary-logs", "late-logs", "late-not-found", "output-bounds"}},
	{"TestMCPWaitCancel", []string{"cancel-accepted", "cancel-terminal", "cancel-errors", "wait-one", "wait-many", "any-terminal", "snapshot", "interim-default",
		"budget-deadline", "delivery-deadline", "plane-cap", "restart-budget", "own-deadline", "cancel-wait-only", "catalog-interim"}},
	{"TestMCPLifetime", []string{"eof", "sigterm", "sigint", "closed-stdout", "stalled-reader", "slow-reader-max-logs", "outstanding-wait", "task-survives", "reaping"}},
}

// mcpNames lists every required MCP name, each parent before its
// children.
func mcpNames() []string { return requiredNames(mcpRequired) }

// qualRequired are iteration 07b's eight coordinator setup and timeout
// qualification function parents and their mandatory direct children
// (design 07b, Function tests), in FP order (FP-9..FP-16).
var qualRequired = []struct {
	test string
	subs []string
}{
	{"TestMCPSetup", []string{"claude", "codex", "grok", "cursor", "runbook-ownership", "client-info"}},
	{"TestMCPQualificationProbe", []string{"immediate", "slow", "progress", "no-token", "cancellation"}},
	{"TestMCPQualificationSchema", []string{"plan", "report", "limits", "paths"}},
	{"TestMCPQualificationDecoders", []string{"claude", "codex", "grok", "cursor", "unknown-version", "non-tool-error"}},
	{"TestMCPQualificationMeasurements", []string{"default", "override", "progress", "absolute", "lower-bound", "partial", "budget"}},
	{"TestMCPQualificationPublish", []string{"verified", "partial", "redaction", "hashes", "refuse-conflict", "worker-facts"}},
	{"TestMCPQualificationReaping", []string{"cooperative", "resistant", "parent-exits-first", "interrupt", "cleanup-failure"}},
	{"TestMCPQualificationInvocation", []string{"denied", "allowed", "model-free", "ci-denied"}},
}

// qualNames lists every required 07b name, each parent before its
// children.
func qualNames() []string { return requiredNames(qualRequired) }

// realRequired are iteration 08's nine real-adapter function parents and
// their mandatory direct children (design 08, Function tests), in FP
// order (FP-1..FP-9).
var realRequired = []struct {
	test string
	subs []string
}{
	{"TestRealAdapterRegistration", []string{"registry", "paths", "selection"}},
	{"TestRealAdapterProbe", []string{"claude", "codex", "refusal"}},
	{"TestRealAdapterInvocation", []string{"claude", "codex", "stdin"}},
	{"TestRealAdapterClaudeFinal", []string{"success", "failure", "malformed"}},
	{"TestRealAdapterCodexFinal", []string{"success", "absent", "unsafe", "cleanup"}},
	{"TestRealAdapterOutcomes", []string{"exits", "denials", "controls", "replay"}},
	{"TestRealAdapterCatalog", []string{"recipes", "evidence", "ownership"}},
	{"TestRealAdapterDispatch", []string{"claude", "codex", "no-vendors"}},
	{"TestRealAdapterSmokeGate", []string{"default-off", "ci-off", "absent", "enabled"}},
}

// realNames lists every required iteration 08 name, each parent before
// its children.
func realNames() []string { return requiredNames(realRequired) }

// wsRequired are iteration 09a's ten workspace function tests in FP order
// (FP-1..FP-10) with FP-7's D1 maximum-path subcase.
var wsRequired = []struct {
	test string
	subs []string
}{
	{"TestWorkspaceCreate", nil}, {"TestWorkspaceList", nil}, {"TestWorkspaceShow", nil}, {"TestWorkspaceRemove", nil},
	{"TestWorkspacePrune", nil}, {"TestWorkspaceTransport", nil}, {"TestWorkspaceRefSet", []string{"max-path"}},
	{"TestWorkspaceStatus", nil}, {"TestWorkspaceDiff", nil}, {"TestWorkspaceNoGit", nil},
}

// wsNames lists every required iteration 09a name.
func wsNames() []string { return requiredNames(wsRequired) }

// trRequired are iteration 09b's ten local transfer function tests in FP
// order (FP-1..FP-10), a separate group after the 09a names.
var trRequired = []struct {
	test string
	subs []string
}{
	{"TestWorkspacePushGit", nil}, {"TestWorkspacePushCleanliness", nil}, {"TestWorkspacePushFolder", nil}, {"TestWorkspaceTransferIgnores", nil},
	{"TestWorkspacePullGit", nil}, {"TestWorkspacePullFolder", nil}, {"TestWorkspaceTransferCLI", nil}, {"TestWorkspaceTransferMCP", nil},
	{"TestWorkspaceTransferNoGit", nil}, {"TestWorkspaceTransferEligibility", nil},
}

// trNames lists every required iteration 09b name.
func trNames() []string { return requiredNames(trRequired) }

// realLocal is iteration 08's tagged sidecar contract with its five
// subtests, required in the sidecar package (NativeTaskProcessPackage).
var realLocal = []string{"TestRealAdapterLocal", "TestRealAdapterLocal/selection", "TestRealAdapterLocal/file", "TestRealAdapterLocal/ordering",
	"TestRealAdapterLocal/diagnostic", "TestRealAdapterLocal/restart"}

// roleNames lists every required role name, parents before subtests.
func roleNames() []string { return requiredNames(roleRequired) }

// planeNames lists every required plane name, parents before subtests.
func planeNames() []string { return requiredNames(planeRequired) }

// nodeNames lists every required node name, parents before subtests.
func nodeNames() []string { return requiredNames(nodeRequired) }

func requiredNames(tests []struct {
	test string
	subs []string
}) []string {
	var out []string
	for _, p := range tests {
		out = append(out, p.test)
		for _, s := range p.subs {
			out = append(out, p.test+"/"+s)
		}
	}
	return out
}

// qualification is a complete synthetic tests/function stream for FP-6,
// the plane trust tests and the node tests.
func qualification() []evt {
	evs := []evt{ev("start", NativePackage, ""), ev("run", NativePackage, fp6),
		ev("output", NativePackage, fp6).with("Output", "=== RUN   TestFP6ProcessGroups\n")}
	for _, s := range scenarios {
		evs = append(evs, ev("run", NativePackage, fp6+"/"+s))
	}
	for _, s := range scenarios {
		evs = append(evs, ev("pass", NativePackage, fp6+"/"+s))
	}
	evs = append(evs, ev("pass", NativePackage, fp6))
	for _, p := range append(append(append(append(append(append(append(append(append(append(planeRequired[:0:0], planeRequired...), nodeRequired...), roleRequired...), taskRequired...), controlRequired...), mcpRequired...), qualRequired...), realRequired...), wsRequired...), trRequired...) {
		evs = append(evs, ev("run", NativePackage, p.test))
		for _, s := range p.subs {
			evs = append(evs, ev("run", NativePackage, p.test+"/"+s), ev("pass", NativePackage, p.test+"/"+s))
		}
		evs = append(evs, ev("pass", NativePackage, p.test))
	}
	evs = append(evs, ev("output", NativePackage, "").with("Output", "ok\n"), ev("pass", NativePackage, ""))
	// The sidecar's own package stream carries the real task-process
	// qualification (iteration 05) and, in the same single package start,
	// the tagged real-adapter contract (iteration 08).
	sc := NativeTaskProcessPackage
	evs = append(evs, ev("start", sc, ""), ev("run", sc, "TestTaskExecutionContract"), ev("run", sc, "TestTaskExecutionContract/process"),
		ev("pass", sc, "TestTaskExecutionContract/process"), ev("pass", sc, "TestTaskExecutionContract"), ev("run", sc, realLocal[0]))
	for _, s := range realLocal[1:] {
		evs = append(evs, ev("run", sc, s), ev("pass", sc, s))
	}
	return append(evs, ev("pass", sc, realLocal[0]), ev("pass", sc, ""))
}

// without drops events matching action and test.
func without(evs []evt, action, test string) []evt {
	var out []evt
	for _, e := range evs {
		if e["Action"] == action && (e["Test"] == test || (test == "" && e["Test"] == nil)) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// replacing swaps the first event matching action/test for repl.
func replacing(evs []evt, action, test string, repl evt) []evt {
	out := append([]evt(nil), evs...)
	for i, e := range out {
		if e["Action"] == action && e["Test"] == test {
			out[i] = repl
			return out
		}
	}
	panic("no event to replace")
}

func check(s string) error { return CheckNativeResults("darwin", strings.NewReader(s)) }

func mustFail(t *testing.T, name, s string, wants ...string) {
	t.Helper()
	err := check(s)
	if err == nil {
		t.Fatalf("%s: accepted", name)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("%s: error %q lacks %q", name, err, w)
		}
	}
}

type countingReader struct{ reads int }

func (c *countingReader) Read([]byte) (int, error) { c.reads++; return 0, io.EOF }

// TestNativeRealAdapterEvidence (iteration 08): the single tagged native
// stream qualifies with the nine function parents and their children in
// tests/function and the tagged sidecar contract in internal/sidecar;
// omitting any of those, moving the sidecar names to another package
// (tests/function included), a second start of the sidecar package or a
// skipped name fails. The native command compiles the tag and never runs a
// second sidecar invocation or enables the real smoke's tag.
func TestNativeRealAdapterEvidence(t *testing.T) {
	steps, _ := NativeSteps("darwin")
	if len(steps) != 1 || !slices.Contains(steps[0].Argv, "-tags=realadaptercheck") || slices.Contains(steps[0].Argv, "./internal/sidecar") ||
		strings.Contains(strings.Join(steps[0].Argv, " "), "realadaptersmoke") {
		t.Fatalf("native plan %+v", steps)
	}
	q := qualification()
	if err := check(stream(q...)); err != nil {
		t.Fatalf("complete single-start stream: %v", err)
	}
	if n := len(realNames()); n != 9+30 || len(NativeTaskProcessTests()) != 8 {
		t.Fatalf("%d iteration 08 names, %d sidecar names", n, len(NativeTaskProcessTests()))
	}
	for _, name := range realNames() {
		mustFail(t, "missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event", unobserved)
		mustFail(t, "no pass "+name, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "skipped "+name, stream(replacing(q, "pass", name, ev("skip", NativePackage, name))...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
	}
	sc := NativeTaskProcessPackage
	for _, name := range realLocal {
		mustFail(t, "sidecar missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" in "+sc+" has no run event", unobserved)
		mustFail(t, "sidecar no pass "+name, stream(without(q, "pass", name)...), name+" in "+sc+" has no pass event", unobserved)
		mustFail(t, "sidecar skipped "+name, stream(replacing(q, "pass", name, ev("skip", sc, name))...), "test "+name+" in "+sc+" skipped: "+unobserved)
	}
	// The sidecar names under the wrong package: omitted from the sidecar
	// stream, then passing in tests/function or another package, fail.
	for _, pkg := range []string{NativePackage, "github.com/wedevwork/callsheet/internal/adapter"} {
		var moved []evt
		for _, e := range q {
			if name, _ := e["Test"].(string); e["Package"] == sc && slices.Contains(realLocal, name) {
				continue
			}
			moved = append(moved, e)
		}
		extra := []evt{}
		if pkg != NativePackage {
			extra = append(extra, ev("start", pkg, ""))
		}
		for _, name := range realLocal {
			extra = append(extra, ev("run", pkg, name), ev("pass", pkg, name))
		}
		if pkg != NativePackage {
			extra = append(extra, ev("pass", pkg, ""))
			moved = append(moved, extra...)
		} else {
			// Inside the function package's own stream, before its pass.
			i := slices.IndexFunc(moved, func(e evt) bool { return e["Package"] == NativePackage && e["Action"] == "pass" && e["Test"] == nil })
			moved = append(moved[:i], append(extra, moved[i:]...)...)
		}
		mustFail(t, "sidecar names in "+pkg, stream(moved...), "TestRealAdapterLocal in "+sc+" has no run event", unobserved)
	}
	// A second start of the sidecar package (a separate tagged invocation
	// appended to the stream) is rejected.
	second := append(append([]evt(nil), q...), ev("start", sc, ""), ev("run", sc, realLocal[0]), ev("pass", sc, realLocal[0]), ev("pass", sc, ""))
	mustFail(t, "second sidecar start", stream(second...), "package "+sc+" started twice")
}

func TestNativeStepsAndUnsupportedOS(t *testing.T) {
	steps, err := NativeSteps("darwin")
	if err != nil || len(steps) != 1 || steps[0].Name != "native" || len(steps[0].Env) != 0 ||
		strings.Join(steps[0].Argv, " ") != "go test -json -tags=realadaptercheck -count=1 -timeout=300s ./..." {
		t.Fatalf("darwin plan = %+v %v", steps, err)
	}
	for _, goos := range []string{"linux", "windows", "freebsd", ""} {
		if steps, err := NativeSteps(goos); err == nil || steps != nil || !strings.Contains(err.Error(), fmt.Sprintf("unsupported on %q", goos)) {
			t.Fatalf("NativeSteps(%q) = %v %v", goos, steps, err)
		}
		r := &countingReader{}
		if err := CheckNativeResults(goos, r); err == nil || !strings.Contains(err.Error(), "darwin only") || r.reads != 0 {
			t.Fatalf("CheckNativeResults(%q) = %v after %d reads", goos, err, r.reads)
		}
	}
	req := NativeRequiredTests()
	if strings.Join(req, ",") != "TestFP6ProcessGroups,TestFP6ProcessGroups/cooperative,TestFP6ProcessGroups/resistant,TestFP6ProcessGroups/leader-exits-first,"+
		"TestPlaneCommands,TestPlaneState,TestPlaneState/paths,TestPlaneState/persistence,TestPlaneState/locking,TestPlaneState/validation,TestPlaneState/contracts,"+
		"TestPlaneBind,TestPlaneInit,TestPlaneInit/issuance,TestPlaneInit/fingerprint,TestPlaneInit/restart-invariance,"+
		"TestPlaneTLS,TestPlaneTLS/https-only,TestPlaneTLS/prelisten-validation,TestPlaneTLS/bounded-shutdown,TestPlaneTLS/contracts,"+
		"TestPlaneReissue,TestPlaneReissue/process,TestPlaneReissue/contracts,TestPlaneStatus,TestPlaneStatus/inspection,TestPlaneStatus/expiry-warnings,TestPlanePlatform,"+
		"TestNodeTrust,TestNodeTrust/ca,TestNodeTrust/pin,TestNodeTrust/rejections,TestNodeEnrollment,TestNodeEnrollment/identity,TestNodeEnrollment/recovery,TestNodeEnrollment/locking,"+
		"TestNodeProtocol,TestNodeProtocol/hello,TestNodeProtocol/limits,TestNodeReconnect,TestNodeReconnect/restart,TestNodeReconnect/disconnect,TestNodeReconnect/shutdown,"+
		"TestNodeLease,TestNodeLease/expiry,TestNodeLease/return,TestNodeRegistry,TestNodeRegistry/restore,TestNodeRegistry/validation,"+
		"TestNodeDiscovery,TestNodeDiscovery/text,TestNodeDiscovery/json,TestNodeDiscovery/errors,"+
		"TestNodePlatform,TestNodePlatform/paths,TestNodePlatform/native-state,TestNodePlatform/policy,TestNodePlatform/sticky-write,"+
		"TestRoleConfiguration,TestRoleConfiguration/fields,TestRoleConfiguration/order,TestRoleAdapter,TestRoleAdapter/disabled,TestRoleAdapter/probe,"+
		"TestRoleValidation,TestRoleValidation/remote,TestRoleValidation/rejections,TestRoleProtocol,TestRoleProtocol/duplex,TestRoleProtocol/bounds,"+
		"TestRoleReadiness,TestRoleReadiness/changes,TestRoleReadiness/reconnect,TestRolePersistence,TestRolePersistence/restore,TestRolePersistence/failures,"+
		"TestRoleCommands,TestRoleCommands/text,TestRoleCommands/json,TestRoleCommands/trust,TestRoleMutation,TestRoleMutation/races,TestRoleMutation/remove,"+
		"TestRolePlatform,TestRolePlatform/manuals,TestRolePlatform/executable,TestRolePlatform/policy,"+
		"TestTaskModel,TestTaskModel/envelope,TestTaskModel/states,TestTaskDispatch,TestTaskDispatch/selection,TestTaskDispatch/gate-race,TestTaskDispatch/reserved-slot,"+
		"TestTaskProtocol,TestTaskProtocol/duplex,TestTaskProtocol/bounds,TestTaskProtocol/result-receipt,TestTaskProtocol/result-ack-loss,"+
		"TestTaskExecution,TestTaskExecution/compose,TestTaskExecution/invoke,TestTaskExecution/exit,TestTaskExecution/process,TestTaskExecution/platform,TestTaskExecution/policy,"+
		"TestTaskLogs,TestTaskLogs/retention,TestTaskLogs/backpressure,TestTaskLogs/final,TestTaskPersistence,TestTaskPersistence/durability,TestTaskPersistence/restore,"+
		"TestTaskCommands,TestTaskCommands/dispatch,TestTaskCommands/ls,TestTaskCommands/show,TestTaskCommands/logs,TestTaskCommands/trust,"+
		"TestTaskRoles,TestTaskRoles/counts,TestTaskRoles/mutation,TestTaskRoles/remaining-capacity,TestTaskRoles/recovery-remove,"+
		"TestTaskRecoveryBoundary,TestTaskRecoveryBoundary/disconnect,TestTaskRecoveryBoundary/remaining-capacity,TestTaskRecoveryBoundary/recovery-remove,"+
		"TestControlDurability,TestControlReconnect,TestControlNodeLoss,TestControlLaunchSafety,TestControlWorkerRecovery,TestControlPlaneRecovery,"+
		"TestControlLateResult,TestControlLegacy,TestControlNativeGroups,TestControlNativeGroups/cooperative,TestControlNativeGroups/resistant,"+
		"TestControlNativeGroups/orphan-restart,TestControlNativeGroups/plane-restart,"+
		"TestControlCancellation,TestControlExecutionTimeout,TestControlBoundedWait,TestControlForceRemove,"+strings.Join(mcpNames(), ",")+","+strings.Join(qualNames(), ",")+
		","+strings.Join(realNames(), ",")+","+strings.Join(wsNames(), ",")+","+strings.Join(trNames(), ",") || len(req) != 312+11+10 {
		t.Fatalf("required = %v", req)
	}
	req[0] = "mutated"
	if NativeRequiredTests()[0] != fp6 {
		t.Fatal("NativeRequiredTests exposes internal state")
	}
}

// TestNativeNewBoundaries (UT-3, iteration 02b): each boundary added to
// separate process-boundary scenarios from delegated contracts is required
// independently. Complete evidence qualifies; a missing run, a missing
// pass, a skip or a failure of any one of them does not, while the other
// 27 names remain present.
func TestNativeNewBoundaries(t *testing.T) {
	if err := check(stream(qualification()...)); err != nil {
		t.Fatalf("complete evidence: %v", err)
	}
	for _, name := range []string{"TestPlaneState/contracts", "TestPlaneTLS/contracts", "TestPlaneReissue/process", "TestPlaneReissue/contracts"} {
		q := qualification()
		mustFail(t, "missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event", unobserved)
		mustFail(t, "no run "+name, stream(without(q, "run", name)...), name+" has no run event", unobserved)
		mustFail(t, "no pass "+name, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "skipped "+name, stream(replacing(q, "pass", name, ev("skip", NativePackage, name))...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		mustFail(t, "failed "+name, stream(replacing(q, "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
		// Only that name is reported missing.
		err := check(stream(without(without(q, "run", name), "pass", name)...))
		if strings.Count(err.Error(), " has no ") != 1 {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}

// TestNativeHostFixture replays the verbatim host capture (see
// testdata/cli-go-test.txt) through the production decoder.
func TestNativeHostFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "cli-go-test.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"Package":"github.com/wedevwork/callsheet/internal/cli"`)) || bytes.Contains(raw, []byte("Synthetic")) {
		t.Fatal("fixture is not the recorded internal/cli capture")
	}
	err = CheckNativeResults("darwin", bytes.NewReader(raw))
	if err == nil {
		t.Fatal("a cli-only stream qualified darwin")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "devcheck: native: required evidence missing ("+unobserved+")") ||
		!strings.Contains(msg, "package "+NativePackage+" has no start event") || !strings.Contains(msg, "TestFP6ProcessGroups/leader-exits-first has no run event") ||
		strings.Contains(msg, "Action") || strings.Contains(msg, "malformed event") || strings.Contains(msg, "internal/cli") {
		t.Fatalf("fixture must fail only for missing qualification, got: %v", err)
	}
	full := string(raw) + stream(qualification()...)
	if err := check(full); err != nil {
		t.Fatalf("fixture plus synthetic qualification: %v", err)
	}
	// Synthetic qualification interleaved into the real segment (which stays
	// unmodified and in order) is also valid.
	lines := strings.SplitAfter(string(raw), "\n")
	q := strings.SplitAfter(stream(qualification()...), "\n")
	var mixed strings.Builder
	for i := 0; i < len(lines) || i < len(q); i++ {
		if i < len(lines) {
			mixed.WriteString(lines[i])
		}
		if i < len(q) {
			mixed.WriteString(q[i])
		}
	}
	if err := check(mixed.String()); err != nil {
		t.Fatalf("interleaved: %v", err)
	}
}

func TestNativeAcceptedActions(t *testing.T) {
	other := "example.com/other"
	nt := "example.com/notests"
	evs := []evt{
		{"ImportPath": other + " [" + other + ".test]", "Action": "build-output", "Output": "# note\n"},
		ev("start", other, ""), ev("start", nt, ""),
		ev("output", nt, "").with("Output", "?   \texample.com/notests\t[no test files]\n"),
		ev("skip", nt, ""),
		ev("run", other, "TestA"), ev("pause", other, "TestA"), ev("cont", other, "TestA"),
		ev("output", other, "TestA").with("Output", "=== RUN TestA\n"),
		ev("run", other, "BenchmarkX"), ev("bench", other, "BenchmarkX").with("Output", "BenchmarkX 3 1 ns/op\n"),
		ev("pass", other, "BenchmarkX"), ev("pass", other, "TestA"),
		ev("bench", "", "").with("Output", "stray\n"), ev("output", "", "").with("Output", "stray\n"),
	}
	evs = append(evs, qualification()...)
	evs = append(evs, ev("pass", other, "").with("Elapsed", 1.5).with("OutputType", "future-field"))
	if err := check(stream(evs...)); err != nil {
		t.Fatalf("all accepted actions: %v", err)
	}
}

func TestNativeActionValidation(t *testing.T) {
	q := qualification()
	for name, bad := range map[string]evt{
		"unknown":  ev("attr", NativePackage, ""),
		"empty":    {"Action": "", "Package": NativePackage},
		"missing":  {"Package": NativePackage, "Output": "x"},
		"non-text": {"Action": 7},
	} {
		s := stream(q[0]) + stream(bad) + stream(q[1:]...)
		j, _ := json.Marshal(bad)
		mustFail(t, name, s, "event 2", string(j))
	}
	mustFail(t, "unknown action text", stream(append([]evt{ev("attr", NativePackage, "")}, q...)...), `unknown Action "attr"`)
	mustFail(t, "empty action text", stream(evt{"Action": ""}), "empty Action")
	mustFail(t, "missing action text", stream(evt{"Package": "p"}), "no Action")
	mustFail(t, "null event", "null\n", "no Action")
	mustFail(t, "non-object", "42\n", "malformed event")
}

func TestNativeFailures(t *testing.T) {
	q := qualification()
	// fail and build-fail always fail; FailedBuild is preserved.
	fb := evt{"Action": "fail", "Package": NativePackage, "FailedBuild": NativePackage + " [" + NativePackage + ".test]"}
	mustFail(t, "fail with FailedBuild", stream(q[0], fb), "FailedBuild "+NativePackage+" ["+NativePackage+".test]", "package "+NativePackage+" failed")
	mustFail(t, "build-fail", stream(append([]evt{{"ImportPath": "x [x.test]", "Action": "build-fail"}}, q...)...), "build failed for x [x.test]")
	mustFail(t, "nonrequired test fail", stream(append([]evt{ev("start", "p", ""), ev("run", "p", "TestX"),
		ev("output", "p", "TestX").with("Output", "denied: operation not permitted\n"), ev("fail", "p", "TestX")}, q...)...),
		"test TestX in p failed", "denied: operation not permitted")
	for _, s := range scenarios {
		name := fp6 + "/" + s
		mustFail(t, "missing run "+s, stream(without(q, "run", name)...), name+" has no run event", unobserved)
		mustFail(t, "missing pass "+s, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "deleted "+s, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event")
		skip := ev("skip", NativePackage, name)
		mustFail(t, "skipped "+s, stream(replacing(q, "pass", name, skip)...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		mustFail(t, "failed "+s, stream(replacing(q, "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
	}
	mustFail(t, "parent skipped", stream(append(q[:3:3], ev("output", NativePackage, fp6).with("Output", "skipping: no facility\n"), ev("skip", NativePackage, fp6))...),
		fp6+" in "+NativePackage+" skipped", "skipping: no facility")
	mustFail(t, "parent missing", stream(without(without(q, "run", fp6), "pass", fp6)...), fp6+" has no run event")
	mustFail(t, "package pass missing", stream(without(q, "pass", "")...), "did not finish")
	mustFail(t, "required package skipped", stream(ev("start", NativePackage, ""), ev("skip", NativePackage, "")), "required package "+NativePackage+" skipped")
	mustFail(t, "zero tests", stream(ev("start", NativePackage, ""), ev("pass", NativePackage, "")), fp6+" has no run event")
	var wrong []evt
	for _, e := range q {
		c := evt{}
		for k, v := range e {
			c[k] = v
		}
		if c["Package"] == NativePackage {
			c["Package"] = "github.com/wedevwork/callsheet/tests/other"
		}
		wrong = append(wrong, c)
	}
	mustFail(t, "wrong package", stream(wrong...), "package "+NativePackage+" has no start event")
	// A pass without its run cannot satisfy a required test.
	mustFail(t, "pass without run", stream(without(q, "run", fp6+"/resistant")...), fp6+"/resistant has no run event")
	full := stream(q...)
	mustFail(t, "truncated", full[:len(full)-9], "malformed event stream after")
	mustFail(t, "garbage", full+"{not json\n", "malformed event stream after "+fmt.Sprint(len(q)))
	mustFail(t, "empty", "", "empty event stream: "+unobserved)
	mustFail(t, "whitespace only", "\n \n", "empty event stream")
	mustFail(t, "unfinished other package", stream(append([]evt{ev("start", "p", "")}, q...)...), "package p did not finish")
	mustFail(t, "skip after tests", stream(append([]evt{ev("start", "p", ""), ev("run", "p", "TestX"), ev("pass", "p", "TestX"), ev("skip", "p", "")}, q...)...), "package p skipped after running tests")
	mustFail(t, "event before start", stream(append([]evt{ev("output", "p", "").with("Output", "x\n")}, q...)...), "output event for package p before its start")
	mustFail(t, "event after finish", stream(append(q, ev("output", NativePackage, "").with("Output", "late\n"))...), "after package "+NativePackage+" finished")
	mustFail(t, "double start", stream(append([]evt{q[0]}, q...)...), "started twice")
	mustFail(t, "run without test", stream(q[0], ev("run", NativePackage, "")), "run event names no Test")
	mustFail(t, "run without package", stream(ev("run", "", "TestX")), "run event names no Package")
	mustFail(t, "pause not running", stream(q[0], ev("pause", NativePackage, "TestX")), "pause event for a test that is not running")
	mustFail(t, "cont after done", stream(q[0], ev("run", NativePackage, "TestX"), ev("pass", NativePackage, "TestX"), ev("cont", NativePackage, "TestX")), "cont event for a test that is not running")
	mustFail(t, "pause without test", stream(q[0], ev("pause", NativePackage, "")), "not running")
}

func TestNativeDiagnosticsBounded(t *testing.T) {
	q := qualification()
	evs := []evt{q[0], ev("run", NativePackage, "TestNoisy")}
	for i := 0; i < 50; i++ {
		evs = append(evs, ev("output", NativePackage, "TestNoisy").with("Output", fmt.Sprintf("line %02d %s\n", i, strings.Repeat("y", 400))))
	}
	evs = append(evs, ev("fail", NativePackage, "TestNoisy").with("Output", strings.Repeat("z", 2000)))
	err := check(stream(evs...))
	if err == nil {
		t.Fatal("accepted")
	}
	msg := err.Error()
	if strings.Contains(msg, "line 29 ") || !strings.Contains(msg, "line 30 ") || !strings.Contains(msg, "line 49 ") {
		t.Fatalf("tail window wrong: %s", msg)
	}
	if strings.Count(msg, "y...") != 20 || len(msg) > 20*(maxTailLine+20)+maxQuote+300 {
		t.Fatalf("diagnostics unbounded (%d bytes)", len(msg))
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 5}
	for _, s := range []string{"abc", "defg", "h"} {
		if n, err := tb.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatal(n, err)
		}
	}
	if string(tb.b) != "defgh" {
		t.Fatalf("tail = %q", tb.b)
	}
}

// --- driver (UT-2 stage dispatch, UT-4 native execution) ---

func TestStagesMatchDispatch(t *testing.T) {
	want := "test coverage bench cross all native stress stress-packages stress-plane-cpu1 stress-plane stress-sidecar-cpu1 stress-sidecar stress-processgroup stress-functions"
	got := Stages()
	if strings.Join(got, " ") != want {
		t.Fatalf("Stages = %v", got)
	}
	got[0] = "bogus"
	if Stages()[0] != "test" {
		t.Fatal("Stages exposes dispatch state")
	}
	if code, _, errOut := runDriver(t, "linux", &fakeRunner{}, "bogus"); code != 2 || !strings.Contains(errOut, `unknown subcommand "bogus"`) {
		t.Fatalf("mutated name dispatched: %d %s", code, errOut)
	}
	for _, goos := range []string{"linux", "darwin"} {
		for _, st := range Stages() {
			f := &fakeRunner{coverTotal: "90%", cmdList: cmdList, profile: goodProfile, native: stream(qualification()...)}
			code, out, errOut := runDriver(t, goos, f, st)
			if code == 2 || strings.Contains(errOut, "unknown subcommand") {
				t.Fatalf("%s %s not recognized: %s", goos, st, errOut)
			}
			wantCode := 0
			if st == "native" && goos != "darwin" {
				wantCode = 1
			}
			if code != wantCode || (code == 0 && !strings.Contains(out, "stage "+st+" ok") && st != "all") {
				t.Fatalf("%s %s = %d\n%s\n%s", goos, st, code, out, errOut)
			}
			if code == 0 && len(f.calls) == 0 {
				t.Fatalf("%s %s succeeded without running anything", goos, st)
			}
			os.RemoveAll(scratchFrom(out))
		}
	}
	if code, _, errOut := runDriver(t, "linux", &fakeRunner{}, "natives"); code != 2 || !strings.HasSuffix(errOut, "\n"+usageLiteral) {
		t.Fatalf("unknown stage = %d %s", code, errOut)
	}
}

// usageLiteral is design 06a-perf's exact usage line, final newline
// included, written independently of devcheck.go.
const usageLiteral = "usage: devcheck test | coverage [-o profile] | bench | cross | all | native | stress | stress-packages | stress-plane-cpu1 | stress-plane | stress-sidecar-cpu1 | stress-sidecar | stress-processgroup | stress-functions\n"

// TestUsageLiteral pins the usage constant and its stage order to the
// fourteen advertised stages.
func TestUsageLiteral(t *testing.T) {
	if usage != usageLiteral {
		t.Fatalf("usage = %q", usage)
	}
	if len(stageNames) != 14 {
		t.Fatalf("%d stages advertised", len(stageNames))
	}
	var out, errOut bytes.Buffer
	if code := runFor(context.Background(), "linux", nil, &out, &errOut, (&fakeRunner{}).run); code != 2 || errOut.String() != usageLiteral || out.Len() != 0 {
		t.Fatalf("no arguments = %d %q", code, errOut.String())
	}
}

// A stage name added to the definition without an implementation cannot
// pass dispatch.
func TestAdvertisedStageMustBeImplemented(t *testing.T) {
	saved := stageNames
	defer func() { stageNames = saved }()
	stageNames[len(stageNames)-1] = "phantom"
	code, out, errOut := runDriver(t, "darwin", &fakeRunner{}, "phantom")
	if code != 1 || !strings.Contains(errOut, `stage "phantom" is advertised but not implemented`) {
		t.Fatalf("phantom = %d %s", code, errOut)
	}
	os.RemoveAll(scratchFrom(out))
}

func TestNativeStageRunFor(t *testing.T) {
	f := &fakeRunner{native: stream(qualification()...), nativeErr: "go: downloading nothing\n"}
	code, out, errOut := runDriver(t, "darwin", f, "native")
	if code != 0 {
		t.Fatalf("native = %d %s", code, errOut)
	}
	// Iteration 09a: the qualification, then the coverage stage and the
	// workspace benchmarks; iteration 09b: then the transfer benchmarks.
	if len(f.calls) != 6 || strings.Join(f.calls[0].argv, " ") != "go test -json -tags=realadaptercheck -count=1 -timeout=300s ./..." || f.calls[0].dir != "" ||
		strings.Join(f.calls[4].argv, " ") != strings.Join(WorkspaceBenchStep().Argv, " ") ||
		strings.Join(f.calls[5].argv, " ") != strings.Join(TransferBenchStep().Argv, " ") {
		t.Fatalf("calls = %+v", f.argvs())
	}
	if !strings.Contains(strings.Join(f.calls[0].env, "\n"), "PATH=") {
		t.Fatal("child env must extend the parent environment")
	}
	if !strings.Contains(out, "devcheck: native qualification passed on darwin/") || !strings.Contains(out, "TestFP6ProcessGroups/leader-exits-first") ||
		!strings.Contains(out, `"Action":"pass"`) || !strings.Contains(out, "stage native ok") {
		t.Fatalf("out = %s", out)
	}
	if strings.Contains(out, "go: downloading") || !strings.Contains(errOut, "go: downloading nothing") {
		t.Fatalf("stderr not separated: out=%q err=%q", out, errOut)
	}
	if _, err := os.Stat(scratchFrom(out)); !os.IsNotExist(err) {
		t.Fatal("successful scratch not deleted")
	}
	for _, goos := range []string{"linux", "windows", "freebsd", ""} {
		f := &fakeRunner{native: stream(qualification()...)}
		code, out, errOut := runDriver(t, goos, f, "native")
		if code != 1 || len(f.calls) != 0 || !strings.Contains(errOut, "stage native FAILED") || !strings.Contains(errOut, "darwin only") {
			t.Fatalf("%q native = %d calls=%d %s", goos, code, len(f.calls), errOut)
		}
		os.RemoveAll(scratchFrom(out))
	}
	for _, args := range [][]string{{"native", "-o", "x"}, {"native", "extra"}} {
		if code, _, errOut := runDriver(t, "darwin", &fakeRunner{}, args...); code != 2 || !strings.Contains(errOut, "invalid arguments for native") {
			t.Fatalf("%v = %d %s", args, code, errOut)
		}
	}
}

func TestNativeStageFailuresRetainScratch(t *testing.T) {
	// Child failure: StepError names the command and keeps stderr.
	f := &fakeRunner{fail: "-json"}
	code, out, errOut := runDriver(t, "darwin", f, "native")
	scratch := scratchFrom(out)
	if code != 1 || !strings.Contains(errOut, "native failed: go test -json -tags=realadaptercheck -count=1 -timeout=300s ./...: exit status 1") ||
		!strings.Contains(errOut, "boom from child") || !strings.Contains(errOut, "logs retained in "+scratch) {
		t.Fatalf("child failure = %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(scratch, "native-events.jsonl")); err != nil {
		t.Fatal("event file not retained")
	}
	os.RemoveAll(scratch)
	// Successful exit with missing evidence fails validation.
	f = &fakeRunner{native: stream(without(qualification(), "run", fp6+"/resistant")...)}
	code, out, errOut = runDriver(t, "darwin", f, "native")
	scratch = scratchFrom(out)
	if code != 1 || !strings.Contains(errOut, unobserved) || !strings.Contains(errOut, filepath.Join(scratch, "native-events.jsonl")) ||
		strings.Contains(out, "stage native ok") {
		t.Fatalf("parser failure = %d %s", code, errOut)
	}
	os.RemoveAll(scratch)
	// Cancellation follows the Runner error path.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var o, e bytes.Buffer
	cancelRunner := func(ctx context.Context, argv, env []string, dir string, stdout, stderr io.Writer) error {
		return ctx.Err()
	}
	if code := runFor(ctx, "darwin", []string{"native"}, &o, &e, cancelRunner); code != 1 || !strings.Contains(e.String(), "context canceled") {
		t.Fatalf("canceled = %d %s", code, e.String())
	}
	os.RemoveAll(scratchFrom(o.String()))
}

func newDriver(t *testing.T, run Runner) (*driver, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return &driver{ctx: context.Background(), run: run, out: &out, errOut: &errOut, scratch: t.TempDir(), goos: "darwin"}, &out, &errOut
}

func TestNativeDriverStreams(t *testing.T) {
	// stderr carrying a JSON failure event must never reach the parser.
	bogus := `{"Action":"fail","Package":"x"}` + "\n"
	cover := &fakeRunner{native: "x"}
	run := func(ctx context.Context, argv, env []string, dir string, stdout, stderr io.Writer) error {
		if strings.Join(argv[:3], " ") != "go test -json" {
			// Iteration 09a: the coverage stage and benchmarks that follow
			// a qualified native run.
			return cover.run(ctx, argv, env, dir, stdout, stderr)
		}
		io.WriteString(stdout, stream(qualification()...))
		io.WriteString(stderr, bogus)
		return nil
	}
	d, out, errOut := newDriver(t, run)
	steps, _ := NativeSteps("darwin")
	if err := d.native(steps); err != nil {
		t.Fatalf("native: %v", err)
	}
	events, _ := os.ReadFile(filepath.Join(d.scratch, "native-events.jsonl"))
	stderrLog, _ := os.ReadFile(filepath.Join(d.scratch, "native-stderr.log"))
	if string(events) != stream(qualification()...) || string(stderrLog) != bogus {
		t.Fatalf("events=%q stderr=%q", events, stderrLog)
	}
	if errOut.String() != bogus || strings.Contains(out.String(), bogus) {
		t.Fatalf("user streams: out=%q err=%q", out, errOut)
	}
	// Fail fast: a later child never runs after a failure, even if an
	// earlier child produced a complete successful stream.
	var calls int
	run = func(_ context.Context, _, _ []string, _ string, stdout, _ io.Writer) error {
		calls++
		if calls == 1 {
			io.WriteString(stdout, stream(qualification()...))
			return nil
		}
		return errors.New("exit status 2")
	}
	d, _, _ = newDriver(t, run)
	two := []Step{{Name: "one", Argv: []string{"a"}}, {Name: "two", Argv: []string{"b"}}, {Name: "three", Argv: []string{"c"}}}
	var se *StepError
	if err := d.native(two); !errors.As(err, &se) || se.Step.Name != "two" || calls != 2 {
		t.Fatalf("fail fast: %v after %d calls", err, calls)
	}
}

func TestNativeDriverFileFailures(t *testing.T) {
	ok := func(_ context.Context, _, _ []string, _ string, stdout, _ io.Writer) error {
		io.WriteString(stdout, stream(qualification()...))
		return nil
	}
	steps, _ := NativeSteps("darwin")
	// Scratch missing: the event file cannot be created.
	d, _, _ := newDriver(t, ok)
	d.scratch = filepath.Join(d.scratch, "missing")
	if err := d.native(steps); err == nil || !strings.Contains(err.Error(), "native-events.jsonl") {
		t.Fatalf("create events: %v", err)
	}
	// The stderr log path is occupied by a directory.
	d, _, _ = newDriver(t, ok)
	os.Mkdir(filepath.Join(d.scratch, "native-stderr.log"), 0o700)
	if err := d.native(steps); err == nil || !strings.Contains(err.Error(), "native-stderr.log") {
		t.Fatalf("create stderr log: %v", err)
	}
	// The event file vanishes before it is reopened for validation.
	d, _, _ = newDriver(t, ok)
	d.run = func(ctx context.Context, argv, env []string, dir string, stdout, stderr io.Writer) error {
		ok(ctx, argv, env, dir, stdout, stderr)
		os.Remove(filepath.Join(d.scratch, "native-events.jsonl"))
		return nil
	}
	if err := d.native(steps); err == nil || !os.IsNotExist(err) {
		t.Fatalf("reopen: %v", err)
	}
}

// Other stages keep the merged stdout/stderr execution path.
func TestOldStagesStillMergeStderr(t *testing.T) {
	run := func(_ context.Context, _, _ []string, _ string, stdout, stderr io.Writer) error {
		io.WriteString(stderr, "from-stderr\n")
		return nil
	}
	var out, errOut bytes.Buffer
	if code := runFor(context.Background(), "darwin", []string{"test"}, &out, &errOut, run); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "from-stderr") || strings.Contains(errOut.String(), "from-stderr") {
		t.Fatalf("test stage streams changed: out=%q err=%q", out.String(), errOut.String())
	}
}

// S1: an unsupported native stage is rejected before any scratch directory
// exists, so nothing is created and no retained-logs line is printed.
func TestUnsupportedNativeCreatesNoScratch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	for _, goos := range []string{"linux", "windows", "freebsd", ""} {
		f := &fakeRunner{}
		code, out, errOut := runDriver(t, goos, f, "native")
		if code != 1 || len(f.calls) != 0 || !strings.Contains(errOut, "stage native FAILED") || !strings.Contains(errOut, "darwin only") {
			t.Fatalf("%q native = %d calls=%d %s", goos, code, len(f.calls), errOut)
		}
		if strings.Contains(errOut, "logs retained") || strings.Contains(out, "devcheck: scratch") {
			t.Fatalf("%q native claims a scratch dir: out=%q err=%q", goos, out, errOut)
		}
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			t.Fatalf("%q native created %v", goos, entries)
		}
	}
}

// TestNativeMCPEvidence (iteration 07a): the eight MCP parents and every
// mandatory child are required by exact name. Complete evidence
// qualifies; a missing, failed or skipped child fails naming that child,
// and a vacuous parent (its own run and pass without its children's)
// fails naming every child it lacks.
func TestNativeMCPEvidence(t *testing.T) {
	if err := check(stream(qualification()...)); err != nil {
		t.Fatalf("complete evidence: %v", err)
	}
	if n := len(mcpNames()); n != 8+69 {
		t.Fatalf("%d MCP names, want 8 parents and 69 children", n)
	}
	for _, name := range []string{"TestMCPProtocol/cancellation", "TestMCPRelay/contract-errors", "TestMCPRoles/operation-rejoin",
		"TestMCPDispatch/invalid-attribution", "TestMCPTaskReads/late-not-found", "TestMCPWaitCancel/catalog-interim", "TestMCPLifetime/closed-stdout",
		"TestMCPLifetime/slow-reader-max-logs", "TestMCPNodes", "TestMCPWaitCancel"} {
		q := qualification()
		mustFail(t, "missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event", unobserved)
		mustFail(t, "no pass "+name, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "skipped "+name, stream(replacing(q, "pass", name, ev("skip", NativePackage, name))...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		mustFail(t, "failed "+name, stream(replacing(q, "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
	}
	// A vacuous parent: TestMCPLifetime runs and passes without any child.
	q := qualification()
	for _, sub := range mcpRequired[7].subs {
		name := "TestMCPLifetime/" + sub
		q = without(without(q, "run", name), "pass", name)
	}
	err := check(stream(q...))
	if err == nil {
		t.Fatal("a vacuous TestMCPLifetime qualified")
	}
	for _, sub := range mcpRequired[7].subs {
		if !strings.Contains(err.Error(), "TestMCPLifetime/"+sub+" has no run event") {
			t.Fatalf("vacuous parent error lacks %s: %v", sub, err)
		}
	}
	// A substitute name (a child of another parent, or a renamed child)
	// never satisfies a required one.
	q = replacing(qualification(), "pass", "TestMCPWaitCancel/cancel-wait-only", ev("pass", NativePackage, "TestMCPWaitCancel/cancel-accepted"))
	mustFail(t, "substitute", stream(q...), "TestMCPWaitCancel/cancel-wait-only has no pass event")
}

// TestNativeQualificationEvidence (iteration 07b): the eight setup and
// qualification parents and every mandatory child are required by exact
// name, after the unchanged 222 earlier names. A missing, failed or
// skipped child fails naming it, and a vacuous parent fails naming every
// child it lacks.
func TestNativeQualificationEvidence(t *testing.T) {
	if err := check(stream(qualification()...)); err != nil {
		t.Fatalf("complete evidence: %v", err)
	}
	req := NativeRequiredTests()
	if n := len(qualNames()); n != 8+43 || len(req) != 222+n+len(realNames())+len(wsNames())+len(trNames()) || strings.Join(req[222:273], ",") != strings.Join(qualNames(), ",") ||
		strings.Join(req[145:222], ",") != strings.Join(mcpNames(), ",") {
		t.Fatalf("%d 07b names; native suffix %v", n, req[222:])
	}
	for _, name := range []string{"TestMCPSetup/runbook-ownership", "TestMCPQualificationProbe/no-token", "TestMCPQualificationDecoders/non-tool-error",
		"TestMCPQualificationMeasurements/budget", "TestMCPQualificationPublish/worker-facts", "TestMCPQualificationReaping/cleanup-failure",
		"TestMCPQualificationInvocation/ci-denied", "TestMCPQualificationSchema"} {
		q := qualification()
		mustFail(t, "missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event", unobserved)
		mustFail(t, "no pass "+name, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "skipped "+name, stream(replacing(q, "pass", name, ev("skip", NativePackage, name))...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		mustFail(t, "failed "+name, stream(replacing(q, "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
	}
	q := qualification()
	for _, sub := range qualRequired[6].subs {
		name := "TestMCPQualificationReaping/" + sub
		q = without(without(q, "run", name), "pass", name)
	}
	err := check(stream(q...))
	if err == nil {
		t.Fatal("a vacuous TestMCPQualificationReaping qualified")
	}
	for _, sub := range qualRequired[6].subs {
		if !strings.Contains(err.Error(), "TestMCPQualificationReaping/"+sub+" has no run event") {
			t.Fatalf("vacuous parent error lacks %s: %v", sub, err)
		}
	}
	// A child of another parent never substitutes for a required one.
	q = replacing(qualification(), "pass", "TestMCPQualificationDecoders/claude", ev("pass", NativePackage, "TestMCPSetup/claude"))
	mustFail(t, "substitute", stream(q...), "TestMCPQualificationDecoders/claude has no pass event")
}
