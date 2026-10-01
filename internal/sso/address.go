package sso

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// The site's providers are given by administrators, and the server fetches
// what they name: an issuer's discovery document, the key set it names, and
// at a sign-in its token endpoint. Unless the server's operator says
// otherwise (SSO_ALLOW_PRIVATE_ISSUERS), none of it is fetched from an
// address that is not public: a private network's, this machine's, a link's
// (where clouds keep their metadata, with credentials in it) or one no host
// has. What is checked is the address each connection is made to, once its
// name is resolved, so that a name that resolves to a public address when it
// is set up and to a private one when it is fetched (DNS rebinding) reaches
// nothing; and a redirect, which sso.test never follows and a sign-in may, is
// checked as it is dialled too.
//
// The operator's provider (OIDC_ISSUER) is the operator's own setting, as
// this one is, and is reached wherever it is.

// ReasonAddressNotAllowed is the reason a provider's address is refused, in
// an error's details and in what sso.test says.
const ReasonAddressNotAllowed = "issuer_address_not_allowed"

// notPublicWhy says why a provider's URL is refused for its address, after
// the URL: it says nothing of the address a name resolved to, so that an
// administrator learns that a name is on a private network, not where.
const notPublicWhy = "on this machine or a private, link-local or reserved address, which this server reaches for no " +
	"identity provider of the site's unless its operator sets SSO_ALLOW_PRIVATE_ISSUERS (" + ReasonAddressNotAllowed + ")"

// ErrAddressNotAllowed is a connection refused for its address.
var ErrAddressNotAllowed = errors.New("the address dialled is not public (" + ReasonAddressNotAllowed + ")")

// notPublic4 are the IPv4 addresses no provider of the site's is reached at:
// every range IANA's special-purpose registry (RFC 6890) says is not
// globally reachable, multicast and the reserved 240/4.
var notPublic4 = prefixes(
	"0.0.0.0/8",       // "this network": 0.0.0.0 is this machine
	"10.0.0.0/8",      // private (RFC 1918)
	"100.64.0.0/10",   // shared (RFC 6598): carrier-grade NAT, and a cloud's metadata (100.100.100.200)
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, and most clouds' metadata (169.254.169.254)
	"172.16.0.0/12",   // private
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"192.168.0.0/16",  // private
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, and the broadcast address
)

// Of IPv6, only global unicast (2000::/3) is public. That leaves out ::/8
// (unspecified, loopback, IPv4-compatible), 64:ff9b:1::/48 (NAT64 of a
// network's own), 100::/64 (discard), fc00::/7 (unique local, a cloud's
// metadata among them, fd00:ec2::254), fe80::/10 (link-local), fec0::/10
// and ff00::/8 (multicast). Within it, these are not public either.
var (
	global6    = netip.MustParsePrefix("2000::/3")
	notPublic6 = prefixes("2001::/23", "2001:db8::/32", "3fff::/20") // protocol assignments (Teredo among them), documentation
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour  = netip.MustParsePrefix("2002::/16")
)

func prefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// Public reports whether a provider of the site's may be reached at a.
// An IPv4 address written in IPv6 is that IPv4 address: mapped
// (::ffff:127.0.0.1), translated by NAT64 (64:ff9b::7f00:1, which an
// IPv6-only server's DNS64 gives for an IPv4-only provider) or under 6to4
// (2002:7f00:1::).
func Public(a netip.Addr) bool {
	a = a.WithZone("").Unmap()
	if a.Is4() {
		for _, p := range notPublic4 {
			if p.Contains(a) {
				return false
			}
		}
		return a.IsValid()
	}
	b := a.As16()
	switch {
	case nat64.Contains(a):
		return Public(netip.AddrFrom4([4]byte(b[12:])))
	case sixToFour.Contains(a):
		return Public(netip.AddrFrom4([4]byte(b[2:6])))
	case !global6.Contains(a):
		return false
	}
	for _, p := range notPublic6 {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// publicHost reports whether an issuer's host may be public, as far as can
// be told without resolving it: localhost and its subdomains (RFC 6761) are
// this machine, an address is Public or not, and a name is checked when it
// is dialled.
func publicHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return Public(a)
	}
	return true
}

// NewClient is what the site's providers are reached with: a request takes
// at most DefaultTimeout, and, unless private, connects only to a Public
// address, checked on the address dialled. Such a client uses no proxy
// (HTTPS_PROXY), which would choose the address itself: a server that
// reaches the internet only through one sets SSO_ALLOW_PRIVATE_ISSUERS.
func NewClient(private bool) *http.Client {
	if private {
		return &http.Client{Timeout: DefaultTimeout}
	}
	return guardedClient(func(a netip.AddrPort) bool { return Public(a.Addr()) })
}

// guardedClient connects only to the addresses allowed says it may.
func guardedClient(allowed func(netip.AddrPort) bool) *http.Client {
	dialer := &net.Dialer{Timeout: DefaultTimeout, KeepAlive: 30 * time.Second,
		ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
			if a, err := netip.ParseAddrPort(address); err != nil || !allowed(a) {
				return ErrAddressNotAllowed
			}
			return nil
		}}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = dialer.DialContext
	return &http.Client{Timeout: DefaultTimeout, Transport: t}
}

// IsAddressNotAllowed reports whether err is a connection refused for its
// address.
func IsAddressNotAllowed(err error) bool { return errors.Is(err, ErrAddressNotAllowed) }
