package cli

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Iteration 10c (UT-C1/UT-C2/UT-C3): the CLI's workspace doors. Dispatch
// forwards --workspace, --base and --workspace-instance unchanged to the
// shared contract (refused before any request when invalid) and renders
// the binding, phase and terminal result metadata; ws pull, status and diff
// accept a TASK_ID form through the typed client.

const (
	dlInst   = "0123456789abcdef0123456789abcdef"
	dlBase   = "1111111111111111111111111111111111111111"
	dlResult = "2222222222222222222222222222222222222222"
)

// wsView is a workspace task view of cliTask in state with base (nil: the
// explicit empty base) and, when terminal, result w.
func wsView(state string, base *string, w *contract.TaskWorkspaceResult) contract.TaskView {
	v := cliView(cliTask, contract.TaskPending)
	if state != contract.TaskPending {
		v = cliView(cliTask, state)
	}
	ws := "proj"
	v.Request.Workspace = &ws
	b := contract.WorkspaceBinding{Name: ws, Instance: dlInst, BaseSelector: contract.DefaultBranchRef, BaseCommit: base}
	if base == nil {
		e := contract.SelectorEmpty
		v.Request.Base, b.BaseSelector = &e, contract.SelectorEmpty
	}
	v.WorkspaceBinding = &b
	if !contract.TaskTerminal(state) {
		p := contract.WorkspacePhasePreparing
		if state == contract.TaskRunning {
			p = contract.WorkspacePhaseExecuting
		}
		v.WorkspacePhase = &p
		return v
	}
	r := v.Result.WithWorkspace(w)
	v.Result = &r
	return v
}

func dlPublished(base *string, paths ...string) *contract.TaskWorkspaceResult {
	c, ref := dlResult, contract.TaskRefPrefix+cliTask
	w := &contract.TaskWorkspaceResult{Name: "proj", Instance: dlInst, BaseCommit: base, Publication: contract.PublicationPublished, Commit: &c, Ref: &ref,
		Diffstat: &contract.TaskDiffstat{Added: int64(len(paths)), NewBytes: 7}, Changes: []contract.WorkspaceChange{}}
	mode, n := "100644", int64(1)
	for _, p := range paths {
		w.Changes = append(w.Changes, contract.WorkspaceChange{PathBase64: contract.EncodePath([]byte(p)), Kind: "added", NewMode: &mode, NewBytes: &n})
	}
	return w
}

func TestWorkspaceDispatchCLI(t *testing.T) {
	sp := startStubPlane(t)
	trust := []string{"--plane", sp.url, "--ca", sp.caFile}
	old := hostname
	hostname = func() (string, error) { return "coord-host", nil }
	t.Cleanup(func() { hostname = old })
	// All three fields reach the plane unchanged; the admitted view prints
	// its binding and phase.
	pending := wsView(contract.TaskPending, nil, nil)
	pending.Request.WorkspaceInstance = new(string)
	*pending.Request.WorkspaceInstance = dlInst
	sp.answer(202, envelope(t, contract.DispatchResponse{Version: 6, TaskID: cliTask, Task: &pending}))
	code, out, errOut := exec(t, "linux", append([]string{"dispatch", "--role-name", "coder", "--goal", "fix \"it\"", "--acceptance", "tests pass",
		"--workspace", "proj", "--base", "empty", "--workspace-instance", dlInst}, trust...)...)
	if code != 0 || errOut != "" {
		t.Fatalf("dispatch %d %q", code, errOut)
	}
	var body map[string]json.RawMessage
	json.Unmarshal([]byte(sp.last().body), &body)
	if string(body["workspace"]) != `"proj"` || string(body["base"]) != `"empty"` || string(body["workspace_instance"]) != `"`+dlInst+`"` {
		t.Fatalf("request %s", sp.last().body)
	}
	if !strings.HasSuffix(out, "timeout_policy: enforced\nworkspace: proj\nworkspace_instance: "+dlInst+"\nbase_commit: -\nworkspace_phase: preparing\n") {
		t.Fatalf("text %q", out)
	}
	// A task ID base is forwarded as given (ParseTaskBase is the authority).
	sp.answer(202, envelope(t, contract.DispatchResponse{Version: 6, TaskID: cliTask, Task: ptrView(cliView(cliTask, contract.TaskPending))}))
	exec(t, "linux", append([]string{"dispatch", "--role-name", "coder", "--goal", "fix \"it\"", "--acceptance", "tests pass", "--workspace", "proj", "--base", cliTaskB}, trust...)...)
	if json.Unmarshal([]byte(sp.last().body), &body); string(body["base"]) != `"`+cliTaskB+`"` {
		t.Fatalf("task base %s", sp.last().body)
	}
	// Scratch dispatch is unchanged: no workspace keys, no workspace lines.
	exec(t, "linux", append([]string{"dispatch", "--role-name", "coder", "--goal", "fix \"it\"", "--acceptance", "tests pass"}, trust...)...)
	if b := sp.last().body; strings.Contains(b, "workspace") || strings.Contains(b, `"base"`) {
		t.Fatalf("scratch request %s", b)
	}
	// Invalid combinations never dispatch.
	before := sp.count()
	for _, c := range []struct {
		args []string
		msg  string
	}{
		{[]string{"--workspace", ""}, "workspace name"},
		{[]string{"--workspace", "Proj"}, "workspace name"},
		{[]string{"--base", "main"}, "base requires workspace"},
		{[]string{"--workspace-instance", dlInst}, "workspace_instance requires workspace"},
		{[]string{"--workspace", "proj", "--workspace", "proj"}, "may be given only once"},
		{[]string{"--workspace", "proj", "--base", ""}, "invalid base"},
		{[]string{"--workspace", "proj", "--base", "HEAD~1"}, "invalid base"},
		{[]string{"--workspace", "proj", "--workspace-instance", "x"}, "32-hex"},
		{[]string{"--workspace", "proj", "--workspace-instance", ""}, "32-hex"},
	} {
		code, out, errOut := exec(t, "linux", append(append([]string{"dispatch", "--role-id", "a", "--goal", "g", "--acceptance", "a"}, c.args...), trust...)...)
		if code != 2 || out != "" || !strings.Contains(errOut, c.msg) {
			t.Fatalf("%v = %d %q", c.args, code, errOut)
		}
	}
	if sp.count() != before {
		t.Fatalf("%d invalid dispatches were sent", sp.count()-before)
	}
	// Help documents the flags and the admission rules; the obsolete
	// sentence is gone.
	_, help, _ := exec(t, "linux", "dispatch", "--help")
	for _, want := range []string{"--workspace NAME", "--base SELECTOR", "--workspace-instance TOKEN", "fixed at admission", "needs --base empty"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help lacks %q", want)
		}
	}
	if strings.Contains(help, "Workspaces are not supported") {
		t.Fatal("help keeps the obsolete sentence")
	}
}

func TestRenderTaskWorkspace(t *testing.T) {
	base := dlBase
	pub := wsView(contract.TaskSucceeded, &base, dlPublished(&base, "a.txt", "esc\x1b.txt"))
	text := RenderTask(pub)
	for _, want := range []string{"workspace: proj\n", "workspace_instance: " + dlInst + "\n", "base_commit: " + dlBase + "\n", "workspace_phase: -\n",
		"publication: published\n", "result_commit: " + dlResult + "\n", "result_ref: refs/callsheet/tasks/" + cliTask + "\n", "publication_error: -\n",
		"diffstat: added=2 modified=0 deleted=0 old_bytes=0 new_bytes=7\n", "changed_path: \"a.txt\"\n", "changed_path: \"esc\\x1b.txt\"\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("published text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") || strings.Contains(text, "changed_paths_truncated") {
		t.Fatalf("unsafe or truncated text %q", text)
	}
	// Truncation is explicit with its cursor.
	w := dlPublished(&base, "a.txt")
	w.ChangesTruncated = true
	w.NextAfter = &w.Changes[0].PathBase64
	trunc := wsView(contract.TaskSucceeded, &base, w)
	if text := RenderTask(trunc); !strings.Contains(text, "changed_paths_truncated: true\nchanged_paths_next_after: "+w.Changes[0].PathBase64+"\n") {
		t.Fatalf("truncated %s", text)
	}
	// A failed publication has no hash, ref or totals.
	failed := contract.NewPublicationFailed(*pub.WorkspaceBinding, contract.PubErrStorageFailed)
	if text := RenderTask(wsView(contract.TaskSucceeded, &base, &failed)); !strings.Contains(text, "publication: failed\nresult_commit: -\nresult_ref: -\npublication_error: workspace_storage_failed\ndiffstat: -\n") {
		t.Fatalf("failed %s", text)
	}
	// A running task shows its phase and no result block; a scratch task
	// has no workspace lines at all.
	if text := RenderTask(wsView(contract.TaskRunning, &base, nil)); !strings.Contains(text, "workspace_phase: executing\n") || strings.Contains(text, "publication:") {
		t.Fatalf("running %s", text)
	}
	if text := RenderTask(cliView(cliTask, contract.TaskSucceeded)); strings.Contains(text, "workspace") {
		t.Fatalf("scratch %s", text)
	}
	// The exact versioned JSON of show carries the binding and the result.
	b, _ := contract.Encode(contract.TaskShowResponse{Version: 6, Task: pub})
	if v, err := contract.ParseTaskShowResponse(b); err != nil || v.Result.Workspace.Commit == nil {
		t.Fatalf("show json %v", err)
	}
}

// deliverySeams replaces the task-ID client operations.
type deliverySeams struct {
	resolved []string
	sel      client.TaskResultSelection
	rerr     error
	status   contract.TaskWorkspaceStatusResponse
	serr     error
	pages    []client.TaskDiffPage
	diff     contract.WorkspaceDiffResponse
	derr     error
}

func seamDelivery(t *testing.T) *deliverySeams {
	t.Helper()
	d := &deliverySeams{}
	or, os, od := resolveTaskResult, taskWorkspaceStatus, taskWorkspaceDiff
	t.Cleanup(func() { resolveTaskResult, taskWorkspaceStatus, taskWorkspaceDiff = or, os, od })
	resolveTaskResult = func(_ *client.Client, _ context.Context, id string) (client.TaskResultSelection, error) {
		d.resolved = append(d.resolved, id)
		return d.sel, d.rerr
	}
	taskWorkspaceStatus = func(_ *client.Client, _ context.Context, id string) (contract.TaskWorkspaceStatusResponse, error) {
		d.resolved = append(d.resolved, id)
		return d.status, d.serr
	}
	taskWorkspaceDiff = func(_ *client.Client, _ context.Context, id string, p client.TaskDiffPage) (contract.WorkspaceDiffResponse, error) {
		d.resolved, d.pages = append(d.resolved, id), append(d.pages, p)
		return d.diff, d.derr
	}
	return d
}

func TestWorkspaceTaskLeaves(t *testing.T) {
	isolate(t)
	s := seamTransfers(t)
	d := seamDelivery(t)
	trust := caFlags(t)
	base := dlBase
	d.sel = client.TaskResultSelection{Name: "proj", Instance: dlInst, Ref: contract.TaskRefPrefix + cliTask, Commit: dlResult, BaseCommit: &base}
	local := "refs/callsheet/proj/tasks/" + cliTask
	s.lres = contract.WorkspacePullResult{Name: "proj", Instance: dlInst, Selector: d.sel.Ref, Commit: dlResult, DestinationKind: contract.KindGit, LocalRef: &local, Changed: true}
	t.Run("pull", func(t *testing.T) {
		code, out, errOut := exec(t, "linux", append([]string{"ws", "pull"}, append(trust, cliTask, "repo")...)...)
		want := workspacetransfer.PullRequest{Name: "proj", Ref: d.sel.Ref, Path: "repo", PathSet: true, ExpectedInstance: dlInst, ExpectedCommit: dlResult}
		if code != 0 || errOut != "" || len(s.pull) != 1 || s.pull[0] != want || !strings.Contains(out, "selector: "+d.sel.Ref+"\n") {
			t.Fatalf("task pull %d %q %+v", code, errOut, s.pull)
		}
		// The path defaults to the process working directory.
		exec(t, "linux", append([]string{"ws", "pull"}, append(trust, cliTask)...)...)
		if s.pull[1].PathSet || s.pull[1].ExpectedCommit != dlResult {
			t.Fatalf("default path %+v", s.pull[1])
		}
		// Explicit form: a bare task ID means its task ref; refs/heads/
		// escapes to a branch.
		exec(t, "linux", append([]string{"ws", "pull"}, append(trust, "proj", cliTaskB)...)...)
		exec(t, "linux", append([]string{"ws", "pull"}, append(trust, "proj", "refs/heads/"+cliTaskB)...)...)
		if s.pull[2].Ref != contract.TaskRefPrefix+cliTaskB || s.pull[2].ExpectedInstance != "" || s.pull[3].Ref != "refs/heads/"+cliTaskB {
			t.Fatalf("explicit %+v", s.pull[2:])
		}
		// A lookup refusal keeps its exit code and nothing is transferred.
		d.rerr = contract.TaskResultPending(cliTask)
		if code, _, errOut := exec(t, "linux", append([]string{"ws", "pull"}, append(trust, cliTask)...)...); code != 4 || !strings.Contains(errOut, "task_result_pending") && !strings.Contains(errOut, "no workspace result yet") {
			t.Fatalf("pending %d %q", code, errOut)
		}
		d.rerr = nil
		n := len(s.pull)
		for _, args := range [][]string{{cliTask, "a", "b"}, {"t_bad"}, {"t_bad", "main"}, {}} {
			if code, _, _ := exec(t, "linux", append([]string{"ws", "pull"}, append(trust, args...)...)...); code != 2 {
				t.Fatalf("%v = %d", args, code)
			}
		}
		if len(s.pull) != n {
			t.Fatal("an invalid pull transferred")
		}
	})
	t.Run("status", func(t *testing.T) {
		r := dlPublished(&base, "a.txt")
		d.status = contract.TaskWorkspaceStatusResponse{Version: 6, Workspace: contract.TaskWorkspaceStatus{TaskID: cliTask, State: contract.TaskSucceeded,
			Binding: contract.WorkspaceBinding{Name: "proj", Instance: dlInst, BaseSelector: contract.DefaultBranchRef, BaseCommit: &base}, Result: r, Available: true}}
		code, out, _ := exec(t, "linux", append([]string{"ws", "status"}, append(trust, cliTask)...)...)
		for _, want := range []string{"task_id: " + cliTask + "\nstate: succeeded\nworkspace_phase: -\nworkspace: proj\n", "base_commit: " + dlBase + "\navailable: true\npublication: published\n",
			"result_commit: " + dlResult + "\n", "KIND\tOLD_MODE\tNEW_MODE\tOLD_BYTES\tNEW_BYTES\tPATH\nadded\t-\t100644\t-\t1\t\"a.txt\"\n"} {
			if code != 0 || !strings.Contains(out, want) {
				t.Fatalf("status text lacks %q: %d %q", want, code, out)
			}
		}
		code, out, _ = exec(t, "linux", append([]string{"ws", "status", "--json"}, append(trust, cliTask)...)...)
		if code != 0 || out != envelope(t, d.status)+"\n" {
			t.Fatalf("status json %d %q", code, out)
		}
		// A pending task: phase, no result, unavailable.
		p := contract.WorkspacePhasePublishing
		d.status.Workspace.State, d.status.Workspace.WorkspacePhase, d.status.Workspace.Result, d.status.Workspace.Available = contract.TaskRunning, &p, nil, false
		if _, out, _ := exec(t, "linux", append([]string{"ws", "status"}, append(trust, cliTask)...)...); !strings.Contains(out, "workspace_phase: publishing\n") ||
			!strings.HasSuffix(out, "available: false\npublication: -\n") {
			t.Fatalf("pending %q", out)
		}
		d.serr = contract.NoTaskWorkspace(cliTask)
		if code, _, errOut := exec(t, "linux", append([]string{"ws", "status"}, append(trust, cliTask)...)...); code != 2 || !strings.Contains(errOut, "no workspace") {
			t.Fatalf("scratch %d %q", code, errOut)
		}
		d.serr = nil
		for _, args := range [][]string{{"--limit", "1", cliTask}, {"--after", "x", "--instance", dlInst, "--generation", dlInst, cliTask}, {"t_x"}} {
			if code, _, _ := exec(t, "linux", append([]string{"ws", "status"}, append(trust, args...)...)...); code != 2 {
				t.Fatalf("%v = %d", args, code)
			}
		}
	})
	t.Run("diff", func(t *testing.T) {
		d.diff = contract.WorkspaceDiffResponse{Name: "proj", Instance: dlInst, Generation: dlInst, BaseCommit: &base, TargetCommit: dlResult, Changes: []contract.WorkspaceChange{}}
		code, out, _ := exec(t, "linux", append([]string{"ws", "diff", "--limit", "5"}, append(trust, cliTask)...)...)
		if code != 0 || !strings.HasPrefix(out, "name: proj\ninstance: "+dlInst+"\ngeneration: "+dlInst+"\nbase_commit: "+dlBase+"\ntarget_commit: "+dlResult+"\n") ||
			d.pages[len(d.pages)-1] != (client.TaskDiffPage{Limit: 5}) {
			t.Fatalf("diff %d %q %+v", code, out, d.pages)
		}
		exec(t, "linux", append([]string{"ws", "diff", "--after", "YQ==", "--instance", dlInst, "--generation", dlInst}, append(trust, cliTask)...)...)
		if d.pages[len(d.pages)-1] != (client.TaskDiffPage{After: "YQ==", Instance: dlInst, Generation: dlInst, Limit: 100}) {
			t.Fatalf("continuation %+v", d.pages)
		}
		code, out, _ = exec(t, "linux", append([]string{"ws", "diff", "--json"}, append(trust, cliTask)...)...)
		if code != 0 || out != envelope(t, d.diff)+"\n" {
			t.Fatalf("diff json %q", out)
		}
		d.derr = contract.New(contract.CodeConflict, "the workspace changed since the previous page; restart pagination")
		if code, _, _ := exec(t, "linux", append([]string{"ws", "diff"}, append(trust, cliTask)...)...); code != 4 {
			t.Fatalf("generation conflict %d", code)
		}
		d.derr = nil
		n := len(d.pages)
		for _, args := range [][]string{{"--base", "main", cliTask}, {cliTask, "main"}, {"--after", "YQ==", cliTask}, {"--limit", "0", cliTask}, {"t_x"}} {
			if code, _, _ := exec(t, "linux", append([]string{"ws", "diff"}, append(trust, args...)...)...); code != 2 {
				t.Fatalf("%v = %d", args, code)
			}
		}
		if len(d.pages) != n {
			t.Fatal("an invalid diff reached the client")
		}
	})
	_, help, _ := exec(t, "linux", "ws", "diff", "--help")
	_, shelp, _ := exec(t, "linux", "ws", "status", "--help")
	if !strings.Contains(help, "(NAME TARGET | TASK_ID)") || !strings.Contains(help, "from the first page") || !strings.Contains(shelp, "(NAME | TASK_ID)") || !strings.Contains(shelp, "available") {
		t.Fatalf("help %q %q", help, shelp)
	}
	// ws ref set keeps its selector semantics (not a task-aware door).
	_, rhelp, _ := exec(t, "linux", "ws", "ref", "set", "--help")
	if strings.Contains(rhelp, "bare task ID") {
		t.Fatal("ref set help claims task-ID selection")
	}
}

// The explicit diff form normalizes bare task IDs as base and target
// before the request.
func TestWorkspaceDiffTaskSelectors(t *testing.T) {
	sp := startStubPlane(t)
	trust := []string{"--plane", sp.url, "--ca", sp.caFile}
	target := contract.TaskRefPrefix + cliTaskB
	sp.answer(200, envelope(t, contract.WorkspaceDiffResponse{Name: "proj", Instance: dlInst, Generation: dlInst, TargetCommit: dlResult, Changes: []contract.WorkspaceChange{}}))
	if code, _, errOut := exec(t, "linux", append([]string{"ws", "diff", "--base", "empty"}, append(trust, "proj", cliTaskB)...)...); code != 0 {
		t.Fatalf("diff %d %q", code, errOut)
	}
	q, _ := url.ParseQuery(sp.last().query)
	if q.Get("target") != target || q.Get("base") != "empty" {
		t.Fatalf("query %v", q)
	}
	sp.answer(200, envelope(t, contract.WorkspaceDiffResponse{Name: "proj", Instance: dlInst, Generation: dlInst, BaseCommit: new(string), TargetCommit: dlResult, Changes: []contract.WorkspaceChange{}}))
	exec(t, "linux", append([]string{"ws", "diff", "--base", cliTaskB}, append(trust, "proj", "refs/heads/"+cliTaskB)...)...)
	q, _ = url.ParseQuery(sp.last().query)
	if q.Get("base") != target || q.Get("target") != "refs/heads/"+cliTaskB {
		t.Fatalf("query %v", q)
	}
	before := sp.count()
	for _, args := range [][]string{{"--base", "HEAD~1", "proj", "main"}, {"--base", "empty", "proj", "HEAD~1"}, {"proj", "main"}} {
		if code, _, _ := exec(t, "linux", append([]string{"ws", "diff"}, append(trust, args...)...)...); code != 2 {
			t.Fatalf("%v = %d", args, code)
		}
	}
	if sp.count() != before {
		t.Fatal("an invalid explicit diff was sent")
	}
}
