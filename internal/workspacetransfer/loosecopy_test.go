package workspacetransfer

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	in := compressedCopyFixtures(t)
	content := []byte(compressedCopyContent)
	h := plumbing.ComputeHash(plumbing.BlobObject, content)
	// Stored (level 0) blocks: an encoding the default deflater never
	// produces, so equal destination bytes prove they were not re-deflated.
	stored := in.get("stored")

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
		bad := map[string]struct {
			raw  []byte
			hash plumbing.Hash
		}{
			"wrong-id":      {in.get("wrong-id"), h},
			"checksum":      {flipAt(stored, len(stored)-1), h},
			"data":          {flipAt(in.get("data"), 20), h},
			"truncated":     {stored[:len(stored)-10], h},
			"not-zlib":      {[]byte("not a zlib stream at all"), h},
			"type":          {in.get("type"), h},
			"delta-type":    {in.get("delta-type"), h},
			"negative-size": {in.get("negative-size"), h},
			"size-short":    {in.get("size-short"), h},
			"size-long":     {in.get("size-long"), h},
			"empty":         {nil, h},
			"long-type":     {in.get("long-type"), h},
			"long-size":     {in.get("long-size"), h},
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
		// oversized is one of the malformed header streams of prefix and
		// body (compressedCopyHeaders): the compressed stream must exceed
		// the 64 KiB read bound below, and the body is kept small enough
		// for the stress run.
		oversized := func(name string) []byte {
			t.Helper()
			raw := in.get(name)
			if len(raw) <= 64<<10 {
				t.Errorf("oversized %s fixture: %d compressed bytes, not above the 64 KiB read bound", name, len(raw))
			}
			if body := in.headerBody[name]; body == 0 || body > 128<<10 {
				t.Errorf("oversized %s fixture: %d-byte body, not in (0, 128 KiB]", name, body)
			}
			return raw
		}
		noDelim := oversized("type-stored")
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
		// Oversized malformed headers (128 KiB without the type's space or
		// without the size's NUL), stored or deflated, served in 1 KiB
		// reads: an integrity failure after a bounded read, nowhere near
		// the stream's end.
		for name, raw := range map[string][]byte{
			"type-stored":   noDelim,
			"type-deflated": oversized("type-deflated"),
			"size-stored":   oversized("size-stored"),
			"size-deflated": oversized("size-deflated"),
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
			raw := in.get(fmt.Sprintf("size-field+%d", extra))
			size, err := verifyCompressed(context.Background(), bytes.NewReader(raw), h)
			if ok := err == nil && size == int64(len(content)); ok != (extra == 0) {
				t.Fatalf("size field of %d digits: %d %v", maxSizeField+extra, size, err)
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

// compressedCopyContent is the blob TestTaskCompressedCopy copies.
var compressedCopyContent = strings.Repeat("compressed copy line\n", 400)

// compressedCopyInputs are TestTaskCompressedCopy's constant zlib
// streams, a pure function of constants: the stored encoding of
// compressedCopyContent, the integrity subtest's malformed encodings and
// the header subtest's oversized malformed headers (128 KiB bodies,
// Huffman-coded or stored) and size-field streams. Deflating them is most
// of the header subtest's cost under the race detector, which the stress
// stage would otherwise pay 60 times per process, so they are built once
// per process (compressedCopyFixtures). They are held as strings, which
// are immutable, and get hands every use its own []byte copy: a mutation
// in one use cannot reach the next (TestCompressedCopyInputsPerUse). Only
// the inputs are shared; every copy, verification and assertion runs on
// every repetition.
type compressedCopyInputs struct {
	raw map[string]string
	// sums are the SHA-256 digests of raw, taken when it was built.
	sums map[string][sha256.Size]byte
	// headerBody is each oversized header stream's body length.
	headerBody map[string]int
}

// get returns a fresh copy of the named input.
func (in compressedCopyInputs) get(name string) []byte {
	s, ok := in.raw[name]
	if !ok {
		panic("no compressed-copy input " + name)
	}
	return []byte(s)
}

// compressedCopyBuilds counts buildCompressedCopyInputs calls in this
// process (TestCompressedCopyInputsPerUse requires one).
var compressedCopyBuilds atomic.Int64

func buildCompressedCopyInputs() (compressedCopyInputs, error) {
	compressedCopyBuilds.Add(1)
	content := []byte(compressedCopyContent)
	in := compressedCopyInputs{raw: map[string]string{}, sums: map[string][sha256.Size]byte{}, headerBody: map[string]int{}}
	var firstErr error
	put := func(name string, level int, parts ...[]byte) {
		var b bytes.Buffer
		zw, err := zlib.NewWriterLevel(&b, level)
		for _, p := range parts {
			if err == nil {
				_, err = zw.Write(p)
			}
		}
		if err == nil {
			err = zw.Close()
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", name, err)
		}
		in.raw[name] = b.String()
		in.sums[name] = sha256.Sum256(b.Bytes())
	}
	loose := func(name, typ string, size int, body []byte, level int) {
		put(name, level, fmt.Appendf(nil, "%s %d\x00", typ, size), body)
	}
	loose("stored", "blob", len(content), content, zlib.NoCompression)
	other := []byte("other content\n")
	loose("wrong-id", "blob", len(other), other, zlib.DefaultCompression)
	loose("data", "blob", len(content), content, zlib.DefaultCompression)
	loose("type", "bogus", len(content), content, zlib.DefaultCompression)
	loose("delta-type", "ofs-delta", len(content), content, zlib.DefaultCompression)
	loose("negative-size", "blob", -1, content, zlib.DefaultCompression)
	loose("size-short", "blob", len(content)-1, content, zlib.DefaultCompression)
	loose("size-long", "blob", len(content)+1, content, zlib.DefaultCompression)
	put("long-type", zlib.DefaultCompression, []byte("blob"), bytes.Repeat([]byte("x"), 4096))
	put("long-size", zlib.DefaultCompression, []byte("blob "), bytes.Repeat([]byte("1"), 4096))
	// The deflated header fixtures are Huffman-coded over a 26-letter
	// cycle, which has neither a space nor a NUL and stays above the bound.
	alphabet := make([]byte, 128<<10)
	for i := range alphabet {
		alphabet[i] = byte('A' + i%26)
	}
	oversized := func(name string, level int, prefix string, body []byte) {
		put(name, level, []byte(prefix), body)
		in.headerBody[name] = len(body)
	}
	oversized("type-stored", zlib.NoCompression, "blob", bytes.Repeat([]byte("x"), 128<<10))
	oversized("type-deflated", zlib.HuffmanOnly, "blob", alphabet)
	oversized("size-stored", zlib.NoCompression, "blob ", bytes.Repeat([]byte("1"), 128<<10))
	oversized("size-deflated", zlib.HuffmanOnly, "blob ", alphabet)
	// A size field of exactly maxSizeField digits (leading zeros), and one
	// more.
	for _, extra := range []int{0, 1} {
		field := fmt.Sprintf("%0*d", maxSizeField+extra, len(content))
		put(fmt.Sprintf("size-field+%d", extra), zlib.DefaultCompression, []byte("blob "+field+"\x00"), content)
	}
	return in, firstErr
}

// compressedCopyOnce builds the process's compressedCopyInputs.
var compressedCopyOnce = sync.OnceValues(buildCompressedCopyInputs)

// compressedCopyFixtures returns the process's compressedCopyInputs.
func compressedCopyFixtures(t testing.TB) compressedCopyInputs {
	t.Helper()
	in, err := compressedCopyOnce()
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// TestCompressedCopyInputsPerUse proves compressedCopyInputs are built
// once per process and copied per use: each repetition overwrites its
// copies with bytes no zlib stream of them holds (all 0xff: an invalid
// zlib header), and both a second copy in the same repetition and the
// first copy of every later repetition (-count) must match the digest
// taken when the inputs were built.
func TestCompressedCopyInputsPerUse(t *testing.T) {
	in := compressedCopyFixtures(t)
	if n := compressedCopyBuilds.Load(); n != 1 {
		t.Fatalf("the compressed-copy inputs were built %d times in this process, want once", n)
	}
	if len(in.raw) != 16 {
		t.Fatalf("%d compressed-copy inputs", len(in.raw))
	}
	for name := range in.raw {
		first := in.get(name)
		if len(first) == 0 || sha256.Sum256(first) != in.sums[name] {
			t.Fatalf("%s: a previous use's mutation leaked into the input", name)
		}
		for i := range first {
			first[i] = 0xff
		}
		again := in.get(name)
		if sha256.Sum256(again) != in.sums[name] || &again[0] == &first[0] {
			t.Fatalf("%s: a use's mutation reached the next use", name)
		}
	}
}

// flipAt returns a copy of b with byte i inverted.
func flipAt(b []byte, i int) []byte {
	c := append([]byte(nil), b...)
	c[i] ^= 0xff
	return c
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
