//go:build request_type_tcp

package fetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// udpEcho is a UDP server on loopback that answers each datagram with
// answer(datagram), recording what it was sent.
func udpEcho(t *testing.T, answer func([]byte) []byte, sent chan<- []byte) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			got := bytes.Clone(buf[:n])
			if sent != nil {
				sent <- got
			}
			_, _ = conn.WriteTo(answer(got), from)
		}
	}()
	return conn.LocalAddr().String()
}

// A tcp collector with udp sends send as one datagram and takes the first
// datagram back as the answer, labelled udp://; send_hex sends binary
// bytes; a datagram past the response limit fails as a limit exceeded, and
// a port nothing listens on fails at once rather than at the deadline.
func TestATCPCollectorSpeaksUDP(t *testing.T) {
	sent := make(chan []byte, 4)
	addr := udpEcho(t, func(b []byte) []byte { return append([]byte("got "), b...) }, sent)
	c := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{UDP: true, Send: "stats\r\n"} })
	if body, err := tcpScrape(t, c, addr, nil); err != nil || body != "got stats\r\n" {
		t.Fatalf("body %q, %v", body, err)
	}
	if got := <-sent; string(got) != "stats\r\n" {
		t.Errorf("sent %q", got)
	}
	if label, err := RequestLabelFor(addr, c, RequestOverrides{}); err != nil || label != "udp://"+addr {
		t.Errorf("label %q, %v", label, err)
	}
	binary := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own = TCPConfig{UDP: true, SendHex: "ff ff ff ff 54 53 6f 75\n72 63 65 00"}
	})
	if body, err := tcpScrape(t, binary, addr, nil); err != nil || body != "got \xff\xff\xff\xffTSource\x00" {
		t.Errorf("binary body %q, %v", body, err)
	}
	<-sent
	if method := RequestMethodFor(binary, RequestOverrides{}); method != "SEND" {
		t.Errorf("method %q", method)
	}
	big := udpEcho(t, func([]byte) []byte { return bytes.Repeat([]byte("x"), 101) }, nil)
	limited := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own, c.Request.MaxResponseBytes = TCPConfig{UDP: true, Send: "x"}, 100
	})
	if _, err := tcpScrape(t, limited, big, nil); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("a datagram of 101 bytes past a limit of 100: %v", err)
	}
	exact := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own, c.Request.MaxResponseBytes = TCPConfig{UDP: true, Send: "x"}, 101
	})
	if body, err := tcpScrape(t, exact, big, nil); err != nil || len(body) != 101 {
		t.Errorf("a datagram of 101 bytes within a limit of 101: %d bytes, %v", len(body), err)
	}
	closed, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	nobody := closed.LocalAddr().String()
	_ = closed.Close()
	began := time.Now()
	if _, err := tcpScrape(t, c, nobody, nil); err == nil || !errors.Is(err, syscall.ECONNREFUSED) || time.Since(began) > 3*time.Second {
		t.Errorf("a port nobody listens on: %v after %v", err, time.Since(began))
	}
}

// A tcp collector of request.unix_socket makes its exchange over the
// socket, the target only naming the service, with or without a port, in
// the url label unix:// and the socket's path; a target list holds the
// target's name as written, never looked up; and the socket's path is
// withheld from a failed probe's caller.
func TestATCPCollectorSpeaksOverAUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "pue")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "haproxy.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				if line, err := testutil.ReadUntil(conn, "\n"); err == nil && line == "show info\n" {
					_, _ = io.WriteString(conn, "Name: HAProxy\nUptime_sec: 60\n")
				}
			}()
		}
	}()
	c := tcpCollector(t, func(c *model.Collector) {
		c.Request.UnixSocket, c.Request.Own = socket, TCPConfig{Send: "show info\n"}
	})
	for _, target := range []string{"haproxy", "haproxy:9999"} {
		if body, err := tcpScrape(t, c, target, nil); err != nil || body != "Name: HAProxy\nUptime_sec: 60\n" {
			t.Errorf("%s: body %q, %v", target, body, err)
		}
	}
	if label, err := RequestLabelFor("haproxy", c, RequestOverrides{}); err != nil || label != "unix://"+socket {
		t.Errorf("label %q, %v", label, err)
	}
	// A target that a name list leaves out is refused, not looked up.
	listed := tcpCollector(t, func(c *model.Collector) {
		c.Request.UnixSocket, c.Request.Own, c.Request.AllowedTargets = socket, TCPConfig{Send: "show info\n"}, []string{"haproxy"}
	})
	if _, err := tcpScrape(t, listed, "haproxy", nil); err != nil {
		t.Errorf("a listed name: %v", err)
	}
	if _, err := tcpScrape(t, listed, "other", nil); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("a name the list leaves out: %v", err)
	}
	gone := tcpCollector(t, func(c *model.Collector) {
		c.Request.UnixSocket, c.Request.Own = filepath.Join(dir, "none.sock"), TCPConfig{Send: "show info\n"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = FetchCollector(ctx, "haproxy", gone, RequestOverrides{}, nil)
	if err == nil || strings.Contains(model.CallerText(err), dir) || !strings.Contains(err.Error(), dir) {
		t.Errorf("a socket that is not there: %v, the caller told %q", err, model.CallerText(err))
	}
	// Without the socket a target still names its port.
	if err := CheckTarget(tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "x"} }), "haproxy", false); err == nil {
		t.Error("a target without a port was taken without request.unix_socket")
	}
}

// request.answer_bytes ends the answer after that many bytes, though the
// server keeps the connection open, as a Modbus TCP server does; one that
// closes sooner fails the scrape.
func TestATCPAnswerEndsAtAnswerBytes(t *testing.T) {
	// A read of two holding registers, function 3: the answer's MBAP
	// header, the unit, the function, a byte count and the registers.
	request := []byte{0, 1, 0, 0, 0, 6, 1, 3, 0, 0, 0, 2}
	answer := []byte{0, 1, 0, 0, 0, 7, 1, 3, 4, 0, 42, 1, 0}
	modbus := testutil.NewTCPStub(t, func(conn net.Conn) {
		got := make([]byte, len(request))
		if _, err := io.ReadFull(conn, got); err == nil && bytes.Equal(got, request) {
			_, _ = conn.Write(answer)
		}
		_, _ = io.Copy(io.Discard, conn)
	})
	c := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own = TCPConfig{SendHex: "0001 0000 0006 0103 0000 0002", AnswerBytes: len(answer)}
	})
	if body, err := tcpScrape(t, c, modbus.Addr, nil); err != nil || body != string(answer) {
		t.Fatalf("body %x, %v", body, err)
	}
	// The server reads the whole request before it answers short and
	// closes: closing with the request unread would make the system reset
	// the connection, which the client may read before the 5 bytes'
	// end, and the scrape would fail as reset rather than as cut short.
	short := testutil.NewTCPStub(t, func(conn net.Conn) {
		if _, err := io.ReadFull(conn, make([]byte, len(request))); err == nil {
			_, _ = conn.Write(answer[:5])
		}
	})
	if _, err := tcpScrape(t, c, short.Addr, nil); err == nil || !strings.Contains(err.Error(), "closed the connection after 5 bytes, and request.answer_bytes is 13") {
		t.Errorf("an answer cut short: %v", err)
	}
}

// The keys of the transports and of binary protocols are checked when the
// configuration loads.
func TestTCPTransportAndBinaryKeysAreChecked(t *testing.T) {
	for _, c := range []struct {
		edit func(*model.Collector)
		want string
	}{
		{func(c *model.Collector) { c.Request.Own = TCPConfig{UDP: true} }, "sets request.udp without request.send or request.send_hex"},
		{func(c *model.Collector) { c.Request.Own = TCPConfig{UDP: true, Send: "x", Terminator: "\n"} }, "over UDP the answer is the first datagram"},
		{func(c *model.Collector) { c.Request.Own = TCPConfig{UDP: true, Send: "x", Greeting: "^x"} }, "a UDP server says nothing before"},
		{func(c *model.Collector) { c.Request.Own, c.Request.StartTLS = TCPConfig{UDP: true, Send: "x"}, "smtp" }, "speaks no TLS over UDP"},
		{func(c *model.Collector) {
			c.Request.Own, c.Request.UnixSocket = TCPConfig{UDP: true, Send: "x"}, "/run/x.sock"
		}, "a Unix socket is reached as a stream"},
		{func(c *model.Collector) { c.Request.Own, c.Request.UnixSocket = TCPConfig{Send: "x"}, "run/x.sock" }, "is not an absolute path"},
		{func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "x", SendHex: "00"} }, "sets request.send and request.send_hex"},
		{func(c *model.Collector) { c.Request.Own = TCPConfig{SendHex: "0g"} }, `"g" is not a hexadecimal digit`},
		{func(c *model.Collector) { c.Request.Own = TCPConfig{SendHex: "001"} }, "3 hexadecimal digits, an odd number"},
		{func(c *model.Collector) { c.Request.Own = TCPConfig{SendHex: " \n"} }, "it holds no byte"},
		{func(c *model.Collector) { c.Request.Own = TCPConfig{SendHex: "{{param_x}}"} }, "request.send_hex has a placeholder"},
		{func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "x", AnswerBytes: -1} }, "request.answer_bytes is -1"},
		{func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "x", AnswerBytes: 4, Terminator: "\n"} }, "sets request.answer_bytes and request.terminator"},
		{func(c *model.Collector) { c.Request.Own = TCPConfig{Greeting: "^220", AnswerBytes: 4} }, "sets request.greeting and request.answer_bytes without request.send or send_hex"},
		{func(c *model.Collector) { c.Request.Own, c.Request.StartTLS = TCPConfig{AnswerBytes: 4}, "smtp" }, "sets request.starttls and request.answer_bytes without request.send or send_hex"},
	} {
		x := &model.Collector{Name: "stats", Request: model.RequestConfig{Type: RequestTypeTCP}}
		c.edit(x)
		if err := ValidateRequest(x); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("got %v, want %q", err, c.want)
		}
	}
	// send_hex is something sent, as send is: a greeting or STARTTLS
	// before it, and an end of the answer after it, are taken.
	for _, own := range []TCPConfig{{Greeting: "^220", SendHex: "00", Terminator: "\n"}, {Greeting: "^220", SendHex: "00", AnswerBytes: 2}} {
		x := &model.Collector{Name: "stats", Request: model.RequestConfig{Type: RequestTypeTCP, Own: own}}
		if err := ValidateRequest(x); err != nil {
			t.Errorf("%+v: %v", own, err)
		}
	}
}
