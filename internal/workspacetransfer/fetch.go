package workspacetransfer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Pull into a git repository (FP-5): the verified selected objects are
// installed in the repository's object database and exactly one
// Callsheet observation ref (refs/callsheet/NAME/...) is set with a local
// compare-and-swap under git-compatible locks. The checkout, index,
// HEAD, config, FETCH_HEAD, remotes and every other ref are never
// touched; no filter, hook or checkout runs.

// localRefState is the observed value of the Callsheet ref.
type localRefState struct {
	hash  plumbing.Hash
	found bool
}

// checkRefPath checks the generated ref and lock paths against goos's
// native limits.
func checkRefPath(goos, root, ref string) error {
	full := filepath.Join(root, ".git", filepath.FromSlash(ref)) + ".lock"
	if len(full) > contract.MaxPathFor(goos) || len(filepath.Join(root, ".git", "packed-refs.lock")) > contract.MaxPathFor(goos) {
		return contract.New(contract.CodeInvalidArgument, "the destination repository's path is too long for the Callsheet ref and its lock file")
	}
	for _, c := range strings.Split(ref, "/") {
		if len(c)+len(".lock") > contract.MaxNameBytes {
			return contract.New(contract.CodeInvalidArgument, "a Callsheet ref component is too long for the destination filesystem")
		}
	}
	return nil
}

// aliasIn reports an entry of the directory that differs from name but
// is the same name under ASCII case folding (a case-insensitive alias).
func aliasIn(dfd int, name string) (bool, error) {
	names, err := readNames(dfd)
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n != name && asciiFold(n) == asciiFold(name) {
			return true, nil
		}
	}
	return false, nil
}

// readLocalRef reads the Callsheet ref: refusing a symbolic ref, a
// namespace (prefix) conflict, a case alias and a HEAD or refs/heads
// symbolic chain that points at it.
func readLocalRef(gitFD int, ref string) (localRefState, error) {
	lr, err := looseRef(gitFD, ref)
	if err != nil {
		if errors.Is(err, errUnsafeMetadata) {
			return localRefState{}, errLocalRef("the Callsheet ref's path holds a symlink or an unexpected file; nothing was written")
		}
		return localRefState{}, errStorage("cannot read the destination's refs", err)
	}
	switch {
	case lr.isDir || lr.prefix:
		return localRefState{}, errLocalRef("the Callsheet ref conflicts with an existing ref namespace; nothing was written")
	case lr.found && lr.value.symbolic != "":
		return localRefState{}, errLocalRef("the Callsheet ref is a symbolic ref; it is never followed")
	}
	packed, err := readPackedRefs(gitFD)
	if err != nil {
		return localRefState{}, errLocalRef("the destination's packed-refs cannot be read safely")
	}
	for _, n := range packed.names {
		switch {
		case n != ref && asciiFold(n) == asciiFold(ref):
			return localRefState{}, errLocalRef("a ref that differs from the Callsheet ref only by case exists")
		case strings.HasPrefix(ref, n+"/") || strings.HasPrefix(n, ref+"/"):
			return localRefState{}, errLocalRef("the Callsheet ref conflicts with an existing ref namespace; nothing was written")
		}
	}
	if err := checkAliases(gitFD, ref); err != nil {
		return localRefState{}, err
	}
	if err := checkSymbolicChains(gitFD, ref); err != nil {
		return localRefState{}, err
	}
	if lr.found {
		return localRefState{hash: lr.value.hash, found: true}, nil
	}
	h, ok := packed.refs[ref]
	return localRefState{hash: h, found: ok}, nil
}

// checkAliases refuses an existing loose ref path component that differs
// from ref's only by case.
func checkAliases(gitFD int, ref string) error {
	fd, err := unix.Dup(gitFD)
	if err != nil {
		return err
	}
	for _, c := range strings.Split(ref, "/") {
		alias, err := aliasIn(fd, c)
		if err != nil {
			closeFD(fd)
			return errStorage("cannot read the destination's refs", err)
		}
		if alias {
			closeFD(fd)
			return errLocalRef("a ref path that differs from the Callsheet ref only by case exists")
		}
		next, err := openDirAt(fd, c)
		closeFD(fd)
		if err != nil {
			return nil
		}
		fd = next
	}
	closeFD(fd)
	return nil
}

// checkSymbolicChains refuses a current-branch chain that reaches ref:
// HEAD, every linked worktree's HEAD and every refs/heads symbolic ref
// are followed through any ref namespace (Git's five-hop limit, every
// name validated and read without following links) and the Callsheet
// ref must never secretly be a current branch. A malformed, unreadable,
// cyclic or overlong chain is refused rather than guessed.
func checkSymbolicChains(gitFD int, ref string) error {
	refuse := errLocalRef("HEAD or a branch is a symbolic ref to the Callsheet ref; it is never updated")
	unsafe := errLocalRef("a HEAD or branch symbolic ref cannot be read safely (malformed, cyclic or too long); nothing was written")
	var starts []string
	readStart := func(dfd int, name string, required bool) error {
		b, found, err := readRefFile(dfd, name)
		switch {
		case err != nil:
			return unsafe
		case !found:
			if required {
				return unsafe
			}
			return nil
		}
		v, ok := parseRefContent(b)
		if !ok {
			return unsafe
		}
		if v.symbolic != "" {
			starts = append(starts, v.symbolic)
		}
		return nil
	}
	if err := readStart(gitFD, "HEAD", true); err != nil {
		return err
	}
	if wfd, err := openDirAt(gitFD, "worktrees"); err == nil {
		names, rerr := readNames(wfd)
		for _, n := range names {
			fd, err := openDirAt(wfd, n)
			if err != nil {
				continue
			}
			err = readStart(fd, "HEAD", false)
			closeFD(fd)
			if err != nil {
				closeFD(wfd)
				return err
			}
		}
		closeFD(wfd)
		if rerr != nil {
			return unsafe
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return unsafe
	}
	if err := collectSymbolic(gitFD, "refs/heads", &starts, 0); err != nil {
		return unsafe
	}
	for _, t := range starts {
		for hop := 0; ; hop++ {
			if t == ref || asciiFold(t) == asciiFold(ref) {
				return refuse
			}
			if hop >= maxSymrefDepth {
				return unsafe
			}
			lr, err := looseRef(gitFD, t)
			if err != nil {
				return unsafe
			}
			if !lr.found || lr.value.symbolic == "" {
				break
			}
			t = lr.value.symbolic
		}
	}
	return nil
}

// collectSymbolic appends the targets of the symbolic loose refs below
// dir (no symlink followed; bounded depth). A malformed or unreadable
// entry is an error.
func collectSymbolic(gitFD int, dir string, out *[]string, depth int) error {
	if depth > 64 {
		return errUnsafeMetadata
	}
	fd, err := openRel(gitFD, dir)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer closeFD(fd)
	names, err := readNames(fd)
	if err != nil {
		return err
	}
	for _, n := range names {
		st, err := lstatAt(fd, n)
		if err != nil {
			return err
		}
		switch {
		case isDirStat(&st):
			if err := collectSymbolic(gitFD, dir+"/"+n, out, depth+1); err != nil {
				return err
			}
		case isRegStat(&st):
			if strings.HasSuffix(n, ".lock") {
				continue
			}
			b, _, err := readRefFile(fd, n)
			if err != nil {
				return err
			}
			v, ok := parseRefContent(b)
			if !ok {
				return errUnsafeMetadata
			}
			if v.symbolic != "" {
				*out = append(*out, v.symbolic)
			}
		default:
			return errUnsafeMetadata
		}
	}
	return nil
}

// installObjects writes every object of the verified closure that the
// repository lacks, durably, before any ref may point at them. Objects
// installed before a later failure may remain unreachable (normal git
// object-store behavior).
func (d *deps) installObjects(ctx context.Context, gitFD int, local storer.EncodedObjectStorer, src storer.EncodedObjectStorer, closure map[plumbing.Hash]plumbing.ObjectType) (int64, error) {
	objFD, err := openDirAt(gitFD, "objects")
	if err != nil {
		return 0, errStorage("cannot open the destination's object database", err)
	}
	defer closeFD(objFD)
	db := &looseDB{d: d, objFD: objFD, sync: true, opPref: "install-"}
	var n int64
	for h, t := range closure {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if local.HasEncodedObject(h) == nil {
			continue
		}
		o, err := src.EncodedObject(t, h)
		if err != nil {
			return n, errIntegrity("a verified object disappeared from the private store")
		}
		r, err := o.Reader()
		if err != nil {
			return n, errIntegrity("a verified object cannot be read")
		}
		got, werr := db.write(ctx, t, o.Size(), r)
		r.Close()
		if werr != nil {
			return n, orStorage(ctxOr(ctx, werr), "cannot install objects in the destination repository")
		}
		if got != h {
			return n, errIntegrity("an installed object does not hash to its ID")
		}
		n++
	}
	if err := db.syncDirs(); err != nil {
		return n, errStorage("cannot sync the destination's object database", err)
	}
	return n, nil
}

// refLocks are the held packed-refs.lock and <ref>.lock.
type refLocks struct {
	gitFD    int
	leafFD   int
	leaf     string
	packed   bool
	ref      bool
	created  []createdDir
	released bool
}

type createdDir struct {
	parentFD int
}

func (l *refLocks) release() {
	if l.released {
		return
	}
	l.released = true
	if l.ref {
		unix.Unlinkat(l.leafFD, l.leaf+".lock", 0)
	}
	if l.packed {
		unix.Unlinkat(l.gitFD, "packed-refs.lock", 0)
	}
	if l.leafFD >= 0 {
		closeFD(l.leafFD)
	}
	for _, c := range l.created {
		closeFD(c.parentFD)
	}
}

// updateLocalRef sets ref to new if it still has the observed old value:
// packed-refs.lock then <ref>.lock are taken with O_EXCL (an existing
// lock is a conflict, never waited for or broken), the value is re-read
// and compared, and the new loose ref is written, synced and renamed into
// place, then its directory and every newly created ancestor directory
// are synced. It reports whether the ref changed; a failure after the
// rename is ambiguous.
func (d *deps) updateLocalRef(ctx context.Context, gitFD int, ref string, old localRefState, new plumbing.Hash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	l := &refLocks{gitFD: gitFD, leafFD: -1}
	defer l.release()
	if err := d.check("ref-packed-lock"); err != nil {
		return false, errStorage("cannot lock the destination's refs", err)
	}
	pfd, err := unix.Openat(gitFD, "packed-refs.lock", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return false, errLocalRef("another git process holds packed-refs.lock; nothing was written")
		}
		return false, errStorage("cannot lock the destination's refs", err)
	}
	closeFD(pfd)
	l.packed = true
	parts := strings.Split(ref, "/")
	fd, err := unix.Dup(gitFD)
	if err != nil {
		return false, errStorage("cannot lock the destination's refs", err)
	}
	for _, c := range parts[:len(parts)-1] {
		st, err := lstatAt(fd, c)
		switch {
		case errors.Is(err, unix.ENOENT):
			if err := d.check("ref-mkdir"); err != nil {
				closeFD(fd)
				return false, errStorage("cannot create the Callsheet ref's directory", err)
			}
			if err := unix.Mkdirat(fd, c, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
				closeFD(fd)
				return false, errStorage("cannot create the Callsheet ref's directory", err)
			}
			pdup, derr := unix.Dup(fd)
			if derr == nil {
				l.created = append(l.created, createdDir{parentFD: pdup})
			}
		case err != nil:
			closeFD(fd)
			return false, errStorage("cannot read the destination's refs", err)
		case !isDirStat(&st):
			closeFD(fd)
			return false, errLocalRef("the Callsheet ref conflicts with an existing ref namespace; nothing was written")
		}
		next, err := openDirAt(fd, c)
		closeFD(fd)
		if err != nil {
			return false, errLocalRef("the Callsheet ref's path holds a symlink or an unexpected file; nothing was written")
		}
		fd = next
	}
	l.leafFD, l.leaf = fd, parts[len(parts)-1]
	if err := d.check("ref-lock"); err != nil {
		return false, errStorage("cannot lock the Callsheet ref", err)
	}
	lfd, err := unix.Openat(fd, l.leaf+".lock", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return false, errLocalRef("another git process holds the Callsheet ref's lock; nothing was written")
		}
		return false, errStorage("cannot lock the Callsheet ref", err)
	}
	l.ref = true
	d.stage("ref-locked")
	cur, err := readLocalRef(gitFD, ref)
	if err != nil {
		closeFD(lfd)
		return false, err
	}
	if cur != old {
		closeFD(lfd)
		return false, errLocalRef("the Callsheet ref changed since it was observed (local compare-and-swap failed); nothing was written")
	}
	if cur.found && cur.hash == new {
		closeFD(lfd)
		return false, nil
	}
	werr := func() error {
		if err := d.check("ref-write"); err != nil {
			return err
		}
		if _, err := unix.Write(lfd, []byte(new.String()+"\n")); err != nil {
			return err
		}
		return d.sync(lfd, "ref-sync")
	}()
	cerr := unix.Close(lfd)
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		return false, orStorage(ctxOr(ctx, werr), "cannot write the Callsheet ref")
	}
	// The last point before publication: a cancellation publishes
	// nothing (the held lock, ours, is removed by release).
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := d.check("ref-rename"); err != nil {
		return false, errStorage("cannot publish the Callsheet ref", err)
	}
	if err := unix.Renameat(fd, l.leaf+".lock", fd, l.leaf); err != nil {
		return false, errStorage("cannot publish the Callsheet ref", err)
	}
	l.ref = false
	d.stage("ref-renamed")
	// After the rename the new value may be visible: failures are
	// ambiguous.
	if err := d.sync(fd, "ref-dirsync"); err != nil {
		return true, ambiguousPull(err)
	}
	for i := len(l.created) - 1; i >= 0; i-- {
		if err := d.sync(l.created[i].parentFD, "ref-dirsync"); err != nil {
			return true, ambiguousPull(err)
		}
	}
	return true, nil
}

// ambiguousPull is a local failure after a publication point.
func ambiguousPull(cause error) error {
	return &contract.Error{Code: contract.CodeInternal, Message: "the local publication could not be made durable; " + contract.PullAmbiguity,
		Details: map[string]any{"reason": contract.ReasonStorageFailure}, Cause: cause}
}
