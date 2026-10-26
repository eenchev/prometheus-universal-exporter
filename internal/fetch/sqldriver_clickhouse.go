//go:build (!select_request_types || request_type_sql) && sql_driver_clickhouse

package fetch

import (
	"context"
	"crypto/tls"
	"database/sql/driver"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	chproto "github.com/ClickHouse/clickhouse-go/v2/lib/proto"
)

// The clickhouse driver is ClickHouse/clickhouse-go: built in only with the
// sql_driver_clickhouse tag (SQL_DRIVERS=clickhouse). ClickHouse has no
// transactions; every statement is sent with the setting readonly=1, which
// lets it only read, and stopped by the server at the scrape's deadline,
// which the driver cancels it at: on the native protocol by a cancel
// packet, on http by ending the request, with the setting
// cancel_http_readonly_queries_on_client_close=1 that has the server stop
// the statement then. protocol is native (port 9000, or 9440
// with TLS) or http (8123, or 8443 with TLS); compress is none, lz4 or
// zstd; secure switches TLS on. On the http protocol, the driver's requests
// go to the target, never through a proxy (clickHouseNoProxy).
func init() {
	registerSQLDriver(&sqlDriver{
		name:      SQLDriverClickHouse,
		port:      clickHousePort,
		options:   map[string][]string{"protocol": {"native", "http"}, "compress": {"none", "lz4", "zstd"}, "secure": {"false", "true"}},
		tlsOption: "secure",
		tlsModes:  map[string]sqlTLSMode{"false": sqlTLSOff, "true": sqlTLSVerify},
		connect:   connectClickHouse,
		run:       sqlPlain,
		readOnly:  "setting",

		serverError: clickHouseServerError,
	})
}

// clickHouseServerError recognises the server's exception, which the driver
// gives as a proto.Exception on either protocol, by its code. Its message
// quotes the statement it failed on.
func clickHouseServerError(err error) (string, bool) {
	var exception *chproto.Exception
	if !errors.As(err, &exception) {
		return "", false
	}
	return "code " + strconv.Itoa(int(exception.Code)), true
}

// clickHousePort is the port of a target that names none: the protocol's,
// plain or with TLS.
func clickHousePort(options map[string]string) string {
	secure := options["secure"] == "true"
	if options["protocol"] == "http" {
		if secure {
			return "8443"
		}
		return "8123"
	}
	if secure {
		return "9440"
	}
	return "9000"
}

// clickHouseOptions is the configuration of a connection made with spec.
func clickHouseOptions(spec *sqlConnectSpec) *clickhouse.Options {
	options := &clickhouse.Options{
		Addr:        []string{spec.hostPort},
		Auth:        clickhouse.Auth{Database: spec.database, Username: spec.username, Password: spec.password},
		TLS:         spec.tls,
		Settings:    clickhouse.Settings{"readonly": 1},
		DialContext: clickHouseDial(spec),
		ClientInfo:  clickhouse.ClientInfo{Products: []struct{ Name, Version string }{{Name: spec.appName}}},
		DialTimeout: 30 * time.Second,
	}
	if spec.options["protocol"] == "http" {
		options.Protocol = clickhouse.HTTP
		options.TransportFunc = clickHouseNoProxy
		// Over http the driver cancels a statement at the deadline by
		// ending its request, and the server stops a read-only statement
		// whose client went away only with this setting, off by default;
		// the driver sends its settings with every request (conn_http.go).
		options.Settings["cancel_http_readonly_queries_on_client_close"] = 1
	}
	switch spec.options["compress"] {
	case "lz4":
		options.Compression = &clickhouse.Compression{Method: clickhouse.CompressionLZ4}
	case "zstd":
		options.Compression = &clickhouse.Compression{Method: clickhouse.CompressionZSTD}
	}
	return options
}

// clickHouseNoProxy is the driver's http transport without a proxy. The
// driver's transport takes the proxy from HTTP_PROXY, HTTPS_PROXY and
// NO_PROXY (clickhouse-go conn_http.go, createHTTPRoundTripper), which would
// send the request to the proxy, a host the exporter's dialer refuses, while
// a statement, like every other driver's, goes to the target itself.
func clickHouseNoProxy(transport *http.Transport) (http.RoundTripper, error) {
	transport.Proxy = nil
	return transport, nil
}

// clickHouseDial is the driver's dial function: the exporter's dialer, and,
// on the native protocol with TLS, the TLS handshake over what it dialed.
// The driver wraps its connection in TLS only when it dials by itself, never
// when given a dial function (clickhouse-go conn.go, dial), so without the
// handshake here the login would go in plaintext; on the http protocol
// net/http makes the handshake itself over the connection dialed.
func clickHouseDial(spec *sqlConnectSpec) func(ctx context.Context, address string) (net.Conn, error) {
	if spec.tls == nil || spec.options["protocol"] == "http" {
		return func(ctx context.Context, address string) (net.Conn, error) {
			return spec.dial(ctx, "tcp", address)
		}
	}
	return func(ctx context.Context, address string) (net.Conn, error) {
		conn, err := spec.dial(ctx, "tcp", address)
		if err != nil {
			return nil, err
		}
		secured := tls.Client(conn, spec.tls)
		if err := secured.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return secured, nil
	}
}

func connectClickHouse(ctx context.Context, spec *sqlConnectSpec) (driver.Conn, error) {
	return clickhouse.Connector(clickHouseOptions(spec)).Connect(ctx)
}
