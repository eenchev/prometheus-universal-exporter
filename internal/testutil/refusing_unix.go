//go:build unix

package testutil

import (
	"net"
	"strconv"
	"syscall"
	"testing"
)

// RefusingAddress is a local TCP address that refuses every connection for
// as long as the test runs. It is a socket that is bound but never listens,
// so a connection gets a reset at once. A closed listener's address refuses
// connections too, but only until another server is given the same port,
// often the test's own next httptest server. A redirect to the address then
// reached that server and looped until the client gave up after ten
// redirects, instead of the refusal the test expected. A bound socket keeps
// the port from every other socket until the test ends.
func RefusingAddress(t testing.TB) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	name, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	bound, ok := name.(*syscall.SockaddrInet4)
	if !ok {
		t.Fatalf("the bound socket's address is %T", name)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(bound.Port))
}
