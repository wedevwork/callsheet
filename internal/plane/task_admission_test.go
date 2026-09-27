package plane

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// wantAdmission checks an admission failure's code, top-level reason and
// candidates (role ID and reason, in order).
func wantAdmission(t *testing.T, err error, code contract.Code, reason string, want ...string) []contract.TaskCandidate {
	t.Helper()
	if contract.CodeOf(err) != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
	if r := reasonOf(err); r != reason {
		t.Fatalf("reason %q (%v), want %q", r, err, reason)
	}
	cs := candidatesOf(t, err)
	var got []string
	for _, c := range cs {
		got = append(got, c.RoleID+":"+c.Reason)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("candidates %v, want %v", got, want)
	}
	return cs
}

func reasonOf(err error) string {
	ce, ok := err.(*contract.Error)
	if !ok || ce.Details == nil {
		return ""
	}
	r, _ := ce.Details["reason"].(string)
	return r
}

// taskCount is the number of tasks the plane lists.
func (tp *taskPlane) taskCount(t *testing.T) int {
	t.Helper()
	tasks, _, err := tp.cl.ListTasks(bg, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(tasks)
}

// gateRace runs the gate-race ordering: first holds the gate at
// before-task-publication while second is released and must fail busy.
func gateRace(t *testing.T, first, second string) {
	tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
	w := tp.worker(t, idA)
	gates := map[string]chan hookCall{first: tp.th.arm("before-gate-CAS", first), second: tp.th.arm("before-gate-CAS", second)}
	pub := tp.th.arm("before-task-publication", first)
	calls := map[string]*call[contract.TaskView]{}
	for _, g := range []string{"caller A", "caller B"} {
		calls[g] = tp.dispatchAsync(taskReq(contract.TargetID, "a", g))
	}
	held := map[string]hookCall{}
	for _, g := range []string{"caller A", "caller B"} {
		held[g] = paused(t, gates[g], "before-gate-CAS "+g)
	}
	close(held[first].release)
	tp.log.await(t, "gate-acquired "+first)
	p := paused(t, pub, "before-task-publication "+first)
	close(held[second].release)
	_, err := calls[second].wait(t)
	cs := wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonBusy, "a:available")
	if c := cs[0]; c.Inflight != 0 || c.Concurrency != 1 || !c.CanAccept || c.RecoveryInflight != 0 {
		t.Fatalf("busy candidate %+v", c)
	}
	if !calls[first].pending() || tp.log.seen("gate-acquired "+second) || tp.log.seen("task-published "+second) {
		t.Fatal("the loser passed the gate or the winner returned early")
	}
	close(p.release)
	v, err := calls[first].wait(t)
	if err != nil || v.State != contract.TaskPending || v.Request.Goal != first {
		t.Fatalf("winner = %+v %v", v, err)
	}
	st := w.start("p2")
	if st.TaskID != v.TaskID || tp.taskCount(t) != 1 || tp.roleInflight(t, "a") != 1 {
		t.Fatalf("started %s for %s; tasks %d", st.TaskID, v.TaskID, tp.taskCount(t))
	}
	if rec := tp.taskFile(t, v.TaskID); rec.State != contract.TaskPending || rec.Request.Goal != first {
		t.Fatalf("durable record %+v", rec)
	}
	// No queued second dispatch and no reroute ever appears.
	w.answer("p2", st.TaskID, nil)
	tp.log.awaitOnce(t, "published running "+st.TaskID)
	if tp.taskCount(t) != 1 || tp.log.seen("task-published "+second) {
		t.Fatal("a second task appeared")
	}
}

// TestTaskAdmission is UT FP-2/8 for dispatch admission; its selection,
// gate-race and reserved-slot subtests are delegated from tests/function
// (TestTaskDispatch). Do not rename or skip its subtests.
func TestTaskAdmission(t *testing.T) {
	t.Parallel()
	t.Run("selection", func(t *testing.T) {
		t.Parallel()
		// By name: the first eligible match in registration order (not
		// list order); by ID: that instance or fail, never a fallback. No
		// queue: a full target fails at once with every candidate.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{
			record(taskCfg("zed", "coder", idA, 1), 1), record(taskCfg("alpha", "coder", idA, 1), 2),
			record(taskCfg("rev", "reviewer", idB, 1), 3)}, idA, idB)
		w := tp.worker(t, idA)
		v1 := tp.admit(t, taskReq(contract.TargetName, "coder", "one"))
		if v1.Role.ID != "zed" || v1.Role.RegistrationOrder != 1 || v1.State != contract.TaskPending || v1.Result != nil || !v1.DurabilityConfirmed {
			t.Fatalf("first by name = %+v", v1.Role)
		}
		// The start is scheduled on the captured attachment after commit,
		// never before: publication precedes the queue.
		st := w.start("p2")
		if st.TaskID != v1.TaskID || st.Execution.Epoch != tp.svc(t).epoch || st.Role.ID != "zed" || st.Request.Goal != "one" {
			t.Fatalf("start %+v", st)
		}
		v2 := tp.admit(t, taskReq(contract.TargetName, "coder", "two"))
		if v2.Role.ID != "alpha" {
			t.Fatalf("second by name = %+v", v2.Role)
		}
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetName, "coder", "three"))
		cs := wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "zed:full", "alpha:full")
		for _, c := range cs {
			if c.Inflight != 1 || c.Concurrency != 1 || c.CanAccept || c.NodeLiveness != contract.LivenessOnline || c.NodeID != idA {
				t.Fatalf("full candidate %+v", c)
			}
		}
		// By ID never falls back to another instance of the name.
		_, err = tp.cl.Dispatch(bg, taskReq(contract.TargetID, "zed", "four"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "zed:full")
		// An offline node's role is a candidate with its reason.
		_, err = tp.cl.Dispatch(bg, taskReq(contract.TargetID, "rev", "five"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "rev:node_offline")
		// Unknown ID or name: not_found with no candidates.
		_, err = tp.cl.Dispatch(bg, taskReq(contract.TargetID, "nobody", "six"))
		wantAdmission(t, err, contract.CodeNotFound, "")
		_, err = tp.cl.Dispatch(bg, taskReq(contract.TargetName, "nobody", "six"))
		wantAdmission(t, err, contract.CodeNotFound, "")
		if n := tp.taskCount(t); n != 2 {
			t.Fatalf("%d tasks after refusals", n)
		}
		// A refusal releases nothing: the reservation is plane-owned.
		if tp.roleInflight(t, "zed") != 1 || tp.roleInflight(t, "alpha") != 1 {
			t.Fatal("reservations changed")
		}
	})
	t.Run("gate-race", func(t *testing.T) {
		t.Parallel()
		// Both release orders: one gate winner held before publication
		// while the other fails unavailable/busy at once with the eligible
		// candidate observed at inflight 0, and creates nothing.
		t.Run("a-first", func(t *testing.T) { t.Parallel(); gateRace(t, "caller A", "caller B") })
		t.Run("b-first", func(t *testing.T) { t.Parallel(); gateRace(t, "caller B", "caller A") })
	})
	t.Run("reserved-slot", func(t *testing.T) {
		t.Parallel()
		// A publishes and releases the gate; its start is held by the
		// worker, whose heartbeat still reports the role ready. B takes the
		// free gate and fails unavailable/no_capacity: the plane-owned
		// count, not the heartbeat, is full.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
		w := tp.worker(t, idA)
		gateB := tp.th.arm("before-gate-CAS", "caller B")
		a := tp.dispatchAsync(taskReq(contract.TargetID, "a", "caller A"))
		b := tp.dispatchAsync(taskReq(contract.TargetID, "a", "caller B"))
		hb := paused(t, gateB, "before-gate-CAS caller B")
		tp.log.await(t, "task-published caller A")
		tp.log.await(t, "gate-released caller A")
		va, err := a.wait(t)
		if err != nil {
			t.Fatal(err)
		}
		// The start was written (no write in flight) and is held unanswered.
		tp.log.awaitOnce(t, "start-sent "+idA+" p2 "+va.TaskID)
		close(hb.release)
		_, err = b.wait(t)
		cs := wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:full")
		if c := cs[0]; c.Inflight != 1 || c.Concurrency != 1 || c.CanAccept || c.RecoveryInflight != 0 {
			t.Fatalf("reserved-slot candidate %+v", c)
		}
		if !tp.log.seen("gate-acquired caller B") {
			t.Fatal("B never took the free gate")
		}
		if n := tp.taskCount(t); n != 1 {
			t.Fatalf("%d tasks, want A alone", n)
		}
		st := w.start("p2")
		w.answer("p2", st.TaskID, nil)
		tp.log.awaitOnce(t, "published running "+st.TaskID)
		if tp.taskCount(t) != 1 {
			t.Fatal("a queued second dispatch ran")
		}
	})
	t.Run("candidates", func(t *testing.T) {
		t.Parallel()
		// Reason precedence: storage_unconfirmed, node_offline,
		// node_detached, worker_unready, full, available; recovery counts
		// are reported without disabling the rest of an instance.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
		w := tp.worker(t, idA)
		w.ready["a"] = false
		w.beat()
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "x"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:worker_unready")
		w.ready["a"] = true
		w.beat()
		tp.run(t, w, "a", "one", "p2")
		v := tp.admit(t, taskReq(contract.TargetID, "a", "two"))
		st := w.start("p3")
		w.answer("p3", st.TaskID, nil)
		tp.log.awaitOnce(t, "published running "+st.TaskID)
		_, err = tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "x"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:full")
		// Detached: the stream ended but the lease holds.
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		_, err = tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "x"))
		cs := wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:node_detached")
		if c := cs[0]; c.Inflight != 2 || c.RecoveryInflight != 2 || c.NodeLiveness != contract.LivenessOnline {
			t.Fatalf("detached candidate %+v", c)
		}
		if !strings.Contains(err.Error(), contract.RecoveryNotice) {
			t.Fatalf("no recovery notice: %v", err)
		}
		tp.clk.Advance(leaseDuration)
		_, err = tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "x"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:node_offline")
		if tp.show(t, v.TaskID).State != contract.TaskRunning || tp.roleInflight(t, "a") != 2 {
			t.Fatal("an offline lease released a reservation")
		}
	})
	t.Run("storage", func(t *testing.T) {
		t.Parallel()
		// A visible but unconfirmed publication blocks admissions
		// (storage_unconfirmed) until a retried directory sync confirms it.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 3), 1)}, idA)
		w := tp.worker(t, idA)
		tp.inj.set("dirsync", tasksName)
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "one"))
		if err != nil {
			t.Fatalf("an unconfirmed publication is visible and admitted: %v", err)
		}
		_, err = tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "two"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:storage_unconfirmed")
		tp.inj.set("", "")
		v := tp.admit(t, taskReq(contract.TargetID, "a", "three"))
		if !v.DurabilityConfirmed {
			t.Fatal("the retried sync did not confirm")
		}
		if n := tp.taskCount(t); n != 2 {
			t.Fatalf("%d tasks", n)
		}
		w.start("p2")
	})
	t.Run("rechecks", func(t *testing.T) {
		t.Parallel()
		// Immediately before publication the caller's liveness, the
		// attachment generation and lease, and the registry revision are
		// rechecked; a failed recheck removes this operation's temporary and
		// creates no task. Nothing is sent before publication.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
		w := tp.worker(t, idA)
		for _, c := range []struct {
			name   string
			change func(hookCall)
		}{
			{"generation", func(hookCall) {
				w.c.CloseNow()
				tp.log.await(t, "detached "+idA)
				w = tp.worker(t, idA)
			}},
			{"revision", func(hookCall) {
				rr := tp.svc(t).roles
				cur := rr.load().visible
				next := &roleDoc{exists: true, revision: cur.revision + 1, nextOrder: cur.nextOrder, roles: cur.roles}
				rr.install(&roleState{visible: next, confirmed: next}, "")
			}},
			{"lease", func(hookCall) { tp.clk.Advance(leaseDuration) }},
		} {
			pub := tp.th.arm("before-task-publication", c.name)
			res := tp.dispatchAsync(taskReq(contract.TargetID, "a", c.name))
			p := paused(t, pub, c.name)
			if tp.log.seenPrefix("start-writing " + idA) {
				t.Fatal("a start was sent before publication")
			}
			c.change(p)
			close(p.release)
			_, err := res.wait(t)
			if contract.CodeOf(err) != contract.CodeUnavailable {
				t.Fatalf("%s: %v", c.name, err)
			}
			if n := tp.taskCount(t); n != 0 {
				t.Fatalf("%s created a task", c.name)
			}
			if c.name == "lease" {
				w = tp.worker(t, idA)
			}
		}
		if left := tempLeftovers(t, tp.root); len(left) != 0 {
			t.Fatalf("temporaries left: %v", left)
		}
		// A canceled caller creates nothing either.
		pub := tp.th.arm("before-task-publication", "canceled")
		ctx, cancel := context.WithCancel(bg)
		res := async(func() (contract.TaskView, error) {
			return tp.cl.Dispatch(ctx, taskReq(contract.TargetID, "a", "canceled"))
		})
		p := paused(t, pub, "canceled")
		cancel()
		if _, err := res.wait(t); err == nil {
			t.Fatal("a canceled dispatch succeeded")
		}
		// The operation observes its caller's cancellation (the server
		// saw the connection close) before its publication recheck.
		select {
		case <-p.ctx.Done():
		case <-time.After(testWait):
			t.Fatal("the dispatch never observed its caller's cancellation")
		}
		close(p.release)
		tp.log.await(t, "gate-released canceled")
		if n := tp.taskCount(t); n != 0 {
			t.Fatal("a canceled caller created a task")
		}
		if entries, _ := os.ReadDir(filepath.Join(tp.root, tasksName)); len(entries) != 0 {
			t.Fatalf("tasks/ holds %v", entries)
		}
	})
	t.Run("override", func(t *testing.T) {
		t.Parallel()
		// An override affects only its task; an effort the resolved
		// adapter lacks fails invalid_argument without trying another
		// instance or reserving anything.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 3), 1), record(taskCfg("b", "coder", idA, 3), 2)}, idA)
		w := tp.worker(t, idA)
		r := taskReq(contract.TargetName, "coder", "override")
		m, e, zero := "other model", "high", time.Duration(0)
		r.Override = &contract.TaskOverride{Model: &m, Effort: &e, Timeout: &zero}
		v := tp.admit(t, r)
		if v.Effective != (contract.TaskEffective{Model: m, Effort: e, Timeout: 0}) || v.TimeoutEnforced {
			t.Fatalf("effective %+v", v.Effective)
		}
		if st := w.start("p2"); st.Effective.Model != m || st.Role.Model != "example model" {
			t.Fatalf("start effective %+v role %+v", st.Effective, st.Role)
		}
		plain := tp.admit(t, taskReq(contract.TargetName, "coder", "plain"))
		if plain.Effective != (contract.TaskEffective{Model: "example model", Effort: "medium", Timeout: 2 * time.Hour}) {
			t.Fatalf("inherited %+v", plain.Effective)
		}
		if rv := tp.view(t, "a"); rv.Model != "example model" || rv.Effort != "medium" {
			t.Fatalf("the override changed the role: %+v", rv)
		}
		bad := "extreme"
		r.Override = &contract.TaskOverride{Effort: &bad}
		r.Goal = "bad"
		_, err := tp.cl.Dispatch(bg, r)
		wantField(t, err, "effort")
		if n := tp.taskCount(t); n != 2 || tp.roleInflight(t, "b") != 0 {
			t.Fatalf("an invalid override reserved something: %d tasks", n)
		}
	})
	t.Run("api", func(t *testing.T) {
		t.Parallel()
		// The raw task endpoints: protocol header first, then path, method,
		// query, body and content type; every refusal is the contract
		// error envelope and echoes no request text; nothing is admitted.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
		tp.worker(t, idA)
		httpc := keepAliveClient(t, readCA(t, tp.root))
		do := func(method, path, version, ctype, body string) (int, string) {
			req, _ := http.NewRequest(method, tp.url+path, strings.NewReader(body))
			if body == "" {
				req.Body = http.NoBody
			}
			if version != "" {
				req.Header.Set(contract.ProtocolHeader, version)
			}
			if ctype != "" {
				req.Header.Set("Content-Type", ctype)
			}
			resp, err := httpc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b)
		}
		js := "application/json"
		id := "t_00000000000000000000000000000001"
		good, _ := contract.Encode(taskReq(contract.TargetID, "a", "SECRET-GOAL"))
		for _, c := range []struct {
			method, path, version, ctype, body string
			status                             int
			code                               contract.Code
		}{
			{"GET", contract.PathTasks, "", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks, "2", "", "", 409, contract.CodeProtocolMismatch},
			{"GET", contract.PathTasks + "/t_%30000000000000000000000000000001", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "/" + id + "/logs/x", "3", "", "", 404, contract.CodeNotFound},
			{"GET", contract.PathTasks + "/SECRET-ID", "3", "", "", 400, contract.CodeInvalidArgument},
			{"PUT", contract.PathTasks, "3", js, "{}", 400, contract.CodeInvalidArgument},
			{"POST", contract.PathTasks + "/" + id, "3", js, "{}", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks, "3", "", "SECRET-BODY", 400, contract.CodeInvalidArgument},
			{"POST", contract.PathTasks + "?x=1", "3", js, string(good), 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "?", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "?=1", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "?SECRET-KEY=1", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "?limit=", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "?limit=1&limit=2", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "?limit=0", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "?limit=101", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "?after=SECRET-AFTER", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "/" + id + "/logs?lines=1", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "/" + id + "/logs", "3", "", "", 404, contract.CodeNotFound},
			{"GET", contract.PathTasks + "/" + id + "?lines=1001", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "/" + id + "?limit=1", "3", "", "", 400, contract.CodeInvalidArgument},
			{"GET", contract.PathTasks + "/" + id, "3", "", "", 404, contract.CodeNotFound},
			{"POST", contract.PathTasks, "3", "text/plain", string(good), 400, contract.CodeInvalidArgument},
			{"POST", contract.PathTasks, "3", js, strings.Repeat(" ", contract.MaxDispatchRequestBytes+1), 400, contract.CodeInvalidArgument},
			{"POST", contract.PathTasks, "3", js, `{"extra":"SECRET-VALUE"}`, 400, contract.CodeInvalidArgument},
		} {
			status, body := do(c.method, c.path, c.version, c.ctype, c.body)
			e, err := contract.ParseErrorBody(bytes.TrimSpace([]byte(body)))
			if status != c.status || err != nil || e.Code != c.code || strings.Contains(body, "SECRET") {
				t.Fatalf("%s %s v=%q = %d %q (%v)", c.method, c.path, c.version, status, body, err)
			}
		}
		if n := tp.taskCount(t); n != 0 {
			t.Fatalf("a refused request created %d tasks", n)
		}
		// The success envelopes: 202 dispatch, then show, logs and list.
		status, body := do("POST", contract.PathTasks, "3", js, string(good))
		v, err := contract.ParseDispatchResponse(bytes.TrimSpace([]byte(body)))
		if status != http.StatusAccepted || err != nil || !strings.HasSuffix(body, "}\n") {
			t.Fatalf("dispatch = %d %q %v", status, body, err)
		}
		for _, p := range []string{"/" + v.TaskID + "?lines=0", "/" + v.TaskID + "/logs", "?limit=1&after=" + id} {
			if status, body := do("GET", contract.PathTasks+p, "3", "", ""); status != 200 || !strings.HasPrefix(body, `{"version":3,`) {
				t.Fatalf("GET %s = %d %q", p, status, body)
			}
		}
	})
}

// wantField checks an invalid_argument naming field.
func wantField(t *testing.T, err error, field string) {
	t.Helper()
	ce, ok := err.(*contract.Error)
	if !ok || ce.Code != contract.CodeInvalidArgument {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
	if f, _ := ce.Details["field"].(string); f != field {
		t.Fatalf("field %q (%v), want %q", f, err, field)
	}
}
