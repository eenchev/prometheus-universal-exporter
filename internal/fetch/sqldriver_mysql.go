//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"net"

	"github.com/go-sql-driver/mysql"
)

// The mysql driver is go-sql-driver/mysql, for MySQL and MariaDB. A
// statement runs in a transaction started READ ONLY. multiStatements stays
// off, so the server refuses two statements joined with ;, and the
// arguments are sent bound, never written into the statement. The driver
// closes the connection when the scrape's deadline comes; the server ends
// the statement when it notices, so an account with a statement limit of its
// own (MAX_EXECUTION_TIME, max_statement_time) stops it sooner.
func init() {
	registerSQLDriver(&sqlDriver{
		name:      SQLDriverMySQL,
		port:      func(map[string]string) string { return "3306" },
		options:   map[string][]string{"tls": {"false", "true", "skip-verify"}},
		tlsOption: "tls",
		tlsModes:  map[string]sqlTLSMode{"false": sqlTLSOff, "true": sqlTLSVerify, "skip-verify": sqlTLSInsecure},
		connect:   connectMySQL,
		run:       sqlReadOnlyTx(nil),
		readOnly:  "transaction",
	})
}

// mysqlConfig is the configuration of a connection made with spec.
func mysqlConfig(spec *sqlConnectSpec) *mysql.Config {
	config := mysql.NewConfig()
	config.Net, config.Addr = "tcp", spec.hostPort
	config.User, config.Passwd, config.DBName = spec.username, spec.password, spec.database
	config.TLS = spec.tls
	config.DialFunc = mysqlDial(spec.dial)
	// DATETIME and TIMESTAMP are read as times, in UTC, as the other
	// drivers read them, rather than as text.
	config.ParseTime = true
	config.MultiStatements = false
	config.InterpolateParams = false
	config.AllowAllFiles = false
	config.AllowCleartextPasswords = false
	config.ConnectionAttributes = "program_name:" + spec.appName
	// The driver's own log of a broken connection would reach stderr
	// unformatted; the scrape's error says what failed.
	config.Logger = &mysql.NopLogger{}
	return config
}

// mysqlDial is the exporter's dialer, its TCP connections handed to the
// driver as another type: the driver switches TCP keepalives on, at the
// operating system's period, on a connection that is a *net.TCPConn
// (go-sql-driver/mysql connector.go, Connect), which would undo the
// keepalives the dialer set, or switched off. The connection still offers
// its file descriptor (syscall.Conn), which the driver's check of an idle
// connection reads.
func mysqlDial(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if tcp, ok := conn.(*net.TCPConn); ok {
			return mysqlTCPConn{tcp}, err
		}
		return conn, err
	}
}

// mysqlTCPConn is a TCP connection that is not a *net.TCPConn to the
// driver, so that it leaves its keepalives as they are.
type mysqlTCPConn struct{ *net.TCPConn }

func connectMySQL(ctx context.Context, spec *sqlConnectSpec) (driver.Conn, error) {
	connector, err := mysql.NewConnector(mysqlConfig(spec))
	if err != nil {
		return nil, err
	}
	return connector.Connect(ctx)
}
