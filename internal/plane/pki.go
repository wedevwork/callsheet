package plane

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

const (
	caCommonName     = "Callsheet internal CA"
	serverCommonName = "Callsheet plane"
	// backdate tolerates small clock differences between machines.
	backdate  = 5 * time.Minute
	caYears   = 10
	leafYears = 2
	maxSANs   = 100
	maxSANLen = 253
	pemCert   = "CERTIFICATE"
	pemKey    = "PRIVATE KEY"
)

// sanSet is a normalized SAN list: DNS names lowercased without a trailing
// dot, and IPs unmapped; both sorted bytewise on their canonical strings
// and deduplicated.
type sanSet struct {
	dns []string
	ips []netip.Addr
}

func (s sanSet) ipStrings() []string {
	out := make([]string, len(s.ips))
	for i, a := range s.ips {
		out[i] = a.String()
	}
	return out
}

func (s sanSet) netIPs() []net.IP {
	out := make([]net.IP, len(s.ips))
	for i, a := range s.ips {
		out[i] = net.IP(a.AsSlice())
	}
	return out
}

func (s sanSet) equal(o sanSet) bool {
	return slices.Equal(s.dns, o.dns) && slices.Equal(s.ipStrings(), o.ipStrings())
}

func (s sanSet) String() string {
	all := append(append([]string(nil), s.dns...), s.ipStrings()...)
	return strings.Join(all, ",")
}

func normalizeSANs(in []string) (sanSet, error) {
	if len(in) == 0 {
		return sanSet{}, errf(contract.CodeInvalidArgument, "at least one --san is required")
	}
	dns := map[string]bool{}
	ips := map[netip.Addr]bool{}
	for _, v := range in {
		if len(v) > maxSANLen {
			return sanSet{}, errf(contract.CodeInvalidArgument, "invalid --san: value of %d bytes exceeds %d", len(v), maxSANLen)
		}
		if a, err := netip.ParseAddr(v); err == nil {
			if a.Zone() != "" {
				return sanSet{}, errf(contract.CodeInvalidArgument, "invalid --san %q: IPv6 zones are not allowed", v)
			}
			a = a.Unmap()
			if a.IsUnspecified() || a.IsMulticast() {
				return sanSet{}, errf(contract.CodeInvalidArgument, "invalid --san %q: unspecified and multicast addresses cannot identify a server", v)
			}
			ips[a] = true
			continue
		}
		name, err := normalizeDNS(v)
		if err != nil {
			return sanSet{}, errf(contract.CodeInvalidArgument, "invalid --san %q: %v", v, err)
		}
		dns[name] = true
	}
	if n := len(dns) + len(ips); n > maxSANs {
		return sanSet{}, errf(contract.CodeInvalidArgument, "too many --san values: %d unique, at most %d", n, maxSANs)
	}
	var s sanSet
	for n := range dns {
		s.dns = append(s.dns, n)
	}
	slices.Sort(s.dns)
	for a := range ips {
		s.ips = append(s.ips, a)
	}
	slices.SortFunc(s.ips, func(a, b netip.Addr) int { return strings.Compare(a.String(), b.String()) })
	return s, nil
}

// normalizeDNS accepts ASCII hostnames: labels of 1-63 letters, digits and
// internal hyphens, at most 253 characters after removing one trailing
// dot, lowercased. Single labels and localhost are allowed.
func normalizeDNS(v string) (string, error) {
	name := strings.TrimSuffix(v, ".")
	switch {
	case name == "":
		return "", errors.New("empty name")
	case len(name) > maxSANLen:
		return "", fmt.Errorf("name longer than %d characters", maxSANLen)
	case strings.Contains(name, "*"):
		return "", errors.New("wildcard names are not allowed")
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return "", errors.New("each label must be 1-63 characters")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case c == '-' && i > 0 && i < len(label)-1:
			default:
				return "", errors.New("names use ASCII letters, digits and internal hyphens only (supply Unicode names as punycode; no URLs, ports or spaces)")
			}
		}
	}
	return strings.ToLower(name), nil
}

// sansOf reads a certificate's SANs in normalized form.
func sansOf(c *x509.Certificate) sanSet {
	var s sanSet
	s.dns = append(s.dns, c.DNSNames...)
	slices.Sort(s.dns)
	for _, ip := range c.IPAddresses {
		if a, ok := netip.AddrFromSlice(ip); ok {
			s.ips = append(s.ips, a.Unmap())
		}
	}
	slices.SortFunc(s.ips, func(a, b netip.Addr) int { return strings.Compare(a.String(), b.String()) })
	return s
}

// material is loaded or generated trust state.
type material struct {
	bind       netip.AddrPort
	caCert     *x509.Certificate
	serverCert *x509.Certificate
	caKey      *ecdsa.PrivateKey
	serverKey  *ecdsa.PrivateKey
}

// Fingerprint is "sha256:" plus the lowercase hex SHA-256 of the
// certificate's DER bytes.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// serial returns a random nonzero serial below 2^128.
func (d *deps) serial() (*big.Int, error) {
	for range 8 {
		n, err := randInt(d)
		if err != nil {
			return nil, err
		}
		if n.Sign() != 0 {
			return n, nil
		}
	}
	return nil, errors.New("entropy source returned only zero serials")
}

func randInt(d *deps) (*big.Int, error) {
	buf := make([]byte, 16)
	if _, err := readFull(d, buf); err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(buf), nil
}

func readFull(d *deps, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := d.rand.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func entropyErr(err error) error {
	return wrapf(contract.CodeInternal, err, "entropy source failed: %v; no state was written", err)
}

func caTemplate(serial *big.Int, now time.Time) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.AddDate(caYears, 0, 0),
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
}

func serverTemplate(serial *big.Int, now time.Time, sans sanSet) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: serverCommonName},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.AddDate(leafYears, 0, 0),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              sans.dns,
		IPAddresses:           sans.netIPs(),
	}
}

// generate issues a new CA and server credential in memory and returns
// the verified material plus every durable file's bytes.
func (d *deps) generate(now time.Time, bind netip.AddrPort, sans sanSet) (*material, map[string][]byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), d.rand)
	if err != nil {
		return nil, nil, entropyErr(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), d.rand)
	if err != nil {
		return nil, nil, entropyErr(err)
	}
	caSerial, err := d.serial()
	if err != nil {
		return nil, nil, entropyErr(err)
	}
	serverSerial, err := d.serial()
	if err != nil {
		return nil, nil, entropyErr(err)
	}
	if caSerial.Cmp(serverSerial) == 0 {
		return nil, nil, entropyErr(errors.New("CA and server serials collided"))
	}
	tmpl := caTemplate(caSerial, now)
	caDER, err := x509.CreateCertificate(d.rand, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, wrapf(contract.CodeInternal, err, "cannot create the CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, wrapf(contract.CodeInternal, err, "cannot parse the new CA certificate: %v", err)
	}
	serverCert, serverDER, err := d.issueServer(now, serverSerial, sans, caCert, caKey, &serverKey.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	m := &material{bind: bind, caCert: caCert, serverCert: serverCert, caKey: caKey, serverKey: serverKey}
	if err := m.validate(true); err != nil {
		return nil, nil, err
	}
	if err := m.verifyAt(now); err != nil {
		return nil, nil, err
	}
	caKeyPEM, err := keyPEM(caKey)
	if err != nil {
		return nil, nil, err
	}
	serverKeyPEM, err := keyPEM(serverKey)
	if err != nil {
		return nil, nil, err
	}
	files := map[string][]byte{
		caCertName:     certPEM(caDER),
		caKeyName:      caKeyPEM,
		serverCertName: certPEM(serverDER),
		serverKeyName:  serverKeyPEM,
		configName:     encodeConfig(bind),
	}
	return m, files, nil
}

// issueServer signs a server certificate with the given, already checked
// serial for pub under the CA.
func (d *deps) issueServer(now time.Time, serial *big.Int, sans sanSet, ca *x509.Certificate, caKey *ecdsa.PrivateKey, pub *ecdsa.PublicKey) (*x509.Certificate, []byte, error) {
	der, err := x509.CreateCertificate(d.rand, serverTemplate(serial, now, sans), ca, pub, caKey)
	if err != nil {
		return nil, nil, wrapf(contract.CodeInternal, err, "cannot create the server certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, wrapf(contract.CodeInternal, err, "cannot parse the new server certificate: %v", err)
	}
	return cert, der, nil
}

func certPEM(der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: pemCert, Bytes: der}) }

func keyPEM(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot encode a private key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemKey, Bytes: der}), nil
}

// decodePEM requires exactly one PEM block of type typ, with only
// whitespace around it and no headers.
func decodePEM(data []byte, typ string) ([]byte, error) {
	start := bytes.Index(data, []byte("-----BEGIN "))
	if start < 0 {
		return nil, errors.New("no PEM block")
	}
	if len(bytes.TrimSpace(data[:start])) != 0 {
		return nil, errors.New("data before the PEM block")
	}
	block, rest := pem.Decode(data[start:])
	switch {
	case block == nil:
		return nil, errors.New("malformed PEM block")
	case block.Type != typ:
		return nil, fmt.Errorf("PEM block type %q, want %q", block.Type, typ)
	case len(block.Headers) != 0:
		return nil, errors.New("PEM headers are not allowed")
	case len(bytes.TrimSpace(rest)) != 0:
		return nil, errors.New("extra PEM blocks or trailing data")
	}
	return block.Bytes, nil
}

func isP256(pub any) bool {
	k, ok := pub.(*ecdsa.PublicKey)
	return ok && k.Curve == elliptic.P256()
}

// loadCert reads and parses one PEM certificate file.
func loadCert(p string) (*x509.Certificate, error) {
	b, err := readBounded(p)
	if err != nil && !errors.Is(err, errTooLarge) {
		return nil, wrapf(contract.CodeInternal, err, "cannot read %s: %v", p, err)
	}
	if err == nil {
		var der []byte
		if der, err = decodePEM(b, pemCert); err == nil {
			var c *x509.Certificate
			if c, err = x509.ParseCertificate(der); err == nil {
				if isP256(c.PublicKey) {
					return c, nil
				}
				err = errors.New("unsupported public key type (want ECDSA P-256)")
			}
		}
	}
	return nil, wrapf(contract.CodeTrustFailed, err, "invalid certificate %s: %v", p, err)
}

// loadKey reads and parses one PEM PKCS#8 ECDSA P-256 private key file.
// Diagnostics never include key material.
func loadKey(p string) (*ecdsa.PrivateKey, error) {
	b, err := readBounded(p)
	if err != nil && !errors.Is(err, errTooLarge) {
		return nil, wrapf(contract.CodeInternal, err, "cannot read %s: %v", p, err)
	}
	if err == nil {
		var der []byte
		if der, err = decodePEM(b, pemKey); err == nil {
			var k any
			if k, err = x509.ParsePKCS8PrivateKey(der); err == nil {
				if ek, ok := k.(*ecdsa.PrivateKey); ok && ek.Curve == elliptic.P256() {
					return ek, nil
				}
				err = errors.New("unsupported private key type (want ECDSA P-256)")
			} else {
				err = errors.New("not a PKCS#8 private key")
			}
		}
	}
	return nil, wrapf(contract.CodeTrustFailed, err, "invalid private key %s: %v", p, err)
}

// load reads config and the certificate chain, plus both private keys when
// withKeys, and validates their time-independent structure.
func (l layout) load(withKeys bool) (*material, error) {
	bind, err := l.loadConfig()
	if err != nil {
		return nil, err
	}
	m := &material{bind: bind}
	if m.caCert, err = loadCert(l.path(caCertName)); err != nil {
		return nil, err
	}
	if m.serverCert, err = loadCert(l.path(serverCertName)); err != nil {
		return nil, err
	}
	if withKeys {
		if m.caKey, err = loadKey(l.path(caKeyName)); err != nil {
			return nil, err
		}
		if m.serverKey, err = loadKey(l.path(serverKeyName)); err != nil {
			return nil, err
		}
	}
	if err := m.validate(withKeys); err != nil {
		return nil, wrapf(contract.CodeTrustFailed, err, "invalid trust in %s: %v", l.root, err)
	}
	return m, nil
}

// validate checks CA constraints, the server certificate's issuer
// signature and usage, and (withKeys) that each key matches its
// certificate. Time is checked separately.
func (m *material) validate(withKeys bool) error {
	ca, srv := m.caCert, m.serverCert
	switch {
	case !ca.BasicConstraintsValid || !ca.IsCA:
		return errors.New("ca.crt is not a CA certificate")
	case ca.MaxPathLen != 0:
		return errors.New("ca.crt must have path length zero")
	case ca.KeyUsage&x509.KeyUsageCertSign == 0:
		return errors.New("ca.crt lacks certificate-signing usage")
	}
	if err := ca.CheckSignatureFrom(ca); err != nil {
		return fmt.Errorf("ca.crt is not validly self-signed: %v", err)
	}
	switch {
	case srv.IsCA:
		return errors.New("server.crt must not be a CA certificate")
	case srv.KeyUsage&x509.KeyUsageDigitalSignature == 0:
		return errors.New("server.crt lacks digital-signature usage")
	case !slices.Equal(srv.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}):
		return errors.New("server.crt must have only server-authentication extended usage")
	}
	if err := srv.CheckSignatureFrom(ca); err != nil {
		return fmt.Errorf("server.crt was not issued by ca.crt: %v", err)
	}
	if withKeys {
		if !m.caKey.PublicKey.Equal(ca.PublicKey) {
			return errors.New("ca.key does not match ca.crt")
		}
		if !m.serverKey.PublicKey.Equal(srv.PublicKey) {
			return errors.New("server.key does not match server.crt")
		}
	}
	return nil
}

// verifyAt checks both certificates are within their validity at now and
// that the server certificate chains to the CA for server authentication.
func (m *material) verifyAt(now time.Time) error {
	for _, c := range []struct {
		name string
		cert *x509.Certificate
	}{{"ca", m.caCert}, {"server", m.serverCert}} {
		if now.Before(c.cert.NotBefore) {
			return errf(contract.CodeTrustFailed, "the %s certificate is not yet valid (valid from %s); check the system clock", c.name, rfc3339(c.cert.NotBefore))
		}
		if !now.Before(c.cert.NotAfter) {
			return errf(contract.CodeTrustFailed, "the %s certificate expired at %s", c.name, rfc3339(c.cert.NotAfter))
		}
	}
	roots := x509.NewCertPool()
	roots.AddCert(m.caCert)
	if _, err := m.serverCert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return wrapf(contract.CodeTrustFailed, err, "server.crt does not verify against ca.crt: %v", err)
	}
	return nil
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
