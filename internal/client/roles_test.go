package client

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

func roleCfg(id, name string) contract.RoleConfig {
	return contract.RoleConfig{ID: id, Name: name, Node: nodeA, Adapter: "fake", Instruction: "/srv/i.md", Runbook: "/srv/r.md",
		Model: "example model", Effort: "medium", Concurrency: 2}
}

func viewJSON(t *testing.T, c contract.RoleConfig, order int) string {
	t.Helper()
	b, err := json.Marshal(contract.RoleView{RoleRecord: contract.RoleRecord{RoleConfig: c.Resolved(), RegistrationOrder: order}, NodeLiveness: contract.LivenessOnline, AdapterTestOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRoleAPIContract is the client half of UT FP-7: the five typed
// operations send exactly their method, path, header, content type and
// body, and decode responses strictly (status, version, schema, expected
// ID, order, uniqueness and the fake adapter's metadata), failing whole
// rather than returning partial results.
func TestRoleAPIContract(t *testing.T) {
	ca := newTestCA(t)
	type req struct {
		method, path, ctype, version, body string
	}
	var mu sync.Mutex
	var last req
	routes := map[string]http.HandlerFunc{}
	s := startServer(t, ca.leaf(t, false), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = req{r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get(contract.ProtocolHeader), string(b)}
		mu.Unlock()
		planeHandler(ca.pem, routes).ServeHTTP(w, r)
	}))
	c, err := New(s.url, Trust{CAPEM: ca.pem})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sent := func() req {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	a := roleCfg("worker-a", "coder")
	one := contract.PathRoles + "/worker-a"
	t.Run("operations", func(t *testing.T) {
		routes[contract.PathRoles] = func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				jsonRoute("6", 201, `{"version":6,"role":`+viewJSON(t, a, 1)+`}`)(w, r)
				return
			}
			jsonRoute("6", 200, `{"version":6,"roles":[`+viewJSON(t, a, 1)+`,`+viewJSON(t, roleCfg("worker-b", "reviewer"), 2)+`]}`)(w, r)
		}
		v, err := c.AddRole(bg, a)
		if got := sent(); err != nil || v.ID != "worker-a" || v.Timeout != 2*time.Hour || got.method != "POST" || got.path != contract.PathRoles ||
			got.ctype != "application/json" || got.version != "6" || got.body != `{"id":"worker-a","name":"coder","node":"`+nodeA+`","adapter":"fake","instruction":"/srv/i.md","runbook":"/srv/r.md","model":"example model","effort":"medium","concurrency":2}` {
			t.Fatalf("add = %+v %v %+v", v, err, got)
		}
		list, err := c.ListRoles(bg)
		if got := sent(); err != nil || len(list) != 2 || got.method != "GET" || got.body != "" || got.ctype != "" {
			t.Fatalf("list = %+v %v %+v", list, err, got)
		}
		routes[one] = func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodDelete:
				jsonRoute("6", 200, `{"version":6,"removed":"worker-a"}`)(w, r)
			default:
				jsonRoute("6", 200, `{"version":6,"role":`+viewJSON(t, a, 1)+`}`)(w, r)
			}
		}
		if v, err := c.ShowRole(bg, "worker-a"); err != nil || v.ID != "worker-a" || sent().method != "GET" {
			t.Fatalf("show = %+v %v", v, err)
		}
		conc, zero := 3, time.Duration(0)
		if _, err := c.SetRole(bg, "worker-a", contract.RolePatch{Concurrency: &conc, Timeout: &zero}); err != nil || sent().body != `{"concurrency":3,"timeout":"0s"}` || sent().method != "PATCH" {
			t.Fatalf("set = %v %+v", err, sent())
		}
		for _, force := range []bool{false, true} {
			if err := rmErr(c.RemoveRole(bg, "worker-a", force, "")); err != nil || sent().method != "DELETE" || sent().body != `{"force":`+map[bool]string{false: "false", true: "true"}[force]+`}` {
				t.Fatalf("rm force=%v = %v %+v", force, err, sent())
			}
		}
	})
	t.Run("local-validation", func(t *testing.T) {
		// Invalid inputs fail before any request.
		before := s.hits.Load()
		bad := a
		bad.Effort = "max"
		if _, err := c.AddRole(bg, bad); !contract.IsCode(err, contract.CodeInvalidArgument) {
			t.Fatalf("bad effort = %v", err)
		}
		if _, err := c.ShowRole(bg, "BAD"); !contract.IsCode(err, contract.CodeInvalidArgument) {
			t.Fatal("bad show ID")
		}
		if _, err := c.SetRole(bg, "BAD", contract.RolePatch{}); !contract.IsCode(err, contract.CodeInvalidArgument) {
			t.Fatal("bad set ID")
		}
		if _, err := c.SetRole(bg, "ok", contract.RolePatch{}); !contract.IsCode(err, contract.CodeInvalidArgument) {
			t.Fatal("empty patch")
		}
		if err := rmErr(c.RemoveRole(bg, "BAD", false, "")); !contract.IsCode(err, contract.CodeInvalidArgument) {
			t.Fatal("bad rm ID")
		}
		if s.hits.Load() != before {
			t.Fatal("an invalid input reached the plane")
		}
	})
	t.Run("responses", func(t *testing.T) {
		b := roleCfg("worker-b", "reviewer")
		other := roleCfg("worker-z", "coder")
		for name, c2 := range map[string]struct {
			route http.HandlerFunc
			call  func() error
			code  contract.Code
			want  string
		}{
			"add other id": {jsonRoute("6", 201, `{"version":6,"role":`+viewJSON(t, other, 1)+`}`), func() error { _, err := c.AddRole(bg, a); return err }, contract.CodeInvalidArgument, "another role"},
			"add status":   {jsonRoute("6", 200, `{"version":6,"role":`+viewJSON(t, a, 1)+`}`), func() error { _, err := c.AddRole(bg, a); return err }, contract.CodeInvalidArgument, "unexpected status"},
			"add error":    {jsonRoute("6", 409, `{"error":{"code":"conflict","message":"role worker-a already exists"}}`), func() error { _, err := c.AddRole(bg, a); return err }, contract.CodeConflict, "already exists"},
			"add busy":     {jsonRoute("6", 503, `{"error":{"code":"unavailable","message":"busy","details":{"reason":"busy"}}}`), func() error { _, err := c.AddRole(bg, a); return err }, contract.CodeUnavailable, "busy"},
			"add version":  {jsonRoute("7", 201, `{}`), func() error { _, err := c.AddRole(bg, a); return err }, contract.CodeProtocolMismatch, "local=6 remote=7"},
			"add bad body": {jsonRoute("6", 201, `{"version":6,"role":{}}`), func() error { _, err := c.AddRole(bg, a); return err }, contract.CodeInvalidArgument, "required field"},
			"add big":      {jsonRoute("6", 201, `{"version":6,"role":"`+strings.Repeat("x", contract.MaxRoleResponseBytes)+`"}`), func() error { _, err := c.AddRole(bg, a); return err }, contract.CodeInvalidArgument, "larger than"},
			"list order":   {jsonRoute("6", 200, `{"version":6,"roles":[`+viewJSON(t, b, 2)+`,`+viewJSON(t, a, 1)+`]}`), func() error { _, err := c.ListRoles(bg); return err }, contract.CodeInvalidArgument, "not sorted"},
			"list dup":     {jsonRoute("6", 200, `{"version":6,"roles":[`+viewJSON(t, a, 1)+`,`+viewJSON(t, a, 1)+`]}`), func() error { _, err := c.ListRoles(bg); return err }, contract.CodeInvalidArgument, "repeats"},
			"list inflight": {jsonRoute("6", 200, `{"version":6,"roles":[`+strings.Replace(viewJSON(t, a, 1), `"inflight":0`, `"inflight":-1`, 1)+`]}`), func() error { _, err := c.ListRoles(bg); return err },
				contract.CodeInvalidArgument, "inflight must be an integer from 0"},
			"list test only": {jsonRoute("6", 200, `{"version":6,"roles":[`+strings.Replace(viewJSON(t, a, 1), `"adapter_test_only":true`, `"adapter_test_only":false`, 1)+`]}`), func() error { _, err := c.ListRoles(bg); return err },
				contract.CodeInvalidArgument, "adapter_test_only"},
			"list unknown adapter": {jsonRoute("6", 200, `{"version":6,"roles":[`+strings.Replace(viewJSON(t, a, 1), `"adapter":"fake"`, `"adapter":"nosuch-adapter"`, 1)+`]}`), func() error { _, err := c.ListRoles(bg); return err },
				contract.CodeInvalidArgument, "unknown adapter"},
			"list float": {jsonRoute("6", 200, `{"version":6,"roles":[`+strings.Replace(viewJSON(t, a, 1), `"concurrency":2`, `"concurrency":2.0`, 1)+`]}`), func() error { _, err := c.ListRoles(bg); return err },
				contract.CodeInvalidArgument, "integer"},
			"list missing": {jsonRoute("6", 200, `{"version":6}`), func() error { _, err := c.ListRoles(bg); return err }, contract.CodeInvalidArgument, "required field"},
			"list status":  {jsonRoute("6", 201, `{"version":6,"roles":[]}`), func() error { _, err := c.ListRoles(bg); return err }, contract.CodeInvalidArgument, "unexpected status"},
			"show other":   {jsonRoute("6", 200, `{"version":6,"role":`+viewJSON(t, other, 1)+`}`), func() error { _, err := c.ShowRole(bg, "worker-a"); return err }, contract.CodeInvalidArgument, "another role"},
			"show missing": {jsonRoute("6", 404, `{"error":{"code":"not_found","message":"role worker-a does not exist"}}`), func() error { _, err := c.ShowRole(bg, "worker-a"); return err }, contract.CodeNotFound, "does not exist"},
			"show status":  {jsonRoute("6", 204, ``), func() error { _, err := c.ShowRole(bg, "worker-a"); return err }, contract.CodeInvalidArgument, "unexpected status"},
			"set other":    {jsonRoute("6", 200, `{"version":6,"role":`+viewJSON(t, other, 1)+`}`), func() error { n := "x"; _, err := c.SetRole(bg, "worker-a", contract.RolePatch{Name: &n}); return err }, contract.CodeInvalidArgument, "another role"},
			"set status":   {jsonRoute("6", 201, `{"version":6,"role":`+viewJSON(t, a, 1)+`}`), func() error { n := "x"; _, err := c.SetRole(bg, "worker-a", contract.RolePatch{Name: &n}); return err }, contract.CodeInvalidArgument, "unexpected status"},
			"rm other":     {jsonRoute("6", 200, `{"version":6,"removed":"worker-z"}`), func() error { return rmErr(c.RemoveRole(bg, "worker-a", false, "")) }, contract.CodeInvalidArgument, "another role"},
			"rm shape":     {jsonRoute("6", 200, `{"version":6,"removed":"worker-a","x":1}`), func() error { return rmErr(c.RemoveRole(bg, "worker-a", false, "")) }, contract.CodeInvalidArgument, "unknown field"},
			"rm status":    {jsonRoute("6", 202, `{"version":6,"removed":"worker-a"}`), func() error { return rmErr(c.RemoveRole(bg, "worker-a", false, "")) }, contract.CodeInvalidArgument, "unexpected status"},
		} {
			routes[contract.PathRoles] = c2.route
			routes[one] = c2.route
			err := c2.call()
			if contract.CodeOf(err) != c2.code || !strings.Contains(err.Error(), c2.want) {
				t.Errorf("%s: %v, want %s containing %q", name, err, c2.code, c2.want)
			}
		}
	})
}

// rmErr keeps a removal's error only.
func rmErr(_ contract.RoleRemoveResult, err error) error { return err }
