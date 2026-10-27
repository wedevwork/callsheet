package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The versioned full-flow evidence bundle (design 12a-dev-flow-verify,
// Bundle and checker). manifest.json carries the distinct schema
// callsheet-dev-full-evidence/v1; every new structured record is a strict
// schema-bearing envelope {schema, run_id, ...} (records wrapping an
// existing 2d.2 type carry it as data). The layout is closed: canonical
// paths only, every ordinal two-digit within its range, no symlink,
// special file, traversal or unexpected directory. Unsafe or malformed
// layouts and unsupported schemas are schema errors (exit 2) before any
// semantic judgment; missing files and mismatched hashes or identities
// are failed evidence (exit 1).

// Evidence versions (design 12a-dev-flow-light, Evidence protocol). New
// attempts write callsheet-dev-evidence/v2 (scenario full or light) with
// callsheet-dev-<kind>/v2 envelopes and inert captured sources; a
// historical callsheet-dev-full-evidence/v1 bundle is read at its original
// paths under its original layout, never rewritten or migrated.
const (
	// DevBundleSchema is the current development evidence manifest's
	// schema.
	DevBundleSchema = "callsheet-dev-evidence/v2"
	// DevBundleSchemaV1 is the historical full-flow evidence schema.
	DevBundleSchemaV1 = "callsheet-dev-full-evidence/v1"
	devBundleV1       = 1
	devBundleV2       = 2
)

// devKind is a record kind's current (v2) envelope schema identifier.
func devKind(kind string) string { return kindFor(devBundleV2, kind) }

// kindFor is a record kind's envelope schema identifier in evidence
// version ver.
func kindFor(ver int, kind string) string {
	if ver == devBundleV1 {
		return "callsheet-dev-full-" + kind + "/v1"
	}
	return "callsheet-dev-" + kind + "/v2"
}

// kind is a record kind's schema identifier in b's evidence version.
func (b *DevBundle) kind(k string) string { return kindFor(b.ver, k) }

// Captured-source storage (design 12a-dev-flow-light, FP-7). Every v2
// captured repository source document and fixture file with logical path
// L is stored inertly at _captured/L.bin, so no evidence directory forms a
// Go package (go help packages: names beginning with "_" are ignored).
// Writers, loaders, inventories, provenance checks and audit links share
// this one mapping.
const capturedDir = "_captured"

// capturedPath is logical capture L's physical v2 storage path.
func capturedPath(logical string) string { return capturedDir + "/" + logical + ".bin" }

// captured returns logical capture L's bytes in b's storage layout (v1:
// the original unpacked path; v2: its inert storage path).
func (b *DevBundle) captured(logical string) ([]byte, bool) {
	if b.ver == devBundleV1 {
		data, ok := b.Files[logical]
		return data, ok
	}
	data, ok := b.Files[capturedPath(logical)]
	return data, ok
}

// foldKey is a physical path's platform-independent collision key: ASCII
// A-Z mapped to a-z (the allowlist is ASCII; no Unicode normalization).
func foldKey(p string) string {
	b := []byte(p)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// checkCaptureFold rejects distinct physical paths that collide under
// ASCII case folding, and duplicates.
func checkCaptureFold(paths []string) error {
	seen := map[string]string{}
	for _, p := range paths {
		k := foldKey(p)
		if prev, ok := seen[k]; ok {
			return fmt.Errorf("captured paths %s and %s collide under case folding", contract.SafeText(prev, 160), contract.SafeText(p, 160))
		}
		seen[k] = p
	}
	return nil
}

// devEnvelope wraps an existing 2d.2 type as data.
type devEnvelope[T any] struct {
	Schema string `json:"schema"`
	RunID  string `json:"run_id"`
	Data   T      `json:"data"`
}

// envelope wraps v as kind for run (the current evidence version).
func envelope[T any](kind, run string, v T) devEnvelope[T] {
	return devEnvelope[T]{Schema: devKind(kind), RunID: run, Data: v}
}

// InventoryEntry is one inventoried bundle file.
type InventoryEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// DevExecutableRecord is the harness executable (which also runs the
// reviewer helper) and its SHA-256 before and after execution.
type DevExecutableRecord struct {
	Path   string `json:"path"`
	Before string `json:"before_sha256"`
	After  string `json:"after_sha256"`
}

// DevManifest is manifest.json.
type DevManifest struct {
	Schema             string              `json:"schema"`
	RunID              string              `json:"run_id"`
	Scenario           string              `json:"scenario"`
	Revision           string              `json:"revision"`
	CallsheetVersion   string              `json:"callsheet_version"`
	GOOS               string              `json:"goos"`
	GOARCH             string              `json:"goarch"`
	Gate               GateRecord          `json:"gate"`
	Versions           []VersionRecord     `json:"versions"`
	FlowLabel          string              `json:"flow_label"`
	FlowSHA256         string              `json:"flow_sha256"`
	SelectionSHA256    string              `json:"selection_sha256"`
	Roles              []FlowRole          `json:"roles"`
	Workspace          WorkspaceRecord     `json:"workspace"`
	ProtectedRoots     []string            `json:"protected_roots"`
	ProtectDepth       int                 `json:"protect_depth"`
	AttemptStart       string              `json:"attempt_start"`
	AttemptDeadline    string              `json:"attempt_deadline"`
	CollectedAt        string              `json:"collected_at"`
	FinalCandidateTask string              `json:"final_candidate_task"`
	FinalCandidate     string              `json:"final_candidate"`
	FinalReviewTask    string              `json:"final_review_task"`
	FinalReview        string              `json:"final_review"`
	StopCode           *string             `json:"stop_code"`
	StopSeq            *int                `json:"stop_seq"`
	Executable         DevExecutableRecord `json:"executable"`
	Helper             reviewHelperSpec    `json:"helper"`
	Render             map[string]string   `json:"render"`
	Inventory          []InventoryEntry    `json:"inventory"`
	// Cursor is a Cursor attempt's executable binding (design
	// 12a-3-cursor-worker, slice 3b): required, non-null, on the
	// callsheet-dev-cursor-evidence/v1 schema only; a historical or default
	// manifest never carries the key.
	Cursor *DevCursorBinding `json:"cursor,omitempty"`
}

// DevObservation is tasks/NN/observation.json: the coordinator's accepted
// report of the slot's admitted task.
type DevObservation struct {
	Schema      string `json:"schema"`
	RunID       string `json:"run_id"`
	Ordinal     int    `json:"ordinal"`
	Task        string `json:"task"`
	AdmittedSeq int    `json:"admitted_seq"`
	ObservedSeq int    `json:"observed_seq"`
	Phase       int    `json:"phase"`
	CreatedAt   string `json:"created_at"`
}

// DevAdmission is tasks/NN/admission.json.
type DevAdmission struct {
	Schema         string            `json:"schema"`
	RunID          string            `json:"run_id"`
	Task           string            `json:"task"`
	Marker         string            `json:"marker"`
	Commit         string            `json:"commit"`
	Parent         string            `json:"parent"`
	Tree           string            `json:"tree"`
	Artifacts      map[string]string `json:"artifacts"`
	Disposition    string            `json:"disposition"`
	Blocker        string            `json:"blocker"`
	DesignRequired string            `json:"design_required"`
	BriefCurrent   bool              `json:"brief_current"`
}

// DevFlowLabel is setup/flow-label.json.
type DevFlowLabel struct {
	Schema          string `json:"schema"`
	RunID           string `json:"run_id"`
	Label           string `json:"label"`
	FlowSHA256      string `json:"flow_sha256"`
	SelectionSHA256 string `json:"selection_sha256"`
	Selection       string `json:"selection"`
}

// DevFeatureRecord is setup/feature-request.json.
type DevFeatureRecord struct {
	Schema          string `json:"schema"`
	RunID           string `json:"run_id"`
	OriginalSHA256  string `json:"original_sha256"`
	GeneratedSHA256 string `json:"generated_sha256"`
	Source          string `json:"source"`
}

// DevSeedPush is setup/seed-push.json.
type DevSeedPush struct {
	Schema    string `json:"schema"`
	RunID     string `json:"run_id"`
	Workspace string `json:"workspace"`
	Instance  string `json:"instance"`
	Ref       string `json:"ref"`
	Commit    string `json:"commit"`
}

// DevLogRecord is tasks/NN/logs.json.
type DevLogRecord struct {
	Schema string     `json:"schema"`
	RunID  string     `json:"run_id"`
	Task   string     `json:"task"`
	Meta   DevLogMeta `json:"meta"`
}

// DevReviewRecord is tasks/NN/review-evidence.json.
type DevReviewRecord struct {
	Schema   string            `json:"schema"`
	RunID    string            `json:"run_id"`
	Evidence DevReviewEvidence `json:"evidence"`
}

// DevSettlement is one owned task's settled state at cleanup, with the
// plane's duration of its final view when it has one (the accounting of a
// task stopped in flight, which has no stored task view).
type DevSettlement struct {
	Task       string `json:"task"`
	Label      string `json:"label"`
	State      string `json:"state"`
	DurationMS *int64 `json:"duration_ms"`
}

// settlementOf is a settlement of task from its final view (nil when it
// could not be read).
func settlementOf(task, label string, v *contract.TaskView) DevSettlement {
	s := DevSettlement{Task: task, Label: label, State: "unknown"}
	if v != nil {
		s.State = v.State
		if d, ok := duration(*v); ok {
			ms := d.Milliseconds()
			s.DurationMS = &ms
		}
	}
	return s
}

// DevCleanup is the development cleanup record (cleanup.json's data); it
// is not a legacy cleanup record.
type DevCleanup struct {
	Started         []StartedProc   `json:"started"`
	Processes       []ProcRecord    `json:"processes"`
	CancelledTasks  []string        `json:"cancelled_tasks"`
	UnsettledTasks  []string        `json:"unsettled_tasks"`
	RuntimeRemoved  bool            `json:"runtime_removed"`
	StartedAt       string          `json:"started_at"`
	FinishedAt      string          `json:"finished_at"`
	Complete        bool            `json:"complete"`
	UnresolvedTasks []string        `json:"unresolved_tasks"`
	OverBudget      bool            `json:"over_budget"`
	Deadline        string          `json:"deadline"`
	ScanError       string          `json:"scan_error"`
	LateAdmissions  []string        `json:"late_admissions"`
	Settled         []DevSettlement `json:"settled"`
}

// Fixed full-flow bundle paths.
const (
	devManifestFile   = "manifest.json"
	devLedgerFile     = "ledger.json"
	devAdmissionsFile = "admissions.json"
	devCleanupFile    = "cleanup.json"
	devCostFile       = "cost.json"
	devEventMapFile   = "session/event-map.json"
	devTreesFile      = "workspace/tree-manifest.json"
	devCoverageFile   = "validation/coverage.json"
	devRepoDir        = "workspace/repo.git"
)

// devDerivedFiles are the derived report and audit files and the optional
// external judgment appendix: never inventoried.
var devDerivedFiles = map[string]bool{"report.json": true, "report.txt": true, "audit.json": true, "audit.md": true, "audit-review.md": true}

// Closed layout patterns.
var (
	devPreflightRE  = regexp.MustCompile(`^preflight/0[1-5]/(observation\.json|task\.json|final\.txt)$`)
	devTaskFileRE   = regexp.MustCompile(`^tasks/(0[1-9]|1[0-4])/(plan\.json|dispatch\.json|observation\.json|task\.json|final\.txt|logs\.txt|logs\.json|admission\.json|record\.json|wait\.json|wait-result\.json|wait-stderr\.txt|wait-exit\.txt|review-evidence\.json|publication-rejection\.json|rejected-review\.bin)$`)
	devTransitionRE = regexp.MustCompile(`^transitions/(0[1-9]|[1-5][0-9]|6[0-4])\.json$`)
	devDecisionRE   = regexp.MustCompile(`^(decisions|receipts)/0[1-3]\.json$`)
	devSessionRE    = regexp.MustCompile(`^session/(phase-[1-4]\.stdout\.jsonl|phase-[1-4]\.stderr\.txt|transcript-phase-[1-4]\.jsonl|coordinator-exit\.json|event-map\.json)$`)
	devLaunchRE     = regexp.MustCompile(`^setup/launch-phase-[1-4]\.json$`)
	devDirRE        = regexp.MustCompile(`^(preflight/0[1-5]|tasks/(0[1-9]|1[0-4]))$`)
)

// devRootFiles, devSetupFiles and devValidationFiles are the fixed names.
var (
	devRootFiles = map[string]bool{devManifestFile: true, "events.jsonl": true, devLedgerFile: true, devAdmissionsFile: true, devCleanupFile: true,
		devCostFile: true, "report.json": true, "report.txt": true, "audit.json": true, "audit.md": true, "audit-review.md": true}
	devSetupFiles = map[string]bool{"setup/sources.json": true, "setup/rendered.json": true, "setup/installed-manuals.json": true,
		"setup/flow-label.json": true, "setup/feature-request.json": true, "setup/seed-push.json": true, "setup/flow.json": true, "setup/roles.json": true,
		"setup/mcp-config.json": true, "setup/coordinator-system.md": true, "setup/phase.md": true, "setup/scenario.md": true}
	devValidationFiles = map[string]bool{"validation/baseline.json": true, "validation/baseline-stdout.txt": true, "validation/baseline-stderr.txt": true,
		"validation/final-test.json": true, "validation/final-stdout.txt": true, "validation/final-stderr.txt": true, buildStdoutFile: true,
		buildStderrFile: true, "validation/cases.json": true, "validation/tests.jsonl": true, "validation/test-json-stderr.txt": true,
		"validation/requirements.json": true, devCoverageFile: true, "validation/coverage.out": true, "validation/coverage-stdout.txt": true,
		"validation/coverage-stderr.txt": true}
)

// devSourcePaths are the captured method sources' bundle paths: exactly
// DevMethodNames' repository paths under setup/sources/.
func devSourcePaths() map[string]bool {
	out := map[string]bool{}
	for _, n := range DevMethodNames() {
		out["setup/sources/"+devSourceRel(n)] = true
	}
	return out
}

// devSourceRel is a method input's repository path.
func devSourceRel(name string) string {
	if name == FlowFile || name == DevPromptFile || name == DevPhaseFile || name == DevScenario || name == DevScenarioLight {
		return DevFullDir + "/" + name
	}
	return DevFlowDir + "/" + name
}

// devFileAllowed reports a closed v1 full-flow layout file path (its
// captured sources unpacked at their logical paths).
func devFileAllowed(rel string) bool {
	switch {
	case devRootFiles[rel], devSetupFiles[rel], devValidationFiles[rel], rel == devTreesFile:
		return true
	case devPreflightRE.MatchString(rel), devTaskFileRE.MatchString(rel), devTransitionRE.MatchString(rel), devDecisionRE.MatchString(rel),
		devSessionRE.MatchString(rel), devLaunchRE.MatchString(rel):
		return true
	case rel == "environment/diff.json" || slices.Contains(envFiles("before"), rel) || slices.Contains(envFiles("after"), rel):
		return true
	}
	return devSourcePaths()[rel]
}

// devDirAllowed reports a closed v1 full-flow layout directory.
func devDirAllowed(rel string) bool {
	switch rel {
	case "setup", "preflight", "tasks", "transitions", "decisions", "receipts", "session", "workspace", devRepoDir, "validation", "environment",
		"setup/sources", "setup/sources/docs", "setup/sources/docs/examples", "setup/sources/" + DevFlowDir, "setup/sources/docs/examples/real-e2e",
		"setup/sources/" + DevFullDir:
		return true
	}
	return devDirRE.MatchString(rel)
}

// captureDirs are the directories of a set of physical capture paths
// (every ancestor below the root).
func captureDirs(paths map[string]bool) map[string]bool {
	out := map[string]bool{}
	for p := range paths {
		for d := filepath.ToSlash(filepath.Dir(p)); d != "." && d != "/"; d = filepath.ToSlash(filepath.Dir(d)) {
			out[d] = true
		}
	}
	return out
}

// devCaptureLogical is a v2 scenario bundle's allowlisted logical
// captures: the scenario's captured method sources and, for light, the
// light fixture.
func devCaptureLogical(scenario string) []string {
	var out []string
	for _, n := range DevMethodNamesFor(scenario) {
		out = append(out, "setup/sources/"+devSourceRel(n))
	}
	if scenario == scenarioLight {
		for _, n := range lightFixtureFiles {
			out = append(out, lightFixtureCaptureDir+"/"+n)
		}
	}
	sort.Strings(out)
	return out
}

// physicalSet maps logical captures to their v2 physical paths.
func physicalSet(logical []string) map[string]bool {
	out := map[string]bool{}
	for _, l := range logical {
		out[capturedPath(l)] = true
	}
	return out
}

// devLayoutFor is the closed bundle layout of evidence version ver and
// scenario (a v1 bundle is always full). The v2 layouts store captures
// inertly: no unpacked setup/sources path and no .go file at all.
func devLayoutFor(ver int, scenario string) devLayout {
	if ver == devBundleV1 {
		return devLayout{ver: devBundleV1, scenario: scenarioFull, file: devFileAllowed, dir: devDirAllowed,
			doc: func(rel string) (any, string) { return devDocTypeV(devBundleV1, scenarioFull, rel) }, ref: devRef}
	}
	caps := physicalSet(devCaptureLogical(scenario))
	capDirs := captureDirs(caps)
	base := devFileAllowed
	baseDir := devDirAllowed
	if scenario == scenarioLight {
		base, baseDir = devLightFileAllowed, devLightDirAllowed
	}
	return devLayout{ver: devBundleV2, scenario: scenario,
		file: func(rel string) bool {
			switch {
			case strings.HasSuffix(rel, ".go"), strings.HasPrefix(rel, "setup/sources/"):
				return false
			case caps[rel]:
				return true
			}
			return base(rel)
		},
		dir: func(rel string) bool {
			if rel == "setup/sources" || strings.HasPrefix(rel, "setup/sources/") {
				return false
			}
			return capDirs[rel] || baseDir(rel)
		},
		doc: func(rel string) (any, string) { return devDocTypeV(devBundleV2, scenario, rel) }, ref: devRef}
}

// DevBundle is a loaded, layout-checked and strictly decoded full-flow
// bundle.
type DevBundle struct {
	Root    string
	Files   map[string][]byte
	RepoInv []InventoryEntry
	Repo    *repoView
	RepoErr error
	// Repos are every loaded evidence repository by directory (Repo is
	// the layout's first; a light qualification has two).
	Repos    map[string]*repoView
	Manifest *DevManifest
	Events   []Event
	// Decoded records by path (nil when absent).
	docs   map[string]any
	layout devLayout
	// derived is the collector's own event-map derivation of this loaded
	// bundle with its helper and runtime render values (nil for a bundle
	// loaded from disk); the checker reuses it only for the same values.
	derived *derivedEventMap
	// ver is the evidence version (devBundleV1 or devBundleV2) and
	// Scenario the layout's scenario (full, light or probe).
	ver      int
	Scenario string
	// cursor is set only from a recognized Cursor outer discriminator (its
	// layout), never from a role, file name, missing record or version
	// list (design 12a-3-cursor-worker, slice 3b); ver stays 2 for its
	// inherited nested records.
	cursor bool
}

// derivedEventMap is one event-map derivation and its render inputs.
type derivedEventMap struct {
	goos, reale2e, runtime string
	em                     DevEventMap
}

// devLayout is a closed evidence layout: its allowed files and
// directories, each strictly typed record's type and schema, and the
// evidence repository's ref policy.
type devLayout struct {
	ver      int
	scenario string
	file     func(string) bool
	dir      func(string) bool
	doc      func(string) (any, string)
	ref      func(string) bool
	// repos are the layout's evidence repositories (nil: the single
	// workspace/repo.git with ref and HEAD at the seed). Several
	// repositories share the Git byte and object budgets.
	repos []devRepoSpec
	// cursor marks a Cursor outer format's layout (devcursor_check.go).
	cursor bool
}

// devRepoSpec is one evidence repository of a layout: its directory, ref
// policy and HEAD target.
type devRepoSpec struct {
	dir  string
	ref  func(string) bool
	head string
}

// repoSpecs are the layout's evidence repositories.
func (l devLayout) repoSpecs() []devRepoSpec {
	if l.repos != nil {
		return l.repos
	}
	return []devRepoSpec{{dir: devRepoDir, ref: l.ref, head: "refs/heads/seed"}}
}

// isRepoDir reports one of the layout's evidence repository directories.
func (l devLayout) isRepoDir(rel string) bool {
	for _, r := range l.repoSpecs() {
		if r.dir == rel {
			return true
		}
	}
	return false
}

// errDevSchema reports an unsupported or malformed full-flow bundle.
func errDevSchema(format string, a ...any) error { return schemaErr(format, a...) }

// LoadDevBundle loads the full-flow bundle at root: the safe root resolved
// once, the closed layout and its budgets enforced before reading, every
// text file valid UTF-8, every JSON record strictly decoded against its
// path's type and schema, and the evidence repository validated before
// go-git opens it.
//
// The layout is the manifest discriminator's (schema and scenario, read
// strictly before the walk): a bundle without a manifest (the collector's
// own load before it writes one) is read as a current full bundle.
func LoadDevBundle(root string) (*DevBundle, error) {
	resolved, err := safeEvidenceRoot(root)
	if err != nil {
		return nil, err
	}
	ver, scenario, cursor, err := peekDevManifest(resolved)
	if err != nil {
		return nil, err
	}
	if cursor {
		return loadDevLayout(root, cursorAttemptLayout(scenario))
	}
	return loadDevLayout(root, devLayoutFor(ver, scenario))
}

// peekDevManifest reads a development bundle's version, scenario and
// Cursor outer format from its manifest discriminator: v1 is full only;
// v2 and the Cursor evidence/v1 format (whose nested records are v2) are
// full or light.
func peekDevManifest(resolved string) (int, string, bool, error) {
	data, err := lstatRegular(resolved, devManifestFile)
	if errors.Is(err, fs.ErrNotExist) {
		return devBundleV2, scenarioFull, false, nil
	}
	if err != nil {
		return 0, "", false, errDevSchema("manifest.json: %v", err)
	}
	schema, err := manifestSchema(data)
	if err != nil {
		return 0, "", false, errDevSchema("manifest.json: %v", err)
	}
	var head struct {
		Scenario string `json:"scenario"`
	}
	_ = json.Unmarshal(data, &head)
	scenarioOK := head.Scenario == scenarioFull || head.Scenario == scenarioLight
	switch {
	case schema == DevBundleSchemaV1:
		return devBundleV1, scenarioFull, false, nil
	case (schema == DevBundleSchema || schema == DevCursorBundleSchema) && scenarioOK:
		return devBundleV2, head.Scenario, schema == DevCursorBundleSchema, nil
	case schema == DevBundleSchema || schema == DevCursorBundleSchema:
		return 0, "", false, errDevSchema("manifest.json: unsupported scenario %q", contract.SafeText(head.Scenario, 32))
	}
	return 0, "", false, errDevSchema("manifest.json: unsupported schema %q", contract.SafeText(schema, 80))
}

// loadDevLayout loads an evidence directory under a closed layout.
func loadDevLayout(root string, layout devLayout) (*DevBundle, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errDevSchema("evidence root: %v", err)
	}
	st, err := os.Lstat(resolved)
	if err != nil || !st.IsDir() {
		return nil, errDevSchema("evidence root is not a directory")
	}
	b := &DevBundle{Root: resolved, Files: map[string][]byte{}, docs: map[string]any{}, layout: layout, ver: layout.ver, Scenario: layout.scenario,
		cursor: layout.cursor}
	if err := b.readFiles(); err != nil {
		return nil, err
	}
	if err := b.decode(); err != nil {
		return nil, err
	}
	budget := &repoBudget{stored: maxRepoFileBytes, decoded: maxDecodedBytes}
	for i, spec := range layout.repoSpecs() {
		if _, ok := b.Files[spec.dir]; !ok {
			continue
		}
		delete(b.Files, spec.dir)
		root := filepath.Join(resolved, filepath.FromSlash(spec.dir))
		repo, err := openEvidenceRepoShared(root, spec.ref, spec.head, budget)
		if err != nil && (errors.Is(err, errObjectBound) || errors.Is(err, errRepoSize) || errors.Is(err, errRepoLayout)) {
			return nil, &SchemaError{Err: err}
		}
		if i == 0 {
			b.Repo, b.RepoErr = repo, err
		} else if err != nil && b.RepoErr == nil {
			b.RepoErr = err
		}
		if err != nil {
			continue
		}
		if b.Repos == nil {
			b.Repos = map[string]*repoView{}
		}
		b.Repos[spec.dir] = repo
		inv, err := repoInventory(root, spec.dir)
		if err != nil {
			return nil, &SchemaError{Err: err}
		}
		b.RepoInv = append(b.RepoInv, inv...)
	}
	return b, nil
}

// devRefRE is a development evidence repository ref.
var devRefRE = regexp.MustCompile(`^refs/heads/(seed|task-(0[1-9]|1[0-4]))$`)

// devRef reports an allowed development evidence ref.
func devRef(name string) bool { return devRefRE.MatchString(name) }

// repoInventory lists the stored Git files under dir (already validated),
// named under the evidence-relative prefix.
func repoInventory(dir, prefix string) ([]InventoryEntry, error) {
	var out []InventoryEntry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := readBounded(p, maxRepoFileBytes)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, InventoryEntry{Path: prefix + "/" + filepath.ToSlash(rel), Bytes: int64(len(data)), SHA256: sha256Hex(data)})
		return nil
	})
	return out, err
}

// readFiles walks the resolved root, enforcing the closed layout and the
// text budgets before reading any file.
func (b *DevBundle) readFiles() error {
	var total, env, envBefore, envAfter int64
	folds := map[string]string{}
	return filepath.WalkDir(b.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return errDevSchema("evidence walk: %v", err)
		}
		if p == b.Root {
			return nil
		}
		rel, err := filepath.Rel(b.Root, p)
		if err != nil || !utf8.ValidString(rel) || strings.HasPrefix(rel, "..") {
			return errDevSchema("evidence path is invalid")
		}
		rel = filepath.ToSlash(rel)
		st, err := os.Lstat(p)
		if err != nil {
			return errDevSchema("evidence %s: %v", rel, err)
		}
		if b.ver == devBundleV2 && strings.HasPrefix(rel, capturedDir+"/") {
			// Distinct physical captures never collide under ASCII case
			// folding, on any platform (design 12a-dev-flow-light, FP-7).
			k := foldKey(rel)
			if prev, ok := folds[k]; ok && prev != rel {
				return errDevSchema("evidence captures %s and %s collide under case folding", contract.SafeText(prev, 160), contract.SafeText(rel, 160))
			}
			folds[k] = rel
		}
		switch {
		case st.Mode()&fs.ModeSymlink != 0:
			return errDevSchema("evidence entry %s is a symlink", rel)
		case st.IsDir():
			if !b.layout.dir(rel) {
				return errDevSchema("evidence directory %s is not part of the layout", rel)
			}
			if b.layout.isRepoDir(rel) {
				b.Files[rel] = nil
				return fs.SkipDir
			}
			return nil
		case !st.Mode().IsRegular():
			return errDevSchema("evidence entry %s is not a regular file", rel)
		case !b.layout.file(rel):
			return errDevSchema("evidence file %s is not part of the layout", rel)
		case st.Size() > maxTextFile:
			return errDevSchema("evidence file %s exceeds 8 MiB", rel)
		case slices.Contains(cursorActivityFiles, rel) && st.Size() > activityMaxOutput:
			// Each shallow sample keeps its own 256 KiB bound within the
			// shared environment budgets (design 12a-3-cursor-worker, 3b).
			return errDevSchema("evidence file %s exceeds %d bytes", rel, activityMaxOutput)
		}
		total += st.Size()
		if isEnvironmentFile(rel) {
			if (rel == "environment/diff.json" && st.Size() > maxDiffBytes) || st.Size() > maxShardBytes {
				return errDevSchema("evidence file %s exceeds its environment bound", rel)
			}
			env += st.Size()
			if strings.HasPrefix(rel, "environment/before-") {
				envBefore += st.Size()
			}
			if strings.HasPrefix(rel, "environment/after-") {
				envAfter += st.Size()
			}
		}
		if total > maxTextTotal || env > maxEnvironmentAll || envBefore > maxSnapshotBytes || envAfter > maxSnapshotBytes || total-env > maxOtherText {
			return errDevSchema("evidence text exceeds a budget")
		}
		data, err := readBounded(p, maxTextFile)
		if err != nil {
			return errDevSchema("evidence %s: %v", rel, err)
		}
		// A rejected review.md's retained bytes are raw (design
		// 12a-dev-flow-verify r0.11): the one exception to the UTF-8 rule,
		// within its own 32 KiB bound and the text budgets.
		switch {
		case filepath.Base(rel) == rejectedReviewFile:
			if len(data) > maxRejectedReviewBytes {
				return errDevSchema("evidence file %s exceeds %d bytes", rel, maxRejectedReviewBytes)
			}
		case !utf8.Valid(data):
			return errDevSchema("evidence file %s is not valid UTF-8", rel)
		case filepath.Base(rel) == publicationRejectionFile && len(data) > maxRejectionRecordBytes:
			return errDevSchema("evidence file %s exceeds %d bytes", rel, maxRejectionRecordBytes)
		}
		b.Files[rel] = data
		return nil
	})
}

// devDocTypeV returns a fresh value of the record type at rel and its
// required schema in evidence version ver and scenario ("" when the file
// is not a strictly typed record).
func devDocTypeV(ver int, scenario, rel string) (any, string) {
	devKind := func(k string) string { return kindFor(ver, k) }
	if ver == devBundleV2 && scenario == scenarioLight {
		if v, schema := devLightDocType(rel); v != nil {
			return v, schema
		}
	}
	switch {
	case rel == devManifestFile:
		if ver == devBundleV1 {
			return &DevManifest{}, DevBundleSchemaV1
		}
		return &DevManifest{}, DevBundleSchema
	case rel == devLedgerFile:
		return &devEnvelope[DevFullState]{}, devKind("ledger")
	case rel == devAdmissionsFile:
		return &DevAdmissions{}, devKind("admissions")
	case rel == devCleanupFile:
		return &devEnvelope[DevCleanup]{}, devKind("cleanup")
	case rel == devCostFile:
		return &DevCost{}, devKind("cost")
	case rel == "report.json":
		return &DevReport{}, devKind("report")
	case rel == "audit.json":
		return &DevAudit{}, devKind("audit")
	case rel == "setup/sources.json":
		return &devEnvelope[[]SourceRecord]{}, devKind("sources")
	case rel == "setup/installed-manuals.json":
		return &devEnvelope[[]SourceRecord]{}, devKind("installed-manuals")
	case rel == "setup/rendered.json":
		return &devEnvelope[[]RenderedRecord]{}, devKind("rendered")
	case rel == "setup/flow-label.json":
		return &DevFlowLabel{}, devKind("flow-label")
	case rel == "setup/feature-request.json":
		return &DevFeatureRecord{}, devKind("feature-request")
	case rel == "setup/seed-push.json":
		return &DevSeedPush{}, devKind("seed-push")
	case rel == "setup/roles.json":
		return &RolesRecord{}, RolesSchema
	case devLaunchRE.MatchString(rel):
		return &LaunchRecord{}, LaunchSchema
	case rel == coordinatorExitFile:
		return &CoordinatorExit{}, CoordExitSchema
	case rel == devEventMapFile:
		return &DevEventMap{}, devKind("event-map")
	case rel == devTreesFile:
		return &devEnvelope[DevTreeManifest]{}, devKind("trees")
	case rel == "validation/baseline.json", rel == "validation/final-test.json":
		return &TestRun{}, TestRunSchema
	case rel == "validation/cases.json":
		return &CasesRecord{}, CasesSchema
	case rel == "validation/requirements.json":
		return &RequirementsRecord{}, RequirementsSchema
	case rel == devCoverageFile:
		return &DevCoverageRecord{}, devKind("coverage")
	case rel == "environment/diff.json":
		return &EnvDiff{}, DiffSchema
	case devTransitionRE.MatchString(rel):
		return &devEnvelope[TransitionRecord]{}, devKind("transition")
	case strings.HasPrefix(rel, "decisions/"):
		return &devEnvelope[DevDecision]{}, devKind("decision")
	case strings.HasPrefix(rel, "receipts/"):
		return &devEnvelope[OwnerReceipt]{}, devKind("receipt")
	case strings.HasPrefix(rel, "preflight/") && strings.HasSuffix(rel, "/observation.json"):
		return &devEnvelope[DevPreflightRecord]{}, devKind("observation")
	case strings.HasPrefix(rel, "tasks/"):
		switch filepath.Base(rel) {
		case "plan.json":
			return &devEnvelope[TaskPlan]{}, devKind("plan")
		case "dispatch.json":
			return &devDispatchDoc{}, devKind("dispatch")
		case "observation.json":
			return &DevObservation{}, devKind("observation")
		case "logs.json":
			return &DevLogRecord{}, devKind("logs")
		case "admission.json":
			return &DevAdmission{}, devKind("admission")
		case "record.json":
			return &devEnvelope[TaskRecord]{}, devKind("task-record")
		case "wait.json":
			return &devEnvelope[DevWaitRecord]{}, devKind("wait")
		case "review-evidence.json":
			return &DevReviewRecord{}, devKind("review-evidence")
		case publicationRejectionFile:
			return &devEnvelope[DevPublicationRejection]{}, devKind("publication-rejection")
		}
	}
	return nil, ""
}

// schemaOf reads a decoded record's schema identifier.
func schemaOf(v any) string {
	switch x := v.(type) {
	case interface{ schemaID() string }:
		return x.schemaID()
	}
	return ""
}

func (e *devEnvelope[T]) schemaID() string     { return e.Schema }
func (m *DevManifest) schemaID() string        { return m.Schema }
func (m *DevAdmissions) schemaID() string      { return m.Schema }
func (m *DevCost) schemaID() string            { return m.Schema }
func (m *DevReport) schemaID() string          { return m.Schema }
func (m *DevAudit) schemaID() string           { return m.Schema }
func (m *DevFlowLabel) schemaID() string       { return m.Schema }
func (m *DevFeatureRecord) schemaID() string   { return m.Schema }
func (m *DevSeedPush) schemaID() string        { return m.Schema }
func (m *RolesRecord) schemaID() string        { return m.Schema }
func (m *LaunchRecord) schemaID() string       { return m.Schema }
func (m *CoordinatorExit) schemaID() string    { return m.Schema }
func (m *DevEventMap) schemaID() string        { return m.Schema }
func (m *TestRun) schemaID() string            { return m.Schema }
func (m *CasesRecord) schemaID() string        { return m.Schema }
func (m *RequirementsRecord) schemaID() string { return m.Schema }
func (m *DevCoverageRecord) schemaID() string  { return m.Schema }
func (m *EnvDiff) schemaID() string            { return m.Schema }
func (m *devDispatchDoc) schemaID() string     { return m.Schema }
func (m *DevObservation) schemaID() string     { return m.Schema }
func (m *DevLogRecord) schemaID() string       { return m.Schema }
func (m *DevAdmission) schemaID() string       { return m.Schema }
func (m *DevReviewRecord) schemaID() string    { return m.Schema }

// decode strictly decodes every typed record, the task views, the events
// and the snapshot headers.
func (b *DevBundle) decode() error {
	names := make([]string, 0, len(b.Files))
	for rel := range b.Files {
		names = append(names, rel)
	}
	sort.Strings(names)
	for _, rel := range names {
		if err := b.decodeOne(rel, b.Files[rel]); err != nil {
			return err
		}
	}
	if data, ok := b.Files["events.jsonl"]; ok {
		evs, err := parseEventLines(data)
		if err != nil {
			return err
		}
		b.Events = evs
	}
	for _, phase := range []string{"before", "after"} {
		if data, ok := b.Files[envFiles(phase)[0]]; ok {
			line, _, _ := bytes.Cut(data, []byte("\n"))
			if got := peekSchema(line); got != "" && got != SnapshotSchema {
				return errDevSchema("%s: unsupported schema %q", envFiles(phase)[0], contract.SafeText(got, 64))
			}
		}
	}
	if m, ok := b.docs[devManifestFile].(*DevManifest); ok {
		b.Manifest = m
	}
	return nil
}

// decodeOne strictly decodes one layout document (a file without a typed
// document is kept as bytes only).
func (b *DevBundle) decodeOne(rel string, data []byte) error {
	if strings.HasSuffix(rel, "/task.json") {
		if err := checkDuplicateKeys(data); err != nil {
			return errDevSchema("%s: %v", rel, err)
		}
		v, err := contract.ParseTaskShowResponse(bytes.TrimSuffix(data, []byte("\n")))
		if err != nil {
			return errDevSchema("%s: %v", rel, err)
		}
		b.docs[rel] = &contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v}
		return nil
	}
	v, want := b.layout.doc(rel)
	if v == nil {
		return nil
	}
	if cursorFieldFile(rel) {
		if err := checkCursorField(data, b.cursor); err != nil {
			return errDevSchema("%s: %v", rel, err)
		}
	}
	if got := peekSchema(data); got != "" && got != want {
		return errDevSchema("%s: unsupported schema %q", rel, contract.SafeText(got, 80))
	}
	if err := decodeStrict(data, v); err != nil {
		return errDevSchema("%s: %v", rel, err)
	}
	if got := schemaOf(v); got != want {
		return errDevSchema("%s: unknown schema %q", rel, contract.SafeText(got, 80))
	}
	b.docs[rel] = v
	return nil
}

// ingest adds one root-level record the collector wrote after loading b
// (the event map, the cost, the manifest), decoded exactly as the loader
// decodes it, so the loaded bundle stays the bytes on disk.
func (b *DevBundle) ingest(rel string, data []byte) error {
	if !b.layout.file(rel) {
		return errDevSchema("%s is not a layout file", rel)
	}
	if err := b.decodeOne(rel, data); err != nil {
		return err
	}
	b.Files[rel] = data
	if m, ok := b.docs[devManifestFile].(*DevManifest); ok {
		b.Manifest = m
	}
	return nil
}

// parseEventLines strictly decodes events.jsonl.
func parseEventLines(data []byte) ([]Event, error) {
	var out []Event
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil, nil
	}
	if !strings.HasSuffix(string(data), "\n") {
		return nil, errDevSchema("events.jsonl does not end with LF")
	}
	for i, line := range strings.Split(s, "\n") {
		var ev Event
		if err := decodeStrict([]byte(line), &ev); err != nil {
			return nil, errDevSchema("events.jsonl line %d: %v", i+1, err)
		}
		if !eventTypes[ev.Type] {
			return nil, errDevSchema("events.jsonl line %d: unknown event type %q", i+1, contract.SafeText(ev.Type, 32))
		}
		out = append(out, ev)
	}
	return out, nil
}

// doc returns the decoded record at rel (nil when absent).
func doc[T any](b *DevBundle, rel string) *T {
	v, _ := b.docs[rel].(*T)
	return v
}

// taskDir is workflow ordinal n's directory.
func taskDir(n int) string { return "tasks/" + hopDir(n-1) }

// ordinalsOf returns the sorted two-digit ordinals present under prefix
// (tasks, preflight, transitions, decisions or receipts).
func (b *DevBundle) ordinalsOf(prefix string) []int {
	seen := map[int]bool{}
	for rel := range b.Files {
		rest, ok := strings.CutPrefix(rel, prefix+"/")
		if !ok || len(rest) < 2 {
			continue
		}
		if n, err := strconv.Atoi(rest[:2]); err == nil {
			seen[n] = true
		}
	}
	var out []int
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// inventory is the bundle's recomputed inventory: every non-derived text
// file but the manifest, and every stored Git file, sorted by path.
func (b *DevBundle) inventory() []InventoryEntry {
	var out []InventoryEntry
	for rel, data := range b.Files {
		if rel == devManifestFile || rel == devQualificationFile || devDerivedFiles[rel] {
			continue
		}
		out = append(out, InventoryEntry{Path: rel, Bytes: int64(len(data)), SHA256: sha256Hex(data)})
	}
	out = append(out, b.RepoInv...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// diskInventory walks an evidence directory the collector wrote and
// returns the same inventory the checker recomputes.
func diskInventory(root string) ([]InventoryEntry, error) {
	var out []InventoryEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == devManifestFile || rel == devQualificationFile || devDerivedFiles[rel] || strings.Contains(rel, "/.tmp-") || strings.HasPrefix(rel, ".tmp-") {
			return nil
		}
		data, err := readBounded(p, maxTextFile)
		if err != nil {
			return err
		}
		out = append(out, InventoryEntry{Path: rel, Bytes: int64(len(data)), SHA256: sha256Hex(data)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// String renders an entry for diagnostics.
func (e InventoryEntry) String() string { return fmt.Sprintf("%s %d %s", e.Path, e.Bytes, e.SHA256) }
