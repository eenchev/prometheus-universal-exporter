//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// The mysql driver is go-sql-driver/mysql, for MySQL and MariaDB. A
// statement runs in a transaction started READ ONLY. multiStatements stays
// off, so the server refuses two statements joined with ;, and the
// arguments are sent bound, never written into the statement. The driver
// closes the connection when the scrape's deadline comes, and the server
// ends the statement only when it notices; so the server is also told to
// end it at the scrape's time left, in the transaction before it, as
// postgres's statement_timeout is: MySQL by max_execution_time, MariaDB by
// max_statement_time, a server of neither kind, or one older than the
// variable, by nothing (mysqlStatementTimeout). Which the server is, it
// said when asked once on the connection (mysqlRun).
func init() {
	registerSQLDriver(&sqlDriver{
		name:      SQLDriverMySQL,
		port:      func(map[string]string) string { return "3306" },
		options:   map[string][]string{"tls": {"false", "true", "skip-verify"}},
		tlsOption: "tls",
		tlsModes:  map[string]sqlTLSMode{"false": sqlTLSOff, "true": sqlTLSVerify, "skip-verify": sqlTLSInsecure},
		connect:   connectMySQL,
		run:       mysqlRun,
		readOnly:  "transaction",
		timedOut:  mysqlTimedOut,

		serverError: mysqlServerError,
	})
}

// mysqlServerError recognises the server's ERR packet, which the driver
// gives as a MySQLError, by its error number and, when the server sent
// one, its SQLSTATE.
func mysqlServerError(err error) (string, bool) {
	var myErr *mysql.MySQLError
	if !errors.As(err, &myErr) {
		return "", false
	}
	code := "error " + strconv.Itoa(int(myErr.Number))
	if myErr.SQLState != [5]byte{} {
		code += " (SQLSTATE " + string(myErr.SQLState[:]) + ")"
	}
	return code, true
}

// mysqlRun runs a statement in a read-only transaction, the server's own
// time limit set first (mysqlStatementTimeout). The server's version is asked
// once on a connection, the first time a statement runs on it, and kept
// with it: SELECT VERSION(), outside the transaction, within the scrape's
// deadline.
func mysqlRun(ctx context.Context, conn *sql.Conn, statement string, args []any, timeout time.Duration) (*sql.Rows, func(), error) {
	var guarded *sqlGuardedConn
	_ = conn.Raw(func(dc any) error {
		guarded = sqlGuardedOf(dc)
		return nil
	})
	version := ""
	if guarded != nil {
		version = guarded.serverVersion
	}
	if version == "" {
		if err := conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
			return nil, nil, &sqlStepError{step: "asking the server's version", err: err}
		}
		if guarded != nil {
			guarded.serverVersion = version
		}
	}
	return sqlReadOnlyTx(func(left time.Duration) []string { return mysqlStatementTimeout(version, left) })(ctx, conn, statement, args, timeout)
}

// mysqlStatementTimeout is what sets the server's own time limit on the
// statements of the session to the scrape's time left, a millisecond at
// least, by the version the server gave: MariaDB's max_statement_time, in
// seconds, from 10.1.1, and MySQL's max_execution_time, in milliseconds,
// which bounds a SELECT, from 5.7.8. A server older than its variable, or
// whose version reads as neither's, is set nothing, rather than refuse
// every scrape over a variable it does not know.
func mysqlStatementTimeout(version string, timeout time.Duration) []string {
	if timeout <= 0 {
		return nil
	}
	ms := max(timeout.Milliseconds(), 1)
	major, minor, patch, ok := mysqlVersionNumbers(version)
	switch {
	case !ok:
		return nil
	case strings.Contains(version, "MariaDB"):
		if mysqlAtLeast(major, minor, patch, 10, 1, 1) {
			return []string{fmt.Sprintf("SET SESSION max_statement_time = %d.%03d", ms/1000, ms%1000)}
		}
	case mysqlAtLeast(major, minor, patch, 5, 7, 8):
		return []string{"SET SESSION max_execution_time = " + strconv.FormatInt(ms, 10)}
	}
	return nil
}

// mysqlVersionNumbers reads the major, minor and patch numbers of a
// server's version: 8.0.36, 8.4.0-log, 10.11.6-MariaDB-1:10.11.6+maria~ubu2204.
// MariaDB's replication prefix, 5.5.5-, which a MariaDB server puts before
// its own version, is skipped.
func mysqlVersionNumbers(version string) (major, minor, patch int, ok bool) {
	version = strings.TrimPrefix(version, "5.5.5-")
	numbers := mysqlVersion.FindStringSubmatch(version)
	if numbers == nil {
		return 0, 0, 0, false
	}
	major, _ = strconv.Atoi(numbers[1])
	minor, _ = strconv.Atoi(numbers[2])
	patch, _ = strconv.Atoi(numbers[3])
	return major, minor, patch, true
}

// mysqlVersion is a version's major, minor and patch numbers at its start.
var mysqlVersion = regexp.MustCompile(`^(\d{1,4})\.(\d{1,4})\.(\d{1,4})`)

// mysqlAtLeast says major.minor.patch is the version a.b.c or a later one.
func mysqlAtLeast(major, minor, patch, a, b, c int) bool {
	if major != a {
		return major > a
	}
	if minor != b {
		return minor > b
	}
	return patch >= c
}

// mysqlTimedOut recognises the server ending a statement at the time limit
// mysqlStatementTimeout set: MySQL's error 3024 (ER_QUERY_TIMEOUT) and
// MariaDB's 1969 (ER_STATEMENT_TIMEOUT).
func mysqlTimedOut(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && (myErr.Number == 3024 || myErr.Number == 1969)
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
