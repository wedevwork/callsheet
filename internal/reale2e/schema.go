package reale2e

import (
	"fmt"
	"regexp"
	"strings"
)

// Evidence documents (design 12a-real-e2e, Evidence and checker, A3
// Checker migration). The live collector writes them and the offline
// checker reads them strictly (unknown fields, duplicate keys and unknown
// schema identifiers are refused before any semantic check). A3 bumps the
// manifest, launch, event-map, cleanup, coordinator-exit and report
// schemas to v2; the task, dispatch, wait, decision, tree, test-run, log
// and roles formats are unchanged; the environment, validation and audit
// records are new (v1). A version 1 manifest is unsupported. A3.6 bumps
// the manifest, cleanup, snapshot and environment-diff schemas to v3 (the
// ownership classifier and started-instance accounting); their earlier
// versions are unsupported, never migrated.

// Schema identifiers.
const (
	ManifestSchema     = "callsheet-real-e2e/v3"
	ManifestSchemaV1   = "callsheet-real-e2e/v1"
	LaunchSchema       = "callsheet-real-e2e-launch/v2"
	RolesSchema        = "callsheet-real-e2e-roles/v1"
	DispatchSchema     = "callsheet-real-e2e-dispatch/v1"
	WaitSchema         = "callsheet-real-e2e-wait/v1"
	DecisionSchema     = "callsheet-real-e2e-decision/v1"
	TreeSchema         = "callsheet-real-e2e-trees/v1"
	TestRunSchema      = "callsheet-real-e2e-test-run/v1"
	CleanupSchema      = "callsheet-real-e2e-cleanup/v3"
	EventMapSchema     = "callsheet-real-e2e-event-map/v2"
	CoordExitSchema    = "callsheet-real-e2e-coordinator-exit/v2"
	LogMetaSchema      = "callsheet-real-e2e-log/v1"
	ReportSchema       = "callsheet-real-e2e-report/v2"
	SnapshotSchema     = "callsheet-real-e2e-snapshot/v3"
	DiffSchema         = "callsheet-real-e2e-environment-diff/v3"
	ExecSchema         = "callsheet-real-e2e-exec/v1"
	CasesSchema        = "callsheet-real-e2e-cases/v1"
	RequirementsSchema = "callsheet-real-e2e-requirements/v1"
	AuditSchema        = "callsheet-real-e2e-audit/v1"
)

// runIDRE is a run ID: UTC start second and 8 random hex digits.
var runIDRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)

// ValidRunID reports whether s is a run ID.
func ValidRunID(s string) bool { return runIDRE.MatchString(s) }

// sessionIDRE is a lowercase canonical UUID.
var sessionIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// hex40 and hex64 are full Git and SHA-256 hashes.
var (
	hex40RE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hex64RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// GateRecord is the startup gate's observed result.
type GateRecord struct {
	OptIn    bool   `json:"opt_in"`
	CIAbsent bool   `json:"ci_absent"`
	Platform string `json:"platform"`
}

// VersionRecord is one executable's probe: its tokenized path, version
// output and the eligibility decision of the existing adapter policy (or
// invocability for go and git).
type VersionRecord struct {
	Tool     string `json:"tool"`
	Path     string `json:"path"`
	Version  string `json:"version"`
	Eligible bool   `json:"eligible"`
}

// WorkspaceRecord is the pinned workspace and its seed.
type WorkspaceRecord struct {
	Name       string `json:"name"`
	Instance   string `json:"instance"`
	SeedCommit string `json:"seed_commit"`
	SeedTree   string `json:"seed_tree"`
}

// FileHash names a file and its SHA-256.
type FileHash struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Manifest is manifest.json.
type Manifest struct {
	Schema           string          `json:"schema"`
	RunID            string          `json:"run_id"`
	Revision         string          `json:"revision"`
	CallsheetVersion string          `json:"callsheet_version"`
	GOOS             string          `json:"goos"`
	GOARCH           string          `json:"goarch"`
	Gate             GateRecord      `json:"gate"`
	Versions         []VersionRecord `json:"versions"`
	FlowKind         string          `json:"flow_kind"`
	FlowSHA256       string          `json:"flow_sha256"`
	Roles            []FlowRole      `json:"roles"`
	Workspace        WorkspaceRecord `json:"workspace"`
	Templates        []FileHash      `json:"templates"`
	Rendered         []FileHash      `json:"rendered"`
	// Launches are the coordinator launch records written, in phase order.
	Launches []FileHash `json:"launches"`
	// ProtectedRoots are the canonical supplied --protect-root values,
	// sorted, tokenized as in the environment snapshots.
	ProtectedRoots []string `json:"protected_roots"`
	// ProtectDepth is the discovery depth below each root (A3.2).
	ProtectDepth int    `json:"protect_depth"`
	StartedAt    string `json:"started_at"`
}

// LaunchRecord is setup/launch-phase-N.json: one coordinator phase's exact
// launch (private evidence) and its sanitized rendering. Argv is the real
// argument vector after the executable (prompts included); Display
// replaces local prefixes with tokens and each prompt with its hash.
type LaunchRecord struct {
	Schema        string   `json:"schema"`
	Phase         int      `json:"phase"`
	Executable    string   `json:"executable"`
	Argv          []string `json:"argv"`
	Cwd           string   `json:"cwd"`
	Display       []string `json:"display"`
	ResumeSession string   `json:"resume_session"`
	PromptSHA256  string   `json:"prompt_sha256"`
	SystemSHA256  string   `json:"system_sha256"`
	StartedSeq    int      `json:"started_seq"`
	// WaitTemplates are the four rendered per-hop background wait
	// commands (TASK_ID where the coordinator substitutes the task ID).
	WaitTemplates []string `json:"wait_templates"`
}

// RoleSnapshot is one registered role as the plane reported it once ready.
type RoleSnapshot struct {
	ID          string `json:"id"`
	Node        string `json:"node"`
	Adapter     string `json:"adapter"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	Concurrency int    `json:"concurrency"`
	Timeout     string `json:"timeout"`
	CanAccept   bool   `json:"can_accept"`
}

// RolesRecord is setup/roles.json.
type RolesRecord struct {
	Schema  string         `json:"schema"`
	ReadyAt string         `json:"ready_at"`
	Roles   []RoleSnapshot `json:"roles"`
}

// InlineFile is one labelled file of the code reviewer's goal.
type InlineFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// DispatchRecord is hops/NN/dispatch.json, written when the coordinator's
// observation was accepted.
type DispatchRecord struct {
	Schema      string       `json:"schema"`
	Hop         string       `json:"hop"`
	Task        string       `json:"task"`
	ObservedSeq int          `json:"observed_seq"`
	RoleID      string       `json:"role_id"`
	Workspace   string       `json:"workspace"`
	Instance    string       `json:"instance"`
	BaseCommit  string       `json:"base_commit"`
	Model       string       `json:"model"`
	Effort      string       `json:"effort"`
	Marker      string       `json:"marker"`
	InlineFiles []InlineFile `json:"inline_files"`
}

// WaitRecord is hops/NN/wait.json: the coordinator's background wait as
// its own redirected output recorded it.
type WaitRecord struct {
	Schema       string `json:"schema"`
	Hop          string `json:"hop"`
	Handle       string `json:"handle"`
	Task         string `json:"task"`
	ExitCode     *int   `json:"exit_code"`
	ResultSHA256 string `json:"result_sha256"`
	Winner       string `json:"winner"`
	WinnerState  string `json:"winner_state"`
	StderrBytes  int    `json:"stderr_bytes"`
}

// LogMeta is hops/NN/logs.json: the stored log's bounds.
type LogMeta struct {
	Schema          string `json:"schema"`
	SourceBytes     int    `json:"source_bytes"`
	RetainedBytes   int    `json:"retained_bytes"`
	StoredBytes     int    `json:"stored_bytes"`
	PlaneTruncated  bool   `json:"plane_truncated"`
	PlaneIncomplete bool   `json:"plane_incomplete"`
	StoredTruncated bool   `json:"stored_truncated"`
}

// DecisionRecord is owner-decision.json.
type DecisionRecord struct {
	Schema       string `json:"schema"`
	RunID        string `json:"run_id"`
	Decision     string `json:"decision"`
	ReviewedTask string `json:"reviewed_task"`
	ResultCommit string `json:"result_commit"`
	DesignSHA256 string `json:"design_sha256"`
	ReviewSHA256 string `json:"review_sha256"`
	Seq          int    `json:"seq"`
	DecidedAt    string `json:"decided_at"`
}

// Owner decisions.
const (
	DecisionYes = "yes"
	DecisionNo  = "no"
)

// HopTree is one published hop result in the tree manifest.
type HopTree struct {
	Hop    string `json:"hop"`
	Task   string `json:"task"`
	Commit string `json:"commit"`
	Parent string `json:"parent"`
	Tree   string `json:"tree"`
}

// TreeManifest is workspace/tree-manifest.json.
type TreeManifest struct {
	Schema     string    `json:"schema"`
	SeedCommit string    `json:"seed_commit"`
	SeedTree   string    `json:"seed_tree"`
	Hops       []HopTree `json:"hops"`
}

// TestRun is validation/baseline.json or validation/final-test.json.
type TestRun struct {
	Schema     string   `json:"schema"`
	Argv       []string `json:"argv"`
	Env        []string `json:"env"`
	Commit     string   `json:"commit"`
	Tree       string   `json:"tree"`
	Toolchain  string   `json:"toolchain"`
	ExitCode   int      `json:"exit_code"`
	Signal     string   `json:"signal"`
	TimedOut   bool     `json:"timed_out"`
	DurationMS int64    `json:"duration_ms"`
	Files      []string `json:"files"`
}

// PhaseProcess is one coordinator phase's supervisor-observed process:
// its captured PID (also its group), whether the leader was reaped and
// the whole group was observed gone, and its exit.
type PhaseProcess struct {
	Phase      int    `json:"phase"`
	PID        int    `json:"pid"`
	Reaped     bool   `json:"reaped"`
	GroupGone  bool   `json:"group_gone"`
	ExitCode   *int   `json:"exit_code"`
	Signal     string `json:"signal"`
	StoppedBy  string `json:"stopped_by"`
	StartedSeq int    `json:"started_seq"`
	ExitedSeq  int    `json:"exited_seq"`
}

// CoordinatorExit is session/coordinator-exit.json: the supervisor's own
// observation of the coordinator phases it started (never owner-written).
type CoordinatorExit struct {
	Schema string         `json:"schema"`
	Phases []PhaseProcess `json:"phases"`
}

// CleanupRecord is cleanup.json.
type CleanupRecord struct {
	Schema         string        `json:"schema"`
	Started        []StartedProc `json:"started"`
	Processes      []ProcRecord  `json:"processes"`
	CancelledTasks []string      `json:"cancelled_tasks"`
	UnsettledTasks []string      `json:"unsettled_tasks"`
	RuntimeRemoved bool          `json:"runtime_removed"`
	StartedAt      string        `json:"started_at"`
	FinishedAt     string        `json:"finished_at"`
	Complete       bool          `json:"complete"`
}

// SourceRef points at an inclusive 1-based line span of a fixed bundle
// file with that file's SHA-256.
type SourceRef struct {
	File      string `json:"file"`
	SHA256    string `json:"sha256"`
	FirstLine int    `json:"first_line"`
	LastLine  int    `json:"last_line"`
}

// MapEntry is one machine-derived session fact.
type MapEntry struct {
	Kind      string `json:"kind"`
	Phase     int    `json:"phase"`
	SessionID string `json:"session_id"`
	Hop       string `json:"hop"`
	Task      string `json:"task"`
	ToolUseID string `json:"tool_use_id"`
	Handle    string `json:"handle"`
	Seq       int    `json:"seq"`
	// Automatic marks a wake consumed with no ordinary user input between.
	Automatic bool        `json:"automatic"`
	Sources   []SourceRef `json:"sources"`
}

// MapFinding is a derivation finding: a fixed code, the tool-use it is
// linked to (when any) and its sources.
type MapFinding struct {
	Code      string      `json:"code"`
	ToolUseID string      `json:"tool_use_id"`
	Tool      string      `json:"tool"`
	Sources   []SourceRef `json:"sources"`
}

// EventMap is session/event-map.json, derived by the supervisor from the
// raw phase streams, transcript snapshots, launches and its own events
// (never attested).
type EventMap struct {
	Schema    string       `json:"schema"`
	RunID     string       `json:"run_id"`
	SessionID string       `json:"session_id"`
	Source    string       `json:"source"`
	Entries   []MapEntry   `json:"entries"`
	Findings  []MapFinding `json:"findings"`
	Init      []InitRecord `json:"init"`
}

// InitRecord is one phase's loaded-configuration evidence from its init
// record. The setup criterion requires PermissionMode auto for this
// harness recipe, including a repeated init of the same session (a repeat
// that differs is session_mismatch). The other fields stay plain evidence.
type InitRecord struct {
	Phase          int      `json:"phase"`
	SessionID      string   `json:"session_id"`
	Model          string   `json:"model"`
	PermissionMode string   `json:"permission_mode"`
	Tools          []string `json:"tools"`
	MCPServers     []string `json:"mcp_servers"`
}

// MapSourceDerived is the event map's only source.
const MapSourceDerived = "machine-derived"

// Event-map kinds.
const (
	MapLaunch       = "launch"
	MapDispatch     = "dispatch"
	MapWaitStart    = "wait_start"
	MapWake         = "wake"
	MapWaitConsumed = "wait_consumed"
	MapPause        = "pause"
	MapDecision     = "decision"
	MapResume       = "resume"
	MapFinalResult  = "final_result"
)

// ExecRecord binds one final-validation command: argv, input tree and
// commit, output hashes, the supervisor sequence of its start and end,
// its exit and the deadline it ran under.
type ExecRecord struct {
	Schema       string   `json:"schema"`
	Name         string   `json:"name"`
	Argv         []string `json:"argv"`
	Commit       string   `json:"commit"`
	Tree         string   `json:"tree"`
	StdoutSHA256 string   `json:"stdout_sha256"`
	StderrSHA256 string   `json:"stderr_sha256"`
	StdoutBytes  int      `json:"stdout_bytes"`
	StderrBytes  int      `json:"stderr_bytes"`
	Truncated    bool     `json:"truncated"`
	StartSeq     int      `json:"start_seq"`
	EndSeq       int      `json:"end_seq"`
	ExitCode     int      `json:"exit_code"`
	Signal       string   `json:"signal"`
	TimedOut     bool     `json:"timed_out"`
	DeadlineMS   int64    `json:"deadline_ms"`
	DurationMS   int64    `json:"duration_ms"`
	Started      bool     `json:"started"`
}

// CaseResult is one CLI feature case run against the built program.
type CaseResult struct {
	ID     string     `json:"id"`
	Exec   ExecRecord `json:"exec"`
	Stdout string     `json:"stdout"`
	Stderr string     `json:"stderr"`
	Pass   bool       `json:"pass"`
	Code   string     `json:"code"`
}

// CasesRecord is validation/cases.json.
type CasesRecord struct {
	Schema string       `json:"schema"`
	Commit string       `json:"commit"`
	Tree   string       `json:"tree"`
	Build  ExecRecord   `json:"build"`
	Cases  []CaseResult `json:"cases"`
}

// RequirementRow maps one feature case to its committed evidence.
type RequirementRow struct {
	CaseID       string `json:"case_id"`
	RequestPath  string `json:"request_path"`
	RequestSHA   string `json:"request_sha256"`
	DesignPath   string `json:"design_path"`
	DesignSHA    string `json:"design_sha256"`
	MainSHA      string `json:"main_sha256"`
	TestSHA      string `json:"test_sha256"`
	TestName     string `json:"test_name"`
	TestLines    []int  `json:"test_lines"`
	TestResult   string `json:"test_result"`
	CLICase      string `json:"cli_case"`
	CLIPass      bool   `json:"cli_pass"`
	MappingFound bool   `json:"mapping_found"`
}

// RequirementsRecord is validation/requirements.json: the supervisor's
// requirement-to-test map plus the source-structure findings.
type RequirementsRecord struct {
	Schema        string           `json:"schema"`
	Commit        string           `json:"commit"`
	TestRun       ExecRecord       `json:"test_run"`
	RunSignature  bool             `json:"run_signature"`
	TableTests    bool             `json:"table_tests"`
	TestCallsRun  bool             `json:"test_calls_run"`
	Rows          []RequirementRow `json:"rows"`
	MissingTests  []string         `json:"missing_tests"`
	FailedTests   []string         `json:"failed_tests"`
	ParseFindings []string         `json:"parse_findings"`
	// TestRunPassed: the whole go test -json run exited 0 cleanly,
	// untruncated, with a well-formed stream and no failing test or
	// package; TestFailures lists every failing test and package.
	TestRunPassed bool     `json:"test_run_passed"`
	TestFailures  []string `json:"test_failures"`
}

// AuditRef is one audit evidence pointer.
type AuditRef struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Lines  []int  `json:"lines"`
	Task   string `json:"task"`
	Commit string `json:"commit"`
}

// AuditRow is one ordered audit row.
type AuditRow struct {
	ID          string     `json:"id"`
	FPs         []string   `json:"fps"`
	Layer       string     `json:"layer"`
	Requirement string     `json:"requirement"`
	Observed    string     `json:"observed"`
	Evidence    []AuditRef `json:"evidence"`
	Verdict     string     `json:"verdict"`
	Correction  string     `json:"correction"`
	Criteria    []string   `json:"criteria"`
	// notReached marks a mechanical row unobserved only because the
	// attempt stopped before its step (A3.8); it is derived, not encoded.
	notReached bool
}

// Audit is audit.json.
type Audit struct {
	Schema       string     `json:"schema"`
	RunID        string     `json:"run_id"`
	ReportSHA256 string     `json:"report_sha256"`
	Scope        string     `json:"scope"`
	Mechanical   string     `json:"mechanical"`
	ResultReview string     `json:"result_review"`
	Sources      []FileHash `json:"sources"`
	Rows         []AuditRow `json:"rows"`
}

// Audit layers, verdicts and fixed statements.
const (
	LayerMechanical    = "mechanical"
	LayerJudgment      = "judgment"
	VerdictCorrect     = "correct"
	VerdictIncorrect   = "incorrect"
	VerdictUnobserved  = "unobserved"
	AuditScope         = "mechanical-and-deterministic-results"
	PendingReview      = "needs owner/coordinator review"
	JudgmentReportLine = "Result judgments: needs owner/coordinator review"
)

// markerFor is the run marker a hop's task carries as its first payload
// pointer.
func markerFor(runID, hop string) string { return "callsheet-real-e2e run=" + runID + " hop=" + hop }

// preflightHop names the 0-based preflight i.
func preflightHop(i int) string { return "preflight-" + hopDir(i) }

// waitHandle names the 0-based hop i's coordinator wait.
func waitHandle(i int) string { return "hop-" + hopDir(i) }

// lastNonEmptyLine returns s's last line with content (trailing spaces and
// CR removed).
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimRight(lines[i], " \t\r"); l != "" {
			return l
		}
	}
	return ""
}

// Final markers per hop.
var hopMarkers = map[string][]string{
	HopDesigner:       {"STATUS: DESIGN_DRAFT"},
	HopDesignReviewer: {"STATUS: DESIGN_REVIEW_APPROVED", "STATUS: DESIGN_CHANGES_REQUESTED"},
	HopCoder:          {"STATUS: IMPL_COMPLETE"},
	HopCodeReviewer:   {"STATUS: REVIEW_APPROVED", "STATUS: REVIEW_CHANGES_REQUESTED"},
}

// approvedMarkers are the markers that let the chain continue.
var approvedMarkers = map[string]string{
	HopDesigner:       "STATUS: DESIGN_DRAFT",
	HopDesignReviewer: "STATUS: DESIGN_REVIEW_APPROVED",
	HopCoder:          "STATUS: IMPL_COMPLETE",
	HopCodeReviewer:   "STATUS: REVIEW_APPROVED",
}

// finalMarker classifies a hop's final message: its approved marker,
// another valid marker of the hop, or "" when none ends it.
func finalMarker(hop, final string) string {
	l := lastNonEmptyLine(final)
	for _, m := range hopMarkers[hop] {
		if l == m {
			return m
		}
	}
	return ""
}

// AuthToken is the answer every authentication preflight asks for. Since
// A3.8 echoing it is information only (token_echoed), never a requirement.
const AuthToken = "CALLSHEET_AUTH_OK"

// authAnswerOK reports whether a preflight final message is the token
// (surrounding whitespace aside): its token_echoed.
func authAnswerOK(final string) bool { return strings.TrimSpace(final) == AuthToken }

// authAnswerPresent reports whether a preflight final message is a
// non-empty answer (A3.8): any such answer, completed by the selected pair,
// demonstrates authentication; a whitespace-only answer is empty.
func authAnswerPresent(final string) bool { return strings.TrimSpace(final) != "" }

// tokenEchoed is a stored preflight's token_echoed (A3.8), information
// only; ok is false when it has no final answer.
func tokenEchoed(t *taskEvidence) (echoed, ok bool) {
	if t == nil || t.View.Result == nil || t.View.Result.FinalMessage == nil {
		return false, false
	}
	return authAnswerOK(*t.View.Result.FinalMessage), true
}

// allowedChanges are each hop's permitted changed paths.
var allowedChanges = map[string][]string{
	HopDesigner:       {"design.md"},
	HopDesignReviewer: {"design-review.md"},
	HopCoder:          {"main.go", "main_test.go"},
	HopCodeReviewer:   {},
}

// inlinePaths are the files the code reviewer's goal must carry inline.
var inlinePaths = []string{"design.md", "main.go", "main_test.go"}

// fileBlockBegin and fileBlockEnd frame one inline file in the code
// reviewer's goal.
func fileBlockBegin(path, sum string) string {
	return fmt.Sprintf("----- BEGIN FILE %s sha256=%s -----\n", path, sum)
}

func fileBlockEnd(path string) string { return fmt.Sprintf("\n----- END FILE %s -----\n", path) }

// parseInlineFiles extracts the labelled files of a goal: each path at
// most once, its declared hash equal to its content's.
func parseInlineFiles(goal string) (map[string][]byte, error) {
	out := map[string][]byte{}
	rest := goal
	for {
		i := strings.Index(rest, "----- BEGIN FILE ")
		if i < 0 {
			return out, nil
		}
		rest = rest[i+len("----- BEGIN FILE "):]
		head, body, ok := strings.Cut(rest, " -----\n")
		if !ok {
			return nil, fmt.Errorf("unterminated inline file header")
		}
		path, sum, ok := strings.Cut(head, " sha256=")
		if !ok || !hex64RE.MatchString(sum) {
			return nil, fmt.Errorf("malformed inline file header")
		}
		end := "\n----- END FILE " + path + " -----\n"
		j := strings.Index(body, end)
		if j < 0 {
			return nil, fmt.Errorf("inline file %s has no end marker", path)
		}
		content := []byte(body[:j])
		if _, dup := out[path]; dup {
			return nil, fmt.Errorf("inline file %s appears twice", path)
		}
		if sha256Hex(content) != sum {
			return nil, fmt.Errorf("inline file %s does not match its declared hash", path)
		}
		out[path] = content
		rest = body[j+len(end)-1:]
	}
}
