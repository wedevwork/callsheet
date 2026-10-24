package reale2e

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing/filemode"
)

// Workspace admission (design 12a-dev-flow-harness, FP-6). The supervisor
// compares the full pulled tree against the planned parent's: every added,
// modified and deleted path and every mode. Whole-tree equality is not
// required when the only intended change is a report. Each producer's
// complete artifact contract is enforced before any transition is chosen.

const (
	maxSourceFixtureBytes = 16 << 10
	maxArtifactBytes      = 32 << 10
	maxArtifactAggregate  = 128 << 10
	// maxTreeEntries bounds a result tree's root entries (the fixture has
	// at most eleven).
	maxTreeEntries = 64
	// reviewScratch is the code reviewer's in-checkout scratch root.
	reviewScratch = ".callsheet-review-scratch"
)

// devArtifacts are the seven planning/report files under the aggregate cap.
var devArtifacts = []string{"design.md", "revision-history.md", "design-questions.md", "design-review.md", "review.md", "rca.md", "fix.md"}

// devSourceFiles are the fixture whose combined size stays under 16 KiB.
var devSourceFiles = []string{"FEATURE.md", "go.mod", "main.go", "main_test.go"}

// devSpecFiles are the fixture's approved specification.
var devSpecFiles = []string{"design.md", "revision-history.md"}

// TreeFile is one regular file in a result tree.
type TreeFile struct {
	Path    string
	Mode    filemode.FileMode
	Content []byte
}

// admitInput is one publication to admit with its producer's contract
// inputs.
type admitInput struct {
	Role, Mode, Marker string
	Base, Result       []TreeFile
	// Final is the task's complete final answer.
	Final string
	// DesignDigest is the design.md digest the audit envelope supplied.
	DesignDigest string
	// Candidate is the code review's candidate input commit and TaskID
	// the reviewer's actual task (its scratch path).
	Candidate, TaskID string
	// ReviewRun is the SHA-256 of the verification helper's complete
	// receipt the captured log corroborates ("" when there is none).
	ReviewRun string
	// Scenario is the attempt's scenario ("" or full: the full flow) and
	// Seed the immutable initial seed tree a light brief's root-cause
	// reference must resolve in (design 12a-dev-flow-light, FP-2).
	Scenario string
	Seed     map[string]TreeFile
}

// admitResult is what an admitted publication supplies to Next.
type admitResult struct {
	Files          map[string]TreeFile
	Blocker        string
	DesignRequired string
	BriefCurrent   bool
	Report         string
}

// AdmitPublication checks a result tree against the producer's allowlist,
// the separate source and artifact budgets and the producer's complete
// artifact contract.
func AdmitPublication(in admitInput) (admitResult, error) {
	var res admitResult
	if err := AdmitTree(in.Role, in.Mode, in.Marker, in.Base, in.Result); err != nil {
		return res, err
	}
	bmap, _ := indexTree(in.Base)
	rmap, _ := indexTree(in.Result)
	res.Files = rmap
	switch in.Role {
	case "design-reviewer":
		report := fileText(rmap, "design-review.md")
		got, err := parseTerminalMarker(report)
		if err != nil || got != in.Marker {
			return res, fmt.Errorf("publication: design-review.md does not end with the final marker %s", in.Marker)
		}
		if !hex64RE.MatchString(in.DesignDigest) || in.DesignDigest != fileSHA(bmap, "design.md") || !strings.Contains(report, in.DesignDigest) {
			return res, fmt.Errorf("publication: design-review.md does not name the supplied design digest")
		}
		res.Report = report
	case "code-reviewer":
		report := fileText(rmap, "review.md")
		blocker, err := parseReviewBlocker(in.Final, report, in.Marker)
		if err != nil {
			return res, fmt.Errorf("publication: %w: the review blocker field", err)
		}
		check := ReviewReportOK
		if in.Mode == modeReviewLight {
			// Dispatched by plan mode, never by the report's wording.
			check = LightReviewReportOK
		}
		if err := check(report, in.Marker, in.Candidate, in.TaskID, bmap, in.ReviewRun); err != nil {
			return res, err
		}
		res.Report, res.Blocker = report, blocker
	case "triage":
		if in.Marker != markerTriageConf && in.Marker != markerTriageProb {
			break
		}
		got, err := parseTerminalMarker(fileText(rmap, "rca.md"))
		if err != nil || got != in.Marker {
			return res, fmt.Errorf("publication: rca.md does not state the %s confidence", in.Marker)
		}
		prev, had := bmap["fix.md"]
		cur := rmap["fix.md"]
		if had && bytes.Equal(prev.Content, cur.Content) {
			return res, fmt.Errorf("publication: fix.md is inherited, not published by this triage")
		}
		req, err := parseDesignRequired(string(cur.Content))
		if err != nil {
			return res, fmt.Errorf("publication: %w: the Design required field", err)
		}
		// The current brief's confidence must be the admitted verdict's.
		if conf, err := parseConfidence(string(cur.Content)); err != nil || "TRIAGE_"+conf != in.Marker {
			return res, fmt.Errorf("publication: fix.md has no single Confidence line matching %s", in.Marker)
		}
		if in.Scenario == scenarioLight {
			if _, _, err := parseRootCause(string(cur.Content), in.Seed); err != nil {
				return res, fmt.Errorf("publication: fix.md: %w", err)
			}
		}
		res.DesignRequired, res.BriefCurrent = req, true
	}
	return res, nil
}

// AdmitTree checks a result tree against the producer's allowlist and the
// separate source and artifact budgets. base is the planned parent tree.
func AdmitTree(role, mode, marker string, base, result []TreeFile) error {
	bmap, err := indexTree(base)
	if err != nil {
		return err
	}
	rmap, err := indexTree(result)
	if err != nil {
		return err
	}
	if err := budgets(rmap); err != nil {
		return err
	}
	allowed := devAllowedChanges(role, marker)
	for _, path := range changedPaths2(bmap, rmap) {
		if !allowed[path] {
			return fmt.Errorf("publication: %s is not an allowed change for %s", path, role)
		}
	}
	// Preservation holds whatever the marker: a diagnostic publication
	// (questions, NEEDS_DESIGN, NEEDS_TRIAGE, DESIGN_NEEDS_INPUT, BLOCKED)
	// deletes no input either. Before a draft exists there is nothing to
	// delete, so initial clarification is unaffected. Only the coder may
	// drop an answered (empty) questions file.
	for path, f := range bmap {
		if _, kept := rmap[path]; !kept && (role != "coder" || path != "design-questions.md" || len(bytes.TrimSpace(f.Content)) > 0) {
			return fmt.Errorf("publication: the %s deleted %s", role, path)
		}
	}
	switch role {
	case "designer":
		if marker == markerDesignDraft {
			if err := nonempty(rmap, "design.md", "revision-history.md"); err != nil {
				return err
			}
		}
		prev, had := bmap["design-questions.md"]
		cur, still := rmap["design-questions.md"]
		switch {
		case !had && still:
			return fmt.Errorf("publication: the designer cannot create design-questions.md")
		case had && !bytes.Equal(prev.Content, cur.Content) && len(bytes.TrimSpace(cur.Content)) > 0:
			return fmt.Errorf("publication: the designer may only empty design-questions.md")
		}
	case "design-reviewer":
		if err := nonempty(rmap, "design-review.md"); err != nil {
			return err
		}
	case "coder":
		if marker == markerImplComplete || marker == markerFixesComplete {
			if err := nonempty(rmap, "main.go", "main_test.go"); err != nil {
				return err
			}
			if q, ok := rmap["design-questions.md"]; ok && len(bytes.TrimSpace(q.Content)) > 0 {
				return fmt.Errorf("publication: questions remain unresolved")
			}
		}
		if marker == markerQuestions && !nonemptyOK(rmap, "design-questions.md") {
			return fmt.Errorf("publication: DESIGN_QUESTIONS requires a nonempty questions file")
		}
	case "code-reviewer":
		if err := nonempty(rmap, "review.md"); err != nil {
			return err
		}
	case "triage":
		if err := nonempty(rmap, "rca.md"); err != nil {
			return err
		}
		if (marker == markerTriageConf || marker == markerTriageProb) && !nonemptyOK(rmap, "fix.md") {
			return fmt.Errorf("publication: a qualified triage publishes fix.md")
		}
	default:
		return fmt.Errorf("publication: unknown role %s", role)
	}
	return nil
}

// changedPaths2 is every added, modified (bytes or mode) and deleted path,
// sorted.
func changedPaths2(base, result map[string]TreeFile) []string {
	var out []string
	for path, f := range result {
		prev, existed := base[path]
		if !existed || prev.Mode != f.Mode || !bytes.Equal(prev.Content, f.Content) {
			out = append(out, path)
		}
	}
	for path := range base {
		if _, ok := result[path]; !ok {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// devAllowedChanges is the producer's allowlist of changed paths.
// Deletion of an existing path is refused separately, for every marker.
func devAllowedChanges(role, marker string) map[string]bool {
	m := map[string]bool{}
	switch role {
	case "designer":
		m["design.md"], m["revision-history.md"], m["design-questions.md"] = true, true, true
	case "design-reviewer":
		m["design-review.md"] = true
	case "coder":
		m["main.go"], m["main_test.go"], m["design-questions.md"] = true, true, true
	case "code-reviewer":
		m["review.md"] = true
	case "triage":
		m["rca.md"] = true
		if marker == markerTriageConf || marker == markerTriageProb {
			m["fix.md"] = true
		}
	}
	return m
}

// indexTree indexes a root-only tree of regular nonexecutable UTF-8 files.
func indexTree(files []TreeFile) (map[string]TreeFile, error) {
	out := map[string]TreeFile{}
	if len(files) > maxTreeEntries {
		return nil, fmt.Errorf("publication: %d tree entries", len(files))
	}
	for _, f := range files {
		if f.Path == reviewScratch {
			return nil, fmt.Errorf("publication: scratch remnant %s", f.Path)
		}
		if f.Path == "" || strings.ContainsAny(f.Path, "/\\") || strings.HasPrefix(f.Path, ".") {
			return nil, fmt.Errorf("publication: unexpected path %q", f.Path)
		}
		if _, dup := out[f.Path]; dup {
			return nil, fmt.Errorf("publication: duplicate path %s", f.Path)
		}
		if f.Mode != filemode.Regular {
			return nil, fmt.Errorf("publication: %s is not a regular nonexecutable file", f.Path)
		}
		if !utf8.Valid(f.Content) || bytes.IndexByte(f.Content, 0) >= 0 {
			return nil, fmt.Errorf("publication: %s is not UTF-8 text", f.Path)
		}
		out[f.Path] = f
	}
	return out, nil
}

// sizeBudgets checks the source and artifact budgets from sizes alone.
func sizeBudgets(sizes map[string]int64) error {
	var source, artifacts int64
	for _, name := range devSourceFiles {
		source += sizes[name]
	}
	if source >= maxSourceFixtureBytes {
		return fmt.Errorf("publication: the source fixture is %d bytes (strictly below %d required)", source, maxSourceFixtureBytes)
	}
	for _, name := range devArtifacts {
		n, ok := sizes[name]
		if !ok {
			continue
		}
		if n > maxArtifactBytes {
			return fmt.Errorf("publication: %s exceeds 32 KiB", name)
		}
		artifacts += n
	}
	if artifacts > maxArtifactAggregate {
		return fmt.Errorf("publication: the artifacts are %d bytes (at most %d)", artifacts, maxArtifactAggregate)
	}
	for name, n := range sizes {
		if n > maxArtifactBytes {
			return fmt.Errorf("publication: %s exceeds 32 KiB", name)
		}
	}
	return nil
}

func budgets(files map[string]TreeFile) error {
	sizes := map[string]int64{}
	for name, f := range files {
		sizes[name] = int64(len(f.Content))
	}
	return sizeBudgets(sizes)
}

func nonempty(files map[string]TreeFile, names ...string) error {
	for _, n := range names {
		if !nonemptyOK(files, n) {
			return fmt.Errorf("publication: %s is missing or empty", n)
		}
	}
	return nil
}

func nonemptyOK(files map[string]TreeFile, name string) bool {
	f, ok := files[name]
	return ok && len(bytes.TrimSpace(f.Content)) > 0
}

// Code-review report fields (the development envelope's contract). Each is
// a plain field line outside code fences, exact case, never trimmed.
var (
	reviewExitRE     = regexp.MustCompile(`^([0-9]{1,3}): (go test .+)$`)
	reviewCoverageRE = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]+)?% [^ ].*$`)
)

// reviewRunField is review.md's reviewer-run digest field (design
// 12a-dev-flow-verify, Reviewer evidence; required by the exercise
// envelope only).
const reviewRunField = "Reviewer run sha256:"

// ReviewReportOK checks the fields the supervisor requires of review.md:
// the terminal marker agrees with the final one, the reviewed candidate
// commit, every candidate source file's SHA-256, both exact verification
// commands with their exits (the coverage profile inside this task's
// scratch), the coverage scope and value, and the cleanup result. It does
// not treat the prose as proof that the tests ran. runDigest is the
// SHA-256 of the verification helper's receipt the captured log
// corroborates: when set, review.md carries exactly one plain
// "Reviewer run sha256: <digest>" line equal to it; when empty, it
// carries none (and an approval is impossible).
func ReviewReportOK(report, marker, candidate, taskID string, candidateFiles map[string]TreeFile, runDigest string) error {
	got, err := parseTerminalMarker(report)
	if err != nil || got != marker {
		return fmt.Errorf("publication: review.md does not end with the final marker %s", marker)
	}
	runs, bad := fieldValues(report, reviewRunField)
	switch {
	case bad || len(runs) > 1 || len(runs) == 1 && !hex64RE.MatchString(runs[0]):
		return fmt.Errorf("publication: review.md has a malformed or repeated %q line", reviewRunField)
	case runDigest == "" && len(runs) != 0:
		return fmt.Errorf("publication: review.md names a reviewer run the captured log does not corroborate")
	case runDigest == "" && marker == markerReviewApproved:
		return fmt.Errorf("publication: an approving review.md has no corroborated reviewer run")
	case runDigest != "" && (len(runs) != 1 || runs[0] != runDigest):
		return fmt.Errorf("publication: review.md does not name the reviewer run sha256 %s", runDigest)
	}
	one := func(key string) (string, error) {
		vals, bad := fieldValues(report, key)
		if bad || len(vals) != 1 {
			return "", fmt.Errorf("publication: review.md needs exactly one %q line", key)
		}
		return vals[0], nil
	}
	v, err := one("Reviewed candidate:")
	if err != nil {
		return err
	}
	if !hex40RE.MatchString(candidate) || v != candidate {
		return fmt.Errorf("publication: review.md does not bind the candidate commit %s", candidate)
	}
	sources, bad := fieldValues(report, "Source sha256:")
	want := map[string]string{}
	for _, p := range devSourceFiles {
		if f, ok := candidateFiles[p]; ok {
			want[p] = sha256Hex(f.Content)
		}
	}
	seen := map[string]bool{}
	for _, s := range sources {
		p, sum, ok := strings.Cut(s, " ")
		if !ok || want[p] == "" || want[p] != sum || seen[p] {
			bad = true
		}
		seen[p] = true
	}
	if bad || len(seen) != len(want) {
		return fmt.Errorf("publication: review.md does not record every candidate source hash")
	}
	exits, bad := fieldValues(report, "Verification exit")
	var plain, cover int
	for _, e := range exits {
		m := reviewExitRE.FindStringSubmatch(e)
		switch {
		case m == nil:
			bad = true
		case m[2] == "go test ./...":
			plain++
		case strings.HasPrefix(m[2], "go test -coverprofile=") && strings.HasSuffix(m[2], "/coverage.out ./...") &&
			strings.Contains(m[2], reviewScratch+"/"+taskID+"/"):
			cover++
		default:
			bad = true
		}
	}
	if bad || plain != 1 || cover != 1 {
		return fmt.Errorf("publication: review.md does not record both exact verification commands and exits in this task's scratch")
	}
	if c, err := one("Coverage:"); err != nil || !reviewCoverageRE.MatchString(c) {
		return fmt.Errorf("publication: review.md does not record the coverage value and scope")
	}
	if c, err := one("Cleanup:"); err != nil || strings.TrimSpace(c) == "" {
		return fmt.Errorf("publication: review.md does not record the cleanup result")
	}
	return nil
}

func fileText(files map[string]TreeFile, name string) string {
	f, ok := files[name]
	if !ok {
		return ""
	}
	return string(f.Content)
}

func fileSHA(files map[string]TreeFile, name string) string {
	f, ok := files[name]
	if !ok {
		return ""
	}
	return sha256Hex(f.Content)
}

// treeDigests maps every file of a tree to its SHA-256.
func treeDigests(files map[string]TreeFile) map[string]string {
	out := map[string]string{}
	for p, f := range files {
		out[p] = sha256Hex(f.Content)
	}
	return out
}
