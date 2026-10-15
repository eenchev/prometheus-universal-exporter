//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/microsoft/go-mssqldb/msdsn"
)

// sqlSpec is a connection's settings as a pool makes them for a driver,
// with its dialer refusing every address, so a driver that connects goes
// through it and is refused.
func sqlSpec(t *testing.T, port string, tlsConfig *tls.Config, options map[string]string) *sqlConnectSpec {
	t.Helper()
	deny, err := compileTargetPolicy(nil, []string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	return &sqlConnectSpec{
		host: "127.0.0.1", port: port, hostPort: net.JoinHostPort("127.0.0.1", port),
		username: "metrics", password: "p@ss:w/rd?", database: "app", options: options, tls: tlsConfig,
		dial: sqlDial(deny, "127.0.0.1", 0), appName: sqlAppName,
	}
}

// listen is a TCP listener that accepts connections and closes them, for a
// driver to dial.
func listen(t *testing.T) string {
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
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}

// The postgres configuration is the collector's alone: the PG* variables
// and the .pgpass file, which pgx reads, change nothing; no fallback to
// another host or to TLS is made; the dialer is given the host, not an
// address pgx looked up; the application name is the exporter's, or the
// option's; and TLS is the spec's.
func TestThePostgresConfigurationIsTheCollectorsAlone(t *testing.T) {
	for name, value := range map[string]string{"PGHOST": "elsewhere", "PGPASSWORD": "env", "PGSSLMODE": "prefer", "PGTARGETSESSIONATTRS": "read-write", "PGAPPNAME": "env", "PGCONNECT_TIMEOUT": "1"} {
		t.Setenv(name, value)
	}
	spec := sqlSpec(t, "5432", nil, nil)
	config, err := postgresConfig(spec)
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != "127.0.0.1" || config.Port != 5432 || config.User != "metrics" || config.Password != "p@ss:w/rd?" || config.Database != "app" ||
		config.TLSConfig != nil || len(config.Fallbacks) != 0 || config.ValidateConnect != nil || config.ConnectTimeout != 0 ||
		config.RuntimeParams["application_name"] != sqlAppName || len(config.RuntimeParams) != 1 {
		t.Errorf("%+v", config.Config)
	}
	if hosts, err := config.LookupFunc(context.Background(), "db.example"); err != nil || len(hosts) != 1 || hosts[0] != "db.example" {
		t.Errorf("lookup gives %v %v", hosts, err)
	}
	withTLS := &tls.Config{ServerName: "db.example", MinVersion: tls.VersionTLS12}
	config, err = postgresConfig(sqlSpec(t, "5433", withTLS, map[string]string{"application_name": "billing"}))
	if err != nil {
		t.Fatal(err)
	}
	if config.TLSConfig != withTLS || config.RuntimeParams["application_name"] != "billing" || config.Port != 5433 {
		t.Errorf("%+v", config.Config)
	}
	if got := postgresStatementTimeout(1500 * time.Microsecond); len(got) != 1 || got[0] != "SET LOCAL statement_timeout = 1" {
		t.Errorf("%q", got)
	}
	if got := postgresStatementTimeout(0); got != nil {
		t.Errorf("%q", got)
	}
}

// The mysql configuration reads times as times, in UTC, never allows two
// statements in one or writes the arguments into the statement, reads no
// local file, sends no cleartext password, and dials through the exporter.
func TestTheMySQLConfiguration(t *testing.T) {
	withTLS := &tls.Config{ServerName: "db.example", MinVersion: tls.VersionTLS12}
	config := mysqlConfig(sqlSpec(t, "3306", withTLS, nil))
	if config.Addr != "127.0.0.1:3306" || config.Net != "tcp" || config.User != "metrics" || config.Passwd != "p@ss:w/rd?" || config.DBName != "app" ||
		!config.ParseTime || config.Loc != time.UTC || config.MultiStatements || config.InterpolateParams || config.AllowAllFiles || config.AllowCleartextPasswords ||
		config.TLS != withTLS || config.DialFunc == nil {
		t.Errorf("%+v", config)
	}
}

// The sqlserver configuration's encrypt is the option, or true with TLS
// settings and false without; the TLS configuration is the spec's where
// there is TLS at all; and the credentials reach it whatever they hold.
func TestTheSQLServerConfiguration(t *testing.T) {
	withTLS := &tls.Config{ServerName: "db.example", MinVersion: tls.VersionTLS12}
	for _, tc := range []struct {
		tls     *tls.Config
		options map[string]string
		want    msdsn.Encryption
	}{
		{nil, nil, msdsn.EncryptionOff},
		{withTLS, nil, msdsn.EncryptionRequired},
		{withTLS, map[string]string{"encrypt": "strict"}, msdsn.EncryptionStrict},
		{nil, map[string]string{"encrypt": "disable"}, msdsn.EncryptionDisabled},
	} {
		config, err := sqlServerConfig(sqlSpec(t, "1433", tc.tls, tc.options))
		if err != nil {
			t.Fatal(err)
		}
		if config.Encryption != tc.want || tc.tls != nil && config.TLSConfig != tc.tls || config.Host != "127.0.0.1" || config.Port != 1433 ||
			config.User != "metrics" || config.Password != "p@ss:w/rd?" || config.Database != "app" || config.AppName != sqlAppName {
			t.Errorf("%v %v: %+v", tc.tls != nil, tc.options, config)
		}
	}
}

// Each default driver connects through the exporter's dialer: a target the
// policy refuses by the address it reached is refused, whatever the driver.
func TestEachDefaultSQLDriverDialsThroughTheExporter(t *testing.T) {
	port := listen(t)
	for _, name := range sqlDefaultDrivers {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := sqlDriverTable[name].connect(ctx, sqlSpec(t, port, nil, nil))
		cancel()
		var refused *TargetRefusedError
		if !errors.As(err, &refused) {
			t.Errorf("%s: %v, want the policy's refusal", name, err)
		}
	}
}
