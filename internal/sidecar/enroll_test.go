package sidecar

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

func newRoot(t *testing.T) string { return filepath.Join(t.TempDir(), "sidecar") }

func (p *inPlane) opts(root string) EnrollOptions {
	return EnrollOptions{StateDir: root, PlaneURL: p.url, CAFile: p.caFile, SoftwareVersion: "dev"}
}

// roster lists the plane's node IDs.
func (p *inPlane) roster(t *testing.T) []string {
	t.Helper()
	nodes, err := p.client(t).ListNodes(bg)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return ids
}

func fileBytes(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return b
}

// TestEnroll covers both trust paths, idempotency without rewrite,
// re-enrollment to a new plane with a stable identity, and the checks
// that precede any state.
func TestEnroll(t *testing.T) {
	p := startPlane(t)
	d := testDeps(nil)
	root := newRoot(t)
	e, err := d.enroll(bg, p.opts(root))
	if err != nil || !contract.ValidNodeID(e.NodeID) || e.PlaneURL != p.url || !client.ValidPin(e.CAFingerprint) {
		t.Fatalf("enroll = %+v %v", e, err)
	}
	l := layout{root: root}
	if id, _ := l.loadIdentity(); id != e.NodeID {
		t.Fatalf("identity %s, enrolled %s", id, e.NodeID)
	}
	en, err := l.loadEnrollment()
	if err != nil || en.PlaneURL != p.url || en.CAPEM != string(fileBytes(t, p.caFile)) || en.CAFingerprint != e.CAFingerprint {
		t.Fatalf("enrollment = %+v %v", en, err)
	}
	if ids := p.roster(t); len(ids) != 1 || ids[0] != e.NodeID {
		t.Fatalf("roster %v", ids)
	}
	// Same target and CA, now through the pin: idempotent, no rewrite.
	info, _ := os.Stat(l.path(enrollmentName))
	o := p.opts(root)
	o.CAFile, o.CAFingerprint = "", e.CAFingerprint
	e2, err := d.enroll(bg, o)
	if err != nil || e2 != e {
		t.Fatalf("repeat = %+v %v", e2, err)
	}
	if info2, _ := os.Stat(l.path(enrollmentName)); !os.SameFile(info, info2) || !info2.ModTime().Equal(info.ModTime()) {
		t.Fatal("identical configuration was rewritten")
	}
	// A new plane: the identity is kept, only the target is replaced, and
	// the old plane still knows the node.
	p2 := startPlane(t)
	e3, err := d.enroll(bg, p2.opts(root))
	if err != nil || e3.NodeID != e.NodeID || e3.PlaneURL != p2.url || e3.CAFingerprint == e.CAFingerprint {
		t.Fatalf("re-enroll = %+v %v", e3, err)
	}
	if en, _ := l.loadEnrollment(); en.PlaneURL != p2.url {
		t.Fatal("target not replaced")
	}
	if ids := p.roster(t); len(ids) != 1 || ids[0] != e.NodeID {
		t.Fatalf("old plane roster %v", ids)
	}
	// Checks before any state: trust, software version, URL, cancellation.
	fresh := newRoot(t)
	for name, c := range map[string]struct {
		o    EnrollOptions
		code contract.Code
	}{
		"no trust":    {EnrollOptions{StateDir: fresh, PlaneURL: p.url, SoftwareVersion: "dev"}, contract.CodeTrustFailed},
		"wrong ca":    {EnrollOptions{StateDir: fresh, PlaneURL: p.url, CAFile: p2.caFile, SoftwareVersion: "dev"}, contract.CodeTrustFailed},
		"wrong pin":   {EnrollOptions{StateDir: fresh, PlaneURL: p.url, CAFingerprint: e3.CAFingerprint, SoftwareVersion: "dev"}, contract.CodeTrustFailed},
		"bad version": {EnrollOptions{StateDir: fresh, PlaneURL: p.url, CAFile: p.caFile, SoftwareVersion: "a b"}, contract.CodeInvalidArgument},
		"bad url":     {EnrollOptions{StateDir: fresh, PlaneURL: "http://x", CAFile: p.caFile, SoftwareVersion: "dev"}, contract.CodeInvalidArgument},
	} {
		_, err := d.enroll(bg, c.o)
		if contract.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
		if c.code == contract.CodeTrustFailed && !strings.Contains(err.Error(), "connection not trusted") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Lstat(fresh); !os.IsNotExist(err) {
			t.Fatalf("%s created state", name)
		}
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := d.enroll(ctx, p.opts(fresh)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	// Invalid existing state is refused without repair.
	os.WriteFile(l.path(enrollmentName), []byte("{"), 0o600)
	if _, err := d.enroll(bg, p.opts(root)); contract.CodeOf(err) != contract.CodeConflict {
		t.Fatalf("corrupt enrollment = %v", err)
	}
	os.Remove(l.path(enrollmentName))
	os.WriteFile(l.path(identityName), []byte("{"), 0o600)
	if _, err := d.enroll(bg, p.opts(root)); contract.CodeOf(err) != contract.CodeConflict {
		t.Fatalf("corrupt identity = %v", err)
	}
	unexpected := newRoot(t)
	os.MkdirAll(unexpected, 0o700)
	os.WriteFile(filepath.Join(unexpected, "x"), nil, 0o600)
	if _, err := d.enroll(bg, p.opts(unexpected)); contract.CodeOf(err) != contract.CodeConflict {
		t.Fatalf("unexpected entry = %v", err)
	}
}

// nthDirsync fails the n-th directory sync (1-based).
func nthDirsync(n int) func(string, string) error {
	var count atomic.Int32
	return func(op, _ string) error {
		if op == "dirsync" && int(count.Add(1)) == n {
			return errors.New("injected dirsync failure")
		}
		return nil
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("injected entropy failure") }

// TestEnrollmentRecoveryContract is delegated from tests/function
// (TestNodeEnrollment/recovery): every local write boundary fails in turn
// and a retry recovers with a stable identity; pending identities,
// ambiguous plane success and re-enrollment failures are safe. Do not
// rename or skip its subtests.
func TestEnrollmentRecoveryContract(t *testing.T) {
	p := startPlane(t)
	t.Run("faults", func(t *testing.T) {
		for _, c := range []struct {
			name     string
			fail     func(string, string) error
			identity bool // the identity is published after the failure
			msg      string
		}{
			{"identity-create", failAt("create", identityName), false, "nothing was published"},
			{"identity-write", failAt("write", identityName), false, "nothing was published"},
			{"identity-sync", failAt("sync", identityName), false, "nothing was published"},
			{"identity-close", failAt("close", identityName), false, "nothing was published"},
			{"identity-publish", failAt("publish", identityName), false, "nothing was published"},
			{"identity-dirsync", nthDirsync(1), true, "durability is not confirmed"},
			{"enrollment-create", failAt("create", enrollmentName), true, "previous configuration is unchanged"},
			{"enrollment-write", failAt("write", enrollmentName), true, "previous configuration is unchanged"},
			{"enrollment-sync", failAt("sync", enrollmentName), true, "previous configuration is unchanged"},
			{"enrollment-close", failAt("close", enrollmentName), true, "previous configuration is unchanged"},
			{"enrollment-rename", failAt("rename", enrollmentName), true, "previous configuration is unchanged"},
			{"enrollment-dirsync", nthDirsync(2), true, "may already be visible"},
		} {
			root := newRoot(t)
			d := testDeps(nil)
			d.fail = c.fail
			_, err := d.enroll(bg, p.opts(root))
			wantCode(t, err, contract.CodeInternal, c.msg)
			l := layout{root: root}
			id, idErr := l.loadIdentity()
			if (idErr == nil) != c.identity {
				t.Fatalf("%s: identity published = %v", c.name, idErr)
			}
			if left, _ := filepath.Glob(filepath.Join(root, tempPrefix+"*")); len(left) != 0 {
				t.Fatalf("%s: temporaries left %v", c.name, left)
			}
			d.fail = nil
			e, err := d.enroll(bg, p.opts(root))
			if err != nil || (c.identity && e.NodeID != id) {
				t.Fatalf("%s: retry = %+v %v (identity %s)", c.name, e, err, id)
			}
			if _, err := l.loadEnrollment(); err != nil {
				t.Fatalf("%s: retry left no enrollment: %v", c.name, err)
			}
		}
	})
	t.Run("pending", func(t *testing.T) {
		// Identity published, registration failed: Run refuses with the
		// next step, and enroll reuses the identity.
		root := newRoot(t)
		d := testDeps(nil)
		down := startPlane(t)
		down.stop(t)
		_, err := d.enroll(bg, EnrollOptions{StateDir: root, PlaneURL: down.url, CAFile: down.caFile, SoftwareVersion: "dev"})
		if err == nil {
			t.Fatal("enrolled against a stopped plane")
		}
		os.MkdirAll(root, 0o700)
		id := "n_" + strings.Repeat("ab", 16)
		os.WriteFile(filepath.Join(root, identityName), encode(identityFile{SchemaVersion: 1, NodeID: id}), 0o600)
		wantCode(t, d.run(bg, RunOptions{StateDir: root, SoftwareVersion: "dev"}), contract.CodeNotFound, "has an identity but no completed enrollment", "run callsheet sidecar enroll")
		e, err := d.enroll(bg, p.opts(root))
		if err != nil || e.NodeID != id {
			t.Fatalf("completing a pending identity = %+v %v", e, err)
		}
	})
	t.Run("ambiguous", func(t *testing.T) {
		// The plane registered the node but the local write failed: the
		// retry is safe (keyed by ID) and the plane knows one node.
		root := newRoot(t)
		fresh := startPlane(t)
		d := testDeps(nil)
		d.fail = failAt("create", enrollmentName)
		if _, err := d.enroll(bg, fresh.opts(root)); err == nil {
			t.Fatal("injected failure ignored")
		}
		id, _ := layout{root: root}.loadIdentity()
		if ids := fresh.roster(t); len(ids) != 1 || ids[0] != id {
			t.Fatalf("roster after ambiguous success %v", ids)
		}
		d.fail = nil
		if e, err := d.enroll(bg, fresh.opts(root)); err != nil || e.NodeID != id {
			t.Fatalf("retry = %+v %v", e, err)
		}
		if ids := fresh.roster(t); len(ids) != 1 {
			t.Fatalf("retry duplicated the node: %v", ids)
		}
	})
	t.Run("reenroll", func(t *testing.T) {
		// A failed re-enrollment before the rename keeps the old,
		// complete configuration usable.
		root := newRoot(t)
		d := testDeps(nil)
		if _, err := d.enroll(bg, p.opts(root)); err != nil {
			t.Fatal(err)
		}
		l := layout{root: root}
		before := fileBytes(t, l.path(enrollmentName))
		other := startPlane(t)
		d.fail = failAt("rename", enrollmentName)
		if _, err := d.enroll(bg, other.opts(root)); err == nil {
			t.Fatal("injected failure ignored")
		}
		if !bytes.Equal(before, fileBytes(t, l.path(enrollmentName))) {
			t.Fatal("old configuration changed")
		}
		if en, err := l.loadEnrollment(); err != nil || en.PlaneURL != p.url {
			t.Fatalf("old configuration unusable: %+v %v", en, err)
		}
	})
	t.Run("entropy", func(t *testing.T) {
		root := newRoot(t)
		d := testDeps(nil)
		d.rand = failingReader{}
		_, err := d.enroll(bg, p.opts(root))
		wantCode(t, err, contract.CodeInternal, "entropy source failed", "no identity was created")
		if _, err := os.Lstat(filepath.Join(root, identityName)); !os.IsNotExist(err) {
			t.Fatal("identity published without entropy")
		}
		d.rand = defaultDeps().rand
		ctx, cancel := context.WithCancel(bg)
		cancel()
		if _, err := d.publishIdentity(ctx, layout{root: root}); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled publish = %v", err)
		}
	})
}
