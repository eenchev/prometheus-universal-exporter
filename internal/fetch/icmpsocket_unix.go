//go:build (!select_request_types || request_type_icmp) && (linux || darwin)

package fetch

import (
	"net"
	"net/netip"
	"os"
	"syscall"
)

// On Linux and macOS a scrape pings on icmpSocket; elsewhere icmpDial keeps
// the refusal of icmpsocket.go. The choice is made when the package starts,
// not by a file left out of the build elsewhere, so that every file of the
// type is in the build of the host the tree is worked on.
func init() { icmpDial = icmpSocket }

// icmpSocket opens an unprivileged ICMP datagram socket connected to
// address: SOCK_DGRAM of IPPROTO_ICMP or IPPROTO_ICMPV6, which needs no
// raw socket, as Go's net package opens no ICMP socket of its own. The
// connected socket is a net.Conn whose remote address the target policy
// checks, and reads only that address's replies.
func icmpSocket(address netip.Addr) (net.Conn, error) {
	// As4 panics on an IPv6 address, so each family's address is made
	// only for it.
	var family, proto int
	var to syscall.Sockaddr
	if address.Is4() {
		family, proto, to = syscall.AF_INET, syscall.IPPROTO_ICMP, &syscall.SockaddrInet4{Addr: address.As4()}
	} else {
		family, proto, to = syscall.AF_INET6, syscall.IPPROTO_ICMPV6, &syscall.SockaddrInet6{Addr: address.As16()}
	}
	fd, err := syscall.Socket(family, syscall.SOCK_DGRAM, proto)
	if err != nil {
		return nil, os.NewSyscallError("socket", err)
	}
	syscall.CloseOnExec(fd)
	if err := syscall.Connect(fd, to); err != nil {
		_ = syscall.Close(fd)
		return nil, os.NewSyscallError("connect", err)
	}
	file := os.NewFile(uintptr(fd), "icmp")
	defer func() { _ = file.Close() }()
	return net.FileConn(file)
}
