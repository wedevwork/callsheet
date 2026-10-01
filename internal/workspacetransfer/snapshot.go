package workspacetransfer

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/sys/unix"
)

// Plain-folder snapshots (FP-3). A classified plain folder is walked with
// its root and nested .gitignore files only (byte-exact, never the global
// or info excludes), and stored as git objects in a private temporary
// store: regular files as 100644 or, with any execute bit, 100755;
// symlinks as 120000 with their link text (never followed); empty
// directories are omitted. Tree entry names are the raw readdir bytes,
// never normalized. Any .git entry is a nested repository (unsupported);
// a nonignored socket, FIFO or device is refused before anything is
// published, and an ignored one is skipped by lstat without opening it.

// Snapshot identity: a fixed author and committer at the Unix epoch with
// a fixed message and no signature, so the same parent and tree always
// give the same commit.
const (
	snapshotName    = "Callsheet"
	snapshotEmail   = "workspace@callsheet.invalid"
	snapshotMessage = "Callsheet workspace snapshot\n"
)

// snapshotCommit is the deterministic snapshot commit of tree with the
// optional parent.
func snapshotCommit(tree plumbing.Hash, parent plumbing.Hash) *object.Commit {
	sig := object.Signature{Name: snapshotName, Email: snapshotEmail, When: time.Unix(0, 0).UTC()}
	c := &object.Commit{Author: sig, Committer: sig, Message: snapshotMessage, TreeHash: tree}
	if !parent.IsZero() {
		c.ParentHashes = []plumbing.Hash{parent}
	}
	return c
}

// snapWalk is one folder scan.
type snapWalk struct {
	ctx context.Context
	d   *deps
	db  *looseDB
	ign ignoreStack
	man *manifest

	files, bytes, objects int64
}

// snapshotFolder scans the plain folder rootFD into db and returns the
// root tree (the empty tree for an empty folder) and the manifest.
func (d *deps) snapshotFolder(ctx context.Context, rootFD int, db *looseDB) (plumbing.Hash, *manifest, error) {
	w := &snapWalk{ctx: ctx, d: d, db: db, man: newManifest()}
	tree, _, err := w.dir(rootFD, "")
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}
	if tree.IsZero() {
		if tree, err = db.writeObject(ctx, &object.Tree{}); err != nil {
			return plumbing.ZeroHash, nil, orStorage(ctxOr(ctx, err), "cannot store the snapshot")
		}
	}
	d.emit("snapshot-files", w.files)
	d.emit("snapshot-bytes-read", w.bytes)
	d.emit("snapshot-objects", w.objects)
	return tree, w.man, nil
}

// sortTree applies git's tree ordering (directories compare with '/').
func sortTree(entries []object.TreeEntry) {
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })
}

// dir snapshots one directory (raw carries a trailing slash below the
// root) and returns its tree, or the zero hash when nothing in it is
// stored.
func (w *snapWalk) dir(fd int, raw string) (plumbing.Hash, bool, error) {
	if err := w.ctx.Err(); err != nil {
		return plumbing.ZeroHash, false, err
	}
	st, err := fstatFD(fd)
	if err != nil {
		return plumbing.ZeroHash, false, errStorage("cannot read the source folder", err)
	}
	names, err := readNames(fd)
	if err != nil {
		return plumbing.ZeroHash, false, errStorage("cannot read the source folder", err)
	}
	md := w.man.dir(strings.TrimSuffix(raw, "/"), names, identityOf(&st))
	for _, n := range names {
		if strings.EqualFold(n, ".git") {
			return plumbing.ZeroHash, false, errUnsupported("a nested repository (.git) in the folder")
		}
	}
	var ign *ignoreList
	if gst, err := lstatAt(fd, ".gitignore"); err == nil && isRegStat(&gst) {
		f, fst, err := openFileAt(fd, ".gitignore")
		if err != nil {
			return plumbing.ZeroHash, false, errSourceChanged()
		}
		b, rerr := readAllCtx(w.ctx, f)
		f.Close()
		if rerr != nil {
			return plumbing.ZeroHash, false, ctxOr(w.ctx, errStorage("cannot read the source folder", rerr))
		}
		if identityOf(&fst) != identityOf(&gst) {
			return plumbing.ZeroHash, false, errSourceChanged()
		}
		md.file(".gitignore", &gst)
		ign = &ignoreList{patterns: parseIgnore(b, raw, false)}
	}
	w.ign.push(ign)
	defer w.ign.pop()
	var entries []object.TreeEntry
	for _, n := range names {
		est, err := lstatAt(fd, n)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return plumbing.ZeroHash, false, errSourceChanged()
			}
			return plumbing.ZeroHash, false, errStorage("cannot read the source folder", err)
		}
		isDir := isDirStat(&est)
		if w.ign.excluded(raw+n, isDir) {
			continue
		}
		md.file(n, &est)
		switch {
		case isDir:
			sub, err := openDirAt(fd, n)
			if err != nil {
				return plumbing.ZeroHash, false, errSourceChanged()
			}
			h, ok, err := w.dir(sub, raw+n+"/")
			closeFD(sub)
			if err != nil {
				return plumbing.ZeroHash, false, err
			}
			if ok {
				entries = append(entries, object.TreeEntry{Name: n, Mode: filemode.Dir, Hash: h})
			}
		case isRegStat(&est):
			h, err := w.file(fd, n, &est)
			if err != nil {
				return plumbing.ZeroHash, false, err
			}
			mode := filemode.Regular
			if est.Mode&0o111 != 0 {
				mode = filemode.Executable
			}
			entries = append(entries, object.TreeEntry{Name: n, Mode: mode, Hash: h})
		case isLinkStat(&est):
			text, err := readlinkAt(fd, n)
			if err != nil {
				return plumbing.ZeroHash, false, errSourceChanged()
			}
			h, err := w.db.write(w.ctx, plumbing.BlobObject, int64(len(text)), strings.NewReader(text))
			if err != nil {
				return plumbing.ZeroHash, false, orStorage(ctxOr(w.ctx, err), "cannot store the snapshot")
			}
			w.objects++
			entries = append(entries, object.TreeEntry{Name: n, Mode: filemode.Symlink, Hash: h})
		default:
			return plumbing.ZeroHash, false, errUnsafeTree("the folder holds a socket, FIFO or device file that is not ignored; ignore or remove it")
		}
	}
	if len(entries) == 0 {
		return plumbing.ZeroHash, false, nil
	}
	sortTree(entries)
	h, err := w.db.writeObject(w.ctx, &object.Tree{Entries: entries})
	if err != nil {
		return plumbing.ZeroHash, false, orStorage(ctxOr(w.ctx, err), "cannot store the snapshot")
	}
	w.objects++
	return h, true, nil
}

// file stores one regular file as a blob; its identity must hold across
// the read.
func (w *snapWalk) file(fd int, name string, st *unix.Stat_t) (plumbing.Hash, error) {
	f, fst, err := openFileAt(fd, name)
	if err != nil {
		return plumbing.ZeroHash, errSourceChanged()
	}
	defer f.Close()
	if identityOf(&fst) != identityOf(st) {
		return plumbing.ZeroHash, errSourceChanged()
	}
	h, err := w.db.write(w.ctx, plumbing.BlobObject, st.Size, f)
	if err != nil {
		if errors.Is(err, errShortRead) {
			return plumbing.ZeroHash, errSourceChanged()
		}
		return plumbing.ZeroHash, orStorage(ctxOr(w.ctx, err), "cannot store the snapshot")
	}
	after, err := fstatFD(int(f.Fd()))
	if err != nil || identityOf(&after) != identityOf(st) {
		return plumbing.ZeroHash, errSourceChanged()
	}
	w.files++
	w.objects++
	w.bytes += st.Size
	return h, nil
}
