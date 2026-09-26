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
