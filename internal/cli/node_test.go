package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/plane"
)

var nodeLeaves = [][]string{{"sidecar", "enroll"}, {"sidecar", "run"}, {"node", "ls"}, {"node", "show"}}

// servePlane runs a real plane in-process (SAN 127.0.0.1) and returns its
// URL, CA file and fingerprint.
func servePlane(t *testing.T) (url, caFile, fp string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "plane")
	logs := &syncBuffer{want: `"msg":"listening"`, seen: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- plane.Run(ctx, plane.RunOptions{StateDir: root, Bind: "127.0.0.1:0", BindSet: true, SANs: []string{"127.0.0.1"}, SANsSet: true, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("plane did not stop")
		}
	})
	select {
	case <-logs.seen:
	case err := <-done:
		t.Fatalf("plane: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("plane not ready")
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "listening" {
			url = "https://" + rec["bind"].(string)
		}
	}
	caFile = filepath.Join(root, "pki", "ca.crt")
	b, _ := os.ReadFile(caFile)
	blk, _ := pem.Decode(b)
	sum := sha256.Sum256(blk.Bytes)
	return url, caFile, "sha256:" + hex.EncodeToString(sum[:])
}

func TestNodeLeafHelp(t *testing.T) {
	home := isolate(t)
	for _, goos := range []string{"linux", "darwin"} {
		for _, leaf := range nodeLeaves {
			path := "callsheet " + strings.Join(leaf, " ")
			code, out, errOut := exec(t, goos, append(append([]string{}, leaf...), "--help")...)
			if code != 0 || errOut != "" || !strings.HasPrefix(out, "Usage: "+path+" ") || !strings.Contains(out, "\nStatus: implemented.\n") {
				t.Fatalf("%v --help = %d %q %q", leaf, code, out, errOut)
			}
			for _, args := range [][]string{append([]string{"help"}, leaf...), append(append([]string{}, leaf...), "--plane", "x", "-h")} {
				if c, o, e := exec(t, goos, args...); c != 0 || o != out || e != "" {
					t.Fatalf("%v = %d %q %q", args, c, o, e)
				}
			}
		}
		_, enroll, _ := exec(t, goos, "sidecar", "enroll", "--help")
		for _, want := range []string{"--plane URL (--ca FILE | --ca-fingerprint SHA256) [--state-dir PATH]", "callsheet sidecar run", "on the worker machine", "connection is\nnot trusted", "callsheet/sidecar"} {
			if !strings.Contains(enroll, want) {
				t.Fatalf("enroll help lacks %q:\n%s", want, enroll)
			}
		}
		_, ls, _ := exec(t, goos, "node", "ls", "--help")
		_, show, _ := exec(t, goos, "node", "show", "--help")
		if !strings.Contains(ls, "on an operator machine") || !strings.Contains(show, "Usage: callsheet node show ID --plane URL") || !strings.Contains(ls, "[--json]") {
			t.Fatalf("node help:\n%s\n%s", ls, show)
		}
		for _, g := range []string{"sidecar", "node"} {
			_, out, _ := exec(t, goos, g)
			for _, l := range []string{"enroll", "run", "ls", "show"} {
				if strings.Contains(out, "\n  "+l+" ") && !regexpLine(out, l, "[implemented]") {
					t.Fatalf("%s group: %q", g, out)
				}
			}
		}
	}
	if f := files(t, home); len(f) != 0 {
		t.Fatalf("help created %v", f)
	}
}

func TestNodeFlagErrors(t *testing.T) {
	home := isolate(t)
	d := home + "/s"
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"sidecar", "enroll", "--bogus"}, 2, "flag provided but not defined"},
		{[]string{"sidecar", "enroll", "--plane", "https://x", "--plane", "https://y", "--ca", "c"}, 2, "may be given only once"},
		{[]string{"sidecar", "enroll", "--plane=", "--ca", "c"}, 2, "--plane must not be empty"},
		{[]string{"sidecar", "enroll", "--plane", "https://x", "--ca="}, 2, "--ca must not be empty"},
		{[]string{"sidecar", "enroll", "--plane", "https://x", "--ca-fingerprint="}, 2, "--ca-fingerprint must not be empty"},
		{[]string{"sidecar", "enroll", "--plane", "https://x", "--ca", "c", "--state-dir="}, 2, "--state-dir must not be empty"},
		{[]string{"sidecar", "enroll", "operand", "--plane", "https://x", "--ca", "c"}, 2, `unexpected argument "operand"`},
		{[]string{"sidecar", "enroll", "--ca", "c"}, 2, "--plane is required"},
		{[]string{"sidecar", "enroll", "--plane", "http://x", "--ca", "c"}, 2, "must be https"},
		{[]string{"sidecar", "enroll", "--plane", "https://x/path", "--ca", "c"}, 2, "paths are not allowed"},
		{[]string{"sidecar", "enroll", "--plane", "https://x", "--ca", "c", "--ca-fingerprint", "sha256:00"}, 2, "only one of --ca and --ca-fingerprint"},
		{[]string{"sidecar", "enroll", "--plane", "https://x", "--ca-fingerprint", "sha256:00"}, 2, "invalid --ca-fingerprint"},
		{[]string{"sidecar", "enroll", "--plane", "https://127.0.0.1:1", "--state-dir", d}, 6, "connection not trusted: supply --ca or --ca-fingerprint"},
		{[]string{"sidecar", "run", "--plane", "https://x"}, 2, "flag provided but not defined: -plane"},
		{[]string{"sidecar", "run", "extra"}, 2, `unexpected argument "extra"`},
		{[]string{"sidecar", "run", "--state-dir", d}, 3, "run callsheet sidecar enroll"},
		{[]string{"node", "ls", "extra", "--plane", "https://x", "--ca", "c"}, 2, `unexpected argument "extra"`},
		{[]string{"node", "ls", "--plane", "https://x", "--ca", "c", "--json", "--json"}, 2, "may be given only once"},
		{[]string{"node", "ls", "--plane", "https://x", "--ca", "c", "--json=false"}, 2, "takes no value"},
		{[]string{"node", "ls", "--state-dir", d, "--plane", "https://x"}, 2, "flag provided but not defined: -state-dir"},
		{[]string{"node", "ls", "--ca", "c"}, 2, "--plane is required"},
		{[]string{"node", "ls", "--plane", "https://127.0.0.1:1"}, 6, "connection not trusted: supply --ca or --ca-fingerprint"},
		{[]string{"node", "show", "--plane", "https://x", "--ca", "c"}, 2, "exactly one node ID"},
		{[]string{"node", "show", "a", "b", "--plane", "https://x", "--ca", "c"}, 2, `unexpected argument "b"`},
		{[]string{"node", "show", "bad-id", "--plane", "https://x", "--ca", "c"}, 2, "invalid node ID"},
		{[]string{"node", "show", "--plane", "https://x", "n_1", "--ca", "c"}, 2, "invalid node ID"},
	} {
		for _, goos := range []string{"linux", "darwin"} {
			code, out, errOut := exec(t, goos, c.args...)
			if code != c.code || out != "" || !strings.Contains(errOut, c.msg) || !strings.HasPrefix(errOut, "callsheet: ") {
				t.Fatalf("%s %v = %d %q %q", goos, c.args, code, out, errOut)
			}
		}
	}
	if f := files(t, home); len(f) != 0 {
		t.Fatalf("invalid commands created %v", f)
	}
}

func TestSidecarEnrollAndRunCommands(t *testing.T) {
	home := isolate(t)
	url, caFile, fp := servePlane(t)
	root := filepath.Join(t.TempDir(), "sidecar")
	code, out, errOut := exec(t, "linux", "sidecar", "enroll", "--plane", url+"/", "--ca", caFile, "--state-dir", root)
	lines := strings.Split(out, "\n")
	if code != 0 || errOut != "" || len(lines) != 4 || !strings.HasPrefix(lines[0], "node_id: n_") || lines[1] != "plane_url: "+url || lines[2] != "ca_fingerprint: "+fp || lines[3] != "" {
		t.Fatalf("enroll = %d %q %q", code, out, errOut)
	}
	// The pin path and the default state directory (HOME) on darwin.
	code, out2, errOut := exec(t, "darwin", "sidecar", "enroll", "--plane", url, "--ca-fingerprint", fp)
	if code != 0 || errOut != "" {
		t.Fatalf("pin enroll = %d %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "Application Support", "callsheet", "sidecar", "identity.json")); err != nil || out2 == out {
		t.Fatalf("default darwin root: %v", err)
	}
	// A failed stdout after durable success is internal and says so.
	var e bytes.Buffer
	c := NewTree("linux").Child("sidecar").Child("enroll")
	if code := sidecarEnroll(context.Background(), "linux", c, []string{"--plane", url, "--ca", caFile, "--state-dir", root}, failWriter{}, &e); code != 1 || !strings.Contains(e.String(), "the enrollment may already have completed") {
		t.Fatalf("stdout failure = %d %q", code, e.String())
	}
	// Run connects, logs JSON on stderr, keeps stdout empty and exits 130
	// on cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	logs := &syncBuffer{want: `"msg":"heartbeat acknowledged"`, seen: make(chan struct{})}
	var stdout bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, NewTree("linux"), "linux", []string{"sidecar", "run", "--state-dir", root}, &stdout, logs)
	}()
	select {
	case <-logs.seen:
	case code := <-done:
		cancel()
		t.Fatalf("run exited %d: %s", code, logs.String())
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatalf("no acknowledgement: %s", logs.String())
	}
	// While running, the node is online and a second run contends.
	_, ls, _ := exec(t, "linux", "node", "ls", "--plane", url, "--ca", caFile)
	if !strings.Contains(ls, "\tonline\t") {
		t.Fatalf("ls while running = %q", ls)
	}
	code, _, errOut = exec(t, "linux", "sidecar", "run", "--state-dir", root)
	if code != 4 || !strings.Contains(errOut, "in use") {
		t.Fatalf("second run = %d %q", code, errOut)
	}
	cancel()
	select {
	case code := <-done:
		if code != 130 || stdout.Len() != 0 || !strings.HasSuffix(logs.String(), "callsheet: interrupted\n") || !strings.Contains(logs.String(), `"component":"sidecar"`) {
			t.Fatalf("run = %d %q %q", code, stdout.String(), logs.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not stop")
	}
}

func TestNodeCommands(t *testing.T) {
	isolate(t)
	url, caFile, fp := servePlane(t)
	// Empty roster: the header only; JSON is the envelope.
	code, out, errOut := exec(t, "linux", "node", "ls", "--plane", url, "--ca", caFile)
	if code != 0 || errOut != "" || out != "ID\tLIVENESS\tLAST_SEEN\tPROTOCOL_VERSION\tSOFTWARE_VERSION\tROLES\n" {
		t.Fatalf("empty ls = %d %q %q", code, out, errOut)
	}
	if _, out, _ := exec(t, "linux", "node", "ls", "--json", "--plane", url, "--ca-fingerprint", fp); out != `{"version":1,"nodes":[]}`+"\n" {
		t.Fatalf("empty json = %q", out)
	}
	var ids []string
	for range 2 {
		root := filepath.Join(t.TempDir(), "s")
		_, out, _ := exec(t, "linux", "sidecar", "enroll", "--plane", url, "--ca", caFile, "--state-dir", root)
		ids = append(ids, strings.TrimPrefix(strings.Split(out, "\n")[0], "node_id: "))
	}
	if ids[0] > ids[1] {
		ids[0], ids[1] = ids[1], ids[0]
	}
	row := func(id string) string { return id + "\toffline\t-\t-\t-\t[]\n" }
	_, out, _ = exec(t, "linux", "node", "ls", "--plane", url, "--ca", caFile)
	if out != "ID\tLIVENESS\tLAST_SEEN\tPROTOCOL_VERSION\tSOFTWARE_VERSION\tROLES\n"+row(ids[0])+row(ids[1]) {
		t.Fatalf("sorted ls = %q", out)
	}
	_, out, _ = exec(t, "linux", "node", "ls", "--plane", url, "--ca", caFile, "--json")
	node := func(id string) string {
		return `{"id":"` + id + `","liveness":"offline","last_seen":null,"protocol_version":null,"software_version":null,"roles":[]}`
	}
	if out != `{"version":1,"nodes":[`+node(ids[0])+`,`+node(ids[1])+`]}`+"\n" {
		t.Fatalf("json ls = %q", out)
	}
	// Flags may surround the operand.
	for _, args := range [][]string{
		{"node", "show", ids[1], "--plane", url, "--ca", caFile},
		{"node", "show", "--plane", url, ids[1], "--ca", caFile},
		{"node", "show", "--plane", url, "--ca", caFile, ids[1]},
	} {
		code, out, errOut := exec(t, "linux", args...)
		if code != 0 || errOut != "" || out != "id: "+ids[1]+"\nliveness: offline\nlast_seen: -\nprotocol_version: -\nsoftware_version: -\nroles: []\n" {
			t.Fatalf("%v = %d %q %q", args, code, out, errOut)
		}
	}
	if _, out, _ := exec(t, "linux", "node", "show", ids[0], "--json", "--plane", url, "--ca", caFile); out != `{"version":1,"node":`+node(ids[0])+`}`+"\n" {
		t.Fatalf("json show = %q", out)
	}
	// Errors: exact codes, stderr only, nothing partial on stdout.
	other := filepath.Join(t.TempDir(), "other.crt")
	os.WriteFile(other, []byte(otherCA(t)), 0o644)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	gone := "https://" + ln.Addr().String()
	ln.Close()
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"node", "show", "n_ffffffffffffffffffffffffffffffff", "--plane", url, "--ca", caFile}, 3, "callsheet: not_found: "},
		{[]string{"node", "ls", "--plane", url, "--ca", other}, 6, "callsheet: trust_failed: connection not trusted"},
		{[]string{"node", "ls", "--plane", url, "--ca-fingerprint", "sha256:" + strings.Repeat("0", 64)}, 6, "does not match --ca-fingerprint"},
		{[]string{"node", "ls", "--plane", gone, "--ca", caFile}, 5, "callsheet: unavailable: cannot reach the plane"},
		{[]string{"node", "show", ids[0], "--plane", gone, "--ca", caFile}, 5, "callsheet: unavailable: "},
	} {
		code, out, errOut := exec(t, "linux", c.args...)
		if code != c.code || out != "" || !strings.Contains(errOut, c.msg) || strings.Count(errOut, "\n") != 1 {
			t.Fatalf("%v = %d %q %q", c.args, code, out, errOut)
		}
	}
	// A stdout failure is internal; nothing was partially written.
	var e bytes.Buffer
	c := NewTree("linux").Child("node").Child("ls")
	if code := nodeLs(context.Background(), "linux", c, []string{"--plane", url, "--ca", caFile}, failWriter{}, &e); code != 1 || !strings.Contains(e.String(), "cannot write to stdout") {
		t.Fatalf("stdout failure = %d %q", code, e.String())
	}
	e.Reset()
	c = NewTree("linux").Child("node").Child("show")
	if code := nodeShow(context.Background(), "linux", c, []string{ids[0], "--json", "--plane", url, "--ca", caFile}, failWriter{}, &e); code != 1 {
		t.Fatalf("show stdout failure = %d %q", code, e.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := nodeLs(ctx, "linux", c, []string{"--plane", url, "--ca", caFile}, &e, &e); code != 130 {
		t.Fatalf("canceled ls = %d", code)
	}
	if code := nodeShow(ctx, "linux", c, []string{ids[0], "--plane", url, "--ca", caFile}, &e, &e); code != 130 {
		t.Fatalf("canceled show = %d", code)
	}
}

// otherCA returns a fresh plane's CA PEM (a valid CA that is not the
// served plane's).
func otherCA(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "other")
	if _, err := plane.Init(context.Background(), plane.InitOptions{StateDir: root, SANs: []string{"x"}, SANsSet: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "pki", "ca.crt"))
	return string(b)
}

func TestRenderNodes(t *testing.T) {
	seen := time.Date(2026, 9, 26, 12, 0, 0, 123000000, time.FixedZone("x", 3600))
	pv, sv := 1, "v1.2.3"
	online := contract.Node{ID: "n_0000000000000000000000000000000a", Liveness: contract.LivenessOnline, LastSeen: &seen, ProtocolVersion: &pv, SoftwareVersion: &sv}
	offline := contract.Node{ID: "n_0000000000000000000000000000000b", Liveness: contract.LivenessOffline, Roles: []contract.RoleStatus{}}
	want := "ID\tLIVENESS\tLAST_SEEN\tPROTOCOL_VERSION\tSOFTWARE_VERSION\tROLES\n" +
		"n_0000000000000000000000000000000a\tonline\t2026-09-26T11:00:00.123Z\t1\tv1.2.3\t[]\n" +
		"n_0000000000000000000000000000000b\toffline\t-\t-\t-\t[]\n"
	if got := RenderNodes([]contract.Node{online, offline}); got != want {
		t.Fatalf("rows\n%q\nwant\n%q", got, want)
	}
	if got := RenderNode(online); got != "id: n_0000000000000000000000000000000a\nliveness: online\nlast_seen: 2026-09-26T11:00:00.123Z\nprotocol_version: 1\nsoftware_version: v1.2.3\nroles: []\n" {
		t.Fatalf("show %q", got)
	}
	withRole := offline
	withRole.Roles = []contract.RoleStatus{{RoleID: "r", Concurrency: 1, CanAccept: true}}
	if got := RenderNode(withRole); !strings.HasSuffix(got, `roles: [{"role_id":"r","inflight":0,"concurrency":1,"can_accept":true}]`+"\n") {
		t.Fatalf("roles %q", got)
	}
	if b := (&boolFlag{}).String(); b != "false" {
		t.Fatal(b)
	}
}
