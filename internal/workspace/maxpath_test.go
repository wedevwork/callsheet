package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// MaxPathRoot returns a canonical, existing, private state root of exactly
// n bytes below dir (components of at most 200 bytes). dir is resolved
// first (macOS /var aliases included).
func maxPathRoot(t testing.TB, n int) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := base
	for len(root) < n {
		need := n - len(root) - 1
		if need < 1 {
			// One byte short of a separator plus a name: widen the last
			// component instead.
			root += "x"
			continue
		}
		root = filepath.Join(root, strings.Repeat("r", min(need, 200)))
	}
	if len(root) != n {
		t.Fatalf("root is %d bytes", len(root))
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if canon, _ := filepath.EvalSymlinks(root); canon != root {
		t.Fatalf("root %q is not canonical (%q)", root, canon)
	}
	return root
}

// MaxRef is a valid 512-byte full branch ref of components <= 200 bytes.
func maxRef() string {
	return "refs/heads/" + strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 99)
}

// D1 maximum-path subcase (delegated by TestWorkspaceRefSet): with a
// 256-byte canonical root, a 63-byte workspace name and a 512-byte ref,
// push, create, explicit move (a copy into a distinct 32-hex generation),
// prune retaining the branch, delete and prune again all succeed; every
// captured filesystem path (temporaries, library files, copy and ref
// destinations) respects the 994-byte total and 255-byte component
// bounds, and the longest published ref path is exactly 898 bytes. A
// 257-byte root and a 513-byte ref are refused before any write.
func TestMaxPathLayout(t *testing.T) {
	ctx := context.Background()
	root := maxPathRoot(t, MaxRootBytes)
	name := strings.Repeat("w", 63)
	ref := maxRef()
	if len(ref) != contract.MaxFullRef {
		t.Fatalf("ref is %d bytes", len(ref))
	}
	rec := &recorder{}
	d := fastDeps()
	d.observe = rec.observe
	m := openAt(t, d, root)
	g := serveGit(t, m)
	v := mustCreate(t, m, name)
	s := testkit.NewMemoryStore()
	c0 := commit(t, s, seedFiles(), "c0")
	files := seedFiles()
	files["only-on-branch.txt"] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte("exclusive")}
	c1 := commit(t, s, files, "c1", c0)
	r := g.remote(name, v.Instance)
	push(t, r, s, "refs/heads/main", plumbing.ZeroHash, c0)
	// The library writes the maximum ref itself on receive.
	push(t, r, s, ref, plumbing.ZeroHash, c1)
	if got := refsOf(t, m, name)[ref]; got != c1.String() {
		t.Fatalf("pushed %s", got)
	}
	// keep holds c1 reachable while the maximum ref is recreated.
	if _, err := setRef(m, name, v.Instance, "keep", contract.ExpectedAbsent, strp(ref), false); err != nil {
		t.Fatal(err)
	}
	if _, err := setRef(m, name, v.Instance, ref, c1.String(), nil, true); err != nil {
		t.Fatal(err)
	}
	// Create through ref set, then an explicit move.
	if r, err := setRef(m, name, v.Instance, ref, contract.ExpectedAbsent, strp("main"), false); err != nil || *r.NewCommit != c0.String() {
		t.Fatalf("create %+v %v", r, err)
	}
	genBefore := genOf(t, m, name)
	rec.mu.Lock()
	rec.ops = nil
	rec.mu.Unlock()
	mv, err := setRef(m, name, v.Instance, ref, c0.String(), strp(c1.String()), false)
	if err != nil || *mv.NewCommit != c1.String() {
		t.Fatalf("move %+v %v", mv, err)
	}
	if !contract.ValidWorkspaceToken(mv.Generation) || mv.Generation == genBefore {
		t.Fatalf("move generation %q after %q", mv.Generation, genBefore)
	}
	wsDir := filepath.Join(root, dirName, name)
	copied := filepath.Join(wsDir, generationsName, mv.Generation, repoName, filepath.FromSlash(ref))
	if len(copied) != 898 {
		t.Fatalf("published ref path is %d bytes", len(copied))
	}
	var copyDst bool
	for _, o := range rec.all() {
		if o == "create "+copied {
			copyDst = true
		}
	}
	if !copyDst {
		t.Fatal("the move did not write the maximum ref into its new generation")
	}
	if b, err := os.ReadFile(copied); err != nil || strings.TrimSpace(string(b)) != c1.String() {
		t.Fatalf("published ref %q %v", b, err)
	}
	if _, err := setRef(m, name, v.Instance, "keep", c1.String(), nil, true); err != nil {
		t.Fatal(err)
	}
	// Reopen and verify the exact hash.
	m.Close()
	d2 := fastDeps()
	d2.observe = rec.observe
	m = openAt(t, d2, root)
	if got := refsOf(t, m, name)[ref]; got != c1.String() {
		t.Fatalf("after reopen %s", got)
	}
	// Prune retains the branch's closure in a new generation.
	if _, err := m.Prune(ctx, name, v.Instance, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !hasObject(t, m, name, c1) || !hasObject(t, m, name, c0) {
		t.Fatal("prune dropped a retained closure")
	}
	// Delete, prune again: the branch's exclusive history is collected.
	if _, err := setRef(m, name, v.Instance, ref, c1.String(), nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prune(ctx, name, v.Instance, time.Now()); err != nil {
		t.Fatal(err)
	}
	if hasObject(t, m, name, c1) || !hasObject(t, m, name, c0) {
		t.Fatal("prune after delete kept exclusive or dropped shared history")
	}
	// Every captured path respects the budget; the library's temporary
	// pack names and the copy destinations were among them.
	var longest int
	var temps int
	for _, o := range rec.all() {
		_, p, _ := strings.Cut(o, " ")
		if err := checkPath(p); err != nil {
			t.Fatalf("%s: %v", o, err)
		}
		longest = max(longest, len(p))
		if strings.Contains(p, "tmp_pack_") || strings.HasSuffix(p, currentTmpName) {
			temps++
		}
	}
	if longest < 898 || temps == 0 {
		t.Fatalf("longest captured path %d, temporaries %d", longest, temps)
	}
	m.Close()
	// A 257-byte root is refused before any state is touched.
	long := maxPathRoot(t, MaxRootBytes+1)
	if _, err := fastDeps().open(ctx, long, true); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("257-byte root: %v", err)
	}
	if es, _ := os.ReadDir(long); len(es) != 0 {
		t.Fatalf("a refused root was written: %v", es)
	}
	// A 513-byte otherwise valid ref is invalid before any transaction.
	if _, err := contract.ValidateRefSet(contract.WorkspaceRefSetRequest{Instance: v.Instance, Branch: ref + "c", Expected: contract.ExpectedAbsent, Target: strp("main")}); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("513-byte ref: %v", err)
	}
}
