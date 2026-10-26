package reale2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
)

// devSupervisor runs one full development flow attempt (designs
// 12a-dev-flow-harness and 12a-dev-flow-verify). It reuses the legacy
// supervisor's process, plane, evidence, fingerprint and cleanup machinery
// and never calls the legacy four-hop judge. Every control request,
// terminal line, phase exit and monitor tick is handled by one loop
// goroutine (devfull_loop.go); the socket goroutines only forward requests
// to it. Its evidence is the versioned callsheet-dev-full-evidence/v1
// bundle, judged at the end by the same checker the offline check runs.
type devSupervisor struct {
	s         *supervisor
	flow      DevFullFlow
	flowLabel string
	selection []byte
	loaded    DevLoaded
	state     DevFullState
	manuals   map[string][2]string

	phases    []*devPhase
	sessionID string

	// gate is set while the coordinator must end its phase at an owner or
	// clarification gate; window is the open decision window.
	gate         *devGate
	window       *devWindow
	term         windowedTerminal
	lines        <-chan ownerInput
	activated    bool
	termEOF      bool
	ignoredLines int
	decisions    int
	receiptBytes map[int][]byte

	slot *devSlot
	// inspection is the slot's running publication pull (nil: none).
	inspection *devInspection
	trees      map[string]map[string]TreeFile
	commits    []devCommit
	rejected   []string
	// late are the rejected tasks first found by the final scan.
	late   []string
	finals map[string]string

	// manifest is the evidence manifest, completed after collection;
	// helper is the reviewer verification helper's fixed recipe; sweeps
	// records every ListTasks sweep; settled are the owned tasks' states
	// at cleanup.
	manifest DevManifest
	helper   reviewHelperSpec
	sweeps   sweepRecorder
	settled  []DevSettlement

	seedVerified bool
	// seedPushSeq is the event sequence at which the seed push was seen.
	seedPushSeq int
	ticks       int
	written     int

	stopCode, stopReason string
	stopSeq              int
	complete             bool
	executed             bool
	cleanupOK            bool
	protected            bool
	finished             bool
	looping              bool
	// passed is the checked result: PASS with every derived file written
	// (for the qualification probe: QUALIFIED).
	passed bool
	// probe, when set, runs the owner-only qualification probe instead of
	// the full flow (devfull_probe.go).
	probe *devProbe
	// light, when set, runs the light scenario instead of the full flow
	// (devlight_*.go).
	light *devLight
	// exerciseChild is a combined exercise's child attempt: its 2m cleanup
	// tail bounds its collection and check too (finalizeExpired when it
	// did not fit).
	exerciseChild   bool
	finalizeExpired bool
}

// devGate is a pending phase-ending gate.
type devGate struct {
	kind string
	gen  int
}

// devWindow is an open owner or clarification decision window.
type devWindow struct {
	kind         string
	gen          int
	epoch        uint64
	deadline     time.Time
	promptSeq    int
	phaseExitSeq int
	sums         map[string]string
}

// devSlot is the one pending workflow plan's live state.
type devSlot struct {
	plan        *TaskPlan
	docPath     string
	docDigest   string
	exposed     bool
	matched     string
	admittedAt  time.Time
	admittedSeq int
	observed    bool
	observedSeq int
	phase       int
	waitStarted bool
	waitSeq     int
	waitWritten bool
	terminal    bool
	cmd         string
	// logs and logMeta are the task's TaskLogs, retrieved once at its
	// terminal observation (the reviewer's execution evidence source).
	logs    []byte
	logMeta DevLogMeta
}

// devPhase is one coordinator phase.
type devPhase struct {
	n      int
	proc   Proc
	stdout *limitBuffer
	stderr *limitBuffer
	exited bool
	rec    PhaseProcess
}

// runDevLive builds the world and runs one checked full-flow attempt.
func runDevLive(h Host, a runArgs) int {
	w, release, err := h.NewWorld(h)
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e run: %v\n", err)
		return 1
	}
	defer release()
	s := &supervisor{stdout: h.Stdout, stderr: h.Stderr, goos: h.GOOS, goarch: h.GOARCH, w: w, args: a, ctx: w.Context,
		known: map[string]string{}, stoppedSet: map[*child]bool{}, reqs: make(chan ctrlMsg), loopDone: make(chan struct{}), envPolicy: envPolicyDev}
	d := &devSupervisor{s: s, manuals: map[string][2]string{}, receiptBytes: map[int][]byte{}, trees: map[string]map[string]TreeFile{},
		finals: map[string]string{}, sweeps: sweepRecorder{clock: w.Clock}}
	if h.observeDev != nil {
		h.observeDev(d)
	}
	return d.run()
}

func (d *devSupervisor) run() int {
	switch {
	case d.probe != nil && d.probe.cursor != nil:
		fmt.Fprint(d.s.stdout, CursorProbeBoundsFor(d.probe.scope))
		fmt.Fprintln(d.s.stdout, cursorProbeDisclosure)
	case d.probe != nil:
		fmt.Fprint(d.s.stdout, ProbeBoundsFor(d.probe.scope))
		fmt.Fprintln(d.s.stdout, "qualification probe: prints QUALIFIED or BLOCKED, never a workflow PASS; exit 0 qualified, 1 blocked, 2 usage or gate error")
	case d.light != nil:
		fmt.Fprintln(d.s.stdout, "light development flow: checked "+DevBundleSchema+" (scenario light) evidence; exit 0 mechanical PASS, 1 executed failure or incomplete evidence, 2 usage, gate or schema error")
	default:
		fmt.Fprint(d.s.stdout, DevBounds)
		fmt.Fprintln(d.s.stdout, "full development flow: checked "+DevBundleSchema+" evidence; exit 0 mechanical PASS, 1 executed failure or incomplete evidence, 2 usage, gate or schema error")
	}
	if err := d.s.locate(); err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e run: %v\n", err)
		return 2
	}
	now := d.s.w.Clock.Now()
	id, err := newRunID(now, d.s.w.Rand)
	if err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e run: %v\n", err)
		return 1
	}
	d.s.runID = id
	if d.s.ev, err = newEvidenceWriter(d.s.evidenceDir, d.s.w.Clock); err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e run: %v\n", err)
		return 1
	}
	d.s.manifest = Manifest{RunID: id, ProtectedRoots: []string{}}
	d.manifest = DevManifest{Schema: DevBundleSchema, RunID: id, Scenario: d.scenario(), GOOS: d.s.goos, GOARCH: d.s.goarch,
		Gate: GateRecord{OptIn: true, CIAbsent: true, Platform: d.s.goos}, Versions: []VersionRecord{}, Roles: []FlowRole{}, ProtectedRoots: []string{},
		Render: map[string]string{}, Inventory: []InventoryEntry{}}
	d.s.ev.event(Event{Type: EvRunStarted})
	d.s.say("run %s: evidence %s", id, d.s.ev.root)
	d.s.attempt = d.s.w.Clock.Now()
	d.manifest.AttemptStart, d.manifest.AttemptDeadline = formatTime(d.s.attempt), formatTime(d.attemptDeadline())
	d.s.ev.event(Event{Type: EvPreflightStarted})
	start := d.start
	switch {
	case d.probe != nil:
		start = d.probeStart
	case d.light != nil:
		start = d.lightStart
	}
	if err := start(); err != nil {
		d.fail(err)
	} else {
		d.loop()
	}
	d.finish()
	return d.exitCode()
}

// scenario is the attempt's scenario (full unless a light attempt).
func (d *devSupervisor) scenario() string {
	if d.light != nil {
		return scenarioLight
	}
	return scenarioFull
}

// fail records err as the attempt's stop (the first one wins).
func (d *devSupervisor) fail(err error) {
	d.stop(codeOf(err), err.Error())
}

// stop records the attempt's first stop code and its event; no further
// dispatch or completion is accepted after it.
func (d *devSupervisor) stop(code, reason string) {
	if d.stopCode != "" {
		return
	}
	d.stopCode, d.stopReason = code, cmpOr(reason, code)
	if d.s.ev != nil {
		d.stopSeq = d.s.ev.event(Event{Type: EvAttemptFailed, Code: code}).Seq
	}
	fmt.Fprintf(d.s.stderr, "reale2e run: stopped (%s): %s\n", code, contract.SafeText(d.stopReason, 400))
}

// halted reports a stopped or fully executed attempt. A sticky evidence
// failure (an event, log or file write) stops the attempt at the first
// check after it: every plan exposure, admission, decision and completion
// is preceded by one.
func (d *devSupervisor) halted() bool {
	if d.stopCode == "" && d.s.ev != nil {
		if err := d.s.ev.failure(); err != nil {
			d.stop("evidence_write_failed", err.Error())
		}
	}
	return d.stopCode != "" || d.executed
}

// attemptDeadline is the attempt's 90m deadline.
func (d *devSupervisor) attemptDeadline() time.Time { return d.s.attempt.Add(AttemptBound) }

// expired reports now >= deadline (equality is expired).
func (d *devSupervisor) expired(deadline time.Time) bool { return !d.s.w.Clock.Now().Before(deadline) }

// writeEvidence writes rel; a failure closes dispatch.
func (d *devSupervisor) writeEvidence(rel string, data []byte) bool {
	if err := d.s.ev.writeFile(rel, data); err != nil {
		d.stop("evidence_write_failed", err.Error())
		return false
	}
	return true
}

// writeEvidenceJSON writes v as rel; a failure closes dispatch.
func (d *devSupervisor) writeEvidenceJSON(rel string, v any) bool {
	if err := d.s.ev.writeJSON(rel, v); err != nil {
		d.stop("evidence_write_failed", err.Error())
		return false
	}
	return true
}

// start runs setup to the first coordinator phase. Every step first
// checks the interrupt and the attempt deadline (now >= deadline).
func (d *devSupervisor) start() error {
	for _, f := range []func() error{d.s.printPermissionTemplate, d.hashExecutable, d.s.protectedRoots, d.s.baselineSnapshot, d.loadSelection,
		d.versionProbes, d.s.createRuntime, d.s.renderPermissions, d.extraDirs, d.deployDev, d.preflightDev, d.seedDev, d.openControl,
		d.activateTerminal, d.initialPlan, d.launchPhase} {
		switch {
		case d.s.ctx.Err() != nil:
			return failure("interrupted", d.s.ctx.Err())
		case d.expired(d.attemptDeadline()):
			return failure("attempt_timeout", errors.New("the attempt reached its 90m bound"))
		}
		if err := f(); err != nil {
			return err
		}
		if d.stopCode != "" {
			return failure(d.stopCode, errors.New(d.stopReason))
		}
	}
	return nil
}

// loadSelection captures the shared method and exercise files once, then
// freezes and validates the selection before anything is deployed. An
// explicit --flow takes only the five-role development schema (a legacy
// four-role flow is refused here, before any paid work).
func (d *devSupervisor) loadSelection() error {
	loaded, err := LoadDevMethodFor(d.s.checkout, d.scenario())
	if err != nil {
		return failure("templates_missing", err)
	}
	flow, err := ParseDevFullFlow(loaded.FlowRaw)
	if err != nil {
		return failure("flow_invalid", err)
	}
	flowRaw := loaded.FlowRaw
	if d.s.args.flow != "" {
		if flowRaw, err = readBounded(d.s.args.flow, maxFlowBytes); err != nil {
			return failure("flow_invalid", err)
		}
		if flow, err = ParseDevFullFlow(flowRaw); err != nil {
			return failure("flow_invalid", err)
		}
	}
	if err := flow.CheckDevPosture(d.s.goos); err != nil {
		return failure("posture_refused", err)
	}
	// The frozen selection must still be the one the command bound before
	// any capability existed (design 12a-3-cursor-worker FP-2).
	if err := d.cursorSelectionBinding(flow); err != nil {
		return err
	}
	instr, runbook, err := loaded.ManualPair("triage")
	if err != nil {
		return failure("templates_missing", err)
	}
	if err := checkTriageManualBudget(instr, runbook); err != nil {
		return failure(CodePromptBudget, err)
	}
	def, _ := DefaultDevFullFlow().SelectionCanonical()
	got, err := flow.SelectionCanonical()
	if err != nil {
		return failure("flow_invalid", err)
	}
	d.flowLabel = FlowDefault
	if string(def) != string(got) {
		d.flowLabel = FlowOverride
	}
	d.flow, d.loaded, d.selection = flow, loaded, got
	d.manifest.FlowLabel, d.manifest.FlowSHA256, d.manifest.SelectionSHA256 = d.flowLabel, sha256Hex(flowRaw), sha256Hex(got)
	d.manifest.Roles = append([]FlowRole{}, flow.Roles...)
	orig, gen := sha256Hex([]byte(FeatureRequest)), ""
	if f, err := devFullFeatureRequest(); err == nil {
		gen = sha256Hex([]byte(f))
	}
	run := d.s.runID
	// The captured bytes are stored inertly (design 12a-dev-flow-light,
	// FP-7): each provenance record names its computed storage path, and
	// no two physical captures collide under case folding.
	sources := captureRecords(loaded.Sources)
	var physical []string
	for _, src := range sources {
		physical = append(physical, src.StoragePath)
	}
	if err := checkCaptureFold(physical); err != nil {
		return failure("templates_missing", err)
	}
	records := map[string]any{
		"setup/sources.json": envelope("sources", run, sources),
		"setup/flow-label.json": DevFlowLabel{Schema: devKind("flow-label"), RunID: run, Label: d.flowLabel, FlowSHA256: sha256Hex(flowRaw),
			SelectionSHA256: sha256Hex(got), Selection: string(got)},
		"setup/feature-request.json": DevFeatureRecord{Schema: devKind("feature-request"), RunID: run, OriginalSHA256: orig, GeneratedSHA256: gen,
			Source: "internal/reale2e/seed.go FeatureRequest"},
	}
	if d.light != nil {
		// The light request is the committed fixture's FEATURE.md, recorded
		// by setup/light-scenario.json instead.
		delete(records, "setup/feature-request.json")
	}
	for rel, v := range records {
		if !d.writeEvidenceJSON(rel, v) {
			return failure(d.stopCode, errors.New(d.stopReason))
		}
	}
	if !d.writeEvidence("setup/flow.json", flowRaw) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	// The captured bytes themselves (manuals, playbook and exercise files)
	// are retained inertly under _captured/setup/sources/<repository
	// path>.bin, within the evidence budget: the private runtime copies are
	// removed by cleanup.
	names := DevMethodNamesFor(d.scenario())
	if len(loaded.Sources) != len(names) {
		return failure("templates_missing", errors.New("the captured sources do not match the method inputs"))
	}
	for i, src := range loaded.Sources {
		b := loaded.Files[names[i]]
		if sha256Hex(b) != src.SHA256 {
			return failure("templates_missing", fmt.Errorf("the capture of %s does not match its record", src.Path))
		}
		if !d.writeEvidence(capturedPath("setup/sources/"+src.Path), b) {
			return failure(d.stopCode, errors.New(d.stopReason))
		}
	}
	return nil
}

// versionProbes checks the executables' versions and eligibility and
// records each observation and the checkout revision in the manifest.
func (d *devSupervisor) versionProbes() error {
	ver, err := d.s.probeOutput(d.s.args.callsheet, "version")
	if err != nil {
		return failure("version_preflight_failed", err)
	}
	d.manifest.CallsheetVersion, d.manifest.Revision = ver, checkoutRevision(d.s.checkout)
	for _, v := range d.versionVendors() {
		exe := d.s.args.vendor(v)
		rec := VersionRecord{Tool: v, Path: d.s.tok(exe)}
		out, err := d.s.probeOutput(exe, "--version")
		if err == nil {
			err = d.s.w.Probe(d.s.ctx, v, exe, d.s.w.TempRoot)
		}
		rec.Version, rec.Eligible = out, err == nil
		d.manifest.Versions = append(d.manifest.Versions, rec)
		if err != nil {
			return failure("version_preflight_failed", err)
		}
	}
	for _, tool := range []struct{ name, arg string }{{"go", "version"}, {"git", "--version"}} {
		exe, err := d.s.w.LookPath(tool.name)
		if err != nil {
			return failure("version_preflight_failed", err)
		}
		out, err := d.s.probeOutput(exe, tool.arg)
		d.manifest.Versions = append(d.manifest.Versions, VersionRecord{Tool: tool.name, Path: d.s.tok(exe), Version: out, Eligible: err == nil})
		if err != nil {
			return failure("version_preflight_failed", err)
		}
		if tool.name == "go" {
			d.s.goBin = exe
		}
	}
	d.s.ev.event(Event{Type: EvVersionsChecked})
	d.s.pathEnv = envValue(d.s.w.Environ, "PATH")
	return nil
}

// extraDirs creates the development runtime directories: the fourteen
// wait directories, dispatch documents, decisions, receipts, presented
// review copies, manuals and inspection repositories.
func (d *devSupervisor) extraDirs() error {
	for _, rel := range []string{"decisions", "receipts", "dispatch", "manuals", "inspect", "intake", filepath.Join("review", "generations")} {
		if err := os.MkdirAll(filepath.Join(d.s.runtime, rel), 0o700); err != nil {
			return failure("runtime_unavailable", err)
		}
	}
	for i := 0; i < maxWorkflowTasks; i++ {
		if err := os.MkdirAll(waitDir(d.s.runtime, i), 0o700); err != nil {
			return failure("runtime_unavailable", err)
		}
	}
	return nil
}

// deployDev starts the plane and sidecars, installs the captured manuals,
// registers all five roles and checks readiness after the final registry
// change.
func (d *devSupervisor) deployDev() error {
	d.s.dep = &deployment{s: d.s}
	if err := d.s.dep.startPlane(); err != nil {
		return failure("plane_start_failed", err)
	}
	pl, pu, closer, err := d.s.w.Connect(d.s.dep.url, d.s.dep.caPEM)
	if err != nil {
		return failure("plane_connect_failed", err)
	}
	d.s.plane, d.s.puller, d.s.closePlane = pl, pu, closer
	if err := d.s.dep.startSidecars(); err != nil {
		return failure("sidecar_start_failed", err)
	}
	if err := d.writeManuals(); err != nil {
		return err
	}
	if err := d.registerDevRoles(); err != nil {
		return err
	}
	d.s.ev.event(Event{Type: EvRolesRegistered})
	for _, a := range d.s.sidecarList() {
		if err := d.s.dep.restart(a); err != nil {
			return failure("sidecar_restart_failed", err)
		}
	}
	d.s.ev.event(Event{Type: EvSidecarsReconnected})
	if err := d.verifyManuals(); err != nil {
		return err
	}
	return d.awaitDevReady()
}

// writeManuals copies the captured role manuals into the private runtime.
// Registration names these files; later checkout edits cannot change an
// in-flight task's method.
func (d *devSupervisor) writeManuals() error {
	dir := filepath.Join(d.s.runtime, "manuals")
	for _, role := range DevFullRoleIDs {
		var paths [2]string
		for i, kind := range []string{"instruction", "runbook"} {
			name := manualName(role, kind)
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, d.loaded.Files[name], 0o600); err != nil {
				return failure("runtime_unavailable", err)
			}
			paths[i] = p
		}
		d.manuals[role] = paths
	}
	return nil
}

// verifyManuals rechecks every installed manual's bytes against the
// capture before final role readiness.
func (d *devSupervisor) verifyManuals() error {
	var rec []SourceRecord
	for _, role := range DevFullRoleIDs {
		for i, kind := range []string{"instruction", "runbook"} {
			name := manualName(role, kind)
			got, err := readBounded(d.manuals[role][i], maxTemplateBytes)
			if err != nil || sha256Hex(got) != sha256Hex(d.loaded.Files[name]) {
				return failure("manual_hash_mismatch", fmt.Errorf("installed manual %s differs from the capture", name))
			}
			rec = append(rec, SourceRecord{Path: "manuals/" + name, Bytes: len(got), SHA256: sha256Hex(got)})
		}
	}
	if !d.writeEvidenceJSON("setup/installed-manuals.json", envelope("installed-manuals", d.s.runID, rec)) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	return nil
}

func (d *devSupervisor) registerDevRoles() error {
	for _, r := range d.flow.Roles {
		paths := d.manuals[r.ID]
		rc := contract.RoleConfig{ID: r.ID, Name: r.ID, Node: d.s.dep.nodes[r.Adapter], Adapter: r.Adapter,
			Instruction: paths[0], Runbook: paths[1], Model: r.Model, Effort: r.Effort,
			Concurrency: RoleConcurrency, Timeout: FeatureTaskBound, HasTimeout: true}
		ctx, cancel := d.s.opCtx()
		_, err := d.s.plane.AddRole(ctx, rc)
		cancel()
		if err != nil {
			return failure("role_registration_failed", err)
		}
	}
	return nil
}

// awaitDevReady waits (on the injected clock, at most 2m) until all five
// roles accept work with their frozen selection.
func (d *devSupervisor) awaitDevReady() error {
	deadline := d.s.w.Clock.Now().Add(2 * time.Minute)
	for {
		ready, snap := true, []RoleSnapshot{}
		for _, r := range d.flow.Roles {
			ctx, cancel := d.s.opCtx()
			v, err := d.s.plane.ShowRole(ctx, r.ID)
			cancel()
			ok := err == nil && v.CanAccept && v.Adapter == r.Adapter && v.Model == r.Model && v.Effort == r.Effort
			ready = ready && ok
			if err == nil {
				snap = append(snap, RoleSnapshot{ID: v.ID, Node: v.Node, Adapter: v.Adapter, Model: v.Model, Effort: v.Effort,
					Concurrency: v.Concurrency, Timeout: v.Resolved().Timeout.String(), CanAccept: v.CanAccept})
			}
		}
		if ready {
			ev := d.s.ev.event(Event{Type: EvRolesReady})
			if !d.writeEvidenceJSON("setup/roles.json", RolesRecord{Schema: RolesSchema, ReadyAt: ev.Time, Roles: snap}) {
				return failure(d.stopCode, errors.New(d.stopReason))
			}
			return nil
		}
		if d.expired(deadline) || d.expired(d.attemptDeadline()) {
			return failure("roles_not_ready", errors.New("the five roles did not become ready"))
		}
		d.s.sleep(time.Second)
	}
}

// preflightDev runs the five individually recorded no-workspace
// authentication preflights before any workflow task.
func (d *devSupervisor) preflightDev() error {
	for i, r := range d.flow.Roles {
		if err := d.preflightOne(i, r); err != nil {
			d.s.say("authentication preflight %s failed; resolve login, model or posture outside the harness, then start a new attempt", r.ID)
			return err
		}
	}
	return nil
}

// DevPreflightRecord is preflight/NN/observation.json's data.
type DevPreflightRecord struct {
	Label       string `json:"label"`
	Role        string `json:"role"`
	Task        string `json:"task"`
	Adapter     string `json:"adapter"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	State       string `json:"state"`
	Answered    bool   `json:"answered"`
	TokenEchoed bool   `json:"token_echoed"`
}

func (d *devSupervisor) preflightOne(i int, role FlowRole) error {
	label := preflightHop(i)
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: role.ID}, Goal: preflightGoal,
		Payload: []string{markerFor(d.s.runID, label)}, Acceptance: preflightAcceptance, RequestedBy: requester}
	sent := d.s.w.Clock.Now()
	id, err := d.s.dispatchOwn(req, sent.Add(PreflightBound))
	if err != nil {
		return err
	}
	d.s.known[id] = label
	d.s.preflight = append(d.s.preflight, id)
	d.s.ev.event(Event{Type: EvTaskAdmitted, Hop: label, Task: id})
	v, err := d.s.awaitTerminal(id, PreflightBound, sent)
	rec := DevPreflightRecord{Label: label, Role: role.ID, Task: id}
	if v != nil {
		d.s.storeTask("preflight/"+hopDir(i), *v)
		rec.Adapter, rec.Model, rec.Effort, rec.State = v.Role.Adapter, v.Effective.Model, v.Effective.Effort, v.State
		if r := v.Result; r != nil && r.FinalMessage != nil && !r.FinalMessageTruncated {
			rec.Answered, rec.TokenEchoed = authAnswerPresent(*r.FinalMessage), authAnswerOK(*r.FinalMessage)
		}
	}
	d.s.ev.event(Event{Type: EvPreflightFinished, Hop: label, Task: id})
	if !d.writeEvidenceJSON("preflight/"+hopDir(i)+"/observation.json", envelope("observation", d.s.runID, rec)) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	if err != nil {
		return err
	}
	return preflightVerdict(label, role, *v)
}

// preflightVerdict judges one authentication preflight's authoritative
// view: no workspace, the exact effective pair, success with a complete
// nonempty answer (the token echo is information only).
func preflightVerdict(label string, role FlowRole, v contract.TaskView) error {
	r := v.Result
	switch {
	case v.WorkspaceBinding != nil:
		return failure("preflight_failed", fmt.Errorf("%s used a workspace", label))
	case v.Role.ID != role.ID || v.Role.Adapter != role.Adapter || v.Request.Override != nil || v.Effective.Model != role.Model || v.Effective.Effort != role.Effort:
		return failure("preflight_pair_mismatch", fmt.Errorf("%s ran %s %s/%s", label, v.Role.Adapter, v.Effective.Model, v.Effective.Effort))
	case v.State != contract.TaskSucceeded || r == nil || r.ExitCode == nil || *r.ExitCode != 0:
		return failure("preflight_failed", fmt.Errorf("%s ended %s", label, v.State))
	case r.FinalMessage == nil || r.FinalMessageTruncated || !authAnswerPresent(*r.FinalMessage):
		return failure("preflight_not_authenticated", fmt.Errorf("%s gave no complete, non-empty answer", label))
	}
	return nil
}

// seedDev creates the development seed (its FEATURE.md derived from the
// legacy request), runs the baseline and creates the workspace the
// coordinator pushes the seed into.
func (d *devSupervisor) seedDev() error {
	gomod, err := readBounded(filepath.Join(d.s.checkout, "go.mod"), 64<<10)
	if err != nil {
		return failure("checkout_unreadable", err)
	}
	ver, err := goDirective(gomod)
	if err != nil {
		return failure("checkout_unreadable", err)
	}
	files, err := devSeedFiles(ver)
	if err != nil {
		return failure("seed_failed", err)
	}
	seed, err := createSeed(filepath.Join(d.s.runtime, "seed"), files)
	if err != nil {
		return failure("seed_failed", err)
	}
	d.s.seed = seed
	if err := copyObjects(seed.repo.Storer, d.s.collect.Storer, []plumbing.Hash{seed.Commit}); err != nil {
		return failure("seed_failed", err)
	}
	d.s.ev.event(Event{Type: EvSeedCreated})
	tree := map[string]TreeFile{}
	for name, b := range files {
		tree[name] = TreeFile{Path: name, Mode: filemode.Regular, Content: b}
	}
	d.trees[seed.Commit.String()] = tree
	run, out, errOut, err := runGoTests(d.s.command, d.s.goBin, d.s.pathEnv, filepath.Join(d.s.runtime, "baseline"), seed.Files, seed.Commit.String(), seed.Tree.String())
	d.s.baseline = &run
	if !d.writeEvidenceJSON("validation/baseline.json", run) || !d.writeEvidence("validation/baseline-stdout.txt", []byte(toValidUTF8(out))) ||
		!d.writeEvidence("validation/baseline-stderr.txt", []byte(toValidUTF8(errOut))) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	d.s.ev.event(Event{Type: EvBaselineTests})
	if err != nil || run.ExitCode != 0 || run.TimedOut {
		return failure("baseline_tests_failed", fmt.Errorf("exit %d: %v", run.ExitCode, err))
	}
	suffix, err := d.s.randomHex(4)
	if err != nil {
		return failure("internal_error", err)
	}
	ctx, cancel := d.s.opCtx()
	defer cancel()
	ws, err := d.s.plane.CreateWorkspace(ctx, "e2e-"+suffix)
	if err != nil {
		return failure("workspace_create_failed", err)
	}
	d.s.workspace = WorkspaceRecord{Name: ws.Name, Instance: ws.Instance, SeedCommit: seed.Commit.String(), SeedTree: seed.Tree.String()}
	return nil
}

// verifySeed reports whether the coordinator's seed push reached the
// workspace: its default branch holds exactly the seed commit in the
// created instance. A different commit or instance stops the attempt.
func (d *devSupervisor) verifySeed() (bool, error) {
	return d.verifyMain(d.s.workspace, d.s.workspace.SeedCommit, "")
}

// openControl writes the development run file and MCP configuration,
// renders the harness-owned prompts and opens the control socket, whose
// requests the loop serves one at a time. The render values and the
// reviewer helper's recipe are recorded for the offline rebuild of every
// prompt and envelope.
func (d *devSupervisor) openControl() error {
	b, err := encodeJSON(DevRunFile{Schema: DevRunSchema, RunID: d.s.runID, Scenario: d.scenario(), Callsheet: d.s.args.callsheet, Plane: d.s.dep.url, CA: d.s.dep.caPath})
	if err != nil {
		return failure("runtime_unavailable", err)
	}
	if err := os.WriteFile(filepath.Join(d.s.runtime, RunFileName), b, 0o600); err != nil {
		return failure("runtime_unavailable", err)
	}
	cfg, err := mcpConfig(d.s.args.callsheet, d.s.dep.url, d.s.dep.caPath)
	if err != nil {
		return failure("runtime_unavailable", err)
	}
	d.s.mcpPath = filepath.Join(d.s.runtime, "mcp.json")
	if err := os.WriteFile(d.s.mcpPath, cfg, 0o600); err != nil {
		return failure("runtime_unavailable", err)
	}
	vals := map[string]string{
		"RUN_ID": d.s.runID, "RUNTIME": d.s.runtime, "PLANE_URL": d.s.dep.url, "CA_PATH": d.s.dep.caPath,
		"REALE2E": d.s.w.Executable, "WORKSPACE": d.s.workspace.Name, "INSTANCE": d.s.workspace.Instance,
		"SEED_PATH": d.s.seed.Path, "SEED_COMMIT": d.s.seed.Commit.String(), "COORD_DIR": filepath.Join(d.s.runtime, "coord"),
	}
	loaded, err := RenderDevPrompts(d.loaded, vals)
	if err != nil {
		return failure("prompt_render_failed", err)
	}
	d.manifest.Render = vals
	d.helper = reviewHelperSpec{Reale2e: d.s.w.Executable, Go: d.s.goBin, Deadline: formatHelperDeadline(d.attemptDeadline())}
	d.manifest.Helper = d.helper
	d.loaded = loaded
	d.s.rendered = map[string][]byte{CoordinatorPrompt: loaded.Rendered[DevSystemName], DevPhaseFile: loaded.Rendered[DevPhaseFile]}
	sanitized, err := mcpConfig(d.s.tok(d.s.args.callsheet), d.s.dep.url, d.s.tok(d.s.dep.caPath))
	if err != nil {
		return failure("internal_error", err)
	}
	if !d.writeEvidenceJSON("setup/rendered.json", envelope("rendered", d.s.runID, loaded.Records)) ||
		!d.writeEvidence("setup/coordinator-system.md", loaded.Rendered[DevSystemName]) ||
		!d.writeEvidence("setup/phase.md", loaded.Rendered[DevPhaseFile]) || !d.writeEvidence("setup/scenario.md", loaded.Rendered[DevScenario]) ||
		!d.writeEvidence("setup/mcp-config.json", sanitized) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	ctl, err := listenDevControl(d.s.runtime, d.s.runID, d.s.handleControl)
	if err != nil {
		return failure("control_socket_failed", err)
	}
	d.s.ctl = ctl
	d.s.ev.event(Event{Type: EvCoordinatorReady})
	return nil
}

// initialPlan schedules the scenario's first plan on the seed: the full
// flow's designer draft, or the light attempt's initial triage.
func (d *devSupervisor) initialPlan() error {
	st, res := InitialPlanFor(d.scenario(), d.s.seed.Commit.String())
	st.RunID = d.s.runID
	d.apply(st, res)
	return nil
}

// launchPhase starts the next coordinator phase headless (a later phase
// resumes the captured session). At most four phases run, one at a time.
func (d *devSupervisor) launchPhase() error {
	n := len(d.phases) + 1
	if n > maxPhases || d.probe != nil && n > maxProbePhases || d.light != nil && n > 1 {
		return failure("phase_cap", errors.New("the coordinator phase cap is reached"))
	}
	if n > 1 && !sessionIDRE.MatchString(d.sessionID) {
		return failure("session_missing", errors.New("the coordinator published no resumable session"))
	}
	prompt := d.s.rendered[DevPhaseFile]
	if d.probe != nil {
		prompt = d.probe.prompts[n]
	}
	system := d.s.rendered[CoordinatorPrompt]
	argv := devLaunchArgv(n, string(prompt), d.s.mcpPath, string(system), d.sessionID)
	coord := filepath.Join(d.s.runtime, "coord")
	p := &devPhase{n: n, stdout: newLimitBuffer(maxTextFile), stderr: newLimitBuffer(maxStderr)}
	spec := ProcSpec{Path: d.s.args.claude, Args: argv, Env: d.s.w.Environ, Dir: coord, Stdout: p.stdout, Stderr: p.stderr}
	proc, err := d.s.w.Launcher.Start(spec)
	if err != nil {
		return failure("coordinator_start_failed", err)
	}
	p.proc = proc
	d.phases = append(d.phases, p)
	seq := d.s.track(&child{name: phaseHandle(n), spec: spec, proc: proc, logs: newLineLog()}, EvPhaseStarted)
	p.rec = PhaseProcess{Phase: n, PID: proc.PID(), StartedSeq: seq}
	rec := devLaunchRecord(n, d.s.tok, d.s.args.claude, coord, argv, prompt, system, seq, d.sessionID,
		devWaitCommand(d.s.tok(d.s.w.Executable), d.s.tok(d.s.runtime), "TASK_ID", 0))
	if !d.writeEvidenceJSON(fmt.Sprintf("setup/launch-phase-%d.json", n), rec) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	d.s.say("coordinator phase %d started (pid %d)", n, proc.PID())
	return nil
}

// devLaunchRecord is phase n's launch record: the sanitized argv with
// each prompt replaced by its hash.
func devLaunchRecord(n int, tok func(string) string, claude, coord string, argv []string, prompt, system []byte, seq int, session, wait string) LaunchRecord {
	rec := LaunchRecord{Schema: LaunchSchema, Phase: n, Executable: tok(claude), Argv: []string{}, Cwd: tok(coord),
		PromptSHA256: sha256Hex(prompt), SystemSHA256: sha256Hex(system), StartedSeq: seq, Display: []string{tok(claude)},
		WaitTemplates: []string{wait}}
	if n > 1 {
		rec.ResumeSession = session
	}
	for i, a := range argv {
		switch {
		case i > 0 && argv[i-1] == "-p":
			a = "<phase prompt sha256=" + rec.PromptSHA256 + ">"
		case i > 0 && argv[i-1] == "--append-system-prompt":
			a = "<coordinator instructions sha256=" + rec.SystemSHA256 + ">"
		default:
			a = tok(a)
		}
		rec.Argv = append(rec.Argv, a)
		rec.Display = append(rec.Display, a)
	}
	return rec
}

func devLaunchArgv(phase int, prompt, mcpPath, system, session string) []string {
	argv := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--settings", SandboxOffSettings,
		"--permission-mode", autoModeFlag, "--permission-prompts", autoPrompts,
		"--mcp-config", mcpPath, "--append-system-prompt", system}
	if phase > 1 {
		argv = append(argv, "--resume", session)
	}
	return argv
}

// exitCode is 0 only for a fully written, checked PASS; an executed
// attempt whose own checker fails, and any collection or write failure,
// is 1.
func (d *devSupervisor) exitCode() int {
	if d.probe != nil && d.probe.cursor != nil {
		return d.cursorProbeExit()
	}
	if d.probe != nil {
		if d.passed {
			fmt.Fprintln(d.s.stdout, "qualification probe: "+ResultQualified)
			return 0
		}
		fmt.Fprintln(d.s.stdout, "qualification probe: "+ResultBlocked)
		return 1
	}
	name := "full development flow"
	if d.light != nil {
		name = "light development flow"
	}
	if d.passed {
		fmt.Fprintln(d.s.stdout, name+": PASS (mechanical and deterministic criteria); "+JudgmentReportLine)
		return 0
	}
	fmt.Fprintln(d.s.stdout, name+": FAIL; "+JudgmentReportLine)
	return 1
}

// finish is cleanup and evidence under one shared 2m cleanup deadline:
// dispatch closes, the running phase is stopped by its handle, every owned
// admission (preflights, workflow, rejected and any task the final scan
// finds on the private plane) is cancelled and its settlement verified,
// every child is stopped and reaped, and only then is the runtime removed.
// The final fingerprint is taken once. Then the primary records are
// completed (cleanup, ledger, admissions, the session map, the cost), the
// manifest with its inventory is written, and the bundle is judged.
func (d *devSupervisor) finish() {
	if d.finished {
		return
	}
	d.finished = true
	d.s.closing.Store(true)
	close(d.s.loopDone)
	d.s.ctx = context.WithoutCancel(d.s.ctx)
	start := d.s.w.Clock.Now()
	d.s.cleanupBy = start.Add(CleanupBound)
	d.s.ev.event(Event{Type: EvCollectionStarted})
	d.stopRunningPhase()
	// A publication pull still running is cancelled and joined before any
	// owned task is settled or the runtime removed.
	d.joinInspection()
	unresolved, scanErr := d.cancelOwned()
	d.collectSlotWait()
	if d.probe != nil {
		d.writeProbeRepository()
		d.writeCursorCoderRepository()
	} else {
		d.writeRepository()
	}
	if len(d.phases) > 0 {
		ce := CoordinatorExit{Schema: CoordExitSchema, Phases: []PhaseProcess{}}
		for _, p := range d.phases {
			ce.Phases = append(ce.Phases, p.rec)
		}
		d.s.ev.writeJSON(coordinatorExitFile, ce)
	}
	rec := d.s.cleanup(start)
	for _, p := range d.phases {
		rec.Complete = rec.Complete && p.rec.GroupGone
	}
	if d.s.ctl != nil {
		d.s.ctl.close()
	}
	if d.s.closePlane != nil {
		d.s.closePlane()
	}
	overBudget := d.s.w.Clock.Now().After(d.s.cleanupBy)
	rec.Complete = rec.Complete && len(unresolved) == 0 && !overBudget && scanErr == nil
	if rec.Complete && d.s.runtime != "" {
		if err := d.s.removeRuntime(); err != nil {
			rec.Complete = false
			fmt.Fprintf(d.s.stderr, "reale2e run: runtime %s kept: %v\n", d.s.runtime, err)
		} else {
			rec.RuntimeRemoved = true
			d.s.ev.event(Event{Type: EvRuntimeRemoved})
		}
	}
	rec.Complete = rec.Complete && (rec.RuntimeRemoved || d.s.runtime == "")
	rec.FinishedAt = formatTime(d.s.w.Clock.Now())
	d.cleanupOK = rec.Complete
	dc := DevCleanup{Started: rec.Started, Processes: rec.Processes, CancelledTasks: rec.CancelledTasks, UnsettledTasks: rec.UnsettledTasks,
		RuntimeRemoved: rec.RuntimeRemoved, StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt, Complete: rec.Complete,
		UnresolvedTasks: append([]string{}, unresolved...), OverBudget: overBudget, Deadline: formatTime(d.s.cleanupBy),
		LateAdmissions: append([]string{}, d.late...), Settled: append([]DevSettlement{}, d.settled...)}
	if scanErr != nil {
		dc.ScanError = contract.SafeText(scanErr.Error(), 400)
	}
	if !d.cleanupOK {
		d.stop("cleanup_incomplete", "cleanup could not prove every owned task settled and every owned process group gone")
		d.s.recovery(rec)
	}
	d.s.ev.writeJSON(devCleanupFile, envelope("cleanup", d.s.runID, dc))
	d.s.ev.event(Event{Type: EvCleanupFinished})
	if d.s.before != nil {
		// A protected change, or a final fingerprint that could not be
		// compared completely, is never a clean execution.
		diff := d.s.environmentDiff()
		if diff == nil || len(diff.Protected) > 0 || !diff.Complete || len(diff.Codes) > 0 {
			d.protected = true
		}
		// A Cursor probe's report-only shallow cache/tmp sample, alongside
		// the after-cleanup fingerprint (it never changes d.protected).
		d.cursorEnvironment(diff)
	}
	if d.probe == nil {
		d.s.ev.writeJSON(devLedgerFile, envelope("ledger", d.s.runID, normalizeState(d.state)))
	}
	d.s.ev.writeJSON(devAdmissionsFile, DevAdmissions{Schema: devKind("admissions"), RunID: d.s.runID, Sweeps: append([]DevSweep{}, d.sweeps.sweeps...)})
	if err := d.s.ev.failure(); err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e run: evidence is incomplete: %v\n", err)
	}
	d.s.ev.close()
	if d.probe != nil && d.probe.cursor != nil {
		// The Cursor diagnostic finalization, factored out before the
		// qualified-record finalizer: never a qualification, never passed.
		d.passed = false
		d.collectCursorProbe()
		return
	}
	if d.probe != nil {
		d.passed = d.collectProbe() && d.s.ev.failure() == nil
		return
	}
	d.passed = d.collect() && d.s.ev.failure() == nil
	if d.exerciseChild && !d.s.w.Clock.Now().Before(d.s.cleanupBy) {
		// Cleanup, collection and the child check share the 2m tail: an
		// expired finalization is never a PASS.
		d.passed, d.finalizeExpired = false, true
		fmt.Fprintf(d.s.stderr, "reale2e run: stopped (%s): cleanup, collection and the check did not finish within the 2m tail\n", stopFinalized)
	}
}

// collect writes the derived primary records (the session map and the
// cost), then the manifest with its inventory, then judges the bundle. It
// reports a checked PASS whose derived files were all written.
func (d *devSupervisor) collect() bool {
	// One load of the collected bundle serves every derivation and the
	// judgment: each record written after it (the session map, the cost,
	// the manifest) is written and then ingested from the same bytes, as
	// the loader would decode them.
	b, err := loadDevBundleAs(d.s.ev.root, devBundleV2, d.scenario())
	put := func(rel string, v any) error {
		data, err := encodeJSON(v)
		if err != nil {
			return d.s.ev.writeJSON(rel, v)
		}
		if err := d.s.ev.writeFile(rel, data); err != nil {
			return err
		}
		return b.ingest(rel, data)
	}
	if err == nil && len(d.phases) > 0 {
		err = d.collectDevSession(b, put)
	}
	if err == nil {
		err = put(devCostFile, deriveDevCost(b, d.s.runID))
	}
	if err == nil {
		err = d.writeDevManifest(put)
	}
	if err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e run: evidence collection failed: %v\n", err)
		return false
	}
	pass, err := d.judgeDev(b)
	if err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e run: the report or audit was not written: %v\n", err)
		return false
	}
	return pass
}

// collectProbe writes the probe's cost and its qualification record and
// decides the result.
func (d *devSupervisor) collectProbe() bool {
	b, err := loadDevQualificationAs(d.s.ev.root, devBundleV2, d.probe.scope)
	var cost DevCost
	if err == nil {
		cost = deriveDevCost(b, d.s.runID)
		err = d.s.ev.writeJSON(devCostFile, cost)
	}
	if err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e probe-dev-full: evidence collection failed: %v\n", err)
		return false
	}
	return d.probeFinish(cost)
}

// stopRunningPhase stops a still running coordinator phase by its
// captured process group and records the observation.
func (d *devSupervisor) stopRunningPhase() {
	p := d.current()
	if p == nil {
		return
	}
	c := d.s.childNamed(phaseHandle(p.n))
	rec := stopProc(phaseHandle(p.n), p.proc, d.s.w.Clock, d.s.left(15*time.Second))
	if c != nil {
		d.s.stopped(c, rec)
	}
	d.observePhase(p, rec.StoppedBy, d.s.left(groupBound))
}

// ownedTasks is every task this attempt owns: its preflights, workflow
// records, the pending slot's admission, every rejected admission and every
// task the final scan lists on its private plane. The final scan is
// authoritative: a listing error is returned (cleanup is then incomplete),
// and a task it finds that was never accounted for is a late admission.
func (d *devSupervisor) ownedTasks() ([]string, error) {
	var ids []string
	ids = append(ids, d.s.preflight...)
	if p := d.probe; p != nil && p.cursor != nil && p.cursor.task != "" {
		// The Cursor probe's one supervisor-owned coder task.
		ids = append(ids, p.cursor.task)
	}
	for _, t := range d.state.Tasks {
		ids = append(ids, t.TaskID)
	}
	if d.slot != nil && d.slot.matched != "" {
		ids = append(ids, d.slot.matched)
	}
	ids = append(ids, d.rejected...)
	all, err := d.listTaskIDs()
	for _, id := range all {
		if d.s.known[id] == "" {
			d.lateAdmission(id)
		}
	}
	return append(ids, all...), err
}

// lateAdmission accounts for a task first found by the final scan. The
// exposed pending plan's own unscanned admission is the slot's; any other
// is an unplanned admission: recorded as rejected, and the attempt fails
// even when its cancellation then succeeds.
func (d *devSupervisor) lateAdmission(id string) {
	if sl := d.slot; sl != nil && sl.exposed && sl.matched == "" {
		if v, err := d.s.show(id, 0); err == nil && v.Role.ID == sl.plan.Role && v.Request.Goal == sl.plan.Goal {
			d.matchSlot(v)
			return
		}
	}
	d.s.known[id] = "rejected"
	d.rejected = append(d.rejected, id)
	d.late = append(d.late, id)
	d.s.ev.event(Event{Type: EvExtraAdmission, Task: id, Code: "late_admission"})
	d.stop("extra_admission", "task "+id+" was first found by the final scan and is not the pending plan's admission")
}

// cancelOwned cancels every owned nonterminal task within the shared
// cleanup deadline and verifies its settlement, recording every owned
// task's settled state. A task whose state cannot be read or that does
// not settle is unresolved; a failed final scan is returned as scanErr.
func (d *devSupervisor) cancelOwned() (unresolved []string, scanErr error) {
	if d.s.plane == nil {
		return nil, nil
	}
	owned, scanErr := d.ownedTasks()
	seen := map[string]bool{}
	for _, id := range owned {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		v, err := d.s.show(id, 0)
		if err == nil && contract.TaskTerminal(v.State) {
			d.settled = append(d.settled, settlementOf(id, d.s.known[id], &v))
			continue
		}
		d.s.cancelTask(id)
		last, ok := d.s.settle(id, d.s.left(30*time.Second))
		d.settled = append(d.settled, settlementOf(id, d.s.known[id], last))
		if !ok {
			unresolved = append(unresolved, id)
			d.s.unsettled = append(d.s.unsettled, id)
		}
	}
	return unresolved, scanErr
}

// listTaskIDs lists every task on the plane, all pages, as the final
// recorded sweep.
func (d *devSupervisor) listTaskIDs() ([]string, error) {
	var out []string
	sw := d.sweeps.begin(sweepFinal)
	after := ""
	for {
		ctx, cancel := d.s.opCtx()
		page, next, err := d.s.plane.ListTasks(ctx, after, contract.MaxTaskListLimit)
		cancel()
		if err != nil {
			sw.end(err)
			return out, err
		}
		sw.page(after, page, next)
		for _, t := range page {
			out = append(out, t.TaskID)
		}
		if next == nil {
			sw.end(nil)
			return out, nil
		}
		after = *next
	}
}

// collectSlotWait keeps the pending slot's raw wait files for diagnosis.
func (d *devSupervisor) collectSlotWait() {
	if d.slot == nil || d.s.runtime == "" || !d.slot.waitStarted {
		return
	}
	d.writeWaitFiles(d.slot, readDevWaitFiles(d.s.runtime, d.slot.plan.Ordinal))
}

// writeWaitFiles copies a slot's raw wait files to its task evidence,
// once.
func (d *devSupervisor) writeWaitFiles(sl *devSlot, f devWaitFiles) bool {
	if sl.waitWritten {
		return true
	}
	sl.waitWritten = true
	dir := d.planDir(sl.plan) + "/"
	ok := true
	if f.OKR {
		ok = ok && d.s.ev.writeFile(dir+waitResultFile, []byte(toValidUTF8(f.Result))) == nil
	}
	if f.OKE {
		ok = ok && d.s.ev.writeFile(dir+waitStderrFile, []byte(toValidUTF8(f.Stderr))) == nil
	}
	if f.OKX {
		ok = ok && d.s.ev.writeFile(dir+waitExitFile, []byte(toValidUTF8(f.Exit))) == nil
	}
	return ok
}

// writeRepository writes the evidence repository (seed and every admitted
// result under refs/heads/task-NN) and its tree manifest.
func (d *devSupervisor) writeRepository() {
	if d.s.seed == nil {
		return
	}
	tm := DevTreeManifest{SeedCommit: d.s.seed.Commit.String(), SeedTree: d.s.seed.Tree.String(), Tasks: append([]devCommit{}, d.commits...)}
	if err := writeDevEvidenceRepo(filepath.Join(d.s.ev.root, "workspace", "repo.git"), d.s.collect.Storer, d.s.seed.Commit, d.commits); err != nil {
		d.stop("evidence_write_failed", err.Error())
	}
	if err := d.s.ev.writeJSON(devTreesFile, envelope("trees", d.s.runID, tm)); err != nil {
		d.stop("evidence_write_failed", err.Error())
	}
}
