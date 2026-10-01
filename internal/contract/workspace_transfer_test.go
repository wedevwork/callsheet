package contract

import (
	"errors"
	"strings"
	"testing"
)

// Iteration 09b (UT-7): the transfer DTOs, validators and local ref
// mapping.

const (
	tInst = "0123456789abcdef0123456789abcdef"
	tHash = "0123456789abcdef0123456789abcdef01234567"
	tOld  = "89abcdef0123456789abcdef0123456789abcdef"
)

func TestTransferReasonsAndErrors(t *testing.T) {
	for _, r := range []string{ReasonDirtySource, ReasonUnsupportedRepository, ReasonNonFastForward, ReasonSourceChanged,
		ReasonDestinationNotEmpty, ReasonUnsafeTree, ReasonLocalRefConflict, ReasonStorageFailure} {
		if !ValidTransferReason(r) {
			t.Fatalf("%s not valid", r)
		}
	}
	if ValidTransferReason("dirty") || ValidTransferReason("") {
		t.Fatal("unknown reason valid")
	}
	e := TransferError(CodeConflict, ReasonDirtySource, DirtyMessage)
	if e.Code != CodeConflict || TransferReason(e) != ReasonDirtySource || !strings.HasSuffix(e.Message, "commit first") {
		t.Fatalf("%+v", e)
	}
	if e := TransferError(CodeInternal, "", "x"); e.Details != nil || TransferReason(e) != "" {
		t.Fatalf("no reason %+v", e)
	}
	if TransferReason(errors.New("x")) != "" || TransferReason(nil) != "" {
		t.Fatal("reason of a non-contract error")
	}
	if !strings.Contains(NonFastForwardMessage, "ws ref set") || !strings.Contains(PushAmbiguity, "may have succeeded") || !strings.Contains(PullAmbiguity, "inspect") {
		t.Fatal("guidance texts")
	}
}

func TestLocalRefFor(t *testing.T) {
	task := "t_" + strings.Repeat("a", 32)
	for _, c := range []struct{ name, sel, want string }{
		{"ws", "refs/heads/main", "refs/callsheet/ws/heads/main"},
		{"ws", "refs/heads/topic/x", "refs/callsheet/ws/heads/topic/x"},
		{"ws", TaskRefPrefix + task, "refs/callsheet/ws/tasks/" + task},
		{"ws", tHash, "refs/callsheet/ws/commits/" + tHash},
		{"ws", "main", ""},
		{"Bad", "refs/heads/main", ""},
		{"ws", "refs/tags/v1", ""},
	} {
		if got := LocalRefFor(c.name, c.sel); got != c.want {
			t.Fatalf("%s %s = %q", c.name, c.sel, got)
		}
	}
}

func TestValidateLocalPath(t *testing.T) {
	for _, c := range []struct {
		goos, p string
		ok      bool
	}{
		{"linux", "a", true},
		{"linux", "/abs/b", true},
		{"linux", strings.Repeat("a", 4095), true},
		{"linux", strings.Repeat("a", 4096), false},
		{"darwin", strings.Repeat("a", 1023), true},
		{"darwin", strings.Repeat("a", 1024), false},
		{"linux", "", false},
		{"linux", "a\x00b", false},
		{"linux", "a\xffb", false},
		{"windows", "a", false},
	} {
		err := ValidateLocalPath(c.goos, c.p)
		if (err == nil) != c.ok {
			t.Fatalf("%s %q: %v", c.goos, c.p[:min(len(c.p), 12)], err)
		}
		if err != nil && (CodeOf(err) != CodeInvalidArgument || strings.ContainsAny(err.Error(), "\x00\xff")) {
			t.Fatalf("%v", err)
		}
	}
	if MaxPathFor("linux") != 4095 || MaxPathFor("darwin") != 1023 || MaxPathFor("") != 0 {
		t.Fatal("limits")
	}
}

func TestValidatePushPull(t *testing.T) {
	in, err := ValidatePush("ws", tInst, "")
	if err != nil || in.Branch != DefaultBranchRef {
		t.Fatalf("%+v %v", in, err)
	}
	if in, err := ValidatePush("ws", tInst, "topic/x"); err != nil || in.Branch != "refs/heads/topic/x" {
		t.Fatalf("%+v %v", in, err)
	}
	for _, c := range [][3]string{{"Ws", tInst, ""}, {"ws", "x", ""}, {"ws", tInst, "Main"}, {"ws", tInst, "refs/tags/x"}, {"ws", tInst, "a..b"}} {
		if _, err := ValidatePush(c[0], c[1], c[2]); CodeOf(err) != CodeInvalidArgument {
			t.Fatalf("%v: %v", c, err)
		}
	}
	p, err := ValidatePull("ws", "main")
	if err != nil || p.Canonical != "refs/heads/main" || p.Selector.Kind != SelectorKindBranch {
		t.Fatalf("%+v %v", p, err)
	}
	if p, _ := ValidatePull("ws", tHash); p.Selector.Kind != SelectorKindHash || p.Canonical != tHash {
		t.Fatalf("%+v", p)
	}
	task := "t_" + strings.Repeat("b", 32)
	if p, _ := ValidatePull("ws", TaskRefPrefix+task); p.Selector.Kind != SelectorKindTask {
		t.Fatalf("%+v", p)
	}
	if p, _ := ValidatePull("ws", task); p.Selector.Kind != SelectorKindBranch || p.Canonical != BranchPrefix+task {
		t.Fatalf("task id shorthand is a branch: %+v", p)
	}
	for _, c := range [][2]string{{"ws", ""}, {"ws", "HEAD~1"}, {"ws", "empty/.."}, {"Ws", "main"}, {"ws", TaskRefPrefix + "x"}} {
		if _, err := ValidatePull(c[0], c[1]); CodeOf(err) != CodeInvalidArgument {
			t.Fatalf("%v: %v", c, err)
		}
	}
}

func TestTransferResults(t *testing.T) {
	old := tOld
	same := tHash
	push := WorkspacePushResult{Name: "ws", Instance: tInst, Branch: DefaultBranchRef, OldCommit: &old, Commit: tHash, SourceKind: KindGit, Changed: true}
	b, err := Encode(push)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"name":"ws","instance":"`+tInst+`","branch":"refs/heads/main","old_commit":"`+tOld+`","commit":"`+tHash+`","source_kind":"git","changed":true}` {
		t.Fatalf("push json %s", b)
	}
	if r, err := ParseWorkspacePushResult(b); err != nil || r.Commit != tHash {
		t.Fatalf("%+v %v", r, err)
	}
	bad := []func(r *WorkspacePushResult){
		func(r *WorkspacePushResult) { r.Name = "" },
		func(r *WorkspacePushResult) { r.Branch = "main" },
		func(r *WorkspacePushResult) { r.Commit = "x" },
		func(r *WorkspacePushResult) { r.SourceKind = "zip" },
		func(r *WorkspacePushResult) { r.OldCommit = nil; r.Changed = false },
		func(r *WorkspacePushResult) { r.OldCommit = &same },
		func(r *WorkspacePushResult) { r.Changed = false },
	}
	for i, f := range bad {
		r := push
		f(&r)
		if r.Validate() == nil {
			t.Fatalf("push case %d valid", i)
		}
	}
	r := push
	r.OldCommit, r.Changed = &same, false
	if r.Validate() != nil {
		t.Fatal("same-value push invalid")
	}
	if _, err := ParseWorkspacePushResult([]byte(`{"name":"ws"}`)); err == nil {
		t.Fatal("partial push parsed")
	}
	ref := "refs/callsheet/ws/heads/main"
	pull := WorkspacePullResult{Name: "ws", Instance: tInst, Selector: DefaultBranchRef, Commit: tHash, DestinationKind: KindGit, LocalRef: &ref, OldCommit: &old, Changed: true}
	b, _ = Encode(pull)
	if p, err := ParseWorkspacePullResult(b); err != nil || *p.LocalRef != ref {
		t.Fatalf("%+v %v", p, err)
	}
	folder := WorkspacePullResult{Name: "ws", Instance: tInst, Selector: tHash, Commit: tHash, DestinationKind: KindFolder, Changed: true}
	b, _ = Encode(folder)
	if !strings.Contains(string(b), `"local_ref":null,"old_commit":null,"changed":true`) {
		t.Fatalf("folder json %s", b)
	}
	if folder.Validate() != nil {
		t.Fatal("folder invalid")
	}
	wrongRef := "refs/callsheet/ws/heads/other"
	badPull := []func(r *WorkspacePullResult){
		func(r *WorkspacePullResult) { r.Instance = "x" },
		func(r *WorkspacePullResult) { r.Selector = "main" },
		func(r *WorkspacePullResult) { r.Commit = "" },
		func(r *WorkspacePullResult) { r.DestinationKind = "zip" },
		func(r *WorkspacePullResult) { r.LocalRef = nil },
		func(r *WorkspacePullResult) { r.LocalRef = &wrongRef },
		func(r *WorkspacePullResult) { r.OldCommit = &same },
		func(r *WorkspacePullResult) { r.OldCommit = nil; r.Changed = false },
	}
	for i, f := range badPull {
		r := pull
		f(&r)
		if r.Validate() == nil {
			t.Fatalf("pull case %d valid", i)
		}
	}
	for i, f := range []func(r *WorkspacePullResult){
		func(r *WorkspacePullResult) { r.LocalRef = &ref },
		func(r *WorkspacePullResult) { r.OldCommit = &old },
		func(r *WorkspacePullResult) { r.Changed = false },
	} {
		r := folder
		f(&r)
		if r.Validate() == nil {
			t.Fatalf("folder case %d valid", i)
		}
	}
	if _, err := ParseWorkspacePullResult([]byte(`{}`)); err == nil {
		t.Fatal("empty pull parsed")
	}
}
