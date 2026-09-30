package workspace

import (
	"context"
	"encoding/base64"
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

const diffSentinel = "DIFF-CONTENT-SENTINEL-51c7"

// diffFixture pushes base and target commits into alpha and returns them.
func diffFixture(t *testing.T, base, target map[string]testkit.FileSpec) (*Manager, plumbing.Hash, plumbing.Hash) {
	t.Helper()
	m, _ := newManager(t)
	v := mustCreate(t, m, "alpha")
	s := testkit.NewMemoryStore()
	b := commit(t, s, base, diffSentinel+" base message")
	c := commit(t, s, target, diffSentinel+" target message", b)
	if r := receiveDirect(t, m, "alpha", v.Instance, cmd("refs/heads/main", plumbing.ZeroHash, c), packOf(t, s, objectsFor(t, s, c)...)); !r.ok() {
		t.Fatal(r)
	}
	if _, err := setRef(m, "alpha", v.Instance, "base", contract.ExpectedAbsent, strp(b.String()), false); err != nil {
		t.Fatal(err)
	}
	return m, b, c
}

func sel(kind, v string) contract.Selector { return contract.Selector{Kind: kind, Value: v} }

// rows renders the changes as "path kind old new oldBytes newBytes".
func rows(t *testing.T, cs []contract.WorkspaceChange) []string {
	t.Helper()
	var out []string
	for _, c := range cs {
		p, err := base64.StdEncoding.DecodeString(c.PathBase64)
		if err != nil {
			t.Fatal(err)
		}
		f := func(s *string) string {
			if s == nil {
				return "-"
			}
			return *s
		}
		n := func(i *int64) string {
			if i == nil {
				return "-"
			}
			return itoa64(*i)
		}
		out = append(out, strings.Join([]string{string(p), c.Kind, f(c.OldMode), f(c.NewMode), n(c.OldBytes), n(c.NewBytes)}, " "))
	}
	return out
}

func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// UT-6: added, deleted, content, mode and type changes, directories
// without rows, rename as delete plus add, binary data, symlink target
// length, raw non-UTF-8 and control-character paths, and no blob content
// or commit message anywhere in the result.
func TestDiffKinds(t *testing.T) {
	bin := []byte{0, 1, 2, 0xff, 0xfe}
	base := map[string]testkit.FileSpec{
		"keep.txt":         {Mode: filemode.Regular, Content: []byte("same " + diffSentinel)},
		"edit.txt":         {Mode: filemode.Regular, Content: []byte("old " + diffSentinel)},
		"chmod.sh":         {Mode: filemode.Regular, Content: []byte("#!/bin/sh\n")},
		"gone.txt":         {Mode: filemode.Regular, Content: []byte("bye")},
		"old-name.txt":     {Mode: filemode.Regular, Content: []byte("renamed " + diffSentinel)},
		"link":             {Mode: filemode.Symlink, Content: []byte("keep.txt")},
		"swap":             {Mode: filemode.Regular, Content: []byte("file then dir")},
		"dir/sub/deep.bin": {Mode: filemode.Regular, Content: bin},
	}
	target := map[string]testkit.FileSpec{
		"keep.txt":            {Mode: filemode.Regular, Content: []byte("same " + diffSentinel)},
		"edit.txt":            {Mode: filemode.Regular, Content: []byte("new longer " + diffSentinel)},
		"chmod.sh":            {Mode: filemode.Executable, Content: []byte("#!/bin/sh\n")},
		"new-name.txt":        {Mode: filemode.Regular, Content: []byte("renamed " + diffSentinel)},
		"link":                {Mode: filemode.Symlink, Content: []byte("a/much/longer/target")},
		"swap/inner":          {Mode: filemode.Regular, Content: []byte("now a dir")},
		"dir/sub/deep.bin":    {Mode: filemode.Regular, Content: append(bin, 9)},
		"raw-\xff\xfe.bin":    {Mode: filemode.Regular, Content: bin},
		"ctl\x1b[31m\x07.txt": {Mode: filemode.Regular, Content: []byte("x")},
		"日本語/ファイル.txt":        {Mode: filemode.Regular, Content: []byte("こんにちは")},
	}
	m, b, c := diffFixture(t, base, target)
	resp, err := m.Diff(context.Background(), "alpha", DiffQuery{Base: sel(contract.SelectorKindBranch, "refs/heads/base"), Target: sel(contract.SelectorKindHash, c.String()), Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := rows(t, resp.Changes)
	want := []string{
		"chmod.sh modified 100644 100755 10 10",
		"ctl\x1b[31m\x07.txt added - 100644 - 1",
		"dir/sub/deep.bin modified 100644 100644 5 6",
		"edit.txt modified 100644 100644 " + itoa64(int64(len("old "+diffSentinel))) + " " + itoa64(int64(len("new longer "+diffSentinel))),
		"gone.txt deleted 100644 - 3 -",
		"link modified 120000 120000 8 20",
		"new-name.txt added - 100644 - " + itoa64(int64(len("renamed "+diffSentinel))),
		"old-name.txt deleted 100644 - " + itoa64(int64(len("renamed "+diffSentinel))) + " -",
		"raw-\xff\xfe.bin added - 100644 - 5",
		"swap deleted 100644 - 13 -",
		"swap/inner added - 100644 - 9",
		"日本語/ファイル.txt added - 100644 - 15",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	enc, _ := contract.Encode(resp)
	if strings.Contains(string(enc), diffSentinel) || strings.Contains(string(enc), "message") {
		t.Fatalf("content leaked: %s", enc)
	}
	if *resp.BaseCommit != b.String() || resp.TargetCommit != c.String() || resp.NextAfter != nil {
		t.Fatalf("header %+v", resp)
	}
	// Equal snapshots: no rows.
	eq, err := m.Diff(context.Background(), "alpha", DiffQuery{Base: sel(contract.SelectorKindBranch, "refs/heads/main"), Target: sel(contract.SelectorKindHash, c.String()), Limit: 100})
	if err != nil || len(eq.Changes) != 0 || eq.NextAfter != nil {
		t.Fatalf("equal %+v %v", eq, err)
	}
	// A root commit against the empty tree lists every file as added.
	root, err := m.Diff(context.Background(), "alpha", DiffQuery{Base: sel(contract.SelectorKindEmpty, ""), Target: sel(contract.SelectorKindHash, b.String()), Limit: 100})
	if err != nil || root.BaseCommit != nil || len(root.Changes) != len(base) {
		t.Fatalf("root %+v %v", root, err)
	}
	for _, ch := range root.Changes {
		if ch.Kind != contract.ChangeAdded || ch.OldMode != nil || ch.OldBytes != nil {
			t.Fatalf("root row %+v", ch)
		}
	}
	// Unknown or unreachable selectors.
	if _, err := m.Diff(context.Background(), "alpha", DiffQuery{Base: sel(contract.SelectorKindEmpty, ""), Target: sel(contract.SelectorKindBranch, "refs/heads/nope"), Limit: 1}); codeOf(err) != contract.CodeNotFound {
		t.Fatalf("missing target: %v", err)
	}
	if _, err := m.Diff(context.Background(), "alpha", DiffQuery{Base: sel(contract.SelectorKindHash, strings.Repeat("1", 40)), Target: sel(contract.SelectorKindBranch, "refs/heads/main"), Limit: 1}); codeOf(err) != contract.CodeNotFound {
		t.Fatalf("unreachable base: %v", err)
	}
}

// UT-6: pages return every changed path exactly once in raw byte order,
// ties of shared prefixes included, with a generation-pinned cursor.
func TestDiffPagination(t *testing.T) {
	names := []string{"a", "a.txt", "a/b", "a/b.c", "a-b", "a0", "b\x00x", "b\x01", "z", "\xff"}
	target := map[string]testkit.FileSpec{}
	for _, n := range names {
		if strings.Contains(n, "\x00") {
			continue
		}
		target[n] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte(n)}
	}
	// "a" is a file and "a/..." a directory: rename the file to keep the
	// tree valid.
	delete(target, "a")
	target["a!"] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte("a!")}
	m, _, c := diffFixture(t, map[string]testkit.FileSpec{"root": {Mode: filemode.Regular, Content: []byte("r")}}, target)
	var all []string
	q := DiffQuery{Base: sel(contract.SelectorKindEmpty, ""), Target: sel(contract.SelectorKindBranch, "refs/heads/main"), Limit: 3}
	for {
		p, err := m.Diff(context.Background(), "alpha", q)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows(t, p.Changes) {
			all = append(all, strings.SplitN(r, " ", 2)[0])
		}
		if p.NextAfter == nil {
			break
		}
		after, err := contract.ParsePathCursor(*p.NextAfter)
		if err != nil {
			t.Fatal(err)
		}
		q = DiffQuery{Base: sel(contract.SelectorKindEmpty, ""), Target: sel(contract.SelectorKindHash, p.TargetCommit), After: after, Instance: p.Instance, Generation: p.Generation, Limit: 3}
	}
	want := []string{}
	for n := range target {
		want = append(want, n)
	}
	sort.Strings(want)
	if strings.Join(all, "|") != strings.Join(want, "|") {
		t.Fatalf("pages %q want %q", all, want)
	}
	// A ref update between pages makes the continuation conflict.
	p, _ := m.Diff(context.Background(), "alpha", DiffQuery{Base: sel(contract.SelectorKindEmpty, ""), Target: sel(contract.SelectorKindHash, c.String()), Limit: 1})
	h, _ := m.lookup("alpha")
	if _, err := setRef(m, "alpha", h.instance, "other", contract.ExpectedAbsent, strp("main"), false); err != nil {
		t.Fatal(err)
	}
	after, _ := contract.ParsePathCursor(*p.NextAfter)
	if _, err := m.Diff(context.Background(), "alpha", DiffQuery{Base: sel(contract.SelectorKindEmpty, ""), Target: sel(contract.SelectorKindHash, c.String()), After: after, Instance: p.Instance, Generation: p.Generation, Limit: 1}); codeOf(err) != contract.CodeConflict {
		t.Fatalf("generation change: %v", err)
	}
}

// UT-6: a single row too large for the 1 MiB bound is invalid_argument,
// never truncated; a page stops before crossing it.
func TestDiffBounds(t *testing.T) {
	long := strings.Repeat("d/", 100) + strings.Repeat("f", 200)
	target := map[string]testkit.FileSpec{}
	for i := range 20 {
		target[long+itoa64(int64(i+1))] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte{byte(i)}}
	}
	m, _, _ := diffFixture(t, map[string]testkit.FileSpec{"x": {Mode: filemode.Regular, Content: []byte("x")}}, target)
	p, err := m.Diff(context.Background(), "alpha", DiffQuery{Base: sel(contract.SelectorKindEmpty, ""), Target: sel(contract.SelectorKindBranch, "refs/heads/main"), Limit: 100})
	if err != nil || len(p.Changes) != 20 {
		t.Fatalf("%d rows %v", len(p.Changes), err)
	}
	big := make([]contract.WorkspaceChange, 1)
	big[0] = contract.WorkspaceChange{PathBase64: strings.Repeat("A", contract.MaxWorkspaceResponse), Kind: contract.ChangeAdded}
	resp := contract.WorkspaceDiffResponse{Changes: []contract.WorkspaceChange{}}
	if err := page(&resp, &resp.Changes, &resp.NextAfter, big, func(c contract.WorkspaceChange) string { return c.PathBase64 }, 100); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("oversized row: %v", err)
	}
	var many []contract.WorkspaceChange
	for i := range 100 {
		many = append(many, contract.WorkspaceChange{PathBase64: strings.Repeat("B", 20<<10) + itoa64(int64(i)), Kind: contract.ChangeAdded})
	}
	resp = contract.WorkspaceDiffResponse{Changes: []contract.WorkspaceChange{}}
	if err := page(&resp, &resp.Changes, &resp.NextAfter, many, func(c contract.WorkspaceChange) string { return c.PathBase64 }, 100); err != nil {
		t.Fatal(err)
	}
	enc, _ := contract.Encode(resp)
	if len(resp.Changes) >= 100 || resp.NextAfter == nil || *resp.NextAfter != resp.Changes[len(resp.Changes)-1].PathBase64 || len(enc)+1 > contract.MaxWorkspaceResponse {
		t.Fatalf("bounded page: %d rows, %d bytes", len(resp.Changes), len(enc))
	}
}
