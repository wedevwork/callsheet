package plane

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestReissueReplacesOnlyTheCertificate: new SANs, serial and validity
// under the same CA and key; every other byte unchanged; clients keeping
// the CA connect after restart when their hostname stays covered.
func TestReissueReplacesOnlyTheCertificate(t *testing.T) {
	d := testDeps(t)
	root := newRoot(t)
	first := mustInit(t, d, root, "localhost", "127.0.0.1")
	before := snapshot(t, root)
	oldSrv := readServer(t, root)
	ca := readCA(t, root)
	for _, sans := range [][]string{{"plane.example", "127.0.0.1"}, {"plane.example", "127.0.0.1"}} {
		st, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: sans})
		if err != nil {
			t.Fatal(err)
		}
		srv := readServer(t, root)
		if srv.SerialNumber.Cmp(oldSrv.SerialNumber) == 0 {
			t.Fatal("serial reused (identical SAN lists still reissue)")
		}
		if strings.Join(srv.DNSNames, ",") != "plane.example" || st.CAFingerprint != first.CAFingerprint ||
			strings.Join(st.DNSNames, ",") != "plane.example" || strings.Join(st.IPAddresses, ",") != "127.0.0.1" {
			t.Fatalf("reissued %+v", st)
		}
		if !srv.PublicKey.(*ecdsa.PublicKey).Equal(oldSrv.PublicKey) {
			t.Fatal("reissue rotated the key")
		}
		if err := srv.CheckSignatureFrom(ca); err != nil {
			t.Fatal(err)
		}
		sameSnapshot(t, before, snapshot(t, root), serverCertName)
		oldSrv = srv
	}
	// A client that kept the original CA verifies the restarted plane.
	s := serveBG(t, testDeps(t), RunOptions{StateDir: root})
	if code, _, _, err := get(t, client(t, ca, "plane.example", tls.VersionTLS13), "https://"+s.addr.String()+HealthPath); err != nil || code != 200 {
		t.Fatalf("retained SAN = %d %v", code, err)
	}
	if _, _, _, err := get(t, client(t, ca, "localhost", tls.VersionTLS13), "https://"+s.addr.String()+HealthPath); err == nil {
		t.Fatal("removed SAN still verifies")
	}
	s.stop(t)
	// Invalid replacement lists change nothing.
	snap := snapshot(t, root)
	for _, sans := range [][]string{nil, {"bad name"}, {"*.example"}} {
		_, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: sans})
		wantCode(t, err, contract.CodeInvalidArgument)
	}
	sameSnapshot(t, snap, snapshot(t, root))
}

// TestReissueFailureContract is delegated from tests/function
// (TestPlaneReissue). Do not rename or skip its subtests.
func TestReissueFailureContract(t *testing.T) {
	setup := func(t *testing.T) (*deps, string, map[string][]byte) {
		d := testDeps(t)
		d.now = fixed(t0)
		root := newRoot(t)
		mustInit(t, d, root, "localhost")
		return d, root, snapshot(t, root)
	}
	t.Run("expired-leaf", func(t *testing.T) {
		d, root, snap := setup(t)
		now := t0.AddDate(3, 0, 0)
		d.now = fixed(now)
		if st, err := d.inspect(bg, root); err != nil || len(st.Warnings) != 1 || st.Warnings[0].Condition != ConditionExpired {
			t.Fatalf("precondition: %+v %v", st, err)
		}
		st, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"localhost"}})
		if err != nil {
			t.Fatalf("expired old certificate must be reissuable: %v", err)
		}
		if !st.ServerNotAfter.Equal(now.AddDate(2, 0, 0)) || !st.ServerNotBefore.Equal(now.Add(-5*time.Minute)) || len(st.Warnings) != 0 {
			t.Fatalf("new validity %v..%v %v", st.ServerNotBefore, st.ServerNotAfter, st.Warnings)
		}
		sameSnapshot(t, snap, snapshot(t, root), serverCertName)
	})
	t.Run("ca-horizon", func(t *testing.T) {
		d, root, snap := setup(t)
		caAfter := readCA(t, root).NotAfter
		for _, c := range []struct {
			now  time.Time
			ok   bool
			want string
		}{
			{t0.Add(-6 * time.Minute), false, "not yet valid"},
			{t0.AddDate(8, 0, 0).Add(time.Second), false, "less than 2 calendar years"},
			{t0.AddDate(9, 11, 0), false, "less than 2 calendar years"},
			{caAfter, false, "expired"},
			{caAfter.AddDate(1, 0, 0), false, "expired"},
			{t0.AddDate(8, 0, 0), true, ""},
		} {
			d.now = fixed(c.now)
			st, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"localhost"}})
			if c.ok {
				if err != nil || !st.ServerNotAfter.Equal(caAfter) {
					t.Fatalf("exact horizon at %v: %v %v", c.now, st.ServerNotAfter, err)
				}
				continue
			}
			wantCode(t, err, contract.CodeTrustFailed, c.want, "Nothing was changed")
			sameSnapshot(t, snap, snapshot(t, root))
		}
		// A structurally invalid state is not reissued either.
		os.WriteFile(layout{root: root}.path(serverKeyName), snapshot(t, root)[caKeyName], 0o600)
		d.now = fixed(t0)
		_, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"localhost"}})
		wantCode(t, err, contract.CodeTrustFailed, "server.key does not match")
	})
	t.Run("before-rename", func(t *testing.T) {
		for _, op := range []string{"create", "write", "sync", "close", "rename"} {
			d, root, snap := setup(t)
			d.fail = failAt(op, serverCertName)
			st, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"new.example"}})
			wantCode(t, err, contract.CodeInternal, "the previous certificate is unchanged", "injected "+op)
			if st.CAFingerprint != "" {
				t.Fatal("failure returned a report")
			}
			sameSnapshot(t, snap, snapshot(t, root))
			if left := tempLeftovers(t, root); len(left) != 0 {
				t.Fatalf("%s left %v", op, left)
			}
			// The old certificate is still complete and served.
			d.fail = nil
			if st, err := d.inspect(bg, root); err != nil || strings.Join(st.DNSNames, ",") != "localhost" {
				t.Fatalf("after %s failure: %+v %v", op, st, err)
			}
		}
	})
	t.Run("after-rename", func(t *testing.T) {
		d, root, snap := setup(t)
		d.fail = failAt("dirsync", pkiName)
		_, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"new.example"}})
		wantCode(t, err, contract.CodeInternal, "the replacement may already be visible", "injected dirsync")
		sameSnapshot(t, snap, snapshot(t, root), serverCertName)
		after := snapshot(t, root)[serverCertName]
		if bytes.Equal(after, snap[serverCertName]) {
			t.Fatal("rename did not happen before the sync failure")
		}
		// The visible certificate is the complete new one, matching the
		// unchanged key and CA.
		d.fail = nil
		m, err := layout{root: root}.load(true)
		if err != nil || strings.Join(m.serverCert.DNSNames, ",") != "new.example" {
			t.Fatalf("visible certificate: %v", err)
		}
	})
}
