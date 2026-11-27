package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The defaults of a dns_sd source.
const (
	// DefaultDNSSDRefresh is Prometheus's own for DNS service discovery.
	DefaultDNSSDRefresh = 30 * time.Second
	// DefaultDNSSDType is the record type a dns_sd source asks for by
	// default.
	DefaultDNSSDType = "SRV"
)

// DNSSDConfig is a dns_sd source (model.DNSSDConfig).
type DNSSDConfig = model.DNSSDConfig

// CheckDNSSD checks a dns_sd source asked every refresh, which refreshText
// names, when the file that has it loads. It writes each name as asked, and
// the default type and the server as host:port.
func CheckDNSSD(n *DNSSDConfig, refresh time.Duration, refreshText string) error {
	if len(n.Names) == 0 {
		return errors.New("dns_sd.names must not be empty")
	}
	seen := map[string]bool{}
	for i, raw := range n.Names {
		name, err := DNSName(raw)
		if err != nil {
			return fmt.Errorf("dns_sd.names entry %w", err)
		}
		if seen[name] {
			return fmt.Errorf("dns_sd.names lists %q twice", name)
		}
		seen[name] = true
		n.Names[i] = name
	}
	if n.Type == "" {
		n.Type = DefaultDNSSDType
	}
	switch n.Type {
	case "SRV":
		if n.Port != 0 {
			return errors.New("dns_sd.port is set for type SRV, whose records carry their own ports; leave it out")
		}
	case "A", "AAAA":
		if n.Port < 1 || n.Port > 65535 {
			return fmt.Errorf("dns_sd.port must be a port from 1 to 65535 for type %s, whose records carry no port", n.Type)
		}
	default:
		return fmt.Errorf("dns_sd.type %q must be SRV, A or AAAA", n.Type)
	}
	if n.Server != "" {
		server, err := fetch.ParseDNSServer(n.Server)
		if err != nil {
			return fmt.Errorf("dns_sd.server %w", err)
		}
		n.Server = server
	}
	return CheckTimeout("dns_sd", n.Timeout, refresh, refreshText)
}

// DNSName reads a name of dns_sd.names: a DNS name, such as
// _metrics._tcp.example.com, in lower case without a trailing dot. So a
// name written twice in two spellings is told, and every replica asks for
// the same name and labels its targets with it.
func DNSName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("is empty")
	}
	if strings.ContainsAny(name, "/?#@:[]") {
		return "", fmt.Errorf("%q is not a DNS name; the server is dns_sd.server and the port dns_sd.port", raw)
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return "", fmt.Errorf("%q is an IP address, not a name to look up", raw)
	}
	canonical, err := fetch.CanonicalDNSName(name)
	var refused *fetch.TargetRefusedError
	if errors.As(err, &refused) {
		return "", fmt.Errorf("%q is not a DNS name: %s", raw, refused.Reason)
	}
	return canonical, err
}

// The meta labels of a dns_sd source's targets, Prometheus's own names.
const (
	DNSNameLabel      = "__meta_dns_name"
	DNSSRVTargetLabel = "__meta_dns_srv_record_target"
	DNSSRVPortLabel   = "__meta_dns_srv_record_port"
)

// RefreshDNSSD looks up each of n's names, in their order, all within
// timeout, and gives a target for each record: an SRV record's target host
// and port, or an A or AAAA record's address at n's port.
//   - A name that does not exist, or has no records of the type, gives no
//     targets and is no failure, as Prometheus has it. So one service of
//     several being gone leaves the others in force.
//   - Any other failure of any name fails the whole answer. The last good
//     answer is then kept, as on every source's failure, rather than
//     dropping the targets of the name that could not be asked.
//   - A target found under several names, or twice under one, is taken once,
//     with the labels of the first name in the list that gives it.
//
// The targets are sorted by address, so the same records give the same
// answer whatever order the server or the resolver gives them in (the
// resolver shuffles SRV records of one priority).
func RefreshDNSSD(ctx context.Context, n *DNSSDConfig, mapping map[string]string, limit int, timeout time.Duration) ([]Target, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resolver := fetch.DNSResolver(n.Server, false)
	server := n.Server
	if server == "" {
		server = "the system's resolver"
	}
	seen := map[string]bool{}
	var out []Target
	for _, name := range n.Names {
		found, err := lookupDNSSD(ctx, resolver, n, name)
		if err != nil {
			var dnsErr *net.DNSError
			switch {
			case ctx.Err() != nil || errors.As(err, &dnsErr) && dnsErr.IsTimeout:
				// The resolver's own deadline is the context's, so a
				// server that never answers can end in either.
				return nil, fmt.Errorf("looking up %s %s on %s: the server did not answer within the timeout, %s", n.Type, name, server, timeout)
			case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
				continue
			case errors.As(err, &dnsErr):
				return nil, fmt.Errorf("looking up %s %s on %s: %s", n.Type, name, server, dnsErr.Err)
			default:
				return nil, fmt.Errorf("looking up %s %s on %s: %w", n.Type, name, server, err)
			}
		}
		for _, d := range found {
			if seen[d.Address] {
				continue
			}
			seen[d.Address] = true
			d.Labels = MapLabels(d.Labels, mapping)
			out = append(out, d)
			if len(out) > limit {
				return nil, fmt.Errorf("the names' records give more than the %d targets max_targets allows", limit)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// lookupDNSSD asks for the records of n's type of name, made absolute so the
// system's search domains are never appended, and makes a target of each.
// The resolver leaves out a record with a malformed name and gives the
// others with an error. That is taken as an answer of those others, as the
// dns request type takes it.
func lookupDNSSD(ctx context.Context, resolver *net.Resolver, n *DNSSDConfig, name string) ([]Target, error) {
	absolute := name + "."
	var out []Target
	if n.Type == "SRV" {
		_, records, err := resolver.LookupSRV(ctx, "", "", absolute)
		if err != nil && len(records) == 0 {
			return nil, err
		}
		for _, srv := range records {
			// A target of "." says the service is not offered at this
			// name (RFC 2782). A name is case-blind, and is written in
			// lower case without its trailing dot, in the address and the
			// label alike, so that every replica writes it the same.
			host := strings.ToLower(strings.TrimSuffix(srv.Target, "."))
			if host == "" {
				continue
			}
			port := strconv.Itoa(int(srv.Port))
			out = append(out, Target{Address: net.JoinHostPort(host, port), Labels: map[string]string{
				DNSNameLabel:      name,
				DNSSRVTargetLabel: host,
				DNSSRVPortLabel:   port,
			}})
		}
		return out, nil
	}
	network := "ip4"
	if n.Type == "AAAA" {
		network = "ip6"
	}
	addrs, err := resolver.LookupNetIP(ctx, network, absolute)
	if err != nil && len(addrs) == 0 {
		return nil, err
	}
	port := strconv.Itoa(n.Port)
	for _, addr := range addrs {
		out = append(out, Target{Address: net.JoinHostPort(unmapped(addr).String(), port), Labels: map[string]string{DNSNameLabel: name}})
	}
	return out, nil
}

// unmapped is addr without its zone, and an IPv4-mapped IPv6 address as
// the IPv4 address it is, so one address is always written alike.
func unmapped(addr netip.Addr) netip.Addr {
	return addr.Unmap().WithZone("")
}
