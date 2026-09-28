package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

const roleNode = "n_0123456789abcdef0123456789abcdef"

// stubPlane is an HTTPS role API stub trusted through a fixture CA: it
// records every request and answers from a canned route.
type stubPlane struct {
	url, caFile, pin string
	mu               sync.Mutex
	reqs             []stubReq
	route            func(r stubReq) (status int, version, body string)
}

type stubReq struct {
	method, path, ctype, body string
	query                     string
}

func startStubPlane(t *testing.T) *stubPlane {
	t.Helper()
	ca, err := testkit.NewFixtureCA()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.Leaf(nil, []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	// Like the plane, serve leaf then CA, so pinned bootstrap finds the CA.
	caBlock, _ := pem.Decode(ca.CertPEM)
	cert.Certificate = append(cert.Certificate[:1:1], caBlock.Bytes)
	s := &stubPlane{caFile: filepath.Join(t.TempDir(), "ca.crt")}
	os.WriteFile(s.caFile, ca.CertPEM, 0o644)
	blk, _ := pem.Decode(ca.CertPEM)
	sum := sha256.Sum256(blk.Bytes)
	s.pin = "sha256:" + hex.EncodeToString(sum[:])
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.url = "https://" + ln.Addr().String()
	srv := &http.Server{ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == contract.PathCA {
			w.Write(ca.CertPEM)
			return
		}
		b, _ := io.ReadAll(r.Body)
		req := stubReq{r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(b), r.URL.RawQuery}
		s.mu.Lock()
		s.reqs = append(s.reqs, req)
		route := s.route
		s.mu.Unlock()
		status, version, body := route(req)
		w.Header().Set(contract.ProtocolHeader, version)
		w.WriteHeader(status)
		io.WriteString(w, body)
	})}
	done := make(chan struct{})
	go func() {
		srv.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}}))
		close(done)
	}()
	t.Cleanup(func() { srv.Close(); <-done })
	return s
}

func (s *stubPlane) answer(status int, body string) {
	s.mu.Lock()
	s.route = func(stubReq) (int, string, string) { return status, "4", body }
	s.mu.Unlock()
}

func (s *stubPlane) last() stubReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		return stubReq{}
	}
	return s.reqs[len(s.reqs)-1]
}

func (s *stubPlane) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func stubView(id, name string, order int, model string, ready bool) contract.RoleView {
	c := contract.RoleConfig{ID: id, Name: name, Node: roleNode, Adapter: "fake", Instruction: "/srv/manuals/" + id + "/instruction.md",
		Runbook: "/srv/manuals/" + id + "/runbook.md", Model: model, Effort: "medium", Concurrency: 2}
	return contract.RoleView{RoleRecord: contract.RoleRecord{RoleConfig: c.Resolved(), RegistrationOrder: order}, CanAccept: ready, NodeLiveness: contract.LivenessOnline, AdapterTestOnly: true}
}

func envelope(t *testing.T, v any) string {
	t.Helper()
	b, err := contract.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRoleCLI is the CLI half of UT FP-7: the five leaves' parsing, local
// validation (never opening a manual path), requests, text and JSON
// output, trust flags and error mapping, against a stub plane.
func TestRoleCLI(t *testing.T) {
	home := isolate(t)
	sp := startStubPlane(t)
	trust := []string{"--plane", sp.url, "--ca", sp.caFile}
	a := stubView("worker-a", "coder", 1, "example \"model\" <x>", false)
	addArgs := func(extra ...string) []string {
		args := []string{"role", "add", "worker-a", "--name", "coder", "--node", roleNode, "--adapter", "fake",
			"--instruction", "/srv/manuals/worker-a/instruction.md", "--runbook", "/srv/manuals/worker-a/runbook.md",
			"--model", "example \"model\" <x>", "--effort", "medium", "--concurrency", "2"}
		return append(append(args, extra...), trust...)
	}
	t.Run("add", func(t *testing.T) {
		sp.answer(201, envelope(t, contract.RoleResponse{Version: 4, Role: a}))
		code, out, errOut := exec(t, "linux", addArgs()...)
		want := "id: worker-a\nname: coder\nnode: " + roleNode + "\nadapter: fake\ninstruction: \"/srv/manuals/worker-a/instruction.md\"\n" +
			"runbook: \"/srv/manuals/worker-a/runbook.md\"\nmodel: \"example \\\"model\\\" <x>\"\neffort: medium\nconcurrency: 2\ntimeout: 2h0m0s\n" +
			"registration_order: 1\ninflight: 0\ncan_accept: false\nnode_liveness: online\nadapter_test_only: true\n"
		if code != 0 || out != want || errOut != "" {
			t.Fatalf("add = %d\n%s\n%s", code, out, errOut)
		}
		req := sp.last()
		if req.method != "POST" || req.path != contract.PathRoles || req.ctype != "application/json" ||
			req.body != `{"id":"worker-a","name":"coder","node":"`+roleNode+`","adapter":"fake","instruction":"/srv/manuals/worker-a/instruction.md","runbook":"/srv/manuals/worker-a/runbook.md","model":"example \"model\" <x>","effort":"medium","concurrency":2}` {
			t.Fatalf("request %+v", req)
		}
		// Flags before the operand, a timeout and --json.
		args := append([]string{"role", "add", "--json", "--timeout", "0"}, addArgs()[2:]...)
		code, out, _ = exec(t, "darwin", args...)
		if code != 0 || out != envelope(t, contract.RoleResponse{Version: 4, Role: a})+"\n" || !strings.Contains(sp.last().body, `"timeout":"0s"`) {
			t.Fatalf("add --json = %d %q %+v", code, out, sp.last())
		}
	})
	t.Run("add-errors", func(t *testing.T) {
		before := sp.count()
		// Manual paths are never opened locally: a FIFO path would block.
		fifo := filepath.Join(home, "fifo")
		syscall.Mkfifo(fifo, 0o644)
		for _, c := range []struct {
			args []string
			code int
			msg  string
			help bool
		}{
			{[]string{"role", "add", "--name", "x"}, 2, "takes exactly one role ID", true},
			{[]string{"role", "add", "a", "b", "--name", "x"}, 2, `unexpected argument "b"`, true},
			{[]string{"role", "add", "a", "--name", "x", "--name", "y"}, 2, "may be given only once", true},
			{[]string{"role", "add", "a", "--force"}, 2, "flag provided but not defined", true},
			{[]string{"role", "add", "BAD", "--name", "x"}, 2, "invalid role ID", false},
			{[]string{"role", "add", "a", "--name", "x", "--node", roleNode}, 2, "--adapter is required", false},
			{[]string{"role", "add", "a", "--name", "x", "--node", roleNode, "--adapter", "fake", "--instruction", "/i", "--runbook", "/r", "--effort", "low", "--concurrency", "1"}, 2, "--model is required", false},
			{[]string{"role", "add", "a", "--name", "x", "--node", roleNode, "--adapter", "fake", "--instruction", "/i", "--runbook", "/r", "--model", "m", "--concurrency", "1"}, 2, "--effort is required", false},
			{[]string{"role", "add", "a", "--name", "x", "--node", roleNode, "--adapter", "fake", "--runbook", "/r", "--model", "m", "--effort", "low", "--concurrency", "1"}, 2, "--instruction is required", false},
		} {
			code, out, errOut := exec(t, "linux", append(c.args, trust...)...)
			if code != c.code || out != "" || !strings.Contains(errOut, c.msg) || strings.Contains(errOut, "Usage:") != c.help {
				t.Fatalf("%v = %d %q %q", c.args, code, out, errOut)
			}
		}
		// A flag missing its value (last on the line).
		if code, _, errOut := exec(t, "linux", append(append([]string{"role", "add", "a"}, trust...), "--name")...); code != 2 || !strings.Contains(errOut, "flag needs an argument") {
			t.Fatalf("missing value = %d %q", code, errOut)
		}
		base := addArgs()
		with := func(extra ...string) []string { return append(append([]string(nil), base...), extra...) }
		mut := func(flag, value string) []string {
			out := append([]string(nil), base...)
			for i := range out {
				if out[i] == flag {
					out[i+1] = value
				}
			}
			return out
		}
		for _, c := range []struct {
			args []string
			msg  string
			help bool
		}{
			{mut("--concurrency", "two"), "--concurrency must be an integer", true},
			{mut("--concurrency", "1.5"), "--concurrency must be an integer", true},
			{mut("--concurrency", "0"), "concurrency must be an integer from 1", false},
			{with("--timeout", "soon"), "--timeout must be a Go duration", true},
			{with("--timeout", ""), "--timeout must be a Go duration", true},
			{with("--timeout", "-1m"), "must not be negative", false},
			{mut("--effort", "max"), "effort is not allowed for adapter fake", false},
			{mut("--adapter", "codex"), "unknown adapter", false},
			{mut("--model", "  "), "model must be nonblank", false},
			{mut("--instruction", "manuals/i.md"), "instruction must be an absolute", false},
			{mut("--instruction", fifo), "", false},
			{mut("--node", "n_1"), "node must be a node ID", false},
			{mut("--name", "Coder"), "name must be a 1-63 character slug", false},
		} {
			sp.answer(201, envelope(t, contract.RoleResponse{Version: 4, Role: a}))
			code, _, errOut := exec(t, "linux", c.args...)
			if c.msg == "" {
				// The FIFO path is sent as is; nothing opened it locally.
				if code != 3 && code != 2 && code != 0 {
					t.Fatalf("fifo path = %d %q", code, errOut)
				}
				before = sp.count()
				continue
			}
			if code != 2 || !strings.Contains(errOut, c.msg) || strings.Contains(errOut, "Usage:") != c.help {
				t.Fatalf("%v = %d %q", c.args, code, errOut)
			}
			if sp.count() != before {
				t.Fatalf("%v reached the plane", c.args)
			}
		}
	})
	t.Run("set", func(t *testing.T) {
		sp.answer(200, envelope(t, contract.RoleResponse{Version: 4, Role: a}))
		code, _, errOut := exec(t, "linux", append([]string{"role", "set", "worker-a", "--concurrency", "3", "--timeout", "90m", "--model", "-x"}, trust...)...)
		req := sp.last()
		if code != 0 || req.method != "PATCH" || req.path != contract.PathRoles+"/worker-a" || req.body != `{"model":"-x","concurrency":3,"timeout":"1h30m0s"}` {
			t.Fatalf("set = %d %q %+v", code, errOut, req)
		}
		before := sp.count()
		for _, c := range []struct {
			args []string
			msg  string
		}{
			{[]string{"role", "set", "worker-a"}, "at least one field flag"},
			{[]string{"role", "set", "worker-a", "--node", roleNode}, "flag provided but not defined"},
			{[]string{"role", "set", "worker-a", "--name", ""}, "name must be a 1-63 character slug"},
			{[]string{"role", "set", "worker-a", "--adapter", "codex"}, "unknown adapter"},
			{[]string{"role", "set", "worker-a", "--concurrency", "x"}, "--concurrency must be an integer"},
		} {
			if code, _, errOut := exec(t, "linux", append(c.args, trust...)...); code != 2 || !strings.Contains(errOut, c.msg) {
				t.Fatalf("%v = %d %q", c.args, code, errOut)
			}
		}
		if sp.count() != before {
			t.Fatal("an invalid set reached the plane")
		}
	})
	t.Run("ls", func(t *testing.T) {
		views := []contract.RoleView{stubView("worker-b", "coder", 2, "m1", true), stubView("worker-d", "coder", 4, "m 2", false), stubView("worker-a", "reviewer", 1, "模型", true)}
		sp.answer(200, envelope(t, contract.RoleListResponse{Version: 4, Roles: views}))
		code, out, _ := exec(t, "linux", append([]string{"role", "ls"}, trust...)...)
		want := "NAME\tID\tNODE\tNODE_LIVENESS\tADAPTER\tMODEL\tEFFORT\tINFLIGHT\tCONCURRENCY\tCAN_ACCEPT\n" +
			"coder\tworker-b\t" + roleNode + "\tonline\tfake\t\"m1\"\tmedium\t0\t2\ttrue\n" +
			"coder\tworker-d\t" + roleNode + "\tonline\tfake\t\"m 2\"\tmedium\t0\t2\tfalse\n" +
			"reviewer\tworker-a\t" + roleNode + "\tonline\tfake\t\"模型\"\tmedium\t0\t2\ttrue\n"
		if code != 0 || out != want {
			t.Fatalf("ls = %d\n%s", code, out)
		}
		code, out, _ = exec(t, "linux", append([]string{"role", "ls", "--json"}, trust...)...)
		if code != 0 || out != envelope(t, contract.RoleListResponse{Version: 4, Roles: views})+"\n" || sp.last().method != "GET" || sp.last().body != "" {
			t.Fatalf("ls --json = %d %q", code, out)
		}
		sp.answer(200, `{"version":4,"roles":[]}`)
		if code, out, _ := exec(t, "linux", append([]string{"role", "ls"}, trust...)...); code != 0 || out != strings.Join(RoleColumns, "\t")+"\n" {
			t.Fatalf("empty ls = %q", out)
		}
		if code, _, errOut := exec(t, "linux", append([]string{"role", "ls", "extra"}, trust...)...); code != 2 || !strings.Contains(errOut, "unexpected argument") {
			t.Fatalf("ls operand = %d %q", code, errOut)
		}
	})
	t.Run("show-rm", func(t *testing.T) {
		sp.answer(200, envelope(t, contract.RoleResponse{Version: 4, Role: a}))
		if code, out, _ := exec(t, "linux", append([]string{"role", "show", "worker-a", "--json"}, trust...)...); code != 0 || out != envelope(t, contract.RoleResponse{Version: 4, Role: a})+"\n" {
			t.Fatalf("show = %d %q", code, out)
		}
		sp.answer(200, `{"version":4,"removed":"worker-a"}`)
		for _, force := range []bool{false, true} {
			args := []string{"role", "rm", "worker-a"}
			if force {
				args = append(args, "--force")
			}
			code, out, _ := exec(t, "linux", append(args, trust...)...)
			if code != 0 || out != "removed: worker-a\n"+roleRmNotice || sp.last().method != "DELETE" || sp.last().body != `{"force":`+map[bool]string{false: "false", true: "true"}[force]+`}` {
				t.Fatalf("rm force=%v = %d %q %+v", force, code, out, sp.last())
			}
		}
		if code, out, _ := exec(t, "linux", append([]string{"role", "rm", "--json", "worker-a"}, trust...)...); code != 0 || out != `{"version":4,"removed":"worker-a"}`+"\n" {
			t.Fatalf("rm --json = %d %q", code, out)
		}
		if code, _, errOut := exec(t, "linux", append([]string{"role", "rm", "worker-a", "--force", "--force"}, trust...)...); code != 2 || !strings.Contains(errOut, "only once") {
			t.Fatalf("double force = %d %q", code, errOut)
		}
		if code, _, errOut := exec(t, "linux", append([]string{"role", "show", "worker-a", "--force"}, trust...)...); code != 2 || !strings.Contains(errOut, "not defined") {
			t.Fatalf("show --force = %d %q", code, errOut)
		}
	})
	t.Run("trust", func(t *testing.T) {
		sp.answer(200, envelope(t, contract.RoleResponse{Version: 4, Role: a}))
		if code, _, errOut := exec(t, "linux", "role", "show", "worker-a", "--plane", sp.url, "--ca-fingerprint", sp.pin); code != 0 {
			t.Fatalf("pin = %d %q", code, errOut)
		}
		wrong := "sha256:" + strings.Repeat("0", 64)
		for _, c := range []struct {
			args []string
			code int
			msg  string
		}{
			{[]string{"role", "show", "worker-a", "--plane", sp.url, "--ca-fingerprint", wrong}, 6, "connection not trusted"},
			{[]string{"role", "show", "worker-a", "--plane", sp.url}, 6, "connection not trusted"},
			{[]string{"role", "ls", "--plane", sp.url, "--ca", sp.caFile, "--ca-fingerprint", sp.pin}, 2, "only one of --ca and --ca-fingerprint"},
			{[]string{"role", "ls", "--ca", sp.caFile}, 2, "--plane is required"},
			{[]string{"role", "ls", "--plane", "http://127.0.0.1:1", "--ca", sp.caFile}, 2, "must be https"},
		} {
			if code, out, errOut := exec(t, "linux", c.args...); code != c.code || out != "" || !strings.Contains(errOut, c.msg) {
				t.Fatalf("%v = %d %q %q", c.args, code, out, errOut)
			}
		}
		// Transport failure: nothing listens.
		dead := "https://" + testkit.RefusingAddr(t)
		if code, _, errOut := exec(t, "linux", "role", "ls", "--plane", dead, "--ca", sp.caFile); code != 5 || !strings.Contains(errOut, "cannot reach the plane") {
			t.Fatalf("dead plane = %d %q", code, errOut)
		}
	})
	t.Run("errors", func(t *testing.T) {
		for _, c := range []struct {
			status  int
			version string
			body    string
			code    int
			stderr  string
		}{
			{409, "4", `{"error":{"code":"conflict","message":"role worker-a already exists"}}`, 4, "callsheet: conflict: role worker-a already exists\n"},
			{404, "4", `{"error":{"code":"not_found","message":"node n_x is not enrolled"}}`, 3, "callsheet: not_found: node n_x is not enrolled\n"},
			{503, "4", `{"error":{"code":"unavailable","message":"another role change is in progress; retry","details":{"reason":"busy"}}}`, 5, "callsheet: unavailable: another role change is in progress; retry\n"},
			{400, "4", `{"error":{"code":"invalid_argument","message":"the runbook manual is not readable on this node: permission denied"}}`, 2, "callsheet: invalid_argument: the runbook manual is not readable on this node: permission denied\n"},
			{500, "4", `{"error":{"code":"internal","message":"role change was published but durability is unconfirmed"}}`, 1, "callsheet: internal: role change was published but durability is unconfirmed\n"},
			{409, "1", `{"error":{"code":"protocol_mismatch","message":"x"}}`, 7, "callsheet: protocol_mismatch: protocol version mismatch: local=4 remote=1\n"},
		} {
			sp.mu.Lock()
			sp.route = func(stubReq) (int, string, string) { return c.status, c.version, c.body }
			sp.mu.Unlock()
			code, out, errOut := exec(t, "linux", addArgs()...)
			if code != c.code || out != "" || errOut != c.stderr {
				t.Fatalf("%d = %d %q %q", c.status, code, out, errOut)
			}
		}
		// A write failure on stdout is internal (1) after a completed call.
		sp.answer(200, envelope(t, contract.RoleResponse{Version: 4, Role: a}))
		var errOut bytes.Buffer
		code := run(context.Background(), NewTree("linux"), "linux", append([]string{"role", "show", "worker-a"}, trust...), failWriter{}, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "cannot write to stdout") {
			t.Fatalf("stdout failure = %d %q", code, errOut.String())
		}
	})
	t.Run("node-roles", func(t *testing.T) {
		// Node discovery renders a nonempty roles array compactly, in the
		// order the plane supplies.
		n := contract.Node{ID: roleNode, Liveness: contract.LivenessOnline, Roles: []contract.RoleStatus{{RoleID: "worker-b", Concurrency: 2, CanAccept: true}, {RoleID: "worker-a", Concurrency: 1}}}
		sp.answer(200, envelope(t, contract.NodeResponse{Version: 4, Node: n}))
		code, out, _ := exec(t, "linux", append([]string{"node", "show", roleNode}, trust...)...)
		if code != 0 || !strings.Contains(out, `roles: [{"role_id":"worker-b","inflight":0,"concurrency":2,"can_accept":true},{"role_id":"worker-a","inflight":0,"concurrency":1,"can_accept":false}]`+"\n") {
			t.Fatalf("node show = %d\n%s", code, out)
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(envelope(t, contract.NodeResponse{Version: 4, Node: n})), &decoded) != nil {
			t.Fatal("envelope")
		}
	})
	t.Run("help", func(t *testing.T) {
		for leaf, wants := range map[string][]string{
			"add":  {"Usage: callsheet role add ID --name NAME --node NODE --adapter fake", "test/demo adapter; never calls a model", "manual contents never leave the node", "[--timeout DURATION]", "--ca-fingerprint"},
			"set":  {"Usage: callsheet role set ID [--name NAME]", "omitted fields keep their values", "node must be online"},
			"ls":   {"NAME, ID, NODE, NODE_LIVENESS, ADAPTER", "sorted by name"},
			"show": {"Usage: callsheet role show ID"},
			"rm":   {"Usage: callsheet role rm ID [--force]", "force_cancel_not_supported", "also while its node is offline", "not proof that old execution stopped"},
		} {
			_, out, _ := exec(t, "linux", "role", leaf, "--help")
			for _, w := range wants {
				if !strings.Contains(out, w) {
					t.Fatalf("role %s help lacks %q:\n%s", leaf, w, out)
				}
			}
		}
		_, sc, _ := exec(t, "linux", "sidecar", "run", "--help")
		for _, w := range []string{"[--fake-adapter PATH]", "test/demo adapter; never calls a model", "give it on every start"} {
			if !strings.Contains(sc, w) {
				t.Fatalf("sidecar run help lacks %q", w)
			}
		}
		for _, bad := range [][]string{{"sidecar", "run", "--fake-adapter", ""}, {"sidecar", "run", "--fake-adapter", "relative/fake"}} {
			if code, _, errOut := exec(t, "linux", bad...); code != 2 || !strings.Contains(errOut, "--fake-adapter must be an absolute path") || !strings.Contains(errOut, "Usage:") {
				t.Fatalf("%v = %d %q", bad, code, errOut)
			}
		}
	})
	if f := files(t, home); len(f) != 1 || filepath.Base(f[0]) != "fifo" {
		t.Fatalf("role commands wrote local state: %v", f)
	}
}
