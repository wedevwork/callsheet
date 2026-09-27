package plane

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
)

// expectClosed requires the plane to close the peer's stream with 1008.
func expectClosed(t *testing.T, w *worker, rid string) {
	t.Helper()
	for {
		f := w.recv()
		if f.Type == contract.FrameError {
			if f.RequestID != rid {
				t.Fatalf("error for %s, want %s", f.RequestID, rid)
			}
			break
		}
	}
	if st := w.closed(); st != websocket.StatusPolicyViolation {
		t.Fatalf("close = %v", st)
	}
}

// TestTaskStream is UT FP-3/9 for the plane's task frames; its duplex,
// bounds, result-receipt and result-ack-loss subtests are delegated from
// tests/function (TestTaskProtocol), together with the sidecar's. Do not
// rename or skip its subtests.
func TestTaskStream(t *testing.T) {
	t.Parallel()
	t.Run("duplex", func(t *testing.T) {
		t.Parallel()
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
		w := tp.worker(t, idA)
		v := tp.admit(t, taskReq(contract.TargetID, "a", "one"))
		st := w.start("p2")
		// Full duplex: with the plane's start in flight, the worker's
		// heartbeat is acknowledged at once on the shared b sequence.
		w.beat()
		w.answer("p2", st.TaskID, nil)
		tp.log.awaitOnce(t, "published running "+st.TaskID)
		if sv := tp.show(t, v.TaskID); sv.State != contract.TaskRunning || sv.StartedAt == nil || sv.RecoveryRequired {
			t.Fatalf("running view %+v", sv)
		}
		// Output and result share the b sequence with heartbeats.
		if next := w.log(st, 0, []byte("hello\n")); next != 6 {
			t.Fatalf("next offset %d", next)
		}
		w.beat()
		// An entirely duplicate range is acknowledged without appending.
		if next := w.log(st, 0, []byte("hello\n")); next != 6 {
			t.Fatalf("duplicate next offset %d", next)
		}
		if next := w.log(st, 6, []byte("world\n")); next != 12 {
			t.Fatalf("next offset %d", next)
		}
		msg := "done"
		res := result(st, 0, 12, &msg)
		rid := w.sendResult(res)
		w.resultAck(rid, st.TaskID)
		// An identical resubmission on the same attachment re-acknowledges.
		w.resultAck(w.sendResult(res), st.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		sv := tp.show(t, v.TaskID)
		if sv.State != contract.TaskSucceeded || *sv.Result.ExitCode != 0 || *sv.Result.FinalMessage != "done" || sv.LogTail != "hello\nworld\n" || sv.CompletionPending {
			t.Fatalf("terminal view %+v", sv)
		}
		// Terminal: later callbacks change nothing; a different result is a
		// protocol error that closes the stream, never a mutation.
		other := result(st, 3, 12, nil)
		bad := w.sendResult(other)
		expectClosed(t, w, bad)
		if again := tp.show(t, v.TaskID); again.State != contract.TaskSucceeded || *again.Result.ExitCode != 0 {
			t.Fatalf("a late callback mutated the task: %+v", again)
		}
		// Fair arbitration: a queued start and a dirty snapshot alternate.
		w = tp.worker(t, idA)
		tp.admit(t, taskReq(contract.TargetID, "a", "two"))
		st2 := w.start("p2")
		name := "renamed"
		res2 := tp.setAsync(bg, "a", contract.RolePatch{Name: &name})
		w.answer("p2", st2.TaskID, nil)
		tp.log.awaitOnce(t, "published running "+st2.TaskID)
		w.validateOK("p3")
		if _, err := res2.wait(t); err != nil {
			t.Fatal(err)
		}
		if b := w.ackReplace("p4"); b.Roles[0].Name != "renamed" {
			t.Fatalf("snapshot %+v", b)
		}
		// Fencing: output for another task, a stale token or a skipped b
		// number closes the stream without touching any task.
		stale := st2
		stale.Execution.Attachment = 1
		expectClosed(t, w, w.sendLog(stale, 0, []byte("x")))
		w = tp.worker(t, idA)
		w.b++ // skip one number
		expectClosed(t, w, w.sendLog(st2, 0, []byte("x")))
		if sv := tp.show(t, st2.TaskID); sv.State != contract.TaskRunning || sv.Log.RetainedBytes != 0 || !sv.RecoveryRequired {
			t.Fatalf("fenced output reached the task: %+v", sv)
		}
	})
	t.Run("bounds", func(t *testing.T) {
		t.Parallel()
		t.Run("start-deadline", func(t *testing.T) {
			t.Parallel()
			// A reply read strictly before the four-second start deadline
			// counts; at the deadline the in-flight start expires: the stream
			// closes and the task stays pending, uncertain, slot held.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
			w := tp.worker(t, idA)
			v := tp.admit(t, taskReq(contract.TargetID, "a", "early"))
			tp.log.awaitOnce(t, "start-sent "+idA+" p2 "+v.TaskID)
			st := w.start("p2")
			tp.clk.Advance(startControl - time.Nanosecond)
			w.answer("p2", st.TaskID, nil)
			tp.log.awaitOnce(t, "task-started "+st.TaskID)
			w.beat()
			v2 := tp.admit(t, taskReq(contract.TargetID, "a", "late"))
			tp.log.awaitOnce(t, "start-sent "+idA+" p3 "+v2.TaskID)
			w.start("p3")
			tp.clk.Advance(startControl)
			tp.log.await(t, "request-expired "+idA+" p3")
			if st := w.closed(); st != websocket.StatusPolicyViolation {
				t.Fatalf("close = %v", st)
			}
			sv := tp.show(t, v2.TaskID)
			if sv.State != contract.TaskPending || !sv.RecoveryRequired || *sv.RecoveryReason != contract.RecoveryStartUnconfirmed || tp.roleInflight(t, "a") != 2 {
				t.Fatalf("expired start %+v", sv)
			}
		})
		t.Run("unsent", func(t *testing.T) {
			t.Parallel()
			// A queued start whose deadline passes before any write is
			// withdrawn as definitely unsent (rejected start_not_sent, slot
			// released) and the healthy stream stays attached.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
			w := tp.worker(t, idA)
			tp.inj.set("dirsync", tasksName)
			v, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "delayed"))
			if err != nil || v.DurabilityConfirmed {
				t.Fatalf("unconfirmed dispatch = %+v %v", v, err)
			}
			tp.inj.set("", "")
			// The deadline is anchored at the first visible publication and
			// never resets for durability confirmation.
			tp.clk.Advance(startControl)
			tp.log.awaitOnce(t, "task-confirmed "+v.TaskID)
			tp.log.awaitOnce(t, "terminal-committed "+v.TaskID)
			sv := tp.show(t, v.TaskID)
			if sv.State != contract.TaskRejected || sv.Reason.Code != contract.ReasonStartNotSent || len(sv.Candidates) != 1 || sv.StartedAt != nil || tp.roleInflight(t, "a") != 0 {
				t.Fatalf("unsent start %+v", sv)
			}
			w.beat()
			if tp.log.seenPrefix("start-writing " + idA) {
				t.Fatal("an expired start was written")
			}
			// Its transport token went back with it: the healthy attachment
			// holds none (a leak would make it look full over time).
			if n := tp.startTokens(t, idA); n != 0 {
				t.Fatalf("the expired unsent start leaked %d transport token(s)", n)
			}
			// The queue itself: expired entries are withdrawn in order and
			// their tokens returned; a later one stays queued.
			ts := tp.svc(t)
			sn := &nodeStream{svc: &nodeService{tasks: ts, clock: tp.clk}, nodeID: idA, kick: make(chan struct{}, 1)}
			now := tp.clk.Now()
			for i, d := range []time.Duration{-time.Second, 0, time.Second} {
				res := sn.reserveStart()
				if res == nil || !sn.enqueueStart(&startItem{id: "t_" + strings.Repeat(strconv.Itoa(i), 32), deadline: now.Add(d), res: res}) {
					t.Fatal("enqueue refused")
				}
			}
			it, next := sn.takeStart(now, true)
			if it == nil || it.deadline != now.Add(time.Second) || !next.IsZero() || sn.tokens != 1 {
				t.Fatalf("takeStart = %+v %v tokens %d", it, next, sn.tokens)
			}
			for i := 0; i < maxQueuedStarts; i++ {
				sn.reserveStart()
			}
			if sn.reserveStart() != nil {
				t.Fatal("more than 100 pending starts per node")
			}
			// Release is exactly once per reservation.
			it.res.release()
			it.res.release()
			if sn.tokens != maxQueuedStarts-1 {
				t.Fatalf("tokens after a double release: %d", sn.tokens)
			}
			// A released reservation keeps no stream alive: a task entry
			// holds its reservation for its lifetime (terminal included),
			// while the attachment it was reserved on retires with its
			// socket and inbox. No goroutine refers to this stream.
			retired := &nodeStream{svc: &nodeService{tasks: ts, clock: tp.clk}, nodeID: idA, inbox: newInbox(tp.clk)}
			entry := &taskEntry{res: retired.reserveStart()}
			entry.res.release()
			wp := weak.Make(retired)
			retired = nil
			runtime.GC()
			if wp.Value() != nil {
				t.Fatal("a released reservation retains its retired stream through the task entry")
			}
			runtime.KeepAlive(entry)
		})
		t.Run("anchor", func(t *testing.T) {
			t.Parallel()
			// The start deadline is four seconds from the link that made the
			// pending record visible: a slow directory sync after the link,
			// successful or failed, counts against the window and never
			// extends it.
			for _, failSync := range []bool{false, true} {
				tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
				tp.worker(t, idA)
				linked := tp.clk.Now()
				tp.inj.onNextSync(func() error {
					tp.clk.Advance(5 * time.Second)
					if failSync {
						return errors.New("injected slow sync failure")
					}
					return nil
				})
				v, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "slow sync"))
				if err != nil || v.DurabilityConfirmed == failSync {
					t.Fatalf("dispatch (sync fails %v) = %+v %v", failSync, v, err)
				}
				if got := tp.startDeadline(t, v.TaskID); !got.Equal(linked.Add(startControl)) {
					t.Fatalf("sync fails %v: deadline anchored after sync: got %v, want %v", failSync, got, linked.Add(startControl))
				}
				if !failSync {
					// Already past its deadline when durable: never written.
					tp.log.awaitOnce(t, "terminal-committed "+v.TaskID)
					if sv := tp.show(t, v.TaskID); sv.State != contract.TaskRejected || sv.Reason.Code != contract.ReasonStartNotSent || tp.log.seenPrefix("start-writing "+idA) {
						t.Fatalf("late durable start %+v", sv)
					}
				}
			}
		})
		t.Run("queued-after-detach", func(t *testing.T) {
			t.Parallel()
			// A frame the original stream delivered, still queued at the
			// plane when that attachment's lease ended, is refused: task
			// output and results count only on the node's current live
			// attachment, checked atomically with acceptance. The task
			// stays running and recovery-required with its slot held.
			for _, kind := range []string{"result", "log"} {
				tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
				w := tp.worker(t, idA)
				st := tp.run(t, w, "a", "queued "+kind, "p2")
				hold := tp.th.arm(kind+"-received", st.TaskID)
				if kind == "log" {
					w.sendLog(st, 0, []byte("late\n"))
				} else {
					w.sendResult(result(st, 0, 0, nil))
				}
				call := paused(t, hold, kind+"-received")
				tp.clk.Advance(leaseDuration)
				if n := tp.nodePlane.show(t, idA); n.Liveness != contract.LivenessOffline {
					t.Fatalf("%s: node still %s", kind, n.Liveness)
				}
				close(call.release)
				tp.log.await(t, "detached "+idA)
				v := tp.show(t, st.TaskID)
				if v.CompletionPending || v.State != contract.TaskRunning || v.Log.ReceivedBytes != 0 || !v.RecoveryRequired || tp.roleInflight(t, "a") != 1 {
					t.Fatalf("%s from the detached original attachment accepted: %+v", kind, v)
				}
			}
		})
		t.Run("frames", func(t *testing.T) {
			t.Parallel()
			// Per-type bounds: a task_log body over 32 KiB, a partial
			// overlap, a log before the start's reply and an unsolicited
			// start reply are protocol errors.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
			w := tp.worker(t, idA)
			st := tp.run(t, w, "a", "frames", "p2")
			w.log(st, 0, []byte("abcdef"))
			expectClosed(t, w, w.sendLog(st, 3, []byte("zzzzzz")))
			w = tp.worker(t, idA)
			rid := w.nextID()
			w.sendRaw([]byte(`{"version":3,"type":"task_log","request_id":"` + rid + `","body":{"x":"` + strings.Repeat("a", contract.MaxTaskLogBody) + `"}}`))
			expectClosed(t, w, rid)
			w = tp.worker(t, idA)
			w.answer("p9", st.TaskID, nil)
			expectClosed(t, w, "p9")
			w = tp.worker(t, idA)
			v := tp.admit(t, taskReq(contract.TargetID, "a", "unreplied"))
			pending := w.start("p2")
			if pending.TaskID != v.TaskID {
				t.Fatal("wrong start")
			}
			expectClosed(t, w, w.sendLog(pending, 0, []byte("early")))
			if sv := tp.show(t, v.TaskID); sv.State != contract.TaskPending || sv.Log.ReceivedBytes != 0 {
				t.Fatalf("output before the start reply was accepted: %+v", sv)
			}
		})
	})
	t.Run("result-receipt", func(t *testing.T) {
		t.Parallel()
		t.Run("slow-commit", func(t *testing.T) {
			t.Parallel()
			// Receipt is acknowledged from bounded memory while the terminal
			// commit is held before any syscall: four seconds later the
			// stream is attached, the other running task unaffected, the
			// slot still held and completion pending. Releasing the disk
			// publishes the terminal record and frees the slot together.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
			w := tp.worker(t, idA)
			st1 := tp.run(t, w, "a", "one", "p2")
			st2 := tp.run(t, w, "a", "two", "p3")
			hold := tp.th.arm("terminal-commit-queued", st1.TaskID)
			w.log(st1, 0, []byte("out\n"))
			rid := w.sendResult(result(st1, 0, 4, nil))
			w.resultAck(rid, st1.TaskID)
			tp.log.await(t, "result-acked "+idA+" "+rid)
			call := paused(t, hold, "terminal-commit-queued")
			tp.clk.Advance(outputAckBound)
			w.beat()
			v1, v2 := tp.show(t, st1.TaskID), tp.show(t, st2.TaskID)
			if v1.State != contract.TaskRunning || !v1.CompletionPending || v1.PersistenceReason != nil || v1.Result != nil || v1.RecoveryRequired || v2.RecoveryRequired {
				t.Fatalf("held commit: %+v / %+v", v1, v2)
			}
			if tp.roleInflight(t, "a") != 2 {
				t.Fatal("receipt released the slot")
			}
			close(call.release)
			tp.log.awaitOnce(t, "terminal-committed "+st1.TaskID)
			if v := tp.show(t, st1.TaskID); v.State != contract.TaskSucceeded || v.CompletionPending || v.LogTail != "out\n" || tp.roleInflight(t, "a") != 1 {
				t.Fatalf("committed %+v", v)
			}
			if rec := tp.taskFile(t, st1.TaskID); rec.State != contract.TaskSucceeded || string(rec.Log.Data) != "out\n" {
				t.Fatalf("durable %+v", rec)
			}
		})
		t.Run("watchdog", func(t *testing.T) {
			t.Parallel()
			// Held through the 30-second commit watchdog: storage diagnostics
			// appear without a disconnect or a fabricated terminal state and
			// admissions are blocked; the late confirmed commit publishes and
			// clears the blockage.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 3), 1)}, idA)
			w := tp.worker(t, idA)
			st := tp.run(t, w, "a", "slow", "p2")
			hold := tp.th.arm("terminal-commit-queued", st.TaskID)
			rid := w.sendResult(result(st, 7, 0, nil))
			w.resultAck(rid, st.TaskID)
			tp.log.await(t, "result-acked "+idA+" "+rid)
			call := paused(t, hold, "terminal-commit-queued")
			for i := 0; i < 6; i++ {
				tp.clk.Advance(5 * time.Second)
				w.beat()
			}
			tp.log.awaitOnce(t, "commit-watchdog "+st.TaskID)
			v := tp.show(t, st.TaskID)
			if v.State != contract.TaskRunning || !v.CompletionPending || v.PersistenceReason == nil || *v.PersistenceReason != contract.ReasonResultStorageUnconfirmed || v.RecoveryRequired {
				t.Fatalf("watchdog view %+v", v)
			}
			_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "blocked"))
			wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:storage_unconfirmed")
			close(call.release)
			tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
			if v := tp.show(t, st.TaskID); v.State != contract.TaskFailed || *v.Result.ExitCode != 7 || v.PersistenceReason != nil {
				t.Fatalf("late commit %+v", v)
			}
			tp.admit(t, taskReq(contract.TargetID, "a", "unblocked"))
		})
	})
	t.Run("result-ack-loss", func(t *testing.T) {
		t.Parallel()
		t.Run("captured", func(t *testing.T) {
			t.Parallel()
			// The receipt ack is written but the worker never reads it and
			// goes away: the captured result is plane-owned and its commit
			// survives the detach; nothing is replayed or requested again.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
			w := tp.worker(t, idA)
			st := tp.run(t, w, "a", "captured", "p2")
			hold := tp.th.arm("terminal-commit-queued", st.TaskID)
			rid := w.sendResult(result(st, 0, 0, nil))
			tp.log.await(t, "result-acked "+idA+" "+rid)
			call := paused(t, hold, "terminal-commit-queued")
			w.c.CloseNow()
			tp.log.await(t, "detached "+idA)
			if v := tp.show(t, st.TaskID); v.RecoveryRequired || !v.CompletionPending {
				t.Fatalf("a captured result became uncertain: %+v", v)
			}
			close(call.release)
			tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
			w = tp.worker(t, idA)
			w.beat()
			if v := tp.show(t, st.TaskID); v.State != contract.TaskSucceeded || tp.roleInflight(t, "a") != 0 {
				t.Fatalf("after detach %+v", v)
			}
			tp.admit(t, taskReq(contract.TargetID, "a", "next"))
			if s := w.start("p2"); s.TaskID == st.TaskID {
				t.Fatal("the old start was replayed")
			}
		})
		t.Run("not-received", func(t *testing.T) {
			t.Parallel()
			// No receipt was accepted before the attachment ended: the task
			// stays running, recovery-required, slot held; the same result on
			// the new attachment is an old-generation report and is refused.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
			w := tp.worker(t, idA)
			st := tp.run(t, w, "a", "lost", "p2")
			w.c.CloseNow()
			tp.log.await(t, "detached "+idA)
			v := tp.show(t, st.TaskID)
			if v.State != contract.TaskRunning || !v.RecoveryRequired || *v.RecoveryReason != contract.RecoveryAttachmentLost || !v.Log.LogMayBeIncomplete || tp.roleInflight(t, "a") != 1 {
				t.Fatalf("lost receipt %+v", v)
			}
			w = tp.worker(t, idA)
			rid := w.sendResult(result(st, 0, 0, nil))
			expectClosed(t, w, rid)
			if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || v.CompletionPending {
				t.Fatalf("an old-generation result was accepted: %+v", v)
			}
		})
	})
}

// outputAckBound is the sidecar's output receipt bound the plane's
// acknowledgement must beat; the plane holds no timer for it.
const outputAckBound = 4*time.Second + time.Millisecond
