package contract

import (
	"strings"
	"testing"
	"time"
)

// UT-5/UT-7: the portable branch-name contract: lowercase ASCII components
// of 1-200 bytes, at most 512 bytes as refs/heads/..., go-git's rules
// (no "..", ".lock" suffix or trailing "."), never repaired.
func TestNormalizeBranch(t *testing.T) {
	a200 := strings.Repeat("a", 200)
	full512 := "refs/heads/" + a200 + "/" + a200 + "/" + strings.Repeat("c", 99)
	for in, want := range map[string]string{
		"main": "refs/heads/main", "refs/heads/main": "refs/heads/main", "a/b-c.d_e": "refs/heads/a/b-c.d_e",
		"0": "refs/heads/0", a200: "refs/heads/" + a200, full512: full512, full512[len("refs/heads/"):]: full512,
		"a.b.c": "refs/heads/a.b.c", "refs": "refs/heads/refs",
	} {
		got, err := NormalizeBranch(in, "branch")
		if err != nil || got != want {
			t.Fatalf("%q = %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "Main", "mAin", "café", "café", "ma\xffin", "ma�in", strings.Repeat("a", 201),
		strings.Repeat("a", 256), full512 + "c", "a..b", "a.lock", "a/b.lock/c", "a.", "-a", ".a", "_a", "a/", "/a", "a//b",
		"refs/tags/v1", "refs/heads/", "refs/callsheet/tasks/t_" + strings.Repeat("0", 32), "HEAD", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[", "a\\b", "a@{1}"} {
		if got, err := NormalizeBranch(bad, "branch"); err == nil {
			t.Fatalf("%q accepted as %q", bad, got)
		} else if CodeOf(err) != CodeInvalidArgument || (len(bad) > 12 && strings.Contains(err.Error(), bad)) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if ValidBranchRef("refs/heads/" + strings.Repeat("x", 501)) {
		t.Fatal("512 with a 501-byte component")
	}
}

// UT-5: selectors: hashes (40 hex always a hash), branches, complete task
// refs and, for a base, empty; no short hashes or revision expressions.
func TestParseSelector(t *testing.T) {
	h := strings.Repeat("ab", 20)
	task := TaskRefPrefix + "t_" + strings.Repeat("0", 32)
	for in, want := range map[string]Selector{
		h: {SelectorKindHash, h}, "main": {SelectorKindBranch, "refs/heads/main"}, "refs/heads/x/y": {SelectorKindBranch, "refs/heads/x/y"},
		task: {SelectorKindTask, task}, "abc1234": {SelectorKindBranch, "refs/heads/abc1234"},
	} {
		got, err := ParseSelector(in, "target", false)
		if err != nil || got != want {
			t.Fatalf("%q = %+v %v", in, got, err)
		}
	}
	if s, err := ParseSelector("empty", "base", true); err != nil || s.Kind != SelectorKindEmpty {
		t.Fatalf("empty base %+v %v", s, err)
	}
	if s, err := ParseSelector("empty", "target", false); err != nil || s.Value != "refs/heads/empty" {
		t.Fatalf("empty target %+v %v", s, err)
	}
	// A 41-hex string is an ordinary (valid) short branch name.
	if s, err := ParseSelector(h+"0", "target", false); err != nil || s.Kind != SelectorKindBranch {
		t.Fatalf("41 hex %+v %v", s, err)
	}
	for _, bad := range []string{"", "HEAD", "main~1", "main^", "@{-1}", strings.ToUpper(h), TaskRefPrefix + "t_x", TaskRefPrefix + "T_" + strings.Repeat("0", 32), "refs/tags/v1"} {
		if _, err := ParseSelector(bad, "target", true); CodeOf(err) != CodeInvalidArgument {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if !ValidStatusCursor(task) || !ValidStatusCursor("refs/heads/main") || ValidStatusCursor("main") || ValidStatusCursor("refs/heads/Main") {
		t.Fatal("status cursor grammar")
	}
}

// UT-3/UT-7: prune cutoffs require a zone and normalize to UTC; diff
// cursors are nonempty canonical base64.
func TestCutoffAndCursor(t *testing.T) {
	c, err := ParseCutoff("2026-09-30T14:00:00.5+02:00")
	if err != nil || !c.Equal(time.Date(2026, 9, 30, 12, 0, 0, 5e8, time.UTC)) || c.Location() != time.UTC {
		t.Fatalf("cutoff %v %v", c, err)
	}
	for _, bad := range []string{"2026-09-30T12:00:00", "2026-09-30", "yesterday", "", "2026-09-30 12:00:00Z"} {
		if _, err := ParseCutoff(bad); CodeOf(err) != CodeInvalidArgument {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if b, err := ParsePathCursor("YQ=="); err != nil || string(b) != "a" {
		t.Fatalf("cursor %q %v", b, err)
	}
	for _, bad := range []string{"", "YQ", "Y Q==", "YR==", "!!!!", "=="} {
		if _, err := ParsePathCursor(bad); CodeOf(err) != CodeInvalidArgument {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

// UT-7: ref set requests: exactly one of target and delete=true, delete
// never with expected absent, full hashes only.
func TestValidateRefSet(t *testing.T) {
	inst := strings.Repeat("a", 32)
	h := strings.Repeat("b", 40)
	tr, fa := true, false
	target := "main"
	for _, r := range []WorkspaceRefSetRequest{
		{Instance: inst, Branch: "x", Expected: ExpectedAbsent, Target: &target},
		{Instance: inst, Branch: "x", Expected: h, Delete: &tr},
		{Instance: inst, Branch: "x", Expected: h, Target: &target, Delete: &fa},
	} {
		if _, err := ValidateRefSet(r); err != nil {
			t.Fatalf("%+v: %v", r, err)
		}
	}
	for _, r := range []WorkspaceRefSetRequest{
		{Instance: "x", Branch: "x", Expected: ExpectedAbsent, Target: &target},
		{Instance: inst, Branch: "x", Expected: ExpectedAbsent},
		{Instance: inst, Branch: "x", Expected: h, Target: &target, Delete: &tr},
		{Instance: inst, Branch: "x", Expected: ExpectedAbsent, Delete: &tr},
		{Instance: inst, Branch: "x", Expected: "abc", Target: &target},
		{Instance: inst, Branch: "X", Expected: ExpectedAbsent, Target: &target},
	} {
		if _, err := ValidateRefSet(r); CodeOf(err) != CodeInvalidArgument {
			t.Fatalf("%+v accepted: %v", r, err)
		}
	}
	if _, err := ParseWorkspaceRefSetRequest([]byte(`{"instance":"` + inst + `","branch":"x","expected":"absent","target":"main","extra":1}`)); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("unknown field: %v", err)
	}
	if _, err := ParseWorkspaceRefSetRequest([]byte("{\"instance\":\"" + inst + "\",\"branch\":\"x\xff\",\"expected\":\"absent\",\"target\":\"main\"}")); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("invalid UTF-8: %v", err)
	}
	if _, err := ParseWorkspaceRefSetRequest([]byte(`{"instance":"` + inst + `","branch":"x","expected":"absent","target":"main"}` + strings.Repeat(" ", MaxWorkspaceBody))); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("oversized: %v", err)
	}
}

// UT-7: strict request and response parsers: required fields, no unknown
// fields, bounds, grammars, ordering and cursors consistent with rows.
func TestWorkspaceWireParsers(t *testing.T) {
	inst, gen := strings.Repeat("a", 32), strings.Repeat("b", 32)
	h := strings.Repeat("c", 40)
	view := `{"name":"w","instance":"` + inst + `","created_at":"2026-09-30T12:00:00Z","generation":"` + gen + `","default_branch":"refs/heads/main","size_bytes":10,"retention":{"automatic_prune":false,"quota_bytes":null}}`
	if v, err := ParseWorkspaceView([]byte(view)); err != nil || v.SizeBytes != 10 {
		t.Fatalf("view %+v %v", v, err)
	}
	for _, bad := range []string{
		strings.Replace(view, `"quota_bytes":null`, `"quota_bytes":1`, 1),
		strings.Replace(view, `"automatic_prune":false`, `"automatic_prune":true`, 1),
		strings.Replace(view, `refs/heads/main`, `refs/heads/x`, 1),
		strings.Replace(view, `"size_bytes":10`, `"size_bytes":-1`, 1),
		strings.Replace(view, `}}`, `},"x":1}`, 1),
		strings.Replace(view, `2026-09-30T12:00:00Z`, `2026-09-30T12:00:00+01:00`, 1),
	} {
		if _, err := ParseWorkspaceView([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	list := `{"workspaces":[{"name":"a","instance":"` + inst + `","created_at":"2026-09-30T12:00:00Z"},{"name":"b","instance":"` + inst + `","created_at":"2026-09-30T12:00:00Z"}],"next_after":"b"}`
	if _, err := ParseWorkspaceList([]byte(list), "", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseWorkspaceList([]byte(list), "b", 2); err == nil {
		t.Fatal("rows not after the cursor accepted")
	}
	if _, err := ParseWorkspaceList([]byte(list), "", 1); err == nil {
		t.Fatal("more rows than the limit accepted")
	}
	if _, err := ParseWorkspaceList([]byte(strings.Replace(list, `"next_after":"b"`, `"next_after":"a"`, 1)), "", 2); err == nil {
		t.Fatal("inconsistent next_after accepted")
	}
	status := `{"name":"w","instance":"` + inst + `","generation":"` + gen + `","default_branch":"refs/heads/main","refs":[{"name":"refs/callsheet/tasks/t_` + strings.Repeat("0", 32) + `","commit":"` + h + `","published_at":"2026-09-30T12:00:00Z"},{"name":"refs/heads/main","commit":"` + h + `","published_at":null}],"next_after":null}`
	if _, err := ParseWorkspaceStatus([]byte(status), "", 100); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(status, `"published_at":null`, `"published_at":"2026-09-30T12:00:00Z"`, 1),
		strings.Replace(status, `"published_at":"2026-09-30T12:00:00Z"`, `"published_at":null`, 1),
		strings.Replace(status, `refs/heads/main"`, `refs/heads/Main"`, 1),
		strings.Replace(status, h, "HEAD", 1),
	} {
		if _, err := ParseWorkspaceStatus([]byte(bad), "", 100); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	diff := `{"name":"w","instance":"` + inst + `","generation":"` + gen + `","base_commit":null,"target_commit":"` + h + `","changes":[{"path_base64":"YQ==","kind":"added","old_mode":null,"new_mode":"100644","old_bytes":null,"new_bytes":1}],"next_after":"YQ=="}`
	if _, err := ParseWorkspaceDiff([]byte(diff), nil, 1); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(diff, `"kind":"added"`, `"kind":"renamed"`, 1),
		strings.Replace(diff, `"old_mode":null`, `"old_mode":"100644"`, 1),
		strings.Replace(diff, `"new_mode":"100644"`, `"new_mode":"644"`, 1),
		strings.Replace(diff, `"new_bytes":1`, `"new_bytes":null`, 1),
		strings.Replace(diff, `"next_after":"YQ=="`, `"next_after":"Yg=="`, 1),
		strings.Replace(diff, `"path_base64":"YQ=="`, `"path_base64":"YQ"`, 1),
	} {
		if _, err := ParseWorkspaceDiff([]byte(bad), nil, 1); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if _, err := ParseWorkspaceDiff([]byte(diff), []byte("a"), 1); err == nil {
		t.Fatal("row at the cursor accepted")
	}
	if _, err := ParseWorkspaceRemove([]byte(`{"name":"w","instance":"` + inst + `","removed":false}`)); err == nil {
		t.Fatal("removed false accepted")
	}
	if _, err := ParseWorkspacePrune([]byte(`{"name":"w","instance":"` + inst + `","generation":"` + gen + `","removed_task_refs":-1,"reclaimed_bytes":0,"size_bytes":0}`)); err == nil {
		t.Fatal("negative count accepted")
	}
	if _, err := ParseWorkspaceRefSet([]byte(`{"name":"w","instance":"` + inst + `","generation":"` + gen + `","ref":"refs/heads/main","old_commit":null,"new_commit":"` + h + `"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseWorkspaceCreateRequest([]byte(`{"name":"W"}`)); CodeOf(err) != CodeInvalidArgument {
		t.Fatal("bad name accepted")
	}
	if _, _, err := ParseWorkspacePruneRequest([]byte(`{"instance":"` + inst + `","before":"2026-09-30T12:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseWorkspaceRemoveRequest([]byte(`{"instance":"x"}`)); CodeOf(err) != CodeInvalidArgument {
		t.Fatal("bad instance accepted")
	}
	if _, err := ParseWorkspaceView([]byte(strings.Repeat(" ", MaxWorkspaceResponse+1))); err == nil {
		t.Fatal("oversized response accepted")
	}
}
