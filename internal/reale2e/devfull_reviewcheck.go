package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Reviewer execution evidence (design 12a-dev-flow-verify, Reviewer
// evidence): the pure part shared by the collector and the offline
// checker. The invocation recognizer accepts only the helper's exact
// argv; the log extraction reads only complete top-level vendor command
// records; the derivation cross-checks the helper's receipt against the
// trusted plan, task and objects and recomputes its claims. Nothing here
// starts a process or reads outside the given bytes.

// The invocation recognizer and log extraction.

// codexCommand is one top-level vendor command_execution record.
type codexCommand struct {
	line      int
	kind      string // item.started or item.completed
	id        string
	command   string
	output    string
	exit      *int
	status    string
	outputRaw json.RawMessage
}

// errAmbiguousLog is a log line that looks like a command record but is
// not one complete top-level record.
var errAmbiguousLog = errors.New("an ambiguous or split command record")

// parseCodexCommands reads every top-level command_execution record of a
// merged task log. Lines that are not JSON stay opaque diagnostics; a line
// mentioning command_execution that is not one complete top-level record,
// or a malformed one, fails. Text embedded in agent messages or another
// command's output is never a record.
func parseCodexCommands(log []byte) ([]codexCommand, error) {
	var out []codexCommand
	for i, l := range bytes.Split(log, []byte("\n")) {
		l = bytes.TrimSuffix(l, []byte("\r"))
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		mentions := bytes.Contains(l, []byte(`command_execution`))
		var top struct {
			Type string          `json:"type"`
			Item json.RawMessage `json:"item"`
		}
		if !json.Valid(l) || json.Unmarshal(l, &top) != nil || !rawKeysUnique(l) {
			if mentions {
				return nil, errAmbiguousLog
			}
			continue
		}
		if !strings.HasPrefix(top.Type, "item.") || top.Item == nil {
			continue
		}
		var item struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Command *string         `json:"command"`
			Output  json.RawMessage `json:"aggregated_output"`
			Exit    *int            `json:"exit_code"`
			Status  string          `json:"status"`
		}
		if json.Unmarshal(top.Item, &item) != nil {
			if mentions {
				return nil, errAmbiguousLog
			}
			continue
		}
		if item.Type != "command_execution" {
			continue
		}
		if (top.Type != "item.started" && top.Type != "item.completed") || item.ID == "" || item.Command == nil {
			return nil, errAmbiguousLog
		}
		var output string
		if item.Output != nil && json.Unmarshal(item.Output, &output) != nil {
			return nil, errAmbiguousLog
		}
		out = append(out, codexCommand{line: i + 1, kind: top.Type, id: item.ID, command: *item.Command, output: output, exit: item.Exit,
			status: item.Status, outputRaw: item.Output})
	}
	return out, nil
}

// helperInvocation reports whether a recorded command is exactly the
// helper's argv: the plain words joined by single spaces, either directly
// or as the whole single-quoted argument of /bin/bash -lc. No other
// prefix, operator, assignment, substitution or wrapper is accepted; no
// shell is parsed or evaluated.
func helperInvocation(command string, argv []string) bool {
	for _, a := range argv {
		if a == "" || validRuleInputs(a) != nil || strings.ContainsAny(a, shellOperators+" \t") {
			return false
		}
	}
	plain := strings.Join(argv, " ")
	return command == plain || command == "/bin/bash -lc '"+plain+"'"
}

// mentionsHelper reports a command that names the helper at all.
func mentionsHelper(command string) bool { return strings.Contains(command, "dev-review-verify") }

// DevLogMeta is tasks/NN/logs.json: the TaskLogs metadata as the plane
// returned it, and what the evidence stored.
type DevLogMeta struct {
	SourceBytes     int    `json:"source_bytes"`
	RetainedBytes   int    `json:"retained_bytes"`
	DroppedBytes    int    `json:"dropped_bytes"`
	Truncated       bool   `json:"truncated"`
	Incomplete      bool   `json:"incomplete"`
	CounterOverflow bool   `json:"counter_overflow"`
	MayBeIncomplete bool   `json:"log_may_be_incomplete"`
	DataBytes       int    `json:"data_bytes"`
	RawSHA256       string `json:"raw_sha256"`
	StoredBytes     int    `json:"stored_bytes"`
	StoredTruncated bool   `json:"stored_truncated"`
	StoredChanged   bool   `json:"stored_changed"`
	Error           string `json:"error"`
}

// complete reports a log retained without any gap, truncation or loss,
// stored byte for byte.
func (m DevLogMeta) complete() bool {
	return m.Error == "" && m.SourceBytes == m.RetainedBytes && m.RetainedBytes == m.DataBytes && m.DroppedBytes == 0 && !m.Truncated &&
		!m.Incomplete && !m.CounterOverflow && !m.MayBeIncomplete && !m.StoredTruncated && !m.StoredChanged && m.StoredBytes == m.DataBytes
}

// SourceRange is an inclusive 1-based line range of a bundle file.
type SourceRange struct {
	File      string `json:"file"`
	SHA256    string `json:"sha256"`
	FirstLine int    `json:"first_line"`
	LastLine  int    `json:"last_line"`
}

// DevReviewEvidence is tasks/NN/review-evidence.json: the reviewer
// execution derived from the captured log and objects.
type DevReviewEvidence struct {
	Ordinal    int          `json:"ordinal"`
	Task       string       `json:"task"`
	Marker     string       `json:"marker"`
	Status     string       `json:"status"`
	Code       string       `json:"code"`
	LogSHA256  string       `json:"log_sha256"`
	LogBytes   int          `json:"log_bytes"`
	ItemID     string       `json:"item_id"`
	Command    string       `json:"command"`
	Started    *SourceRange `json:"started"`
	Completed  *SourceRange `json:"completed"`
	HelperExit *int         `json:"helper_exit"`
	RunSHA256  string       `json:"run_sha256"`
	RunBytes   int          `json:"run_bytes"`
	RunStatus  string       `json:"run_status"`
	Coverage   *DevCoverage `json:"coverage"`
	Approving  bool         `json:"approving"`
	// RejectedEntry is the receipt's rejected checkout entry, retained as
	// explanatory evidence (design 12a-dev-flow-verify r0.10).
	RejectedEntry *ReviewRejectedEntry `json:"rejected_entry,omitempty"`
	// Light is a light review's derived facts (design 12a-dev-flow-light;
	// absent for a full review).
	Light *LightReviewFacts `json:"light,omitempty"`
}

// Review evidence statuses.
const (
	evidenceVerified   = "verified"
	evidenceFailedRun  = "failed_run"
	evidenceUnobserved = "unobserved"
	evidenceMismatch   = "mismatch"
)

// reviewEvidenceInput is everything the derivation trusts: the captured
// log and its metadata, the authoritative task and its plan's helper
// recipe, the candidate and seed trees and the published report.
type reviewEvidenceInput struct {
	Ordinal    int
	Log        []byte
	LogFile    string
	Meta       DevLogMeta
	Task       string
	Helper     reviewHelperSpec
	Candidate  string
	Initial    string
	CandTree   string
	CandFiles  map[string]TreeFile
	SeedMain   []byte
	AdmittedAt time.Time
	FinishedAt time.Time
	Marker     string
	Report     string
	// RequireExport requires the receipt's export manifests (review 2d.4
	// round 1, C2): every live derivation and every v2 bundle; a v1
	// bundle's historical receipts have none.
	RequireExport bool
}

// DeriveDevReviewEvidence derives one code review's execution evidence.
// It blocks an approval unless the helper's complete successful receipt
// is corroborated by exactly one captured top-level command record; a
// complete failed receipt permits changes requested; a review that never
// started the helper has no evidence (unobserved).
func DeriveDevReviewEvidence(in reviewEvidenceInput) DevReviewEvidence {
	ev := DevReviewEvidence{Ordinal: in.Ordinal, Task: in.Task, Marker: in.Marker, LogSHA256: sha256Hex(in.Log), LogBytes: len(in.Log),
		Approving: in.Marker == markerReviewApproved}
	set := func(status, code string) DevReviewEvidence {
		ev.Status, ev.Code = status, code
		return ev
	}
	if !in.Meta.complete() {
		return set(evidenceUnobserved, "review_log_incomplete")
	}
	cmds, err := parseCodexCommands(in.Log)
	if err != nil {
		return set(evidenceUnobserved, "review_log_ambiguous")
	}
	argv := in.Helper.argv(in.Task, in.Candidate, in.Initial)
	var started, completed []codexCommand
	for _, c := range cmds {
		if !mentionsHelper(c.command) {
			continue
		}
		if !helperInvocation(c.command, argv) {
			return set(evidenceMismatch, "review_command_mismatch")
		}
		if c.kind == "item.started" {
			started = append(started, c)
		} else {
			completed = append(completed, c)
		}
	}
	if len(started) == 0 && len(completed) == 0 {
		return set(evidenceUnobserved, "review_helper_not_started")
	}
	if len(started) != 1 || len(completed) != 1 || started[0].id != completed[0].id || started[0].command != completed[0].command ||
		started[0].line >= completed[0].line {
		return set(evidenceMismatch, "review_command_records_invalid")
	}
	s, c := started[0], completed[0]
	ev.ItemID, ev.Command = c.id, c.command
	ev.Started = &SourceRange{File: in.LogFile, SHA256: ev.LogSHA256, FirstLine: s.line, LastLine: s.line}
	ev.Completed = &SourceRange{File: in.LogFile, SHA256: ev.LogSHA256, FirstLine: c.line, LastLine: c.line}
	ev.HelperExit = c.exit
	if c.status != "completed" || c.exit == nil {
		return set(evidenceMismatch, "review_command_incomplete")
	}
	// Design 12a-dev-flow-verify r0.11: the helper's stdout is its JSON
	// receipt line, then the report-line trailer; only the receipt line
	// (with its LF) is the run and its digest. Historical receipt-only
	// output has no trailer.
	out := []byte(c.output)
	i := bytes.IndexByte(out, '\n')
	if i < 0 {
		ev.RunSHA256, ev.RunBytes = sha256Hex(out), len(out)
		return set(evidenceUnobserved, "review_receipt_incomplete")
	}
	receipt, trailer := out[:i+1], out[i+1:]
	ev.RunSHA256, ev.RunBytes = sha256Hex(receipt), len(receipt)
	if len(receipt) > maxReviewRunBytes || len(trailer) > maxReviewTrailerBytes {
		return set(evidenceUnobserved, "review_receipt_incomplete")
	}
	var run DevReviewRun
	if err := decodeStrict(out[:i], &run); err != nil || run.Schema != DevReviewRunSchema {
		return set(evidenceUnobserved, "review_receipt_incomplete")
	}
	ev.RunStatus = run.Status
	if code := checkReviewRun(in, run, *c.exit); code != "" {
		return set(evidenceMismatch, code)
	}
	ev.RejectedEntry = run.RejectedEntry
	ev.Coverage = run.Coverage
	if len(trailer) > 0 {
		// The trailer is exactly the shared renderer's lines for this
		// receipt, and review.md carries each of them exactly once.
		if want, ok := renderReviewReportLines(run, receipt); !ok || want != string(trailer) {
			return set(evidenceMismatch, "review_report_lines_mismatch")
		}
		if code := checkReviewTrailerClaims(in.Report, string(trailer)); code != "" {
			return set(evidenceMismatch, code)
		}
	}
	if code := checkReviewReportClaims(in.Report, run); code != "" {
		return set(evidenceMismatch, code)
	}
	if run.Status != reviewPassed {
		if ev.Approving {
			return set(evidenceMismatch, "review_approved_failed_run")
		}
		return set(evidenceFailedRun, run.Code)
	}
	return set(evidenceVerified, "")
}

// checkReviewRun cross-checks a receipt against the trusted plan, task and
// objects, and recomputes its derived claims.
func checkReviewRun(in reviewEvidenceInput, run DevReviewRun, exit int) string {
	want := reviewHelperSpec{Go: in.Helper.Go, Deadline: in.Helper.Deadline}
	switch {
	case run.Task != in.Task || run.Candidate != in.Candidate || run.InitialSource != in.Initial:
		return "review_receipt_binding"
	case run.Go.Path != want.Go || run.Deadline != want.Deadline:
		return "review_receipt_binding"
	case !hex64RE.MatchString(run.Go.SHA256) && (run.Status != reviewRefused || run.Go.SHA256 != ""):
		// A refusal before the toolchain was read (at checkout
		// inspection, r0.10's unexpected_file included) records no digest.
		return "review_receipt_binding"
	case run.InputTree != in.CandTree && (run.Status != reviewRefused || run.InputTree != ""):
		return "review_receipt_binding"
	case !filepath.IsAbs(run.Scratch) || filepath.Clean(run.Scratch) != run.Scratch || !strings.HasSuffix(run.Scratch, "/"+reviewScratch+"/"+in.Task):
		return "review_receipt_binding"
	}
	// The inputs: every candidate file but review.md, regular and
	// nonexecutable, with its pinned bytes (a refusal before the checkout
	// was inspected records none).
	inputs := []ReviewInputFile{}
	for _, name := range sortedTree(in.CandFiles) {
		if name != "review.md" {
			inputs = append(inputs, ReviewInputFile{Path: name, SHA256: sha256Hex(in.CandFiles[name].Content), Mode: filemode.Regular.String()})
		}
	}
	if !slices.Equal(run.Inputs, inputs) && (run.Status != reviewRefused || len(run.Inputs) != 0) {
		return "review_inputs_mismatch"
	}
	// The after/ export's manifests, against the pinned objects: present
	// on every run that created its scratch (when required), never on a
	// refusal.
	wantExport := treeInputs(in.CandFiles)
	exportIntact := run.Export != nil && slices.Equal(run.Export.Created, wantExport) && slices.Equal(run.Export.After, wantExport)
	switch {
	case run.Export != nil && run.Status == reviewRefused:
		return "review_export_mismatch"
	case run.Export == nil && in.RequireExport && run.Status != reviewRefused:
		return "review_export_mismatch"
	}
	start, ok1 := parseTime(run.Start)
	end, ok2 := parseTime(run.End)
	deadline, ok3 := parseHelperDeadline(run.Deadline)
	if !ok1 || !ok2 || !ok3 || end.Before(start) || start.Before(in.AdmittedAt) || !end.Before(in.AdmittedAt.Add(FeatureTaskBound)) ||
		end.After(deadline) || (!in.FinishedAt.IsZero() && end.After(in.FinishedAt)) {
		return "review_receipt_time"
	}
	// Each recorded command: the fixed name and argv in order, in after/,
	// within the run's own window.
	last := start
	for i, c := range run.Commands {
		if i >= len(reviewCommandNames) || c.Name != reviewCommandNames[i] || !slices.Equal(c.Argv, reviewLogicalArgv(c.Name, run.Scratch)) ||
			c.Cwd != run.Scratch+"/after" {
			return "review_command_mismatch"
		}
		cs, ok1 := parseTime(c.Start)
		ce, ok2 := parseTime(c.End)
		if !ok1 || !ok2 || cs.Before(last) || ce.Before(cs) || end.Before(ce) {
			return "review_receipt_time"
		}
		last = ce
	}
	switch p := run.Profile; {
	case p.Present && p.SHA256 != sha256Hex([]byte(p.Text)), !p.Present && (p.SHA256 != "" || p.Text != ""):
		return "review_profile_mismatch"
	}
	var cov *DevCoverage
	if run.Profile.Present {
		if c, err := deriveChangedCoverage(in.SeedMain, fileContent(in.CandFiles, "main.go"), []byte(run.Profile.Text),
			modulePathOf(fileContent(in.CandFiles, "go.mod"))); err == nil {
			cov = &c
		}
	}
	if !bytes.Equal(mustJSON(cov), mustJSON(run.Coverage)) {
		return "review_coverage_mismatch"
	}
	tests := ReviewTests{Required: requiredTestNames(), Missing: []string{}, Failures: []string{}}
	if c := commandNamed(run.Commands, "go_test_json"); c != nil {
		tests = reviewTestsOf([]byte(c.Stdout), *c)
	}
	if !bytes.Equal(mustJSON(tests), mustJSON(run.Tests)) {
		return "review_tests_mismatch"
	}
	// The run's own verdict, recomputed: every command clean, coverage
	// above the bar, the tests passed, the scratch removed and the inputs
	// unchanged.
	ok := len(run.Commands) == len(reviewCommandNames) && cov != nil && cov.Pass && tests.Passed && run.ScratchCreated && run.ScratchRemoved &&
		run.InputsUnchanged && slices.Equal(run.InputsAfter, run.Inputs) && !run.OutputsOmitted && (exportIntact || run.Export == nil && !in.RequireExport)
	for _, c := range run.Commands {
		ok = ok && c.Started && !c.TimedOut && !c.Truncated && c.ProcessGone && c.Exit == 0 && c.Signal == ""
	}
	if (run.Status == reviewPassed) != ok || (run.Status == reviewPassed) != (exit == 0) || (exit != 0 && exit != 1) {
		return "review_status_mismatch"
	}
	if run.Status != reviewPassed && run.Status != reviewFailed && run.Status != reviewRefused {
		return "review_status_mismatch"
	}
	// The optional rejected checkout entry (r0.10): a root basename and a
	// known kind, never on a successful receipt. It explains a refusal; it
	// proves nothing about the filesystem.
	if e := run.RejectedEntry; e != nil && (run.Status == reviewPassed || e.Name == "" || e.Name == "." || e.Name == ".." ||
		strings.ContainsAny(e.Name, "/\x00") || !slices.Contains(reviewEntryKinds, e.Kind)) {
		return "review_rejected_entry_invalid"
	}
	return ""
}

// maxReviewTrailerBytes bounds the helper's report-line trailer (r0.11);
// the whole helper stdout is at most maxReviewRunBytes plus this.
const maxReviewTrailerBytes = 8 << 10

// reviewPercentRE is a derived coverage percent the report can state.
var reviewPercentRE = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]+)?$`)

// renderReviewReportLines is the one pure report-line renderer the
// helper's emission and the derivation share (design 12a-dev-flow-verify
// r0.11): from receipt run and its finalized JSON line receipt (with its
// LF), in order, the reviewed candidate, the four source digests, the
// two verification commands with their recorded exits (each command's
// exact logical argv, reviewLogicalArgv, in the receipt's absolute task
// scratch), the derived coverage percent, the cleanup booleans and the
// receipt digest. It renders only facts the receipt actually has (no
// line for an unrun command or an absent coverage) and reports false when
// the lines exceed their bound.
func renderReviewReportLines(run DevReviewRun, receipt []byte) (string, bool) {
	var b strings.Builder
	if hex40RE.MatchString(run.Candidate) {
		fmt.Fprintf(&b, "Reviewed candidate: %s\n", run.Candidate)
	}
	for _, p := range devSourceFiles {
		for _, in := range run.Inputs {
			if in.Path == p && hex64RE.MatchString(in.SHA256) {
				fmt.Fprintf(&b, "Source sha256: %s %s\n", p, in.SHA256)
				break
			}
		}
	}
	scratchOK := contract.ValidTaskID(run.Task) && filepath.IsAbs(run.Scratch) && filepath.Clean(run.Scratch) == run.Scratch &&
		strings.HasSuffix(run.Scratch, "/"+reviewScratch+"/"+run.Task) && !strings.ContainsAny(run.Scratch, "\r\n\x00")
	for _, name := range []string{"go_test", "go_test_cover"} {
		var c *ReviewCommand
		n := 0
		for i := range run.Commands {
			if run.Commands[i].Name == name {
				c, n = &run.Commands[i], n+1
			}
		}
		if n != 1 || !c.Started || c.Exit < 0 || c.Exit > 999 || !scratchOK || !slices.Equal(c.Argv, reviewLogicalArgv(name, run.Scratch)) {
			continue
		}
		fmt.Fprintf(&b, "Verification exit %d: %s\n", c.Exit, strings.Join(c.Argv, " "))
	}
	if run.Coverage != nil && reviewPercentRE.MatchString(run.Coverage.Percent) {
		fmt.Fprintf(&b, "Coverage: %s%% changed production statements\n", run.Coverage.Percent)
	}
	fmt.Fprintf(&b, "Cleanup: scratch_created=%t; scratch_removed=%t; inputs_unchanged=%t\n", run.ScratchCreated, run.ScratchRemoved, run.InputsUnchanged)
	fmt.Fprintf(&b, "%s %s\n", reviewRunField, sha256Hex(receipt))
	out := b.String()
	return out, len(out) <= maxReviewTrailerBytes
}

// reviewTrailerKeys are the report-line fields, in rendering order.
var reviewTrailerKeys = []string{"Reviewed candidate:", "Source sha256:", "Verification exit", "Coverage:", "Cleanup:", reviewRunField}

// checkReviewTrailerClaims requires review.md to carry every rendered
// report line's field exactly as rendered, once each (the four sources and
// the two verification commands distinct), with no other line of a
// rendered field's key: copied lines are compared, never trusted as
// authority (the receipt checks stand on their own).
func checkReviewTrailerClaims(report, trailer string) string {
	want := map[string][]string{}
	for _, l := range strings.Split(strings.TrimSuffix(trailer, "\n"), "\n") {
		for _, k := range reviewTrailerKeys {
			if v, ok := strings.CutPrefix(l, k+" "); ok {
				want[k] = append(want[k], v)
				break
			}
		}
	}
	for k, vs := range want {
		got, bad := fieldValues(report, k)
		got, vs = slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(vs))
		if bad || !slices.Equal(got, vs) || len(slices.Compact(slices.Clone(got))) != len(got) {
			return "review_report_lines_mismatch"
		}
	}
	return ""
}

// reportLinesComplete reports a trailer with every field a successful
// receipt must supply: the candidate, the four sources, exactly the two
// verification lines and the coverage.
func reportLinesComplete(trailer string) bool {
	count := func(prefix string) int {
		n := 0
		for _, l := range strings.Split(trailer, "\n") {
			if strings.HasPrefix(l, prefix) {
				n++
			}
		}
		return n
	}
	return count("Reviewed candidate: ") == 1 && count("Source sha256: ") == len(devSourceFiles) && count("Verification exit ") == 2 &&
		count("Coverage: ") == 1
}

// checkReviewReportClaims compares review.md's command, coverage and
// hash claims with the receipt.
func checkReviewReportClaims(report string, run DevReviewRun) string {
	exits, _ := fieldValues(report, "Verification exit")
	got := map[string]string{}
	for _, e := range exits {
		if m := reviewExitRE.FindStringSubmatch(e); m != nil {
			got[m[2]] = m[1]
		}
	}
	for _, name := range []string{"go_test", "go_test_cover"} {
		c := commandNamed(run.Commands, name)
		if c == nil {
			continue
		}
		cmd := strings.Join(c.Argv, " ")
		if got[cmd] != itoa(c.Exit) {
			return "review_report_exit_mismatch"
		}
	}
	if run.Coverage != nil {
		vals, _ := fieldValues(report, "Coverage:")
		if len(vals) != 1 || !strings.HasPrefix(vals[0], run.Coverage.Percent+"% ") {
			return "review_report_coverage_mismatch"
		}
	}
	return ""
}

// blocks reports whether evidence stops the attempt for a review with
// marker, and the stop code.
func (ev DevReviewEvidence) blocks() (string, bool) {
	switch {
	case ev.Status == evidenceMismatch:
		return CodeReviewMismatch, true
	case ev.Approving && ev.Status != evidenceVerified:
		return CodeReviewUnobserved, true
	}
	return "", false
}

// runDigest is the receipt digest review.md must name ("" when the
// helper never produced a complete receipt).
func (ev DevReviewEvidence) runDigest() string {
	if ev.Status == evidenceVerified || ev.Status == evidenceFailedRun {
		return ev.RunSHA256
	}
	return ""
}
