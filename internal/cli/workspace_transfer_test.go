package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Iteration 09b (UT-7): the ws push and ws pull leaves.

const (
	trInst = "0123456789abcdef0123456789abcdef"
	trHash = "0123456789abcdef0123456789abcdef01234567"
	trOld  = "89abcdef0123456789abcdef0123456789abcdef"
)

// caFlags starts a verified TLS test plane and returns trust flags that
// resolve against it (the transfers themselves are replaced by seams).
func caFlags(t *testing.T) []string {
	t.Helper()
	h := testkit.NewHarness(t)
	p := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(p, h.CAPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{"--plane", h.URL, "--ca", p}
}

// transferSeams replaces the transfer functions and process reads.
type transferSeams struct {
	push []workspacetransfer.PushRequest
	pull []workspacetransfer.PullRequest
	opts []workspacetransfer.Options
	perr error
	lerr error
	pres contract.WorkspacePushResult
	lres contract.WorkspacePullResult
}

func seamTransfers(t *testing.T) *transferSeams {
	t.Helper()
	s := &transferSeams{}
	op, ol, oc, oe := transferPush, transferPull, processCwd, processEnv
	t.Cleanup(func() { transferPush, transferPull, processCwd, processEnv = op, ol, oc, oe })
	processCwd = func() (string, error) { return "/work/here", nil }
	processEnv = func() []string { return []string{"HOME=/home/x", "XDG_CONFIG_HOME=/cfg"} }
	transferPush = func(_ context.Context, o workspacetransfer.Options, req workspacetransfer.PushRequest) (contract.WorkspacePushResult, error) {
		s.push, s.opts = append(s.push, req), append(s.opts, o)
		return s.pres, s.perr
	}
	transferPull = func(_ context.Context, o workspacetransfer.Options, req workspacetransfer.PullRequest) (contract.WorkspacePullResult, error) {
		s.pull, s.opts = append(s.pull, req), append(s.opts, o)
		return s.lres, s.lerr
	}
	return s
}

func TestTransferLeafHelp(t *testing.T) {
	isolate(t)
	for _, leaf := range []string{"push", "pull"} {
		code, out, errOut := exec(t, "darwin", "ws", leaf, "--help")
		if code != 0 || errOut != "" || !strings.Contains(out, "\nStatus: implemented.\n") || !strings.Contains(out, "PATH is on this machine") ||
			strings.Contains(out, "nothing local is read or written") {
			t.Fatalf("%s help %d %q", leaf, code, out)
		}
	}
	_, out, _ := exec(t, "linux", "ws", "push", "--help")
	for _, want := range []string{"Usage: callsheet ws push --instance TOKEN [--branch BRANCH] --plane URL", "[--json] NAME [PATH]", "otherwise\ncommit first", "line-ending-only difference is clean", "ws ref set"} {
		if !strings.Contains(out, want) {
			t.Fatalf("push help lacks %q", want)
		}
	}
	_, out, _ = exec(t, "linux", "ws", "pull", "--help")
	for _, want := range []string{"Usage: callsheet ws pull --plane URL", "[--json] (TASK_ID | NAME REF) [PATH]", "git push <your-remote> <commit>:refs/heads/<delivery-branch>",
		"refs/callsheet/NAME/heads/", "existing empty one", "a bare task ID meaning that task ref", "refs/heads/t_...", "workspace_instance_mismatch"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pull help lacks %q", want)
		}
	}
	_, out, _ = exec(t, "linux", "ws")
	for _, leaf := range []string{"push", "pull"} {
		if !strings.Contains(out, "\n  "+leaf+" ") || strings.Contains(out, "future stub") {
			t.Fatalf("ws group %q", out)
		}
	}
}

// Usage and validation errors exit 2 before any transfer, trust or
// network work.
func TestTransferLeafUsage(t *testing.T) {
	isolate(t)
	s := seamTransfers(t)
	trust := caFlags(t)
	inst := "--instance=" + trInst
	long := strings.Repeat("a", 1024)
	for _, c := range []struct {
		goos string
		args []string
	}{
		{"linux", []string{"ws", "push", inst}},
		{"linux", []string{"ws", "push", inst, "a", "p", "extra"}},
		{"linux", []string{"ws", "push", "a"}},
		{"linux", []string{"ws", "push", "--instance", "x", "a"}},
		{"linux", []string{"ws", "push", inst, inst, "a"}},
		{"linux", []string{"ws", "push", inst, "--branch", "", "a"}},
		{"linux", []string{"ws", "push", inst, "--branch", "Main", "a"}},
		{"linux", []string{"ws", "push", inst, "--branch", "a", "--branch", "b", "a"}},
		{"linux", []string{"ws", "push", inst, "--force", "a"}},
		{"linux", []string{"ws", "push", inst, "--commit", "a"}},
		{"linux", []string{"ws", "push", inst, "Bad"}},
		{"linux", []string{"ws", "push", inst, "a", ""}},
		{"linux", []string{"ws", "push", inst, "a", "x\x00y"}},
		{"linux", []string{"ws", "push", inst, "a", "\xff"}},
		{"darwin", []string{"ws", "push", inst, "a", long}},
		{"linux", []string{"ws", "pull"}},
		{"linux", []string{"ws", "pull", "a"}},
		{"linux", []string{"ws", "pull", "a", "main", "p", "extra"}},
		{"linux", []string{"ws", "pull", "a", "HEAD~1"}},
		{"linux", []string{"ws", "pull", "a", "abc~1"}},
		{"linux", []string{"ws", "pull", "a", "main", ""}},
		{"linux", []string{"ws", "pull", "--remote", "x", "a", "main"}},
		{"linux", []string{"ws", "pull", "A", "main"}},
	} {
		code, out, errOut := exec(t, c.goos, append(c.args, trust...)...)
		if code != 2 || out != "" || !strings.HasPrefix(errOut, "callsheet: invalid_argument: ") || strings.ContainsAny(errOut, "\x00\xff") {
			t.Fatalf("%s %q = %d %q %q", c.goos, c.args, code, out, errOut)
		}
	}
	// A 1024-byte path is fine on Linux (up to 4095).
	s.lres = contract.WorkspacePullResult{Name: "a", Instance: trInst, Selector: "refs/heads/main", Commit: trHash, DestinationKind: contract.KindFolder, Changed: true}
	if code, _, errOut := exec(t, "linux", append([]string{"ws", "pull", "a", "main", long}, trust...)...); code != 0 {
		t.Fatalf("linux long path %d %q", code, errOut)
	}
	if len(s.push) != 0 || len(s.pull) != 1 {
		t.Fatalf("transfers %v %v", s.push, s.pull)
	}
	// Missing trust is trust_failed before any transfer.
	if code, _, _ := exec(t, "linux", "ws", "pull", "--plane", trust[1], "a", "main"); code != 6 {
		t.Fatalf("no trust = %d", code)
	}
}

// Successful transfers relay the operands with the explicit goos, the
// process working directory and environment, and print the exact DTO as
// JSON or stable key: value text (null as none).
func TestTransferLeafOutput(t *testing.T) {
	isolate(t)
	s := seamTransfers(t)
	trust := caFlags(t)
	old := trOld
	s.pres = contract.WorkspacePushResult{Name: "alpha", Instance: trInst, Branch: "refs/heads/topic", OldCommit: &old, Commit: trHash, SourceKind: contract.KindFolder, Changed: true}
	code, out, errOut := exec(t, "darwin", append([]string{"ws", "push", "--instance", trInst, "--branch", "topic"}, append(trust, "alpha", "src")...)...)
	want := "name: alpha\ninstance: " + trInst + "\nbranch: refs/heads/topic\nold_commit: " + trOld + "\ncommit: " + trHash + "\nsource_kind: folder\nchanged: true\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("push text %d %q %q", code, out, errOut)
	}
	if s.push[0] != (workspacetransfer.PushRequest{Name: "alpha", Instance: trInst, Branch: "topic", Path: "src", PathSet: true}) {
		t.Fatalf("push request %+v", s.push[0])
	}
	o := s.opts[0]
	if o.GOOS != "darwin" || o.Cwd != "/work/here" || o.Plane == nil {
		t.Fatalf("options %+v", o)
	}
	if v, _ := o.Env.Lookup("XDG_CONFIG_HOME"); v != "/cfg" {
		t.Fatalf("env %v", v)
	}
	s.pres.OldCommit, s.pres.Changed = nil, true
	code, out, _ = exec(t, "linux", append([]string{"ws", "push", "--json", "--instance", trInst}, append(trust, "alpha")...)...)
	b, _ := contract.Encode(s.pres)
	if code != 0 || out != string(b)+"\n" || !strings.Contains(out, `"old_commit":null`) {
		t.Fatalf("push json %d %q", code, out)
	}
	if s.push[1].PathSet || s.push[1].Branch != "" {
		t.Fatalf("default path/branch %+v", s.push[1])
	}
	ref := "refs/callsheet/alpha/heads/main"
	s.lres = contract.WorkspacePullResult{Name: "alpha", Instance: trInst, Selector: "refs/heads/main", Commit: trHash, DestinationKind: contract.KindGit, LocalRef: &ref, Changed: true}
	code, out, _ = exec(t, "linux", append([]string{"ws", "pull"}, append(trust, "alpha", "main", "/abs/repo")...)...)
	want = "name: alpha\ninstance: " + trInst + "\nselector: refs/heads/main\ncommit: " + trHash + "\ndestination_kind: git\nlocal_ref: " + ref + "\nold_commit: none\nchanged: true\n"
	if code != 0 || out != want {
		t.Fatalf("pull text %d %q", code, out)
	}
	var dto map[string]any
	code, out, _ = exec(t, "linux", append([]string{"ws", "pull", "--json"}, append(trust, "alpha", "main")...)...)
	if err := json.Unmarshal([]byte(out), &dto); err != nil || code != 0 || len(dto) != 8 || dto["old_commit"] != nil {
		t.Fatalf("pull json %d %q", code, out)
	}
	if !bytes.HasSuffix([]byte(out), []byte("}\n")) {
		t.Fatalf("pull json newline %q", out)
	}
}

// Transfer errors map to the contract exit codes with their safe
// guidance; cancellation exits 130.
func TestTransferLeafErrors(t *testing.T) {
	isolate(t)
	s := seamTransfers(t)
	trust := caFlags(t)
	push := append([]string{"ws", "push", "--instance", trInst}, append(trust, "alpha")...)
	for _, c := range []struct {
		err  error
		code int
		msg  string
	}{
		{contract.TransferError(contract.CodeConflict, contract.ReasonDirtySource, contract.DirtyMessage), 4, "commit first"},
		{contract.TransferError(contract.CodeConflict, contract.ReasonNonFastForward, contract.NonFastForwardMessage), 4, "ws ref set"},
		{contract.New(contract.CodeNotFound, "no such workspace"), 3, "no such workspace"},
		{contract.TransferError(contract.CodeInvalidArgument, contract.ReasonUnsupportedRepository, "unsupported repository: a shallow clone"), 2, "shallow"},
		{contract.New(contract.CodeUnavailable, "cannot reach the plane; "+contract.PushAmbiguity), 5, "may have succeeded"},
		{contract.New(contract.CodeTrustFailed, "connection not trusted"), 6, "not trusted"},
		{contract.VersionMismatch(contract.ProtocolVersion, 4), 7, ""},
		{contract.TransferError(contract.CodeInternal, contract.ReasonStorageFailure, "cannot stage the pack: the disk is full"), 1, "disk is full"},
		{context.Canceled, 130, "interrupted"},
		{errors.New("raw"), 1, "raw"},
	} {
		s.perr = c.err
		code, out, errOut := exec(t, "linux", push...)
		if code != c.code || out != "" || !strings.Contains(errOut, c.msg) {
			t.Fatalf("%v: %d %q %q", c.err, code, out, errOut)
		}
	}
	s.lerr = contract.TransferError(contract.CodeConflict, contract.ReasonDestinationNotEmpty, "destination must be new or empty; nothing was written")
	if code, _, errOut := exec(t, "linux", append([]string{"ws", "pull"}, append(trust, "alpha", "main")...)...); code != 4 || !strings.Contains(errOut, "new or empty") {
		t.Fatalf("pull conflict %d %q", code, errOut)
	}
}
