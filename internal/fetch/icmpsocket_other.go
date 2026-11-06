//go:build request_type_icmp && !linux && !darwin

package fetch

import (
	"errors"
	"net"
	"net/netip"
	"runtime"
)

// icmpDial fails on a system the exporter opens no unprivileged ICMP socket
// on.
func icmpDial(netip.Addr) (net.Conn, error) {
	return nil, errors.New("the icmp request type pings on Linux and macOS alone, and the exporter runs on " + runtime.GOOS)
}
