package plane

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// oracleCA is a public CA certificate issued by this package on
// 2026-09-26. oracleFingerprint was computed independently with
//
//	openssl x509 -in ca.crt -outform DER | sha256sum
const (
	oracleCA = `-----BEGIN CERTIFICATE-----
MIIBhDCCASqgAwIBAgIRAP3vSjmNItycRX+5dP7Llj8wCgYIKoZIzj0EAwIwIDEe
MBwGA1UEAxMVQ2FsbHNoZWV0IGludGVybmFsIENBMB4XDTI2MDkyNjA1MTcyN1oX
DTM2MDkyNjA1MjIyN1owIDEeMBwGA1UEAxMVQ2FsbHNoZWV0IGludGVybmFsIENB
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE1Ax8HiMuCDFzgKBLdBVi7CDpGVVT
FRVm4RidBVMYxiAuOwSP/5XQGE4Ax9/QWXwbhOz6jmHNYpywVpKhj6lqQ6NFMEMw
DgYDVR0PAQH/BAQDAgGGMBIGA1UdEwEB/wQIMAYBAf8CAQAwHQYDVR0OBBYEFORM
faLjKdgjb0tDEOiITt0/DZ6UMAoGCCqGSM49BAMCA0gAMEUCIQDOgky0CBX7C6Vb
o8Z+7cI8DQwp6iFtCQq8cJyK3TAJywIgFiCTNJ9x/0CdytbKVCWoSiUPW7YzvCyJ
O1D+4mNF92I=
-----END CERTIFICATE-----
`
	oracleFingerprint = "sha256:033f577297e1e353c5c56973183b3fb3973e867014ae2105e4460cb5ff123cff"
)

func TestFingerprintOracle(t *testing.T) {
	der, err := decodePEM([]byte(oracleCA), pemCert)
	if err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(der); got != oracleFingerprint {
		t.Fatalf("fingerprint = %s", got)
	}
	// PEM formatting (line endings, surrounding whitespace) is not hashed.
	crlf := "\r\n\t" + strings.ReplaceAll(oracleCA, "\n", "\r\n") + "\n\n"
	der2, err := decodePEM([]byte(crlf), pemCert)
	if err != nil || Fingerprint(der2) != oracleFingerprint {
		t.Fatalf("CRLF PEM: %v", err)
	}
	c, _ := x509.ParseCertificate(der)
	if Fingerprint(c.RawSubjectPublicKeyInfo) == oracleFingerprint {
		t.Fatal("fingerprint must hash the certificate DER, not the SPKI")
	}
}

func TestDecodePEM(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "no PEM block",
		"x" + oracleCA:                  "data before the PEM block",
		oracleCA + "x":                  "extra PEM blocks or trailing data",
		oracleCA + oracleCA:             "extra PEM blocks or trailing data",
		"-----BEGIN CERTIFICATE-----\n": "malformed PEM block",
		strings.Replace(oracleCA, "-----\nMIIB", "-----\nProc-Type: 4,ENCRYPTED\n\nMIIB", 1): "PEM headers",
	} {
		if _, err := decodePEM([]byte(in), pemCert); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("decodePEM(%.30q) = %v, want %q", in, err, want)
		}
	}
	if _, err := decodePEM([]byte(oracleCA), pemKey); err == nil || !strings.Contains(err.Error(), `want "PRIVATE KEY"`) {
		t.Fatalf("type mismatch: %v", err)
	}
}

func TestIssuedCredentials(t *testing.T) {
	d := testDeps(t)
	root := newRoot(t)
	st := mustInit(t, d, root, "Plane.Example.", "localhost", "127.0.0.1", "::1", "::ffff:10.0.0.9", "plane.example")
	ca, srv := readCA(t, root), readServer(t, root)
	if ca.Subject.CommonName != "Callsheet internal CA" || !ca.IsCA || !ca.BasicConstraintsValid || ca.MaxPathLen != 0 || !ca.MaxPathLenZero ||
		ca.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign|x509.KeyUsageDigitalSignature || len(ca.ExtKeyUsage) != 0 {
		t.Fatalf("CA = %+v", ca)
	}
	if srv.Subject.CommonName != "Callsheet plane" || srv.IsCA || srv.KeyUsage != x509.KeyUsageDigitalSignature ||
		len(srv.ExtKeyUsage) != 1 || srv.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || !bytes.Equal(srv.RawIssuer, ca.RawSubject) {
		t.Fatalf("server = %+v", srv)
	}
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	if ca.SerialNumber.Sign() <= 0 || srv.SerialNumber.Sign() <= 0 || ca.SerialNumber.Cmp(limit) >= 0 || srv.SerialNumber.Cmp(limit) >= 0 || ca.SerialNumber.Cmp(srv.SerialNumber) == 0 {
		t.Fatalf("serials %v %v", ca.SerialNumber, srv.SerialNumber)
	}
	if strings.Join(srv.DNSNames, ",") != "localhost,plane.example" || strings.Join(st.DNSNames, ",") != "localhost,plane.example" ||
		strings.Join(st.IPAddresses, ",") != "10.0.0.9,127.0.0.1,::1" {
		t.Fatalf("SANs = %v %v / %v", srv.DNSNames, srv.IPAddresses, st)
	}
	caKey, _ := loadKey(layout{root: root}.path(caKeyName))
	srvKey, _ := loadKey(layout{root: root}.path(serverKeyName))
	if caKey.PublicKey.Equal(&srvKey.PublicKey) {
		t.Fatal("CA and server share a key")
	}
	for _, rel := range []string{caKeyName, serverKeyName} {
		b, _ := os.ReadFile(layout{root: root}.path(rel))
		if !bytes.HasPrefix(b, []byte("-----BEGIN PRIVATE KEY-----\n")) {
			t.Fatalf("%s is not PKCS#8 PEM", rel)
		}
	}
	if st.CAFingerprint != Fingerprint(ca.Raw) || len(st.CAFingerprint) != len("sha256:")+64 {
		t.Fatalf("fingerprint %s", st.CAFingerprint)
	}
	// A second state has independent keys and serials.
	other := newRoot(t)
	mustInit(t, d, other, "localhost")
	if readCA(t, other).SerialNumber.Cmp(ca.SerialNumber) == 0 || Fingerprint(readCA(t, other).Raw) == st.CAFingerprint {
		t.Fatal("states share CA identity")
	}
}

// Repeat init prints the same report, rewrites nothing, and refuses
// mismatched inputs.
func TestRepeatInit(t *testing.T) {
	d := testDeps(t)
	root := newRoot(t)
	first := mustInit(t, d, root, "localhost", "10.0.0.1")
	snap := snapshot(t, root)
	infos := map[string]os.FileInfo{}
	for _, rel := range durable {
		infos[rel], _ = os.Stat(layout{root: root}.path(rel))
	}
	for _, o := range []InitOptions{
		{StateDir: root},
		initOpts(root, "10.0.0.1", "LOCALHOST.", "localhost"),
		{StateDir: root, Bind: "[::ffff:127.0.0.1]:0", BindSet: true},
	} {
		st, err := d.init(bg, o)
		if err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
		st.Warnings, first.Warnings = nil, nil
		if fmt.Sprint(st) != fmt.Sprint(first) {
			t.Fatalf("repeat report differs:\n%+v\n%+v", st, first)
		}
	}
	for _, c := range []struct {
		o    InitOptions
		want string
	}{
		{initOpts(root, "localhost"), "use callsheet plane cert reissue"},
		{initOpts(root, "localhost", "10.0.0.1", "extra"), "use callsheet plane cert reissue"},
		{InitOptions{StateDir: root, Bind: "127.0.0.1:8443", BindSet: true}, "stop the plane and edit"},
	} {
		_, err := d.init(bg, c.o)
		wantCode(t, err, contract.CodeConflict, c.want)
	}
	sameSnapshot(t, snap, snapshot(t, root))
	for _, rel := range durable {
		fi, _ := os.Stat(layout{root: root}.path(rel))
		if !os.SameFile(fi, infos[rel]) || !fi.ModTime().Equal(infos[rel].ModTime()) {
			t.Fatalf("%s was rewritten", rel)
		}
	}
}

// zeroReader yields only zero bytes.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

// TestIssuanceContract is delegated from tests/function
// (TestPlaneInit/issuance). Do not rename or skip its subtests.
func TestIssuanceContract(t *testing.T) {
	t.Run("calendar", func(t *testing.T) {
		for _, c := range []struct {
			now                         time.Time
			caBefore, caAfter, srvAfter string
		}{
			{time.Date(2028, 2, 29, 10, 20, 30, 999_000_000, time.UTC), "2028-02-29T10:15:30Z", "2038-03-01T10:20:30Z", "2030-03-01T10:20:30Z"},
			{time.Date(2026, 9, 26, 12, 0, 0, 0, time.FixedZone("x", 5*3600)), "2026-09-26T06:55:00Z", "2036-09-26T07:00:00Z", "2028-09-26T07:00:00Z"},
			{time.Date(2027, 1, 1, 0, 2, 0, 0, time.UTC), "2026-12-31T23:57:00Z", "2037-01-01T00:02:00Z", "2029-01-01T00:02:00Z"},
			{time.Date(2027, 12, 31, 23, 59, 59, 0, time.UTC), "2027-12-31T23:54:59Z", "2037-12-31T23:59:59Z", "2029-12-31T23:59:59Z"},
		} {
			d := testDeps(t)
			d.now = fixed(c.now)
			root := newRoot(t)
			st := mustInit(t, d, root, "localhost")
			ca, srv := readCA(t, root), readServer(t, root)
			if rfc3339(ca.NotBefore) != c.caBefore || rfc3339(ca.NotAfter) != c.caAfter ||
				rfc3339(srv.NotBefore) != c.caBefore || rfc3339(srv.NotAfter) != c.srvAfter {
				t.Fatalf("%v: ca %s..%s server %s..%s", c.now, rfc3339(ca.NotBefore), rfc3339(ca.NotAfter), rfc3339(srv.NotBefore), rfc3339(srv.NotAfter))
			}
			if rfc3339(st.CANotBefore) != c.caBefore || rfc3339(st.ServerNotAfter) != c.srvAfter || st.CANotAfter.Location() != time.UTC {
				t.Fatalf("status times %+v", st)
			}
		}
	})
	t.Run("entropy", func(t *testing.T) {
		for name, r := range map[string]interface{ Read([]byte) (int, error) }{"failing": failReader{}, "zero": zeroReader{}} {
			d := testDeps(t)
			d.rand = r
			root := newRoot(t)
			_, err := d.init(bg, initOpts(root, "localhost"))
			wantCode(t, err, contract.CodeInternal, "entropy source failed")
			for rel, b := range snapshot(t, root) {
				if b != nil {
					t.Fatalf("%s: %s written after an entropy failure", name, rel)
				}
			}
			if left := tempLeftovers(t, root); len(left) != 0 {
				t.Fatalf("%s: temporaries %v", name, left)
			}
		}
		// A reissue entropy failure changes nothing.
		d := testDeps(t)
		root := newRoot(t)
		mustInit(t, d, root, "localhost")
		snap := snapshot(t, root)
		d.rand = failReader{}
		_, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"x"}})
		wantCode(t, err, contract.CodeInternal, "entropy source failed")
		sameSnapshot(t, snap, snapshot(t, root))
	})
	t.Run("san-validation", func(t *testing.T) {
		label63 := strings.Repeat("a", 63)
		name253 := strings.Join([]string{label63, label63, label63, strings.Repeat("b", 61)}, ".")
		ok := []struct {
			in       []string
			dns, ips string
		}{
			{[]string{"Plane.Example.", "plane.example", "LOCALHOST", "host1"}, "host1,localhost,plane.example", ""},
			{[]string{"10.0.0.2", "10.0.0.10", "::1", "::ffff:10.0.0.2", "fd00::a", "2001:db8::1", "8.8.8.8", "fe80::1"}, "", "10.0.0.10,10.0.0.2,2001:db8::1,8.8.8.8,::1,fd00::a,fe80::1"},
			{[]string{"xn--bcher-kva.example", "a-b.c-d", "1.2.3", "0a"}, "0a,1.2.3,a-b.c-d,xn--bcher-kva.example", ""},
			{[]string{name253, strings.ToUpper(name253)}, name253, ""},
			{[]string{label63}, label63, ""},
		}
		for _, c := range ok {
			s, err := normalizeSANs(c.in)
			if err != nil || strings.Join(s.dns, ",") != c.dns || strings.Join(s.ipStrings(), ",") != c.ips {
				t.Fatalf("normalizeSANs(%q) = %v %v %v", c.in, s.dns, s.ipStrings(), err)
			}
		}
		for _, in := range [][]string{
			nil, {}, {""}, {"."}, {" "}, {"a b"}, {"host "}, {"\thost"}, {"*.example"}, {"a.*"}, {"bücher.example"}, {"https://plane.example"},
			{"plane.example:8443"}, {"plane.example/x"}, {"-a.example"}, {"a-.example"}, {"a..b"}, {"a_b"}, {label63 + "a"},
			{name253 + "c"}, {name253 + "."}, {strings.Repeat("a", 254)}, {"0.0.0.0"}, {"::"}, {"224.0.0.1"}, {"ff02::1"}, {"fe80::1%eth0"}, {"[::1]"}, {"10.0.0.1:1"},
		} {
			if _, err := normalizeSANs(in); codeOf(err) != contract.CodeInvalidArgument {
				t.Fatalf("normalizeSANs(%q) = %v", in, err)
			}
		}
		var hundred []string
		for i := range 100 {
			hundred = append(hundred, fmt.Sprintf("h%d", i))
		}
		if _, err := normalizeSANs(append(hundred, "h0", "H1.")); err != nil {
			t.Fatalf("100 unique SANs with duplicates: %v", err)
		}
		_, err := normalizeSANs(append(hundred, "10.0.0.1"))
		wantCode(t, err, contract.CodeInvalidArgument, "at most 100")
		// Invalid SANs are rejected before any state exists.
		d := testDeps(t)
		root := newRoot(t)
		_, err = d.init(bg, initOpts(root, "ok.example", "bad name"))
		wantCode(t, err, contract.CodeInvalidArgument, `"bad name"`)
		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Fatal("invalid SAN created state")
		}
		// Canonical SANs land in the certificate.
		mustInit(t, d, root, "B.example.", "a.example", "::ffff:192.168.1.1", "10.0.0.1")
		srv := readServer(t, root)
		if strings.Join(srv.DNSNames, ",") != "a.example,b.example" || len(srv.IPAddresses) != 2 || srv.IPAddresses[0].String() != "10.0.0.1" || len(srv.IPAddresses[1]) != 4 {
			t.Fatalf("certificate SANs %v %v", srv.DNSNames, srv.IPAddresses)
		}
	})
}

func TestValidateRejectsWeakMaterial(t *testing.T) {
	d := testDeps(t)
	now := time.Now().UTC().Truncate(time.Second)
	sans, _ := normalizeSANs([]string{"localhost"})
	m, _, err := d.generate(now, netip.MustParseAddrPort(DefaultBind), sans)
	if err != nil {
		t.Fatal(err)
	}
	// A CA without path-length zero, or without cert-sign usage, is rejected.
	for name, mutate := range map[string]func(*x509.Certificate){
		"pathlen":  func(c *x509.Certificate) { c.MaxPathLenZero = false; c.MaxPathLen = -1 },
		"certsign": func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageDigitalSignature },
	} {
		tmpl := caTemplate(big.NewInt(7), now)
		mutate(tmpl)
		der, err := x509.CreateCertificate(d.rand, tmpl, tmpl, &m.caKey.PublicKey, m.caKey)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		bad := &material{caCert: c, serverCert: m.serverCert}
		if err := bad.validate(false); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// Server usage must be exactly server auth with digital signature.
	for name, mutate := range map[string]func(*x509.Certificate){
		"clientauth": func(c *x509.Certificate) {
			c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
		},
		"nosig": func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageKeyEncipherment },
	} {
		tmpl := serverTemplate(big.NewInt(9), now, sans)
		mutate(tmpl)
		der, err := x509.CreateCertificate(d.rand, tmpl, m.caCert, &m.serverKey.PublicKey, m.caKey)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		bad := &material{caCert: m.caCert, serverCert: c}
		if err := bad.validate(false); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// Unsupported key types (here P-384) are rejected on load.
	k384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := caTemplate(big.NewInt(11), now)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k384.PublicKey, k384)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(dir+"/c.crt", certPEM(der), 0o600)
	if _, err := loadCert(dir + "/c.crt"); codeOf(err) != contract.CodeTrustFailed || !strings.Contains(err.Error(), "ECDSA P-256") {
		t.Fatalf("P-384 certificate: %v", err)
	}
	kp, _ := x509.MarshalPKCS8PrivateKey(k384)
	os.WriteFile(dir+"/k.key", pem.EncodeToMemory(&pem.Block{Type: pemKey, Bytes: kp}), 0o600)
	if _, err := loadKey(dir + "/k.key"); codeOf(err) != contract.CodeTrustFailed || !strings.Contains(err.Error(), "ECDSA P-256") {
		t.Fatalf("P-384 key: %v", err)
	}
	if _, err := loadKey(dir + "/missing"); codeOf(err) != contract.CodeInternal {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := loadCert(dir + "/missing"); codeOf(err) != contract.CodeInternal {
		t.Fatalf("missing cert: %v", err)
	}
}

// bytesReader repeats a fixed 16-byte block, so every serial drawn from it
// is the same.
type bytesReader struct{ b []byte }

func (r bytesReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b[i%len(r.b)]
	}
	return len(p), nil
}

// The serial embedded in the server certificate is the one checked
// against the CA serial (and, on reissue, the existing serials).
func TestSerialCollisions(t *testing.T) {
	d := testDeps(t)
	d.rand = bytesReader{bytes.Repeat([]byte{0x5a}, 16)}
	root := newRoot(t)
	_, err := d.init(bg, initOpts(root, "localhost"))
	wantCode(t, err, contract.CodeInternal, "CA and server serials collided")
	for rel, b := range snapshot(t, root) {
		if b != nil {
			t.Fatalf("%s written after a serial collision", rel)
		}
	}
	// The published server serial is the checked draw: with a sequence of
	// distinct serials, CA gets the first and the server the second.
	d = testDeps(t)
	var draws [][]byte
	d.rand = readerFunc(func(p []byte) (int, error) {
		n, err := rand.Read(p)
		draws = append(draws, append([]byte(nil), p...))
		return n, err
	})
	root = newRoot(t)
	mustInit(t, d, root, "localhost")
	if len(draws) != 2 || readCA(t, root).SerialNumber.Cmp(new(big.Int).SetBytes(draws[0])) != 0 ||
		readServer(t, root).SerialNumber.Cmp(new(big.Int).SetBytes(draws[1])) != 0 {
		t.Fatalf("serial draws %d do not match the published serials", len(draws))
	}
	// Reissue refuses a serial equal to the CA's or the current server's.
	snap := snapshot(t, root)
	for _, c := range []*x509.Certificate{readCA(t, root), readServer(t, root)} {
		d := testDeps(t)
		d.rand = bytesReader{c.SerialNumber.FillBytes(make([]byte, 16))}
		_, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"localhost"}})
		wantCode(t, err, contract.CodeInternal, "collided with an existing serial")
		sameSnapshot(t, snap, snapshot(t, root))
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
