package function

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/mcpqual"
)

// qualFailureReportLimit is how much of report.json a failure message
// inlines. A zero-case report is far smaller. A report that still holds
// case bodies can approach MaxEvidenceFileBytes and must not be pasted
// into the log; the outcome, client and phase lines already name the path.
const qualFailureReportLimit = 256 << 10

// qualFailureFacts is the text of a qualify case-count failure. Call it
// only from t.Fatalf: a passing run does not format or print it.
//
// len(cases) is the count the assertion used. Each client line is id,
// outcome and reason; each phase line is name, status, reason and case
// count. Together those name the three zero-case paths: runClient notRun
// (client unqualified, every phase not_run with that reason), a first
// session skipped by canRun (setup not_run with the skip reason, later
// phases "setup not conclusive"), and boundReport (phase reason
// report_bound, cases dropped). The output directory is included, and a
// small report.json is inlined so the CI log still has it after the temp
// directory is removed.
func qualFailureFacts(rep *mcpqual.Report, cases []mcpqual.CaseReport, out string) string {
	var b strings.Builder
	if rep == nil {
		fmt.Fprintf(&b, "outcome: <nil report>\ncases: %d\n", len(cases))
	} else {
		fmt.Fprintf(&b, "outcome: %s\ncases: %d\n", rep.Outcome, len(cases))
		if len(rep.Clients) == 0 {
			b.WriteString("clients: 0\n")
		}
		for _, c := range rep.Clients {
			fmt.Fprintf(&b, "client %s outcome=%s reason=%s\n", c.ID, c.Outcome, deref(c.Reason))
			if len(c.Phases) == 0 {
				b.WriteString("  phases: 0\n")
			}
			for _, ph := range c.Phases {
				fmt.Fprintf(&b, "  phase %s status=%s reason=%s cases=%d\n", ph.Name, ph.Status, deref(ph.Reason), len(ph.Cases))
			}
		}
	}
	fmt.Fprintf(&b, "out: %s\n", out)
	reasons := caseReasons(cases)
	if reasons == "" {
		reasons = "(none)"
	}
	fmt.Fprintf(&b, "case reasons: %s\n", reasons)
	b.WriteString(qualFailureReport(out))
	return b.String()
}

// qualFailureReport is the output directory's report.json, inlined when
// it is present and within qualFailureReportLimit.
func qualFailureReport(out string) string {
	p := filepath.Join(out, "report.json")
	st, err := os.Stat(p)
	if err != nil {
		return "report.json: " + err.Error()
	}
	if st.Size() > qualFailureReportLimit {
		return fmt.Sprintf("report.json: %d bytes at %s (not inlined; limit %d)", st.Size(), p, qualFailureReportLimit)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		return "report.json: " + err.Error()
	}
	return "report.json:\n" + string(body)
}

func TestQualFailureFactsZeroCase(t *testing.T) {
	absent := mcpqual.ReasonAbsentBinary
	skip := mcpqual.ReasonBudget + ": max_client_ms"
	later := "setup not conclusive"
	bound := mcpqual.ReasonReportBound + ": the phase's detail exceeded the report budget"
	out := filepath.Join("qual-out", "run")

	notRun := &mcpqual.Report{
		Outcome: "partial",
		Clients: []mcpqual.ClientReport{{
			ID: "cursor", Outcome: "unqualified", Reason: &absent,
			Phases: []mcpqual.PhaseReport{
				{Name: mcpqual.PhaseSetup, Status: mcpqual.StatusNotRun, Reason: &absent},
				{Name: mcpqual.PhaseDefault, Status: mcpqual.StatusNotRun, Reason: &absent},
			},
		}},
	}
	skipped := &mcpqual.Report{
		Outcome: "partial",
		Clients: []mcpqual.ClientReport{{
			ID: "cursor", Outcome: "unqualified", Reason: &skip,
			Phases: []mcpqual.PhaseReport{
				{Name: mcpqual.PhaseSetup, Status: mcpqual.StatusNotRun, Reason: &skip},
				{Name: mcpqual.PhaseDefault, Status: mcpqual.StatusNotRun, Reason: &later},
			},
		}},
	}
	clientBound := mcpqual.PhaseSetup + ": " + mcpqual.ReasonReportBound
	dropped := &mcpqual.Report{
		Outcome: "partial",
		Clients: []mcpqual.ClientReport{{
			ID: "cursor", Outcome: "partial", Reason: &clientBound,
			Phases: []mcpqual.PhaseReport{
				{Name: mcpqual.PhaseSetup, Status: mcpqual.StatusInconclusive, Reason: &bound},
			},
		}},
	}

	for name, tc := range map[string]struct {
		rep  *mcpqual.Report
		want []string
	}{
		"notRun": {notRun, []string{
			"outcome: partial",
			"cases: 0",
			"out: " + out,
			"client cursor outcome=unqualified reason=" + absent,
			"phase setup status=not_run reason=" + absent + " cases=0",
			"phase default status=not_run reason=" + absent + " cases=0",
			"case reasons: (none)",
		}},
		"canRun": {skipped, []string{
			"outcome: partial",
			"cases: 0",
			"client cursor outcome=unqualified reason=" + skip,
			"phase setup status=not_run reason=" + skip + " cases=0",
			"phase default status=not_run reason=" + later + " cases=0",
		}},
		"report_bound": {dropped, []string{
			"outcome: partial",
			"cases: 0",
			"client cursor outcome=partial reason=" + clientBound,
			"phase setup status=inconclusive reason=" + bound + " cases=0",
		}},
	} {
		msg := qualFailureFacts(tc.rep, nil, out)
		for _, want := range tc.want {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: failure text lacks %q\n%s", name, want, msg)
			}
		}
		if strings.Contains(msg, "case reasons: ") && !strings.Contains(msg, "case reasons: (none)") {
			t.Errorf("%s: zero-case text invented a case reason\n%s", name, msg)
		}
	}

	// A missing report.json is named, not omitted, so the log still shows
	// that the directory was recorded.
	missing := qualFailureFacts(notRun, nil, out)
	if !strings.Contains(missing, "report.json:") || !strings.Contains(missing, filepath.Join(out, "report.json")) {
		t.Fatalf("missing report.json was not named:\n%s", missing)
	}

	dir := t.TempDir()
	body := []byte("{\"outcome\":\"partial\",\"note\":\"zero-case\"}\n")
	if err := os.WriteFile(filepath.Join(dir, "report.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	inlined := qualFailureFacts(notRun, nil, dir)
	if !strings.Contains(inlined, "report.json:\n"+string(body)) {
		t.Fatalf("small report.json was not inlined:\n%s", inlined)
	}

	over := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(over, make([]byte, qualFailureReportLimit+1), 0o600); err != nil {
		t.Fatal(err)
	}
	held := qualFailureFacts(notRun, nil, filepath.Dir(over))
	if !strings.Contains(held, "not inlined") || !strings.Contains(held, fmt.Sprintf("%d bytes", qualFailureReportLimit+1)) {
		t.Fatalf("over-limit report.json was inlined:\n%s", held[:min(len(held), 400)])
	}

	// The printed count is the slice the assertion checked, and a non-empty
	// slice still keeps the case id, outcome and reason.
	caseReason := "hashed candidate exists"
	withCases := qualFailureFacts(notRun, []mcpqual.CaseReport{{
		CaseID: "cursor-setup", Outcome: mcpqual.OutcomeInconclusive, Reason: &caseReason,
	}}, out)
	if !strings.Contains(withCases, "cases: 1") || !strings.Contains(withCases, "cursor-setup "+mcpqual.OutcomeInconclusive+" "+caseReason) {
		t.Fatalf("case detail dropped:\n%s", withCases)
	}
}
