package workspacetransfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Task result snapshots (iteration 10b): the entire result tree is derived
// from the work directory alone, after every child writer is gone, with
// the plain-folder policy (root and nested .gitignore files only,
// byte-exact, negation and pruning, never global or info excludes,
// attributes or filters). Ignore rules apply to every candidate path,
// previously tracked ones included. The top-level .git the child owns and
// the exact owned runtime directory are excluded without traversal; any
// other included .git (an alias in case or, on darwin, HFS+ ignorable code
// points) is a nested repository and unsupported. Regular files are stored
// byte for byte (no clean filter or re-normalization), symlinks as their
// link text (never followed, and checked like a checkout's), empty
// directories omitted. Raw native names are mapped back to their base tree
// spelling through the checkout's path map.

// SnapshotOptions is one result snapshot.
type SnapshotOptions struct {
	GOOS string
	Work string
	DB   *TaskDB
	// Map is the checkout's raw-to-native path map (nil: identity).
	Map *PathMap
	// RuntimeDir is the exact owned top-level directory to exclude ("" for
	// none).
	RuntimeDir string
}

// Snapshot is a stored result tree and its scan counters.
type Snapshot struct {
	Tree    plumbing.Hash
	Files   int64
	Bytes   int64
	Objects int64
}

// errMetadataOverflow is a counter beyond the contract's safe integers.
func errMetadataOverflow() error {
	return contract.TransferError(contract.CodeInternal, contract.PubErrMetadataOverflow, "the result's tree metadata exceeds the safe integer range")
}

// SnapshotTask stores the work directory's result tree in the database
// (blobs streamed to disk, synced) and returns its ID: the canonical empty
// tree when nothing is included.
func SnapshotTask(ctx context.Context, o SnapshotOptions) (Snapshot, error) {
	return entryDeps().snapshotTask(ctx, o)
}

type taskWalk struct {
	ctx     context.Context
	d       *deps
	goos    string
	db      *looseDB
	m       *PathMap
	runtime string
	ign     ignoreStack
	man     *manifest
	entries []exportEntry
	snap    Snapshot
}

func (d *deps) snapshotTask(ctx context.Context, o SnapshotOptions) (Snapshot, error) {
	if err := checkGOOS(o.GOOS); err != nil {
		return Snapshot{}, err
	}
	rootFD, err := openPathDir(o.Work)
	if err != nil {
		return Snapshot{}, errStorage("cannot open the work directory", err)
	}
	defer closeFD(rootFD)
	w := &taskWalk{ctx: ctx, d: d, goos: o.GOOS, db: o.DB.db, m: o.Map, runtime: o.RuntimeDir, man: newManifest()}
	tree, ok, err := w.dir(rootFD, "", "")
	if err != nil {
		return Snapshot{}, err
	}
	if !ok {
		if tree, err = w.db.writeObject(ctx, &object.Tree{}); err != nil {
			return Snapshot{}, orStorage(ctxOr(ctx, err), "cannot store the snapshot")
		}
		w.snap.Objects++
	}
	// Result links must resolve inside the tree like a checkout's (the next
	// hop checks the result out), on an aliasing filesystem too.
	if err := checkLinks(o.DB.store, w.entries, nil); err != nil {
		return Snapshot{}, err
	}
	if o.GOOS == "darwin" {
		if err := checkLinks(o.DB.store, w.entries, aliasKey(cases.Fold())); err != nil {
			return Snapshot{}, err
		}
	}
	d.stage("task-snapshot-scanned")
	if err := w.man.verify(ctx, rootFD); err != nil {
		if errors.Is(err, errChanged) {
			return Snapshot{}, errSourceChanged()
		}
		return Snapshot{}, err
	}
	if err := w.db.syncDirs(); err != nil {
		return Snapshot{}, errStorage("cannot sync the snapshot", err)
	}
	w.snap.Tree = tree
	d.emit("task-snapshot-files", w.snap.Files)
	d.emit("task-snapshot-bytes", w.snap.Bytes)
	d.emit("task-snapshot-objects", w.snap.Objects)
	return w.snap, nil
}

// isGitAlias reports a name a checkout or git would treat as .git on goos.
func isGitAlias(goos, n string) bool {
	return strings.EqualFold(n, ".git") || (goos == "darwin" && isHFSDotGit(n))
}

// add adds n to a counter within the safe integer range.
func addSafe(c *int64, n int64) error {
	if n < 0 || *c > contract.MaxSafeInteger-n {
		return errMetadataOverflow()
	}
	*c += n
	return nil
}

// dir snapshots one directory: native is its native relative path, raw
// its raw tree path (both "" at the root, otherwise with a trailing
// slash for raw children).
func (w *taskWalk) dir(fd int, native, raw string) (plumbing.Hash, bool, error) {
	if err := w.ctx.Err(); err != nil {
		return plumbing.ZeroHash, false, err
	}
	st, err := fstatFD(fd)
	if err != nil {
		return plumbing.ZeroHash, false, errStorage("cannot read the work directory", err)
	}
	names, err := readNames(fd)
	if err != nil {
		return plumbing.ZeroHash, false, errStorage("cannot read the work directory", err)
	}
	md := w.man.dir(native, names, identityOf(&st))
	top := native == ""
	var ign *ignoreList
	if gst, err := lstatAt(fd, ".gitignore"); err == nil && isRegStat(&gst) {
		f, fst, err := openFileAt(fd, ".gitignore")
		if err != nil {
			return plumbing.ZeroHash, false, errSourceChanged()
		}
		b, rerr := readAllCtx(w.ctx, f)
		f.Close()
		if rerr != nil {
			return plumbing.ZeroHash, false, ctxOr(w.ctx, errStorage("cannot read the work directory", rerr))
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
		if top && (n == TaskGitName || (w.runtime != "" && n == w.runtime)) {
			// The child's own repository and the owned runtime directory are
			// never traversed or stored.
			continue
		}
		nativePath := n
		if native != "" {
			nativePath = native + "/" + n
		}
		rawPath := w.m.Raw(nativePath)
		rawName := rawPath[strings.LastIndexByte(rawPath, '/')+1:]
		est, err := lstatAt(fd, n)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return plumbing.ZeroHash, false, errSourceChanged()
			}
			return plumbing.ZeroHash, false, errStorage("cannot read the work directory", err)
		}
		isDir := isDirStat(&est)
		if w.ign.excluded(rawPath, isDir) {
			continue
		}
		if isGitAlias(w.goos, n) || isGitAlias(w.goos, rawName) {
			return plumbing.ZeroHash, false, errUnsupported("a nested repository (.git) in the task's result")
		}
		if !validName(w.goos, rawName) || len(rawName) > contract.MaxNameBytes {
			return plumbing.ZeroHash, false, errUnsafeTree("the task's result has an entry name that cannot be stored safely")
		}
		md.file(n, &est)
		switch {
		case isDir:
			sub, err := openDirAt(fd, n)
			if err != nil {
				return plumbing.ZeroHash, false, errSourceChanged()
			}
			h, ok, err := w.dir(sub, nativePath, rawPath+"/")
			closeFD(sub)
			if err != nil {
				return plumbing.ZeroHash, false, err
			}
			if ok {
				entries = append(entries, object.TreeEntry{Name: rawName, Mode: filemode.Dir, Hash: h})
				w.entries = append(w.entries, exportEntry{path: rawPath, mode: filemode.Dir, hash: h})
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
			entries = append(entries, object.TreeEntry{Name: rawName, Mode: mode, Hash: h})
			w.entries = append(w.entries, exportEntry{path: rawPath, mode: mode, hash: h})
		case isLinkStat(&est):
			text, err := readlinkAt(fd, n)
			if err != nil {
				return plumbing.ZeroHash, false, errSourceChanged()
			}
			if len(text) >= maxLinkTarget {
				return plumbing.ZeroHash, false, errUnsafeTree("the task's result has a symlink target too long to store safely")
			}
			h, err := w.db.write(w.ctx, plumbing.BlobObject, int64(len(text)), strings.NewReader(text))
			if err != nil {
				return plumbing.ZeroHash, false, orStorage(ctxOr(w.ctx, err), "cannot store the snapshot")
			}
			if err := addSafe(&w.snap.Objects, 1); err != nil {
				return plumbing.ZeroHash, false, err
			}
			entries = append(entries, object.TreeEntry{Name: rawName, Mode: filemode.Symlink, Hash: h})
			w.entries = append(w.entries, exportEntry{path: rawPath, mode: filemode.Symlink, hash: h})
		default:
			return plumbing.ZeroHash, false, errUnsafeTree("the task's result holds a socket, FIFO or device file that is not ignored")
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
	if err := addSafe(&w.snap.Objects, 1); err != nil {
		return plumbing.ZeroHash, false, err
	}
	return h, true, nil
}

// file stores one regular file as a blob; its identity must hold across
// the read.
func (w *taskWalk) file(fd int, name string, st *unix.Stat_t) (plumbing.Hash, error) {
	f, fst, err := openFileAt(fd, name)
	if err != nil {
		return plumbing.ZeroHash, errSourceChanged()
	}
	defer f.Close()
	if identityOf(&fst) != identityOf(st) {
		return plumbing.ZeroHash, errSourceChanged()
	}
	var src io.Reader = f
	if st.Size < maxLFSPointer {
		// A Git LFS pointer is an LFS representation, not ordinary data:
		// the result is unsupported (small files only can be one).
		b, err := readAllCtx(w.ctx, f)
		if err != nil {
			return plumbing.ZeroHash, ctxOr(w.ctx, errStorage("cannot read the work directory", err))
		}
		if int64(len(b)) != st.Size {
			return plumbing.ZeroHash, errSourceChanged()
		}
		if isLFSPointer(b) {
			return plumbing.ZeroHash, errUnsupported("a Git LFS pointer file in the task's result")
		}
		src = bytes.NewReader(b)
	}
	h, err := w.db.write(w.ctx, plumbing.BlobObject, st.Size, src)
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
	for _, c := range []struct {
		p *int64
		n int64
	}{{&w.snap.Files, 1}, {&w.snap.Objects, 1}, {&w.snap.Bytes, st.Size}} {
		if err := addSafe(c.p, c.n); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	return h, nil
}
