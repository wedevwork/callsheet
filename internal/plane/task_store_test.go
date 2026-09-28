package plane

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// restart stops the served plane and serves its root again with fresh
// deps (fast syncs), keeping the event log and hooks.
func (tp *taskPlane) restart(t *testing.T) {
	t.Helper()
	for _, w := range tp.workers {
		w.c.CloseNow()
	}
	tp.workers = nil
	tp.served.stop(t)
	d := fast(testDeps(t))
	d.streamEvents = tp.log.add
	d.roleHook = tp.hooks.fn
	d.taskHook = tp.th.fn
	d.fail = tp.inj.fail
	d.onTasks = func(ts *taskService) { tp.ts <- ts }
	tp.nodePlane = serveNodePlaneAt(t, d, tp.root)
}

// TestTaskStore is UT FP-1/6/9 for the durable task store; its states,
// durability and restore subtests are delegated from tests/function
// (TestTaskModel/states, TestTaskPersistence). Do not rename or skip
// them.
func TestTaskStore(t *testing.T) {
	t.Parallel()
	t.Run("states", func(t *testing.T) {
		t.Parallel()
		// Every state this build produces, persisted as one strict
		// document per publication with a monotonic revision; terminal
		// records are final.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 5), 1)}, idA)
		w := tp.worker(t, idA)
		// Output and result arrive before running is published, so the
		// writer's pass after running finds the captured result, whose
		// terminal commit outranks a checkpoint: exactly one publication
		// follows running (output alone, acknowledged after running,
		// could be checkpointed by that pass first).
		st, running := tp.runHeld(t, w, "a", "succeed", "p2")
		w.log(st, 0, []byte("line 1\nline 2\n"))
		msg := "fake task completed"
		w.resultAck(w.sendResult(result(st, 0, 14, &msg)), st.TaskID)
		commit := tp.th.arm("terminal-commit-queued", st.TaskID)
		running()
		tc := paused(t, commit, "terminal-commit-queued")
		pending := tp.taskFile(t, st.TaskID)
		if pending.State != contract.TaskRunning || pending.Revision != 2 || pending.StartedAt == nil || pending.FinishedAt != nil {
			t.Fatalf("running record %+v", pending)
		}
		close(tc.release)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		rec := tp.taskFile(t, st.TaskID)
		if rec.State != contract.TaskSucceeded || *rec.ExitCode != 0 || *rec.FinalMessage != msg || string(rec.Log.Data) != "line 1\nline 2\n" ||
			rec.Log.SourceBytes != 14 || rec.Revision != 3 || rec.FinishedAt == nil || rec.Execution.Epoch != tp.svc(t).epoch {
			t.Fatalf("succeeded record %+v", rec)
		}
		// failed by exit code and by signal (null exit code).
		f1 := tp.run(t, w, "a", "exit", "p3")
		tp.finish(t, w, f1, 7, 0)
		f2 := tp.run(t, w, "a", "signal", "p4")
		sig := "SIGKILL"
		w.resultAck(w.sendResult(contract.TaskResultBody{TaskID: f2.TaskID, Execution: f2.Execution, Outcome: contract.OutcomeNatural, Signal: &sig,
			LogIncomplete: true}.Sealed()), f2.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+f2.TaskID)
		if r := tp.taskFile(t, f1.TaskID); r.State != contract.TaskFailed || *r.ExitCode != 7 || r.Signal != nil {
			t.Fatalf("exit 7 record %+v", r)
		}
		if r := tp.taskFile(t, f2.TaskID); r.State != contract.TaskFailed || r.ExitCode != nil || *r.Signal != "SIGKILL" || !r.Log.Incomplete {
			t.Fatalf("signal record %+v", r)
		}
		// rejected: a definite refusal, with candidates sampled at the
		// terminal publication; no start time, exit or final message.
		v := tp.admit(t, taskReq(contract.TargetID, "a", "refused"))
		w.start("p5")
		w.answer("p5", v.TaskID, contract.TaskError(contract.CodeUnavailable, "", contract.ReasonLocalFull, "the worker is full"))
		tp.log.awaitOnce(t, "terminal-committed "+v.TaskID)
		r := tp.taskFile(t, v.TaskID)
		if r.State != contract.TaskRejected || r.Reason.Code != contract.ReasonLocalFull || r.StartedAt != nil || r.ExitCode != nil || len(r.Candidates) != 1 ||
			r.Candidates[0].RoleID != "a" || r.Revision != 2 {
			t.Fatalf("rejected record %+v", r)
		}
		sv := tp.show(t, v.TaskID)
		if sv.Result == nil || sv.Result.State != contract.TaskRejected || sv.Reason.Code != contract.ReasonLocalFull || len(sv.Candidates) != 1 {
			t.Fatalf("rejected view %+v", sv)
		}
		// Every document parses strictly and states are final.
		entries, _ := os.ReadDir(filepath.Join(tp.root, tasksName))
		for _, e := range entries {
			if !isTaskFile(e.Name()) {
				t.Fatalf("unexpected %s", e.Name())
			}
		}
		if len(entries) != 4 || tp.roleInflight(t, "a") != 0 {
			t.Fatalf("%d documents", len(entries))
		}
		before := taskBytes(t, tp.root, st.TaskID)
		w.beat()
		if !bytes.Equal(before, taskBytes(t, tp.root, st.TaskID)) {
			t.Fatal("a terminal document was rewritten")
		}
		// Checkpoints coalesce at most once per second while dirty and
		// never replace newer live bytes. Both writes are acknowledged
		// before running is published, so the writer's pass after running
		// takes the first checkpoint (due at once) with both; acknowledged
		// after running, that pass could checkpoint the first alone.
		c, running := tp.runHeld(t, w, "a", "checkpoint", "p6")
		w.log(c, 0, []byte("a\n"))
		w.log(c, 2, []byte("b\n"))
		running()
		tp.log.await(t, "published checkpoint "+c.TaskID)
		if r := tp.taskFile(t, c.TaskID); string(r.Log.Data) != "a\nb\n" || r.State != contract.TaskRunning {
			t.Fatalf("checkpoint %+v", r)
		}
		w.log(c, 4, []byte("c\n"))
		w.beat()
		if r := tp.taskFile(t, c.TaskID); string(r.Log.Data) != "a\nb\n" {
			t.Fatal("a second checkpoint within the same second")
		}
		if sv := tp.show(t, c.TaskID); sv.LogTail != "a\nb\nc\n" {
			t.Fatalf("live tail %q", sv.LogTail)
		}
		tp.finish(t, w, c, 0, 6)
		if r := tp.taskFile(t, c.TaskID); string(r.Log.Data) != "a\nb\nc\n" {
			t.Fatalf("terminal log %q", r.Log.Data)
		}
	})
	t.Run("durability", func(t *testing.T) {
		t.Parallel()
		t.Run("admission", func(t *testing.T) {
			t.Parallel()
			// Each step before visibility fails the dispatch with nothing
			// left behind; the first publication's directory creation
			// retries the root sync until it succeeds.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 9), 1)}, idA)
			tp.worker(t, idA)
			for _, c := range []struct{ op, prefix string }{
				{"mkdir", tasksName}, {"dirsync", rootName}, {"create", tasksName + "/"}, {"write", tasksName + "/"},
				{"sync", tasksName + "/"}, {"close", tasksName + "/"}, {"publish", tasksName + "/"},
			} {
				tp.inj.set(c.op, c.prefix)
				_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", c.op))
				if contract.CodeOf(err) != contract.CodeInternal || tp.inj.hits() == 0 {
					t.Fatalf("%s: %v (%d hits)", c.op, err, tp.inj.hits())
				}
				if n := tp.taskCount(t); n != 0 {
					t.Fatalf("%s created a task", c.op)
				}
				if left := tempLeftovers(t, tp.root); len(left) != 0 {
					t.Fatalf("%s left %v", c.op, left)
				}
			}
			tp.inj.set("", "")
			if v := tp.admit(t, taskReq(contract.TargetID, "a", "ok")); !v.DurabilityConfirmed {
				t.Fatal("unconfirmed after recovery")
			}
			if st := tp.svc(t).st; !st.parentSynced.Load() {
				t.Fatal("the root sync was skipped on retry")
			}
		})
		t.Run("overlapping-retries", func(t *testing.T) {
			t.Parallel()
			// Two directory-sync retries overlap. The later one confirms the
			// running revision it synced; the writer then publishes the
			// terminal revision, whose own sync fails. The earlier retry,
			// resuming after a sync that preceded that publication, must not
			// confirm it: the slot stays held and admissions blocked until a
			// sync after the terminal publication succeeds.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1), record(taskCfg("b", "coder", idA, 1), 2)}, idA)
			w := tp.worker(t, idA)
			v := tp.admit(t, taskReq(contract.TargetID, "a", "overlap"))
			st := w.start("p2")
			runHold := tp.th.arm("running-queued", v.TaskID)
			w.answer("p2", st.TaskID, nil)
			rc := paused(t, runHold, "running-queued")
			tp.inj.set("dirsync", tasksName)
			close(rc.release)
			tp.log.awaitOnce(t, "published running "+v.TaskID)
			termHold := tp.th.arm("terminal-commit-queued", v.TaskID)
			w.resultAck(w.sendResult(result(st, 0, 0, nil)), v.TaskID)
			tp.inj.set("", "")
			// Retry A (an admission) syncs, then pauses before confirming.
			syncA := tp.th.arm("resync-synced", "store")
			a := tp.dispatchAsync(taskReq(contract.TargetID, "b", "retry A"))
			callA := paused(t, syncA, "resync-synced")
			// Retry B (the sweep) confirms running; the terminal follows.
			tp.clk.Advance(storageRetry)
			tp.log.awaitOnce(t, "task-confirmed "+v.TaskID)
			ct := paused(t, termHold, "terminal-commit-queued")
			tp.inj.set("dirsync", tasksName)
			close(ct.release)
			tp.log.awaitOnce(t, "published terminal "+v.TaskID)
			close(callA.release)
			_, err := a.wait(t)
			if tp.log.seen("terminal-committed " + v.TaskID) {
				t.Fatal("an earlier sync confirmed the later, unsynced terminal revision")
			}
			if sv := tp.show(t, v.TaskID); sv.DurabilityConfirmed || tp.roleInflight(t, "a") != 1 {
				t.Fatalf("unsynced terminal %+v, inflight %d", sv, tp.roleInflight(t, "a"))
			}
			wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "b:storage_unconfirmed")
			// A sync after the terminal publication confirms it.
			tp.inj.set("", "")
			tp.clk.Advance(storageRetry)
			tp.log.awaitOnce(t, "terminal-committed "+v.TaskID)
			if tp.roleInflight(t, "a") != 0 {
				t.Fatal("the confirmed terminal kept its slot")
			}
		})
		t.Run("terminal", func(t *testing.T) {
			t.Parallel()
			// A terminal commit failing before its rename keeps the old
			// record authoritative, holds the slot, exposes the storage
			// condition and retries once per second; a failed directory
			// sync after the rename exposes the observed result unconfirmed
			// with the reservation held until a retried sync confirms it.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
			w := tp.worker(t, idA)
			st := tp.run(t, w, "a", "rename", "p2")
			tp.inj.set("rename", taskRel(st.TaskID))
			w.resultAck(w.sendResult(result(st, 0, 0, nil)), st.TaskID)
			tp.log.await(t, "publish-failed terminal "+st.TaskID)
			v := tp.show(t, st.TaskID)
			if v.State != contract.TaskRunning || !v.CompletionPending || *v.PersistenceReason != contract.ReasonResultStorageUnconfirmed || tp.roleInflight(t, "a") != 1 {
				t.Fatalf("failed commit %+v", v)
			}
			if r := tp.taskFile(t, st.TaskID); r.State != contract.TaskRunning {
				t.Fatal("the old record was replaced")
			}
			_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "blocked"))
			wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:storage_unconfirmed")
			tp.inj.set("", "")
			tp.clk.Advance(storageRetry)
			tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
			if v := tp.show(t, st.TaskID); v.State != contract.TaskSucceeded || v.PersistenceReason != nil || tp.roleInflight(t, "a") != 0 {
				t.Fatalf("retried commit %+v", v)
			}
			w.beat()
			st2 := tp.run(t, w, "a", "dirsync", "p3")
			tp.inj.set("dirsync", tasksName)
			w.resultAck(w.sendResult(result(st2, 3, 0, nil)), st2.TaskID)
			tp.log.awaitOnce(t, "published terminal "+st2.TaskID)
			v = tp.show(t, st2.TaskID)
			if v.State != contract.TaskFailed || v.DurabilityConfirmed || tp.roleInflight(t, "a") != 1 {
				t.Fatalf("unconfirmed terminal %+v (inflight %d)", v, tp.roleInflight(t, "a"))
			}
			if r := tp.taskFile(t, st2.TaskID); r.State != contract.TaskFailed {
				t.Fatal("the visible record is not the terminal one")
			}
			hits := tp.inj.hits()
			tp.clk.Advance(storageRetry)
			tp.log.await(t, "resync-failed")
			if tp.inj.hits() <= hits {
				t.Fatal("the directory sync was not retried")
			}
			tp.inj.set("", "")
			tp.clk.Advance(storageRetry)
			tp.log.awaitOnce(t, "task-confirmed "+st2.TaskID)
			if v := tp.show(t, st2.TaskID); !v.DurabilityConfirmed || tp.roleInflight(t, "a") != 0 {
				t.Fatalf("confirmed %+v", v)
			}
		})
	})
	t.Run("restore", func(t *testing.T) {
		t.Parallel()
		// A restart rebuilds reservations of pending and running records
		// before listening and marks them reconciling (iteration 06a),
		// keeps terminal records without reservations, and replays no
		// start. Loading rewrites nothing. Ordinary removal of an instance
		// holding a reservation stays busy. The returning worker's
		// complete inventory reconciles: its running execution continues,
		// its completed one sends its result, and the pending start it
		// never received is lost (execution_missing); then the instance's
		// remaining slot is usable and the resolved role removable; a
		// re-added role is a new instance without history.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 3), 1), record(taskCfg("b", "coder", idA, 1), 2)}, idA)
		w := tp.worker(t, idA)
		done := tp.run(t, w, "a", "done", "p2")
		w.log(done, 0, []byte("kept output\n"))
		tp.finish(t, w, done, 0, 12)
		running := tp.run(t, w, "a", "running", "p3")
		started := tp.clk.Now()
		w.log(running, 0, []byte("partial\n"))
		tp.clk.Advance(checkpointEvery)
		tp.log.await(t, "published checkpoint "+running.TaskID)
		w.beat()
		onB := tp.run(t, w, "b", "on b", "p4")
		// The pending task's start is written and never answered: the
		// control slot stays with it until the stream ends.
		pending := tp.admit(t, taskReq(contract.TargetID, "a", "pending"))
		w.start("p5")
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		files := map[string][]byte{}
		for _, id := range []string{done.TaskID, running.TaskID, pending.TaskID, onB.TaskID} {
			files[id] = taskBytes(t, tp.root, id)
		}
		tp.restart(t)
		for id, b := range files {
			if !bytes.Equal(b, taskBytes(t, tp.root, id)) {
				t.Fatalf("loading rewrote %s", id)
			}
		}
		for _, id := range []string{running.TaskID, pending.TaskID, onB.TaskID} {
			v := tp.show(t, id)
			if !v.Reconciling || !v.Log.LogMayBeIncomplete {
				t.Fatalf("restored %s = %+v", id, v)
			}
		}
		if v := tp.show(t, running.TaskID); v.State != contract.TaskRunning || v.LogTail != "partial\n" {
			t.Fatalf("restored running %+v", v)
		}
		if v := tp.show(t, done.TaskID); v.State != contract.TaskSucceeded || v.Reconciling || v.LogTail != "kept output\n" {
			t.Fatalf("restored terminal %+v", v)
		}
		if logs, err := tp.cl.TaskLogs(bg, done.TaskID); err != nil || string(logs.Data) != "kept output\n" {
			t.Fatalf("restored logs %v", err)
		}
		if n := tp.roleInflight(t, "a"); n != 2 {
			t.Fatalf("rebuilt a reservations = %d", n)
		}
		if err := rmRole(tp.cl, bg, "b", false); contract.CodeOf(err) != contract.CodeConflict {
			t.Fatalf("rm of an instance with a restored reservation: %v", err)
		}
		bres := result(onB, 0, 0, nil)
		w = tp.workerInv(t, idA, map[string]string{running.TaskID: contract.ActionContinue, onB.TaskID: contract.ActionSendResult},
			entry(running, contract.PhaseRunning, &started, nil), entry(onB, contract.PhaseResult, &started, &bres.Digest))
		tp.log.awaitOnce(t, "lost-latched "+pending.TaskID+" "+contract.ReasonExecutionMissing)
		tp.log.awaitOnce(t, "terminal-committed "+pending.TaskID)
		// The remaining unreserved slot of a is usable; no start replays.
		v := tp.admit(t, taskReq(contract.TargetID, "a", "fresh"))
		if s := w.start("p2"); s.TaskID != v.TaskID {
			t.Fatalf("replayed %s", s.TaskID)
		}
		w.answer("p2", v.TaskID, nil)
		tp.log.awaitOnce(t, "published running "+v.TaskID)
		// The completed execution's result, sent on the reconciled
		// attachment, is the task's terminal result; then b is removable.
		w.resultAck(w.sendResult(bres), onB.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+onB.TaskID)
		if !w.resultAck(w.sendResult(bres), onB.TaskID) {
			t.Fatal("the durable result was not acknowledged as committed")
		}
		if err := rmRole(tp.cl, bg, "b", false); err != nil {
			t.Fatalf("rm after resolution: %v", err)
		}
		w.ackReplace("p3")
		// Re-adding b is a new instance with a new order and no history.
		rv := tp.add(t, w.peer, "p4", "p5", taskCfg("b", "coder", idA, 1))
		if rv.RegistrationOrder != 3 || rv.Inflight != 0 {
			t.Fatalf("re-added b = %+v", rv)
		}
		tp.restart(t)
		if v := tp.show(t, onB.TaskID); v.State != contract.TaskSucceeded || v.Role.RegistrationOrder != 2 {
			t.Fatalf("history after re-add %+v", v)
		}
	})
	t.Run("validation", func(t *testing.T) {
		t.Parallel()
		// Corrupt or unsafe task state stops every loader (run, init,
		// status, reissue) before anything listens or is rewritten; stale
		// temporaries are ignored and a tasks/ directory without trust is
		// partial state.
		base := freshRoot(t)
		writeNodeRecord(t, base, idA+".json", encodeNodeRecord(idA, t0), 0o600)
		writeRegistry(t, base, docOf(1, 3, record(taskCfg("a", "coder", idA, 1), 1)))
		now := t0
		rec := contract.TaskRecord{TaskID: "t_00000000000000000000000000000001", Request: taskReq(contract.TargetID, "a", "g"),
			Role: record(taskCfg("a", "coder", idA, 1), 1), Effective: contract.TaskEffective{Model: "m", Effort: "low", Timeout: time.Hour},
			Execution: contract.ExecutionToken{Epoch: strings.Repeat("0", 32), Attachment: 1}, State: contract.TaskPending, CreatedAt: now, Revision: 1}
		good, _ := contract.EncodeTaskRecord(rec)
		write := func(root, name string, b []byte, mode os.FileMode) {
			os.MkdirAll(filepath.Join(root, tasksName), 0o700)
			if err := os.WriteFile(filepath.Join(root, tasksName, name), b, mode); err != nil {
				t.Fatal(err)
			}
			os.Chmod(filepath.Join(root, tasksName, name), mode)
		}
		load := func(root string) error {
			_, err := layout{root: root}.scan()
			if err == nil {
				_, _, err = layout{root: root}.loadState(roleLookup)
			}
			return err
		}
		other := rec
		other.Role = record(taskCfg("x", "coder", idA, 1), 1)
		otherB, _ := contract.EncodeTaskRecord(other)
		future := rec
		future.Role = record(taskCfg("a", "coder", idA, 1), 7)
		futureB, _ := contract.EncodeTaskRecord(future)
		for name, c := range map[string]struct {
			prep func(root string)
			code contract.Code
		}{
			"garbage":      {func(r string) { write(r, rec.TaskID+".json", []byte("{"), 0o600) }, contract.CodeConflict},
			"public mode":  {func(r string) { write(r, rec.TaskID+".json", good, 0o644) }, contract.CodeTrustFailed},
			"name":         {func(r string) { write(r, "t_00000000000000000000000000000002.json", good, 0o600) }, contract.CodeConflict},
			"unknown file": {func(r string) { write(r, "notes.txt", []byte("x"), 0o600) }, contract.CodeConflict},
			"subdir":       {func(r string) { os.MkdirAll(filepath.Join(r, tasksName, rec.TaskID+".json"), 0o700) }, contract.CodeConflict},
			"other role":   {func(r string) { write(r, rec.TaskID+".json", otherB, 0o600) }, contract.CodeConflict},
			"future order": {func(r string) { write(r, rec.TaskID+".json", futureB, 0o600) }, contract.CodeConflict},
			"symlink": {func(r string) {
				real := filepath.Join(r, "..", "elsewhere")
				os.MkdirAll(real, 0o700)
				os.Symlink(real, filepath.Join(r, tasksName))
			}, contract.CodeTrustFailed},
			"big": {func(r string) {
				// Sparse growth past the bound: the size alone refuses it.
				write(r, rec.TaskID+".json", good, 0o600)
				if err := os.Truncate(filepath.Join(r, tasksName, rec.TaskID+".json"), maxTaskFile+1); err != nil {
					t.Fatal(err)
				}
			}, contract.CodeConflict},
		} {
			root := filepath.Join(t.TempDir(), "state")
			copyTree(t, base, root)
			c.prep(root)
			if err := load(root); contract.CodeOf(err) != c.code {
				t.Fatalf("%s: %v, want %s", name, err, c.code)
			}
			if _, err := (&deps{now: fixed(t0)}).inspect(bg, root); contract.CodeOf(err) != c.code {
				t.Fatalf("%s status: %v", name, err)
			}
		}
		// A removed instance's history (its order vanished) is accepted,
		// as are stale temporaries, which are never promoted or deleted.
		root := filepath.Join(t.TempDir(), "state")
		copyTree(t, base, root)
		write(root, rec.TaskID+".json", good, 0o600)
		write(root, tempPrefix+"x", []byte("partial"), 0o600)
		writeRegistry(t, root, docOf(2, 3))
		_, tasks, err := layout{root: root}.loadState(roleLookup)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("history: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, tasksName, tempPrefix+"x")); err != nil {
			t.Fatal("a stale temporary was removed")
		}
		// tasks/ without trust is partial state, never an empty root.
		bare := filepath.Join(t.TempDir(), "state")
		os.MkdirAll(filepath.Join(bare, tasksName), 0o700)
		os.Chmod(bare, 0o700)
		if s, err := (layout{root: bare}).scan(); err != nil || s.kind != kindPartial {
			t.Fatalf("bare tasks/ = %+v %v", s, err)
		}
		if err := (layout{root: bare}).partialError(scanResult{tasksPresent: true, missing: durable}); !strings.Contains(err.Error(), "task history") {
			t.Fatalf("partial message %v", err)
		}
	})
}
