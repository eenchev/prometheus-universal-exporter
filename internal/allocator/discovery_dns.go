package allocator

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
)

// The meta labels of a dns_sd source's targets, Prometheus's own names.
const (
	dnsNameLabel      = "__meta_dns_name"
	dnsSRVTargetLabel = "__meta_dns_srv_record_target"
	dnsSRVPortLabel   = "__meta_dns_srv_record_port"
)

// refreshDNSSD looks each of n's names up, in their order, all within
// timeout, and gives a target for each record: an SRV record's target host
// and port, an A or AAAA record's address at n's port. A name that does not
// exist, or has no records of the type, gives no targets and is no failure,
// as Prometheus has it, so one service of several being gone leaves the
// others in force; any other failure of any name fails the whole answer,
// which then keeps the last good one, as every source's failure does, rather
// than dropping the targets of the name that could not be asked. A target
// found under several names, or twice under one, is taken once, with the
// labels of the first name in the list that gives it. The targets are sorted
// by address, so that the same records give the same answer whatever order
// the server, or the resolver, which shuffles SRV records of one priority,
// gives them in.
func refreshDNSSD(ctx context.Context, n *DNSSDConfig, mapping map[string]string, limit int, timeout time.Duration) ([]discovered, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resolver := fetch.DNSResolver(n.Server, false)
	server := n.Server
	if server == "" {
		server = "the system's resolver"
	}
	seen := map[string]bool{}
	var out []discovered
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
			d.Labels = mapLabels(d.Labels, mapping)
			out = append(out, d)
			if len(out) > limit {
				return nil, fmt.Errorf("the names' records give more than the %d targets max_targets allows", limit)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// lookupDNSSD asks for the records of n's type of name, absolute so that
// the system's search domains are never appended, and makes a target of
// each. The resolver leaves out a record with a malformed name and gives
// the others with an error, which is taken as an answer of those, as the
// dns request type takes it.
func lookupDNSSD(ctx context.Context, resolver *net.Resolver, n *DNSSDConfig, name string) ([]discovered, error) {
	absolute := name + "."
	var out []discovered
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
			out = append(out, discovered{Address: net.JoinHostPort(host, port), Labels: map[string]string{
				dnsNameLabel:      name,
				dnsSRVTargetLabel: host,
				dnsSRVPortLabel:   port,
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
		out = append(out, discovered{Address: net.JoinHostPort(unmapped(addr).String(), port), Labels: map[string]string{dnsNameLabel: name}})
	}
	return out, nil
}

// unmapped is addr without its zone, and an IPv4-mapped IPv6 address as
// the IPv4 address it is, so one address is always written alike.
func unmapped(addr netip.Addr) netip.Addr {
	return addr.Unmap().WithZone("")
}
