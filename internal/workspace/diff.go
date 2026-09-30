package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/wedevwork/callsheet/internal/contract"
)

// DiffQuery is one diff page request. A continuation carries After (the
// last raw path), Instance and Generation together, and full hashes (or
// the empty base) as selectors (validated by the caller).
type DiffQuery struct {
	Base, Target         contract.Selector
	After                []byte
	Instance, Generation string
	Limit                int
}

// diffStats counts one diff walk.
type diffStats struct {
	trees, entries int
}

// Diff compares two stored snapshots by raw path bytes and returns one
// page of changed-path metadata: added, deleted or modified (object ID or
// mode differs; a type change is modified), with six-digit modes and blob
// byte lengths. Directories have no rows, renames are a delete and an
// add, and no blob content, hash of content, patch, line count or commit
// message is read into the result. Selectors are resolved once, under the
// read lock; a continuation whose generation changed conflicts before any
// selector is resolved.
func (m *Manager) Diff(ctx context.Context, name string, q DiffQuery) (contract.WorkspaceDiffResponse, error) {
	resp, _, err := m.diff(ctx, name, q)
	return resp, err
}

func (m *Manager) diff(ctx context.Context, name string, q DiffQuery) (contract.WorkspaceDiffResponse, diffStats, error) {
	var resp contract.WorkspaceDiffResponse
	var st diffStats
	if !contract.ValidWorkspaceName(name) {
		return resp, st, contract.InvalidWorkspaceName()
	}
	ctx, done := m.opCtx(ctx)
	defer done()
	h, release, err := m.read(ctx, name, "")
	if err != nil {
		return resp, st, opError("diff", err)
	}
	defer release()
	if q.Instance != "" && (q.Instance != h.instance || q.Generation != h.gen) {
		return resp, st, restartPagination()
	}
	refs, err := readRefs(h.repo())
	if err != nil {
		return resp, st, opError("diff", err)
	}
	s, sfs := openStorage(h.repo(), m.d)
	defer closeStorage(s, sfs)
	var baseTree plumbing.Hash
	var baseCommit *string
	if q.Base.Kind != contract.SelectorKindEmpty {
		c, err := resolve(ctx, s, refs, q.Base)
		if err != nil {
			return resp, st, opError("diff", err)
		}
		if baseTree, err = commitTree(s, c); err != nil {
			return resp, st, opError("diff", err)
		}
		baseCommit = hashPtr(c, true)
	}
	target, err := resolve(ctx, s, refs, q.Target)
	if err != nil {
		return resp, st, opError("diff", err)
	}
	targetTree, err := commitTree(s, target)
	if err != nil {
		return resp, st, opError("diff", err)
	}
	var rows []contract.WorkspaceChange
	w := &treeDiff{ctx: ctx, s: s, after: q.After, max: q.Limit + 1, st: &st}
	w.emit = func(r contract.WorkspaceChange) { rows = append(rows, r) }
	if err := w.walk(nil, baseTree, targetTree); err != nil && err != errDiffFull {
		return resp, st, opError("diff", err)
	}
	resp = contract.WorkspaceDiffResponse{Name: h.name, Instance: h.instance, Generation: h.gen, BaseCommit: baseCommit,
		TargetCommit: target.String(), Changes: []contract.WorkspaceChange{}}
	if err := page(&resp, &resp.Changes, &resp.NextAfter, rows, func(r contract.WorkspaceChange) string { return r.PathBase64 }, q.Limit); err != nil {
		return resp, st, opError("diff", err)
	}
	m.d.emit("diff-trees", int64(st.trees))
	m.d.emit("diff-entries", int64(st.entries))
	return resp, st, nil
}

func commitTree(s storer.EncodedObjectStorer, c plumbing.Hash) (plumbing.Hash, error) {
	cm, err := object.GetCommit(s, c)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return cm.TreeHash, nil
}

var errDiffFull = fmt.Errorf("diff page full")

// treeDiff merges two trees in git order, which for flattened paths is
// raw path byte order, skipping equal subtrees and subtrees wholly at or
// before the cursor, and stops after max rows.
type treeDiff struct {
	ctx   context.Context
	s     storer.EncodedObjectStorer
	after []byte
	max   int
	n     int
	emit  func(contract.WorkspaceChange)
	st    *diffStats
}

type tentry struct {
	name []byte
	mode filemode.FileMode
	hash plumbing.Hash
}

func (e tentry) key() []byte {
	if e.mode == filemode.Dir {
		return append(append([]byte(nil), e.name...), '/')
	}
	return e.name
}

func (w *treeDiff) entries(h plumbing.Hash) ([]tentry, error) {
	if h.IsZero() {
		return nil, nil
	}
	t, err := object.GetTree(w.s, h)
	if err != nil {
		return nil, err
	}
	w.st.trees++
	out := make([]tentry, len(t.Entries))
	for i, e := range t.Entries {
		out[i] = tentry{name: []byte(e.Name), mode: e.Mode, hash: e.Hash}
	}
	sort.SliceStable(out, func(i, j int) bool { return bytes.Compare(out[i].key(), out[j].key()) < 0 })
	w.st.entries += len(out)
	return out, nil
}

// before reports whether every path starting with prefix sorts at or
// before the cursor.
func (w *treeDiff) before(prefix []byte) bool {
	return w.after != nil && len(prefix) > 0 && bytes.Compare(prefix, w.after) < 0 && !bytes.HasPrefix(w.after, prefix)
}

func (w *treeDiff) walk(prefix []byte, a, b plumbing.Hash) error {
	if a == b || w.before(prefix) {
		return nil
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	ea, err := w.entries(a)
	if err != nil {
		return err
	}
	eb, err := w.entries(b)
	if err != nil {
		return err
	}
	i, j := 0, 0
	for i < len(ea) || j < len(eb) {
		var c int
		switch {
		case i == len(ea):
			c = 1
		case j == len(eb):
			c = -1
		default:
			c = bytes.Compare(ea[i].key(), eb[j].key())
		}
		switch {
		case c < 0:
			err = w.side(prefix, ea[i], true)
			i++
		case c > 0:
			err = w.side(prefix, eb[j], false)
			j++
		default:
			x, y := ea[i], eb[j]
			i, j = i+1, j+1
			if x.mode == filemode.Dir {
				err = w.walk(join(prefix, x.name, true), x.hash, y.hash)
			} else if x.hash != y.hash || x.mode != y.mode {
				err = w.row(join(prefix, x.name, false), &x, &y)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func join(prefix, name []byte, dir bool) []byte {
	p := append(append([]byte(nil), prefix...), name...)
	if dir {
		p = append(p, '/')
	}
	return p
}

// side emits a path present on one side only (old when deleted).
func (w *treeDiff) side(prefix []byte, e tentry, deleted bool) error {
	if e.mode == filemode.Dir {
		if deleted {
			return w.walk(join(prefix, e.name, true), e.hash, plumbing.ZeroHash)
		}
		return w.walk(join(prefix, e.name, true), plumbing.ZeroHash, e.hash)
	}
	if deleted {
		return w.row(join(prefix, e.name, false), &e, nil)
	}
	return w.row(join(prefix, e.name, false), nil, &e)
}

func modeString(m filemode.FileMode) *string {
	s := fmt.Sprintf("%06o", uint32(m))
	return &s
}

// blobSize reads a blob's stored byte length (never its content).
func (w *treeDiff) blobSize(h plumbing.Hash) (*int64, error) {
	o, err := w.s.EncodedObject(plumbing.BlobObject, h)
	if err != nil {
		return nil, err
	}
	n := o.Size()
	return &n, nil
}

func (w *treeDiff) row(path []byte, old, new *tentry) error {
	if w.after != nil && bytes.Compare(path, w.after) <= 0 {
		return nil
	}
	r := contract.WorkspaceChange{PathBase64: base64.StdEncoding.EncodeToString(path)}
	var err error
	switch {
	case old == nil:
		r.Kind = contract.ChangeAdded
	case new == nil:
		r.Kind = contract.ChangeDeleted
	default:
		r.Kind = contract.ChangeModified
	}
	if old != nil {
		r.OldMode = modeString(old.mode)
		if r.OldBytes, err = w.blobSize(old.hash); err != nil {
			return err
		}
	}
	if new != nil {
		r.NewMode = modeString(new.mode)
		if r.NewBytes, err = w.blobSize(new.hash); err != nil {
			return err
		}
	}
	w.emit(r)
	w.n++
	if w.n >= w.max {
		return errDiffFull
	}
	return nil
}
