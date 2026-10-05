// Package workspace is the plane's workspace hub (iteration 09a): durable,
// named bare git repositories under the plane state root, published
// through atomically swapped disk generations, served over the plane's
// existing TLS endpoint as guarded smart HTTP, with explicit branch
// compare-and-swap, task-ref pruning with object collection, and
// content-free ref and tree-metadata inspection. It uses the pinned go-git
// storage, protocol and server packages only: no git executable, shell,
// hooks, remotes, alternates or file/SSH transport.
//
// Concurrency: the plane's state lock excludes other processes. A
// registry lock serializes name creation and removal; a context-aware
// readers/writer lock per workspace serializes its mutations against
// reads, and is always taken after the registry lock when both are
// needed. Every mutation copies the current generation to a fresh private
// sibling, applies itself there, validates the result and publishes it
// by renaming a synced CURRENT; nothing else is ever reader-visible.
package workspace

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Manager owns every workspace of one plane state root.
type Manager struct {
	d    *deps
	root string
	dir  string

	reg *ctxMutex

	mu     sync.Mutex
	live   map[string]*handle
	fenced bool
	// pubs are the active task publication fences (iteration 10b).
	pubs *fences

	base   context.Context
	cancel context.CancelFunc
}

// handle is one live workspace instance. gen and dead change only under
// the workspace's exclusive lock and are read under either lock.
type handle struct {
	name     string
	instance string
	created  time.Time
	dir      string

	lock   rwLock
	gen    string
	dead   bool
	fenced atomic.Bool
}

// Open validates every workspace under root (the canonical plane state
// root, at most MaxRootBytes) before the plane listens, removes recognized
// leftovers (non-current generations, a CURRENT temporary, create staging
// and removal tombstones) and syncs the affected directories. Missing or
// malformed identity or CURRENT, a corrupt active generation or any
// unexpected or unsafe entry fails; nothing is repaired or guessed. A
// state root without workspaces/ is an empty registry.
func Open(ctx context.Context, root string) (*Manager, error) {
	return defaultDeps().open(ctx, root, true)
}

// OpenOptions are OpenWith's injected persistence primitives (iteration
// 10b: the cross-store publication crash tests fail a directory sync at a
// named boundary). The zero value is production.
type OpenOptions struct {
	// DirSyncFault, when non-nil, is consulted with a directory's path
	// before its production sync; a non-nil error is returned instead of
	// syncing.
	DirSyncFault func(dir string) error
}

// OpenWith is Open with o's persistence primitives.
func OpenWith(ctx context.Context, root string, o OpenOptions) (*Manager, error) {
	d := defaultDeps()
	if fault := o.DirSyncFault; fault != nil {
		sync := d.syncDir
		d.syncDir = func(f *os.File) error {
			if err := fault(f.Name()); err != nil {
				return err
			}
			return sync(f)
		}
	}
	return d.open(ctx, root, true)
}

// Inspect validates root's workspaces read-only (plane status): the same
// checks as Open, leftovers admitted and never removed. It returns the
// number of workspaces.
func Inspect(ctx context.Context, root string) (int, error) {
	m, err := defaultDeps().open(ctx, root, false)
	if err != nil {
		return 0, err
	}
	defer m.Close()
	return len(m.live), nil
}

func (d *deps) open(ctx context.Context, root string, cleanup bool) (*Manager, error) {
	if len(root) > MaxRootBytes {
		return nil, contract.New(contract.CodeInvalidArgument, "plane state root exceeds 256 bytes; choose a shorter --state-dir")
	}
	m := &Manager{d: d, root: root, dir: filepath.Join(root, dirName), reg: newCtxMutex(), live: map[string]*handle{}}
	m.base, m.cancel = context.WithCancel(context.Background())
	fi, err := os.Lstat(m.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, startupError(err)
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return nil, startupError(errUnsafe{m.dir})
	}
	if err := checkPrivate(m.dir, fi); err != nil {
		return nil, startupError(err)
	}
	es, err := readDir(m.dir)
	if err != nil {
		return nil, startupError(err)
	}
	removed := false
	for _, e := range es {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, p := e.Name(), filepath.Join(m.dir, e.Name())
		if tok, ok := stagingToken(n); ok && contract.ValidWorkspaceToken(tok) {
			if cleanup {
				if err := d.removeTree(p); err != nil {
					return nil, startupError(err)
				}
				removed = true
			} else if err := checkTree(p); err != nil {
				return nil, startupError(err)
			}
			continue
		}
		if !contract.ValidWorkspaceName(n) {
			return nil, startupError(corrupt(p, "unexpected entry in workspaces/"))
		}
		h, err := d.load(ctx, p, n, cleanup)
		if err != nil {
			return nil, startupError(err)
		}
		m.live[n] = h
	}
	if removed {
		if err := d.syncDirPath(m.dir); err != nil {
			return nil, startupError(err)
		}
	}
	return m, nil
}

func stagingToken(n string) (string, bool) {
	if t, ok := strings.CutPrefix(n, createPrefix); ok {
		return t, true
	}
	return strings.CutPrefix(n, removedPrefix)
}

// checkTree rejects unsafe entries below p without changing anything.
func checkTree(p string) error {
	_, err := treeSize(p)
	return err
}

// load validates one workspace directory and, when cleanup, removes its
// non-current generations and CURRENT temporary.
func (d *deps) load(ctx context.Context, dir, name string, cleanup bool) (*handle, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if err := checkPrivate(dir, fi); err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, corrupt(dir, "a workspace must be a directory")
	}
	es, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	var tmp bool
	seen := map[string]bool{}
	for _, e := range es {
		switch e.Name() {
		case identityName, currentName, generationsName:
			seen[e.Name()] = true
		case currentTmpName:
			tmp = true
		default:
			return nil, corrupt(filepath.Join(dir, e.Name()), "unexpected workspace entry")
		}
	}
	for _, n := range []string{identityName, currentName, generationsName} {
		if !seen[n] {
			return nil, corrupt(filepath.Join(dir, n), "missing")
		}
	}
	id, created, err := readIdentity(filepath.Join(dir, identityName), name)
	if err != nil {
		return nil, err
	}
	gen, err := readCurrent(filepath.Join(dir, currentName))
	if err != nil {
		return nil, err
	}
	gens := filepath.Join(dir, generationsName)
	gfi, err := os.Lstat(gens)
	if err != nil {
		return nil, err
	}
	if err := checkPrivate(gens, gfi); err != nil {
		return nil, err
	}
	if !gfi.IsDir() {
		return nil, corrupt(gens, "must be a directory")
	}
	ges, err := readDir(gens)
	if err != nil {
		return nil, err
	}
	found := false
	var stale []string
	for _, e := range ges {
		p := filepath.Join(gens, e.Name())
		if !contract.ValidWorkspaceToken(e.Name()) {
			return nil, corrupt(p, "unexpected generations entry")
		}
		// Every generation, the active one included, must be a private real
		// directory before anything traverses it: never a link.
		if err := checkGenerationEntry(p); err != nil {
			return nil, err
		}
		if e.Name() == gen {
			found = true
			continue
		}
		if err := checkTree(p); err != nil {
			return nil, err
		}
		stale = append(stale, p)
	}
	if !found {
		return nil, corrupt(filepath.Join(dir, currentName), "CURRENT names no existing generation")
	}
	if _, err := d.validateGeneration(ctx, filepath.Join(gens, gen), nil); err != nil {
		return nil, err
	}
	if cleanup {
		if tmp {
			if err := d.remove(filepath.Join(dir, currentTmpName)); err != nil {
				return nil, err
			}
			if err := d.syncDirPath(dir); err != nil {
				return nil, err
			}
		}
		for _, p := range stale {
			if err := d.removeTree(p); err != nil {
				return nil, err
			}
		}
		if len(stale) > 0 {
			if err := d.syncDirPath(gens); err != nil {
				return nil, err
			}
		}
	} else if tmp {
		if _, err := readBounded(filepath.Join(dir, currentTmpName), maxMetaFile); err != nil {
			return nil, err
		}
	}
	return &handle{name: name, instance: id.Instance, created: created, dir: dir, gen: gen}, nil
}

// checkGenerationEntry requires p (without following it) to be a private
// real directory.
func checkGenerationEntry(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if err := checkPrivate(p, fi); err != nil {
		return err
	}
	if !fi.IsDir() {
		return corrupt(p, "a generation must be a directory")
	}
	return nil
}

// startupError maps a load failure: corrupt or unsafe state is a conflict
// (never repaired), anything else internal.
func startupError(err error) error {
	var ce *contract.Error
	if errors.As(err, &ce) {
		return ce
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var c errCorrupt
	var u errUnsafe
	var cl errClosure
	if errors.As(err, &c) || errors.As(err, &u) || errors.As(err, &cl) || errors.Is(err, errPathBudget) {
		return contract.Wrap(contract.CodeConflict, fmt.Sprintf("invalid workspace state: %v; workspace state is never repaired or regenerated automatically: stop the plane, then restore a complete stopped backup of the plane state directory", err), err)
	}
	return contract.Wrap(contract.CodeInternal, fmt.Sprintf("cannot load workspace state: %v", err), err)
}

// Close cancels every operation in progress (plane shutdown); the plane's
// handler tracker joins them.
func (m *Manager) Close() { m.cancel() }

// opCtx merges an operation's context with the manager's shutdown.
func (m *Manager) opCtx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(m.base, cancel)
	return ctx, func() { stop(); cancel() }
}

func (d *deps) token() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(d.rand, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ---- Errors ----

var (
	errNotFound = func() error {
		return contract.New(contract.CodeNotFound, "no such workspace")
	}
	errStaleInstance = func() error {
		return contract.New(contract.CodeConflict, "the workspace instance does not match: the name was removed or recreated; read workspace show again")
	}
)

// opError maps an operation failure to a safe contract error naming the
// operation. Specific branches keep the cause off the serialized message.
// The generic storage failure includes the cause type and text because
// contract.Error omits the wrapped value, and a stress log otherwise
// cannot tell a pack error from a corrupt ref or a runner error.
func opError(op string, err error) error {
	var ce *contract.Error
	switch {
	case errors.As(err, &ce):
		return ce
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return contract.Wrap(contract.CodeUnavailable, "workspace "+op+" was cancelled before publication; nothing changed", err)
	}
	var te txnError
	if errors.As(err, &te) && te.committed {
		return contract.Wrap(contract.CodeInternal, "workspace "+op+" was published but its storage outcome is ambiguous; the workspace is fenced until the plane restarts: inspect it with workspace show and status before acting again", err)
	}
	var cl errClosure
	if errors.As(err, &cl) {
		return contract.Wrap(contract.CodeInternal, "workspace "+op+" failed: the stored object closure is incomplete or invalid; nothing changed", err)
	}
	if errors.Is(err, errPathBudget) {
		return contract.Wrap(contract.CodeInternal, "workspace "+op+" failed: a storage path exceeds its length budget; nothing changed", err)
	}
	return contract.Wrap(contract.CodeInternal, fmt.Sprintf("workspace %s failed: storage failure; nothing changed [%T] %s", op, err, err.Error()), err)
}

func fencedError() error {
	return contract.New(contract.CodeInternal, "workspace mutations are fenced after an ambiguous storage outcome; restart the plane (callsheet plane run) to recover, and inspect before acting again")
}

// ---- Handles and locks ----

func (m *Manager) lookup(name string) (*handle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.live[name]
	if h == nil {
		return nil, errNotFound()
	}
	return h, nil
}

func (m *Manager) managerFenced() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fenced
}

func (m *Manager) fence() {
	m.mu.Lock()
	m.fenced = true
	m.mu.Unlock()
}

// read takes a stable handle, its shared lock and rechecks it is live;
// instance, when nonempty, must match.
func (m *Manager) read(ctx context.Context, name, instance string) (*handle, func(), error) {
	h, err := m.lookup(name)
	if err != nil {
		return nil, nil, err
	}
	if err := h.lock.RLock(ctx); err != nil {
		return nil, nil, err
	}
	if h.dead {
		h.lock.RUnlock()
		return nil, nil, errNotFound()
	}
	if instance != "" && instance != h.instance {
		h.lock.RUnlock()
		return nil, nil, errStaleInstance()
	}
	return h, h.lock.RUnlock, nil
}

// write takes a stable handle whose instance matches, its exclusive lock,
// and rechecks it is live, matching and not fenced.
func (m *Manager) write(ctx context.Context, name, instance string) (*handle, func(), error) {
	h, err := m.lookup(name)
	if err != nil {
		return nil, nil, err
	}
	if h.instance != instance {
		return nil, nil, errStaleInstance()
	}
	if err := h.lock.Lock(ctx); err != nil {
		return nil, nil, err
	}
	switch {
	case h.dead:
		h.lock.Unlock()
		return nil, nil, errNotFound()
	case h.fenced.Load() || m.managerFenced():
		h.lock.Unlock()
		return nil, nil, fencedError()
	}
	return h, h.lock.Unlock, nil
}

func (h *handle) genDir(gen string) string { return filepath.Join(h.dir, generationsName, gen) }
func (h *handle) repo() string             { return filepath.Join(h.genDir(h.gen), repoName) }

// ---- Create, list, show, remove ----

// Create creates the empty workspace name: an empty bare repository whose
// HEAD names the unborn main, published whole by renaming its synced
// staging directory. A duplicate name conflicts, also for an identical
// request; an invalid name writes nothing.
func (m *Manager) Create(ctx context.Context, name string) (contract.WorkspaceView, error) {
	if !contract.ValidWorkspaceName(name) {
		return contract.WorkspaceView{}, contract.InvalidWorkspaceName()
	}
	ctx, done := m.opCtx(ctx)
	defer done()
	if m.managerFenced() {
		return contract.WorkspaceView{}, fencedError()
	}
	m.d.stage("create-queued", ctx)
	if err := m.reg.Lock(ctx); err != nil {
		return contract.WorkspaceView{}, opError("create", err)
	}
	defer m.reg.Unlock()
	// A namespace operation that held the registry lock while this one
	// waited may have fenced the manager: recheck before touching disk.
	if m.managerFenced() {
		return contract.WorkspaceView{}, fencedError()
	}
	if _, err := m.lookup(name); err == nil {
		return contract.WorkspaceView{}, contract.New(contract.CodeConflict, "a workspace with this name exists; workspace names are unique (read workspace show)")
	}
	dest := filepath.Join(m.dir, name)
	if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
		return contract.WorkspaceView{}, opError("create", fmt.Errorf("%s exists but is not a live workspace", dest))
	}
	return m.create(ctx, name, dest)
}

func (m *Manager) create(ctx context.Context, name, dest string) (contract.WorkspaceView, error) {
	d := m.d
	fail := func(err error) (contract.WorkspaceView, error) {
		return contract.WorkspaceView{}, opError("create", err)
	}
	if err := m.ensureDir(); err != nil {
		return fail(err)
	}
	stok, err := d.token()
	if err != nil {
		return fail(err)
	}
	inst, err := d.token()
	if err != nil {
		return fail(err)
	}
	gen, err := d.token()
	if err != nil {
		return fail(err)
	}
	created := d.now().UTC()
	staging := filepath.Join(m.dir, createPrefix+stok)
	build := func() error {
		if err := d.mkdir(staging); err != nil {
			return err
		}
		id := identity{Schema: schemaVersion, Name: name, Instance: inst, CreatedAt: contract.FormatTime(created)}
		if err := d.writeFile(filepath.Join(staging, identityName), encodeJSON(id)); err != nil {
			return err
		}
		if err := d.writeFile(filepath.Join(staging, currentName), []byte(gen+"\n")); err != nil {
			return err
		}
		if err := d.mkdir(filepath.Join(staging, generationsName)); err != nil {
			return err
		}
		g := filepath.Join(staging, generationsName, gen)
		if err := d.mkdir(g); err != nil {
			return err
		}
		if err := d.initRepo(filepath.Join(g, repoName)); err != nil {
			return err
		}
		if err := d.writeFile(filepath.Join(g, refsName), encodeRefsDoc(nil)); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.syncTree(ctx, staging); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return d.rename(staging, dest)
	}
	if err := build(); err != nil {
		d.removeTree(staging)
		return contract.WorkspaceView{}, opError("create", err)
	}
	// The name is published: register it whatever follows, after measuring
	// its size (so no request can mutate it during the walk).
	h := &handle{name: name, instance: inst, created: created, dir: dest, gen: gen}
	register := func() {
		m.mu.Lock()
		m.live[name] = h
		m.mu.Unlock()
	}
	if err := d.syncDirPath(m.dir); err != nil {
		register()
		m.fence()
		return contract.WorkspaceView{}, opError("create", txnError{committed: true, cause: err})
	}
	v, err := m.view(h)
	register()
	return v, err
}

// ensureDir creates workspaces/ (0700) and makes it durable in the state
// root before the first create.
func (m *Manager) ensureDir() error {
	fi, err := os.Lstat(m.dir)
	if err == nil {
		if !fi.IsDir() {
			return errUnsafe{m.dir}
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := m.d.mkdir(m.dir); err != nil {
		return err
	}
	return m.d.syncDirPath(m.root)
}

// view builds a workspace view; the caller holds its lock (or has just
// created it). size_bytes is measured now.
func (m *Manager) view(h *handle) (contract.WorkspaceView, error) {
	var entries int64
	size, err := measure(h.dir, &entries)
	if err != nil {
		return contract.WorkspaceView{}, opError("show", err)
	}
	m.d.emit("scanned-entries", entries)
	return contract.WorkspaceView{Name: h.name, Instance: h.instance, CreatedAt: contract.FormatTime(h.created), Generation: h.gen,
		DefaultBranch: contract.DefaultBranchRef, SizeBytes: size, Retention: contract.NewRetention()}, nil
}

// Show returns name's identity, generation and owned storage size,
// measured under its read lock.
func (m *Manager) Show(ctx context.Context, name string) (contract.WorkspaceView, error) {
	if !contract.ValidWorkspaceName(name) {
		return contract.WorkspaceView{}, contract.InvalidWorkspaceName()
	}
	ctx, done := m.opCtx(ctx)
	defer done()
	h, release, err := m.read(ctx, name, "")
	if err != nil {
		return contract.WorkspaceView{}, opError("show", err)
	}
	defer release()
	return m.view(h)
}

// List returns one page of workspaces sorted by ASCII name after the
// exclusive cursor. Every page is a fresh observation: pages are not a
// snapshot across concurrent create and remove. No disk traversal.
func (m *Manager) List(after string, limit int) (contract.WorkspaceListResponse, error) {
	m.mu.Lock()
	var rows []contract.WorkspaceSummary
	for n, h := range m.live {
		if n > after {
			rows = append(rows, contract.WorkspaceSummary{Name: n, Instance: h.instance, CreatedAt: contract.FormatTime(h.created)})
		}
	}
	m.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	resp := contract.WorkspaceListResponse{Workspaces: []contract.WorkspaceSummary{}}
	err := page(&resp, &resp.Workspaces, &resp.NextAfter, rows, func(s contract.WorkspaceSummary) string { return s.Name }, limit)
	return resp, err
}

// Remove deletes the whole workspace name if instance matches: it waits
// (cancellably) for readers and a writer, renames the directory to a
// tombstone (the commit point), syncs, deletes the tombstone and syncs
// again before acknowledging.
func (m *Manager) Remove(ctx context.Context, name, instance string) (contract.WorkspaceRemoveResponse, error) {
	var resp contract.WorkspaceRemoveResponse
	if !contract.ValidWorkspaceName(name) {
		return resp, contract.InvalidWorkspaceName()
	}
	if err := contract.ValidateInstance(instance); err != nil {
		return resp, err
	}
	ctx, done := m.opCtx(ctx)
	defer done()
	if m.managerFenced() {
		return resp, fencedError()
	}
	// Iteration 10b: a task publication between its workspace commit and
	// its terminal record holds removal off. The fence is waited for
	// cancellably with no lock held, and rechecked once both locks are
	// held: a publication may have fenced and committed while this removal
	// was queued for them (then both are released and the wait repeats).
	var h *handle
	for {
		if err := m.awaitFences(ctx, name); err != nil {
			return resp, opError("remove", err)
		}
		if err := m.reg.Lock(ctx); err != nil {
			return resp, opError("remove", err)
		}
		hh, release, err := m.write(ctx, name, instance)
		if err != nil {
			m.reg.Unlock()
			return resp, opError("remove", err)
		}
		if !m.Fenced(name) {
			h = hh
			defer m.reg.Unlock()
			defer release()
			break
		}
		release()
		m.reg.Unlock()
		m.d.stage("remove-refenced", ctx)
	}
	d := m.d
	tok, err := d.token()
	if err != nil {
		return resp, opError("remove", err)
	}
	d.stage("remove-locked", ctx)
	tomb := filepath.Join(m.dir, removedPrefix+tok)
	if err := ctx.Err(); err != nil {
		return resp, opError("remove", err)
	}
	if err := d.rename(h.dir, tomb); err != nil {
		return resp, opError("remove", err)
	}
	h.dead = true
	m.mu.Lock()
	delete(m.live, name)
	m.mu.Unlock()
	ambiguous := func(err error) (contract.WorkspaceRemoveResponse, error) {
		m.fence()
		return resp, contract.Wrap(contract.CodeInternal, "the workspace name was removed but cleanup or its durability is unconfirmed; workspace mutations are fenced until the plane restarts: inspect with workspace ls before acting again", err)
	}
	if err := d.syncDirPath(m.dir); err != nil {
		return ambiguous(err)
	}
	if err := d.removeTree(tomb); err != nil {
		return ambiguous(err)
	}
	if err := d.syncDirPath(m.dir); err != nil {
		return ambiguous(err)
	}
	return contract.WorkspaceRemoveResponse{Name: name, Instance: instance, Removed: true}, nil
}

// ---- Generation transactions ----

// txnError is a transaction failure; committed marks an ambiguous outcome
// after the CURRENT rename (the workspace is fenced).
type txnError struct {
	committed bool
	cause     error
}

func (e txnError) Error() string { return e.cause.Error() }
func (e txnError) Unwrap() error { return e.cause }

// txn is one private generation of workspace h: a copy of the current one
// or a fresh repository. Only commit makes it visible.
type txn struct {
	d     *deps
	h     *handle
	gen   string
	dir   string
	stats copyStats
}

// begin creates the new sibling generation: a regular-file copy of the
// current generation when copy, else an empty directory.
func (m *Manager) begin(ctx context.Context, h *handle, copy bool) (*txn, error) {
	gen, err := m.d.token()
	if err != nil {
		return nil, err
	}
	t := &txn{d: m.d, h: h, gen: gen, dir: h.genDir(gen)}
	if copy {
		err = m.d.copyTree(ctx, h.genDir(h.gen), t.dir, &t.stats)
	} else {
		err = m.d.mkdir(t.dir)
	}
	if err != nil {
		t.discard()
		return nil, err
	}
	m.d.emit("copied-bytes", t.stats.bytes)
	m.d.emit("copied-files", int64(t.stats.files))
	return t, nil
}

func (t *txn) repo() string { return filepath.Join(t.dir, repoName) }

// discard removes an unpublished generation (best effort: a leftover is
// non-current and removed at the next startup).
func (t *txn) discard() {
	if t.d.removeTree(t.dir) == nil {
		t.d.syncDirPath(filepath.Dir(t.dir))
	}
}

// commit publishes the generation: sync it bottom-up and then its entry in
// generations/, write and sync a CURRENT temporary, rename it onto CURRENT
// (the commit point) and sync the workspace directory; then delete the
// previous generation and sync generations/ again. Syncing generations/
// before the rename keeps a durable CURRENT from naming a generation whose
// directory entry could still be lost. Before the rename every failure
// (and cancellation) discards the generation and keeps the old one
// authoritative; after it, a failure fences the workspace and is
// ambiguous.
func (t *txn) commit(ctx context.Context) error {
	d, h := t.d, t.h
	pre := func(err error) error {
		t.discard()
		return txnError{cause: err}
	}
	if err := ctx.Err(); err != nil {
		return pre(err)
	}
	if err := d.syncTree(ctx, t.dir); err != nil {
		return pre(err)
	}
	if err := d.syncDirPath(filepath.Dir(t.dir)); err != nil {
		return pre(err)
	}
	tmp := filepath.Join(h.dir, currentTmpName)
	if _, err := os.Lstat(tmp); err == nil {
		if err := d.remove(tmp); err != nil {
			return pre(err)
		}
	}
	if err := d.writeFileSync(tmp, []byte(t.gen+"\n")); err != nil {
		os.Remove(tmp)
		return pre(err)
	}
	d.stage("before-publish", ctx)
	if err := ctx.Err(); err != nil {
		os.Remove(tmp)
		return pre(err)
	}
	if err := d.rename(tmp, filepath.Join(h.dir, currentName)); err != nil {
		os.Remove(tmp)
		return pre(err)
	}
	old := h.gen
	h.gen = t.gen
	post := func(err error) error {
		h.fenced.Store(true)
		return txnError{committed: true, cause: err}
	}
	if err := d.syncDirPath(h.dir); err != nil {
		return post(err)
	}
	if err := d.removeTree(h.genDir(old)); err != nil {
		return post(err)
	}
	if err := d.syncDirPath(filepath.Join(h.dir, generationsName)); err != nil {
		return post(err)
	}
	return nil
}

// ---- Pages ----

// page fills rows[:limit] into *dst of resp, stopping before the encoded
// response would exceed contract.MaxWorkspaceResponse; *next is the last
// row's key when rows remain. A single row that cannot fit is
// invalid_argument (never truncated).
func page[T any](resp any, dst *[]T, next **string, rows []T, key func(T) string, limit int) error {
	*dst = (*dst)[:0]
	*next = nil
	base, err := contract.Encode(resp)
	if err != nil {
		return err
	}
	size := len(base) + 1
	for i, r := range rows {
		if i == limit {
			k := key(rows[i-1])
			*next = &k
			break
		}
		b, err := contract.Encode(r)
		if err != nil {
			return err
		}
		add := len(b)
		if i > 0 {
			add++
		}
		k := key(r)
		kb, _ := contract.Encode(k)
		// Reserve room for next_after naming this row.
		if size+add+len(kb)-len("null") > contract.MaxWorkspaceResponse {
			if i == 0 {
				return contract.New(contract.CodeInvalidArgument, "a single row does not fit the 1 MiB response bound")
			}
			pk := key(rows[i-1])
			*next = &pk
			break
		}
		size += add
		*dst = append(*dst, r)
	}
	final, err := contract.Encode(resp)
	if err != nil {
		return err
	}
	if len(final)+1 > contract.MaxWorkspaceResponse {
		return contract.New(contract.CodeInternal, "the response exceeds its 1 MiB bound")
	}
	return nil
}
