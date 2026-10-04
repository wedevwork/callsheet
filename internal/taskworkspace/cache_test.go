package taskworkspace

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestCache is UT-B3 (iteration 10b): the shared per-node cache's locking,
// independent copies, corruption versus corrupt responses, the single
// fresh retry, idle eviction and the oversize bypass, storage failures and
// startup recovery. Delegated from tests/function
// (TestTaskWorkspaceCache/matrix). Do not rename it.
func TestCache(t *testing.T) {
	t.Parallel()
	t.Run("cold-then-warm", func(t *testing.T) {
		src := newSource(t)
		c0 := src.commit(spec("a", strings.Repeat("a", 4096), "b", "b"))
		c1 := src.commit(spec("a", strings.Repeat("a", 4096), "b", "b2"), c0)
		m, _ := openCache(t, 0)
		f := &fetcher{src: src}
		db := newTaskDB(t)
		st, err := m.Fetch(context.Background(), "proj", instA, c0, f, db)
		if err != nil || st.Hit || st.Fetched == 0 || st.Copied == 0 || st.Bypassed {
			t.Fatalf("cold %+v %v", st, err)
		}
		hasClosure(t, db, c0)
		// The next base reuses the verified cached closure as haves.
		db2 := newTaskDB(t)
		st2, err := m.Fetch(context.Background(), "proj", instA, c1, f, db2)
		if err != nil || !st2.Hit || st2.Saved == 0 || st2.Fetched >= st.Fetched || len(f.calls[1]) != 1 || f.calls[1][0] != c0 {
			t.Fatalf("warm %+v %v (haves %v)", st2, err, f.calls)
		}
		hasClosure(t, db2, c1)
		// Each task gets its own copy: no object is shared with the cache.
		if db.Dir() == db2.Dir() || m.Size() <= 0 {
			t.Fatal("copies are not independent")
		}
		// A repeated base still makes one authorized round trip.
		db3 := newTaskDB(t)
		if _, err := m.Fetch(context.Background(), "proj", instA, c1, f, db3); err != nil || f.nCalls() != 3 {
			t.Fatalf("repeat %v (%d calls)", err, f.nCalls())
		}
		hasClosure(t, db3, c1)
	})
	t.Run("concurrency", func(t *testing.T) {
		src := newSource(t)
		c0 := src.commit(spec("x", "x"))
		m, _ := openCache(t, 0)
		entered := make(chan string, 4)
		release := make(chan struct{})
		f := &fetcher{src: src, before: func(n int, ctx context.Context) error {
			entered <- "in"
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		// Different instances proceed independently.
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, inst := range []string{instA, instB} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := m.Fetch(context.Background(), "proj", inst, c0, f, newTaskDB(t))
				errs <- err
			}()
		}
		for i := 0; i < 2; i++ {
			within(t, entered, "two instances fetching concurrently")
		}
		// The same instance waits for the lock, and its wait is cancellable.
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := m.Fetch(ctx, "proj", instA, c0, f, newTaskDB(t))
			done <- err
		}()
		absentFor(t, entered, time.After(20*time.Millisecond), "a second fetch of the same instance ran while the first held its lock")
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled lock wait: %v", err)
		}
		close(release)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if f.nCalls() != 2 {
			t.Fatalf("%d fetches, want 2", f.nCalls())
		}
		// A cancelled context before the lock never fetches.
		if _, err := m.Fetch(ctx, "proj", instA, c0, f, newTaskDB(t)); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("stalled-peer", func(t *testing.T) {
		// UT-B8: a peer that stops mid-pack. Cancelling the fetch ends the
		// blocked body read; Fetch returns the cancellation only after its
		// staging is gone, and the instance lock is free again.
		src := newSource(t)
		c0 := src.commit(spec("x", "x"))
		m, root := openCache(t, 0)
		f := &fetcher{src: src, stall: func(n int) bool { return n == 0 }, parked: make(chan struct{}, 1)}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := m.Fetch(ctx, "proj", instA, c0, f, newTaskDB(t))
			done <- err
		}()
		within(t, f.parked, "the fetch blocking in the stalled body")
		cancel()
		if err := within(t, done, "the cancelled fetch's return (not blocked in its stalled peer)"); !errors.Is(err, context.Canceled) {
			t.Fatalf("stalled fetch: %v", err)
		}
		entries, _ := os.ReadDir(filepath.Join(root, CacheDirName, instA))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), stagePrefix) {
				t.Fatalf("staging %s left after a cancelled fetch", e.Name())
			}
		}
		if _, err := m.Fetch(context.Background(), "proj", instA, c0, f, newTaskDB(t)); err != nil {
			t.Fatalf("fetch after a cancelled stalled one: %v", err)
		}
	})
	t.Run("corrupt-cache", func(t *testing.T) {
		src := newSource(t)
		c0 := src.commit(spec("x", strings.Repeat("x", 1000)))
		m, root := openCache(t, 0)
		f := &fetcher{src: src}
		if _, err := m.Fetch(context.Background(), "proj", instA, c0, f, newTaskDB(t)); err != nil {
			t.Fatal(err)
		}
		if corruptDir(t, filepath.Join(root, CacheDirName, instA, currentName)) == 0 {
			t.Fatal("no cached objects")
		}
		db := newTaskDB(t)
		st, err := m.Fetch(context.Background(), "proj", instA, c0, f, db)
		if err != nil || !st.Purged || st.Hit || len(f.calls[1]) != 0 {
			t.Fatalf("corrupt cache %+v %v haves %v", st, err, f.calls)
		}
		hasClosure(t, db, c0)
		// A corrupt cache.json is an unusable entry too.
		os.WriteFile(filepath.Join(root, CacheDirName, instA, cacheMetaName), []byte("{"), 0o600)
		if st, err := m.Fetch(context.Background(), "proj", instA, c0, f, newTaskDB(t)); err != nil || !st.Purged {
			t.Fatalf("corrupt meta %+v %v", st, err)
		}
	})
	t.Run("corrupt-response", func(t *testing.T) {
		src := newSource(t)
		c0 := src.commit(spec("x", "1", "y", "2"))
		c1 := src.commit(spec("x", "1", "y", "3", "z", "4"), c0)
		cm, _ := object.GetCommit(src.s, c1)
		m, _ := openCache(t, 0)
		f := &fetcher{src: src}
		// A fresh response missing its tree fails, without a retry.
		f.omit = func(n int, h plumbing.Hash) bool { return h == cm.TreeHash }
		if _, err := m.Fetch(context.Background(), "proj", instA, c1, f, newTaskDB(t)); !errors.Is(err, errCorruptResponse) || f.nCalls() != 1 {
			t.Fatalf("corrupt fresh response: %v (%d calls)", err, f.nCalls())
		}
		// With cached haves, an incomplete combined closure purges the
		// entry and tries exactly one fresh fetch without haves.
		f.omit = nil
		if _, err := m.Fetch(context.Background(), "proj", instA, c0, f, newTaskDB(t)); err != nil {
			t.Fatal(err)
		}
		f.calls = nil
		f.omit = func(n int, h plumbing.Hash) bool { return n == 0 && h == cm.TreeHash }
		db := newTaskDB(t)
		st, err := m.Fetch(context.Background(), "proj", instA, c1, f, db)
		if err != nil || !st.Retried || st.Hit || f.nCalls() != 2 || len(f.calls[1]) != 0 {
			t.Fatalf("retry %+v %v (haves %v)", st, err, f.calls)
		}
		hasClosure(t, db, c1)
		// A garbage pack is a fetch failure.
		f.omit, f.garbage = nil, func(int) bool { return true }
		if _, err := m.Fetch(context.Background(), "proj", instB, c0, f, newTaskDB(t)); err == nil {
			t.Fatal("a garbage pack was accepted")
		}
	})
	t.Run("denial-not-retried", func(t *testing.T) {
		src := newSource(t)
		c0 := src.commit(spec("x", "1"))
		m, _ := openCache(t, 0)
		f := &fetcher{src: src}
		if _, err := m.Fetch(context.Background(), "proj", instA, c0, f, newTaskDB(t)); err != nil {
			t.Fatal(err)
		}
		f.before = func(int, context.Context) error { return errDenied }
		if _, err := m.Fetch(context.Background(), "proj", instA, c0, f, newTaskDB(t)); !errors.Is(err, errDenied) || f.nCalls() != 2 {
			t.Fatalf("denial: %v (%d calls)", err, f.nCalls())
		}
	})
	t.Run("eviction", func(t *testing.T) {
		src := newSource(t)
		big := noise(64<<10, 1)
		ca := src.commit(spec("a", big+"a"))
		cb := src.commit(spec("b", big+"b"))
		cc := src.commit(spec("c", big+"c"))
		m, root := openCache(t, 100<<10)
		l := hookOf(m)
		f := &fetcher{src: src}
		if _, err := m.Fetch(context.Background(), "proj", instA, ca, f, newTaskDB(t)); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Fetch(context.Background(), "proj", instB, cb, f, newTaskDB(t)); err != nil {
			t.Fatal(err)
		}
		// The least recently used idle entry goes.
		l.await(t, "evicted "+instA)
		if _, err := os.Stat(filepath.Join(root, CacheDirName, instA)); !os.IsNotExist(err) || m.Size() > 100<<10 {
			t.Fatalf("A not evicted (%v), size %d", err, m.Size())
		}
		// A busy entry is not idle: B's lock is held while C's fetch evicts.
		eb := m.entryFor(instB)
		eb.lock <- struct{}{}
		instC := strings.Repeat("c", 32)
		if _, err := m.Fetch(context.Background(), "proj", instC, cc, f, newTaskDB(t)); err != nil {
			t.Fatal(err)
		}
		if l.has("evicted " + instB) {
			t.Fatal("a busy entry was evicted")
		}
		<-eb.lock
		// A closure above the target is served privately and not retained.
		m2, root2 := openCache(t, 1)
		db := newTaskDB(t)
		st, err := m2.Fetch(context.Background(), "proj", instA, ca, f, db)
		if err != nil || !st.Bypassed {
			t.Fatalf("oversize %+v %v", st, err)
		}
		hasClosure(t, db, ca)
		if _, err := os.Stat(filepath.Join(root2, CacheDirName, instA, currentName)); !os.IsNotExist(err) {
			t.Fatal("an oversize closure was retained")
		}
	})
	t.Run("storage-failures", func(t *testing.T) {
		src := newSource(t)
		c0 := src.commit(spec("x", "1"))
		m, root := openCache(t, 0)
		f := &fetcher{src: src}
		boom := errors.New("disk failure (fixture)")
		// A failed replacement keeps the task's own copy.
		m.fail = func(op string) error {
			if op == "cache-replace" {
				return boom
			}
			return nil
		}
		db := newTaskDB(t)
		if _, err := m.Fetch(context.Background(), "proj", instA, c0, f, db); err != nil {
			t.Fatal(err)
		}
		hasClosure(t, db, c0)
		// A failed staging creation fails the fetch.
		m.fail = func(op string) error {
			if op == "cache-stage" {
				return boom
			}
			return nil
		}
		if _, err := m.Fetch(context.Background(), "proj", instB, c0, f, newTaskDB(t)); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		// An entry that cannot be removed safely is disabled and bypassed.
		m.fail = nil
		if _, err := m.Fetch(context.Background(), "proj", instB, c0, f, newTaskDB(t)); err != nil {
			t.Fatal(err)
		}
		corruptDir(t, filepath.Join(root, CacheDirName, instB, currentName))
		m.fail = func(op string) error {
			if op == "cache-remove" {
				return boom
			}
			return nil
		}
		if _, err := m.Fetch(context.Background(), "proj", instB, c0, f, newTaskDB(t)); err != nil {
			t.Fatal(err)
		}
		m.fail = nil
		st, err := m.Fetch(context.Background(), "proj", instB, c0, f, newTaskDB(t))
		if err != nil || !st.Bypassed {
			t.Fatalf("disabled entry %+v %v", st, err)
		}
		// A failed metadata write keeps the task's copy too.
		m.fail = func(op string) error {
			if op == "cache-meta" {
				return boom
			}
			return nil
		}
		instC := strings.Repeat("c", 32)
		if _, err := m.Fetch(context.Background(), "proj", instC, c0, f, newTaskDB(t)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("compressed-copy", func(t *testing.T) {
		// Iteration 10b r0.5: real cold and warm Prepare copies. Once the
		// cache holds loose objects, every leg (cache to staging, staging to
		// the task database, the task database to the checkout) copies their
		// compressed bytes verbatim: the cache's objects are re-encoded as
		// stored (level 0) zlib blocks, which the deflater never produces,
		// and exactly those bytes arrive in the task database and the
		// checkout's .git/objects, hashes unchanged.
		src := newSource(t)
		c0 := src.commit(spec("a", strings.Repeat("compressed copy\n", 512), "d/b", "bee"))
		m, root := openCache(t, 0)
		f := &fetcher{src: src}
		_, cold := prepared(t, m, f, binding(c0), false)
		// The first warm run copies the cached pack into loose objects.
		prepared(t, m, f, binding(c0), false)
		cur := filepath.Join(root, CacheDirName, instA, currentName, "objects")
		stored := restoreLoose(t, cur)
		if len(stored) == 0 {
			t.Fatal("the cache holds no loose objects after a warm run")
		}
		dir, warm := prepared(t, m, f, binding(c0), false)
		if !warm.Stats.Hit || warm.Stats.Retried || warm.Stats.Purged || warm.Stats.Copied != cold.Stats.Copied {
			t.Fatalf("warm %+v, cold %+v", warm.Stats, cold.Stats)
		}
		for name, want := range stored {
			for _, p := range []string{filepath.Join(dir, ObjectsName, "objects", name), filepath.Join(dir, WorkName, ".git", "objects", name)} {
				if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s not copied verbatim: %v", p, err)
				}
			}
		}
		// Tampering: a cached loose object that is a link (readable through
		// the generic storage, which follows it) is refused by the copy, not
		// followed, and is cache corruption: the entry is purged and one
		// fresh fetch without haves serves the task.
		var victim string
		for name := range stored {
			victim = name
			break
		}
		p := filepath.Join(cur, victim)
		elsewhere := filepath.Join(t.TempDir(), "obj")
		os.WriteFile(elsewhere, stored[victim], 0o444)
		os.Chmod(filepath.Dir(p), 0o755)
		os.Remove(p)
		if err := os.Symlink(elsewhere, p); err != nil {
			t.Fatal(err)
		}
		f.calls = nil
		dir2, tampered := prepared(t, m, f, binding(c0), false)
		if !tampered.Stats.Retried || tampered.Stats.Hit || f.nCalls() != 2 || len(f.calls[0]) != 1 || len(f.calls[1]) != 0 {
			t.Fatalf("tampered cache %+v (haves %v)", tampered.Stats, f.calls)
		}
		if fi, err := os.Lstat(filepath.Join(dir2, ObjectsName, "objects", victim)); err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("the task's object is not an independent regular file: %v", err)
		}
	})
	t.Run("startup", func(t *testing.T) {
		src := newSource(t)
		c0 := src.commit(spec("x", "1"))
		m, root := openCache(t, 0)
		f := &fetcher{src: src}
		for _, inst := range []string{instA, instB} {
			if _, err := m.Fetch(context.Background(), "proj", inst, c0, f, newTaskDB(t)); err != nil {
				t.Fatal(err)
			}
		}
		cache := filepath.Join(root, CacheDirName)
		// Leftovers: a crash's staging generation (removed), a corrupt
		// identity (entry removed), an unexpected name (left untouched).
		os.MkdirAll(filepath.Join(cache, instA, stagePrefix+"crash"), 0o700)
		os.WriteFile(filepath.Join(cache, instB, cacheMetaName), []byte(`{"schema":9}`), 0o600)
		os.MkdirAll(filepath.Join(cache, "not-an-instance"), 0o700)
		m2, err := OpenCache(context.Background(), CacheOptions{Root: root, Origin: "https://plane.test fp"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(cache, instA, stagePrefix+"crash")); !os.IsNotExist(err) {
			t.Fatal("a staging leftover survived startup")
		}
		if _, err := os.Stat(filepath.Join(cache, instB)); !os.IsNotExist(err) {
			t.Fatal("a corrupt entry survived startup")
		}
		if _, err := os.Stat(filepath.Join(cache, "not-an-instance")); err != nil {
			t.Fatal("an unexpected entry was removed")
		}
		if st, err := m2.Fetch(context.Background(), "proj", instA, c0, f, newTaskDB(t)); err != nil || !st.Hit {
			t.Fatalf("surviving entry %+v %v", st, err)
		}
		// Another enrollment target purges every disposable entry.
		if _, err := OpenCache(context.Background(), CacheOptions{Root: root, Origin: "https://other.test fp"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(cache, instA)); !os.IsNotExist(err) {
			t.Fatal("a foreign-target entry survived")
		}
		// An unsafe cache root refuses.
		os.Chmod(cache, 0o755)
		if _, err := OpenCache(context.Background(), CacheOptions{Root: root, Origin: "x"}); err == nil {
			t.Fatal("a non-private cache root was accepted")
		}
		os.Chmod(cache, 0o700)
		link := t.TempDir()
		os.Symlink(cache, filepath.Join(link, CacheDirName))
		if _, err := OpenCache(context.Background(), CacheOptions{Root: link, Origin: "x"}); err == nil {
			t.Fatal("a symlinked cache root was accepted")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		os.MkdirAll(filepath.Join(cache, instA), 0o700)
		if _, err := OpenCache(ctx, CacheOptions{Root: root, Origin: "x"}); err == nil {
			t.Fatal("a cancelled startup succeeded")
		}
	})
}

// restoreLoose re-encodes every loose object below the objects directory
// objs as stored (level 0) zlib blocks, the same object and hash, and
// returns the new bytes by fan-out path ("ab/cdef...").
func restoreLoose(t *testing.T, objs string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	fans, err := os.ReadDir(objs)
	if err != nil {
		t.Fatal(err)
	}
	for _, fan := range fans {
		if !fan.IsDir() || len(fan.Name()) != 2 {
			continue
		}
		names, err := os.ReadDir(filepath.Join(objs, fan.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			p := filepath.Join(objs, fan.Name(), n.Name())
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			zr, err := zlib.NewReader(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			plain, err := io.ReadAll(zr)
			if err != nil {
				t.Fatal(err)
			}
			var b bytes.Buffer
			zw, _ := zlib.NewWriterLevel(&b, zlib.NoCompression)
			zw.Write(plain)
			zw.Close()
			os.Chmod(p, 0o600)
			if err := os.WriteFile(p, b.Bytes(), 0o444); err != nil {
				t.Fatal(err)
			}
			os.Chmod(p, 0o444)
			out[fan.Name()+"/"+n.Name()] = b.Bytes()
		}
	}
	return out
}
