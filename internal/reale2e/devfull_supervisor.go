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

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// devSupervisor runs one development full-flow attempt (design
// 12a-dev-flow-harness). It reuses the legacy supervisor's process, plane,
// evidence, fingerprint and cleanup machinery and never calls the legacy
// four-hop judge. Every control request, terminal line, phase exit and
// monitor tick is handled by one loop goroutine (devfull_loop.go); the
// socket goroutines only forward requests to it.
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

	seedVerified bool
	ticks        int
	written      int

	stopCode, stopReason string
	complete             bool
	executed             bool
	cleanupOK            bool
	protected            bool
	finished             bool
	looping              bool
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

// runDevLive builds the world and runs one development attempt.
func runDevLive(h Host, a runArgs) int {
	w, release, err := h.NewWorld(h)
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e run-dev-full: %v\n", err)
		return 1
	}
	defer release()
	s := &supervisor{stdout: h.Stdout, stderr: h.Stderr, goos: h.GOOS, goarch: h.GOARCH, w: w, args: a, ctx: w.Context,
		known: map[string]string{}, stoppedSet: map[*child]bool{}, reqs: make(chan ctrlMsg), loopDone: make(chan struct{})}
	d := &devSupervisor{s: s, manuals: map[string][2]string{}, receiptBytes: map[int][]byte{}, trees: map[string]map[string]TreeFile{},
		finals: map[string]string{}}
	if h.observeDev != nil {
		h.observeDev(d)
	}
	return d.run()
}

func (d *devSupervisor) run() int {
	fmt.Fprint(d.s.stdout, DevBounds)
	fmt.Fprintln(d.s.stdout, "development full flow: exit 3 means executed but unverified; this command never reports PASS")
	if err := d.s.locate(); err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e run-dev-full: %v\n", err)
		return 2
	}
	now := d.s.w.Clock.Now()
	id, err := newRunID(now, d.s.w.Rand)
	if err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e run-dev-full: %v\n", err)
		return 1
	}
	d.s.runID = id
	if d.s.ev, err = newEvidenceWriter(d.s.evidenceDir, d.s.w.Clock); err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e run-dev-full: %v\n", err)
		return 1
	}
	d.s.manifest = Manifest{RunID: id, ProtectedRoots: []string{}}
	d.s.ev.event(Event{Type: EvRunStarted})
	d.s.say("development run %s: evidence %s", id, d.s.ev.root)
	d.s.attempt = d.s.w.Clock.Now()
	d.s.ev.event(Event{Type: EvPreflightStarted})
	if err := d.start(); err != nil {
		d.fail(err)
	} else {
		d.loop()
	}
	d.finish()
	return d.exitCode()
}

// fail records err as the attempt's stop (the first one wins).
func (d *devSupervisor) fail(err error) {
	d.stop(codeOf(err), err.Error())
}

// stop records the attempt's first stop code; no further dispatch or
// completion is accepted after it.
func (d *devSupervisor) stop(code, reason string) {
	if d.stopCode != "" {
		return
	}
	d.stopCode, d.stopReason = code, cmpOr(reason, code)
	if d.s.ev != nil {
		d.s.ev.event(Event{Type: EvAttemptFailed, Code: code})
	}
	fmt.Fprintf(d.s.stderr, "reale2e run-dev-full: stopped (%s): %s\n", code, contract.SafeText(d.stopReason, 400))
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
	for _, f := range []func() error{d.s.printPermissionTemplate, d.s.protectedRoots, d.s.baselineSnapshot, d.loadSelection, d.versionProbes,
		d.s.createRuntime, d.s.renderPermissions, d.extraDirs, d.deployDev, d.preflightDev, d.seedDev, d.openControl, d.activateTerminal,
		d.initialPlan, d.launchPhase} {
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
// freezes and validates the selection before anything is deployed.
func (d *devSupervisor) loadSelection() error {
	loaded, err := LoadDevMethod(d.s.checkout)
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
	d.flowLabel = "default-flow"
	if string(def) != string(got) {
		d.flowLabel = "override-flow"
	}
	d.flow, d.loaded, d.selection = flow, loaded, got
	orig, gen := sha256Hex([]byte(FeatureRequest)), ""
	if f, err := devFullFeatureRequest(); err == nil {
		gen = sha256Hex([]byte(f))
	}
	for rel, v := range map[string]any{
		"setup/sources.json":    loaded.Sources,
		"setup/flow-label.json": map[string]string{"label": d.flowLabel, "flow_sha256": sha256Hex(flowRaw), "selection_sha256": sha256Hex(got), "selection": string(got)},
		"setup/feature-request.json": map[string]string{"original_sha256": orig, "generated_sha256": gen,
			"source": "internal/reale2e/seed.go FeatureRequest"},
	} {
		if !d.writeEvidenceJSON(rel, v) {
			return failure(d.stopCode, errors.New(d.stopReason))
		}
	}
	if !d.writeEvidence("setup/flow.json", flowRaw) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	// The captured bytes themselves (manuals, playbook and exercise files)
	// are retained under setup/sources/<repository path>, within the
	// evidence budget: the private runtime copies are removed by cleanup.
	names := DevMethodNames()
	if len(loaded.Sources) != len(names) {
		return failure("templates_missing", errors.New("the captured sources do not match the method inputs"))
	}
	for i, src := range loaded.Sources {
		b := loaded.Files[names[i]]
		if sha256Hex(b) != src.SHA256 {
			return failure("templates_missing", fmt.Errorf("the capture of %s does not match its record", src.Path))
		}
		if !d.writeEvidence("setup/sources/"+src.Path, b) {
			return failure(d.stopCode, errors.New(d.stopReason))
		}
	}
	return nil
}

// versionProbes checks the executables' versions and eligibility.
func (d *devSupervisor) versionProbes() error {
	if _, err := d.s.probeOutput(d.s.args.callsheet, "version"); err != nil {
		return failure("version_preflight_failed", err)
	}
	for _, v := range []string{"claude", "codex", "grok"} {
		exe := d.s.args.vendor(v)
		if _, err := d.s.probeOutput(exe, "--version"); err != nil {
			return failure("version_preflight_failed", err)
		}
		if err := d.s.w.Probe(d.s.ctx, v, exe, d.s.w.TempRoot); err != nil {
			return failure("version_preflight_failed", err)
		}
	}
	for _, tool := range []struct{ name, arg string }{{"go", "version"}, {"git", "--version"}} {
		exe, err := d.s.w.LookPath(tool.name)
		if err != nil {
			return failure("version_preflight_failed", err)
		}
		if _, err := d.s.probeOutput(exe, tool.arg); err != nil {
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
	for _, rel := range []string{"decisions", "receipts", "dispatch", "manuals", "inspect", filepath.Join("review", "generations")} {
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
	for _, a := range sidecarAdapters {
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
	if !d.writeEvidenceJSON("setup/installed-manuals.json", rec) {
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
			d.s.ev.event(Event{Type: EvRolesReady})
			if !d.writeEvidenceJSON("setup/roles.json", snap) {
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

// DevPreflightRecord is preflight/NN/observation.json.
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
	if !d.writeEvidenceJSON("preflight/"+hopDir(i)+"/observation.json", rec) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	if err != nil {
		return err
	}
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
	d.s.say("%s authenticated (token_echoed: %t)", label, authAnswerOK(*r.FinalMessage))
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
	page := client.StatusPage{Limit: contract.MaxWorkspaceLimit}
	for {
		ctx, cancel := d.s.opCtx()
		r, err := d.s.plane.WorkspaceStatus(ctx, d.s.workspace.Name, page)
		cancel()
		if err != nil {
			return false, failure("seed_unverified", err)
		}
		if r.Instance != d.s.workspace.Instance {
			return false, failure("seed_unverified", errors.New("the workspace instance changed"))
		}
		for _, ref := range r.Refs {
			if ref.Name != contract.DefaultBranchRef {
				continue
			}
			if ref.Commit != d.s.workspace.SeedCommit {
				return false, failure("seed_mismatch", fmt.Errorf("the workspace's main branch is %s, not the seed", contract.SafeText(ref.Commit, 64)))
			}
			return true, nil
		}
		if r.NextAfter == nil {
			return false, nil
		}
		page = client.StatusPage{After: *r.NextAfter, Instance: r.Instance, Generation: r.Generation, Limit: contract.MaxWorkspaceLimit}
	}
}

// openControl writes the development run file and MCP configuration,
// renders the harness-owned prompts and opens the control socket, whose
// requests the loop serves one at a time.
func (d *devSupervisor) openControl() error {
	b, err := encodeJSON(DevRunFile{Schema: DevRunSchema, RunID: d.s.runID, Callsheet: d.s.args.callsheet, Plane: d.s.dep.url, CA: d.s.dep.caPath})
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
	d.loaded = loaded
	d.s.rendered = map[string][]byte{CoordinatorPrompt: loaded.Rendered[DevSystemName], DevPhaseFile: loaded.Rendered[DevPhaseFile]}
	sanitized, err := mcpConfig(d.s.tok(d.s.args.callsheet), d.s.dep.url, d.s.tok(d.s.dep.caPath))
	if err != nil {
		return failure("internal_error", err)
	}
	if !d.writeEvidenceJSON("setup/rendered.json", loaded.Records) || !d.writeEvidence("setup/coordinator-system.md", loaded.Rendered[DevSystemName]) ||
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

// initialPlan schedules the first designer draft on the seed.
func (d *devSupervisor) initialPlan() error {
	st, res := InitialPlan(d.s.seed.Commit.String())
	st.RunID = d.s.runID
	d.apply(st, res)
	return nil
}

// launchPhase starts the next coordinator phase headless (a later phase
// resumes the captured session). At most four phases run, one at a time.
func (d *devSupervisor) launchPhase() error {
	n := len(d.phases) + 1
	if n > maxPhases {
		return failure("phase_cap", errors.New("at most four coordinator phases"))
	}
	if n > 1 && !sessionIDRE.MatchString(d.sessionID) {
		return failure("session_missing", errors.New("the coordinator published no resumable session"))
	}
	prompt := d.s.rendered[DevPhaseFile]
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
	rec := LaunchRecord{Schema: LaunchSchema, Phase: n, Executable: d.s.tok(d.s.args.claude), Argv: []string{}, Cwd: d.s.tok(coord),
		PromptSHA256: sha256Hex(prompt), SystemSHA256: sha256Hex(system), StartedSeq: seq, Display: []string{d.s.tok(d.s.args.claude)},
		WaitTemplates: []string{devWaitCommand(d.s.tok(d.s.w.Executable), d.s.tok(d.s.runtime), "TASK_ID", 0)}}
	if n > 1 {
		rec.ResumeSession = d.sessionID
	}
	for i, a := range argv {
		switch {
		case i > 0 && argv[i-1] == "-p":
			a = "<phase prompt sha256=" + rec.PromptSHA256 + ">"
		case i > 0 && argv[i-1] == "--append-system-prompt":
			a = "<coordinator instructions sha256=" + rec.SystemSHA256 + ">"
		default:
			a = d.s.tok(a)
		}
		rec.Argv = append(rec.Argv, a)
		rec.Display = append(rec.Display, a)
	}
	if !d.writeEvidenceJSON(fmt.Sprintf("setup/launch-phase-%d.json", n), rec) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	d.s.say("coordinator phase %d started (pid %d)", n, proc.PID())
	return nil
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

// exitCode is 3 only for an executed attempt with verified cleanup, no
// protected change and complete evidence; every stop or failure is 1.
func (d *devSupervisor) exitCode() int {
	if d.outcome() == outcomeUnverified {
		fmt.Fprintln(d.s.stdout, devUnverifiedLine)
		return 3
	}
	return 1
}

func (d *devSupervisor) outcome() string {
	switch {
	case d.protected || (d.s.ev != nil && d.s.ev.failure() != nil):
		return outcomeFailed
	case d.executed && d.stopCode == "" && d.cleanupOK:
		return outcomeUnverified
	case d.stopCode != "":
		return outcomeStopped
	}
	return outcomeFailed
}

// finish is cleanup and evidence under one shared 2m cleanup deadline:
// dispatch closes, the running phase is stopped by its handle, every owned
// admission (preflights, workflow, rejected and any task the final scan
// finds on the private plane) is cancelled and its settlement verified,
// every child is stopped and reaped, and only then is the runtime removed.
// The final fingerprint is taken once.
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
	d.writeRepository()
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
			fmt.Fprintf(d.s.stderr, "reale2e run-dev-full: runtime %s kept: %v\n", d.s.runtime, err)
		} else {
			rec.RuntimeRemoved = true
			d.s.ev.event(Event{Type: EvRuntimeRemoved})
		}
	}
	rec.Complete = rec.Complete && (rec.RuntimeRemoved || d.s.runtime == "")
	rec.FinishedAt = formatTime(d.s.w.Clock.Now())
	d.cleanupOK = rec.Complete
	dc := DevCleanup{CleanupRecord: rec, UnresolvedTasks: append([]string{}, unresolved...), OverBudget: overBudget, Deadline: formatTime(d.s.cleanupBy),
		LateAdmissions: append([]string{}, d.late...)}
	if scanErr != nil {
		dc.ScanError = contract.SafeText(scanErr.Error(), 400)
	}
	d.s.ev.writeJSON("cleanup.json", dc)
	d.s.ev.event(Event{Type: EvCleanupFinished})
	if !d.cleanupOK {
		d.stop("cleanup_incomplete", "cleanup could not prove every owned task settled and every owned process group gone")
		d.s.recovery(rec)
	}
	if d.s.before != nil {
		// A protected change, or a final fingerprint that could not be
		// compared completely, is never a clean execution.
		if diff := d.s.environmentDiff(); diff == nil || len(diff.Protected) > 0 || !diff.Complete || len(diff.Codes) > 0 {
			d.protected = true
		}
	}
	d.s.ev.writeJSON("ledger.json", d.state)
	ex := DevExecution{Schema: DevExecutionSchema, RunID: d.s.runID, StopCode: d.stopCode, StopReason: contract.SafeText(d.stopReason, 400),
		FlowLabel: d.flowLabel, FlowSHA256: d.loaded.FlowSum, SelectionSHA256: sha256Hex(d.selection), Preflights: len(d.s.preflight),
		Tasks: len(d.state.Tasks), Rejected: len(d.rejected), Transitions: len(d.state.Transitions), Decisions: d.decisions,
		Receipts: len(d.state.Receipts), Phases: len(d.phases), IgnoredLines: d.ignoredLines, ProtectedChange: d.protected,
		Cleanup: map[bool]string{true: "complete", false: "incomplete"}[d.cleanupOK]}
	if d.complete {
		ex.CandidateTask, ex.FinalCandidate = d.state.CandidateTask, d.state.CandidateCommit
		ex.ReviewTask, ex.FinalReview = d.state.ReviewTask, d.state.ReviewCommit
	}
	if err := d.s.ev.failure(); err != nil {
		ex.EvidenceError = contract.SafeText(err.Error(), 400)
	}
	ex.Outcome = d.outcome()
	if err := d.s.ev.writeJSON(DevExecutionFile, ex); err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e run-dev-full: %s not written: %v\n", DevExecutionFile, err)
	}
	if err := d.s.ev.failure(); err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e run-dev-full: evidence is incomplete: %v\n", err)
	}
	d.s.ev.close()
	d.s.say("development execution record: %s (%s)", filepath.Join(d.s.ev.root, DevExecutionFile), ex.Outcome)
}

// DevCleanup is the development cleanup.json.
type DevCleanup struct {
	CleanupRecord
	UnresolvedTasks []string `json:"unresolved_tasks"`
	OverBudget      bool     `json:"over_budget"`
	Deadline        string   `json:"deadline"`
	// ScanError is the final task listing's failure: the scan is then
	// incomplete and cleanup cannot be complete.
	ScanError string `json:"scan_error,omitempty"`
	// LateAdmissions are tasks first found by the final scan that were not
	// the exposed plan's admission; each fails the attempt.
	LateAdmissions []string `json:"late_admissions"`
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
// cleanup deadline and verifies its settlement. A task whose state cannot
// be read or that does not settle is unresolved; a failed final scan is
// returned as scanErr.
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
			continue
		}
		d.s.cancelTask(id)
		if _, ok := d.s.settle(id, d.s.left(30*time.Second)); !ok {
			unresolved = append(unresolved, id)
			d.s.unsettled = append(d.s.unsettled, id)
		}
	}
	return unresolved, scanErr
}

// listTaskIDs lists every task on the plane, all pages.
func (d *devSupervisor) listTaskIDs() ([]string, error) {
	var out []string
	after := ""
	for {
		ctx, cancel := d.s.opCtx()
		page, next, err := d.s.plane.ListTasks(ctx, after, contract.MaxTaskListLimit)
		cancel()
		if err != nil {
			return out, err
		}
		for _, t := range page {
			out = append(out, t.TaskID)
		}
		if next == nil {
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
	dir := "tasks/" + hopDir(sl.plan.Ordinal-1) + "/"
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
	if err := d.s.ev.writeJSON("workspace/tree-manifest.json", tm); err != nil {
		d.stop("evidence_write_failed", err.Error())
	}
}
