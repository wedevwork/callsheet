package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

var bg = context.Background()

// testCA is an in-test CA whose leaves can be expired, mis-named or
// chained with the CA.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t testing.TB, mutate ...func(*x509.Certificate)) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	for _, m := range mutate {
		m(tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{cert: c, key: key, pem: pemOf(der)}
}

// leaf issues a server certificate for 127.0.0.1 (and localhost), chained
// with the CA when chain is set.
func (ca *testCA) leaf(t testing.TB, chain bool, mutate ...func(*x509.Certificate)) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "test server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	for _, m := range mutate {
		m(tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	c := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	if chain {
		c.Certificate = append(c.Certificate, ca.cert.Raw)
	}
	return c
}

func expired(c *x509.Certificate) {
	c.NotBefore, c.NotAfter = time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour)
}

// server is a TLS test server counting requests that reached a handler.
type server struct {
	url  string
	addr string
	hits atomic.Int64
}

func startServer(t testing.TB, cert tls.Certificate, h http.Handler) *server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{addr: ln.Addr().String()}
	s.url = "https://" + s.addr
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		h.ServeHTTP(w, r)
	}), ErrorLog: log.New(io.Discard, "", 0)}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	done := make(chan struct{})
	go func() { srv.Serve(tlsLn); close(done) }()
	t.Cleanup(func() { srv.Close(); <-done })
	return s
}

// planeHandler serves the CA route and versioned JSON routes.
func planeHandler(caPEM []byte, routes map[string]http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == contract.PathCA {
			w.Write(caPEM)
			return
		}
		if h := routes[r.URL.Path]; h != nil {
			h(w, r)
			return
		}
		w.Header().Set(contract.ProtocolHeader, "1")
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"not_found","message":"no such endpoint"}}`)
	})
}

func writeFile(t *testing.T, name string, b []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func wantCode(t *testing.T, err error, code contract.Code, substr ...string) {
	t.Helper()
	if contract.CodeOf(err) != code {
		t.Fatalf("err = %v (code %q), want %q", err, contract.CodeOf(err), code)
	}
	for _, s := range substr {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("err = %v, want it to contain %q", err, s)
		}
	}
}

func TestNormalizePlaneURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://Plane.Example":        "https://plane.example",
		"https://plane.example.":       "https://plane.example",
		"https://plane.example/":       "https://plane.example",
		"https://plane.example:443":    "https://plane.example",
		"https://plane.example:8443/":  "https://plane.example:8443",
		"https://127.0.0.1:1":          "https://127.0.0.1:1",
		"https://[::1]:65535":          "https://[::1]:65535",
		"https://[FD00:0::1]":          "https://[fd00::1]",
		"https://[::ffff:127.0.0.1]":   "https://[::ffff:127.0.0.1]",
		"https://localhost:00443":      "https://localhost",
		"HTTPS://plane.example":        "https://plane.example",
		"https://plane-1.example:8443": "https://plane-1.example:8443",
	} {
		got, err := NormalizePlaneURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizePlaneURL(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	for in, msg := range map[string]string{
		"":                                                "empty",
		"http://plane.example":                            "must be https",
		"plane.example":                                   "must be https",
		"https://user@plane.example":                      "user information",
		"https://plane.example/api":                       "paths are not allowed",
		"https://plane.example/%2F":                       "paths are not allowed",
		"https://plane.example?x":                         "queries and fragments",
		"https://plane.example?":                          "queries and fragments",
		"https://plane.example#":                          "queries and fragments",
		"https://plane.example:0":                         "1-65535",
		"https://plane.example:65536":                     "1-65535",
		"https://plane.example:x":                         "cannot be parsed",
		"https://plane.example:":                          "empty port",
		"https://pla ne.example":                          "spaces",
		"https://plané.example":                           "non-ASCII",
		"https:plane.example":                             "want https://HOST",
		"https://[fe80::1%25eth0]":                        "without a zone",
		"https://[127.0.0.1]":                             "cannot be parsed",
		"https://plane.example:044300":                    "1-65535",
		"https://-plane.example":                          "internal hyphens",
		"https://" + strings.Repeat("a", 64) + ".example": "1-63",
		"https://" + strings.Repeat("a.", 127) + "a":      "DNS name or IP",
		"https://a..b":                                    "1-63",
		"https://::1":                                     "cannot be parsed",
		"https://%zz":                                     "cannot be parsed",
	} {
		_, err := NormalizePlaneURL(in)
		if contract.CodeOf(err) != contract.CodeInvalidArgument || !strings.Contains(err.Error(), msg) {
			t.Errorf("NormalizePlaneURL(%q) = %v, want %q", in, err, msg)
		}
	}
}

func TestLoadCAFile(t *testing.T) {
	ca := newTestCA(t)
	good := writeFile(t, "ca.crt", append(append([]byte{}, ca.pem...), " \n\t\n"...), 0o644)
	c, err := LoadCAFile(good)
	if err != nil || !c.Equal(ca.cert) {
		t.Fatalf("load = %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(good, link)
	leaf := ca.leaf(t, false)
	noSign := newTestCA(t, func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageDigitalSignature })
	notCA := newTestCA(t, func(c *x509.Certificate) { c.IsCA = false })
	other := newTestCA(t)
	signed := func() []byte {
		// A CA certificate issued by another CA: not self-signed.
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "sub"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, other.cert, &key.PublicKey, other.key)
		return pemOf(der)
	}()
	for name, c := range map[string]struct {
		path string
		msg  string
	}{
		"missing":    {filepath.Join(t.TempDir(), "none"), "does not exist"},
		"symlink":    {link, "symbolic link"},
		"directory":  {t.TempDir(), "not a regular file"},
		"large":      {writeFile(t, "big", make([]byte, maxCertInput+1), 0o644), "larger than 64 KiB"},
		"garbage":    {writeFile(t, "g", []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"), 0o644), "not one PEM CERTIFICATE"},
		"empty":      {writeFile(t, "e", nil, 0o644), "not one PEM CERTIFICATE"},
		"two":        {writeFile(t, "two", append(append([]byte{}, ca.pem...), other.pem...), 0o644), "more than one PEM block"},
		"leading":    {writeFile(t, "lead", append([]byte("junk\n"), ca.pem...), 0o644), "data before the PEM block"},
		"leaf":       {writeFile(t, "leaf", pemOf(leaf.Certificate[0]), 0o644), "not a CA certificate"},
		"no sign":    {writeFile(t, "ns", noSign.pem, 0o644), "certificate-signing"},
		"not ca":     {writeFile(t, "nc", notCA.pem, 0o644), "not a CA certificate"},
		"not self":   {writeFile(t, "sub", signed, 0o644), "not self-signed"},
		"unparsable": {writeFile(t, "u", []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), 0o644), "not a parseable certificate"},
	} {
		_, err := LoadCAFile(c.path)
		if contract.CodeOf(err) != contract.CodeTrustFailed || !strings.Contains(err.Error(), "connection not trusted: ") || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v, want %q", name, err, c.msg)
		}
		if strings.Contains(fmt.Sprint(err), "-----") {
			t.Errorf("%s: diagnostic quotes PEM", name)
		}
	}
	if os.Geteuid() != 0 {
		unreadable := writeFile(t, "u2", ca.pem, 0o200)
		if _, err := LoadCAFile(unreadable); contract.CodeOf(err) != contract.CodeTrustFailed {
			t.Errorf("unreadable: %v", err)
		}
	}
	// A bad-self-signature CA (issuer equals subject, wrong key).
	forged := func() []byte {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: other.cert.Subject, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, other.cert, &key.PublicKey, other.key)
		return pemOf(der)
	}()
	if _, err := LoadCAFile(writeFile(t, "forged", forged, 0o644)); contract.CodeOf(err) != contract.CodeTrustFailed || !strings.Contains(err.Error(), "validly self-signed") {
		t.Errorf("forged: %v", err)
	}
}

func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// TestResolveTrustCA is UT-Trust's --ca path: the verified handshake
// happens before any application request, and every failure mode maps to
// its code.
func TestResolveTrustCA(t *testing.T) {
	ca := newTestCA(t)
	caFile := writeFile(t, "ca.crt", ca.pem, 0o644)
	ok := startServer(t, ca.leaf(t, false), planeHandler(ca.pem, nil))
	tr, err := ResolveTrust(bg, TrustOptions{PlaneURL: ok.url, CAFile: caFile})
	if err != nil || string(tr.CAPEM) != string(ca.pem) || tr.Fingerprint != Fingerprint(ca.cert.Raw) {
		t.Fatalf("resolve = %+v %v", tr, err)
	}
	wrong := newTestCA(t)
	wrongSrv := startServer(t, wrong.leaf(t, false), planeHandler(wrong.pem, nil))
	sanSrv := startServer(t, ca.leaf(t, false, func(c *x509.Certificate) { c.IPAddresses = nil }), planeHandler(ca.pem, nil))
	expSrv := startServer(t, ca.leaf(t, false, expired), planeHandler(ca.pem, nil))
	clientAuth := startServer(t, ca.leaf(t, false, func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} }), planeHandler(ca.pem, nil))
	plainLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer plainLn.Close()
	go func() {
		for {
			c, err := plainLn.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "HTTP/1.0 400 Bad Request\r\n\r\n")
			c.Close()
		}
	}()
	for name, c := range map[string]struct {
		o    TrustOptions
		code contract.Code
		msg  string
	}{
		"none":        {TrustOptions{PlaneURL: ok.url}, contract.CodeTrustFailed, "connection not trusted: supply --ca or --ca-fingerprint"},
		"both":        {TrustOptions{PlaneURL: ok.url, CAFile: caFile, CAFingerprint: tr.Fingerprint}, contract.CodeInvalidArgument, "only one of"},
		"bad url":     {TrustOptions{PlaneURL: "http://x", CAFile: caFile}, contract.CodeInvalidArgument, "https"},
		"bad pin":     {TrustOptions{PlaneURL: ok.url, CAFingerprint: "sha256:XYZ"}, contract.CodeInvalidArgument, "--ca-fingerprint"},
		"wrong ca":    {TrustOptions{PlaneURL: wrongSrv.url, CAFile: caFile}, contract.CodeTrustFailed, "not signed by the supplied CA"},
		"san":         {TrustOptions{PlaneURL: sanSrv.url, CAFile: caFile}, contract.CodeTrustFailed, "not valid for 127.0.0.1"},
		"expired":     {TrustOptions{PlaneURL: expSrv.url, CAFile: caFile}, contract.CodeTrustFailed, "expired or not yet valid"},
		"client auth": {TrustOptions{PlaneURL: clientAuth.url, CAFile: caFile}, contract.CodeTrustFailed, "server authentication"},
		"plaintext":   {TrustOptions{PlaneURL: "https://" + plainLn.Addr().String(), CAFile: caFile}, contract.CodeTrustFailed, "did not answer with TLS"},
		"unreachable": {TrustOptions{PlaneURL: "https://" + closedPort(t), CAFile: caFile}, contract.CodeUnavailable, "cannot reach the plane"},
		"bad file":    {TrustOptions{PlaneURL: ok.url, CAFile: filepath.Join(t.TempDir(), "x")}, contract.CodeTrustFailed, "does not exist"},
	} {
		_, err := ResolveTrust(bg, c.o)
		if contract.CodeOf(err) != c.code || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v, want %s %q", name, err, c.code, c.msg)
		}
		if c.code == contract.CodeTrustFailed && !strings.Contains(err.Error(), "connection not trusted") {
			t.Errorf("%s: missing literal phrase: %v", name, err)
		}
	}
	for _, s := range []*server{ok, wrongSrv, sanSrv, expSrv, clientAuth} {
		if s.hits.Load() != 0 {
			t.Fatal("ResolveTrust sent an application request")
		}
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := ResolveTrust(ctx, TrustOptions{PlaneURL: ok.url, CAFile: caFile}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
}

// TestFetchCA is UT-Trust's pinned bootstrap: full verification before
// the one GET, exact DER equality, every rejection without application
// bytes, and no proxy or redirect.
func TestFetchCA(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	ca := newTestCA(t)
	pin := Fingerprint(ca.cert.Raw)
	good := startServer(t, ca.leaf(t, true), planeHandler(ca.pem, nil))
	tr, err := FetchCA(bg, good.url, pin)
	if err != nil || string(tr.CAPEM) != string(ca.pem) || tr.Fingerprint != pin || good.hits.Load() != 1 {
		t.Fatalf("fetch = %+v %v hits=%d", tr, err, good.hits.Load())
	}
	// ResolveTrust's pin path is FetchCA.
	if tr2, err := ResolveTrust(bg, TrustOptions{PlaneURL: good.url + "/", CAFingerprint: pin}); err != nil || tr2.Fingerprint != pin {
		t.Fatalf("resolve pin = %v", err)
	}
	other := newTestCA(t)
	noRoot := startServer(t, ca.leaf(t, false), planeHandler(ca.pem, nil))
	twoRoots := func() *server {
		c := ca.leaf(t, true)
		c.Certificate = append(c.Certificate, other.cert.Raw)
		return startServer(t, c, planeHandler(ca.pem, nil))
	}()
	expSrv := startServer(t, ca.leaf(t, true, expired), planeHandler(ca.pem, nil))
	sanSrv := startServer(t, ca.leaf(t, true, func(c *x509.Certificate) { c.IPAddresses = nil }), planeHandler(ca.pem, nil))
	foreignLeaf := func() *server {
		// The pinned CA is in the chain, but the leaf was issued by another.
		c := other.leaf(t, false)
		c.Certificate = append(c.Certificate, ca.cert.Raw)
		return startServer(t, c, planeHandler(ca.pem, nil))
	}()
	swapped := startServer(t, ca.leaf(t, true), planeHandler(other.pem, nil))
	notFound := startServer(t, ca.leaf(t, true), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	huge := startServer(t, ca.leaf(t, true), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, maxCertInput+1)) }))
	garbage := startServer(t, ca.leaf(t, true), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "not pem") }))
	redirect := startServer(t, ca.leaf(t, true), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/", http.StatusFound)
	}))
	for name, c := range map[string]struct {
		s       *server
		pin     string
		code    contract.Code
		msg     string
		noBytes bool
	}{
		"wrong pin":    {good, Fingerprint(other.cert.Raw), contract.CodeTrustFailed, "does not match --ca-fingerprint", true},
		"no root":      {noRoot, pin, contract.CodeTrustFailed, "no self-signed CA", true},
		"two roots":    {twoRoots, pin, contract.CodeTrustFailed, "more than one self-signed CA", true},
		"expired":      {expSrv, pin, contract.CodeTrustFailed, "expired or not yet valid", true},
		"san":          {sanSrv, pin, contract.CodeTrustFailed, "not valid for 127.0.0.1", true},
		"foreign leaf": {foreignLeaf, pin, contract.CodeTrustFailed, "does not verify against the pinned CA", true},
		"swapped":      {swapped, pin, contract.CodeTrustFailed, "differs from the CA it presented", false},
		"not found":    {notFound, pin, contract.CodeTrustFailed, "HTTP 404", false},
		"huge":         {huge, pin, contract.CodeTrustFailed, "larger than 64 KiB", false},
		"garbage":      {garbage, pin, contract.CodeTrustFailed, "not one PEM CERTIFICATE", false},
		"redirect":     {redirect, pin, contract.CodeInvalidArgument, "redirect", false},
		"bad pin":      {good, "sha256:" + strings.Repeat("A", 64), contract.CodeInvalidArgument, "--ca-fingerprint", true},
	} {
		before := c.s.hits.Load()
		_, err := FetchCA(bg, c.s.url, c.pin)
		if contract.CodeOf(err) != c.code || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v, want %s %q", name, err, c.code, c.msg)
		}
		if c.code == contract.CodeTrustFailed && !strings.Contains(err.Error(), "connection not trusted") {
			t.Errorf("%s: missing literal phrase", name)
		}
		if c.noBytes && c.s.hits.Load() != before {
			t.Errorf("%s: an application request was sent before verification", name)
		}
	}
	if _, err := FetchCA(bg, "https://"+closedPort(t), pin); contract.CodeOf(err) != contract.CodeUnavailable {
		t.Fatalf("unreachable = %v", err)
	}
	if _, err := FetchCA(bg, "ftp://x", pin); contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("bad url = %v", err)
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := FetchCA(ctx, good.url, pin); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
}

// TestBootstrapVerifierMandatory: the replacement verifier cannot be built
// without a valid pin, always installs VerifyConnection and disables
// resumption, and rejects an empty chain.
func TestBootstrapVerifierMandatory(t *testing.T) {
	for _, pin := range []string{"", "sha256:", "sha1:" + strings.Repeat("0", 64)} {
		if _, err := bootstrapConfig(&pinVerifier{pin: pin}); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("pin %q accepted", pin)
		}
	}
	v := &pinVerifier{pin: "sha256:" + strings.Repeat("0", 64), host: "127.0.0.1", now: time.Now}
	cfg, err := bootstrapConfig(v)
	if err != nil || cfg.VerifyConnection == nil || !cfg.SessionTicketsDisabled || cfg.ClientSessionCache != nil || cfg.MinVersion != tls.VersionTLS12 || cfg.ServerName != "127.0.0.1" {
		t.Fatalf("config = %+v %v", cfg, err)
	}
	if err := v.verify(tls.ConnectionState{}); err == nil || v.verified() != nil {
		t.Fatal("empty chain accepted")
	}
	var pe *pinError
	if err := v.verify(tls.ConnectionState{}); !errors.As(err, &pe) || pe.Error() == "" {
		t.Fatal("pin error type")
	}
}

// TestInsecureSkipVerifyGuard: the bootstrap config is the sole
// production use of InsecureSkipVerify in the repository, set once.
func TestInsecureSkipVerifyGuard(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	var uses []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "design", "vendor", "testdata":
				return fs.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") && p != root {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "InsecureSkipVerify" {
				uses = append(uses, filepath.ToSlash(rel))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(uses, ",") != "internal/client/trust.go" {
		t.Fatalf("InsecureSkipVerify production uses = %v; only the pinned bootstrap config may use it", uses)
	}
}

func TestNew(t *testing.T) {
	ca := newTestCA(t)
	c, err := New("https://127.0.0.1:8443/", Trust{CAPEM: ca.pem})
	if err != nil || c.URL() != "https://127.0.0.1:8443" || c.Trust().Fingerprint != Fingerprint(ca.cert.Raw) {
		t.Fatalf("new = %v %v", c, err)
	}
	c.Close()
	if _, err := New("http://x", Trust{CAPEM: ca.pem}); contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatal("bad url")
	}
	if _, err := New("https://x", Trust{CAPEM: []byte("junk")}); contract.CodeOf(err) != contract.CodeTrustFailed {
		t.Fatal("bad CA")
	}
	if _, err := New("https://x", Trust{CAPEM: ca.pem, Fingerprint: Fingerprint(nil)}); contract.CodeOf(err) != contract.CodeTrustFailed {
		t.Fatal("fingerprint mismatch")
	}
	if !ValidPin(Fingerprint([]byte("x"))) || ValidPin("sha256:abc") {
		t.Fatal("ValidPin")
	}
}

func jsonRoute(version string, status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if version != "" {
			w.Header().Set(contract.ProtocolHeader, version)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

const nodeA = "n_0000000000000000000000000000000a"

func nodeJSON(id string) string {
	return `{"id":"` + id + `","liveness":"offline","last_seen":null,"protocol_version":null,"software_version":null,"roles":[]}`
}

// TestRequests is UT-Trust/UT-NodeCLI's client part: request versions,
// response version checks before bodies, error mapping, bounds, redirects
// and the operation deadline.
func TestRequests(t *testing.T) {
	ca := newTestCA(t)
	var gotVersion atomic.Value
	routes := map[string]http.HandlerFunc{}
	s := startServer(t, ca.leaf(t, false), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion.Store(r.Header.Values(contract.ProtocolHeader))
		planeHandler(ca.pem, routes).ServeHTTP(w, r)
	}))
	c, err := New(s.url, Trust{CAPEM: ca.pem})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	set := func(path string, h http.HandlerFunc) { routes[path] = h }
	set(contract.PathNodes, jsonRoute("1", 200, `{"version":1,"nodes":[`+nodeJSON(nodeA)+`]}`))
	nodes, err := c.ListNodes(bg)
	if err != nil || len(nodes) != 1 || nodes[0].ID != nodeA {
		t.Fatalf("list = %v %v", nodes, err)
	}
	if v := gotVersion.Load().([]string); len(v) != 1 || v[0] != "1" {
		t.Fatalf("request version header = %v", v)
	}
	for name, c2 := range map[string]struct {
		h    http.HandlerFunc
		code contract.Code
		msg  string
	}{
		"mismatch":       {jsonRoute("2", 200, `{"version":2,"nodes":[]}`), contract.CodeProtocolMismatch, "local=1 remote=2"},
		"mismatch error": {jsonRoute("2", 409, `{"error":{"code":"protocol_mismatch","message":"x"}}`), contract.CodeProtocolMismatch, "local=1 remote=2"},
		"missing header": {jsonRoute("", 200, `{}`), contract.CodeInvalidArgument, "lacks exactly one"},
		"bad header":     {jsonRoute("one", 200, `{}`), contract.CodeInvalidArgument, "not an integer"},
		"two headers": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add(contract.ProtocolHeader, "1")
			w.Header().Add(contract.ProtocolHeader, "1")
		}, contract.CodeInvalidArgument, "exactly one"},
		"error body":      {jsonRoute("1", 404, `{"error":{"code":"not_found","message":"no\nsuch node"}}`), contract.CodeNotFound, "no?such node"},
		"server mismatch": {jsonRoute("1", 409, `{"error":{"code":"protocol_mismatch","message":"m","details":{"local_version":3,"remote_version":1}}}`), contract.CodeProtocolMismatch, "local=1 remote=3"},
		"503":             {jsonRoute("1", 503, ``), contract.CodeUnavailable, "HTTP 503"},
		"500":             {jsonRoute("1", 500, `garbage`), contract.CodeInternal, "HTTP 500"},
		"malformed":       {jsonRoute("1", 200, `{"version":1,"nodes":[{}]}`), contract.CodeInvalidArgument, "required field"},
		"body version":    {jsonRoute("1", 200, `{"version":2,"nodes":[]}`), contract.CodeProtocolMismatch, "remote=2"},
		"redirect": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(contract.ProtocolHeader, "1")
			http.Redirect(w, r, "/elsewhere", 302)
		}, contract.CodeInvalidArgument, "redirect"},
		"oversized": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(contract.ProtocolHeader, "1")
			w.Write(make([]byte, maxRosterBody+1))
		}, contract.CodeInvalidArgument, "larger than"},
	} {
		set(contract.PathNodes, c2.h)
		_, err := c.ListNodes(bg)
		if contract.CodeOf(err) != c2.code || !strings.Contains(err.Error(), c2.msg) {
			t.Errorf("%s: %v, want %s %q", name, err, c2.code, c2.msg)
		}
	}
	// Show and enroll validate the named node.
	set(contract.PathNodes+"/"+nodeA, jsonRoute("1", 200, `{"version":1,"node":`+nodeJSON("n_0000000000000000000000000000000b")+`}`))
	if _, err := c.ShowNode(bg, nodeA); contract.CodeOf(err) != contract.CodeInvalidArgument || !strings.Contains(err.Error(), "names another node") {
		t.Fatalf("show other = %v", err)
	}
	set(contract.PathNodes+"/"+nodeA, jsonRoute("1", 200, `{"version":1,"node":`+nodeJSON(nodeA)+`}`))
	if n, err := c.ShowNode(bg, nodeA); err != nil || n.ID != nodeA {
		t.Fatalf("show = %v %v", n, err)
	}
	set(contract.PathNodes+"/"+nodeA, jsonRoute("1", 200, `{"version":1}`))
	if _, err := c.ShowNode(bg, nodeA); contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatal("show malformed")
	}
	set(contract.PathNodes+"/"+nodeA, jsonRoute("1", 404, `{"error":{"code":"not_found","message":"unknown"}}`))
	if _, err := c.ShowNode(bg, nodeA); contract.CodeOf(err) != contract.CodeNotFound {
		t.Fatal("show 404")
	}
	if _, err := c.ShowNode(bg, "x"); contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatal("show invalid ID")
	}
	for _, c2 := range []struct {
		status int
		body   string
		code   contract.Code
	}{
		{201, `{"version":1,"node":` + nodeJSON(nodeA) + `}`, ""},
		{200, `{"version":1,"node":` + nodeJSON(nodeA) + `}`, ""},
		{202, `{"version":1,"node":` + nodeJSON(nodeA) + `}`, contract.CodeInvalidArgument},
		{201, `{"version":1,"node":` + nodeJSON("n_0000000000000000000000000000000b") + `}`, contract.CodeInvalidArgument},
		{201, `[]`, contract.CodeInvalidArgument},
		{409, `{"error":{"code":"conflict","message":"c"}}`, contract.CodeConflict},
	} {
		set(contract.PathEnroll, jsonRoute("1", c2.status, c2.body))
		_, err := c.EnrollNode(bg, nodeA, "dev")
		if contract.CodeOf(err) != c2.code {
			t.Fatalf("enroll %d %s = %v", c2.status, c2.body, err)
		}
	}
	// The operation deadline bounds a hung plane; the caller's own
	// cancellation is returned unchanged.
	release := make(chan struct{})
	defer close(release)
	set(contract.PathNodes, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	old := operationTimeout
	operationTimeout = 100 * time.Millisecond
	defer func() { operationTimeout = old }()
	start := time.Now()
	_, err = c.ListNodes(bg)
	if contract.CodeOf(err) != contract.CodeUnavailable || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 10*time.Second {
		t.Fatalf("hung plane = %v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := c.ListNodes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	if code := contract.CodeOf(classify(endpoint{url: "https://x"}, io.ErrUnexpectedEOF)); code != contract.CodeUnavailable {
		t.Fatal("EOF classification")
	}
	if !strings.Contains(classify(endpoint{url: "https://x"}, io.EOF).Error(), "closed") {
		t.Fatal("EOF message")
	}
	ce := contract.New(contract.CodeConflict, "x")
	if classify(endpoint{}, ce) != ce {
		t.Fatal("contract errors pass through")
	}
	if _, err := readLimited(errReader{}, 10); err == nil {
		t.Fatal("read error")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken") }

// TestDialNodeStream covers the upgrade outcomes.
func TestDialNodeStream(t *testing.T) {
	ca := newTestCA(t)
	var mode atomic.Value
	mode.Store("ok")
	s := startServer(t, ca.leaf(t, false), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case "503":
			w.WriteHeader(503)
		case "404":
			w.WriteHeader(404)
		default:
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			ws.Close(websocket.StatusNormalClosure, "")
		}
	}))
	c, _ := New(s.url, Trust{CAPEM: ca.pem})
	defer c.Close()
	ws, err := c.DialNodeStream(bg)
	if err != nil {
		t.Fatal(err)
	}
	ws.CloseNow()
	mode.Store("503")
	if _, err := c.DialNodeStream(bg); contract.CodeOf(err) != contract.CodeUnavailable {
		t.Fatalf("503 = %v", err)
	}
	mode.Store("404")
	if _, err := c.DialNodeStream(bg); contract.CodeOf(err) != contract.CodeInvalidArgument || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 = %v", err)
	}
	wrong, _ := New(s.url, Trust{CAPEM: newTestCA(t).pem})
	defer wrong.Close()
	if _, err := wrong.DialNodeStream(bg); contract.CodeOf(err) != contract.CodeTrustFailed {
		t.Fatalf("wrong CA = %v", err)
	}
	gone, _ := New("https://"+closedPort(t), Trust{CAPEM: ca.pem})
	if _, err := gone.DialNodeStream(bg); contract.CodeOf(err) != contract.CodeUnavailable {
		t.Fatalf("unreachable = %v", err)
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := c.DialNodeStream(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
}
