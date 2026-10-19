package reale2e

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Refusal and fault paths of the development supervisor's loop, owner gate
// and evidence (design 12a-dev-flow-harness, FP-3, FP-5 and FP-7): hostile
// or out-of-order control requests, plane and stream failures, tampered
// receipts and presented copies, and evidence write failures. Requests are
// served through the loop's own serve, on the loop goroutine.

func TestDevFullRefusals(t *testing.T) {
	t.Run("nonfatal", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.owner = []string{"maybe\nno 1"}
		got := map[string]string{}
		r.onTick = func(r *devRig) {
			d := r.d
			sl := d.slot
			switch {
			case sl != nil && sl.plan.Ordinal == 1 && sl.exposed && sl.matched == "" && got["missing"] == "":
				got["missing"] = d.serve(ControlRequest{Op: opDevObserve, Task: hexID("t_", 999), Ordinal: "1"}).Code
				d.s.plane.(*fakePlane).showErr = func(string) error { return errors.New("transport") }
				got["show_error"] = d.serve(ControlRequest{Op: opDevObserve, Task: hexID("t_", 998), Ordinal: "1"}).Code
				d.s.plane.(*fakePlane).showErr = nil
			case sl != nil && sl.plan.Ordinal == 1 && sl.observed && !sl.waitStarted && got["again"] == "":
				got["again"] = d.serve(ControlRequest{Op: opDevObserve, Task: sl.matched, Ordinal: "1"}).Code
				got["bogus"] = d.serve(ControlRequest{Op: "bogus"}).Code
				got["ordinal"] = d.serve(ControlRequest{Op: opDevObserve, Task: sl.matched, Ordinal: "x"}).Code
				got["other_slot"] = d.serve(ControlRequest{Op: opDevObserve, Task: sl.matched, Ordinal: "2"}).Code
				got["wait_other"] = d.serve(ControlRequest{Op: opDevWait, Task: hexID("t_", 997), Ordinal: "1"}).Code
				got["next"] = d.serve(ControlRequest{Op: opDevNext}).Code
			case d.gate != nil && got["paused"] == "":
				resp := d.serve(ControlRequest{Op: opDevNext})
				got["paused"] = resp.Code + " " + resp.Summary
			case d.window != nil && got["window"] == "":
				got["window"] = d.serve(ControlRequest{Op: opDevNext}).Code
				got["recorded"] = d.serve(ControlRequest{Op: opDevObserve, Task: d.state.Tasks[0].TaskID, Ordinal: "1"}).Code
			}
		}
		r.expect(1, "owner_no")
		want := map[string]string{"missing": CodeTaskMismatch, "show_error": CodeInternalError, "again": CodeAlreadyObserved, "bogus": CodeInvalidRequest,
			"ordinal": CodeInvalidRequest, "other_slot": CodeTaskMismatch, "wait_other": CodeTaskMismatch, "next": CodePending,
			"paused": CodePaused + " gate=owner generation=1", "window": CodeOwnerGateClosed, "recorded": CodeAlreadyObserved}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q want %q", k, got[k], v)
			}
		}
		if !strings.Contains(r.f.stdout.String(), "not a decision for generation 1") {
			t.Fatal("an invalid owner line was not reported")
		}
		if r.d.serve(ControlRequest{Op: opDevNext}).Code != CodeClosing {
			t.Fatal("a stopped supervisor answered")
		}
	})
	for name, c := range map[string]struct {
		steps []devAct
		fire  func(r *devRig) (string, bool)
		code  string
		stop  string
	}{
		"conflict_observed": {happy(), func(r *devRig) (string, bool) {
			sl := r.d.slot
			if sl == nil || !sl.observed {
				return "", false
			}
			return r.d.serve(ControlRequest{Op: opDevObserve, Task: hexID("t_", 996), Ordinal: itoa(sl.plan.Ordinal)}).Code, true
		}, CodeObservationClash, "observation_conflict"},
		"conflict_record": {ownerNoSteps(), func(r *devRig) (string, bool) {
			if r.d.slot == nil || r.d.slot.plan.Ordinal != 2 {
				return "", false
			}
			return r.d.serve(ControlRequest{Op: opDevObserve, Task: r.d.state.Tasks[0].TaskID, Ordinal: "2"}).Code, true
		}, CodeObservationClash, "observation_conflict"},
		"preflight_task": {happy(), func(r *devRig) (string, bool) {
			sl := r.d.slot
			if sl == nil || !sl.exposed || sl.matched != "" {
				return "", false
			}
			return r.d.serve(ControlRequest{Op: opDevObserve, Task: r.d.s.preflight[0], Ordinal: "1"}).Code, true
		}, CodeObservationClash, "observation_conflict"},
		"premature": {happy(), func(r *devRig) (string, bool) {
			sl := r.d.slot
			if sl == nil || sl.exposed {
				return "", false
			}
			ws, inst, base := r.d.s.workspace.Name, r.d.s.workspace.Instance, sl.plan.ExpectedBase
			v := r.mustDispatch(contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: sl.plan.Role}, Goal: sl.plan.Goal,
				Acceptance: sl.plan.Acceptance, Payload: []string{}, RequestedBy: requester, Workspace: &ws, Base: &base, WorkspaceInstance: &inst})
			return r.d.serve(ControlRequest{Op: opDevObserve, Task: v.TaskID, Ordinal: "1"}).Code, true
		}, CodeTaskMismatch, "premature_admission"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, c.steps)
			r.owner = []string{"yes 1"}
			got, fired := "", false
			r.onTick = func(r *devRig) {
				if !fired {
					got, fired = c.fire(r)
				}
			}
			r.expect(1, c.stop)
			if got != c.code {
				t.Fatalf("code %q want %q", got, c.code)
			}
		})
	}
	t.Run("binding", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.fault = "bad-acceptance"
		r.expect(1, "binding_rejected")
		if r.terminalState(r.d.rejected[0]) != contract.TaskCancelled {
			t.Fatal("the misbound task was not cancelled")
		}
	})
}

func TestDevFullFaults(t *testing.T) {
	type tc struct {
		steps []devAct
		setup func(r *devRig)
		stop  string
	}
	firstObserved := func(f func(r *devRig)) func(r *devRig) {
		done := false
		return func(r *devRig) {
			r.onTick = func(r *devRig) {
				if !done && r.d.slot != nil && r.d.slot.observed {
					done = true
					f(r)
				}
			}
		}
	}
	for name, c := range map[string]tc{
		"list_error": {happy(), firstObserved(func(r *devRig) { r.f.plane.listErr = errors.New("page lost") }), "task_scan_failed"},
		"show_error": {happy(), firstObserved(func(r *devRig) {
			id := r.d.slot.matched
			r.f.plane.showErr = func(t string) error {
				if t == id {
					return errors.New("transport")
				}
				return nil
			}
		}), "task_scan_failed"},
		"duration": {happy(), func(r *devRig) {
			r.onTick = func(r *devRig) { r.f.plane.finishSkew = FeatureTaskBound }
		}, "task_timeout"},
		"failed":       {[]devAct{{marker: markerDesignDraft, alter: "failed"}}, nil, "task_failed"},
		"truncated":    {[]devAct{{marker: markerDesignDraft, alter: "truncated"}}, nil, "final_message_missing"},
		"bad_final":    {[]devAct{{marker: markerDesignDraft, alter: "bad-final"}}, nil, "invalid_marker"},
		"pull":         {happy(), func(r *devRig) { r.pullErr = errors.New("transfer refused") }, "publication_rejected"},
		"wrong_parent": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved, alter: "wrong-parent"}}, nil, "publication_rejected"},
		"exit_code":    {ownerNoSteps(), func(r *devRig) { r.phaseExit = 1 }, "coordinator_exit_nonzero"},
		"no_result":    {ownerNoSteps(), func(r *devRig) { r.noResult = true }, devResultFailed},
		"wrong_gate":   {ownerNoSteps(), func(r *devRig) { r.fault = "wrong-gate" }, "phase_result"},
		"group_alive":  {ownerNoSteps(), func(r *devRig) { r.fault = "linger1" }, "coordinator_group_alive"},
		"status_error": {happy(), func(r *devRig) { r.f.plane.statusErr = errors.New("status unavailable") }, "seed_unverified"},
		"seed_mismatch": {happy(), func(r *devRig) {
			r.fault = "no-push"
			r.onTick = func(r *devRig) {
				if r.d.current() != nil && !r.seeded {
					r.seeded = true
					r.f.plane.pushSeed(commitHash(commitOf("other")))
				}
			}
		}, "seed_mismatch"},
		"presented_copy": {happy(), func(r *devRig) {
			r.onTick = func(r *devRig) {
				if r.d.window != nil && !r.answered[r.d.window] {
					p := filepath.Join(r.d.s.runtime, "review", "generations", "01", "design.md")
					os.Chmod(p, 0o600)
					os.WriteFile(p, []byte("tampered\n"), 0o600)
				}
			}
		}, "decision_binding_changed"},
		"receipt_file": {happy(), func(r *devRig) {
			done := false
			r.onTick = func(r *devRig) {
				if !done && r.d.slot != nil && r.d.slot.plan.Role == "coder" && !r.d.slot.exposed {
					done = true
					p := filepath.Join(r.d.s.runtime, "receipts", "01.json")
					os.Chmod(p, 0o600)
					os.WriteFile(p, []byte("{}"), 0o600)
				}
			}
		}, "stale_receipt"},
		"decision_evidence": {happy(), nil, ""},
		"presented_dir": {happy(), atGate(func(r *devRig) {
			os.MkdirAll(filepath.Join(r.d.s.runtime, "review", "generations", "01"), 0o700)
		}), "decision_binding_changed"},
		"decision_runtime": {happy(), atGate(func(r *devRig) {
			os.WriteFile(filepath.Join(r.d.s.runtime, "decisions", "01.json"), []byte("x"), 0o600)
		}), "runtime_unavailable"},
		"receipt_runtime": {happy(), atGate(func(r *devRig) {
			os.WriteFile(filepath.Join(r.d.s.runtime, "receipts", "01.json"), []byte("x"), 0o600)
		}), "runtime_unavailable"},
		"receipt_evidence": {happy(), atGate(func(r *devRig) {
			os.MkdirAll(filepath.Join(r.f.evidence, "receipts"), 0o700)
			os.WriteFile(filepath.Join(r.f.evidence, "receipts", "01.json"), []byte("x"), 0o600)
		}), "evidence_write_failed"},
		"interrupt": {happy(), func(r *devRig) {
			ctx, cancel := context.WithCancel(context.Background())
			r.f.ctx = ctx
			r.onTick = func(r *devRig) {
				if r.d.slot != nil && r.d.slot.observed {
					cancel()
				}
			}
		}, "interrupted"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, c.steps)
			r.owner = []string{"yes 1"}
			if c.setup != nil {
				c.setup(r)
			}
			if name == "decision_evidence" {
				// The decision's evidence path is taken once the window opens.
				r.onTick = func(r *devRig) {
					if r.d.window != nil && !r.answered[r.d.window] {
						os.MkdirAll(filepath.Join(r.f.evidence, "decisions"), 0o700)
						os.WriteFile(filepath.Join(r.f.evidence, "decisions", "01.json"), []byte("{}"), 0o600)
					}
				}
				c.stop = "evidence_write_failed"
			}
			r.expect(1, c.stop)
			if r.d.passed {
				t.Fatal("a fault executed")
			}
		})
	}
	t.Run("eof_between_windows", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerQuestions},
			{marker: markerDesignDraft}, {marker: markerDesignApproved}})
		r.owner = []string{"yes 1"}
		r.early = []string{""}
		r.expect(1, "owner_eof")
		if len(r.d.state.Receipts) != 1 || r.taskCount() != 5 {
			t.Fatal("work continued after the terminal closed")
		}
	})
}

// atGate runs f once, when the supervisor first holds an owner gate.
func atGate(f func(r *devRig)) func(r *devRig) {
	return func(r *devRig) {
		done := false
		r.onTick = func(r *devRig) {
			if !done && r.d.gate != nil {
				done = true
				f(r)
			}
		}
	}
}

// commitHash parses a 40-hex commit.
func commitHash(s string) (h [20]byte) {
	for i := 0; i < 20; i++ {
		var b byte
		for _, c := range s[2*i : 2*i+2] {
			b <<= 4
			switch {
			case c >= '0' && c <= '9':
				b |= byte(c - '0')
			default:
				b |= byte(c-'a') + 10
			}
		}
		h[i] = b
	}
	return h
}

func TestDevFullEvidenceEdges(t *testing.T) {
	st := memory.NewStorage()
	big := make([]byte, maxRepoFileBytes+1<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	root, _ := writeCommit(t, st, map[string][]byte{"blob.bin": big})
	if err := writeDevEvidenceRepo(filepath.Join(t.TempDir(), "repo.git"), st, root, nil); !errors.Is(err, errDevRepoLimit) {
		t.Fatalf("an oversized evidence repository: %v", err)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocked, []byte("x"), 0o600)
	if err := writeDevEvidenceRepo(filepath.Join(blocked, "repo.git"), st, root, nil); err == nil {
		t.Fatal("an unwritable evidence repository")
	}
	if _, err := jsonContentLen([]byte{0xff}); err == nil {
		t.Fatal("invalid UTF-8 content")
	}
	if err := checkTriageManualBudget([]byte("a"), []byte{0xff}); err == nil {
		t.Fatal("an invalid runbook")
	}
	if err := checkTriageManualBudget([]byte{0xff}, []byte("a")); err == nil {
		t.Fatal("an invalid instruction")
	}
	if err := checkTriageTaskBudget("\xff", ""); err == nil {
		t.Fatal("an invalid goal")
	}
	if err := checkTriageTaskBudget("g", "\xff"); err == nil {
		t.Fatal("an invalid acceptance")
	}
	if err := checkGoalBudget("\xff"); err == nil {
		t.Fatal("an invalid goal")
	}
	d := &devSupervisor{s: &supervisor{}, trees: map[string]map[string]TreeFile{}, finals: map[string]string{}}
	st2, _ := InitialPlan(commitOf("seed"))
	if _, _, _, err := d.envelope(st2.Pending); codeOf(err) != "input_unavailable" {
		t.Fatalf("an unadmitted base: %v", err)
	}
	d.trees[commitOf("seed")] = map[string]TreeFile{}
	p := *st2.Pending
	p.Inputs = []PlanInput{{Kind: "candidate", Commit: commitOf("missing")}}
	if _, _, _, err := d.envelope(&p); codeOf(err) != "input_unavailable" {
		t.Fatalf("a missing candidate: %v", err)
	}
	p.Inputs = []PlanInput{{Kind: "escalation", Task: "t_x"}}
	if _, _, _, err := d.envelope(&p); codeOf(err) != "input_unavailable" {
		t.Fatalf("a missing final answer: %v", err)
	}
	if devAcceptance(&TaskPlan{Role: "x"}) == "" || fpSubset("triage") == "" || evidenceLocation("designer") == "" {
		t.Fatal("fallback texts")
	}
}
