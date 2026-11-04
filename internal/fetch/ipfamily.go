package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
)

// An http or graphite collector's request.ip_family is the address family
// its requests connect to the target over, as blackbox_exporter's
// preferred_ip_protocol and ip_protocol_fallback choose it:
//
//   - ip4 and ip6 dial that family alone, Go's "tcp4" and "tcp6": a name
//     is looked up for addresses of that family only, and a target that has
//     none fails, saying so, as an address of the other family written as
//     the target does. An uptime check of a site's IPv6 is then of its IPv6
//     alone, and not answered over IPv4 by the race below.
//   - prefer_ip4 and prefer_ip6 dial that family first, and the other when
//     the first has no address, or none of its addresses takes the
//     connection: a site whose IPv6 is broken is still reached, over IPv4.
//     Under a deadline the first family is given half of the time left, so
//     an address that never answers leaves the other its half. The deadline
//     is the request's (withDialDeadline): net/http dials with a context
//     detached from the request's, which keeps its values and loses its
//     deadline, so a dial's own context has none.
//   - Left out, Go dials as it always has: the addresses of both families,
//     the first one's family raced against the other's a moment later
//     (RFC 6555's "happy eyeballs").
//
// The family is of the connection to the target. A request through a proxy
// is connected to the target by the proxy, which looks the name up and
// chooses the family itself, so a request with ip_family is refused rather
// than sent through one (policyGuard.check), and the key is refused beside
// a proxy_url when the configuration loads, and beside a unix_socket, whose
// connection has no address family. The family is part of the transport's
// settings, so a connection made over one family never carries a request
// that asked for another. A login's request is made as before, over either
// family.

// IPFamilies are the values request.ip_family may take.
var IPFamilies = []string{"ip4", "ip6", "prefer_ip4", "prefer_ip6"}

// checkIPFamily refuses a request.ip_family, a probe's ip_family or a static
// target's, that is none of IPFamilies. "" is the key left out.
func checkIPFamily(family string) error {
	if family == "" || slices.Contains(IPFamilies, family) {
		return nil
	}
	return fmt.Errorf("ip_family %q is not ip4, ip6, prefer_ip4 or prefer_ip6; leave it out for both families, Go's default", family)
}

// familyNetworks are the networks ip_family dials, in order: one for ip4
// and ip6, two for prefer_ip4 and prefer_ip6, none for "".
func familyNetworks(family string) []string {
	switch family {
	case "ip4":
		return []string{"tcp4"}
	case "ip6":
		return []string{"tcp6"}
	case "prefer_ip4":
		return []string{"tcp4", "tcp6"}
	case "prefer_ip6":
		return []string{"tcp6", "tcp4"}
	}
	return nil
}

// familyName is how an error names network's family.
func familyName(network string) string {
	if network == "tcp6" {
		return "IPv6"
	}
	return "IPv4"
}

// familyDialer is dial with its TCP connections made over family's networks
// (familyNetworks), and dial itself for "".
func familyDialer(dial func(ctx context.Context, network, address string) (net.Conn, error), family string) func(ctx context.Context, network, address string) (net.Conn, error) {
	networks := familyNetworks(family)
	if networks == nil {
		return dial
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" {
			return dial(ctx, network, address)
		}
		if len(networks) == 1 {
			conn, err := dial(ctx, networks[0], address)
			if err != nil && noAddressOf(err) {
				return nil, fmt.Errorf("%s has no %s address, and request.ip_family is %s: %w", hostOf(address), familyName(networks[0]), family, err)
			}
			return conn, err
		}
		first := ctx
		cancel := func() {}
		if deadline, ok := dialDeadline(ctx); ok {
			first, cancel = context.WithDeadline(ctx, time.Now().Add(time.Until(deadline)/2))
		}
		conn, err := dial(first, networks[0], address)
		cancel()
		if err == nil || ctx.Err() != nil {
			return conn, err
		}
		conn, fallbackErr := dial(ctx, networks[1], address)
		switch {
		case fallbackErr == nil:
			return conn, nil
		case noAddressOf(err):
			return nil, fallbackErr
		case noAddressOf(fallbackErr):
			return nil, err
		}
		return nil, fmt.Errorf("%w; then over %s: %w", err, familyName(networks[1]), fallbackErr)
	}
}

type dialDeadlineKey struct{}

// withDialDeadline is ctx carrying its deadline as a value too, for the
// dials of the requests made with it (dialDeadline). net/http dials a
// request's connection with context.WithoutCancel of the request's context,
// so that a connection a cancelled request began can still serve the next:
// the dial keeps the values and loses the deadline, and a prefer_ip4 or
// prefer_ip6 dial would give its first family the Dialer's whole timeout,
// failing the request at its deadline over an address that never answers
// without trying the other family.
func withDialDeadline(ctx context.Context) context.Context {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, dialDeadlineKey{}, deadline)
}

// dialDeadline is the deadline of a dial: its context's own, or else the
// one of the request that asked for it (withDialDeadline).
func dialDeadline(ctx context.Context) (time.Time, bool) {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline, true
	}
	deadline, ok := ctx.Value(dialDeadlineKey{}).(time.Time)
	return deadline, ok
}

// noAddressOf says whether err is a dial's over one family to a host that
// has no address of it: an address of the other family written as the host
// (net's "no suitable address"), or a name whose lookup for the family found
// nothing.
func noAddressOf(err error) bool {
	var addrErr *net.AddrError
	if errors.As(err, &addrErr) && strings.Contains(addrErr.Err, "no suitable address") {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// hostOf is the host of address, host:port.
func hostOf(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}
