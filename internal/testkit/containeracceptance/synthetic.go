package containeracceptance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Synthetic evidence (never an observation): a complete, valid iteration
// stream written with WriteRecord, for the gate's offline tests, the
// in-container evidence self-test and the parser benchmark. Its task
// metadata conforms to the static catalog with clearly synthetic IDs and
// hashes; it carries no file contents.

// SyntheticNodeA and SyntheticNodeB are the synthetic node bindings.
const (
	SyntheticNodeA = "n_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	SyntheticNodeB = "n_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// synthHex derives a deterministic lowercase hex string of n digits.
func synthHex(seed string, n int) string {
	h := sha256.Sum256([]byte("synthetic:" + seed))
	return hex.EncodeToString(h[:])[:n]
}

func sp(s string) *string { return &s }
func ip(n int) *int       { return &n }

// synthTask builds one catalog-conforming synthetic task.
func synthTask(caseID, label, role, alias, profile, inst, base, result string) Task {
	node := SyntheticNodeA
	if alias == "b" {
		node = SyntheticNodeB
	}
	id := "t_" + synthHex(caseID+"/"+label, 32)
	t := Task{Label: label, RoleID: role, NodeID: node, TaskID: id}
	switch profile {
	case "answer":
		t.TerminalState, t.ExitCode, t.FinalMessage = "succeeded", ip(0), sp(FinalMessage)
		return t
	case "published":
		t.TerminalState, t.ExitCode, t.FinalMessage = "succeeded", ip(0), sp(FinalMessage)
	case "failed":
		t.TerminalState, t.ExitCode = "failed", ip(3)
	case "lost":
		t.TerminalState, t.PublicationStatus = "lost", sp("not_applicable")
		t.WorkspaceInstance, t.BaseCommit = sp(inst), sp(base)
		return t
	default:
		t.TerminalState = profile
	}
	t.PublicationStatus = sp("published")
	t.WorkspaceInstance, t.BaseCommit, t.ResultCommit, t.ResultRef = sp(inst), sp(base), sp(result), sp("refs/callsheet/tasks/"+id)
	return t
}

// SyntheticRecords returns the 14 case records (test order) and the end
// record of one passing iteration with common c (its Outcome is set).
func SyntheticRecords(c Common) ([]CaseRecord, EndRecord) {
	c.Outcome = OutcomePass
	m4, sample := synthHex("m4-instance", 32), synthHex("sample-instance", 32)
	base, rA := synthHex("m4-base", 40), synthHex("m4-A", 40)
	mk := func(id string, tasks ...Task) CaseRecord {
		if tasks == nil {
			tasks = []Task{}
		}
		return CaseRecord{Common: c, CaseID: id, Subcases: map[string]string{}, Tasks: tasks, Boundary: boundaries[id]}
	}
	pub := func(caseID, label, role, alias, b, r string) Task {
		return synthTask(caseID, label, role, alias, "published", m4, b, r)
	}
	runtimeRec := mk(CaseRuntime)
	runtimeRec.NodeBindings = map[string]string{"a": SyntheticNodeA, "b": SyntheticNodeB}
	coord := mk(CaseCoordinator, synthTask(CaseCoordinator, "answer-a", "proof-a", "a", "answer", "", "", ""),
		synthTask(CaseCoordinator, "answer-b", "proof-b", "b", "answer", "", "", ""))
	coord.Subcases = map[string]string{"prepare": OutcomePass, "goal_answer": OutcomePass}
	var flow []Task
	prev := synthHex("sample-seed", 40)
	for _, st := range sampleSteps {
		next := synthHex("sample-"+st.label, 40)
		flow = append(flow, synthTask(CaseSampleFlow, st.label, st.role, st.alias, "published", sample, prev, next))
		prev = next
	}
	cont := mk(CaseContinuation, pub(CaseContinuation, "A", "proof-a", "a", base, rA), pub(CaseContinuation, "B", "proof-b", "b", rA, synthHex("m4-B", 40)),
		pub(CaseContinuation, "sibling", "proof-a", "a", base, synthHex("m4-sibling", 40)))
	cont.Subcases = map[string]string{"continuation": OutcomePass, "sibling": OutcomePass}
	partial := mk(CasePartialResults)
	partial.Subcases = map[string]string{"failed": OutcomePass, "cancelled": OutcomePass, "timed_out": OutcomePass}
	recs := []CaseRecord{
		runtimeRec,
		coord,
		mk(CaseSampleFlow, flow...),
		mk(CasePublication, pub(CasePublication, "A", "proof-a", "a", base, rA)),
		cont,
		mk(CasePartialFailed, synthTask(CasePartialFailed, "failed", "proof-a", "a", "failed", m4, base, synthHex("failed", 40))),
		mk(CasePartialCancelled, synthTask(CasePartialCancelled, "cancelled", "proof-a", "a", "cancelled", m4, base, synthHex("cancelled", 40))),
		mk(CasePartialTimedOut, synthTask(CasePartialTimedOut, "timed_out", "proof-a", "a", "timed_out", m4, base, synthHex("timed_out", 40))),
		partial,
		mk(CaseLost, synthTask(CaseLost, "lost", "proof-a", "a", "lost", m4, base, ""), pub(CaseLost, "recovered", "proof-a", "a", base, synthHex("recovered", 40))),
		mk(CasePublicationRestart, pub(CasePublicationRestart, "restart", "proof-a", "a", base, synthHex("restart", 40)),
			pub(CasePublicationRestart, "fresh", "proof-a", "a", base, synthHex("fresh", 40))),
		mk(CaseDirtyPull, pub(CaseDirtyPull, "B", "proof-b", "b", rA, synthHex("m4-B", 40))),
		mk(CaseClaims),
		mk(CaseEvidence),
	}
	return recs, EndRecord{Common: c, CaseIDs: RequiredCaseIDs(), CleanupOK: true}
}

// SyntheticTests are the verbose test names of one passing iteration, in
// run order: each parent, then its mandatory subtests.
var SyntheticTests = []string{
	CaseRuntime, CaseCoordinator, PhasePrepare, PhaseGoalAnswer, CaseSampleFlow, CasePublication,
	CaseContinuation, PhaseSibling, PhaseContinuation, CasePartialResults, CasePartialFailed, CasePartialCancelled,
	CasePartialTimedOut, CaseLost, CasePublicationRestart, CaseDirtyPull, CaseClaims, CaseEvidence,
}

// SyntheticStream renders one passing iteration's output: for every test
// its run event, any records, its pass event (subtests indented), the
// end record after the final PASS line.
func SyntheticStream(c Common) []byte {
	recs, end := SyntheticRecords(c)
	byID := map[string]CaseRecord{}
	for _, r := range recs {
		byID[r.CaseID] = r
	}
	var b bytes.Buffer
	for _, name := range SyntheticTests {
		b.WriteString("=== RUN   " + name + "\n")
	}
	for _, name := range SyntheticTests {
		if r, ok := byID[name]; ok {
			rr := r
			WriteRecord(&b, Record{Case: &rr})
		}
		indent := strings.Repeat("    ", strings.Count(name, "/"))
		b.WriteString(indent + "--- PASS: " + name + " (0.01s)\n")
	}
	b.WriteString("PASS\n")
	WriteRecord(&b, Record{End: &end})
	return b.Bytes()
}
