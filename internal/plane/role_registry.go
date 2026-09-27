package plane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Role registry state (iteration 04).
const (
	// rolesName is the optional role directory, created lazily on the
	// first role publication.
	rolesName = "roles"
	// roleRegistryName is the single atomic registry document.
	roleRegistryName = "roles/registry.json"
	// RoleSchemaVersion is roles/registry.json's schema.
	RoleSchemaVersion = 1
	// maxRoleRegistry bounds a registry read (plus one sentinel byte).
	maxRoleRegistry = 2 << 20
)

// roleDoc is one immutable registry document: counters and every role
// sorted by registration order. exists reports that registry.json exists.
type roleDoc struct {
	exists    bool
	revision  int
	nextOrder int
	roles     []contract.RoleRecord
}

// emptyRoleDoc is the state without a registry file.
func emptyRoleDoc() *roleDoc { return &roleDoc{nextOrder: 1} }

// find returns the role with id and its index.
func (d *roleDoc) find(id string) (contract.RoleRecord, int, bool) {
	for i, r := range d.roles {
		if r.ID == id {
			return r, i, true
		}
	}
	return contract.RoleRecord{}, -1, false
}

// forNode returns the node's roles in registration order (a fresh slice
// of immutable records).
func (d *roleDoc) forNode(id string) []contract.RoleRecord {
	var out []contract.RoleRecord
	for _, r := range d.roles {
		if r.Node == id {
			out = append(out, r)
		}
	}
	return out
}

// snapshotFor is the node's full role snapshot at this revision.
func (d *roleDoc) snapshotFor(id string) roleSnap {
	return roleSnap{rev: d.revision, roles: d.forNode(id)}
}

// withAdded returns a new document with r appended at the next order.
func (d *roleDoc) withAdded(c contract.RoleConfig) (*roleDoc, contract.RoleRecord) {
	rec := contract.RoleRecord{RoleConfig: c.Resolved(), RegistrationOrder: d.nextOrder}
	n := &roleDoc{exists: true, revision: d.revision + 1, nextOrder: d.nextOrder + 1, roles: append(append([]contract.RoleRecord(nil), d.roles...), rec)}
	return n, rec
}

// withReplaced returns a new document with the role at i replaced by c,
// keeping its registration order.
func (d *roleDoc) withReplaced(i int, c contract.RoleConfig) (*roleDoc, contract.RoleRecord) {
	rec := contract.RoleRecord{RoleConfig: c.Resolved(), RegistrationOrder: d.roles[i].RegistrationOrder}
	roles := append([]contract.RoleRecord(nil), d.roles...)
	roles[i] = rec
	return &roleDoc{exists: true, revision: d.revision + 1, nextOrder: d.nextOrder, roles: roles}, rec
}

// withRemoved returns a new document without the role at i; counters are
// retained and the file is never deleted.
func (d *roleDoc) withRemoved(i int) *roleDoc {
	roles := append(append([]contract.RoleRecord(nil), d.roles[:i]...), d.roles[i+1:]...)
	return &roleDoc{exists: true, revision: d.revision + 1, nextOrder: d.nextOrder, roles: roles}
}

// registryFile is the ordered roles/registry.json schema.
type registryFile struct {
	SchemaVersion         int                   `json:"schema_version"`
	Revision              int                   `json:"revision"`
	NextRegistrationOrder int                   `json:"next_registration_order"`
	Roles                 []contract.RoleRecord `json:"roles"`
}

// encodeRoleDoc renders the exact writer bytes: two-space indentation,
// schema field order, roles sorted by registration order, one final LF,
// no HTML escaping.
func encodeRoleDoc(d *roleDoc) []byte {
	roles := d.roles
	if roles == nil {
		roles = []contract.RoleRecord{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(registryFile{SchemaVersion: RoleSchemaVersion, Revision: d.revision, NextRegistrationOrder: d.nextOrder, Roles: roles}); err != nil {
		panic(err) // unreachable: validated fixed-type fields
	}
	return b.Bytes()
}

// parseRoleDoc strictly decodes and validates a registry document: one
// object with exactly the four schema fields, schema 1, exact counters,
// at most MaxRoles valid resolved records (canonical timeouts, adapter
// and effort, encoded size) with unique IDs, strictly increasing orders
// below next_registration_order, and every node among nodes.
func parseRoleDoc(data []byte, lookup contract.AdapterLookup, nodes map[string]bool) (*roleDoc, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	raw := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("malformed JSON: %v", err)
		}
		key, _ := tok.(string)
		if _, dup := raw[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", contract.SafeText(key, 32))
		}
		switch key {
		case "schema_version", "revision", "next_registration_order", "roles":
		default:
			return nil, fmt.Errorf("unknown field %q", contract.SafeText(key, 32))
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("malformed JSON: %v", err)
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, fmt.Errorf("%q must not be null", key)
		}
		raw[key] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("malformed JSON: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON object")
	}
	for _, k := range []string{"schema_version", "revision", "next_registration_order", "roles"} {
		if _, ok := raw[k]; !ok {
			return nil, fmt.Errorf("%q is required", k)
		}
	}
	integer := func(key string, lo int) (int, error) {
		n, ok := contract.ParseInteger(string(bytes.TrimSpace(raw[key])))
		if !ok || n < lo || n > contract.MaxSafeInteger {
			return 0, fmt.Errorf("%q must be an integer from %d to %d", key, lo, contract.MaxSafeInteger)
		}
		return n, nil
	}
	if n, ok := contract.ParseInteger(string(bytes.TrimSpace(raw["schema_version"]))); !ok || n != RoleSchemaVersion {
		return nil, fmt.Errorf("unsupported schema_version %s (this build supports %d)", contract.SafeText(string(raw["schema_version"]), 32), RoleSchemaVersion)
	}
	d := &roleDoc{exists: true}
	var err error
	if d.revision, err = integer("revision", 1); err != nil {
		return nil, err
	}
	if d.nextOrder, err = integer("next_registration_order", 1); err != nil {
		return nil, err
	}
	var elems []json.RawMessage
	if r := bytes.TrimSpace(raw["roles"]); len(r) == 0 || r[0] != '[' || json.Unmarshal(r, &elems) != nil {
		return nil, errors.New(`"roles" must be an array`)
	}
	if len(elems) > contract.MaxRoles {
		return nil, fmt.Errorf("%d roles exceed the limit of %d", len(elems), contract.MaxRoles)
	}
	ids := map[string]bool{}
	for i, e := range elems {
		rec, err := contract.ParseRoleRecord(e, lookup)
		if err != nil {
			return nil, fmt.Errorf("role %d: %v", i+1, err)
		}
		if ids[rec.ID] {
			return nil, fmt.Errorf("role ID %q appears more than once", rec.ID)
		}
		ids[rec.ID] = true
		if i > 0 && d.roles[i-1].RegistrationOrder >= rec.RegistrationOrder {
			return nil, errors.New("roles are not sorted by unique registration_order")
		}
		if rec.RegistrationOrder >= d.nextOrder {
			return nil, fmt.Errorf("role %q has registration_order %d, not below next_registration_order %d", rec.ID, rec.RegistrationOrder, d.nextOrder)
		}
		if !nodes[rec.Node] {
			return nil, fmt.Errorf("role %q references node %s, which is not enrolled with this plane", rec.ID, rec.Node)
		}
		d.roles = append(d.roles, rec)
	}
	return d, nil
}

// scanRoles checks the optional roles/ directory without reading the
// registry: the directory itself (a real 0700 directory, never a symlink),
// registry.json (regular, not group/other writable) and regular .tmp-
// artifacts; every other entry, subdirectories included, is unexpected.
func (l layout) scanRoles() (present bool, unexpected []string, err error) {
	p := l.path(rolesName)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", p, err)
	}
	if err := checkDir(p, fi); err != nil {
		return true, nil, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return true, nil, wrapf(contract.CodeInternal, err, "cannot list %s: %v", p, err)
	}
	for _, e := range entries {
		ep := filepath.Join(p, e.Name())
		name := e.Name()
		if name != filepath.Base(roleRegistryName) && !strings.HasPrefix(name, tempPrefix) {
			unexpected = append(unexpected, ep)
			continue
		}
		fi, err := os.Lstat(ep)
		if err != nil {
			return true, nil, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", ep, err)
		}
		if fi.IsDir() {
			unexpected = append(unexpected, ep)
			continue
		}
		check := checkRegular
		if name == filepath.Base(roleRegistryName) {
			check = checkPublic
		}
		if err := check(ep, fi); err != nil {
			return true, nil, err
		}
	}
	return true, unexpected, nil
}

// loadRoleDoc reads and validates roles/registry.json (after scan admitted
// the layout) against the enrolled node IDs. A missing file or an empty
// roles/ directory is the empty registry. Stale temporaries are ignored:
// never promoted, deleted or counted. Invalid content is conflict and is
// never repaired.
func (l layout) loadRoleDoc(lookup contract.AdapterLookup, nodes map[string]bool) (*roleDoc, error) {
	p := l.path(roleRegistryName)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return emptyRoleDoc(), nil
	}
	if err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", p, err)
	}
	if err := checkPublic(p, fi); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot read %s: %v", p, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRoleRegistry+1))
	if err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot read %s: %v", p, err)
	}
	var d *roleDoc
	if len(b) > maxRoleRegistry {
		err = fmt.Errorf("file is larger than %d bytes", maxRoleRegistry)
	} else {
		d, err = parseRoleDoc(b, lookup, nodes)
	}
	if err != nil {
		return nil, wrapf(contract.CodeConflict, err, "invalid role registry %s: %v; the role registry is never repaired or regenerated automatically: stop the plane, then fix the file or restore a complete stopped backup of the plane state directory", p, err)
	}
	return d, nil
}

// loadRoles loads the role registry against the durable node records.
func (l layout) loadRoles(lookup contract.AdapterLookup) (*roleDoc, error) {
	recs, err := l.loadNodeRecords()
	if err != nil {
		return nil, err
	}
	nodes := map[string]bool{}
	for _, r := range recs {
		nodes[r.id] = true
	}
	return l.loadRoleDoc(lookup, nodes)
}

// roleState is one immutable view of the registry. visible is what reads
// report; confirmed is the last durably confirmed document, the only one
// ever distributed. blocked marks a publication whose directory sync is
// unconfirmed (visible != confirmed); blockedNode is the node it affected.
type roleState struct {
	visible, confirmed *roleDoc
	blocked            bool
	blockedNode        string
}

// roleRegistry is the plane's durable role registry. Its state pointer is
// replaced only by mutations, which the role service serializes behind
// its nonblocking gate; readers load it lock-free.
type roleRegistry struct {
	l      layout
	d      *deps
	lookup contract.AdapterLookup
	state  atomic.Pointer[roleState]
	// parentSynced records a successful sync of the state root after
	// roles/ existed (mutation path only, serialized by the gate).
	parentSynced bool
	// adopt, when set (by the node registry), installs a new state under
	// the node observation mutex together with the affected node's
	// desired snapshot, so that no reader sees one without the other. It
	// runs store, which only swaps the pointer: no I/O under that mutex.
	adopt func(next *roleState, node string, store func())
}

// install makes next the registry's state. node is the node whose
// snapshot the change affects ("" for none).
func (r *roleRegistry) install(next *roleState, node string) {
	store := func() { r.state.Store(next) }
	if r.adopt != nil {
		r.adopt(next, node, store)
		return
	}
	store()
}

func newRoleRegistry(l layout, d *deps, lookup contract.AdapterLookup, doc *roleDoc) *roleRegistry {
	r := &roleRegistry{l: l, d: d, lookup: lookup}
	r.state.Store(&roleState{visible: doc, confirmed: doc})
	return r
}

func (r *roleRegistry) load() *roleState { return r.state.Load() }

// resync retries the directory sync of a blocked registry. On success the
// visible document becomes confirmed and the affected node is returned for
// distribution.
func (r *roleRegistry) resync() (node string, resynced bool, err error) {
	s := r.load()
	if !s.blocked {
		return "", false, nil
	}
	if err := r.d.syncDir(r.l, rolesName); err != nil {
		return "", false, wrapf(contract.CodeInternal, err, "the previous role change is published but its durability is still unconfirmed (syncing %s failed: %v); nothing else was changed: retry after storage recovery", r.l.path(rolesName), err)
	}
	r.install(&roleState{visible: s.visible, confirmed: s.visible}, s.blockedNode)
	return s.blockedNode, true, nil
}

// ensureRolesDir makes roles/ (0700) durable before the first registry
// publication: it creates the directory when absent and then syncs the
// state root, on every attempt until one sync succeeds. A roles/ left by
// an attempt whose sync failed, or an empty one found at startup, does
// not prove its entry in the state directory durable. Once a registry
// document exists (loaded or published) or the parent sync succeeded,
// nothing is done.
func (r *roleRegistry) ensureRolesDir() error {
	if r.parentSynced || r.load().visible.exists {
		return nil
	}
	p := r.l.path(rolesName)
	if _, err := os.Lstat(p); err != nil {
		if err := r.d.hook("mkdir", rolesName); err != nil {
			return wrapf(contract.CodeInternal, err, "cannot create %s: %v; nothing was changed", p, err)
		}
		if err := os.Mkdir(p, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return wrapf(contract.CodeInternal, err, "cannot create %s: %v; nothing was changed", p, err)
		}
	}
	if err := r.d.syncDir(r.l, rootName); err != nil {
		return wrapf(contract.CodeInternal, err, "created %s but syncing the state directory failed: %v; nothing was changed, retry the role change", p, err)
	}
	r.parentSynced = true
	return nil
}

// prepare writes next's bytes to a synced, closed temporary sibling of the
// registry. On failure nothing is left behind and nothing changed.
func (r *roleRegistry) prepare(next *roleDoc) (string, error) {
	if err := r.ensureRolesDir(); err != nil {
		return "", err
	}
	tmp, err := r.d.writeTemp(r.l, roleRegistryName, encodeRoleDoc(next))
	if err != nil {
		return "", wrapf(contract.CodeInternal, err, "cannot write the role registry: %v; nothing was changed, retry the role change", err)
	}
	return tmp, nil
}

// errUnconfirmed marks a publication whose directory sync failed: it is
// visible and adopted, but its durability is unconfirmed.
var errUnconfirmed = errors.New("role registry durability unconfirmed")

// publish makes the prepared temporary the registry: a no-replace link
// for the first document, an atomic rename afterwards, then a sync of
// roles/. Before the name changes a failure leaves the old document
// authoritative (the temporary is removed). After it, a sync failure is
// ambiguous durability: the visible document is adopted, the registry is
// blocked, and errUnconfirmed is returned wrapped in an internal error.
func (r *roleRegistry) publish(tmp string, next *roleDoc, node string) error {
	prev := r.load()
	var err error
	if !prev.visible.exists {
		err = r.d.publishNew(r.l, tmp, roleRegistryName)
		if err != nil && errors.Is(err, fs.ErrExist) {
			err = fmt.Errorf("%s appeared unexpectedly", r.l.path(roleRegistryName))
		}
	} else {
		err = r.d.replaceRegistry(r.l, tmp)
	}
	if err != nil {
		return wrapf(contract.CodeInternal, err, "cannot publish the role registry: %v; nothing was changed, retry the role change", err)
	}
	if err := r.d.syncDir(r.l, rolesName); err != nil {
		r.install(&roleState{visible: next, confirmed: prev.confirmed, blocked: true, blockedNode: node}, node)
		return contract.Wrap(contract.CodeInternal, "role change was published but durability is unconfirmed; inspect role show and retry after storage recovery",
			errors.Join(errUnconfirmed, err))
	}
	r.install(&roleState{visible: next, confirmed: next}, node)
	return nil
}

// replaceRegistry atomically renames tmp over the registry.
func (d *deps) replaceRegistry(l layout, tmp string) error {
	if err := d.hook("rename", roleRegistryName); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, l.path(roleRegistryName)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// sortedViews orders records for the role list.
func sortedForList(recs []contract.RoleRecord) []contract.RoleRecord {
	out := append([]contract.RoleRecord(nil), recs...)
	sort.Slice(out, func(i, j int) bool { return contract.RoleListLess(out[i], out[j]) })
	return out
}
