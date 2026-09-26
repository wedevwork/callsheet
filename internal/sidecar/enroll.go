package sidecar

import (
	"context"
	"encoding/hex"
	"errors"
	"os"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// newNodeID returns "n_" plus 16 random bytes in lowercase hex. The ID is
// opaque: never a hostname, address or MAC.
func (d *deps) newNodeID() (string, error) {
	b := make([]byte, 16)
	n := 0
	for n < len(b) {
		m, err := d.rand.Read(b[n:])
		n += m
		if err != nil {
			return "", err
		}
	}
	return "n_" + hex.EncodeToString(b), nil
}

// enroll is Enroll: validate arguments and trust (a verified connection),
// take the state lock and validate existing state, publish the identity if
// absent, register it, then atomically record the enrollment. An existing
// identity is never replaced.
func (d *deps) enroll(ctx context.Context, o EnrollOptions) (Enrollment, error) {
	if err := ctx.Err(); err != nil {
		return Enrollment{}, err
	}
	if !contract.ValidSoftwareVersion(o.SoftwareVersion) {
		return Enrollment{}, errf(contract.CodeInvalidArgument, "invalid software version %q", contract.SafeText(o.SoftwareVersion, 64))
	}
	url, err := client.NormalizePlaneURL(o.PlaneURL)
	if err != nil {
		return Enrollment{}, err
	}
	trust, err := d.resolveTrust(ctx, client.TrustOptions{PlaneURL: url, CAFile: o.CAFile, CAFingerprint: o.CAFingerprint})
	if err != nil {
		return Enrollment{}, err
	}
	l := layout{root: o.StateDir}
	pre, err := l.scan()
	if err != nil {
		return Enrollment{}, err
	}
	if !pre.rootExists {
		if err := os.MkdirAll(l.root, dirMode); err != nil {
			return Enrollment{}, wrapf(contract.CodeInternal, err, "cannot create state directory: %v", err)
		}
	}
	lk, err := l.acquire()
	if err != nil {
		return Enrollment{}, err
	}
	defer lk.release()
	s, err := l.scan()
	if err != nil {
		return Enrollment{}, err
	}
	var old *enrollmentFile
	if s.hasEnrollment {
		e, err := l.loadEnrollment()
		if err != nil {
			return Enrollment{}, err
		}
		old = &e
	}
	var id string
	if s.hasIdentity {
		if id, err = l.loadIdentity(); err != nil {
			return Enrollment{}, err
		}
	} else if id, err = d.publishIdentity(ctx, l); err != nil {
		return Enrollment{}, err
	}
	c, err := d.newClient(url, trust)
	if err != nil {
		return Enrollment{}, err
	}
	defer c.Close()
	if _, err := c.EnrollNode(ctx, id, o.SoftwareVersion); err != nil {
		return Enrollment{}, err
	}
	next := enrollmentFile{SchemaVersion: SchemaVersion, PlaneURL: url, CAPEM: string(trust.CAPEM), CAFingerprint: trust.Fingerprint}
	if old == nil || *old != next {
		if err := d.writeEnrollment(l, next); err != nil {
			return Enrollment{}, err
		}
	}
	return Enrollment{NodeID: id, PlaneURL: url, CAFingerprint: trust.Fingerprint}, nil
}

// publishIdentity generates a node ID and publishes identity.json without
// replacement before the first registration request. A failure before the
// link publishes nothing; after it, the identity is kept and reused.
func (d *deps) publishIdentity(ctx context.Context, l layout) (string, error) {
	id, err := d.newNodeID()
	if err != nil {
		return "", wrapf(contract.CodeInternal, err, "entropy source failed: %v; no identity was created", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tmp, err := d.writeTemp(l, identityName, encode(identityFile{SchemaVersion: SchemaVersion, NodeID: id}))
	if err != nil {
		return "", wrapf(contract.CodeInternal, err, "cannot create the node identity (writing %s failed: %v); nothing was published", l.path(identityName), err)
	}
	if err := d.publishNew(l, tmp, identityName); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", errf(contract.CodeConflict, "%s appeared while enrolling; nothing was replaced, retry", l.path(identityName))
		}
		return "", wrapf(contract.CodeInternal, err, "cannot publish the node identity %s: %v; nothing was published", l.path(identityName), err)
	}
	if err := d.syncDir(l); err != nil {
		return "", wrapf(contract.CodeInternal, err, "the node identity %s was published but syncing %s failed: %v; its durability is not confirmed. Retry the enrollment: it reuses this identity", l.path(identityName), l.root, err)
	}
	return id, nil
}

// writeEnrollment atomically replaces enrollment.json: temporary write,
// sync, close, rename, directory sync. Before the rename the old
// configuration stays usable; after it a failed sync is reported as
// possibly visible.
func (d *deps) writeEnrollment(l layout, e enrollmentFile) error {
	p := l.path(enrollmentName)
	tmp, err := d.writeTemp(l, enrollmentName, encode(e))
	if err != nil {
		return wrapf(contract.CodeInternal, err, "the plane registered this node, but writing %s failed: %v; the previous configuration is unchanged. Retry the enrollment: registration is keyed by the node ID", p, err)
	}
	if err := d.replace(l, tmp, enrollmentName); err != nil {
		return wrapf(contract.CodeInternal, err, "the plane registered this node, but replacing %s failed: %v; the previous configuration is unchanged. Retry the enrollment", p, err)
	}
	if err := d.syncDir(l); err != nil {
		return wrapf(contract.CodeInternal, err, "%s was replaced but syncing %s failed: %v; the new configuration may already be visible and its durability is not confirmed. Retry the enrollment", p, l.root, err)
	}
	return nil
}
