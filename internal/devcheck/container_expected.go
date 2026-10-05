package devcheck

// The container acceptance's static per-task expected catalog (design
// m3-m4-container-e2e, expected-catalog.md). It is hand-authored from the
// scenarios, never generated from observed records or scenario output, and
// it is read-only: ValidateContainerEvidence audits every case record
// against it, even a passing one. It holds metadata expectations only; the
// scenarios keep their own pull, content and parent assertions.

// Container case IDs: the eleven container parents and the three
// partial-result subcases, in test order.
const (
	CaseRuntime            = "TestContainerRuntime"
	CaseCoordinator        = "TestContainerCoordinator"
	CaseSampleFlow         = "TestContainerSampleFlow"
	CasePublication        = "TestContainerPublication"
	CaseContinuation       = "TestContainerContinuation"
	CasePartialResults     = "TestContainerPartialResults"
	CasePartialFailed      = "TestContainerPartialResults/failed"
	CasePartialCancelled   = "TestContainerPartialResults/cancelled"
	CasePartialTimedOut    = "TestContainerPartialResults/timed_out"
	CaseLost               = "TestContainerLost"
	CasePublicationRestart = "TestContainerPublicationRestart"
	CaseDirtyPull          = "TestContainerDirtyPull"
	CaseClaims             = "TestContainerClaims"
	CaseEvidence           = "TestContainerEvidence"
)

// containerCaseIDs is the exact required case manifest (14 IDs).
var containerCaseIDs = []string{
	CaseRuntime, CaseCoordinator, CasePublication, CaseContinuation, CasePartialResults,
	CasePartialFailed, CasePartialCancelled, CasePartialTimedOut, CaseLost, CasePublicationRestart,
	CaseDirtyPull, CaseClaims, CaseEvidence, CaseSampleFlow,
}

// containerSubcases are the mandatory nested subtests of each parent that
// has them; their outcomes are recorded in the parent's subcases map.
var containerSubcases = map[string][]string{
	CaseCoordinator:    {"prepare", "goal_answer"},
	CaseContinuation:   {"continuation", "sibling"},
	CasePartialResults: {"failed", "cancelled", "timed_out"},
}

// ContainerCaseIDs returns a fresh copy of the 14 required case IDs.
func ContainerCaseIDs() []string { return append([]string(nil), containerCaseIDs...) }

// ContainerSubcases returns a fresh copy of the required subcase manifest.
func ContainerSubcases() map[string][]string {
	out := map[string][]string{}
	for k, v := range containerSubcases {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// Profiles expand to exact values (expected-catalog.md, profile table).
const (
	profileAnswer    = "answer"
	profilePublished = "published"
	profileFailed    = "failed"
	profileCancelled = "cancelled"
	profileTimedOut  = "timed_out"
	profileLost      = "lost"
)

// FakeFinalMessage is the fake adapter's fixed decoded final answer.
const FakeFinalMessage = "fake task completed"

// taskProfile is one profile's exact expectations. A nil pointer means the
// field must be JSON null.
type taskProfile struct {
	state       string
	publication *string
	exit        *int
	final       *string
	// instanceBase and resultRef: whether workspace_instance/base_commit and
	// result_commit/result_ref are non-null.
	instanceBase, resultRef bool
}

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

var taskProfiles = map[string]taskProfile{
	profileAnswer:    {state: "succeeded", exit: intp(0), final: strp(FakeFinalMessage)},
	profilePublished: {state: "succeeded", publication: strp("published"), exit: intp(0), final: strp(FakeFinalMessage), instanceBase: true, resultRef: true},
	profileFailed:    {state: "failed", publication: strp("published"), exit: intp(3), instanceBase: true, resultRef: true},
	profileCancelled: {state: "cancelled", publication: strp("published"), instanceBase: true, resultRef: true},
	profileTimedOut:  {state: "timed_out", publication: strp("published"), instanceBase: true, resultRef: true},
	profileLost:      {state: "lost", publication: strp("not_applicable"), instanceBase: true},
}

// expectedTask is one catalog row: label / role_id / node alias / profile.
type expectedTask struct {
	label, role, alias, profile string
}

// containerCatalog is the ordered task catalog per case ID; a case absent
// from it (or listed empty) must carry an empty tasks array.
var containerCatalog = map[string][]expectedTask{
	CaseCoordinator: {
		{"answer-a", "proof-a", "a", profileAnswer},
		{"answer-b", "proof-b", "b", profileAnswer},
	},
	CaseSampleFlow: {
		{"design", "proof-a", "a", profilePublished},
		{"design-review", "proof-b", "b", profilePublished},
		{"code", "proof-code", "a", profilePublished},
		{"code-review", "proof-code-review", "b", profilePublished},
	},
	CasePublication: {{"A", "proof-a", "a", profilePublished}},
	CaseContinuation: {
		{"A", "proof-a", "a", profilePublished},
		{"B", "proof-b", "b", profilePublished},
		{"sibling", "proof-a", "a", profilePublished},
	},
	CasePartialResults:     {},
	CasePartialFailed:      {{"failed", "proof-a", "a", profileFailed}},
	CasePartialCancelled:   {{"cancelled", "proof-a", "a", profileCancelled}},
	CasePartialTimedOut:    {{"timed_out", "proof-a", "a", profileTimedOut}},
	CaseLost:               {{"lost", "proof-a", "a", profileLost}, {"recovered", "proof-a", "a", profilePublished}},
	CasePublicationRestart: {{"restart", "proof-a", "a", profilePublished}, {"fresh", "proof-a", "a", profilePublished}},
	CaseDirtyPull:          {{"B", "proof-b", "b", profilePublished}},
	CaseRuntime:            {},
	CaseClaims:             {},
	CaseEvidence:           {},
}

// Boundary labels: the named observation boundary of each case record
// ("" where none applies). They are fixed scenario labels, compared
// exactly.
const (
	BoundaryRegistryRestart    = "registry_revision_restart_node_a_before_dispatch"
	BoundarySiblingWhileHeld   = "sibling_while_continuation_held"
	BoundaryWrittenBeforeStop  = "written_barrier_before_stop"
	BoundaryLostBeforeReturn   = "sidecar_restart_after_written_barrier_before_worker_return"
	BoundaryPublicationRestart = "after_acknowledged_publication_before_coordinator_delivery"
)

var containerBoundaries = map[string]string{
	CaseCoordinator:        BoundaryRegistryRestart,
	CaseContinuation:       BoundarySiblingWhileHeld,
	CasePartialCancelled:   BoundaryWrittenBeforeStop,
	CasePartialTimedOut:    BoundaryWrittenBeforeStop,
	CaseLost:               BoundaryLostBeforeReturn,
	CasePublicationRestart: BoundaryPublicationRestart,
}

// ContainerBoundary returns caseID's fixed boundary label ("" when none).
func ContainerBoundary(caseID string) string { return containerBoundaries[caseID] }

// containerRelation names the in-case chain checks of a case.
type containerRelation int

const (
	relNone containerRelation = iota
	// relChain: all instances equal; task i's base is task i-1's result.
	relChain
	// relContinuation: all three instances equal; B.base = A.result and
	// sibling.base = A.base.
	relContinuation
	// relSameBase: both tasks share instance and base.
	relSameBase
)

var containerRelations = map[string]containerRelation{
	CaseSampleFlow:         relChain,
	CaseContinuation:       relContinuation,
	CaseLost:               relSameBase,
	CasePublicationRestart: relSameBase,
}
