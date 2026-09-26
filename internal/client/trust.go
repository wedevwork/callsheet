package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TrustOptions are the operator's trust inputs: the plane URL and exactly
// one of CAFile (--ca) or CAFingerprint (--ca-fingerprint).
type TrustOptions struct {
	PlaneURL      string
	CAFile        string
	CAFingerprint string
}

// Trust is verified trust material: the plane's CA certificate (one PEM
// block) and its fingerprint.
type Trust struct {
	CAPEM       []byte
	Fingerprint string
}

var pinRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Fingerprint is "sha256:" plus the lowercase hex SHA-256 of DER bytes,
// the format plane init prints.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidPin reports whether s has the exact fingerprint format.
func ValidPin(s string) bool { return pinRE.MatchString(s) }

func untrustedErr(reason string) *contract.Error {
	return contract.New(contract.CodeTrustFailed, untrusted+": "+reason)
}

// ResolveTrust implements the two mutually exclusive trust paths and
// proves trust before any application operation. With CAFile it loads the
// CA and completes a verified TLS handshake with the plane (CA, hostname,
// server usage and validity). With CAFingerprint it performs the pinned
// fetch-once bootstrap (FetchCA). Syntax errors (both inputs, a bad URL or
// pin) are invalid_argument; no trust input is trust_failed.
func ResolveTrust(ctx context.Context, o TrustOptions) (Trust, error) {
	ep, err := parseEndpoint(o.PlaneURL)
	if err != nil {
		return Trust{}, err
	}
	switch {
	case o.CAFile != "" && o.CAFingerprint != "":
		return Trust{}, contract.New(contract.CodeInvalidArgument, "give only one of --ca and --ca-fingerprint")
	case o.CAFingerprint != "":
		return FetchCA(ctx, ep.url, o.CAFingerprint)
	case o.CAFile == "":
		return Trust{}, untrustedErr("supply --ca or --ca-fingerprint")
	}
	ca, err := LoadCAFile(o.CAFile)
	if err != nil {
		return Trust{}, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if err := handshake(ctx, ep, &tls.Config{RootCAs: pool, ServerName: ep.host, MinVersion: tls.VersionTLS12}); err != nil {
		return Trust{}, err
	}
	return Trust{CAPEM: pemOf(ca.Raw), Fingerprint: Fingerprint(ca.Raw)}, nil
}

// handshake completes one bounded TLS handshake with cfg and closes it.
func handshake(ctx context.Context, ep endpoint, cfg *tls.Config) error {
	octx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: operationTimeout}, Config: cfg}
	conn, err := d.DialContext(octx, "tcp", ep.addr)
	if err != nil {
		if perr := ctx.Err(); perr != nil {
			return perr
		}
		return classify(ep, err)
	}
	return conn.Close()
}

func pemOf(der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}) }

// LoadCAFile reads --ca: a regular file (a symlink or other type is
// refused), at most 64 KiB, exactly one PEM CERTIFICATE (trailing
// whitespace allowed) that is a valid self-signed CA with certificate
// signing. Every failure is trust_failed with the literal phrase; the
// diagnostic never quotes the file's content. Private-key modes are not
// required: it is public.
func LoadCAFile(path string) (*x509.Certificate, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, untrustedErr(fmt.Sprintf("the --ca file %s does not exist", path))
	case err != nil:
		return nil, contract.Wrap(contract.CodeTrustFailed, untrusted+": cannot read the --ca file "+path, err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, untrustedErr(fmt.Sprintf("the --ca file %s is a symbolic link; give the certificate file itself", path))
	case !fi.Mode().IsRegular():
		return nil, untrustedErr(fmt.Sprintf("the --ca file %s is not a regular file", path))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, contract.Wrap(contract.CodeTrustFailed, untrusted+": cannot read the --ca file "+path, err)
	}
	defer f.Close()
	b, err := readLimited(f, maxCertInput)
	if err != nil {
		return nil, untrustedErr(fmt.Sprintf("the --ca file %s is unreadable or larger than 64 KiB", path))
	}
	return parseCAPEM(b, "the --ca file "+path)
}

// parseCAPEM requires exactly one PEM CERTIFICATE (only trailing
// whitespace after it) holding a valid self-signed CA with CertSign.
func parseCAPEM(b []byte, what string) (*x509.Certificate, error) {
	block, rest := pem.Decode(b)
	switch {
	case block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0:
		return nil, untrustedErr(what + " is not one PEM CERTIFICATE")
	case len(bytes.TrimSpace(rest)) != 0:
		return nil, untrustedErr(what + " holds more than one PEM block or trailing data")
	case len(bytes.TrimSpace(b[:bytes.Index(b, []byte("-----BEGIN"))])) != 0:
		return nil, untrustedErr(what + " has data before the PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, untrustedErr(what + " is not a parseable certificate")
	}
	if err := checkCA(c); err != nil {
		return nil, untrustedErr(what + " " + err.Error())
	}
	return c, nil
}

func checkCA(c *x509.Certificate) error {
	switch {
	case !c.BasicConstraintsValid || !c.IsCA:
		return errors.New("is not a CA certificate")
	case c.KeyUsage&x509.KeyUsageCertSign == 0:
		return errors.New("lacks certificate-signing usage")
	case !bytes.Equal(c.RawIssuer, c.RawSubject):
		return errors.New("is not self-signed")
	}
	if err := c.CheckSignatureFrom(c); err != nil {
		return errors.New("is not validly self-signed")
	}
	return nil
}

// pinError is a pinned-bootstrap verification failure.
type pinError struct{ reason string }

func (e *pinError) Error() string { return e.reason }

// pinVerifier is the mandatory bootstrap verifier: it finds the single
// self-signed CA in the presented chain, compares its DER's SHA-256 with
// pin, and verifies the leaf with only that CA, the URL host, server
// authentication and the current time. It records the verified CA.
type pinVerifier struct {
	pin  string
	host string
	now  func() time.Time

	mu sync.Mutex
	ca *x509.Certificate
}

func (v *pinVerifier) verify(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return &pinError{"the plane presented no certificate"}
	}
	var roots []*x509.Certificate
	for _, c := range cs.PeerCertificates[1:] {
		if checkCA(c) == nil {
			roots = append(roots, c)
		}
	}
	switch len(roots) {
	case 0:
		return &pinError{"the plane's TLS chain contains no self-signed CA to compare with --ca-fingerprint"}
	case 1:
	default:
		return &pinError{"the plane's TLS chain contains more than one self-signed CA"}
	}
	ca := roots[0]
	if Fingerprint(ca.Raw) != v.pin {
		return &pinError{"the plane's CA does not match --ca-fingerprint"}
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, DNSName: v.host, CurrentTime: v.now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		var he x509.HostnameError
		if errors.As(err, &he) {
			return &pinError{"the plane's certificate is not valid for " + v.host + "; use a --plane host that the plane's certificate names"}
		}
		var ci x509.CertificateInvalidError
		if errors.As(err, &ci) && ci.Reason == x509.Expired {
			return &pinError{"the plane's certificate or CA is expired or not yet valid; check the clocks, or reissue"}
		}
		return &pinError{"the plane's certificate does not verify against the pinned CA"}
	}
	v.mu.Lock()
	v.ca = ca
	v.mu.Unlock()
	return nil
}

func (v *pinVerifier) verified() *x509.Certificate {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.ca
}

// bootstrapConfig is the only TLS configuration that replaces Go's
// built-in verifier, and it always installs the complete pin, chain and
// hostname verifier; there is no way to build it without a valid pin, and
// session resumption is disabled so every connection is verified afresh.
// InsecureSkipVerify only hands verification to VerifyConnection here;
// it is not unverified TLS.
func bootstrapConfig(v *pinVerifier) (*tls.Config, error) {
	if !ValidPin(v.pin) {
		return nil, contract.New(contract.CodeInvalidArgument, "the pinned bootstrap requires a sha256: fingerprint")
	}
	return &tls.Config{
		MinVersion:             tls.VersionTLS12,
		ServerName:             v.host,
		InsecureSkipVerify:     true,
		VerifyConnection:       v.verify,
		SessionTicketsDisabled: true,
	}, nil
}

// FetchCA is the mandatory pinned fetch-once bootstrap. It completes the
// pinned handshake first (bootstrapConfig), only then sends
// GET /api/v1/ca, and requires status 200 and a bounded single-CA PEM whose
// DER equals the pinned handshake CA. Later requests use normal
// verification with the returned CA.
func FetchCA(ctx context.Context, planeURL, fingerprint string) (Trust, error) {
	ep, err := parseEndpoint(planeURL)
	if err != nil {
		return Trust{}, err
	}
	if !ValidPin(fingerprint) {
		return Trust{}, contract.New(contract.CodeInvalidArgument, "invalid --ca-fingerprint: want sha256: followed by 64 lowercase hex digits, as printed by callsheet plane init")
	}
	v := &pinVerifier{pin: fingerprint, host: ep.host, now: time.Now}
	cfg, err := bootstrapConfig(v)
	if err != nil {
		return Trust{}, err
	}
	tr := newTransport(cfg)
	defer tr.CloseIdleConnections()
	octx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(octx, http.MethodGet, ep.url+contract.PathCA, nil)
	if err != nil {
		return Trust{}, contract.Wrap(contract.CodeInternal, "cannot build the request", err)
	}
	resp, err := newHTTPClient(tr).Do(req)
	if err != nil {
		if perr := ctx.Err(); perr != nil {
			return Trust{}, perr
		}
		return Trust{}, classify(ep, err)
	}
	defer resp.Body.Close()
	pinned := v.verified()
	if pinned == nil {
		// Unreachable: a response implies a verified handshake.
		return Trust{}, untrustedErr("the pinned handshake was not verified")
	}
	if resp.StatusCode != http.StatusOK {
		return Trust{}, untrustedErr(fmt.Sprintf("the plane answered the CA request with HTTP %d", resp.StatusCode))
	}
	b, err := readLimited(resp.Body, maxCertInput)
	if err != nil {
		if errors.Is(err, errTooLarge) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Trust{}, untrustedErr("the plane's CA response is unreadable or larger than 64 KiB")
		}
		if perr := ctx.Err(); perr != nil {
			return Trust{}, perr
		}
		return Trust{}, classify(ep, err)
	}
	ca, err := parseCAPEM(b, "the plane's CA response")
	if err != nil {
		return Trust{}, err
	}
	if !bytes.Equal(ca.Raw, pinned.Raw) {
		return Trust{}, untrustedErr("the plane's CA response differs from the CA it presented in the pinned handshake")
	}
	return Trust{CAPEM: pemOf(ca.Raw), Fingerprint: fingerprint}, nil
}
