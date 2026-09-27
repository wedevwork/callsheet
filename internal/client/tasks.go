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
