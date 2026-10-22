package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The full-flow criteria beyond the replay (design 12a-dev-flow-verify):
// provenance, roles, preflights, the coordinator session and waits,
// admissions, decisions, the final-candidate oracle, cleanup, the owner
// environment, deadlines and accounting. Each is recomputed from captured
// primary evidence; a stored derivation is only compared afterwards.

// reached reports whether any event matches pred.
func (c *devChecker) reached(pred func(Event) bool) bool { return c.seqOf(pred) > 0 }

func typeIs(t string) func(Event) bool { return func(e Event) bool { return e.Type == t } }

// checkProvenance re-derives the captured method's records and the
// rendered prompts from the captured bytes and recorded values.
func (c *devChecker) checkProvenance() {
	m := c.m
	rb := c.newRow("setup/provenance", "provenance",
		"The fifteen shared-method and exercise sources are captured byte for byte with their records; the coordinator system input, phase and scenario re-render exactly from them; the installed manuals and the generated FEATURE.md are those captured bytes; the flow label, flow and selection digests agree",
		groupSetup)
	defer rb.done()
	srcs := doc[devEnvelope[[]SourceRecord]](c.b, "setup/sources.json")
	if srcs == nil {
		rb.miss("sources_missing", c.reached(typeIs(EvVersionsChecked)))
		return
	}
	rb.ref("setup/sources.json", nil, "", "")
	names := DevMethodNames()
	loaded := DevLoaded{Files: map[string][]byte{}}
	var want []SourceRecord
	for _, name := range names {
		rel := devSourceRel(name)
		data, ok := c.b.Files["setup/sources/"+rel]
		if !ok {
			rb.bad("source_missing")
			continue
		}
		loaded.Files[name] = data
		want = append(want, SourceRecord{Path: rel, Bytes: len(data), SHA256: sha256Hex(data)})
	}
	if len(names) != 15 || !bytes.Equal(mustJSON(want), mustJSON(srcs.Data)) {
		rb.bad("source_inventory_mismatch")
	}
	if ins, run, err := loaded.ManualPair("triage"); err != nil || checkTriageManualBudget(ins, run) != nil {
		rb.bad(CodePromptBudget)
	}
	flowRaw := c.b.Files["setup/flow.json"]
	label := doc[DevFlowLabel](c.b, "setup/flow-label.json")
	sel, serr := c.flow.SelectionCanonical()
	def, _ := DefaultDevFullFlow().SelectionCanonical()
	wantLabel := FlowOverride
	if serr == nil && bytes.Equal(sel, def) {
		wantLabel = FlowDefault
	}
	switch {
	case label == nil || serr != nil:
		rb.bad("flow_label_missing")
	case label.Label != m.FlowLabel || label.Label != wantLabel || label.FlowSHA256 != sha256Hex(flowRaw) || label.SelectionSHA256 != sha256Hex(sel) ||
		label.Selection != string(sel) || m.FlowSHA256 != label.FlowSHA256 || m.SelectionSHA256 != label.SelectionSHA256:
		rb.bad("flow_label_mismatch")
	}
	feature := doc[DevFeatureRecord](c.b, "setup/feature-request.json")
	gen, _ := devFullFeatureRequest()
	if feature == nil || feature.OriginalSHA256 != sha256Hex([]byte(FeatureRequest)) || feature.GeneratedSHA256 != sha256Hex([]byte(gen)) {
		rb.bad("feature_request_mismatch")
	}
	if seed, ok := c.trees[m.Workspace.SeedCommit]; ok && string(fileContent(seed, "FEATURE.md")) != gen {
		rb.bad("feature_request_mismatch")
	}
	installed := doc[devEnvelope[[]SourceRecord]](c.b, "setup/installed-manuals.json")
	if installed != nil || c.reached(typeIs(EvRolesRegistered)) {
		var wantInst []SourceRecord
		for _, role := range DevFullRoleIDs {
			for _, kind := range []string{"instruction", "runbook"} {
				name := manualName(role, kind)
				wantInst = append(wantInst, SourceRecord{Path: "manuals/" + name, Bytes: len(loaded.Files[name]), SHA256: sha256Hex(loaded.Files[name])})
			}
		}
		if installed == nil || !bytes.Equal(mustJSON(installed.Data), mustJSON(wantInst)) {
			rb.bad("installed_manuals_mismatch")
		}
	}
	rendered := doc[devEnvelope[[]RenderedRecord]](c.b, "setup/rendered.json")
	if rendered == nil {
		rb.miss("rendered_missing", c.reached(typeIs(EvCoordinatorReady)))
		return
	}
	out, err := RenderDevPrompts(loaded, m.Render)
	switch {
	case err != nil:
		rb.bad("render_values_invalid")
	case !bytes.Equal(mustJSON(out.Records), mustJSON(rendered.Data)) || !bytes.Equal(out.Rendered[DevSystemName], c.b.Files["setup/coordinator-system.md"]) ||
		!bytes.Equal(out.Rendered[DevPhaseFile], c.b.Files["setup/phase.md"]) || !bytes.Equal(out.Rendered[DevScenario], c.b.Files["setup/scenario.md"]):
		rb.bad("rendered_mismatch")
	}
	if !mcpConfigOK(c.b.Files["setup/mcp-config.json"]) {
		rb.bad("mcp_config_invalid")
	}
	for _, f := range []string{"setup/rendered.json", "setup/coordinator-system.md", "setup/phase.md", "setup/scenario.md", "setup/flow.json", "setup/flow-label.json"} {
		rb.ref(f, nil, "", "")
	}
	rb.see("%d sources, label %s", len(srcs.Data), m.FlowLabel)
}

// mcpConfigOK reports whether data is exactly the session-local MCP
// configuration the renderer writes for its own command, plane URL and CA
// (byte for byte: no extra server, field or argument survives).
func mcpConfigOK(data []byte) bool {
	var cfg struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return false
	}
	srv, ok := cfg.Servers[mcpServerName]
	if !ok || len(srv.Args) != 7 {
		return false
	}
	want, err := mcpConfig(srv.Command, srv.Args[2], srv.Args[4])
	return err == nil && bytes.Equal(want, data)
}

// checkRoles requires the five frozen roles registered and ready.
func (c *devChecker) checkRoles() {
	rb := c.newRow("setup/roles", "roles",
		"The five roles were registered with the frozen flow's adapter, model and effort (concurrency 1, timeout 10m) on one node per adapter, the sidecars reconnected and every role was ready before any task", groupSetup)
	defer rb.done()
	r := doc[RolesRecord](c.b, "setup/roles.json")
	if r == nil {
		rb.miss("roles_missing", c.reached(typeIs(EvRolesReady)))
		return
	}
	rb.ref("setup/roles.json", nil, "", "")
	if !c.flowOK || len(r.Roles) != len(c.flow.Roles) {
		rb.bad("roles_mismatch")
		return
	}
	nodes := map[string]string{}
	for i, s := range r.Roles {
		w := c.flow.Roles[i]
		if s.ID != w.ID || s.Adapter != w.Adapter || s.Model != w.Model || s.Effort != w.Effort || s.Concurrency != RoleConcurrency ||
			s.Timeout != FeatureTaskBound.String() || !contract.ValidNodeID(s.Node) {
			rb.bad("roles_mismatch")
		}
		if !s.CanAccept {
			rb.bad("role_not_ready")
		}
		if n, ok := nodes[s.Adapter]; ok && n != s.Node {
			rb.bad("adapter_nodes_inconsistent")
		}
		nodes[s.Adapter] = s.Node
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n] {
			rb.bad("adapter_nodes_inconsistent")
		}
		seen[n] = true
	}
	for _, typ := range []string{EvRolesRegistered, EvSidecarsReconnected, EvRolesReady} {
		if len(c.eventsOf(typ)) != 1 {
			rb.bad("required_event_missing")
		}
	}
	if e, ok := c.event(typeIs(EvRolesReady)); !ok || e.Time != r.ReadyAt {
		rb.bad("ready_at_mismatch")
	}
	rb.see("%d roles ready at %s", len(r.Roles), auditText(r.ReadyAt, 40))
}

// checkPreflight requires the five individually recorded authentication
// preflights before any workflow admission.
func (c *devChecker) checkPreflight() {
	firstWorkflow := c.seqOf(func(e Event) bool { return e.Type == EvTaskAdmitted && strings.HasPrefix(e.Hop, "workflow-") })
	ids := map[string]bool{}
	for i := range len(DevFullRoleIDs) {
		label := preflightHop(i)
		dir := "preflight/" + hopDir(i)
		adm := c.seqOf(func(e Event) bool { return e.Type == EvTaskAdmitted && e.Hop == label })
		rb := c.newRow(dir+"/selection", "preflight",
			fmt.Sprintf("Authentication preflight %d ran no-workspace on %s's exact effective pair with no override and gave a complete, nonempty answer within 2m, before any workflow task (the token echo is information only)", i+1, DevFullRoleIDs[i]),
			groupSetup)
		v := doc[contract.TaskShowResponse](c.b, dir+"/task.json")
		obs := doc[devEnvelope[DevPreflightRecord]](c.b, dir+"/observation.json")
		if v == nil || obs == nil {
			rb.miss("preflight_missing", adm > 0)
			rb.done()
			continue
		}
		rb.r.Task = v.Task.TaskID
		rb.ref(dir+"/task.json", nil, v.Task.TaskID, "")
		rb.ref(dir+"/observation.json", nil, v.Task.TaskID, "")
		role := FlowRole{}
		if c.flowOK {
			role = c.flow.Roles[i]
		}
		if err := preflightVerdict(label, role, v.Task); err != nil {
			rb.bad(codeOf(err))
		}
		if len(v.Task.Request.Payload) != 1 || v.Task.Request.Payload[0] != markerFor(c.runID, label) || v.Task.Request.Goal != preflightGoal {
			rb.bad("preflight_binding_mismatch")
		}
		want := DevPreflightRecord{Label: label, Role: role.ID, Task: v.Task.TaskID, Adapter: v.Task.Role.Adapter, Model: v.Task.Effective.Model,
			Effort: v.Task.Effective.Effort, State: v.Task.State}
		if r := v.Task.Result; r != nil && r.FinalMessage != nil && !r.FinalMessageTruncated {
			want.Answered, want.TokenEchoed = authAnswerPresent(*r.FinalMessage), authAnswerOK(*r.FinalMessage)
			if got, ok := c.b.Files[dir+"/final.txt"]; !ok || string(got) != *r.FinalMessage {
				rb.bad("final_text_mismatch")
			}
		}
		if obs.Data != want {
			rb.bad("preflight_record_mismatch")
		}
		if d, ok := duration(v.Task); !ok || d >= PreflightBound {
			rb.bad("preflight_deadline_exceeded")
		}
		if adm == 0 || (firstWorkflow > 0 && adm > firstWorkflow) {
			rb.bad("preflight_out_of_order")
		}
		if ids[v.Task.TaskID] {
			rb.bad("task_not_unique")
		}
		ids[v.Task.TaskID] = true
		rb.see("task %s on %s/%s/%s, answered %t, token_echoed %t (information only)", auditText(v.Task.TaskID, 64), auditText(want.Adapter, 16),
			auditText(want.Model, 64), auditText(want.Effort, 16), want.Answered, want.TokenEchoed)
		rb.done()
	}
}

// checkSession judges every phase's launch, init and exit, and every
// reached task's wait, from the re-derived session map (the stored map is
// compared only afterwards).
func (c *devChecker) checkSession() {
	m := c.m
	phases := c.phaseNumbers()
	stored := doc[DevEventMap](c.b, devEventMapFile)
	rb := c.newRow("setup/session", "waits",
		"The stored session event map is exactly the re-derivation from the raw phase streams, the observed tasks, their plans and the exact wait commands", groupSetup)
	if len(phases) > 0 {
		if stored == nil {
			rb.miss("event_map_missing", true)
		} else if !bytes.Equal(mustJSON(*stored), mustJSON(c.eventMap)) {
			rb.bad("event_map_mismatch")
		}
	}
	rb.ref(devEventMapFile, nil, "", "")
	rb.done()
	exits := map[int]PhaseProcess{}
	if ce := doc[CoordinatorExit](c.b, coordinatorExitFile); ce != nil {
		for _, p := range ce.Phases {
			exits[p.Phase] = p
		}
		if len(ce.Phases) != len(phases) {
			c.fail("coordinator_setup", "coordinator_exit_inventory")
		}
	} else if len(phases) > 0 {
		c.fail("coordinator_setup", "coordinator_exit_missing")
	}
	gates := c.expectedGates()
	// Every recorded Callsheet MCP call against the captured authority
	// (design 12a-dev-flow-verify r0.6, Recorded Callsheet MCP
	// verification boundary): an offline condition on the completed
	// evidence, reported in its phase's init row.
	mcp := devMCPProblems(c.b, m, c.stop != nil)
	for _, p := range phases {
		handle := phaseHandle(p)
		start, _ := c.event(func(e Event) bool { return e.Type == EvPhaseStarted && e.Handle == handle })
		exitEv, exited := c.event(func(e Event) bool { return e.Type == EvPhaseExited && e.Handle == handle })
		launch := doc[LaunchRecord](c.b, fmt.Sprintf("setup/launch-phase-%d.json", p))
		lrb := c.newRow(fmt.Sprintf("phase/%d/launch", p), "coordinator_setup",
			fmt.Sprintf("Phase %d was launched headless with exactly the session-only auto-mode recipe (stream-json, sandbox-off settings, --permission-mode auto, --permission-prompts none, the runtime MCP configuration, the rendered prompts by hash; a later phase resumes the captured session)", p),
			groupEvents).at(start.Seq)
		if launch == nil {
			lrb.miss("launch_missing", start.Seq > 0)
		} else {
			lrb.ref(fmt.Sprintf("setup/launch-phase-%d.json", p), nil, "", "")
			if code := c.launchShape(p, launch, start.Seq); code != "" {
				lrb.bad(code)
			}
		}
		lrb.done()
		var facts *DevPhaseFacts
		for i := range c.eventMap.Phases {
			if c.eventMap.Phases[i].Phase == p {
				facts = &c.eventMap.Phases[i]
			}
		}
		irb := c.newRow(fmt.Sprintf("phase/%d/init", p), "coordinator_setup",
			fmt.Sprintf("Every init record of phase %d reports auto and keeps the session; the stream is well formed, without a denial, a failed vendor tool, a long poll or a session change; every Callsheet call is a permitted, paired, valid call bound to its request and the captured authority", p),
			groupEvents).at(start.Seq)
		switch {
		case facts == nil || len(facts.Inits) == 0:
			irb.miss("session_unobserved", exited)
		case facts.Problem != "":
			irb.bad(facts.Problem)
		case facts.SessionID != c.eventMap.SessionID || !sessionIDRE.MatchString(facts.SessionID):
			irb.bad(devSessionMismatch)
		case mcp[p] != "":
			irb.bad(mcp[p])
		}
		if facts != nil {
			irb.refs(facts.Inits, "")
			if facts.ProblemSource != nil {
				irb.refs([]SourceRef{*facts.ProblemSource}, "")
			}
			irb.see("session %s, %d init records", auditText(facts.SessionID, 40), len(facts.Inits))
		}
		irb.done()
		erb := c.newRow(fmt.Sprintf("phase/%d/exit", p), "coordinator_setup",
			fmt.Sprintf("Phase %d exited on its own with exit 0, its leader reaped and its whole group gone, and a successful result ending at its gate line (or after completion)", p),
			groupEvents).at(exitEv.Seq)
		x, has := exits[p]
		switch {
		case !has || !exited:
			erb.miss("coordinator_exit_unobserved", exited)
		case !x.Reaped || !x.GroupGone || x.StartedSeq != start.Seq || x.ExitedSeq != exitEv.Seq:
			erb.bad("coordinator_group_alive")
		case x.StoppedBy != StoppedExited:
			// Stopped by the cleanup after the attempt's stop: its own exit
			// was never reached.
			erb.miss("coordinator_exit_unobserved", c.stop == nil || c.stop.seq > exitEv.Seq)
		case x.ExitCode == nil || *x.ExitCode != 0 || x.Signal != "":
			erb.bad("coordinator_exit_nonzero")
		case facts == nil || !facts.ResultOK || facts.Result == nil:
			erb.bad(devResultFailed)
		default:
			c.judgeGate(erb, p, facts, gates)
		}
		erb.ref(coordinatorExitFile, nil, "", "")
		if facts != nil && facts.Result != nil {
			erb.refs([]SourceRef{*facts.Result}, "")
		}
		erb.done()
	}
	if len(phases) > maxPhases {
		c.fail("coordinator_setup", "phase_cap")
	}
	if len(phases) == 0 {
		c.absent("coordinator_setup", "launch_missing", c.reached(typeIs(EvPhaseStarted)))
	}
	c.checkWaits()
	_ = m
}

// expectedGates are the replayed pauses in order (each phase but the
// final one ends at one).
func (c *devChecker) expectedGates() []devGate {
	var out []devGate
	for k := 1; ; k++ {
		d := doc[devEnvelope[DevDecision]](c.b, fmt.Sprintf("decisions/%02d.json", k))
		if d == nil {
			break
		}
		out = append(out, devGate{kind: d.Data.Window, gen: d.Data.Generation})
	}
	if c.rp != nil && c.rp.pausedAt != nil && (len(out) == 0 || out[len(out)-1] != *c.rp.pausedAt) {
		out = append(out, *c.rp.pausedAt)
	}
	return out
}

// judgeGate requires phase p's result to be its gate line (or, for the
// phase after the last gate, completion).
func (c *devChecker) judgeGate(rb *rowBuilder, p int, facts *DevPhaseFacts, gates []devGate) {
	if p <= len(gates) {
		g := gates[p-1]
		kind, gen, err := parsePhaseGate(facts.ResultText)
		if err != nil || kind != g.kind || gen != g.gen {
			rb.bad("phase_result")
			return
		}
		rb.see("ended at gate=%s generation=%d", kind, gen)
		return
	}
	if c.rp == nil || !c.rp.complete {
		rb.bad("coordinator_exited")
		return
	}
	rb.see("ended after the approving review")
}

// launchShape checks one sanitized launch record against the only
// permitted recipe.
func (c *devChecker) launchShape(p int, l *LaunchRecord, startSeq int) string {
	system, phase := c.b.Files["setup/coordinator-system.md"], c.b.Files["setup/phase.md"]
	want := []string{"-p", "<phase prompt sha256=" + sha256Hex(phase) + ">", "--output-format", "stream-json", "--verbose", "--settings", SandboxOffSettings,
		"--permission-mode", autoModeFlag, "--permission-prompts", autoPrompts, "--mcp-config", "$RUN/mcp.json", "--append-system-prompt",
		"<coordinator instructions sha256=" + sha256Hex(system) + ">"}
	if p > 1 {
		want = append(want, "--resume", c.eventMap.SessionID)
	}
	switch {
	case l.Phase != p || l.StartedSeq != startSeq || startSeq == 0:
		return "launch_binding_mismatch"
	case !slices.Equal(l.Argv, want) || len(l.Display) != len(want)+1 || !slices.Equal(l.Display[1:], want):
		return "launch_shape_invalid"
	case l.PromptSHA256 != sha256Hex(phase) || l.SystemSHA256 != sha256Hex(system):
		return "launch_shape_invalid"
	case l.Cwd != "$RUN/coord" || len(l.WaitTemplates) != 1 || !strings.HasSuffix(l.WaitTemplates[0], " dev-wait-task --run $RUN --task TASK_ID --ordinal 0"):
		return "launch_shape_invalid"
	case p == 1 && l.ResumeSession != "", p > 1 && l.ResumeSession != c.eventMap.SessionID:
		return "session_resume_mismatch"
	}
	return ""
}

// checkWaits judges every observed task's single qualified wait.
func (c *devChecker) checkWaits() {
	for i, t := range c.eventMap.Tasks {
		n := t.Ordinal
		dir := taskDir(n)
		ev := c.eventsOfTask(n, t.Task)
		var plan TaskPlan
		if p := doc[devEnvelope[TaskPlan]](c.b, dir+"/plan.json"); p != nil {
			plan = p.Data
		}
		rb := c.newRow(fmt.Sprintf("task/%02d/wait", n), "waits",
			fmt.Sprintf("Task %d's one exact background wait command started, its handle's own completion notification arrived automatically, one task_show of the task consumed it with no ordinary user input between, the captured wait exited 0 with the task's terminal result, and the consumption preceded the successor", n),
			groupEvents).task(n, t.Task, plan.Role, plan.Mode, plan.Round, plan.ExpectedBase, "").at(cmpOrInt(ev.waitStart, ev.observed))
		rb.refs(t.WaitSources, t.Task)
		if t.ProblemSource != nil {
			// The refused notification record (design 12a-dev-flow-verify
			// r0.8).
			rb.refs([]SourceRef{*t.ProblemSource}, t.Task)
		}
		problem := t.Problem
		if problem == devDispatchUnobserved || problem == devDispatchMismatch || problem == devDispatchOrder {
			problem = ""
		}
		wait := doc[devEnvelope[DevWaitRecord]](c.b, dir+"/wait.json")
		switch {
		case problem != "":
			rb.bad(problem)
		case !t.Wait.complete():
			rb.miss(CodeSessionUnobserved, ev.consumed > 0)
		case wait == nil:
			rb.miss("wait_record_missing", ev.consumed > 0)
		default:
			files := devWaitFiles{}
			files.Result, files.OKR = c.b.Files[dir+"/"+waitResultFile]
			files.Stderr, files.OKE = c.b.Files[dir+"/"+waitStderrFile]
			files.Exit, files.OKX = c.b.Files[dir+"/"+waitExitFile]
			w := wait.Data
			want := DevWaitRecord{Ordinal: n, Task: t.Task, Command: w.Command, StartSeq: ev.waitStart, ConsumedSeq: ev.consumed, Wait: t.Wait}
			captured := files.captured(t.Task, &want)
			if !strings.HasSuffix(w.Command, " dev-wait-task --run $RUN --task "+t.Task+" --ordinal "+itoa(n)) {
				rb.bad(devWaitMismatch)
			}
			if !bytes.Equal(mustJSON(w), mustJSON(want)) {
				rb.bad("wait_record_mismatch")
			}
			if !captured {
				rb.bad(devWaitFailed)
			}
			if len(c.eventsOfType(EvTaskAdmitted, workflowLabel(n), "wait_started")) != 1 {
				rb.bad(devDuplicateWait)
			}
			rb.ref(dir+"/wait.json", nil, t.Task, "")
			rb.ref(dir+"/"+waitResultFile, nil, t.Task, "")
			rb.see("handle %s started by %s, consumed by %s at seq %d", auditText(t.Wait.Handle, 32), auditText(t.Wait.StartUse, 64),
				auditText(t.Wait.ConsumeUse, 64), ev.consumed)
		}
		// The consumption precedes the successor's admission and any later
		// prompt.
		if i+1 < len(c.eventMap.Tasks) {
			nx := c.eventsOfTask(c.eventMap.Tasks[i+1].Ordinal, c.eventMap.Tasks[i+1].Task)
			if ev.consumed == 0 || (nx.admitted > 0 && nx.admitted < ev.consumed) {
				rb.bad("successor_before_consumption")
			}
		}
		rb.done()
	}
}

// preflightDirs and workflowDirs are the present task directories.
func (c *devChecker) preflightDirs() []string {
	var out []string
	for _, n := range c.b.ordinalsOf("preflight") {
		out = append(out, "preflight/"+hopDir(n-1))
	}
	return out
}

func (c *devChecker) workflowDirs() []string {
	var out []string
	for _, n := range c.b.ordinalsOf("tasks") {
		out = append(out, taskDir(n))
	}
	return out
}

// eventsOfType are the events of type typ on hop with code.
func (c *devChecker) eventsOfType(typ, hop, code string) []Event {
	var out []Event
	for _, e := range c.b.Events {
		if e.Type == typ && e.Hop == hop && e.Code == code {
			out = append(out, e)
		}
	}
	return out
}

// checkAdmissions requires every listed task accounted for: the sweeps'
// union is exactly the preflights, the workflow admissions and the
// rejected or late admissions, and any extra admission fails.
func (c *devChecker) checkAdmissions() {
	rb := c.newRow("setup/admissions", "admissions",
		"Every complete ListTasks sweep (page boundaries, IDs and request identities) lists only the five preflights, the workflow admissions (each ordinal admitted once, contiguous, unique) and accounted rejected or late admissions; no extra admission exists", groupSetup)
	defer rb.done()
	a := doc[DevAdmissions](c.b, devAdmissionsFile)
	if a == nil {
		rb.miss("admissions_missing", c.reached(typeIs(EvCollectionStarted)))
		return
	}
	rb.ref(devAdmissionsFile, nil, "", "")
	expected := map[string]bool{}
	byHop := map[string]int{}
	for _, e := range c.b.Events {
		switch {
		case e.Type == EvTaskAdmitted && e.Code == "":
			expected[e.Task] = true
			byHop[e.Hop]++
		case e.Type == EvExtraAdmission:
			expected[e.Task] = true
			rb.bad("extra_admission")
		}
	}
	for hop, n := range byHop {
		if n != 1 {
			rb.bad("duplicate_admission")
			rb.see("%s admitted %d times", hop, n)
		}
	}
	// One task identity per slot, never reused across slots.
	seen := map[string]bool{}
	for _, dir := range append(c.preflightDirs(), c.workflowDirs()...) {
		if v := doc[contract.TaskShowResponse](c.b, dir+"/task.json"); v != nil {
			if seen[v.Task.TaskID] {
				rb.bad("task_not_unique")
			}
			seen[v.Task.TaskID] = true
		}
	}
	// Every rejected or late admission has its own row.
	for _, e := range c.eventsOf(EvExtraAdmission) {
		arb := c.newRow("admission/"+e.Task, "admissions", "A task admitted on the private plane that is not the pending plan's single admission is cancelled, accounted and fails the attempt", groupEvents).at(e.Seq)
		arb.r.Task = e.Task
		arb.bad("extra_admission")
		arb.see("rejected as %s at seq %d", auditText(e.Code, 40), e.Seq)
		arb.done()
	}
	// The captured request identity of every task with an authoritative
	// record is that record's; any other listed task keeps one identity
	// across every sweep.
	identity := map[string]DevSweepTask{}
	for _, dir := range append(c.preflightDirs(), c.workflowDirs()...) {
		if v := doc[contract.TaskShowResponse](c.b, dir+"/task.json"); v != nil {
			identity[v.Task.TaskID] = DevSweepTask{ID: v.Task.TaskID, Role: v.Task.Role.ID, CreatedAt: v.Task.CreatedAt,
				RequestedBy: v.Task.Request.RequestedBy.Name}
		}
	}
	listed := map[string]bool{}
	final := false
	for _, s := range a.Sweeps {
		if s.Count < 1 || len(s.Pages) == 0 && s.Error == "" {
			rb.bad("sweep_invalid")
		}
		after := ""
		ids := map[string]bool{}
		for i, p := range s.Pages {
			if p.After != after {
				rb.bad("sweep_pages_inconsistent")
			}
			after = p.Next
			if p.Next == "" && i != len(s.Pages)-1 {
				rb.bad("sweep_pages_inconsistent")
			}
			for _, id := range p.IDs {
				if ids[id] {
					rb.bad("sweep_tasks_inconsistent")
				}
				ids[id], listed[id] = true, true
			}
		}
		if s.Error == "" && after != "" {
			rb.bad("sweep_incomplete")
		}
		// Exactly the listed tasks, once each, sorted, with their captured
		// identities.
		var taskIDs []string
		for _, t := range s.Tasks {
			if !ids[t.ID] || len(taskIDs) > 0 && taskIDs[len(taskIDs)-1] == t.ID {
				rb.bad("sweep_tasks_inconsistent")
			}
			taskIDs = append(taskIDs, t.ID)
			if want, ok := identity[t.ID]; !ok {
				identity[t.ID] = t
			} else if want != t {
				rb.bad("sweep_identity_mismatch")
			}
		}
		if len(taskIDs) != len(ids) || !slices.IsSorted(taskIDs) {
			rb.bad("sweep_tasks_inconsistent")
		}
		if s.Kind == sweepFinal {
			final = final || s.Error == ""
		}
	}
	for id := range listed {
		if !expected[id] {
			rb.bad("unaccounted_admission")
		}
	}
	if final {
		for id := range expected {
			if !listed[id] {
				rb.bad("admission_unlisted")
			}
		}
	} else {
		rb.miss("final_sweep_missing", c.reached(typeIs(EvCollectionStarted)) && c.b.Files[devCleanupFile] != nil)
	}
	rb.see("%d sweep records, %d listed tasks", len(a.Sweeps), len(listed))
}

// checkReceipts requires every decision and receipt file to be one the
// replay consumed.
func (c *devChecker) checkReceipts() {
	used := 0
	for _, r := range c.rows {
		if strings.HasPrefix(r.ID, "decision/") {
			used++
		}
	}
	for _, n := range c.b.ordinalsOf("decisions") {
		if n > used {
			c.fail("owner_receipts", "unconsumed_decision")
		}
	}
	gens := map[int]bool{}
	for _, r := range c.rows {
		var g int
		if _, err := fmt.Sscanf(r.ID, "receipt/%02d/authorization", &g); err == nil {
			gens[g] = true
		}
	}
	for _, n := range c.b.ordinalsOf("receipts") {
		if !gens[n] {
			c.fail("owner_receipts", "unconsumed_receipt")
		}
	}
}

// checkValidation judges the baseline and the final candidate's oracle.
func (c *devChecker) checkValidation() {
	seedCommit := c.m.Workspace.SeedCommit
	seed := c.trees[seedCommit]
	brb := c.newRow("validation/baseline", "baseline", "The seed's baseline go test -buildvcs=false -count=1 ./... passed in the isolated offline environment on an export of exactly the seed", groupTail)
	// Every validation ran the pinned Go the startup observed.
	pinned := ""
	for _, v := range c.m.Versions {
		if v.Tool == "go" {
			pinned = v.Version
		}
	}
	if t := doc[TestRun](c.b, "validation/baseline.json"); t == nil {
		brb.miss("baseline_missing", c.reached(typeIs(EvBaselineTests)))
	} else {
		if code := testRunCode(t, seedCommit, c.m.Workspace.SeedTree, seed); code != "" {
			brb.bad(code)
		}
		if t.Toolchain != pinned {
			brb.bad("toolchain_mismatch")
		}
		brb.ref("validation/baseline.json", nil, "", seedCommit)
	}
	brb.done()
	rp := c.rp
	reached := c.reached(typeIs(EvValidationStarted)) || (rp != nil && rp.complete && c.stop == nil)
	cand := ""
	if rp != nil {
		cand = rp.state.CandidateCommit
	}
	files := map[string][]byte{}
	for p, f := range c.trees[cand] {
		files[p] = f.Content
	}
	tree := c.treeHash[cand]
	trb := c.newRow("validation/tests", "final_tests",
		"The final candidate (the replay's last implementation-ready commit, not the review publication) passed go test, the build and go test -json, each bound to its commit and tree within one shared 60s deadline inside the attempt",
		groupTail)
	ft := doc[TestRun](c.b, "validation/final-test.json")
	cs := doc[CasesRecord](c.b, "validation/cases.json")
	rq := doc[RequirementsRecord](c.b, "validation/requirements.json")
	cov := doc[DevCoverageRecord](c.b, devCoverageFile)
	if ft == nil || cs == nil || rq == nil || cov == nil || !(rp != nil && rp.complete) {
		trb.miss("final_tests_missing", reached)
		trb.done()
		c.absent("coverage", "coverage_missing", reached)
		c.absent("feature_oracle", "oracle_missing", reached)
		return
	}
	trb.r.ResultCommit = cand
	if code := testRunCode(ft, cand, tree, c.trees[cand]); code != "" {
		trb.bad(code)
	}
	if ft.Toolchain != pinned {
		trb.bad("toolchain_mismatch")
	}
	execs := append([]ExecRecord{cs.Build}, rq.TestRun, cov.Exec)
	for _, r := range cs.Cases {
		if r.Exec.Started {
			execs = append(execs, r.Exec)
		}
	}
	if code := c.execBinding(execs, cand, tree); code != "" {
		trb.bad(code)
	}
	if passed, _ := testRunVerdict(rq.TestRun, c.b.Files["validation/tests.jsonl"]); !passed || rq.TestRun.StdoutSHA256 != sha256Hex(c.b.Files["validation/tests.jsonl"]) {
		trb.bad(CodeValidationFail)
	}
	// The build ran exactly its fixed command to a clean, complete end.
	if bd := cs.Build; bd.Name != "go_build" || !slices.Equal(bd.Argv, validationArgvBuild) || !bd.Started || bd.ExitCode != 0 || bd.Signal != "" ||
		bd.TimedOut || bd.Truncated {
		trb.bad("build_failed")
	}
	// Every output of the reached validation exists, and the captured
	// outputs are the ones the records hash.
	for _, f := range finalValidationOutputs {
		if _, ok := c.b.Files[f]; !ok {
			trb.bad("validation_file_missing")
		}
	}
	if cs.Build.StdoutSHA256 != sha256Hex(c.b.Files[buildStdoutFile]) || cs.Build.StderrSHA256 != sha256Hex(c.b.Files[buildStderrFile]) ||
		cov.Exec.StdoutSHA256 != sha256Hex(c.b.Files["validation/coverage-stdout.txt"]) || cov.Exec.StderrSHA256 != sha256Hex(c.b.Files["validation/coverage-stderr.txt"]) ||
		rq.TestRun.StderrSHA256 != sha256Hex(c.b.Files["validation/test-json-stderr.txt"]) ||
		cov.FinalStdout != sha256Hex(c.b.Files["validation/final-stdout.txt"]) || cov.FinalStderr != sha256Hex(c.b.Files["validation/final-stderr.txt"]) {
		trb.bad("validation_output_mismatch")
	}
	for _, f := range []string{"validation/final-test.json", "validation/cases.json", "validation/requirements.json", "validation/tests.jsonl"} {
		trb.ref(f, nil, "", cand)
	}
	trb.done()
	oracleOK := len(trb.r.Codes) == 0
	// The requirement map and source structure, recomputed from the
	// candidate tree and the captured stream.
	want := buildRequirements(files, c.b.Files["validation/tests.jsonl"], cs.Cases, rq.TestRun, cand)
	srb := c.newRow("validation/signature", "feature_oracle", "main.go declares run(args []string, stdout, stderr io.Writer) int", groupTail)
	srb.ref("validation/requirements.json", nil, "", cand)
	if !want.RunSignature {
		srb.bad(CodeTestMapping)
	}
	if !requirementsEqual(want, *rq) {
		srb.bad("requirements_mismatch")
	}
	srb.done()
	tbl := c.newRow("validation/table", "feature_oracle", "main_test.go has TestRun with a table loop that calls run, and every TestRun/<case-id> passed in the complete go test -json stream with nothing failing or skipped", groupTail)
	tbl.ref("validation/tests.jsonl", nil, "", cand)
	if !want.TableTests || !want.TestCallsRun || len(want.MissingTests) > 0 || len(want.FailedTests) > 0 || !want.TestRunPassed {
		tbl.bad(CodeTestMapping)
	}
	tbl.done()
	oracleOK = oracleOK && len(srb.r.Codes) == 0 && len(tbl.r.Codes) == 0
	if len(cs.Cases) != len(FeatureCases) || cs.Commit != cand || cs.Tree != tree {
		c.fail("feature_oracle", CodeCaseFailed)
		oracleOK = false
	}
	for i, fc := range FeatureCases {
		crb := c.newRow("case/"+fc.ID, "feature_oracle", fmt.Sprintf("The built CLI run with argv %q gives %s", fc.Args, caseExpectation(fc)), groupTail)
		crb.ref("validation/cases.json", nil, "", cand)
		if i >= len(cs.Cases) || cs.Cases[i].ID != fc.ID {
			crb.bad(CodeCaseFailed)
			crb.done()
			oracleOK = false
			continue
		}
		r := cs.Cases[i]
		pass, code := evaluateCase(fc, r.Exec, r.Stdout, r.Stderr)
		if pass != r.Pass || code != r.Code || !slices.Equal(r.Exec.Argv, append([]string{"greeting"}, fc.Args...)) || r.Exec.Name != "case_"+fc.ID ||
			(r.Exec.Started && (sha256Hex([]byte(r.Stdout)) != r.Exec.StdoutSHA256 || sha256Hex([]byte(r.Stderr)) != r.Exec.StderrSHA256)) {
			crb.bad("case_record_mismatch")
		}
		if !pass {
			crb.bad(CodeCaseFailed)
		}
		oracleOK = oracleOK && len(crb.r.Codes) == 0
		crb.see("exit %d, stdout %q, stderr %d bytes", r.Exec.ExitCode, auditText(r.Stdout, 80), len(r.Stderr))
		crb.done()
	}
	vrb := c.newRow("validation/coverage", "coverage",
		"The final candidate's changed-code coverage, recomputed from its own mode: set profile against the seed main.go, is above 80% (integer arithmetic, nonzero denominator)", groupTail)
	vrb.ref(devCoverageFile, nil, "", cand)
	vrb.ref("validation/coverage.out", nil, "", cand)
	profile := c.b.Files["validation/coverage.out"]
	got, err := deriveChangedCoverage(fileContent(c.trees[seedCommit], "main.go"), files["main.go"], profile, modulePathOf(files["go.mod"]))
	switch {
	case cov.Commit != cand || cov.Tree != tree || cov.Exec.Name != "go_test_cover" || !slices.Equal(cov.Exec.Argv, validationArgvCover) ||
		cov.Profile != sha256Hex(profile):
		vrb.bad("coverage_binding_mismatch")
	case !cov.Exec.Started || cov.Exec.ExitCode != 0 || cov.Exec.TimedOut || cov.Exec.Signal != "":
		vrb.bad(CodeCoverageUnobserved)
	case err != nil:
		vrb.bad(CodeCoverageUnobserved)
	case !bytes.Equal(mustJSON(&got), mustJSON(cov.Coverage)):
		vrb.bad("coverage_record_mismatch")
	case !got.Pass:
		vrb.bad(CodeCoverageBelow)
	default:
		vrb.see("changed-code coverage %d/%d (%s%%) over %v", got.Covered, got.Total, got.Percent, got.Functions)
	}
	vrb.done()
	oracleOK = oracleOK && len(vrb.r.Codes) == 0
	if !oracleOK {
		// An approving review alongside a failing final oracle.
		c.fail("feature_oracle", CodeReviewContra)
	}
}

// finalValidationOutputs are the files a reached final validation always
// writes (the coverage profile is judged by the coverage row).
var finalValidationOutputs = []string{"validation/final-test.json", "validation/final-stdout.txt", "validation/final-stderr.txt", buildStdoutFile,
	buildStderrFile, "validation/cases.json", "validation/tests.jsonl", "validation/test-json-stderr.txt", "validation/requirements.json",
	devCoverageFile, "validation/coverage-stdout.txt", "validation/coverage-stderr.txt"}

// testRunCode judges one go test record bound to commit and tree.
func testRunCode(t *TestRun, commit, tree string, files map[string]TreeFile) string {
	envOK := true
	for _, e := range finalTestEnv {
		envOK = envOK && slices.Contains(t.Env, e)
	}
	names := sortedTree(files)
	switch {
	case !slices.Equal(t.Argv, testArgv) || !envOK || commit == "" || t.Commit != commit || t.Tree != tree || !slices.Equal(t.Files, names):
		return "test_binding_mismatch"
	case t.ExitCode != 0 || t.Signal != "" || t.TimedOut || strings.TrimSpace(t.Toolchain) == "":
		return "tests_failed"
	case t.DurationMS < 0 || time.Duration(t.DurationMS)*time.Millisecond > FinalTestBound:
		return "test_deadline_exceeded"
	}
	return ""
}

// execBinding binds every final validation command to the candidate, to
// its own start and end validation events, and to the one shared deadline.
func (c *devChecker) execBinding(execs []ExecRecord, commit, tree string) string {
	var first, last time.Time
	for _, e := range execs {
		start, end := c.eventAt(e.StartSeq), c.eventAt(e.EndSeq)
		if e.Schema != ExecSchema || e.Commit != commit || e.Tree != tree || start == nil || end == nil || start.Type != EvValidationStarted ||
			end.Type != EvValidationFinished || start.Code != e.Name || end.Code != e.Name || e.StartSeq >= e.EndSeq {
			return "validation_binding_mismatch"
		}
		s, _ := parseTime(start.Time)
		f, _ := parseTime(end.Time)
		if first.IsZero() || s.Before(first) {
			first = s
		}
		if f.After(last) {
			last = f
		}
	}
	vs := c.eventsOf(EvValidationStarted)
	if len(vs) > 0 {
		s, _ := parseTime(vs[0].Time)
		if s.Before(first) || first.IsZero() {
			first = s
		}
	}
	if !last.Before(finalDeadline(first, c.attemptDeadline().Add(-AttemptBound))) {
		return "validation_deadline_exceeded"
	}
	return ""
}

// checkCleanup requires every owned instance stopped and verified, every
// owned task settled, the final listing complete and the runtime removed
// within the shared 2m cleanup budget.
func (c *devChecker) checkCleanup() {
	cl := doc[devEnvelope[DevCleanup]](c.b, devCleanupFile)
	rb := c.newRow("cleanup/summary", "cleanup",
		"Cleanup settled every owned task, stopped and reaped every owned process group by its captured handle, completed the final task listing, joined control work and removed the runtime only after verified cleanup, within the shared 2m budget", groupTail)
	defer rb.done()
	if cl == nil {
		rb.miss("cleanup_missing", true)
		return
	}
	rb.ref(devCleanupFile, nil, "", "")
	d := cl.Data
	if !d.Complete || len(d.UnresolvedTasks) != 0 || len(d.UnsettledTasks) != 0 || d.ScanError != "" || d.OverBudget {
		rb.bad("cleanup_incomplete")
	}
	if len(d.LateAdmissions) != 0 {
		rb.bad("late_admission")
	}
	if !d.RuntimeRemoved && c.reached(typeIs(EvRuntimeCreated)) {
		rb.bad("runtime_not_removed")
	}
	s, ok1 := parseTime(d.StartedAt)
	e, ok2 := parseTime(d.FinishedAt)
	if !ok1 || !ok2 || e.Before(s) || !e.Before(s.Add(CleanupBound)) {
		rb.bad("cleanup_deadline_exceeded")
	}
	var started []StartedProc
	for _, ev := range c.b.Events {
		if ev.Type == EvProcessStarted || ev.Type == EvPhaseStarted {
			started = append(started, StartedProc{Name: ev.Handle, PID: ev.PID, StartedSeq: ev.Seq})
		}
	}
	if !bytes.Equal(mustJSON(started), mustJSON(d.Started)) {
		rb.bad("process_record_conflict")
	}
	stops := map[int]ProcRecord{}
	for _, p := range d.Processes {
		if _, dup := stops[p.StartedSeq]; dup {
			rb.bad("process_record_conflict")
		}
		stops[p.StartedSeq] = p
	}
	for _, sp := range started {
		irb := c.newRow(fmt.Sprintf("cleanup/%s-%03d", instanceSlug(sp.Name), sp.StartedSeq), "cleanup",
			fmt.Sprintf("The owned instance %s (pid %d, started at seq %d) was stopped by its captured handle and its exit and group end verified", auditText(sp.Name, 64), sp.PID, sp.StartedSeq),
			groupTail)
		p, ok := stops[sp.StartedSeq]
		switch {
		case !ok:
			irb.bad("process_record_missing")
		case p.Name != sp.Name || p.PID != sp.PID:
			irb.bad("process_record_conflict")
		case !p.ExitVerified:
			irb.bad("process_exit_unverified")
		default:
			irb.see("stopped by %s, %s", p.StoppedBy, auditText(p.Exit, 40))
		}
		irb.ref(devCleanupFile, nil, "", "")
		irb.done()
	}
	settled := map[string]string{}
	for _, s := range d.Settled {
		settled[s.Task] = s.State
	}
	owned := map[string]bool{}
	for _, ev := range c.b.Events {
		if (ev.Type == EvTaskAdmitted && ev.Code == "") || ev.Type == EvExtraAdmission {
			owned[ev.Task] = true
		}
	}
	var ids []string
	for id := range owned {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !contract.TaskTerminal(settled[id]) {
			rb.bad("task_unsettled")
		}
	}
	rb.see("%d instances, %d owned tasks settled, runtime removed %t", len(started), len(d.Settled), d.RuntimeRemoved)
}

// instanceSlug renders a process handle as a row ID component.
func instanceSlug(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 'a' - 'A')
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// checkEnvironment recomputes the owner-environment comparison.
func (c *devChecker) checkEnvironment() {
	m := c.m
	rb := c.newRow("environment/protected", "owner_environment",
		"Complete before and after fingerprints of every supplied protected root's repositories and the four vendor homes show no owner-configuration change; the stored diff is exactly its recomputation and tool-maintained changes stay report-only", groupTail)
	in := envCheckInput{manifest: true, roots: m.ProtectedRoots, depth: m.ProtectDepth, files: c.b.Files, diff: doc[EnvDiff](c.b, "environment/diff.json"),
		notReached: func() bool {
			return c.stop != nil && !c.reached(func(e Event) bool { return e.Type == EvSnapshotTaken && e.Code == "before" })
		}, policy: envPolicyDev}
	checkOwnerEnvironment(in, func(code string) {
		if code == CodeEnvUnobserved || code == "environment_diff_missing" {
			rb.miss(code, true)
			return
		}
		rb.bad(code)
	})
	if in.notReached() {
		rb.unreached = true
		c.unreach["owner_environment"] = true
	}
	rb.ref("environment/diff.json", nil, "", "")
	for _, ph := range []string{"before", "after"} {
		for _, f := range envFiles(ph) {
			rb.ref(f, nil, "", "")
		}
	}
	rb.see("%d protected roots at depth %d", len(m.ProtectedRoots), m.ProtectDepth)
	rb.done()
}

// checkDeadlines reconstructs the bounds from the plane's duration
// records and the supervisor's events: both must fit, and equality with a
// deadline is expired.
func (c *devChecker) checkDeadlines() {
	rb := c.newRow("deadlines/bounds", "deadlines",
		"Every workflow task finished before admission+10m on both the plane's record and the supervisor's consumption, every admission, decision and validation event precedes the attempt's 90m deadline, and pauses never reset it", groupTail)
	defer rb.done()
	deadline := c.attemptDeadline()
	for _, n := range c.b.ordinalsOf("tasks") {
		v := c.taskView(n)
		if v == nil {
			continue
		}
		created, ok := contract.ParseTime(v.CreatedAt)
		if d, dok := duration(*v); !dok || !ok || d >= FeatureTaskBound {
			rb.bad("task_deadline_exceeded")
		}
		if ev := c.eventsOfTask(n, v.TaskID); ev.consumed > 0 {
			if e := c.eventAt(ev.consumed); e != nil {
				if t, _ := parseTime(e.Time); !t.Before(created.Add(FeatureTaskBound)) {
					rb.bad("task_deadline_exceeded")
				}
			}
		}
	}
	for _, e := range c.b.Events {
		switch e.Type {
		case EvTaskAdmitted, EvObserved, EvOwnerDecision, EvReceiptCreated, EvValidationStarted, EvValidationFinished, EvFinalTests:
			if t, _ := parseTime(e.Time); !t.Before(deadline) {
				rb.bad("attempt_deadline_exceeded")
			}
		}
	}
	rb.see("attempt deadline %s", c.m.AttemptDeadline)
}

// checkCost recomputes the accounting and its ceilings.
func (c *devChecker) checkCost() {
	rb := c.newRow("cost/accounting", "cost",
		"The derived accounting lists every preflight, workflow and rejected admission (failed paid calls included), every phase, the owner windows, the attempt and the cleanup, exactly as recomputed; provider usage is unknown, never estimated", groupTail)
	defer rb.done()
	got := doc[DevCost](c.b, devCostFile)
	if got == nil {
		rb.miss("cost_missing", true)
		return
	}
	rb.ref(devCostFile, nil, "", "")
	want := deriveDevCost(c.b, c.runID)
	if !bytes.Equal(mustJSON(*got), mustJSON(want)) {
		rb.bad("cost_mismatch")
	}
	switch {
	case len(want.PreflightTasks) > len(DevFullRoleIDs) || len(want.WorkflowTasks) > maxWorkflowTasks || want.PhaseStarts > maxPhases:
		rb.bad("cost_ceiling_exceeded")
	case want.ProviderUsage != usageUnknown:
		rb.bad("usage_invented")
	}
	accounted := map[string]bool{}
	for _, t := range want.Tasks {
		accounted[t.Task] = true
		if t.DurationMS == nil && t.Kind != "rejected" {
			rb.bad("task_accounting_missing")
		}
	}
	// Every task cleanup settled (the complete final listing of owned
	// tasks) is an accounted call.
	if cl := doc[devEnvelope[DevCleanup]](c.b, devCleanupFile); cl != nil {
		for _, s := range cl.Data.Settled {
			if !accounted[s.Task] {
				rb.bad("task_accounting_missing")
			}
		}
	}
	for _, p := range want.Phases {
		if p.DurationMS == nil {
			rb.bad("phase_accounting_missing")
		}
	}
	rb.see("%d worker calls (%d completed, %d failed), %d phase starts", want.WorkerCalls, want.Completed, want.Failed, want.PhaseStarts)
}
