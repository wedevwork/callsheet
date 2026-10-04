package workspacetransfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/objfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"
	gitsync "github.com/go-git/go-git/v5/utils/sync"
	"golang.org/x/sys/unix"
)

// Object handling: the private disk-backed temporary stores, streamed
// loose-object writes (with a no-overwrite publication), the typed
// closure walk with its checks, and pack encoding of a selected object
// set (decoded and re-encoded object by object: nothing unselected rides
// along from a shared pack).

// tempStore is a private temporary bare object store (mode 0700) in the
// process temporary directory, removed by close on every path.
type tempStore struct {
	dir   string
	store *filesystem.Storage
	objFD int
}

func (d *deps) newTempStore() (*tempStore, error) {
	parent := d.tempDir
	if parent == "" {
		parent = os.TempDir()
	}
	if err := d.check("temp-create"); err != nil {
		return nil, errStorage("cannot create a private temporary store", err)
	}
	dir, err := os.MkdirTemp(parent, "callsheet-transfer-")
	if err != nil {
		return nil, errStorage("cannot create a private temporary store", err)
	}
	// MkdirTemp creates the directory with mode 0700.
	t := &tempStore{dir: dir, objFD: -1}
	err = os.MkdirAll(filepath.Join(dir, "objects", "pack"), 0o700)
	if err == nil {
		err = os.MkdirAll(filepath.Join(dir, "refs"), 0o700)
	}
	if err == nil {
		t.objFD, err = openPathDir(filepath.Join(dir, "objects"))
	}
	if err != nil {
		t.close()
		return nil, errStorage("cannot create a private temporary store", err)
	}
	// The store is private (created 0700 by this process, holding only
	// its own objects), so go-git's plain OS filesystem is used.
	t.store = filesystem.NewStorageWithOptions(osfs.New(dir), cache.NewObjectLRU(8<<20),
		filesystem.Options{LargeObjectThreshold: largeObject})
	return t, nil
}

// close releases the store and removes its directory.
func (t *tempStore) close() {
	if t.store != nil {
		t.store.Close()
	}
	if t.objFD >= 0 {
		closeFD(t.objFD)
		t.objFD = -1
	}
	os.RemoveAll(t.dir)
}

// looseDB writes loose objects below an open objects directory.
type looseDB struct {
	d      *deps
	objFD  int
	sync   bool
	opPref string
	// created records fan-out directories this db created (their parent
	// needs a sync before a ref may point at the objects).
	created bool
	dirty   map[string]bool
	// batch (iteration 10b task databases) defers durability: new objects
	// are not synced one by one (an object that already exists is not
	// rewritten or synced at all) but recorded in pending, and syncDirs
	// makes them and their directories durable in one platform batch.
	batch   bool
	pending []string
}

// write stores one object streamed from r (size bytes of type t) and
// returns its ID. The object is written to a private temporary file in
// the objects directory, synced when durable, and published with a
// no-replace link (or rename): an existing object is verified, never
// overwritten.
func (db *looseDB) write(ctx context.Context, t plumbing.ObjectType, size int64, r io.Reader) (plumbing.Hash, error) {
	tok, err := db.d.token()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	tmp := "tmp_obj_" + tok
	if err := db.d.check(db.opPref + "object-create"); err != nil {
		return plumbing.ZeroHash, err
	}
	fd, err := unix.Openat(db.objFD, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	f := os.NewFile(uintptr(fd), tmp)
	h, werr := func() (plumbing.Hash, error) {
		ow := db.d.objWriter(f)
		if err := ow.WriteHeader(t, size); err != nil {
			return plumbing.ZeroHash, err
		}
		n, err := copyCtx(ctx, ow, r)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if n != size {
			return plumbing.ZeroHash, errShortRead
		}
		if err := db.d.check(db.opPref + "object-write"); err != nil {
			return plumbing.ZeroHash, err
		}
		if err := ow.Close(); err != nil {
			return plumbing.ZeroHash, err
		}
		if err := unix.Fchmod(fd, 0o444); err != nil {
			return plumbing.ZeroHash, err
		}
		if db.sync && !db.batch {
			if err := db.d.sync(fd, db.opPref+"object-sync"); err != nil {
				return plumbing.ZeroHash, err
			}
		}
		return ow.Hash(), nil
	}()
	cerr := f.Close()
	if werr == nil && cerr != nil {
		werr = cerr
	}
	if werr == nil {
		werr = db.d.check(db.opPref + "object-close")
	}
	if werr != nil {
		unix.Unlinkat(db.objFD, tmp, 0)
		return plumbing.ZeroHash, werr
	}
	if err := db.publish(tmp, h); err != nil {
		unix.Unlinkat(db.objFD, tmp, 0)
		return plumbing.ZeroHash, err
	}
	return h, nil
}

// publish moves the temporary object tmp to its fan-out name without
// replacing an existing object (which must hash correctly).
func (db *looseDB) publish(tmp string, h plumbing.Hash) error {
	hex := h.String()
	fan, name := hex[:2], hex[2:]
	dfd, err := openDirAt(db.objFD, fan)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(db.objFD, fan, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		db.created = true
		dfd, err = openDirAt(db.objFD, fan)
	}
	if err != nil {
		return err
	}
	defer closeFD(dfd)
	if err := db.d.check(db.opPref + "object-rename"); err != nil {
		return err
	}
	fresh := false
	err = db.d.linkAt(db.objFD, tmp, dfd, name, 0)
	switch {
	case err == nil:
		unix.Unlinkat(db.objFD, tmp, 0)
		fresh = true
	case errors.Is(err, unix.EEXIST):
		unix.Unlinkat(db.objFD, tmp, 0)
		if err := verifyLoose(dfd, name, h); err != nil {
			return err
		}
	case errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EMLINK):
		// No hard links here: a no-replace rename.
		if err := renameNoReplaceAt(db.objFD, tmp, dfd, name); err != nil {
			if errors.Is(err, unix.EEXIST) {
				unix.Unlinkat(db.objFD, tmp, 0)
				return verifyLoose(dfd, name, h)
			}
			return err
		}
		fresh = true
	default:
		return err
	}
	if db.dirty == nil {
		db.dirty = map[string]bool{}
	}
	db.dirty[fan] = true
	if db.batch && fresh {
		// A newly published object (an existing one was verified instead).
		db.pending = append(db.pending, fan+"/"+name)
	}
	return nil
}

// verifyLoose checks that an existing loose object hashes to h.
func verifyLoose(dfd int, name string, h plumbing.Hash) error {
	f, _, err := openFileAt(dfd, name)
	if err != nil {
		return errIntegrity("an existing object in the destination repository is unreadable")
	}
	defer f.Close()
	r, err := objfile.NewReader(f)
	if err != nil {
		return errIntegrity("an existing object in the destination repository is corrupt")
	}
	defer r.Close()
	t, size, err := r.Header()
	if err != nil {
		return errIntegrity("an existing object in the destination repository is corrupt")
	}
	hh := plumbing.NewHasher(t, size)
	if n, err := io.Copy(hh, r); err != nil || n != size || hh.Sum() != h {
		return errIntegrity("an existing object in the destination repository is corrupt")
	}
	return nil
}

// copyLoose copies the loose object h from src, its compressed source
// file, into db without recompressing it: the compressed bytes are streamed
// verbatim into a private exclusive temporary file in the objects
// directory, that same file is verified (verifyCompressed), and only then
// is it published with the no-replace primitive of write (an existing
// object is verified, never overwritten), with write's permission
// transition, fault hooks and durability. The bytes verified are the bytes
// published; the source is never reopened. No deflater runs. It returns the
// object's content size. A source that cannot be read or does not verify is
// an integrity failure; cancellation is observed while copying and while
// verifying, and the temporary file is removed on every failure.
func (db *looseDB) copyLoose(ctx context.Context, src io.Reader, h plumbing.Hash) (int64, error) {
	tok, err := db.d.token()
	if err != nil {
		return 0, err
	}
	tmp := "tmp_obj_" + tok
	if err := db.d.check(db.opPref + "object-create"); err != nil {
		return 0, err
	}
	fd, err := unix.Openat(db.objFD, tmp, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return 0, err
	}
	f := os.NewFile(uintptr(fd), tmp)
	size, werr := func() (int64, error) {
		sr := &sourceReader{r: src}
		if _, err := copyCtx(ctx, f, sr); err != nil {
			if sr.err != nil && ctx.Err() == nil {
				return 0, errIntegrity("a selected object cannot be read")
			}
			return 0, err
		}
		if err := db.d.check(db.opPref + "object-write"); err != nil {
			return 0, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		size, err := verifyCompressed(ctx, f, h)
		if err != nil {
			return 0, err
		}
		if err := unix.Fchmod(fd, 0o444); err != nil {
			return 0, err
		}
		if db.sync && !db.batch {
			if err := db.d.sync(fd, db.opPref+"object-sync"); err != nil {
				return 0, err
			}
		}
		return size, nil
	}()
	cerr := f.Close()
	if werr == nil && cerr != nil {
		werr = cerr
	}
	if werr == nil {
		werr = db.d.check(db.opPref + "object-close")
	}
	if werr != nil {
		unix.Unlinkat(db.objFD, tmp, 0)
		return 0, werr
	}
	if err := db.publish(tmp, h); err != nil {
		unix.Unlinkat(db.objFD, tmp, 0)
		return 0, err
	}
	return size, nil
}

// sourceReader records a read failure of the copied source (an integrity
// failure), as distinct from a failure to write the destination.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}

// Loose header field bounds: the longest base type name ("commit") and an
// int64 in decimal fit with room to spare (a sign and leading zeros).
const (
	maxTypeField = 16
	maxSizeField = 32
)

// verifyCompressed verifies a loose object stream as verifyLoose does an
// existing object: it inflates through the pooled zlib reader the normal
// object reader uses, decodes the "<type> <size>\x00" header into bounded
// storage (looseHeader), requires a base object type and a nonnegative
// declared length, consumes the content through the zlib end of stream and
// its checksum, requires exactly the declared length, and hashes the header
// and content against h. Bytes after the zlib stream are not inspected. It
// returns the content size; every rejection is an integrity failure, and
// cancellation is observed before every compressed read and every header
// byte, so neither a malformed header nor a cancelled verification consumes
// the rest of the stream.
func verifyCompressed(ctx context.Context, r io.Reader, h plumbing.Hash) (int64, error) {
	corrupt := errIntegrity("a selected object is corrupt")
	fail := func() (int64, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return 0, corrupt
	}
	zr, err := gitsync.GetZlibReader(&ctxReader{ctx: ctx, r: r})
	if err != nil {
		return fail()
	}
	defer gitsync.PutZlibReader(zr)
	t, size, err := looseHeader(ctx, zr.Reader)
	if err != nil {
		return fail()
	}
	hh := plumbing.NewHasher(t, size)
	n, err := copyCtx(ctx, hh, zr.Reader)
	if err != nil {
		return fail()
	}
	if n != size {
		return 0, corrupt
	}
	if hh.Sum() != h {
		return 0, errIntegrity("a selected object does not match its hash")
	}
	return size, nil
}

// looseHeader decodes a loose object header from the inflated stream zr:
// the type up to its space (at most maxTypeField bytes), which must be a
// base object type, and the decimal size up to its NUL (at most
// maxSizeField bytes), which must be nonnegative. Fields are read a byte at
// a time into fixed storage, so the stream is consumed only through the
// header; a field without its delimiter within its bound is malformed, and
// cancellation is checked before every byte.
func looseHeader(ctx context.Context, zr io.Reader) (plumbing.ObjectType, int64, error) {
	var typ [maxTypeField]byte
	tf, err := headerField(ctx, zr, ' ', typ[:])
	if err != nil {
		return plumbing.InvalidObject, 0, err
	}
	t, err := plumbing.ParseObjectType(string(tf))
	if err != nil || (t != plumbing.CommitObject && t != plumbing.TreeObject && t != plumbing.BlobObject && t != plumbing.TagObject) {
		return plumbing.InvalidObject, 0, objfile.ErrHeader
	}
	var num [maxSizeField]byte
	sf, err := headerField(ctx, zr, 0, num[:])
	if err != nil {
		return plumbing.InvalidObject, 0, err
	}
	size, err := strconv.ParseInt(string(sf), 10, 64)
	if err != nil || size < 0 {
		return plumbing.InvalidObject, 0, objfile.ErrHeader
	}
	return t, size, nil
}

// headerField reads zr up to delim into dst and returns the bytes before
// it; a stream ending first, or a field longer than dst, is malformed.
func headerField(ctx context.Context, zr io.Reader, delim byte, dst []byte) ([]byte, error) {
	var one [1]byte
	n := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, err := zr.Read(one[:])
		if m == 1 {
			if one[0] == delim {
				return dst[:n], nil
			}
			if n == len(dst) {
				return nil, objfile.ErrHeader
			}
			dst[n] = one[0]
			n++
		}
		if err == io.EOF {
			return nil, objfile.ErrHeader
		}
		if err != nil {
			return nil, err
		}
	}
}

// syncDirs syncs every fan-out directory written to and, when one was
// created, the objects directory.
func (db *looseDB) syncDirs() error {
	if !db.sync {
		return nil
	}
	if db.batch {
		// One platform batch: the new objects, their fan-out directories
		// and (when one was created) the objects directory.
		if len(db.pending) == 0 && len(db.dirty) == 0 && !db.created {
			return nil
		}
		for _, op := range []string{"object-sync", "object-dirsync"} {
			if err := db.d.check(db.opPref + op); err != nil {
				return err
			}
		}
		dirs := make([]string, 0, len(db.dirty)+1)
		for fan := range db.dirty {
			dirs = append(dirs, fan)
		}
		if db.created {
			dirs = append(dirs, ".")
		}
		if err := db.d.syncBatch(db.objFD, db.pending, dirs); err != nil {
			return err
		}
		db.pending = nil
		return nil
	}
	for fan := range db.dirty {
		fd, err := openDirAt(db.objFD, fan)
		if err != nil {
			return err
		}
		err = db.d.sync(fd, db.opPref+"object-dirsync")
		closeFD(fd)
		if err != nil {
			return err
		}
	}
	if db.created {
		return db.d.sync(db.objFD, db.opPref+"object-dirsync")
	}
	return nil
}

// writeObject stores an encodable object (tree or commit) in db.
func (db *looseDB) writeObject(ctx context.Context, obj interface {
	Encode(plumbing.EncodedObject) error
}) (plumbing.Hash, error) {
	o := &plumbing.MemoryObject{}
	if err := obj.Encode(o); err != nil {
		return plumbing.ZeroHash, err
	}
	r, err := o.Reader()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	defer r.Close()
	return db.write(ctx, o.Type(), o.Size(), r)
}

// Closure errors.
type errClosure struct{ reason string }

func (e errClosure) Error() string { return "invalid object closure: " + e.reason }

// lfsHeader is the first line of a canonical Git LFS pointer.
const lfsHeader = "version https://git-lfs.github.com/spec/v1"

// maxLFSPointer bounds a pointer file (the LFS specification's limit).
const maxLFSPointer = 1024

// isLFSPointer reports a canonical Git LFS pointer: the version line (LF
// or CRLF), optional ext- lines, then oid sha256:<64 hex> and size lines.
func isLFSPointer(b []byte) bool {
	if len(b) >= maxLFSPointer {
		return false
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(s, lfsHeader+"\n") {
		return false
	}
	var oid, size bool
	for _, l := range strings.Split(strings.TrimSuffix(s[len(lfsHeader)+1:], "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "oid sha256:") && len(l) == len("oid sha256:")+64:
			oid = true
		case strings.HasPrefix(l, "size "):
			size = len(l) > len("size ")
		case strings.HasPrefix(l, "ext-"):
		default:
			return false
		}
	}
	return oid && size
}

// closureStats counts a closure walk.
type closureStats struct {
	objects, bytes int64
}

// safeEntryName rejects empty names, "." and "..", slash or NUL and .git
// in any ASCII case (the plane's own rule).
func safeEntryName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\x00") && !strings.EqualFold(n, ".git")
}

// walkClosure validates the complete typed closure of the commits roots
// in s: every object exists, has the type its referrer requires, decodes
// and hashes to its ID; gitlinks and Git LFS pointers are unsupported;
// tree entries are safe single components with supported modes. With
// lfs, canonical Git LFS pointer blobs are unsupported too (git sources
// and git destinations; a folder's content is never interpreted).
func walkClosure(ctx context.Context, s storer.EncodedObjectStorer, roots []plumbing.Hash, st *closureStats, lfs bool) (map[plumbing.Hash]plumbing.ObjectType, error) {
	type pending struct {
		h plumbing.Hash
		t plumbing.ObjectType
	}
	visited := map[plumbing.Hash]plumbing.ObjectType{}
	stack := make([]pending, 0, len(roots))
	for i := len(roots) - 1; i >= 0; i-- {
		stack = append(stack, pending{roots[i], plumbing.CommitObject})
	}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if t, ok := visited[p.h]; ok {
			if t != p.t {
				return nil, errClosure{fmt.Sprintf("%s is a %s, want a %s", p.h, t, p.t)}
			}
			continue
		}
		visited[p.h] = p.t
		o, err := s.EncodedObject(plumbing.AnyObject, p.h)
		if err != nil {
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				return nil, errClosure{fmt.Sprintf("missing %s %s", p.t, p.h)}
			}
			return nil, err
		}
		if o.Type() != p.t {
			return nil, errClosure{fmt.Sprintf("%s is a %s, want a %s", p.h, o.Type(), p.t)}
		}
		head, err := verifyHash(o, st)
		if err != nil {
			return nil, err
		}
		switch p.t {
		case plumbing.BlobObject:
			if lfs && o.Size() < maxLFSPointer && isLFSPointer(head) {
				return nil, errUnsupported("Git LFS pointer files in the history")
			}
		case plumbing.CommitObject:
			c := &object.Commit{}
			if err := c.Decode(o); err != nil {
				return nil, errClosure{"malformed commit " + p.h.String()}
			}
			stack = append(stack, pending{c.TreeHash, plumbing.TreeObject})
			for i := len(c.ParentHashes) - 1; i >= 0; i-- {
				stack = append(stack, pending{c.ParentHashes[i], plumbing.CommitObject})
			}
		case plumbing.TreeObject:
			t := &object.Tree{}
			if err := t.Decode(o); err != nil {
				return nil, errClosure{"malformed tree " + p.h.String()}
			}
			for i := len(t.Entries) - 1; i >= 0; i-- {
				e := t.Entries[i]
				if !safeEntryName(e.Name) {
					return nil, errUnsafeTree("the history has an unsafe tree entry name (empty, dot, a slash or .git)")
				}
				switch e.Mode {
				case filemode.Dir:
					stack = append(stack, pending{e.Hash, plumbing.TreeObject})
				case filemode.Regular, filemode.Deprecated, filemode.Executable, filemode.Symlink:
					stack = append(stack, pending{e.Hash, plumbing.BlobObject})
				case filemode.Submodule:
					return nil, errUnsupported("submodules (gitlinks) in the history")
				default:
					return nil, errClosure{"unsupported entry mode in " + p.h.String()}
				}
			}
		}
	}
	return visited, nil
}

// verifyHash streams o's content through the object hasher and returns
// the content's first maxLFSPointer bytes.
func verifyHash(o plumbing.EncodedObject, st *closureStats) ([]byte, error) {
	r, err := o.Reader()
	if err != nil {
		return nil, errClosure{"unreadable " + o.Hash().String()}
	}
	defer r.Close()
	h := plumbing.NewHasher(o.Type(), o.Size())
	var head bytes.Buffer
	n, err := io.Copy(io.MultiWriter(h, &limitedBuffer{b: &head, max: maxLFSPointer}), r)
	if err != nil || n != o.Size() || h.Sum() != o.Hash() {
		return nil, errClosure{"hash mismatch for " + o.Hash().String()}
	}
	if st != nil {
		st.objects++
		st.bytes += n
	}
	return head.Bytes(), nil
}

type limitedBuffer struct {
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// isAncestor reports whether anc is new or reachable from new through
// any parent.
func isAncestor(ctx context.Context, s storer.EncodedObjectStorer, anc, new plumbing.Hash) (bool, error) {
	if anc == new {
		return true, nil
	}
	seen := map[plumbing.Hash]bool{new: true}
	stack := []plumbing.Hash{new}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		c, err := object.GetCommit(s, h)
		if err != nil {
			return false, err
		}
		for _, p := range c.ParentHashes {
			if p == anc {
				return true, nil
			}
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return false, nil
}

// writePack encodes exactly the objects hashes from s (no deltas: every
// object is decoded and re-encoded, streamed) into w.
func writePack(ctx context.Context, s storer.EncodedObjectStorer, hashes []plumbing.Hash, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := packfile.NewEncoder(&ctxWriter{ctx: ctx, w: w}, s, false).Encode(hashes, 0)
	return err
}

// ctxWriter fails writes once ctx is done.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}

// multiStore reads objects from the first store holding them.
type multiStore struct {
	storer.EncodedObjectStorer
	others []storer.EncodedObjectStorer
}

func (m multiStore) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	o, err := m.EncodedObjectStorer.EncodedObject(t, h)
	if err == nil || !errors.Is(err, plumbing.ErrObjectNotFound) {
		return o, err
	}
	for _, s := range m.others {
		if o, err := s.EncodedObject(t, h); err == nil || !errors.Is(err, plumbing.ErrObjectNotFound) {
			return o, err
		}
	}
	return nil, plumbing.ErrObjectNotFound
}

func (m multiStore) HasEncodedObject(h plumbing.Hash) error {
	if err := m.EncodedObjectStorer.HasEncodedObject(h); err == nil {
		return nil
	}
	for _, s := range m.others {
		if s.HasEncodedObject(h) == nil {
			return nil
		}
	}
	return plumbing.ErrObjectNotFound
}
