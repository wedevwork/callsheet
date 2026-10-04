package cli

import (
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 06b CLI families: TestControlCancel (FP-1), TestControlWait
// (FP-3) and TestControlRemove (FP-4), against the stub plane. Successful
// commands exit 0 whatever the task's own result; errors keep the
// contract exit mapping; user cancellation is 130.

// stopView is a running view with a stop request.
func stopView(id string) contract.TaskView {
	v := cliView(id, contract.TaskRunning)
	v.StopRequested = true
	return v
}

// TestControlCancel is UT FP-1 on the CLI, delegated from tests/function
// (TestControlCancellation): task cancel's request, its accepted and
// terminal text and JSON, and its error exits. Do not rename or skip it.
func TestControlCancel(t *testing.T) {
	isolate(t)
	sp := startStubPlane(t)
	trust := []string{"--plane", sp.url, "--ca", sp.caFile}
	t.Run("accepted", func(t *testing.T) {
		resp := contract.CancelResponse{Version: 6, TaskID: cliTask, Accepted: true, Task: stopView(cliTask)}
		sp.answer(202, envelope(t, resp))
		code, out, errOut := exec(t, "linux", append([]string{"task", "cancel", cliTask}, trust...)...)
		if code != 0 || out != "cancel accepted: "+cliTask+"\n" || errOut != "" {
			t.Fatalf("text %d %q %q", code, out, errOut)
		}
		if r := sp.last(); r.method != "POST" || r.path != contract.PathTasks+"/"+cliTask+"/cancel" || r.body != "{}" || r.ctype != "application/json" {
			t.Fatalf("request %+v", r)
		}
		code, out, _ = exec(t, "linux", append([]string{"task", "cancel", cliTask, "--json"}, trust...)...)
		if code != 0 || out != envelope(t, resp)+"\n" {
			t.Fatalf("json %d %q", code, out)
		}
	})
	t.Run("terminal", func(t *testing.T) {
		sp.answer(200, envelope(t, contract.CancelResponse{Version: 6, TaskID: cliTask, Task: cliView(cliTask, contract.TaskSucceeded)}))
		code, out, _ := exec(t, "linux", append([]string{"task", "cancel", cliTask}, trust...)...)
		if code != 0 || out != "task already terminal: "+cliTask+" succeeded\n" {
			t.Fatalf("terminal %d %q", code, out)
		}
	})
	t.Run("errors", func(t *testing.T) {
		n := sp.count()
		for _, args := range [][]string{{"task", "cancel"}, {"task", "cancel", cliTask, cliTaskB}, {"task", "cancel", "t_X"}} {
			if code, _, _ := exec(t, "linux", append(args, trust...)...); code != 2 {
				t.Fatalf("%v = %d", args, code)
			}
		}
		if code, _, _ := exec(t, "linux", "task", "cancel", cliTask); code == 0 {
			t.Fatal("a cancel without trust succeeded")
		}
		if sp.count() != n {
			t.Fatal("an invalid cancel reached the plane")
		}
		for status, want := range map[int]int{404: 3, 503: 5, 500: 1} {
			code := map[int]string{404: "not_found", 503: "unavailable", 500: "internal"}[status]
			sp.answer(status, `{"error":{"code":"`+code+`","message":"m"}}`)
			if got, _, _ := exec(t, "linux", append([]string{"task", "cancel", cliTask}, trust...)...); got != want {
				t.Fatalf("%d = %d, want %d", status, got, want)
			}
		}
	})
}

// TestControlWait is UT FP-3 on the CLI, delegated from tests/function
// (TestControlBoundedWait): task wait and dispatch --wait text (the
// winner and task show rendering, or the escaped compact table) and JSON,
// the default and explicit waits, and exits. Do not rename or skip it.
func TestControlWait(t *testing.T) {
	isolate(t)
	sp := startStubPlane(t)
	trust := []string{"--plane", sp.url, "--ca", sp.caFile}
	rows := []contract.WaitRow{{TaskID: cliTask, State: contract.TaskRunning, ElapsedMS: 1500, LastLogLine: "tab\there\x1b[2J", LogTruncated: true, DurabilityConfirmed: true},
		{TaskID: cliTaskB, State: contract.TaskPending, ElapsedMS: 0}}
	still := contract.WaitResponse{Version: 6, Status: contract.WaitStillRunning, EffectiveWaitMS: 5000, Tasks: rows}
	t.Run("text", func(t *testing.T) {
		b, _ := contract.EncodeWaitResponse(still)
		sp.answer(200, string(b))
		code, out, errOut := exec(t, "linux", append([]string{"task", "wait", cliTask, cliTaskB}, trust...)...)
		want := "TASK_ID\tSTATE\tELAPSED_MS\tLAST_LOG_LINE\tLOG_TRUNCATED\tDURABILITY_CONFIRMED\n" +
			cliTask + "\trunning\t1500\ttab\\x09here\\x1b[2J\ttrue\ttrue\n" + cliTaskB + "\tpending\t0\t\tfalse\tfalse\n"
		if code != 0 || out != want || errOut != "" || strings.Contains(out, "\x1b") {
			t.Fatalf("still running %d\n%s", code, out)
		}
		if r := sp.last(); r.path != contract.PathTaskWait || r.body != `{"task_ids":["`+cliTask+`","`+cliTaskB+`"],"wait":"5s"}` {
			t.Fatalf("default wait request %+v", r)
		}
		v := cliView(cliTaskB, contract.TaskSucceeded)
		term := contract.WaitResponse{Version: 6, Status: contract.WaitTerminal, EffectiveWaitMS: 1000, Winner: cliTaskB, Task: &v}
		tb, _ := contract.EncodeWaitResponse(term)
		sp.answer(200, string(tb))
		code, out, _ = exec(t, "linux", append([]string{"task", "wait", cliTask, cliTaskB, "--wait", "1s"}, trust...)...)
		if code != 0 || out != "winner: "+cliTaskB+"\n"+RenderTask(v) {
			t.Fatalf("terminal %d\n%s", code, out)
		}
		// dispatch --wait: task_id first, then the same renderings.
		one := contract.WaitResponse{Version: 6, Status: contract.WaitStillRunning, EffectiveWaitMS: 2000, Tasks: rows[:1]}
		sp.answer(202, envelope(t, contract.DispatchResponse{Version: 6, TaskID: cliTask, WaitResult: &one}))
		code, out, _ = exec(t, "linux", append([]string{"dispatch", "--role-name", "coder", "--goal", "g", "--acceptance", "a", "--wait", "2s"}, trust...)...)
		if code != 0 || !strings.HasPrefix(out, "task_id: "+cliTask+"\nTASK_ID\t") || len(out) > contract.MaxStillRunningOneBytes {
			t.Fatalf("dispatch wait %d\n%s", code, out)
		}
		if r := sp.last(); !strings.Contains(r.body, `"wait":"2s"`) {
			t.Fatalf("dispatch request %s", r.body)
		}
	})
	t.Run("json", func(t *testing.T) {
		snap := still
		snap.EffectiveWaitMS = 0
		b, _ := contract.EncodeWaitResponse(snap)
		sp.answer(200, string(b))
		code, out, _ := exec(t, "linux", append([]string{"task", "wait", cliTask, cliTaskB, "--json", "--wait", "0"}, trust...)...)
		if code != 0 || out != string(b)+"\n" {
			t.Fatalf("json %d %q", code, out)
		}
		if r := sp.last(); !strings.Contains(r.body, `"wait":"0s"`) {
			t.Fatalf("zero wait %s", r.body)
		}
		one := contract.WaitResponse{Version: 6, Status: contract.WaitStillRunning, EffectiveWaitMS: 2000, Tasks: rows[:1]}
		d := contract.DispatchResponse{Version: 6, TaskID: cliTask, WaitResult: &one}
		sp.answer(202, envelope(t, d))
		code, out, _ = exec(t, "linux", append([]string{"dispatch", "--role-name", "coder", "--goal", "g", "--acceptance", "a", "--wait", "2s", "--json"}, trust...)...)
		if code != 0 || out != envelope(t, d)+"\n" {
			t.Fatalf("dispatch json %d %q", code, out)
		}
	})
	t.Run("exit", func(t *testing.T) {
		// Both answers exit 0 (a failed task's too); invalid input is 2
		// before any request; errors map; an unknown ID is not_found (3).
		v := cliView(cliTask, contract.TaskSucceeded)
		v.State = contract.TaskFailed
		one := 1
		v.Result.State, v.Result.ExitCode = contract.TaskFailed, &one
		term := contract.WaitResponse{Version: 6, Status: contract.WaitTerminal, EffectiveWaitMS: 1000, Winner: cliTask, Task: &v}
		tb, _ := contract.EncodeWaitResponse(term)
		sp.answer(200, string(tb))
		if code, _, _ := exec(t, "linux", append([]string{"task", "wait", cliTask}, trust...)...); code != 0 {
			t.Fatalf("failed task wait = %d", code)
		}
		n := sp.count()
		ids := make([]string, 17)
		for i := range ids {
			ids[i] = "t_" + strings.Repeat("0", 30) + string("0123456789abcdefg"[i]) + "1"
		}
		for _, args := range [][]string{{"task", "wait"}, {"task", "wait", cliTask, "--wait", "6m"}, {"task", "wait", cliTask, "--wait", "-1s"},
			{"task", "wait", cliTask, cliTask}, {"task", "wait", cliTask, "--wait"}, append([]string{"task", "wait"}, ids...),
			{"dispatch", "--role-name", "coder", "--goal", "g", "--acceptance", "a", "--wait", "x"}} {
			if code, _, _ := exec(t, "linux", append(args, trust...)...); code != 2 {
				t.Fatalf("%v = %d", args, code)
			}
		}
		if sp.count() != n {
			t.Fatal("an invalid wait reached the plane")
		}
		sp.answer(404, `{"error":{"code":"not_found","message":"task does not exist"}}`)
		if code, _, _ := exec(t, "linux", append([]string{"task", "wait", cliTask}, trust...)...); code != 3 {
			t.Fatalf("unknown = %d", code)
		}
		sp.answer(503, `{"error":{"code":"unavailable","message":"full","details":{"reason":"wait_capacity"}}}`)
		if code, _, _ := exec(t, "linux", append([]string{"task", "wait", cliTask}, trust...)...); code != 5 {
			t.Fatalf("capacity = %d", code)
		}
	})
}

// TestControlRemove is UT FP-4 on the CLI, delegated from tests/function
// (TestControlForceRemove): role rm --force's completed (200) and pending
// (202) answers with the exact safe retry command, --operation's request
// and validation, and the client's strict parsing of both arms. Do not
// rename or skip it.
func TestControlRemove(t *testing.T) {
	isolate(t)
	sp := startStubPlane(t)
	trust := []string{"--plane", sp.url, "--ca", sp.caFile}
	op := strings.Repeat("ab", 16)
	pending := contract.RoleRemovePendingResponse{Version: 6, OperationID: op, RoleID: "worker-a", RegistrationOrder: 3, Removing: true}
	t.Run("completed", func(t *testing.T) {
		sp.answer(200, `{"version":6,"removed":"worker-a"}`)
		code, out, _ := exec(t, "linux", append([]string{"role", "rm", "worker-a", "--force"}, trust...)...)
		if code != 0 || out != "removed: worker-a\n"+roleRmNotice {
			t.Fatalf("completed %d %q", code, out)
		}
		if r := sp.last(); r.method != "DELETE" || r.body != `{"force":true}` {
			t.Fatalf("request %+v", r)
		}
		code, out, _ = exec(t, "linux", append([]string{"role", "rm", "worker-a", "--force", "--json"}, trust...)...)
		if code != 0 || out != `{"version":6,"removed":"worker-a"}`+"\n" {
			t.Fatalf("json %d %q", code, out)
		}
	})
	t.Run("pending", func(t *testing.T) {
		sp.answer(202, envelope(t, pending))
		code, out, _ := exec(t, "linux", append([]string{"role", "rm", "worker-a", "--force"}, trust...)...)
		want := "removal pending: worker-a operation: " + op + "\nretry: callsheet role rm worker-a --force --operation " + op + "\n"
		if code != 0 || !strings.HasPrefix(out, want) || strings.Contains(out, "task_id") {
			t.Fatalf("pending %d %q", code, out)
		}
		code, out, _ = exec(t, "linux", append([]string{"role", "rm", "worker-a", "--force", "--json"}, trust...)...)
		if code != 0 || out != envelope(t, pending)+"\n" {
			t.Fatalf("pending json %d %q", code, out)
		}
		// Strict arms: a pending answer to an ordinary rm, a pending answer
		// for another role, removing=false, a bad token.
		for name, c := range map[string]struct {
			status int
			body   string
			args   []string
		}{
			"no force":     {202, envelope(t, pending), []string{"role", "rm", "worker-a"}},
			"other role":   {202, strings.Replace(envelope(t, pending), `"worker-a"`, `"worker-b"`, 1), []string{"role", "rm", "worker-a", "--force"}},
			"not removing": {202, strings.Replace(envelope(t, pending), `"removing":true`, `"removing":false`, 1), []string{"role", "rm", "worker-a", "--force"}},
			"bad token":    {202, strings.Replace(envelope(t, pending), op, "XYZ", 1), []string{"role", "rm", "worker-a", "--force"}},
			"200 as 202":   {200, envelope(t, pending), []string{"role", "rm", "worker-a", "--force"}},
		} {
			sp.answer(c.status, c.body)
			if code, _, _ := exec(t, "linux", append(c.args, trust...)...); code != 2 {
				t.Fatalf("%s = %d", name, code)
			}
		}
	})
	t.Run("retry", func(t *testing.T) {
		sp.answer(202, envelope(t, pending))
		if code, _, _ := exec(t, "linux", append([]string{"role", "rm", "worker-a", "--force", "--operation", op}, trust...)...); code != 0 {
			t.Fatalf("retry = %d", code)
		}
		if r := sp.last(); r.body != `{"force":true,"operation_id":"`+op+`"}` {
			t.Fatalf("retry request %s", r.body)
		}
		other := pending
		other.OperationID = strings.Repeat("cd", 16)
		sp.answer(202, envelope(t, other))
		if code, _, _ := exec(t, "linux", append([]string{"role", "rm", "worker-a", "--force", "--operation", op}, trust...)...); code != 2 {
			t.Fatalf("another operation's answer = %d", code)
		}
		n := sp.count()
		for _, args := range [][]string{{"role", "rm", "worker-a", "--operation", op}, {"role", "rm", "worker-a", "--force", "--operation", "ABC"}} {
			if code, _, _ := exec(t, "linux", append(args, trust...)...); code != 2 {
				t.Fatalf("%v = %d", args, code)
			}
		}
		if sp.count() != n {
			t.Fatal("an invalid retry reached the plane")
		}
		sp.answer(409, `{"error":{"code":"conflict","message":"mismatch","details":{"reason":"removal_operation_mismatch"}}}`)
		if code, _, errOut := exec(t, "linux", append([]string{"role", "rm", "worker-a", "--force", "--operation", op}, trust...)...); code != 4 ||
			!strings.Contains(errOut, "conflict") {
			t.Fatalf("mismatch = %d %q", code, errOut)
		}
		sp.answer(404, `{"error":{"code":"not_found","message":"role worker-a does not exist"}}`)
		if code, _, _ := exec(t, "linux", append([]string{"role", "rm", "worker-a", "--force", "--operation", op}, trust...)...); code != 3 {
			t.Fatalf("deleted = %d", code)
		}
	})
}
