package plane

import (
	"context"
	"crypto/x509"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// warnWindow is the inclusive expiry warning window (Q14).
const warnWindow = 30 * 24 * time.Hour

// certWarning returns at most one condition for c at now, with precedence
// not yet valid, expired, expires soon.
func certWarning(now time.Time, which string, c *x509.Certificate) (Warning, bool) {
	switch {
	case now.Before(c.NotBefore):
		return Warning{Certificate: which, Condition: ConditionNotYetValid, At: c.NotBefore.UTC()}, true
	case !now.Before(c.NotAfter):
		return Warning{Certificate: which, Condition: ConditionExpired, At: c.NotAfter.UTC()}, true
	case c.NotAfter.Sub(now) <= warnWindow:
		return Warning{Certificate: which, Condition: ConditionExpiresSoon, At: c.NotAfter.UTC()}, true
	}
	return Warning{}, false
}

// warnings lists the CA condition then the server condition.
func (m *material) warnings(now time.Time) []Warning {
	var out []Warning
	if w, ok := certWarning(now, "ca", m.caCert); ok {
		out = append(out, w)
	}
	if w, ok := certWarning(now, "server", m.serverCert); ok {
		out = append(out, w)
	}
	return out
}

// status builds the public report for m under layout l.
func (l layout) status(m *material, now time.Time) Status {
	s := sansOf(m.serverCert)
	dns := s.dns
	if dns == nil {
		dns = []string{}
	}
	return Status{
		StateDir:        l.root,
		Bind:            m.bind.String(),
		CACertPath:      l.path(caCertName),
		CAKeyPath:       l.path(caKeyName),
		ServerCertPath:  l.path(serverCertName),
		ServerKeyPath:   l.path(serverKeyName),
		DNSNames:        dns,
		IPAddresses:     s.ipStrings(),
		CAFingerprint:   Fingerprint(m.caCert.Raw),
		CANotBefore:     m.caCert.NotBefore.UTC(),
		CANotAfter:      m.caCert.NotAfter.UTC(),
		ServerNotBefore: m.serverCert.NotBefore.UTC(),
		ServerNotAfter:  m.serverCert.NotAfter.UTC(),
		Warnings:        m.warnings(now),
	}
}

// inspect is offline, read-only status: it never locks, writes, repairs or
// reads private key contents (key modes are still checked). Expired or
// not-yet-valid certificates are reported with warnings, not errors.
func (d *deps) inspect(ctx context.Context, stateDir string) (Status, error) {
	if err := canceled(ctx); err != nil {
		return Status{}, err
	}
	l := layout{root: stateDir}
	s, err := l.scan()
	if err != nil {
		return Status{}, err
	}
	switch s.kind {
	case kindEmpty:
		return Status{}, errf(contract.CodeNotFound, "no initialized plane state in %s; run callsheet plane init", l.root)
	case kindPartial:
		return Status{}, l.partialError(s)
	}
	m, err := l.load(false)
	if err != nil {
		return Status{}, err
	}
	if _, err := l.loadNodeRecords(); err != nil {
		return Status{}, err
	}
	return l.status(m, d.clock()), nil
}
