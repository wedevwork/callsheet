package devcheck

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// budgetChild is one planned test-stage child: its step name, its exact
// argv and its scratch log file, all independent literals.
type budgetChild struct{ name, argv, log string }

var (
	budgetOrdinary = []budgetChild{
		{"test", wantTestOrdinaryShared, "test.log"},
		{"test reale2e", wantTestOrdinaryReale2e, "test-reale2e.log"},
		{"test function", wantTestOrdinaryFunction, "test-function.log"},
	}
	budgetRace = []budgetChild{
		{"test -race", wantTestRace, "test--race.log"},
		{"test -race function", wantTestRaceFunction, "test--race-function.log"},
	}
	budgetRaceReale2e = budgetChild{"test -race reale2e", wantTestRaceReale2e, "test--race-reale2e.log"}
	budgetTagged      = budgetChild{"test realadaptercheck", wantTestTagged, "test-realadaptercheck.log"}
	budgetTaggedRace  = budgetChild{"test realadaptercheck -race", wantTestTaggedRace, "test-realadaptercheck--race.log"}
)

// budgetPlan is the exact expected test plan of goos in CI mode (ci) or
// local mode (design 12a-linux-test-budget): Linux local 8 children, Linux
// CI 7, Darwin 4 in both modes.
func budgetPlan(goos string, ci bool) []budgetChild {
	plan := append([]budgetChild(nil), budgetOrdinary...)
	if goos == "darwin" {
		return append(plan, budgetTagged)
	}
	plan = append(plan, budgetRace...)
	if !ci {
		plan = append(plan, budgetRaceReale2e)
	}
	return append(plan, budgetTagged, budgetTaggedRace)
}

// runBudget runs a stage through runMode in the given mode with a fresh
// evidence directory and returns the code, streams and the evidence
// directory; retained scratch is removed at cleanup.
func runBudget(t *testing.T, goos string, ci bool, r runnerLike, args ...string) (int, string, string, string) {
	t.Helper()
	opts := testOpts(t)
	var out, errOut bytes.Buffer
	code := runMode(context.Background(), goos, ci, args, &out, &errOut, r.run, opts)
	t.Cleanup(func() { os.RemoveAll(scratchFrom(out.String())) })
	return code, out.String(), errOut.String(), opts.EvidenceDir
}

// TestTestBudgetDriverContract is the FP-3 driver contract of design
// 12a-linux-test-budget, delegated by exact name from tests/function
// (TestCITestBudgetExecution). For Linux and Darwin, local and CI mode,
// through test and all, it drives the revised plan with a fake Runner
// (and, on Linux, the synthetic container fixture): success runs every
// child once in the exact order, then, on Linux, exactly one container
// acceptance before any later stage; a failure of any child stops every
// later child, the tagged checks, the container setup and the later
// stages, names the failed step, retains its scratch log and, on Linux,
// records the failure phase in the evidence report. It never runs a real
// child. Do not rename or skip its subtests.
func TestTestBudgetDriverContract(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, ci := range []bool{false, true} {
			mode, modeLine := "local", TestPlanLocalLine
			if ci {
				mode, modeLine = "ci", TestPlanCILine
			}
			for _, stage := range []string{"test", "all"} {
				t.Run(goos+"-"+mode+"-"+stage, func(t *testing.T) {
					plan := budgetPlan(goos, ci)
					if want := map[string]int{"linux-local": 8, "linux-ci": 7, "darwin-local": 4, "darwin-ci": 4}[goos+"-"+mode]; len(plan) != want ||
						len(testSteps(goos, ci)) != want {
						t.Fatalf("plan length %d, want %d", len(testSteps(goos, ci)), want)
					}
					var argvs []string
					for _, c := range plan {
						argvs = append(argvs, c.argv)
					}
					fixture := func(fail string) (*fakeRunner, *containerRunner, runnerLike) {
						f := &fakeRunner{fail: fail, coverTotal: "90%", cmdList: cmdList, profile: goodProfile}
						if goos != "linux" {
							return f, nil, f
						}
						cr := newContainerRunnerFixture(t, f).(*containerRunner)
						return f, cr, cr
					}
					// Success: every child once, in order, then the container
					// acceptance once (Linux), then the later stages (all).
					f, cr, r := fixture("")
					code, out, errOut, dir := runBudget(t, goos, ci, r, stage)
					if code != 0 {
						t.Fatalf("success = %d %s", code, errOut)
					}
					calls := f.argvs()
					wantCalls := len(plan)
					if stage == "all" {
						// Coverage (3), bench (14) and cross (12) follow.
						wantCalls += 3 + 14 + 12
					}
					if len(calls) != wantCalls || strings.Join(calls[:len(plan)], "|") != strings.Join(argvs, "|") {
						t.Fatalf("success calls = %d %v", len(calls), calls)
					}
					if stage == "all" && !strings.HasPrefix(calls[len(plan)], "go test -count=1 -tags=realadaptercheck -covermode=atomic") {
						t.Fatalf("the stage after test is %q", calls[len(plan)])
					}
					if strings.Count(out, modeLine+"\n") != 1 {
						t.Fatalf("mode line in:\n%s", out)
					}
					pos := 0
					for _, c := range plan {
						at := strings.Index(out, "devcheck: "+c.name+": "+c.argv+"\n")
						if at < pos {
							t.Fatalf("child %q out of order in:\n%s", c.name, out)
						}
						pos = at + 1
					}
					if goos == "linux" {
						if got := strings.Join(cr.containerKinds(), "|"); got != strings.Join(oneIterationKinds, "|") {
							t.Fatalf("container calls = %s", got)
						}
						if !cr.ordered(plan[len(plan)-1].argv, "docker context inspect") {
							t.Fatal("the container acceptance started before the last plan child")
						}
						if stage == "all" && !cr.ordered("docker image ls", "-coverprofile=") {
							t.Fatal("the coverage stage started before the container acceptance ended")
						}
						if report := readEvidence(t, dir, evidenceReportFile); !strings.Contains(report, "outcome: pass\n") {
							t.Fatalf("success report:\n%s", report)
						}
					} else if _, err := os.Stat(dir); !os.IsNotExist(err) {
						t.Fatal("darwin created the evidence directory")
					}
					// A failure at every child stops everything after it.
					for i, c := range plan {
						f, cr, r := fixture(c.argv)
						code, out, errOut, dir := runBudget(t, goos, ci, r, stage)
						if code != 1 || strings.Join(f.argvs(), "|") != strings.Join(argvs[:i+1], "|") {
							t.Fatalf("%s failure = %d calls %v", c.name, code, f.argvs())
						}
						if !strings.Contains(errOut, "devcheck: stage test FAILED: "+c.name+" failed: "+c.argv) || !strings.Contains(errOut, "boom from child") ||
							!strings.Contains(errOut, "devcheck: logs retained in ") || strings.Contains(out, "devcheck: stage test ok") {
							t.Fatalf("%s failure output:\n%s\n%s", c.name, out, errOut)
						}
						if _, err := os.Stat(filepath.Join(scratchFrom(out), c.log)); err != nil {
							t.Fatalf("%s log: %v", c.name, err)
						}
						if goos == "linux" {
							if len(cr.calls) != 0 {
								t.Fatalf("%s failure set up the container: %v", c.name, cr.argvs())
							}
							report := readEvidence(t, dir, evidenceReportFile)
							if !strings.Contains(report, "container: not started\n") || !strings.Contains(report, "  - failed before container setup: "+c.name+"\n") ||
								!strings.Contains(report, "outcome: fail\n") {
								t.Fatalf("%s failure report:\n%s", c.name, report)
							}
						} else if _, err := os.Stat(dir); !os.IsNotExist(err) {
							t.Fatalf("%s failure created the evidence directory", c.name)
						}
					}
				})
			}
		}
	}
}
