package plane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// BenchmarkPlaneIssue measures P-256 CA and server generation, signing and
// verification. Each iteration checks the chain; no latency limit is
// asserted.
func BenchmarkPlaneIssue(b *testing.B) {
	d := defaultDeps()
	sans, err := normalizeSANs([]string{"localhost", "127.0.0.1"})
	if err != nil {
		b.Fatal(err)
	}
	bind := netip.MustParseAddrPort(DefaultBind)
	b.ReportAllocs()
	for b.Loop() {
		now := d.clock()
		m, files, err := d.generate(now, bind, sans)
		if err != nil {
			b.Fatal(err)
		}
		if err := m.verifyAt(now); err != nil || len(files) != len(durable) {
			b.Fatalf("invariants: %v %d", err, len(files))
		}
	}
}

// BenchmarkPlaneInit measures a complete first initialization in a fresh
// root, including temporary writes, publication and directory syncs.
func BenchmarkPlaneInit(b *testing.B) {
	d := defaultDeps()
	d.addrs = nil // the default loopback bind never enumerates
	parent := b.TempDir()
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		root := filepath.Join(parent, strconv.Itoa(i))
		st, err := d.init(bg, InitOptions{StateDir: root, SANs: []string{"localhost"}, SANsSet: true})
		if err != nil {
			b.Fatal(err)
		}
		if st.CAFingerprint == "" || st.Bind != DefaultBind || len(tempLeftovers(b, root)) != 0 {
			b.Fatalf("report %+v", st)
		}
		b.StopTimer()
		if err := os.RemoveAll(root); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

// BenchmarkPlaneTLSHealth measures one verified loopback TLS handshake plus
// a health request per iteration, without connection reuse or session
// resumption.
func BenchmarkPlaneTLSHealth(b *testing.B) {
	d := defaultDeps()
	d.addrs = nil
	root := filepath.Join(b.TempDir(), "state")
	if _, err := d.init(bg, InitOptions{StateDir: root, Bind: "127.0.0.1:0", BindSet: true, SANs: []string{"127.0.0.1"}, SANsSet: true}); err != nil {
		b.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(readCA(b, root))
	ready := make(chan net.Addr, 1)
	d.ready = func(a net.Addr) { ready <- a }
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- d.run(ctx, RunOptions{StateDir: root}) }()
	var addr net.Addr
	select {
	case addr = <-ready:
	case err := <-done:
		cancel()
		b.Fatalf("run: %v", err)
	case <-time.After(20 * time.Second):
		cancel()
		b.Fatal("run did not become ready")
	}
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			b.Errorf("run = %v", err)
		}
	}()
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true, Proxy: nil}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	url := "https://" + addr.String() + HealthPath
	b.ReportAllocs()
	for b.Loop() {
		resp, err := c.Get(url)
		if err != nil {
			b.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || string(body) != healthBody || resp.TLS == nil || !resp.TLS.HandshakeComplete || resp.TLS.DidResume {
			b.Fatalf("health = %d %q %v", resp.StatusCode, body, err)
		}
	}
}

// BenchmarkNodeHeartbeat measures the plane's heartbeat path: decoding a
// heartbeat message, the registry update and encoding the acknowledgement.
// Every iteration checks the lease and the acknowledgement.
func BenchmarkNodeHeartbeat(b *testing.B) {
	root := filepath.Join(b.TempDir(), "state")
	os.MkdirAll(root, 0o700)
	r, err := loadNodeRegistry(layout{root: root}, realClock{})
	if err != nil {
		b.Fatal(err)
	}
	r.d = defaultDeps()
	if _, _, err := r.Enroll(bg, idA); err != nil {
		b.Fatal(err)
	}
	gen, err := r.Attach(idA, "bench", contract.ProtocolVersion, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	n := 0
	for b.Loop() {
		n++
		rid := "b" + strconv.Itoa(n)
		msg, err := contract.EncodeFrame(contract.ProtocolVersion, contract.FrameHeartbeat, rid, contract.HeartbeatBody{})
		if err != nil {
			b.Fatal(err)
		}
		f, err := contract.DecodeFrame(msg, contract.FromSidecar)
		if err != nil || f.RequestID != rid {
			b.Fatalf("decode: %v", err)
		}
		hb, err := contract.DecodeHeartbeat(f.Body)
		if err != nil {
			b.Fatal(err)
		}
		if err := r.Heartbeat(idA, gen, hb.Roles); err != nil {
			b.Fatal(err)
		}
		ack, err := contract.EncodeFrame(contract.ProtocolVersion, contract.FrameHeartbeatAck, f.RequestID, nil)
		if err != nil {
			b.Fatal(err)
		}
		af, err := contract.DecodeFrame(ack, contract.FromPlane)
		if err != nil || af.Type != contract.FrameHeartbeatAck || af.RequestID != rid || contract.DecodeAck(af.Body) != nil {
			b.Fatalf("ack %s: %v", ack, err)
		}
		if st := r.nodes[idA]; !st.online || !st.deadline.After(time.Now()) {
			b.Fatal("lease not refreshed")
		}
	}
}

// BenchmarkNodeSnapshot measures one roster snapshot of 100 known nodes
// (one clock sample, expiry, copies, sorting) and checks it is complete,
// sorted and stable.
func BenchmarkNodeSnapshot(b *testing.B) {
	root := filepath.Join(b.TempDir(), "state")
	os.MkdirAll(filepath.Join(root, nodesName), 0o700)
	var ids []string
	for i := range 100 {
		id := fmt.Sprintf("n_%032x", 1000-i)
		ids = append(ids, id)
		os.WriteFile(filepath.Join(root, nodesName, id+".json"), encodeNodeRecord(id, t0), 0o600)
	}
	sort.Strings(ids)
	r, err := loadNodeRegistry(layout{root: root}, realClock{})
	if err != nil {
		b.Fatal(err)
	}
	for _, id := range ids[:50] {
		g, _ := r.Attach(id, "bench", 1, nil)
		r.Heartbeat(id, g, nil)
	}
	b.ReportAllocs()
	for b.Loop() {
		nodes := r.Snapshot()
		if len(nodes) != 100 {
			b.Fatalf("%d nodes", len(nodes))
		}
		for i, n := range nodes {
			if n.ID != ids[i] || (i < 50) != (n.Liveness == contract.LivenessOnline) {
				b.Fatalf("node %d = %+v", i, n)
			}
		}
	}
}

// BenchmarkNodeEnrollment measures a new node's durable enrollment (write,
// fsync, no-replace link, directory syncs) plus an idempotent repeat, and
// checks the published record.
func BenchmarkNodeEnrollment(b *testing.B) {
	root := filepath.Join(b.TempDir(), "state")
	os.MkdirAll(root, 0o700)
	r, err := loadNodeRegistry(layout{root: root}, realClock{})
	if err != nil {
		b.Fatal(err)
	}
	r.d = defaultDeps()
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		id := fmt.Sprintf("n_%032x", i)
		n, created, err := r.Enroll(bg, id)
		if err != nil || !created || n.ID != id {
			b.Fatalf("enroll = %v %v", created, err)
		}
		if _, created, err := r.Enroll(bg, id); err != nil || created {
			b.Fatalf("repeat = %v %v", created, err)
		}
		if _, err := r.l.readNodeRecord(id); err != nil {
			b.Fatal(err)
		}
	}
}

// benchRoles builds n role records alternating over two nodes and two
// names, in registration order.
func benchRoles(n int) []contract.RoleRecord {
	var out []contract.RoleRecord
	for i := 1; i <= n; i++ {
		node, name := idA, "implementer"
		if i%2 == 0 {
			node, name = idB, "reviewer"
		}
		out = append(out, record(roleCfg("role-"+strconv.Itoa(i), name, node), i))
	}
	return out
}

// BenchmarkRoleSnapshot measures the role list and node views for 1 and
// 100 roles: one node online with an acknowledged snapshot reporting every
// role ready, the other offline. Every iteration checks the grouping and
// order, zero inflight, readiness only for the ready node and false
// masking for the offline one.
func BenchmarkRoleSnapshot(b *testing.B) {
	for _, n := range []int{1, 100} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			root := filepath.Join(b.TempDir(), "state")
			os.MkdirAll(filepath.Join(root, nodesName), 0o700)
			for _, id := range []string{idA, idB} {
				os.WriteFile(filepath.Join(root, nodesName, id+".json"), encodeNodeRecord(id, t0), 0o600)
			}
			recs := benchRoles(n)
			doc := docOf(1, n+1, recs...)
			r, err := loadNodeRegistry(layout{root: root}, realClock{})
			if err != nil {
				b.Fatal(err)
			}
			r.roles = newRoleRegistry(layout{root: root}, defaultDeps(), roleLookup, doc)
			gen, err := r.attach(idA, "bench", contract.ProtocolVersion, nil, nil)
			if err != nil {
				b.Fatal(err)
			}
			snap := doc.snapshotFor(idA)
			r.ackSnapshot(idA, gen, snap)
			hb := contract.HeartbeatBody{RolesRevision: snap.rev}
			for _, rec := range snap.roles {
				hb.Roles = append(hb.Roles, contract.RoleStatus{RoleID: rec.ID, Concurrency: rec.Concurrency, CanAccept: true})
			}
			if err := r.heartbeat(idA, gen, hb); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				views := r.roleViews(func(s *roleState) []contract.RoleRecord { return sortedForList(s.visible.roles) }, roleLookup)
				if len(views) != n {
					b.Fatalf("%d views", len(views))
				}
				for i, v := range views {
					if i > 0 && !contract.RoleListLess(views[i-1].RoleRecord, v.RoleRecord) {
						b.Fatal("views not grouped by name and order")
					}
					online := v.Node == idA
					if v.Inflight != 0 || v.CanAccept != online || (v.NodeLiveness == contract.LivenessOnline) != online {
						b.Fatalf("view %+v", v)
					}
				}
				nodes := r.Snapshot()
				for _, nd := range nodes {
					for _, rs := range nd.Roles {
						if rs.Inflight != 0 || rs.CanAccept != (nd.ID == idA) {
							b.Fatalf("node %s role %+v", nd.ID, rs)
						}
					}
				}
			}
		})
	}
}

// BenchmarkRoleMutation measures one durable add, set and rm transaction
// each (temporary write and fsync, no-replace link or rename, directory
// sync) on a registry of 1 and 100 roles, and checks the published
// document, counters and order after every iteration.
func BenchmarkRoleMutation(b *testing.B) {
	for _, n := range []int{1, 100} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			root := filepath.Join(b.TempDir(), "state")
			os.MkdirAll(root, 0o700)
			recs := benchRoles(n - 1)
			// The registry exists on disk, as it would after loading it.
			writeRegistry(b, root, docOf(1, n, recs...))
			reg := newRoleRegistry(layout{root: root}, defaultDeps(), roleLookup, docOf(1, n, recs...))
			nodes := map[string]bool{idA: true, idB: true}
			commit := func(next *roleDoc) {
				tmp, err := reg.prepare(next)
				if err == nil {
					err = reg.publish(tmp, next, idA)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				doc := reg.load().visible
				next, rec := doc.withAdded(roleCfg("bench-"+strconv.Itoa(i), "implementer", idA))
				commit(next)
				c := rec.RoleConfig
				c.Concurrency = 3
				_, idx, _ := next.find(rec.ID)
				set, _ := next.withReplaced(idx, c)
				commit(set)
				commit(set.withRemoved(idx))
				got, err := layout{root: root}.loadRoleDoc(roleLookup, nodes)
				if err != nil || got.revision != doc.revision+3 || got.nextOrder != doc.nextOrder+1 || len(got.roles) != n-1 || len(roleTempsB(root)) != 0 {
					b.Fatalf("after iteration %d: %+v %v", i, got, err)
				}
			}
		})
	}
}

// roleTempsB lists temporaries in roles/.
func roleTempsB(root string) []string {
	var out []string
	entries, _ := os.ReadDir(layout{root: root}.path(rolesName))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}
