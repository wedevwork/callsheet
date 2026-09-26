package plane

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

func TestParseBind(t *testing.T) {
	ok := map[string]string{
		"127.0.0.1:8443":                "127.0.0.1:8443",
		"127.0.0.1:0":                   "127.0.0.1:0",
		"127.0.0.1:65535":               "127.0.0.1:65535",
		"[::1]:1":                       "[::1]:1",
		"[::ffff:127.0.0.1]:8443":       "127.0.0.1:8443",
		"[::ffff:10.1.2.3]:9":           "10.1.2.3:9",
		"10.0.0.0:1":                    "10.0.0.0:1",
		"10.255.255.255:1":              "10.255.255.255:1",
		"172.16.0.0:1":                  "172.16.0.0:1",
		"172.31.255.255:1":              "172.31.255.255:1",
		"192.168.0.1:1":                 "192.168.0.1:1",
		"192.168.255.255:1":             "192.168.255.255:1",
		"100.64.0.0:1":                  "100.64.0.0:1",
		"100.127.255.255:1":             "100.127.255.255:1",
		"[fc00::]:1":                    "[fc00::]:1",
		"[fd12:3456:789a::1]:8443":      "[fd12:3456:789a::1]:8443",
		"[FDFF:FFFF::1]:8443":           "[fdff:ffff::1]:8443",
		"[fdff:ffff:ffff:ffff::]:08443": "[fdff:ffff:ffff:ffff::]:8443",
	}
	for in, want := range ok {
		ap, err := parseBind(in)
		if err != nil || ap.String() != want {
			t.Errorf("parseBind(%q) = %v, %v; want %s", in, ap, err, want)
		}
	}
	bad := map[string]string{
		"":                    "literal IP address and port",
		"127.0.0.1":           "literal IP address and port",
		"::1:8443":            "literal IP address and port",
		"localhost:8443":      "not a literal IP",
		"plane.example:8443":  "not a literal IP",
		"[fe80::1%eth0]:8443": "zones",
		"127.0.0.1:65536":     "port",
		"127.0.0.1:-1":        "port",
		"127.0.0.1:+1":        "port",
		"127.0.0.1:":          "port",
		"127.0.0.1:123456":    "port",
		"127.0.0.1:0x10":      "port",
		"0.0.0.0:8443":        "wildcard",
		"[::]:8443":           "wildcard",
		"127.0.0.2:8443":      "use exactly 127.0.0.1",
		"127.255.255.254:1":   "use exactly 127.0.0.1",
		"224.0.0.1:1":         "multicast",
		"[ff02::1]:1":         "multicast",
		"169.254.1.1:1":       "link-local",
		"[fe80::1]:1":         "link-local",
		"8.8.8.8:1":           "not a permitted private address",
		"9.255.255.255:1":     "not a permitted private address",
		"11.0.0.0:1":          "not a permitted private address",
		"172.15.255.255:1":    "not a permitted private address",
		"172.32.0.0:1":        "not a permitted private address",
		"192.167.255.255:1":   "not a permitted private address",
		"192.169.0.0:1":       "not a permitted private address",
		"100.63.255.255:1":    "not a permitted private address",
		"100.128.0.0:1":       "not a permitted private address",
		"192.0.2.1:1":         "not a permitted private address",
		"198.51.100.1:1":      "not a permitted private address",
		"240.0.0.1:1":         "not a permitted private address",
		"255.255.255.255:1":   "not a permitted private address",
		"[fbff:ffff::1]:1":    "not a permitted private address",
		"[fe00::1]:1":         "not a permitted private address",
		"[2001:db8::1]:1":     "not a permitted private address",
		"[2606:4700::1]:1":    "not a permitted private address",
		"[::ffff:8.8.8.8]:1":  "not a permitted private address",
		"[::ffff:0.0.0.0]:1":  "wildcard",
	}
	for in, want := range bad {
		_, err := parseBind(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseBind(%q) = %v, want %q", in, err, want)
		}
	}
}

// membershipTable exercises injected enumeration results. Bind policy has
// no goos parameter: the same decisions hold on both systems.
func membershipTable(t *testing.T) {
	enum := func(addrs []string, err error) func() ([]netip.Addr, error) {
		return func() ([]netip.Addr, error) {
			var out []netip.Addr
			for _, a := range addrs {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, err
		}
	}
	for _, c := range []struct {
		bind  string
		addrs []string
		err   error
		code  contract.Code
		calls int
	}{
		{"127.0.0.1:0", nil, errors.New("must not be consulted"), "", 0},
		{"[::1]:0", nil, errors.New("must not be consulted"), "", 0},
		{"10.1.2.3:8443", []string{"127.0.0.1", "10.1.2.3"}, nil, "", 1},
		{"10.1.2.3:8443", []string{"::ffff:10.1.2.3"}, nil, "", 1},
		{"[fd00::5]:8443", []string{"fd00::5%en0"}, nil, "", 1},
		{"100.100.1.1:1", []string{"100.100.1.1"}, nil, "", 1},
		{"192.168.1.2:1", []string{"192.168.1.3", "10.1.2.3"}, nil, contract.CodeUnavailable, 1},
		{"192.168.1.2:1", nil, nil, contract.CodeUnavailable, 1},
		{"192.168.1.2:1", []string{"192.168.1.2"}, errors.New("netlink denied"), contract.CodeUnavailable, 1},
		{"[fd00::5]:1", []string{"fd00::6"}, nil, contract.CodeUnavailable, 1},
	} {
		calls := 0
		d := testDeps(t)
		inner := enum(c.addrs, c.err)
		d.addrs = func() ([]netip.Addr, error) { calls++; return inner() }
		ap, err := parseBind(c.bind)
		if err != nil {
			t.Fatal(err)
		}
		err = d.checkPresent(ap)
		if codeOf(err) != c.code || calls != c.calls {
			t.Fatalf("%s with %v/%v: %v after %d enumerations", c.bind, c.addrs, c.err, err, calls)
		}
		// Explicit binds are checked before any state exists; a failure
		// creates nothing and never falls back to a wildcard.
		root := newRoot(t)
		var listened []string
		d.listen = func(n, a string) (net.Listener, error) {
			listened = append(listened, a)
			return nil, errors.New("no listener in this test")
		}
		_, err = d.init(bg, InitOptions{StateDir: root, Bind: c.bind, BindSet: true, SANs: []string{"localhost"}, SANsSet: true})
		if codeOf(err) != c.code {
			t.Fatalf("init --bind %s: %v", c.bind, err)
		}
		if c.code != "" {
			if _, serr := os.Lstat(root); !os.IsNotExist(serr) {
				t.Fatalf("failed bind check created state")
			}
		} else if st, _ := d.inspect(bg, root); st.Bind != ap.String() {
			t.Fatalf("persisted bind %q", st.Bind)
		}
		if len(listened) != 0 {
			t.Fatalf("init listened on %v", listened)
		}
	}
	// Every run rechecks the persisted bind: a removed interface is
	// unavailable before any listener exists.
	d := testDeps(t)
	present := true
	d.addrs = func() ([]netip.Addr, error) {
		if present {
			return []netip.Addr{netip.MustParseAddr("10.9.8.7")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("10.9.8.6")}, nil
	}
	root := newRoot(t)
	if _, err := d.init(bg, InitOptions{StateDir: root, Bind: "10.9.8.7:0", BindSet: true, SANs: []string{"a"}, SANsSet: true}); err != nil {
		t.Fatal(err)
	}
	present = false
	listened := 0
	d.listen = func(string, string) (net.Listener, error) { listened++; return nil, errors.New("unexpected") }
	err := d.run(bg, RunOptions{StateDir: root})
	wantCode(t, err, contract.CodeUnavailable, "not assigned to any local interface")
	if listened != 0 {
		t.Fatal("run listened after a failed interface check")
	}
	// Status and reissue validate syntax only, for offline maintenance.
	d.addrs = func() ([]netip.Addr, error) { t.Error("status/reissue enumerated"); return nil, nil }
	if st, err := d.inspect(bg, root); err != nil || st.Bind != "10.9.8.7:0" {
		t.Fatalf("status = %+v %v", st, err)
	}
	if _, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
}

// TestInterfacePolicyContract is delegated from tests/function
// (TestPlaneBind). Do not rename or skip its subtests.
func TestInterfacePolicyContract(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			membershipTable(t)
			// The default config is written without enumerating or opening
			// a socket.
			d := testDeps(t)
			d.listen = func(string, string) (net.Listener, error) {
				t.Error("init opened a listener")
				return nil, errors.New("unexpected")
			}
			root, err := ResolveStateDir(goos, "", t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.init(bg, InitOptions{StateDir: root, SANs: []string{"localhost"}, SANsSet: true}); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(layout{root: root}.path(configName))
			if string(b) != "{\n  \"schema_version\": 1,\n  \"bind\": \"127.0.0.1:8443\"\n}\n" {
				t.Fatalf("%s default config = %q", goos, b)
			}
		})
	}
	t.Run("native-adapter", func(t *testing.T) {
		got, err := interfaceAddrs()
		if err != nil {
			t.Fatalf("host enumeration: %v", err)
		}
		raw, err := net.InterfaceAddrs()
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, a := range raw {
			s := a.String()
			if i := strings.IndexByte(s, '/'); i >= 0 {
				s = s[:i]
			}
			ip := net.ParseIP(s)
			if ip == nil {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				ip = v4
			}
			want = append(want, ip.String())
		}
		var gotS []string
		for _, a := range got {
			gotS = append(gotS, a.String())
		}
		slices.Sort(want)
		slices.Sort(gotS)
		if !slices.Equal(gotS, want) {
			t.Fatalf("adapter %v, host %v", gotS, want)
		}
		if !slices.Contains(gotS, "127.0.0.1") {
			t.Fatalf("host enumeration lacks 127.0.0.1: %v", gotS)
		}
		d := defaultDeps()
		for _, a := range got {
			if checkBindAddr(a) == nil && !isLoopbackBind(a) {
				if err := d.checkPresent(netip.AddrPortFrom(a, 0)); err != nil {
					t.Fatalf("host private address %s rejected: %v", a, err)
				}
			}
		}
		for _, cand := range []string{"10.255.254.253", "172.31.254.253", "192.168.254.253", "fd7e:57::1"} {
			a := netip.MustParseAddr(cand)
			if slices.Contains(got, a) {
				continue
			}
			wantCode(t, d.checkPresent(netip.AddrPortFrom(a, 0)), contract.CodeUnavailable, "not assigned")
			break
		}
		// Non-IP address kinds are ignored by the adapter.
		mixed := addrsFrom([]net.Addr{&net.IPAddr{IP: net.ParseIP("10.0.0.1")}, &net.UnixAddr{Name: "x"}, &net.IPNet{IP: net.ParseIP("::ffff:10.0.0.2")}, &net.IPNet{IP: net.IP{1}}})
		if len(mixed) != 2 || mixed[0].String() != "10.0.0.1" || mixed[1].String() != "10.0.0.2" {
			t.Fatalf("addrsFrom = %v", mixed)
		}
	})
}
