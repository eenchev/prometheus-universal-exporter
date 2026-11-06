//go:build request_type_icmp && (linux || darwin)

package fetch

import (
	"net"
	"net/netip"
	"os"
	"syscall"
)

// icmpDial opens an unprivileged ICMP datagram socket connected to
// address: SOCK_DGRAM of IPPROTO_ICMP or IPPROTO_ICMPV6, which needs no
// raw socket, as Go's net package opens no ICMP socket of its own. The
// connected socket is a net.Conn whose remote address the target policy
// checks, and reads only that address's replies.
func icmpDial(address netip.Addr) (net.Conn, error) {
	family, proto := syscall.AF_INET, syscall.IPPROTO_ICMP
	var to syscall.Sockaddr = &syscall.SockaddrInet4{Addr: address.As4()}
	if address.Is6() {
		family, proto = syscall.AF_INET6, syscall.IPPROTO_ICMPV6
		to = &syscall.SockaddrInet6{Addr: address.As16()}
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
