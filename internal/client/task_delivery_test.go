package client

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 10c (UT-C2/UT-C3): the task-ID forms' shared client
// resolution over a scriptable plane: the task workspace status, the
// task-result selection and its error precedence, the exact-ref check over
// instance/generation-bound status pages, and the task diff with its
// per-page instance and ref rechecks.

const (
	dInst   = "11111111111111111111111111111111"
	dOther  = "22222222222222222222222222222222"
	dGen    = "33333333333333333333333333333333"
	dGen2   = "44444444444444444444444444444444"
	dBase   = "5555555555555555555555555555555555555555"
	dResult = "6666666666666666666666666666666666666666"
	dElse   = "7777777777777777777777777777777777777777"
)

// fakeHub is the scripted plane state; every field is guarded by mu.
type fakeHub struct {
	mu        sync.Mutex
	view      contract.TaskView
	showCode  int
	instance  string // "" : the workspace does not exist
	gen       string
	refs      []contract.WorkspaceRef
	pageSize  int
	statusErr int // a forced status failure (HTTP code)
	diffInst  string
	diffErr   int
	diffAbort bool   // the diff's connection is dropped without an answer
	wsStatus  string // the task workspace status body ("" : 404)
	lines     []string
	diffs     []url.Values
	statuses  int
	// after runs after each handled request with its path (under mu).
	after func(path string)
}

func errBody(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set(contract.ProtocolHeader, "6")
	w.WriteHeader(status)
	io.WriteString(w, `{"error":{"code":"`+code+`","message":"`+msg+`"}}`)
}

func okBody(t *testing.T, w http.ResponseWriter, v any) {
	w.Header().Set(contract.ProtocolHeader, "6")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, envelopeOf(t, v))
}

func (h *fakeHub) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	defer func() {
		if h.after != nil {
			h.after(r.URL.Path)
		}
	}()
	q := r.URL.Query()
	ws := contract.PathWorkspaces + "/proj"
	switch r.URL.Path {
	case contract.PathTasks + "/" + taskA:
		h.lines = append(h.lines, q.Get("lines"))
		if h.showCode != 0 {
			errBody(w, h.showCode, "not_found", "task does not exist")
			return
		}
		okBody(t, w, contract.TaskShowResponse{Version: 6, Task: h.view})
	case contract.TaskWorkspacePath(taskA):
		if h.wsStatus == "" {
			errBody(w, 404, "not_found", "task does not exist")
			return
		}
		w.Header().Set(contract.ProtocolHeader, "6")
		io.WriteString(w, h.wsStatus)
	case ws:
		if h.instance == "" {
			errBody(w, 404, "not_found", "no such workspace")
			return
		}
		okBody(t, w, contract.WorkspaceView{Name: "proj", Instance: h.instance, CreatedAt: "2026-10-01T00:00:00Z", Generation: h.gen, DefaultBranch: contract.DefaultBranchRef})
	case ws + "/status":
		h.statuses++
		switch {
		case h.statusErr != 0:
			errBody(w, h.statusErr, "internal", "storage failed")
			return
		case h.instance == "":
			errBody(w, 404, "not_found", "no such workspace")
			return
		case q.Get("instance") != "" && (q.Get("instance") != h.instance || q.Get("generation") != h.gen):
			errBody(w, 409, "conflict", "the workspace changed since the previous page")
			return
		}
		after, size := q.Get("after"), h.pageSize
		if size == 0 {
			size = 100
		}
		page := contract.WorkspaceStatusResponse{Name: "proj", Instance: h.instance, Generation: h.gen, DefaultBranch: contract.DefaultBranchRef, Refs: []contract.WorkspaceRef{}}
		for _, ref := range h.refs {
			if ref.Name > after {
				if len(page.Refs) == size {
					last := page.Refs[len(page.Refs)-1].Name
					page.NextAfter = &last
					break
				}
				page.Refs = append(page.Refs, ref)
			}
		}
		okBody(t, w, page)
	case ws + "/diff":
		h.diffs = append(h.diffs, q)
		switch {
		case h.diffAbort:
			// A transport failure: the connection closes without a
			// response (the deferred hooks still run).
			panic(http.ErrAbortHandler)
		case h.diffErr != 0:
			errBody(w, h.diffErr, map[int]string{404: "not_found", 409: "conflict", 500: "internal"}[h.diffErr], "diff refused")
			return
		case q.Get("generation") != "" && q.Get("generation") != h.gen:
			errBody(w, 409, "conflict", "the workspace changed since the previous page")
			return
		}
		inst := h.instance
		if h.diffInst != "" {
			inst = h.diffInst
		}
		var base *string
		if b := q.Get("base"); b != contract.SelectorEmpty {
			base = &b
		}
		mode, n := "100644", int64(1)
		changes := []contract.WorkspaceChange{{PathBase64: contract.EncodePath([]byte("a.txt")), Kind: "added", NewMode: &mode, NewBytes: &n}}
		if q.Get("after") != "" {
			changes = []contract.WorkspaceChange{}
		}
		okBody(t, w, contract.WorkspaceDiffResponse{Name: "proj", Instance: inst, Generation: h.gen, BaseCommit: base, TargetCommit: q.Get("target"), Changes: changes})
	default:
		errBody(w, 404, "not_found", "no such endpoint")
	}
}

// boundView is a terminal (or running) workspace task view of taskA bound
// to proj/dInst with base (nil: the empty base) and result w.
func boundView(state string, base *string, w *contract.TaskWorkspaceResult) contract.TaskView {
	req := taskRequest("g")
	req.Workspace = strPtr("proj")
	b := contract.WorkspaceBinding{Name: "proj", Instance: dInst, BaseSelector: contract.DefaultBranchRef, BaseCommit: base}
	if base == nil {
		req.Base = strPtr(contract.SelectorEmpty)
		b.BaseSelector = contract.SelectorEmpty
	}
	v := taskView(taskA, req, contract.TaskRunning)
	now := "2026-09-27T10:00:00Z"
	v.WorkspaceBinding, v.State, v.StartedAt = &b, state, &now
	if !contract.TaskTerminal(state) {
		p := contract.WorkspacePhaseExecuting
		v.WorkspacePhase = &p
		return v
	}
	zero := 0
	v.FinishedAt = &now
	r := contract.TaskResult{State: state, ExitCode: &zero}.WithWorkspace(w)
	if state == contract.TaskLost {
		r.ExitCode = nil
		v.Reason = &contract.TaskReason{Code: contract.ReasonLeaseExpired, Message: "lease expired"}
	}
	v.Result = &r
	return v
}

func strPtr(s string) *string { return &s }

func publishedResult(base *string) *contract.TaskWorkspaceResult {
	c, ref := dResult, contract.TaskRefPrefix+taskA
	return &contract.TaskWorkspaceResult{Name: "proj", Instance: dInst, BaseCommit: base, Publication: contract.PublicationPublished, Commit: &c, Ref: &ref,
		Diffstat: &contract.TaskDiffstat{Added: 1, NewBytes: 1}, Changes: []contract.WorkspaceChange{}}
}

func taskRefRow(id, commit string) contract.WorkspaceRef {
	at := "2026-10-01T00:00:00Z"
	return contract.WorkspaceRef{Name: contract.TaskRefPrefix + id, Commit: commit, PublishedAt: &at}
}

// deliveryClient starts the fake plane and a verified client.
func deliveryClient(t *testing.T) (*Client, *fakeHub) {
	t.Helper()
	ca := newTestCA(t)
	h := &fakeHub{instance: dInst, gen: dGen}
	s := startServer(t, ca.leaf(t, false), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == contract.PathCA {
			w.Write(ca.pem)
			return
		}
		h.serve(t, w, r)
	}))
	c, err := New(s.url, Trust{CAPEM: ca.pem})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, h
}

// reset scripts a published task whose ref is present among others.
func (h *fakeHub) reset(base *string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.view = boundView(contract.TaskSucceeded, base, publishedResult(base))
	h.showCode, h.instance, h.gen, h.pageSize, h.statusErr, h.diffInst, h.diffErr, h.diffAbort, h.after = 0, dInst, dGen, 0, 0, "", 0, false, nil
	h.refs = []contract.WorkspaceRef{taskRefRow(taskA[:len(taskA)-1]+"0", dElse), taskRefRow(taskA, dResult), {Name: contract.DefaultBranchRef, Commit: dBase}}
	h.lines, h.diffs, h.statuses = nil, nil, 0
}

func (h *fakeHub) set(f func(h *fakeHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

func wantReason(t *testing.T, err error, code contract.Code, reason string) {
	t.Helper()
	ce, _ := err.(*contract.Error)
	if contract.CodeOf(err) != code || (reason != "" && (ce == nil || ce.Details["reason"] != reason)) {
		t.Fatalf("err = %v (%+v), want %s/%s", err, ce, code, reason)
	}
}

func TestResolveTaskResult(t *testing.T) {
	c, h := deliveryClient(t)
	ctx := context.Background()
	base := strPtr(dBase)
	h.reset(base)
	sel, err := c.ResolveTaskResult(ctx, taskA)
	if err != nil || sel.Name != "proj" || sel.Instance != dInst || sel.Ref != contract.TaskRefPrefix+taskA || sel.Commit != dResult || sel.BaseCommit == nil || *sel.BaseCommit != dBase {
		t.Fatalf("selection %+v %v", sel, err)
	}
	// The authoritative view is read without output lines.
	if h.lines[0] != "0" {
		t.Fatalf("lines = %v", h.lines)
	}
	if _, err := c.ResolveTaskResult(ctx, "t_bad"); contract.CodeOf(err) != contract.CodeInvalidArgument || len(h.lines) != 1 {
		t.Fatalf("bad id %v", err)
	}
	failed := contract.NewPublicationFailed(contract.WorkspaceBinding{Name: "proj", Instance: dInst, BaseSelector: contract.DefaultBranchRef, BaseCommit: base}, contract.PubErrStorageFailed)
	notApplicable := contract.NewNotApplicable(contract.WorkspaceBinding{Name: "proj", Instance: dInst, BaseSelector: contract.DefaultBranchRef, BaseCommit: base})
	scratch := taskView(taskA, taskRequest("g"), contract.TaskPending)
	for name, c2 := range map[string]struct {
		f      func(h *fakeHub)
		code   contract.Code
		reason string
	}{
		// No binding is checked before the state.
		"scratch":           {func(h *fakeHub) { h.view = scratch }, contract.CodeInvalidArgument, contract.ReasonNoTaskWorkspace},
		"running":           {func(h *fakeHub) { h.view = boundView(contract.TaskRunning, base, nil) }, contract.CodeConflict, contract.ReasonTaskResultPending},
		"failed-pub":        {func(h *fakeHub) { h.view = boundView(contract.TaskSucceeded, base, &failed) }, contract.CodeConflict, contract.ReasonTaskResultUnavailable},
		"lost":              {func(h *fakeHub) { h.view = boundView(contract.TaskLost, base, &notApplicable) }, contract.CodeConflict, contract.ReasonTaskResultUnavailable},
		"unknown":           {func(h *fakeHub) { h.showCode = 404 }, contract.CodeNotFound, ""},
		"removed":           {func(h *fakeHub) { h.instance = "" }, contract.CodeConflict, contract.ReasonWorkspaceInstanceMismatch},
		"recreated":         {func(h *fakeHub) { h.instance = dOther }, contract.CodeConflict, contract.ReasonWorkspaceInstanceMismatch},
		"pruned":            {func(h *fakeHub) { h.refs = h.refs[2:] }, contract.CodeNotFound, ""},
		"pruned-last":       {func(h *fakeHub) { h.refs = h.refs[:1] }, contract.CodeNotFound, ""},
		"other-hash":        {func(h *fakeHub) { h.refs[1].Commit = dElse }, contract.CodeConflict, contract.ReasonTaskResultUnavailable},
		"storage":           {func(h *fakeHub) { h.statusErr = 500 }, contract.CodeInternal, ""},
		"recreated-in-scan": {func(h *fakeHub) { h.after = recreateAfter(h, "/status", dOther) }, contract.CodeConflict, contract.ReasonWorkspaceInstanceMismatch},
	} {
		h.reset(base)
		h.set(c2.f)
		if name == "recreated-in-scan" {
			// Small pages: the first page shows the old instance, then the
			// workspace is recreated; the continuation conflicts and the
			// fresh ShowWorkspace proves a recreation.
			h.set(func(h *fakeHub) { h.pageSize = 1 })
		}
		_, err := c.ResolveTaskResult(ctx, taskA)
		if ce, _ := err.(*contract.Error); c2.reason == "" && ce != nil && ce.Details["reason"] != nil {
			t.Fatalf("%s: unexpected reason %v", name, ce.Details)
		}
		wantReason(t, err, c2.code, c2.reason)
	}
	// Pages: the ref is found on a later page; a generation change between
	// pages propagates as the plane's conflict (no restart).
	h.reset(base)
	h.set(func(h *fakeHub) { h.pageSize = 1 })
	if _, err := c.ResolveTaskResult(ctx, taskA); err != nil || h.statuses != 2 {
		t.Fatalf("paged %v %d", err, h.statuses)
	}
	h.reset(base)
	h.set(func(h *fakeHub) {
		h.pageSize = 1
		h.after = func(path string) {
			if strings.HasSuffix(path, "/status") {
				h.gen = dGen2
			}
		}
	})
	_, err = c.ResolveTaskResult(ctx, taskA)
	if ce, _ := err.(*contract.Error); contract.CodeOf(err) != contract.CodeConflict || (ce != nil && ce.Details["reason"] == contract.ReasonWorkspaceInstanceMismatch) {
		t.Fatalf("generation change %v", err)
	}
	// A first status page of another instance (recreated after the show).
	h.reset(base)
	h.set(func(h *fakeHub) { h.after = recreateAfter(h, "/proj", dOther) })
	_, err = c.ResolveTaskResult(ctx, taskA)
	wantReason(t, err, contract.CodeConflict, contract.ReasonWorkspaceInstanceMismatch)
	// An invalid selection is refused before any request.
	if err := c.CheckTaskResultRef(ctx, TaskResultSelection{Name: "proj", Instance: "x", Ref: "refs/heads/main", Commit: dResult}); contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("invalid selection %v", err)
	}
}

// recreateAfter returns an after hook that recreates the workspace with
// instance inst once a request whose path ends with suffix was answered.
func recreateAfter(h *fakeHub, suffix, inst string) func(string) {
	return func(path string) {
		if strings.HasSuffix(path, suffix) {
			h.instance, h.gen = inst, dGen2
		}
	}
}

func TestTaskWorkspaceStatusClient(t *testing.T) {
	c, h := deliveryClient(t)
	ctx := context.Background()
	b := contract.WorkspaceBinding{Name: "proj", Instance: dInst, BaseSelector: contract.DefaultBranchRef, BaseCommit: strPtr(dBase)}
	st := contract.TaskWorkspaceStatus{TaskID: taskA, State: contract.TaskSucceeded, Binding: b, Result: publishedResult(strPtr(dBase)), Available: true}
	body := envelopeOf(t, contract.TaskWorkspaceStatusResponse{Workspace: st})
	h.set(func(h *fakeHub) { h.wsStatus = body })
	r, err := c.TaskWorkspaceStatus(ctx, taskA)
	if err != nil || r.Version != 6 || !r.Workspace.Available || r.Workspace.Result.Commit == nil || *r.Workspace.Result.Commit != dResult {
		t.Fatalf("status %+v %v", r, err)
	}
	// Another task's status, a malformed body or a missing task.
	other := st
	other.TaskID = taskB
	other.Result = nil
	other.State = contract.TaskRunning
	p := contract.WorkspacePhasePreparing
	other.WorkspacePhase = &p
	other.Available = false
	for name, body := range map[string]string{"other": envelopeOf(t, contract.TaskWorkspaceStatusResponse{Workspace: other}), "bad": `{"version":6}`} {
		h.set(func(h *fakeHub) { h.wsStatus = body })
		if _, err := c.TaskWorkspaceStatus(ctx, taskA); contract.CodeOf(err) != contract.CodeInternal && contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("%s: %v", name, err)
		}
	}
	h.set(func(h *fakeHub) { h.wsStatus = "" })
	if _, err := c.TaskWorkspaceStatus(ctx, taskA); contract.CodeOf(err) != contract.CodeNotFound {
		t.Fatalf("missing %v", err)
	}
	if _, err := c.TaskWorkspaceStatus(ctx, "x"); contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("bad id %v", err)
	}
}

func TestTaskWorkspaceDiffClient(t *testing.T) {
	c, h := deliveryClient(t)
	ctx := context.Background()
	h.reset(strPtr(dBase))
	r, err := c.TaskWorkspaceDiff(ctx, taskA, TaskDiffPage{Limit: 7})
	if err != nil || r.TargetCommit != dResult || r.BaseCommit == nil || *r.BaseCommit != dBase || len(r.Changes) != 1 {
		t.Fatalf("first page %+v %v", r, err)
	}
	// The immutable full hashes, the limit and no continuation; the exact
	// ref was checked before and again after the page.
	if q := h.diffs[0]; q.Get("base") != dBase || q.Get("target") != dResult || q.Get("limit") != "7" || q.Get("after") != "" || h.statuses != 2 {
		t.Fatalf("diff query %v, %d status reads", q, h.statuses)
	}
	// The empty base compares against the empty tree.
	h.reset(nil)
	if r, err := c.TaskWorkspaceDiff(ctx, taskA, TaskDiffPage{Limit: 100}); err != nil || r.BaseCommit != nil || h.diffs[0].Get("base") != contract.SelectorEmpty {
		t.Fatalf("empty base %+v %v", r, err)
	}
	// A continuation submits the same hashes with the caller's cursor.
	h.reset(strPtr(dBase))
	cont := TaskDiffPage{After: contract.EncodePath([]byte("a.txt")), Instance: dInst, Generation: dGen, Limit: 100}
	if _, err := c.TaskWorkspaceDiff(ctx, taskA, cont); err != nil || h.diffs[0].Get("after") != cont.After || h.diffs[0].Get("generation") != dGen {
		t.Fatalf("continuation %v %v", err, h.diffs)
	}
	for name, c2 := range map[string]struct {
		page   TaskDiffPage
		f      func(h *fakeHub)
		code   contract.Code
		reason string
		diffs  int
	}{
		// A recreation with identical hashes between lookup and diff fails.
		"recreated-same-hashes": {TaskDiffPage{Limit: 100}, func(h *fakeHub) { h.diffInst = dOther }, contract.CodeConflict, contract.ReasonWorkspaceInstanceMismatch, 1},
		"continuation-instance": {TaskDiffPage{After: cont.After, Instance: dOther, Generation: dGen, Limit: 100}, nil, contract.CodeConflict, contract.ReasonWorkspaceInstanceMismatch, 0},
		// A changed generation is the plane's conflict; nothing restarts.
		"generation": {TaskDiffPage{After: cont.After, Instance: dInst, Generation: dGen2, Limit: 100}, nil, contract.CodeConflict, "", 1},
		// A removal observed after the diff's own failure takes precedence.
		"removed-during-diff": {TaskDiffPage{Limit: 100}, func(h *fakeHub) {
			h.diffErr = 404
			h.after = func(path string) {
				if strings.HasSuffix(path, "/diff") {
					h.instance = ""
				}
			}
		}, contract.CodeConflict, contract.ReasonWorkspaceInstanceMismatch, 1},
		"diff-not-found": {TaskDiffPage{Limit: 100}, func(h *fakeHub) { h.diffErr = 404 }, contract.CodeNotFound, "", 1},
		// The exact ref disappears after the page: not_found, page withheld.
		"pruned-after-page": {TaskDiffPage{Limit: 100}, func(h *fakeHub) {
			h.after = func(path string) {
				if strings.HasSuffix(path, "/diff") {
					h.refs = h.refs[2:]
				}
			}
		}, contract.CodeNotFound, "", 1},
		"pending": {TaskDiffPage{Limit: 100}, func(h *fakeHub) { h.view = boundView(contract.TaskRunning, strPtr(dBase), nil) }, contract.CodeConflict, contract.ReasonTaskResultPending, 0},
	} {
		h.reset(strPtr(dBase))
		if c2.f != nil {
			h.set(c2.f)
		}
		_, err := c.TaskWorkspaceDiff(ctx, taskA, c2.page)
		ce, _ := err.(*contract.Error)
		if contract.CodeOf(err) != c2.code || (c2.reason != "" && ce.Details["reason"] != c2.reason) ||
			(c2.reason == "" && ce != nil && ce.Details["reason"] == contract.ReasonWorkspaceInstanceMismatch) || len(h.diffs) != c2.diffs {
			t.Fatalf("%s: %v (%d diffs)", name, err, len(h.diffs))
		}
	}
	// Storage and transport failures of the diff propagate unchanged, even
	// when the task ref is pruned or the workspace removed meanwhile: no
	// recheck runs (the one status scan is the lookup's), so nothing can
	// remap them to not_found or a mismatch. A recheck that itself fails
	// with storage leaves the diff's own conflict standing.
	onDiff := func(h *fakeHub, f func(h *fakeHub)) {
		h.after = func(path string) {
			if strings.HasSuffix(path, "/diff") {
				f(h)
			}
		}
	}
	prune := func(h *fakeHub) { h.refs = h.refs[2:] }
	remove := func(h *fakeHub) { h.instance = "" }
	for name, c2 := range map[string]struct {
		f        func(h *fakeHub)
		code     contract.Code
		statuses int
	}{
		"storage-while-pruned":    {func(h *fakeHub) { h.diffErr = 500; onDiff(h, prune) }, contract.CodeInternal, 1},
		"storage-while-removed":   {func(h *fakeHub) { h.diffErr = 500; onDiff(h, remove) }, contract.CodeInternal, 1},
		"transport-while-pruned":  {func(h *fakeHub) { h.diffAbort = true; onDiff(h, prune) }, contract.CodeUnavailable, 1},
		"transport-while-removed": {func(h *fakeHub) { h.diffAbort = true; onDiff(h, remove) }, contract.CodeUnavailable, 1},
		"conflict-recheck-storage": {func(h *fakeHub) {
			h.diffErr = 409
			onDiff(h, func(h *fakeHub) { h.statusErr = 500 })
		}, contract.CodeConflict, 2},
	} {
		h.reset(strPtr(dBase))
		h.set(c2.f)
		_, err := c.TaskWorkspaceDiff(ctx, taskA, TaskDiffPage{Limit: 100})
		ce, _ := err.(*contract.Error)
		h.mu.Lock()
		statuses, diffs := h.statuses, len(h.diffs)
		h.mu.Unlock()
		if contract.CodeOf(err) != c2.code || (ce != nil && ce.Details["reason"] != nil) || statuses != c2.statuses || diffs == 0 {
			t.Fatalf("%s: %v (%d status reads, %d diffs)", name, err, statuses, diffs)
		}
	}
	// Invalid arguments are refused before any request.
	h.reset(strPtr(dBase))
	for _, p := range []TaskDiffPage{{Limit: 0}, {Limit: 101}, {After: "YQ==", Limit: 1}, {After: "YQ", Instance: dInst, Generation: dGen, Limit: 1},
		{After: "YQ==", Instance: "x", Generation: dGen, Limit: 1}} {
		if _, err := c.TaskWorkspaceDiff(ctx, taskA, p); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("%+v: %v", p, err)
		}
	}
	if _, err := c.TaskWorkspaceDiff(ctx, "t_"+strconv.Itoa(1), TaskDiffPage{Limit: 1}); contract.CodeOf(err) != contract.CodeInvalidArgument || len(h.lines) != 0 {
		t.Fatalf("bad id %v %v", err, h.lines)
	}
}
