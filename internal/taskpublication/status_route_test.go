package taskpublication_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/taskworkspace"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/taskhub"
)

// TestTaskWorkspaceStatusRoute qualifies iteration 10c's GET
// /api/v1/tasks/<id>/workspace from outside the plane package (no plane
// stress growth): its exact route and request checks, the pending phase,
// a published result's availability as a fresh observation (false after a
// prune or a recreation, the historical result intact), a failed
// publication, a scratch task and a real storage failure (an error, never
// available=false).
func TestTaskWorkspaceStatusRoute(t *testing.T) {
	t.Parallel()
	w := newWiring(t, filepath.Join(t.TempDir(), "plane"))
	ctx := context.Background()
	name := "proj"
	status := func(id string) contract.TaskWorkspaceStatus {
		t.Helper()
		r, err := w.p.Client.TaskWorkspaceStatus(ctx, id)
		if err != nil {
			t.Fatalf("status %s: %v", id, err)
		}
		return r.Workspace
	}
	// published runs a workspace task to a published result.
	published := func() (contract.TaskView, taskworkspace.Publication) {
		t.Helper()
		v := w.dispatch(t, &name, nil)
		st := taskhub.Recv(t, w.n, w.n.Starts, "task_start")
		dir, p := w.prepare(t, st)
		w.n.Prepared(t, st, nil)
		os.WriteFile(filepath.Join(dir, taskworkspace.WorkName, "new.txt"), []byte("n"), 0o644)
		pub := w.publish(t, st, dir, p, 0)
		if ack := w.n.Result(t, *pub.Result); !ack.Committed {
			t.Fatal("result not committed")
		}
		return v, pub
	}

	// Pending: the phase, no result, not available.
	v := w.dispatch(t, &name, nil)
	st := taskhub.Recv(t, w.n, w.n.Starts, "task_start")
	pending := status(v.TaskID)
	if pending.State != contract.TaskPending || pending.WorkspacePhase == nil || *pending.WorkspacePhase != contract.WorkspacePhasePreparing ||
		pending.Result != nil || pending.Available || pending.Binding.Instance != st.Workspace.Instance || pending.Binding.BaseCommit == nil ||
		*pending.Binding.BaseCommit != *st.Workspace.BaseCommit {
		t.Fatalf("pending %+v", pending)
	}
	// A failed publication on the ordinary result path is never available.
	w.prepare(t, st)
	w.n.Prepared(t, st, nil)
	zero := 0
	f := contract.NewPublicationFailed(*st.Workspace, contract.PubErrPublicationTransportFail)
	w.n.Result(t, contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: &zero, Workspace: &f}.Sealed())
	if s := status(v.TaskID); s.State != contract.TaskSucceeded || s.WorkspacePhase != nil || s.Result == nil || s.Result.Publication != contract.PublicationFailed || s.Available {
		t.Fatalf("failed publication %+v", s)
	}

	// Published: available, the exact result.
	a, pubA := published()
	sa := status(a.TaskID)
	if !sa.Available || sa.Result.Commit == nil || *sa.Result.Commit != *pubA.Result.Workspace.Commit || *sa.Result.Ref != contract.TaskRefPrefix+a.TaskID {
		t.Fatalf("published %+v", sa)
	}

	// Route exactness and request checks.
	hc, _ := testkit.GitHTTPClient(w.p.PEM)
	do := func(method, path, body string, version bool) int {
		t.Helper()
		req, _ := http.NewRequest(method, w.p.URL+path, strings.NewReader(body))
		if version {
			req.Header.Set(contract.ProtocolHeader, strconv.Itoa(contract.ProtocolVersion))
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	path := contract.TaskWorkspacePath(a.TaskID)
	for what, c := range map[string]struct {
		method, path, body string
		version            bool
		status             int
	}{
		"get":           {http.MethodGet, path, "", true, 200},
		"post":          {http.MethodPost, path, "", true, 400},
		"query":         {http.MethodGet, path + "?lines=1", "", true, 400},
		"empty-query":   {http.MethodGet, path + "?", "", true, 400},
		"body":          {http.MethodGet, path, "x", true, 400},
		"extra-segment": {http.MethodGet, path + "/x", "", true, 404},
		"prefix-only":   {http.MethodGet, path + "x", "", true, 404},
		"trailing":      {http.MethodGet, path + "/", "", true, 404},
		"bad-id":        {http.MethodGet, contract.TaskWorkspacePath("t_x"), "", true, 400},
		"unknown":       {http.MethodGet, contract.TaskWorkspacePath("t_" + strings.Repeat("0", 32)), "", true, 404},
		"no-version":    {http.MethodGet, path, "", false, 400},
		// The publication routes are unchanged (assignment headers first).
		"publication": {http.MethodGet, contract.PublicationPath(a.TaskID, "", false), "", true, 400},
	} {
		if got := do(c.method, c.path, c.body, c.version); got != c.status {
			t.Fatalf("%s: HTTP %d, want %d", what, got, c.status)
		}
	}

	// A real storage failure is an error, never available=false.
	view, err := w.p.Client.ShowWorkspace(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	refsDocs, _ := filepath.Glob(filepath.Join(w.p.Root, "*", name, "generations", view.Generation, "refs.json"))
	if len(refsDocs) != 1 {
		refsDocs, _ = filepath.Glob(filepath.Join(w.p.Root, "*", "*", name, "generations", view.Generation, "refs.json"))
	}
	if len(refsDocs) != 1 {
		t.Fatalf("refs document not found under %s", w.p.Root)
	}
	good, err := os.ReadFile(refsDocs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(refsDocs[0], []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := w.p.Client.TaskWorkspaceStatus(ctx, a.TaskID)
	if os.WriteFile(refsDocs[0], good, 0o600); err == nil || contract.CodeOf(err) != contract.CodeInternal {
		t.Fatalf("storage failure %+v %v", r, err)
	}
	if !status(a.TaskID).Available {
		t.Fatal("not available after the storage recovered")
	}

	// Pruned: available=false, the historical result intact.
	if _, err := w.p.Client.PruneWorkspace(ctx, name, w.inst, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if s := status(a.TaskID); s.Available || s.Result == nil || *s.Result.Commit != *pubA.Result.Workspace.Commit {
		t.Fatalf("pruned %+v", s)
	}
	// Recreated (same name, a new instance): available=false.
	b, _ := published()
	if !status(b.TaskID).Available {
		t.Fatal("second result not available")
	}
	if _, err := w.p.Client.RemoveWorkspace(ctx, name, w.inst); err != nil {
		t.Fatal(err)
	}
	if _, err := w.p.Client.CreateWorkspace(ctx, name); err != nil {
		t.Fatal(err)
	}
	if s := status(b.TaskID); s.Available || s.Result == nil || s.Binding.Instance != w.inst {
		t.Fatalf("recreated %+v", s)
	}
	// A scratch task has no workspace status.
	sv := w.dispatch(t, nil, nil)
	taskhub.Recv(t, w.n, w.n.Starts, "task_start")
	_, err = w.p.Client.TaskWorkspaceStatus(ctx, sv.TaskID)
	if ce, _ := err.(*contract.Error); contract.CodeOf(err) != contract.CodeInvalidArgument || ce == nil || ce.Details["reason"] != contract.ReasonNoTaskWorkspace {
		t.Fatalf("scratch %v", err)
	}
}
