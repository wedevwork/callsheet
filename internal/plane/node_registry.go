package plane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Node registry state and lease constants (iteration 03).
const (
	// nodesName is the optional managed node directory, created lazily on
	// the first enrollment.
	nodesName = "nodes"
	// NodeSchemaVersion is the nodes/<id>.json schema.
	NodeSchemaVersion = 1
	// maxNodeRecord bounds one node record.
	maxNodeRecord = 4 << 10

	heartbeatInterval    = contract.HeartbeatIntervalMS * time.Millisecond
	leaseDuration        = contract.LeaseMS * time.Millisecond
	firstHeartbeatWindow = 5 * time.Second
	sweepInterval        = time.Second
)

// nodeRel is the root-relative path of a node record.
func nodeRel(id string) string { return nodesName + "/" + id + ".json" }

// isNodeFile reports whether name is a canonical record file name.
func isNodeFile(name string) bool {
	id, ok := strings.CutSuffix(name, ".json")
	return ok && contract.ValidNodeID(id)
}

// nodeRecord is the ordered nodes/<id>.json schema: only stable
// registration is durable.
type nodeRecord struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	EnrolledAt    string `json:"enrolled_at"`
}

// encodeNodeRecord renders the exact writer bytes: two-space indentation,
// schema field order, one final LF.
func encodeNodeRecord(id string, enrolledAt time.Time) []byte {
	b, err := json.MarshalIndent(nodeRecord{SchemaVersion: NodeSchemaVersion, ID: id, EnrolledAt: enrolledAt.UTC().Format(time.RFC3339Nano)}, "", "  ")
	if err != nil {
		panic(err) // unreachable: three fixed-type fields
	}
	return append(b, '\n')
}

// parseNodeRecord strictly decodes one record: one object, no duplicate or
// unknown keys, no trailing JSON, schema 1, a canonical ID equal to the
// file's and a UTC RFC3339 enrollment time.
func parseNodeRecord(data []byte, wantID string) (time.Time, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return time.Time{}, errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	var version json.RawMessage
	var id, at string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return time.Time{}, fmt.Errorf("malformed JSON: %v", err)
		}
		key, _ := tok.(string)
		if seen[key] {
			return time.Time{}, fmt.Errorf("duplicate key %q", key)
		}
		seen[key] = true
		switch key {
		case "schema_version":
			err = dec.Decode(&version)
		case "id":
			err = dec.Decode(&id)
		case "enrolled_at":
			err = dec.Decode(&at)
		default:
			return time.Time{}, fmt.Errorf("unknown field %q", contract.SafeText(key, 32))
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid %q: %v", key, err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return time.Time{}, fmt.Errorf("malformed JSON: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return time.Time{}, errors.New("trailing data after the JSON object")
	}
	if !seen["schema_version"] || !seen["id"] || !seen["enrolled_at"] {
		return time.Time{}, errors.New(`"schema_version", "id" and "enrolled_at" are required`)
	}
	if n, ok := contract.ParseInteger(string(version)); !ok || n != NodeSchemaVersion {
		return time.Time{}, fmt.Errorf("unsupported schema_version %s (this build supports %d)", contract.SafeText(string(version), 32), NodeSchemaVersion)
	}
	if !contract.ValidNodeID(id) {
		return time.Time{}, errors.New("id is not a valid node ID")
	}
	if id != wantID {
		return time.Time{}, errors.New("id does not match the file name")
	}
	t, ok := contract.ParseTime(at)
	if !ok {
		return time.Time{}, errors.New("enrolled_at is not a UTC RFC3339 timestamp")
	}
	return t, nil
}

// scanNodes checks the optional nodes/ directory without reading record
// contents: the directory itself, canonical record files (regular, not
// group/other writable) and regular .tmp- artifacts; every other entry,
// nested directories included, is unexpected.
func (l layout) scanNodes() (present bool, unexpected []string, err error) {
	p := l.path(nodesName)
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
		switch {
		case isNodeFile(e.Name()):
			fi, err := os.Lstat(ep)
			if err != nil {
				return true, nil, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", ep, err)
			}
			if fi.IsDir() {
				unexpected = append(unexpected, ep)
				continue
			}
			if err := checkPublic(ep, fi); err != nil {
				return true, nil, err
			}
		case strings.HasPrefix(e.Name(), tempPrefix):
			fi, err := os.Lstat(ep)
			if err != nil {
				return true, nil, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", ep, err)
			}
			if fi.IsDir() {
				unexpected = append(unexpected, ep)
				continue
			}
			if err := checkRegular(ep, fi); err != nil {
				return true, nil, err
			}
		default:
			unexpected = append(unexpected, ep)
		}
	}
	return true, unexpected, nil
}

// storedNode is one validated durable record.
type storedNode struct {
	id         string
	enrolledAt time.Time
}

// loadNodeRecords reads and validates every record (after scan admitted
// the layout), sorted by ID. Invalid records are conflict and are never
// deleted or regenerated.
func (l layout) loadNodeRecords() ([]storedNode, error) {
	p := l.path(nodesName)
	entries, err := os.ReadDir(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot list %s: %v", p, err)
	}
	var out []storedNode
	for _, e := range entries {
		if !isNodeFile(e.Name()) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		t, err := l.readNodeRecord(id)
		if err != nil {
			return nil, err
		}
		out = append(out, storedNode{id: id, enrolledAt: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// readNodeRecord validates one record file on disk.
func (l layout) readNodeRecord(id string) (time.Time, error) {
	p := l.path(nodeRel(id))
	fi, err := os.Lstat(p)
	if err != nil {
		return time.Time{}, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", p, err)
	}
	if err := checkPublic(p, fi); err != nil {
		return time.Time{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return time.Time{}, wrapf(contract.CodeInternal, err, "cannot read %s: %v", p, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxNodeRecord+1))
	if err != nil {
		return time.Time{}, wrapf(contract.CodeInternal, err, "cannot read %s: %v", p, err)
	}
	var t time.Time
	if len(b) > maxNodeRecord {
		err = fmt.Errorf("file is larger than %d bytes", maxNodeRecord)
	} else {
		t, err = parseNodeRecord(b, id)
	}
	if err != nil {
		return time.Time{}, wrapf(contract.CodeConflict, err, "invalid node record %s: %v; node state is never deleted or regenerated automatically: stop the plane, then fix or move the file, or restore a complete stopped backup", p, err)
	}
	return t, nil
}

// nodeRegistry is the plane's node roster: durable registrations plus
// runtime observations (last seen, observed versions, lease, attachment).
// regMu serializes enrollment publication (and its fsyncs); mu guards
// observations and is never held during file I/O, encoding or callbacks.
type nodeRegistry struct {
	l     layout
	clock nodeClock
	d     *deps

	regMu sync.Mutex

	mu     sync.Mutex
	nodes  map[string]*nodeState
	gen    uint64
	closed bool
}

// nodeState is one known node.
type nodeState struct {
	id         string
	enrolledAt time.Time
	// synced is false while a published record's directory sync is
	// unconfirmed; an idempotent retry reattempts it.
	synced bool

	lastSeen        *time.Time
	protocolVersion *int
	softwareVersion *string
	online          bool
	// deadline is the monotonic lease end (instant of the last valid
	// heartbeat plus the lease); it is never serialized.
	deadline time.Time
	att      *attachment
}

// attachment is the node's current stream. Only the attachment with the
// current generation can refresh the lease or clear itself.
type attachment struct {
	gen           uint64
	closeConn     func()
	firstDeadline time.Time
	heartbeated   bool
	sw            string
	pv            int
}

// loadNodeRegistry loads and validates every durable record; all nodes
// start offline with null observations and no lease.
func loadNodeRegistry(l layout, clock nodeClock) (*nodeRegistry, error) {
	recs, err := l.loadNodeRecords()
	if err != nil {
		return nil, err
	}
	r := &nodeRegistry{l: l, clock: clock, nodes: map[string]*nodeState{}}
	for _, rec := range recs {
		r.nodes[rec.id] = &nodeState{id: rec.id, enrolledAt: rec.enrolledAt, synced: true}
	}
	return r, nil
}

func (r *nodeRegistry) deps() *deps {
	if r.d == nil {
		r.d = defaultDeps()
	}
	return r.d
}

// Enroll registers id durably and idempotently. For a new ID it publishes
// nodes/<id>.json (write, sync, close, no-replace link, directory sync)
// before inserting it into memory; created reports that. For a known ID it
// revalidates the durable record, completes an unconfirmed directory sync,
// and returns the current snapshot without rewriting anything.
func (r *nodeRegistry) Enroll(ctx context.Context, id string) (contract.Node, bool, error) {
	if !contract.ValidNodeID(id) {
		return contract.Node{}, false, errf(contract.CodeInvalidArgument, "invalid node ID")
	}
	r.regMu.Lock()
	defer r.regMu.Unlock()
	if err := ctx.Err(); err != nil {
		return contract.Node{}, false, wrapf(contract.CodeUnavailable, err, "enrollment canceled: %v", err)
	}
	r.mu.Lock()
	st, closed := r.nodes[id], r.closed
	var synced bool
	if st != nil {
		synced = st.synced
	}
	r.mu.Unlock()
	if closed {
		return contract.Node{}, false, errf(contract.CodeUnavailable, "the plane is shutting down")
	}
	d := r.deps()
	if st != nil {
		if _, err := r.l.readNodeRecord(id); err != nil {
			return contract.Node{}, false, err
		}
		if !synced {
			if err := d.syncDir(r.l, nodesName); err != nil {
				return contract.Node{}, false, r.unconfirmed(id, err)
			}
			r.mu.Lock()
			st.synced = true
			r.mu.Unlock()
		}
		n, err := r.Show(id)
		return n, false, err
	}
	if err := r.ensureNodesDir(d); err != nil {
		return contract.Node{}, false, err
	}
	rel := nodeRel(id)
	enrolledAt := r.clock.Now().UTC()
	tmp, err := d.writeTemp(r.l, rel, encodeNodeRecord(id, enrolledAt))
	if err != nil {
		return contract.Node{}, false, wrapf(contract.CodeInternal, err, "cannot register node %s: writing %s failed: %v; nothing was registered, retry the enrollment", id, rel, err)
	}
	if err := d.publishNew(r.l, tmp, rel); err != nil {
		return contract.Node{}, false, wrapf(contract.CodeInternal, err, "cannot register node %s: publishing %s failed: %v; nothing was registered, retry the enrollment", id, rel, err)
	}
	st = &nodeState{id: id, enrolledAt: enrolledAt}
	r.mu.Lock()
	r.nodes[id] = st
	r.mu.Unlock()
	if err := d.syncDir(r.l, nodesName); err != nil {
		return contract.Node{}, false, r.unconfirmed(id, err)
	}
	r.mu.Lock()
	st.synced = true
	r.mu.Unlock()
	n, err := r.Show(id)
	return n, true, err
}

func (r *nodeRegistry) unconfirmed(id string, err error) error {
	return wrapf(contract.CodeInternal, err, "node %s is registered but syncing %s failed: %v; its durability is not confirmed: retry the enrollment, which confirms it (the record is kept and never replaced)", id, r.l.path(nodesName), err)
}

// ensureNodesDir creates nodes/ (0700) and syncs the root when absent.
func (r *nodeRegistry) ensureNodesDir(d *deps) error {
	p := r.l.path(nodesName)
	if _, err := os.Lstat(p); err == nil {
		return nil
	}
	if err := d.hook("mkdir", nodesName); err != nil {
		return wrapf(contract.CodeInternal, err, "cannot create %s: %v; nothing was registered", p, err)
	}
	if err := os.Mkdir(p, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return wrapf(contract.CodeInternal, err, "cannot create %s: %v; nothing was registered", p, err)
	}
	if err := d.syncDir(r.l, rootName); err != nil {
		return wrapf(contract.CodeInternal, err, "created %s but syncing the state directory failed: %v; nothing was registered, retry the enrollment", p, err)
	}
	return nil
}

// expireLocked samples nothing itself: at now it marks every lease with
// now >= deadline offline and evicts attachments whose lease expired or
// whose first-heartbeat window ended, except the attachment with
// generation keep. It returns the close callbacks to run outside mu.
func (r *nodeRegistry) expireLocked(now time.Time, keep uint64) []func() {
	var closers []func()
	for _, st := range r.nodes {
		if st.online && !now.Before(st.deadline) {
			st.online = false
		}
		a := st.att
		if a == nil || a.gen == keep {
			continue
		}
		if (!a.heartbeated && !now.Before(a.firstDeadline)) || (a.heartbeated && !now.Before(st.deadline)) {
			st.att = nil
			if a.closeConn != nil {
				closers = append(closers, a.closeConn)
			}
		}
	}
	return closers
}

func runAll(fs []func()) {
	for _, f := range fs {
		f()
	}
}

// Attach installs the node's stream after a valid hello: the node must be
// known and have no current attachment (a live stream, or one still in its
// first-heartbeat window). It returns the attachment's generation. Hello
// never makes a node online.
func (r *nodeRegistry) Attach(id, softwareVersion string, protocolVersion int, closeConn func()) (uint64, error) {
	r.mu.Lock()
	now := r.clock.Now()
	closers := r.expireLocked(now, 0)
	gen, err := r.attachLocked(now, id, softwareVersion, protocolVersion, closeConn)
	r.mu.Unlock()
	runAll(closers)
	return gen, err
}

func (r *nodeRegistry) attachLocked(now time.Time, id, sw string, pv int, closeConn func()) (uint64, error) {
	if r.closed {
		return 0, errf(contract.CodeUnavailable, "the plane is shutting down")
	}
	st := r.nodes[id]
	if st == nil {
		return 0, errf(contract.CodeNotFound, "node %s is not enrolled with this plane; run callsheet sidecar enroll", id)
	}
	if st.att != nil {
		return 0, errf(contract.CodeConflict, "node %s already has an active stream", id)
	}
	r.gen++
	st.att = &attachment{gen: r.gen, closeConn: closeConn, firstDeadline: now.Add(firstHeartbeatWindow), sw: sw, pv: pv}
	return r.gen, nil
}

// Heartbeat refreshes the lease for the current attachment only: under mu
// it samples one instant, first expires (an exactly expired lease goes
// offline), then sets deadline = instant + lease, last_seen = the same
// instant in UTC, the observed versions, and online.
func (r *nodeRegistry) Heartbeat(id string, generation uint64, roles []contract.RoleStatus) error {
	if len(roles) != 0 {
		return errf(contract.CodeInvalidArgument, "roles must be empty in protocol %d", contract.ProtocolVersion)
	}
	r.mu.Lock()
	now := r.clock.Now()
	closers := r.expireLocked(now, generation)
	st := r.nodes[id]
	var err error
	if st == nil || st.att == nil || st.att.gen != generation {
		err = errf(contract.CodeUnavailable, "the stream for node %s is no longer current; reconnect", id)
	} else {
		a := st.att
		seen := now.UTC()
		pv, sw := a.pv, a.sw
		st.deadline = now.Add(leaseDuration)
		st.lastSeen, st.protocolVersion, st.softwareVersion = &seen, &pv, &sw
		st.online = true
		a.heartbeated = true
	}
	r.mu.Unlock()
	runAll(closers)
	return err
}

// Detach clears the node's attachment if generation is still current. The
// lease is unchanged: a closed socket alone never makes a node offline.
func (r *nodeRegistry) Detach(id string, generation uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.nodes[id]; st != nil && st.att != nil && st.att.gen == generation {
		st.att = nil
	}
}

func (st *nodeState) snapshot() contract.Node {
	n := contract.Node{ID: st.id, Liveness: contract.LivenessOffline, Roles: []contract.RoleStatus{}}
	if st.online {
		n.Liveness = contract.LivenessOnline
	}
	if st.lastSeen != nil {
		t := *st.lastSeen
		n.LastSeen = &t
	}
	if st.protocolVersion != nil {
		v := *st.protocolVersion
		n.ProtocolVersion = &v
	}
	if st.softwareVersion != nil {
		v := *st.softwareVersion
		n.SoftwareVersion = &v
	}
	return n
}

// Snapshot samples the clock once, expires under mu and returns copies of
// every node sorted by ID.
func (r *nodeRegistry) Snapshot() []contract.Node {
	r.mu.Lock()
	closers := r.expireLocked(r.clock.Now(), 0)
	out := make([]contract.Node, 0, len(r.nodes))
	for _, st := range r.nodes {
		out = append(out, st.snapshot())
	}
	r.mu.Unlock()
	runAll(closers)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Show is Snapshot for one node; an unknown ID is not_found.
func (r *nodeRegistry) Show(id string) (contract.Node, error) {
	r.mu.Lock()
	closers := r.expireLocked(r.clock.Now(), 0)
	st := r.nodes[id]
	var n contract.Node
	if st != nil {
		n = st.snapshot()
	}
	r.mu.Unlock()
	runAll(closers)
	if st == nil {
		return contract.Node{}, errf(contract.CodeNotFound, "node %s is not known to this plane", id)
	}
	return n, nil
}

// Expire is the sweep: it expires leases and evicts stale attachments.
func (r *nodeRegistry) Expire() {
	r.mu.Lock()
	closers := r.expireLocked(r.clock.Now(), 0)
	r.mu.Unlock()
	runAll(closers)
}

// Close stops admission and closes every attachment outside the lock. The
// plane's Run joins the stream handlers and the sweep afterwards.
func (r *nodeRegistry) Close() {
	r.mu.Lock()
	r.closed = true
	var closers []func()
	for _, st := range r.nodes {
		if st.att != nil {
			if st.att.closeConn != nil {
				closers = append(closers, st.att.closeConn)
			}
			st.att = nil
		}
	}
	r.mu.Unlock()
	runAll(closers)
}
