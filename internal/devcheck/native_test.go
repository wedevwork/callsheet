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
	"runtime"
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
	{"TestRealAdapterProbe", []string{"claude", "codex", "refusal", "auto-minima"}},
	{"TestRealAdapterInvocation", []string{"claude", "codex", "stdin", "auto-claude", "auto-codex"}},
	{"TestRealAdapterClaudeFinal", []string{"success", "failure", "malformed"}},
	{"TestRealAdapterCodexFinal", []string{"success", "absent", "unsafe", "cleanup"}},
	{"TestRealAdapterOutcomes", []string{"exits", "denials", "controls", "replay", "auto-policy", "auto-denials"}},
	{"TestRealAdapterCatalog", []string{"recipes", "evidence", "ownership", "auto-contract"}},
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

// latRequired are iteration 10a's two latency function tests in FP order
// (FP-1, FP-2), a separate group after the 09b names.
var latRequired = []struct {
	test string
	subs []string
}{
	{"TestTaskPromptReadiness", nil}, {"TestTaskFastGroupCleanup", nil},
}

// latNames lists every required iteration 10a name.
func latNames() []string { return requiredNames(latRequired) }

// wsTaskRequired are iteration 10b's nine workspace execution function
// tests in FP order (FP-1..FP-9), each named acceptance scenario right
// after its parent, a separate group after the 10a names.
var wsTaskRequired = []struct {
	test string
	subs []string
}{
	{"TestTaskWorkspaceAdmission", nil}, {"TestTaskWorkspaceAccess", nil}, {"TestTaskWorkspaceCache", nil}, {"TestTaskWorkspaceCheckout", nil},
	{"TestTaskWorkspaceCommit", []string{"AC-WS-1"}}, {"TestTaskWorkspacePublication", []string{"AC-WS-5"}}, {"TestTaskWorkspaceMetadata", nil},
	{"TestTaskWorkspaceRecovery", nil}, {"TestTaskWorkspaceIsolation", []string{"AC-WS-2"}},
}

// wsTaskNames lists every required iteration 10b name.
func wsTaskNames() []string { return requiredNames(wsTaskRequired) }

// wsDoorRequired are iteration 10c's six coordinator delivery function
// tests in FP order (FP-1..FP-6), a separate group after the 10b names;
// their named subtests are not inventory entries.
var wsDoorRequired = []struct {
	test string
	subs []string
}{
	{"TestWorkspaceDispatchDoors", nil}, {"TestWorkspaceTaskPull", nil}, {"TestWorkspaceTaskInspect", nil},
	{"TestWorkspaceTaskMCP", nil}, {"TestWorkspaceMultiHop", nil}, {"TestWorkspaceOperatorWorkflow", nil},
}

// wsDoorNames lists every required iteration 10c name.
func wsDoorNames() []string { return requiredNames(wsDoorRequired) }

// wave2Required are iteration 11's nine wave-2 real-adapter function
// parents in FP order (FP-1..FP-9) with their mandatory direct subtests in
// the design's order, a separate group after the 10c names.
var wave2Required = []struct {
	test string
	subs []string
}{
	{"TestWave2Registration", []string{"registry", "paths", "selection", "posture"}},
	{"TestWave2Probe", []string{"grok", "cursor", "refusal"}},
	{"TestWave2Invocation", []string{"grok", "cursor-boundaries", "prompt", "auto-grok"}},
	{"TestWave2GrokFinal", []string{"success", "error", "cancelled", "malformed"}},
	{"TestWave2CursorFinal", []string{"success", "absent", "malformed", "blocked"}},
	{"TestWave2Outcomes", []string{"exits", "controls", "refusal", "retry"}},
	{"TestWave2Catalog", []string{"recipes", "evidence", "ownership"}},
	{"TestWave2Dispatch", []string{"grok", "auto-cursor", "no-vendors"}},
	{"TestWave2SmokeGate", []string{"default-off", "ci-off", "absent", "enabled", "posture"}},
}

// wave2Names lists every required iteration 11 name.
func wave2Names() []string { return requiredNames(wave2Required) }

// nbwRequired are the non-blocking coordinator waits design's eight
// function parents in FP order (FP-1..FP-8), a separate group after the
// iteration 11 names; their scenarios assert within each parent and are
// not inventory entries.
var nbwRequired = []struct {
	test string
	subs []string
}{
	{"TestWaitUntilDoneCLI", nil}, {"TestWaitUntilDoneRenewal", nil}, {"TestWaitUntilDoneFailure", nil}, {"TestWaitUntilDoneOutput", nil},
	{"TestCoordinatorBackgroundWait", nil}, {"TestMCPShortPollGuidance", nil}, {"TestShortPollCatalogPolicy", nil}, {"TestMCPShortConfirmation", nil},
}

// nbwNames lists every required non-blocking coordinator waits name.
func nbwNames() []string { return requiredNames(nbwRequired) }

// dceRequired are the decoder-enrollment design's nine function parents
// in FP order (FP-1..FP-9), a separate group after the non-blocking waits
// names; their literal case inventories assert within each parent and are
// not inventory entries.
var dceRequired = []struct {
	test string
	subs []string
}{
	{"TestMCPCaptureInvocation", nil}, {"TestMCPCaptureRecipes", nil}, {"TestMCPCaptureEvidence", nil}, {"TestMCPCaptureLifecycle", nil},
	{"TestMCPEnrollmentContract", nil}, {"TestMCPEnrollmentReplay", nil}, {"TestMCPEnrolledShortConfirmation", nil}, {"TestMCPCaptureRunbook", nil},
	{"TestMCPEnrollmentCIPolicy", nil},
}

// dceNames lists every required decoder-enrollment name.
func dceNames() []string { return requiredNames(dceRequired) }

// b1Required are decoder-enrollment slice B1's four function parents in FP
// order (FP-10..FP-13), a separate group after the slice A names; their
// literal case inventories assert within each parent.
var b1Required = []struct {
	test string
	subs []string
}{
	{"TestMCPCaptureProtocolNegotiation", nil}, {"TestMCPCaptureCodexApproval", nil}, {"TestMCPCaptureGrokRecipe", nil}, {"TestMCPCaptureCursorTrust", nil},
}

// b1Names lists every required slice B1 name.
func b1Names() []string { return requiredNames(b1Required) }

// b15Required are decoder-enrollment slice B1.5's three function parents
// in FP order (FP-14..FP-16), a separate group after the B1 names; their
// literal local case inventories assert within each parent.
var b15Required = []struct {
	test string
	subs []string
}{
	{"TestMCPProbeTerminalObservation", nil}, {"TestMCPCaptureCursorProjectApproval", nil}, {"TestMCPCaptureCursorInventoryPolicy", nil},
}

// b15Names lists every required slice B1.5 name.
func b15Names() []string { return requiredNames(b15Required) }

// b2Required are decoder-enrollment slice B2's five function parents in FP
// order (FP-17..FP-21), a separate group after the B1.5 names; their
// literal local case inventories assert within each parent.
var b2Required = []struct {
	test string
	subs []string
}{
	{"TestMCPRealDecoderMappings", nil}, {"TestMCPFixtureSanitization", nil}, {"TestMCPRealEnrollment", nil}, {"TestMCPCaptureCursorToolPermission", nil},
	{"TestMCPRealEnrollmentConfirmation", nil},
}

// b2Names lists every required slice B2 name.
func b2Names() []string { return requiredNames(b2Required) }

// b3Required are decoder-enrollment slice B3's five function parents in FP
// order (FP-22..FP-26), a separate group after the B2 names; their literal
// local case inventories assert within each parent.
var b3Required = []struct {
	test string
	subs []string
}{
	{"TestMCPCursorRealDecoder", nil}, {"TestMCPCursorEnrollment", nil}, {"TestMCPCursorQualifyPreparation", nil}, {"TestMCPCursorWorkerResidue", nil},
	{"TestMCPCursorConfirmation", nil},
}

// b3Names lists every required slice B3 name.
func b3Names() []string { return requiredNames(b3Required) }

// realLocal is iteration 08's tagged sidecar contract with its five
// subtests and iteration 11's four wave-2 subtests, required in the
// sidecar package (NativeTaskProcessPackage).
var realLocal = []string{"TestRealAdapterLocal", "TestRealAdapterLocal/selection", "TestRealAdapterLocal/file", "TestRealAdapterLocal/ordering",
	"TestRealAdapterLocal/diagnostic", "TestRealAdapterLocal/restart",
	"TestRealAdapterLocal/wave2-posture", "TestRealAdapterLocal/wave2-invocation", "TestRealAdapterLocal/wave2-outcomes", "TestRealAdapterLocal/wave2-retry"}

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

// reale2eLiteral pins NativeReale2ePackage independently of production.
const reale2eLiteral = "github.com/wedevwork/callsheet/internal/reale2e"

// reale2eTest is a synthetic test of the dedicated reale2e stream.
const reale2eTest = "TestRealE2ESynthetic"

// reale2eEvents is the second native command's complete synthetic stream:
// internal/reale2e's sole package start, one test and its package pass
// (design 12a-native-reale2e-budget).
func reale2eEvents() []evt {
	return []evt{ev("start", reale2eLiteral, ""), ev("run", reale2eLiteral, reale2eTest), ev("pass", reale2eLiteral, reale2eTest),
		ev("output", reale2eLiteral, "").with("Output", "ok\n"), ev("pass", reale2eLiteral, "")}
}

// qualification is the complete synthetic combined native stream: the
// first command's stream (sharedQualification) followed by the dedicated
// reale2e command's (reale2eEvents).
func qualification() []evt { return append(sharedQualification(), reale2eEvents()...) }

// nativeRunner is a fake runner whose two native children each write their
// own disjoint part of the combined stream evs: the events of
// internal/reale2e to the dedicated command, every other event to the
// first command, never the complete stream twice.
func nativeRunner(evs []evt) *fakeRunner {
	var shared, own []evt
	for _, e := range evs {
		if e["Package"] == reale2eLiteral {
			own = append(own, e)
		} else {
			shared = append(shared, e)
		}
	}
	return &fakeRunner{native: stream(shared...), nativeReale2e: stream(own...)}
}

// sharedQualification is a complete synthetic first-command stream: the
// tests/function package for FP-6, the plane trust tests, the node tests
// and every later required group, then the sidecar package's tuple.
func sharedQualification() []evt {
	evs := []evt{ev("start", NativePackage, ""), ev("run", NativePackage, fp6),
		ev("output", NativePackage, fp6).with("Output", "=== RUN   TestFP6ProcessGroups\n")}
	for _, s := range scenarios {
		evs = append(evs, ev("run", NativePackage, fp6+"/"+s))
	}
	for _, s := range scenarios {
		evs = append(evs, ev("pass", NativePackage, fp6+"/"+s))
	}
	evs = append(evs, ev("pass", NativePackage, fp6))
	for _, p := range append(append(append(append(append(append(append(append(append(append(append(append(append(append(append(append(append(append(append(append(planeRequired[:0:0], planeRequired...), nodeRequired...), roleRequired...), taskRequired...), controlRequired...), mcpRequired...), qualRequired...), realRequired...), wsRequired...), trRequired...), latRequired...), wsTaskRequired...), wsDoorRequired...), wave2Required...), nbwRequired...), dceRequired...), b1Required...), b15Required...), b2Required...), b3Required...) {
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
	if len(steps) != 2 || !slices.Contains(steps[0].Argv, "./internal/sidecar/...") {
		t.Fatalf("native plan %+v", steps)
	}
	for _, s := range steps {
		if !slices.Contains(s.Argv, "-tags=realadaptercheck") || slices.Contains(s.Argv, "./internal/sidecar") ||
			strings.Contains(strings.Join(s.Argv, " "), "realadaptersmoke") {
			t.Fatalf("native plan %+v", steps)
		}
	}
	q := qualification()
	if err := check(stream(q...)); err != nil {
		t.Fatalf("complete single-start stream: %v", err)
	}
	if n := len(realNames()); n != 9+36 || len(NativeTaskProcessTests()) != 12 || len(wave2Names()) != 9+34 {
		t.Fatalf("%d iteration 08 names, %d sidecar names, %d iteration 11 names", n, len(NativeTaskProcessTests()), len(wave2Names()))
	}
	for _, name := range append(realNames(), wave2Names()...) {
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
	// Design 12a-native-reale2e-budget: two commands in order, the shared
	// patterns under 300 s, then internal/reale2e alone under 600 s, each
	// pinned by an independent literal.
	steps, err := NativeSteps("darwin")
	if err != nil || len(steps) != 2 || steps[0].Name != "native" || len(steps[0].Env) != 0 || strings.Join(steps[0].Argv, " ") != wantNativeShared ||
		steps[1].Name != "native reale2e" || len(steps[1].Env) != 0 || strings.Join(steps[1].Argv, " ") != wantNativeReale2e {
		t.Fatalf("darwin plan = %+v %v", steps, err)
	}
	if !strings.Contains(wantNativeShared, " -timeout=300s ") || strings.Contains(wantNativeShared, "600s") || strings.Contains(wantNativeShared, "reale2e") ||
		!strings.HasSuffix(wantNativeReale2e, " -timeout=600s ./internal/reale2e") || strings.Contains(wantNativeReale2e, "300s") {
		t.Fatal("the 300s and 600s literals are not pinned independently")
	}
	// Fresh argv: mutating a returned plan changes neither the next plan
	// nor the shared race patterns.
	saved := slices.Clone(RaceSharedPackages)
	for _, s := range steps {
		for i := range s.Argv {
			s.Argv[i] = "mutated"
		}
	}
	again, _ := NativeSteps("darwin")
	if strings.Join(argvOf(again), "|") != wantNative || !slices.Equal(RaceSharedPackages, saved) {
		t.Fatalf("NativeSteps aliases its argv: %v, race patterns %v", argvOf(again), RaceSharedPackages)
	}
	if strings.Join(argvOf(TestSteps("linux")), "|") != wantTestPlanLinux {
		t.Fatal("the race plan changed")
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
		","+strings.Join(realNames(), ",")+","+strings.Join(wsNames(), ",")+","+strings.Join(trNames(), ",")+","+strings.Join(latNames(), ",")+","+strings.Join(wsTaskNames(), ",")+","+strings.Join(wsDoorNames(), ",")+
		","+strings.Join(wave2Names(), ",")+","+strings.Join(nbwNames(), ",")+","+strings.Join(dceNames(), ",")+","+strings.Join(b1Names(), ",")+","+strings.Join(b15Names(), ",")+","+strings.Join(b2Names(), ",")+","+strings.Join(b3Names(), ",") || len(req) != 318+11+10+2+12+6+43+8+9+4+3+5+5 {
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
	want := "test coverage bench cross all native stress stress-packages stress-packages-cpu stress-plane-cpu1 stress-plane stress-sidecar-cpu1 stress-sidecar stress-sidecar-cpu4 stress-processgroup stress-functions container-e2e"
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
			f := nativeRunner(qualification())
			f.coverTotal, f.cmdList, f.profile = "90%", cmdList, goodProfile
			// Linux test, all and container-e2e need the synthetic
			// container evidence; Darwin never invokes the container.
			var r runnerLike = f
			cr := newContainerRunnerFixture(t, f).(*containerRunner)
			if goos == "linux" && (st == "test" || st == "all" || st == "container-e2e") {
				r = cr
			}
			code, out, errOut := runDriver(t, goos, r, st)
			if code == 2 || strings.Contains(errOut, "unknown subcommand") {
				t.Fatalf("%s %s not recognized: %s", goos, st, errOut)
			}
			wantCode := 0
			if (st == "native" && goos != "darwin") || (st == "container-e2e" && goos != "linux") {
				wantCode = 1
			}
			if code != wantCode || (code == 0 && !strings.Contains(out, "stage "+st+" ok") && st != "all") {
				t.Fatalf("%s %s = %d\n%s\n%s", goos, st, code, out, errOut)
			}
			if code == 0 && len(f.calls) == 0 && len(cr.calls) == 0 {
				t.Fatalf("%s %s succeeded without running anything", goos, st)
			}
			if st == "container-e2e" && goos == "darwin" && (len(f.calls) != 0 || out != "" ||
				errOut != "devcheck: stage container-e2e FAILED: container-e2e is unsupported on \"darwin\"\n") {
				t.Fatalf("darwin container-e2e = %d calls=%d %q %q", code, len(f.calls), out, errOut)
			}
			os.RemoveAll(scratchFrom(out))
		}
	}
	if code, _, errOut := runDriver(t, "linux", &fakeRunner{}, "natives"); code != 2 || !strings.HasSuffix(errOut, "\n"+usageLiteral) {
		t.Fatalf("unknown stage = %d %s", code, errOut)
	}
}

// usageLiteral is design 06a-perf's exact usage line, with the stress
// worker rebalance's stress-packages-cpu after stress-packages, final
// newline included, written independently of devcheck.go.
const usageLiteral = "usage: devcheck test | coverage [-o profile] | bench | cross | all | native | stress | stress-packages | stress-packages-cpu | stress-plane-cpu1 | stress-plane | stress-sidecar-cpu1 | stress-sidecar | stress-sidecar-cpu4 | stress-processgroup | stress-functions | container-e2e [--count=N]\n"

// TestUsageLiteral pins the usage constant and its stage order to the
// seventeen advertised stages (container-e2e since m3-m4-container-e2e,
// stress-packages-cpu since the stress worker rebalance,
// stress-sidecar-cpu4 since design 12a-stress-sidecar-margin).
func TestUsageLiteral(t *testing.T) {
	if usage != usageLiteral {
		t.Fatalf("usage = %q", usage)
	}
	if len(stageNames) != 17 {
		t.Fatalf("%d stages advertised", len(stageNames))
	}
	var out, errOut bytes.Buffer
	if code := runFor(context.Background(), "linux", nil, &out, &errOut, (&fakeRunner{}).run, testOpts(t)); code != 2 || errOut.String() != usageLiteral || out.Len() != 0 {
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
	f := nativeRunner(qualification())
	f.nativeErr = "go: downloading nothing\n"
	code, out, errOut := runDriver(t, "darwin", f, "native")
	if code != 0 {
		t.Fatalf("native = %d %s", code, errOut)
	}
	// Iteration 09a: the qualification, then the coverage stage and the
	// workspace benchmarks; iteration 09b: then the transfer benchmarks;
	// iteration 10b: then the task workspace benchmarks; non-blocking
	// coordinator waits: then the client wait benchmark; decoder
	// enrollment: then the qualification harness's benchmark; design
	// 12a-native-reale2e-budget: two qualification commands (11 calls).
	if len(f.calls) != 11 || strings.Join(f.calls[0].argv, " ") != wantNativeShared || f.calls[0].dir != "" ||
		strings.Join(f.calls[1].argv, " ") != wantNativeReale2e || f.calls[1].dir != "" ||
		strings.Join(f.calls[5].argv, " ") != strings.Join(WorkspaceBenchStep().Argv, " ") ||
		strings.Join(f.calls[6].argv, " ") != strings.Join(TransferBenchStep().Argv, " ") ||
		strings.Join(f.calls[7].argv, " ") != strings.Join(TaskWorkspaceBenchStep().Argv, " ") ||
		strings.Join(f.calls[8].argv, " ") != "go test ./internal/client -run=^$ -bench=^BenchmarkWaitUntilDone$ -benchmem -benchtime=3x -count=1 -timeout=180s" ||
		strings.Join(f.calls[9].argv, " ") != "go test ./internal/mcpqual -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s" ||
		strings.Join(f.calls[10].argv, " ") != strings.Join(RealE2EDevBenchStep().Argv, " ") {
		t.Fatalf("calls = %+v", f.argvs())
	}
	if !strings.Contains(strings.Join(f.calls[0].env, "\n"), "PATH=") || !strings.Contains(strings.Join(f.calls[1].env, "\n"), "PATH=") {
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
		f := nativeRunner(qualification())
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
	if code != 1 || len(f.calls) != 1 || !strings.Contains(errOut, "native failed: "+wantNativeShared+": exit status 1") ||
		!strings.Contains(errOut, "boom from child") || !strings.Contains(errOut, "logs retained in "+scratch) {
		t.Fatalf("child failure = %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(scratch, "native-events.jsonl")); err != nil {
		t.Fatal("event file not retained")
	}
	os.RemoveAll(scratch)
	// Successful exit with missing evidence fails validation.
	f = nativeRunner(without(qualification(), "run", fp6+"/resistant"))
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
	if code := runFor(ctx, "darwin", []string{"native"}, &o, &e, cancelRunner, testOpts(t)); code != 1 || !strings.Contains(e.String(), "context canceled") {
		t.Fatalf("canceled = %d %s", code, e.String())
	}
	os.RemoveAll(scratchFrom(o.String()))
}

func newDriver(t *testing.T, run Runner) (*driver, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return &driver{ctx: context.Background(), run: run, out: &out, errOut: &errOut, scratch: t.TempDir(), goos: "darwin"}, &out, &errOut
}

// childStream is each native child's own synthetic stream: the dedicated
// reale2e command's, or the first command's (design
// 12a-native-reale2e-budget).
func childStream(argv []string) string {
	if slices.Contains(argv, "./internal/reale2e") {
		return stream(reale2eEvents()...)
	}
	return stream(sharedQualification()...)
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
		io.WriteString(stdout, childStream(argv))
		io.WriteString(stderr, bogus)
		return nil
	}
	d, out, errOut := newDriver(t, run)
	steps, _ := NativeSteps("darwin")
	if err := d.native(steps); err != nil {
		t.Fatalf("native: %v", err)
	}
	// Both children append to the one event file, in order, and both
	// stderr streams reach the log only.
	events, _ := os.ReadFile(filepath.Join(d.scratch, "native-events.jsonl"))
	stderrLog, _ := os.ReadFile(filepath.Join(d.scratch, "native-stderr.log"))
	if string(events) != stream(sharedQualification()...)+stream(reale2eEvents()...) || string(events) != stream(qualification()...) || string(stderrLog) != bogus+bogus {
		t.Fatalf("events=%q stderr=%q", events, stderrLog)
	}
	if errOut.String() != bogus+bogus || strings.Contains(out.String(), bogus) {
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
	ok := func(_ context.Context, argv, _ []string, _ string, stdout, _ io.Writer) error {
		io.WriteString(stdout, childStream(argv))
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
	if code := runFor(context.Background(), "darwin", []string{"test"}, &out, &errOut, run, testOpts(t)); code != 0 {
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
	if n := len(qualNames()); n != 8+43 || len(req) != 222+n+len(realNames())+len(wsNames())+len(trNames())+len(latNames())+len(wsTaskNames())+len(wsDoorNames())+len(wave2Names())+len(nbwNames())+len(dceNames())+len(b1Names())+len(b15Names())+len(b2Names())+len(b3Names()) || strings.Join(req[222:273], ",") != strings.Join(qualNames(), ",") ||
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

// TestNativeNonblockingWaitEvidence (design nonblocking-coordinator-waits):
// the eight function parents are required by exact name after the 395
// earlier names, unchanged and first (403 then; the decoder-enrollment
// group follows them, 412 in all); a missing, failed or
// skipped parent fails naming it, and a parent passing in another package
// never satisfies it.
func TestNativeNonblockingWaitEvidence(t *testing.T) {
	q := qualification()
	if err := check(stream(q...)); err != nil {
		t.Fatalf("complete evidence: %v", err)
	}
	req := NativeRequiredTests()
	// The eight names stay a bounded segment (395..403) before the
	// decoder-enrollment group.
	if n := len(nbwNames()); n != 8 || len(req) != 436 || strings.Join(req[402:410], ",") != strings.Join(nbwNames(), ",") ||
		strings.Join(req[359:402], ",") != strings.Join(wave2Names(), ",") {
		t.Fatalf("%d names; segment %v", len(req), req[402:410])
	}
	for _, name := range nbwNames() {
		mustFail(t, "missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event", unobserved)
		mustFail(t, "no pass "+name, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "skipped "+name, stream(replacing(q, "pass", name, ev("skip", NativePackage, name))...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		mustFail(t, "failed "+name, stream(replacing(q, "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
		// Only that name is reported missing.
		if err := check(stream(without(without(q, "run", name), "pass", name)...)); strings.Count(err.Error(), " has no ") != 1 {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	// The cli package's own until-done test never stands in for the
	// function parent of the same FP.
	other := "github.com/wedevwork/callsheet/internal/cli"
	moved := append(without(without(q, "run", "TestWaitUntilDoneCLI"), "pass", "TestWaitUntilDoneCLI"),
		ev("start", other, ""), ev("run", other, "TestWaitUntilDoneCLI"), ev("pass", other, "TestWaitUntilDoneCLI"), ev("pass", other, ""))
	mustFail(t, "moved", stream(moved...), "TestWaitUntilDoneCLI has no run event")
}

// TestNativeDecoderEnrollmentEvidence (design decoder-enrollment): the
// nine function parents are required by exact name after the 403 earlier
// names, unchanged and first (412 then; the slice B1, B1.5, B2 and B3
// groups follow them, 429 in all); a missing, failed or skipped parent fails
// naming it, and a parent passing in another package never satisfies it.
func TestNativeDecoderEnrollmentEvidence(t *testing.T) {
	q := qualification()
	if err := check(stream(q...)); err != nil {
		t.Fatalf("complete evidence: %v", err)
	}
	req := NativeRequiredTests()
	// The nine names stay a bounded segment (403..412) before the B1 group.
	if n := len(dceNames()); n != 9 || len(req) != 436 || strings.Join(req[410:419], ",") != strings.Join(dceNames(), ",") ||
		strings.Join(req[402:410], ",") != strings.Join(nbwNames(), ",") {
		t.Fatalf("%d names; segment %v", len(req), req[410:419])
	}
	for _, name := range dceNames() {
		mustFail(t, "missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event", unobserved)
		mustFail(t, "no pass "+name, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "skipped "+name, stream(replacing(q, "pass", name, ev("skip", NativePackage, name))...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		mustFail(t, "failed "+name, stream(replacing(q, "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
		if err := check(stream(without(without(q, "run", name), "pass", name)...)); strings.Count(err.Error(), " has no ") != 1 {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	// The mcpqual package's own capture test never stands in for the
	// function parent of the same FP.
	other := "github.com/wedevwork/callsheet/internal/mcpqual"
	moved := append(without(without(q, "run", "TestMCPCaptureInvocation"), "pass", "TestMCPCaptureInvocation"),
		ev("start", other, ""), ev("run", other, "TestMCPCaptureInvocation"), ev("pass", other, "TestMCPCaptureInvocation"), ev("pass", other, ""))
	mustFail(t, "moved", stream(moved...), "TestMCPCaptureInvocation has no run event")
	// The native benchmark tail ends with the shared mcpqual step.
	tail := NativeBenchSteps()
	if len(tail) != 6 || strings.Join(tail[4].Argv, " ") != strings.Join(MCPQualBenchStep().Argv, " ") || tail[4].Name != "bench mcpqual" ||
		strings.Join(tail[5].Argv, " ") != strings.Join(RealE2EDevBenchStep().Argv, " ") {
		t.Fatalf("native tail %+v", tail)
	}
}

// TestNativeDecoderEnrollmentB1Evidence (design decoder-enrollment B1):
// the four function parents of FP-10..FP-13 are required by exact name, in
// FP order, after the 412 earlier names, unchanged and first (416 then; the
// B1.5, B2 and B3 groups follow them, 429 in all), a bounded segment 412..416; a
// missing, failed or skipped parent fails naming it, and a parent passing
// in another package never satisfies it. Native ordinary calls, benchmark
// steps and the native benchmark tail are unchanged.
func TestNativeDecoderEnrollmentB1Evidence(t *testing.T) {
	q := qualification()
	if err := check(stream(q...)); err != nil {
		t.Fatalf("complete evidence: %v", err)
	}
	req := NativeRequiredTests()
	want := []string{"TestMCPCaptureProtocolNegotiation", "TestMCPCaptureCodexApproval", "TestMCPCaptureGrokRecipe", "TestMCPCaptureCursorTrust"}
	if len(req) != 436 || !slices.Equal(req[419:423], want) || !slices.Equal(b1Names(), want) || strings.Join(req[410:419], ",") != strings.Join(dceNames(), ",") {
		t.Fatalf("%d names; segment %v", len(req), req[419:423])
	}
	for _, name := range want {
		mustFail(t, "missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event", unobserved)
		mustFail(t, "no pass "+name, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "skipped "+name, stream(replacing(q, "pass", name, ev("skip", NativePackage, name))...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		mustFail(t, "failed "+name, stream(replacing(q, "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
		if err := check(stream(without(without(q, "run", name), "pass", name)...)); strings.Count(err.Error(), " has no ") != 1 {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	other := "github.com/wedevwork/callsheet/internal/mcpqual"
	moved := append(without(without(q, "run", "TestMCPCaptureGrokRecipe"), "pass", "TestMCPCaptureGrokRecipe"),
		ev("start", other, ""), ev("run", other, "TestMCPCaptureGrokRecipe"), ev("pass", other, "TestMCPCaptureGrokRecipe"), ev("pass", other, ""))
	mustFail(t, "moved", stream(moved...), "TestMCPCaptureGrokRecipe has no run event")
	if _, err := NativeSteps("darwin"); err != nil || len(BenchSteps()) != 14 || len(NativeBenchSteps()) != 6 {
		t.Fatalf("native steps %v, %d bench steps", err, len(BenchSteps()))
	}
}

// TestNativeDecoderEnrollmentB15Evidence (design decoder-enrollment B1.5):
// the three function parents of FP-14..FP-16 are required by exact name,
// in FP order, after the 416 earlier names, unchanged and first (419 then;
// the B2 and B3 groups follow them, 429 in all), a bounded segment 416..419; a
// missing, failed or skipped parent fails naming it, and a parent passing
// in another package never satisfies it. Native ordinary calls, benchmark
// steps and the native benchmark tail are unchanged.
func TestNativeDecoderEnrollmentB15Evidence(t *testing.T) {
	q := qualification()
	if err := check(stream(q...)); err != nil {
		t.Fatalf("complete evidence: %v", err)
	}
	req := NativeRequiredTests()
	want := []string{"TestMCPProbeTerminalObservation", "TestMCPCaptureCursorProjectApproval", "TestMCPCaptureCursorInventoryPolicy"}
	if len(req) != 436 || !slices.Equal(req[423:426], want) || !slices.Equal(b15Names(), want) || !slices.Equal(req[419:423], b1Names()) ||
		strings.Join(req[410:419], ",") != strings.Join(dceNames(), ",") {
		t.Fatalf("%d names; segment %v", len(req), req[423:426])
	}
	for _, name := range want {
		mustFail(t, "missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event", unobserved)
		mustFail(t, "no pass "+name, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "skipped "+name, stream(replacing(q, "pass", name, ev("skip", NativePackage, name))...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		mustFail(t, "failed "+name, stream(replacing(q, "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
		if err := check(stream(without(without(q, "run", name), "pass", name)...)); strings.Count(err.Error(), " has no ") != 1 {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	other := "github.com/wedevwork/callsheet/internal/mcpqual"
	moved := append(without(without(q, "run", "TestMCPProbeTerminalObservation"), "pass", "TestMCPProbeTerminalObservation"),
		ev("start", other, ""), ev("run", other, "TestMCPProbeTerminalObservation"), ev("pass", other, "TestMCPProbeTerminalObservation"), ev("pass", other, ""))
	mustFail(t, "moved", stream(moved...), "TestMCPProbeTerminalObservation has no run event")
	if _, err := NativeSteps("darwin"); err != nil || len(BenchSteps()) != 14 || len(NativeBenchSteps()) != 6 {
		t.Fatalf("native steps %v, %d bench steps", err, len(BenchSteps()))
	}
}

// TestNativeDecoderEnrollmentB2Evidence (design decoder-enrollment B2):
// the five function parents of FP-17..FP-21 are required by exact name, in
// FP order, after the 419 earlier names, unchanged and first (424 then; the
// B3 group follows them, 429 in all), a bounded segment 419..424; a
// missing, failed or skipped parent fails
// naming it, and a parent passing in another package never satisfies it.
// Native ordinary calls, benchmark steps and the native benchmark tail are
// unchanged.
func TestNativeDecoderEnrollmentB2Evidence(t *testing.T) {
	q := qualification()
	if err := check(stream(q...)); err != nil {
		t.Fatalf("complete evidence: %v", err)
	}
	req := NativeRequiredTests()
	want := []string{"TestMCPRealDecoderMappings", "TestMCPFixtureSanitization", "TestMCPRealEnrollment", "TestMCPCaptureCursorToolPermission", "TestMCPRealEnrollmentConfirmation"}
	if len(req) != 436 || !slices.Equal(req[426:431], want) || !slices.Equal(b2Names(), want) || !slices.Equal(req[423:426], b15Names()) ||
		!slices.Equal(req[419:423], b1Names()) || strings.Join(req[410:419], ",") != strings.Join(dceNames(), ",") {
		t.Fatalf("%d names; segment %v", len(req), req[426:431])
	}
	for _, name := range want {
		mustFail(t, "missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event", unobserved)
		mustFail(t, "no pass "+name, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "skipped "+name, stream(replacing(q, "pass", name, ev("skip", NativePackage, name))...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		mustFail(t, "failed "+name, stream(replacing(q, "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
		if err := check(stream(without(without(q, "run", name), "pass", name)...)); strings.Count(err.Error(), " has no ") != 1 {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	// The mcpqual package's own real-decoder tests never stand in for the
	// function parent of the same FP.
	other := "github.com/wedevwork/callsheet/internal/mcpqual"
	moved := append(without(without(q, "run", "TestMCPRealDecoderMappings"), "pass", "TestMCPRealDecoderMappings"),
		ev("start", other, ""), ev("run", other, "TestMCPRealDecoderMappings"), ev("pass", other, "TestMCPRealDecoderMappings"), ev("pass", other, ""))
	mustFail(t, "moved", stream(moved...), "TestMCPRealDecoderMappings has no run event")
	if _, err := NativeSteps("darwin"); err != nil || len(BenchSteps()) != 14 || len(NativeBenchSteps()) != 6 {
		t.Fatalf("native steps %v, %d bench steps", err, len(BenchSteps()))
	}
}

// TestNativeDecoderEnrollmentB3Evidence (design decoder-enrollment B3):
// the five function parents of FP-22..FP-26 are required by exact name, in
// FP order, after the 424 earlier names, unchanged and first (429 in all,
// the new range 424..429); a missing, failed or skipped parent fails
// naming it, and a parent passing in another package never satisfies it.
// Native ordinary calls, benchmark steps and the native benchmark tail are
// unchanged.
func TestNativeDecoderEnrollmentB3Evidence(t *testing.T) {
	q := qualification()
	if err := check(stream(q...)); err != nil {
		t.Fatalf("complete evidence: %v", err)
	}
	req := NativeRequiredTests()
	want := []string{"TestMCPCursorRealDecoder", "TestMCPCursorEnrollment", "TestMCPCursorQualifyPreparation", "TestMCPCursorWorkerResidue", "TestMCPCursorConfirmation"}
	if len(req) != 436 || !slices.Equal(req[431:436], want) || !slices.Equal(b3Names(), want) || !slices.Equal(req[426:431], b2Names()) ||
		!slices.Equal(req[423:426], b15Names()) || !slices.Equal(req[419:423], b1Names()) || strings.Join(req[410:419], ",") != strings.Join(dceNames(), ",") {
		t.Fatalf("%d names; suffix %v", len(req), req[431:])
	}
	for _, name := range want {
		mustFail(t, "missing "+name, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event", unobserved)
		mustFail(t, "no pass "+name, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "skipped "+name, stream(replacing(q, "pass", name, ev("skip", NativePackage, name))...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		mustFail(t, "failed "+name, stream(replacing(q, "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
		if err := check(stream(without(without(q, "run", name), "pass", name)...)); strings.Count(err.Error(), " has no ") != 1 {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	// The mcpqual package's own Cursor tests never stand in for the function
	// parent of the same FP.
	other := "github.com/wedevwork/callsheet/internal/mcpqual"
	moved := append(without(without(q, "run", "TestMCPCursorRealDecoder"), "pass", "TestMCPCursorRealDecoder"),
		ev("start", other, ""), ev("run", other, "TestMCPCursorRealDecoder"), ev("pass", other, "TestMCPCursorRealDecoder"), ev("pass", other, ""))
	mustFail(t, "moved", stream(moved...), "TestMCPCursorRealDecoder has no run event")
	if _, err := NativeSteps("darwin"); err != nil || len(BenchSteps()) != 14 || len(NativeBenchSteps()) != 6 {
		t.Fatalf("native steps %v, %d bench steps", err, len(BenchSteps()))
	}
}

// --- design 12a-native-reale2e-budget: the split native qualification ---

// nativeSelections are a native plan's package patterns, one list per
// step (every argument after "go test" that is not a flag).
func nativeSelections(plan []Step) [][]string {
	var out [][]string
	for _, s := range plan {
		var pats []string
		for _, a := range s.Argv[2:] {
			if !strings.HasPrefix(a, "-") {
				pats = append(pats, a)
			}
		}
		out = append(out, pats)
	}
	return out
}

// TestNativeSelectionCoversModule (FP-1) is the tagged counterpart of
// TestRaceSelectionCoversModule for Darwin's two native commands, executed
// by name from tests/function (TestCIDarwinQualification). Each command's
// actual patterns resolve separately with go list -tags=realadaptercheck
// (go list deduplicates within one invocation, never across commands):
// every package of go list -tags=realadaptercheck ./... is selected exactly
// once, nothing else is; internal/reale2e is absent from the 300-second
// command and the sole package of the 600-second one, and internal/sidecar
// (the tagged contract's package) is in the first command only. Discovery
// errors and warnings fail (goList). Selections are resolved, not compared
// as strings, so an enclosing pattern such as ./internal/... is caught. On
// Darwin this proves the actual target package set; on Linux it is an
// additional early check. Do not rename or skip.
func TestNativeSelectionCoversModule(t *testing.T) {
	const tag = "-tags=realadaptercheck"
	const sidecar = "github.com/wedevwork/callsheet/internal/sidecar"
	all := goList(t, tag, "./...")
	if !slices.Contains(all, reale2eLiteral) || !slices.Contains(all, sidecar) {
		t.Fatalf("go list %s ./... lacks reale2e or sidecar: %v", tag, all)
	}
	check := func(plan []Step) []string {
		sel := nativeSelections(plan)
		if len(sel) != 2 {
			return []string{fmt.Sprintf("%d native commands, want 2", len(sel))}
		}
		first, second := goList(t, append([]string{tag}, sel[0]...)...), goList(t, append([]string{tag}, sel[1]...)...)
		counts := map[string]int{}
		for _, p := range append(slices.Clone(first), second...) {
			counts[p]++
		}
		var bad []string
		for _, p := range all {
			if counts[p] != 1 {
				bad = append(bad, fmt.Sprintf("%s selected by %d native commands, want 1", p, counts[p]))
			}
			delete(counts, p)
		}
		for p, n := range counts {
			bad = append(bad, fmt.Sprintf("%s (outside ./...) selected by %d native commands", p, n))
		}
		if slices.Contains(first, reale2eLiteral) {
			bad = append(bad, "the 300s command selects "+reale2eLiteral)
		}
		if !slices.Equal(second, []string{reale2eLiteral}) {
			bad = append(bad, fmt.Sprintf("the 600s command selects %v, want %s alone", second, reale2eLiteral))
		}
		if !slices.Contains(first, sidecar) || slices.Contains(second, sidecar) {
			bad = append(bad, "the sidecar is not selected by the first command only")
		}
		slices.Sort(bad)
		return bad
	}
	plan, err := NativeSteps("darwin")
	if err != nil || len(plan) != 2 || !slices.Contains(plan[0].Argv, "-timeout=300s") || !slices.Contains(plan[1].Argv, "-timeout=600s") ||
		!slices.Contains(plan[0].Argv, tag) || !slices.Contains(plan[1].Argv, tag) {
		t.Fatalf("native plan %+v %v", plan, err)
	}
	if bad := check(plan); len(bad) != 0 {
		t.Errorf("the native selections do not cover the tagged module once each:\n%s", strings.Join(bad, "\n"))
	}
	// Planted negatives, each a copy of the real plan with one change.
	planted := func(i int, edit func([]string) []string) []Step {
		p, _ := NativeSteps("darwin")
		p[i].Argv = edit(p[i].Argv)
		return p
	}
	has := func(bad []string, want string) bool {
		return slices.ContainsFunc(bad, func(b string) bool { return strings.Contains(b, want) })
	}
	// A duplicate: the second command also selects a shared package.
	dup := check(planted(1, func(a []string) []string { return append(a, "./internal/mcp/...") }))
	if !has(dup, "/internal/mcp selected by 2 native commands") || !has(dup, "the 600s command selects") {
		t.Errorf("a planted duplicate: %v", dup)
	}
	// An omission: the first command loses one pattern.
	omit := check(planted(0, func(a []string) []string {
		return slices.DeleteFunc(a, func(s string) bool { return s == "./internal/workspace/..." })
	}))
	if len(omit) != 1 || !has(omit, "/internal/workspace selected by 0 native commands") {
		t.Errorf("a planted omission: %v", omit)
	}
	// A wrong 300s selection: the old ./..., or an enclosing pattern that
	// only resolution reveals, puts internal/reale2e under 300 s.
	for name, pats := range map[string][]string{"./...": {"./..."}, "./internal/...": {"./cmd/...", "./internal/...", "./tests/..."}} {
		wrong := check(planted(0, func(a []string) []string { return append(slices.Clone(a[:6]), pats...) }))
		if !has(wrong, "/internal/reale2e selected by 2 native commands") || !has(wrong, "the 300s command selects "+reale2eLiteral) {
			t.Errorf("a planted %s first command: %v", name, wrong)
		}
	}
	// The sidecar moved to the second command.
	moved := check(planted(1, func(a []string) []string { return append(a, "./internal/sidecar") }))
	if !has(moved, "the sidecar is not selected by the first command only") {
		t.Errorf("a planted second sidecar: %v", moved)
	}
	if bad := check(planted(1, func(a []string) []string { return a })[:1]); len(bad) != 1 || bad[0] != "1 native commands, want 2" {
		t.Errorf("a one-command plan: %v", bad)
	}
}

// TestNativeReale2eEvidence (FP-2): the combined stream of the two native
// commands qualifies with internal/reale2e's sole start, its tests and its
// package pass after a valid first stream; a missing, failed, skipped or
// truncated reale2e stream, any package started by both commands (the
// sidecar, tests/function or reale2e itself) and missing existing function
// or sidecar evidence each fail.
func TestNativeReale2eEvidence(t *testing.T) {
	if NativeReale2ePackage != "github.com/wedevwork/callsheet/internal/reale2e" || reale2eLiteral != NativeReale2ePackage {
		t.Fatalf("NativeReale2ePackage = %q", NativeReale2ePackage)
	}
	r, sc := reale2eLiteral, NativeTaskProcessPackage
	shared, own := sharedQualification(), reale2eEvents()
	first := stream(shared...)
	if err := check(first + stream(own...)); err != nil {
		t.Fatalf("a valid first stream followed by the reale2e stream: %v", err)
	}
	mustFail(t, "missing reale2e", first, "package "+r+" has no start event", unobserved)
	mustFail(t, "reale2e only", stream(own...), "package "+NativePackage+" has no start event", unobserved)
	mustFail(t, "unfinished reale2e", first+stream(own[:3]...), "package "+r+" did not finish")
	full := first + stream(own...)
	mustFail(t, "torn reale2e", full[:len(full)-9], "malformed event stream after")
	mustFail(t, "failed reale2e test", first+stream(replacing(own, "pass", reale2eTest, ev("fail", r, reale2eTest))...), "test "+reale2eTest+" in "+r+" failed")
	mustFail(t, "skipped reale2e test", first+stream(replacing(own, "pass", reale2eTest, ev("skip", r, reale2eTest))...), "test "+reale2eTest+" in "+r+" skipped: "+unobserved)
	mustFail(t, "failed reale2e package", first+stream(append(slices.Clone(own[:4]), ev("fail", r, ""))...), "package "+r+" failed")
	mustFail(t, "skipped reale2e package", first+stream(ev("start", r, ""), ev("output", r, "").with("Output", "?   \t"+r+"\t[no test files]\n"), ev("skip", r, "")),
		"package "+r+" has no pass event", unobserved)
	mustFail(t, "build-failed reale2e", first+stream(evt{"ImportPath": r + " [" + r + ".test]", "Action": "build-fail"}), "build failed for "+r)
	// Duplicate starts across the two commands.
	mustFail(t, "reale2e in both commands", stream(append(slices.Clone(shared), own...)...)+stream(own...), "package "+r+" started twice")
	mustFail(t, "sidecar in both commands", first+stream(append(slices.Clone(own), ev("start", sc, ""), ev("run", sc, realLocal[0]), ev("pass", sc, realLocal[0]), ev("pass", sc, ""))...),
		"package "+sc+" started twice")
	mustFail(t, "function package in both commands", first+stream(append([]evt{ev("start", NativePackage, "")}, own...)...), "package "+NativePackage+" started twice")
	// The existing evidence stays required beside a passing reale2e.
	mustFail(t, "function evidence missing", stream(without(without(shared, "run", fp6), "pass", fp6)...)+stream(own...), fp6+" has no run event", unobserved)
	mustFail(t, "sidecar evidence missing", stream(without(without(shared, "run", realLocal[0]), "pass", realLocal[0])...)+stream(own...),
		realLocal[0]+" in "+sc+" has no run event", unobserved)
	// A reale2e result under another package name never satisfies it.
	var renamed []evt
	for _, e := range own {
		c := evt{}
		for k, v := range e {
			c[k] = v
		}
		c["Package"] = r + "other"
		renamed = append(renamed, c)
	}
	mustFail(t, "renamed reale2e", first+stream(renamed...), "package "+r+" has no start event")
	// Only the reale2e package is reported missing.
	if err := check(first); strings.Count(err.Error(), " has no ") != 1 {
		t.Fatalf("missing reale2e: %v", err)
	}
}

// TestNativeSplitDriverContract (FP-4) is executed by name from
// tests/function (TestCINativeSplitExecution). Through the injected Darwin
// driver (fake runner, no real native suite) on any host: the two native
// children run in order and the first child's failure stops before the
// second; the second child's failure, a malformed second stream or a
// combined stream without reale2e's pass each stop before qualification
// succeeds, with the scratch logs retained and no coverage or benchmark
// child; success prints the unchanged qualification line once, after both
// children and before the tail, in eleven runner calls. Do not rename or
// skip.
func TestNativeSplitDriverContract(t *testing.T) {
	const passed = "devcheck: native qualification passed on "
	noTail := func(t *testing.T, f *fakeRunner, n int, out string) {
		t.Helper()
		if len(f.calls) != n || strings.Contains(out, passed) || strings.Contains(out, "devcheck: coverage: ") || strings.Contains(out, "devcheck: bench ") ||
			strings.Contains(out, "stage native ok") {
			t.Fatalf("%d calls %v, want %d and no tail:\n%s", len(f.calls), f.argvs(), n, out)
		}
		for i, c := range f.calls {
			if want := []string{wantNativeShared, wantNativeReale2e}[i]; strings.Join(c.argv, " ") != want {
				t.Fatalf("call %d = %v, want %s", i, c.argv, want)
			}
		}
	}
	retained := func(t *testing.T, out, errOut, wantEvents string) {
		t.Helper()
		scratch := scratchFrom(out)
		defer os.RemoveAll(scratch)
		events, err := os.ReadFile(filepath.Join(scratch, "native-events.jsonl"))
		if scratch == "" || err != nil || string(events) != wantEvents || !strings.Contains(errOut, "logs retained in "+scratch) {
			t.Fatalf("scratch %q not retained with the streams so far (%v): %q\n%s", scratch, err, events, errOut)
		}
		if _, err := os.Stat(filepath.Join(scratch, "native-stderr.log")); err != nil {
			t.Fatalf("stderr log not retained: %v", err)
		}
	}
	t.Run("first-child-failure", func(t *testing.T) {
		f := nativeRunner(qualification())
		f.fail = "-timeout=300s"
		code, out, errOut := runDriver(t, "darwin", f, "native")
		if code != 1 || !strings.Contains(errOut, "stage native FAILED") || !strings.Contains(errOut, "native failed: "+wantNativeShared+": exit status 1") ||
			!strings.Contains(errOut, "boom from child") || strings.Contains(out, "devcheck: native reale2e: ") {
			t.Fatalf("first child failure = %d\n%s\n%s", code, out, errOut)
		}
		noTail(t, f, 1, out)
		retained(t, out, errOut, "")
	})
	t.Run("second-child-failure", func(t *testing.T) {
		f := nativeRunner(qualification())
		f.fail = "-timeout=600s"
		code, out, errOut := runDriver(t, "darwin", f, "native")
		if code != 1 || !strings.Contains(errOut, "stage native FAILED") || !strings.Contains(errOut, "native reale2e failed: "+wantNativeReale2e+": exit status 1") ||
			!strings.Contains(errOut, "boom from child") || !strings.Contains(out, "devcheck: native reale2e: "+wantNativeReale2e+"\n") {
			t.Fatalf("second child failure = %d\n%s\n%s", code, out, errOut)
		}
		noTail(t, f, 2, out)
		retained(t, out, errOut, stream(sharedQualification()...))
	})
	t.Run("malformed-second-stream", func(t *testing.T) {
		f := nativeRunner(qualification())
		f.nativeReale2e = f.nativeReale2e[:len(f.nativeReale2e)-9]
		code, out, errOut := runDriver(t, "darwin", f, "native")
		if code != 1 || !strings.Contains(errOut, "malformed event stream after") || !strings.Contains(errOut, "native-events.jsonl") {
			t.Fatalf("malformed second stream = %d\n%s", code, errOut)
		}
		noTail(t, f, 2, out)
		retained(t, out, errOut, f.native+f.nativeReale2e)
	})
	t.Run("tail-suppression", func(t *testing.T) {
		// Both children exit zero, but the combined stream lacks reale2e's
		// start or its package pass: no qualification, no tail.
		for name, c := range map[string]struct{ second, want string }{
			"absent":  {"", "package " + reale2eLiteral + " has no start event"},
			"skipped": {stream(ev("start", reale2eLiteral, ""), ev("skip", reale2eLiteral, "")), "package " + reale2eLiteral + " has no pass event"},
		} {
			f := nativeRunner(sharedQualification())
			f.nativeReale2e = c.second
			code, out, errOut := runDriver(t, "darwin", f, "native")
			if code != 1 || !strings.Contains(errOut, c.want) || !strings.Contains(errOut, unobserved) {
				t.Fatalf("%s = %d\n%s", name, code, errOut)
			}
			noTail(t, f, 2, out)
			retained(t, out, errOut, f.native+c.second)
		}
	})
	t.Run("success", func(t *testing.T) {
		f := nativeRunner(qualification())
		code, out, errOut := runDriver(t, "darwin", f, "native")
		os.RemoveAll(scratchFrom(out))
		// Two qualification children, the three coverage children (the
		// profile run, its report and the cmd listing, scratch paths
		// varying), then the six native benchmarks in their order.
		want := []string{wantNativeShared, wantNativeReale2e, "go test -count=1 -tags=realadaptercheck -covermode=atomic -coverpkg=./internal/...,./cmd/... -coverprofile=",
			"go tool cover -func=", "go list -f {{.ImportPath}}|{{len .GoFiles}} ./cmd/...",
			wantBenchWorkspace, wantBenchTransfer, wantBenchTaskWorkspace, wantBenchClientWait, wantBenchMCPQual, wantBenchDevFull}
		a := f.argvs()
		if code != 0 || len(a) != len(want) || len(want) != 11 {
			t.Fatalf("success = %d %v\n%s", code, a, errOut)
		}
		for i, w := range want {
			if prefix := i == 2 || i == 3; (prefix && !strings.HasPrefix(a[i], w)) || (!prefix && a[i] != w) {
				t.Fatalf("call %d = %q, want %q", i, a[i], w)
			}
		}
		// The unchanged success line, once, between the two children and
		// the tail.
		line := "devcheck: native qualification passed on darwin/" + runtime.GOARCH + ": github.com/wedevwork/callsheet/tests/function: " + strings.Join(NativeRequiredTests(), ", ") + "\n"
		at := []int{strings.Index(out, "devcheck: native: "+wantNativeShared+"\n"), strings.Index(out, "devcheck: native reale2e: "+wantNativeReale2e+"\n"),
			strings.Index(out, line), strings.Index(out, "devcheck: coverage: "), strings.Index(out, "devcheck: bench reale2e dev-full: "), strings.Index(out, "stage native ok")}
		if strings.Count(out, line) != 1 || strings.Count(out, passed) != 1 || !slices.IsSorted(at) || at[0] < 0 || slices.Contains(at, -1) {
			t.Fatalf("output order %v:\n%s", at, out)
		}
	})
}
