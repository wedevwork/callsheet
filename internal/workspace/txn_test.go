package workspace

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// copyDir copies a stopped state root with modes preserved (fixtures).
func copyDir(t testing.TB, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(target, fi.Mode().Perm())
		case fi.Mode().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, fi.Mode().Perm())
			if err != nil {
				return err
			}
			_, err = io.Copy(out, in)
			return errors.Join(err, out.Close())
		}
		return fmt.Errorf("unexpected %s", p)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// currentGen returns the generation directory named by CURRENT.
func currentGen(t testing.TB, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, dirName, name, currentName))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, dirName, name, generationsName, strings.TrimSpace(string(b)))
}

// plantObjects copies every object of src into a stopped workspace's
// current generation through the private storage layer.
func plantObjects(t testing.TB, root, name string, src storer.EncodedObjectStorer) {
	t.Helper()
	s, fs := openStorage(filepath.Join(currentGen(t, root, name), repoName), fastDeps())
	defer closeStorage(s, fs)
	iter, err := src.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		t.Fatal(err)
	}
	err = iter.ForEach(func(o plumbing.EncodedObject) error {
		_, err := s.SetEncodedObject(o)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// plantTask writes a task ref and its publication metadata into a stopped
// workspace's current generation (test fixtures only: no public API
// creates task refs in 09a).
func plantTask(t testing.TB, root, name, id string, c plumbing.Hash, published time.Time) {
	t.Helper()
	gen := currentGen(t, root, name)
	d := fastDeps()
	ref := contract.TaskRefPrefix + id
	if err := d.writeRef(filepath.Join(gen, repoName), ref, c); err != nil {
		t.Fatal(err)
	}
	tasks, err := readRefsDoc(filepath.Join(gen, refsName))
	if err != nil {
		t.Fatal(err)
	}
	tasks[ref] = taskMeta{Commit: c.String(), PublishedAt: contract.FormatTime(published)}
	os.Remove(filepath.Join(gen, refsName))
	if err := d.writeFile(filepath.Join(gen, refsName), encodeRefsDoc(tasks)); err != nil {
		t.Fatal(err)
	}
}

// snapshot is the restart-observed state: every workspace's refs.
func snapshot(t testing.TB, root string) string {
	t.Helper()
	m, err := fastDeps().open(context.Background(), root, true)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer m.Close()
	var out []string
	for n, h := range m.live {
		refs, err := readRefs(h.repo())
		if err != nil {
			t.Fatal(err)
		}
		var rs []string
		for r, c := range refs {
			rs = append(rs, r+"="+c.String()[:8])
		}
		sort.Strings(rs)
		out = append(out, n+"{"+strings.Join(rs, ",")+"}")
		// Recovery left exactly the current generation.
		if es, _ := os.ReadDir(filepath.Join(h.dir, generationsName)); len(es) != 1 {
			t.Fatalf("%s holds %d generations after recovery", n, len(es))
		}
		if _, err := os.Lstat(filepath.Join(h.dir, currentTmpName)); err == nil {
			t.Fatalf("%s kept its CURRENT temporary", n)
		}
	}
	es, _ := os.ReadDir(filepath.Join(root, dirName))
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("recovery kept staging %s", e.Name())
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// mutating reports whether op is a failure-injection boundary.
func mutating(op string) bool {
	switch op {
	case "mkdir", "create", "write", "close", "sync", "dirsync", "rename", "remove":
		return true
	}
	return false
}

type durabilityCase struct {
	name string
	// op performs the mutation against m (served by g) and reports its
	// outcome.
	op func(m *Manager, g *gitServer, inst string) error
	// commit reports whether a recorded boundary is the commit point.
	commit func(op, path string) bool
	// managerFence is true for namespace operations (create, remove).
	managerFence bool
}

// UT-2: a failure injected at every write, close, sync, rename and remove
// boundary of create, remove, ref set, prune and receive never reports
// success; before the commit point the running manager keeps the old
// state, after it the workspace (or manager) is fenced; a restart always
// observes the old or the new complete generation, with no leftovers.
func TestDurabilityBoundaries(t *testing.T) {
	ctx := context.Background()
	template := tempRoot(t)
	tm := openAt(t, fastDeps(), template)
	g := serveGit(t, tm)
	v := mustCreate(t, tm, "alpha")
	local := testkit.NewMemoryStore()
	c0 := commit(t, local, seedFiles(), "c0")
	push(t, g.remote("alpha", v.Instance), local, "refs/heads/main", plumbing.ZeroHash, c0)
	files := seedFiles()
	files["new.txt"] = testkit.FileSpec{Mode: 0o100644, Content: []byte("new\n")}
	c1 := commit(t, local, files, "c1", c0)
	tm.Close()
	plantTask(t, template, "alpha", "t_"+strings.Repeat("a", 32), c0, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	isCurrent := func(op, p string) bool {
		return op == "rename" && strings.HasSuffix(p, string(filepath.Separator)+currentName)
	}
	cases := []durabilityCase{
		{name: "create", managerFence: true,
			op: func(m *Manager, _ *gitServer, _ string) error { _, err := m.Create(ctx, "beta"); return err },
			commit: func(op, p string) bool {
				return op == "rename" && strings.HasSuffix(p, string(filepath.Separator)+"beta")
			}},
		{name: "remove", managerFence: true,
			op: func(m *Manager, _ *gitServer, inst string) error { _, err := m.Remove(ctx, "alpha", inst); return err },
			commit: func(op, p string) bool {
				return op == "rename" && strings.Contains(p, removedPrefix)
			}},
		{name: "ref set", commit: isCurrent,
			op: func(m *Manager, _ *gitServer, inst string) error {
				_, err := setRef(m, "alpha", inst, "topic", contract.ExpectedAbsent, strp("main"), false)
				return err
			}},
		{name: "ref delete", commit: isCurrent,
			op: func(m *Manager, _ *gitServer, inst string) error {
				_, err := setRef(m, "alpha", inst, "main", c0.String(), nil, true)
				return err
			}},
		{name: "prune", commit: isCurrent,
			op: func(m *Manager, _ *gitServer, inst string) error {
				_, err := m.Prune(ctx, "alpha", inst, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
				return err
			}},
		{name: "receive", commit: isCurrent,
			op: func(m *Manager, _ *gitServer, inst string) error {
				res := receiveDirect(t, m, "alpha", inst, cmd("refs/heads/main", c0, c1), packOf(t, local, objectsFor(t, local, c1, c0)...))
				if !res.ok() {
					return errors.New(res.String())
				}
				return nil
			}},
	}
	oldState := snapshot(t, copyTemplate(t, template))
	stride, offset := boundarySample()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A successful run records the boundaries and the new state.
			root := copyTemplate(t, template)
			recRoot := root
			rec := &recorder{}
			d := fastDeps()
			d.observe = rec.observe
			m := openAt(t, d, root)
			d.observe = nil
			rec.ops = nil
			d.observe = rec.observe
			if err := c.op(m, nil, v.Instance); err != nil {
				t.Fatalf("successful run: %v", err)
			}
			d.observe = nil
			m.Close()
			newState := snapshot(t, root)
			if newState == oldState {
				t.Fatalf("the operation changed nothing: %s", newState)
			}
			var bounds []string
			commitAt := -1
			for _, o := range rec.all() {
				op, p, _ := strings.Cut(o, " ")
				if !mutating(op) {
					continue
				}
				if c.commit(op, p) {
					commitAt = len(bounds)
				}
				bounds = append(bounds, o)
			}
			if commitAt < 0 {
				t.Fatalf("no commit point among %d boundaries", len(bounds))
			}
			for i := range bounds {
				if i%stride != offset {
					continue
				}
				root := copyTemplate(t, template)
				d := fastDeps()
				// The library's pack writer creates files on its own
				// goroutine, so a boundary is addressed by its normalized
				// name and occurrence, not by a global index.
				target := normalize(bounds[i], recRoot)
				occurrence := 0
				for _, b := range bounds[:i] {
					if normalize(b, recRoot) == target {
						occurrence++
					}
				}
				var mu sync.Mutex
				seen := 0
				injected := errors.New("injected failure at " + bounds[i])
				hit := ""
				d.fail = func(op, p string) error {
					if !mutating(op) || normalize(op+" "+p, root) != target {
						return nil
					}
					mu.Lock()
					defer mu.Unlock()
					seen++
					if seen == occurrence+1 {
						hit = op + " " + p
						return injected
					}
					return nil
				}
				m := openAt(t, d, root)
				err := c.op(m, nil, v.Instance)
				d.fail = nil
				mu.Lock()
				h := hit
				mu.Unlock()
				if normalize(h, root) != target {
					t.Fatalf("boundary %d: injected at %q, recorded %q", i, h, bounds[i])
				}
				if err == nil {
					t.Fatalf("boundary %d (%s): success reported after a failure", i, bounds[i])
				}
				fenced := m.managerFenced()
				if h, lerr := m.lookup("alpha"); lerr == nil {
					fenced = fenced || h.fenced.Load()
				}
				if i <= commitAt && fenced {
					t.Fatalf("boundary %d (%s) before the commit point fenced the workspace", i, bounds[i])
				}
				if i > commitAt && !fenced {
					t.Fatalf("boundary %d (%s) after the commit point left the workspace unfenced", i, bounds[i])
				}
				m.Close()
				got := snapshot(t, root)
				switch {
				case i <= commitAt && got != oldState:
					t.Fatalf("boundary %d (%s) before the commit point changed state: %s", i, bounds[i], got)
				case i > commitAt && got != newState:
					t.Fatalf("boundary %d (%s) after the commit point lost the commit: %s", i, bounds[i], got)
				}
			}
			t.Logf("%s: %d boundaries, commit point %d", c.name, len(bounds), commitAt)
		})
	}
}

var (
	tokenRE = regexp.MustCompile(`[0-9a-f]{32}`)
	tempRE  = regexp.MustCompile(`(tmp_[a-z]+_)[0-9]+`)
)

// normalize masks a boundary's root, random tokens and temp suffixes.
func normalize(boundary, root string) string {
	s := strings.Replace(boundary, root, "<root>", 1)
	s = tempRE.ReplaceAllString(s, "${1}N")
	return tokenRE.ReplaceAllString(s, "<tok>")
}

// durabilityRuns counts TestDurabilityBoundaries invocations in this
// process.
var durabilityRuns atomic.Int64

// boundarySample returns which boundaries this invocation injects: all of
// them in an ordinary run; under a repeated run (-count=N, the stress
// shard, which repeats N times at each CPU setting) every N-th boundary
// with an offset rotating per repetition, so the N repetitions at each CPU
// setting together inject every boundary exactly once.
func boundarySample() (stride, offset int) {
	run := int(durabilityRuns.Add(1) - 1)
	count := 1
	if f := flag.Lookup("test.count"); f != nil {
		if n, err := strconv.Atoi(f.Value.String()); err == nil && n > 1 {
			count = n
		}
	}
	return count, run % count
}

func copyTemplate(t testing.TB, template string) string {
	t.Helper()
	root := filepath.Join(tempRoot(t), "state")
	copyDir(t, template, root)
	return root
}

// UT-2: startup removes recognized leftovers (non-current generations, a
// CURRENT temporary, create staging, removal tombstones) and syncs, never
// choosing a generation by name or mtime; a cleanup failure fails startup.
func TestStartupCleanup(t *testing.T) {
	ctx := context.Background()
	root := tempRoot(t)
	m := openAt(t, fastDeps(), root)
	mustCreate(t, m, "alpha")
	m.Close()
	ws := filepath.Join(root, dirName, "alpha")
	cur := currentGen(t, root, "alpha")
	newer := filepath.Join(ws, generationsName, strings.Repeat("f", 32))
	copyDir(t, cur, newer)
	future := time.Now().Add(time.Hour)
	os.Chtimes(newer, future, future)
	os.WriteFile(filepath.Join(ws, currentTmpName), []byte(strings.Repeat("f", 32)+"\n"), 0o600)
	os.MkdirAll(filepath.Join(root, dirName, createPrefix+strings.Repeat("1", 32), "x"), 0o700)
	os.MkdirAll(filepath.Join(root, dirName, removedPrefix+strings.Repeat("2", 32)), 0o700)
	if n, err := Inspect(ctx, root); err != nil || n != 1 {
		t.Fatalf("inspection of leftovers: %d %v", n, err)
	}
	if _, err := os.Lstat(newer); err != nil {
		t.Fatal("inspection removed a leftover")
	}
	d := fastDeps()
	d.fail = func(op, p string) error {
		if op == "remove" && strings.Contains(p, createPrefix) {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := d.open(ctx, root, true); codeOf(err) != contract.CodeInternal {
		t.Fatalf("cleanup failure: %v", err)
	}
	var synced []string
	d = fastDeps()
	d.observe = func(op, p string) {
		if op == "dirsync" {
			synced = append(synced, p)
		}
	}
	m2 := openAt(t, d, root)
	h, _ := m2.lookup("alpha")
	if filepath.Join(ws, generationsName, h.gen) != cur {
		t.Fatal("startup chose another generation")
	}
	if _, err := os.Lstat(newer); err == nil {
		t.Fatal("stale generation kept")
	}
	if len(synced) < 3 {
		t.Fatalf("affected directories not synced: %v", synced)
	}
	snapshot(t, root)
}

// UT-2 / review r1 C1: generations/ is synced twice around a publication:
// after the new generation's own tree and before the CURRENT rename (so a
// durable CURRENT never names a generation whose directory entry could be
// lost), and after the old generation's deletion. A failure at the first
// keeps the old generation authoritative and the workspace unfenced; a
// failure at the second keeps the commit and fences.
func TestGenerationEntrySync(t *testing.T) {
	build := func(t *testing.T) (string, *deps, *Manager, string, plumbing.Hash) {
		root := tempRoot(t)
		d := fastDeps()
		m := openAt(t, d, root)
		v := mustCreate(t, m, "alpha")
		s := testkit.NewMemoryStore()
		c0 := commit(t, s, seedFiles(), "c0")
		push(t, serveGit(t, m).remote("alpha", v.Instance), s, "refs/heads/main", plumbing.ZeroHash, c0)
		return root, d, m, v.Instance, c0
	}
	mutate := func(m *Manager, inst string) error {
		_, err := setRef(m, "alpha", inst, "topic", contract.ExpectedAbsent, strp("main"), false)
		return err
	}
	t.Run("order", func(t *testing.T) {
		root, d, m, inst, _ := build(t)
		ws := filepath.Join(root, dirName, "alpha")
		gens := filepath.Join(ws, generationsName)
		oldGen := filepath.Join(gens, genOf(t, m, "alpha"))
		rec := &recorder{}
		d.observe = rec.observe
		if err := mutate(m, inst); err != nil {
			t.Fatal(err)
		}
		d.observe = nil
		newGen := filepath.Join(gens, genOf(t, m, "alpha"))
		newSynced, preSync, rename, oldRemoved, postSync := -1, -1, -1, -1, -1
		for i, o := range rec.all() {
			switch {
			case o == "dirsync "+newGen:
				newSynced = i
			case o == "dirsync "+gens && rename < 0 && newSynced >= 0:
				preSync = i
			case o == "rename "+filepath.Join(ws, currentName):
				rename = i
			case o == "remove "+oldGen:
				oldRemoved = i
			case o == "dirsync "+gens && oldRemoved >= 0 && postSync < 0:
				postSync = i
			}
		}
		if newSynced < 0 || preSync < newSynced || rename < preSync {
			t.Fatalf("generations/ not synced between the new generation (%d) and the CURRENT rename (%d): %d", newSynced, rename, preSync)
		}
		if oldRemoved < rename || postSync < oldRemoved {
			t.Fatalf("generations/ not synced after deleting the old generation (%d): %d", oldRemoved, postSync)
		}
	})
	// inject fails the n-th sync of generations/ during the mutation.
	inject := func(t *testing.T, n int) (*Manager, string, string, string, error) {
		root, d, m, inst, _ := build(t)
		gens := filepath.Join(root, dirName, "alpha", generationsName)
		before := genOf(t, m, "alpha")
		seen := 0
		d.fail = func(op, p string) error {
			if op == "dirsync" && p == gens {
				if seen++; seen == n {
					return errors.New("injected")
				}
			}
			return nil
		}
		err := mutate(m, inst)
		d.fail = nil
		return m, root, before, gens, err
	}
	t.Run("failure before publication", func(t *testing.T) {
		m, root, before, gens, err := inject(t, 1)
		if err == nil {
			t.Fatal("success reported after a failed generations/ sync")
		}
		h, _ := m.lookup("alpha")
		if h.fenced.Load() || m.managerFenced() || h.gen != before {
			t.Fatalf("pre-publication failure: fenced %v, generation %s (was %s)", h.fenced.Load(), h.gen, before)
		}
		if es, _ := os.ReadDir(gens); len(es) != 1 || es[0].Name() != before {
			t.Fatalf("the unpublished generation was kept: %v", es)
		}
		m.Close()
		if refs := snapshot(t, root); strings.Contains(refs, "refs/heads/topic") {
			t.Fatalf("restart observed the unpublished ref: %s", refs)
		}
	})
	t.Run("failure after cleanup", func(t *testing.T) {
		m, root, before, _, err := inject(t, 2)
		if err == nil {
			t.Fatal("success reported after a failed generations/ sync")
		}
		h, _ := m.lookup("alpha")
		if !h.fenced.Load() || h.gen == before {
			t.Fatalf("post-cleanup failure: fenced %v, generation %s (was %s)", h.fenced.Load(), h.gen, before)
		}
		m.Close()
		if refs := snapshot(t, root); !strings.Contains(refs, "refs/heads/topic") {
			t.Fatalf("restart lost the committed ref: %s", refs)
		}
	})
}

// UT-2: acknowledgement waits for real durability: with the production
// sync functions every published file and directory is synced before the
// CURRENT rename and the workspace directory after it.
func TestRealSyncOrder(t *testing.T) {
	d := defaultDeps()
	var order []string
	d.observe = func(op, p string) { order = append(order, op+" "+p) }
	root := tempRoot(t)
	m := openAt(t, d, root)
	v := mustCreate(t, m, "alpha")
	order = nil
	if _, err := setRef(m, "alpha", v.Instance, "main", contract.ExpectedAbsent, strp(strings.Repeat("0", 40)), false); codeOf(err) != contract.CodeNotFound {
		t.Fatalf("unreachable hash: %v", err)
	}
	g := serveGit(t, m)
	s := testkit.NewMemoryStore()
	c0 := commit(t, s, seedFiles(), "c0")
	push(t, g.remote("alpha", v.Instance), s, "refs/heads/main", plumbing.ZeroHash, c0)
	renameAt, lastSync, dirAfter := -1, -1, -1
	for i, o := range order {
		switch {
		case strings.HasPrefix(o, "rename ") && strings.HasSuffix(o, currentName):
			renameAt = i
		case renameAt < 0 && (strings.HasPrefix(o, "sync ") || strings.HasPrefix(o, "dirsync ")):
			lastSync = i
		case renameAt >= 0 && dirAfter < 0 && o == "dirsync "+filepath.Join(root, dirName, "alpha"):
			dirAfter = i
		}
	}
	if renameAt < 0 || lastSync < 0 || lastSync > renameAt || dirAfter < renameAt {
		t.Fatalf("sync order: rename %d last sync %d dir sync %d", renameAt, lastSync, dirAfter)
	}
}
