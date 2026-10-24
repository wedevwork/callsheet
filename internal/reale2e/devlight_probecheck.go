package reale2e

import (
	"bytes"
	"slices"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The light and both qualification's offline check (design
// 12a-dev-flow-light, Qualification; FP-9): the light workspace and its
// inert seed capture, the probe-triage task readmitted from its captured
// trees under the light admission rules, the harness-authored candidate's
// exact construction, the light reviewer's re-derived execution evidence
// and the authoritative three-push table, each only from captured records.

// probeLightCheck returns the light record's contradictions (none: it
// holds).
func probeLightCheck(b *DevBundle, q *DevQualification) []string {
	var codes []string
	bad := func(code string) {
		if !slices.Contains(codes, code) {
			codes = append(codes, code)
		}
	}
	l := q.Light
	repo := b.Repos[probeLightRepoDir]
	if l.Workspace.Name == "" || l.Workspace.Name == q.Workspace.Name || l.Workspace.Instance == q.Workspace.Instance ||
		l.Workspace.SeedCommit != l.Seed || l.Workspace.SeedTree != l.SeedTree {
		bad("light_workspace_invalid")
	}
	// The light fixture: exactly the four inert captures, each its
	// recorded bytes, at its computed storage path.
	fixture := map[string][]byte{}
	if len(l.Fixture) != len(lightFixtureFiles) {
		bad("light_fixture_mismatch")
	}
	for i, f := range l.Fixture {
		if i >= len(lightFixtureFiles) {
			break
		}
		name := lightFixtureFiles[i]
		origin := devLightFixtureDir + "/" + name
		if name == "go.mod" {
			origin = "generated: module example.com/greeting with the checkout's go directive"
		}
		data, ok := b.captured(f.Path)
		if !ok || f.Path != lightFixtureCaptureDir+"/"+name || f.StoragePath != capturedPath(f.Path) || f.Origin != origin || len(data) != f.Bytes ||
			sha256Hex(data) != f.SHA256 {
			bad("light_fixture_mismatch")
			continue
		}
		fixture[name] = data
	}
	// The light seed is exactly the captured fixture, with the seeded
	// defect.
	seed, seedTree, seedFiles, ok := repoRefTree(repo, "seed")
	if !ok || seed != l.Seed || seedTree != l.SeedTree || !sameTreeBytes(seedFiles, fixture) || lightDefectOf(fixture["main.go"]).Line == 0 {
		bad("light_seed_mismatch")
	}
	for _, code := range probeTriageCheck(b, q, repo, seedFiles) {
		bad(code)
	}
	// The candidate: the admitted triage tree with only main.go and
	// main_test.go replaced by the captured known-good fixture bytes, its
	// single parent the triage commit.
	triage, _, triageFiles, okT := repoRefTree(repo, "triage")
	cand, candTree, candFiles, okC := repoRefTree(repo, "candidate")
	want := map[string][]byte{}
	for name, f := range triageFiles {
		want[name] = f.Content
	}
	for _, name := range []string{"main.go", "main_test.go"} {
		good, has := b.captured("setup/probe-fixture/" + name)
		if !has {
			bad("light_candidate_mismatch")
		}
		want[name] = good
	}
	if !okT || !okC || triage != l.TriageCommit || cand != l.Candidate || candTree != l.CandidateTree || !sameTreeBytes(candFiles, want) ||
		!singleParent(repo, cand, triage) {
		bad("light_candidate_mismatch")
	}
	if code := probeLightReviewerCheck(b, q, repo, seedFiles, candTree, candFiles); code != "" {
		bad(code)
	}
	for _, code := range probePushCheck(b, q) {
		bad(code)
	}
	// A retained rejection of either light probe task is never part of a
	// qualified record; it is judged by the shared validator.
	for _, t := range []struct {
		dir string
		n   int
	}{{probeTriageDir, probeTriageOrdinal}, {probeLightReviewDir, probeLightReviewOrdinal}} {
		in := rejectionInput{b: b, dir: t.dir, ordinal: t.n, runID: q.RunID}
		if q.StopCode != nil && probeStopped(b, q) {
			in.stopCode = *q.StopCode
		}
		if t.n == probeLightReviewOrdinal && okC {
			in.review = &reviewRecheck{helper: q.Helper, candidate: cand, initial: l.Seed, candTree: candTree, cand: candFiles,
				seedMain: fileContent(seedFiles, "main.go"), light: &lightRecheck{seedFiles: seedFiles, seedTree: l.SeedTree, seedCLI: true}}
		}
		if v := judgeRejection(in); v.present {
			bad("publication_rejected")
			for _, code := range v.bad {
				bad(code)
			}
			if v.recheck != "" {
				bad(v.recheck)
			}
		}
	}
	return codes
}

// sameTreeBytes reports a tree holding exactly files' paths and bytes, all
// regular files.
func sameTreeBytes(tree map[string]TreeFile, files map[string][]byte) bool {
	if len(tree) != len(files) {
		return false
	}
	for name, f := range tree {
		b, ok := files[name]
		if !ok || !bytes.Equal(f.Content, b) || f.Mode != filemode.Regular {
			return false
		}
	}
	return true
}

// singleParent reports commit c's only parent is parent.
func singleParent(v *repoView, c, parent string) bool {
	if v == nil || !contract.ValidCommitHash(c) {
		return false
	}
	cm, err := v.repo.CommitObject(plumbing.NewHash(c))
	return err == nil && len(cm.ParentHashes) == 1 && cm.ParentHashes[0].String() == parent
}

// probeTriageCheck re-admits the probe-triage publication from the light
// repository's seed and triage trees under the light admission rules
// (current confidence-qualified brief, Design required: no, one root-cause
// reference resolving in the seed), against its rebuilt envelope, plan and
// authoritative task.
func probeTriageCheck(b *DevBundle, q *DevQualification, repo *repoView, seedFiles map[string]TreeFile) []string {
	l := q.Light
	v := doc[contract.TaskShowResponse](b, probeTriageDir+"/task.json")
	plan := doc[devEnvelope[TaskPlan]](b, probeTriageDir+"/plan.json")
	adm := doc[DevAdmission](b, probeTriageDir+"/admission.json")
	if v == nil || plan == nil || adm == nil {
		return []string{"triage_evidence_unobserved"}
	}
	goal, acc := probeTriageEnvelope(q.RunID, l.Seed)
	p, t := plan.Data, v.Task
	wb := t.WorkspaceBinding
	switch {
	case p.Ordinal != probeTriageOrdinal || p.Role != "triage" || p.Mode != modeInvestigate || p.Scenario != scenarioLight || p.ExpectedBase != l.Seed ||
		p.Goal != goal || p.Acceptance != acc:
		return []string{"triage_binding_mismatch"}
	case t.TaskID != l.TriageTask || t.Request.Goal != goal || t.Request.Acceptance != acc || t.Request.Override != nil || len(t.Request.Payload) != 0:
		return []string{"triage_binding_mismatch"}
	case wb == nil || wb.Name != l.Workspace.Name || wb.Instance != l.Workspace.Instance || wb.BaseCommit == nil || *wb.BaseCommit != l.Seed:
		return []string{"triage_binding_mismatch"}
	}
	triage, triageTree, triageFiles, ok := repoRefTree(repo, "triage")
	r := t.Result
	if !ok || triage != l.TriageCommit || r == nil || r.FinalMessage == nil || r.Workspace == nil || r.Workspace.Commit == nil || *r.Workspace.Commit != triage ||
		!singleParent(repo, triage, l.Seed) {
		return []string{"triage_binding_mismatch"}
	}
	marker, err := parseTerminalMarker(*r.FinalMessage)
	if err != nil {
		return []string{"triage_admission_invalid"}
	}
	res, err := AdmitPublication(admitInput{Role: "triage", Mode: modeInvestigate, Marker: marker, Base: treeList(seedFiles), Result: treeList(triageFiles),
		Final: *r.FinalMessage, TaskID: t.TaskID, Scenario: scenarioLight, Seed: seedFiles})
	switch {
	case err != nil:
		return []string{"triage_admission_invalid"}
	case marker != markerTriageConf && marker != markerTriageProb || res.DesignRequired != designRequiredNo || !res.BriefCurrent:
		return []string{"triage_unqualified"}
	case adm.Task != t.TaskID || adm.Marker != marker || adm.Commit != triage || adm.Parent != l.Seed || adm.Tree != triageTree ||
		adm.DesignRequired != res.DesignRequired || adm.BriefCurrent != res.BriefCurrent:
		return []string{"triage_admission_mismatch"}
	}
	return nil
}

// probeLightReviewerCheck re-derives the light reviewer probe's execution
// evidence from its log, metadata, authoritative task and the light
// repository's seed, candidate and review, and requires a verified
// approval equal to the stored evidence ("" when it holds).
func probeLightReviewerCheck(b *DevBundle, q *DevQualification, repo *repoView, seedFiles map[string]TreeFile, candTree string, candFiles map[string]TreeFile) string {
	l := q.Light
	dir := probeLightReviewDir
	v := doc[contract.TaskShowResponse](b, dir+"/task.json")
	plan := doc[devEnvelope[TaskPlan]](b, dir+"/plan.json")
	stored := doc[DevReviewRecord](b, dir+"/review-evidence.json")
	if v == nil || plan == nil || stored == nil {
		return "light_reviewer_evidence_unobserved"
	}
	if _, ok := b.Files[dir+"/logs.txt"]; !ok {
		return "light_reviewer_evidence_unobserved"
	}
	goal, acc := probeLightReviewEnvelope(q.RunID, q.Helper, l.Candidate, l.Seed, l.TriageTask, l.TriageCommit)
	p, t := plan.Data, v.Task
	review, _, reviewFiles, ok := repoRefTree(repo, "review")
	r, wb := t.Result, t.WorkspaceBinding
	switch {
	case !ok || review != l.ReviewCommit || t.TaskID != l.ReviewTask || !singleParent(repo, review, l.Candidate):
		return "light_reviewer_binding_mismatch"
	case p.Ordinal != probeLightReviewOrdinal || p.Role != HopCodeReviewer || p.Mode != modeReviewLight || p.ExpectedBase != l.Candidate || p.Goal != goal ||
		p.Acceptance != acc || t.Request.Goal != goal || t.Request.Acceptance != acc || t.Request.Override != nil || len(t.Request.Payload) != 0:
		return "light_reviewer_binding_mismatch"
	case wb == nil || wb.Name != l.Workspace.Name || wb.Instance != l.Workspace.Instance || wb.BaseCommit == nil || *wb.BaseCommit != l.Candidate:
		return "light_reviewer_binding_mismatch"
	case r == nil || r.FinalMessage == nil || r.Workspace == nil || r.Workspace.Commit == nil || *r.Workspace.Commit != review:
		return "light_reviewer_binding_mismatch"
	}
	marker, _ := parseTerminalMarker(*r.FinalMessage)
	base := bundleReviewInput(b, dir, probeLightReviewOrdinal, &t, q.Helper, l.Candidate, l.Seed, candTree, candFiles, fileContent(seedFiles, "main.go"), marker,
		fileText(reviewFiles, "review.md"))
	derived := DeriveLightReviewEvidence(lightReviewInput{reviewEvidenceInput: base, SeedFiles: seedFiles, SeedTree: l.SeedTree, SeedCLI: true})
	switch {
	case derived.Status != evidenceVerified || !derived.Approving:
		return "light_reviewer_evidence_unobserved"
	case !bytes.Equal(mustJSON(stored.Evidence), mustJSON(derived)):
		return "light_reviewer_evidence_mismatch"
	}
	return ""
}

// probePushPlans is the light qualification's authoritative push table,
// from the record only where it agrees with the regenerated render values,
// the workspace identities and the captured repositories (false: it does
// not).
func probePushPlans(b *DevBundle, q *DevQualification) ([]mcpPushPlan, bool) {
	l := q.Light
	if len(l.Pushes) != 3 {
		return nil, false
	}
	seedRef, _, _, okS := repoRefTree(b.Repos[probeLightRepoDir], "seed")
	full, _, _, okF := probeRefTree(b, "candidate")
	type want struct {
		ws     WorkspaceRecord
		path   string
		commit string
		after  string
		refOK  bool
		late   bool
	}
	cand, _, _, okC := repoRefTree(b.Repos[probeLightRepoDir], "candidate")
	wants := []want{
		{ws: q.Workspace, path: q.Render["SEED_PATH"], commit: q.Candidate, after: pushAfterSeed, refOK: okF && full == q.Candidate},
		{ws: l.Workspace, path: q.Render["LIGHT_SEED_PATH"], commit: l.Seed, after: pushAfterSeed, refOK: okS && seedRef == l.Seed},
		{ws: l.Workspace, path: q.Render["LIGHT_CANDIDATE_PATH"], commit: l.Candidate, after: pushAfterTriage, refOK: l.Candidate == "" || okC && cand == l.Candidate,
			late: true},
	}
	var plans []mcpPushPlan
	for i, w := range wants {
		p := l.Pushes[i]
		if p.Order != i+1 || p.Workspace != w.ws.Name || p.Instance != w.ws.Instance || p.Branch != contract.DefaultBranchRef || p.Path != w.path || p.Path == "" ||
			p.Commit != w.commit || p.After != w.after || !w.refOK {
			return nil, false
		}
		plans = append(plans, mcpPushPlan{workspace: p.Workspace, instance: p.Instance, path: p.Path, commit: p.Commit, late: w.late})
	}
	if plans[1].path == plans[2].path {
		return nil, false
	}
	return plans, true
}

// probeLightAuthority adds the light qualification's tasks, workspace and
// push table to its call authority; a table that does not agree with its
// sources refuses every push.
func probeLightAuthority(b *DevBundle, q *DevQualification, in *mcpAuthorityInput) {
	l := q.Light
	in.dirs[probeTriageOrdinal] = probeTriageDir
	in.labels[probeTriageNamespace+"-01"] = probeTriageOrdinal
	in.dirs[probeLightReviewOrdinal] = probeLightReviewDir
	in.labels[probeLightReviewNamespace+"-01"] = probeLightReviewOrdinal
	in.lateAfter, in.lateBefore = probeTriageOrdinal, probeLightReviewOrdinal
	var objects storer.EncodedObjectStorer
	if repo := b.Repos[probeLightRepoDir]; repo != nil {
		objects = repo.repo.Storer
	}
	w := &mcpWorkspace{name: l.Workspace.Name, instance: l.Workspace.Instance, objects: objects}
	in.extra = []*mcpWorkspace{w}
	plans, ok := probePushPlans(b, q)
	if !ok {
		in.pushes = []mcpPushPlan{}
		return
	}
	in.pushes = plans
	w.mains = []int{1, 2}
}

// probePushCheck checks the push table's supervisor observations: each
// push seen once on its workspace's main branch, the two seed pushes before
// any exposure and the candidate push after the triage task's consumed
// wait and before the light review's exposure.
func probePushCheck(b *DevBundle, q *DevQualification) []string {
	if _, ok := probePushPlans(b, q); !ok {
		return []string{"push_table_invalid"}
	}
	l := q.Light
	seqOf := func(match func(Event) bool) []int {
		var out []int
		for _, e := range b.Events {
			if match(e) {
				out = append(out, e.Seq)
			}
		}
		return out
	}
	pushed := func(code string) []int {
		return seqOf(func(e Event) bool { return e.Type == EvSeedCreated && e.Code == code })
	}
	firstExposure := seqOf(func(e Event) bool { return e.Type == EvCoordinatorReady && e.Code == "plan_exposed" })
	consumed := seqOf(func(e Event) bool {
		return e.Type == EvTaskTerminal && e.Code == "wait_consumed" && e.Hop == probeTriageNamespace+"-01"
	})
	exposed := seqOf(func(e Event) bool {
		return e.Type == EvCoordinatorReady && e.Code == "plan_exposed" && e.Hop == probeLightReviewNamespace+"-01"
	})
	var codes []string
	for i, code := range []string{"seed_pushed", "light_seed_pushed", "light_candidate_pushed"} {
		seqs := pushed(code)
		if len(seqs) != 1 || l.Pushes[i].VerifiedSeq != seqs[0] {
			codes = append(codes, "push_unverified")
			continue
		}
		switch i {
		case 0, 1:
			if len(firstExposure) == 0 || seqs[0] > firstExposure[0] {
				codes = append(codes, "push_order_invalid")
			}
		case 2:
			if len(consumed) != 1 || len(exposed) != 1 || seqs[0] < consumed[0] || seqs[0] > exposed[0] {
				codes = append(codes, "push_order_invalid")
			}
		}
	}
	return codes
}
