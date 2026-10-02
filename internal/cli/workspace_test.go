package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

var wsLeaves = [][]string{{"ws", "create"}, {"ws", "ls"}, {"ws", "show"}, {"ws", "rm"}, {"ws", "prune"}, {"ws", "status"}, {"ws", "diff"}, {"ws", "ref", "set"}}

// UT-7: every workspace leaf has help, is implemented, and refuses usage
// errors (exit 2) before contacting anything (iteration 09b implements
// push and pull; their own cases are in workspace_transfer_test.go).
func TestWorkspaceLeafHelp(t *testing.T) {
	isolate(t)
	for _, leaf := range wsLeaves {
		code, out, errOut := exec(t, "linux", append(append([]string{}, leaf...), "--help")...)
		if code != 0 || errOut != "" || !strings.Contains(out, "\nStatus: implemented.\n") || !strings.Contains(out, "on the plane") {
			t.Fatalf("%v --help = %d %q", leaf, code, out)
		}
	}
	trust := []string{"--plane", "https://127.0.0.1:1", "--ca-fingerprint", "sha256:" + strings.Repeat("0", 64)}
	inst := "--instance=" + strings.Repeat("a", 32)
	for _, args := range [][]string{
		{"ws", "create"},
		{"ws", "create", "Bad"},
		{"ws", "show", "a", "b"},
		{"ws", "ls", "--limit", "0"},
		{"ws", "ls", "--after="},
		{"ws", "rm", "a"},
		{"ws", "rm", "--instance", "x", "a"},
		{"ws", "prune", inst, "a"},
		{"ws", "prune", inst, "--before", "2026-09-30", "a"},
		{"ws", "status", "--after", "refs/heads/x", "a"},
		{"ws", "diff", "a", "main"},
		{"ws", "diff", "--base", "empty", "a"},
		{"ws", "ref", "set", inst, "a", "main", "main"},
		{"ws", "ref", "set", inst, "--expected", "absent", "a", "Main", "main"},
		{"ws", "ref", "set", inst, "--expected", "absent", "a", "main"},
		{"ws", "ref", "set", inst, "--expected", "absent", "--delete", "a", "main"},
		{"ws", "ref", "set", inst, "--expected", "absent", "a", "ma\xffin", "main"},
	} {
		code, out, errOut := exec(t, "linux", append(args, trust...)...)
		if code != 2 || out != "" || !strings.HasPrefix(errOut, "callsheet: invalid_argument: ") {
			t.Fatalf("%q = %d %q %q", args, code, out, errOut)
		}
	}
}

// UT-7: the leaves against a real plane: exact JSON objects, text
// renderings, CAS and error exit codes (conflict 4, not found 3), paging
// flags, and path display with control escaping and base64 fallback.
func TestWorkspaceCLI(t *testing.T) {
	isolate(t)
	url, caFile, _ := servePlane(t)
	trust := []string{"--plane", url, "--ca", caFile}
	run := func(args ...string) (int, string, string) {
		t.Helper()
		return exec(t, "linux", append(args, trust...)...)
	}
	code, out, errOut := run("ws", "create", "--json", "alpha")
	var v contract.WorkspaceView
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v.Name != "alpha" || errOut != "" {
		t.Fatalf("create = %d %q %q", code, out, errOut)
	}
	if code, _, errOut := run("ws", "create", "alpha"); code != 4 || !strings.Contains(errOut, "conflict") {
		t.Fatalf("duplicate = %d %q", code, errOut)
	}
	if code, out, _ := run("ws", "show", "alpha"); code != 0 || !strings.Contains(out, "instance: "+v.Instance+"\n") || !strings.Contains(out, "retention: automatic_prune=false quota_bytes=null\n") {
		t.Fatalf("show = %d %q", code, out)
	}
	if code, _, _ := run("ws", "show", "nope"); code != 3 {
		t.Fatalf("missing show = %d", code)
	}
	run("ws", "create", "beta")
	if code, out, _ := run("ws", "ls", "--limit", "1"); code != 0 || !strings.HasPrefix(out, "NAME\tINSTANCE\tCREATED_AT\nalpha\t") || !strings.HasSuffix(out, "next_after: alpha\n") {
		t.Fatalf("ls = %d %q", code, out)
	}
	if code, out, _ := run("ws", "ls", "--after", "alpha", "--json"); code != 0 || !strings.Contains(out, `"name":"beta"`) || !strings.Contains(out, `"next_after":null`) {
		t.Fatalf("ls page 2 = %d %q", code, out)
	}
	// Seed commits over the plane's TLS endpoint.
	pem, _ := os.ReadFile(caFile)
	hc, err := testkit.GitHTTPClient(pem)
	if err != nil {
		t.Fatal(err)
	}
	remote := testkit.GitRemote{URL: url + "/ws/alpha.git", Instance: v.Instance, HTTP: hc}
	s := testkit.NewMemoryStore()
	files := map[string]testkit.FileSpec{"a.txt": {Mode: filemode.Regular, Content: []byte("a")}}
	c0, _ := testkit.CommitFiles(s, files, nil, "c0")
	files["esc\x1b[2J.txt"] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte("x")}
	files["raw\xff.bin"] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte("yy")}
	c1, _ := testkit.CommitFiles(s, files, []plumbing.Hash{c0}, "c1")
	if res := remote.Push(context.Background(), s, "refs/heads/main", plumbing.ZeroHash, c1); !res.OK() {
		t.Fatalf("push %+v", res)
	}
	inst := "--instance=" + v.Instance
	if code, out, _ := run("ws", "ref", "set", inst, "--expected", "absent", "alpha", "base", c0.String()); code != 0 || !strings.Contains(out, "old_commit: -\nnew_commit: "+c0.String()+"\n") {
		t.Fatalf("ref set = %d %q", code, out)
	}
	if code, _, errOut := run("ws", "ref", "set", inst, "--expected", "absent", "alpha", "base", "main"); code != 4 || !strings.Contains(errOut, "stale") {
		t.Fatalf("stale ref set = %d %q", code, errOut)
	}
	code, out, _ = run("ws", "status", "--limit", "1", "alpha")
	if code != 0 || !strings.Contains(out, "REF\tCOMMIT\tPUBLISHED_AT\nrefs/heads/base\t"+c0.String()+"\t-\n") || !strings.Contains(out, "next_after: refs/heads/base\n") {
		t.Fatalf("status = %d %q", code, out)
	}
	var gen string
	for _, l := range strings.Split(out, "\n") {
		if g, ok := strings.CutPrefix(l, "generation: "); ok {
			gen = g
		}
	}
	if code, out, _ := run("ws", "status", "--after", "refs/heads/base", "--instance", v.Instance, "--generation", gen, "--json", "alpha"); code != 0 || !strings.Contains(out, `"name":"refs/heads/main"`) {
		t.Fatalf("status page 2 = %d %q", code, out)
	}
	code, out, _ = run("ws", "diff", "--base", "base", "alpha", "main")
	if code != 0 || !strings.Contains(out, "added\t-\t100644\t-\t1\t\"esc\\x1b[2J.txt\"\n") || !strings.Contains(out, "added\t-\t100644\t-\t2\tbase64:"+base64.StdEncoding.EncodeToString([]byte("raw\xff.bin"))+"\n") ||
		strings.Contains(out, "\x1b") {
		t.Fatalf("diff = %d %q", code, out)
	}
	if code, out, _ := run("ws", "diff", "--base", "empty", "--json", "alpha", c0.String()); code != 0 || !strings.Contains(out, `"base_commit":null`) || !strings.Contains(out, `"path_base64":"YS50eHQ="`) {
		t.Fatalf("diff json = %d %q", code, out)
	}
	if code, out, _ := run("ws", "prune", inst, "--before", "2026-09-30T12:00:00Z", "alpha"); code != 0 || !strings.Contains(out, "removed_task_refs: 0\n") {
		t.Fatalf("prune = %d %q", code, out)
	}
	if code, out, _ := run("ws", "ref", "set", inst, "--expected", c0.String(), "--delete", "--json", "alpha", "base"); code != 0 || !strings.Contains(out, `"new_commit":null`) {
		t.Fatalf("delete = %d %q", code, out)
	}
	if code, _, _ := run("ws", "rm", "--instance", strings.Repeat("0", 32), "alpha"); code != 4 {
		t.Fatalf("stale rm = %d", code)
	}
	if code, out, _ := run("ws", "rm", inst, "alpha"); code != 0 || out != "removed: alpha instance: "+v.Instance+"\n" {
		t.Fatalf("rm = %d %q", code, out)
	}
}

// UT-7: DisplayPath quotes valid UTF-8 with control escapes and shows
// anything else as base64.
func TestDisplayPath(t *testing.T) {
	for raw, want := range map[string]string{
		"a/b.txt":    `"a/b.txt"`,
		"日本":         `"日本"`,
		"x\x1by\x07": `"x\x1by\a"`,
		"\xff":       "base64:/w==",
	} {
		if got := DisplayPath(base64.StdEncoding.EncodeToString([]byte(raw))); got != want {
			t.Fatalf("%q = %q, want %q", raw, got, want)
		}
	}
	if DisplayPath("!!") != "base64:!!" {
		t.Fatal("malformed base64")
	}
}
