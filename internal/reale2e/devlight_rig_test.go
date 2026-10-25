package reale2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// The light rig (design 12a-dev-flow-light): the shared development rig
// running run-dev-light with the semantic fake Go, the owner's light
// intake answered as a fresh line right after its prompt, and scripted
// light workers: a triage brief with its root-cause reference, the coder's
// restoration and regression, and a light code review running the light
// helper and pasting its report lines.

// lightHappy is the light happy path: triage, the defect coder and the
// approving light review.
func lightHappy() []devAct {
	return []devAct{{marker: markerTriageConf, designRequired: designRequiredNo}, {marker: markerImplComplete}, {marker: markerReviewApproved}}
}

// newLightRig is a light attempt's rig.
func newLightRig(t testing.TB, steps []devAct) *devRig {
	t.Helper()
	r := newDevRig(t, steps)
	r.command = "run-dev-light"
	r.f.light = true
	r.intake = intakeLight
	return r
}

// answerIntake types the rig's intake answer as a fresh line after the
// prompt: light, no, a wrong attempt ID, EOF, or nothing (expiry).
func (r *devRig) answerIntake() {
	run := r.d.s.runID
	switch r.intake {
	case intakeLight:
		r.term.typeLine("light " + run)
	case intakeNo:
		r.term.typeLine("no " + run)
	case "wrong":
		r.term.typeLine("light 20990101T000000Z-00000000")
		r.term.typeLine("yes " + run)
	case intakeEOF:
		r.term.close()
	}
}

// lightDefectLineNo is the seed main.go's defect line number.
func lightDefectLineNo(r *devRig) int {
	return lightDefectOf(fileContent(r.d.trees[r.d.state.InitialCommit], "main.go")).Line
}

// lightPublication is a scripted light worker's visible tree.
func (r *devRig) lightPublication(act devAct, doc devDispatchDoc, base map[string][]byte, task string) map[string][]byte {
	files := map[string][]byte{}
	for k, v := range base {
		files[k] = v
	}
	n := doc.Ordinal
	switch doc.Role {
	case "triage":
		switch act.marker {
		case markerTriageConf, markerTriageProb:
			files["rca.md"] = []byte(fmt.Sprintf("# RCA %d\n\nThe empty-name check trims the name, so a whitespace-only name is refused.\n\nSTATUS: %s\n", n, act.marker))
			confidence := strings.TrimPrefix(act.marker, "TRIAGE_")
			root := fmt.Sprintf("Root cause: main.go:%d\n", lightDefectLineNo(r))
			switch act.alter {
			case "no-root":
				root = ""
			case "two-roots":
				root += "Root cause: main_test.go:1\n"
			case "root-range":
				root = "Root cause: main.go:999\n"
			case "root-other-path":
				root = "Root cause: FEATURE.md:1\n"
			case "lookalike":
				root = "Root cause: the TrimSpace call rejects whitespace\n" + root
			case "lookalike-only":
				root = "Root cause: the TrimSpace call rejects whitespace\n"
			case "root-fenced":
				root = "```\n" + root + "```\n"
			}
			brief := fmt.Sprintf("# Fix brief %d\n\n%sConfidence: %s\nDesign required: %s\n", n, root, confidence, cmpOr(act.designRequired, designRequiredNo))
			if act.alter == "inherited" {
				break
			}
			files["fix.md"] = []byte(brief)
		default:
			files["rca.md"] = []byte(fmt.Sprintf("# RCA %d\n\nThe cause is not established.\n\nSTATUS: %s\n", n, act.marker))
			if act.alter == "new-fix" {
				files["fix.md"] = []byte("# Fix brief\n\nConfidence: PROBABLE\nDesign required: no\n")
			}
		}
		switch act.alter {
		case "triage-edits-source":
			files["main.go"] = []byte(fixtureMain)
		case "scratch-left":
			// Design 12a-light-triage-scratch: scratch left behind with
			// a file is a root directory in the snapshot.
			files[".callsheet-triage-scratch/raw/evidence.txt"] = []byte("raw evidence\n")
		case "scratch-file":
			files[".callsheet-triage-scratch"] = []byte("left behind\n")
		case "unrelated-path":
			files["notes.txt"] = []byte("notes\n")
		}
	case "coder":
		switch act.marker {
		case markerImplComplete, markerFixesComplete:
			if _, ok := files["design-questions.md"]; ok {
				files["design-questions.md"] = []byte{}
			}
			files["main.go"] = []byte(strings.Replace(fixtureMain, "package main", fmt.Sprintf("// revision %d\npackage main", n), 1))
			files["main_test.go"] = []byte(fixtureTest)
			switch act.alter {
			case "still-defective":
				files["main.go"] = base["main.go"]
			case "no-regression":
				files["main_test.go"] = base["main_test.go"]
			case "edit-feature":
				files["FEATURE.md"] = append(append([]byte(nil), base["FEATURE.md"]...), "\nmore\n"...)
			}
		case markerQuestions:
			files["design-questions.md"] = []byte("Is a tab a whitespace-only name too?\n")
		case markerNeedsDesign, markerNeedsTriage:
			files["main.go"] = []byte("package main\n\n// partial work\nfunc main() {}\n")
		}
	case "code-reviewer":
		return r.lightReview(act, doc, base, task)
	}
	return files
}

// lightReview is the scripted light code reviewer: in a pinned checkout
// of its base it runs the light verification helper once (its scripted Go
// is the semantic fake; realHelper runs the real toolchain), records the
// execution as one top-level Codex command record, and publishes only
// review.md with the helper's light report lines pasted verbatim.
func (r *devRig) lightReview(act devAct, doc devDispatchDoc, base map[string][]byte, task string) map[string][]byte {
	co := physicalTempDir(r.t)
	pinReviewCheckout(r.t, co, r.f.plane.hub, plumbing.NewHash(doc.Base), base)
	candidate := strings.Fields(goalField(doc.Goal, "candidate_commit") + " x")[0]
	initial := strings.Fields(goalField(doc.Goal, "initial_source_commit") + " x")[0]
	argv := r.d.helper.argvFor(scenarioLight, task, candidate, initial)
	switch act.alter {
	case "full-receipt":
		// The full recipe's receipt for a light review: never a substitute.
		argv = r.d.helper.argv(task, candidate, initial)
	case "cross-task":
		argv = r.d.helper.argvFor(scenarioLight, hexID("t_", 99), candidate, initial)
	}
	run := reviewRun{task: task, exits: map[string]int{}}
	var out string
	ran := act.alter != "prose-only" && act.alter != "no-helper"
	if ran {
		env := reviewEnv{cwd: co, clock: r.f.clock, launcher: &fakeLauncher{script: r.reviewGo.script}, path: "/usr/bin:/bin"}
		if r.realHelper != "" {
			env = reviewEnv{cwd: co, clock: newOffsetClock(r.f.clock), launcher: ExecLauncher{}, path: r.realHelper}
		}
		var code int
		out, _, code = verifyHelperRun(env, argv)
		if c, ok := env.clock.(*offsetClock); ok {
			c.settle()
		}
		var rec struct {
			ScratchRemoved bool `json:"scratch_removed"`
		}
		if err := json.Unmarshal([]byte(receiptLine(out)), &rec); err != nil {
			r.t.Fatalf("the light helper printed no receipt: %v\n%s", err, out)
		}
		r.helperOut[task] = out
		run.scratchOK = rec.ScratchRemoved
		r.reviewLogs[task] = codexLog("/bin/bash -lc '"+strings.Join(argv, " ")+"'", out, code)
	}
	var rep strings.Builder
	rep.WriteString("# Light review\n\nThe root cause is the trimmed empty-name check; the regression asserts the whitespace case.\n\n")
	if _, trailer, _ := strings.Cut(out, "\n"); ran && trailer != "" {
		for _, l := range strings.SplitAfter(trailer, "\n") {
			switch {
			case act.alter == "missing-field" && strings.HasPrefix(l, "Coverage comparison: "):
				continue
			case act.alter == "bad-hash" && strings.HasPrefix(l, reviewRunField+" "):
				l = reviewRunField + " " + strings.Repeat("0", 64) + "\n"
			}
			rep.WriteString(l)
		}
	} else {
		rep.WriteString("Scenario: light\nReviewed candidate: " + candidate + "\n")
		rep.WriteString("The verification helper could not start: no execution evidence exists.\n")
	}
	fmt.Fprintf(&rep, "\nCALLSHEET_REVIEW_BLOCKER: %s\nSTATUS: %s\n", cmpOr(act.blocker, blockerNone), act.marker)
	r.lastReport = []byte(rep.String())
	if err := os.WriteFile(filepath.Join(co, "review.md"), r.lastReport, 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.reviews = append(r.reviews, run)
	return r.reviewPublication(act, co)
}
