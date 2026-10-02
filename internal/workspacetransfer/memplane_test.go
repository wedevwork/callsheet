package workspacetransfer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// memPlane is an in-memory plane for orchestration tests (no TLS, no
// storage durability): the same CAS, instance, pagination and
// reachability rules as the hub, so many transfers per test stay cheap
// under the repeated stress runs. The receive-pack proofs that must use
// the production handler (UT-1) run against testPlane instead.
type memPlane struct {
	mu  sync.Mutex
	ws  map[string]*memWorkspace
	gen int
	// hooks, when set, run inside operations (tests: move refs, recreate).
	onFetch func()
	fetches int
}

type memWorkspace struct {
	instance string
	gen      string
	refs     map[string]plumbing.Hash
	store    *memory.Storage
}

func newMemPlane() *memPlane { return &memPlane{ws: map[string]*memWorkspace{}} }

func (p *memPlane) token() string {
	p.gen++
	return fmt.Sprintf("%032x", p.gen)
}

// create creates (or recreates) name with a new instance.
func (p *memPlane) create(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := &memWorkspace{instance: p.token(), gen: p.token(), refs: map[string]plumbing.Hash{}, store: memory.NewStorage()}
	p.ws[name] = w
	return w.instance
}

func (p *memPlane) get(name string) (*memWorkspace, error) {
	w := p.ws[name]
	if w == nil {
		return nil, contract.New(contract.CodeNotFound, "no such workspace")
	}
	return w, nil
}

// setRef sets or (zero) deletes a ref directly.
func (p *memPlane) setRef(name, ref string, h plumbing.Hash) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.ws[name]
	if h.IsZero() {
		delete(w.refs, ref)
	} else {
		w.refs[ref] = h
	}
	w.gen = p.token()
}

func (p *memPlane) refs(name string) map[string]plumbing.Hash {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]plumbing.Hash{}
	for k, v := range p.ws[name].refs {
		out[k] = v
	}
	return out
}

func (p *memPlane) view(name string, w *memWorkspace) contract.WorkspaceView {
	return contract.WorkspaceView{Name: name, Instance: w.instance, CreatedAt: "2026-09-30T12:00:00Z", Generation: w.gen,
		DefaultBranch: contract.DefaultBranchRef, Retention: contract.NewRetention()}
}

func (p *memPlane) ShowWorkspace(_ context.Context, name string) (contract.WorkspaceView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, err := p.get(name)
	if err != nil {
		return contract.WorkspaceView{}, err
	}
	return p.view(name, w), nil
}

func (p *memPlane) WorkspaceStatus(_ context.Context, name string, pg client.StatusPage) (contract.WorkspaceStatusResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, err := p.get(name)
	if err != nil {
		return contract.WorkspaceStatusResponse{}, err
	}
	if pg.Instance != "" && (pg.Instance != w.instance || pg.Generation != w.gen) {
		return contract.WorkspaceStatusResponse{}, contract.New(contract.CodeConflict, "restart pagination")
	}
	var names []string
	for n := range w.refs {
		if n > pg.After {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	r := contract.WorkspaceStatusResponse{Name: name, Instance: w.instance, Generation: w.gen, DefaultBranch: contract.DefaultBranchRef, Refs: []contract.WorkspaceRef{}}
	for i, n := range names {
		if i == pg.Limit {
			last := names[i-1]
			r.NextAfter = &last
			break
		}
		r.Refs = append(r.Refs, contract.WorkspaceRef{Name: n, Commit: w.refs[n].String()})
	}
	return r, nil
}

// reachable reports whether commit is reachable from w's refs.
func (w *memWorkspace) reachable(commit plumbing.Hash) bool {
	for _, tip := range w.refs {
		if tip == commit {
			return true
		}
		ok, err := isAncestor(context.Background(), w.store, commit, tip)
		if err == nil && ok {
			return true
		}
	}
	return false
}

func (p *memPlane) WorkspaceDiff(_ context.Context, name string, pg client.DiffPage) (contract.WorkspaceDiffResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, err := p.get(name)
	if err != nil {
		return contract.WorkspaceDiffResponse{}, err
	}
	h := plumbing.NewHash(pg.Target)
	if !w.reachable(h) {
		return contract.WorkspaceDiffResponse{}, contract.New(contract.CodeNotFound, "no reachable commit")
	}
	return contract.WorkspaceDiffResponse{Name: name, Instance: w.instance, Generation: w.gen, TargetCommit: pg.Target, Changes: []contract.WorkspaceChange{}}, nil
}

func (p *memPlane) WorkspaceGit(name string) (Git, error) { return &memGit{p: p, name: name}, nil }

type memGit struct {
	p    *memPlane
	name string
}

func (g *memGit) Close() {}

func (g *memGit) ReceiveRefs(_ context.Context, instance string) (map[string]plumbing.Hash, error) {
	g.p.mu.Lock()
	defer g.p.mu.Unlock()
	w, err := g.p.get(g.name)
	if err != nil {
		return nil, err
	}
	if w.instance != instance {
		return nil, contract.New(contract.CodeConflict, "stale workspace instance")
	}
	out := map[string]plumbing.Hash{}
	for k, v := range w.refs {
		if strings.HasPrefix(k, contract.BranchPrefix) {
			out[k] = v
		}
	}
	return out, nil
}

// Push applies one CAS command with its pack (the same rules as the hub:
// zero old creates only; otherwise old must equal the current value).
func (g *memGit) Push(ctx context.Context, instance, ref string, old, new plumbing.Hash, pack io.Reader) error {
	b, err := io.ReadAll(pack)
	if err != nil {
		return err
	}
	g.p.mu.Lock()
	defer g.p.mu.Unlock()
	w, err := g.p.get(g.name)
	if err != nil {
		return err
	}
	if w.instance != instance {
		return contract.New(contract.CodeConflict, "stale workspace instance")
	}
	cur, ok := w.refs[ref]
	if (old.IsZero() && ok) || (!old.IsZero() && (!ok || cur != old)) {
		return contract.New(contract.CodeConflict, "the branch moved since it was observed (compare-and-swap failed)")
	}
	if len(b) > 32 {
		if err := packfile.UpdateObjectStorage(w.store, bytes.NewReader(b)); err != nil {
			return contract.New(contract.CodeInternal, "bad pack")
		}
	}
	if _, err := walkClosure(ctx, w.store, []plumbing.Hash{new}, nil, false); err != nil {
		return contract.New(contract.CodeInternal, "incomplete object closure")
	}
	w.refs[ref] = new
	w.gen = g.p.token()
	return nil
}

// Fetch serves wants reachable from the refs (haves excluded).
func (g *memGit) Fetch(ctx context.Context, wants, haves []plumbing.Hash, sink func(io.Reader) error) error {
	if g.p.onFetch != nil {
		g.p.onFetch()
	}
	g.p.mu.Lock()
	g.p.fetches++
	w, err := g.p.get(g.name)
	if err != nil {
		g.p.mu.Unlock()
		return err
	}
	for _, h := range wants {
		if !w.reachable(h) {
			g.p.mu.Unlock()
			return contract.New(contract.CodeConflict, "the plane no longer offers the selected commit")
		}
	}
	var have []plumbing.Hash
	for _, h := range haves {
		if w.store.HasEncodedObject(h) == nil {
			have = append(have, h)
		}
	}
	objs, err := revlist.Objects(w.store, wants, have)
	var buf bytes.Buffer
	if err == nil {
		_, err = packfile.NewEncoder(&buf, w.store, false).Encode(objs, 0)
	}
	g.p.mu.Unlock()
	if err != nil {
		return err
	}
	return sink(&buf)
}

// commitFiles stores files as a commit (with parent, if nonzero) in
// name's store and sets ref to it.
func (p *memPlane) commitFiles(name, ref string, files map[string]fspec, parent plumbing.Hash) plumbing.Hash {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.ws[name]
	specs := map[string]testkit.FileSpec{}
	for k, f := range files {
		specs[k] = testkit.FileSpec{Mode: f.mode, Content: []byte(f.content)}
	}
	var parents []plumbing.Hash
	if !parent.IsZero() {
		parents = []plumbing.Hash{parent}
	}
	h, err := testkit.CommitFiles(w.store, specs, parents, "seed")
	if err != nil {
		panic(err)
	}
	w.refs[ref] = h
	w.gen = p.token()
	return h
}

// commit returns a stored commit of name.
func (p *memPlane) commit(name string, h plumbing.Hash) *object.Commit {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, err := object.GetCommit(p.ws[name].store, h)
	if err != nil {
		panic(err)
	}
	return c
}
