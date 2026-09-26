package plane

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Loopback binds need no interface membership check.
var (
	loopback4 = netip.MustParseAddr("127.0.0.1")
	loopback6 = netip.MustParseAddr("::1")
)

// privatePrefixes are the non-loopback private ranges a bind may use:
// RFC 1918, shared/CGNAT (common private VPNs) and IPv6 ULA.
var privatePrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// parseBind parses a literal IP and decimal port 0-65535, unmaps
// IPv4-mapped addresses and applies the address allowlist.
func parseBind(s string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return netip.AddrPort{}, errors.New("want a literal IP address and port, e.g. 127.0.0.1:8443 or [fd00::1]:8443")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%q is not a literal IP address (hostnames are not resolved)", host)
	}
	if addr.Zone() != "" {
		return netip.AddrPort{}, errors.New("IPv6 zones are not allowed")
	}
	p, err := parsePort(port)
	if err != nil {
		return netip.AddrPort{}, err
	}
	addr = addr.Unmap()
	if err := checkBindAddr(addr); err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(addr, p), nil
}

func parsePort(s string) (uint16, error) {
	if s == "" || len(s) > 5 {
		return 0, fmt.Errorf("port %q must be a decimal number 0-65535", s)
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("port %q must be a decimal number 0-65535", s)
		}
	}
	n, _ := strconv.Atoi(s)
	if n > 65535 {
		return 0, fmt.Errorf("port %q must be a decimal number 0-65535", s)
	}
	return uint16(n), nil
}

// checkBindAddr applies the conservative private allowlist: exactly
// 127.0.0.1 or ::1, or an address in privatePrefixes.
func checkBindAddr(a netip.Addr) error {
	switch {
	case a == loopback4 || a == loopback6:
		return nil
	case a.IsUnspecified():
		return fmt.Errorf("wildcard address %s would listen on every interface; choose 127.0.0.1 or a private interface address", a)
	case a.IsLoopback():
		return fmt.Errorf("loopback address %s is not allowed; use exactly 127.0.0.1 or ::1", a)
	case a.IsMulticast():
		return fmt.Errorf("multicast address %s is not allowed", a)
	case a.IsLinkLocalUnicast():
		return fmt.Errorf("link-local address %s is not allowed", a)
	}
	for _, p := range privatePrefixes {
		if p.Contains(a) {
			return nil
		}
	}
	return fmt.Errorf("%s is not a permitted private address (allowed: 127.0.0.1, ::1, 10/8, 172.16/12, 192.168/16, 100.64/10, fc00::/7); public listeners are never allowed", a)
}

func isLoopbackBind(a netip.Addr) bool { return a == loopback4 || a == loopback6 }

// checkPresent requires a non-loopback bind address to be assigned to a
// local interface. Enumeration errors fail closed; there is no wildcard
// fallback.
func (d *deps) checkPresent(ap netip.AddrPort) error {
	if isLoopbackBind(ap.Addr()) {
		return nil
	}
	addrs, err := d.addrs()
	if err != nil {
		return wrapf(contract.CodeUnavailable, err, "cannot enumerate local interface addresses to check bind %s: %v", ap, err)
	}
	for _, a := range addrs {
		if a.Unmap().WithZone("") == ap.Addr() {
			return nil
		}
	}
	return errf(contract.CodeUnavailable, "bind address %s is not assigned to any local interface; bring the interface up or stop the plane and edit config.json", ap.Addr())
}

// interfaceAddrs is the native enumeration adapter: every IP of
// net.InterfaceAddrs, unmapped and without zones.
func interfaceAddrs() ([]netip.Addr, error) {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	return addrsFrom(as), nil
}

func addrsFrom(as []net.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range as {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		if addr, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, addr.Unmap().WithZone(""))
		}
	}
	return out
}
