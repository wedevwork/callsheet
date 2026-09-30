package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/wedevwork/callsheet/internal/contract"
)

// reachableCommits returns every commit reachable from the tips by
// parent links, checking ctx per commit. It stops early when stop
// reports true for a visited commit.
func reachableCommits(ctx context.Context, s storer.EncodedObjectStorer, roots []plumbing.Hash, stop func(plumbing.Hash) bool) (map[plumbing.Hash]bool, error) {
	seen := map[plumbing.Hash]bool{}
	stack := append([]plumbing.Hash(nil), roots...)
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[h] {
			continue
		}
		seen[h] = true
		if stop != nil && stop(h) {
			return seen, nil
		}
		c, err := object.GetCommit(s, h)
		if err != nil {
			return nil, err
		}
		stack = append(stack, c.ParentHashes...)
	}
	return seen, nil
}

func noSuchRef() error {
	return contract.New(contract.CodeNotFound, "no such ref in this workspace")
}

func noReachableCommit() error {
	return contract.New(contract.CodeNotFound, "no commit with this hash is reachable from a current ref of this workspace")
}

// resolve returns the commit a selector names under the caller's lock: a
// hash must be a commit reachable from a current ref (orphaned objects
// are never retrievable or promotable); a branch or task ref must exist.
func resolve(ctx context.Context, s storer.EncodedObjectStorer, refs map[string]plumbing.Hash, sel contract.Selector) (plumbing.Hash, error) {
	switch sel.Kind {
	case contract.SelectorKindHash:
		want := plumbing.NewHash(sel.Value)
		seen, err := reachableCommits(ctx, s, tips(refs), func(h plumbing.Hash) bool { return h == want })
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if !seen[want] {
			return plumbing.ZeroHash, noReachableCommit()
		}
		return want, nil
	case contract.SelectorKindBranch, contract.SelectorKindTask:
		h, ok := refs[sel.Value]
		if !ok {
			return plumbing.ZeroHash, noSuchRef()
		}
		return h, nil
	}
	return plumbing.ZeroHash, contract.New(contract.CodeInvalidArgument, "unsupported selector")
}

func hashPtr(h plumbing.Hash, ok bool) *string {
	if !ok {
		return nil
	}
	s := h.String()
	return &s
}

// SetRef explicitly creates, moves or deletes one branch with
// compare-and-swap in one generation transaction: expected must equal the
// branch's current hash (or it must be absent); commit parents are never
// changed, no merge or fast-forward is required, and no other ref or
// metadata changes. Deleting main leaves HEAD unborn. Objects stay until
// prune.
func (m *Manager) SetRef(ctx context.Context, name string, in contract.RefSetIntent) (contract.WorkspaceRefSetResponse, error) {
	var resp contract.WorkspaceRefSetResponse
	if !contract.ValidWorkspaceName(name) {
		return resp, contract.InvalidWorkspaceName()
	}
	ctx, done := m.opCtx(ctx)
	defer done()
	h, release, err := m.write(ctx, name, in.Instance)
	if err != nil {
		return resp, opError("ref set", err)
	}
	defer release()
	m.d.stage("refset-locked", ctx)
	refs, err := readRefs(h.repo())
	if err != nil {
		return resp, opError("ref set", err)
	}
	var target plumbing.Hash
	if !in.Delete {
		s, sfs := openStorage(h.repo(), m.d)
		target, err = resolve(ctx, s, refs, in.Target)
		closeStorage(s, sfs)
		if err != nil {
			return resp, opError("ref set", err)
		}
	}
	cur, exists := refs[in.Ref]
	switch {
	case in.Expected == "" && exists:
		return resp, contract.New(contract.CodeConflict, "stale expected value: the branch exists (expected absent); read workspace status and retry explicitly")
	case in.Expected != "" && (!exists || cur.String() != in.Expected):
		return resp, contract.New(contract.CodeConflict, "stale expected value: the branch's current value differs; read workspace status and retry explicitly")
	case !exists && namespaceConflict(refs, in.Ref):
		return resp, contract.New(contract.CodeConflict, "branch namespace conflict: an existing branch is a path prefix of this name or has it as a prefix")
	}
	t, err := m.begin(ctx, h, true)
	if err != nil {
		return resp, opError("ref set", err)
	}
	if in.Delete {
		err = m.d.deleteRef(t.repo(), in.Ref)
	} else {
		err = m.d.writeRef(t.repo(), in.Ref, target)
	}
	if err == nil {
		_, err = m.d.validateGeneration(ctx, t.dir, nil)
	}
	if err != nil {
		t.discard()
		return resp, opError("ref set", err)
	}
	if err := t.commit(ctx); err != nil {
		return resp, opError("ref set", err)
	}
	return contract.WorkspaceRefSetResponse{Name: h.name, Instance: h.instance, Generation: h.gen, Ref: in.Ref,
		OldCommit: hashPtr(cur, exists), NewCommit: hashPtr(target, !in.Delete)}, nil
}

// StatusQuery is one status page request. A continuation carries After,
// Instance and Generation together (validated by the caller).
type StatusQuery struct {
	After, Instance, Generation string
	Limit                       int
}

func restartPagination() error {
	return contract.New(contract.CodeConflict, "the workspace changed since the previous page (instance or generation); restart pagination from the first page")
}

// Status returns one page of the stored ref inventory sorted by raw byte
// name: branches with null published_at, task refs with their plane
// publication time. An unborn main is simply absent; HEAD is no row.
func (m *Manager) Status(ctx context.Context, name string, q StatusQuery) (contract.WorkspaceStatusResponse, error) {
	var resp contract.WorkspaceStatusResponse
	if !contract.ValidWorkspaceName(name) {
		return resp, contract.InvalidWorkspaceName()
	}
	ctx, done := m.opCtx(ctx)
	defer done()
	h, release, err := m.read(ctx, name, "")
	if err != nil {
		return resp, opError("status", err)
	}
	defer release()
	if q.Instance != "" && (q.Instance != h.instance || q.Generation != h.gen) {
		return resp, restartPagination()
	}
	refs, err := readRefs(h.repo())
	if err != nil {
		return resp, opError("status", err)
	}
	tasks, err := readRefsDoc(filepath.Join(h.genDir(h.gen), refsName))
	if err != nil {
		return resp, opError("status", err)
	}
	var rows []contract.WorkspaceRef
	for n, c := range refs {
		if n <= q.After {
			continue
		}
		r := contract.WorkspaceRef{Name: n, Commit: c.String()}
		if meta, ok := tasks[n]; ok {
			p := meta.PublishedAt
			r.PublishedAt = &p
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	resp = contract.WorkspaceStatusResponse{Name: h.name, Instance: h.instance, Generation: h.gen, DefaultBranch: contract.DefaultBranchRef, Refs: []contract.WorkspaceRef{}}
	if err := page(&resp, &resp.Refs, &resp.NextAfter, rows, func(r contract.WorkspaceRef) string { return r.Name }, q.Limit); err != nil {
		return resp, opError("status", err)
	}
	return resp, nil
}

// errIsNotFound reports a go-git missing-object error.
func errIsNotFound(err error) bool { return errors.Is(err, plumbing.ErrObjectNotFound) }
