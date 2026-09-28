package plane

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The task API (iteration 05; 06a adds ?late=true on logs): dispatch and task ls/show/logs, typed so a
// future MCP server can mirror each operation one to one. Global task
// reads expose goals, payload pointers, attribution and output to anyone
// with network and trust access, matching v1's security model; manual
// bytes, prompts and output never enter the plane's diagnostic logs.

// taskQuery strictly parses a query string: only the allowed keys, each
// at most once with a nonempty value; no empty or repeated key.
func taskQuery(r *http.Request, allowed ...string) (map[string]string, error) {
	out := map[string]string{}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return nil, invalid("an empty query string is not accepted")
	}
	if r.URL.RawQuery == "" {
		return out, nil
	}
	for _, part := range strings.Split(r.URL.RawQuery, "&") {
		k, v, _ := strings.Cut(part, "=")
		known := false
		for _, a := range allowed {
			known = known || a == k
		}
		switch {
		case k == "":
			return nil, invalid("empty query keys are not accepted")
		case !known && len(allowed) == 0:
			return nil, invalid("this endpoint accepts no query parameters")
		case !known:
			// The request's own key is never echoed.
			return nil, invalid("unknown query parameter; accepted: " + strings.Join(allowed, ", "))
		case v == "":
			return nil, invalid("query parameter " + k + " must have a value")
		}
		if _, dup := out[k]; dup {
			return nil, invalid("query parameter " + k + " may be given only once")
		}
		out[k] = v
	}
	return out, nil
}

// queryInt parses an optional bounded integer query value.
func queryInt(q map[string]string, key string, def, lo, hi int) (int, error) {
	v, ok := q[key]
	if !ok {
		return def, nil
	}
	n, valid := contract.ParseInteger(v)
	if !valid || n < lo || n > hi {
		return 0, invalid(key + " must be an integer from " + strconv.Itoa(lo) + " to " + strconv.Itoa(hi))
	}
	return n, nil
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, invalid("method not allowed; use "+strings.ReplaceAll(allow, ", ", " or ")))
}

// handleTasks serves /api/v1/tasks, /api/v1/tasks/{id} and
// /api/v1/tasks/{id}/logs. The protocol header is checked first, then the
// path, method, query, body and size; IDs use the task grammar before
// any lookup, and encoded path aliases are refused.
func (s *nodeService) handleTasks(w http.ResponseWriter, r *http.Request) {
	if !s.checkVersion(w, r) {
		return
	}
	ts := s.tasks
	if r.URL.RawPath != "" {
		writeError(w, invalid("encoded task paths are not accepted"))
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, contract.PathTasks)
	var id string
	logs := false
	if rest != "" {
		parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
		switch {
		case len(parts) == 1:
			id = parts[0]
		case len(parts) == 2 && parts[1] == "logs":
			id, logs = parts[0], true
		default:
			writeError(w, contract.New(contract.CodeNotFound, "no such endpoint"))
			return
		}
		if !contract.ValidTaskID(id) {
			writeError(w, invalid("invalid task ID; want t_ followed by 32 lowercase hex digits"))
			return
		}
	}
	switch {
	case id == "" && r.Method != http.MethodGet && r.Method != http.MethodPost:
		methodNotAllowed(w, "GET, POST")
		return
	case id != "" && r.Method != http.MethodGet:
		methodNotAllowed(w, "GET")
		return
	}
	if r.Method == http.MethodGet && hasBody(r) {
		writeError(w, invalid("GET requests must not carry a body"))
		return
	}
	switch {
	case r.Method == http.MethodPost:
		if _, err := taskQuery(r); err != nil {
			writeCodeError(w, err)
			return
		}
		s.dispatchTask(w, r, ts)
	case id == "":
		q, err := taskQuery(r, "limit", "after")
		if err != nil {
			writeCodeError(w, err)
			return
		}
		limit, err := queryInt(q, "limit", contract.DefaultTaskListLimit, 1, contract.MaxTaskListLimit)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		after := q["after"]
		if _, ok := q["after"]; ok && !contract.ValidTaskID(after) {
			writeError(w, invalid("after must be a task ID"))
			return
		}
		tasks, next := ts.list(after, limit)
		writeBounded(w, http.StatusOK, contract.TaskListResponse{Version: contract.ProtocolVersion, Tasks: tasks, NextAfter: next}, contract.MaxTaskListBytes)
	case logs:
		// ?late=true selects the late-evidence tail (iteration 06a); the
		// ordinary logs response is unchanged.
		q, err := taskQuery(r, "late")
		if err != nil {
			writeCodeError(w, err)
			return
		}
		late := false
		if v, ok := q["late"]; ok {
			if v != "true" {
				writeError(w, invalid("late must be true"))
				return
			}
			late = true
		}
		resp, err := ts.logs(id, late)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		writeBounded(w, http.StatusOK, resp, contract.MaxTaskLogsBytes)
	default:
		q, err := taskQuery(r, "lines")
		if err != nil {
			writeCodeError(w, err)
			return
		}
		lines, err := queryInt(q, "lines", contract.DefaultTailLines, 0, contract.MaxTailLines)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		v, err := ts.show(id, lines)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		writeBounded(w, http.StatusOK, contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v}, contract.MaxTaskViewBytes)
	}
}

// dispatchTask serves POST /api/v1/tasks: a JSON DispatchRequest of at
// most 256 KiB, admitted or refused at once. 202 means admission, not
// execution.
func (s *nodeService) dispatchTask(w http.ResponseWriter, r *http.Request, ts *taskService) {
	if !jsonContent(r) {
		writeError(w, invalid("a dispatch must have Content-Type application/json"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, contract.MaxDispatchRequestBytes))
	if err != nil {
		writeError(w, invalid("the request body is unreadable or larger than "+strconv.Itoa(contract.MaxDispatchRequestBytes)+" bytes"))
		return
	}
	req, err := contract.ParseDispatchRequest(body)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	v, err := ts.dispatch(r.Context(), req)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	writeBounded(w, http.StatusAccepted, contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: v.TaskID, Task: v}, contract.MaxTaskViewBytes)
}
