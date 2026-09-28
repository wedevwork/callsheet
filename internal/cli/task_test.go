package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

const (
	cliTask  = "t_0123456789abcdef0123456789abcdef"
	cliTaskB = "t_0123456789abcdef0123456789abcdf0"
)

func cliRequest(goal string) contract.DispatchRequest {
	return contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetName, Value: "coder"}, Goal: goal, Payload: []string{},
		Acceptance: "tests pass", RequestedBy: contract.RequestedBy{Name: "callsheet", Version: "dev", Hostname: "coord-host"}}
}

func cliView(id, state string) contract.TaskView {
	now := "2026-09-27T10:00:00Z"
	v := contract.TaskView{TaskID: id, TimeoutPolicy: contract.TimeoutPolicyEnforced, Request: cliRequest("fix \"it\""), Role: contract.TaskRole{ID: "worker-a", Name: "coder", Node: roleNode, Adapter: "fake", RegistrationOrder: 2},
		Effective: contract.TaskEffective{Model: "m", Effort: "low", Timeout: 2 * time.Hour}, State: state, CreatedAt: now, DurabilityConfirmed: true}
	switch state {
	case contract.TaskSucceeded:
		zero, msg := 0, "done \x1b[31m"
		v.StartedAt, v.FinishedAt = &now, &now
		v.LogTail = "ok\n\x1b[2Jred\x7f\r\n"
		v.Log = contract.TaskLogMeta{RetainedBytes: 14, SourceBytes: 20, ReceivedBytes: 20, DroppedBytes: 6, Truncated: true}
		v.TailTruncated = true
		v.Result = &contract.TaskResult{State: state, ExitCode: &zero, FinalMessage: &msg, LogTail: v.LogTail}
	case contract.TaskRejected:
		v.FinishedAt = &now
		v.Reason = &contract.TaskReason{Code: contract.ReasonLocalFull, Message: "the worker is full"}
		v.Candidates = []contract.TaskCandidate{{RoleID: "worker-a", NodeID: roleNode, RegistrationOrder: 2, NodeLiveness: contract.LivenessOnline,
			Inflight: 1, Concurrency: 1, Reason: contract.ReasonFull}}
		v.Result = &contract.TaskResult{State: state}
	case contract.TaskRunning:
		v.StartedAt = &now
		v.Reconciling = true
		v.Log.LogMayBeIncomplete = true
		v.DurabilityConfirmed = false
	case contract.TaskLost:
		v.StartedAt, v.FinishedAt = &now, &now
		v.Reason = &contract.TaskReason{Code: contract.ReasonLeaseExpired, Message: "the node's lease expired"}
		v.Result = &contract.TaskResult{State: state}
		one := 1
		late := "late done"
		v.LateResult = &contract.TaskLateSummary{Digest: strings.Repeat("d", 64), ReceivedAt: now, Outcome: contract.OutcomeNatural,
			ExitCode: &one, FinalMessage: &late, OutputBytes: 9, Log: contract.TaskLogMeta{RetainedBytes: 4, SourceBytes: 9, ReceivedBytes: 4, DroppedBytes: 5,
				Truncated: true, Incomplete: true}}
	}
	return v
}

// TestTaskCLI is UT FP-7 for dispatch and task ls/show/logs: parsing,
// local validation before any request, attribution, requests, text and
// JSON output, safe rendering of output, trust flags and error mapping,
// against a stub plane.
func TestTaskCLI(t *testing.T) {
	sp := startStubPlane(t)
	trust := []string{"--plane", sp.url, "--ca", sp.caFile}
	old := hostname
	hostname = func() (string, error) { return "coord-host", nil }
	t.Cleanup(func() { hostname = old })
	t.Run("dispatch", func(t *testing.T) {
		sp.answer(202, envelope(t, contract.DispatchResponse{Version: 5, TaskID: cliTask, Task: ptrView(cliView(cliTask, contract.TaskPending))}))
		args := append([]string{"dispatch", "--role-name", "coder", "--goal", "fix \"it\"", "--acceptance", "tests pass", "--timeout", "90m",
			"--payload", "a://1", "--payload", "-x literal", "--effort", "high"}, trust...)
		code, out, errOut := exec(t, "linux", args...)
		if code != 0 || errOut != "" {
			t.Fatalf("dispatch = %d %q", code, errOut)
		}
		got := sp.last()
		var body map[string]json.RawMessage
		json.Unmarshal([]byte(got.body), &body)
		if got.method != "POST" || got.path != contract.PathTasks || string(body["target"]) != `{"kind":"name","value":"coder"}` ||
			string(body["payload"]) != `["a://1","-x literal"]` || string(body["override"]) != `{"effort":"high","timeout":"1h30m0s"}` ||
			string(body["requested_by"]) != `{"name":"callsheet","version":"`+Version+`","hostname":"coord-host"}` {
			t.Fatalf("request %+v", got)
		}
		want := "task_id: " + cliTask + "\nstate: pending\nrole_id: worker-a\nrole_name: coder\nnode: " + roleNode + "\nregistration_order: 2\nmodel: \"m\"\neffort: low\ntimeout: 2h0m0s\ntimeout_policy: enforced\n"
		if out != want {
			t.Fatalf("text\n%s\nwant\n%s", out, want)
		}
		byID := cliView(cliTask, contract.TaskPending)
		byID.Request.Target = contract.TaskTarget{Kind: contract.TargetID, Value: "worker-a"}
		byID.Request.Goal, byID.Request.Acceptance = "g", "a"
		sp.answer(202, envelope(t, contract.DispatchResponse{Version: 5, TaskID: cliTask, Task: &byID}))
		code, out, _ = exec(t, "linux", append([]string{"dispatch", "--json", "--role-id", "worker-a", "--goal", "g", "--acceptance", "a"}, trust...)...)
		if code != 0 || out != envelope(t, contract.DispatchResponse{Version: 5, TaskID: cliTask, Task: &byID})+"\n" {
			t.Fatalf("json %d %q", code, out)
		}
		if !strings.Contains(sp.last().body, `"target":{"kind":"id","value":"worker-a"}`) {
			t.Fatalf("by id %s", sp.last().body)
		}
		// A task already rejected when the response returned prints its
		// reason and candidates; exit 0 means admission.
		rejected := cliView(cliTask, contract.TaskRejected)
		rejected.Request = byID.Request
		sp.answer(202, envelope(t, contract.DispatchResponse{Version: 5, TaskID: cliTask, Task: &rejected}))
		code, out, _ = exec(t, "linux", append([]string{"dispatch", "--role-id", "worker-a", "--goal", "g", "--acceptance", "a"}, trust...)...)
		if code != 0 || !strings.Contains(out, "reason: local_full \"the worker is full\"\n") || !strings.Contains(out, "candidate: role_id=worker-a node="+roleNode) {
			t.Fatalf("rejected %d %q", code, out)
		}
		// Admission failures keep their exit code and list candidates.
		sp.answer(503, `{"error":{"code":"unavailable","message":"no candidate role can accept the task now","details":{"reason":"no_capacity","candidates":[{"role_id":"worker-a","node_id":"`+roleNode+`","registration_order":2,"node_liveness":"online","inflight":1,"reconciling_inflight":1,"concurrency":1,"can_accept":false,"reason":"full"}]}}}`)
		code, out, errOut = exec(t, "linux", append([]string{"dispatch", "--role-id", "worker-a", "--goal", "g", "--acceptance", "a"}, trust...)...)
		if code != 5 || out != "" || !strings.Contains(errOut, "callsheet: unavailable: no candidate") || !strings.Contains(errOut, "callsheet: candidate: role_id=worker-a") || !strings.Contains(errOut, "reconciling_inflight=1") {
			t.Fatalf("no capacity %d %q %q", code, out, errOut)
		}
		// Local failures before any request.
		before := sp.count()
		for _, c := range []struct {
			args []string
			code int
			msg  string
		}{
			{[]string{"dispatch", "--goal", "g", "--acceptance", "a"}, 2, "exactly one of --role-id and --role-name"},
			{[]string{"dispatch", "--role-id", "a", "--role-name", "b", "--goal", "g", "--acceptance", "a"}, 2, "exactly one"},
			{[]string{"dispatch", "--role-id", "a", "--acceptance", "a"}, 2, "--goal is required"},
			{[]string{"dispatch", "--role-id", "a", "--goal", "g"}, 2, "--acceptance is required"},
			{[]string{"dispatch", "--role-id", "a", "--goal", " ", "--acceptance", "a"}, 2, "whitespace only"},
			{[]string{"dispatch", "--role-id", "A", "--goal", "g", "--acceptance", "a"}, 2, "target value"},
			{[]string{"dispatch", "--role-id", "a", "--goal", "g", "--acceptance", "a", "--timeout", "soon"}, 2, "--timeout must be a Go duration"},
			{[]string{"dispatch", "--role-id", "a", "--goal", "g", "--acceptance", "a", "--timeout", "-1s"}, 2, "negative"},
			{[]string{"dispatch", "--role-id", "a", "--goal", "g", "--goal", "h", "--acceptance", "a"}, 2, "may be given only once"},
			{[]string{"dispatch", "--role-id", "a", "--goal", "g", "--acceptance", "a", "extra"}, 2, "unexpected argument"},
			{[]string{"dispatch", "--role-id", "a", "--goal", "g", "--acceptance", "a", "--payload", ""}, 2, "must not be empty"},
			{[]string{"dispatch", "--role-id", "a", "--goal", "g", "--acceptance", "a", "--model", " "}, 2, "nonblank"},
		} {
			args := c.args
			if !strings.Contains(c.msg, "exactly one") && c.msg != "unexpected argument" {
				args = append(append([]string{}, c.args...), trust...)
			}
			code, out, errOut := exec(t, "linux", args...)
			if code != c.code || out != "" || !strings.Contains(errOut, c.msg) {
				t.Fatalf("%v = %d %q %q", c.args, code, out, errOut)
			}
		}
		// An unavailable or invalid hostname fails before sending; the
		// requester is never invented.
		for _, h := range []func() (string, error){func() (string, error) { return "", errors.New("no hostname") }, func() (string, error) { return "bad host", nil }} {
			hostname = h
			code, _, errOut := exec(t, "linux", append([]string{"dispatch", "--role-id", "a", "--goal", "g", "--acceptance", "a"}, trust...)...)
			if code != 2 || !strings.Contains(errOut, "hostname") {
				t.Fatalf("hostname failure = %d %q", code, errOut)
			}
		}
		hostname = func() (string, error) { return "coord-host", nil }
		if sp.count() != before {
			t.Fatalf("%d requests sent by failing invocations", sp.count()-before)
		}
	})
	t.Run("ls", func(t *testing.T) {
		sum := func(id string, rec bool) contract.TaskSummary {
			v := cliView(id, contract.TaskPending)
			s := contract.TaskSummary{TaskID: id, Target: v.Request.Target, Role: v.Role, State: v.State, CreatedAt: v.CreatedAt, ElapsedMS: 1500, Effective: v.Effective,
				RequestedBy: v.Request.RequestedBy, DurabilityConfirmed: true}
			s.Reconciling = rec
			return s
		}
		next := cliTaskB
		resp := contract.TaskListResponse{Version: 5, Tasks: []contract.TaskSummary{sum(cliTask, false), sum(cliTaskB, true)}, NextAfter: &next}
		sp.answer(200, envelope(t, resp))
		code, out, _ := exec(t, "linux", append([]string{"task", "ls", "--limit", "2", "--after", "t_0000000000000000000000000000000a"}, trust...)...)
		want := "TASK_ID\tSTATE\tROLE_ID\tNODE\tELAPSED_MS\tRECONCILING\n" + cliTask + "\tpending\tworker-a\t" + roleNode + "\t1500\tfalse\n" +
			cliTaskB + "\tpending\tworker-a\t" + roleNode + "\t1500\ttrue\nnext_after: " + cliTaskB + "\n"
		if code != 0 || out != want || sp.last().query != "after=t_0000000000000000000000000000000a&limit=2" || sp.last().method != "GET" {
			t.Fatalf("ls %d\n%s\n%+v", code, out, sp.last())
		}
		code, out, _ = exec(t, "linux", append([]string{"task", "ls", "--json"}, trust...)...)
		if code != 0 || out != envelope(t, resp)+"\n" || sp.last().query != "" {
			t.Fatalf("ls json %d %q", code, out)
		}
		sp.answer(200, `{"version":5,"tasks":[],"next_after":null}`)
		if code, out, _ := exec(t, "linux", append([]string{"task", "ls"}, trust...)...); code != 0 || out != "TASK_ID\tSTATE\tROLE_ID\tNODE\tELAPSED_MS\tRECONCILING\n" {
			t.Fatalf("empty ls %d %q", code, out)
		}
		for _, args := range [][]string{{"task", "ls", "--limit", "0"}, {"task", "ls", "--limit", "x"}, {"task", "ls", "--after", "t_bad"}, {"task", "ls", "extra"}} {
			if code, _, _ := exec(t, "linux", append(args, trust...)...); code != 2 {
				t.Fatalf("%v = %d", args, code)
			}
		}
	})
	t.Run("show", func(t *testing.T) {
		sp.answer(200, envelope(t, contract.TaskShowResponse{Version: 5, Task: cliView(cliTask, contract.TaskSucceeded)}))
		code, out, _ := exec(t, "linux", append([]string{"task", "show", cliTask, "--lines", "5"}, trust...)...)
		if code != 0 || sp.last().query != "lines=5" || sp.last().path != contract.PathTasks+"/"+cliTask {
			t.Fatalf("show %d %+v", code, sp.last())
		}
		for _, want := range []string{"task_id: " + cliTask + "\n", "goal: \"fix \\\"it\\\"\"\n", "exit_code: 0\n", "final_message: \"done \\u001b[31m\"\n",
			"warning: the retained output is truncated: 6 earlier bytes are not retained\n", "warning: only the last lines are shown",
			"log_tail:\nok\n\\x1b[2Jred\\x7f\\x0d\n"} {
			if !strings.Contains(out, want) {
				t.Fatalf("show lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "\x1b") {
			t.Fatal("show printed a raw escape")
		}
		sp.answer(200, envelope(t, contract.TaskShowResponse{Version: 5, Task: cliView(cliTask, contract.TaskRunning)}))
		code, out, _ = exec(t, "linux", append([]string{"task", "show", "--json", cliTask}, trust...)...)
		if code != 0 || !strings.HasPrefix(out, `{"version":5,"task":{"task_id":"`+cliTask+`"`) {
			t.Fatalf("show json %d %q", code, out)
		}
		code, out, _ = exec(t, "linux", append([]string{"task", "show", cliTask}, trust...)...)
		for _, want := range []string{"warning: " + contract.ReconcilingNotice, "warning: this task's durability is unconfirmed", "reconciling: true\n", "may be incomplete"} {
			if code != 0 || !strings.Contains(out, want) {
				t.Fatalf("reconciling show lacks %q:\n%s", want, out)
			}
		}
		// A lost task with a late result: the state is unchanged, the late
		// outcome is summarized separately.
		sp.answer(200, envelope(t, contract.TaskShowResponse{Version: 5, Task: cliView(cliTask, contract.TaskLost)}))
		code, out, _ = exec(t, "linux", append([]string{"task", "show", cliTask}, trust...)...)
		for _, want := range []string{"state: lost\n", "reason: lease_expired", "late_result: outcome=natural digest=" + strings.Repeat("d", 64) + " received_at=", "late_exit_code: 1\n",
			"late_final_message: \"late done\"\n", "late_log: retained_bytes=4 source_bytes=9 received_bytes=4 truncated=true incomplete=true\n",
			"warning: the execution's outcome and cleanup are unconfirmed (lost)", "notice: a late result was recorded"} {
			if code != 0 || !strings.Contains(out, want) {
				t.Fatalf("lost show lacks %q:\n%s", want, out)
			}
		}
		sp.answer(404, `{"error":{"code":"not_found","message":"task `+cliTask+` does not exist"}}`)
		if code, _, errOut := exec(t, "linux", append([]string{"task", "show", cliTask}, trust...)...); code != 3 || !strings.Contains(errOut, "does not exist") {
			t.Fatalf("unknown %d %q", code, errOut)
		}
		for _, args := range [][]string{{"task", "show"}, {"task", "show", "t_x"}, {"task", "show", cliTask, "--lines", "201"}, {"task", "show", cliTask, cliTaskB}} {
			if code, _, _ := exec(t, "linux", append(args, trust...)...); code != 2 {
				t.Fatalf("%v = %d", args, code)
			}
		}
	})
	t.Run("logs", func(t *testing.T) {
		data := []byte("line\n\x1b]0;title\x07\xff tail")
		lr := contract.TaskLogsResponse{Version: 5, TaskID: cliTask, Data: data, RetainedBytes: len(data), SourceBytes: len(data) + 10, DroppedBytes: 10, Truncated: true, Incomplete: true}
		sp.answer(200, envelope(t, lr))
		code, out, errOut := exec(t, "linux", append([]string{"task", "logs", cliTask}, trust...)...)
		if code != 0 || out != "line\n\\x1b]0;title\\x07\uFFFD tail" || !strings.Contains(errOut, "callsheet: notice: the retained output is truncated") ||
			!strings.Contains(errOut, "callsheet: notice: the output is incomplete") || sp.last().path != contract.PathTasks+"/"+cliTask+"/logs" {
			t.Fatalf("logs %d %q %q", code, out, errOut)
		}
		code, out, errOut = exec(t, "linux", append([]string{"task", "logs", "--json", cliTask}, trust...)...)
		if code != 0 || out != envelope(t, lr)+"\n" || errOut != "" {
			t.Fatalf("logs json %d %q %q", code, out, errOut)
		}
		// --late: the late result's separate tail (iteration 06a).
		late := contract.TaskLogsResponse{Version: 5, TaskID: cliTask, Data: []byte("late tail\n"), RetainedBytes: 10, SourceBytes: 10}
		sp.answer(200, envelope(t, late))
		code, out, errOut = exec(t, "linux", append([]string{"task", "logs", cliTask, "--late"}, trust...)...)
		if code != 0 || out != "late tail\n" || errOut != "" || sp.last().query != "late=true" {
			t.Fatalf("late logs %d %q %q %+v", code, out, errOut, sp.last())
		}
		sp.answer(404, `{"error":{"code":"not_found","message":"task `+cliTask+` has no late result","details":{"reason":"no_late_result"}}}`)
		if code, _, errOut := exec(t, "linux", append([]string{"task", "logs", "--late", cliTask}, trust...)...); code != 3 || !strings.Contains(errOut, "no late result") {
			t.Fatalf("no late result %d %q", code, errOut)
		}
		if code, _, errOut := exec(t, "linux", "task", "logs", cliTask, "--late=maybe"); code != 2 || errOut == "" {
			t.Fatalf("bad --late %d %q", code, errOut)
		}
	})
	t.Run("trust", func(t *testing.T) {
		before := sp.count()
		for _, args := range [][]string{
			{"task", "ls", "--plane", sp.url},
			{"task", "show", cliTask, "--plane", sp.url, "--ca-fingerprint", "sha256:" + strings.Repeat("0", 64)},
		} {
			if code, out, errOut := exec(t, "linux", args...); code != 6 || out != "" || !strings.Contains(errOut, "connection not trusted") {
				t.Fatalf("%v = %d %q", args, code, errOut)
			}
		}
		sp.answer(200, `{"version":5,"tasks":[],"next_after":null}`)
		if code, _, _ := exec(t, "linux", "task", "ls", "--plane", sp.url, "--ca-fingerprint", sp.pin); code != 0 {
			t.Fatalf("pinned ls = %d", code)
		}
		if sp.count() == before {
			t.Fatal("the pinned request never reached the plane")
		}
		if code, _, errOut := exec(t, "linux", "task", "ls", "--plane", "http://x", "--ca", sp.caFile); code != 2 || !strings.Contains(errOut, "https") {
			t.Fatalf("plaintext = %d %q", code, errOut)
		}
	})
}

// TestSafeLog checks the terminal rendering of child output.
func TestSafeLog(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\n": "plain\ttext\n",
		"\x1b[31mred":   "\\x1b[31mred",
		"cr\r\n":        "cr\\x0d\n",
		"del\x7f":       "del\\x7f",
		"bad\xffbyte":   "bad\uFFFDbyte",
		"c1\u0085":      "c1\\u0085",
		"é漢":            "é漢",
	} {
		if got := SafeLog(in); got != want {
			t.Fatalf("SafeLog(%q) = %q, want %q", in, got, want)
		}
	}
}

// ptrView is v's address (a dispatch response's task).
func ptrView(v contract.TaskView) *contract.TaskView { return &v }
