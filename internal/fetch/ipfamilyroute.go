//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import (
	"errors"
	"fmt"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// ipFamilyRoute refuses request.ip_family on a collector whose requests are
// not connected to the target's address by the exporter: one that sends them
// to a Unix socket, or through a proxy of its own (proxy_url other than
// direct), which connects to the target itself. The environment's proxy is
// known only when a request is sent (policyGuard.check).
func ipFamilyRoute(c *model.Collector) error {
	switch {
	case CollectorPolicy(c).UnixSocket != "":
		return errors.New("ip_family does not apply to a collector whose requests go to request.unix_socket, whose connection has no address family")
	case c.Request.ProxyURL != "" && c.Request.ProxyURL != model.ProxyDirect:
		return errors.New("ip_family does not apply to a collector whose requests go through request.proxy_url: the proxy looks the target's name up and chooses the family it connects over; leave ip_family out, or set proxy_url: direct")
	}
	return nil
}

// checkIPFamilyOverride is an http or graphite collector's CheckOverride: the
// ip_family probe parameter, or a static target's, on a collector whose
// requests ipFamilyRoute refuses it for.
func checkIPFamilyOverride(c *model.Collector, key string) error {
	if key != "ip_family" {
		return nil
	}
	if err := ipFamilyRoute(c); err != nil {
		return fmt.Errorf("collector %q: %w", c.Name, err)
	}
	return nil
}
