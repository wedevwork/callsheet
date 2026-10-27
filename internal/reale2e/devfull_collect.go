package reale2e

import (
	"errors"
	"fmt"

	"path/filepath"

	git "github.com/go-git/go-git/v5"
)

// Collector producers (design 12a-dev-flow-verify, Collector changes).
// They record what the checker cannot infer from counts alone: every
// ListTasks sweep with its page boundaries and captured request identity
// (coalescing only unchanged complete sweeps), the session event map,
// the derived accounting, the executable fingerprints and the
// inventory-bearing manifest, and finally the canonical report and audit
// produced by the same checker the offline check runs.

// collectDevSession derives session/event-map.json from the final phase
// observations of the loaded bundle b and writes it with put.
func (d *devSupervisor) collectDevSession(b *DevBundle, put func(string, any) error) error {
	em := deriveDevEventMap(sessionMapInput(b, d.s.runID, d.s.goos, d.s.w.Executable, d.s.runtime))
	b.derived = &derivedEventMap{goos: d.s.goos, reale2e: d.s.w.Executable, runtime: d.s.runtime, em: em}
	return put(devEventMapFile, em)
}

// hashExecutable records the harness executable's SHA-256 (the reviewer
// helper is the same executable).
func (d *devSupervisor) hashExecutable() error {
	b, err := readBounded(d.s.w.Executable, 1<<30)
	if err != nil {
		return failure("executable_unreadable", err)
	}
	d.manifest.Executable = DevExecutableRecord{Path: d.s.w.Executable, Before: sha256Hex(b)}
	return nil
}

// checkoutRevision is the checkout's HEAD commit ("" when unreadable).
func checkoutRevision(checkout string) string {
	if r, err := git.PlainOpen(checkout); err == nil {
		if h, err := r.Head(); err == nil {
			return h.Hash().String()
		}
	}
	return ""
}

// writeDevManifest completes the manifest after primary collection: the
// final executable hash, the stop, the final candidate and review, and
// the inventory of every written non-derived file, read back from disk
// (the judgment then compares it with the loaded bundle's own inventory).
// put writes it.
func (d *devSupervisor) writeDevManifest(put func(string, any) error) error {
	m := &d.manifest
	m.ProtectedRoots, m.ProtectDepth = append([]string{}, d.s.manifest.ProtectedRoots...), d.s.manifest.ProtectDepth
	m.Workspace = d.s.workspace
	m.CollectedAt = formatTime(d.s.w.Clock.Now())
	if b, err := readBounded(d.s.w.Executable, 1<<30); err == nil {
		m.Executable.After = sha256Hex(b)
	}
	// A Cursor attempt's executable binding (before any vendor execution,
	// after cleanup); a default manifest never carries the field.
	m.Cursor = d.cursorManifestBinding()
	if d.stopCode != "" {
		code, seq := d.stopCode, d.stopSeq
		m.StopCode, m.StopSeq = &code, &seq
	}
	if d.complete {
		m.FinalCandidateTask, m.FinalCandidate = d.state.CandidateTask, d.state.CandidateCommit
		m.FinalReviewTask, m.FinalReview = d.state.ReviewTask, d.state.ReviewCommit
	}
	inv, err := diskInventory(d.s.ev.root)
	if err != nil {
		return err
	}
	m.Inventory = inv
	return put(devManifestFile, m)
}

// judgeDev judges the collected bundle b (loaded once by the collector,
// with the records it then wrote ingested from their written bytes): it
// runs the initial-generation form of the checker and writes the
// canonical report, then the audit binding the report's SHA-256. It
// returns whether the checked result is PASS and every derived file was
// written.
func (d *devSupervisor) judgeDev(b *DevBundle) (bool, error) {
	res := checkDevBundle(b, true)
	writes := []struct {
		name string
		data []byte
	}{{"report.json", res.reportJSON}, {"report.txt", []byte(res.reportText)}, {"audit.json", res.auditJSON}, {"audit.md", []byte(res.auditText)}}
	var errs []error
	for _, w := range writes {
		if err := d.s.ev.writeFile(w.name, w.data); err != nil {
			errs = append(errs, err)
		}
	}
	fmt.Fprint(d.s.stdout, res.reportText)
	d.s.say("report: %s", filepath.Join(d.s.ev.root, "report.txt"))
	d.s.say("audit: %s (%s)", filepath.Join(d.s.ev.root, "audit.md"), PendingReview)
	if err := errors.Join(errs...); err != nil {
		return false, err
	}
	return res.report.Result == ResultPass, nil
}

// normalizeState is st with every absent list empty rather than null (the
// strict decoder never reads a null list as empty), for ledger.json and
// its comparison.
func normalizeState(st DevFullState) DevFullState {
	if st.Receipts == nil {
		st.Receipts = []OwnerReceipt{}
	}
	if st.Tasks == nil {
		st.Tasks = []TaskRecord{}
	}
	if st.Transitions == nil {
		st.Transitions = []TransitionRecord{}
	}
	if p := st.Pending; p != nil {
		q := *p
		if q.Inputs == nil {
			q.Inputs = []PlanInput{}
		}
		if q.Allowed == nil {
			q.Allowed = []string{}
		}
		st.Pending = &q
	}
	return st
}
