//go:build !select_request_types || request_type_http

package fetch

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// http10Server answers every request in HTTP/1.0, as an old server or an
// appliance still does, and closes the connection after it.
func http10Server(t *testing.T) string {
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
			go func() {
				defer func() { _ = conn.Close() }()
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					return
				}
				_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nok")
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

// The trip has the version of HTTP the answer came in, "1.0", "1.1" or
// "2", whether or not the request was traced, and the address of the
// connection it came on when it was: 127.0.0.1 for a target there, and
// null for a request that was not traced, one through a proxy, whose
// connection is to the proxy — the environment's, asked the request
// whole, and a collector's own, asked for a tunnel — and one over a Unix
// socket.
func TestTheTripHasTheAnswersHTTPVersionAndRemoteIP(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer plain.Close()
	h2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	h2.EnableHTTP2 = true
	h2.StartTLS()
	defer h2.Close()
	socket, _ := socketServer(t, nil, nil)

	traced := httpCollector(t, nil)
	untraced := httpCollector(t, func(c *model.Collector) { c.TimingsUnread = true })
	secure := httpCollector(t, func(c *model.Collector) {
		c.Request.EnableHTTP2 = true
		c.Request.TLS.InsecureSkipVerify = true
	})
	proxy := newTestProxy(t)
	tunnelled := httpCollector(t, func(c *model.Collector) {
		c.Request.ProxyURL = proxy.URL
		c.Request.TLS.InsecureSkipVerify = true
	})
	loopback := netip.MustParseAddr("127.0.0.1")
	for _, c := range []struct {
		name      string
		target    string
		collector *model.Collector
		version   any
		remote    any
	}{
		{"HTTP/1.1, traced", plain.URL, traced, "1.1", "127.0.0.1"},
		{"HTTP/1.1, not traced", plain.URL, untraced, "1.1", nil},
		{"HTTP/1.0", http10Server(t), traced, "1.0", "127.0.0.1"},
		{"HTTP/2", h2.URL, secure, "2", "127.0.0.1"},
		{"through a collector's proxy's tunnel", h2.URL, tunnelled, "1.1", nil},
		{"over a Unix socket", "http://docker.example/", socketCollector(t, "docker", socket), "1.1", nil},
	} {
		response, err := FetchCollector(context.Background(), c.target, c.collector, RequestOverrides{}, nil)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := response.Trip.JQHTTPVersion(); got != c.version {
			t.Errorf("%s: $http_version %v, want %v", c.name, got, c.version)
		}
		if got := response.Trip.JQRemoteIP(); got != c.remote {
			t.Errorf("%s: $remote_ip %v, want %v", c.name, got, c.remote)
		}
		if c.remote != nil && response.Trip.RemoteIP != loopback {
			t.Errorf("%s: RemoteIP %v", c.name, response.Trip.RemoteIP)
		}
	}
	if asked, _ := proxy.seen(); len(asked) != 1 || asked[0] != "CONNECT "+h2.Listener.Addr().String() {
		t.Errorf("the collector's proxy was asked %v", asked)
	}

	// Through the environment's proxy, which answers the request itself.
	environment := proxyEnvironment(t)
	unresolvable(t, nil)
	response, err := FetchCollector(context.Background(), "http://site.example/", httpCollector(t, nil), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if asked, _ := environment.seen(); len(asked) != 1 {
		t.Fatalf("the environment's proxy was asked %v", asked)
	}
	if response.Trip.JQRemoteIP() != nil || response.Trip.JQHTTPVersion() != "1.1" {
		t.Errorf("through the environment's proxy: $remote_ip %v, $http_version %v", response.Trip.JQRemoteIP(), response.Trip.JQHTTPVersion())
	}

	var none *HTTPTrip
	if none.JQHTTPVersion() != nil || none.JQRemoteIP() != nil {
		t.Error("no trip has a version or an address")
	}
	mapped := &HTTPTrip{ProtoMajor: 1, ProtoMinor: 1, RemoteIP: remoteIP(&net.TCPAddr{IP: net.ParseIP("::ffff:203.0.113.7")})}
	if mapped.JQRemoteIP() != "203.0.113.7" {
		t.Errorf("an IPv4 address mapped into IPv6 is %v", mapped.JQRemoteIP())
	}
	if six := (&HTTPTrip{RemoteIP: remoteIP(&net.TCPAddr{IP: net.ParseIP("2001:db8::7")})}); six.JQRemoteIP() != "2001:db8::7" {
		t.Errorf("an IPv6 address is %v", six.JQRemoteIP())
	}
	if remoteIP(&net.UnixAddr{Name: "/run/x.sock", Net: "unix"}).IsValid() {
		t.Error("a Unix socket's address is an IP address")
	}
}
