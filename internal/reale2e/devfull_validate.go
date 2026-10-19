package reale2e

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Final-candidate validation (design 12a-dev-flow-verify, Candidate
// oracle). After the approving review's consumed wait and the clean
// coordinator exit, and before runtime cleanup, the supervisor exports the
// replay-derived last implementation-ready candidate (never the review
// publication, never a fixed ordinal) after full development tree
// admission, and runs, within one shared deadline min(start+60s,
// attempt+90m), go version, the unchanged go test, the build, the ten
// fixed CLI cases, go test -json and the coverage run. The records are
// independent final evidence, not reviewer evidence. A failure keeps the
// partial records and stops the attempt; nothing is retried.

// DevCoverageRecord is validation/coverage.json.
type DevCoverageRecord struct {
	Schema   string       `json:"schema"`
	RunID    string       `json:"run_id"`
	Commit   string       `json:"commit"`
	Tree     string       `json:"tree"`
	Exec     ExecRecord   `json:"exec"`
	Profile  string       `json:"profile_sha256"`
	Coverage *DevCoverage `json:"coverage"`
	Error    string       `json:"error"`
	// FinalStdout and FinalStderr are the SHA-256 of the final go test's
	// captured streams (validation/final-stdout.txt and final-stderr.txt),
	// which its legacy-shaped run record does not hash.
	FinalStdout string `json:"final_stdout_sha256"`
	FinalStderr string `json:"final_stderr_sha256"`
}

// devTreePolicy is the development candidate's export policy: the source
// fixture plus the seven planning/report artifacts as inert files.
func devTreePolicy() map[string]bool {
	m := map[string]bool{}
	for _, n := range append(append([]string{}, devSourceFiles...), devArtifacts...) {
		m[n] = true
	}
	return m
}

// Final validation stop codes.
const (
	CodeCoverageBelow      = "coverage_below_bar"
	CodeCoverageUnobserved = "coverage_unobserved"
)

// candidateTree is the admitted commit's tree hash.
func (d *devSupervisor) candidateTree(commit string) string {
	for _, c := range d.commits {
		if c.Commit == commit {
			return c.Tree
		}
	}
	return ""
}

// runDevFinalValidation validates the final candidate and reports whether
// it passed (a failure stops the attempt).
func (d *devSupervisor) runDevFinalValidation() bool {
	cand := d.state.CandidateCommit
	tree, ok := d.trees[cand]
	treeHash := d.candidateTree(cand)
	if !ok || treeHash == "" {
		d.stop("final_tests_failed", "the final candidate is not an admitted publication")
		return false
	}
	var list []TreeFile
	files := map[string][]byte{}
	for _, name := range sortedTree(tree) {
		list = append(list, tree[name])
		files[name] = tree[name].Content
	}
	if _, err := indexTree(list); err != nil {
		d.stop("final_tests_failed", err.Error())
		return false
	}
	if err := budgets(tree); err != nil {
		d.stop("final_tests_failed", err.Error())
		return false
	}
	start := d.s.w.Clock.Now()
	deadline := minTime(start.Add(FinalTestBound), d.attemptDeadline())
	mark := func(typ, name string) int { return d.s.ev.event(Event{Type: typ, Code: name}).Seq }
	v := runFinalValidationWith(validationOptions{run: d.s.command, clock: d.s.w.Clock, goBin: d.s.goBin, pathEnv: d.s.pathEnv,
		dir: filepath.Join(d.s.runtime, "final"), files: files, commit: cand, tree: treeHash, mark: mark, deadline: deadline,
		allowed: devTreePolicy(), cover: true})
	cov := DevCoverageRecord{Schema: devKind("coverage"), RunID: d.s.runID, Commit: cand, Tree: treeHash, Profile: sha256Hex(v.Profile),
		FinalStdout: sha256Hex([]byte(toValidUTF8(v.TestOut))), FinalStderr: sha256Hex([]byte(toValidUTF8(v.TestErr)))}
	if v.Cover != nil {
		cov.Exec = *v.Cover
	} else {
		cov.Exec = ExecRecord{Schema: ExecSchema, Name: "go_test_cover", Argv: validationArgvCover, Commit: cand, Tree: treeHash}
	}
	seedMain := fileContent(d.trees[d.state.InitialCommit], "main.go")
	if c, err := deriveChangedCoverage(seedMain, files["main.go"], v.Profile, modulePathOf(files["go.mod"])); err == nil {
		cov.Coverage = &c
	} else {
		cov.Error = contract.SafeText(err.Error(), 200)
	}
	d.writeValidation(v, cov)
	d.s.ev.event(Event{Type: EvFinalTests, Hop: workflowLabel(len(d.state.Tasks)), Task: d.state.ReviewTask})
	if d.halted() {
		return false
	}
	casesOK := v.Cases.Build.Started && v.Cases.Build.ExitCode == 0 && len(v.Cases.Cases) == len(FeatureCases)
	for _, c := range v.Cases.Cases {
		casesOK = casesOK && c.Pass
	}
	switch {
	case v.Err != nil || v.Test.ExitCode != 0 || v.Test.TimedOut || v.Test.Toolchain == "":
		d.stop("final_tests_failed", fmt.Sprintf("go test exited %d: %v", v.Test.ExitCode, v.Err))
	case !casesOK:
		d.stop(CodeCaseFailed, "a CLI feature case did not produce its required result")
	case len(v.Reqs.MissingTests) > 0 || len(v.Reqs.FailedTests) > 0 || !v.Reqs.RunSignature || !v.Reqs.TableTests || !v.Reqs.TestCallsRun:
		d.stop(CodeTestMapping, "a required TestRun/<case-id> subtest or the run/table structure is missing or failing")
	case !v.Reqs.TestRunPassed:
		d.stop(CodeValidationFail, fmt.Sprintf("go test -json did not pass (failures %v)", v.Reqs.TestFailures))
	case v.Cover == nil || !v.Cover.Started || v.Cover.ExitCode != 0 || v.Cover.TimedOut || cov.Coverage == nil:
		d.stop(CodeCoverageUnobserved, "the final coverage run gave no usable profile")
	case !cov.Coverage.Pass:
		d.stop(CodeCoverageBelow, fmt.Sprintf("changed-code coverage %d/%d is not above 80%%", cov.Coverage.Covered, cov.Coverage.Total))
	default:
		d.s.say("final validation of candidate %s passed (changed-code coverage %s%%, %d/%d)", cand, cov.Coverage.Percent, cov.Coverage.Covered,
			cov.Coverage.Total)
		return true
	}
	return false
}

// writeValidation writes every final validation record that exists.
func (d *devSupervisor) writeValidation(v finalValidation, cov DevCoverageRecord) {
	var errs []error
	w := func(rel string, data []byte) { errs = append(errs, d.s.ev.writeFile(rel, data)) }
	j := func(rel string, x any) { errs = append(errs, d.s.ev.writeJSON(rel, x)) }
	j("validation/final-test.json", v.Test)
	w("validation/final-stdout.txt", []byte(toValidUTF8(v.TestOut)))
	w("validation/final-stderr.txt", []byte(toValidUTF8(v.TestErr)))
	j("validation/cases.json", v.Cases)
	w(buildStdoutFile, []byte(toValidUTF8(v.BuildOut)))
	w(buildStderrFile, []byte(toValidUTF8(v.BuildErr)))
	w("validation/tests.jsonl", []byte(toValidUTF8(v.TestJSON)))
	w("validation/test-json-stderr.txt", []byte(toValidUTF8(v.TestJSONEr)))
	j("validation/requirements.json", v.Reqs)
	if v.Profile != nil {
		w("validation/coverage.out", []byte(toValidUTF8(v.Profile)))
	}
	w("validation/coverage-stdout.txt", []byte(toValidUTF8(v.CoverOut)))
	w("validation/coverage-stderr.txt", []byte(toValidUTF8(v.CoverErr)))
	j(devCoverageFile, cov)
	if err := errors.Join(errs...); err != nil {
		d.stop("evidence_write_failed", err.Error())
	}
}

// finalDeadline is the shared final-validation deadline for a start.
func finalDeadline(start, attempt time.Time) time.Time {
	return minTime(start.Add(FinalTestBound), attempt.Add(AttemptBound))
}
