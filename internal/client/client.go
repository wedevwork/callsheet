// Package client is the verified plane client (iteration 03): plane URL
// grammar, explicit CA or pinned CA trust, and the verified HTTPS and WSS
// transports used by enrollment, the sidecar's node stream and the
// operator's roster commands. Verification is always on: there are no
// redirects, ambient proxies, cookies, system roots, plaintext fallback or
// switch that disables verification.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Bounds of every HTTP operation.
const (
	// OperationTimeout is the total deadline of one HTTP operation,
	// dialing, handshake, bootstrap and response reading included.
	OperationTimeout = 10 * time.Second
	// maxCertInput bounds a CA file or CA response.
	maxCertInput = 64 << 10
	// maxErrorBody bounds an error response.
	maxErrorBody = 64 << 10
	// maxRosterBody bounds a successful roster response.
	maxRosterBody = 8 << 20
)

// operationTimeout is OperationTimeout; tests shorten it to observe the
// bound without waiting ten seconds.
var operationTimeout = OperationTimeout

// untrusted is the literal AC-TLS-3 phrase every trust failure carries.
const untrusted = "connection not trusted"

// NormalizePlaneURL validates and canonicalizes a plane origin: https, a
// DNS name (lowercased, one trailing dot removed) or a literal IP (IPv6 in
// brackets, no zone), an optional port 1-65535 (443 is the default and is
// omitted), and an empty path or "/". Userinfo, queries, fragments,
// opaque URLs and other paths are rejected as invalid_argument.
func NormalizePlaneURL(raw string) (string, error) {
	bad := func(format string, args ...any) (string, error) {
		return "", contract.New(contract.CodeInvalidArgument, "invalid --plane URL: "+fmt.Sprintf(format, args...))
	}
	if raw == "" {
		return bad("empty; want https://HOST[:PORT]")
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] >= 0x7f {
			return bad("it contains spaces, control or non-ASCII characters")
		}
	}
	if strings.ContainsAny(raw, "?#") {
		return bad("queries and fragments are not allowed")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bad("cannot be parsed")
	}
	switch {
	case u.Scheme != "https":
		return bad("the scheme must be https (plaintext is never used)")
	case u.Opaque != "":
		return bad("want https://HOST[:PORT]")
	case u.User != nil:
		return bad("user information is not allowed")
	case u.Path != "" && u.Path != "/":
		return bad("paths are not allowed; give only the plane origin")
	case u.RawPath != "":
		return bad("paths are not allowed; give only the plane origin")
	}
	host, port := u.Hostname(), u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return bad("empty port")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if len(port) > 5 || strings.Trim(port, "0123456789") != "" || err != nil || n < 1 || n > 65535 {
			return bad("port %q must be a number 1-65535", port)
		}
		port = strconv.Itoa(n)
	}
	var canon string
	if strings.HasPrefix(u.Host, "[") {
		a, err := netip.ParseAddr(host)
		if err != nil || !a.Is6() || a.Zone() != "" {
			return bad("the bracketed host must be an IPv6 address without a zone")
		}
		canon = "[" + a.String() + "]"
	} else if a, err := netip.ParseAddr(host); err == nil {
		if !a.Is4() {
			return bad("IPv6 addresses must be in brackets")
		}
		canon = a.String()
	} else {
		name, err := normalizeHost(host)
		if err != nil {
			return bad("host: %v", err)
		}
		canon = name
	}
	if port != "" && port != "443" {
		canon += ":" + port
	}
	return "https://" + canon, nil
}

// normalizeHost accepts ASCII DNS names: labels of 1-63 letters, digits
// and internal hyphens, at most 253 characters without one trailing dot.
func normalizeHost(h string) (string, error) {
	name := strings.TrimSuffix(h, ".")
	if name == "" || len(name) > 253 {
		return "", errors.New("want a DNS name or IP address")
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return "", errors.New("each DNS label must be 1-63 characters")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case c == '-' && i > 0 && i < len(label)-1:
			default:
				return "", errors.New("DNS names use ASCII letters, digits and internal hyphens only")
			}
		}
	}
	return strings.ToLower(name), nil
}

// endpoint is a normalized plane URL split for dialing and verification.
type endpoint struct {
	url  string // https://host[:port]
	host string // verification name: DNS name or IP, no brackets
	addr string // host:port for dialing
}

func parseEndpoint(planeURL string) (endpoint, error) {
	norm, err := NormalizePlaneURL(planeURL)
	if err != nil {
		return endpoint{}, err
	}
	u, _ := url.Parse(norm)
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return endpoint{url: norm, host: u.Hostname(), addr: net.JoinHostPort(u.Hostname(), port)}, nil
}

// Client is a verified plane client for one normalized URL and one CA.
// Its transports always verify the server with that CA only.
type Client struct {
	ep    endpoint
	trust Trust
	tr    *http.Transport
	http  *http.Client
}

// New revalidates the URL and trust and builds the private verified
// transports. It never contacts the plane.
func New(planeURL string, trust Trust) (*Client, error) {
	ep, err := parseEndpoint(planeURL)
	if err != nil {
		return nil, err
	}
	ca, err := parseCAPEM(trust.CAPEM, "the configured CA")
	if err != nil {
		return nil, err
	}
	fp := Fingerprint(ca.Raw)
	if trust.Fingerprint != "" && trust.Fingerprint != fp {
		return nil, contract.New(contract.CodeTrustFailed, untrusted+": the configured CA does not match its recorded fingerprint")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	tr := newTransport(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	return &Client{
		ep:    ep,
		trust: Trust{CAPEM: trust.CAPEM, Fingerprint: fp},
		tr:    tr,
		http:  newHTTPClient(tr),
	}, nil
}

// URL is the normalized plane URL.
func (c *Client) URL() string { return c.ep.url }

// Trust is the client's verified trust material.
func (c *Client) Trust() Trust { return c.trust }

// Close closes idle HTTP connections.
func (c *Client) Close() { c.tr.CloseIdleConnections() }

// newTransport is the only transport constructor: no proxy, no HTTP/2
// upgrade, bounded dialing and handshake.
func newTransport(cfg *tls.Config) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: operationTimeout}).DialContext,
		TLSClientConfig:       cfg,
		TLSHandshakeTimeout:   operationTimeout,
		ResponseHeaderTimeout: operationTimeout,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
}

// errRedirect refuses every redirect.
var errRedirect = errors.New("the plane answered with a redirect; redirects are never followed")

func newHTTPClient(tr *http.Transport) *http.Client {
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect }}
}

// transportError classifies a request or dial failure: the caller's own
// cancellation is returned unchanged (exit 130), verification failures are
// trust_failed with the literal phrase, everything else (refused, reset,
// EOF, timeouts) is unavailable. A plain timeout is never trust_failed.
func (c *Client) transportError(parent context.Context, err error) error {
	if perr := parent.Err(); perr != nil {
		return perr
	}
	return classify(c.ep, err)
}

func classify(ep endpoint, err error) error {
	var ce *contract.Error
	if errors.As(err, &ce) {
		return ce
	}
	if errors.Is(err, errRedirect) {
		return contract.Wrap(contract.CodeInvalidArgument, "the plane at "+ep.url+" answered with a redirect; redirects are never followed", err)
	}
	if reason, ok := trustReason(ep, err); ok {
		return contract.Wrap(contract.CodeTrustFailed, untrusted+": "+reason, err)
	}
	msg := "cannot reach the plane at " + ep.url
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		msg += ": timed out"
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		msg += ": the connection was closed"
	default:
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Err != nil {
			msg += ": " + contract.SafeText(oe.Err.Error(), 200)
		}
	}
	return contract.Wrap(contract.CodeUnavailable, msg, err)
}

// trustReason recognizes certificate verification failures and returns a
// safe, actionable reason (never peer-controlled text).
func trustReason(ep endpoint, err error) (string, bool) {
	var pe *pinError
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	var ve *tls.CertificateVerificationError
	var rh tls.RecordHeaderError
	switch {
	case errors.As(err, &pe):
		return pe.reason, true
	case errors.As(err, &he):
		return "the plane's certificate is not valid for " + ep.host + "; use a --plane host that the plane's certificate names (see callsheet plane status)", true
	case errors.As(err, &ua):
		return "the plane's certificate is not signed by the supplied CA; check --ca or --ca-fingerprint", true
	case errors.As(err, &ci):
		if ci.Reason == x509.Expired {
			return "the plane's certificate or CA is expired or not yet valid; check the clocks, or reissue", true
		}
		return "the plane's certificate is not valid for server authentication", true
	case errors.As(err, &ve):
		return "the plane's certificate failed verification", true
	case errors.As(err, &rh):
		return "the plane did not answer with TLS", true
	}
	return "", false
}

// request performs one bounded, versioned request. It returns the status
// and body of a 2xx response (read up to limit), or the plane's contract
// error for any other status. The protocol header is validated before any
// success body is consumed.
func (c *Client) request(ctx context.Context, method, path string, body []byte, limit int64) (int, []byte, error) {
	octx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(octx, method, c.ep.url+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, contract.Wrap(contract.CodeInternal, "cannot build the request", err)
	}
	req.Header.Set(contract.ProtocolHeader, strconv.Itoa(contract.ProtocolVersion))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, c.transportError(ctx, err)
	}
	defer resp.Body.Close()
	if err := checkResponseVersion(resp.Header); err != nil {
		return 0, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := readLimited(resp.Body, maxErrorBody)
		return resp.StatusCode, nil, responseError(c.ep, resp.StatusCode, b)
	}
	b, err := readLimited(resp.Body, limit)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return 0, nil, contract.New(contract.CodeInvalidArgument, fmt.Sprintf("the plane's response is larger than %d bytes", limit))
		}
		return 0, nil, c.transportError(ctx, err)
	}
	return resp.StatusCode, b, nil
}

// checkResponseVersion requires exactly one integer protocol header equal
// to this build's (reported local=client, remote=server).
func checkResponseVersion(h http.Header) error {
	vals := h.Values(contract.ProtocolHeader)
	if len(vals) != 1 {
		return contract.New(contract.CodeInvalidArgument, "the plane's response lacks exactly one "+contract.ProtocolHeader+" header; is it a callsheet plane of this version?")
	}
	n, ok := contract.ParseInteger(strings.TrimSpace(vals[0]))
	if !ok {
		return contract.New(contract.CodeInvalidArgument, "the plane's "+contract.ProtocolHeader+" header is not an integer")
	}
	if n != contract.ProtocolVersion {
		return contract.VersionMismatch(contract.ProtocolVersion, n)
	}
	return nil
}

// responseError converts an error response into the plane's contract
// error (sanitized), or a generic error for a malformed one.
func responseError(ep endpoint, status int, body []byte) error {
	if e, err := contract.ParseErrorBody(bytes.TrimSpace(body)); err == nil {
		if e.Code == contract.CodeProtocolMismatch {
			// Present the pair from this side: local=client.
			if remote, ok := e.DetailInt("local_version"); ok {
				return contract.VersionMismatch(contract.ProtocolVersion, remote)
			}
		}
		return &contract.Error{Code: e.Code, Message: e.Message, Details: e.Details}
	}
	code := contract.CodeInternal
	if status == http.StatusServiceUnavailable {
		code = contract.CodeUnavailable
	}
	return contract.New(code, fmt.Sprintf("the plane at %s answered HTTP %d without a valid error body", ep.url, status))
}

var errTooLarge = errors.New("response too large")

// readLimited reads at most limit bytes and fails beyond it, so a response
// is never truncated into a partial result.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errTooLarge
	}
	return b, nil
}
