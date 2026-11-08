//go:build !select_request_types || request_type_icmp

package fetch

import (
	"errors"
	"net"
	"net/netip"
	"runtime"
)

// icmpDial opens the socket a scrape pings on: on Linux and macOS
// icmpSocket (icmpsocket_unix.go), and on a system the exporter opens no
// unprivileged ICMP socket on, a refusal.
var icmpDial = func(netip.Addr) (net.Conn, error) {
	return nil, errors.New("the icmp request type pings on Linux and macOS alone, and the exporter runs on " + runtime.GOOS)
}
