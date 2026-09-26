package plane

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

func TestInspectReport(t *testing.T) {
	d := testDeps(t)
	root := newRoot(t)
	initRep := mustInit(t, d, root, "b.example", "a.example", "fd00::1", "10.0.0.1")
	l := layout{root: root}
	os.Remove(l.path(lockName))
	before := snapshot(t, root)
	infos := map[string]os.FileInfo{}
	for _, rel := range append(durable, rootName, pkiName) {
		infos[rel], _ = os.Stat(l.path(rel))
	}
	d.listen = func(string, string) (net.Listener, error) {
		t.Error("status opened a listener")
		return nil, errors.New("x")
	}
	st, err := d.inspect(bg, root)
	if err != nil {
		t.Fatal(err)
	}
	ca, srv := readCA(t, root), readServer(t, root)
	want := Status{
		StateDir: root, Bind: "127.0.0.1:0",
		CACertPath: root + "/pki/ca.crt", CAKeyPath: root + "/pki/ca.key", ServerCertPath: root + "/pki/server.crt", ServerKeyPath: root + "/pki/server.key",
		DNSNames: []string{"a.example", "b.example"}, IPAddresses: []string{"10.0.0.1", "fd00::1"},
		CAFingerprint: Fingerprint(ca.Raw), CANotBefore: ca.NotBefore, CANotAfter: ca.NotAfter,
		ServerNotBefore: srv.NotBefore, ServerNotAfter: srv.NotAfter,
	}
	if fmt.Sprintf("%+v", st) != fmt.Sprintf("%+v", want) || fmt.Sprintf("%+v", initRep) != fmt.Sprintf("%+v", want) {
		t.Fatalf("status\n%+v\nwant\n%+v\ninit\n%+v", st, want, initRep)
	}
	sameSnapshot(t, before, snapshot(t, root))
	for rel, fi := range infos {
		now, _ := os.Stat(l.path(rel))
		if !now.ModTime().Equal(fi.ModTime()) {
			t.Fatalf("status modified %s", rel)
		}
	}
	if _, err := os.Stat(l.path(lockName)); !os.IsNotExist(err) {
		t.Fatal("status created a lock")
	}
	// Status does not read private key contents (only their modes).
	keyPEM := before[caKeyName]
	os.WriteFile(l.path(caKeyName), []byte("not a key"), 0o600)
	if _, err := d.inspect(bg, root); err != nil {
		t.Fatalf("status read a private key: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v", st), string(keyPEM[30:70])) {
		t.Fatal("status leaks key material")
	}
	os.WriteFile(l.path(caKeyName), keyPEM, 0o600)
	// Status works on a running plane (it takes no lock).
	s := serveBG(t, testDeps(t), RunOptions{StateDir: root})
	if _, err := testDeps(t).inspect(bg, root); err != nil {
		t.Fatal(err)
	}
	s.stop(t)
	// An IPv6 bind is bracketed in config and status.
	root6 := newRoot(t)
	d6 := testDeps(t)
	if _, err := d6.init(bg, InitOptions{StateDir: root6, Bind: "[::1]:8443", BindSet: true, SANs: []string{"::1"}, SANsSet: true}); err != nil {
		t.Fatal(err)
	}
	if st, _ := d6.inspect(bg, root6); st.Bind != "[::1]:8443" || len(st.DNSNames) != 0 || st.DNSNames == nil {
		t.Fatalf("ipv6 status %+v", st)
	}
	if b, _ := os.ReadFile(layout{root: root6}.path(configName)); !strings.Contains(string(b), `"bind": "[::1]:8443"`) {
		t.Fatalf("ipv6 config %s", b)
	}
}

// craft builds complete state whose CA is issued at caAt and whose server
// certificate is issued at serverAt, bypassing the reissue horizon so that
// each certificate's time conditions can be tested independently.
func craft(t *testing.T, caAt, serverAt time.Time) string {
	t.Helper()
	d := testDeps(t)
	d.now = fixed(caAt)
	root := newRoot(t)
	mustInit(t, d, root, "localhost")
	m, err := layout{root: root}.load(true)
	if err != nil {
		t.Fatal(err)
	}
	sans, _ := normalizeSANs([]string{"localhost"})
	_, der, err := d.issueServer(serverAt, big.NewInt(serverAt.Unix()), sans, m.caCert, m.caKey, &m.serverKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout{root: root}.path(serverCertName), certPEM(der), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

type clockCase struct {
	name  string
	root  string
	now   time.Time
	which string // "" = no warning
	cond  string
	at    time.Time
}

func checkClockCase(t *testing.T, c clockCase) {
	t.Helper()
	d := testDeps(t)
	d.now = fixed(c.now)
	st, err := d.inspect(bg, c.root)
	if err != nil {
		t.Fatalf("%s: status must exit 0: %v", c.name, err)
	}
	var want []Warning
	if c.which != "" {
		want = []Warning{{Certificate: c.which, Condition: c.cond, At: c.at.UTC()}}
	}
	if fmt.Sprint(st.Warnings) != fmt.Sprint(want) {
		t.Fatalf("%s: warnings %v, want %v", c.name, st.Warnings, want)
	}
	// Startup: warnings first; invalid time refuses with trust_failed
	// before any listener exists.
	listens := 0
	d.listen = func(n, a string) (net.Listener, error) { listens++; return net.Listen(n, a) }
	logs := &logBuffer{}
	valid := c.cond == "" || c.cond == ConditionExpiresSoon
	if valid {
		s := serveBG(t, d, RunOptions{StateDir: c.root, Logger: logs.logger()})
		s.stop(t)
	} else {
		d.ready = func(net.Addr) { t.Errorf("%s: listening with invalid time", c.name) }
		err := d.run(bg, RunOptions{StateDir: c.root, Logger: logs.logger()})
		wantCode(t, err, contract.CodeTrustFailed, c.which)
		if contract.ExitCode(err) != 6 || listens != 0 {
			t.Fatalf("%s: exit %d after %d listeners", c.name, contract.ExitCode(err), listens)
		}
	}
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var r map[string]any
		if line != "" && json.Unmarshal([]byte(line), &r) == nil {
			recs = append(recs, r)
		}
	}
	var warns []map[string]any
	listenAt := -1
	for i, r := range recs {
		if r["level"] == "WARN" {
			warns = append(warns, r)
			if listenAt >= 0 {
				t.Fatalf("%s: warning after the listening record", c.name)
			}
		}
		if r["msg"] == "listening" {
			listenAt = i
		}
	}
	if valid != (listenAt >= 0) {
		t.Fatalf("%s: listening record present=%v", c.name, listenAt >= 0)
	}
	if len(warns) != len(want) {
		t.Fatalf("%s: warn records %v", c.name, warns)
	}
	if len(want) == 1 {
		key := "expires_at"
		if c.cond == ConditionNotYetValid {
			key = "valid_from"
		}
		w := warns[0]
		if w["msg"] != c.cond || w["certificate"] != c.which || w[key] != rfc3339(c.at) || w["component"] != "plane" {
			t.Fatalf("%s: warn record %v", c.name, w)
		}
	}
}

// TestClockContract is delegated from tests/function
// (TestPlaneStatus/expiry-warnings). Do not rename or skip its subtests.
func TestClockContract(t *testing.T) {
	t.Run("ca", func(t *testing.T) {
		caNA := t0.AddDate(caYears, 0, 0)
		late := craft(t, t0, caNA.AddDate(-1, 0, 0)) // server outlives the CA
		early := craft(t, t0, t0.AddDate(-1, 0, 0))  // server valid before the CA
		caNB := t0.Add(-backdate)
		for _, c := range []clockCase{
			{"window+1s", late, caNA.Add(-warnWindow - time.Second), "", "", time.Time{}},
			{"window", late, caNA.Add(-warnWindow), "ca", ConditionExpiresSoon, caNA},
			{"window-1s", late, caNA.Add(-warnWindow + time.Second), "ca", ConditionExpiresSoon, caNA},
			{"last second", late, caNA.Add(-time.Second), "ca", ConditionExpiresSoon, caNA},
			{"exact expiry", late, caNA, "ca", ConditionExpired, caNA},
			{"past expiry", late, caNA.AddDate(0, 0, 1), "ca", ConditionExpired, caNA},
			{"future NotBefore", early, caNB.Add(-time.Second), "ca", ConditionNotYetValid, caNB},
		} {
			checkClockCase(t, c)
		}
	})
	t.Run("server", func(t *testing.T) {
		now := craft(t, t0, t0)
		srvNA := t0.AddDate(leafYears, 0, 0)
		future := craft(t, t0, t0.AddDate(1, 0, 0))
		srvNB := t0.AddDate(1, 0, 0).Add(-backdate)
		for _, c := range []clockCase{
			{"window+1s", now, srvNA.Add(-warnWindow - time.Second), "", "", time.Time{}},
			{"window", now, srvNA.Add(-warnWindow), "server", ConditionExpiresSoon, srvNA},
			{"window-1s", now, srvNA.Add(-warnWindow + time.Second), "server", ConditionExpiresSoon, srvNA},
			{"exact expiry", now, srvNA, "server", ConditionExpired, srvNA},
			{"past expiry", now, srvNA.AddDate(0, 0, 1), "server", ConditionExpired, srvNA},
			{"future NotBefore", future, srvNB.Add(-time.Second), "server", ConditionNotYetValid, srvNB},
		} {
			checkClockCase(t, c)
		}
	})
}

func TestWarningPrecedenceAndOrder(t *testing.T) {
	root := craft(t, t0, t0)
	d := testDeps(t)
	// Both certificates not yet valid: CA first, server second.
	d.now = fixed(t0.Add(-time.Hour))
	st, err := d.inspect(bg, root)
	if err != nil || len(st.Warnings) != 2 || st.Warnings[0].Certificate != "ca" || st.Warnings[1].Certificate != "server" ||
		st.Warnings[0].Condition != ConditionNotYetValid || st.Warnings[1].Condition != ConditionNotYetValid {
		t.Fatalf("both not yet valid: %+v %v", st.Warnings, err)
	}
	// Both expired long ago: one condition each ("expired" beats "expires soon").
	d.now = fixed(t0.AddDate(20, 0, 0))
	st, _ = d.inspect(bg, root)
	if len(st.Warnings) != 2 || st.Warnings[0].Condition != ConditionExpired || st.Warnings[1].Condition != ConditionExpired {
		t.Fatalf("both expired: %+v", st.Warnings)
	}
	// A reissued state far from expiry has no warnings.
	d.now = fixed(t0)
	if st, _ := d.inspect(bg, root); len(st.Warnings) != 0 {
		t.Fatalf("fresh state warnings %+v", st.Warnings)
	}
	// Clock rollback beyond the backdate reads as not yet valid; within it
	// the state is valid.
	d.now = fixed(t0.Add(-backdate))
	if st, _ := d.inspect(bg, root); len(st.Warnings) != 0 {
		t.Fatalf("within backdate %+v", st.Warnings)
	}
}
