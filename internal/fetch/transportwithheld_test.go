package fetch

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The addresses of the exporter's side of the network — where a connection
// was made from, the proxy a request went through, with all a connection to
// it failed with and what it said in refusing a tunnel, the name server a lookup
// asked, and the source address in a resolver's or a gRPC status's text —
// are withheld from a probe's caller; the address the connection went to
// stays, and the error keeps them all.
func TestTheExportersSideOfTheNetworkIsWithheldFromTheCaller(t *testing.T) {
	target := &net.TCPAddr{IP: net.IPv4(10, 1, 2, 3), Port: 443}
	for name, c := range map[string]struct {
		err        error
		kept, told string
	}{
		"a reset connection": {
			err:  &url.Error{Op: "Get", URL: "https://db.example/", Err: &net.OpError{Op: "read", Net: "tcp", Source: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 51234}, Addr: target, Err: syscall.ECONNRESET}},
			kept: "10.0.0.7:51234",
			told: `Get "https://db.example/": read tcp ` + model.WithheldMark + `->10.1.2.3:443: connection reset by peer`,
		},
		"a proxy that refused": {
			err:  &url.Error{Op: "Get", URL: "https://db.example/", Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 3128}, Err: syscall.ECONNREFUSED}}},
			kept: "10.0.0.5:3128",
			told: `Get "https://db.example/": proxyconnect tcp: ` + model.WithheldMark,
		},
		"a proxy whose name did not resolve": {
			err:  &url.Error{Op: "Get", URL: "https://db.example/", Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Name: "proxy.corp.example", Err: "no such host", IsNotFound: true}}}},
			kept: "proxy.corp.example",
			told: `Get "https://db.example/": proxyconnect tcp: ` + model.WithheldMark,
		},
		"a proxy that refused the tunnel": {
			err:  &url.Error{Op: "Get", URL: "https://db.example/", Err: &ProxyRefusedError{Target: "db.example:443", Status: "403 Blocked by policy at proxy-7.corp"}},
			kept: "proxy-7.corp",
			told: `Get "https://db.example/": the proxy refused the tunnel to db.example:443: ` + model.WithheldMark,
		},
		"a lookup's name server": {
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Name: "db.example", Server: "10.96.0.10:53", Err: "read udp 10.0.0.7:41234->10.96.0.10:53: i/o timeout", IsTimeout: true}},
			kept: "on 10.96.0.10:53",
			told: "dial tcp: lookup db.example on " + model.WithheldMark + ": read udp " + model.WithheldMark + "->10.96.0.10:53: i/o timeout",
		},
		"a gRPC status": {
			err:  &CallStatusError{Code: 14, CodeName: "Unavailable", Message: "connection error: desc = \"transport: read tcp 10.0.0.7:5050->10.1.2.3:443: read: connection reset by peer\""},
			kept: "10.0.0.7:5050",
			told: model.WithheldMark + "->10.1.2.3:443",
		},
	} {
		marked := withTransportWithheld(c.err)
		if marked.Error() != c.err.Error() || !strings.Contains(marked.Error(), c.kept) || !errors.Is(marked, c.err) {
			t.Errorf("%s: the error reads %q, want it as it was", name, marked)
		}
		if told := model.CallerText(marked); strings.Contains(told, c.kept) || !strings.Contains(told, c.told) {
			t.Errorf("%s: the caller is told %q, want %q in it", name, told, c.told)
		}
	}
	plain := &url.Error{Op: "Get", URL: "https://db.example/", Err: &net.OpError{Op: "dial", Net: "tcp", Addr: target, Err: syscall.ECONNREFUSED}}
	if marked := withTransportWithheld(plain); model.CallerText(marked) != plain.Error() || marked.Error() != plain.Error() {
		t.Error("an error naming only the address the connection went to is marked")
	}
}
