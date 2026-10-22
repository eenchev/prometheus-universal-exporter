//go:build unix

package testutil

import (
	"net"
	"strconv"
	"syscall"
	"testing"
)

// RefusingAddress is a local TCP address that refuses every connection for
// as long as the test runs: a socket bound to it that never listens, so a
// connection is answered with a reset at once. The address of a listener
// that was closed refuses connections too, but only until another server,
// often the test's own next httptest server, is handed the same port: a
// redirect to it then reached that server and went round until the client
// gave up after ten redirects, in place of the refusal the test waited for.
// A bound socket keeps the port from every other socket until the test ends.
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
