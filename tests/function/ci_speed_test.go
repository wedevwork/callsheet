package function

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wedevwork/callsheet/internal/cicheck"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 02b function tests: one top-level TestCISpeed* per FP. They read
// tracked code and docs only, never contact GitHub and never run a real
// suite, stress or devcheck recursively; the one child go test runs a
// standard-library-only fixture module in a temporary directory.

// The literal deduplicated stress plan (design 02b, Stress selection), as
// sharded by design 02c: processgroup left the packages command for three
// single-CPU invocations, and so did ./internal/plane by design 05b and
// ./internal/sidecar by its sidecar follow-up, and ./internal/contract
// (into the packages shard's per-CPU group) by the contract headroom fix,
// and ./internal/mcpqual and ./internal/workspace likewise by the workspace
// and mcpqual headroom fix (those three groups in the packages-cpu shard
// since the stress worker rebalance);
// the function commands are unchanged.
const (
	speedPackages      = "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/adapter ./internal/mcp ./internal/workspacetransfer ./internal/taskworkspace ./internal/taskpublication"
	speedContract1     = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/contract"
	speedContract2     = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/contract"
	speedContract4     = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/contract"
	speedMcpqual1      = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/mcpqual"
	speedMcpqual2      = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/mcpqual"
	speedMcpqual4      = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/mcpqual"
	speedWorkspace1    = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/workspace"
	speedWorkspace2    = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/workspace"
	speedWorkspace4    = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/workspace"
	speedPlane1        = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/plane"
	speedPlane2        = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/plane"
	speedPlane4        = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/plane"
	speedSidecar1      = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/sidecar"
	speedSidecar2      = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/sidecar"
	speedSidecar4      = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/sidecar"
	speedPG1           = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/spikes/processgroup"
	speedPG2           = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/spikes/processgroup"
	speedPG4           = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/spikes/processgroup"
	speedFunction      = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function"
	speedPlaneSelector = "^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$"
	speedPlaneFunction = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=" + speedPlaneSelector + " ./tests/function"
	// speedNodeFunction is iteration 03's node process-boundary step.
	speedNodeFunction = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$ ./tests/function"
)

// speedSubtests are the direct subtests of the three stressed plane
// parents, in source order: the retained process-boundary scenarios, then
// each parent's excluded "contracts" boundary.
var speedSubtests = map[string][]string{
	"TestPlaneState":   {"paths", "persistence", "locking", "validation", "contracts"},
	"TestPlaneTLS":     {"https-only", "prelisten-validation", "bounded-shutdown", "contracts"},
	"TestPlaneReissue": {"process", "contracts"},
}

// speedContracts are the delegated contracts each parent's "contracts"
// subtest runs (and nothing else in the parent may run).
var speedContracts = map[string][]string{
	"TestPlaneState":   {"TestNativeStateContract", "TestStateFailureContract"},
	"TestPlaneTLS":     {"TestServerFailureContract"},
	"TestPlaneReissue": {"TestReissueFailureContract"},
}

// speedSelected are the eight subtests the plane stress step selects.
var speedSelected = []string{
	"TestPlaneState/paths", "TestPlaneState/persistence", "TestPlaneState/locking", "TestPlaneState/validation",
	"TestPlaneTLS/https-only", "TestPlaneTLS/prelisten-validation", "TestPlaneTLS/bounded-shutdown",
	"TestPlaneReissue/process",
}

// speedNative is the 28-name native qualification list (design 02b, Policy
// consistency), written out independently of devcheck; iteration 03
// appends speedNodeNative after it.
var speedNative = []string{
	"TestFP6ProcessGroups", "TestFP6ProcessGroups/cooperative", "TestFP6ProcessGroups/resistant", "TestFP6ProcessGroups/leader-exits-first",
	"TestPlaneCommands",
	"TestPlaneState", "TestPlaneState/paths", "TestPlaneState/persistence", "TestPlaneState/locking", "TestPlaneState/validation", "TestPlaneState/contracts",
	"TestPlaneBind",
	"TestPlaneInit", "TestPlaneInit/issuance", "TestPlaneInit/fingerprint", "TestPlaneInit/restart-invariance",
	"TestPlaneTLS", "TestPlaneTLS/https-only", "TestPlaneTLS/prelisten-validation", "TestPlaneTLS/bounded-shutdown", "TestPlaneTLS/contracts",
	"TestPlaneReissue", "TestPlaneReissue/process", "TestPlaneReissue/contracts",
	"TestPlaneStatus", "TestPlaneStatus/inspection", "TestPlaneStatus/expiry-warnings",
	"TestPlanePlatform",
}

// speedNodeNative are iteration 03's 30 required node names.
var speedNodeNative = []string{
	"TestNodeTrust", "TestNodeTrust/ca", "TestNodeTrust/pin", "TestNodeTrust/rejections",
	"TestNodeEnrollment", "TestNodeEnrollment/identity", "TestNodeEnrollment/recovery", "TestNodeEnrollment/locking",
	"TestNodeProtocol", "TestNodeProtocol/hello", "TestNodeProtocol/limits",
	"TestNodeReconnect", "TestNodeReconnect/restart", "TestNodeReconnect/disconnect", "TestNodeReconnect/shutdown",
	"TestNodeLease", "TestNodeLease/expiry", "TestNodeLease/return",
	"TestNodeRegistry", "TestNodeRegistry/restore", "TestNodeRegistry/validation",
	"TestNodeDiscovery", "TestNodeDiscovery/text", "TestNodeDiscovery/json", "TestNodeDiscovery/errors",
	"TestNodePlatform", "TestNodePlatform/paths", "TestNodePlatform/native-state", "TestNodePlatform/policy", "TestNodePlatform/sticky-write",
}

// speedRoleNative are iteration 04's 29 required role names.
var speedRoleNative = []string{
	"TestRoleConfiguration", "TestRoleConfiguration/fields", "TestRoleConfiguration/order",
	"TestRoleAdapter", "TestRoleAdapter/disabled", "TestRoleAdapter/probe",
	"TestRoleValidation", "TestRoleValidation/remote", "TestRoleValidation/rejections",
	"TestRoleProtocol", "TestRoleProtocol/duplex", "TestRoleProtocol/bounds",
	"TestRoleReadiness", "TestRoleReadiness/changes", "TestRoleReadiness/reconnect",
	"TestRolePersistence", "TestRolePersistence/restore", "TestRolePersistence/failures",
	"TestRoleCommands", "TestRoleCommands/text", "TestRoleCommands/json", "TestRoleCommands/trust",
	"TestRoleMutation", "TestRoleMutation/races", "TestRoleMutation/remove",
	"TestRolePlatform", "TestRolePlatform/manuals", "TestRolePlatform/executable", "TestRolePlatform/policy",
}

// speedTaskNative are iteration 05's 41 required task names.
// speedControlNative are iteration 06a's native names: the eight
// control function parents and the native group qualification.
var speedControlNative = []string{
	"TestControlDurability", "TestControlReconnect", "TestControlNodeLoss", "TestControlLaunchSafety", "TestControlWorkerRecovery",
	"TestControlPlaneRecovery", "TestControlLateResult", "TestControlLegacy",
	"TestControlNativeGroups", "TestControlNativeGroups/cooperative", "TestControlNativeGroups/resistant",
	"TestControlNativeGroups/orphan-restart", "TestControlNativeGroups/plane-restart",
	// Iteration 06b: the four task-control function parents.
	"TestControlCancellation", "TestControlExecutionTimeout", "TestControlBoundedWait", "TestControlForceRemove",
}

// speedMCPNative are iteration 07a's 77 required MCP names: the eight
// function parents, each followed by its mandatory children.
var speedMCPNative = []string{
	"TestMCPProtocol", "TestMCPProtocol/initialize", "TestMCPProtocol/version", "TestMCPProtocol/discovery", "TestMCPProtocol/framing",
	"TestMCPProtocol/concurrency", "TestMCPProtocol/cancellation",
	"TestMCPRelay", "TestMCPRelay/ca", "TestMCPRelay/pin", "TestMCPRelay/trust-errors", "TestMCPRelay/protocol-mismatch", "TestMCPRelay/contract-errors",
	"TestMCPRelay/no-cache", "TestMCPRelay/recovery",
	"TestMCPNodes", "TestMCPNodes/node-ls", "TestMCPNodes/node-show", "TestMCPNodes/shared-roster",
	"TestMCPRoles", "TestMCPRoles/role-add", "TestMCPRoles/role-set", "TestMCPRoles/role-ls", "TestMCPRoles/role-show", "TestMCPRoles/role-rm",
	"TestMCPRoles/global-slots", "TestMCPRoles/force-pending", "TestMCPRoles/force-completed", "TestMCPRoles/operation-rejoin", "TestMCPRoles/instance-reuse",
	"TestMCPDispatch", "TestMCPDispatch/target-id", "TestMCPDispatch/target-name", "TestMCPDispatch/overrides", "TestMCPDispatch/async",
	"TestMCPDispatch/attribution", "TestMCPDispatch/invalid-attribution", "TestMCPDispatch/last-slot", "TestMCPDispatch/wait-fast", "TestMCPDispatch/wait-slow",
	"TestMCPDispatch/lost-response",
	"TestMCPTaskReads", "TestMCPTaskReads/task-ls", "TestMCPTaskReads/task-show", "TestMCPTaskReads/task-logs", "TestMCPTaskReads/pagination",
	"TestMCPTaskReads/tails", "TestMCPTaskReads/binary-logs", "TestMCPTaskReads/late-logs", "TestMCPTaskReads/late-not-found", "TestMCPTaskReads/output-bounds",
	"TestMCPWaitCancel", "TestMCPWaitCancel/cancel-accepted", "TestMCPWaitCancel/cancel-terminal", "TestMCPWaitCancel/cancel-errors",
	"TestMCPWaitCancel/wait-one", "TestMCPWaitCancel/wait-many", "TestMCPWaitCancel/any-terminal", "TestMCPWaitCancel/snapshot",
	"TestMCPWaitCancel/interim-default", "TestMCPWaitCancel/budget-deadline", "TestMCPWaitCancel/delivery-deadline", "TestMCPWaitCancel/plane-cap",
	"TestMCPWaitCancel/restart-budget", "TestMCPWaitCancel/own-deadline", "TestMCPWaitCancel/cancel-wait-only", "TestMCPWaitCancel/catalog-interim",
	"TestMCPLifetime", "TestMCPLifetime/eof", "TestMCPLifetime/sigterm", "TestMCPLifetime/sigint", "TestMCPLifetime/closed-stdout",
	"TestMCPLifetime/stalled-reader", "TestMCPLifetime/slow-reader-max-logs", "TestMCPLifetime/outstanding-wait", "TestMCPLifetime/task-survives",
	"TestMCPLifetime/reaping",
}

// speedQualNative are iteration 07b's 51 required names: the eight setup
// and qualification function parents, each followed by its mandatory
// children, after the unchanged 07a block.
var speedQualNative = []string{
	"TestMCPSetup", "TestMCPSetup/claude", "TestMCPSetup/codex", "TestMCPSetup/grok", "TestMCPSetup/cursor", "TestMCPSetup/runbook-ownership", "TestMCPSetup/client-info",
	"TestMCPQualificationProbe", "TestMCPQualificationProbe/immediate", "TestMCPQualificationProbe/slow", "TestMCPQualificationProbe/progress",
	"TestMCPQualificationProbe/no-token", "TestMCPQualificationProbe/cancellation",
	"TestMCPQualificationSchema", "TestMCPQualificationSchema/plan", "TestMCPQualificationSchema/report", "TestMCPQualificationSchema/limits", "TestMCPQualificationSchema/paths",
	"TestMCPQualificationDecoders", "TestMCPQualificationDecoders/claude", "TestMCPQualificationDecoders/codex", "TestMCPQualificationDecoders/grok",
	"TestMCPQualificationDecoders/cursor", "TestMCPQualificationDecoders/unknown-version", "TestMCPQualificationDecoders/non-tool-error",
	"TestMCPQualificationMeasurements", "TestMCPQualificationMeasurements/default", "TestMCPQualificationMeasurements/override", "TestMCPQualificationMeasurements/progress",
	"TestMCPQualificationMeasurements/absolute", "TestMCPQualificationMeasurements/lower-bound", "TestMCPQualificationMeasurements/partial", "TestMCPQualificationMeasurements/budget",
	"TestMCPQualificationPublish", "TestMCPQualificationPublish/verified", "TestMCPQualificationPublish/partial", "TestMCPQualificationPublish/redaction",
	"TestMCPQualificationPublish/hashes", "TestMCPQualificationPublish/refuse-conflict", "TestMCPQualificationPublish/worker-facts",
	"TestMCPQualificationReaping", "TestMCPQualificationReaping/cooperative", "TestMCPQualificationReaping/resistant", "TestMCPQualificationReaping/parent-exits-first",
	"TestMCPQualificationReaping/interrupt", "TestMCPQualificationReaping/cleanup-failure",
	"TestMCPQualificationInvocation", "TestMCPQualificationInvocation/denied", "TestMCPQualificationInvocation/allowed", "TestMCPQualificationInvocation/model-free",
	"TestMCPQualificationInvocation/ci-denied",
}

var speedTaskNative = []string{
	"TestTaskModel", "TestTaskModel/envelope", "TestTaskModel/states",
	"TestTaskDispatch", "TestTaskDispatch/selection", "TestTaskDispatch/gate-race", "TestTaskDispatch/reserved-slot",
	"TestTaskProtocol", "TestTaskProtocol/duplex", "TestTaskProtocol/bounds", "TestTaskProtocol/result-receipt", "TestTaskProtocol/result-ack-loss",
	"TestTaskExecution", "TestTaskExecution/compose", "TestTaskExecution/invoke", "TestTaskExecution/exit", "TestTaskExecution/process", "TestTaskExecution/platform", "TestTaskExecution/policy",
	"TestTaskLogs", "TestTaskLogs/retention", "TestTaskLogs/backpressure", "TestTaskLogs/final",
	"TestTaskPersistence", "TestTaskPersistence/durability", "TestTaskPersistence/restore",
	"TestTaskCommands", "TestTaskCommands/dispatch", "TestTaskCommands/ls", "TestTaskCommands/show", "TestTaskCommands/logs", "TestTaskCommands/trust",
	"TestTaskRoles", "TestTaskRoles/counts", "TestTaskRoles/mutation", "TestTaskRoles/remaining-capacity", "TestTaskRoles/recovery-remove",
	"TestTaskRecoveryBoundary", "TestTaskRecoveryBoundary/disconnect", "TestTaskRecoveryBoundary/remaining-capacity", "TestTaskRecoveryBoundary/recovery-remove",
}

// speedTaskProcess is the sidecar package's native tuple (iteration 05),
// with iteration 08's tagged contract and its five subtests appended, then
// iteration 11's four wave-2 subtests of that contract.
var speedTaskProcess = []string{"TestTaskExecutionContract", "TestTaskExecutionContract/process",
	"TestRealAdapterLocal", "TestRealAdapterLocal/selection", "TestRealAdapterLocal/file", "TestRealAdapterLocal/ordering",
	"TestRealAdapterLocal/diagnostic", "TestRealAdapterLocal/restart",
	"TestRealAdapterLocal/wave2-posture", "TestRealAdapterLocal/wave2-invocation", "TestRealAdapterLocal/wave2-outcomes", "TestRealAdapterLocal/wave2-retry"}

// speedRealNative are iteration 08's nine function parents, each followed
// by its mandatory children.
var speedRealNative = []string{
	"TestRealAdapterRegistration", "TestRealAdapterRegistration/registry", "TestRealAdapterRegistration/paths", "TestRealAdapterRegistration/selection",
	"TestRealAdapterProbe", "TestRealAdapterProbe/claude", "TestRealAdapterProbe/codex", "TestRealAdapterProbe/refusal",
	"TestRealAdapterInvocation", "TestRealAdapterInvocation/claude", "TestRealAdapterInvocation/codex", "TestRealAdapterInvocation/stdin",
	"TestRealAdapterClaudeFinal", "TestRealAdapterClaudeFinal/success", "TestRealAdapterClaudeFinal/failure", "TestRealAdapterClaudeFinal/malformed",
	"TestRealAdapterCodexFinal", "TestRealAdapterCodexFinal/success", "TestRealAdapterCodexFinal/absent", "TestRealAdapterCodexFinal/unsafe", "TestRealAdapterCodexFinal/cleanup",
	"TestRealAdapterOutcomes", "TestRealAdapterOutcomes/exits", "TestRealAdapterOutcomes/denials", "TestRealAdapterOutcomes/controls", "TestRealAdapterOutcomes/replay",
	"TestRealAdapterCatalog", "TestRealAdapterCatalog/recipes", "TestRealAdapterCatalog/evidence", "TestRealAdapterCatalog/ownership",
	"TestRealAdapterDispatch", "TestRealAdapterDispatch/claude", "TestRealAdapterDispatch/codex", "TestRealAdapterDispatch/no-vendors",
	"TestRealAdapterSmokeGate", "TestRealAdapterSmokeGate/default-off", "TestRealAdapterSmokeGate/ci-off", "TestRealAdapterSmokeGate/absent", "TestRealAdapterSmokeGate/enabled",
}

// speedWorkspaceNative are iteration 09a's ten workspace function tests
// and the D1 maximum-path subcase, in FP order.
var speedWorkspaceNative = []string{
	"TestWorkspaceCreate", "TestWorkspaceList", "TestWorkspaceShow", "TestWorkspaceRemove", "TestWorkspacePrune", "TestWorkspaceTransport",
	"TestWorkspaceRefSet", "TestWorkspaceRefSet/max-path", "TestWorkspaceStatus", "TestWorkspaceDiff", "TestWorkspaceNoGit",
}

// speedTransferNative are iteration 09b's ten local transfer function
// tests in FP order, after the 09a names.
var speedTransferNative = []string{
	"TestWorkspacePushGit", "TestWorkspacePushCleanliness", "TestWorkspacePushFolder", "TestWorkspaceTransferIgnores", "TestWorkspacePullGit",
	"TestWorkspacePullFolder", "TestWorkspaceTransferCLI", "TestWorkspaceTransferMCP", "TestWorkspaceTransferNoGit", "TestWorkspaceTransferEligibility",
}

// speedLatencyNative are iteration 10a's two latency function tests in FP
// order, after the 09b names.
var speedLatencyNative = []string{"TestTaskPromptReadiness", "TestTaskFastGroupCleanup"}

// speedWorkspaceTaskNative are iteration 10b's nine workspace execution
// function tests in FP order, each named acceptance scenario after its
// parent, after the 10a names.
var speedWorkspaceTaskNative = []string{
	"TestTaskWorkspaceAdmission", "TestTaskWorkspaceAccess", "TestTaskWorkspaceCache", "TestTaskWorkspaceCheckout",
	"TestTaskWorkspaceCommit", "TestTaskWorkspaceCommit/AC-WS-1", "TestTaskWorkspacePublication", "TestTaskWorkspacePublication/AC-WS-5",
	"TestTaskWorkspaceMetadata", "TestTaskWorkspaceRecovery", "TestTaskWorkspaceIsolation", "TestTaskWorkspaceIsolation/AC-WS-2",
}

// speedWorkspaceDoorNative are iteration 10c's six coordinator delivery
// function tests in FP order, after the 10b names.
var speedWorkspaceDoorNative = []string{
	"TestWorkspaceDispatchDoors", "TestWorkspaceTaskPull", "TestWorkspaceTaskInspect", "TestWorkspaceTaskMCP", "TestWorkspaceMultiHop", "TestWorkspaceOperatorWorkflow",
}

// speedWave2Native are iteration 11's nine wave-2 real-adapter function
// parents in FP order, each followed by its mandatory children, after the
// 10c names.
var speedWave2Native = []string{
	"TestWave2Registration", "TestWave2Registration/registry", "TestWave2Registration/paths", "TestWave2Registration/selection", "TestWave2Registration/posture",
	"TestWave2Probe", "TestWave2Probe/grok", "TestWave2Probe/cursor", "TestWave2Probe/refusal",
	"TestWave2Invocation", "TestWave2Invocation/grok", "TestWave2Invocation/cursor-refused", "TestWave2Invocation/prompt",
	"TestWave2GrokFinal", "TestWave2GrokFinal/success", "TestWave2GrokFinal/error", "TestWave2GrokFinal/cancelled", "TestWave2GrokFinal/malformed",
	"TestWave2CursorFinal", "TestWave2CursorFinal/success", "TestWave2CursorFinal/absent", "TestWave2CursorFinal/malformed", "TestWave2CursorFinal/blocked",
	"TestWave2Outcomes", "TestWave2Outcomes/exits", "TestWave2Outcomes/controls", "TestWave2Outcomes/refusal", "TestWave2Outcomes/retry",
	"TestWave2Catalog", "TestWave2Catalog/recipes", "TestWave2Catalog/evidence", "TestWave2Catalog/ownership",
	"TestWave2Dispatch", "TestWave2Dispatch/grok", "TestWave2Dispatch/cursor-refused", "TestWave2Dispatch/no-vendors",
	"TestWave2SmokeGate", "TestWave2SmokeGate/default-off", "TestWave2SmokeGate/ci-off", "TestWave2SmokeGate/absent", "TestWave2SmokeGate/enabled", "TestWave2SmokeGate/posture",
}

// speedNBWNative are the non-blocking coordinator waits design's eight
// function parents in FP order, after the iteration 11 names.
var speedNBWNative = []string{
	"TestWaitUntilDoneCLI", "TestWaitUntilDoneRenewal", "TestWaitUntilDoneFailure", "TestWaitUntilDoneOutput",
	"TestCoordinatorBackgroundWait", "TestMCPShortPollGuidance", "TestShortPollCatalogPolicy", "TestMCPShortConfirmation",
}

// speedDCENative are the decoder-enrollment design's nine function
// parents in FP order, after the non-blocking waits names.
var speedDCENative = []string{
	"TestMCPCaptureInvocation", "TestMCPCaptureRecipes", "TestMCPCaptureEvidence", "TestMCPCaptureLifecycle", "TestMCPEnrollmentContract",
	"TestMCPEnrollmentReplay", "TestMCPEnrolledShortConfirmation", "TestMCPCaptureRunbook", "TestMCPEnrollmentCIPolicy",
}

// speedB1Native are decoder-enrollment slice B1's four function parents
// in FP order (FP-10..FP-13), after the slice A names.
var speedB1Native = []string{"TestMCPCaptureProtocolNegotiation", "TestMCPCaptureCodexApproval", "TestMCPCaptureGrokRecipe", "TestMCPCaptureCursorTrust"}

// speedB15Native are decoder-enrollment slice B1.5's three function
// parents in FP order (FP-14..FP-16), after the B1 names.
var speedB15Native = []string{"TestMCPProbeTerminalObservation", "TestMCPCaptureCursorProjectApproval", "TestMCPCaptureCursorInventoryPolicy"}

// speedB2Native are decoder-enrollment slice B2's five function parents in
// FP order (FP-17..FP-21), after the B1.5 names.
var speedB2Native = []string{"TestMCPRealDecoderMappings", "TestMCPFixtureSanitization", "TestMCPRealEnrollment", "TestMCPCaptureCursorToolPermission",
	"TestMCPRealEnrollmentConfirmation"}

// speedB3Native are decoder-enrollment slice B3's five function parents in
// FP order (FP-22..FP-26), after the B2 names.
var speedB3Native = []string{"TestMCPCursorRealDecoder", "TestMCPCursorEnrollment", "TestMCPCursorQualifyPreparation", "TestMCPCursorWorkerResidue",
	"TestMCPCursorConfirmation"}

// taskProcessEvents is a passing sidecar-package stream for the tuple
// except drop.
func taskProcessEvents(drop string) []map[string]any {
	pkg := devcheck.NativeTaskProcessPackage
	evs := []map[string]any{synth("start", pkg, "")}
	for _, name := range speedTaskProcess {
		if name != drop {
			evs = append(evs, synth("run", pkg, name))
		}
	}
	for i := len(speedTaskProcess) - 1; i >= 0; i-- {
		if speedTaskProcess[i] != drop {
			evs = append(evs, synth("pass", pkg, speedTaskProcess[i]))
		}
	}
	return append(evs, synth("pass", pkg, ""))
}

// speedJobs is the table of ordinary jobs: the two main jobs (design 02b,
// Workflow topology) and the sixteen stress workers that replaced its two
// stress jobs (design 02c, with design 05b's plane workers, its sidecar
// follow-up's sidecar workers, design 06a-perf's plane and sidecar CPU1
// workers and the stress worker rebalance's packages-cpu workers). The two
// summaries that keep the stress contexts are checked by
// TestStressShardSummaries.
var speedJobs = []struct {
	id, name, runner, timeout string
	checks                    []string
}{
	{"linux", "ci-linux", "ubuntu-24.04", "45", []string{"go run ./cmd/devcheck test", "go run ./cmd/devcheck coverage", "go run ./cmd/devcheck bench", "go run ./cmd/devcheck cross"}},
	{"macos", "ci-macos", "macos-15", "30", []string{"go run ./cmd/devcheck native"}},
	{"linux-stress-packages", "ci-linux-stress-packages", "ubuntu-24.04", "20", []string{"go run ./cmd/devcheck stress-packages"}},
	{"linux-stress-packages-cpu", "ci-linux-stress-packages-cpu", "ubuntu-24.04", "20", []string{"go run ./cmd/devcheck stress-packages-cpu"}},
	{"linux-stress-plane-cpu1", "ci-linux-stress-plane-cpu1", "ubuntu-24.04", "20", []string{"go run ./cmd/devcheck stress-plane-cpu1"}},
	{"linux-stress-plane", "ci-linux-stress-plane", "ubuntu-24.04", "20", []string{"go run ./cmd/devcheck stress-plane"}},
	{"linux-stress-sidecar-cpu1", "ci-linux-stress-sidecar-cpu1", "ubuntu-24.04", "20", []string{"go run ./cmd/devcheck stress-sidecar-cpu1"}},
	{"linux-stress-sidecar", "ci-linux-stress-sidecar", "ubuntu-24.04", "20", []string{"go run ./cmd/devcheck stress-sidecar"}},
	{"linux-stress-processgroup", "ci-linux-stress-processgroup", "ubuntu-24.04", "20", []string{"go run ./cmd/devcheck stress-processgroup"}},
	{"linux-stress-functions", "ci-linux-stress-functions", "ubuntu-24.04", "20", []string{"go run ./cmd/devcheck stress-functions"}},
	{"macos-stress-packages", "ci-macos-stress-packages", "macos-15", "20", []string{"go run ./cmd/devcheck stress-packages"}},
	{"macos-stress-packages-cpu", "ci-macos-stress-packages-cpu", "macos-15", "20", []string{"go run ./cmd/devcheck stress-packages-cpu"}},
	{"macos-stress-plane-cpu1", "ci-macos-stress-plane-cpu1", "macos-15", "20", []string{"go run ./cmd/devcheck stress-plane-cpu1"}},
	{"macos-stress-plane", "ci-macos-stress-plane", "macos-15", "20", []string{"go run ./cmd/devcheck stress-plane"}},
	{"macos-stress-sidecar-cpu1", "ci-macos-stress-sidecar-cpu1", "macos-15", "20", []string{"go run ./cmd/devcheck stress-sidecar-cpu1"}},
	{"macos-stress-sidecar", "ci-macos-stress-sidecar", "macos-15", "20", []string{"go run ./cmd/devcheck stress-sidecar"}},
	{"macos-stress-processgroup", "ci-macos-stress-processgroup", "macos-15", "20", []string{"go run ./cmd/devcheck stress-processgroup"}},
	{"macos-stress-functions", "ci-macos-stress-functions", "macos-15", "20", []string{"go run ./cmd/devcheck stress-functions"}},
}

// tRunName returns the literal name and body of a t.Run("name", func...)
// expression statement, or ok=false.
func tRunName(st ast.Stmt) (string, *ast.FuncLit, bool) {
	es, ok := st.(*ast.ExprStmt)
	if !ok {
		return "", nil, false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return "", nil, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Run" {
		return "", nil, false
	}
	if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "t" {
		return "", nil, false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	fn, fok := call.Args[1].(*ast.FuncLit)
	if !ok || !fok || lit.Kind != token.STRING {
		return "", nil, false
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", nil, false
	}
	return name, fn, true
}

// delegatedCall reports whether call is a delegated contract invocation
// (planeContract) or a contract build (testkit.BuildTestBinary), and for a
// planeContract call the contract name.
func delegatedCall(call *ast.CallExpr) (kind, contract string) {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		if f.Name == "planeContract" && len(call.Args) >= 4 {
			if lit, ok := call.Args[3].(*ast.BasicLit); ok {
				name, _ := strconv.Unquote(lit.Value)
				return "planeContract", name
			}
			return "planeContract", ""
		}
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok && x.Name == "testkit" && f.Sel.Name == "BuildTestBinary" {
			return "BuildTestBinary", ""
		}
	}
	return "", ""
}

// FP-1: the exact deduplicated selection, the real test organization it
// relies on, and the selector's execution semantics on a fixture.
func TestCISpeedSelection(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		steps, err := devcheck.StressSteps(goos)
		if err != nil || len(steps) != 22 {
			t.Fatalf("%s: %+v %v", goos, steps, err)
		}
		for i, want := range []struct{ name, argv string }{{"stress packages", speedPackages},
			{"stress contract cpu1", speedContract1}, {"stress contract cpu2", speedContract2}, {"stress contract cpu4", speedContract4},
			{"stress mcpqual cpu1", speedMcpqual1}, {"stress mcpqual cpu2", speedMcpqual2}, {"stress mcpqual cpu4", speedMcpqual4},
			{"stress workspace cpu1", speedWorkspace1}, {"stress workspace cpu2", speedWorkspace2}, {"stress workspace cpu4", speedWorkspace4},
			{"stress plane cpu1", speedPlane1}, {"stress plane cpu2", speedPlane2}, {"stress plane cpu4", speedPlane4},
			{"stress sidecar cpu1", speedSidecar1}, {"stress sidecar cpu2", speedSidecar2}, {"stress sidecar cpu4", speedSidecar4}, {"stress processgroup cpu1", speedPG1},
			{"stress processgroup cpu2", speedPG2}, {"stress processgroup cpu4", speedPG4}, {"stress function", speedFunction}, {"stress plane function", speedPlaneFunction},
			{"stress node function", speedNodeFunction}} {
			if steps[i].Name != want.name || strings.Join(steps[i].Argv, " ") != want.argv || strings.Join(steps[i].Env, " ") != "CGO_ENABLED=1" {
				t.Fatalf("%s step %d = %+v, want %s: %s", goos, i, steps[i], want.name, want.argv)
			}
		}
		// FP-6 is no longer repeated by a function step; its experiment
		// stays repeated through the complete processgroup package.
		for _, s := range steps[19:] {
			if strings.Contains(strings.Join(s.Argv, " "), "TestFP6ProcessGroups") {
				t.Fatalf("%s: %s still selects TestFP6ProcessGroups", goos, s.Name)
			}
		}
		for _, s := range steps[16:19] {
			if !slices.Contains(s.Argv, "./internal/spikes/processgroup") || len(s.Argv) != 7 {
				t.Fatalf("%s %s lost the complete processgroup package: %v", goos, s.Name, s.Argv)
			}
		}
		// The plane package (its delegated contracts included) is repeated
		// completely in its own shard (design 05b), never in packages.
		for _, s := range steps[10:13] {
			if !slices.Contains(s.Argv, "./internal/plane") || len(s.Argv) != 7 {
				t.Fatalf("%s %s lost the complete plane package: %v", goos, s.Name, s.Argv)
			}
		}
		if slices.Contains(steps[0].Argv, "./internal/plane") {
			t.Fatalf("%s packages step still repeats plane: %v", goos, steps[0].Argv)
		}
		// Likewise the sidecar package (its task children and reconnect
		// contracts included), in its own shard since the 05b sidecar
		// follow-up.
		for _, s := range steps[13:16] {
			if !slices.Contains(s.Argv, "./internal/sidecar") || len(s.Argv) != 7 {
				t.Fatalf("%s %s lost the complete sidecar package: %v", goos, s.Name, s.Argv)
			}
		}
		if slices.Contains(steps[0].Argv, "./internal/sidecar") {
			t.Fatalf("%s packages step still repeats sidecar: %v", goos, steps[0].Argv)
		}
		// The contract, mcpqual and workspace packages run once per CPU
		// setting in the per-CPU groups (the headroom fixes; the
		// packages-cpu shard's since the stress worker rebalance), never in
		// the combined command; workspacetransfer stays combined.
		for g, pkg := range []string{"./internal/contract", "./internal/mcpqual", "./internal/workspace"} {
			for _, s := range steps[1+3*g : 4+3*g] {
				if !slices.Contains(s.Argv, pkg) || len(s.Argv) != 7 {
					t.Fatalf("%s %s lost the complete %s package: %v", goos, s.Name, pkg, s.Argv)
				}
			}
			if slices.Contains(steps[0].Argv, pkg) {
				t.Fatalf("%s packages step still repeats %s: %v", goos, pkg, steps[0].Argv)
			}
		}
		if !slices.Contains(steps[0].Argv, "./internal/workspacetransfer") {
			t.Fatalf("%s packages step lost workspacetransfer: %v", goos, steps[0].Argv)
		}
	}

	// The real plane tests: exact direct subtests, and every delegated
	// contract call and contract build inside the contracts callbacks only.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(testkit.MustRepoRoot(t), "tests", "function", "plane_trust_test.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || speedSubtests[fd.Name.Name] == nil {
			continue
		}
		parent := fd.Name.Name
		found[parent] = true
		var subs []string
		bodies := map[string]*ast.FuncLit{}
		for _, st := range fd.Body.List {
			if name, fn, ok := tRunName(st); ok {
				subs = append(subs, name)
				bodies[name] = fn
			}
		}
		if !slices.Equal(subs, speedSubtests[parent]) {
			t.Fatalf("%s subtests = %q, want %q", parent, subs, speedSubtests[parent])
		}
		var contracts []string
		builds := 0
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			kind, contract := delegatedCall(call)
			if kind == "" {
				return true
			}
			c := bodies["contracts"]
			if call.Pos() < c.Pos() || call.End() > c.End() {
				t.Fatalf("%s: %s at %s is outside its contracts subtest", parent, kind, fset.Position(call.Pos()))
			}
			if kind == "BuildTestBinary" {
				builds++
			} else {
				contracts = append(contracts, contract)
			}
			return true
		})
		if builds != 1 || !slices.Equal(contracts, speedContracts[parent]) {
			t.Fatalf("%s contracts subtest: %d builds, contracts %q, want 1 build and %q", parent, builds, contracts, speedContracts[parent])
		}
	}
	if len(found) != 3 {
		t.Fatalf("plane parents found: %v", found)
	}

	// Execute the actual selector once against a standard-library-only
	// fixture with the same names, near-prefix neighbours and FP-6.
	var selector string
	steps, _ := devcheck.StressSteps(runtime.GOOS)
	for _, a := range steps[20].Argv {
		if v, ok := strings.CutPrefix(a, "-run="); ok {
			selector = v
		}
	}
	if selector != speedPlaneSelector {
		t.Fatalf("host plane selector = %q", selector)
	}
	ran, logged := runSelectorFixture(t, selector)
	var wantRan []string
	for _, p := range []string{"TestPlaneState", "TestPlaneTLS", "TestPlaneReissue"} {
		wantRan = append(wantRan, p)
		for _, s := range speedSelected {
			if strings.HasPrefix(s, p+"/") {
				wantRan = append(wantRan, s)
			}
		}
	}
	slices.Sort(wantRan)
	if !slices.Equal(ran, wantRan) {
		t.Fatalf("fixture passed %q, want exactly %q", ran, wantRan)
	}
	wantLog := []string{"setup TestPlaneState"}
	for _, s := range speedSelected {
		if s == "TestPlaneTLS/https-only" {
			wantLog = append(wantLog, "setup TestPlaneTLS")
		}
		if s == "TestPlaneReissue/process" {
			wantLog = append(wantLog, "setup TestPlaneReissue")
		}
		wantLog = append(wantLog, "child "+s)
	}
	if !slices.Equal(logged, wantLog) {
		t.Fatalf("fixture log %q, want %q", logged, wantLog)
	}
}

// selectorFixture mirrors the plane parents' subtest names (contracts
// included), plus FP-6, near-prefix and unrelated tests. Every parent logs
// its setup and every child its run to $SELECTOR_FIXTURE_LOG.
const selectorFixture = `package fixture

import (
	"os"
	"testing"
)

func record(t *testing.T, line string) {
	f, err := os.OpenFile(os.Getenv("SELECTOR_FIXTURE_LOG"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func parent(t *testing.T, subs ...string) {
	record(t, "setup "+t.Name())
	for _, s := range subs {
		t.Run(s, func(t *testing.T) { record(t, "child "+t.Name()) })
	}
}

func TestPlaneState(t *testing.T) {
	parent(t, "paths", "persistence", "locking", "validation", "contracts")
}
func TestPlaneTLS(t *testing.T) {
	parent(t, "https-only", "prelisten-validation", "bounded-shutdown", "contracts")
}
func TestPlaneReissue(t *testing.T)    { parent(t, "process", "contracts") }
func TestFP6ProcessGroups(t *testing.T) { parent(t, "cooperative", "resistant", "leader-exits-first") }
func TestPlaneStatus(t *testing.T)     { parent(t, "inspection", "expiry-warnings", "process", "paths") }
func TestPlaneStateExtra(t *testing.T) { parent(t, "paths", "process") }
func TestPlaneInit(t *testing.T)       { parent(t, "issuance", "fingerprint", "restart-invariance") }
func TestFP4TransportHarness(t *testing.T) { parent(t, "paths") }
`

// runSelectorFixture runs go test -json -run=selector once in a temporary
// fixture module and returns the sorted tests with a pass event (after
// checking each has a run event and nothing failed or skipped) and the
// fixture's setup/child log lines in execution order.
func runSelectorFixture(t *testing.T, selector string) (passed, logged []string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":           "module example.com/selectorfixture\n\ngo 1.22\n",
		"selector_test.go": selectorFixture,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(t.TempDir(), "fixture.log")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, testkit.GoTool(), "test", "-json", "-count=1", "-run="+selector, ".")
	cmd.Dir = dir
	// The fixture has no dependencies: inherited build flags and workspaces
	// are dropped, the toolchain stays local and nothing is downloaded.
	cmd.Env = testkit.EnvWithout(os.Environ(), []string{"GOFLAGS", "GOWORK", "GOTOOLCHAIN", "SELECTOR_FIXTURE_LOG"},
		"GOWORK=off", "GOTOOLCHAIN=local", "SELECTOR_FIXTURE_LOG="+logPath)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("fixture go test: %v\n%s\n%s", err, out.String(), errOut.String())
	}
	runs := map[string]bool{}
	dec := json.NewDecoder(&out)
	for dec.More() {
		var ev struct{ Action, Test string }
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("fixture events: %v", err)
		}
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "run":
			runs[ev.Test] = true
		case "pass":
			if !runs[ev.Test] {
				t.Fatalf("fixture: pass without run for %s", ev.Test)
			}
			passed = append(passed, ev.Test)
		case "fail", "skip":
			t.Fatalf("fixture: %s %s", ev.Action, ev.Test)
		}
	}
	for name := range runs {
		if !slices.Contains(passed, name) {
			t.Fatalf("fixture: %s ran without passing", name)
		}
	}
	slices.Sort(passed)
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fixture log: %v", err)
	}
	return passed, strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// FP-2: the actual workflow's main jobs and, since iteration 02c, its
// stress workers (ten since iteration 05b's sidecar follow-up, fourteen
// since design 06a-perf, sixteen since the stress worker rebalance) are
// independent, with
// identical pinned setup and exact check commands; the two summaries
// follow them.
func TestCISpeedJobs(t *testing.T) {
	data := ciWorkflow(t)
	if err := cicheck.ValidateWorkflow(data); err != nil {
		t.Fatalf("checked-in workflow: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	jobs := node(t, &doc, "jobs")
	var ids []string
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		ids = append(ids, jobs.Content[i].Value)
	}
	if strings.Join(ids, " ") != "linux macos linux-stress-packages linux-stress-packages-cpu linux-stress-plane-cpu1 linux-stress-plane linux-stress-sidecar-cpu1 linux-stress-sidecar "+
		"linux-stress-processgroup linux-stress-functions macos-stress-packages macos-stress-packages-cpu macos-stress-plane-cpu1 macos-stress-plane macos-stress-sidecar-cpu1 "+
		"macos-stress-sidecar macos-stress-processgroup macos-stress-functions linux-stress macos-stress" || len(ids) != 20 {
		t.Fatalf("job ids = %v", ids)
	}
	var names []string
	var setup string
	for _, j := range speedJobs {
		job := node(t, &doc, "jobs", j.id)
		var keys []string
		for i := 0; i+1 < len(job.Content); i += 2 {
			keys = append(keys, job.Content[i].Value)
		}
		slices.Sort(keys)
		// No needs, condition, matrix, concurrency, bypass or permission
		// override: exactly the contract's keys.
		if strings.Join(keys, " ") != "defaults env name runs-on steps timeout-minutes" {
			t.Fatalf("%s keys = %v", j.id, keys)
		}
		if node(t, job, "name").Value != j.name || node(t, job, "runs-on").Value != j.runner || node(t, job, "timeout-minutes").Value != j.timeout {
			t.Fatalf("%s identity differs from the table", j.id)
		}
		names = append(names, j.name)
		steps := node(t, job, "steps")
		// The Linux job ends with the always-run container evidence
		// publication (m3-m4-container-e2e), after its check steps.
		extra := 0
		if j.id == "linux" {
			extra = 1
			if ev := node(t, steps, 3+len(j.checks)); node(t, ev, "name").Value != "Publish container E2E evidence" || node(t, ev, "if").Value != "always()" ||
				node(t, ev, "run").Value != "cat /tmp/callsheet-container-e2e-evidence/report.txt" {
				t.Fatalf("linux evidence step = %+v", ev)
			}
		}
		if len(steps.Content) != 3+len(j.checks)+extra {
			t.Fatalf("%s has %d steps", j.id, len(steps.Content))
		}
		// Identical pinned setup in every job.
		var b bytes.Buffer
		enc := yaml.NewEncoder(&b)
		for i := 0; i < 3; i++ {
			if err := enc.Encode(steps.Content[i]); err != nil {
				t.Fatal(err)
			}
		}
		if setup == "" {
			setup = b.String()
		} else if b.String() != setup {
			t.Fatalf("%s setup differs:\n%s\nwant\n%s", j.id, b.String(), setup)
		}
		if co, sg := node(t, steps, 0, "uses"), node(t, steps, 1, "uses"); co.Value != checkoutPin || co.LineComment != "# v6.0.2" || sg.Value != setupGoPin || sg.LineComment != "# v6.3.0" {
			t.Fatalf("%s pins = %s %s", j.id, co.Value, sg.Value)
		}
		for i, want := range j.checks {
			st := steps.Content[3+i]
			if node(t, st, "run").Value != want || node(t, st, "env", "GOPROXY").Value != "off" || node(t, st, "env", "GOSUMDB").Value != "off" ||
				len(node(t, st, "env").Content) != 4 {
				t.Fatalf("%s check %d differs from %q", j.id, i, want)
			}
		}
	}
	if strings.Join(names[:2], ",") != strings.Join(cicheck.RequiredChecks()[:2], ",") || len(names) != 18 {
		t.Fatalf("contexts %v, contract %v", names, cicheck.RequiredChecks())
	}
	for _, top := range []string{"concurrency", "env", "defaults"} {
		if child := func() *yaml.Node {
			root := doc.Content[0]
			for i := 0; i+1 < len(root.Content); i += 2 {
				if root.Content[i].Value == top {
					return root.Content[i+1]
				}
			}
			return nil
		}(); child != nil {
			t.Fatalf("workflow has top-level %s", top)
		}
	}
	// Each stress job removed, or a worker made dependent, is rejected; a
	// removed summary also loses its required context.
	for _, id := range []string{"linux-stress", "macos-stress"} {
		mustReject(t, id+" removed", mutated(t, func(r *yaml.Node) { deleteKey(t, node(t, r, "jobs"), id) }), "jobs."+id+": missing required field")
		mustReject(t, id+" needs", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", id+"-packages"), "needs", "linux") }), "jobs."+id+"-packages.needs: unknown field")
		removed := mutated(t, func(r *yaml.Node) { deleteKey(t, node(t, r, "jobs"), id) })
		if err := cicheck.CheckJobNames(map[string][]byte{"ci.yml": removed}); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("required check %q is not defined", "ci-"+id)) {
			t.Fatalf("%s removed: job names %v", id, err)
		}
	}
	mustReject(t, "main job waits for stress", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", "linux"), "needs", "linux-stress") }), "jobs.linux.needs: unknown field")
}

// qualifyingStream is a complete synthetic native stream for every
// required name except drop.
func qualifyingStream(drop string) string {
	pkg := devcheck.NativePackage
	evs := []map[string]any{synth("start", pkg, "")}
	for _, name := range append(append(append(append(append(append(append(append(append(append(append(append(append(append(append(append(append(append(append(slices.Clone(speedNative), speedNodeNative...), speedRoleNative...), speedTaskNative...), speedControlNative...), speedMCPNative...), speedQualNative...), speedRealNative...), speedWorkspaceNative...), speedTransferNative...), speedLatencyNative...), speedWorkspaceTaskNative...), speedWorkspaceDoorNative...), speedWave2Native...), speedNBWNative...), speedDCENative...), speedB1Native...), speedB15Native...), speedB2Native...), speedB3Native...) {
		if name != drop {
			evs = append(evs, synth("run", pkg, name), synth("pass", pkg, name))
		}
	}
	evs = append(evs, synth("pass", pkg, ""))
	return events(append(evs, taskProcessEvents(drop)...)...)
}

// FP-3: validator, plans, native evidence and documentation agree.
func TestCISpeedPolicy(t *testing.T) {
	t.Run("native", func(t *testing.T) {
		if got := devcheck.NativeRequiredTests(); len(got) != 429 || len(speedQualNative) != 51 || len(speedRealNative) != 39 || len(speedWave2Native) != 42 || !slices.Equal(got[419:424], speedB2Native) ||
			!slices.Equal(got[424:429], speedB3Native) ||
			!slices.Equal(got[353:395], speedWave2Native) || !slices.Equal(got[395:403], speedNBWNative) || !slices.Equal(got[403:412], speedDCENative) || !slices.Equal(got[412:416], speedB1Native) ||
			!slices.Equal(got[416:419], speedB15Native) || !slices.Equal(got[:28], speedNative) || !slices.Equal(got[28:58], speedNodeNative) || !slices.Equal(got[58:87], speedRoleNative) || !slices.Equal(got[87:128], speedTaskNative) ||
			!slices.Equal(got[128:145], speedControlNative) || !slices.Equal(got[145:222], speedMCPNative) || !slices.Equal(got[222:273], speedQualNative) || !slices.Equal(got[273:312], speedRealNative) ||
			!slices.Equal(got[312:323], speedWorkspaceNative) || !slices.Equal(got[323:333], speedTransferNative) || !slices.Equal(got[333:335], speedLatencyNative) ||
			!slices.Equal(got[335:347], speedWorkspaceTaskNative) || !slices.Equal(got[347:353], speedWorkspaceDoorNative) {
			t.Fatalf("native required = %v", got)
		}
		if err := devcheck.CheckNativeResults("darwin", strings.NewReader(qualifyingStream(""))); err != nil {
			t.Fatalf("complete evidence: %v", err)
		}
		for _, name := range []string{"TestPlaneState/contracts", "TestPlaneTLS/contracts", "TestPlaneReissue/process", "TestPlaneReissue/contracts"} {
			err := devcheck.CheckNativeResults("darwin", strings.NewReader(qualifyingStream(name)))
			if err == nil || !strings.Contains(err.Error(), name+" has no run event") || !strings.Contains(err.Error(), "native qualification unobserved") {
				t.Fatalf("missing %s: %v", name, err)
			}
		}
		for _, name := range []string{"TestTaskCommands/trust", "TestTaskExecution/process"} {
			err := devcheck.CheckNativeResults("darwin", strings.NewReader(qualifyingStream(name)))
			if err == nil || !strings.Contains(err.Error(), name+" has no run event") || !strings.Contains(err.Error(), "native qualification unobserved") {
				t.Fatalf("missing %s: %v", name, err)
			}
		}
		// The sidecar tuple is its own package's evidence.
		if !slices.Equal(devcheck.NativeTaskProcessTests(), speedTaskProcess) {
			t.Fatalf("sidecar tuple = %v", devcheck.NativeTaskProcessTests())
		}
		err := devcheck.CheckNativeResults("darwin", strings.NewReader(qualifyingStream("TestTaskExecutionContract/process")))
		if err == nil || !strings.Contains(err.Error(), "TestTaskExecutionContract/process in "+devcheck.NativeTaskProcessPackage+" has no run event") {
			t.Fatalf("missing sidecar tuple: %v", err)
		}
		fixture := string(repoFile(t, "internal/devcheck/testdata/cli-go-test.jsonl"))
		if err := devcheck.CheckNativeResults("darwin", strings.NewReader(fixture)); err == nil || !strings.Contains(err.Error(), "TestPlaneReissue/contracts has no run event") {
			t.Fatalf("recorded CLI-only fixture: %v", err)
		}
	})
	t.Run("workflow", func(t *testing.T) {
		data := ciWorkflow(t)
		if err := cicheck.ValidateWorkflow(data); err != nil {
			t.Fatal(err)
		}
		stages, err := cicheck.ExtractStages(data)
		if err != nil {
			t.Fatal(err)
		}
		contract := cicheck.Jobs()
		if len(contract) != len(speedJobs)+2 || len(stages) != len(speedJobs)+2 {
			t.Fatalf("contract %d jobs, workflow %d, table %d plus two summaries", len(contract), len(stages), len(speedJobs))
		}
		for i, j := range speedJobs {
			var want []string
			for _, c := range j.checks {
				want = append(want, strings.TrimPrefix(c, "go run ./cmd/devcheck "))
			}
			c := contract[i]
			if c.ID != j.id || c.Name != j.name || c.RunsOn != j.runner || fmt.Sprint(c.TimeoutMinutes) != j.timeout ||
				!slices.Equal(c.Stages, want) || !slices.Equal(stages[j.id], want) {
				t.Fatalf("%s: contract %+v, workflow %v, want %v", j.id, c, stages[j.id], want)
			}
		}
		// The complete local stress stage dispatches the whole plan, with the
		// plane and sidecar CPU1 singletons alone, their CPU2/CPU4 pairs and
		// processgroup's three invocations concurrent (each compared as a
		// set; design 06a-perf), the packages-cpu groups right after the
		// combined packages command (the stress worker rebalance).
		r := &ciRunner{}
		if code, out, errOut := devcheckRun(t, r, "stress"); code != 0 || !strings.Contains(out, "stage stress ok") ||
			!sameGroups(r.calls, [][]string{{speedPackages}, {speedContract1, speedContract2, speedContract4},
				{speedMcpqual1, speedMcpqual2, speedMcpqual4}, {speedWorkspace1, speedWorkspace2, speedWorkspace4}, {speedPlane1}, {speedPlane2, speedPlane4}, {speedSidecar1}, {speedSidecar2, speedSidecar4},
				{speedPG1, speedPG2, speedPG4}, {speedFunction}, {speedPlaneFunction}, {speedNodeFunction}}) {
			t.Fatalf("stress dispatch = %d %v %s", code, r.calls, errOut)
		}
		// Drift: stress back in a main job is rejected.
		mustReject(t, "stress in ci-macos", mutated(t, func(r *yaml.Node) {
			steps := node(t, r, "jobs", "macos", "steps")
			steps.Content = append(steps.Content, node(t, r, "jobs", "macos-stress-packages", "steps", 3))
		}), "jobs.macos.steps[4]: unexpected extra step")
	})
	t.Run("docs", func(t *testing.T) {
		linux, _ := devcheck.StressSteps("linux")
		darwin, _ := devcheck.StressSteps("darwin")
		stress := docSection(t, "Stress checks")
		for i := range linux {
			l, d := strings.Join(linux[i].Argv, " "), strings.Join(darwin[i].Argv, " ")
			if l != d {
				t.Fatalf("plans differ at step %d: %s / %s", i, l, d)
			}
			if !strings.Contains(stress, "\n"+l+"\n") {
				t.Fatalf("Stress checks lacks the command %q", l)
			}
		}
		requireTerms(t, "Stress checks", stress, "`stress packages`", "`stress contract cpu1`", "`stress mcpqual cpu1`", "`stress workspace cpu1`", "`stress plane cpu1`", "`stress sidecar cpu1`", "`stress processgroup cpu1`", "`stress function`", "`stress plane function`",
			"`TestFP6ProcessGroups`", "`TestExperiment`", "`TestNativeStateContract`", "`TestStateFailureContract`",
			"`TestServerFailureContract`", "`TestReissueFailureContract`", "`contracts`", "`process`",
			"quote the entire `-run` argument")
		checks := docSection(t, "Checks")
		for _, j := range speedJobs {
			requireTerms(t, "Checks", checks, fmt.Sprintf("| `%s` | `%s` | %s min |", j.name, j.runner, j.timeout))
		}
		for _, name := range []string{"ci-linux-stress", "ci-macos-stress"} {
			requireTerms(t, "Checks", checks, fmt.Sprintf("| `%s` | `ubuntu-24.04` | 5 min |", name))
		}
		for _, name := range speedNative {
			requireTerms(t, "Checks", checks, "`"+name[strings.LastIndex(name, "/")+1:]+"`")
		}
		local := docSection(t, "Local verification")
		requireTerms(t, "Local verification", local, "go run ./cmd/devcheck all", "go run ./cmd/devcheck stress",
			"go test -json -count=1 -run '"+speedPlaneSelector+"' ./tests/function")
	})
}

// FP-4: the documented timing evidence and the owner-operated handoff for
// pull request #2. This proves instructions, not remote state.
func TestCISpeedHandoff(t *testing.T) {
	bp := docSection(t, "Branch protection")
	requireTerms(t, "Branch protection", bp,
		"`ci-linux`, `ci-macos`, `ci-linux-stress`, `ci-macos-stress`",
		"owner-only", "never executed by CI, tests or this flow",
		"https://docs.github.com/en/rest/branches/branch-protection#add-status-check-contexts",
		"preserving the existing contexts",
		"gh api repos/wedevwork/callsheet/branches/main/protection",
		"before and after", "all four contexts", "strict", "enforce_admins", "rulesets",
		"stop the handoff", "A green workflow alone does not prove protection")
	if !strings.Contains(bp, "\n"+ownerAddContexts) {
		t.Fatalf("Branch protection lacks the exact additive command:\n%s", ownerAddContexts)
	}
	const sub = "\n### Adding the stress contexts (pull request #2)\n"
	i := strings.Index(bp, sub)
	if i < 0 {
		t.Fatalf("Branch protection lacks %q", strings.TrimSpace(sub))
	}
	handoff := bp[i+len(sub):]
	if j := strings.Index(handoff, "\n### "); j >= 0 {
		handoff = handoff[:j]
	}
	steps := numberedSteps(t, handoff)
	review := stepIndex(t, steps, "REVIEW_APPROVED", "commit the reviewed code")
	push := stepIndex(t, steps, "Push `iter-02-plane-trust`")
	green := stepIndex(t, steps, "all ten jobs", "`ci-linux-stress`", "`ci-macos-stress`", "current PR merge revision")
	add := stepIndex(t, steps, "owner adds", "`ci-linux-stress`", "`ci-macos-stress`", "after they have reported")
	verify := stepIndex(t, steps, "owner verifies", "read-only")
	merge := stepIndex(t, steps, "Merge iterations 02, 02b and 02c together", "all four checks green")
	if !(review < push && push < green && green < add && add < verify && verify < merge) {
		t.Fatalf("handoff order review=%d push=%d green=%d add=%d verify=%d merge=%d", review, push, green, add, verify, merge)
	}
	requireTerms(t, "handoff", handoff, "skipped, canceled, pending or unobserved result qualifies",
		"later push requires fresh current-revision evidence", "pull request #1 protection handoff remains a prerequisite")
	requireTerms(t, "PR flow", docSection(t, "PR flow"), "iterations 02, 02b and 02c join pull request #2")

	// Timing provenance: the 02b-era hosted baseline, local measurement,
	// estimate and target stay as history; iteration 02c's measured hosted
	// baseline (run 36236333755) supersedes 02b's pending language.
	stress := docSection(t, "Stress checks")
	requireTerms(t, "Stress checks", stress,
		"`ci-linux` 685 s", "stress 576 s", "`ci-macos` 889 s", "stress 806 s",
		"5–6 minutes", "an optimization target, not a measurement",
		"Measured with iteration 02b (deduplicated)", "go1.26.4 linux/amd64",
		"Expected per-job wall-clock after iteration 02b", "estimate", "806 s of the 900 s watchdog",
		"since measured by run 36236333755")
	if strings.Contains(stress, "pending until the first `ci-macos-stress` run of pull request #2") {
		t.Fatal("Stress checks keeps 02b's superseded pending first-run language")
	}
	first := docSection(t, "First remote run")
	requireTerms(t, "First remote run", first, "critical path", "actual job and step times",
		"compare each worker with the expected per-job wall-clock")
	// Nothing in the workflow can change repository settings.
	wf := string(ciWorkflow(t))
	for _, forbidden := range []string{"gh ", "--method", "POST", "curl", "protection", "contents: write", "secrets."} {
		if strings.Contains(wf, forbidden) {
			t.Fatalf("workflow contains %q", forbidden)
		}
	}
}
