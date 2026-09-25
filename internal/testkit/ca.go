package testkit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"time"
)

// FixtureCA is an in-memory ECDSA P-256 test CA. It is fixture code, not the
// plane's trust implementation.
type FixtureCA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	key     *ecdsa.PrivateKey
	now     func() time.Time
}

// ServerSANs are the leaf's subject alternative names.
var (
	ServerDNSNames = []string{"localhost"}
	ServerIPs      = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
)

// NewFixtureCA generates a fresh CA valid from now-1h to now+24h.
func NewFixtureCA() (*FixtureCA, error) {
	return newFixtureCA(time.Now)
}

func newFixtureCA(now func() time.Time) (*FixtureCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	t := now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "callsheet fixture CA"},
		NotBefore:             t.Add(-time.Hour),
		NotAfter:              t.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &FixtureCA{
		Cert:    cert,
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:     key,
		now:     now,
	}, nil
}

func randSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
}

// ServerCertificate issues a server leaf for localhost, 127.0.0.1 and ::1.
func (ca *FixtureCA) ServerCertificate() (tls.Certificate, error) {
	return ca.Leaf(ServerDNSNames, ServerIPs)
}

// Leaf issues a server certificate for the given SANs.
func (ca *FixtureCA) Leaf(dnsNames []string, ips []net.IP) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	t := ca.now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "callsheet fixture server"},
		NotBefore:    t.Add(-time.Hour),
		NotAfter:     t.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// Pool returns a new cert pool containing only this CA.
func (ca *FixtureCA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// Client returns a new verified HTTPS client trusting only this CA, with TLS
// 1.2 minimum, hostname verification and no environment proxy.
func (ca *FixtureCA) Client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12},
		Proxy:             nil,
		ForceAttemptHTTP2: false,
	}}
}
