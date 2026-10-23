package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"
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
	out := []byte(c.output)
	ev.RunSHA256, ev.RunBytes = sha256Hex(out), len(out)
	if len(out) == 0 || len(out) > maxReviewRunBytes || out[len(out)-1] != '\n' || bytes.Count(out, []byte("\n")) != 1 {
		return set(evidenceUnobserved, "review_receipt_incomplete")
	}
	var run DevReviewRun
	if err := decodeStrict(bytes.TrimSuffix(out, []byte("\n")), &run); err != nil || run.Schema != DevReviewRunSchema {
		return set(evidenceUnobserved, "review_receipt_incomplete")
	}
	ev.RunStatus = run.Status
	if code := checkReviewRun(in, run, *c.exit); code != "" {
		return set(evidenceMismatch, code)
	}
	ev.RejectedEntry = run.RejectedEntry
	ev.Coverage = run.Coverage
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
		run.InputsUnchanged && slices.Equal(run.InputsAfter, run.Inputs) && !run.OutputsOmitted
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
