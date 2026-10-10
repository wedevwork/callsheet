package reale2e

import (
	"fmt"
	"regexp"
	"strings"
)

// Version 1 evidence documents (design 12a-real-e2e, Evidence and
// checker). The live collector writes them and the offline checker reads
// them strictly (unknown fields, duplicate keys and unknown schema
// identifiers are refused before any semantic check).

// Schema identifiers.
const (
	ManifestSchema    = "callsheet-real-e2e/v1"
	LaunchSchema      = "callsheet-real-e2e-launch/v1"
	RolesSchema       = "callsheet-real-e2e-roles/v1"
	DispatchSchema    = "callsheet-real-e2e-dispatch/v1"
	WaitSchema        = "callsheet-real-e2e-wait/v1"
	DecisionSchema    = "callsheet-real-e2e-decision/v1"
	TreeSchema        = "callsheet-real-e2e-trees/v1"
	TestRunSchema     = "callsheet-real-e2e-test-run/v1"
	CleanupSchema     = "callsheet-real-e2e-cleanup/v1"
	EventMapSchema    = "callsheet-real-e2e-event-map/v1"
	AttestationSchema = "callsheet-real-e2e-setup-attestation/v1"
	CoordExitSchema   = "callsheet-real-e2e-coordinator-exit/v1"
	LogMetaSchema     = "callsheet-real-e2e-log/v1"
	ReportSchema      = "callsheet-real-e2e-report/v1"
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
	StartedAt        string          `json:"started_at"`
}

// LaunchRecord is setup/launch.json: the coordinator launch with personal
// paths tokenized.
type LaunchRecord struct {
	Schema         string   `json:"schema"`
	SessionID      string   `json:"session_id"`
	Argv           []string `json:"argv"`
	CoordinatorDir string   `json:"coordinator_dir"`
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

// WaitHandleExit is one owner-reported coordinator wait handle's exit.
type WaitHandleExit struct {
	Handle   string `json:"handle"`
	ExitCode int    `json:"exit_code"`
}

// CoordinatorExit is the owner's record of the owner-started processes
// (supplied as owner/coordinator-exit.json, copied into cleanup.json).
type CoordinatorExit struct {
	Schema         string           `json:"schema"`
	SessionClosed  bool             `json:"session_closed"`
	MCPChildExited bool             `json:"mcp_child_exited"`
	WaitHandles    []WaitHandleExit `json:"wait_handles"`
}

// CleanupRecord is cleanup.json.
type CleanupRecord struct {
	Schema         string           `json:"schema"`
	Processes      []ProcRecord     `json:"processes"`
	CancelledTasks []string         `json:"cancelled_tasks"`
	UnsettledTasks []string         `json:"unsettled_tasks"`
	Owner          *CoordinatorExit `json:"owner"`
	StartedAt      string           `json:"started_at"`
	FinishedAt     string           `json:"finished_at"`
	Complete       bool             `json:"complete"`
}

// SetupAttestation is session/setup-attestation.json, the owner's
// statement about the coordinator session's setup (trusted provenance,
// not cryptographic proof).
type SetupAttestation struct {
	Schema                    string   `json:"schema"`
	RunID                     string   `json:"run_id"`
	SessionID                 string   `json:"session_id"`
	ClaudeVersion             string   `json:"claude_version"`
	FreshSession              bool     `json:"fresh_session"`
	Resumed                   bool     `json:"resumed"`
	EmptyCoordinatorDir       bool     `json:"empty_coordinator_dir"`
	NoPriorConversation       bool     `json:"no_prior_conversation"`
	NoExtraPrompt             bool     `json:"no_extra_prompt"`
	NoUnrelatedInstructions   bool     `json:"no_unrelated_instructions"`
	InstructionSourcesChecked bool     `json:"instruction_sources_checked"`
	SetupInputs               []string `json:"setup_inputs"`
	TranscriptExport          string   `json:"transcript_export"`
}

// allowedSetupInputs are the only declared coordinator setup inputs.
var allowedSetupInputs = []string{"runbook", "coordinator-prompt", "mcp-config"}

// MapEvent is one owner-attested transcript event: a kind, its hop/task
// bindings and an inclusive 1-based line span of the transcript. Every
// field is explicit in every event ("" or false or 0 where it does not
// apply): an omitted user_message_between or budget_ms must never read as
// an attested "none".
type MapEvent struct {
	Kind               string `json:"kind"`
	Hop                string `json:"hop"`
	Task               string `json:"task"`
	Handle             string `json:"handle"`
	Background         bool   `json:"background"`
	AutomaticWake      bool   `json:"automatic_wake"`
	UserMessageBetween bool   `json:"user_message_between"`
	BudgetMS           int    `json:"budget_ms"`
	Recovery           bool   `json:"recovery"`
	Lines              []int  `json:"lines"`
}

// EventMap is session/event-map.json.
type EventMap struct {
	Schema           string     `json:"schema"`
	RunID            string     `json:"run_id"`
	SessionID        string     `json:"session_id"`
	TranscriptSHA256 string     `json:"transcript_sha256"`
	AttestedByOwner  bool       `json:"attested_by_owner"`
	Events           []MapEvent `json:"events"`
}

// Event-map kinds.
const (
	MapSessionLaunch = "session_launch"
	MapDispatch      = "dispatch"
	MapWaitStart     = "wait_start"
	MapWaitConsumed  = "wait_consumed"
	MapOwnerPause    = "owner_gate_pause"
	MapOwnerResume   = "owner_resume"
	MapMCPWait       = "mcp_wait"
	MapFinalResult   = "final_result"
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

// AuthToken is the exact answer of every authentication preflight.
const AuthToken = "CALLSHEET_AUTH_OK"

// authAnswerOK reports whether a preflight final message is the token
// (surrounding whitespace aside).
func authAnswerOK(final string) bool { return strings.TrimSpace(final) == AuthToken }

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
