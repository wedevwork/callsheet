package reale2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The post-run audit (design 12a-real-e2e, amendment A3, FP-12): a pure,
// deterministic, ordered view of the captured evidence. Mechanical rows
// (and the deterministic layer-2 rows) are correct only on positive,
// complete evidence, incorrect when the evidence contradicts the
// requirement (naming the failure code) and unobserved when proof is
// missing (naming what). Judgment rows always stay unobserved, pending the
// owner's external dev-flow coordinator review; no keyword heuristic
// promotes them. The rows are derived from the bundle alone; the audit's
// header binds the report (its SHA-256 and mechanical result), so the
// report is generated first and the audit from it. Building the audit
// executes nothing: no model, no test, no process.

// auditText bounds and control-escapes an untrusted excerpt.
func auditText(s string, n int) string { return contract.SafeText(s, n) }

// rowFacts accumulates one row's observations.
type rowFacts struct {
	observed []string
	missing  []string
	codes    []string
	refs     []AuditRef
}

func (f *rowFacts) see(format string, args ...any) {
	f.observed = append(f.observed, fmt.Sprintf(format, args...))
}

func (f *rowFacts) miss(what string) {
	if !slices.Contains(f.missing, what) {
		f.missing = append(f.missing, what)
	}
}

func (f *rowFacts) bad(code string) {
	if !slices.Contains(f.codes, code) {
		f.codes = append(f.codes, code)
	}
}

// refFile cites a bundle file (when present) with an optional line span,
// task and commit.
func (f *rowFacts) refFile(c *checker, file string, lines []int, task, commit string) {
	data, ok := c.b.Files[file]
	if !ok {
		return
	}
	if lines == nil {
		lines = []int{}
	}
	f.refs = append(f.refs, AuditRef{File: file, SHA256: sha256Hex(data), Lines: lines, Task: task, Commit: commit})
}

// refSources cites derived event-map sources.
func (f *rowFacts) refSources(srcs []SourceRef, task string) {
	for _, s := range srcs {
		f.refs = append(f.refs, AuditRef{File: s.File, SHA256: s.SHA256, Lines: []int{s.FirstLine, s.LastLine}, Task: task})
	}
}

// refTree cites a file of a result tree by its content hash and commit.
func (f *rowFacts) refTree(files map[string][]byte, path, commit string) {
	if b, ok := files[path]; ok {
		f.refs = append(f.refs, AuditRef{File: path, SHA256: sha256Hex(b), Lines: []int{}, Commit: commit})
	}
}

// classify files a criterion's codes: those in absent are missing proof,
// the rest are contradictions.
func (f *rowFacts) classify(codes []string, absent ...string) {
	for _, code := range codes {
		if slices.Contains(absent, code) {
			f.miss(code)
		} else {
			f.bad(code)
		}
	}
}

// rowSpec is one row's fixed identity.
type rowSpec struct {
	id, layer, requirement string
	fps, criteria          []string
}

// row renders a row from its facts.
func row(spec rowSpec, f rowFacts) AuditRow {
	r := AuditRow{ID: spec.id, FPs: spec.fps, Layer: spec.layer, Requirement: spec.requirement, Evidence: f.refs, Criteria: slices.Clone(spec.criteria)}
	if r.Evidence == nil {
		r.Evidence = []AuditRef{}
	}
	if r.Criteria == nil {
		r.Criteria = []string{}
	}
	r.Observed = strings.Join(f.observed, "; ")
	if len(f.missing) > 0 {
		obs := "missing: " + strings.Join(f.missing, ", ")
		if r.Observed == "" {
			r.Observed = obs
		} else {
			r.Observed += "; " + obs
		}
	}
	if r.Observed == "" {
		r.Observed = "nothing observed"
	}
	switch {
	case spec.layer == LayerJudgment:
		r.Verdict, r.Correction = VerdictUnobserved, PendingReview
	case len(f.codes) > 0:
		r.Verdict = VerdictIncorrect
		r.Correction = "The captured evidence contradicts this requirement (" + strings.Join(f.codes, ", ") +
			"); the report records a mechanical FAIL. Nothing was repaired; a rerun creates new evidence."
		for _, c := range f.codes {
			r.Criteria = append(r.Criteria, "code:"+c)
		}
	case len(f.missing) > 0:
		r.Verdict = VerdictUnobserved
		r.Correction = "Proof is missing (" + strings.Join(f.missing, ", ") + "); unobserved mechanical proof is a FAIL and nothing was inferred."
	default:
		r.Verdict = VerdictCorrect
	}
	return r
}

// scratch returns a checker sharing c's derived facts and memo with no
// codes, so a criterion can be evaluated on its own.
func (c *checker) scratch() *checker {
	return &checker{b: c.b, codes: map[string][]string{}, views: c.views, commits: c.commits, files: c.files, seed: c.seed, derived: c.derived, memo: c.memo}
}

// codesOf evaluates one criterion method alone and returns its codes for
// crit.
func (c *checker) codesOf(f func(*checker), crit string) []string {
	return slices.Clone(c.methodCodes(f)[crit])
}

// fixed FP lists.
var (
	fpPreflight = []string{"FP-10"}
	fpDeploy    = []string{"FP-2"}
	fpPhase     = []string{"FP-3"}
	fpDispatch  = []string{"FP-4", "FP-6"}
	fpWait      = []string{"FP-6"}
	fpResult    = []string{"FP-4"}
	fpSignoff   = []string{"FP-5"}
	fpFinal     = []string{"FP-7"}
	fpCleanup   = []string{"FP-8"}
	fpEnv       = []string{"FP-11"}
	fpDetermin  = []string{"FP-7", "FP-12"}
	fpJudgment  = []string{"FP-12"}
)

// auditRows derives the ordered rows from a loaded checker (pure), once
// per check (shared: never modify them).
func auditRows(c *checker) []AuditRow {
	if c.memo != nil && c.memo.rows != nil {
		return c.memo.rows
	}
	rows := deriveAuditRows(c)
	if c.memo != nil {
		c.memo.rows = rows
	}
	return rows
}

// deriveAuditRows computes the ordered rows.
func deriveAuditRows(c *checker) []AuditRow {
	var rows []AuditRow
	add := func(spec rowSpec, f rowFacts) { rows = append(rows, row(spec, f)) }
	add(rowSpec{id: "preflight", layer: LayerMechanical, fps: fpPreflight, criteria: []string{CritPreflight},
		requirement: "Four authenticated no-workspace preflight tasks, one per configured role and pair, each succeeded with the exact answer " + AuthToken + " within 2m, after deployment and before feature work"},
		c.preflightFacts())
	add(rowSpec{id: "deployment_roles", layer: LayerMechanical, fps: fpDeploy, criteria: []string{CritRoles},
		requirement: "The four roles registered with the frozen flow's adapter, model and effort (concurrency 1, timeout 10m), all sidecars reconnected and every role ready"},
		c.rolesFacts())
	add(rowSpec{id: "coordinator_phase1", layer: LayerMechanical, fps: fpPhase, criteria: []string{CritSetup, CritEventMap},
		requirement: "The supervisor launched the fresh headless phase-1 session with the exact configuration-preserving argv (the session-only sandbox-off settings and the exact 16-rule allow-list, no bypass), observed its init and successful pause result, exit 0 and its whole process group gone"},
		c.phaseFacts(1))
	for i := range Hops {
		if i == 2 {
			add(rowSpec{id: "signoff", layer: LayerMechanical, fps: fpSignoff, criteria: []string{CritOwner},
				requirement: "After the approved design review and phase 1's exit, the terminal prompt took the owner's explicit yes within 20m, bound to the reviewed task, commit and both artifact digests, before any coding admission"},
				c.signoffFacts())
			add(rowSpec{id: "coordinator_phase2", layer: LayerMechanical, fps: fpPhase, criteria: []string{CritSetup, CritEventMap},
				requirement: "After the yes only, the supervisor resumed the same session (--resume with phase 1's session ID) with the same configuration, observed its init and successful final result, exit 0 and its whole process group gone"},
				c.phaseFacts(2))
		}
		n := "hop" + hopDir(i)
		add(rowSpec{id: n + "_dispatch", layer: LayerMechanical, fps: fpDispatch, criteria: []string{CritHops, CritEventMap, CritAdmissions},
			requirement: "The coordinator dispatched hop " + Hops[i] + " once through MCP (no wait) on the pinned workspace instance and required base, with the run marker and the role's frozen selection, and the supervisor observed it"},
			c.dispatchFacts(i))
		add(rowSpec{id: n + "_wait", layer: LayerMechanical, fps: fpWait, criteria: []string{CritWaits},
			requirement: "The hop's direct CLI wait ran as one Bash background job from the rendered template, its status captured by the capture-wait helper, its raw output was captured and reparsed, and the coordinator consumed its completion"},
			c.waitFacts(i))
		add(rowSpec{id: n + "_result", layer: LayerMechanical, fps: fpResult, criteria: []string{CritHops, CritChain, CritArtifacts},
			requirement: "Hop " + Hops[i] + " succeeded with exit 0 on its role's adapter, model and effort, published one commit whose single parent is its base, changed only its allowed paths and ended its complete, untruncated final answer with " + approvedMarkers[Hops[i]]},
			c.resultFacts(i))
		rows = append(rows, c.layerTwo(i)...)
	}
	add(rowSpec{id: "final_tests", layer: LayerMechanical, fps: fpFinal, criteria: []string{CritFinalTests},
		requirement: "The supervisor ran go test -buildvcs=false -count=1 ./... (exit 0) on an export of the fourth result, the build, the ten CLI cases and go test -json, all bound to that commit within the shared 60s budget; the baseline tests passed"},
		c.criterionFacts((*checker).finalTests, CritFinalTests, "final_tests_missing", "baseline_missing", "validation_missing"))
	add(rowSpec{id: "cleanup", layer: LayerMechanical, fps: fpCleanup, criteria: []string{CritCleanup, CritDeadlines},
		requirement: "Every started owned process (each started coordinator group, every sidecar generation and the plane) was stopped by its captured handle with its exit verified, every task settled and the runtime removed within the 2m cleanup bound"},
		c.cleanupFacts())
	add(rowSpec{id: "owner_environment", layer: LayerMechanical, fps: fpEnv, criteria: []string{CritOwnerEnv},
		requirement: "Complete before and after fingerprints of every supplied protected root's repositories and the four vendor homes show no owner-configuration change; tool-maintained changes are summarized per vendor top-level directory only"},
		c.environmentFacts())
	return rows
}

// criterionFacts is a row over one whole criterion.
func (c *checker) criterionFacts(f func(*checker), crit string, absent ...string) rowFacts {
	var facts rowFacts
	codes := c.codesOf(f, crit)
	facts.classify(codes, absent...)
	if len(codes) == 0 {
		facts.see("criterion %s holds on the captured evidence", crit)
	}
	if crit == CritFinalTests {
		for _, name := range []string{"validation/final-test.json", "validation/baseline.json", "validation/cases.json", buildStdoutFile, buildStderrFile,
			"validation/tests.jsonl", "validation/requirements.json"} {
			facts.refFile(c, name, nil, "", "")
		}
	}
	return facts
}

func (c *checker) preflightFacts() rowFacts {
	var f rowFacts
	for i := range Hops {
		dir := "preflight/" + hopDir(i)
		t := c.b.Preflight[i]
		if t == nil {
			f.miss(dir + "/task.json")
			continue
		}
		v := t.View
		fin := ""
		if v.FinishedAt != nil {
			fin = *v.FinishedAt
		}
		f.see("%s: task %s on %s/%s/%s, created %s, finished %s, state %s", preflightHop(i), auditText(v.TaskID, 64), auditText(v.Role.Adapter, 32),
			auditText(v.Effective.Model, 64), auditText(v.Effective.Effort, 16), auditText(v.CreatedAt, 40), auditText(fin, 40), auditText(v.State, 16))
		f.refFile(c, dir+"/task.json", nil, v.TaskID, "")
		f.refFile(c, dir+"/final.txt", nil, v.TaskID, "")
	}
	f.classify(c.codesOf((*checker).preflight, CritPreflight), "preflight_missing")
	for i := range Hops {
		if t := c.b.Preflight[i]; t != nil {
			if d, ok := duration(t.View); !ok || d > PreflightBound {
				f.bad("preflight_deadline_exceeded")
			}
		}
	}
	return f
}

func (c *checker) rolesFacts() rowFacts {
	var f rowFacts
	r := c.b.Roles
	if r == nil {
		f.miss("setup/roles.json")
	} else {
		for _, s := range r.Roles {
			f.see("%s: %s/%s/%s node %s ready %t", auditText(s.ID, 64), auditText(s.Adapter, 16), auditText(s.Model, 64), auditText(s.Effort, 16),
				auditText(s.Node, 40), s.CanAccept)
		}
		f.see("ready at %s", auditText(r.ReadyAt, 40))
		f.refFile(c, "setup/roles.json", nil, "", "")
	}
	for _, typ := range []string{EvRolesRegistered, EvSidecarsReconnected, EvRolesReady} {
		if e, ok := c.firstEvent(typ, nil); ok {
			f.see("%s at %s (seq %d)", typ, e.Time, e.Seq)
		} else {
			f.miss(typ + " event")
		}
	}
	f.classify(c.codesOf((*checker).roles, CritRoles), "roles_missing")
	return f
}

// streamResult reports whether a phase stream holds a result record and
// whether it was successful and bound to the stream's init session.
func streamResult(stream []byte) (present, ok bool) {
	d := &deriver{codes: map[string]bool{}}
	return recordsResult(d.decodeLines(phaseStreamFiles[0], stream))
}

// recordsResult is streamResult over a stream's decoded records.
func recordsResult(records []sessionRecord) (present, ok bool) {
	sid, hasInit := "", false
	for _, r := range records {
		switch r.kind {
		case "init":
			if !hasInit {
				sid, hasInit = r.sessionID, true
			}
		case "result":
			return true, r.resultOK && hasInit && r.sessionID == sid
		}
	}
	return false, false
}

// phaseStreamResult is streamResult of phase p's stream, which the
// derivation newChecker ran has already decoded.
func (c *checker) phaseStreamResult(p int, stream []byte) (present, ok bool) {
	if c.memo != nil {
		r := c.memo.streams[p-1]
		return r[0], r[1]
	}
	return streamResult(stream)
}

// toolCommand is the raw input of tool use id in a phase stream, with the
// line that carries it: a Bash command, otherwise the input's JSON.
func toolCommand(file string, stream []byte, id string) (string, SourceRef, bool) {
	d := &deriver{codes: map[string]bool{}}
	for _, r := range d.decodeLines(file, stream) {
		for _, b := range r.blocks {
			if r.kind != "assistant" || b.typ != "tool_use" || b.id != id {
				continue
			}
			var in struct {
				Command *string `json:"command"`
			}
			if json.Unmarshal(b.input, &in) == nil && in.Command != nil {
				return *in.Command, r.src, true
			}
			return string(b.input), r.src, true
		}
	}
	return "", SourceRef{}, false
}

// findingPhases reports the phases a finding belongs to (both when it has
// no source).
func findingPhases(f MapFinding) [2]bool {
	if len(f.Sources) == 0 {
		return [2]bool{true, true}
	}
	var out [2]bool
	for _, s := range f.Sources {
		for p := 0; p < 2; p++ {
			if s.File == phaseStreamFiles[p] || s.File == transcriptFiles[p] {
				out[p] = true
			}
		}
	}
	return out
}

// unobservedFindings are derivation findings that mean missing proof.
var unobservedFindings = []string{CodeSessionUnobserved, CodeWakeUnsupported}

func (c *checker) phaseFacts(p int) rowFacts {
	var f rowFacts
	l := c.b.Launches[p-1]
	stream, hasStream := c.b.Files[phaseStreamFiles[p-1]]
	if l == nil {
		f.miss(launchFiles[p-1])
	}
	if !hasStream {
		f.miss(phaseStreamFiles[p-1])
	}
	var exit *PhaseProcess
	if ce := c.b.CoordExit; ce != nil {
		for i := range ce.Phases {
			if ce.Phases[i].Phase == p {
				exit = &ce.Phases[i]
			}
		}
	}
	if exit == nil {
		f.miss("phase " + fmt.Sprint(p) + " process observation")
	}
	var init *InitRecord
	for i := range c.derived.Init {
		if c.derived.Init[i].Phase == p {
			init = &c.derived.Init[i]
		}
	}
	if init == nil {
		f.miss("phase " + fmt.Sprint(p) + " init record")
	} else {
		f.see("session %s, model %s, permission mode %s, %d tools, MCP servers [%s]", auditText(init.SessionID, 40), auditText(init.Model, 64),
			auditText(init.PermissionMode, 32), len(init.Tools), auditText(strings.Join(init.MCPServers, ","), 400))
		if p == 2 && init.SessionID != c.derived.SessionID {
			f.bad("session_resume_mismatch")
		}
	}
	if hasStream {
		present, ok := c.phaseStreamResult(p, stream)
		switch {
		case !present:
			f.miss("phase " + fmt.Sprint(p) + " result record")
		case !ok:
			f.bad("phase_result_failed")
		}
	}
	if l != nil {
		if !c.launchValid(p) {
			f.bad("launch_shape_invalid")
		}
		f.see("launch %s", auditText(strings.Join(l.Display, " "), 1200))
		f.refFile(c, launchFiles[p-1], nil, "", "")
	}
	if exit != nil {
		code := "none"
		if exit.ExitCode != nil {
			code = fmt.Sprint(*exit.ExitCode)
		}
		f.see("pid %d exit %s signal %q, leader reaped %t, group gone %t", exit.PID, code, exit.Signal, exit.Reaped, exit.GroupGone)
		if !exit.Reaped || !exit.GroupGone {
			f.bad("coordinator_group_alive")
		}
		if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Signal != "" {
			f.bad("coordinator_exit_nonzero")
		}
		f.refFile(c, coordinatorExitFile, nil, "", "")
	}
	for _, fi := range c.derived.Findings {
		if !findingPhases(fi)[p-1] {
			continue
		}
		if slices.Contains(unobservedFindings, fi.Code) {
			f.miss(fi.Code)
		} else {
			f.bad(fi.Code)
		}
		f.refSources(fi.Sources, "")
		if (fi.Code == CodeToolDenied || fi.Code == CodeToolFailed) && fi.ToolUseID != "" {
			if cmd, src, ok := toolCommand(phaseStreamFiles[p-1], stream, fi.ToolUseID); ok {
				f.see("%s: %s tool use %s, command %s", fi.Code, auditText(fi.Tool, 64), auditText(fi.ToolUseID, 64), auditText(cmd, 1200))
				f.refSources([]SourceRef{src}, "")
			} else {
				f.miss("tool use " + auditText(fi.ToolUseID, 64) + " input")
			}
		}
	}
	if hasStream {
		f.refFile(c, phaseStreamFiles[p-1], nil, "", "")
	}
	return f
}

func (c *checker) signoffFacts() rowFacts {
	var f rowFacts
	if d := c.b.Decision; d != nil {
		f.see("decision %q for task %s commit %s (design sha256 %s, review sha256 %s) at %s seq %d", auditText(d.Decision, 8), auditText(d.ReviewedTask, 64),
			auditText(d.ResultCommit, 40), auditText(d.DesignSHA256, 64), auditText(d.ReviewSHA256, 64), auditText(d.DecidedAt, 40), d.Seq)
		f.refFile(c, "owner-decision.json", nil, d.ReviewedTask, d.ResultCommit)
	}
	if e, ok := c.firstEvent(EvAwaitingOwner, nil); ok {
		f.see("prompt opened at %s (seq %d)", e.Time, e.Seq)
	}
	f.classify(c.codesOf((*checker).owner, CritOwner), "decision_missing")
	if ok := len(c.eventsOf(EvPrematureAdmission)) == 0; !ok {
		f.bad("premature_admission")
	}
	return f
}

func (c *checker) dispatchFacts(i int) rowFacts {
	var f rowFacts
	dir := "hops/" + hopDir(i)
	d, v := c.b.Hops[i].Dispatch, c.views[i]
	if d == nil {
		f.miss(dir + "/dispatch.json")
	}
	if v == nil {
		f.miss(dir + "/task.json")
	}
	es := entriesOf(c.derived, MapDispatch, Hops[i])
	if len(es) != 1 {
		f.miss("the coordinator's dispatch tool use linked to the observed task")
	}
	if v != nil {
		m := c.b.Manifest
		if v.Role.ID != Hops[i] || m == nil || len(v.Request.Payload) == 0 || v.Request.Payload[0] != markerFor(m.RunID, Hops[i]) {
			f.bad("hop_binding_mismatch")
		}
		if !c.selectionMatches(*v, Hops[i]) {
			f.bad("selection_mismatch")
		}
		b := v.WorkspaceBinding
		if b == nil || m == nil || b.Name != m.Workspace.Name || b.Instance != m.Workspace.Instance {
			f.bad("workspace_instance_mismatch")
		}
		base := c.expectedBaseOf(i)
		if b == nil || b.BaseCommit == nil || *b.BaseCommit != base || base == "" {
			f.bad("base_binding_mismatch")
		}
		f.see("task %s role %s on %s/%s/%s, created %s", auditText(v.TaskID, 64), auditText(v.Role.ID, 32), auditText(v.Role.Adapter, 16),
			auditText(v.Effective.Model, 64), auditText(v.Effective.Effort, 16), auditText(v.CreatedAt, 40))
		if b != nil && b.BaseCommit != nil {
			f.see("workspace %s instance %s base %s", auditText(b.Name, 64), auditText(b.Instance, 64), auditText(*b.BaseCommit, 40))
		}
		f.refFile(c, dir+"/task.json", nil, v.TaskID, "")
		if d != nil {
			bb := v.WorkspaceBinding
			if d.Hop != Hops[i] || d.Task != v.TaskID || d.RoleID != Hops[i] || bb == nil || bb.BaseCommit == nil || d.Workspace != bb.Name ||
				d.Instance != bb.Instance || d.BaseCommit != *bb.BaseCommit || d.Model != v.Effective.Model || d.Effort != v.Effective.Effort ||
				len(v.Request.Payload) == 0 || d.Marker != v.Request.Payload[0] {
				f.bad("dispatch_record_mismatch")
			}
		}
	}
	if d != nil {
		f.see("observed at seq %d", d.ObservedSeq)
		f.refFile(c, dir+"/dispatch.json", nil, d.Task, "")
		if len(es) == 1 && es[0].Seq != d.ObservedSeq {
			f.bad("dispatch_not_observed")
		}
	}
	if len(es) == 1 {
		f.see("dispatched by tool-use %s in phase %d", auditText(es[0].ToolUseID, 64), es[0].Phase)
		f.refSources(es[0].Sources, es[0].Task)
	}
	if i == 2 {
		receipt, ok1 := c.firstEvent(EvReceiptCreated, nil)
		obs, ok2 := c.firstEvent(EvObserved, func(e Event) bool { return e.Hop == HopCoder })
		if ok2 && (!ok1 || obs.Seq < receipt.Seq) {
			f.bad("coding_before_approval")
		}
	}
	return f
}

// expectedBaseOf is hop i's required base from the bundle (the seed, or
// the previous hop's published result), or "".
func (c *checker) expectedBaseOf(i int) string {
	if i == 0 {
		if m := c.b.Manifest; m != nil {
			return m.Workspace.SeedCommit
		}
		return ""
	}
	return c.resultCommit(i - 1)
}

func (c *checker) waitFacts(i int) rowFacts {
	var f rowFacts
	dir := "hops/" + hopDir(i) + "/"
	w := c.b.Hops[i].Wait
	result, okR := c.b.Files[dir+waitResultFile]
	exit, okX := c.b.Files[dir+waitExitFile]
	if w == nil {
		f.miss(dir + "wait.json")
	}
	if !okR {
		f.miss(dir + waitResultFile)
	}
	if !okX {
		f.miss(dir + waitExitFile)
	}
	starts := entriesOf(c.derived, MapWaitStart, Hops[i])
	consumed := entriesOf(c.derived, MapWaitConsumed, Hops[i])
	wakes := entriesOf(c.derived, MapWake, Hops[i])
	if len(starts) != 1 {
		f.miss("the background wait start")
	} else {
		f.see("background handle %s started by tool-use %s in phase %d", auditText(starts[0].Handle, 64), auditText(starts[0].ToolUseID, 64), starts[0].Phase)
		f.refSources(starts[0].Sources, starts[0].Task)
	}
	switch {
	case len(wakes) == 1 && wakes[0].Automatic:
		f.see("automatic wake by the completion notification")
		f.refSources(wakes[0].Sources, wakes[0].Task)
	case len(wakes) == 1:
		f.see("completion notification observed (not consumed automatically)")
		f.refSources(wakes[0].Sources, wakes[0].Task)
	default:
		f.see("no completion notification before consumption")
	}
	if len(consumed) != 1 {
		f.miss("the coordinator's consumption of the wait")
	} else {
		f.see("consumed by tool-use %s", auditText(consumed[0].ToolUseID, 64))
		f.refSources(consumed[0].Sources, consumed[0].Task)
	}
	if w != nil {
		code := "none"
		if w.ExitCode != nil {
			code = fmt.Sprint(*w.ExitCode)
		}
		f.see("wait exit %s, winner %s state %s", code, auditText(w.Winner, 64), auditText(w.WinnerState, 16))
		f.refFile(c, dir+"wait.json", nil, w.Task, "")
		switch {
		case w.Hop != Hops[i] || w.Handle != waitHandle(i) || w.Task == "" || w.Task != c.hopTask(i) || w.Winner != w.Task:
			f.bad("wait_binding_mismatch")
		case w.ExitCode == nil || *w.ExitCode != 0 || w.WinnerState != contract.TaskSucceeded:
			f.bad("wait_unsuccessful")
		}
		if okR && okX && !c.waitRecordMatches(i, w, result, c.b.Files[dir+waitStderrFile], exit) {
			f.bad("wait_record_mismatch")
		}
	}
	f.refFile(c, dir+waitResultFile, nil, "", "")
	f.refFile(c, dir+waitExitFile, nil, "", "")
	return f
}

func (c *checker) resultFacts(i int) rowFacts {
	var f rowFacts
	dir := "hops/" + hopDir(i)
	v, cm := c.views[i], c.commits[i]
	t := c.b.Hops[i].Task
	if v == nil {
		f.miss(dir + "/task.json")
	}
	if cm == nil {
		f.miss("the result commit in workspace/repo.git")
	}
	if v != nil {
		r := v.Result
		f.see("task %s state %s on %s/%s/%s", auditText(v.TaskID, 64), auditText(v.State, 16), auditText(v.Role.Adapter, 16),
			auditText(v.Effective.Model, 64), auditText(v.Effective.Effort, 16))
		switch {
		case !succeeded(t):
			f.bad("hop_not_succeeded")
		case finalMarker(Hops[i], *r.FinalMessage) != approvedMarkers[Hops[i]]:
			f.bad("final_marker_missing")
		default:
			f.see("final answer %d bytes, complete, last line %q", len(*r.FinalMessage), auditText(lastNonEmptyLine(*r.FinalMessage), 80))
		}
		if r != nil && r.FinalMessage != nil && r.FinalMessageTruncated {
			f.bad("final_message_truncated")
		}
		if r == nil || r.Workspace == nil || r.Workspace.Publication != contract.PublicationPublished || r.Workspace.Commit == nil {
			f.bad("publication_missing")
		}
		if !c.selectionMatches(*v, Hops[i]) {
			f.bad("selection_mismatch")
		}
		f.refFile(c, dir+"/task.json", nil, v.TaskID, c.resultCommit(i))
		f.refFile(c, dir+"/final.txt", nil, v.TaskID, "")
	}
	if cm != nil {
		base := c.expectedBaseOf(i)
		if len(cm.ParentHashes) != 1 || cm.ParentHashes[0].String() != base {
			f.bad("parent_mismatch")
		}
		if c.resultCommit(i) != cm.Hash.String() {
			f.bad("result_commit_mismatch")
		}
		changed := changedPaths(c.files[i], c.files[i+1])
		for _, p := range changed {
			if !slices.Contains(allowedChanges[Hops[i]], p) {
				f.bad("disallowed_change")
			}
		}
		f.see("commit %s parent %s changed [%s]", cm.Hash, base, auditText(strings.Join(changed, ","), 200))
		switch Hops[i] {
		case HopDesigner:
			if len(strings.TrimSpace(string(c.files[1]["design.md"]))) == 0 {
				f.bad("design_missing")
			}
		case HopDesignReviewer:
			if lastNonEmptyLine(string(c.files[2]["design-review.md"])) != approvedMarkers[HopDesignReviewer] {
				f.bad("design_review_not_approved")
			}
		case HopCoder:
			for _, p := range []string{"main.go", "main_test.go"} {
				if string(c.files[2][p]) == string(c.files[3][p]) {
					f.bad("implementation_missing")
				}
			}
		case HopCodeReviewer:
			if c.commits[2] == nil || c.commits[2].TreeHash != cm.TreeHash {
				f.bad("reviewer_tree_changed")
			}
		}
	}
	return f
}

// layerTwo returns hop i's result-correctness rows (deterministic and
// judgment), which follow its result row.
func (c *checker) layerTwo(i int) []AuditRow {
	var rows []AuditRow
	n := "hop" + hopDir(i)
	judge := func(id, req string, f rowFacts) {
		rows = append(rows, row(rowSpec{id: n + "_" + id, layer: LayerJudgment, fps: fpJudgment, requirement: req}, f))
	}
	det := func(id, req string, crit []string, f rowFacts) {
		rows = append(rows, row(rowSpec{id: n + "_" + id, layer: LayerMechanical, fps: fpDetermin, requirement: req, criteria: crit}, f))
	}
	commit := c.resultCommit(i)
	switch Hops[i] {
	case HopDesigner:
		for _, fc := range FeatureCases {
			judge("design_case_"+fc.ID, fmt.Sprintf("The design specifies case %s (argv %q: %s) and requires its TestRun/%s table test", fc.ID, fc.Args, caseExpectation(fc), fc.ID),
				c.designFacts(commit))
		}
		judge("design_run_signature", "The design requires a testable run(args []string, stdout, stderr io.Writer) int", c.designFacts(commit))
		judge("design_table_tests", "The design requires TestRun table tests covering every case, including Unicode, whitespace, every error and help", c.designFacts(commit))
	case HopDesignReviewer:
		f := c.designFacts(c.resultCommit(0))
		f.refTree(c.files[2], "design-review.md", commit)
		judge("review_verdict", "The design review's verdict agrees with the design's coverage of the request", f)
	case HopCoder:
		cases, reqs := c.coderEvidence()
		for k, fc := range FeatureCases {
			det("case_"+fc.ID, fmt.Sprintf("The built CLI run with argv %q gives %s", fc.Args, caseExpectation(fc)), []string{CritResult}, c.caseFacts(cases, k))
		}
		for k, fc := range FeatureCases {
			det("test_"+fc.ID, "The subtest TestRun/"+fc.ID+" executed and passed in go test -json on the fourth result", []string{CritResult}, c.testFacts(reqs, k))
		}
		det("run_signature", "main.go declares run(args []string, stdout, stderr io.Writer) int", []string{CritResult}, c.structureFacts(reqs, true))
		det("table_tests", "main_test.go has TestRun with a table loop that calls run", []string{CritResult}, c.structureFacts(reqs, false))
		f := rowFacts{}
		f.refTree(c.files[3], "main_test.go", commit)
		judge("assertions", "The table tests' assertions actually exercise every required behavior", f)
		f = rowFacts{}
		f.refTree(c.files[3], "main.go", commit)
		judge("completeness", "The implementation is complete beyond the sampled inputs", f)
	case HopCodeReviewer:
		det("review_marker", "The Grok review is a nonempty, untruncated final answer ending "+approvedMarkers[HopCodeReviewer], []string{CritGrok}, c.reviewMarkerFacts())
		det("inline_sources", "The review goal carried design.md, main.go and main_test.go inline with hashes equal to the coder tree's bytes", []string{CritGrok},
			c.inlineFacts())
		det("verdict_tests", "The review's verdict is consistent with the final test outcome", []string{CritResult}, c.verdictFacts())
		f := rowFacts{}
		f.refFile(c, "hops/04/final.txt", nil, c.hopTask(3), "")
		judge("review_quality", "The review analyzes this code, respects the requested scope and reaches a defensible verdict", f)
	}
	return rows
}

// caseExpectation describes a case's required result.
func caseExpectation(fc FeatureCase) string {
	switch fc.Kind {
	case "ok":
		return fmt.Sprintf("exit 0, stdout %q, empty stderr", fc.Stdout)
	case "error":
		return "exit 2, empty stdout, nonempty stderr"
	}
	return "exit 0 and nonempty usage mentioning name"
}

// designFacts cites the request and the committed design.
func (c *checker) designFacts(commit string) rowFacts {
	var f rowFacts
	if d, ok := c.files[1]["design.md"]; ok {
		f.see("design.md %d bytes at the designer's result", len(d))
	} else {
		f.see("design.md not observed")
	}
	if c.seed != nil {
		f.refTree(c.files[0], "FEATURE.md", c.seed.Hash.String())
	}
	f.refTree(c.files[1], "design.md", commit)
	return f
}

// coderEvidence returns the stored cases and the recomputed requirement
// map (nil when absent).
func (c *checker) coderEvidence() (*CasesRecord, *RequirementsRecord) {
	cs, rq := c.b.Cases, c.b.Requirements
	if rq == nil {
		return cs, nil
	}
	commit := ""
	if cm := c.commits[3]; cm != nil {
		commit = cm.Hash.String()
	}
	var cases []CaseResult
	if cs != nil {
		cases = cs.Cases
	}
	want := buildRequirements(c.files[4], c.b.Files["validation/tests.jsonl"], cases, rq.TestRun, commit)
	return cs, &want
}

func (c *checker) caseFacts(cs *CasesRecord, k int) rowFacts {
	var f rowFacts
	if cs == nil || k >= len(cs.Cases) {
		f.miss("validation/cases.json")
		return f
	}
	r := cs.Cases[k]
	f.refFile(c, "validation/cases.json", nil, "", cs.Commit)
	if r.ID != FeatureCases[k].ID {
		f.bad("case_record_mismatch")
		return f
	}
	if !r.Exec.Started {
		f.miss("the case's execution (the build did not succeed)")
		return f
	}
	f.see("exit %d, stdout %q, stderr %d bytes", r.Exec.ExitCode, auditText(r.Stdout, 120), len(r.Stderr))
	pass, code := evaluateCase(FeatureCases[k], r.Exec, r.Stdout, r.Stderr)
	if pass != r.Pass || code != r.Code || sha256Hex([]byte(r.Stdout)) != r.Exec.StdoutSHA256 || sha256Hex([]byte(r.Stderr)) != r.Exec.StderrSHA256 {
		f.bad("case_record_mismatch")
	}
	if !pass {
		f.bad(CodeCaseFailed)
	}
	return f
}

func (c *checker) testFacts(rq *RequirementsRecord, k int) rowFacts {
	var f rowFacts
	if rq == nil || k >= len(rq.Rows) {
		f.miss("validation/requirements.json")
		return f
	}
	r := rq.Rows[k]
	f.refFile(c, "validation/tests.jsonl", r.TestLines, "", rq.Commit)
	f.refFile(c, "validation/requirements.json", nil, "", rq.Commit)
	f.see("%s: %s", r.TestName, r.TestResult)
	if !r.MappingFound || r.TestResult != "pass" {
		f.bad(CodeTestMapping)
	}
	return f
}

func (c *checker) structureFacts(rq *RequirementsRecord, signature bool) rowFacts {
	var f rowFacts
	if rq == nil || c.files[4] == nil {
		f.miss("validation/requirements.json and the fourth result's tree")
		return f
	}
	f.refFile(c, "validation/requirements.json", nil, "", rq.Commit)
	if signature {
		f.refTree(c.files[4], "main.go", rq.Commit)
		f.see("run signature %t", rq.RunSignature)
		if !rq.RunSignature {
			f.bad(CodeTestMapping)
		}
		return f
	}
	f.refTree(c.files[4], "main_test.go", rq.Commit)
	f.see("table loop %t, calls run %t", rq.TableTests, rq.TestCallsRun)
	if !rq.TableTests || !rq.TestCallsRun {
		f.bad(CodeTestMapping)
	}
	return f
}

func (c *checker) reviewMarkerFacts() rowFacts {
	var f rowFacts
	v := c.views[3]
	if v == nil {
		f.miss("hops/04/task.json")
		return f
	}
	f.refFile(c, "hops/04/final.txt", nil, v.TaskID, "")
	r := v.Result
	switch {
	case r == nil || r.FinalMessage == nil || strings.TrimSpace(*r.FinalMessage) == "" || r.FinalMessageTruncated:
		f.bad("review_missing")
	case lastNonEmptyLine(*r.FinalMessage) != approvedMarkers[HopCodeReviewer]:
		f.bad("review_not_approved")
	default:
		f.see("review %d bytes ending %s", len(*r.FinalMessage), approvedMarkers[HopCodeReviewer])
	}
	return f
}

func (c *checker) inlineFacts() rowFacts {
	var f rowFacts
	if c.views[3] == nil || c.files[3] == nil {
		f.miss("the code reviewer's task and the coder tree")
		return f
	}
	f.classify(c.codesOf((*checker).grok, CritGrok), "review_missing", "inline_hashes_missing")
	// The marker codes belong to the review_marker row.
	f.codes = slices.DeleteFunc(f.codes, func(s string) bool { return s == "review_not_approved" })
	f.missing = slices.DeleteFunc(f.missing, func(s string) bool { return s == "review_missing" })
	if d := c.b.Hops[3].Dispatch; d != nil {
		for _, x := range d.InlineFiles {
			f.see("%s sha256 %s", auditText(x.Path, 32), auditText(x.SHA256, 64))
		}
		f.refFile(c, "hops/04/dispatch.json", nil, d.Task, "")
	}
	return f
}

func (c *checker) verdictFacts() rowFacts {
	var f rowFacts
	v, fin := c.views[3], c.b.Final
	if v == nil || v.Result == nil || v.Result.FinalMessage == nil {
		f.miss("the code review's final answer")
	}
	if fin == nil {
		f.miss("validation/final-test.json")
	}
	if len(f.missing) > 0 {
		return f
	}
	approved := finalMarker(HopCodeReviewer, *v.Result.FinalMessage) == approvedMarkers[HopCodeReviewer]
	_, rq := c.coderEvidence()
	failed := fin.ExitCode != 0 || fin.TimedOut || (rq != nil && (len(rq.FailedTests) > 0 || !rq.TestRunPassed))
	f.see("review approved %t, final tests failed %t", approved, failed)
	f.refFile(c, "validation/final-test.json", nil, "", fin.Commit)
	f.refFile(c, "hops/04/final.txt", nil, v.TaskID, "")
	if approved && failed {
		f.bad(CodeReviewContra)
	}
	return f
}

func (c *checker) cleanupFacts() rowFacts {
	f := c.criterionFacts((*checker).cleanup, CritCleanup, "cleanup_missing")
	f.observed = nil
	if cl := c.b.Cleanup; cl != nil {
		f.see("%d started process instances, %d stop records, %d tasks cancelled, %d unsettled, runtime removed %t, started %s, finished %s",
			len(cl.Started), len(cl.Processes), len(cl.CancelledTasks), len(cl.UnsettledTasks), cl.RuntimeRemoved, auditText(cl.StartedAt, 40),
			auditText(cl.FinishedAt, 40))
		f.refFile(c, "cleanup.json", nil, "", "")
	}
	f.refFile(c, coordinatorExitFile, nil, "", "")
	if slices.Contains(c.codesOf((*checker).cleanup, CritDeadlines), "cleanup_deadline_exceeded") {
		f.bad("cleanup_deadline_exceeded")
	}
	return f
}

func (c *checker) environmentFacts() rowFacts {
	f := c.criterionFacts((*checker).ownerEnvironment, CritOwnerEnv, "environment_diff_missing", CodeEnvUnobserved)
	f.observed = nil
	if d := c.b.Diff; d != nil {
		roots := "none (no repository root was protected)"
		if len(d.ProtectedRoots) > 0 {
			roots = strings.Join(d.ProtectedRoots, ", ")
		}
		f.see("protected roots: %s; %d protected changes, %d tool-maintained changes (report-only; %d detailed, %d omitted), complete %t; %d differing entries detailed in diff.json against the compact snapshots",
			auditText(roots, 1000), len(d.Protected), d.ToolDetailCount+d.ToolOmittedCount, d.ToolDetailCount, d.ToolOmittedCount, d.Complete, len(d.Detail))
		for _, ch := range d.Protected {
			f.see("protected change %s %s %s [%s]", auditText(ch.Scope, 200), auditText(ch.Path, 400), ch.Kind, strings.Join(ch.Fields, ","))
		}
		for _, row := range d.ToolSummary {
			f.see("tool-maintained %s %s: %d added, %d changed, %d deleted, %d total", auditText(row.Scope, 40), auditText(row.TopLevel, 200),
				row.Added, row.Changed, row.Deleted, row.Total)
		}
		f.see("%s", strings.Join(ClaudeStateExclusions, "; "))
		f.refFile(c, "environment/diff.json", nil, "", "")
	}
	for _, phase := range []string{"before", "after"} {
		for _, name := range envFiles(phase) {
			f.refFile(c, name, nil, "", "")
		}
	}
	return f
}

// BuildAudit derives the audit of a loaded bundle and its report (pure:
// it reads only the bundle and executes nothing).
func BuildAudit(b *Bundle, r Report) Audit {
	return buildAudit(newChecker(b), r)
}

// buildAudit is BuildAudit over a loaded checker of the bundle.
func buildAudit(c *checker, r Report) Audit {
	data, _ := encodeJSON(r)
	a := Audit{Schema: AuditSchema, RunID: r.RunID, ReportSHA256: sha256Hex(data), Scope: AuditScope, Mechanical: r.Result, ResultReview: PendingReview,
		Sources: slices.Clone(r.Artifacts), Rows: slices.Clone(auditRows(c))}
	if a.Sources == nil {
		a.Sources = []FileHash{}
	}
	return a
}

// RenderAudit is the human-readable rendering of the same rows.
func RenderAudit(a Audit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Callsheet real end-to-end audit\n\n")
	fmt.Fprintf(&b, "- run: %s\n- report sha256: %s\n- scope: %s\n- mechanical result: %s\n- result review: %s\n- source files: %d\n\n",
		a.RunID, a.ReportSHA256, a.Scope, a.Mechanical, a.ResultReview, len(a.Sources))
	b.WriteString("Judgment rows stay unobserved until the owner's external dev-flow coordinator reviews them; corrections go to an appended audit-review.md, never into this file.\n")
	for i, r := range a.Rows {
		fmt.Fprintf(&b, "\n## %d. %s (%s; %s)\n\n", i+1, r.ID, r.Layer, strings.Join(r.FPs, ", "))
		fmt.Fprintf(&b, "- Requirement: %s\n- Observed: %s\n- Verdict: %s\n", r.Requirement, r.Observed, r.Verdict)
		if r.Correction != "" {
			fmt.Fprintf(&b, "- Correction: %s\n", r.Correction)
		}
		if len(r.Criteria) > 0 {
			fmt.Fprintf(&b, "- Criteria: %s\n", strings.Join(r.Criteria, ", "))
		}
		for _, e := range r.Evidence {
			fmt.Fprintf(&b, "- Evidence: %s sha256 %s", e.File, e.SHA256)
			if len(e.Lines) == 2 {
				fmt.Fprintf(&b, " lines %d-%d", e.Lines[0], e.Lines[1])
			}
			if e.Task != "" {
				fmt.Fprintf(&b, " task %s", e.Task)
			}
			if e.Commit != "" {
				fmt.Fprintf(&b, " commit %s", e.Commit)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
