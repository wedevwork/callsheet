package plane

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// handleTasks serves /api/v1/tasks, /api/v1/tasks/wait (iteration 06b,
// matched before any task ID), /api/v1/tasks/{id}, /api/v1/tasks/{id}/logs
// and /api/v1/tasks/{id}/cancel (iteration 06b). The protocol header is
// checked first, then the path, method, query, body and size; IDs use the
// task grammar before any lookup, and encoded path aliases are refused.
func (s *nodeService) handleTasks(w http.ResponseWriter, r *http.Request) {
	if !s.checkVersion(w, r) {
		return
	}
	ts := s.tasks
	if r.URL.RawPath != "" {
		writeError(w, invalid("encoded task paths are not accepted"))
		return
	}
	if r.URL.Path == contract.PathTaskWait {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		if _, err := taskQuery(r); err != nil {
			writeCodeError(w, err)
			return
		}
		s.waitTasks(w, r, ts)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, contract.PathTasks)
	var id string
	logs, cancel := false, false
	if rest != "" {
		parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
		if len(parts) >= 2 && "/"+parts[1] == contract.PublicationSuffix {
			// Iteration 10b: the workspace publication endpoints.
			pub, finish, ok := contract.SplitPublicationPath("/" + strings.Join(parts[1:], "/"))
			switch {
			case !strings.HasPrefix(rest, "/") || !contract.ValidTaskID(parts[0]):
				writeError(w, invalid("invalid task ID; want t_ followed by 32 lowercase hex digits"))
			case !ok:
				writeError(w, contract.New(contract.CodeNotFound, "no such endpoint"))
			default:
				s.handlePublication(w, r, parts[0], pub, finish)
			}
			return
		}
		switch {
		case len(parts) == 1:
			id = parts[0]
		case len(parts) == 2 && parts[1] == "logs":
			id, logs = parts[0], true
		case len(parts) == 2 && parts[1] == "cancel":
			id, cancel = parts[0], true
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
	case cancel && r.Method != http.MethodPost:
		methodNotAllowed(w, "POST")
		return
	case cancel:
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
	case cancel:
		if _, err := taskQuery(r); err != nil {
			writeCodeError(w, err)
			return
		}
		s.cancelTask(w, r, ts, id)
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

// readJSON reads a JSON request body of at most limit bytes.
func readJSON(w http.ResponseWriter, r *http.Request, limit int, what string) ([]byte, bool) {
	if !jsonContent(r) {
		writeError(w, invalid(what+" must have Content-Type application/json"))
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(limit)))
	if err != nil {
		writeError(w, invalid("the request body is unreadable or larger than "+strconv.Itoa(limit)+" bytes"))
		return nil, false
	}
	return body, true
}

// extendWrite lets a bounded wait's response be written after the
// ordinary 10 s server write bound: requested wait plus the ordinary
// bound.
func extendWrite(w http.ResponseWriter, wait time.Duration) {
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + writeTimeout))
}

// dispatchTask serves POST /api/v1/tasks: a JSON dispatch request of at
// most 256 KiB with the optional transport wait (iteration 06b), admitted
// or refused at once and never retried. 202 means admission, not
// execution, whether a wait then answers terminal or still running. A
// positive wait reserves its registration before admission, so a full
// wait capacity refuses before anything is admitted.
func (s *nodeService) dispatchTask(w http.ResponseWriter, r *http.Request, ts *taskService) {
	body, ok := readJSON(w, r, contract.MaxDispatchEnvelopeBytes, "a dispatch")
	if !ok {
		return
	}
	req, wait, err := contract.ParseDispatchEnvelope(body)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	var reserved *waitReservation
	if wait != nil && ts.effectiveWait(*wait) > 0 {
		if reserved, err = ts.reserveWait(); err != nil {
			writeCodeError(w, err)
			return
		}
		defer reserved.release()
		extendWrite(w, ts.effectiveWait(*wait))
	}
	v, err := ts.dispatch(r.Context(), req)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	if wait == nil {
		writeBounded(w, http.StatusAccepted, contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: v.TaskID, Task: &v}, contract.MaxTaskViewBytes)
		return
	}
	wr, err := ts.wait(r.Context(), []string{v.TaskID}, *wait, reserved)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	limit := contract.MaxWaitResponseBytes
	if wr.Status == contract.WaitStillRunning {
		limit = contract.MaxStillRunningOneBytes
	}
	writeBounded(w, http.StatusAccepted, contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: v.TaskID, WaitResult: &wr}, limit)
}

// cancelTask serves POST /api/v1/tasks/{id}/cancel: a strict empty JSON
// object; 202 accepted once the stop intent is durable, 200 not accepted
// for an already durably terminal task.
func (s *nodeService) cancelTask(w http.ResponseWriter, r *http.Request, ts *taskService, id string) {
	body, ok := readJSON(w, r, contract.MaxControlBody, "a cancel")
	if !ok {
		return
	}
	if err := contract.ParseCancelRequest(body); err != nil {
		writeCodeError(w, err)
		return
	}
	v, accepted, err := ts.cancel(r.Context(), id)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	status := http.StatusOK
	if accepted {
		status = http.StatusAccepted
	}
	writeBounded(w, status, contract.CancelResponse{Version: contract.ProtocolVersion, TaskID: id, Accepted: accepted, Task: v}, contract.MaxWaitResponseBytes)
}

// waitTasks serves POST /api/v1/tasks/wait: {"task_ids":[...],"wait":D}
// of at most 2 KiB; 200 with the wait union, its compact bound asserted
// after encoding and before any byte is written.
func (s *nodeService) waitTasks(w http.ResponseWriter, r *http.Request, ts *taskService) {
	body, ok := readJSON(w, r, contract.MaxWaitRequestBytes, "a wait")
	if !ok {
		return
	}
	req, err := contract.ParseTaskWaitRequest(body)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	extendWrite(w, ts.effectiveWait(req.Wait))
	wr, err := ts.wait(r.Context(), req.TaskIDs, req.Wait, nil)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	b, err := contract.EncodeWaitResponse(wr)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(append(b, '\n'))
}
