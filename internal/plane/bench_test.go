package plane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
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
