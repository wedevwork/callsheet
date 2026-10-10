//go:build realadaptercheck && (linux || darwin)

package sidecar

import (
	"context"
	"errors"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Design 12a-worker-selection's tagged sidecar checks (UT-4, UT-5),
// called from TestRealAdapterLocal/selection and /ordering: never a
// stress shard, zero OS processes (scripted probes and the counted
// injected guardians).

// scriptedVersionProbe is a vendor adapter whose probe runs nothing: it
// counts, then reports the scripted probe error (an older or malformed
// version, exactly as the adapter's probe words it) or passes (an eligible
// version, the minimum or a newer one).
type scriptedVersionProbe struct {
	adapter.Adapter
	n   *atomic.Int32
	err *atomic.Pointer[adapter.ProbeError]
}

func (s scriptedVersionProbe) Probe(context.Context, string) error {
	s.n.Add(1)
	if e := s.err.Load(); e != nil {
		return e
	}
	return nil
}

// wideEfforts is a vendor adapter whose descriptor also lists an effort
// outside the adapter's union (the metadata of a plane of another policy),
// so a delivered start can carry it past the contract decode to the
// worker's own selection check.
type wideEfforts struct{ adapter.Adapter }

func (w wideEfforts) Descriptor() adapter.Descriptor {
	d := w.Adapter.Descriptor()
	d.Efforts = append(d.Efforts, "turbo")
	return d
}

// records are cfgs as installed role records (orders 1..n).
func records(cfgs ...contract.RoleConfig) []contract.RoleRecord {
	out := make([]contract.RoleRecord, len(cfgs))
	for i, c := range cfgs {
		out[i] = contract.RoleRecord{RoleConfig: c, RegistrationOrder: i + 1}
	}
	return out
}

// wantCheckError requires the role check error of field and reason with
// exactly msg.
func wantCheckError(t *testing.T, err error, field, reason, msg string) {
	t.Helper()
	var ce *contract.Error
	if !errors.As(err, &ce) || ce.Code != contract.CodeInvalidArgument || ce.Details["field"] != field || ce.Details["reason"] != reason || ce.Message != msg {
		t.Fatalf("check error %v (%+v), want %s/%s %q", err, ce, field, reason, msg)
	}
}

// selectionLifecycle is UT-4 (FP-4) on vr's role environment with
// scripted version probes: alternate valid selections become ready;
// grammar and union failures refuse only their role, before any probe;
// each adapter is probed once per cycle and shared; a downgrade below the
// minimum (or a malformed version) makes exactly that adapter's roles
// unready and an update restores them on the next check, with no restart
// and no paid call.
func selectionLifecycle(t *testing.T, vr *vendorRun, ins, run string) {
	t.Helper()
	env := vr.super(t).env
	n := &atomic.Int32{}
	claudeErr, codexErr := &atomic.Pointer[adapter.ProbeError]{}, &atomic.Pointer[adapter.ProbeError]{}
	reg, err := adapter.NewRegistry(scriptedVersionProbe{adapter.NewClaude(""), n, claudeErr}, scriptedVersionProbe{adapter.NewCodex(""), n, codexErr})
	if err != nil {
		t.Fatal(err)
	}
	env.adapters = reg
	observed := vendorRole("claude", "a", ins, run)
	alt := observed
	alt.ID, alt.Model, alt.Effort = "b", "claude-opus-5-5", "high"
	codexAlt := vendorRole("codex", "c", ins, run)
	codexAlt.Model, codexAlt.Effort = "gpt-6-astra", "ultra"
	// A persisted role of another policy: its effort is outside the union.
	codexBad := vendorRole("codex", "d", ins, run)
	codexBad.Effort = "minimal"
	roles := records(observed, alt, codexAlt, codexBad)
	cycle := func(what string, want ...bool) {
		t.Helper()
		before := n.Load()
		if got := vr.d.checkCycle(bg, env, roles); !slices.Equal(got, want) || n.Load()-before != 2 {
			t.Fatalf("%s: cycle %v (%d probes), want %v and one probe per adapter", what, got, n.Load()-before, want)
		}
	}
	cycle("eligible versions", true, true, true, false)
	cycle("a repeated check", true, true, true, false)
	// Registration of the alternate pairs passes after one probe each.
	for _, c := range []contract.RoleConfig{alt, codexAlt} {
		before := n.Load()
		if err := vr.d.checkCandidate(bg, env, c); err != nil || n.Load()-before != 1 {
			t.Fatalf("%s %s/%s candidate: %v (%d probes)", c.Adapter, c.Model, c.Effort, err, n.Load()-before)
		}
	}
	// A Claude downgrade below the minimum: only the Claude roles fail, and
	// a candidate gets the probe failure envelope.
	claudeErr.Store(&adapter.ProbeError{Reason: "has an older claude version; minimum 2.1.285 (Claude Code)"})
	cycle("a claude downgrade", false, false, true, false)
	wantCheckError(t, vr.d.checkCandidate(bg, env, alt), "adapter", contract.ReasonProbeFailed,
		"the claude adapter cannot be invoked on this node: the adapter executable has an older claude version; minimum 2.1.285 (Claude Code)")
	// A malformed Codex version as well: every role is unready.
	codexErr.Store(&adapter.ProbeError{Reason: "has an invalid codex version; expected an orderable version at or above codex-cli 0.159.0"})
	cycle("both refused", false, false, false, false)
	// Updated executables are eligible again on the next check.
	claudeErr.Store(nil)
	codexErr.Store(nil)
	cycle("updated again", true, true, true, false)
	// Grammar and union failures: the offending field, probe_failed and a
	// static message, after the manuals and before any probe.
	for _, c := range []struct{ id, model, effort, field, msg string }{
		{"claude", "\t", "high", "model", "adapter: invalid model"},
		{"claude", "SECRET\n", "low", "model", "adapter: invalid model"},
		{"claude", "SECRET-MODEL", "ultra", "effort", "adapter: effort is not allowed for adapter claude; allowed: low, medium, high, xhigh, max"},
		{"codex", "gpt-6-astra", "", "effort", "adapter: effort is not allowed for adapter codex; allowed: low, medium, high, xhigh, max, ultra"},
		{"codex", "gpt-6-astra", "High", "effort", "adapter: effort is not allowed for adapter codex; allowed: low, medium, high, xhigh, max, ultra"},
	} {
		cfg := vendorRole(c.id, "x", ins, run)
		cfg.Model, cfg.Effort = c.model, c.effort
		before := n.Load()
		err := vr.d.checkCandidate(bg, env, cfg)
		wantCheckError(t, err, c.field, contract.ReasonProbeFailed, c.msg)
		if n.Load() != before || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("%s %q/%q: probed or reflected: %v", c.id, c.model, c.effort, err)
		}
	}
}

// checkOrder is UT-4's retained check order (/ordering): instruction,
// runbook, selection, then the probe (posture after it); an invalid
// selection stops before the probe and a missing manual before the
// selection.
func checkOrder(t *testing.T, vr *vendorRun, ins, run string) {
	t.Helper()
	var seen []string
	d := testDeps(nil)
	d.observeCheck = func(kind, name string) { seen = append(seen, kind) }
	env := vr.super(t).env
	alt := vendorRole("codex", "o", ins, run)
	alt.Model, alt.Effort = "gpt-6-astra", "max"
	if err := d.checkCandidate(bg, env, alt); err != nil || !slices.Equal(seen, []string{"manual", "manual", "probe"}) {
		t.Fatalf("valid alternate: %v, checks %v", err, seen)
	}
	seen = nil
	bad := alt
	bad.Effort = "none"
	if err := d.checkCandidate(bg, env, bad); err == nil || !slices.Equal(seen, []string{"manual", "manual"}) {
		t.Fatalf("invalid effort: %v, checks %v", err, seen)
	}
	seen = nil
	bad.Instruction = "/nonexistent/instruction.md"
	wantCheckError(t, d.checkCandidate(bg, env, bad), "instruction", contract.ReasonManualUnreadable,
		"the instruction manual is not readable on this node: it does not exist (or is a broken symbolic link)")
	if !slices.Equal(seen, []string{"manual"}) {
		t.Fatalf("missing manual: checks %v", seen)
	}
}

// overrideStarts is UT-5 (FP-5) on a codex role: the plane's resolution
// (absent override fields inherit, an explicit empty or out-of-union value
// is refused), a delivered alternate effective selection that runs exactly
// once, the next task back on the role's values, and a delivered invalid
// selection (a plane of another policy) refused start_failed before any
// child with the renamed safe diagnostic.
func overrideStarts(t *testing.T) {
	t.Helper()
	fp := startFakePlane(t)
	vr := startVendorRun(t, fp, runtime.GOOS, nil, nil)
	ins, run := manuals(t, vr.dir, "a", "m")
	s := vr.connect(t, 1, 1, vendorRole("codex", "a", ins, run))
	role := contract.RoleRecord{RoleConfig: s.cfgs[0], RegistrationOrder: 1}
	str := func(v string) *string { return &v }
	for _, o := range []*contract.TaskOverride{nil, {}} {
		if eff, err := contract.ResolveEffective(role, o, adapter.Lookup()); err != nil || eff.Model != "gpt-6.1-sol" || eff.Effort != "low" {
			t.Fatalf("absent override %+v: %+v %v", o, eff, err)
		}
	}
	for field, o := range map[string]*contract.TaskOverride{"model": {Model: str("")}, "effort": {Effort: str("minimal")}} {
		if _, err := contract.ResolveEffective(role, o, adapter.Lookup()); contract.CodeOf(err) != contract.CodeInvalidArgument || !strings.Contains(err.Error(), field) {
			t.Fatalf("invalid %s override: %v", field, err)
		}
	}
	alt, err := contract.ResolveEffective(role, &contract.TaskOverride{Model: str("gpt-6-astra"), Effort: str("xhigh")}, adapter.Lookup())
	if err != nil || alt.Model != "gpt-6-astra" || alt.Effort != "xhigh" || alt.Timeout != role.Resolved().Timeout {
		t.Fatalf("alternate override %+v %v", alt, err)
	}
	launch := func(n int, eff contract.TaskEffective, model, effort string) {
		t.Helper()
		b := startBody(n, s.cfgs[0], 1, s.gen, "override")
		b.Effective = eff
		rid := s.nextP()
		s.c.sendStart(rid, b)
		if r := s.c.startResult(rid); r.Err != nil {
			t.Fatalf("start %d refused: %v", n, r.Err)
		}
		ch := vr.child(t)
		vr.ev.awaitMatch(t, evStartReplied, func(ev event) bool { return ev.id == b.TaskID })
		want := []string{"-a", "never", "exec", "--model", model, "-c", "model_reasoning_effort=\"" + effort + "\"", "--json", "--skip-git-repo-check",
			"--output-last-message", finalPathOf(t, ch), "-"}
		if !slices.Equal(ch.spec.argv, want) {
			t.Fatalf("task %d argv %q, want %q", n, ch.spec.argv, want)
		}
		os.WriteFile(finalPathOf(t, ch), []byte("pong"), 0o600)
		ch.exitCode(0)
		if res, _ := s.drain(t, b); strOf(res.FinalMessage) != "pong" {
			t.Fatalf("task %d result %+v", n, res)
		}
	}
	launch(1, alt, "gpt-6-astra", "xhigh")
	launch(2, contract.TaskEffective{Model: "gpt-6.1-sol", Effort: "low", Timeout: alt.Timeout}, "gpt-6.1-sol", "low")

	// A delivered selection outside the worker's union: definite
	// start_failed, no child, no scratch, the renamed diagnostic without
	// the submitted values, and the slot released.
	fp = startFakePlane(t)
	n := &atomic.Int32{}
	exe := fakeExeFile(t)
	tr := startTaskRun(t, fp, taskOpts{adapters: func(string) adapter.Registry {
		r, err := adapter.NewRegistry(wideEfforts{countingProbe{adapter.NewCodex(""), n}}, countingProbe{adapter.NewFake(""), n})
		if err != nil {
			panic(err)
		}
		return r
	}, options: func(o *RunOptions) { o.CodexAdapterPath = exe }})
	ins, run = manuals(t, tr.dir, "a", "m")
	ws := tr.connect(t, 1, 1, vendorRole("codex", "a", ins, run))
	for i := range 2 {
		b := startBody(10+i, ws.cfgs[0], 1, ws.gen, "invalid delivered selection")
		b.Effective.Model, b.Effective.Effort = "SECRET-MODEL-TEXT", "turbo"
		rid := ws.nextP()
		ws.c.sendStart(rid, b)
		r := ws.c.startResult(rid)
		if r.Err == nil || r.Err.Details["reason"] != contract.ReasonStartFailed || r.Preparing {
			t.Fatalf("invalid delivered selection = %+v", r)
		}
		tr.ev.awaitMatch(t, evTaskRefused, func(ev event) bool { return ev.id == b.TaskID })
		tr.noChild(t)
		logs := tr.logs.String()
		if !hasLog(logs, "task model/effort selection invalid", "task_id", b.TaskID) || !hasLog(logs, "task model/effort selection invalid", "reason", "selection_invalid") ||
			strings.Contains(logs, "SECRET-MODEL-TEXT") || strings.Contains(logs, "turbo") || strings.Contains(logs, "selection_not_qualified") {
			t.Fatalf("invalid selection diagnostic:\n%s", logs)
		}
		if entries, _ := os.ReadDir(tr.tmp); len(entries) != 0 {
			t.Fatalf("a refused start left scratch %v", entries)
		}
	}
	// The adapter's own Invocation refuses it as well (defense in depth).
	if inv, err := adapter.NewCodex("").Invocation(adapter.TaskInput{TaskID: taskID(1), Model: "m", Effort: "turbo", ScratchDir: "/s"}); err == nil || inv.Argv != nil {
		t.Fatalf("codex invocation of an invalid effort %+v %v", inv, err)
	}
}
