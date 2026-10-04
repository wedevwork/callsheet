// Package taskworkspace is a workspace task's worker-side lifecycle
// (iteration 10b): the shared per-node object cache, preparation (an
// authorized fetch, the task's trusted object copy and its private
// checkout), the result snapshot, its deterministic commit and bounded
// tree metadata, and the publication client that pushes the create-once
// task ref inside the plane's publication transaction. The sidecar keeps
// thin wiring: every decision here is injectable and joinable, and no
// lock of this package is ever held while a child runs or publishes.
package taskworkspace

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"crypto/rand"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Cache layout under the sidecar state root:
//
//	workspace-cache/<instance>/current/     bare object database
//	workspace-cache/<instance>/cache.json   version, name, instance, origin, last use, commits
//	workspace-cache/<instance>/.stage-<tok>/ exclusive staging (and retired) generations
const (
	CacheDirName  = "workspace-cache"
	cacheMetaName = "cache.json"
	currentName   = "current"
	stagePrefix   = ".stage-"
	cacheSchema   = 1
	// CacheTarget is the fixed idle cache target: logical regular-file
	// bytes over every entry.
	CacheTarget  int64 = 1 << 30
	maxCacheMeta       = 64 << 10
)

// cacheMeta is cache.json: the entry's identity, the enrollment target
// it belongs to, its last-use sequence and the cached base commits whose
// closures were verified when stored.
type cacheMeta struct {
	Schema   int      `json:"schema"`
	Name     string   `json:"name"`
	Instance string   `json:"instance"`
	Origin   string   `json:"origin"`
	LastUse  int64    `json:"last_use"`
	Commits  []string `json:"commits"`
}

// entry is one instance's cache entry. lock serializes its fetch,
// verification, replacement and eviction; size and lastUse change under
// the manager's registry mutex.
type entry struct {
	instance string
	lock     chan struct{}
	size     int64
	lastUse  int64
	disabled bool
	present  bool
}

// lockEntry acquires e's lock unless ctx ends first.
func lockEntry(ctx context.Context, e *entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case e.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func tryLockEntry(e *entry) bool {
	select {
	case e.lock <- struct{}{}:
		return true
	default:
		return false
	}
}

func unlockEntry(e *entry) { <-e.lock }

// Manager is one sidecar Run's shared object cache (owned under the
// sidecar's state lock). Different instances proceed independently; the
// registry mutex is never held while waiting for an entry's lock.
type Manager struct {
	root   string
	origin string
	target int64
	logger *slog.Logger
	// fail injects storage failures at named steps; hook observes stages
	// (tests).
	fail func(op string) error
	hook func(stage string)

	mu      sync.Mutex
	entries map[string]*entry
	seq     int64
}

// CacheOptions configures OpenCache.
type CacheOptions struct {
	// Root is the sidecar state root (the cache lives in its
	// workspace-cache directory).
	Root string
	// Origin identifies the enrollment target (plane URL and CA
	// fingerprint); a different one purges every entry before use.
	Origin string
	// Target overrides CacheTarget (tests); 0 means CacheTarget.
	Target int64
	Logger *slog.Logger
}

// OpenCache validates the cache root under the caller's state lock:
// recognized incomplete staging is discarded, corrupt or foreign entries
// (another enrollment target) are removed, unexpected names are left
// untouched and reported, and idle eviction is applied. A known entry that
// cannot be removed safely is reported and disabled.
func OpenCache(ctx context.Context, o CacheOptions) (*Manager, error) {
	m := &Manager{root: filepath.Join(o.Root, CacheDirName), origin: o.Origin, target: o.Target, logger: o.Logger, entries: map[string]*entry{}}
	if m.target <= 0 {
		m.target = CacheTarget
	}
	if m.logger == nil {
		m.logger = slog.New(slog.DiscardHandler)
	}
	return m, m.load(ctx)
}

func (m *Manager) check(op string) error {
	if m.fail != nil {
		return m.fail(op)
	}
	return nil
}

func (m *Manager) stage(s string) {
	if m.hook != nil {
		m.hook(s)
	}
}

func (m *Manager) load(ctx context.Context) error {
	fi, err := os.Lstat(m.root)
	if errors.Is(err, fs.ErrNotExist) {
		return os.Mkdir(m.root, 0o700)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 || fi.Mode().Perm()&0o077 != 0 {
		return contract.New(contract.CodeTrustFailed, "the sidecar's workspace cache directory must be a private real directory (mode 0700)")
	}
	des, err := os.ReadDir(m.root)
	if err != nil {
		return err
	}
	for _, de := range des {
		if err := ctx.Err(); err != nil {
			return err
		}
		inst := de.Name()
		p := filepath.Join(m.root, inst)
		if !contract.ValidWorkspaceToken(inst) {
			m.logger.Warn("workspace cache holds an unexpected entry; it is left untouched", "entry", contract.SafeText(inst, 64))
			continue
		}
		e := &entry{instance: inst, lock: make(chan struct{}, 1)}
		meta, ok := m.validEntry(ctx, p, inst)
		if !ok {
			if err := m.removeEntryDir(p); err != nil {
				m.logger.Error("workspace cache entry cannot be removed safely; it is disabled", "instance", inst)
				e.disabled = true
				m.entries[inst] = e
			}
			continue
		}
		e.present, e.lastUse = true, meta.LastUse
		if e.size, err = workspacetransfer.LogicalSize(ctx, filepath.Join(p, currentName)); err != nil {
			e.disabled = true
		}
		if meta.LastUse > m.seq {
			m.seq = meta.LastUse
		}
		m.entries[inst] = e
	}
	m.evict(ctx, "")
	return nil
}

// validEntry checks one entry directory: a private real directory holding
// only cache.json, current/ and .stage-* leftovers (removed here), an
// identity of this instance and enrollment target, and a current object
// database of the managed layout.
func (m *Manager) validEntry(ctx context.Context, p, inst string) (cacheMeta, bool) {
	fi, err := os.Lstat(p)
	if err != nil || !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return cacheMeta{}, false
	}
	des, err := os.ReadDir(p)
	if err != nil {
		return cacheMeta{}, false
	}
	for _, de := range des {
		n := de.Name()
		switch {
		case n == cacheMetaName, n == currentName:
		case strings.HasPrefix(n, stagePrefix):
			if err := workspacetransfer.RemoveTree(filepath.Join(p, n)); err != nil {
				return cacheMeta{}, false
			}
		default:
			return cacheMeta{}, false
		}
	}
	meta, err := readMeta(filepath.Join(p, cacheMetaName))
	if err != nil || meta.Instance != inst || meta.Origin != m.origin {
		return cacheMeta{}, false
	}
	db, err := workspacetransfer.OpenTaskDB(filepath.Join(p, currentName))
	if err != nil {
		return cacheMeta{}, false
	}
	db.Close()
	return meta, true
}

func readMeta(p string) (cacheMeta, error) {
	var meta cacheMeta
	fi, err := os.Lstat(p)
	if err != nil {
		return meta, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxCacheMeta {
		return meta, errors.New("invalid cache.json")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return meta, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil {
		return meta, err
	}
	if meta.Schema != cacheSchema || !contract.ValidWorkspaceName(meta.Name) || !contract.ValidWorkspaceToken(meta.Instance) || meta.LastUse < 0 {
		return meta, errors.New("invalid cache.json")
	}
	for _, c := range meta.Commits {
		if !contract.ValidCommitHash(c) {
			return meta, errors.New("invalid cache.json")
		}
	}
	return meta, nil
}

// writeMeta publishes cache.json atomically (temporary, sync, rename,
// directory sync).
func (m *Manager) writeMeta(dir string, meta cacheMeta) error {
	if err := m.check("cache-meta"); err != nil {
		return err
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, stagePrefix+"meta-"+token())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err == nil {
		err = f.Sync()
		f.Close()
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, cacheMetaName))
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	return errors.Join(err, f.Close())
}

func token() string {
	var b [16]byte
	io.ReadFull(rand.Reader, b[:])
	return hex.EncodeToString(b[:])
}

func (m *Manager) removeEntryDir(p string) error {
	if err := m.check("cache-remove"); err != nil {
		return err
	}
	if err := workspacetransfer.RemoveTree(p); err != nil {
		return err
	}
	return syncDir(m.root)
}

// entryFor returns instance's registry entry, creating an absent one.
func (m *Manager) entryFor(inst string) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[inst]
	if e == nil {
		e = &entry{instance: inst, lock: make(chan struct{}, 1)}
		m.entries[inst] = e
	}
	return e
}

// Size reports the cache's total logical bytes (idle and active entries).
func (m *Manager) Size() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, e := range m.entries {
		if e.present {
			n += e.size
		}
	}
	return n
}

// evict removes least-recently-used idle entries (ties: more bytes first)
// until the total is within the target. keep is never a candidate (the
// caller holds its lock). A candidate is selected under the registry
// mutex, its lock taken without waiting (a busy entry is not idle), its
// ownership rechecked, then it is removed.
func (m *Manager) evict(ctx context.Context, keep string) {
	tried := map[string]bool{}
	for {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		var total int64
		var cands []*entry
		for _, e := range m.entries {
			if !e.present {
				continue
			}
			total += e.size
			if e.instance != keep && !e.disabled && !tried[e.instance] {
				cands = append(cands, e)
			}
		}
		if total <= m.target || len(cands) == 0 {
			m.mu.Unlock()
			return
		}
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].lastUse != cands[j].lastUse {
				return cands[i].lastUse < cands[j].lastUse
			}
			if cands[i].size != cands[j].size {
				return cands[i].size > cands[j].size
			}
			return cands[i].instance < cands[j].instance
		})
		c := cands[0]
		m.mu.Unlock()
		tried[c.instance] = true
		if !tryLockEntry(c) {
			continue
		}
		m.stage("evict-locked")
		m.mu.Lock()
		owned := m.entries[c.instance] == c && c.present
		m.mu.Unlock()
		if owned {
			if err := m.removeEntryDir(filepath.Join(m.root, c.instance)); err != nil {
				m.logger.Error("workspace cache entry cannot be removed safely; it is disabled", "instance", c.instance)
				m.mu.Lock()
				c.disabled = true
				m.mu.Unlock()
			} else {
				m.mu.Lock()
				c.present, c.size = false, 0
				m.mu.Unlock()
				m.stage("evicted " + c.instance)
			}
		}
		unlockEntry(c)
	}
}

// Fetcher is one authorized fetch session of a task's bound instance
// (the node transfer route): wants with haves, the pack streamed to sink.
type Fetcher interface {
	Fetch(ctx context.Context, wants, haves []plumbing.Hash, sink func(io.Reader) error) error
}

// FetchStats is what one cache fetch did.
type FetchStats struct {
	// Hit reports that verified cache objects served as haves; Saved the
	// object bytes they spared the network; Fetched the received pack's
	// bytes; Copied the bytes copied into the task's database.
	Hit     bool
	Saved   int64
	Fetched int64
	Copied  int64
	Retried bool
	// Purged: a corrupt or foreign cached entry was discarded before the
	// fetch (which then went without haves).
	Purged bool
	// Bypassed: the closure exceeded the cache target and was not retained.
	Bypassed bool
}

// Fetch makes base's complete closure available in the task's trusted
// database dst: under the instance's entry lock, verified cached commits
// serve as haves of a fresh authorized fetch into a staging generation,
// the complete closure is verified across the staging and cached objects,
// the generation is synced and atomically replaces the cache's (unless the
// closure alone exceeds the target), and the closure is copied into dst
// before the lock is released. A corrupt cache entry is purged and one
// fresh fetch without haves is tried; a corrupt fresh response fails. A
// fetch error (denial, transport) is returned as is and never retried
// through the cache.
func (m *Manager) Fetch(ctx context.Context, name, instance string, base plumbing.Hash, f Fetcher, dst *workspacetransfer.TaskDB) (FetchStats, error) {
	var st FetchStats
	e := m.entryFor(instance)
	if err := lockEntry(ctx, e); err != nil {
		return st, err
	}
	defer unlockEntry(e)
	m.stage("fetch-locked " + instance)
	m.mu.Lock()
	disabled := e.disabled
	m.mu.Unlock()
	dir := filepath.Join(m.root, instance)
	var cur *workspacetransfer.TaskDB
	var meta cacheMeta
	if !disabled {
		cur, meta, st.Purged = m.openCurrent(ctx, e, dir, instance)
	}
	defer func() {
		if cur != nil {
			cur.Close()
		}
	}()
	haves, saved := m.verifiedHaves(ctx, cur, meta)
	if err := ctx.Err(); err != nil {
		return st, err
	}
	if cur != nil && haves == nil && len(meta.Commits) > 0 {
		// The cached closures did not verify: this disposable entry is
		// corrupt. Purge it; the fetch below goes without haves.
		cur.Close()
		cur = nil
		m.purge(e, dir)
		st.Purged = true
	}
	st.Hit, st.Saved = len(haves) > 0, saved
	stage, closure, fetched, err := m.fetchStage(ctx, dir, base, haves, cur, f)
	if err != nil && errors.Is(err, errCorruptResponse) && len(haves) > 0 {
		// The combined closure did not verify with cached haves: purge the
		// entry and try exactly one fresh authorized fetch without haves.
		st.Retried, st.Hit, st.Saved = true, false, 0
		if cur != nil {
			cur.Close()
			cur = nil
		}
		m.purge(e, dir)
		stage, closure, fetched, err = m.fetchStage(ctx, dir, base, nil, nil, f)
	}
	if err != nil {
		return st, err
	}
	st.Fetched = fetched
	defer func() {
		if stage != nil {
			p := stage.Dir()
			stage.Close()
			workspacetransfer.RemoveTree(p)
		}
	}()
	cp, err := dst.CopyFrom(ctx, stage.Store(), closure)
	if err != nil {
		return st, err
	}
	st.Copied = cp.Bytes
	size, err := workspacetransfer.LogicalSize(ctx, stage.Dir())
	if err != nil {
		return st, err
	}
	if size > m.target || disabled {
		st.Bypassed = true
		return st, nil
	}
	if err := m.replace(ctx, e, dir, name, instance, stage, base, size); err != nil {
		m.logger.Warn("workspace cache replacement failed; the task continues from its own copy", "instance", instance)
		return st, nil
	}
	stage = nil
	m.evict(ctx, instance)
	return st, nil
}

// errCorruptResponse marks a fetched closure that does not verify.
var errCorruptResponse = errors.New("the fetched closure is incomplete or corrupt")

// openCurrent opens e's current generation and identity, or nil; purged
// reports an existing unusable entry it discarded.
func (m *Manager) openCurrent(ctx context.Context, e *entry, dir, inst string) (*workspacetransfer.TaskDB, cacheMeta, bool) {
	meta, err := readMeta(filepath.Join(dir, cacheMetaName))
	if err != nil || meta.Instance != inst || meta.Origin != m.origin {
		if _, serr := os.Lstat(dir); serr == nil {
			m.purge(e, dir)
			return nil, cacheMeta{}, true
		}
		return nil, cacheMeta{}, false
	}
	db, err := workspacetransfer.OpenTaskDB(filepath.Join(dir, currentName))
	if err != nil {
		m.purge(e, dir)
		return nil, cacheMeta{}, true
	}
	return db, meta, false
}

// verifiedHaves returns the cached commits whose complete closures verify
// in cur (independently re-hashed before use) and their object bytes; nil
// when any fails (the entry is corrupt).
func (m *Manager) verifiedHaves(ctx context.Context, cur *workspacetransfer.TaskDB, meta cacheMeta) ([]plumbing.Hash, int64) {
	if cur == nil || len(meta.Commits) == 0 {
		return nil, 0
	}
	var haves []plumbing.Hash
	for _, c := range meta.Commits {
		haves = append(haves, plumbing.NewHash(c))
	}
	_, st, err := cur.Closure(ctx, haves)
	if err != nil {
		return nil, 0
	}
	return haves, st.Bytes
}

// purge removes e's entry directory (a corrupt disposable cache).
func (m *Manager) purge(e *entry, dir string) {
	if err := m.removeEntryDir(dir); err != nil {
		m.logger.Error("workspace cache entry cannot be removed safely; it is disabled", "instance", e.instance)
		m.mu.Lock()
		e.disabled = true
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	e.present, e.size = false, 0
	m.mu.Unlock()
}

// fetchStage fetches base into a fresh staging generation and completes
// it from cur's verified objects: the returned database holds base's whole
// verified closure.
func (m *Manager) fetchStage(ctx context.Context, dir string, base plumbing.Hash, haves []plumbing.Hash, cur *workspacetransfer.TaskDB,
	f Fetcher) (*workspacetransfer.TaskDB, []plumbing.Hash, int64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, 0, err
	}
	if err := m.check("cache-stage"); err != nil {
		return nil, nil, 0, err
	}
	stage, err := workspacetransfer.CreateTaskDB(filepath.Join(dir, stagePrefix+token()))
	if err != nil {
		return nil, nil, 0, err
	}
	fail := func(err error) (*workspacetransfer.TaskDB, []plumbing.Hash, int64, error) {
		p := stage.Dir()
		stage.Close()
		workspacetransfer.RemoveTree(p)
		return nil, nil, 0, err
	}
	var n int64
	err = f.Fetch(ctx, []plumbing.Hash{base}, haves, func(r io.Reader) error {
		cr := &countReader{r: r}
		err := stage.ReceivePack(ctx, cr)
		n = cr.n
		return err
	})
	if err != nil {
		return fail(err)
	}
	m.stage("fetched")
	var src storer.EncodedObjectStorer = stage.Store()
	if cur != nil {
		src = combined{stage.Store(), cur.Store()}
	}
	closure, _, err := workspacetransfer.ClosureOf(ctx, src, []plumbing.Hash{base})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return fail(cerr)
		}
		return fail(errCorruptResponse)
	}
	if cur != nil {
		var missing []plumbing.Hash
		for _, h := range closure {
			if !stage.Has(h) {
				missing = append(missing, h)
			}
		}
		if _, err := stage.CopyFrom(ctx, cur.Store(), missing); err != nil {
			return fail(err)
		}
	}
	return stage, closure, n, nil
}

// replace publishes stage as dir's current generation: the old one is
// renamed to a staging name (recognized at startup), stage renamed onto
// current, cache.json published, the old generation removed.
func (m *Manager) replace(ctx context.Context, e *entry, dir, name, inst string, stage *workspacetransfer.TaskDB, base plumbing.Hash, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.check("cache-replace"); err != nil {
		return err
	}
	p := stage.Dir()
	stage.Close()
	cur := filepath.Join(dir, currentName)
	old := filepath.Join(dir, stagePrefix+"old-"+token())
	if _, err := os.Lstat(cur); err == nil {
		if err := os.Rename(cur, old); err != nil {
			return err
		}
	}
	if err := os.Rename(p, cur); err != nil {
		return err
	}
	m.mu.Lock()
	m.seq++
	seq := m.seq
	m.mu.Unlock()
	meta := cacheMeta{Schema: cacheSchema, Name: name, Instance: inst, Origin: m.origin, LastUse: seq, Commits: []string{base.String()}}
	if err := m.writeMeta(dir, meta); err != nil {
		return err
	}
	if err := workspacetransfer.RemoveTree(old); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	m.mu.Lock()
	e.present, e.size, e.lastUse = true, size, seq
	m.mu.Unlock()
	m.stage("replaced " + inst)
	return nil
}

// combined reads from the staging database first, then the cache.
type combined struct {
	storer.EncodedObjectStorer
	other storer.EncodedObjectStorer
}

func (c combined) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	o, err := c.EncodedObjectStorer.EncodedObject(t, h)
	if err == nil || !errors.Is(err, plumbing.ErrObjectNotFound) {
		return o, err
	}
	return c.other.EncodedObject(t, h)
}

func (c combined) HasEncodedObject(h plumbing.Hash) error {
	if c.EncodedObjectStorer.HasEncodedObject(h) == nil {
		return nil
	}
	return c.other.HasEncodedObject(h)
}

// countReader counts bytes read.
type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
