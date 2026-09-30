package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// UT-1: names use the slug grammar and an invalid name writes nothing.
func TestCreateNames(t *testing.T) {
	m, root := newManager(t)
	for _, bad := range []string{"", "A", "-a", "a-", "a_b", "a.b", "a/b", "..", strings.Repeat("a", 64), "é", "a\x00"} {
		if _, err := m.Create(context.Background(), bad); codeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if files := listFiles(t, root); len(files) != 0 {
		t.Fatalf("invalid names wrote %v", files)
	}
	v := mustCreate(t, m, strings.Repeat("a", 63))
	if v.Name != strings.Repeat("a", 63) {
		t.Fatalf("view %+v", v)
	}
}

// UT-1: identity is persisted once, tokens are fresh, duplicates conflict
// (identical requests included) and nothing is adopted.
func TestCreateIdentity(t *testing.T) {
	ctx := context.Background()
	m, root := newManager(t)
	d := m.d
	fixed := time.Date(2026, 9, 30, 1, 2, 3, 456789, time.FixedZone("x", 3600))
	d.now = func() time.Time { return fixed }
	a := mustCreate(t, m, "alpha")
	b := mustCreate(t, m, "beta")
	if a.Instance == b.Instance || a.Generation == b.Generation || a.CreatedAt != "2026-09-30T00:02:03.000456789Z" {
		t.Fatalf("tokens %+v %+v", a, b)
	}
	if _, err := m.Create(ctx, "alpha"); codeOf(err) != contract.CodeConflict {
		t.Fatalf("duplicate: %v", err)
	}
	// A directory that is not a live workspace is never adopted.
	if err := os.Mkdir(filepath.Join(root, dirName, "gamma"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "gamma"); err == nil {
		t.Fatal("adopted an existing directory")
	}
	os.Remove(filepath.Join(root, dirName, "gamma"))
	// Every file is private and the layout is exact.
	want := []string{"identity.json", "CURRENT", "generations"}
	es, _ := os.ReadDir(filepath.Join(root, dirName, "alpha"))
	if len(es) != len(want) {
		t.Fatalf("layout %v", es)
	}
	filepath.Walk(filepath.Join(root, dirName), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s: %v mode %v", p, err, fi.Mode())
		}
		return nil
	})
	m.Close()
	m2 := openAt(t, fastDeps(), root)
	got, err := m2.Show(ctx, "alpha")
	if err != nil || got.Instance != a.Instance || got.CreatedAt != a.CreatedAt || got.Generation != a.Generation {
		t.Fatalf("restart %+v %v", got, err)
	}
}

// UT-1: list is sorted by ASCII name, paged by an exclusive cursor that
// need not exist, with next_after only when rows remain; empty is [].
func TestListPagination(t *testing.T) {
	m, _ := newManager(t)
	l, err := m.List("", 100)
	if err != nil || l.Workspaces == nil || len(l.Workspaces) != 0 || l.NextAfter != nil {
		t.Fatalf("empty %+v %v", l, err)
	}
	for _, n := range []string{"c", "a", "b0", "b", "z9"} {
		mustCreate(t, m, n)
	}
	var names []string
	after := ""
	for {
		p, err := m.List(after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range p.Workspaces {
			names = append(names, s.Name)
		}
		if p.NextAfter == nil {
			break
		}
		after = *p.NextAfter
	}
	if strings.Join(names, ",") != "a,b,b0,c,z9" {
		t.Fatalf("pages %v", names)
	}
	if p, _ := m.List("b00", 100); len(p.Workspaces) != 2 || p.Workspaces[0].Name != "c" {
		t.Fatalf("missing cursor %+v", p)
	}
}

// UT-1: size_bytes is exactly the sum of the workspace's regular files,
// including retained old generations; any unsafe entry fails the size.
func TestSizeArithmetic(t *testing.T) {
	m, root := newManager(t)
	v := mustCreate(t, m, "alpha")
	var sum int64
	filepath.Walk(filepath.Join(root, dirName, "alpha"), func(p string, fi os.FileInfo, err error) error {
		if fi.Mode().IsRegular() {
			sum += fi.Size()
		}
		return nil
	})
	if v.SizeBytes != sum {
		t.Fatalf("size %d, files %d", v.SizeBytes, sum)
	}
	extra := filepath.Join(root, dirName, "alpha", generationsName, strings.Repeat("0", 32))
	if err := os.MkdirAll(extra, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(extra, "x"), make([]byte, 1000), 0o600)
	if got, _ := m.Show(context.Background(), "alpha"); got.SizeBytes != sum+1000 {
		t.Fatalf("retained generation not counted: %d", got.SizeBytes)
	}
	os.Symlink("/etc/passwd", filepath.Join(extra, "link"))
	if _, err := m.Show(context.Background(), "alpha"); codeOf(err) != contract.CodeInternal {
		t.Fatalf("unsafe entry sized: %v", err)
	}
}

// UT-1: startup accepts a missing workspaces/ (existing installations) and
// rejects links, special files, unexpected entries and bad modes instead
// of treating them as an empty registry.
func TestStartupValidation(t *testing.T) {
	ctx := context.Background()
	d := fastDeps()
	root := tempRoot(t)
	if m, err := d.open(ctx, root, true); err != nil || len(m.live) != 0 {
		t.Fatalf("missing workspaces/: %v", err)
	}
	if _, err := Inspect(ctx, root); err != nil {
		t.Fatal(err)
	}
	build := func(t *testing.T) string {
		r := tempRoot(t)
		m := openAt(t, d, r)
		mustCreate(t, m, "alpha")
		m.Close()
		return r
	}
	ws := func(r string) string { return filepath.Join(r, dirName, "alpha") }
	gen := func(r string) string {
		b, _ := os.ReadFile(filepath.Join(ws(r), currentName))
		return filepath.Join(ws(r), generationsName, strings.TrimSpace(string(b)))
	}
	// aliased plants a main whose closure reaches one tree object as a
	// directory and as a regular file (every object present and hashing
	// correctly).
	aliased := func(dirFirst bool) func(t *testing.T, r string) {
		return func(t *testing.T, r string) {
			s := testkit.NewMemoryStore()
			c, _ := aliasedTreeCommit(t, s, dirFirst)
			plantObjects(t, r, "alpha", s)
			if err := fastDeps().writeRef(filepath.Join(gen(r), repoName), contract.DefaultBranchRef, c); err != nil {
				t.Fatal(err)
			}
		}
	}
	// stale adds a non-current copy of the active generation.
	stale := func(t *testing.T, r string) string {
		p := filepath.Join(ws(r), generationsName, strings.Repeat("f", 32))
		copyDir(t, gen(r), p)
		return p
	}
	// linkOut moves the valid directory p outside the workspace and links
	// it back in place.
	linkOut := func(t *testing.T, r, p string) {
		outside := filepath.Join(r, "elsewhere")
		if err := os.Rename(p, outside); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, p); err != nil {
			t.Fatal(err)
		}
	}
	// Generation entries and object closures (fixtures that need t).
	tcases := []struct {
		name   string
		break_ func(t *testing.T, r string)
	}{
		{"tree reused as a file", aliased(true)},
		{"tree reused as a directory", aliased(false)},
		{"symlinked active generation", func(t *testing.T, r string) { linkOut(t, r, gen(r)) }},
		{"active generation is a file", func(t *testing.T, r string) {
			g := gen(r)
			if err := os.RemoveAll(g); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(g, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"group-accessible active generation", func(t *testing.T, r string) {
			if err := os.Chmod(gen(r), 0o750); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlinked stale generation", func(t *testing.T, r string) { linkOut(t, r, stale(t, r)) }},
		{"stale generation is a file", func(t *testing.T, r string) {
			if err := os.WriteFile(filepath.Join(ws(r), generationsName, strings.Repeat("f", 32)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"group-accessible stale generation", func(t *testing.T, r string) {
			if err := os.Chmod(stale(t, r), 0o750); err != nil {
				t.Fatal(err)
			}
		}},
	}
	cases := []struct {
		name   string
		break_ func(r string)
	}{
		{"symlinked workspaces", func(r string) {
			os.Rename(filepath.Join(r, dirName), filepath.Join(r, "real"))
			os.Symlink(filepath.Join(r, "real"), filepath.Join(r, dirName))
		}},
		{"unexpected root entry", func(r string) { os.WriteFile(filepath.Join(r, dirName, "Bad"), nil, 0o600) }},
		{"unexpected workspace entry", func(r string) { os.WriteFile(filepath.Join(ws(r), "notes"), nil, 0o600) }},
		{"missing CURRENT", func(r string) { os.Remove(filepath.Join(ws(r), currentName)) }},
		{"malformed CURRENT", func(r string) { os.WriteFile(filepath.Join(ws(r), currentName), []byte("latest\n"), 0o600) }},
		{"CURRENT names nothing", func(r string) {
			os.WriteFile(filepath.Join(ws(r), currentName), []byte(strings.Repeat("a", 32)+"\n"), 0o600)
		}},
		{"identity of another name", func(r string) {
			b, _ := os.ReadFile(filepath.Join(ws(r), identityName))
			os.WriteFile(filepath.Join(ws(r), identityName), []byte(strings.Replace(string(b), `"alpha"`, `"beta"`, 1)), 0o600)
		}},
		{"group-readable file", func(r string) { os.Chmod(filepath.Join(ws(r), identityName), 0o640) }},
		{"symlink in repo", func(r string) { os.Symlink("/etc", filepath.Join(gen(r), repoName, "refs", "heads", "x")) }},
		{"packed-refs", func(r string) { os.WriteFile(filepath.Join(gen(r), repoName, "packed-refs"), nil, 0o600) }},
		{"hooks", func(r string) { os.Mkdir(filepath.Join(gen(r), repoName, "hooks"), 0o700) }},
		{"foreign config", func(r string) {
			os.WriteFile(filepath.Join(gen(r), repoName, "config"), []byte("[remote \"x\"]\n"), 0o600)
		}},
		{"tag ref", func(r string) {
			os.MkdirAll(filepath.Join(gen(r), repoName, "refs", "tags"), 0o700)
		}},
		{"nonportable stored ref", func(r string) {
			os.WriteFile(filepath.Join(gen(r), repoName, "refs", "heads", "Main"), []byte(strings.Repeat("a", 40)+"\n"), 0o600)
		}},
		{"symbolic ref", func(r string) {
			os.WriteFile(filepath.Join(gen(r), repoName, "refs", "heads", "main"), []byte("ref: refs/heads/x\n"), 0o600)
		}},
		{"dangling ref", func(r string) {
			os.WriteFile(filepath.Join(gen(r), repoName, "refs", "heads", "main"), []byte(strings.Repeat("a", 40)+"\n"), 0o600)
		}},
		{"orphan task metadata", func(r string) {
			os.WriteFile(filepath.Join(gen(r), refsName), encodeRefsDoc(map[string]taskMeta{contract.TaskRefPrefix + "t_" + strings.Repeat("0", 32): {Commit: strings.Repeat("a", 40), PublishedAt: "2026-09-30T00:00:00Z"}}), 0o600)
		}},
		{"unsafe staging", func(r string) {
			os.Mkdir(filepath.Join(r, dirName, createPrefix+strings.Repeat("1", 32)), 0o700)
			os.Symlink("/etc", filepath.Join(r, dirName, createPrefix+strings.Repeat("1", 32), "x"))
		}},
	}
	run := func(name, want string, break_ func(t *testing.T, r string)) {
		t.Run(name, func(t *testing.T) {
			r := build(t)
			break_(t, r)
			before := listFiles(t, r)
			m, err := d.open(ctx, r, true)
			if err == nil {
				m.Close()
				t.Fatal("startup accepted corrupt state")
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("startup failed for another reason: %v", err)
			}
			if _, err := Inspect(ctx, r); err == nil {
				t.Fatal("inspection accepted corrupt state")
			} else if !strings.Contains(err.Error(), want) {
				t.Fatalf("inspection failed for another reason: %v", err)
			}
			if after := listFiles(t, r); strings.Join(before, ",") != strings.Join(after, ",") {
				t.Fatalf("a failed startup changed state")
			}
		})
	}
	for _, c := range cases {
		run(c.name, "", func(_ *testing.T, r string) { c.break_(r) })
	}
	wants := map[string]string{
		"tree reused as a file":              "is a tree, want a blob",
		"tree reused as a directory":         "is a tree, want a blob",
		"symlinked active generation":        "is not a regular file or directory",
		"active generation is a file":        "a generation must be a directory",
		"group-accessible active generation": "must not be accessible by group or others",
		"symlinked stale generation":         "is not a regular file or directory",
		"stale generation is a file":         "a generation must be a directory",
		"group-accessible stale generation":  "must not be accessible by group or others",
	}
	for _, c := range tcases {
		run(c.name, wants[c.name], c.break_)
	}
	// Control: the planting fixture itself yields a valid workspace, so the
	// closure cases above fail on the aliased edge, not on the mechanics.
	t.Run("planted valid closure", func(t *testing.T) {
		r := build(t)
		s := testkit.NewMemoryStore()
		c := commit(t, s, seedFiles(), "valid")
		plantObjects(t, r, "alpha", s)
		if err := fastDeps().writeRef(filepath.Join(gen(r), repoName), contract.DefaultBranchRef, c); err != nil {
			t.Fatal(err)
		}
		m, err := d.open(ctx, r, true)
		if err != nil {
			t.Fatalf("a valid planted closure: %v", err)
		}
		m.Close()
	})
}

// UT-1: a workspace survives restart with its refs, and a removed name can
// be recreated as a new instance; a stale instance never touches it.
func TestRemoveRecreate(t *testing.T) {
	ctx := context.Background()
	m, root := newManager(t)
	a := mustCreate(t, m, "alpha")
	if _, err := m.Remove(ctx, "alpha", strings.Repeat("0", 32)); codeOf(err) != contract.CodeConflict {
		t.Fatalf("stale remove: %v", err)
	}
	if _, err := m.Remove(ctx, "nope", a.Instance); codeOf(err) != contract.CodeNotFound {
		t.Fatalf("missing remove: %v", err)
	}
	if r, err := m.Remove(ctx, "alpha", a.Instance); err != nil || !r.Removed || r.Instance != a.Instance {
		t.Fatalf("remove %+v %v", r, err)
	}
	if _, err := m.Show(ctx, "alpha"); codeOf(err) != contract.CodeNotFound {
		t.Fatalf("show removed: %v", err)
	}
	b := mustCreate(t, m, "alpha")
	if b.Instance == a.Instance {
		t.Fatal("instance reused")
	}
	if _, err := m.Remove(ctx, "alpha", a.Instance); codeOf(err) != contract.CodeConflict {
		t.Fatalf("old instance removed the new workspace: %v", err)
	}
	if _, err := m.Prune(ctx, "alpha", a.Instance, time.Now()); codeOf(err) != contract.CodeConflict {
		t.Fatalf("old instance pruned: %v", err)
	}
	if es, _ := os.ReadDir(filepath.Join(root, dirName)); len(es) != 1 {
		t.Fatalf("tombstones left: %v", es)
	}
}

// UT-1: concurrent create and remove of one name serialize on the registry
// lock: every outcome is a consistent sequence and the directory matches
// the registry afterwards.
func TestRemoveRecreateRace(t *testing.T) {
	ctx := context.Background()
	m, root := newManager(t)
	v := mustCreate(t, m, "alpha")
	var wg sync.WaitGroup
	var created int
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			m.Remove(ctx, "alpha", v.Instance)
		}()
		go func() {
			defer wg.Done()
			if _, err := m.Create(ctx, "alpha"); err == nil {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	_, live := m.lookup("alpha")
	_, statErr := os.Lstat(filepath.Join(root, dirName, "alpha"))
	if (live == nil) != (statErr == nil) || created > 1 {
		t.Fatalf("registry %v disk %v created %d", live, statErr, created)
	}
	m.Close()
	openAt(t, fastDeps(), root)
}

// UT-1 / review r1 C4: a create queued on the registry lock behind a
// namespace operation whose outcome turns ambiguous (a remove whose
// post-commit sync fails, fencing the manager) is refused once it gets the
// lock, before any filesystem mutation.
func TestCreateQueuedBehindFence(t *testing.T) {
	ctx := context.Background()
	root := tempRoot(t)
	d := fastDeps()
	m := openAt(t, d, root)
	v := mustCreate(t, m, "alpha")
	wsDir := filepath.Join(root, dirName)
	removeLocked, releaseRemove := make(chan struct{}), make(chan struct{})
	createQueued := make(chan struct{})
	var mu sync.Mutex
	failed, afterFence := false, []string{}
	fenced := false
	d.hook = func(stage string, _ context.Context) {
		switch stage {
		case "remove-locked":
			close(removeLocked)
			<-releaseRemove
		case "create-queued":
			close(createQueued)
		}
	}
	d.observe = func(op, p string) {
		mu.Lock()
		defer mu.Unlock()
		if fenced && mutating(op) {
			afterFence = append(afterFence, op+" "+p)
		}
	}
	// The remove's first sync of workspaces/ (after its tombstone rename,
	// the commit point) fails once: an ambiguous outcome.
	d.fail = func(op, p string) error {
		mu.Lock()
		defer mu.Unlock()
		if op == "dirsync" && p == wsDir && !failed {
			failed = true
			fenced = true
			return errors.New("injected")
		}
		return nil
	}
	removed := make(chan error, 1)
	go func() {
		_, err := m.Remove(ctx, "alpha", v.Instance)
		removed <- err
	}()
	<-removeLocked
	created := make(chan error, 1)
	go func() {
		_, err := m.Create(ctx, "beta")
		created <- err
	}()
	// The create has passed its pre-lock fence check and is about to wait
	// on the registry lock the remove holds.
	<-createQueued
	close(releaseRemove)
	if err := <-removed; codeOf(err) != contract.CodeInternal || !m.managerFenced() {
		t.Fatalf("ambiguous remove: %v (fenced %v)", err, m.managerFenced())
	}
	var err error
	select {
	case err = <-created:
	case <-time.After(10 * time.Second):
		t.Fatal("the queued create never returned")
	}
	if err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("queued create after the fence: %v", err)
	}
	mu.Lock()
	writes := append([]string(nil), afterFence...)
	mu.Unlock()
	if len(writes) != 0 {
		t.Fatalf("the queued create touched storage after the fence: %v", writes)
	}
	if _, lerr := m.lookup("beta"); lerr == nil {
		t.Fatal("the queued create registered its name")
	}
	es, rerr := os.ReadDir(wsDir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range es {
		if e.Name() == "beta" || strings.HasPrefix(e.Name(), createPrefix) {
			t.Fatalf("the queued create left %s", e.Name())
		}
	}
}

// UT-1: the registry and workspace locks are cancellable; a writer waits
// for readers and new readers wait for a waiting writer.
func TestLocks(t *testing.T) {
	var l rwLock
	ctx := context.Background()
	if err := l.RLock(ctx); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithCancel(ctx)
	got := make(chan error, 1)
	go func() { got <- l.Lock(wctx) }()
	for {
		l.mu.Lock()
		w := l.waiting
		l.mu.Unlock()
		if w == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	rctx, rcancel := context.WithTimeout(ctx, 20*time.Millisecond)
	if err := l.RLock(rctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reader overtook a waiting writer: %v", err)
	}
	rcancel()
	cancel()
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Fatalf("writer %v", err)
	}
	if err := l.RLock(ctx); err != nil {
		t.Fatalf("reader after a cancelled writer: %v", err)
	}
	l.RUnlock()
	l.RUnlock()
	if err := l.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	done, dcancel := context.WithCancel(ctx)
	dcancel()
	if err := l.RLock(done); !errors.Is(err, context.Canceled) {
		t.Fatalf("ended context acquired: %v", err)
	}
	if err := l.Lock(done); !errors.Is(err, context.Canceled) {
		t.Fatalf("ended context acquired: %v", err)
	}
	l.Unlock()
	mu := newCtxMutex()
	mu.Lock(ctx)
	tctx, tcancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer tcancel()
	if err := mu.Lock(tctx); err == nil {
		t.Fatal("registry lock acquired twice")
	}
	mu.Unlock()
	if err := mu.Lock(done); !errors.Is(err, context.Canceled) {
		t.Fatalf("ended context: %v", err)
	}
}

// UT-2: readers observe the old or the new generation during a mutation,
// never a partial one; a writer waits (cancellably) for readers.
func TestReadersDuringMutation(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t)
	g := serveGit(t, m)
	v := mustCreate(t, m, "alpha")
	s := testkit.NewMemoryStore()
	c0 := commit(t, s, seedFiles(), "c0")
	push(t, g.remote("alpha", v.Instance), s, "refs/heads/main", plumbing.ZeroHash, c0)
	entered, release := make(chan struct{}), make(chan struct{})
	m.d.hook = func(stage string, _ context.Context) {
		if stage == "refset-locked" {
			close(entered)
			<-release
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := setRef(m, "alpha", v.Instance, "b", contract.ExpectedAbsent, strp("main"), false)
		done <- err
	}()
	<-entered
	rctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := m.Show(rctx, "alpha"); codeOf(err) != contract.CodeUnavailable {
		t.Fatalf("reader during a mutation: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m.d.hook = nil
	if st, err := m.Status(ctx, "alpha", StatusQuery{Limit: 10}); err != nil || len(st.Refs) != 2 {
		t.Fatalf("after %+v %v", st, err)
	}
}

// UT-2 path budget: generated paths during create, copy, receive, ref set
// and prune stay within 994 bytes and 255-byte components (asserted in
// TestMaxPathLayout); a path over the budget is refused before the call.
func TestPathBudget(t *testing.T) {
	if err := checkPath("/" + strings.Repeat("a", 255)); err != nil {
		t.Fatal(err)
	}
	if err := checkPath("/" + strings.Repeat("a", 256)); !errors.Is(err, errPathBudget) {
		t.Fatalf("component: %v", err)
	}
	long := "/" + strings.Repeat(strings.Repeat("a", 200)+"/", 5)
	if err := checkPath(long[:MaxPathBytes]); err != nil {
		t.Fatalf("994: %v", err)
	}
	if err := checkPath(long[:MaxPathBytes+1]); !errors.Is(err, errPathBudget) {
		t.Fatalf("995: %v", err)
	}
	var called bool
	d := fastDeps()
	d.fail = func(string, string) error { called = true; return nil }
	if err := d.mkdir(long[:MaxPathBytes+1]); !errors.Is(err, errPathBudget) || called {
		t.Fatalf("over-budget mkdir attempted: %v", err)
	}
	if _, err := d.open(context.Background(), "/"+strings.Repeat("r", MaxRootBytes), true); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("257-byte root: %v", err)
	}
	if _, err := Inspect(context.Background(), "/"+strings.Repeat("r", MaxRootBytes)); err == nil {
		t.Fatal("inspect over-long root")
	}
}
