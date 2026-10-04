package workspacetransfer

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/objfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 10b r0.5 UT-B3: the verified compressed loose copy. A loose
// object of a task database is copied as its compressed bytes into a
// private exclusive temporary file, that file is verified (inflated, typed,
// its declared length and zlib checksum consumed, re-hashed) and published
// without replacement; the deflater never runs on that branch. Packed and
// generic sources keep the decoded writer.

// countingDeps returns fast deps whose loose-object writer constructions
// (the deflater) are counted in n.
func countingDeps(n *atomic.Int64) *deps {
	d := fastDeps()
	d.newObjWriter = func(w io.Writer) *objfile.Writer {
		n.Add(1)
		return objfile.NewWriter(w)
	}
	return d
}

// looseEncoding returns the loose encoding of content as typ, the header
// declaring size, compressed at level.
func looseEncoding(t testing.TB, typ string, size int, content []byte, level int) []byte {
	t.Helper()
	var b bytes.Buffer
	zw, err := zlib.NewWriterLevel(&b, level)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(zw, "%s %d\x00", typ, size)
	zw.Write(content)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// looseOf returns h's loose file path in db.
func looseOf(db *TaskDB, h plumbing.Hash) string {
	return filepath.Join(db.Dir(), "objects", h.String()[:2], h.String()[2:])
}

// plantLoose writes raw as h's loose file in db (a read-only file).
func plantLoose(t testing.TB, db *TaskDB, h plumbing.Hash, raw []byte) string {
	t.Helper()
	p := looseOf(db, h)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	return p
}

// tempObjects lists the temporary object files left in db's objects
// directory.
func tempObjects(t testing.TB, db *TaskDB) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(db.Dir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tmp_obj_") {
			left = append(left, e.Name())
		}
	}
	return left
}

func inodeOf(t testing.TB, p string) uint64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return uint64(st.Ino)
}

// genericStore is a storer without the loose-source opener.
type genericStore struct{ storer.EncodedObjectStorer }

// TestTaskCompressedCopy is UT-B3's compressed-copy instrumentation through
// the package-private writer-construction seam.
func TestTaskCompressedCopy(t *testing.T) {
	content := []byte(strings.Repeat("compressed copy line\n", 400))
	h := plumbing.ComputeHash(plumbing.BlobObject, content)
	// Stored (level 0) blocks: an encoding the default deflater never
	// produces, so equal destination bytes prove they were not re-deflated.
	stored := looseEncoding(t, "blob", len(content), content, zlib.NoCompression)

	t.Run("loose-preserved", func(t *testing.T) {
		var n atomic.Int64
		d := countingDeps(&n)
		src := newTaskDB(t, d)
		sp := plantLoose(t, src, h, stored)
		dst := newTaskDB(t, d)
		st, err := dst.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h})
		if err != nil || st != (CopyStats{Objects: 1, Bytes: int64(len(content))}) {
			t.Fatalf("copy %+v %v", st, err)
		}
		got, err := os.ReadFile(looseOf(dst, h))
		if err != nil || !bytes.Equal(got, stored) {
			t.Fatalf("compressed bytes not preserved (%d bytes, %v)", len(got), err)
		}
		if n.Load() != 0 {
			t.Fatalf("the deflater ran %d times on a loose copy", n.Load())
		}
		fi, _ := os.Lstat(looseOf(dst, h))
		if fi.Mode().Perm() != 0o444 || inodeOf(t, looseOf(dst, h)) == inodeOf(t, sp) {
			t.Fatalf("destination mode %v, shares the source inode", fi.Mode())
		}
		// Batched durability: the new object pends until the copy's
		// directory sync, which ran.
		if len(dst.db.pending) != 0 || len(tempObjects(t, dst)) != 0 {
			t.Fatalf("pending %v, temporaries %v", dst.db.pending, tempObjects(t, dst))
		}
		// Every loose object of a real closure (commit, trees, blobs)
		// copies byte for byte, deflater-free, with the decoded stats.
		c := dbCommit(t, src, map[string]fspec{"a": reg("1"), "d/b": reg(string(content))})
		order, want, err := src.Closure(context.Background(), []plumbing.Hash{c})
		if err != nil {
			t.Fatal(err)
		}
		all := newTaskDB(t, d)
		got2, err := all.CopyFrom(context.Background(), src.Store(), order)
		if err != nil || got2 != want || n.Load() != 0 {
			t.Fatalf("closure copy %+v (closure %+v) %v, %d deflates", got2, want, err, n.Load())
		}
		for _, o := range order {
			a, _ := os.ReadFile(looseOf(src, o))
			b, _ := os.ReadFile(looseOf(all, o))
			if len(a) == 0 || !bytes.Equal(a, b) {
				t.Fatalf("object %s not copied verbatim", o)
			}
		}
		// A generic storer (no opener) takes the decoded writer: the same
		// logical stats, one deflate, re-encoded bytes.
		gen := newTaskDB(t, d)
		gst, err := gen.CopyFrom(context.Background(), genericStore{src.Store()}, []plumbing.Hash{h})
		if err != nil || gst != st || n.Load() != 1 {
			t.Fatalf("generic copy %+v %v, %d deflates", gst, err, n.Load())
		}
		if b, _ := os.ReadFile(looseOf(gen, h)); bytes.Equal(b, stored) {
			t.Fatal("the decoded writer reproduced the stored encoding")
		}
	})

	t.Run("packed-fallback", func(t *testing.T) {
		var n atomic.Int64
		d := countingDeps(&n)
		src := newTaskDB(t, d)
		c := dbCommit(t, src, map[string]fspec{"a": reg("1"), "b": reg(string(content))})
		order, want, err := src.Closure(context.Background(), []plumbing.Hash{c})
		if err != nil {
			t.Fatal(err)
		}
		var pack bytes.Buffer
		if err := src.WritePack(context.Background(), order, &pack); err != nil {
			t.Fatal(err)
		}
		// A pack-only database: every loose object is genuinely absent, so
		// each takes the decoded writer with unchanged logical stats.
		packed := newTaskDB(t, d)
		if err := packed.ReceivePack(context.Background(), &pack); err != nil {
			t.Fatal(err)
		}
		dst := newTaskDB(t, d)
		got, err := dst.CopyFrom(context.Background(), packed.Store(), order)
		if err != nil || got != want || n.Load() != int64(len(order)) {
			t.Fatalf("packed copy %+v (want %+v) %v, %d deflates", got, want, err, n.Load())
		}
		if _, _, err := dst.Closure(context.Background(), []plumbing.Hash{c}); err != nil {
			t.Fatal(err)
		}
		// A missing object is still an integrity failure, not a copy.
		if _, err := dst.CopyFrom(context.Background(), packed.Store(), []plumbing.Hash{plumbing.NewHash(strings.Repeat("e", 40))}); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("missing object: %v", err)
		}
	})

	t.Run("checkout", func(t *testing.T) {
		// The task database to checkout copy (the checkout's direct copy of
		// the database's store) takes the loose branch.
		var n atomic.Int64
		d := countingDeps(&n)
		db := newTaskDB(t, d)
		c := dbCommit(t, db, map[string]fspec{"a.txt": reg("alpha"), "d/b": reg(string(content))})
		order, _, err := db.Closure(context.Background(), []plumbing.Hash{c})
		if err != nil {
			t.Fatal(err)
		}
		work := newWork(t)
		if _, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "linux", Work: work, DB: db, Commit: c}); err != nil {
			t.Fatal(err)
		}
		if n.Load() != 0 {
			t.Fatalf("the checkout copy deflated %d objects", n.Load())
		}
		for _, o := range order {
			a, _ := os.ReadFile(looseOf(db, o))
			b, err := os.ReadFile(filepath.Join(work, ".git", "objects", o.String()[:2], o.String()[2:]))
			if err != nil || !bytes.Equal(a, b) {
				t.Fatalf("checkout object %s not copied verbatim: %v", o, err)
			}
		}
	})

	t.Run("integrity", func(t *testing.T) {
		other := []byte("other content\n")
		bad := map[string]struct {
			raw  []byte
			hash plumbing.Hash
		}{
			"wrong-id":      {looseEncoding(t, "blob", len(other), other, zlib.DefaultCompression), h},
			"checksum":      {flipAt(stored, len(stored)-1), h},
			"data":          {flipAt(looseEncoding(t, "blob", len(content), content, zlib.DefaultCompression), 20), h},
			"truncated":     {stored[:len(stored)-10], h},
			"not-zlib":      {[]byte("not a zlib stream at all"), h},
			"type":          {looseEncoding(t, "bogus", len(content), content, zlib.DefaultCompression), h},
			"delta-type":    {looseEncoding(t, "ofs-delta", len(content), content, zlib.DefaultCompression), h},
			"negative-size": {looseEncoding(t, "blob", -1, content, zlib.DefaultCompression), h},
			"size-short":    {looseEncoding(t, "blob", len(content)-1, content, zlib.DefaultCompression), h},
			"size-long":     {looseEncoding(t, "blob", len(content)+1, content, zlib.DefaultCompression), h},
			"empty":         {nil, h},
			"long-type":     {zlibRaw(t, zlib.DefaultCompression, append([]byte("blob"), bytes.Repeat([]byte("x"), 4096)...)), h},
			"long-size":     {zlibRaw(t, zlib.DefaultCompression, append([]byte("blob "), bytes.Repeat([]byte("1"), 4096)...)), h},
		}
		for name, c := range bad {
			var n atomic.Int64
			d := countingDeps(&n)
			src, dst := newTaskDB(t, d), newTaskDB(t, d)
			plantLoose(t, src, c.hash, c.raw)
			_, err := dst.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{c.hash})
			// An integrity failure, never a fallback to the decoded writer:
			// nothing published, no temporary left, no deflate.
			if !errors.Is(err, ErrIntegrity) || contract.CodeOf(err) != contract.CodeInternal || contract.TransferReason(err) == contract.ReasonStorageFailure {
				t.Fatalf("%s: %v", name, err)
			}
			if _, serr := os.Lstat(looseOf(dst, c.hash)); !os.IsNotExist(serr) || len(tempObjects(t, dst)) != 0 || n.Load() != 0 {
				t.Fatalf("%s: published %v, temporaries %v, %d deflates", name, serr, tempObjects(t, dst), n.Load())
			}
		}
		// Bytes after the zlib stream are not inspected (as verifyLoose):
		// the copy preserves them verbatim.
		d := fastDeps()
		src, dst := newTaskDB(t, d), newTaskDB(t, d)
		tail := append(append([]byte(nil), stored...), "tail"...)
		plantLoose(t, src, h, tail)
		if _, err := dst.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h}); err != nil {
			t.Fatalf("tail: %v", err)
		}
		if b, _ := os.ReadFile(looseOf(dst, h)); !bytes.Equal(b, tail) {
			t.Fatal("tail not preserved")
		}
	})

	t.Run("not-regular", func(t *testing.T) {
		// A present loose entry that is not a regular file below real
		// directories is unreadable: an integrity failure, although the
		// generic storage (which follows links) could read the object.
		for _, kind := range []string{"symlink", "fifo", "directory", "fanout-link", "fanout-file"} {
			d := fastDeps()
			src, dst := newTaskDB(t, d), newTaskDB(t, d)
			valid := filepath.Join(tempDir(t), "valid")
			os.WriteFile(valid, stored, 0o444)
			p := looseOf(src, h)
			fan := filepath.Dir(p)
			switch kind {
			case "fanout-link":
				real := filepath.Join(tempDir(t), "fan")
				os.MkdirAll(real, 0o755)
				os.WriteFile(filepath.Join(real, filepath.Base(p)), stored, 0o444)
				os.Symlink(real, fan)
			case "fanout-file":
				os.WriteFile(fan, stored, 0o444)
			default:
				os.MkdirAll(fan, 0o755)
				switch kind {
				case "symlink":
					os.Symlink(valid, p)
				case "fifo":
					unixMkfifo(t, p)
				case "directory":
					os.Mkdir(p, 0o755)
				}
			}
			if _, err := dst.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h}); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("%s: %v", kind, err)
			}
			if _, err := os.Lstat(looseOf(dst, h)); !os.IsNotExist(err) {
				t.Fatalf("%s: published", kind)
			}
		}
		if os.Geteuid() != 0 {
			d := fastDeps()
			src, dst := newTaskDB(t, d), newTaskDB(t, d)
			os.Chmod(plantLoose(t, src, h, stored), 0)
			if _, err := dst.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h}); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("unreadable: %v", err)
			}
		}
		// A closed database opens nothing.
		d := fastDeps()
		src, dst := newTaskDB(t, d), newTaskDB(t, d)
		plantLoose(t, src, h, stored)
		s := src.Store()
		src.Close()
		if _, err := dst.CopyFrom(context.Background(), s, []plumbing.Hash{h}); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("closed source: %v", err)
		}
		// A source that fails mid-read is an integrity failure.
		if _, err := dst.db.copyLoose(context.Background(), io.MultiReader(bytes.NewReader(stored[:8]), errReader{}), h); !errors.Is(err, ErrIntegrity) || len(tempObjects(t, dst)) != 0 {
			t.Fatalf("failing source: %v", err)
		}
	})

	t.Run("destination", func(t *testing.T) {
		d := fastDeps()
		src, dst := newTaskDB(t, d), newTaskDB(t, d)
		plantLoose(t, src, h, stored)
		if _, err := dst.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h}); err != nil {
			t.Fatal(err)
		}
		// An existing valid destination is verified, never overwritten.
		ino := inodeOf(t, looseOf(dst, h))
		if st, err := dst.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h}); err != nil || st.Objects != 1 || inodeOf(t, looseOf(dst, h)) != ino || len(tempObjects(t, dst)) != 0 {
			t.Fatalf("existing valid: %+v %v", st, err)
		}
		// An existing corrupt destination fails without repair in place.
		cor := newTaskDB(t, d)
		plantLoose(t, cor, h, []byte("corrupt"))
		if _, err := cor.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h}); !errors.Is(err, ErrIntegrity) || len(tempObjects(t, cor)) != 0 {
			t.Fatalf("existing corrupt: %v", err)
		}
		if b, _ := os.ReadFile(looseOf(cor, h)); string(b) != "corrupt" {
			t.Fatal("a corrupt destination was repaired in place")
		}
		// Publication races: a concurrent publisher's object appears just
		// before the link; a valid one is verified, a corrupt one fails, and
		// neither is replaced.
		for _, race := range []struct {
			name string
			raw  []byte
			ok   bool
		}{{"valid", stored, true}, {"corrupt", []byte("corrupt"), false}} {
			rd := fastDeps()
			rdb := newTaskDB(t, rd)
			rd.linkAt = func(olddirfd int, oldpath string, newdirfd int, newpath string, flags int) error {
				plantLoose(t, rdb, h, race.raw)
				return unix.Linkat(olddirfd, oldpath, newdirfd, newpath, flags)
			}
			_, err := rdb.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h})
			if (err == nil) != race.ok || (!race.ok && !errors.Is(err, ErrIntegrity)) || len(tempObjects(t, rdb)) != 0 {
				t.Fatalf("race %s: %v", race.name, err)
			}
			if b, _ := os.ReadFile(looseOf(rdb, h)); !bytes.Equal(b, race.raw) {
				t.Fatalf("race %s: the concurrent object was replaced", race.name)
			}
		}
		// A filesystem without hard links publishes by no-replace rename.
		nd := fastDeps()
		nd.linkAt = func(int, string, int, string, int) error { return unix.EPERM }
		ndb := newTaskDB(t, nd)
		if _, err := ndb.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h}); err != nil || len(tempObjects(t, ndb)) != 0 {
			t.Fatalf("rename publication: %v", err)
		}
		if b, _ := os.ReadFile(looseOf(ndb, h)); !bytes.Equal(b, stored) {
			t.Fatal("rename publication changed the bytes")
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		// Cancelled while copying (before the first read) and while
		// verifying (after the copy, before the inflate): the cancellation
		// is returned, nothing is published and no temporary remains.
		for _, at := range []string{"taskdb-object-create", "taskdb-object-write"} {
			d := fastDeps()
			src, dst := newTaskDB(t, d), newTaskDB(t, d)
			plantLoose(t, src, h, stored)
			ctx, cancel := context.WithCancel(context.Background())
			d.fail = func(op string) error {
				if op == at {
					cancel()
				}
				return nil
			}
			if _, err := dst.CopyFrom(ctx, src.Store(), []plumbing.Hash{h}); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled at %s: %v", at, err)
			}
			if _, err := os.Lstat(looseOf(dst, h)); !os.IsNotExist(err) || len(tempObjects(t, dst)) != 0 {
				t.Fatalf("cancelled at %s: published or left a temporary", at)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := verifyCompressed(ctx, bytes.NewReader(stored), h); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled verification: %v", err)
		}
	})

	t.Run("header", func(t *testing.T) {
		// Header decoding is bounded and observes cancellation (review r0.5
		// C1): a header without its delimiters is neither accumulated nor
		// consumed to the end of the stream.
		noDelim := zlibRaw(t, zlib.NoCompression, append([]byte("blob"), bytes.Repeat([]byte("x"), 8<<20)...))
		// Cancelled during header parsing: the stream is served one
		// compressed byte per read and cancelled during the tenth read (the
		// zlib and stored-block headers take seven), so parsing stops at the
		// next read with the cancellation.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sr := &stepReader{data: noDelim, step: 1, onRead: func(n int) {
			if n == 10 {
				cancel()
			}
		}}
		if _, err := verifyCompressed(ctx, sr, h); !errors.Is(err, context.Canceled) || sr.reads != 10 {
			t.Fatalf("cancelled during the header: %v after %d reads (%d bytes)", err, sr.reads, sr.n)
		}
		// Oversized malformed headers (8 MiB without the type's space or
		// without the size's NUL), stored or deflated, served in 1 KiB
		// reads: an integrity failure after a bounded read, nowhere near
		// the stream's end.
		for name, raw := range map[string][]byte{
			"type-stored":   noDelim,
			"type-deflated": zlibRaw(t, zlib.DefaultCompression, append([]byte("blob"), bytes.Repeat([]byte("x"), 8<<20)...)),
			"size-stored":   zlibRaw(t, zlib.NoCompression, append([]byte("blob "), bytes.Repeat([]byte("1"), 8<<20)...)),
			"size-deflated": zlibRaw(t, zlib.DefaultCompression, append([]byte("blob "), bytes.Repeat([]byte("1"), 8<<20)...)),
		} {
			sr := &stepReader{data: raw, step: 1 << 10}
			_, err := verifyCompressed(context.Background(), sr, h)
			if !errors.Is(err, ErrIntegrity) || sr.n > 64<<10 {
				t.Fatalf("%s: %v after %d of %d compressed bytes", name, err, sr.n, len(raw))
			}
		}
		// The bound admits every well-formed header: a size field of
		// exactly maxSizeField digits (leading zeros) verifies, one more is
		// rejected.
		for _, extra := range []int{0, 1} {
			field := fmt.Sprintf("%0*d", maxSizeField+extra, len(content))
			raw := zlibRaw(t, zlib.DefaultCompression, append([]byte("blob "+field+"\x00"), content...))
			size, err := verifyCompressed(context.Background(), bytes.NewReader(raw), h)
			if ok := err == nil && size == int64(len(content)); ok != (extra == 0) {
				t.Fatalf("size field of %d digits: %d %v", len(field), size, err)
			}
		}
	})

	t.Run("faults", func(t *testing.T) {
		// A write fault at each boundary is a storage failure that leaves
		// no temporary and publishes nothing.
		for _, op := range []string{"taskdb-object-create", "taskdb-object-write", "taskdb-object-close", "taskdb-object-rename"} {
			d := fastDeps()
			src, dst := newTaskDB(t, d), newTaskDB(t, d)
			plantLoose(t, src, h, stored)
			d.fail = func(o string) error {
				if o == op {
					return syscall.EIO
				}
				return nil
			}
			_, err := dst.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h})
			if contract.TransferReason(err) != contract.ReasonStorageFailure || errors.Is(err, ErrIntegrity) {
				t.Fatalf("%s: %v", op, err)
			}
			if _, err := os.Lstat(looseOf(dst, h)); !os.IsNotExist(err) || len(tempObjects(t, dst)) != 0 {
				t.Fatalf("%s: published or left a temporary", op)
			}
		}
		// The batched directory sync of a task database fails the copy.
		d := fastDeps()
		src, dst := newTaskDB(t, d), newTaskDB(t, d)
		plantLoose(t, src, h, stored)
		d.fail = func(o string) error {
			if o == "taskdb-object-sync" {
				return syscall.EIO
			}
			return nil
		}
		if _, err := dst.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h}); contract.TransferReason(err) != contract.ReasonStorageFailure {
			t.Fatalf("batch sync: %v", err)
		}
		// An unbatched durable database syncs each copied file before its
		// publication; a failing sync publishes nothing.
		var synced int
		ud := fastDeps()
		ud.syncFD = func(int) error { synced++; return nil }
		udb := newTaskDB(t, ud)
		plain := &looseDB{d: ud, objFD: udb.objFD, sync: true, opPref: "plain-"}
		if st, err := copyObjects(context.Background(), plain, src.Store(), []plumbing.Hash{h}); err != nil || st.Objects != 1 || synced < 2 {
			t.Fatalf("unbatched copy %+v %v (%d syncs)", st, err, synced)
		}
		fdb := newTaskDB(t, ud)
		ud.fail = func(o string) error {
			if o == "plain-object-sync" {
				return syscall.EIO
			}
			return nil
		}
		failing := &looseDB{d: ud, objFD: fdb.objFD, sync: true, opPref: "plain-"}
		if _, err := copyObjects(context.Background(), failing, src.Store(), []plumbing.Hash{h}); contract.TransferReason(err) != contract.ReasonStorageFailure || len(tempObjects(t, fdb)) != 0 {
			t.Fatalf("unbatched sync fault: %v", err)
		}
		if _, err := os.Lstat(looseOf(fdb, h)); !os.IsNotExist(err) {
			t.Fatal("published after a failing sync")
		}
		// Entropy failure creates nothing.
		ed := fastDeps()
		ed.rand = errReader{}
		edb := newTaskDB(t, fastDeps())
		edb.db.d = ed
		if _, err := edb.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{h}); err == nil || len(tempObjects(t, edb)) != 0 {
			t.Fatalf("entropy failure: %v", err)
		}
	})
}

// flipAt returns a copy of b with byte i inverted.
func flipAt(b []byte, i int) []byte {
	c := append([]byte(nil), b...)
	c[i] ^= 0xff
	return c
}

// zlibRaw returns data as one zlib stream compressed at level.
func zlibRaw(t testing.TB, level int, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw, err := zlib.NewWriterLevel(&b, level)
	if err != nil {
		t.Fatal(err)
	}
	zw.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// stepReader serves data at most step bytes per read, counting reads and
// bytes; onRead, when set, runs after each read with the read's number.
type stepReader struct {
	data   []byte
	step   int
	reads  int
	n      int64
	onRead func(int)
}

func (s *stepReader) Read(p []byte) (int, error) {
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	s.reads++
	k := copy(p[:min(len(p), s.step)], s.data)
	s.data = s.data[k:]
	s.n += int64(k)
	if s.onRead != nil {
		s.onRead(s.reads)
	}
	return k, nil
}

// errReader fails every read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, syscall.EIO }
