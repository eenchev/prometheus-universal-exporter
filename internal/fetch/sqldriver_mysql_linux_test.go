//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// A mysql connection keeps the TCP keepalives the exporter's dialer set:
// switched off with keepalive check other than tcp, and at the collector's
// period with it, though the driver switches them on for a connection it
// sees as TCP; and the connection still offers its file descriptor, which
// the driver's check of an idle connection reads.
func TestAMySQLConnectionKeepsTheExportersTCPKeepalives(t *testing.T) {
	for _, tc := range []struct {
		interval time.Duration
		on       int
	}{
		{0, 0},
		{7 * time.Second, 1},
	} {
		port := mysqlRefusingServer(t)
		spec := sqlSpec(t, port, nil, nil)
		files := make(chan *os.File, 1)
		dial := sqlDial(nil, "127.0.0.1", tc.interval)
		spec.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			// A duplicate of the socket, which outlives the driver's
			// closing of the connection.
			file, err := conn.(*net.TCPConn).File()
			if err != nil {
				return nil, err
			}
			files <- file
			return conn, nil
		}
		wrapped, err := mysqlDial(spec.dial)(context.Background(), "tcp", spec.hostPort)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := wrapped.(syscall.Conn); !ok {
			t.Errorf("the driver's connection offers no file descriptor: %T", wrapped)
		}
		_ = wrapped.Close()
		_ = (<-files).Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = connectMySQL(ctx, spec)
		cancel()
		if err == nil {
			t.Fatal("connected to a server that refuses every connection")
		}
		file := <-files
		on, errOn := syscall.GetsockoptInt(int(file.Fd()), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
		idle, errIdle := syscall.GetsockoptInt(int(file.Fd()), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
		_ = file.Close()
		if errOn != nil || errIdle != nil || on != tc.on || tc.on == 1 && idle != int(tc.interval/time.Second) {
			t.Errorf("interval %v: keepalive %d every %ds (%v %v), want %d every %v", tc.interval, on, idle, errOn, errIdle, tc.on, tc.interval)
		}
	}
}

// mysqlRefusingServer is a stand-in for MySQL that answers each connection
// with an error packet in place of its greeting, and closes it.
func mysqlRefusingServer(t *testing.T) string {
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
			message := "Too many connections"
			packet := append([]byte{byte(3 + len(message)), 0, 0, 0, 0xff, 0x10, 0x04}, message...)
			_, _ = conn.Write(packet)
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}
