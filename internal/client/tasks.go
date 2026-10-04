package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The four task operations (iteration 05): typed inputs and domain
// results independent of formatting, so the CLI and a future MCP server
// mirror them one to one. They use the verified client, the protocol
// header, the ten-second operation deadline and the shared error mapping;
// responses are decoded strictly with operation-specific bounds. Nothing
// is cached and nothing is retried: a lost dispatch response may hide a
// committed task, which task ls shows.

func invalidTaskID() error {
	return contract.New(contract.CodeInvalidArgument, "invalid task ID; want t_ followed by 32 lowercase hex digits")
}

// Dispatch submits req and returns the admitted task's view (HTTP 202:
// admission, not execution). An admission failure carries the reason and
// the candidate snapshot in its details.
func (c *Client) Dispatch(ctx context.Context, req contract.DispatchRequest) (contract.TaskView, error) {
	if err := req.Validate(); err != nil {
		return contract.TaskView{}, err
	}
	body, err := encodeBody(req)
	if err != nil {
		return contract.TaskView{}, err
	}
	status, b, err := c.request(ctx, http.MethodPost, contract.PathTasks, body, contract.MaxTaskViewBytes)
	if err != nil {
		return contract.TaskView{}, err
	}
	if status != http.StatusAccepted {
		return contract.TaskView{}, invalidResponse("unexpected status for a dispatch")
	}
	v, err := contract.ParseDispatchResponse(b)
	if err != nil {
		return contract.TaskView{}, err
	}
	if v.Request.Goal != req.Goal || v.Request.Target != req.Target {
		return contract.TaskView{}, invalidResponse("it describes another request")
	}
	return v, nil
}

// ListTasks returns one page of tasks ordered by task ID after the
// exclusive cursor after ("" for the first page), at most limit (0: the
// plane's default of 100), and the next cursor when more rows exist.
func (c *Client) ListTasks(ctx context.Context, after string, limit int) ([]contract.TaskSummary, *string, error) {
	q := url.Values{}
	if after != "" {
		if !contract.ValidTaskID(after) {
			return nil, nil, invalidTaskID()
		}
		q.Set("after", after)
	}
	if limit != 0 {
		if limit < 1 || limit > contract.MaxTaskListLimit {
			return nil, nil, contract.New(contract.CodeInvalidArgument, "limit must be an integer from 1 to "+strconv.Itoa(contract.MaxTaskListLimit))
		}
		q.Set("limit", strconv.Itoa(limit))
	}
	path := contract.PathTasks
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	status, b, err := c.request(ctx, http.MethodGet, path, nil, contract.MaxTaskListBytes)
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusOK {
		return nil, nil, invalidResponse("unexpected status for a task list")
	}
	tasks, next, err := contract.ParseTaskListResponse(b)
	if err != nil {
		return nil, nil, err
	}
	if len(tasks) > 0 && after != "" && tasks[0].TaskID <= after {
		return nil, nil, invalidResponse("it is not after the cursor")
	}
	if limit != 0 && len(tasks) > limit {
		return nil, nil, invalidResponse("it has more tasks than the limit")
	}
	return tasks, next, nil
}

// ShowTask returns task id with its last lines (0..200) of output.
func (c *Client) ShowTask(ctx context.Context, id string, lines int) (contract.TaskView, error) {
	if !contract.ValidTaskID(id) {
		return contract.TaskView{}, invalidTaskID()
	}
	if lines < 0 || lines > contract.MaxTailLines {
		return contract.TaskView{}, contract.New(contract.CodeInvalidArgument, "lines must be an integer from 0 to "+strconv.Itoa(contract.MaxTailLines))
	}
	status, b, err := c.request(ctx, http.MethodGet, contract.PathTasks+"/"+id+"?lines="+strconv.Itoa(lines), nil, contract.MaxTaskViewBytes)
	if err != nil {
		return contract.TaskView{}, err
	}
	if status != http.StatusOK {
		return contract.TaskView{}, invalidResponse("unexpected status for a task show")
	}
	v, err := contract.ParseTaskShowResponse(b)
	if err != nil {
		return contract.TaskView{}, err
	}
	if v.TaskID != id {
		return contract.TaskView{}, invalidResponse("it names another task")
	}
	return v, nil
}

// TaskLogs returns task id's complete retained output, byte exact.
func (c *Client) TaskLogs(ctx context.Context, id string) (contract.TaskLogsResponse, error) {
	if !contract.ValidTaskID(id) {
		return contract.TaskLogsResponse{}, invalidTaskID()
	}
	status, b, err := c.request(ctx, http.MethodGet, contract.PathTasks+"/"+id+"/logs", nil, contract.MaxTaskLogsBytes)
	if err != nil {
		return contract.TaskLogsResponse{}, err
	}
	if status != http.StatusOK {
		return contract.TaskLogsResponse{}, invalidResponse("unexpected status for task logs")
	}
	r, err := contract.ParseTaskLogsResponse(b)
	if err != nil {
		return contract.TaskLogsResponse{}, err
	}
	if r.TaskID != id {
		return contract.TaskLogsResponse{}, invalidResponse("it names another task")
	}
	return r, nil
}

// TaskLateLogs returns task id's late-evidence output tail (iteration
// 06a), byte exact; a task without a late result is not_found with reason
// no_late_result.
func (c *Client) TaskLateLogs(ctx context.Context, id string) (contract.TaskLogsResponse, error) {
	if !contract.ValidTaskID(id) {
		return contract.TaskLogsResponse{}, invalidTaskID()
	}
	status, b, err := c.request(ctx, http.MethodGet, contract.PathTasks+"/"+id+"/logs?late=true", nil, contract.MaxTaskLogsBytes)
	if err != nil {
		return contract.TaskLogsResponse{}, err
	}
	if status != http.StatusOK {
		return contract.TaskLogsResponse{}, invalidResponse("unexpected status for task logs")
	}
	r, err := contract.ParseTaskLogsResponse(b)
	if err != nil {
		return contract.TaskLogsResponse{}, err
	}
	if r.TaskID != id {
		return contract.TaskLogsResponse{}, invalidResponse("it names another task")
	}
	return r, nil
}

// ---- Task workspace delivery (iteration 10c) ----

// TaskWorkspaceStatus returns task id's workspace status envelope: its
// state, phase, binding, bounded terminal result and the availability of
// its exact result ref (a fresh observation, not a retention lease).
func (c *Client) TaskWorkspaceStatus(ctx context.Context, id string) (contract.TaskWorkspaceStatusResponse, error) {
	if !contract.ValidTaskID(id) {
		return contract.TaskWorkspaceStatusResponse{}, invalidTaskID()
	}
	status, b, err := c.request(ctx, http.MethodGet, contract.TaskWorkspacePath(id), nil, contract.MaxWorkspaceResponse)
	if err != nil {
		return contract.TaskWorkspaceStatusResponse{}, err
	}
	if status != http.StatusOK {
		return contract.TaskWorkspaceStatusResponse{}, invalidResponse("unexpected status for a task workspace status")
	}
	r, err := contract.ParseTaskWorkspaceStatusResponse(b)
	if err != nil {
		return contract.TaskWorkspaceStatusResponse{}, err
	}
	if r.Workspace.TaskID != id {
		return contract.TaskWorkspaceStatusResponse{}, invalidResponse("it names another task")
	}
	return r, nil
}

// TaskResultSelection is a task's published result as the task-ID forms
// select it: the bound workspace name and instance, the canonical task ref
// and its immutable result commit, and the admitted base commit (null for
// the empty base), the base of a task diff.
type TaskResultSelection struct {
	Name, Instance, Ref, Commit string
	BaseCommit                  *string
}

// taskResultOf selects a task view's published result: no binding is
// invalid_argument/no_task_workspace (checked first), a nonterminal bound
// task conflict/task_result_pending, and a terminal one without a
// published result conflict/task_result_unavailable. Nothing of the view's
// final message or output is kept.
func taskResultOf(v contract.TaskView) (TaskResultSelection, error) {
	b := v.WorkspaceBinding
	switch {
	case b == nil:
		return TaskResultSelection{}, contract.NoTaskWorkspace(v.TaskID)
	case !contract.TaskTerminal(v.State):
		return TaskResultSelection{}, contract.TaskResultPending(v.TaskID)
	case v.Result == nil || v.Result.Workspace == nil:
		return TaskResultSelection{}, contract.TaskResultUnavailable(v.TaskID, "the task ("+v.State+") has no workspace result")
	}
	w := v.Result.Workspace
	if w.Publication != contract.PublicationPublished || w.Commit == nil {
		return TaskResultSelection{}, contract.TaskResultUnavailable(v.TaskID, "its publication is "+w.Publication+" (task "+v.State+")")
	}
	s := TaskResultSelection{Name: b.Name, Instance: b.Instance, Ref: contract.TaskRefPrefix + v.TaskID, Commit: *w.Commit}
	if b.BaseCommit != nil {
		bc := *b.BaseCommit
		s.BaseCommit = &bc
	}
	return s, nil
}

// ResolveTaskResult resolves task id to its bound workspace instance and
// immutable published result (ShowTask with no output lines), then
// confirms with CheckTaskResultRef that the current matching instance
// holds exactly that task ref and hash. There is no name inference, no
// fallback to a recreated workspace of the same name and no local cache.
func (c *Client) ResolveTaskResult(ctx context.Context, id string) (TaskResultSelection, error) {
	if !contract.ValidTaskID(id) {
		return TaskResultSelection{}, invalidTaskID()
	}
	v, err := c.ShowTask(ctx, id, 0)
	if err != nil {
		return TaskResultSelection{}, err
	}
	s, err := taskResultOf(v)
	if err != nil {
		return TaskResultSelection{}, err
	}
	if err := c.CheckTaskResultRef(ctx, s); err != nil {
		return TaskResultSelection{}, err
	}
	return s, nil
}

// CheckTaskResultRef observes s's exact task ref in its bound instance:
// ShowWorkspace first checks the instance, then the raw-ref-sorted status
// pages (bound by their instance and generation) are scanned until the
// ref is found or passed. A missing workspace or a changed instance is
// conflict/workspace_instance_mismatch, a missing ref not_found (pruned)
// and a ref naming another hash conflict/task_result_unavailable; a
// generation change between pages and storage or transport errors
// propagate (nothing restarts). It grants no retention lease.
func (c *Client) CheckTaskResultRef(ctx context.Context, s TaskResultSelection) error {
	if !contract.ValidWorkspaceName(s.Name) || !contract.ValidWorkspaceToken(s.Instance) || !contract.ValidTaskRef(s.Ref) || !contract.ValidCommitHash(s.Commit) {
		return contract.New(contract.CodeInvalidArgument, "a task result selection names a workspace, its instance, a task ref and a full commit hash")
	}
	id := s.Ref[len(contract.TaskRefPrefix):]
	mismatch := contract.WorkspaceInstanceMismatch(s.Name)
	// current reports whether the workspace still has the bound instance.
	current := func() error {
		v, err := c.ShowWorkspace(ctx, s.Name)
		switch {
		case contract.CodeOf(err) == contract.CodeNotFound:
			return mismatch
		case err != nil:
			return err
		case v.Instance != s.Instance:
			return mismatch
		}
		return nil
	}
	if err := current(); err != nil {
		return err
	}
	page := StatusPage{Limit: contract.MaxWorkspaceLimit}
	for {
		r, err := c.WorkspaceStatus(ctx, s.Name, page)
		switch {
		case contract.CodeOf(err) == contract.CodeNotFound:
			return mismatch
		case contract.CodeOf(err) == contract.CodeConflict && page.Instance != "":
			// A continuation conflicts alike for a changed instance and a
			// changed generation: only the former is a mismatch.
			if cerr := current(); cerr != nil {
				return cerr
			}
			return err
		case err != nil:
			return err
		case r.Instance != s.Instance:
			return mismatch
		}
		for _, ref := range r.Refs {
			if ref.Name == s.Ref {
				if ref.Commit != s.Commit {
					return contract.TaskResultUnavailable(id, "its task ref names another commit than the recorded result")
				}
				return nil
			}
			if ref.Name > s.Ref {
				return taskRefMissing(id, s.Name)
			}
		}
		if r.NextAfter == nil {
			return taskRefMissing(id, s.Name)
		}
		page = StatusPage{After: *r.NextAfter, Instance: r.Instance, Generation: r.Generation, Limit: contract.MaxWorkspaceLimit}
	}
}

func taskRefMissing(id, name string) error {
	return contract.TaskError(contract.CodeNotFound, "task_id", "", "task %s's result ref is no longer in workspace %s (pruned); it is never rebuilt", id, name)
}

// TaskDiffPage selects one task diff page: the first page leaves After,
// Instance and Generation empty; a continuation gives all three from the
// previous page.
type TaskDiffPage struct {
	After, Instance, Generation string
	Limit                       int
}

// TaskWorkspaceDiff returns one page of task id's result metadata against
// its admitted base (or the empty tree): the task record is resolved (and
// its exact ref checked) for every page, a continuation must name the
// bound instance, the existing workspace diff compares the immutable full
// hashes, its answer must name the bound instance (a recreation with the
// same hashes fails), and the exact ref is checked again before the page
// is returned. A changed generation conflicts (restart from the first
// page); nothing restarts or mixes pages.
func (c *Client) TaskWorkspaceDiff(ctx context.Context, id string, p TaskDiffPage) (contract.WorkspaceDiffResponse, error) {
	if !contract.ValidTaskID(id) {
		return contract.WorkspaceDiffResponse{}, invalidTaskID()
	}
	if err := checkLimit(p.Limit); err != nil {
		return contract.WorkspaceDiffResponse{}, err
	}
	if err := checkContinuation(p.After, p.Instance, p.Generation); err != nil {
		return contract.WorkspaceDiffResponse{}, err
	}
	if p.After != "" {
		if _, err := contract.ParsePathCursor(p.After); err != nil {
			return contract.WorkspaceDiffResponse{}, err
		}
	}
	s, err := c.ResolveTaskResult(ctx, id)
	if err != nil {
		return contract.WorkspaceDiffResponse{}, err
	}
	if p.Instance != "" && p.Instance != s.Instance {
		return contract.WorkspaceDiffResponse{}, contract.TaskError(contract.CodeConflict, "instance", contract.ReasonWorkspaceInstanceMismatch,
			"the continuation's instance is not the task's bound instance; restart from the first page")
	}
	base := contract.SelectorEmpty
	if s.BaseCommit != nil {
		base = *s.BaseCommit
	}
	r, err := c.WorkspaceDiff(ctx, s.Name, DiffPage{Base: base, Target: s.Commit, After: p.After, Instance: p.Instance, Generation: p.Generation, Limit: p.Limit})
	if err != nil {
		return contract.WorkspaceDiffResponse{}, c.taskDiffError(ctx, s, err)
	}
	if r.Instance != s.Instance {
		return contract.WorkspaceDiffResponse{}, contract.WorkspaceInstanceMismatch(s.Name)
	}
	if err := c.CheckTaskResultRef(ctx, s); err != nil {
		return contract.WorkspaceDiffResponse{}, err
	}
	return r, nil
}

// taskDiffError is the error a failed task diff reports. Storage,
// transport and every other failure propagate unchanged. Only a not_found
// or conflict answer, which a removal, recreation or prune of the bound
// workspace also produces, is checked against the exact task ref again: a
// removal, recreation or prune observed then (itself a not_found or
// conflict) takes its place; otherwise, a changed generation included, the
// diff's own answer stands.
func (c *Client) taskDiffError(ctx context.Context, s TaskResultSelection, err error) error {
	if code := contract.CodeOf(err); code != contract.CodeNotFound && code != contract.CodeConflict {
		return err
	}
	cerr := c.CheckTaskResultRef(ctx, s)
	if code := contract.CodeOf(cerr); cerr != nil && (code == contract.CodeNotFound || code == contract.CodeConflict) {
		return cerr
	}
	return err
}
