package reale2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/mcp"
)

// The coordinator's Callsheet tool policy against the server itself
// (design 12a-dev-flow-verify r0.6): the registered tools are enumerated
// from a real in-process server's tools/list and each is classified as
// permitted or forbidden, exactly once; success judges exist for the
// permitted tools only. Separately, the server's argument conformance for
// all 23 tools: a valid argument set passes the server's validation and
// the shared contract validator, a server-refused value is
// invalid_argument at both (the server API is unchanged).

// mcpSession is a real in-process Callsheet MCP server whose plane and
// transfer services are unreachable: a call that passes argument
// validation fails afterwards with a non-argument error.
type mcpSession struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Scanner
	done chan int
	id   int
}

func newMCPSession(t *testing.T) *mcpSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &mcpSession{t: t, in: inW, out: bufio.NewScanner(outR), done: make(chan int, 1)}
	s.out.Buffer(nil, 1<<22)
	unreachable := func(context.Context) (mcp.Client, error) {
		return nil, contract.New(contract.CodeUnavailable, "no plane in this test")
	}
	go func() {
		s.done <- mcp.Serve(context.Background(), mcp.Config{In: inR, Out: outW, GOOS: "linux", Factory: unreachable,
			Hostname: func() (string, error) { return "coordinator", nil }, CloseIn: inR.Close, CloseOut: outW.Close})
	}()
	t.Cleanup(func() {
		inW.Close()
		if code := <-s.done; code != 0 {
			t.Errorf("the server ended %d", code)
		}
	})
	s.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "reale2e", "version": "1"}})
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return s
}

func (s *mcpSession) send(v any) {
	b, _ := json.Marshal(v)
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

// request sends one request and returns its result.
func (s *mcpSession) request(method string, params any) json.RawMessage {
	s.t.Helper()
	s.id++
	s.send(map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method, "params": params})
	for s.out.Scan() {
		var m struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(s.out.Bytes(), &m) == nil && m.ID == s.id {
			return m.Result
		}
	}
	s.t.Fatalf("no answer to %s", method)
	return nil
}

// tools returns the server's registered tool names.
func (s *mcpSession) tools() []string {
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(s.request("tools/list", map[string]any{}), &list); err != nil {
		s.t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// refusal calls tool with args and returns the server's error code ("" for
// a successful answer).
func (s *mcpSession) refusal(tool, args string) contract.Code {
	s.t.Helper()
	var r struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(s.request("tools/call", map[string]any{"name": tool, "arguments": json.RawMessage(args)}), &r); err != nil || len(r.Content) != 1 {
		s.t.Fatalf("%s %s: %v %+v", tool, args, err, r)
	}
	if !r.IsError {
		return ""
	}
	e, err := contract.ParseErrorBody([]byte(r.Content[0].Text))
	if err != nil {
		s.t.Fatalf("%s %s: %q %v", tool, args, r.Content[0].Text, err)
	}
	return e.Code
}

func TestDevFullMCPPolicyCoverage(t *testing.T) {
	t.Parallel()
	registered := newMCPSession(t).tools()
	if strings.Join(registered, ",") != strings.Join(mcp.ToolNames, ",") || len(registered) != 23 {
		t.Fatalf("the server registers %v, ToolNames %v", registered, mcp.ToolNames)
	}
	// Every registered tool is classified exactly once; nothing else is.
	classified := map[string]int{}
	for _, n := range append(slices.Clone(mcpPermittedTools), mcpForbiddenTools...) {
		classified[n]++
	}
	for _, n := range registered {
		if classified[n] != 1 {
			t.Errorf("%s is classified %d times", n, classified[n])
		}
		delete(classified, n)
	}
	if len(classified) != 0 || len(mcpPermittedTools) != 9 || len(mcpForbiddenTools) != 14 {
		t.Fatalf("classification outside the registration: %v (%d permitted, %d forbidden)", classified, len(mcpPermittedTools), len(mcpForbiddenTools))
	}
	// Success judges exist for exactly the permitted tools.
	var judged []string
	for n := range mcpTools {
		judged = append(judged, n)
	}
	sort.Strings(judged)
	permitted := slices.Clone(mcpPermittedTools)
	sort.Strings(permitted)
	if !slices.Equal(judged, permitted) {
		t.Fatalf("judges %v, permitted %v", judged, permitted)
	}
	// A forbidden or unknown tool is refused by policy before anything
	// else, whatever its arguments and answer.
	for _, n := range append(slices.Clone(mcpForbiddenTools), "task_peek", "") {
		var pe *mcpPolicyError
		if _, err := judgeMCPCall(&mcpAuthority{}, mcpToolPrefix+n, json.RawMessage(`{}`), `{}`); !errors.As(err, &pe) {
			t.Errorf("%s: %v", n, err)
		}
	}
	for _, n := range []string{"mcp__other__task_show", "task_show", "dispatch"} {
		var pe *mcpPolicyError
		if _, err := judgeMCPCall(&mcpAuthority{}, n, json.RawMessage(`{}`), `{}`); !errors.As(err, &pe) {
			t.Errorf("%s was judged a Callsheet tool: %v", n, err)
		}
	}
}

// mcpConformance is one tool's server argument-conformance table.
type mcpConformance struct {
	valid    []string
	refused  []string
	validate func(args []byte) error
}

func TestDevFullMCPServerConformance(t *testing.T) {
	t.Parallel()
	s := newMCPSession(t)
	inst, gen, h := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 40)
	node, op, task := "n_"+strings.Repeat("1", 32), strings.Repeat("e", 32), "t_"+strings.Repeat("0", 31)+"1"
	role := `{"id":"worker-a","name":"implementer","node":"` + node + `","adapter":"fake","instruction":"/srv/manuals/instruction.md","runbook":"/srv/manuals/runbook.md","model":"example model","effort":"medium","concurrency":2,"timeout":"90m"}`
	dispatch := `{"target":{"kind":"name","value":"designer"},"goal":"g","acceptance":"a"}`
	push := `{"name":"w","instance":"` + inst + `","branch":"main","path":"/srv/seed"}`
	lookup := devAdapterLookup
	// with adds one member to a JSON object.
	with := func(obj, member string) string { return strings.TrimSuffix(obj, "}") + "," + member + "}" }
	table := map[string]mcpConformance{
		"node_ls": {valid: []string{`{}`}, refused: []string{`{"all":true}`, `{"x":null}`}, validate: contract.ParseNoArgs},
		"node_show": {valid: []string{`{"id":"` + node + `"}`}, refused: []string{`{}`, `{"id":"N"}`, `{"id":null}`},
			validate: func(b []byte) error { _, err := contract.ParseNodeShowArgs(b); return err }},
		"role_add": {valid: []string{role}, refused: []string{strings.Replace(role, `"fake"`, `"nope"`, 1), strings.Replace(role, `"concurrency":2`, `"concurrency":0`, 1),
			strings.Replace(role, `"90m"`, `"forever"`, 1), with(role, `"extra":1`)},
			validate: func(b []byte) error { _, err := contract.ParseRoleAddArgs(b, lookup); return err }},
		"role_set": {valid: []string{`{"id":"worker-a","model":"other model"}`}, refused: []string{`{"id":"worker-a"}`, `{"id":"worker-a","adapter":"nope"}`, `{"id":"worker-a","concurrency":0}`},
			validate: func(b []byte) error { _, _, err := contract.ParseRoleSetArgs(b, lookup); return err }},
		"role_ls": {valid: []string{`{}`}, refused: []string{`{"all":true}`}, validate: contract.ParseNoArgs},
		"role_show": {valid: []string{`{"id":"worker-a"}`}, refused: []string{`{"id":""}`, `{"id":"BAD ID"}`},
			validate: func(b []byte) error { _, err := contract.ParseRoleShowArgs(b); return err }},
		"role_rm": {valid: []string{`{"id":"worker-a"}`, `{"id":"worker-a","force":true,"operation":"` + op + `"}`},
			refused:  []string{`{"id":"worker-a","operation":"` + op + `"}`, `{"id":"worker-a","force":true,"operation":"x"}`},
			validate: func(b []byte) error { _, _, _, err := contract.ParseRoleRemoveArgs(b); return err }},
		"dispatch": {valid: []string{dispatch, with(dispatch, `"wait":"30s"`)},
			refused: []string{strings.Replace(dispatch, `"name"`, `"bogus"`, 1), with(dispatch, `"payload":null`),
				with(dispatch, `"wait":"soon"`), with(dispatch, `"override":{"timeout":"forever"}`)},
			validate: func(b []byte) error { return validateDispatchShared(b) }},
		"task_ls": {valid: []string{`{}`, `{"limit":2}`}, refused: []string{`{"limit":0}`, `{"limit":101}`, `{"after":"t_x"}`, `{"cursor":"x"}`},
			validate: func(b []byte) error { _, _, err := contract.ParseTaskListArgs(b); return err }},
		"task_show": {valid: []string{`{"id":"` + task + `"}`, `{"id":"` + task + `","lines":0}`}, refused: []string{`{"id":"T"}`, `{"id":"` + task + `","lines":-1}`, `{"id":"` + task + `","lines":201}`},
			validate: func(b []byte) error { _, _, err := contract.ParseTaskShowArgs(b); return err }},
		"task_logs": {valid: []string{`{"id":"` + task + `"}`, `{"id":"` + task + `","late":true}`}, refused: []string{`{"id":"T"}`, `{"id":"` + task + `","lines":5}`},
			validate: func(b []byte) error { _, _, err := contract.ParseTaskLogsArgs(b); return err }},
		"task_cancel": {valid: []string{`{"id":"` + task + `"}`}, refused: []string{`{"id":"T"}`, `{}`},
			validate: func(b []byte) error { _, err := contract.ParseTaskCancelArgs(b); return err }},
		"task_wait": {valid: []string{`{"task_ids":["` + task + `"]}`, `{"task_ids":["` + task + `"],"wait":"10s"}`},
			refused:  []string{`{"task_ids":[]}`, `{"task_ids":["` + task + `"],"wait":"6m"}`, `{"task_ids":["` + task + `","` + task + `"]}`},
			validate: func(b []byte) error { _, _, err := contract.ParseTaskWaitArgs(b); return err }},
		"ws_create": {valid: []string{`{"name":"w"}`}, refused: []string{`{"name":"W"}`, `{"name":""}`},
			validate: func(b []byte) error { _, err := contract.ParseWsNameArgs(b); return err }},
		"ws_show": {valid: []string{`{"name":"w"}`}, refused: []string{`{"name":"W"}`, `{}`},
			validate: func(b []byte) error { _, err := contract.ParseWsNameArgs(b); return err }},
		"ws_ls": {valid: []string{`{}`, `{"limit":2}`}, refused: []string{`{"limit":0}`, `{"limit":101}`, `{"after":""}`},
			validate: func(b []byte) error { _, _, err := contract.ParseWsListArgs(b); return err }},
		"ws_rm": {valid: []string{`{"name":"w","instance":"` + inst + `"}`}, refused: []string{`{"name":"w","instance":"x"}`, `{"name":"w"}`},
			validate: func(b []byte) error { _, _, err := contract.ParseWsRemoveArgs(b); return err }},
		"ws_prune": {valid: []string{`{"name":"w","instance":"` + inst + `","before":"2026-09-30T12:00:00Z"}`},
			refused:  []string{`{"name":"w","instance":"` + inst + `","before":"yesterday"}`, `{"name":"w","instance":"` + inst + `"}`},
			validate: func(b []byte) error { _, _, _, err := contract.ParseWsPruneArgs(b); return err }},
		"ws_ref_set": {valid: []string{`{"name":"w","instance":"` + inst + `","branch":"feature","expected":"absent","target":"` + h + `"}`},
			refused:  []string{`{"name":"w","instance":"` + inst + `","branch":"feature","expected":"absent","delete":true}`, `{"name":"w","instance":"` + inst + `","branch":"x.lock","expected":"absent","target":"main"}`},
			validate: func(b []byte) error { _, _, err := contract.ParseWsRefSetArgs(b); return err }},
		"ws_status": {valid: []string{`{"task_id":"` + task + `"}`, `{"name":"w"}`, `{"name":"w","after":"refs/heads/main","instance":"` + inst + `","generation":"` + gen + `"}`},
			refused:  []string{`{"task_id":"` + task + `","name":"w"}`, `{"task_id":"` + task + `","limit":5}`, `{"name":"w","after":"refs/heads/main"}`, `{"name":"w","limit":0}`},
			validate: func(b []byte) error { _, err := contract.ParseWsStatusArgs(b); return err }},
		"ws_diff": {valid: []string{`{"task_id":"` + task + `","limit":1}`, `{"name":"w","base":"empty","target":"` + h + `"}`},
			refused:  []string{`{"name":"w"}`, `{"task_id":"` + task + `","base":"main"}`, `{"name":"w","base":"empty","target":"` + h + `","after":"!!","instance":"` + inst + `","generation":"` + gen + `"}`},
			validate: func(b []byte) error { _, err := contract.ParseWsDiffArgs(b); return err }},
		"ws_push": {valid: []string{push, `{"name":"w","instance":"` + inst + `"}`}, refused: []string{`{"name":"w"}`, strings.Replace(push, `"main"`, `"Main"`, 1), strings.Replace(push, `"/srv/seed"`, `""`, 1)},
			validate: func(b []byte) error { _, err := contract.ParseWsPushArgs(b, "linux"); return err }},
		"ws_pull": {valid: []string{`{"task_id":"` + task + `"}`, `{"name":"w","ref":"main","path":"out"}`},
			refused:  []string{`{"task_id":"` + task + `","name":"w"}`, `{"path":"out"}`, `{"name":"w","ref":"HEAD~1"}`},
			validate: func(b []byte) error { _, err := contract.ParseWsPullArgs(b, "linux"); return err }},
	}
	for _, tool := range mcp.ToolNames {
		c, ok := table[tool]
		if !ok || len(c.valid) == 0 || len(c.refused) == 0 {
			t.Errorf("%s has no conformance table", tool)
			continue
		}
		for _, args := range c.valid {
			if code := s.refusal(tool, args); code == contract.CodeInvalidArgument {
				t.Errorf("%s %s: the server refuses valid arguments", tool, args)
			}
			if err := c.validate([]byte(args)); err != nil {
				t.Errorf("%s %s: the shared validator refuses valid arguments: %v", tool, args, err)
			}
		}
		for _, args := range c.refused {
			if code := s.refusal(tool, args); code != contract.CodeInvalidArgument {
				t.Errorf("%s %s: the server answers %q, not invalid_argument", tool, args, code)
			}
			if err := c.validate([]byte(args)); contract.CodeOf(err) != contract.CodeInvalidArgument {
				t.Errorf("%s %s: the shared validator answers %v", tool, args, err)
			}
		}
	}
}

// validateDispatchShared is dispatch's shared server validation alone
// (decode, request with an attribution, wait), without the exercise's
// asynchronous-only and prepared-document policy.
func validateDispatchShared(b []byte) error {
	d, err := contract.DecodeDispatchArgs(b)
	if err != nil {
		return err
	}
	if _, err := d.Request(placeholderRequester); err != nil {
		return err
	}
	_, err = d.WaitDuration()
	return err
}

// forbiddenPairs are otherwise valid request/answer pairs of the fourteen
// forbidden tools: each argument set passes the shared validator and each
// answer the tool's strict contract parser.
func forbiddenPairs(t *testing.T) map[string][2]string {
	t.Helper()
	pv := contract.ProtocolVersion
	inst, gen, h := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 40)
	node, op, task := "n_"+strings.Repeat("1", 32), strings.Repeat("e", 32), "t_"+strings.Repeat("0", 31)+"1"
	role := `{"id":"worker-a","name":"implementer","node":"` + node + `","adapter":"fake","instruction":"/srv/manuals/instruction.md","runbook":"/srv/manuals/runbook.md","model":"example model","effort":"medium","concurrency":2,"timeout":"90m"}`
	rc, err := contract.ParseRoleAddArgs([]byte(role), devAdapterLookup)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := devAdapterLookup(rc.Adapter)
	view := contract.RoleView{RoleRecord: contract.RoleRecord{RoleConfig: rc.Resolved(), RegistrationOrder: 1}, NodeLiveness: contract.LivenessOffline,
		AdapterTestOnly: spec.TestOnly}
	roleAnswer := encodeAnswer(t, contract.RoleResponse{Version: pv, Role: view})
	wsView := `{"name":"w","instance":"` + inst + `","created_at":"2026-09-30T12:00:00Z","generation":"` + gen + `","default_branch":"refs/heads/main","size_bytes":10,"retention":{"automatic_prune":false,"quota_bytes":null}}`
	running := contract.WaitRow{TaskID: task, State: contract.TaskRunning}
	return map[string][2]string{
		"task_wait": {`{"task_ids":["` + task + `"],"wait":"5s"}`, encodeAnswer(t, contract.WaitResponse{Version: pv, Status: contract.WaitStillRunning,
			EffectiveWaitMS: 5000, Tasks: []contract.WaitRow{running}})},
		"node_ls":    {`{}`, encodeAnswer(t, contract.NodeListResponse{Version: pv})},
		"node_show":  {`{"id":"` + node + `"}`, encodeAnswer(t, contract.NodeResponse{Version: pv, Node: contract.Node{ID: node, Liveness: contract.LivenessOffline}})},
		"role_add":   {role, roleAnswer},
		"role_set":   {`{"id":"worker-a","model":"example model"}`, roleAnswer},
		"role_ls":    {`{}`, encodeAnswer(t, contract.RoleListResponse{Version: pv, Roles: []contract.RoleView{view}})},
		"role_show":  {`{"id":"worker-a"}`, roleAnswer},
		"role_rm":    {`{"id":"worker-a","force":true,"operation":"` + op + `"}`, encodeAnswer(t, contract.RoleRemovePendingResponse{Version: pv, OperationID: op, RoleID: "worker-a", RegistrationOrder: 1, Removing: true})},
		"ws_create":  {`{"name":"w"}`, wsView},
		"ws_show":    {`{"name":"w"}`, wsView},
		"ws_ls":      {`{}`, `{"workspaces":[{"name":"w","instance":"` + inst + `","created_at":"2026-09-30T12:00:00Z"}],"next_after":null}`},
		"ws_rm":      {`{"name":"w","instance":"` + inst + `"}`, `{"name":"w","instance":"` + inst + `","removed":true}`},
		"ws_prune":   {`{"name":"w","instance":"` + inst + `","before":"2026-09-30T12:00:00Z"}`, `{"name":"w","instance":"` + inst + `","generation":"` + gen + `","removed_task_refs":0,"reclaimed_bytes":0,"size_bytes":0}`},
		"ws_ref_set": {`{"name":"w","instance":"` + inst + `","branch":"feature","expected":"absent","target":"` + h + `"}`, `{"name":"w","instance":"` + inst + `","generation":"` + gen + `","ref":"refs/heads/feature","old_commit":null,"new_commit":"` + h + `"}`},
	}
}

// forbiddenAnswerValid strictly parses a forbidden tool's answer.
func forbiddenAnswerValid(tool, args, answer string) error {
	b := []byte(answer)
	switch tool {
	case "task_wait":
		ids, wait, err := contract.ParseTaskWaitArgs([]byte(args))
		if err != nil {
			return err
		}
		_, err = contract.ParseWaitResponse(b, ids, *wait)
		return err
	case "node_ls":
		_, err := contract.ParseNodeListResponse(b)
		return err
	case "node_show":
		_, err := contract.ParseNodeResponse(b)
		return err
	case "role_add", "role_set", "role_show":
		_, err := contract.ParseRoleResponse(b, devAdapterLookup)
		return err
	case "role_ls":
		_, err := contract.ParseRoleListResponse(b, devAdapterLookup)
		return err
	case "role_rm":
		_, err := contract.ParseRoleRemovePendingResponse(b)
		return err
	case "ws_create", "ws_show":
		_, err := contract.ParseWorkspaceView(b)
		return err
	case "ws_ls":
		_, err := contract.ParseWorkspaceList(b, "", contract.DefaultWorkspaceLimit)
		return err
	case "ws_rm":
		_, err := contract.ParseWorkspaceRemove(b)
		return err
	case "ws_prune":
		_, err := contract.ParseWorkspacePrune(b)
		return err
	case "ws_ref_set":
		_, err := contract.ParseWorkspaceRefSet(b)
		return err
	}
	return errors.New("unknown forbidden tool " + tool)
}

func TestDevFullMCPForbiddenPairs(t *testing.T) {
	t.Parallel()
	pairs := forbiddenPairs(t)
	if len(pairs) != len(mcpForbiddenTools) {
		t.Fatalf("%d pairs for %d forbidden tools", len(pairs), len(mcpForbiddenTools))
	}
	auth := &mcpAuthority{goos: "linux", views: map[string]contract.TaskView{}}
	stopped := &mcpAuthority{goos: "linux", views: map[string]contract.TaskView{}, stopped: true, lastPhase: 1}
	for _, n := range mcpForbiddenTools {
		p, ok := pairs[n]
		if !ok {
			t.Errorf("%s has no valid pair", n)
			continue
		}
		if err := forbiddenAnswerValid(n, p[0], p[1]); err != nil {
			t.Errorf("%s: the pair is not otherwise valid: %v", n, err)
		}
		tool := mcpToolPrefix + n
		if _, err := judgeMCPCall(auth, tool, json.RawMessage(p[0]), p[1]); !isPolicy(err) {
			t.Errorf("%s: a valid pair was judged %v", n, err)
		}
		// In a stream: answered, error-answered or unanswered, every
		// forbidden call fails the policy; task_wait is the stream
		// qualifier's coordinator_long_poll instead.
		for _, recs := range [][]sessionRecord{
			{policyRec("assistant", 1, use("u1", tool, p[0])), policyRec("user", 2, result("u1", p[1], false))},
			{policyRec("assistant", 1, use("u1", tool, p[0])), policyRec("user", 2, result("u1", errorBody(contract.CodeNotFound), true))},
			{policyRec("assistant", 1, use("u1", tool, p[0]))},
		} {
			got, _ := mcpAnalyze(map[int][]sessionRecord{1: recs}, stopped)
			want := devMCPCallInvalid
			if n == "task_wait" {
				want = ""
			}
			if got[1] != want {
				t.Errorf("%s in a stream: %q, want %q", n, got[1], want)
			}
		}
		if n == "task_wait" {
			s := decodeDevStream([]byte(`{"type":"assistant","session_id":"s","message":{"content":[{"type":"tool_use","id":"p","name":"`+tool+`","input":`+p[0]+`}]}}`+"\n"), "")
			if s.problem != devLongPoll {
				t.Errorf("task_wait in a stream: %q", s.problem)
			}
		}
	}
}

// devAdapterLookup is the registered adapters' contract lookup, the one
// the MCP server's role tools validate against (a static registry:
// looking it up starts nothing).
var devAdapterLookup = adapter.Lookup()
