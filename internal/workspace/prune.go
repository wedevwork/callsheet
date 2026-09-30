package workspace

import (
	"context"
	"path/filepath"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"

	"github.com/wedevwork/callsheet/internal/contract"
)

// pruneStats counts one prune.
type pruneStats struct {
	objects        int
	reclaimedBytes int64
}

// Prune deletes every task ref whose plane publication time is strictly
// before the cutoff (equality remains; commit dates and mtimes are never
// age), then, in the same generation transaction, rewrites a fresh bare
// object store holding exactly the closure of every surviving ref (all
// branches regardless of age, and younger task refs) with the surviving
// loose refs, HEAD and metadata. Unreachable objects (rewound or deleted
// branches, abandoned received objects, expired task history) are
// collected; nothing reachable is. Zero candidates still collect.
func (m *Manager) Prune(ctx context.Context, name, instance string, before time.Time) (contract.WorkspacePruneResponse, error) {
	resp, _, err := m.prune(ctx, name, instance, before)
	return resp, err
}

func (m *Manager) prune(ctx context.Context, name, instance string, before time.Time) (contract.WorkspacePruneResponse, pruneStats, error) {
	var resp contract.WorkspacePruneResponse
	var st pruneStats
	if !contract.ValidWorkspaceName(name) {
		return resp, st, contract.InvalidWorkspaceName()
	}
	if err := contract.ValidateInstance(instance); err != nil {
		return resp, st, err
	}
	ctx, done := m.opCtx(ctx)
	defer done()
	h, release, err := m.write(ctx, name, instance)
	if err != nil {
		return resp, st, opError("prune", err)
	}
	defer release()
	d := m.d
	d.stage("prune-locked", ctx)
	beforeSize, err := treeSize(h.dir)
	if err != nil {
		return resp, st, opError("prune", err)
	}
	refs, err := readRefs(h.repo())
	if err != nil {
		return resp, st, opError("prune", err)
	}
	tasks, err := readRefsDoc(filepath.Join(h.genDir(h.gen), refsName))
	if err != nil {
		return resp, st, opError("prune", err)
	}
	survivors := map[string]plumbing.Hash{}
	keep := map[string]taskMeta{}
	var removed int64
	for n, c := range refs {
		if meta, ok := tasks[n]; ok {
			at, ok := contract.ParseTime(meta.PublishedAt)
			if !ok || meta.Commit != c.String() {
				return resp, st, opError("prune", corrupt(filepath.Join(h.genDir(h.gen), refsName), "task metadata does not match its ref"))
			}
			if at.Before(before) {
				removed++
				continue
			}
			keep[n] = meta
		} else if contract.ValidTaskRef(n) {
			return resp, st, opError("prune", corrupt(filepath.Join(h.genDir(h.gen), refsName), "a task ref has no publication metadata"))
		}
		survivors[n] = c
	}
	src, sfs := openStorage(h.repo(), d)
	closure, err := walkClosure(ctx, src, tips(survivors), nil)
	if err != nil {
		closeStorage(src, sfs)
		return resp, st, opError("prune", err)
	}
	st.objects = len(closure)
	t, err := m.begin(ctx, h, false)
	if err != nil {
		closeStorage(src, sfs)
		return resp, st, opError("prune", err)
	}
	err = t.build(ctx, src, closure, survivors, keep)
	closeStorage(src, sfs)
	if err == nil {
		_, err = d.validateGeneration(ctx, t.dir, nil)
	}
	if err != nil {
		t.discard()
		return resp, st, opError("prune", err)
	}
	if err := t.commit(ctx); err != nil {
		return resp, st, opError("prune", err)
	}
	after, err := treeSize(h.dir)
	if err != nil {
		return resp, st, opError("prune", txnError{committed: true, cause: err})
	}
	st.reclaimedBytes = max(0, beforeSize-after)
	d.emit("pruned-objects", int64(st.objects))
	d.emit("reclaimed-bytes", st.reclaimedBytes)
	return contract.WorkspacePruneResponse{Name: h.name, Instance: h.instance, Generation: h.gen, RemovedTaskRefs: removed,
		ReclaimedBytes: st.reclaimedBytes, SizeBytes: after}, st, nil
}

// build fills the empty generation: a bare repository, one pack of exactly
// the closure (encoded by go-git from the old store and indexed by the new
// one), the surviving loose refs and their metadata.
func (t *txn) build(ctx context.Context, src *storageT, closure []plumbing.Hash, refs map[string]plumbing.Hash, tasks map[string]taskMeta) error {
	d := t.d
	if err := d.initRepo(t.repo()); err != nil {
		return err
	}
	if len(closure) > 0 {
		dst, dfs := openStorage(t.repo(), d)
		w, err := dst.PackfileWriter()
		if err != nil {
			closeStorage(dst, dfs)
			return err
		}
		_, eerr := packfile.NewEncoder(&ctxWriter{ctx: ctx, w: w}, src, false).Encode(closure, 10)
		cerr := w.Close()
		serr := closeStorage(dst, dfs)
		if eerr != nil {
			return eerr
		}
		if cerr != nil {
			return cerr
		}
		if serr != nil {
			return serr
		}
	}
	for n, c := range refs {
		if err := d.writeRef(t.repo(), n, c); err != nil {
			return err
		}
	}
	return d.writeFile(filepath.Join(t.dir, refsName), encodeRefsDoc(tasks))
}

// ctxWriter fails writes once ctx ends.
type ctxWriter struct {
	ctx context.Context
	w   interface{ Write([]byte) (int, error) }
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}
