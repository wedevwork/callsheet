//go:build linux || darwin

package function

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/cli"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/mcp"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Iteration 07a function tests: one top-level TestMCP* per FP, in FP
// order, each with its mandatory direct subtests (devcheck's
// nativeRequired). They drive the real "callsheet mcp" binary over
// subprocess pipes with a Go MCP client against a real in-process TLS
// plane, a scripted worker and, where a response must be observed or
// held, a TLS proxy in front of the plane.

func nodeIDN(c byte) string { return "n_" + strings.Repeat(string(c), 32) }

// FP-1: MCP transport and lifecycle.
func TestMCPProtocol(t *testing.T) {
	p := startMCPPlane(t, 0)
	x := startPlaneProxy(t, p)
	t.Run("initialize", func(t *testing.T) {
		m := startMCPProc(t, p.trust()...)
		m.dispatch()
		if r := m.request("ping", nil); r.Error != nil || string(r.Result) != "{}" {
			t.Fatalf("ping before initialize %s", r.raw)
		}
		if r := m.request("tools/list", nil); r.Error == nil || r.Error.Code != -32600 {
			t.Fatalf("tools/list before initialize %s", r.raw)
		}
		r := m.request("initialize", map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"roots": map[string]any{}},
			"clientInfo": map[string]any{"name": "coordinator-test", "title": "Coordinator", "version": "7.0.1"}, "_meta": map[string]any{}})
		want := `{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":false}},"serverInfo":{"name":"callsheet","version":"` + cli.Version + `"}}`
		if r.Error != nil {
			t.Fatalf("initialize %s", r.raw)
		}
		sameJSON(t, string(r.Result), want)
		if r := m.request("tools/list", nil); r.Error == nil || r.Error.Code != -32600 {
			t.Fatalf("tools/list before initialized %s", r.raw)
		}
		m.notify("notifications/initialized", nil)
		if r := m.request("tools/list", nil); r.Error != nil {
			t.Fatalf("tools/list %s", r.raw)
		}
		m.stdin.Close()
		m.requireExit(0)
		m.protocolOnly()
	})
	t.Run("version", func(t *testing.T) {
		m := startMCPProc(t, p.trust()...)
		m.dispatch()
		r := m.request("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "old", "version": "1"}})
		if r.Error != nil || !strings.Contains(string(r.Result), `"protocolVersion":"2025-06-18"`) {
			t.Fatalf("incompatible version %s", r.raw)
		}
		if r := m.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "a", "version": "b"}}); r.Error == nil || r.Error.Code != -32600 {
			t.Fatalf("second initialize %s", r.raw)
		}
	})
	t.Run("discovery", func(t *testing.T) {
		m := startMCP(t, p.trust()...)
		r := m.request("tools/list", map[string]any{})
		var list struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := json.Unmarshal(r.Result, &list); err != nil || list.NextCursor != nil {
			t.Fatalf("tools/list %.300s %v", r.raw, err)
		}
		var names []string
		for _, tl := range list.Tools {
			names = append(names, tl.Name)
			if !strings.Contains(string(tl.InputSchema), `"additionalProperties":false`) || strings.Contains(string(tl.InputSchema), "$ref") {
				t.Fatalf("%s schema %s", tl.Name, tl.InputSchema)
			}
			waiting := tl.Name == "task_wait" || tl.Name == "dispatch"
			if waiting != strings.Contains(tl.Description, "B=10s") || waiting != strings.Contains(tl.Description, mcp.BudgetDeferralNote) ||
				strings.Contains(tl.Description, "ends within it") {
				t.Fatalf("%s description %q", tl.Name, tl.Description)
			}
		}
		if strings.Join(names, " ") != "node_ls node_show role_add role_set role_ls role_show role_rm dispatch task_ls task_show task_logs task_cancel task_wait" {
			t.Fatalf("tools %v", names)
		}
	})
	t.Run("framing", func(t *testing.T) {
		m := startMCP(t, p.trust()...)
		io.WriteString(m.stdin, "not json\n")
		io.WriteString(m.stdin, `[{"jsonrpc":"2.0","id":99,"method":"ping"}]`+"\n")
		io.WriteString(m.stdin, `{"jsonrpc":"2.0","id":98,"method":"ping","id":97}`+"\n")
		if r := m.request("ping", nil); r.Error != nil {
			t.Fatalf("ping after malformed lines %s", r.raw)
		}
		if r := m.request("shutdown", nil); r.Error == nil || r.Error.Code != -32601 {
			t.Fatalf("shutdown %s", r.raw)
		}
		m.stdin.Close()
		m.requireExit(0)
		m.drained()
		m.mu.Lock()
		got := strings.Join(m.unexpected, "")
		m.mu.Unlock()
		// The three malformed lines were answered with a null ID.
		if strings.Count(got, `"id":null`) != 3 || strings.Count(got, `"code":-32700`) != 2 || strings.Count(got, `"code":-32600`) != 1 {
			t.Fatalf("malformed answers %q", got)
		}
		// CRLF framing, then an oversized line and an unterminated line end
		// the session with exit 2 and a diagnostic.
		m2 := startMCPProc(t, p.trust()...)
		br := bufio.NewReader(m2.stdout)
		io.WriteString(m2.stdin, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\r\n")
		if line, err := br.ReadString('\n'); err != nil || line != `{"jsonrpc":"2.0","id":1,"result":{}}`+"\n" {
			t.Fatalf("CRLF %q %v", line, err)
		}
		go m2.stdin.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"ping","x":"` + strings.Repeat("a", mcp.MaxLineBytes) + "\"}\n"))
		m2.requireExit(2)
		if !strings.Contains(m2.stderr.String(), "exceeds 512 KiB") {
			t.Fatalf("stderr %q", m2.stderr.String())
		}
		m3 := startMCPProc(t, p.trust()...)
		io.WriteString(m3.stdin, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		m3.stdin.Close()
		m3.requireExit(2)
	})
	t.Run("concurrency", func(t *testing.T) {
		m := startMCP(t, x.trust()...)
		x.events.clear()
		release := x.hold("GET /api/v1/nodes")
		defer release()
		_, a := m.callAsync("node_ls", nil)
		_, b := m.callAsync("node_ls", nil)
		x.await(t, "response GET /api/v1/nodes")
		x.await(t, "response GET /api/v1/nodes")
		third := m.call("node_ls", nil)
		if e, err := contract.ParseErrorBody([]byte(third.text)); !third.isError || err != nil || e.Details["reason"] != mcp.ReasonCapacity {
			t.Fatalf("third call %s", third.raw)
		}
		if r := m.request("ping", nil); r.Error != nil {
			t.Fatal("ping blocked by active calls")
		}
		if n := x.count("GET /api/v1/nodes"); n != 2 {
			t.Fatalf("the refused call reached the plane: %d requests", n)
		}
		release()
		for _, c := range []<-chan mcpResponse{a, b} {
			if r := decodeTool(t, m.wait(c)); r.isError || !strings.HasPrefix(r.text, `{"version":5,"nodes":[`) {
				t.Fatalf("held call %s", r.raw)
			}
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		m := startMCP(t, x.trust()...)
		x.events.clear()
		release := x.hold("GET /api/v1/roles")
		defer release()
		id, c := m.callAsync("role_ls", nil)
		x.await(t, "response GET /api/v1/roles")
		m.notify("notifications/cancelled", map[string]any{"requestId": json.Number(id), "reason": "user"})
		x.await(t, "canceled GET /api/v1/roles")
		release()
		if r := m.request("ping", nil); r.Error != nil {
			t.Fatal("session unusable after cancellation")
		}
		if r := m.call("node_ls", nil); r.isError {
			t.Fatalf("call after cancellation %s", r.raw)
		}
		m.stdin.Close()
		m.requireExit(0)
		m.protocolOnly()
		select {
		case r := <-c:
			t.Fatalf("the cancelled call was answered: %s", r.raw)
		default:
		}
	})
}

// FP-2: stateless verified relay and one error mapping.
func TestMCPRelay(t *testing.T) {
	p := startMCPPlane(t, 0)
	if _, err := p.cl.EnrollNode(context.Background(), nodeIDN('1'), "relay-1"); err != nil {
		t.Fatal(err)
	}
	t.Run("ca", func(t *testing.T) {
		m := startMCP(t, p.trust()...)
		if text := m.ok("node_ls", nil); !strings.Contains(text, nodeIDN('1')) {
			t.Fatalf("node_ls %s", text)
		}
	})
	t.Run("pin", func(t *testing.T) {
		m := startMCP(t, "--plane", p.url, "--ca-fingerprint", p.fp)
		if text := m.ok("node_show", map[string]any{"id": nodeIDN('1')}); !strings.Contains(text, `"id":"`+nodeIDN('1')+`"`) {
			t.Fatalf("node_show %s", text)
		}
	})
	t.Run("trust-errors", func(t *testing.T) {
		other, err := testkit.NewFixtureCA()
		if err != nil {
			t.Fatal(err)
		}
		wrongCA := filepath.Join(t.TempDir(), "other-ca.crt")
		os.WriteFile(wrongCA, other.CertPEM, 0o644)
		port := strings.TrimPrefix(p.url, "https://127.0.0.1:")
		for _, args := range [][]string{
			{"--plane", p.url, "--ca", wrongCA},
			{"--plane", "https://localhost:" + port, "--ca", p.ca},
			{"--plane", p.url, "--ca-fingerprint", "sha256:" + strings.Repeat("0", 64)},
			{"--plane", p.url, "--ca", filepath.Join(t.TempDir(), "missing.crt")},
		} {
			m := startMCP(t, args...)
			e := m.fails("node_ls", nil, contract.CodeTrustFailed)
			if !strings.Contains(e.Message, "connection not trusted") {
				t.Fatalf("%v: %+v", args, e)
			}
			if r := m.request("ping", nil); r.Error != nil {
				t.Fatal("session ended after a trust failure")
			}
		}
	})
	t.Run("protocol-mismatch", func(t *testing.T) {
		x := startPlaneProxy(t, p)
		x.mu.Lock()
		x.version = "4"
		x.mu.Unlock()
		m := startMCP(t, x.trust()...)
		e := m.fails("node_ls", nil, contract.CodeProtocolMismatch)
		if v, ok := e.DetailInt("remote_version"); !ok || v != 4 {
			t.Fatalf("mismatch %+v", e)
		}
	})
	t.Run("contract-errors", func(t *testing.T) {
		// Every contract code maps one to one to a tool error.
		x := startPlaneProxy(t, p)
		m := startMCP(t, x.trust()...)
		m.fails("node_show", map[string]any{"id": "n_bad"}, contract.CodeInvalidArgument)
		m.fails("node_show", map[string]any{"id": nodeIDN('9')}, contract.CodeNotFound)
		for _, code := range []contract.Code{contract.CodeConflict, contract.CodeUnavailable, contract.CodeNotImplemented, contract.CodeInternal} {
			x.mu.Lock()
			x.inject["GET /api/v1/roles"] = &contract.Error{Code: code, Message: "injected " + string(code), Details: map[string]any{"reason": "probe"}}
			x.mu.Unlock()
			e := m.fails("role_ls", nil, code)
			if e.Message != "injected "+string(code) || e.Details["reason"] != "probe" {
				t.Fatalf("%s %+v", code, e)
			}
		}
		x.mu.Lock()
		delete(x.inject, "GET /api/v1/roles")
		x.version = "4"
		x.mu.Unlock()
		m.fails("role_ls", nil, contract.CodeProtocolMismatch)
		m2 := startMCP(t, "--plane", p.url, "--ca", x.ca)
		m2.fails("role_ls", nil, contract.CodeTrustFailed)
		if strings.Contains(m.stderr.String()+m2.stderr.String(), "panic") {
			t.Fatal("server panicked")
		}
	})
	m := startMCP(t, p.trust()...) // one session across the outage and the restart
	t.Run("no-cache", func(t *testing.T) {
		first := m.ok("node_ls", nil)
		if !strings.Contains(first, nodeIDN('1')) {
			t.Fatalf("node_ls %s", first)
		}
		p.stop(t)
		e := m.fails("node_ls", nil, contract.CodeUnavailable)
		if strings.Contains(e.Message, nodeIDN('1')) {
			t.Fatalf("outage served data: %+v", e)
		}
		m.fails("node_show", map[string]any{"id": nodeIDN('1')}, contract.CodeUnavailable)
	})
	t.Run("recovery", func(t *testing.T) {
		p.start(t)
		if _, err := p.cl.EnrollNode(context.Background(), nodeIDN('2'), "relay-2"); err != nil {
			t.Fatal(err)
		}
		text := m.ok("node_ls", nil)
		if !strings.Contains(text, nodeIDN('1')) || !strings.Contains(text, nodeIDN('2')) {
			t.Fatalf("after restart %s", text)
		}
	})
}

// FP-3: node tools with the CLI's global view.
func TestMCPNodes(t *testing.T) {
	p := startMCPPlane(t, 0)
	for _, c := range []byte{'3', '4'} {
		if _, err := p.cl.EnrollNode(context.Background(), nodeIDN(c), "nodes-"+string(c)); err != nil {
			t.Fatal(err)
		}
	}
	cliA := newNodeCLI(t)
	m := startMCP(t, p.trust()...)
	t.Run("node-ls", func(t *testing.T) {
		r := cliA.run(t, append([]string{"node", "ls", "--json"}, p.trust()...)...)
		if r.code != 0 {
			t.Fatalf("cli %+v", r)
		}
		sameJSON(t, m.ok("node_ls", map[string]any{}), r.stdout)
	})
	t.Run("node-show", func(t *testing.T) {
		r := cliA.run(t, append([]string{"node", "show", nodeIDN('3'), "--json"}, p.trust()...)...)
		sameJSON(t, m.ok("node_show", map[string]any{"id": nodeIDN('3')}), r.stdout)
		m.fails("node_show", map[string]any{"id": nodeIDN('f')}, contract.CodeNotFound)
		m.fails("node_show", map[string]any{"id": "N_1"}, contract.CodeInvalidArgument)
		m.fails("node_show", map[string]any{}, contract.CodeInvalidArgument)
	})
	t.Run("shared-roster", func(t *testing.T) {
		other := startMCP(t, "--plane", p.url, "--ca-fingerprint", p.fp)
		a, b := m.ok("node_ls", nil), other.ok("node_ls", nil)
		sameJSON(t, a, b)
		if !strings.Contains(a, nodeIDN('3')) || !strings.Contains(a, nodeIDN('4')) || strings.Contains(a, "localhost") {
			t.Fatalf("roster %s", a)
		}
	})
}

// roleArgs is a complete role_add argument object for node.
func roleArgs(id, name, node string, conc int) map[string]any {
	return map[string]any{"id": id, "name": name, "node": node, "adapter": "fake", "instruction": "/srv/i.md", "runbook": "/srv/r.md",
		"model": "example model", "effort": "medium", "concurrency": conc}
}

// FP-4: role tools, the global slots and forced removal.
func TestMCPRoles(t *testing.T) {
	p := startMCPPlane(t, 0)
	w := startMCPWorker(t, p, nodeIDN('5'))
	cliA := newNodeCLI(t)
	m := startMCP(t, p.trust()...)
	m2 := startMCP(t, p.trust()...) // a second coordinator session
	t.Run("role-add", func(t *testing.T) {
		text := m.ok("role_add", roleArgs("worker-b", "impl", w.id, 1))
		if v := roleOf(t, text); v.ID != "worker-b" || v.Timeout != contract.DefaultRoleTimeout || v.Node != w.id {
			t.Fatalf("role_add %s", text)
		}
		m.fails("role_add", roleArgs("worker-b", "impl", w.id, 1), contract.CodeConflict)
		m.fails("role_add", roleArgs("worker-z", "impl", w.id, 0), contract.CodeInvalidArgument)
		a := roleArgs("worker-a", "impl", w.id, 2)
		a["timeout"] = "90m"
		if text := m.ok("role_add", a); !strings.Contains(text, `"timeout":"1h30m0s"`) {
			t.Fatalf("timeout %s", text)
		}
	})
	t.Run("role-set", func(t *testing.T) {
		text := m.ok("role_set", map[string]any{"id": "worker-b", "model": "other model", "timeout": "0"})
		if !strings.Contains(text, `"model":"other model"`) || !strings.Contains(text, `"timeout":"0s"`) || !strings.Contains(text, `"concurrency":1`) {
			t.Fatalf("role_set %s", text)
		}
		m.fails("role_set", map[string]any{"id": "worker-b"}, contract.CodeInvalidArgument)
		m.fails("role_set", map[string]any{"id": "worker-b", "node": nodeIDN('6')}, contract.CodeInvalidArgument)
		m.fails("role_set", map[string]any{"id": "nobody", "model": "m"}, contract.CodeNotFound)
	})
	t.Run("role-ls", func(t *testing.T) {
		r := cliA.run(t, append([]string{"role", "ls", "--json"}, p.trust()...)...)
		text := m.ok("role_ls", nil)
		sameJSON(t, text, r.stdout)
		if strings.Index(text, `"id":"worker-b"`) > strings.Index(text, `"id":"worker-a"`) {
			t.Fatalf("role_ls order (name, then registration order) %s", text)
		}
	})
	t.Run("role-show", func(t *testing.T) {
		r := cliA.run(t, append([]string{"role", "show", "worker-a", "--json"}, p.trust()...)...)
		sameJSON(t, m.ok("role_show", map[string]any{"id": "worker-a"}), r.stdout)
		m.fails("role_show", map[string]any{"id": "nobody"}, contract.CodeNotFound)
	})
	t.Run("role-rm", func(t *testing.T) {
		if text := m.ok("role_rm", map[string]any{"id": "worker-b"}); text != `{"version":5,"removed":"worker-b"}` {
			t.Fatalf("role_rm %s", text)
		}
		m.fails("role_show", map[string]any{"id": "worker-b"}, contract.CodeNotFound)
		m.fails("role_rm", map[string]any{"id": "worker-b", "operation": strings.Repeat("a", 32)}, contract.CodeInvalidArgument)
	})
	t.Run("global-slots", func(t *testing.T) {
		// AC-SH-1: the CLI on "machine A" registers a role; this MCP
		// process ("machine B") sees the same slots and their use.
		r := cliA.run(t, append([]string{"role", "add", "slots", "--name", "slots", "--node", w.id, "--adapter", "fake", "--instruction", "/srv/i.md",
			"--runbook", "/srv/r.md", "--model", "m", "--effort", "low", "--concurrency", "2"}, p.trust()...)...)
		if r.code != 0 {
			t.Fatalf("cli role add %+v", r)
		}
		poll(t, "slots ready", func() bool {
			return strings.Contains(m.ok("role_show", map[string]any{"id": "slots"}), `"can_accept":true`)
		})
		r = cliA.run(t, append([]string{"dispatch", "--role-id", "slots", "--goal", "hold slot", "--acceptance", "a", "--json"}, p.trust()...)...)
		if r.code != 0 {
			t.Fatalf("cli dispatch %+v", r)
		}
		id := taskIDOf(t, r.stdout)
		if v := roleOf(t, m.ok("role_show", map[string]any{"id": "slots"})); v.Concurrency != 2 || v.Inflight != 1 {
			t.Fatalf("slots seen from B: %+v", v)
		}
		w.release(id)
		p.awaitTask(t, id, func(v contract.TaskView) bool { return v.State == contract.TaskSucceeded })
	})
	var op string
	var rejoin <-chan mcpResponse
	t.Run("force-pending", func(t *testing.T) {
		p.addRole(t, "doomed", "doomed", w.id, 1)
		poll(t, "doomed ready", func() bool {
			return strings.Contains(m.ok("role_show", map[string]any{"id": "doomed"}), `"can_accept":true`)
		})
		id := taskIDOf(t, m.ok("dispatch", dispatchArgs("id", "doomed", "stubborn task", nil)))
		w.await(t, "started "+id)
		e := m.fails("role_rm", map[string]any{"id": "doomed"}, contract.CodeConflict)
		if e.Details["reason"] != contract.ReasonTasksInflight {
			t.Fatalf("non-force rm %+v", e)
		}
		_, first := m.callAsync("role_rm", map[string]any{"id": "doomed", "force": true})
		poll(t, "removal fence", func() bool {
			if v := roleOf(t, m2.ok("role_show", map[string]any{"id": "doomed"})); v.Removal != nil {
				op = v.Removal.OperationID
			}
			return op != ""
		})
		_, rejoin = m2.callAsync("role_rm", map[string]any{"id": "doomed", "force": true, "operation": op})
		r := decodeTool(t, m.wait(first))
		pend, err := contract.ParseRoleRemovePendingResponse([]byte(r.text))
		if r.isError || err != nil || pend.OperationID != op || !pend.Removing || pend.RoleID != "doomed" {
			t.Fatalf("force rm %s %v", r.text, err)
		}
		w.await(t, "cancel "+id)
	})
	t.Run("operation-rejoin", func(t *testing.T) {
		var r toolResult
		select {
		case resp := <-rejoin:
			r = decodeTool(t, resp)
		case <-time.After(mcpWait):
			t.Fatal("the rejoining removal did not answer")
		}
		pend, err := contract.ParseRoleRemovePendingResponse([]byte(r.text))
		if r.isError || err != nil || pend.OperationID != op {
			t.Fatalf("rejoin %s %v", r.text, err)
		}
		// The worker ends the stubborn task: the plane completes the removal.
		w.mu.Lock()
		var ids []string
		for id, s := range w.tasks {
			if s.script == scriptStubborn && s.result == nil {
				ids = append(ids, id)
			}
		}
		w.mu.Unlock()
		for _, id := range ids {
			w.release(id)
			p.awaitTask(t, id, func(v contract.TaskView) bool { return v.State == contract.TaskCancelled })
		}
		poll(t, "removal completed", func() bool {
			r := m.call("role_show", map[string]any{"id": "doomed"})
			return r.isError && strings.Contains(r.text, `"not_found"`)
		})
	})
	t.Run("instance-reuse", func(t *testing.T) {
		m.ok("role_add", roleArgs("doomed", "doomed", w.id, 1))
		e := m.fails("role_rm", map[string]any{"id": "doomed", "force": true, "operation": op}, contract.CodeConflict)
		if e.Details["reason"] != contract.ReasonRemovalOperationMismatch {
			t.Fatalf("stale token %+v", e)
		}
		if text := m.ok("role_show", map[string]any{"id": "doomed"}); !strings.Contains(text, `"removing":false`) {
			t.Fatalf("the new instance was touched: %s", text)
		}
	})
	t.Run("force-completed", func(t *testing.T) {
		p.addRole(t, "quickrm", "quickrm", w.id, 1)
		poll(t, "quickrm ready", func() bool {
			return strings.Contains(m.ok("role_show", map[string]any{"id": "quickrm"}), `"can_accept":true`)
		})
		id := taskIDOf(t, m.ok("dispatch", dispatchArgs("id", "quickrm", "hold then cancel", nil)))
		w.await(t, "started "+id)
		if text := m.ok("role_rm", map[string]any{"id": "quickrm", "force": true}); text != `{"version":5,"removed":"quickrm"}` {
			t.Fatalf("force rm %s", text)
		}
		if v := p.awaitTask(t, id, func(v contract.TaskView) bool { return contract.TaskTerminal(v.State) }); v.State != contract.TaskCancelled {
			t.Fatalf("task %s", v.State)
		}
	})
}

// FP-5: dispatch, bounded waiting and attribution.
func TestMCPDispatch(t *testing.T) {
	p := startMCPPlane(t, 0)
	w := startMCPWorker(t, p, nodeIDN('7'))
	p.addRole(t, "worker-a", "impl", w.id, 4)
	p.addRole(t, "solo", "solo", w.id, 1)
	x := startPlaneProxy(t, p)
	m := startMCP(t, x.trust()...)
	poll(t, "roles ready", func() bool {
		return strings.Count(m.ok("role_ls", nil), `"can_accept":true`) == 2
	})
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	var held []string
	t.Run("target-id", func(t *testing.T) {
		text := m.ok("dispatch", dispatchArgs("id", "worker-a", "quick one", map[string]any{"payload": []string{"repo://x"}}))
		v, err := contract.ParseDispatchResponse([]byte(text))
		if err != nil || v.Role.ID != "worker-a" || v.Request.Target.Kind != "id" || !strings.HasSuffix(text, `"wait_result":null}`) {
			t.Fatalf("dispatch %s %v", text, err)
		}
	})
	t.Run("target-name", func(t *testing.T) {
		v, err := contract.ParseDispatchResponse([]byte(m.ok("dispatch", dispatchArgs("name", "impl", "quick two", nil))))
		if err != nil || v.Role.ID != "worker-a" || v.Request.Target != (contract.TaskTarget{Kind: "name", Value: "impl"}) {
			t.Fatalf("dispatch by name %+v %v", v, err)
		}
		m.fails("dispatch", dispatchArgs("name", "nobody", "quick", nil), contract.CodeNotFound)
		m.fails("dispatch", map[string]any{"target": map[string]any{"kind": "id", "value": "worker-a", "name": "impl"}, "goal": "g", "acceptance": "a"}, contract.CodeInvalidArgument)
	})
	t.Run("overrides", func(t *testing.T) {
		v, err := contract.ParseDispatchResponse([]byte(m.ok("dispatch", dispatchArgs("id", "worker-a", "quick three",
			map[string]any{"override": map[string]any{"model": "override model", "effort": "high", "timeout": "0"}}))))
		if err != nil || v.Effective.Model != "override model" || v.Effective.Effort != "high" || v.Effective.Timeout != 0 {
			t.Fatalf("overrides %+v %v", v.Effective, err)
		}
		m.fails("dispatch", dispatchArgs("id", "worker-a", "quick", map[string]any{"override": map[string]any{}}), contract.CodeInvalidArgument)
	})
	t.Run("async", func(t *testing.T) {
		start := time.Now()
		text := m.ok("dispatch", dispatchArgs("id", "worker-a", "hold async", nil))
		v, err := contract.ParseDispatchResponse([]byte(text))
		if err != nil || contract.TaskTerminal(v.State) || !strings.HasSuffix(text, `"wait_result":null}`) {
			t.Fatalf("async %s %v", text, err)
		}
		held = append(held, v.TaskID)
		if el := time.Since(start); el > 5*time.Second {
			t.Fatalf("admission took %v", el)
		}
	})
	t.Run("attribution", func(t *testing.T) {
		v := p.awaitTask(t, held[0], func(contract.TaskView) bool { return true })
		want := contract.RequestedBy{Name: "coordinator-test", Version: "7.0.1", Hostname: host}
		if v.Request.RequestedBy != want {
			t.Fatalf("requested_by %+v, want %+v", v.Request.RequestedBy, want)
		}
		sum, _, err := p.cl.ListTasks(context.Background(), "", 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sum {
			if s.TaskID == held[0] && s.RequestedBy != want {
				t.Fatalf("persisted summary %+v", s.RequestedBy)
			}
		}
	})
	t.Run("invalid-attribution", func(t *testing.T) {
		before := x.count("POST /api/v1/tasks")
		bad := startMCPProc(t, x.trust()...)
		bad.dispatch()
		bad.initialize("Claude Code", "2.1.0")
		e := bad.fails("dispatch", dispatchArgs("id", "worker-a", "quick", nil), contract.CodeInvalidArgument)
		if e.Details["field"] != "requested_by" || x.count("POST /api/v1/tasks") != before {
			t.Fatalf("invalid attribution %+v", e)
		}
		bad.ok("task_ls", map[string]any{"limit": 1})
	})
	t.Run("last-slot", func(t *testing.T) {
		other := startMCP(t, x.trust()...)
		_, a := m.callAsync("dispatch", dispatchArgs("id", "solo", "hold solo", nil))
		_, b := other.callAsync("dispatch", dispatchArgs("id", "solo", "hold solo", nil))
		ra, rb := decodeTool(t, m.wait(a)), decodeTool(t, other.wait(b))
		if ra.isError == rb.isError {
			t.Fatalf("both or neither admitted: %s / %s", ra.text, rb.text)
		}
		ok, lost := ra, rb
		if ra.isError {
			ok, lost = rb, ra
		}
		e, err := contract.ParseErrorBody([]byte(lost.text))
		// The plane refuses the loser outright (no capacity, or its gate
		// busy with the winner): nothing was dispatched, never ambiguous.
		if err != nil || e.Code != contract.CodeUnavailable || (e.Details["reason"] != contract.ReasonNoCapacity && e.Details["reason"] != contract.ReasonBusy) ||
			e.Details["candidates"] == nil || strings.Contains(e.Message, "may have happened") {
			t.Fatalf("last slot loser %s", lost.text)
		}
		held = append(held, taskIDOf(t, ok.text))
	})
	t.Run("wait-fast", func(t *testing.T) {
		text := m.ok("dispatch", dispatchArgs("id", "worker-a", "quick wait", map[string]any{"wait": "5s"}))
		id, wr, err := contract.ParseDispatchWaitResponse([]byte(text), 2*time.Second)
		if err != nil || wr.Status != contract.WaitTerminal || wr.Task.State != contract.TaskSucceeded || id != wr.Winner || wr.EffectiveWaitMS > 2000 {
			t.Fatalf("wait-fast %s %v", text, err)
		}
	})
	t.Run("wait-slow", func(t *testing.T) {
		// At B=1s the 6s admission allowance leaves no plane wait: a prompt
		// still_running snapshot (effective wait 0), not a one-second block.
		fast := startMCP(t, append(x.trust(), "--wait-call-budget", "1s")...)
		text := fast.ok("dispatch", dispatchArgs("id", "worker-a", "hold slow", map[string]any{"wait": "5s"}))
		id, wr, err := contract.ParseDispatchWaitResponse([]byte(text), 0)
		if err != nil || wr.Status != contract.WaitStillRunning || wr.EffectiveWaitMS != 0 || len(wr.Tasks) != 1 || wr.Tasks[0].TaskID != id {
			t.Fatalf("wait-slow: %s %v", text, err)
		}
		held = append(held, id)
	})
	t.Run("lost-response", func(t *testing.T) {
		x.mu.Lock()
		x.drops["POST /api/v1/tasks"] = true
		x.mu.Unlock()
		before := x.count("POST /api/v1/tasks")
		e := m.fails("dispatch", dispatchArgs("id", "worker-a", "quick lost", nil), contract.CodeUnavailable)
		x.mu.Lock()
		delete(x.drops, "POST /api/v1/tasks")
		x.mu.Unlock()
		if !strings.Contains(e.Message, "inspect task_ls before dispatching again") || x.count("POST /api/v1/tasks") != before+1 {
			t.Fatalf("lost response %+v (%d requests)", e, x.count("POST /api/v1/tasks")-before)
		}
		var list contract.TaskListResponse
		n := 0
		tasks, _, err := p.cl.ListTasks(context.Background(), "", 100)
		if err != nil {
			t.Fatal(err)
		}
		list.Tasks = tasks
		for _, s := range list.Tasks {
			v, _ := p.cl.ShowTask(context.Background(), s.TaskID, 0)
			if v.Request.Goal == "quick lost" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d tasks admitted for one lost dispatch", n)
		}
	})
	for _, id := range held {
		w.release(id)
	}
}

// ranTask dispatches goal to role and waits until it is terminal.
func ranTask(t *testing.T, p *mcpPlane, m *mcpProc, role, goal string) string {
	t.Helper()
	id := taskIDOf(t, m.ok("dispatch", dispatchArgs("id", role, goal, nil)))
	p.awaitTask(t, id, func(v contract.TaskView) bool { return contract.TaskTerminal(v.State) && v.DurabilityConfirmed })
	return id
}

// FP-6: task reads and logs.
func TestMCPTaskReads(t *testing.T) {
	p := startMCPPlane(t, 0)
	w := startMCPWorker(t, p, nodeIDN('8'))
	p.addRole(t, "reader", "reader", w.id, 4)
	cliA := newNodeCLI(t)
	m := startMCP(t, p.trust()...)
	poll(t, "role ready", func() bool {
		return strings.Contains(m.ok("role_show", map[string]any{"id": "reader"}), `"can_accept":true`)
	})
	ok := ranTask(t, p, m, "reader", "quick read")
	failed := ranTask(t, p, m, "reader", "fail read")
	binary := ranTask(t, p, m, "reader", "logs 70000")
	t.Run("task-ls", func(t *testing.T) {
		r := cliA.run(t, append([]string{"task", "ls", "--json"}, p.trust()...)...)
		sameJSON(t, m.ok("task_ls", nil), r.stdout)
	})
	t.Run("task-show", func(t *testing.T) {
		for _, id := range []string{ok, failed} {
			r := cliA.run(t, append([]string{"task", "show", id, "--json"}, p.trust()...)...)
			sameJSON(t, m.ok("task_show", map[string]any{"id": id}), r.stdout)
		}
		if text := m.ok("task_show", map[string]any{"id": failed}); !strings.Contains(text, `"state":"failed"`) || !strings.Contains(text, `"exit_code":7`) {
			t.Fatalf("failed task %s", text)
		}
		m.fails("task_show", map[string]any{"id": "t_" + strings.Repeat("0", 32)}, contract.CodeNotFound)
	})
	t.Run("task-logs", func(t *testing.T) {
		r := cliA.run(t, append([]string{"task", "logs", ok, "--json"}, p.trust()...)...)
		sameJSON(t, m.ok("task_logs", map[string]any{"id": ok}), r.stdout)
	})
	t.Run("pagination", func(t *testing.T) {
		seen := map[string]bool{}
		args := map[string]any{"limit": 1}
		for page := 0; ; page++ {
			tasks, next, err := contract.ParseTaskListResponse([]byte(m.ok("task_ls", args)))
			if err != nil || len(tasks) != 1 || page > 5 {
				t.Fatalf("page %d: %v %v", page, tasks, err)
			}
			seen[tasks[0].TaskID] = true
			if next == nil {
				break
			}
			args = map[string]any{"limit": 1, "after": *next}
		}
		if !seen[ok] || !seen[failed] || !seen[binary] || len(seen) != 3 {
			t.Fatalf("pages %v", seen)
		}
		m.fails("task_ls", map[string]any{"limit": 101}, contract.CodeInvalidArgument)
	})
	t.Run("tails", func(t *testing.T) {
		v, err := contract.ParseTaskShowResponse([]byte(m.ok("task_show", map[string]any{"id": ok, "lines": 0})))
		if err != nil || v.LogTail != "" {
			t.Fatalf("lines 0: %q %v", v.LogTail, err)
		}
		v, err = contract.ParseTaskShowResponse([]byte(m.ok("task_show", map[string]any{"id": ok, "lines": 1})))
		if err != nil || v.LogTail != "line two\n" || !v.TailTruncated {
			t.Fatalf("lines 1: %q %v", v.LogTail, err)
		}
		v, err = contract.ParseTaskShowResponse([]byte(m.ok("task_show", map[string]any{"id": binary, "lines": 200})))
		if err != nil || len(v.LogTail) > contract.MaxTailBytes {
			t.Fatalf("lines 200: %d bytes %v", len(v.LogTail), err)
		}
		m.fails("task_show", map[string]any{"id": ok, "lines": 201}, contract.CodeInvalidArgument)
	})
	t.Run("binary-logs", func(t *testing.T) {
		r, err := contract.ParseTaskLogsResponse([]byte(m.ok("task_logs", map[string]any{"id": binary})))
		if err != nil || string(r.Data) != string(binaryOutput(70000)) || r.RetainedBytes != 70000 {
			t.Fatalf("binary logs %d bytes %v", len(r.Data), err)
		}
	})
	t.Run("late-logs", func(t *testing.T) {
		// A task the worker ran is decided lost (its complete inventory on
		// a new attachment omits it); the worker's result on the next
		// attachment is kept as late evidence with its own output.
		id := taskIDOf(t, m.ok("dispatch", dispatchArgs("id", "reader", "hold late", nil)))
		w.await(t, "started "+id)
		p.awaitTask(t, id, func(v contract.TaskView) bool { return v.State == contract.TaskRunning })
		w.detach()
		w.attach(t, nil)
		p.awaitTask(t, id, func(v contract.TaskView) bool { return v.State == contract.TaskLost })
		w.sealLocal(id)
		entry := w.inventoryEntry(id)
		w.detach()
		w.attach(t, []contract.TaskInventoryEntry{entry})
		w.await(t, "result-acked "+id)
		v := p.awaitTask(t, id, func(v contract.TaskView) bool { return v.LateResult != nil })
		if v.State != contract.TaskLost {
			t.Fatalf("late result changed the state: %s", v.State)
		}
		r, err := contract.ParseTaskLogsResponse([]byte(m.ok("task_logs", map[string]any{"id": id, "late": true})))
		w.mu.Lock()
		want := string(w.tasks[id].out)
		w.mu.Unlock()
		if err != nil || string(r.Data) != want {
			t.Fatalf("late logs %q %v", r.Data, err)
		}
	})
	t.Run("late-not-found", func(t *testing.T) {
		e := m.fails("task_logs", map[string]any{"id": ok, "late": true}, contract.CodeNotFound)
		if e.Details["reason"] != contract.ReasonNoLateResult {
			t.Fatalf("late not found %+v", e)
		}
	})
	t.Run("output-bounds", func(t *testing.T) {
		id := ranTask(t, p, m, "reader", fmt.Sprintf("logs %d", contract.MaxLogRetainedBytes+100))
		// The maximum retained output (10 MiB) crosses whole: the frame is
		// exactly the CLI's --json logs envelope wrapped once, and that
		// envelope carries the worker's last 10 MiB byte for byte.
		cliOut := cliA.run(t, append([]string{"task", "logs", id, "--json"}, p.trust()...)...)
		logical := strings.TrimSuffix(cliOut.stdout, "\n")
		data := base64.StdEncoding.EncodeToString(binaryOutput(contract.MaxLogRetainedBytes + 100)[100:])
		if cliOut.code != 0 || !strings.Contains(logical, `"data":"`+data+`"`) || !strings.Contains(logical, `"retained_bytes":10485760,"source_bytes":10485860,"dropped_bytes":100,"truncated":true`) {
			t.Fatalf("cli logs %d bytes: %.300s", len(logical), cliOut.stderr)
		}
		rid, c := m.callAsync("task_logs", map[string]any{"id": id})
		r := m.wait(c)
		if frame := logsFrame(rid, logical); r.raw != frame || len(r.raw) > mcp.MaxFrameBytes {
			t.Fatalf("maximum logs frame of %d bytes differs from the CLI's %d-byte envelope", len(r.raw), len(logical))
		}
		v, err := contract.ParseTaskShowResponse([]byte(m.ok("task_show", map[string]any{"id": id, "lines": 200})))
		if err != nil || len(v.LogTail) > contract.MaxTailBytes || !v.Log.Truncated {
			t.Fatalf("maximum tail %d %v", len(v.LogTail), err)
		}
	})
}

// logsFrame is the exact tool answer frame for request id whose logical
// JSON is a task logs envelope (no byte in it needs escaping but quotes).
func logsFrame(id, logical string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"result":{"content":[{"type":"text","text":"` + strings.ReplaceAll(logical, `"`, `\"`) + `"}],"isError":false}}` + "\n"
}

// waitOf decodes a task_wait answer.
func waitOf(t *testing.T, text string, ids []string) contract.WaitResponse {
	t.Helper()
	r, err := contract.ParseWaitResponse([]byte(text), ids, 5*time.Minute)
	if err != nil {
		t.Fatalf("wait answer %.300s: %v", text, err)
	}
	return r
}

// FP-7: cancellation and bounded waits under the interim call budget.
func TestMCPWaitCancel(t *testing.T) {
	p := startMCPPlane(t, 0)
	w := startMCPWorker(t, p, nodeIDN('a'))
	p.addRole(t, "waiter", "waiter", w.id, 24)
	x := startPlaneProxy(t, p)
	m := startMCP(t, x.trust()...)
	poll(t, "role ready", func() bool {
		return strings.Contains(m.ok("role_show", map[string]any{"id": "waiter"}), `"can_accept":true`)
	})
	hold := func(t *testing.T) string {
		t.Helper()
		id := taskIDOf(t, m.ok("dispatch", dispatchArgs("id", "waiter", "hold wait", nil)))
		w.await(t, "started "+id)
		p.awaitTask(t, id, func(v contract.TaskView) bool { return v.State == contract.TaskRunning })
		return id
	}
	// cancelOf decodes a task_cancel answer's decisive fields.
	cancelOf := func(t *testing.T, text string) (id string, accepted, stop bool, state string) {
		t.Helper()
		var r struct {
			TaskID   string `json:"task_id"`
			Accepted bool   `json:"accepted"`
			Task     struct {
				State         string `json:"state"`
				StopRequested bool   `json:"stop_requested"`
			} `json:"task"`
		}
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			t.Fatalf("cancel answer %.300s: %v", text, err)
		}
		return r.TaskID, r.Accepted, r.Task.StopRequested, r.Task.State
	}
	done := ranTask(t, p, m, "waiter", "quick done")
	var holds []string
	defer func() {
		for _, id := range holds {
			w.release(id)
		}
	}()
	t.Run("cancel-accepted", func(t *testing.T) {
		id := hold(t)
		text := m.ok("task_cancel", map[string]any{"id": id})
		if got, accepted, stop, _ := cancelOf(t, text); !accepted || !stop || got != id {
			t.Fatalf("cancel %s", text)
		}
		if v := p.awaitTask(t, id, func(v contract.TaskView) bool { return contract.TaskTerminal(v.State) }); v.State != contract.TaskCancelled {
			t.Fatalf("cancelled task %s", v.State)
		}
	})
	t.Run("cancel-terminal", func(t *testing.T) {
		text := m.ok("task_cancel", map[string]any{"id": done})
		if _, accepted, _, state := cancelOf(t, text); accepted || state != contract.TaskSucceeded {
			t.Fatalf("cancel terminal %s", text)
		}
	})
	t.Run("cancel-errors", func(t *testing.T) {
		m.fails("task_cancel", map[string]any{"id": "t_" + strings.Repeat("0", 32)}, contract.CodeNotFound)
		m.fails("task_cancel", map[string]any{"id": "nope"}, contract.CodeInvalidArgument)
		m.fails("task_cancel", map[string]any{"id": done, "force": true}, contract.CodeInvalidArgument)
	})
	t.Run("wait-one", func(t *testing.T) {
		r := waitOf(t, m.ok("task_wait", map[string]any{"task_ids": []string{done}, "wait": "1s"}), []string{done})
		if r.Status != contract.WaitTerminal || r.Winner != done || r.Task.State != contract.TaskSucceeded {
			t.Fatalf("wait one %+v", r)
		}
	})
	t.Run("wait-many", func(t *testing.T) {
		var ids []string
		for range 16 {
			ids = append(ids, hold(t))
		}
		holds = append(holds, ids...)
		r := waitOf(t, m.ok("task_wait", map[string]any{"task_ids": ids, "wait": "0"}), ids)
		if r.Status != contract.WaitStillRunning || len(r.Tasks) != 16 {
			t.Fatalf("wait many %+v", r)
		}
		for i, row := range r.Tasks {
			if row.TaskID != ids[i] || row.State != contract.TaskRunning {
				t.Fatalf("row %d %+v", i, row)
			}
		}
		ids = append(ids, "t_"+strings.Repeat("1", 32))
		m.fails("task_wait", map[string]any{"task_ids": ids}, contract.CodeInvalidArgument)
	})
	t.Run("any-terminal", func(t *testing.T) {
		ids := []string{holds[0], done, holds[1]}
		r := waitOf(t, m.ok("task_wait", map[string]any{"task_ids": ids}), ids)
		if r.Status != contract.WaitTerminal || r.Winner != done {
			t.Fatalf("any terminal %+v", r)
		}
	})
	t.Run("snapshot", func(t *testing.T) {
		start := time.Now()
		r := waitOf(t, m.ok("task_wait", map[string]any{"task_ids": []string{holds[2]}, "wait": "0"}), []string{holds[2]})
		if r.Status != contract.WaitStillRunning || r.EffectiveWaitMS != 0 || time.Since(start) > 5*time.Second {
			t.Fatalf("snapshot %+v", r)
		}
	})
	t.Run("interim-default", func(t *testing.T) {
		// The shipping 10s budget: an omitted wait asks for B minus both
		// reserves (8s at most); an immediately terminal task answers at
		// once.
		r := waitOf(t, m.ok("task_wait", map[string]any{"task_ids": []string{done}}), []string{done})
		if r.Status != contract.WaitTerminal || r.EffectiveWaitMS <= 6000 || r.EffectiveWaitMS > 8000 {
			t.Fatalf("interim default %+v", r)
		}
		if l := m.request("tools/list", nil); !strings.Contains(string(l.Result), "B=10s") || !strings.Contains(string(l.Result), "UNVERIFIED") {
			t.Fatal("the interim budget is not described")
		}
	})
	t.Run("budget-deadline", func(t *testing.T) {
		// The 5m request is capped by the 1s budget: the plane is asked for
		// at most B minus both reserves (500 ms) and answers still_running;
		// the session never ends (an answer after D would close it).
		b := startMCP(t, append(x.trust(), "--wait-call-budget", "1s")...)
		r := waitOf(t, b.ok("task_wait", map[string]any{"task_ids": []string{holds[3]}, "wait": "5m"}), []string{holds[3]})
		if r.Status != contract.WaitStillRunning || r.EffectiveWaitMS > 500 {
			t.Fatalf("budget %+v", r)
		}
		if res := b.request("ping", nil); res.Error != nil {
			t.Fatal("the session ended")
		}
	})
	t.Run("delivery-deadline", func(t *testing.T) {
		// D itself closes a session whose answer is incomplete while no
		// final Write is in progress (design r0.4, DS1 guard): the frame is
		// at least 4 x 64 KiB, the pipe holds at most 64 KiB, and this
		// reader takes at most 4 KiB per read (plus its 4 KiB buffer) at
		// least 100 ms apart, so by D the writer cannot reach the last
		// chunk (the one Write that could finish the frame).
		const (
			readSize  = 4 << 10
			readerBuf = 4 << 10
			pace      = 150 * time.Millisecond
		)
		big := ranTask(t, p, m, "waiter", "big result")
		logical := m.ok("task_wait", map[string]any{"task_ids": []string{big}})
		esc, _ := json.Marshal(logical)
		frameMin := len(esc) // the text item alone, escaped: a lower bound of the frame
		raw := startMCPProc(t, append(x.trust(), "--wait-call-budget", "1s")...)
		capacity := pipeCapacity(t, raw.stdout)
		reachable := capacity + readerBuf + readSize*(int(time.Second/pace)+2)
		if frameMin < 4*(64<<10) || capacity > 64<<10 || reachable >= frameMin-mcp.WriteChunk {
			t.Fatalf("guard: frame >= %d bytes, pipe %d bytes, %d bytes reachable by D", frameMin, capacity, reachable)
		}
		t.Logf("guard: frame >= %d bytes, pipe %d bytes, at most %d bytes reachable by D", frameMin, capacity, reachable)
		br := bufio.NewReaderSize(raw.stdout, readerBuf)
		rawInit(t, raw, br)
		start := time.Now()
		io.WriteString(raw.stdin, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"task_wait","arguments":{"task_ids":["`+big+`"]}}}`+"\n")
		buf := make([]byte, readSize)
		got, paced := 0, true
		for {
			n, err := io.ReadFull(br, buf)
			got += n
			if strings.IndexByte(string(buf[:n]), '\n') >= 0 {
				t.Fatalf("the frame completed (%d bytes); it must outlast D", got)
			}
			if err != nil {
				break // the session closed its output at D
			}
			select {
			case <-raw.exited:
				paced = false // drain what the pipe still holds
			default:
			}
			if paced {
				time.Sleep(pace) // keeps the writer's progress timer alive
			}
		}
		raw.requireExit(5)
		elapsed := time.Since(start)
		stderr := raw.stderr.String()
		if !strings.Contains(stderr, "not delivered within its 1s call budget") || strings.Contains(stderr, "no progress") ||
			strings.Contains(stderr, mcp.LateWriteNote) || strings.Contains(stderr, mcp.LateShortNote) || elapsed < time.Second {
			t.Fatalf("after %v (%d bytes): %q", elapsed, got, stderr)
		}
		// Everything the writer wrote was read (the pipe drained to EOF):
		// it never reached the final chunk.
		t.Logf("the writer wrote %d bytes before the session ended", got)
		if got >= frameMin-mcp.WriteChunk {
			t.Fatalf("the writer wrote %d of at least %d bytes: it may have reached the final chunk", got, frameMin)
		}
	})
	t.Run("plane-cap", func(t *testing.T) {
		pc := startMCPPlane(t, 100*time.Millisecond)
		wc := startMCPWorker(t, pc, nodeIDN('b'))
		pc.addRole(t, "capped", "capped", wc.id, 1)
		mc := startMCP(t, pc.trust()...)
		poll(t, "capped ready", func() bool {
			return strings.Contains(mc.ok("role_show", map[string]any{"id": "capped"}), `"can_accept":true`)
		})
		id := taskIDOf(t, mc.ok("dispatch", dispatchArgs("id", "capped", "hold capped", nil)))
		wc.await(t, "started "+id)
		r := waitOf(t, mc.ok("task_wait", map[string]any{"task_ids": []string{id}}), []string{id})
		if r.Status != contract.WaitStillRunning || r.EffectiveWaitMS != 100 {
			t.Fatalf("plane cap %+v", r)
		}
		wc.release(id)
	})
	t.Run("restart-budget", func(t *testing.T) {
		pr := startMCPPlane(t, 0)
		wr := startMCPWorker(t, pr, nodeIDN('c'))
		pr.addRole(t, "restart", "restart", wr.id, 1)
		xr := startPlaneProxy(t, pr)
		mr := startMCP(t, xr.trust()...)
		poll(t, "restart ready", func() bool {
			return strings.Contains(mr.ok("role_show", map[string]any{"id": "restart"}), `"can_accept":true`)
		})
		id := taskIDOf(t, mr.ok("dispatch", dispatchArgs("id", "restart", "hold restart", nil)))
		wr.await(t, "started "+id)
		_, c := mr.callAsync("task_wait", map[string]any{"task_ids": []string{id}})
		xr.await(t, "request POST /api/v1/tasks/wait")
		// The restart interrupts the transport (the plane's own shutdown
		// answer is dropped with the connection): the client retries with
		// the same IDs within the one outer budget.
		xr.mu.Lock()
		xr.drops["POST /api/v1/tasks/wait"] = true
		xr.mu.Unlock()
		entry := wr.runningEntry(id)
		wr.detach()
		pr.stop(t)
		xr.await(t, "request POST /api/v1/tasks/wait")
		pr.start(t)
		xr.mu.Lock()
		delete(xr.drops, "POST /api/v1/tasks/wait")
		xr.mu.Unlock()
		// The worker reconnects to the restarted plane, still running the
		// task, and then finishes it: the retried wait sees it end.
		wr.attach(t, []contract.TaskInventoryEntry{entry})
		wr.release(id)
		r := decodeTool(t, mr.wait(c))
		wres := waitOf(t, r.text, []string{id})
		if r.isError || wres.Status != contract.WaitTerminal || wres.Winner != id || xr.count("POST /api/v1/tasks/wait") < 3 {
			t.Fatalf("restart (%d requests): %s; worker %v", xr.count("POST /api/v1/tasks/wait"), r.text, wr.events.snapshot())
		}
	})
	t.Run("own-deadline", func(t *testing.T) {
		b := startMCP(t, append(x.trust(), "--wait-call-budget", "1s")...)
		x.events.clear()
		release := x.hold("POST /api/v1/tasks/wait")
		defer release()
		_, c := b.callAsync("task_wait", map[string]any{"task_ids": []string{holds[4]}})
		x.await(t, "response POST /api/v1/tasks/wait")
		// The plane answered; the proxy delivers it only after the MCP
		// client's own deadline (D-R) cancelled the request.
		x.await(t, "canceled POST /api/v1/tasks/wait")
		release()
		r := decodeTool(t, b.wait(c))
		e, err := contract.ParseErrorBody([]byte(r.text))
		if !r.isError || err != nil || e.Code != contract.CodeUnavailable || e.Details["reason"] != mcp.ReasonCallBudget {
			t.Fatalf("own deadline %s", r.text)
		}
	})
	t.Run("cancel-wait-only", func(t *testing.T) {
		x.events.clear()
		id, c := m.callAsync("task_wait", map[string]any{"task_ids": []string{holds[5]}})
		x.await(t, "request POST /api/v1/tasks/wait")
		m.notify("notifications/cancelled", map[string]any{"requestId": json.Number(id)})
		x.await(t, "canceled POST /api/v1/tasks/wait")
		if r := m.request("ping", nil); r.Error != nil {
			t.Fatal("session unusable")
		}
		if v := p.awaitTask(t, holds[5], func(contract.TaskView) bool { return true }); v.State != contract.TaskRunning || v.StopRequested {
			t.Fatalf("cancelling the wait touched the task: %+v", v.State)
		}
		if n := x.count("POST /api/v1/tasks/" + holds[5] + "/cancel"); n != 0 {
			t.Fatalf("CancelTask sent %d times", n)
		}
		// The whole transcript: the cancelled wait never answered.
		m.stdin.Close()
		m.requireExit(0)
		m.drained()
		select {
		case r := <-c:
			t.Fatalf("the cancelled wait answered %s", r.raw)
		default:
		}
	})
	t.Run("catalog-interim", func(t *testing.T) {
		root := testkit.MustRepoRoot(t)
		doc, err := os.ReadFile(filepath.Join(root, "docs", "support-catalog.md"))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(root, "tests", "testdata", "support-catalog.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := checkInterimCatalog(string(doc), raw); err != nil {
			t.Fatal(err)
		}
		// A promotion to VERIFIED without new evidence, a lost sentence or
		// a surviving prohibition is rejected.
		for _, mutate := range []func(string, []byte) (string, []byte){
			func(d string, j []byte) (string, []byte) {
				return d, []byte(strings.Replace(string(j), `"status": "UNVERIFIED",
        "value": "MCP tool-call timeout`, `"status": "VERIFIED",
        "value": "MCP tool-call timeout`, 1))
			},
			func(d string, j []byte) (string, []byte) { return strings.Replace(d, interimSentence, "", 1), j },
			func(d string, j []byte) (string, []byte) {
				return d + "\nIteration 07 must verify the effective MCP timeout before any MCP tool exposes a wait.\n", j
			},
			func(d string, j []byte) (string, []byte) {
				return d, []byte(strings.Replace(string(j), " "+interimSentence, "", 1))
			},
		} {
			d, j := mutate(string(doc), raw)
			if checkInterimCatalog(d, j) == nil {
				t.Fatal("a broken catalog passed the interim check")
			}
		}
	})
}

// rawInit initializes a raw-mode process reading its answers from br.
func rawInit(t *testing.T, m *mcpProc, br *bufio.Reader) {
	t.Helper()
	io.WriteString(m.stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`+"\n")
	if line, err := br.ReadString('\n'); err != nil || !strings.Contains(line, `"serverInfo"`) {
		t.Fatalf("initialize %q %v", line, err)
	}
	io.WriteString(m.stdin, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
}

// interimSentence is coordinator.md's normative sentence.
const interimSentence = "Owner-authorized interim exception: iteration 07a ships task_wait and dispatch-with-wait with an UNVERIFIED 10s outer call budget, " +
	"shorter plane waits reserve transport/admission/response time, and any increase requires local timeout qualification with an explicit response margin."

// checkInterimCatalog is the 07a catalog policy: both prose locations
// carry the sentence (the second links to the first), the old
// prohibitions are gone, and every mcp_timeout fact ends with the
// sentence while staying UNVERIFIED with its evidence and owner.
func checkInterimCatalog(doc string, raw []byte) error {
	if strings.Count(doc, interimSentence) != 1 || !strings.Contains(doc, "(#interim-mcp-wait-exception)") || !strings.Contains(doc, `<a id="interim-mcp-wait-exception"></a>`) {
		return errors.New("docs/support-catalog.md lacks the interim sentence or its link")
	}
	for _, old := range []string{"must expose the dependency as a blocker, not manufacture a timeout default", "before any MCP tool exposes a wait"} {
		if strings.Contains(doc, old) {
			return fmt.Errorf("docs/support-catalog.md keeps the prohibition %q", old)
		}
	}
	entries, err := catalog.Decode(raw)
	if err != nil {
		return err
	}
	if len(entries) != 4 {
		return fmt.Errorf("%d catalog entries", len(entries))
	}
	for _, e := range entries {
		f := e.Facts["mcp_timeout"]
		if f.Status != catalog.Unverified || !strings.HasSuffix(f.Value, " "+interimSentence) || len(f.Evidence) == 0 || f.VerificationIteration != catalog.Owner(e.ID, "mcp_timeout") {
			return fmt.Errorf("%s mcp_timeout %+v", e.ID, f)
		}
		for _, k := range []string{"mcp_timeout_override", "mcp_progress_extension"} {
			if e.Facts[k].Status != catalog.Unverified || strings.Contains(e.Facts[k].Value, interimSentence) {
				return fmt.Errorf("%s %s changed", e.ID, k)
			}
		}
	}
	return nil
}

// FP-8: process lifetime, isolation and shutdown.
func TestMCPLifetime(t *testing.T) {
	p := startMCPPlane(t, 0)
	w := startMCPWorker(t, p, nodeIDN('d'))
	p.addRole(t, "life", "life", w.id, 4)
	x := startPlaneProxy(t, p)
	var procs []*mcpProc
	started := func(m *mcpProc) *mcpProc { procs = append(procs, m); return m }
	setup := started(startMCP(t, p.trust()...))
	poll(t, "role ready", func() bool {
		return strings.Contains(setup.ok("role_show", map[string]any{"id": "life"}), `"can_accept":true`)
	})
	big := ranTask(t, p, setup, "life", "big result")
	maxLogs := ranTask(t, p, setup, "life", fmt.Sprintf("logs %d", contract.MaxLogRetainedBytes))
	t.Run("eof", func(t *testing.T) {
		m := started(startMCP(t, p.trust()...))
		m.ok("node_ls", nil)
		m.stdin.Close()
		m.requireExit(0)
		m.protocolOnly()
	})
	for _, c := range []struct {
		name string
		sig  syscall.Signal
	}{{"sigterm", syscall.SIGTERM}, {"sigint", syscall.SIGINT}} {
		t.Run(c.name, func(t *testing.T) {
			m := started(startMCP(t, p.trust()...))
			m.ok("node_ls", nil)
			m.cmd.Process.Signal(c.sig)
			m.requireExit(130)
		})
	}
	t.Run("closed-stdout", func(t *testing.T) {
		m := started(startMCPProc(t, p.trust()...))
		br := bufio.NewReader(m.stdout)
		rawInit(t, m, br)
		io.WriteString(m.stdin, `{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n")
		if line, err := br.ReadString('\n'); err != nil || !strings.Contains(line, `"id":2`) {
			t.Fatalf("ping %q %v", line, err)
		}
		m.stdout.Close() // the parent stops reading; stdin stays open so EOF cannot win
		io.WriteString(m.stdin, `{"jsonrpc":"2.0","id":3,"method":"ping"}`+"\n")
		m.requireExit(5)
		if !strings.Contains(m.stderr.String(), "cannot write to stdout") {
			t.Fatalf("stderr %q", m.stderr.String())
		}
	})
	t.Run("stalled-reader", func(t *testing.T) {
		m := started(startMCPProc(t, p.trust()...))
		br := bufio.NewReader(m.stdout)
		rawInit(t, m, br)
		io.WriteString(m.stdin, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"task_show","arguments":{"id":"`+big+`","lines":200}}}`+"\n")
		// Never read again: once the pipe is full the writer makes no
		// progress and the session ends after one second.
		m.requireExit(5)
		if !strings.Contains(m.stderr.String(), "made no progress for 1s") {
			t.Fatalf("stderr %q", m.stderr.String())
		}
	})
	t.Run("slow-reader-max-logs", func(t *testing.T) {
		m := started(startMCPProc(t, p.trust()...))
		br := bufio.NewReaderSize(m.stdout, 4<<10)
		rawInit(t, m, br)
		io.WriteString(m.stdin, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"task_logs","arguments":{"id":"`+maxLogs+`"}}}`+"\n")
		start := time.Now()
		var frame []byte
		chunk := make([]byte, 4<<10)
		// A paced acknowledgment per 4 KiB chunk: about 700 µs each on
		// schedule (sleeping only when ahead of it, so coarse sleeps do not
		// accumulate), 2-3 s for the ~14 MB frame; every chunk progresses
		// far within the writer's one second.
		const pace = 700 * time.Microsecond
		next := start
		for {
			if d := time.Until(next); d > 0 {
				time.Sleep(d)
			}
			next = next.Add(pace)
			n, err := br.Read(chunk)
			frame = append(frame, chunk[:n]...)
			if len(frame) > 0 && frame[len(frame)-1] == '\n' {
				break
			}
			if err != nil {
				t.Fatalf("read after %d bytes: %v; stderr %q", len(frame), err, m.stderr.String())
			}
		}
		elapsed := time.Since(start)
		cliOut := newNodeCLI(t).run(t, append([]string{"task", "logs", maxLogs, "--json"}, p.trust()...)...)
		logical := strings.TrimSuffix(cliOut.stdout, "\n")
		if cliOut.code != 0 || !strings.Contains(logical, `"data":"`+base64.StdEncoding.EncodeToString(binaryOutput(contract.MaxLogRetainedBytes))+`"`) {
			t.Fatalf("cli logs: %.300s", cliOut.stderr)
		}
		if string(frame) != logsFrame("2", logical) || elapsed <= time.Second {
			t.Fatalf("paced maximum logs in %v: the %d-byte frame differs", elapsed, len(frame))
		}
		t.Logf("paced transfer of a %d-byte frame took %v", len(frame), elapsed)
		io.WriteString(m.stdin, `{"jsonrpc":"2.0","id":3,"method":"ping"}`+"\n")
		if line, err := br.ReadString('\n'); err != nil || line != `{"jsonrpc":"2.0","id":3,"result":{}}`+"\n" {
			t.Fatalf("ping after the paced frame %q %v", line, err)
		}
	})
	t.Run("outstanding-wait", func(t *testing.T) {
		id := taskIDOf(t, setup.ok("dispatch", dispatchArgs("id", "life", "hold outstanding", nil)))
		w.await(t, "started "+id)
		p.awaitTask(t, id, func(v contract.TaskView) bool { return v.State == contract.TaskRunning })
		m := started(startMCP(t, append(x.trust(), "--wait-call-budget", "1s")...))
		x.events.clear()
		_, c := m.callAsync("task_wait", map[string]any{"task_ids": []string{id}})
		x.await(t, "request POST /api/v1/tasks/wait")
		m.stdin.Close()
		m.requireExit(0)
		m.drained()
		select {
		case r := <-c:
			t.Fatalf("an outstanding wait answered after EOF: %s", r.raw)
		default:
		}
		if v := p.awaitTask(t, id, func(contract.TaskView) bool { return true }); v.State != contract.TaskRunning || v.StopRequested {
			t.Fatalf("EOF touched the task: %s", v.State)
		}
		w.release(id)
	})
	t.Run("task-survives", func(t *testing.T) {
		// AC-SH-2: an admitted task survives the MCP process's termination
		// during a wait and completes.
		m := started(startMCP(t, x.trust()...))
		id := taskIDOf(t, m.ok("dispatch", dispatchArgs("id", "life", "hold survivor", nil)))
		w.await(t, "started "+id)
		p.awaitTask(t, id, func(v contract.TaskView) bool { return v.State == contract.TaskRunning })
		x.events.clear()
		m.callAsync("task_wait", map[string]any{"task_ids": []string{id}})
		x.await(t, "request POST /api/v1/tasks/wait")
		m.cmd.Process.Signal(syscall.SIGTERM)
		m.requireExit(130)
		if x.count("POST /api/v1/tasks/"+id+"/cancel") != 0 {
			t.Fatal("termination cancelled the task")
		}
		if v := p.awaitTask(t, id, func(contract.TaskView) bool { return true }); v.State != contract.TaskRunning {
			t.Fatalf("after termination %s", v.State)
		}
		w.release(id)
		if v := p.awaitTask(t, id, func(v contract.TaskView) bool { return contract.TaskTerminal(v.State) }); v.State != contract.TaskSucceeded {
			t.Fatalf("survivor %s", v.State)
		}
		if _, err := p.cl.ListNodes(context.Background()); err != nil {
			t.Fatalf("the plane stopped: %v", err)
		}
	})
	t.Run("reaping", func(t *testing.T) {
		m := started(startMCP(t, p.trust()...))
		m.ok("task_ls", nil)
		m.stdin.Close()
		m.requireExit(0)
		for _, q := range procs {
			q.cmd.Process.Signal(syscall.SIGKILL)
			q.reaped()
		}
		if s := setup.stderr.String(); strings.Contains(s, "panic") {
			t.Fatalf("stderr %q", s)
		}
	})
}
