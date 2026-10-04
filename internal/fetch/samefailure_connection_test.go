//go:build !select_request_types || request_type_http || request_type_graphite || request_type_grpc

package fetch

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// connectionTarget hands every connection made to it to serve, and returns
// its address.
func connectionTarget(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return listener.Addr().String()
}

// resetConnection closes conn so that the other end reads a reset rather
// than an end of file.
func resetConnection(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

// resettingTarget is the address of a target that resets every connection
// made to it once it has been sent something: a request, or the start of a
// TLS handshake.
func resettingTarget(t *testing.T) string {
	t.Helper()
	return connectionTarget(t, func(conn net.Conn) {
		_, _ = conn.Read(make([]byte, 16))
		resetConnection(conn)
	})
}

// fetchFailures fetches target twice, with a connection of its own each
// time, and returns the two errors.
func fetchFailures(t *testing.T, c *model.Collector, target string) (first, second error) {
	t.Helper()
	failure := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
		if err == nil {
			t.Fatalf("the fetch of %s did not fail", target)
		}
		return err
	}
	return failure(), failure()
}
