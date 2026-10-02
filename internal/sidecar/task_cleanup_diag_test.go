package sidecar

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// logRecord returns the first JSON log record of msg naming task id.
func logRecord(logs, msg, id string) (map[string]any, bool) {
	for _, line := range strings.Split(logs, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg && rec["task_id"] == id {
			return rec, true
		}
	}
	return nil, false
}

// TestTaskCleanupDiagnostics is UT-A2 of iteration 10a for the sidecar's
// bounded cleanup diagnostics on the injected clock: the adapter-exit
// receipt and the proven absence (ESRCH only) are logged with the task's
// group, and the elapsed time between them is the injected clock's; an
// absence denied by EPERM never reports one (the generic unconfirmed
// cleanup is TestTaskExecutionContract/exit's).
func TestTaskCleanupDiagnostics(t *testing.T) {
	t.Parallel()
	fp := startFakePlane(t)
	tr := startTaskRun(t, fp, taskOpts{})
	ins, run := manuals(t, tr.dir, "a", "m")
	s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
	release := tr.groups.hold()
	st, ch := s.run(t, 1, 0, "proved")
	ch.prompt(t)
	ch.exitCode(0)
	tr.groups.awaitGone(t, ch.gpid)
	tr.clk.Advance(250 * time.Millisecond) // the group observation takes 250 ms
	release()
	tr.settled(t, st.TaskID)
	logs := tr.logs.await(t, "task group gone", func(l string) bool { _, ok := logRecord(l, "task group gone", st.TaskID); return ok })
	exit, ok := logRecord(logs, "task adapter exit observed", st.TaskID)
	gone, _ := logRecord(logs, "task group gone", st.TaskID)
	if !ok || exit["pgid"] != float64(ch.gpid) || gone["pgid"] != float64(ch.gpid) || gone["cleanup_elapsed_ns"] != float64(250*time.Millisecond) {
		t.Fatalf("diagnostics %v / %v", exit, gone)
	}
	s.result(t, st)
	// EPERM is never absence (and blocks the node: the run's last task).
	tr.groups.mu.Lock()
	tr.groups.err = errPermission
	tr.groups.mu.Unlock()
	st2, ch2 := s.run(t, 2, 0, "unproved")
	ch2.prompt(t)
	ch2.exitCode(0)
	tr.settled(t, st2.TaskID)
	logs = tr.logs.await(t, "cleanup diagnostic", func(l string) bool {
		_, ok := logRecord(l, "task process group cleanup unconfirmed", st2.TaskID)
		return ok
	})
	if _, ok := logRecord(logs, "task adapter exit observed", st2.TaskID); !ok {
		t.Fatal("EPERM: no exit diagnostic")
	}
	if rec, ok := logRecord(logs, "task group gone", st2.TaskID); ok {
		t.Fatalf("EPERM: an unproved absence was reported: %v", rec)
	}
	if r := s.result(t, st2); r.Outcome != contract.OutcomeNatural {
		t.Fatalf("EPERM: %+v", r)
	}
}
