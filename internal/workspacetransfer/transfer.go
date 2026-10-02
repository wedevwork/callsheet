// Package workspacetransfer implements the local workspace transfers of
// iteration 09b: ws push (a clean git repository's committed history, or
// a plain folder's snapshot, to a workspace branch with fast-forward-only
// compare-and-swap) and ws pull (a selected workspace commit into an
// existing git repository as objects plus one Callsheet ref, or into a
// new or empty plain folder). The CLI and the MCP server call the same
// Push and Pull with an injected verified plane, an explicit OS value and
// an injected environment. go-git v5.16.3 is the only git engine: no git
// executable, hook, filter, credential helper or remote is ever used.
package workspacetransfer

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Plane is the verified plane access of one transfer call.
type Plane interface {
	ShowWorkspace(ctx context.Context, name string) (contract.WorkspaceView, error)
	WorkspaceStatus(ctx context.Context, name string, p client.StatusPage) (contract.WorkspaceStatusResponse, error)
	WorkspaceDiff(ctx context.Context, name string, p client.DiffPage) (contract.WorkspaceDiffResponse, error)
	WorkspaceGit(name string) (Git, error)
}

// Git is one workspace's smart-HTTP session.
type Git interface {
	ReceiveRefs(ctx context.Context, instance string) (map[string]plumbing.Hash, error)
	Push(ctx context.Context, instance, ref string, old, new plumbing.Hash, pack io.Reader) error
	Fetch(ctx context.Context, wants, haves []plumbing.Hash, sink func(io.Reader) error) error
	Close()
}

// clientPlane adapts the verified client.
type clientPlane struct{ *client.Client }

func (c clientPlane) WorkspaceGit(name string) (Git, error) {
	g, err := c.Client.WorkspaceGit(name)
	if err != nil {
		return nil, err
	}
	return g, nil
}

// ClientPlane is the production Plane over a verified client.
func ClientPlane(c *client.Client) Plane { return clientPlane{c} }

// Options are one transfer's explicit inputs.
type Options struct {
	// GOOS is the host OS value from the CLI seam (linux or darwin).
	GOOS string
	// Env provides the environment and system git files.
	Env Env
	// Cwd resolves relative and omitted paths (the process working
	// directory captured at admission).
	Cwd   string
	Plane Plane
}

// PushRequest is ws push's input; an unset Path is the working directory.
type PushRequest struct {
	Name, Instance, Branch string
	Path                   string
	PathSet                bool
}

// PullRequest is ws pull's input; an unset Path is the working directory.
type PullRequest struct {
	Name, Ref string
	Path      string
	PathSet   bool
}

// ResolvePath returns the absolute cleaned path of a PATH argument (the
// working directory when unset): valid UTF-8, no NUL, within goos's
// native pathname limit, never expanded (no "~" or environment).
func ResolvePath(goos, cwd, p string, set bool) (string, error) {
	if !set {
		p = cwd
	}
	if err := contract.ValidateLocalPath(goos, p); err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		if !filepath.IsAbs(cwd) {
			return "", contract.New(contract.CodeInvalidArgument, "the working directory is unknown; give an absolute path")
		}
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	if len(p) > contract.MaxPathFor(goos) {
		return "", contract.New(contract.CodeInvalidArgument, "the resolved path exceeds the native pathname limit")
	}
	return p, nil
}

func checkGOOS(goos string) error {
	if contract.MaxPathFor(goos) == 0 {
		return contract.New(contract.CodeInvalidArgument, "unsupported operating system "+contract.SafeText(goos, 16)+"; supported: linux, darwin")
	}
	return nil
}

// Push pushes a clean repository's HEAD or a folder snapshot.
func Push(ctx context.Context, o Options, req PushRequest) (contract.WorkspacePushResult, error) {
	return defaultDeps().push(ctx, o, req)
}

// Pull pulls a selected commit into a repository or a folder.
func Pull(ctx context.Context, o Options, req PullRequest) (contract.WorkspacePullResult, error) {
	return defaultDeps().pull(ctx, o, req)
}

// pushSource is a scanned source ready to be packed.
type pushSource struct {
	kind   string
	repo   *repo
	rootFD int
	man    *manifest
	head   plumbing.Hash // git source
	tree   plumbing.Hash // folder source
}

func (s *pushSource) close() {
	if s.repo != nil {
		s.repo.close()
	} else if s.rootFD >= 0 {
		closeFD(s.rootFD)
	}
}

func (d *deps) push(ctx context.Context, o Options, req PushRequest) (res contract.WorkspacePushResult, err error) {
	if err := checkGOOS(o.GOOS); err != nil {
		return res, err
	}
	in, err := contract.ValidatePush(req.Name, req.Instance, req.Branch)
	if err != nil {
		return res, err
	}
	abs, err := ResolvePath(o.GOOS, o.Cwd, req.Path, req.PathSet)
	if err != nil {
		return res, err
	}
	canon, err := canonicalDir(abs)
	if err != nil {
		return res, err
	}
	rootFD, err := openPathDir(canon)
	if err != nil {
		return res, errStorage("cannot open the source", err)
	}
	src := &pushSource{rootFD: rootFD}
	defer src.close()
	kind, err := classify(rootFD)
	if err != nil {
		return res, err
	}
	temp, err := d.newTempStore()
	if err != nil {
		return res, err
	}
	defer temp.close()
	if kind == kindRepo {
		closeFD(rootFD)
		src.rootFD = -1
		if src.repo, err = openRepo(ctx, o.Env, canon); err != nil {
			return res, err
		}
		src.rootFD = src.repo.rootFD
		src.kind, src.head = contract.KindGit, src.repo.head.hash
		if src.man, err = checkClean(ctx, d, src.repo, o.Env, o.GOOS); err != nil {
			return res, ctxOr(ctx, err)
		}
	} else {
		if err := checkAncestors(canon); err != nil {
			return res, err
		}
		src.kind = contract.KindFolder
		db := &looseDB{d: d, objFD: temp.objFD, opPref: "snapshot-"}
		if src.tree, src.man, err = d.snapshotFolder(ctx, rootFD, db); err != nil {
			return res, ctxOr(ctx, err)
		}
	}
	d.stage("push-scanned")
	g, err := o.Plane.WorkspaceGit(in.Name)
	if err != nil {
		return res, err
	}
	defer g.Close()
	refs, err := g.ReceiveRefs(ctx, in.Instance)
	if err != nil {
		return res, err
	}
	old := refs[in.Branch]
	d.stage("push-observed")
	var store storer.EncodedObjectStorer
	var new plumbing.Hash
	var objects []plumbing.Hash
	if src.kind == contract.KindGit {
		store, new = src.repo.store, src.head
		if objects, err = d.gitPushObjects(ctx, src.repo, refs, old, new); err != nil {
			return res, err
		}
	} else {
		store = temp.store
		if new, objects, err = d.folderPushObjects(ctx, g, temp, src.tree, old); err != nil {
			return res, err
		}
	}
	packPath := filepath.Join(temp.dir, "push.pack")
	pack, err := os.OpenFile(packPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return res, errStorage("cannot stage the pack", err)
	}
	defer pack.Close()
	if err := d.check("push-pack"); err != nil {
		return res, errStorage("cannot stage the pack", err)
	}
	if err := writePack(ctx, store, objects, pack); err != nil {
		return res, orStorage(ctxOr(ctx, err), "cannot stage the pack")
	}
	if _, err := pack.Seek(0, io.SeekStart); err != nil {
		return res, errStorage("cannot stage the pack", err)
	}
	d.stage("push-packed")
	if err := src.man.verify(ctx, src.rootFD); err != nil {
		return res, ctxOr(ctx, errSourceChanged())
	}
	d.stage("push-publish")
	// The last point before the request is sent: a cancellation sends
	// nothing.
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if err := g.Push(ctx, in.Instance, in.Branch, old, new, pack); err != nil {
		return res, err
	}
	res = contract.WorkspacePushResult{Name: in.Name, Instance: in.Instance, Branch: in.Branch, Commit: new.String(),
		SourceKind: src.kind, Changed: old != new}
	if !old.IsZero() {
		o := old.String()
		res.OldCommit = &o
	}
	return res, nil
}

// gitPushObjects validates HEAD's complete closure and fast-forward
// ancestry and selects the objects the plane lacks.
func (d *deps) gitPushObjects(ctx context.Context, r *repo, refs map[string]plumbing.Hash, old, new plumbing.Hash) ([]plumbing.Hash, error) {
	st := &closureStats{}
	if _, err := walkClosure(ctx, r.store, []plumbing.Hash{new}, st, true); err != nil {
		return nil, closureFailure(ctx, err)
	}
	d.emit("push-closure-objects", st.objects)
	if !old.IsZero() {
		ok := false
		if r.store.HasEncodedObject(old) == nil {
			var err error
			if ok, err = isAncestor(ctx, r.store, old, new); err != nil {
				return nil, closureFailure(ctx, err)
			}
		}
		if !ok {
			return nil, errNonFastForward()
		}
	}
	if old == new {
		return nil, nil
	}
	var haves []plumbing.Hash
	for _, h := range refs {
		if r.store.HasEncodedObject(h) == nil {
			haves = append(haves, h)
		}
	}
	objs, err := revlist.Objects(r.store, []plumbing.Hash{new}, haves)
	if err != nil {
		return nil, closureFailure(ctx, err)
	}
	return objs, nil
}

// closureFailure maps a closure walk failure: refusals and cancellation
// unchanged, everything else a repository integrity error.
func closureFailure(ctx context.Context, err error) error {
	if isContract(err) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errUnsupported("an incomplete or corrupt object database")
}

// folderPushObjects fetches the target's complete history (when it
// exists) into the private store, creates the deterministic snapshot
// commit parented on it (none when the tree is unchanged) and selects the
// new objects.
func (d *deps) folderPushObjects(ctx context.Context, g Git, temp *tempStore, tree, old plumbing.Hash) (plumbing.Hash, []plumbing.Hash, error) {
	if !old.IsZero() {
		if err := d.fetchInto(ctx, g, temp, []plumbing.Hash{old}, nil); err != nil {
			return plumbing.ZeroHash, nil, err
		}
		st := &closureStats{}
		if _, err := walkClosure(ctx, temp.store, []plumbing.Hash{old}, st, false); err != nil {
			return plumbing.ZeroHash, nil, fetchedFailure(ctx, err)
		}
		d.emit("push-parent-objects", st.objects)
		d.emit("push-parent-bytes", st.bytes)
		c, err := object.GetCommit(temp.store, old)
		if err != nil {
			return plumbing.ZeroHash, nil, fetchedFailure(ctx, err)
		}
		if c.TreeHash == tree {
			return old, nil, nil
		}
	}
	db := &looseDB{d: d, objFD: temp.objFD, opPref: "snapshot-"}
	new, err := db.writeObject(ctx, snapshotCommit(tree, old))
	if err != nil {
		return plumbing.ZeroHash, nil, orStorage(ctxOr(ctx, err), "cannot store the snapshot")
	}
	var haves []plumbing.Hash
	if !old.IsZero() {
		haves = []plumbing.Hash{old}
	}
	objs, err := revlist.Objects(temp.store, []plumbing.Hash{new}, haves)
	if err != nil {
		return plumbing.ZeroHash, nil, orStorage(ctxOr(ctx, err), "cannot select the snapshot's objects")
	}
	return new, objs, nil
}

// fetchedFailure maps a failed validation of fetched objects.
func fetchedFailure(ctx context.Context, err error) error {
	if isContract(err) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errIntegrity("the fetched objects are incomplete or corrupt; nothing was published")
}

// fetchInto fetches wants (with haves) from the plane into the private
// store.
func (d *deps) fetchInto(ctx context.Context, g Git, temp *tempStore, wants, haves []plumbing.Hash) error {
	d.stage("fetch")
	err := g.Fetch(ctx, wants, haves, func(r io.Reader) error {
		if err := d.check("fetch-write"); err != nil {
			return err
		}
		return packfile.UpdateObjectStorage(temp.store, r)
	})
	if err == nil || isContract(err) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errStorage("cannot store the fetched objects", err)
}

// destination is a classified pull destination.
type destination struct {
	repo   *repo
	export *exportDest
	root   string
}

func (dst *destination) close() {
	if dst.repo != nil {
		dst.repo.close()
	}
	if dst.export != nil {
		dst.export.close()
	}
}

// classifyDestination classifies a pull destination before any network
// work: an existing repository root, or a new or empty folder outside any
// repository.
func classifyDestination(ctx context.Context, o Options, abs string) (*destination, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err == nil {
		full := filepath.Join(parent, filepath.Base(abs))
		if st, lerr := os.Lstat(full); lerr == nil && st.IsDir() {
			fd, err := openPathDir(full)
			if err != nil {
				return nil, errStorage("cannot open the destination", err)
			}
			kind, err := classify(fd)
			closeFD(fd)
			if err != nil {
				return nil, err
			}
			if kind == kindRepo {
				r, err := openRepo(ctx, o.Env, full)
				if err != nil {
					return nil, err
				}
				return &destination{repo: r, root: full}, nil
			}
		}
	}
	e, err := prepareExport(abs)
	if err != nil {
		return nil, err
	}
	return &destination{export: e}, nil
}

// resolveSelector resolves the pull's selector to one immutable commit:
// branch and task refs through the status pages pinned by instance and
// generation, a hash through the reachability-validating diff (base
// empty, limit 1). A generation change aborts; nothing restarts.
func resolveSelector(ctx context.Context, p Plane, name, instance string, sel contract.Selector) (plumbing.Hash, error) {
	notFound := contract.New(contract.CodeNotFound, "the workspace has no such branch or task ref (an unborn branch has no commit)")
	recreated := contract.New(contract.CodeConflict, "the workspace was removed or recreated during the pull; nothing was written")
	if sel.Kind == contract.SelectorKindHash {
		r, err := p.WorkspaceDiff(ctx, name, client.DiffPage{Base: contract.SelectorEmpty, Target: sel.Value, Limit: 1})
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if r.Instance != instance {
			return plumbing.ZeroHash, recreated
		}
		return plumbing.NewHash(r.TargetCommit), nil
	}
	page := client.StatusPage{Limit: contract.MaxWorkspaceLimit}
	for {
		r, err := p.WorkspaceStatus(ctx, name, page)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if r.Instance != instance {
			return plumbing.ZeroHash, recreated
		}
		for _, ref := range r.Refs {
			if ref.Name == sel.Value {
				return plumbing.NewHash(ref.Commit), nil
			}
			if ref.Name > sel.Value {
				return plumbing.ZeroHash, notFound
			}
		}
		if r.NextAfter == nil {
			return plumbing.ZeroHash, notFound
		}
		page = client.StatusPage{After: *r.NextAfter, Instance: r.Instance, Generation: r.Generation, Limit: contract.MaxWorkspaceLimit}
	}
}

func (d *deps) pull(ctx context.Context, o Options, req PullRequest) (res contract.WorkspacePullResult, err error) {
	if err := checkGOOS(o.GOOS); err != nil {
		return res, err
	}
	in, err := contract.ValidatePull(req.Name, req.Ref)
	if err != nil {
		return res, err
	}
	abs, err := ResolvePath(o.GOOS, o.Cwd, req.Path, req.PathSet)
	if err != nil {
		return res, err
	}
	dst, err := classifyDestination(ctx, o, abs)
	if err != nil {
		return res, err
	}
	defer dst.close()
	var refName string
	var old localRefState
	if dst.repo != nil {
		refName = contract.LocalRefFor(in.Name, in.Canonical)
		if err := checkRefPath(o.GOOS, dst.root, refName); err != nil {
			return res, err
		}
		if old, err = readLocalRef(dst.repo.gitFD, refName); err != nil {
			return res, err
		}
	}
	d.stage("pull-classified")
	view, err := o.Plane.ShowWorkspace(ctx, in.Name)
	if err != nil {
		return res, err
	}
	commit, err := resolveSelector(ctx, o.Plane, in.Name, view.Instance, in.Selector)
	if err != nil {
		return res, err
	}
	if dst.repo != nil && in.Selector.Kind == contract.SelectorKindHash && old.found && old.hash != commit {
		return res, errLocalRef("the Callsheet ref of this hash holds another commit; it is never repaired")
	}
	d.stage("pull-resolved")
	g, err := o.Plane.WorkspaceGit(in.Name)
	if err != nil {
		return res, err
	}
	defer g.Close()
	temp, err := d.newTempStore()
	if err != nil {
		return res, err
	}
	defer temp.close()
	var store storer.EncodedObjectStorer = temp.store
	if dst.repo != nil {
		// Objects already present are read (and so verified) from the
		// repository itself: an existing corrupt object is an integrity
		// failure, never replaced or trusted.
		local := dst.repo.store
		store = multiStore{EncodedObjectStorer: local, others: []storer.EncodedObjectStorer{temp.store}}
		have := false
		if local.HasEncodedObject(commit) == nil {
			if _, err := walkClosure(ctx, local, []plumbing.Hash{commit}, nil, true); err == nil {
				have = true
			} else if ctx.Err() != nil {
				return res, ctx.Err()
			}
		}
		if !have {
			var haves []plumbing.Hash
			for _, h := range []plumbing.Hash{old.hash, dst.repo.head.hash} {
				if !h.IsZero() && local.HasEncodedObject(h) == nil {
					haves = append(haves, h)
				}
			}
			if err := d.fetchInto(ctx, g, temp, []plumbing.Hash{commit}, haves); err != nil {
				return res, err
			}
		}
	} else if err := d.fetchInto(ctx, g, temp, []plumbing.Hash{commit}, nil); err != nil {
		return res, err
	}
	d.stage("pull-fetched")
	again, err := o.Plane.ShowWorkspace(ctx, in.Name)
	if err != nil {
		return res, err
	}
	if again.Instance != view.Instance {
		return res, contract.New(contract.CodeConflict, "the workspace was removed or recreated during the pull; nothing was written")
	}
	st := &closureStats{}
	closure, err := walkClosure(ctx, store, []plumbing.Hash{commit}, st, dst.repo != nil)
	if err != nil {
		return res, fetchedFailure(ctx, err)
	}
	d.emit("pull-objects-verified", st.objects)
	res = contract.WorkspacePullResult{Name: in.Name, Instance: view.Instance, Selector: in.Canonical, Commit: commit.String()}
	if dst.repo != nil {
		res.DestinationKind = contract.KindGit
		n, err := d.installObjects(ctx, dst.repo.gitFD, dst.repo.store, store, closure)
		if err != nil {
			return contract.WorkspacePullResult{}, err
		}
		d.emit("pull-objects-installed", n)
		d.stage("pull-installed")
		changed, err := d.updateLocalRef(ctx, dst.repo.gitFD, refName, old, commit)
		if err != nil {
			return contract.WorkspacePullResult{}, err
		}
		res.LocalRef, res.Changed = &refName, changed
		if old.found {
			o := old.hash.String()
			res.OldCommit = &o
		}
		return res, nil
	}
	res.DestinationKind = contract.KindFolder
	entries, err := collectTree(ctx, o.GOOS, temp.store, commit, dst.export)
	if err != nil {
		return contract.WorkspacePullResult{}, err
	}
	d.emit("export-files", int64(len(entries)))
	if err := d.export(ctx, o.GOOS, temp.store, entries, dst.export); err != nil {
		return contract.WorkspacePullResult{}, err
	}
	res.Changed = true
	return res, nil
}
