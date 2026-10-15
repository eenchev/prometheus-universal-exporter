//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// The database drivers of the sql request type. Three are built in with the
// type: mysql, postgres and sqlserver. The other four bring large
// dependencies of their own (the Snowflake driver alone brings the AWS and
// Azure SDKs and Apache Arrow), so each is built in only when asked for, by
// its build tag, sql_driver_<name> (make build SQL_DRIVERS=<name>,...).
// Each lives in its own sqldriver_<name>.go, the one file that imports its
// module, and registers itself in sqlDriverTable from there.

// knownSQLDrivers is every driver in the source tree, built in or not, so
// that a collector naming one the build left out is told how to build it
// in, rather than that it does not exist. A test keeps it in step with the
// sqldriver_<name>.go files.
var knownSQLDrivers = []string{SQLDriverClickHouse, SQLDriverMySQL, SQLDriverOracle, SQLDriverPostgres, SQLDriverSnowflake, SQLDriverSQLServer, SQLDriverVertica}

// sqlDefaultDrivers are the drivers every build with the sql type carries.
var sqlDefaultDrivers = []string{SQLDriverMySQL, SQLDriverPostgres, SQLDriverSQLServer}

// sqlTLSMode is what a driver's TLS option asks for: no TLS, TLS with the
// server's certificate verified, TLS without the verification, or TLS with
// the certificate verified as the tls keys say and not verified when the
// collector sets none (SQL Server's encryption of the login alone, which
// SQL Server's own tools do not verify either).
type sqlTLSMode int

const (
	sqlTLSOff sqlTLSMode = iota
	sqlTLSVerify
	sqlTLSInsecure
	sqlTLSVerifyByKeys
)

// sqlDriver is one database driver as the sql request type uses it.
type sqlDriver struct {
	name string
	// port is the port of a target that names none, by the options.
	port func(options map[string]string) string
	// options are the request.options the driver takes, each with the
	// values it takes, or nil for any text.
	options map[string][]string
	// tlsOption is the option that says whether, and how, TLS is used, and
	// tlsModes what each of its values asks for. A collector that leaves it
	// out has TLS, verified, when it sets a tls key, and none otherwise.
	// Empty, the driver always uses TLS, verified (Snowflake's HTTPS).
	tlsOption string
	tlsModes  map[string]sqlTLSMode
	// refusedTLS are the tls keys the driver cannot honour.
	refusedTLS []string
	// serviceName says the driver names its database by request.service_name,
	// which it requires, and takes no request.database (Oracle).
	serviceName bool
	// keyPair says the driver may authenticate with request.private_key_file
	// instead of a password (Snowflake's key-pair authentication).
	keyPair bool
	// account says a target is an account identifier, not host:port, and
	// host makes the host its connections reach of it (Snowflake).
	account bool
	host    func(account string) string
	// ownDial says the driver dials by itself, not through the exporter's
	// dialer: its connections' addresses are checked against the policy
	// before the connection, not on it, and TCP keepalives are the
	// operating system's (Vertica).
	ownDial bool
	// connect makes one connection with spec.
	connect func(ctx context.Context, spec *sqlConnectSpec) (driver.Conn, error)
	// run runs a statement on conn as the driver can make it read-only and
	// bounded in time (sqlReadOnlyTx and the others); done ends it.
	run func(ctx context.Context, conn *sql.Conn, statement string, args []any, timeout time.Duration) (rows *sql.Rows, done func(), err error)
	// rowsError, when set, puts a failure reading the rows that only the
	// driver can recognise in the operator's words (Snowflake).
	rowsError func(err error) error
	// readOnly says how run makes a statement read-only, for the docs and
	// the debug report: "transaction", "setting", or "" for not at all.
	readOnly string
	// statement, when set, refuses a statement the driver would not run as
	// the one statement it makes read-only, when the configuration is
	// loaded.
	statement func(statement string) error
}

// sqlDriverTable is the drivers built in, by name.
var sqlDriverTable = map[string]*sqlDriver{}

// registerSQLDriver adds a driver, from the init of its file.
func registerSQLDriver(d *sqlDriver) {
	if _, exists := sqlDriverTable[d.name]; exists {
		panic(fmt.Sprintf("sql driver %q registered twice", d.name))
	}
	sqlDriverTable[d.name] = d
	sqlDriverNames = append(sqlDriverNames, d.name)
}

// sqlConnectSpec is what one connection is made with: the target, the
// account with the credentials read from their files for this connection,
// the database, the options, the TLS configuration (nil for none) and the
// dialer that holds every connection to the collector's policy and keeps
// it alive.
type sqlConnectSpec struct {
	host, port string
	// hostPort is host:port, what is dialed.
	hostPort string
	// account is a Snowflake target's account identifier.
	account     string
	username    string
	password    string
	privateKey  *rsa.PrivateKey
	database    string
	serviceName string
	options     map[string]string
	tls         *tls.Config
	// tlsDigest is a digest of the TLS settings and the files read for
	// them, which tells one configuration from another.
	tlsDigest string
	dial      func(ctx context.Context, network, address string) (net.Conn, error)
	// appName is the name the connection gives itself where the driver
	// sends one (sqlAppName).
	appName string
}

// sqlAppName is the name the exporter's connections give themselves where a
// driver sends one, so that a database administrator can tell them apart.
const sqlAppName = "prometheus-universal-exporter"

// sqlDial is the dialer of a pool's connections: it connects to address,
// checks the address it connected to against the collector's policy, as an
// http request's connections are (checkConnection), and sets TCP
// keepalives at interval, none at 0. It dials TCP alone: a driver asking
// for anything else (go-mssqldb asks the SQL Server Browser over UDP for an
// instance a routing answer names without a port, tds.go getInstances)
// would reach an address checkConnAddr does not see as TCP, so it is
// refused.
func sqlDial(policy *targetPolicy, host string, interval time.Duration) func(ctx context.Context, network, address string) (net.Conn, error) {
	keepalive := interval
	if keepalive <= 0 {
		keepalive = -1
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		switch network {
		case "tcp", "tcp4", "tcp6":
		default:
			return nil, fmt.Errorf("the driver asked for a %s connection to %s, and the exporter makes TCP connections alone, each held to the target lists; "+
				"make the server reachable over TCP at the target's host and port", network, address)
		}
		conn, err := (&net.Dialer{KeepAlive: keepalive}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if policy == nil {
			return conn, nil
		}
		dialed, _, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			dialed = address
		}
		// A driver that speaks HTTP dials the host it was given, and a
		// driver dials no host but the target's: anything else is refused,
		// as an http request's connection to a host it did not check is.
		if canonical, err := canonicalHost(dialed); err != nil || !sameHost(canonical, host) {
			_ = conn.Close()
			return nil, &TargetRefusedError{Host: dialed, Reason: "the connection is to a host the request did not check"}
		}
		if err := checkConnAddr(policy, host, conn); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

// sqlDriverPanic is the error of a driver that panicked, with the value
// recover gave: a driver's fault, which an answer the driver did not expect
// can set off, fails the one connection or statement, never the process,
// and the connection it was on is given up.
func sqlDriverPanic(driverName string, recovered any) error {
	return fmt.Errorf("the %s driver failed on what the server sent (it panicked: %v); the connection is given up, "+
		"and if this repeats the server or something between it and the exporter is answering in a way the driver cannot read", driverName, recovered)
}

// sqlConnectRecovering runs connect, turning a panic of the driver into
// sqlDriverPanic. database/sql calls a connector both in the goroutine of
// the scrape that asks for a connection, where a panic would leave the
// connection counted as open, holding a place at max_per_target for good,
// and in its own goroutine that opens connections for scrapes waiting for
// one, where a panic would end the process.
func sqlConnectRecovering(ctx context.Context, driverName string, connect func(ctx context.Context, spec *sqlConnectSpec) (driver.Conn, error), spec *sqlConnectSpec) (conn driver.Conn, err error) {
	defer func() {
		if r := recover(); r != nil {
			conn, err = nil, sqlDriverPanic(driverName, r)
		}
	}()
	return connect(ctx, spec)
}

// checkOwnDial applies the policy to host for a driver that dials by itself
// (sqlDriver.ownDial): its name, then every address it resolves to, before
// the driver connects, since nothing checks the connection. A host whose
// addresses could not be looked up fails, unlike one behind a proxy
// (unresolved): the driver looks the name up itself and connects to
// whatever that gives, which nothing here has held to the policy.
func (p *targetPolicy) checkOwnDial(ctx context.Context, host string) error {
	_, err := p.checkHost(ctx, host, true, true)
	return err
}

// sameHost says two canonical hosts are the same: the same name, or the
// same address however each is written.
func sameHost(a, b string) bool {
	if a == b {
		return true
	}
	x, errA := netip.ParseAddr(a)
	y, errB := netip.ParseAddr(b)
	return errA == nil && errB == nil && x.Unmap() == y.Unmap()
}

// checkConnAddr holds the address conn is connected to to policy, for host.
func checkConnAddr(policy *targetPolicy, host string, conn net.Conn) error {
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	addr, ok := netip.AddrFromSlice(remote.IP)
	if !ok {
		return nil
	}
	nameAllowed, err := policy.checkName(host)
	if err != nil {
		return err
	}
	return policy.checkAddr(host, addr.Unmap(), nameAllowed)
}

// sqlReadOnlyTx runs a statement in a transaction begun read-only through
// database/sql, which the driver turns into its own words (BEGIN READ ONLY,
// START TRANSACTION READ ONLY), after the statements setup gives for the
// timeout, run in it first. The transaction is rolled back at the end: it
// has done nothing to keep.
func sqlReadOnlyTx(setup func(timeout time.Duration) []string) func(ctx context.Context, conn *sql.Conn, statement string, args []any, timeout time.Duration) (*sql.Rows, func(), error) {
	return sqlTx(sql.TxOptions{ReadOnly: true}, setup)
}

// sqlTx runs a statement in a transaction begun with opts, after the
// statements setup gives.
func sqlTx(opts sql.TxOptions, setup func(timeout time.Duration) []string) func(ctx context.Context, conn *sql.Conn, statement string, args []any, timeout time.Duration) (*sql.Rows, func(), error) {
	return func(ctx context.Context, conn *sql.Conn, statement string, args []any, timeout time.Duration) (*sql.Rows, func(), error) {
		tx, err := conn.BeginTx(ctx, &opts)
		if err != nil {
			return nil, nil, fmt.Errorf("beginning the read-only transaction: %w", err)
		}
		if setup != nil {
			for _, s := range setup(timeout) {
				if _, err := tx.ExecContext(ctx, s); err != nil {
					_ = tx.Rollback()
					return nil, nil, fmt.Errorf("running %s: %w", s, err)
				}
			}
		}
		rows, err := tx.QueryContext(ctx, statement, args...)
		if err != nil {
			_ = tx.Rollback()
			return nil, nil, err
		}
		return rows, func() { _ = rows.Close(); _ = tx.Rollback() }, nil
	}
}

// sqlPlain runs a statement as it is, for a driver that has no read-only
// transaction: the account must be one that can only read.
func sqlPlain(ctx context.Context, conn *sql.Conn, statement string, args []any, _ time.Duration) (*sql.Rows, func(), error) {
	rows, err := conn.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, nil, err
	}
	return rows, func() { _ = rows.Close() }, nil
}

// sqlTLSModeOf is the TLS a collector's request asks of d: by its TLS
// option, or, left out, verified TLS when a tls key is set and none
// otherwise.
func sqlTLSModeOf(d *sqlDriver, options map[string]string, tlsSet bool) sqlTLSMode {
	if d.tlsOption == "" {
		return sqlTLSVerify
	}
	if value, ok := options[d.tlsOption]; ok {
		return d.tlsModes[value]
	}
	if tlsSet {
		return sqlTLSVerify
	}
	return sqlTLSOff
}

// sqlOptionNames lists the options of d, sorted, for messages.
func sqlOptionNames(d *sqlDriver) string {
	names := make([]string, 0, len(d.options))
	for name := range d.options {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// errSQLNoDriver is the error of a driver's Open, which the exporter never
// calls: every connection is made through a connector (sqlConnector).
var errSQLNoDriver = errors.New("connections of the sql request type are made through its connector")

// sqlDriverStub is the driver.Driver a connector names, as database/sql
// asks of one.
type sqlDriverStub struct{}

func (sqlDriverStub) Open(string) (driver.Conn, error) { return nil, errSQLNoDriver }
