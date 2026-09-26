package plane

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// request is the validated input of init and run.
type request struct {
	l       layout
	bind    netip.AddrPort
	bindSet bool
	sans    sanSet
	sansSet bool
}

// validate checks every operator input before any state is created: bind
// syntax and policy, interface membership for an explicit non-loopback
// bind, and SAN syntax.
func (d *deps) validate(stateDir, bind string, bindSet bool, sans []string, sansSet bool) (request, error) {
	r := request{l: layout{root: stateDir}, bindSet: bindSet, sansSet: sansSet}
	var err error
	if bindSet {
		if r.bind, err = parseBind(bind); err != nil {
			return r, wrapf(contract.CodeInvalidArgument, err, "invalid --bind %q: %v", bind, err)
		}
		if err := d.checkPresent(r.bind); err != nil {
			return r, err
		}
	} else {
		r.bind = netip.MustParseAddrPort(DefaultBind)
	}
	if sansSet {
		if r.sans, err = normalizeSANs(sans); err != nil {
			return r, err
		}
	}
	return r, nil
}

func needSANs(l layout) error {
	return errf(contract.CodeInvalidArgument, "no plane state in %s: new state requires at least one --san (names are never discovered automatically)", l.root)
}

// open validates inputs, creates the root when needed, takes the state
// lock and either initializes empty state or loads and checks complete
// state against any supplied inputs. The caller owns the returned lock.
// initialized reports whether this call created the state.
func (d *deps) open(ctx context.Context, now time.Time, stateDir, bind string, bindSet bool, sans []string, sansSet bool) (lk *stateLock, m *material, initialized bool, err error) {
	if err := canceled(ctx); err != nil {
		return nil, nil, false, err
	}
	r, err := d.validate(stateDir, bind, bindSet, sans, sansSet)
	if err != nil {
		return nil, nil, false, err
	}
	pre, err := r.l.scan()
	if err != nil {
		return nil, nil, false, err
	}
	if !pre.rootExists {
		if !r.sansSet {
			return nil, nil, false, needSANs(r.l)
		}
		if err := os.MkdirAll(r.l.root, dirMode); err != nil {
			return nil, nil, false, wrapf(contract.CodeInternal, err, "cannot create state directory: %v", err)
		}
	}
	lk, err = r.l.acquire()
	if err != nil {
		return nil, nil, false, err
	}
	defer func() {
		if err != nil {
			lk.release()
			lk = nil
		}
	}()
	s, err := r.l.scan()
	if err != nil {
		return lk, nil, false, err
	}
	switch s.kind {
	case kindEmpty:
		if !r.sansSet {
			return lk, nil, false, needSANs(r.l)
		}
		if _, err = d.initialize(ctx, r.l, now, r.bind, r.sans); err != nil {
			return lk, nil, false, err
		}
		// Serve and report exactly what was published.
		m, err = r.l.load(true)
		return lk, m, err == nil, err
	case kindPartial:
		return lk, nil, false, r.l.partialError(s)
	}
	if m, err = r.l.load(true); err != nil {
		return lk, nil, false, err
	}
	if r.bindSet && r.bind != m.bind {
		return lk, nil, false, errf(contract.CodeConflict, "--bind %s differs from the configured bind %s; init never edits existing configuration: stop the plane and edit %s to change it", r.bind, m.bind, r.l.path(configName))
	}
	if r.sansSet {
		if have := sansOf(m.serverCert); !have.equal(r.sans) {
			return lk, nil, false, errf(contract.CodeConflict, "--san list %s differs from the server certificate's SANs %s; use callsheet plane cert reissue to change them", r.sans, have)
		}
	}
	return lk, m, false, nil
}

func (d *deps) init(ctx context.Context, o InitOptions) (Status, error) {
	now := d.clock()
	lk, m, _, err := d.open(ctx, now, o.StateDir, o.Bind, o.BindSet, o.SANs, o.SANsSet)
	if err != nil {
		return Status{}, err
	}
	defer lk.release()
	return layout{root: o.StateDir}.status(m, now), nil
}

// reissue replaces only server.crt under the exclusive lock. The existing
// server certificate may be expired; the CA must be valid now and for the
// full two-year lifetime of the new certificate.
func (d *deps) reissue(ctx context.Context, o ReissueOptions) (Status, error) {
	if err := canceled(ctx); err != nil {
		return Status{}, err
	}
	sans, err := normalizeSANs(o.SANs)
	if err != nil {
		return Status{}, err
	}
	l := layout{root: o.StateDir}
	pre, err := l.scan()
	if err != nil {
		return Status{}, err
	}
	if !pre.rootExists {
		return Status{}, errf(contract.CodeNotFound, "no initialized plane state in %s; run callsheet plane init", l.root)
	}
	lk, err := l.acquire()
	if err != nil {
		return Status{}, err
	}
	defer lk.release()
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
	m, err := l.load(true)
	if err != nil {
		return Status{}, err
	}
	now := d.clock()
	ca := m.caCert
	switch {
	case now.Before(ca.NotBefore):
		return Status{}, errf(contract.CodeTrustFailed, "the CA is not yet valid (valid from %s); check the system clock. Nothing was changed", rfc3339(ca.NotBefore))
	case !now.Before(ca.NotAfter):
		return Status{}, errf(contract.CodeTrustFailed, "the CA expired at %s; the CA is never renewed implicitly. Nothing was changed", rfc3339(ca.NotAfter))
	case now.AddDate(leafYears, 0, 0).After(ca.NotAfter):
		return Status{}, errf(contract.CodeTrustFailed, "the CA expires at %s, less than %d calendar years from now, so a new %d-year server certificate would outlive it; the CA is never renewed implicitly. Nothing was changed",
			rfc3339(ca.NotAfter), leafYears, leafYears)
	}
	if err := canceled(ctx); err != nil {
		return Status{}, err
	}
	serial, err := d.serial()
	if err != nil {
		return Status{}, entropyErr(err)
	}
	if serial.Cmp(ca.SerialNumber) == 0 || serial.Cmp(m.serverCert.SerialNumber) == 0 {
		return Status{}, entropyErr(errors.New("the new server serial collided with an existing serial"))
	}
	cert, der, err := d.issueServer(now, serial, sans, ca, m.caKey, &m.serverKey.PublicKey)
	if err != nil {
		return Status{}, err
	}
	next := &material{bind: m.bind, caCert: ca, serverCert: cert, caKey: m.caKey, serverKey: m.serverKey}
	if err := next.validate(true); err != nil {
		return Status{}, wrapf(contract.CodeInternal, err, "the new server certificate failed validation: %v; nothing was changed", err)
	}
	if err := next.verifyAt(now); err != nil {
		return Status{}, err
	}
	if err := d.replaceServerCert(l, certPEM(der)); err != nil {
		return Status{}, err
	}
	return l.status(next, now), nil
}
