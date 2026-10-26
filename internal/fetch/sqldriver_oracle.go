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

// The oracle driver is sijms/go-ora, pure Go: built in only with the
// sql_driver_oracle tag (SQL_DRIVERS=oracle). The database is named by
// request.service_name. go-ora refuses database/sql's read-only
// transaction, so a statement runs after SET TRANSACTION READ ONLY, the
// first statement of the transaction it opens, which is rolled back. The
// driver breaks off the statement when the scrape's deadline comes. ssl
// switches TLS on, for every connection go-ora makes (oracleTLSOnly); the
// certificate is checked against the target's host, which go-ora sets
// itself, so tls.server_name is refused.
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
// here rather than by goora.BuildUrl, which escapes the account with
// url.PathEscape (connection_string.go BuildUrl), leaving a ':' in it as it
// is, so that go-ora, which reads the URL with url.Parse and takes the
// account and password from its user information
// (configurations/connect_config.go ParseConfig), would end the account at
// that ':' and take the rest for the password: url.UserPassword escapes
// what would end either of them.
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

// oracleTLSOnly is dial for a connection with ssl true: each connection it
// makes must begin with a TLS handshake, and is closed, nothing written,
// when the driver begins it otherwise. go-ora follows a listener's REDIRECT
// to the address it names (network/session.go Connect,
// UpdateDatabaseInfoForRedirect) and takes TLS on or off from that
// address's PROTOCOL (configurations/session_info.go UpdateSSL), so a
// redirect naming PROTOCOL=TCP for the target's own host, which the dialer
// lets through, would have the next connection, and the login on it, go
// unencrypted. go-ora's own TLS stays on, rather than the handshake being
// made here, since go-ora makes it again on that connection when the
// server asks it to (session.go readPacket, negotiate on RESEND and DATA).
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
// record: content type 22, then the major version 3. A TNS packet begins
// with its length, which would have to be 5,635 bytes or so to look the
// same, and the CONNECT packet go-ora writes first is a few hundred bytes
// at most.
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
