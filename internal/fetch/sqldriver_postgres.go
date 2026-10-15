//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// The postgres driver is pgx, through its database/sql adapter. A statement
// is always sent with the extended protocol, bound even when it takes no
// argument, so the server refuses two statements joined with ;. It runs in
// a transaction begun READ ONLY, with the statement timeout set to the
// scrape's time left by SET LOCAL statement_timeout, so the server stops a
// statement the scrape no longer waits for. The connection is configured
// from the collector alone: pgx's PG* environment variables and its
// .pgpass file, which it would otherwise read, change nothing, but for
// PGSERVICE, which no connection string unsets and which is refused.
func init() {
	registerSQLDriver(&sqlDriver{
		name:      SQLDriverPostgres,
		port:      func(map[string]string) string { return "5432" },
		options:   map[string][]string{"sslmode": {"disable", "require", "verify-full"}, "application_name": nil},
		tlsOption: "sslmode",
		tlsModes:  map[string]sqlTLSMode{"disable": sqlTLSOff, "require": sqlTLSInsecure, "verify-full": sqlTLSVerify},
		connect:   connectPostgres,
		run:       sqlReadOnlyTx(postgresStatementTimeout),
		readOnly:  "transaction",
	})
}

// postgresStatementTimeout bounds the statement at the server to the
// scrape's time left, in milliseconds, a millisecond at least.
func postgresStatementTimeout(timeout time.Duration) []string {
	if timeout <= 0 {
		return nil
	}
	return []string{"SET LOCAL statement_timeout = " + strconv.FormatInt(max(timeout.Milliseconds(), 1), 10)}
}

// postgresPinned is the connection string postgresConfig parses: every
// key of pgconn's parseEnvSettings, PGSERVICE's service aside, which a
// connection string cannot unset and postgresConfig refuses, named with the
// value pgx gives it when neither the connection string nor the environment
// does, but sslmode, which is disable, and user, which is a placeholder
// replaced as host, port and password are, so that pgx does not look up the
// system's account for it. A variable pgx comes to read later is caught by
// the test of the configuration's fields.
const postgresPinned = "host=localhost port=5432 dbname='' user=exporter password='' passfile='' application_name='' connect_timeout=0 " +
	"sslmode=disable sslkey='' sslcert='' sslsni=1 sslrootcert='' sslpassword='' sslnegotiation='' target_session_attrs=any " +
	"servicefile='' timezone='' options='' min_protocol_version=3.0 max_protocol_version=3.0 channel_binding=prefer require_auth=''"

// postgresConfig is the pgx configuration of a connection made with spec.
func postgresConfig(spec *sqlConnectSpec) (*pgx.ConnConfig, error) {
	if os.Getenv("PGSERVICE") != "" {
		return nil, errors.New("the exporter's environment sets PGSERVICE, which the postgres driver reads whatever the collector says, and would take settings from; unset it")
	}
	// Parsed from a connection string that names every setting pgx would
	// otherwise take from a PG* variable (pgconn parseEnvSettings), each as
	// pgx takes it when nothing names it, but for plaintext, so that no
	// fallback to another host or to TLS is made and no variable changes the
	// configuration, or fails the parse; everything else is set here.
	config, err := pgx.ParseConfig(postgresPinned)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(spec.port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("port %q: %w", spec.port, err)
	}
	config.Host, config.Port = spec.host, uint16(port)
	config.User, config.Password, config.Database = spec.username, spec.password, spec.database
	config.TLSConfig = spec.tls
	config.Fallbacks = nil
	config.ConnectTimeout = 0
	config.KerberosSrvName, config.KerberosSpn = "", ""
	config.SSLNegotiation = ""
	config.ValidateConnect, config.AfterConnect = nil, nil
	config.DialFunc = postgresDial(spec)
	// The dialer is given the host, which it resolves and holds to the
	// policy itself, rather than each address pgx would look up.
	config.LookupFunc = func(_ context.Context, host string) ([]string, error) { return []string{host}, nil }
	name := spec.appName
	if value, ok := spec.options["application_name"]; ok {
		name = value
	}
	config.RuntimeParams = map[string]string{"application_name": name}
	return config, nil
}

// postgresDial is the exporter's dialer as pgx is given it. pgx dials the
// target's host for a connection and, for the cancel request it sends when
// a statement's context ends (pgconn asyncClose, after the read the
// context's deadline broke off), the address its connection reached
// (PgConn.CancelRequest), which the dialer would refuse, under a policy, as
// a host the request did not check: for that one the target's host is
// dialed again, held to the policy as every connection is, and the
// connection kept only when it reached that very address, so the cancel
// request goes to the server running the statement or to none.
func postgresDial(spec *sqlConnectSpec) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == spec.host {
			return spec.dial(ctx, network, address)
		}
		conn, err := spec.dial(ctx, network, net.JoinHostPort(spec.host, port))
		if err != nil {
			return nil, err
		}
		if conn.RemoteAddr().String() != address {
			_ = conn.Close()
			return nil, fmt.Errorf("the cancel request's connection reached %s, not the statement's server %s", conn.RemoteAddr(), address)
		}
		return conn, nil
	}
}

func connectPostgres(ctx context.Context, spec *sqlConnectSpec) (driver.Conn, error) {
	config, err := postgresConfig(spec)
	if err != nil {
		return nil, err
	}
	return stdlib.GetConnector(*config).Connect(ctx)
}
