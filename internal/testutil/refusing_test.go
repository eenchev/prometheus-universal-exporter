package testutil

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
)

// The address refuses connections, and keeps its port while the test runs:
// no server can listen on it, and none that asks for any free port is
// handed it.
func TestARefusingAddressKeepsItsPort(t *testing.T) {
	address := RefusingAddress(t)
	if listener, err := net.Listen("tcp", address); err == nil {
		_ = listener.Close()
		t.Fatalf("a server could listen on %s, the refusing address", address)
	}
	for range 200 {
		server := httptest.NewServer(http.NotFoundHandler())
		taken := server.Listener.Addr().String()
		server.Close()
		if taken == address {
			t.Fatalf("a server was handed %s, the refusing address", address)
		}
	}
	conn, err := net.Dial("tcp", address)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("a connection to %s was taken", address)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("a connection to %s failed with %v, want it refused", address, err)
	}
}
