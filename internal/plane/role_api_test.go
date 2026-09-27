package plane

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestRoleAPIContract is the plane half of UT FP-7: the five operations
// through the exported client against the production handlers over
// verified TLS, then every route, method, version, query, body, content
// type and size rule at the HTTP level, with no request echo.
func TestRoleAPIContract(t *testing.T) {
	t.Parallel()
	rp := startRolePlane(t, idA, idB)
	p := rp.online(t, idA)
	t.Run("operations", func(t *testing.T) {
		if list, err := rp.cl.ListRoles(bg); err != nil || len(list) != 0 {
			t.Fatalf("empty list = %+v %v", list, err)
		}
		c := roleCfg("worker-b", "reviewer", idA)
		c.Model = "free text <&> -model"
		c.Timeout, c.HasTimeout = 90*time.Minute, true
		v := rp.add(t, p, "p2", "p3", c)
		if v.ID != "worker-b" || v.Timeout != 90*time.Minute || v.RegistrationOrder != 1 || v.Inflight != 0 || v.NodeLiveness != contract.LivenessOnline || !v.AdapterTestOnly || v.Model != c.Model {
			t.Fatalf("add = %+v", v)
		}
		rp.add(t, p, "p4", "p5", roleCfg("worker-a", "implementer", idA))
		rp.add(t, p, "p6", "p7", roleCfg("worker-c", "reviewer", idA))
		// Grouped by name, then registration order.
		list, err := rp.cl.ListRoles(bg)
		if err != nil || len(list) != 3 || list[0].ID != "worker-a" || list[1].ID != "worker-b" || list[2].ID != "worker-c" {
			t.Fatalf("list = %+v %v", list, err)
		}
		conc := 4
		res := rp.setAsync(bg, "worker-b", contract.RolePatch{Concurrency: &conc})
		p.validateOK("p8")
		if v, err := res.wait(t); err != nil || v.Concurrency != 4 || v.RegistrationOrder != 1 {
			t.Fatalf("set = %+v %v", v, err)
		}
		p.ackReplace("p9")
		if v := rp.view(t, "worker-b"); v.Concurrency != 4 || v.Name != "reviewer" {
			t.Fatalf("show = %+v", v)
		}
		// Node discovery lists the node's configured roles in registration
		// order with the configured concurrency.
		n := rp.show(t, idA)
		if len(n.Roles) != 3 || n.Roles[0].RoleID != "worker-b" || n.Roles[0].Concurrency != 4 || n.Roles[1].RoleID != "worker-a" || n.Roles[2].RoleID != "worker-c" {
			t.Fatalf("node roles = %+v", n.Roles)
		}
		if nodes, err := rp.cl.ListNodes(bg); err != nil || len(nodes) != 2 || len(nodes[0].Roles) != 3 || len(nodes[1].Roles) != 0 {
			t.Fatalf("node list = %+v %v", nodes, err)
		}
		if err := rp.cl.RemoveRole(bg, "worker-c", true); err != nil {
			t.Fatal(err)
		}
		p.ackReplace("p10")
		p.heartbeat(2)
	})
	httpc := keepAliveClient(t, readCA(t, rp.root))
	do := func(method, path, version, ctype, body string, extra map[string]string) (int, string, string) {
		req, _ := http.NewRequest(method, rp.url+path, strings.NewReader(body))
		if body == "" {
			req.Body = http.NoBody
		}
		if version != "" {
			req.Header.Set(contract.ProtocolHeader, version)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		for k, v := range extra {
			req.Header.Add(k, v)
		}
		resp, err := httpc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := new(bytes.Buffer)
		b.ReadFrom(resp.Body)
		return resp.StatusCode, resp.Header.Get(contract.ProtocolHeader), b.String()
	}
	t.Run("http", func(t *testing.T) {
		js := "application/json"
		addBody := `{"id":"x","name":"coder","node":"` + idB + `","adapter":"fake","instruction":"/a","runbook":"/b","model":"m","effort":"low","concurrency":1}`
		for _, c := range []struct {
			method, path, version, ctype, body string
			extra                              map[string]string
			status                             int
			code                               contract.Code
		}{
			{"GET", contract.PathRoles, "", "", "", nil, 400, contract.CodeInvalidArgument},
			{"GET", contract.PathRoles, "x", "", "", nil, 400, contract.CodeInvalidArgument},
			{"GET", contract.PathRoles, "4", "", "", nil, 409, contract.CodeProtocolMismatch},
			{"POST", contract.PathRoles, "1", js, `SECRET-BODY`, nil, 409, contract.CodeProtocolMismatch},
			{"GET", contract.PathRoles, "3", "", "", map[string]string{contract.ProtocolHeader: "3"}, 400, contract.CodeInvalidArgument},
			{"GET", contract.PathRoles + "?all=SECRET-QUERY", "3", "", "", nil, 400, contract.CodeInvalidArgument},
			{"GET", contract.PathRoles + "?", "3", "", "", nil, 400, contract.CodeInvalidArgument},
			{"GET", contract.PathRoles + "/worker-a?x", "3", "", "", nil, 400, contract.CodeInvalidArgument},
			{"GET", contract.PathRoles, "3", "", "SECRET-BODY", nil, 400, contract.CodeInvalidArgument},
			{"GET", contract.PathRoles + "/worker-a", "3", "", "SECRET-BODY", nil, 400, contract.CodeInvalidArgument},
			{"PUT", contract.PathRoles, "3", js, `{}`, nil, 400, contract.CodeInvalidArgument},
			{"PATCH", contract.PathRoles, "3", js, `{}`, nil, 400, contract.CodeInvalidArgument},
			{"DELETE", contract.PathRoles, "3", js, `{"force":true}`, nil, 400, contract.CodeInvalidArgument},
			{"POST", contract.PathRoles + "/worker-a", "3", js, addBody, nil, 400, contract.CodeInvalidArgument},
			{"PUT", contract.PathRoles + "/worker-a", "3", js, `{}`, nil, 400, contract.CodeInvalidArgument},
			{"GET", contract.PathRoles + "/SECRET-ID", "3", "", "", nil, 400, contract.CodeInvalidArgument},
			{"GET", contract.PathRoles + "/", "3", "", "", nil, 400, contract.CodeInvalidArgument},
			{"GET", contract.PathRoles + "/worker-a/x", "3", "", "", nil, 400, contract.CodeInvalidArgument},
			{"GET", contract.PathRoles + "/zz", "3", "", "", nil, 404, contract.CodeNotFound},
			{"POST", contract.PathRoles, "3", "", addBody, nil, 400, contract.CodeInvalidArgument},
			{"POST", contract.PathRoles, "3", "text/plain", addBody, nil, 400, contract.CodeInvalidArgument},
			{"POST", contract.PathRoles, "3", js, addBody, map[string]string{"Content-Type": js}, 400, contract.CodeInvalidArgument},
			{"POST", contract.PathRoles, "3", "application/json; charset=utf-8", strings.Replace(addBody, `"low"`, `"SECRET-EFFORT"`, 1), nil, 400, contract.CodeInvalidArgument},
			{"POST", contract.PathRoles, "3", js, `{"id":"x","extra":"SECRET-VALUE"}`, nil, 400, contract.CodeInvalidArgument},
			{"POST", contract.PathRoles, "3", js, strings.Replace(addBody, `"name":"coder"`, `"name":"coder","name":"SECRET-DUP"`, 1), nil, 400, contract.CodeInvalidArgument},
			{"POST", contract.PathRoles, "3", js, strings.Replace(addBody, `"model":"m"`, `"model":null`, 1), nil, 400, contract.CodeInvalidArgument},
			{"POST", contract.PathRoles, "3", js, addBody + ` {"SECRET":1}`, nil, 400, contract.CodeInvalidArgument},
			{"POST", contract.PathRoles, "3", js, strings.Replace(addBody, idB, "n_ffffffffffffffffffffffffffffffff", 1), nil, 404, contract.CodeNotFound},
			{"POST", contract.PathRoles, "3", js, addBody, nil, 503, contract.CodeUnavailable},
			{"POST", contract.PathRoles, "3", js, strings.Replace(addBody, `"id":"x"`, `"id":"worker-a"`, 1), nil, 409, contract.CodeConflict},
			{"PATCH", contract.PathRoles + "/worker-a", "3", js, `{}`, nil, 400, contract.CodeInvalidArgument},
			{"PATCH", contract.PathRoles + "/worker-a", "3", js, `{"node":"` + idA + `"}`, nil, 400, contract.CodeInvalidArgument},
			{"PATCH", contract.PathRoles + "/zz", "3", js, `{"name":"x"}`, nil, 404, contract.CodeNotFound},
			{"PATCH", contract.PathRoles + "/worker-a", "3", "", `{"name":"x"}`, nil, 400, contract.CodeInvalidArgument},
			{"DELETE", contract.PathRoles + "/worker-a", "3", js, ``, nil, 400, contract.CodeInvalidArgument},
			{"DELETE", contract.PathRoles + "/worker-a", "3", js, `{}`, nil, 400, contract.CodeInvalidArgument},
			{"DELETE", contract.PathRoles + "/worker-a", "3", js, `{"force":"yes"}`, nil, 400, contract.CodeInvalidArgument},
			{"DELETE", contract.PathRoles + "/zz", "3", js, `{"force":false}`, nil, 404, contract.CodeNotFound},
		} {
			status, hdr, body := do(c.method, c.path, c.version, c.ctype, c.body, c.extra)
			e, err := contract.ParseErrorBody(bytes.TrimSpace([]byte(body)))
			if status != c.status || err != nil || e.Code != c.code || strings.Contains(body, "SECRET") || hdr != "3" {
				t.Fatalf("%s %s v=%q = %d %q (%v) header %q", c.method, c.path, c.version, status, body, err, hdr)
			}
			if c.code == contract.CodeProtocolMismatch && !strings.Contains(e.Message, "local=3 remote=") {
				t.Fatalf("mismatch message %q", e.Message)
			}
		}
		// The success envelopes are compact with one final LF.
		status, _, body := do("GET", contract.PathRoles+"/worker-a", "3", "", "", nil)
		v := rp.view(t, "worker-a")
		if want, _ := contract.Encode(contract.RoleResponse{Version: 3, Role: v}); status != 200 || body != string(want)+"\n" {
			t.Fatalf("show body %q", body)
		}
		status, _, body = do("GET", contract.PathRoles, "3", "", "", nil)
		if status != 200 || !strings.HasPrefix(body, `{"version":3,"roles":[{"id":"worker-a",`) || !strings.HasSuffix(body, "}]}\n") {
			t.Fatalf("list body %q", body)
		}
		status, _, body = do("DELETE", contract.PathRoles+"/worker-a", "3", "application/json", `{"force":false}`, nil)
		if status != 200 || body != `{"version":3,"removed":"worker-a"}`+"\n" {
			t.Fatalf("rm body %d %q", status, body)
		}
		p.ackReplace("p11")
		p.heartbeat(3)
	})
	t.Run("limits", func(t *testing.T) {
		// Request bodies are bounded at 16 KiB (checked on the handler: over
		// the network net/http lingers on such a connection).
		big := `{"id":"x","name":"` + strings.Repeat("n", contract.MaxRoleRequestBytes) + `"}`
		req := httptest.NewRequest("POST", contract.PathRoles, strings.NewReader(big))
		req.Header.Set(contract.ProtocolHeader, "3")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		rp.srv(t).handleRoles(rec, req)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "larger than "+strconv.Itoa(contract.MaxRoleRequestBytes)+" bytes") {
			t.Fatalf("oversized = %d %s", rec.Code, rec.Body.String())
		}
		// A response is never truncated: one over its bound is an error.
		rec = httptest.NewRecorder()
		writeBounded(rec, 200, map[string]string{"x": strings.Repeat("y", 64)}, 32)
		if rec.Code != 500 || !strings.Contains(rec.Body.String(), "exceeds 32 bytes") {
			t.Fatalf("bounded = %d %s", rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		writeBounded(rec, 200, make(chan int), 32)
		if rec.Code != 500 {
			t.Fatalf("unencodable = %d", rec.Code)
		}
	})
}

// srv returns a role-serving node service over the plane's registries
// (read-only use on the handler).
func (rp *rolePlane) srv(t *testing.T) *nodeService {
	t.Helper()
	reg := rp.srvReg(t)
	doc, err := layout{root: rp.root}.loadRoles(roleLookup)
	if err != nil {
		t.Fatal(err)
	}
	reg.roles = newRoleRegistry(layout{root: rp.root}, testDeps(t), roleLookup, doc)
	svc := newNodeService(reg, rp.clk, discardLogger(), nil, time.Second)
	svc.roles = newRoleService(reg.roles, reg, rp.clk, discardLogger())
	return svc
}
