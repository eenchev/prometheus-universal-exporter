//go:build (!select_request_types || request_type_sql) && sql_driver_clickhouse

package fetch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The clickhouse driver's port is its protocol's, plain or with TLS; every
// statement is sent with readonly=1; the protocol, compression and TLS are
// the options' and the spec's; and it connects through the exporter's
// dialer.
func TestTheClickHouseDriver(t *testing.T) {
	for options, want := range map[*map[string]string]string{
		{}:                                     "9000",
		{"secure": "true"}:                     "9440",
		{"protocol": "http"}:                   "8123",
		{"protocol": "http", "secure": "true"}: "8443",
		{"protocol": "native", "compress": "lz4"}: "9000",
	} {
		if got := clickHousePort(*options); got != want {
			t.Errorf("%v: port %s, want %s", *options, got, want)
		}
	}
	withTLS := &tls.Config{ServerName: "db.example", MinVersion: tls.VersionTLS12}
	options := clickHouseOptions(sqlSpec(t, "8443", withTLS, map[string]string{"protocol": "http", "compress": "zstd", "secure": "true"}))
	if options.Protocol != clickhouse.HTTP || options.Compression == nil || options.Compression.Method != clickhouse.CompressionZSTD || options.TLS != withTLS ||
		options.Settings["readonly"] != 1 || options.Auth.Username != "metrics" || options.Auth.Password != "p@ss:w/rd?" || options.Auth.Database != "app" || options.Addr[0] != "127.0.0.1:8443" {
		t.Errorf("%+v", options)
	}
	f := newSQLFixture(t)
	c := f.collector(t, "db", SQLDriverClickHouse, "SELECT 1 AS one", func(r *model.RequestConfig) { r.Options = map[string]string{"protocol": "http"} })
	if address, err := parseSQLTarget(c, "db.example"); err != nil || address.hostPort != "db.example:8123" {
		t.Errorf("%+v %v", address, err)
	}
	for _, protocol := range []string{"native", "http"} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := connectClickHouse(ctx, sqlSpec(t, listen(t), nil, map[string]string{"protocol": protocol}))
		cancel()
		var refused *TargetRefusedError
		if protocol == "native" && !errors.As(err, &refused) || err == nil {
			t.Errorf("%s: %v, want the policy's refusal", protocol, err)
		}
	}
}

// On the native protocol with TLS, the driver's connection is TLS from the
// first byte: a server with a certificate the collector trusts completes a
// handshake, one the collector does not trust is refused unless
// insecure_skip_verify says not to verify it, and a server speaking no TLS
// receives a ClientHello and never the login in clear.
func TestClickHouseNativeWithTLSHandshakesBeforeTheLogin(t *testing.T) {
	server := httptest.NewUnstartedServer(nil)
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	for _, tc := range []struct {
		name      string
		config    *tls.Config
		handshake bool
	}{
		{"trusted", &tls.Config{ServerName: "127.0.0.1", RootCAs: roots, MinVersion: tls.VersionTLS12}, true},
		{"untrusted", &tls.Config{ServerName: "127.0.0.1", RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}, false},
		{"not verified", &tls.Config{ServerName: "127.0.0.1", InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, true}, //nolint:gosec // The case of insecure_skip_verify.
	} {
		port, handshakes := clickHouseTLSListener(t, server.TLS.Certificates)
		spec := sqlSpec(t, port, tc.config, map[string]string{"secure": "true"})
		spec.dial = sqlDial(nil, "127.0.0.1", 0)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := connectClickHouse(ctx, spec)
		cancel()
		if completed := <-handshakes; completed != tc.handshake {
			t.Errorf("%s: handshake completed %t, want %t (%v)", tc.name, completed, tc.handshake, err)
		}
		var unknown x509.UnknownAuthorityError
		if !tc.handshake && !errors.As(err, &unknown) {
			t.Errorf("%s: %v, want the certificate refused", tc.name, err)
		}
	}
	port, received := clickHousePlainListener(t)
	spec := sqlSpec(t, port, &tls.Config{ServerName: "127.0.0.1", RootCAs: roots, MinVersion: tls.VersionTLS12}, map[string]string{"secure": "true"})
	spec.dial = sqlDial(nil, "127.0.0.1", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := connectClickHouse(ctx, spec); err == nil {
		t.Error("connected to a server speaking no TLS")
	}
	if got := <-received; len(got) < 3 || got[0] != 0x16 || got[1] != 0x03 || bytes.Contains(got, []byte(spec.password)) || bytes.Contains(got, []byte(spec.username)) {
		t.Errorf("a server speaking no TLS received %q, want a ClientHello", got)
	}
}

// clickHouseTLSListener is a TLS server with certificates that sends, for
// each connection, whether its handshake completed, and closes it.
func clickHouseTLSListener(t *testing.T, certificates []tls.Certificate) (port string, handshakes chan bool) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	handshakes = make(chan bool, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			secured := tls.Server(conn, &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12})
			handshakes <- secured.Handshake() == nil
			_ = secured.Close()
		}
	}()
	_, port, _ = net.SplitHostPort(listener.Addr().String())
	return port, handshakes
}

// clickHousePlainListener is a server speaking no TLS that sends what each
// connection wrote first: a whole TLS record when it starts with one, or
// its first read otherwise (the driver's hello, with the login); then it
// closes it.
func clickHousePlainListener(t *testing.T) (port string, received chan []byte) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received = make(chan []byte, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var got []byte
			buf := make([]byte, 4096)
			for !clickHouseFirstWrite(got) {
				n, err := conn.Read(buf)
				got = append(got, buf[:n]...)
				if err != nil {
					break
				}
			}
			received <- got
			_ = conn.Close()
		}
	}()
	_, port, _ = net.SplitHostPort(listener.Addr().String())
	return port, received
}

// clickHouseFirstWrite says got holds a connection's first write: a whole
// TLS record, or anything that does not start one.
func clickHouseFirstWrite(got []byte) bool {
	return len(got) > 0 && got[0] != 0x16 || len(got) >= 5 && len(got) >= 5+int(binary.BigEndian.Uint16(got[3:5]))
}

// On the http protocol, the driver's requests go to the target even when
// HTTP_PROXY names a proxy: the proxy, a listener here, receives nothing,
// while the target's stand-in receives the driver's first request. The
// target is a name, as a proxy is not used for a loopback address; the
// exporter's dialer, here, connects that name to the stand-in and anything
// else where it is. net/http reads the proxy variables once in a process,
// so the transport the driver is given is also checked directly: the
// driver's own, with a proxy, comes back without one.
func TestClickHouseOverHTTPUsesNoProxy(t *testing.T) {
	proxied := make(chan string, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied <- r.Method + " " + r.RequestURI
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	received := make(chan string, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Method + " " + r.Host
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(target.Close)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	spec := sqlSpec(t, "8123", nil, map[string]string{"protocol": "http"})
	spec.host, spec.hostPort = "clickhouse.example", "clickhouse.example:8123"
	spec.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == spec.hostPort {
			address = target.Listener.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := connectClickHouse(ctx, spec); err == nil {
		t.Error("connected to a stand-in that answers 500")
	}
	select {
	case got := <-received:
		if got != "POST clickhouse.example:8123" {
			t.Errorf("the target received %q", got)
		}
	default:
		t.Error("the target received nothing")
	}
	select {
	case got := <-proxied:
		t.Errorf("the proxy received %q", got)
	default:
	}
	options := clickHouseOptions(spec)
	if options.TransportFunc == nil {
		t.Fatal("the driver is given no transport function on the http protocol")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := options.TransportFunc(&http.Transport{Proxy: http.ProxyURL(proxyURL)})
	if transport, ok := rt.(*http.Transport); err != nil || !ok || transport.Proxy != nil {
		t.Errorf("the driver's transport comes back as %#v (%v), want it without a proxy", rt, err)
	}
	if native := clickHouseOptions(sqlSpec(t, "9000", nil, nil)); native.TransportFunc != nil {
		t.Error("the native protocol is given a transport function")
	}
}

// On the http protocol every request of the driver carries the setting
// cancel_http_readonly_queries_on_client_close=1 beside readonly=1, so that
// the server stops the statement when the driver ends its request at the
// deadline; the native protocol, whose driver cancels by a packet of its
// own, is not given it.
func TestClickHouseOverHTTPHasTheServerStopAStatementItsClientLeft(t *testing.T) {
	queries := make(chan url.Values, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries <- r.URL.Query()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(target.Close)
	_, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	spec := sqlSpec(t, port, nil, map[string]string{"protocol": "http"})
	spec.dial = sqlDial(nil, "127.0.0.1", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := connectClickHouse(ctx, spec); err == nil {
		t.Error("connected to a stand-in that answers 500")
	}
	select {
	case query := <-queries:
		if query.Get("cancel_http_readonly_queries_on_client_close") != "1" || query.Get("readonly") != "1" {
			t.Errorf("the request's settings are %v", query)
		}
	default:
		t.Error("the stand-in received nothing")
	}
	if native := clickHouseOptions(sqlSpec(t, "9000", nil, nil)); native.Settings["cancel_http_readonly_queries_on_client_close"] != nil || native.Settings["readonly"] != 1 {
		t.Errorf("the native protocol's settings are %v", native.Settings)
	}
}
