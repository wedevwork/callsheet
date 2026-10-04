package workspacetransfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Task workspace primitives (iteration 10b): the narrow pieces of folder
// export, plain-folder snapshot and object handling a workspace task's
// worker uses, exported without the CLI-oriented Push and Pull policies
// (which keep their behavior). A task owns a trusted object database
// outside its work directory, a private checkout materialized into its
// existing empty work directory with an independent ordinary .git, and a
// result snapshot of the work directory under the plain-folder ignore
// policy. Every write is descriptor-relative and never follows a link.

// TaskDB is a private trusted object database: a bare layout
// dir/{objects/pack,refs} (directories 0700), written with synced loose
// objects published without replacement and received packs; it never has
// alternates and is never shared with a child.
type TaskDB struct {
	d     *deps
	dir   string
	objFD int
	store *filesystem.Storage
	db    *looseDB
}

// CreateTaskDB creates the database directory dir (absent; its parent
// exists) and opens it.
func CreateTaskDB(dir string) (*TaskDB, error) { return defaultDeps().createTaskDB(dir) }

func (d *deps) createTaskDB(dir string) (*TaskDB, error) {
	if err := d.check("taskdb-create"); err != nil {
		return nil, errStorage("cannot create the task object database", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, errStorage("cannot create the task object database", err)
	}
	for _, p := range []string{"objects", filepath.Join("objects", "pack"), "refs"} {
		if err := os.Mkdir(filepath.Join(dir, p), 0o700); err != nil {
			return nil, errStorage("cannot create the task object database", err)
		}
	}
	return d.openTaskDB(dir)
}

// OpenTaskDB opens an existing database directory created by CreateTaskDB:
// a real directory (never a link) holding real objects and refs
// directories.
func OpenTaskDB(dir string) (*TaskDB, error) { return defaultDeps().openTaskDB(dir) }

func (d *deps) openTaskDB(dir string) (*TaskDB, error) {
	for _, p := range []string{dir, filepath.Join(dir, "objects"), filepath.Join(dir, "objects", "pack"), filepath.Join(dir, "refs")} {
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, errStorage("cannot open the task object database", err)
		}
		if !fi.IsDir() {
			return nil, errStorage("cannot open the task object database", errors.New("not a directory"))
		}
	}
	objFD, err := openPathDir(filepath.Join(dir, "objects"))
	if err != nil {
		return nil, errStorage("cannot open the task object database", err)
	}
	t := &TaskDB{d: d, dir: dir, objFD: objFD}
	t.store = filesystem.NewStorageWithOptions(osfs.New(dir), cache.NewObjectLRU(8<<20), filesystem.Options{LargeObjectThreshold: largeObject})
	t.db = &looseDB{d: d, objFD: objFD, sync: true, batch: true, opPref: "taskdb-"}
	return t, nil
}

// Close releases the database (its directory stays).
func (t *TaskDB) Close() {
	if t.store != nil {
		t.store.Close()
		t.store = nil
	}
	if t.objFD >= 0 {
		closeFD(t.objFD)
		t.objFD = -1
	}
}

// Dir is the database directory.
func (t *TaskDB) Dir() string { return t.dir }

// Store is the database's object storer (reads, and the closure walk).
func (t *TaskDB) Store() storer.EncodedObjectStorer { return t.store }

// Has reports whether h is present (not verified).
func (t *TaskDB) Has(h plumbing.Hash) bool { return t.store.HasEncodedObject(h) == nil }

// ReceivePack stores a received pack stream (packfile parsing writes the
// pack and its index into objects/pack) and syncs the pack directory.
func (t *TaskDB) ReceivePack(ctx context.Context, r io.Reader) error {
	if err := t.d.check("taskdb-pack"); err != nil {
		return errStorage("cannot store the fetched objects", err)
	}
	if err := packfile.UpdateObjectStorage(t.store, &ctxReader{ctx: ctx, r: r}); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return errStorage("cannot store the fetched objects", err)
	}
	return t.syncPack()
}

func (t *TaskDB) syncPack() error {
	fd, err := openDirAt(t.objFD, "pack")
	if err != nil {
		return errStorage("cannot sync the task object database", err)
	}
	defer closeFD(fd)
	if err := t.d.sync(fd, "taskdb-dirsync"); err != nil {
		return errStorage("cannot sync the task object database", err)
	}
	return nil
}

// ctxReader fails reads once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// CopyStats counts a closure copy.
type CopyStats struct {
	Objects, Bytes int64
}

// CopyFrom copies the objects hashes from src (each streamed, re-hashed by
// the loose writer and checked against its ID) into the database; objects
// already present are verified, never overwritten.
func (t *TaskDB) CopyFrom(ctx context.Context, src storer.EncodedObjectStorer, hashes []plumbing.Hash) (CopyStats, error) {
	return copyObjects(ctx, t.db, src, hashes)
}

func copyObjects(ctx context.Context, db *looseDB, src storer.EncodedObjectStorer, hashes []plumbing.Hash) (CopyStats, error) {
	var st CopyStats
	for _, h := range hashes {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		o, err := src.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return st, errIntegrity("a selected object cannot be read")
		}
		r, err := o.Reader()
		if err != nil {
			return st, errIntegrity("a selected object cannot be read")
		}
		got, err := db.write(ctx, o.Type(), o.Size(), r)
		r.Close()
		if err != nil {
			return st, orStorage(ctxOr(ctx, err), "cannot copy the selected objects")
		}
		if got != h {
			return st, errIntegrity("a selected object does not match its hash")
		}
		st.Objects++
		st.Bytes += o.Size()
	}
	if err := db.syncDirs(); err != nil {
		return st, errStorage("cannot sync the copied objects", err)
	}
	return st, nil
}

// Closure validates the complete typed closure of the commit roots in the
// database (every object present, typed, decodable and hashing to its ID;
// safe names and modes; no gitlinks) and returns it in traversal order.
func (t *TaskDB) Closure(ctx context.Context, roots []plumbing.Hash) ([]plumbing.Hash, CopyStats, error) {
	return ClosureOf(ctx, t.store, roots)
}

// ClosureOf is Closure over any storer (a staging database combined with
// verified cache objects, for example).
func ClosureOf(ctx context.Context, s storer.EncodedObjectStorer, roots []plumbing.Hash) ([]plumbing.Hash, CopyStats, error) {
	st := &closureStats{}
	types, err := walkClosureOrdered(ctx, s, roots, st)
	if err != nil {
		return nil, CopyStats{}, closureFailure(ctx, err)
	}
	return types, CopyStats{Objects: st.objects, Bytes: st.bytes}, nil
}

// walkClosureOrdered is walkClosure returning the closure in traversal
// order (a deterministic copy order).
func walkClosureOrdered(ctx context.Context, s storer.EncodedObjectStorer, roots []plumbing.Hash, st *closureStats) ([]plumbing.Hash, error) {
	types, err := walkClosure(ctx, s, roots, st, false)
	if err != nil {
		return nil, err
	}
	out := make([]plumbing.Hash, 0, len(types))
	seen := map[plumbing.Hash]bool{}
	var visit func(h plumbing.Hash) error
	stack := append([]plumbing.Hash(nil), roots...)
	visit = func(h plumbing.Hash) error {
		if seen[h] {
			return nil
		}
		seen[h] = true
		out = append(out, h)
		switch types[h] {
		case plumbing.CommitObject:
			c, err := object.GetCommit(s, h)
			if err != nil {
				return err
			}
			stack = append(stack, c.TreeHash)
			stack = append(stack, c.ParentHashes...)
		case plumbing.TreeObject:
			tr, err := object.GetTree(s, h)
			if err != nil {
				return err
			}
			for _, e := range tr.Entries {
				stack = append(stack, e.Hash)
			}
		}
		return nil
	}
	for len(stack) > 0 {
		h := stack[0]
		stack = stack[1:]
		if err := visit(h); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// WriteObject stores an encodable object (a tree or commit) synced.
func (t *TaskDB) WriteObject(ctx context.Context, obj interface {
	Encode(plumbing.EncodedObject) error
}) (plumbing.Hash, error) {
	h, err := t.db.writeObject(ctx, obj)
	if err != nil {
		return plumbing.ZeroHash, orStorage(ctxOr(ctx, err), "cannot store the object")
	}
	return h, nil
}

// Sync makes every loose object written so far durable (fan-out and
// objects directories).
func (t *TaskDB) Sync() error {
	if err := t.db.syncDirs(); err != nil {
		return errStorage("cannot sync the task object database", err)
	}
	t.db.dirty, t.db.created = nil, false
	return nil
}

// WritePack encodes exactly hashes (no deltas) into w.
func (t *TaskDB) WritePack(ctx context.Context, hashes []plumbing.Hash, w io.Writer) error {
	return writePack(ctx, t.store, hashes, w)
}

// LogicalSize returns the regular-file bytes below the database, without
// following links (the cache's eviction measure).
func LogicalSize(ctx context.Context, dir string) (int64, error) {
	fd, err := openPathDir(dir)
	if err != nil {
		return 0, err
	}
	defer closeFD(fd)
	return sizeAt(ctx, fd)
}

func sizeAt(ctx context.Context, fd int) (int64, error) {
	names, err := readNames(fd)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		st, err := lstatAt(fd, n)
		if err != nil {
			return 0, err
		}
		switch {
		case isDirStat(&st):
			sub, err := openDirAt(fd, n)
			if err != nil {
				return 0, err
			}
			s, err := sizeAt(ctx, sub)
			closeFD(sub)
			if err != nil {
				return 0, err
			}
			total += s
		case isRegStat(&st):
			total += st.Size
		}
	}
	return total, nil
}

// RemoveTree removes the directory path recursively without following any
// symlink (a missing path is success). Read-only loose objects are
// unlinked through their writable directories.
func RemoveTree(path string) error {
	parent, err := openPathDir(filepath.Dir(path))
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer closeFD(parent)
	if err := makeWritable(parent, filepath.Base(path)); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return removeTreeAt(parent, filepath.Base(path))
}

// makeWritable gives every directory below name owner write and search
// permission (a child may have removed them), never following a link.
func makeWritable(dfd int, name string) error {
	st, err := lstatAt(dfd, name)
	if err != nil || !isDirStat(&st) {
		return err
	}
	if err := unix.Fchmodat(dfd, name, uint32(st.Mode)&0o7777|0o700, 0); err != nil {
		return err
	}
	fd, err := openDirAt(dfd, name)
	if err != nil {
		return err
	}
	defer closeFD(fd)
	names, err := readNames(fd)
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := makeWritable(fd, n); err != nil {
			return err
		}
	}
	return nil
}

// ---- Checkout ----

// TaskGitName is the child's own repository directory inside work.
const TaskGitName = ".git"

// Child repository content: the minimal local configuration (no remotes,
// hooks, alternates or filters; autocrlf off, filemode on), the detached
// or unborn HEAD.
const (
	taskGitConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = false\n\tautocrlf = false\n"
	unbornHead    = "ref: refs/heads/main\n"
)

// CheckoutOptions is one private checkout: the explicit OS, the existing
// empty canonical work directory (the child's cwd, 0700), the database
// holding the selected closure and the base commit (zero for the explicit
// empty base: an unborn HEAD and an empty index).
type CheckoutOptions struct {
	GOOS   string
	Work   string
	DB     *TaskDB
	Commit plumbing.Hash
	// listNames reads a directory's native names (tests force the darwin
	// normalization spelling on Linux).
	listNames func(fd int) ([]string, error)
}

// Checkout is a materialized base: its raw-to-native pathname map and the
// counts of files and bytes written.
type Checkout struct {
	Map   *PathMap
	Files int64
	Bytes int64
}

// PathMap maps the native spelling of every materialized base path whose
// directory entry differs from its raw tree path (normalization-insensitive
// APFS) back to the raw path; it is the identity on Linux. It is
// one-to-one by construction.
type PathMap struct {
	native map[string]string
	raw    map[string]string
}

// NewPathMap is the identity map.
func NewPathMap() *PathMap { return &PathMap{native: map[string]string{}, raw: map[string]string{}} }

// Raw returns the raw tree path of a native relative path: a mapped path
// component by component, its observed raw bytes otherwise.
func (m *PathMap) Raw(native string) string {
	if m == nil || len(m.native) == 0 {
		return native
	}
	if r, ok := m.native[native]; ok {
		return r
	}
	if i := strings.LastIndexByte(native, '/'); i >= 0 {
		return m.Raw(native[:i]) + native[i:]
	}
	return native
}

// Len is the number of mapped (differing) paths.
func (m *PathMap) Len() int {
	if m == nil {
		return 0
	}
	return len(m.native)
}

func (m *PathMap) add(native, raw string) error {
	if r, ok := m.native[native]; ok && r != raw {
		return errCollision()
	}
	if n, ok := m.raw[raw]; ok && n != native {
		return errCollision()
	}
	m.native[native], m.raw[raw] = raw, native
	return nil
}

// CheckoutTask materializes the base commit's tree from the database into
// the empty work directory with 09b's export rules (exact names, case and
// normalization collisions refused, modes 0644/0755, directories 0755,
// safe relative symlinks created last, native path budgets), then
// initializes the child's independent ordinary repository work/.git: its
// own copy of the selected history, a detached HEAD at the base (or an
// unborn refs/heads/main for empty), the exact base index and the minimal
// configuration.
func CheckoutTask(ctx context.Context, o CheckoutOptions) (*Checkout, error) {
	return defaultDeps().checkoutTask(ctx, o)
}

func (d *deps) checkoutTask(ctx context.Context, o CheckoutOptions) (*Checkout, error) {
	if err := checkGOOS(o.GOOS); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(o.Work) || filepath.Clean(o.Work) != o.Work {
		return nil, errStorage("the work directory must be an absolute clean path", errors.New("relative work"))
	}
	workFD, err := openPathDir(o.Work)
	if err != nil {
		return nil, errStorage("cannot open the work directory", err)
	}
	defer closeFD(workFD)
	if names, err := readNames(workFD); err != nil || len(names) > 0 {
		return nil, errNotEmpty()
	}
	// The child's repository paths (the longest a fan-out object or the
	// index lock) must fit the native budget too.
	if len(o.Work)+len("/.git/objects/00/")+38+len(".lock") > contract.MaxPathFor(o.GOOS) {
		return nil, errUnsafeTree("the task's work directory path is too long for this filesystem")
	}
	var entries []exportEntry
	if !o.Commit.IsZero() {
		dest := &exportDest{parent: filepath.Dir(o.Work), name: filepath.Base(o.Work), parentFD: -1}
		if entries, err = collectTree(ctx, o.GOOS, o.DB.store, o.Commit, dest); err != nil {
			return nil, err
		}
		if o.GOOS != "darwin" {
			aliasing, err := d.aliasProbe(workFD)
			if err != nil {
				return nil, errStorage("cannot probe the work filesystem", err)
			}
			if aliasing {
				if err := checkLinks(o.DB.store, entries, aliasKey(cases.Fold())); err != nil {
					return nil, err
				}
			}
		}
	}
	m := &materializer{ctx: ctx, d: d, s: o.DB.store, rootFD: workFD, noSync: true}
	defer m.closeAll()
	if err := m.run(entries); err != nil {
		return nil, err
	}
	if err := m.finishDirs(0o700); err != nil {
		return nil, err
	}
	co := &Checkout{Map: NewPathMap(), Bytes: m.bytes}
	list := o.listNames
	if list == nil {
		list = readNames
	}
	if o.GOOS == "darwin" || o.listNames != nil {
		if err := observeNames(ctx, workFD, entries, co.Map, list); err != nil {
			return nil, err
		}
	}
	idx := &index.Index{Version: 2}
	for _, e := range entries {
		if e.mode == filemode.Dir {
			continue
		}
		co.Files++
		dir, name := split(co.Map.nativeOf(e.path))
		fd, err := openRel(workFD, dir)
		if err != nil {
			return nil, errStorage("cannot index the checkout", err)
		}
		st, err := lstatAt(fd, name)
		closeFD(fd)
		if err != nil {
			return nil, errStorage("cannot index the checkout", err)
		}
		idx.Entries = append(idx.Entries, &index.Entry{Hash: e.hash, Name: e.path, Mode: e.mode, Size: uint32(st.Size), Dev: uint32(st.Dev),
			Inode: uint32(st.Ino), UID: st.Uid, GID: st.Gid, CreatedAt: time.Unix(st.Ctim.Unix()), ModifiedAt: time.Unix(st.Mtim.Unix())})
	}
	sortIndex(idx)
	if err := d.initChildRepo(ctx, workFD, o.DB, o.Commit, idx); err != nil {
		return nil, err
	}
	if err := d.sync(workFD, "checkout-dirsync"); err != nil {
		return nil, errStorage("cannot sync the checkout", err)
	}
	return co, nil
}

// nativeOf is the native spelling of a raw path (the identity unless
// mapped).
func (m *PathMap) nativeOf(raw string) string {
	if m == nil || len(m.raw) == 0 {
		return raw
	}
	if n, ok := m.raw[raw]; ok {
		return n
	}
	if i := strings.LastIndexByte(raw, '/'); i >= 0 {
		return m.nativeOf(raw[:i]) + raw[i:]
	}
	return raw
}

// observeNames records, for every materialized entry, the native spelling
// readdir reports for it: the exact raw name when present, otherwise the
// unique entry with the same normalization (NFC) identity. A missing or
// ambiguous spelling is a collision.
func observeNames(ctx context.Context, rootFD int, entries []exportEntry, m *PathMap, list func(int) ([]string, error)) error {
	byDir := map[string][]exportEntry{}
	var dirs []string
	for _, e := range entries {
		dir, _ := split(e.path)
		if _, ok := byDir[dir]; !ok {
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], e)
	}
	if _, ok := byDir[""]; !ok && len(entries) > 0 {
		dirs = append([]string{""}, dirs...)
	}
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		fd, err := openRel(rootFD, m.nativeOf(dir))
		if err != nil {
			return errStorage("cannot read the checkout", err)
		}
		names, err := list(fd)
		closeFD(fd)
		if err != nil {
			return errStorage("cannot read the checkout", err)
		}
		have := map[string]bool{}
		for _, n := range names {
			have[n] = true
		}
		nativeDir := m.nativeOf(dir)
		for _, e := range byDir[dir] {
			_, raw := split(e.path)
			if have[raw] {
				continue
			}
			var match []string
			for _, n := range names {
				if utf8.ValidString(n) && utf8.ValidString(raw) && norm.NFC.String(n) == norm.NFC.String(raw) {
					match = append(match, n)
				}
			}
			if len(match) != 1 {
				return errCollision()
			}
			native := match[0]
			if nativeDir != "" {
				native = nativeDir + "/" + native
			}
			if err := m.add(native, e.path); err != nil {
				return err
			}
		}
	}
	return nil
}

// sortIndex orders index entries by raw path bytes (git's index order).
func sortIndex(idx *index.Index) {
	es := idx.Entries
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && es[j-1].Name > es[j].Name; j-- {
			es[j-1], es[j] = es[j], es[j-1]
		}
	}
}

// initChildRepo writes work/.git: HEAD, config, refs/{heads,tags},
// objects/{pack,info} with its own copy of the base's closure, and the
// index (directories 0700, files 0600).
func (d *deps) initChildRepo(ctx context.Context, workFD int, db *TaskDB, commit plumbing.Hash, idx *index.Index) error {
	fail := func(err error) error { return orStorage(ctxOr(ctx, err), "cannot initialize the task repository") }
	if err := d.check("checkout-git"); err != nil {
		return fail(err)
	}
	if err := unix.Mkdirat(workFD, TaskGitName, 0o700); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errCollision()
		}
		return fail(err)
	}
	gitFD, err := openDirAt(workFD, TaskGitName)
	if err != nil {
		return fail(err)
	}
	defer closeFD(gitFD)
	for _, p := range []string{"objects", "objects/pack", "objects/info", "refs", "refs/heads", "refs/tags"} {
		if err := mkdirRel(gitFD, p); err != nil {
			return fail(err)
		}
	}
	head := unbornHead
	if !commit.IsZero() {
		head = commit.String() + "\n"
	}
	var ib bytes.Buffer
	if err := index.NewEncoder(&ib).Encode(idx); err != nil {
		return fail(err)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{"HEAD", []byte(head)}, {"config", []byte(taskGitConfig)}, {"index", ib.Bytes()}} {
		if err := writeFileAt(gitFD, f.name, f.data); err != nil {
			return fail(err)
		}
	}
	if !commit.IsZero() {
		objFD, err := openDirAt(gitFD, "objects")
		if err != nil {
			return fail(err)
		}
		hashes, _, err := db.Closure(ctx, []plumbing.Hash{commit})
		if err == nil {
			_, err = copyObjects(ctx, &looseDB{d: d, objFD: objFD, opPref: "checkout-"}, db.store, hashes)
		}
		closeFD(objFD)
		if err != nil {
			return err
		}
	}
	return nil
}

func mkdirRel(dfd int, rel string) error {
	dir, name := split(rel)
	fd, err := openRel(dfd, dir)
	if err != nil {
		return err
	}
	defer closeFD(fd)
	return unix.Mkdirat(fd, name, 0o700)
}

// writeFileAt creates name below dfd exclusively (0600) with data.
func writeFileAt(dfd int, name string, data []byte) error {
	fd, err := unix.Openat(dfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// ---- Result commit ----

// TaskResultCommit is a task result's deterministic commit: the result
// tree, the parent list exactly [base] (empty for the empty base), the
// fixed author and committer at the Unix epoch in UTC and message, no
// signature or encoding.
func TaskResultCommit(tree, base plumbing.Hash, message string) *object.Commit {
	sig := object.Signature{Name: "Callsheet", Email: "workspace@callsheet.invalid", When: time.Unix(0, 0).UTC()}
	c := &object.Commit{Author: sig, Committer: sig, Message: message, TreeHash: tree}
	if !base.IsZero() {
		c.ParentHashes = []plumbing.Hash{base}
	}
	return c
}

// CommitHash is the object ID c encodes to.
func CommitHash(c *object.Commit) (plumbing.Hash, error) {
	o := &plumbing.MemoryObject{}
	if err := c.Encode(o); err != nil {
		return plumbing.ZeroHash, err
	}
	return o.Hash(), nil
}

// EmptyTree is the canonical empty tree's ID.
var EmptyTree = plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
