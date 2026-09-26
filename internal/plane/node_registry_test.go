package plane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Node IDs used by the plane tests, in sorted order.
const (
	idA = "n_0000000000000000000000000000000a"
	idB = "n_0000000000000000000000000000000b"
	idC = "n_0000000000000000000000000000000c"
)

// newRegistry returns an empty registry on a fresh (base-less) root with a
// fake clock at t0.
func newRegistry(t testing.TB) (*nodeRegistry, *testkit.FakeClock) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	clk := testkit.NewFakeClock(t0)
	r, err := loadNodeRegistry(layout{root: root}, clk)
	if err != nil {
		t.Fatal(err)
	}
	r.d = testDeps(t)
	return r, clk
}

func mustEnroll(t testing.TB, r *nodeRegistry, id string) contract.Node {
	t.Helper()
	n, _, err := r.Enroll(bg, id)
	if err != nil {
		t.Fatalf("enroll %s: %v", id, err)
	}
	return n
}

func show(t *testing.T, r *nodeRegistry, id string) contract.Node {
	t.Helper()
	n, err := r.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// closeRecorder counts attachment close callbacks and signals the first
// close of each name on a channel.
type closeRecorder struct {
	mu     sync.Mutex
	n      map[string]int
	closed map[string]chan struct{}
}

func (c *closeRecorder) ch(name string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chLocked(name)
}

func (c *closeRecorder) chLocked(name string) chan struct{} {
	if c.closed == nil {
		c.closed = map[string]chan struct{}{}
	}
	if c.closed[name] == nil {
		c.closed[name] = make(chan struct{})
	}
	return c.closed[name]
}

func (c *closeRecorder) fn(name string) func() {
	ch := c.ch(name)
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.n == nil {
			c.n = map[string]int{}
		}
		c.n[name]++
		if c.n[name] == 1 {
			close(ch)
		}
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func (c *closeRecorder) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[name]
}

func TestNodeRecordCodec(t *testing.T) {
	at := time.Date(2026, 9, 26, 12, 0, 0, 123, time.FixedZone("x", 7200))
	b := encodeNodeRecord(idA, at)
	want := "{\n  \"schema_version\": 1,\n  \"id\": \"" + idA + "\",\n  \"enrolled_at\": \"2026-09-26T10:00:00.000000123Z\"\n}\n"
	if string(b) != want {
		t.Fatalf("record = %q", b)
	}
	got, err := parseNodeRecord(b, idA)
	if err != nil || !got.Equal(at) {
		t.Fatalf("parse = %v %v", got, err)
	}
	good := `{"schema_version":1,"id":"` + idA + `","enrolled_at":"2026-09-26T10:00:00Z"}`
	if _, err := parseNodeRecord([]byte(good), idA); err != nil {
		t.Fatal(err)
	}
	for in, msg := range map[string]string{
		`[]`:                                    "not a JSON object",
		good + `{}`:                             "trailing data",
		strings.Replace(good, `"id"`, `"x"`, 1): `unknown field "x"`,
		`{"schema_version":1,"schema_version":1}`:                                        "duplicate key",
		`{"schema_version":2,"id":"` + idA + `","enrolled_at":"2026-09-26T10:00:00Z"}`:   "unsupported schema_version 2",
		`{"schema_version":"1","id":"` + idA + `","enrolled_at":"2026-09-26T10:00:00Z"}`: "unsupported schema_version",
		`{"schema_version":1,"id":"n_1","enrolled_at":"2026-09-26T10:00:00Z"}`:           "not a valid node ID",
		`{"schema_version":1,"id":"` + idB + `","enrolled_at":"2026-09-26T10:00:00Z"}`:   "does not match the file name",
		`{"schema_version":1,"id":"` + idA + `","enrolled_at":"yesterday"}`:              "not a UTC RFC3339",
		`{"schema_version":1,"id":"` + idA + `"}`:                                        "are required",
		`{"schema_version":1,"id":7,"enrolled_at":"x"}`:                                  `invalid "id"`,
		`{"schema_version":1,`:      "malformed JSON",
		`{"schema_version":1 "id"}`: "malformed JSON",
		`{"schema_version":1,"id":"` + idA + `","enrolled_at":"2026-09-26T10:00:00Z"`: "malformed JSON",
	} {
		if _, err := parseNodeRecord([]byte(in), idA); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("parse(%q) = %v, want %q", in, err, msg)
		}
	}
	if !isNodeFile(idA+".json") || isNodeFile(idA) || isNodeFile("n_1.json") || isNodeFile(idA+".json.bak") {
		t.Fatal("isNodeFile")
	}
}

// TestLeaseAlgorithm is UT-Lease: empty and first heartbeat, 14.999 s
// online and 15 s offline, reads before a delayed sweep, disconnect
// retention, exact-boundary ordering, and stale generations.
func TestLeaseAlgorithm(t *testing.T) {
	r, clk := newRegistry(t)
	mustEnroll(t, r, idA)
	// Enrollment and hello alone never make a node online.
	n := show(t, r, idA)
	if n.Liveness != contract.LivenessOffline || n.LastSeen != nil || n.ProtocolVersion != nil || n.SoftwareVersion != nil || len(n.Roles) != 0 || n.Roles == nil {
		t.Fatalf("enrolled = %+v", n)
	}
	var closes closeRecorder
	gen, err := r.Attach(idA, "v1.2", 1, closes.fn("a1"))
	if err != nil {
		t.Fatal(err)
	}
	if n := show(t, r, idA); n.Liveness != contract.LivenessOffline || n.LastSeen != nil {
		t.Fatalf("after hello = %+v", n)
	}
	// First heartbeat: online, last_seen = the heartbeat instant in UTC,
	// observed versions from this connection.
	clk.Advance(time.Second)
	hbAt := clk.Now()
	if err := r.Heartbeat(idA, gen, nil); err != nil {
		t.Fatal(err)
	}
	n = show(t, r, idA)
	if n.Liveness != contract.LivenessOnline || !n.LastSeen.Equal(hbAt) || n.LastSeen.Location() != time.UTC || *n.ProtocolVersion != 1 || *n.SoftwareVersion != "v1.2" {
		t.Fatalf("after heartbeat = %+v", n)
	}
	// 14.999 s later still online; exactly 15 s offline, observed with no
	// sweep at all (reads expire under the same mutex).
	clk.Advance(leaseDuration - time.Millisecond)
	if show(t, r, idA).Liveness != contract.LivenessOnline {
		t.Fatal("offline before 15 s")
	}
	clk.Advance(time.Millisecond)
	n = show(t, r, idA)
	if n.Liveness != contract.LivenessOffline || !n.LastSeen.Equal(hbAt) || *n.SoftwareVersion != "v1.2" {
		t.Fatalf("at 15 s = %+v", n)
	}
	// The expired attachment was evicted and closed by the read itself.
	if closes.count("a1") != 1 {
		t.Fatalf("expired attachment closes = %d", closes.count("a1"))
	}
	if err := r.Heartbeat(idA, gen, nil); !contract.IsCode(err, contract.CodeUnavailable) {
		t.Fatalf("heartbeat of an evicted attachment = %v", err)
	}
	r.Detach(idA, gen) // a late cleanup of the evicted attachment is harmless
	// A new stream restores online without changing identity.
	gen2, err := r.Attach(idA, "v2", 1, closes.fn("a2"))
	if err != nil || gen2 <= gen {
		t.Fatalf("reattach = %d %v", gen2, err)
	}
	if err := r.Heartbeat(idA, gen2, []contract.RoleStatus{}); err != nil {
		t.Fatal(err)
	}
	if n := show(t, r, idA); n.Liveness != contract.LivenessOnline || n.ID != idA || *n.SoftwareVersion != "v2" {
		t.Fatalf("returned = %+v", n)
	}
	// Disconnect retains the lease until its deadline; a replacement may
	// attach immediately and the old generation cannot touch it.
	r.Detach(idA, gen2)
	clk.Advance(10 * time.Second)
	if show(t, r, idA).Liveness != contract.LivenessOnline {
		t.Fatal("socket close made the node offline")
	}
	gen3, err := r.Attach(idA, "v3", 1, closes.fn("a3"))
	if err != nil {
		t.Fatalf("replacement after close: %v", err)
	}
	r.Detach(idA, gen2)
	if err := r.Heartbeat(idA, gen2, nil); !contract.IsCode(err, contract.CodeUnavailable) {
		t.Fatalf("stale generation refreshed: %v", err)
	}
	if _, err := r.Attach(idA, "v4", 1, nil); !contract.IsCode(err, contract.CodeConflict) {
		t.Fatalf("second live stream = %v", err)
	}
	// Exact boundary: a heartbeat arriving exactly at the first-heartbeat
	// deadline of a still-current attachment first expires, then renews.
	clk.Advance(firstHeartbeatWindow)
	if err := r.Heartbeat(idA, gen3, nil); err != nil {
		t.Fatalf("heartbeat at the exact window end: %v", err)
	}
	// Exact lease boundary with the attachment current: offline for an
	// instant is never observable; the renewal wins in one locked step.
	clk.Advance(leaseDuration)
	if err := r.Heartbeat(idA, gen3, nil); err != nil {
		t.Fatalf("heartbeat at the exact lease end: %v", err)
	}
	if n := show(t, r, idA); n.Liveness != contract.LivenessOnline || !n.LastSeen.Equal(clk.Now()) {
		t.Fatalf("after boundary renewal = %+v", n)
	}
	// Once the sweep evicted it, reconnect is required.
	clk.Advance(leaseDuration)
	r.Expire()
	if err := r.Heartbeat(idA, gen3, nil); !contract.IsCode(err, contract.CodeUnavailable) || closes.count("a3") != 1 {
		t.Fatalf("heartbeat after sweep eviction = %v (closes %d)", err, closes.count("a3"))
	}
	// Nonempty roles are rejected, never silently accepted.
	if err := r.Heartbeat(idA, gen3, []contract.RoleStatus{{RoleID: "x"}}); !contract.IsCode(err, contract.CodeInvalidArgument) {
		t.Fatalf("roles = %v", err)
	}
	// Unknown nodes cannot attach, and no read creates them.
	if _, err := r.Attach(idB, "v", 1, nil); !contract.IsCode(err, contract.CodeNotFound) {
		t.Fatalf("unknown attach = %v", err)
	}
	if _, err := r.Show(idB); !contract.IsCode(err, contract.CodeNotFound) {
		t.Fatal("unknown show")
	}
}

// TestLeaseFirstHeartbeatWindow: an attachment without a heartbeat blocks
// a duplicate for 5 s, then is evicted by the sweep and closed once.
func TestLeaseFirstHeartbeatWindow(t *testing.T) {
	r, clk := newRegistry(t)
	mustEnroll(t, r, idA)
	var closes closeRecorder
	if _, err := r.Attach(idA, "v", 1, closes.fn("first")); err != nil {
		t.Fatal(err)
	}
	clk.Advance(firstHeartbeatWindow - time.Nanosecond)
	if _, err := r.Attach(idA, "v", 1, nil); !contract.IsCode(err, contract.CodeConflict) {
		t.Fatalf("duplicate inside the window = %v", err)
	}
	clk.Advance(time.Nanosecond)
	r.Expire()
	if closes.count("first") != 1 {
		t.Fatal("window end did not evict")
	}
	if _, err := r.Attach(idA, "v", 1, nil); err != nil {
		t.Fatalf("attach after eviction: %v", err)
	}
	if n := show(t, r, idA); n.Liveness != contract.LivenessOffline || n.LastSeen != nil {
		t.Fatalf("no heartbeat yet = %+v", n)
	}
}

// TestLeaseWallClock: the lease compares instants only. Production
// instants carry a monotonic reading that the stored deadline keeps and
// the displayed last_seen drops; a wall-clock step on the fake clock moves
// last_seen (even backward) and never extends an expired lease's
// deadline arithmetic beyond the instants compared.
func TestLeaseWallClock(t *testing.T) {
	root := t.TempDir()
	r, err := loadNodeRegistry(layout{root: root}, realClock{})
	if err != nil {
		t.Fatal(err)
	}
	r.d = testDeps(t)
	mustEnroll(t, r, idA)
	gen, _ := r.Attach(idA, "v", 1, nil)
	if err := r.Heartbeat(idA, gen, nil); err != nil {
		t.Fatal(err)
	}
	st := r.nodes[idA]
	if !strings.Contains(st.deadline.String(), " m=") || strings.Contains(st.lastSeen.String(), " m=") || st.lastSeen.Location() != time.UTC {
		t.Fatalf("deadline %s must keep, last_seen %s must drop the monotonic reading", st.deadline, st.lastSeen)
	}
	// A wall step backward on the fake clock: last_seen moves backward.
	r2, clk := newRegistry(t)
	mustEnroll(t, r2, idA)
	g, _ := r2.Attach(idA, "v", 1, nil)
	r2.Heartbeat(idA, g, nil)
	first := *show(t, r2, idA).LastSeen
	clk.Set(t0.Add(-time.Hour))
	r2.Heartbeat(idA, g, nil)
	if second := *show(t, r2, idA).LastSeen; !second.Before(first) {
		t.Fatalf("last_seen %v did not move backward from %v", second, first)
	}
}

// TestLeaseSweepTicker: the service's sweep runs Expire on the node
// clock's one-second ticker and is joined by shutdown.
func TestLeaseSweepTicker(t *testing.T) {
	r, clk := newRegistry(t)
	mustEnroll(t, r, idA)
	var closes closeRecorder
	r.Attach(idA, "v", 1, closes.fn("a"))
	svc := newNodeService(r, clk, discardLogger(), nil, time.Second)
	svc.startSweep()
	if err := clk.AwaitWaiter(20*time.Second, testkit.HasTicker(sweepInterval)); err != nil {
		t.Fatal(err)
	}
	clk.Advance(firstHeartbeatWindow)
	select {
	case <-closes.ch("a"):
	case <-time.After(20 * time.Second):
		t.Fatal("sweep did not evict the expired attachment")
	}
	svc.shutdown(time.Now().Add(5 * time.Second))
	if len(clk.Waiters()) != 0 {
		t.Fatalf("sweep ticker leaked: %v", clk.Waiters())
	}
}

// TestRegistryEnrollment is UT-Registry's publication part: lazy nodes/
// creation, exact bytes and modes, idempotency without rewrite, concurrent
// enrollment of one ID, and every publication failure with its retry.
func TestRegistryEnrollment(t *testing.T) {
	r, clk := newRegistry(t)
	if _, err := os.Lstat(r.l.path(nodesName)); !os.IsNotExist(err) {
		t.Fatal("nodes/ created before the first enrollment")
	}
	n, created, err := r.Enroll(bg, idA)
	if err != nil || !created || n.ID != idA || n.Liveness != contract.LivenessOffline {
		t.Fatalf("first enroll = %+v %v %v", n, created, err)
	}
	p := r.l.path(nodeRel(idA))
	b, _ := os.ReadFile(p)
	if string(b) != string(encodeNodeRecord(idA, t0)) {
		t.Fatalf("record bytes %q", b)
	}
	for path, mode := range map[string]os.FileMode{r.l.path(nodesName): fs.ModeDir | 0o700, p: 0o600} {
		if fi, err := os.Lstat(path); err != nil || fi.Mode() != mode {
			t.Fatalf("%s mode %v %v", path, fi.Mode(), err)
		}
	}
	info, _ := os.Stat(p)
	clk.Advance(time.Hour)
	if _, created, err := r.Enroll(bg, idA); err != nil || created {
		t.Fatalf("repeat = %v %v", created, err)
	}
	if info2, _ := os.Stat(p); !os.SameFile(info, info2) || !info2.ModTime().Equal(info.ModTime()) {
		t.Fatal("repeat enrollment rewrote the record")
	}
	if b2, _ := os.ReadFile(p); !bytes.Equal(b, b2) {
		t.Fatal("record changed")
	}
	// Concurrent enrollment of one new ID: exactly one creation.
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, c, err := r.Enroll(bg, idB)
			if err != nil {
				t.Error(err)
			}
			results <- c
		}()
	}
	wg.Wait()
	close(results)
	createdN := 0
	for c := range results {
		if c {
			createdN++
		}
	}
	if createdN != 1 {
		t.Fatalf("concurrent creations = %d", createdN)
	}
	if left := tempLeftovers(t, r.l.root); len(left) != 0 {
		t.Fatalf("temporaries left: %v", left)
	}
	if _, _, err := r.Enroll(bg, "n_1"); !contract.IsCode(err, contract.CodeInvalidArgument) {
		t.Fatalf("invalid ID = %v", err)
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, _, err := r.Enroll(ctx, idC); !contract.IsCode(err, contract.CodeUnavailable) {
		t.Fatalf("canceled = %v", err)
	}
	// A known ID whose record was corrupted on disk is refused, not
	// rewritten.
	os.WriteFile(p, []byte("{"), 0o600)
	if _, _, err := r.Enroll(bg, idA); !contract.IsCode(err, contract.CodeConflict) {
		t.Fatalf("corrupt known record = %v", err)
	}
	r.Close()
	if _, _, err := r.Enroll(bg, idC); !contract.IsCode(err, contract.CodeUnavailable) {
		t.Fatalf("after close = %v", err)
	}
}

// TestRegistryPublicationFailures injects a failure at every boundary.
func TestRegistryPublicationFailures(t *testing.T) {
	rel := nodeRel(idA)
	for _, c := range []struct {
		op, name string
		visible  bool // the record is visible (published) after the failure
	}{
		{"mkdir", nodesName, false}, {"dirsync", rootName, false},
		{"create", rel, false}, {"write", rel, false}, {"sync", rel, false}, {"close", rel, false}, {"publish", rel, false},
		{"dirsync", nodesName, true},
	} {
		t.Run(c.op+"-"+strings.ReplaceAll(c.name, "/", "_"), func(t *testing.T) {
			r, _ := newRegistry(t)
			r.d.fail = failAt(c.op, c.name)
			_, _, err := r.Enroll(bg, idA)
			wantCode(t, err, contract.CodeInternal, "injected "+c.op)
			if c.visible {
				wantCode(t, err, contract.CodeInternal, "durability is not confirmed")
			} else if !strings.Contains(err.Error(), "nothing was registered") {
				t.Fatalf("pre-publication failure message: %v", err)
			}
			_, showErr := r.Show(idA)
			if (showErr == nil) != c.visible {
				t.Fatalf("visible after failure = %v", showErr)
			}
			_, statErr := os.Stat(r.l.path(rel))
			if (statErr == nil) != c.visible {
				t.Fatalf("published after failure = %v", statErr)
			}
			if left := tempLeftovers(t, r.l.root); len(left) != 0 {
				t.Fatalf("temporaries left: %v", left)
			}
			// A retry with the fault removed succeeds; after a published but
			// unconfirmed record it reattempts the directory sync and never
			// replaces the record.
			var synced []string
			r.d.fail = func(op, name string) error {
				if op == "dirsync" {
					synced = append(synced, name)
				}
				return nil
			}
			before, _ := os.ReadFile(r.l.path(rel))
			n, created, err := r.Enroll(bg, idA)
			if err != nil || n.ID != idA || created == c.visible {
				t.Fatalf("retry = %+v created=%v %v", n, created, err)
			}
			if c.visible {
				after, _ := os.ReadFile(r.l.path(rel))
				if !bytes.Equal(before, after) || strings.Join(synced, ",") != nodesName {
					t.Fatalf("retry rewrote or did not resync: synced %v", synced)
				}
				// Confirmed now: a further retry syncs nothing.
				synced = nil
				if _, _, err := r.Enroll(bg, idA); err != nil || len(synced) != 0 {
					t.Fatalf("confirmed retry synced %v: %v", synced, err)
				}
			}
		})
	}
	// A second unconfirmed sync failure keeps the record and says so.
	r, _ := newRegistry(t)
	r.d.fail = failAt("dirsync", nodesName)
	r.Enroll(bg, idA)
	_, _, err := r.Enroll(bg, idA)
	wantCode(t, err, contract.CodeInternal, "durability is not confirmed")
}

// writeNodeRecord writes a raw record file into root's nodes/.
func writeNodeRecord(t *testing.T, root, name string, data []byte, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(root, nodesName)
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
	return p
}

// TestRegistryStateValidation is UT-Registry's scan part: the optional
// directory, strict validation through every plane command before any
// listener or mutation, and the no-bootstrap-around-a-roster rule.
func TestRegistryStateValidation(t *testing.T) {
	d := testDeps(t)
	// An iteration-02 root without nodes/ stays valid, and no command
	// creates nodes/ or rewrites anything.
	root := freshRoot(t)
	snap := snapshot(t, root)
	if _, err := d.init(bg, InitOptions{StateDir: root}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.inspect(bg, root); err != nil {
		t.Fatal(err)
	}
	if _, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"localhost"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, nodesName)); !os.IsNotExist(err) {
		t.Fatal("a plane command created nodes/")
	}
	sameSnapshot(t, snap, snapshot(t, root), serverCertName)
	// Valid records load offline with null observations.
	writeNodeRecord(t, root, idA+".json", encodeNodeRecord(idA, t0), 0o600)
	writeNodeRecord(t, root, idB+".json", encodeNodeRecord(idB, t0), 0o644)
	writeNodeRecord(t, root, tempPrefix+"x", []byte("junk"), 0o600)
	reg, err := loadNodeRegistry(layout{root: root}, testkit.NewFakeClock(t0))
	if err != nil {
		t.Fatal(err)
	}
	nodes := reg.Snapshot()
	if len(nodes) != 2 || nodes[0].ID != idA || nodes[1].ID != idB || nodes[0].Liveness != contract.LivenessOffline || nodes[0].LastSeen != nil || nodes[1].SoftwareVersion != nil {
		t.Fatalf("loaded = %+v", nodes)
	}
	for _, op := range allOps(d, root) {
		if err := op.run(); err != nil && !errors.Is(err, errListenReached) {
			t.Fatalf("%s with valid nodes: %v", op.name, err)
		}
	}
	// Every invalid shape is refused by every command, named, never
	// deleted.
	for _, c := range []struct {
		name  string
		setup func(root string) string
		code  contract.Code
		msg   string
	}{
		{"corrupt", func(r string) string {
			return writeNodeRecord(t, r, idC+".json", []byte("{"), 0o600)
		}, contract.CodeConflict, "invalid node record"},
		{"unknown key", func(r string) string {
			return writeNodeRecord(t, r, idC+".json", []byte(`{"schema_version":1,"id":"`+idC+`","enrolled_at":"2026-09-26T12:00:00Z","liveness":"online"}`), 0o600)
		}, contract.CodeConflict, `unknown field "liveness"`},
		{"schema", func(r string) string {
			return writeNodeRecord(t, r, idC+".json", []byte(`{"schema_version":2,"id":"`+idC+`","enrolled_at":"2026-09-26T12:00:00Z"}`), 0o600)
		}, contract.CodeConflict, "unsupported schema_version"},
		{"mismatch", func(r string) string {
			return writeNodeRecord(t, r, idC+".json", encodeNodeRecord(idA, t0), 0o600)
		}, contract.CodeConflict, "does not match the file name"},
		{"oversized", func(r string) string {
			return writeNodeRecord(t, r, idC+".json", append(encodeNodeRecord(idC, t0), bytes.Repeat([]byte(" "), maxNodeRecord)...), 0o600)
		}, contract.CodeConflict, "larger than 4096 bytes"},
		{"unknown file", func(r string) string {
			return writeNodeRecord(t, r, "notes.txt", []byte("x"), 0o600)
		}, contract.CodeConflict, "unexpected"},
		{"nested dir", func(r string) string {
			p := filepath.Join(r, nodesName, "sub")
			os.Mkdir(p, 0o700)
			return p
		}, contract.CodeConflict, "unexpected"},
		{"record dir", func(r string) string {
			p := filepath.Join(r, nodesName, idC+".json")
			os.Mkdir(p, 0o700)
			return p
		}, contract.CodeConflict, "unexpected"},
		{"symlink", func(r string) string {
			target := writeNodeRecord(t, t.TempDir(), idC+".json", encodeNodeRecord(idC, t0), 0o600)
			p := filepath.Join(r, nodesName, idC+".json")
			os.Symlink(target, p)
			return p
		}, contract.CodeTrustFailed, "symbolic link"},
		{"temp symlink", func(r string) string {
			p := filepath.Join(r, nodesName, tempPrefix+"y")
			os.Symlink("/nonexistent", p)
			return p
		}, contract.CodeTrustFailed, "symbolic link"},
		{"mode", func(r string) string {
			return writeNodeRecord(t, r, idC+".json", encodeNodeRecord(idC, t0), 0o666)
		}, contract.CodeTrustFailed, "writable by group or others"},
		{"dir mode", func(r string) string {
			p := filepath.Join(r, nodesName)
			os.Chmod(p, 0o755)
			return p
		}, contract.CodeTrustFailed, "mode 0755"},
		{"nodes file", func(r string) string {
			p := filepath.Join(r, nodesName)
			os.RemoveAll(p)
			os.WriteFile(p, nil, 0o600)
			return p
		}, contract.CodeTrustFailed, "not a directory"},
	} {
		r := freshRoot(t)
		writeNodeRecord(t, r, idA+".json", encodeNodeRecord(idA, t0), 0o600)
		p := c.setup(r)
		before := snapshot(t, r)
		for _, op := range allOps(d, r) {
			err := op.run()
			if contract.CodeOf(err) != c.code || !strings.Contains(err.Error(), c.msg) {
				t.Fatalf("%s: %s = %v, want %s %q", c.name, op.name, err, c.code, c.msg)
			}
		}
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s: invalid state was deleted", c.name)
		}
		sameSnapshot(t, before, snapshot(t, r))
		if c.code == contract.CodeConflict && c.name != "unknown file" && c.name != "nested dir" && c.name != "record dir" {
			if !strings.Contains(fmt.Sprint(allOps(d, r)[0].run()), p) {
				t.Fatalf("%s: diagnostic does not name %s", c.name, p)
			}
		}
	}
	// A roster without base state is never bootstrapped around.
	bare := newRoot(t)
	os.MkdirAll(bare, 0o700)
	writeNodeRecord(t, bare, idA+".json", encodeNodeRecord(idA, t0), 0o600)
	_, err = d.init(bg, initOpts(bare, "localhost"))
	wantCode(t, err, contract.CodeConflict, "node registry", "never bootstrapped around an existing roster")
	for _, rel := range durable {
		if _, err := os.Lstat(layout{root: bare}.path(rel)); !os.IsNotExist(err) {
			t.Fatalf("%s was created beside a roster", rel)
		}
	}
	err = d.run(bg, RunOptions{StateDir: bare, SANs: []string{"localhost"}, SANsSet: true})
	wantCode(t, err, contract.CodeConflict, "node registry")
	_, err = d.inspect(bg, bare)
	wantCode(t, err, contract.CodeConflict, "node registry")
	// An empty nodes/ beside a partial base is still partial, not empty.
	bare2 := newRoot(t)
	os.MkdirAll(filepath.Join(bare2, nodesName), 0o700)
	_, err = d.inspect(bg, bare2)
	wantCode(t, err, contract.CodeConflict)
}

// errListenReached marks a run that passed every pre-listen check.
var errListenReached = errors.New("listen reached")

type namedOp struct {
	name string
	run  func() error
}

// allOps are the four plane commands against root.
func allOps(d *deps, root string) []namedOp {
	return []namedOp{
		{"init", func() error { _, err := d.init(bg, InitOptions{StateDir: root}); return err }},
		{"run", func() error {
			d2 := *d
			d2.listen = func(string, string) (net.Listener, error) { return nil, errListenReached }
			return d2.run(bg, RunOptions{StateDir: root})
		}},
		{"status", func() error { _, err := d.inspect(bg, root); return err }},
		{"reissue", func() error {
			_, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"localhost"}})
			return err
		}},
	}
}

// TestRegistryRestartAndRestore: records survive a restart and a stopped
// backup copy with stable IDs and byte-identical PKI; observations reset.
func TestRegistryRestartAndRestore(t *testing.T) {
	d := testDeps(t)
	root := freshRoot(t)
	clk := testkit.NewFakeClock(t0)
	r, err := loadNodeRegistry(layout{root: root}, clk)
	if err != nil {
		t.Fatal(err)
	}
	r.d = d
	mustEnroll(t, r, idB)
	mustEnroll(t, r, idA)
	g, _ := r.Attach(idA, "v", 1, nil)
	r.Heartbeat(idA, g, nil)
	pki := snapshot(t, root)
	backup := newRoot(t)
	copyTree(t, root, backup)
	for _, dir := range []string{root, backup} {
		r2, err := loadNodeRegistry(layout{root: dir}, clk)
		if err != nil {
			t.Fatal(err)
		}
		nodes := r2.Snapshot()
		if len(nodes) != 2 || nodes[0].ID != idA || nodes[1].ID != idB {
			t.Fatalf("%s: %+v", dir, nodes)
		}
		for _, n := range nodes {
			if n.Liveness != contract.LivenessOffline || n.LastSeen != nil || n.ProtocolVersion != nil || n.SoftwareVersion != nil || len(n.Roles) != 0 {
				t.Fatalf("%s: observation survived a restart: %+v", dir, n)
			}
		}
		if _, err := d.init(bg, InitOptions{StateDir: dir}); err != nil {
			t.Fatal(err)
		}
		sameSnapshot(t, pki, snapshot(t, dir))
	}
	// The loader refuses what the scan admitted but the validator rejects.
	os.WriteFile(filepath.Join(root, nodesName, idA+".json"), []byte("{}"), 0o600)
	if _, err := loadNodeRegistry(layout{root: root}, clk); !contract.IsCode(err, contract.CodeConflict) {
		t.Fatalf("load corrupt = %v", err)
	}
}

func TestRegistryIOErrors(t *testing.T) {
	// Unreadable nodes/ and records surface as internal errors.
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	d := testDeps(t)
	root := freshRoot(t)
	p := writeNodeRecord(t, root, idA+".json", encodeNodeRecord(idA, t0), 0o600)
	os.Chmod(p, 0o200)
	_, err := d.inspect(bg, root)
	wantCode(t, err, contract.CodeInternal, "cannot read")
	os.Chmod(p, 0o600)
	dir := filepath.Join(root, nodesName)
	os.Chmod(dir, 0o300)
	_, err = d.inspect(bg, root)
	wantCode(t, err, contract.CodeInternal, "cannot list")
	if _, err := (layout{root: root}).loadNodeRecords(); !contract.IsCode(err, contract.CodeInternal) {
		t.Fatalf("load unlistable = %v", err)
	}
	os.Chmod(dir, 0o700)
	if _, err := (layout{root: root}).readNodeRecord(idB); !contract.IsCode(err, contract.CodeInternal) {
		t.Fatalf("missing record = %v", err)
	}
}
