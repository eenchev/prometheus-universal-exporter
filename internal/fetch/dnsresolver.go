package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// What the dns request type and the allocator's dns_sd discovery source
// share: reading a server and a name, and a resolver that asks that server.
// This file has no build tag, unlike the request type's own, so that the
// allocator, which builds in every selection of request types, can use it.

// dnsDefaultPort is the port of a server that names none.
const dnsDefaultPort = "53"

// ParseDNSServer reads a DNS server: host, host:port or [v6]:port, port 53
// when it names none. Nothing else may come with it. It returns the server
// as host:port, the host as canonicalHost writes it.
func ParseDNSServer(raw string) (string, error) {
	server := strings.TrimSpace(raw)
	if server == "" {
		return "", errors.New("is written with blanks alone; name the server, host:port, or leave the key out for the system's resolver")
	}
	if strings.ContainsAny(server, "/?#@") {
		return "", fmt.Errorf("%q has a scheme, path, query or user in it; write the server as host or host:port, such as 10.0.0.53:53", raw)
	}
	host, port := server, dnsDefaultPort
	if h, p, err := net.SplitHostPort(server); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(server, "[") && strings.HasSuffix(server, "]") {
		host = server[1 : len(server)-1]
	} else if strings.Count(server, ":") == 1 {
		return "", fmt.Errorf("%q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::53]:53", raw)
	}
	if host == "" {
		return "", fmt.Errorf("%q names no host", raw)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", fmt.Errorf("%q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return "", fmt.Errorf("%q: %w", raw, err)
	}
	return net.JoinHostPort(canonical, port), nil
}

// CanonicalDNSName reads a DNS name as canonicalHost reads a host, held
// besides to the lengths of the DNS: at most 253 characters, each label from
// 1 to 63. It returns the name in lower case without a trailing dot; a name
// canonicalHost refuses is refused with its *TargetRefusedError, and one of
// the wrong length with an error starting with the name quoted.
func CanonicalDNSName(raw string) (string, error) {
	name, err := canonicalHost(raw)
	if err != nil {
		return "", err
	}
	if len(name) > 253 {
		return "", fmt.Errorf("%q is %d characters long; a DNS name has at most 253", raw, len(name))
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("%q has a label %q; each label of a DNS name, between the dots, has from 1 to 63 characters", raw, label)
		}
	}
	return name, nil
}

// DNSResolver is Go's own resolver (PreferGo), which needs no library and
// tells a name that does not exist, or has no records of the type, from a
// failure. With server, host:port as ParseDNSServer writes it, every
// question goes there instead of to the system's nameservers; with tcp, over
// TCP. Over UDP an answer marked truncated is asked again over TCP, as the
// resolver always does.
func DNSResolver(server string, tcp bool) *net.Resolver {
	dialer := &net.Dialer{}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if server != "" {
			address = server
		}
		if tcp {
			network = "tcp"
		}
		return dialer.DialContext(ctx, network, address)
	}}
}
