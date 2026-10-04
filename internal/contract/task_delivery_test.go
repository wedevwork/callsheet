package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

// Iteration 10c contract tests (UT-C2/UT-C3): the task-aware selector
// wrapper, the task-result door errors and the strict task workspace
// status envelope (bounds, keys, nulls and consistency).

const deliveryTask = "t_0123456789abcdef0123456789abcdef"

func TestParseTaskSelector(t *testing.T) {
	for _, c := range []struct {
		in, field string
		empty     bool
		kind, val string
	}{
		{deliveryTask, "ref", false, SelectorKindTask, TaskRefPrefix + deliveryTask},
		{TaskRefPrefix + deliveryTask, "ref", false, SelectorKindTask, TaskRefPrefix + deliveryTask},
		// An actual branch named like a task ID escapes with refs/heads/.
		{"refs/heads/" + deliveryTask, "ref", false, SelectorKindBranch, "refs/heads/" + deliveryTask},
		{"main", "ref", false, SelectorKindBranch, DefaultBranchRef},
		{wsCommit, "target", false, SelectorKindHash, wsCommit},
		{"empty", "base", true, SelectorKindEmpty, ""},
		// "empty" without allowEmpty is the branch refs/heads/empty, and a
		// malformed t_ string is just a branch, as ParseSelector says.
		{"empty", "target", false, SelectorKindBranch, "refs/heads/empty"},
		{"t_abc", "ref", false, SelectorKindBranch, "refs/heads/t_abc"},
	} {
		sel, err := ParseTaskSelector(c.in, c.field, c.empty)
		if err != nil || sel.Kind != c.kind || sel.Value != c.val {
			t.Fatalf("%q: %+v %v", c.in, sel, err)
		}
		if c.kind == SelectorKindTask {
			// The canonical value passes the unchanged ValidatePull.
			if in, err := ValidatePull("proj", SelectorString(sel)); err != nil || in.Canonical != TaskRefPrefix+deliveryTask {
				t.Fatalf("ValidatePull(%q): %+v %v", SelectorString(sel), in, err)
			}
		}
	}
	for _, bad := range []string{"", "HEAD~1", "refs/callsheet/tasks/t_1", "T_0123456789abcdef0123456789abcdef", "Main"} {
		if _, err := ParseTaskSelector(bad, "ref", false); CodeOf(err) != CodeInvalidArgument {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
	if TaskWorkspacePath(deliveryTask) != "/api/v1/tasks/"+deliveryTask+"/workspace" {
		t.Fatal(TaskWorkspacePath(deliveryTask))
	}
}

func TestTaskDeliveryErrors(t *testing.T) {
	for _, c := range []struct {
		err    error
		code   Code
		reason string
	}{
		{NoTaskWorkspace(deliveryTask), CodeInvalidArgument, ReasonNoTaskWorkspace},
		{TaskResultPending(deliveryTask), CodeConflict, ReasonTaskResultPending},
		{TaskResultUnavailable(deliveryTask, "its publication is failed"), CodeConflict, ReasonTaskResultUnavailable},
		{WorkspaceInstanceMismatch("proj"), CodeConflict, ReasonWorkspaceInstanceMismatch},
	} {
		e := c.err.(*Error)
		if e.Code != c.code || e.Details["reason"] != c.reason || e.Message == "" {
			t.Fatalf("%+v", e)
		}
	}
	if ReasonTaskResultPending != "task_result_pending" || ReasonTaskResultUnavailable != "task_result_unavailable" || ReasonNoTaskWorkspace != "no_task_workspace" {
		t.Fatal("reason constants")
	}
}

// statusOf is a published, available status of deliveryTask.
func statusOf() TaskWorkspaceStatus {
	b := wsBinding()
	r := wsPublished(b, deliveryTask)
	return TaskWorkspaceStatus{TaskID: deliveryTask, State: TaskSucceeded, Binding: b, Result: &r, Available: true}
}

func TestTaskWorkspaceStatusEnvelope(t *testing.T) {
	ph := WorkspacePhaseExecuting
	pending := TaskWorkspaceStatus{TaskID: deliveryTask, State: TaskRunning, WorkspacePhase: &ph, Binding: wsBinding()}
	for name, s := range map[string]TaskWorkspaceStatus{"published": statusOf(), "pending": pending} {
		b, err := Encode(TaskWorkspaceStatusResponse{Workspace: s})
		if err != nil {
			t.Fatal(err)
		}
		// Every payload key is present, nulls included, under version 6.
		var top map[string]json.RawMessage
		var payload map[string]json.RawMessage
		json.Unmarshal(b, &top)
		json.Unmarshal(top["workspace"], &payload)
		if string(top["version"]) != "6" || len(top) != 2 || len(payload) != 6 {
			t.Fatalf("%s envelope %s", name, b)
		}
		got, err := ParseTaskWorkspaceStatusResponse(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		again, _ := Encode(got)
		if string(again) != string(b) || got.Version != ProtocolVersion {
			t.Fatalf("%s round trip %s != %s", name, again, b)
		}
	}
	if b, _ := Encode(TaskWorkspaceStatusResponse{Workspace: pending}); !strings.Contains(string(b), `"result":null,"available":false`) {
		t.Fatalf("pending %s", b)
	}
	// A recreated or pruned workspace: available=false with the result
	// intact.
	gone := statusOf()
	gone.Available = false
	if err := gone.Validate(); err != nil {
		t.Fatal(err)
	}
	good, _ := Encode(TaskWorkspaceStatusResponse{Workspace: statusOf()})
	edit := func(f func(*TaskWorkspaceStatus)) []byte {
		s := statusOf()
		f(&s)
		b, _ := compact(struct {
			Version   int                 `json:"version"`
			Workspace TaskWorkspaceStatus `json:"workspace"`
		}{6, s})
		return b
	}
	failed := NewPublicationFailed(wsBinding(), PubErrStorageFailed)
	other := wsBinding()
	other.Instance = strings.Repeat("f", 32)
	otherRes := wsPublished(other, deliveryTask)
	wrongRef := wsPublished(wsBinding(), "t_"+strings.Repeat("9", 32))
	for name, b := range map[string][]byte{
		"too-large":       append(good, make([]byte, MaxWorkspaceResponse)...),
		"version":         []byte(strings.Replace(string(good), `"version":6`, `"version":5`, 1)),
		"no-version":      []byte(`{"workspace":` + string(good[len(`{"version":6,"workspace":`):])),
		"extra-key":       []byte(strings.Replace(string(good), `"available":true`, `"available":true,"x":1`, 1)),
		"dup-key":         []byte(strings.Replace(string(good), `"available":true`, `"available":true,"available":true`, 1)),
		"missing-result":  []byte(strings.Replace(string(good), `"result":`, `"resultx":`, 1)),
		"null-binding":    []byte(strings.Replace(string(good), string(mustBinding(t)), "null", 1)),
		"bad-task":        edit(func(s *TaskWorkspaceStatus) { s.TaskID = "t_x" }),
		"bad-state":       edit(func(s *TaskWorkspaceStatus) { s.State = "done" }),
		"bad-binding":     edit(func(s *TaskWorkspaceStatus) { s.Binding.Instance = "x" }),
		"terminal-phase":  edit(func(s *TaskWorkspaceStatus) { s.WorkspacePhase = &ph }),
		"running-result":  edit(func(s *TaskWorkspaceStatus) { s.State = TaskRunning; s.WorkspacePhase = &ph }),
		"terminal-no-res": edit(func(s *TaskWorkspaceStatus) { s.Result, s.Available = nil, false }),
		"bad-phase": edit(func(s *TaskWorkspaceStatus) {
			p := "waiting"
			s.State, s.WorkspacePhase, s.Result, s.Available = TaskRunning, &p, nil, false
		}),
		"failed-avail":   edit(func(s *TaskWorkspaceStatus) { s.Result = &failed }),
		"other-instance": edit(func(s *TaskWorkspaceStatus) { s.Result = &otherRes }),
		"other-ref":      edit(func(s *TaskWorkspaceStatus) { s.Result = &wrongRef }),
		"bad-result":     edit(func(s *TaskWorkspaceStatus) { s.Result.Publication = "done" }),
		"not-json":       []byte(`{`),
		"null-workspace": []byte(`{"version":6,"workspace":null}`),
		"null-task":      []byte(strings.Replace(string(good), `"task_id":"`+deliveryTask+`"`, `"task_id":null`, 1)),
	} {
		if _, err := ParseTaskWorkspaceStatusResponse(b); CodeOf(err) != CodeInvalidArgument {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

func mustBinding(t *testing.T) []byte {
	t.Helper()
	b, err := Encode(wsBinding())
	if err != nil {
		t.Fatal(err)
	}
	return b
}
