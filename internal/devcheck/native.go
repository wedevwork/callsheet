package devcheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// NativePackage is the package whose process-group, plane trust and node
// tests qualify a native macOS run.
const NativePackage = "github.com/wedevwork/callsheet/tests/function"

// nativeRequired are the tests in NativePackage that must both run and
// pass: the process-group scenarios (iteration 01), every plane trust
// function test with the mandatory subtests of its compound FPs
// (iteration 02), including the "process" and "contracts" boundaries that
// separate process-boundary scenarios from delegated contracts
// (iteration 02b), every node function test with its mandatory subtests
// (iteration 03), every role function test with its mandatory subtests
// (iteration 04), every task function test with its mandatory subtests
// (iteration 05), the control function tests and native groups
// (iteration 06a), the MCP function tests with their mandatory subtests
// (iteration 07a), the coordinator setup and timeout qualification
// function tests with their mandatory subtests (iteration 07b), the
// workspace hub function tests (iteration 09a), the local transfer
// function tests (iteration 09b) and every later group below, the last the
// decoder-enrollment function parents.
var nativeRequired = []string{
	"TestFP6ProcessGroups",
	"TestFP6ProcessGroups/cooperative",
	"TestFP6ProcessGroups/resistant",
	"TestFP6ProcessGroups/leader-exits-first",
	"TestPlaneCommands",
	"TestPlaneState",
	"TestPlaneState/paths",
	"TestPlaneState/persistence",
	"TestPlaneState/locking",
	"TestPlaneState/validation",
	"TestPlaneState/contracts",
	"TestPlaneBind",
	"TestPlaneInit",
	"TestPlaneInit/issuance",
	"TestPlaneInit/fingerprint",
	"TestPlaneInit/restart-invariance",
	"TestPlaneTLS",
	"TestPlaneTLS/https-only",
	"TestPlaneTLS/prelisten-validation",
	"TestPlaneTLS/bounded-shutdown",
	"TestPlaneTLS/contracts",
	"TestPlaneReissue",
	"TestPlaneReissue/process",
	"TestPlaneReissue/contracts",
	"TestPlaneStatus",
	"TestPlaneStatus/inspection",
	"TestPlaneStatus/expiry-warnings",
	"TestPlanePlatform",
	// Iteration 03 (nodes): one function test per FP with its mandatory
	// subtests, delegated clock contracts included.
	"TestNodeTrust",
	"TestNodeTrust/ca",
	"TestNodeTrust/pin",
	"TestNodeTrust/rejections",
	"TestNodeEnrollment",
	"TestNodeEnrollment/identity",
	"TestNodeEnrollment/recovery",
	"TestNodeEnrollment/locking",
	"TestNodeProtocol",
	"TestNodeProtocol/hello",
	"TestNodeProtocol/limits",
	"TestNodeReconnect",
	"TestNodeReconnect/restart",
	"TestNodeReconnect/disconnect",
	"TestNodeReconnect/shutdown",
	"TestNodeLease",
	"TestNodeLease/expiry",
	"TestNodeLease/return",
	"TestNodeRegistry",
	"TestNodeRegistry/restore",
	"TestNodeRegistry/validation",
	"TestNodeDiscovery",
	"TestNodeDiscovery/text",
	"TestNodeDiscovery/json",
	"TestNodeDiscovery/errors",
	"TestNodePlatform",
	"TestNodePlatform/paths",
	"TestNodePlatform/native-state",
	"TestNodePlatform/policy",
	"TestNodePlatform/sticky-write",
	// Iteration 04 (roles): one function test per FP with its mandatory
	// subtests, delegated package contracts included.
	"TestRoleConfiguration",
	"TestRoleConfiguration/fields",
	"TestRoleConfiguration/order",
	"TestRoleAdapter",
	"TestRoleAdapter/disabled",
	"TestRoleAdapter/probe",
	"TestRoleValidation",
	"TestRoleValidation/remote",
	"TestRoleValidation/rejections",
	"TestRoleProtocol",
	"TestRoleProtocol/duplex",
	"TestRoleProtocol/bounds",
	"TestRoleReadiness",
	"TestRoleReadiness/changes",
	"TestRoleReadiness/reconnect",
	"TestRolePersistence",
	"TestRolePersistence/restore",
	"TestRolePersistence/failures",
	"TestRoleCommands",
	"TestRoleCommands/text",
	"TestRoleCommands/json",
	"TestRoleCommands/trust",
	"TestRoleMutation",
	"TestRoleMutation/races",
	"TestRoleMutation/remove",
	"TestRolePlatform",
	"TestRolePlatform/manuals",
	"TestRolePlatform/executable",
	"TestRolePlatform/policy",
	// Iteration 05 (tasks): one function test per FP with its mandatory
	// subtests, delegated package contracts included.
	"TestTaskModel",
	"TestTaskModel/envelope",
	"TestTaskModel/states",
	"TestTaskDispatch",
	"TestTaskDispatch/selection",
	"TestTaskDispatch/gate-race",
	"TestTaskDispatch/reserved-slot",
	"TestTaskProtocol",
	"TestTaskProtocol/duplex",
	"TestTaskProtocol/bounds",
	"TestTaskProtocol/result-receipt",
	"TestTaskProtocol/result-ack-loss",
	"TestTaskExecution",
	"TestTaskExecution/compose",
	"TestTaskExecution/invoke",
	"TestTaskExecution/exit",
	"TestTaskExecution/process",
	"TestTaskExecution/platform",
	"TestTaskExecution/policy",
	"TestTaskLogs",
	"TestTaskLogs/retention",
	"TestTaskLogs/backpressure",
	"TestTaskLogs/final",
	"TestTaskPersistence",
	"TestTaskPersistence/durability",
	"TestTaskPersistence/restore",
	"TestTaskCommands",
	"TestTaskCommands/dispatch",
	"TestTaskCommands/ls",
	"TestTaskCommands/show",
	"TestTaskCommands/logs",
	"TestTaskCommands/trust",
	"TestTaskRoles",
	"TestTaskRoles/counts",
	"TestTaskRoles/mutation",
	"TestTaskRoles/remaining-capacity",
	"TestTaskRoles/recovery-remove",
	"TestTaskRecoveryBoundary",
	"TestTaskRecoveryBoundary/disconnect",
	"TestTaskRecoveryBoundary/remaining-capacity",
	"TestTaskRecoveryBoundary/recovery-remove",
	// Iteration 06a (resilient execution): the eight control function
	// parents (one per FP, each a conjunction of its delegated package
	// contracts) and the direct real-binary native group qualification.
	"TestControlDurability",
	"TestControlReconnect",
	"TestControlNodeLoss",
	"TestControlLaunchSafety",
	"TestControlWorkerRecovery",
	"TestControlPlaneRecovery",
	"TestControlLateResult",
	"TestControlLegacy",
	"TestControlNativeGroups",
	"TestControlNativeGroups/cooperative",
	"TestControlNativeGroups/resistant",
	"TestControlNativeGroups/orphan-restart",
	"TestControlNativeGroups/plane-restart",
	// Iteration 06b (task controls): the four control function parents,
	// one per FP, each the conjunction of its delegated package contracts
	// with their mandatory subcases.
	"TestControlCancellation",
	"TestControlExecutionTimeout",
	"TestControlBoundedWait",
	"TestControlForceRemove",
	// Iteration 07a (coordinator door): the eight MCP function parents in
	// FP order, each followed by its mandatory direct children in listed
	// order; every name needs its own run and pass event.
	"TestMCPProtocol",
	"TestMCPProtocol/initialize",
	"TestMCPProtocol/version",
	"TestMCPProtocol/discovery",
	"TestMCPProtocol/framing",
	"TestMCPProtocol/concurrency",
	"TestMCPProtocol/cancellation",
	"TestMCPRelay",
	"TestMCPRelay/ca",
	"TestMCPRelay/pin",
	"TestMCPRelay/trust-errors",
	"TestMCPRelay/protocol-mismatch",
	"TestMCPRelay/contract-errors",
	"TestMCPRelay/no-cache",
	"TestMCPRelay/recovery",
	"TestMCPNodes",
	"TestMCPNodes/node-ls",
	"TestMCPNodes/node-show",
	"TestMCPNodes/shared-roster",
	"TestMCPRoles",
	"TestMCPRoles/role-add",
	"TestMCPRoles/role-set",
	"TestMCPRoles/role-ls",
	"TestMCPRoles/role-show",
	"TestMCPRoles/role-rm",
	"TestMCPRoles/global-slots",
	"TestMCPRoles/force-pending",
	"TestMCPRoles/force-completed",
	"TestMCPRoles/operation-rejoin",
	"TestMCPRoles/instance-reuse",
	"TestMCPDispatch",
	"TestMCPDispatch/target-id",
	"TestMCPDispatch/target-name",
	"TestMCPDispatch/overrides",
	"TestMCPDispatch/async",
	"TestMCPDispatch/attribution",
	"TestMCPDispatch/invalid-attribution",
	"TestMCPDispatch/last-slot",
	"TestMCPDispatch/wait-fast",
	"TestMCPDispatch/wait-slow",
	"TestMCPDispatch/lost-response",
	"TestMCPTaskReads",
	"TestMCPTaskReads/task-ls",
	"TestMCPTaskReads/task-show",
	"TestMCPTaskReads/task-logs",
	"TestMCPTaskReads/pagination",
	"TestMCPTaskReads/tails",
	"TestMCPTaskReads/binary-logs",
	"TestMCPTaskReads/late-logs",
	"TestMCPTaskReads/late-not-found",
	"TestMCPTaskReads/output-bounds",
	"TestMCPWaitCancel",
	"TestMCPWaitCancel/cancel-accepted",
	"TestMCPWaitCancel/cancel-terminal",
	"TestMCPWaitCancel/cancel-errors",
	"TestMCPWaitCancel/wait-one",
	"TestMCPWaitCancel/wait-many",
	"TestMCPWaitCancel/any-terminal",
	"TestMCPWaitCancel/snapshot",
	"TestMCPWaitCancel/interim-default",
	"TestMCPWaitCancel/budget-deadline",
	"TestMCPWaitCancel/delivery-deadline",
	"TestMCPWaitCancel/plane-cap",
	"TestMCPWaitCancel/restart-budget",
	"TestMCPWaitCancel/own-deadline",
	"TestMCPWaitCancel/cancel-wait-only",
	"TestMCPWaitCancel/catalog-interim",
	"TestMCPLifetime",
	"TestMCPLifetime/eof",
	"TestMCPLifetime/sigterm",
	"TestMCPLifetime/sigint",
	"TestMCPLifetime/closed-stdout",
	"TestMCPLifetime/stalled-reader",
	"TestMCPLifetime/slow-reader-max-logs",
	"TestMCPLifetime/outstanding-wait",
	"TestMCPLifetime/task-survives",
	"TestMCPLifetime/reaping",
	// Iteration 07b (coordinator setup and timeout qualification): the
	// eight function parents in FP order (FP-9..FP-16), each followed by
	// its mandatory direct children in listed order; every name needs its
	// own run and pass event.
	"TestMCPSetup",
	"TestMCPSetup/claude",
	"TestMCPSetup/codex",
	"TestMCPSetup/grok",
	"TestMCPSetup/cursor",
	"TestMCPSetup/runbook-ownership",
	"TestMCPSetup/client-info",
	"TestMCPQualificationProbe",
	"TestMCPQualificationProbe/immediate",
	"TestMCPQualificationProbe/slow",
	"TestMCPQualificationProbe/progress",
	"TestMCPQualificationProbe/no-token",
	"TestMCPQualificationProbe/cancellation",
	"TestMCPQualificationSchema",
	"TestMCPQualificationSchema/plan",
	"TestMCPQualificationSchema/report",
	"TestMCPQualificationSchema/limits",
	"TestMCPQualificationSchema/paths",
	"TestMCPQualificationDecoders",
	"TestMCPQualificationDecoders/claude",
	"TestMCPQualificationDecoders/codex",
	"TestMCPQualificationDecoders/grok",
	"TestMCPQualificationDecoders/cursor",
	"TestMCPQualificationDecoders/unknown-version",
	"TestMCPQualificationDecoders/non-tool-error",
	"TestMCPQualificationMeasurements",
	"TestMCPQualificationMeasurements/default",
	"TestMCPQualificationMeasurements/override",
	"TestMCPQualificationMeasurements/progress",
	"TestMCPQualificationMeasurements/absolute",
	"TestMCPQualificationMeasurements/lower-bound",
	"TestMCPQualificationMeasurements/partial",
	"TestMCPQualificationMeasurements/budget",
	"TestMCPQualificationPublish",
	"TestMCPQualificationPublish/verified",
	"TestMCPQualificationPublish/partial",
	"TestMCPQualificationPublish/redaction",
	"TestMCPQualificationPublish/hashes",
	"TestMCPQualificationPublish/refuse-conflict",
	"TestMCPQualificationPublish/worker-facts",
	"TestMCPQualificationReaping",
	"TestMCPQualificationReaping/cooperative",
	"TestMCPQualificationReaping/resistant",
	"TestMCPQualificationReaping/parent-exits-first",
	"TestMCPQualificationReaping/interrupt",
	"TestMCPQualificationReaping/cleanup-failure",
	"TestMCPQualificationInvocation",
	"TestMCPQualificationInvocation/denied",
	"TestMCPQualificationInvocation/allowed",
	"TestMCPQualificationInvocation/model-free",
	"TestMCPQualificationInvocation/ci-denied",
	// Iteration 08 (real adapters): the nine function parents in FP order
	// (FP-1..FP-9), each followed by its mandatory direct children in listed
	// order; every name needs its own run and pass event.
	"TestRealAdapterRegistration",
	"TestRealAdapterRegistration/registry",
	"TestRealAdapterRegistration/paths",
	"TestRealAdapterRegistration/selection",
	"TestRealAdapterProbe",
	"TestRealAdapterProbe/claude",
	"TestRealAdapterProbe/codex",
	"TestRealAdapterProbe/refusal",
	"TestRealAdapterInvocation",
	"TestRealAdapterInvocation/claude",
	"TestRealAdapterInvocation/codex",
	"TestRealAdapterInvocation/stdin",
	"TestRealAdapterClaudeFinal",
	"TestRealAdapterClaudeFinal/success",
	"TestRealAdapterClaudeFinal/failure",
	"TestRealAdapterClaudeFinal/malformed",
	"TestRealAdapterCodexFinal",
	"TestRealAdapterCodexFinal/success",
	"TestRealAdapterCodexFinal/absent",
	"TestRealAdapterCodexFinal/unsafe",
	"TestRealAdapterCodexFinal/cleanup",
	"TestRealAdapterOutcomes",
	"TestRealAdapterOutcomes/exits",
	"TestRealAdapterOutcomes/denials",
	"TestRealAdapterOutcomes/controls",
	"TestRealAdapterOutcomes/replay",
	"TestRealAdapterCatalog",
	"TestRealAdapterCatalog/recipes",
	"TestRealAdapterCatalog/evidence",
	"TestRealAdapterCatalog/ownership",
	"TestRealAdapterDispatch",
	"TestRealAdapterDispatch/claude",
	"TestRealAdapterDispatch/codex",
	"TestRealAdapterDispatch/no-vendors",
	"TestRealAdapterSmokeGate",
	"TestRealAdapterSmokeGate/default-off",
	"TestRealAdapterSmokeGate/ci-off",
	"TestRealAdapterSmokeGate/absent",
	"TestRealAdapterSmokeGate/enabled",
	// Iteration 09a (workspace hub): the ten function tests in FP order
	// (FP-1..FP-10) and the D1 maximum-path subcase of FP-7; absence or a
	// skip never satisfies native qualification.
	"TestWorkspaceCreate",
	"TestWorkspaceList",
	"TestWorkspaceShow",
	"TestWorkspaceRemove",
	"TestWorkspacePrune",
	"TestWorkspaceTransport",
	"TestWorkspaceRefSet",
	"TestWorkspaceRefSet/max-path",
	"TestWorkspaceStatus",
	"TestWorkspaceDiff",
	"TestWorkspaceNoGit",
	// Iteration 09b (workspace local transfers): the ten function tests in
	// FP order (FP-1..FP-10); absence or a skip never satisfies native
	// qualification.
	"TestWorkspacePushGit",
	"TestWorkspacePushCleanliness",
	"TestWorkspacePushFolder",
	"TestWorkspaceTransferIgnores",
	"TestWorkspacePullGit",
	"TestWorkspacePullFolder",
	"TestWorkspaceTransferCLI",
	"TestWorkspaceTransferMCP",
	"TestWorkspaceTransferNoGit",
	"TestWorkspaceTransferEligibility",
	// Iteration 10a (prompt readiness and group completion): the two
	// function tests in FP order (FP-1, FP-2), a separate group after the
	// 09b names; their scenarios assert within each parent, and absence or
	// a skip never satisfies native qualification.
	"TestTaskPromptReadiness",
	"TestTaskFastGroupCleanup",
	// Iteration 10b (workspace execution): the nine function tests in FP
	// order (FP-1..FP-9), a separate group after the 10a names, each named
	// acceptance scenario right after its parent (AC-WS-1 under the
	// commit, AC-WS-5 under the publication, AC-WS-2 under the isolation
	// test); absence or a skip never satisfies native qualification.
	"TestTaskWorkspaceAdmission",
	"TestTaskWorkspaceAccess",
	"TestTaskWorkspaceCache",
	"TestTaskWorkspaceCheckout",
	"TestTaskWorkspaceCommit",
	"TestTaskWorkspaceCommit/AC-WS-1",
	"TestTaskWorkspacePublication",
	"TestTaskWorkspacePublication/AC-WS-5",
	"TestTaskWorkspaceMetadata",
	"TestTaskWorkspaceRecovery",
	"TestTaskWorkspaceIsolation",
	"TestTaskWorkspaceIsolation/AC-WS-2",
	// Iteration 10c (coordinator delivery): the six function tests in FP
	// order (FP-1..FP-6), a separate group after the 10b names; their named
	// subtests assert within each parent and are not inventory entries.
	// Absence or a skip never satisfies native qualification.
	"TestWorkspaceDispatchDoors",
	"TestWorkspaceTaskPull",
	"TestWorkspaceTaskInspect",
	"TestWorkspaceTaskMCP",
	"TestWorkspaceMultiHop",
	"TestWorkspaceOperatorWorkflow",
	// Iteration 11 (real adapters, wave 2: Grok and Cursor): the nine
	// function parents in FP order (FP-1..FP-9), a separate group after the
	// 10c names, each followed by its mandatory direct subtests in the
	// design's order; absence or a skip never satisfies native
	// qualification.
	"TestWave2Registration",
	"TestWave2Registration/registry",
	"TestWave2Registration/paths",
	"TestWave2Registration/selection",
	"TestWave2Registration/posture",
	"TestWave2Probe",
	"TestWave2Probe/grok",
	"TestWave2Probe/cursor",
	"TestWave2Probe/refusal",
	"TestWave2Invocation",
	"TestWave2Invocation/grok",
	"TestWave2Invocation/cursor-refused",
	"TestWave2Invocation/prompt",
	"TestWave2GrokFinal",
	"TestWave2GrokFinal/success",
	"TestWave2GrokFinal/error",
	"TestWave2GrokFinal/cancelled",
	"TestWave2GrokFinal/malformed",
	"TestWave2CursorFinal",
	"TestWave2CursorFinal/success",
	"TestWave2CursorFinal/absent",
	"TestWave2CursorFinal/malformed",
	"TestWave2CursorFinal/blocked",
	"TestWave2Outcomes",
	"TestWave2Outcomes/exits",
	"TestWave2Outcomes/controls",
	"TestWave2Outcomes/refusal",
	"TestWave2Outcomes/retry",
	"TestWave2Catalog",
	"TestWave2Catalog/recipes",
	"TestWave2Catalog/evidence",
	"TestWave2Catalog/ownership",
	"TestWave2Dispatch",
	"TestWave2Dispatch/grok",
	"TestWave2Dispatch/cursor-refused",
	"TestWave2Dispatch/no-vendors",
	"TestWave2SmokeGate",
	"TestWave2SmokeGate/default-off",
	"TestWave2SmokeGate/ci-off",
	"TestWave2SmokeGate/absent",
	"TestWave2SmokeGate/enabled",
	"TestWave2SmokeGate/posture",
	// Non-blocking coordinator waits: the eight function parents in FP
	// order (FP-1..FP-8), a separate group after the iteration 11 names;
	// their scenarios assert within each parent and are not inventory
	// entries. Absence or a skip never satisfies native qualification.
	"TestWaitUntilDoneCLI",
	"TestWaitUntilDoneRenewal",
	"TestWaitUntilDoneFailure",
	"TestWaitUntilDoneOutput",
	"TestCoordinatorBackgroundWait",
	"TestMCPShortPollGuidance",
	"TestShortPollCatalogPolicy",
	"TestMCPShortConfirmation",
	// Decoder enrollment (slice A): the nine function parents in FP order
	// (FP-1..FP-9), a separate group after the non-blocking waits names;
	// each asserts its literal case inventory within the parent (no
	// inventory entries). Absence or a skip never satisfies native
	// qualification.
	"TestMCPCaptureInvocation",
	"TestMCPCaptureRecipes",
	"TestMCPCaptureEvidence",
	"TestMCPCaptureLifecycle",
	"TestMCPEnrollmentContract",
	"TestMCPEnrollmentReplay",
	"TestMCPEnrolledShortConfirmation",
	"TestMCPCaptureRunbook",
	"TestMCPEnrollmentCIPolicy",
	// Decoder enrollment slice B1: the four function parents of FP-10..FP-13
	// in FP order, a separate group after the slice A names; each asserts
	// its literal case inventory within the parent (no inventory entries).
	// Absence or a skip never satisfies native qualification.
	"TestMCPCaptureProtocolNegotiation",
	"TestMCPCaptureCodexApproval",
	"TestMCPCaptureGrokRecipe",
	"TestMCPCaptureCursorTrust",
	// Decoder enrollment slice B1.5: the three function parents of
	// FP-14..FP-16 in FP order, a separate group after the B1 names; each
	// asserts its literal local case inventory within the parent (no
	// inventory entries, no subtest names). Absence or a skip never
	// satisfies native qualification.
	"TestMCPProbeTerminalObservation",
	"TestMCPCaptureCursorProjectApproval",
	"TestMCPCaptureCursorInventoryPolicy",
	// Decoder enrollment slice B2: the five function parents of
	// FP-17..FP-21 in FP order, a separate group after the B1.5 names; each
	// asserts its literal local case inventory within the parent (no
	// inventory entries, no subtest names). Absence or a skip never
	// satisfies native qualification.
	"TestMCPRealDecoderMappings",
	"TestMCPFixtureSanitization",
	"TestMCPRealEnrollment",
	"TestMCPCaptureCursorToolPermission",
	"TestMCPRealEnrollmentConfirmation",
	// Decoder enrollment slice B3: the five function parents of
	// FP-22..FP-26 in FP order, a separate group after the B2 names; each
	// asserts its literal local case inventory within the parent (no
	// inventory entries, no subtest names). Absence or a skip never
	// satisfies native qualification.
	"TestMCPCursorRealDecoder",
	"TestMCPCursorEnrollment",
	"TestMCPCursorQualifyPreparation",
	"TestMCPCursorWorkerResidue",
	"TestMCPCursorConfirmation",
}

// NativeTaskProcessPackage and nativeTaskProcess are the separate native
// tuple (iteration 05): the real task-process qualification must run and
// pass in the sidecar's own package within the same full-suite stream.
// Iteration 08 appends its realadaptercheck-tagged sidecar contract and its
// five subtests, and iteration 11 that contract's four wave-2 subtests
// (direct subtests of the same parent): the native command compiles the
// tag, and names passing in tests/function never satisfy these.
const NativeTaskProcessPackage = "github.com/wedevwork/callsheet/internal/sidecar"

var nativeTaskProcess = []string{"TestTaskExecutionContract", "TestTaskExecutionContract/process",
	"TestRealAdapterLocal", "TestRealAdapterLocal/selection", "TestRealAdapterLocal/file", "TestRealAdapterLocal/ordering",
	"TestRealAdapterLocal/diagnostic", "TestRealAdapterLocal/restart",
	"TestRealAdapterLocal/wave2-posture", "TestRealAdapterLocal/wave2-invocation", "TestRealAdapterLocal/wave2-outcomes", "TestRealAdapterLocal/wave2-retry"}

// RealAdapterTag is iteration 08's ordinary-only build tag: it adds the
// sidecar's TestRealAdapterLocal and BenchmarkRealAdapterFile to the
// tagged test, coverage, bench and native commands, never to a stress
// shard. It does not enable the opt-in real smoke (realadaptersmoke).
const RealAdapterTag = "realadaptercheck"

// NativeTaskProcessTests returns a fresh copy of the sidecar tuple.
func NativeTaskProcessTests() []string { return append([]string(nil), nativeTaskProcess...) }

// NativeRequiredTests returns a fresh copy of the test names in NativePackage
// that a native run must execute and pass.
func NativeRequiredTests() []string { return append([]string(nil), nativeRequired...) }

// unobserved marks every failure that leaves native qualification unproven.
const unobserved = "native qualification unobserved"

func unsupportedNative(goos string) error {
	return fmt.Errorf("devcheck: native stage is unsupported on %q: it qualifies darwin only (use test or all on other hosts)", goos)
}

// NativeSteps returns the native qualification plan: the complete suite as a
// go test JSON event stream. Only darwin is supported; every other goos
// (including linux and windows) is rejected before any child runs.
func NativeSteps(goos string) ([]Step, error) {
	if goos != "darwin" {
		return nil, unsupportedNative(goos)
	}
	// One invocation (one package start each): the tag compiles and runs the
	// sidecar's tagged contract within the complete suite (iteration 08).
	return []Step{{Name: "native", Argv: []string{"go", "test", "-json", "-tags=" + RealAdapterTag, "-count=1", "-timeout=300s", "./..."}}}, nil
}

// acceptedActions is the exact go test -json Action set the parser accepts.
var acceptedActions = map[string]bool{
	"start": true, "run": true, "pause": true, "cont": true, "output": true, "bench": true,
	"pass": true, "fail": true, "skip": true, "build-output": true, "build-fail": true,
}

// testEvent holds the fields the parser reads; unknown fields are ignored.
type testEvent struct {
	Action      *string
	Package     string
	Test        string
	Output      string
	FailedBuild string
	ImportPath  string
}

const (
	maxQuote     = 600
	maxTailLines = 20
	maxTailLine  = 300
)

// quote renders a raw event for a diagnostic, bounded in size.
func quote(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > maxQuote {
		s = s[:maxQuote] + "..."
	}
	return s
}

type pkgState struct {
	done, passed, ranTests bool
}

type testState struct {
	ran, done, passed bool
	// tail is a bounded window of the test's most recent output lines.
	tail []string
}

// nativeChecker is the streaming validation state: per-package and per-test
// status plus bounded diagnostics, never the full output.
type nativeChecker struct {
	n     int
	pkgs  map[string]*pkgState
	order []string
	tests map[string]*testState
}

func testKey(pkg, test string) string { return pkg + "\x00" + test }

func (c *nativeChecker) test(pkg, name string) *testState {
	k := testKey(pkg, name)
	st := c.tests[k]
	if st == nil {
		st = &testState{}
		c.tests[k] = st
	}
	return st
}

func (c *nativeChecker) fail(raw []byte, format string, args ...any) error {
	return fmt.Errorf("devcheck: native: event %d: %s: %s", c.n, fmt.Sprintf(format, args...), quote(raw))
}

// output appends an output line to the owning test's bounded tail.
func (c *nativeChecker) output(ev testEvent) {
	line := strings.TrimRight(ev.Output, "\n")
	if line == "" || ev.Package == "" {
		return
	}
	if len(line) > maxTailLine {
		line = line[:maxTailLine] + "..."
	}
	st := c.test(ev.Package, ev.Test)
	st.tail = append(st.tail, line)
	if len(st.tail) > maxTailLines {
		st.tail = st.tail[len(st.tail)-maxTailLines:]
	}
}

func (c *nativeChecker) tailOf(pkg, test string) string {
	st := c.tests[testKey(pkg, test)]
	if st == nil || len(st.tail) == 0 {
		return ""
	}
	return "\n" + strings.Join(st.tail, "\n")
}

func describe(pkg, test string) string {
	if test == "" {
		return "package " + pkg
	}
	return "test " + test + " in " + pkg
}

func (c *nativeChecker) apply(raw []byte, ev testEvent) error {
	if ev.Action == nil {
		return c.fail(raw, "event has no Action")
	}
	action := *ev.Action
	switch {
	case action == "":
		return c.fail(raw, "event has an empty Action")
	case !acceptedActions[action]:
		return c.fail(raw, "unknown Action %q", action)
	}
	switch action {
	case "build-output":
		return nil
	case "build-fail":
		return c.fail(raw, "build failed for %s", ev.ImportPath)
	case "fail":
		msg := describe(ev.Package, ev.Test) + " failed"
		if ev.FailedBuild != "" {
			msg += " (FailedBuild " + ev.FailedBuild + ")"
		}
		return fmt.Errorf("%w%s", c.fail(raw, "%s", msg), c.tailOf(ev.Package, ev.Test))
	}
	if ev.Package == "" {
		if action == "output" || action == "bench" {
			return nil
		}
		return c.fail(raw, "%s event names no Package", action)
	}
	p := c.pkgs[ev.Package]
	if action == "start" {
		if p != nil {
			return c.fail(raw, "package %s started twice", ev.Package)
		}
		c.pkgs[ev.Package] = &pkgState{}
		c.order = append(c.order, ev.Package)
		return nil
	}
	if p == nil {
		return c.fail(raw, "%s event for package %s before its start", action, ev.Package)
	}
	if p.done {
		return c.fail(raw, "%s event after package %s finished", action, ev.Package)
	}
	switch action {
	case "output", "bench":
		// Diagnostics only; a bench result never satisfies a required test.
		c.output(ev)
	case "run":
		if ev.Test == "" {
			return c.fail(raw, "run event names no Test")
		}
		c.test(ev.Package, ev.Test).ran = true
		p.ranTests = true
	case "pause", "cont":
		st := c.tests[testKey(ev.Package, ev.Test)]
		if ev.Test == "" || st == nil || !st.ran || st.done {
			return c.fail(raw, "%s event for a test that is not running", action)
		}
	case "pass":
		if ev.Test == "" {
			p.done, p.passed = true, true
			return nil
		}
		st := c.test(ev.Package, ev.Test)
		st.done, st.passed = true, true
	case "skip":
		if ev.Test != "" {
			return fmt.Errorf("%w%s", c.fail(raw, "%s skipped: %s", describe(ev.Package, ev.Test), unobserved), c.tailOf(ev.Package, ev.Test))
		}
		if ev.Package == NativePackage {
			return c.fail(raw, "required package %s skipped: %s", ev.Package, unobserved)
		}
		if p.ranTests {
			return c.fail(raw, "package %s skipped after running tests", ev.Package)
		}
		p.done = true
	}
	return nil
}

func (c *nativeChecker) finish() error {
	if c.n == 0 {
		return fmt.Errorf("devcheck: native: empty event stream: %s", unobserved)
	}
	for _, pkg := range c.order {
		if !c.pkgs[pkg].done {
			return fmt.Errorf("devcheck: native: package %s did not finish before the end of the stream", pkg)
		}
	}
	var missing []string
	if p := c.pkgs[NativePackage]; p == nil {
		missing = append(missing, "package "+NativePackage+" has no start event")
	} else if !p.passed {
		missing = append(missing, "package "+NativePackage+" has no pass event")
	}
	for _, name := range nativeRequired {
		st := c.tests[testKey(NativePackage, name)]
		switch {
		case st == nil || !st.ran:
			missing = append(missing, name+" has no run event")
		case !st.passed:
			missing = append(missing, name+" has no pass event")
		}
	}
	for _, name := range nativeTaskProcess {
		st := c.tests[testKey(NativeTaskProcessPackage, name)]
		switch {
		case st == nil || !st.ran:
			missing = append(missing, name+" in "+NativeTaskProcessPackage+" has no run event")
		case !st.passed:
			missing = append(missing, name+" in "+NativeTaskProcessPackage+" has no pass event")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("devcheck: native: required evidence missing (%s): %s", unobserved, strings.Join(missing, "; "))
	}
	return nil
}

// CheckNativeResults validates a go test -json stdout event stream from the
// NativeSteps plan without invoking any tool. Only goos darwin is accepted.
// It fails for malformed or truncated JSON, a missing, empty or unknown
// Action, any fail or build-fail, any test skip, a package skip other than
// a nonrequired no-test-files package, a started package that never
// finishes, and missing run/pass evidence for NativePackage and its
// required process-group and plane trust tests.
func CheckNativeResults(goos string, r io.Reader) error {
	if goos != "darwin" {
		return unsupportedNative(goos)
	}
	c := &nativeChecker{pkgs: map[string]*pkgState{}, tests: map[string]*testState{}}
	dec := json.NewDecoder(r)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("devcheck: native: malformed event stream after %d events: %v", c.n, err)
		}
		c.n++
		var ev testEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return c.fail(raw, "malformed event: %v", err)
		}
		if err := c.apply(raw, ev); err != nil {
			return err
		}
	}
	return c.finish()
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

// native runs the native plan with separate stdout and stderr: stdout goes
// to the user and a scratch JSON file (the only parser input), stderr to a
// scratch log and the user's diagnostics. It stops at the first child
// failure and validates the JSON only after every child succeeded; only
// then (iteration 09a) does it run the coverage stage and the workspace
// benchmark step, and (iteration 09b) the transfer benchmark step.
func (d *driver) native(steps []Step) error {
	jsonPath := filepath.Join(d.scratch, "native-events.jsonl")
	errPath := filepath.Join(d.scratch, "native-stderr.log")
	events, err := os.Create(jsonPath)
	if err != nil {
		return err
	}
	stderrLog, err := os.Create(errPath)
	if err != nil {
		events.Close()
		return err
	}
	runErr := d.nativeChildren(steps, events, stderrLog)
	closeErr := errors.Join(events.Close(), stderrLog.Close())
	if runErr != nil {
		return runErr
	}
	if closeErr != nil {
		return closeErr
	}
	f, err := os.Open(jsonPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := CheckNativeResults(d.goos, f); err != nil {
		return fmt.Errorf("%w\ndevcheck: native events %s, stderr %s", err, jsonPath, errPath)
	}
	fmt.Fprintf(d.out, "devcheck: native qualification passed on %s/%s: %s: %s\n",
		d.goos, runtime.GOARCH, NativePackage, strings.Join(nativeRequired, ", "))
	// Iteration 09a: after the qualification succeeded, and outside its
	// parsed JSON event stream, the coverage gates (project-wide and the
	// new/changed manifest) and the workspace benchmarks run natively;
	// iteration 09b adds the transfer benchmarks after them, iteration 10b
	// the task workspace benchmarks after those, and the non-blocking
	// coordinator waits design the client wait benchmark after those, and
	// the decoder-enrollment design the qualification harness's benchmark
	// step last (NativeBenchSteps).
	if err := d.coverage(""); err != nil {
		return err
	}
	return d.steps(NativeBenchSteps())
}

func (d *driver) nativeChildren(steps []Step, events, stderrLog io.Writer) error {
	for _, s := range steps {
		fmt.Fprintf(d.out, "devcheck: %s: %s\n", s.Name, strings.Join(s.Argv, " "))
		tail := &tailBuffer{max: 2000}
		err := d.run(d.ctx, s.Argv, MergeEnv(os.Environ(), s.Env), "",
			io.MultiWriter(events, d.out), io.MultiWriter(stderrLog, d.errOut, tail))
		if err != nil {
			return &StepError{Step: s, Err: err, Stderr: string(tail.b)}
		}
	}
	return nil
}
