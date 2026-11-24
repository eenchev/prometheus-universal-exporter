//go:build (!select_request_types || request_type_sql) && sql_driver_oracle

package fetch

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	goora "github.com/sijms/go-ora/v2"
	oranetwork "github.com/sijms/go-ora/v2/network"
)

// The oracle driver is sijms/go-ora, in pure Go. It is built in only with
// the sql_driver_oracle tag (SQL_DRIVERS=oracle). request.service_name
// names the database. go-ora refuses database/sql's read-only transaction,
// so the statement runs after SET TRANSACTION READ ONLY, as the first
// statement of the transaction it opens, which is then rolled back. The
// driver aborts the statement at the scrape's deadline. ssl turns on TLS
// for every connection go-ora makes (oracleTLSOnly). go-ora sets the host
// the certificate is checked against to the target's host itself, so
// tls.server_name is refused.
func init() {
	registerSQLDriver(&sqlDriver{
		name:        SQLDriverOracle,
		port:        func(map[string]string) string { return "1521" },
		options:     map[string][]string{"ssl": {"false", "true"}},
		tlsOption:   "ssl",
		tlsModes:    map[string]sqlTLSMode{"false": sqlTLSOff, "true": sqlTLSVerify},
		refusedTLS:  []string{"server_name"},
		serviceName: true,
		connect:     connectOracle,
		run:         sqlTx(sql.TxOptions{}, func(time.Duration) []string { return []string{"SET TRANSACTION READ ONLY"} }),
		readOnly:    "transaction",
		serverError: oracleServerError,
	})
}

// oracleServerError recognises the server's error, which go-ora gives as a
// network.OracleError, by its ORA- number.
func oracleServerError(err error) (string, bool) {
	var oraErr *oranetwork.OracleError
	if !errors.As(err, &oraErr) {
		return "", false
	}
	return fmt.Sprintf("ORA-%05d", oraErr.ErrCode), true
}

// oracleURL is the go-ora URL of a connection made with spec. It is built
// here rather than by goora.BuildUrl. BuildUrl escapes the account with
// url.PathEscape (connection_string.go BuildUrl), which leaves a ':'
// unescaped. go-ora reads the URL with url.Parse and takes the account and
// password from its user information (configurations/connect_config.go
// ParseConfig). It would end the account at that ':' and take the rest as
// the password. url.UserPassword escapes anything that would end either.
func oracleURL(spec *sqlConnectSpec) string {
	port, _ := strconv.Atoi(spec.port)
	query := url.Values{"PROGRAM": {spec.appName}}
	if spec.tls != nil {
		query.Set("SSL", "TRUE")
		if spec.tls.InsecureSkipVerify {
			query.Set("SSL VERIFY", "FALSE")
		}
	}
	u := url.URL{
		Scheme: "oracle", User: url.UserPassword(spec.username, spec.password),
		Host: net.JoinHostPort(spec.host, strconv.Itoa(port)), Path: "/" + spec.serviceName, RawQuery: query.Encode(),
	}
	return u.String()
}

func connectOracle(ctx context.Context, spec *sqlConnectSpec) (driver.Conn, error) {
	connector, ok := goora.NewConnector(oracleURL(spec)).(*goora.OracleConnector)
	if !ok {
		return nil, errSQLNoDriver
	}
	dial := spec.dial
	if spec.tls != nil {
		dial = oracleTLSOnly(spec.dial)
	}
	connector.Dialer(oracleDialer(dial))
	if spec.tls != nil {
		// go-ora writes the host into the configuration's ServerName, so
		// each connection is given a copy of its own.
		connector.WithTLSConfig(spec.tls.Clone())
	}
	return connector.Connect(ctx)
}

// oracleDialer hands go-ora's connections to the exporter's dialer.
type oracleDialer func(ctx context.Context, network, address string) (net.Conn, error)

func (d oracleDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d(ctx, network, address)
}

// oracleTLSOnly is the dial for a connection with ssl true. Each connection
// it makes must begin with a TLS handshake. If the driver begins it any
// other way, it is closed with nothing written. The reason: go-ora follows
// a listener's REDIRECT to the address it names (network/session.go
// Connect, UpdateDatabaseInfoForRedirect) and turns TLS on or off from
// that address's PROTOCOL (configurations/session_info.go UpdateSSL). A
// redirect naming PROTOCOL=TCP for the target's own host, which the dialer
// allows, would make the next connection, and the login on it,
// unencrypted. go-ora's own TLS stays on, rather than doing the handshake
// here, because go-ora repeats it on that connection when the server asks
// (session.go readPacket, negotiate on RESEND and DATA).
func oracleTLSOnly(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &oracleTLSConn{Conn: conn}, nil
	}
}

// errOracleNoTLS is the error of a connection go-ora began without TLS
// though ssl is true.
var errOracleNoTLS = errors.New("request.options.ssl is true, and the oracle driver began a connection without TLS, as a listener's redirect to an address " +
	"with PROTOCOL=TCP has it do, and nothing was sent on it; have the listener redirect to a TCPS address")

// oracleTLSConn is a connection whose first write must be a TLS handshake
// record: content type 22, then major version 3. A TNS packet begins with
// its length, which would need to be about 5,635 bytes to look the same.
// The CONNECT packet go-ora writes first is at most a few hundred bytes.
type oracleTLSConn struct {
	net.Conn
	once sync.Once
	err  error
}

func (c *oracleTLSConn) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return c.Conn.Write(b)
	}
	c.once.Do(func() {
		if len(b) < 2 || b[0] != 22 || b[1] != 3 {
			c.err = errOracleNoTLS
			_ = c.Close()
		}
	})
	if c.err != nil {
		return 0, c.err
	}
	return c.Conn.Write(b)
}
