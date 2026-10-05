//go:build containeracceptance

// Package container is the M3/M4 container acceptance (design
// m3-m4-container-e2e): eleven tagged function tests that run only inside
// the isolated Linux container cmd/devcheck's container-e2e operation
// builds, never in the ordinary go test ./... package set. Each wrapper
// asks the shared stub coordinator (internal/testkit/containeracceptance)
// for its case, writes exactly one evidence record per required case ID,
// and fails on any error; TestMain writes the iteration's end record only
// after the suite's cleanup. A failed prerequisite fails its dependents,
// never skips them.
package container

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/devcheck"
	ca "github.com/wedevwork/callsheet/internal/testkit/containeracceptance"
)

// setupBound bounds the fixture's start (within the 6-minute test limit).
const setupBound = 2 * time.Minute

var (
	cfg      ca.Config
	suite    *ca.Suite
	setupErr error
	outMu    sync.Mutex
	runCtx   = context.Background()
)

func TestMain(m *testing.M) {
	cfg, setupErr = ca.ConfigFromEnv(os.Getenv)
	if setupErr == nil {
		if err := os.MkdirAll(os.Getenv("HOME"), 0o700); err != nil {
			setupErr = err
		}
	}
	if setupErr == nil {
		ctx, cancel := context.WithTimeout(runCtx, setupBound)
		suite, setupErr = ca.New(ctx, cfg)
		cancel()
	}
	code := m.Run()
	if suite != nil {
		cleanupErr := suite.Close(runCtx)
		// The end record's outcome is the overall result: a failed test
		// (m.Run's nonzero code) or case fails it even after a successful
		// cleanup; cleanup_ok reports cleanup alone.
		var testsErr error
		if code != 0 {
			testsErr = fmt.Errorf("the acceptance tests exited %d", code)
		}
		end := suite.EndRecord(testsErr, cleanupErr)
		if err := emit(ca.Record{End: &end}); err != nil || cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "container acceptance cleanup: %v %v\n", cleanupErr, err)
			code = 1
		}
	} else if code == 0 {
		code = 1
	}
	os.Exit(code)
}

// emit writes one evidence line to stdout atomically.
func emit(r ca.Record) error {
	outMu.Lock()
	defer outMu.Unlock()
	return ca.WriteRecord(os.Stdout, r)
}

// fixture returns the shared suite or fails (never skips) on a setup error.
func fixture(t *testing.T) *ca.Suite {
	t.Helper()
	if setupErr != nil {
		t.Fatalf("container acceptance setup: %v", setupErr)
	}
	return suite
}

// runCase runs id, emits its record (passing or failed) and fails on error.
func runCase(t *testing.T, id string, extra error) ca.CaseRecord {
	t.Helper()
	rec, err := fixture(t).RunCase(runCtx, id)
	if extra != nil && rec.CaseID != "" {
		rec.Outcome, rec.Error = ca.OutcomeFail, extra.Error()
	}
	if rec.CaseID != "" {
		if werr := emit(ca.Record{Case: &rec}); werr != nil {
			t.Fatal(werr)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if extra != nil {
		t.Fatal(extra)
	}
	return rec
}

// runPhase runs a nested phase that is recorded in its parent's record.
func runPhase(id string) func(*testing.T) {
	return func(t *testing.T) {
		if _, err := fixture(t).RunCase(runCtx, id); err != nil {
			t.Fatal(err)
		}
	}
}

// containerEnv checks what only the container itself can observe: Linux on
// the host-built architecture, the numeric non-root user, the read-only
// root with only /tmp writable, the fixed image paths, no network
// interface beyond loopback and the explicit HOME and TMPDIR.
func containerEnv() error {
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if runtime.GOOS != "linux" || runtime.GOARCH != cfg.Architecture {
		bad("platform %s/%s, want linux/%s", runtime.GOOS, runtime.GOARCH, cfg.Architecture)
	}
	if os.Getuid() != 65532 || os.Getgid() != 65532 {
		bad("uid/gid %d/%d, want 65532/65532", os.Getuid(), os.Getgid())
	}
	if err := os.WriteFile("/root-probe", nil, 0o600); err == nil {
		os.Remove("/root-probe")
		bad("the root filesystem is writable")
	}
	if os.Getenv("HOME") != ca.ImageWorkRoot+"/home" || os.Getenv("TMPDIR") != "/tmp" {
		bad("HOME %q TMPDIR %q", os.Getenv("HOME"), os.Getenv("TMPDIR"))
	}
	for _, p := range []string{ca.ImageCallsheet, ca.ImageFakeAdapter, ca.ImageTest, filepath.Join(ca.ImageFixtures, "manuals"), filepath.Join(ca.ImageFixtures, "docs")} {
		if _, err := os.Stat(p); err != nil {
			bad("image path %s: %v", p, err)
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		bad("interfaces: %v", err)
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback == 0 && i.Flags&net.FlagUp != 0 {
			bad("network interface %s is up beyond loopback", i.Name)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("container environment: %s", strings.Join(problems, "; "))
	}
	return nil
}

// FP-1: the isolated runtime, real subprocesses and loopback TLS.
func TestContainerRuntime(t *testing.T) {
	runCase(t, ca.CaseRuntime, containerEnv())
}

// FP-2: four roles prepared and the two-worker goal-and-answer exchange.
func TestContainerCoordinator(t *testing.T) {
	t.Run("prepare", runPhase(ca.PhasePrepare))
	t.Run("goal_answer", runPhase(ca.PhaseGoalAnswer))
	runCase(t, ca.CaseCoordinator, nil)
}

// FP-11: the ordered design, design-review, code and code-review flow.
func TestContainerSampleFlow(t *testing.T) {
	runCase(t, ca.CaseSampleFlow, nil)
}

// FP-3: push, publish and inspect a real change.
func TestContainerPublication(t *testing.T) {
	runCase(t, ca.CasePublication, nil)
}

// FP-4: the sibling on the original base while B is held, then B on A's
// exact result.
func TestContainerContinuation(t *testing.T) {
	t.Run("sibling", runPhase(ca.PhaseSibling))
	t.Run("continuation", runPhase(ca.PhaseContinuation))
	runCase(t, ca.CaseContinuation, nil)
}

// FP-5: failed, cancelled and timed-out partial results.
func TestContainerPartialResults(t *testing.T) {
	for _, sub := range []string{"failed", "cancelled", "timed_out"} {
		t.Run(sub, func(t *testing.T) { runCase(t, ca.CasePartialResults+"/"+sub, nil) })
	}
	runCase(t, ca.CasePartialResults, nil)
}

// FP-6: a lost worker publishes nothing.
func TestContainerLost(t *testing.T) {
	runCase(t, ca.CaseLost, nil)
}

// FP-7: one stable receipt across a sidecar restart after publication.
func TestContainerPublicationRestart(t *testing.T) {
	runCase(t, ca.CasePublicationRestart, nil)
}

// FP-8: a pull into a dirty checkout preserves it.
func TestContainerDirtyPull(t *testing.T) {
	runCase(t, ca.CaseDirtyPull, nil)
}

// FP-9: the bundled docs make this run the demonstration with bounded
// claims.
func TestContainerClaims(t *testing.T) {
	runCase(t, ca.CaseClaims, nil)
}

// FP-10: the evidence schema and catalog gate, on synthetic ledgers with
// this iteration's metadata (never on its own yet-unwritten records): the
// writer's complete stream passes the host validator, and each defect is
// rejected. The end record follows in TestMain after cleanup.
func TestContainerEvidence(t *testing.T) {
	runCase(t, ca.CaseEvidence, evidenceSelfTest())
}

func evidenceSelfTest() error {
	if setupErr != nil {
		return setupErr
	}
	if got, want := strings.Join(ca.RequiredCaseIDs(), ","), strings.Join(devcheck.ContainerCaseIDs(), ","); got != want {
		return fmt.Errorf("manifest %s differs from the host's %s", got, want)
	}
	meta := devcheck.ContainerMetadata{RunID: cfg.RunID, SourceRevision: cfg.SourceRevision, Architecture: cfg.Architecture,
		Iteration: cfg.Iteration, Total: cfg.Total, BinaryHashes: cfg.ExpectedHashes}
	exp := devcheck.ContainerExpectation{Metadata: meta, CaseIDs: devcheck.ContainerCaseIDs(), Subcases: devcheck.ContainerSubcases()}
	common := ca.Common{RunID: cfg.RunID, Iteration: cfg.Iteration, Total: cfg.Total, SourceRevision: cfg.SourceRevision, Arch: cfg.Architecture,
		BinaryHashes: cfg.ExpectedHashes}
	valid := ca.SyntheticStream(common)
	if err := devcheck.ValidateContainerEvidence(bytes.NewReader(valid), exp); err != nil {
		return fmt.Errorf("the complete synthetic stream was rejected: %v", err)
	}
	lines := strings.SplitAfter(string(valid), "\n")
	drop := func(match string) string {
		var b strings.Builder
		for _, l := range lines {
			if !strings.Contains(l, match) {
				b.WriteString(l)
			}
		}
		return b.String()
	}
	var publication string
	for _, l := range lines {
		if strings.HasPrefix(l, ca.EvidencePrefix) && strings.Contains(l, `"case_id":"`+ca.CasePublication+`"`) {
			publication = l
		}
	}
	mutants := map[string]string{
		"missing case":     drop(`"case_id":"` + ca.CaseLost + `"`),
		"missing end":      drop(`"kind":"end"`),
		"skipped subtest":  strings.Replace(string(valid), "--- PASS: TestContainerPartialResults/timed_out", "--- SKIP: TestContainerPartialResults/timed_out", 1),
		"failed outcome":   strings.Replace(string(valid), `"outcome":"pass"`, `"outcome":"fail"`, 1),
		"cleanup failed":   strings.Replace(string(valid), `"cleanup_ok":true`, `"cleanup_ok":false`, 1),
		"truncated record": strings.TrimSuffix(string(valid), "\n")[:len(valid)-20],
		"wrong node":       strings.Replace(string(valid), `"node_id":"`+ca.SyntheticNodeB, `"node_id":"`+ca.SyntheticNodeA, 1),
		"duplicate case":   strings.Replace(string(valid), publication, publication+publication, 1),
		"other run":        strings.ReplaceAll(string(valid), `"run_id":"`+cfg.RunID+`"`, `"run_id":"other-run"`),
	}
	if publication == "" {
		return fmt.Errorf("the synthetic stream has no %s record", ca.CasePublication)
	}
	for name, m := range mutants {
		if err := devcheck.ValidateContainerEvidence(strings.NewReader(m), exp); err == nil {
			return fmt.Errorf("the %s mutant was accepted", name)
		}
	}
	return nil
}
