//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The sql request type runs one statement on a database and hands the rows
// it returns to the transforms: by column name to a csv transform, one
// series per row, and as a list of objects, [{column: value}, ...], to jq,
// yq and python.
//
//	request:
//	  type: sql
//	  driver: postgres                       # required
//	  database: app
//	  username: metrics                      # required
//	  password_file: /etc/exporter/db/password
//	  statement: SELECT state, count(*) AS n FROM jobs WHERE queue = $1 GROUP BY state
//	  args: ["{{param_queue:default}}"]
//
// The probe's target is the server, host or host:port, the driver's port
// when it is left out; a Snowflake target is the account identifier. No
// credentials, database or options are written in the target: they are the
// collector's keys, so a probe cannot reach a database the configuration
// does not name. The statement is written in the driver's own placeholder
// style ($1, ?, @p1, :1), and args are sent as values, never as SQL: bound
// to it, or, by the clickhouse driver, written into it quoted as literals;
// args and database may hold {{param_...}} placeholders, nothing else.
//
//   - The statement runs read-only where the database can make it so: in a
//     read-only transaction (postgres, mysql, oracle, vertica) or with a
//     read-only setting (clickhouse). SQL Server and Snowflake cannot, and
//     need an account that can only read.
//   - It is bounded by the scrape's deadline, which the drivers cancel it
//     at, and by the database's own statement timeout where it has one the
//     exporter can set (postgres).
//   - Connections are kept in a pool per collector, target and connection
//     settings, shared by the collector's scrapes of the target, and closed
//     when a reload removes or changes the collector (sqlpool.go).
//
// The drivers live in sqldriver_<name>.go files, each the only file that
// imports its module (sqldriver.go): mysql, postgres and sqlserver with the
// type, the others only with their sql_driver_<name> tag. Without this
// type's build constraint none of them is linked.

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeSQL,
		Fields: []string{
			"driver", "database", "statement", "args", "options", "service_name",
			"username", "password_file", "private_key_file", "tls",
			"max_rows", "max_response_bytes", "connections",
			"allowed_targets", "denied_targets",
		},
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout", "username", "password_file", "private_key_file", "connections", "tls"},
		Requester:    sqlRequester{},
		Rows:         true,
		TargetHint: func(c *model.Collector) string {
			if c.Request.Driver == SQLDriverSnowflake {
				return "the Snowflake account identifier"
			}
			return "the database server, host or host:port"
		},
		Templates: sqlTemplates,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its request.database, its args or its label values")
		},
		SchemaRule:         sqlSchemaRule,
		CheckTargetRequest: checkSQLTargetRequest,
		Stage:              "sql",
		CheckFiles:         checkSQLFiles,
		Follow:             sqlPools.follow,
		CloseConnections:   sqlPools.closeAll,
		Connections:        sqlPools.collectorConnections,
		ConnectionLimit:    sqlPools.limitCounts,
		ConnectionPools:    sqlPools.collectorPools,
		Families: []string{
			"http_exporter_sql_connections", "http_exporter_sql_idle_connections", "http_exporter_sql_targets",
			"http_exporter_sql_connects_total", "http_exporter_sql_keepalive_failures_total",
			"http_exporter_sql_limit_waits_total", "http_exporter_sql_limit_failures_total", "http_exporter_sql_rows",
		},
	})
	templateEscapers["database"] = escapeSQLDatabase
}

// sqlRequester is the sql request type's Requester.
type sqlRequester struct{}

// Validate checks a sql collector's request and fills in its defaults.
func (sqlRequester) Validate(c *model.Collector) error {
	return validateSQLRequest(c)
}

// Fetch performs one request of a sql collector.
func (sqlRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return fetchSQL(ctx, target, c, overrides, forwarded)
}

// CheckTarget checks a sql collector's target.
func (sqlRequester) CheckTarget(c *model.Collector, target string, static bool) error {
	return checkSQLTarget(c, target, static)
}

// Label is the url label of a sql request.
func (sqlRequester) Label(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	return sqlLabel(target, c, overrides)
}

// Method is the http_method label of a sql request.
func (sqlRequester) Method(*model.Collector, RequestOverrides) string { return sqlMethod }

// Display renders a sql target for logs and labels.
func (sqlRequester) Display(target string) string {
	return sqlDisplay(target)
}

// sqlMethod is the http_method label of a statement.
const sqlMethod = "QUERY"

// The defaults of request.max_rows and of the connection settings.
const (
	defaultSQLMaxRows          = 10000
	defaultSQLMaxPerTarget     = 2
	defaultSQLMaxIdlePerTarget = 1
	defaultSQLMaxTargets       = 50
	defaultSQLIdleTimeout      = 5 * time.Minute
	defaultSQLMaxLifetime      = time.Hour
	defaultSQLKeepalive        = 30 * time.Second
)

// The ways a kept connection is checked (request.connections.keepalive.check).
const (
	sqlCheckTCP  = "tcp"
	sqlCheckPing = "ping"
)

// sqlMinKeepalive is the shortest keepalive interval but 0, as sshfile's.
const sqlMinKeepalive = time.Second

// sqlAccount is what a Snowflake account identifier may be made of: an
// organisation and account name, myorg-myaccount, or a locator with its
// region, xy12345.eu-west-1.
var sqlAccount = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// sqlDriverOf is the driver of a validated collector.
func sqlDriverOf(c *model.Collector) *sqlDriver {
	return sqlDriverTable[c.Request.Driver]
}

func validateSQLRequest(x *model.Collector) error {
	r := &x.Request
	r.Driver = strings.ToLower(strings.TrimSpace(r.Driver))
	d, err := checkSQLDriver(x.Name, r.Driver)
	if err != nil {
		return err
	}
	if strings.TrimSpace(r.Statement) == "" {
		return fmt.Errorf("collector %q has no request.statement; a sql collector runs one statement, such as SELECT state, count(*) AS n FROM jobs GROUP BY state", x.Name)
	}
	if d.statement != nil {
		if err := d.statement(r.Statement); err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
	}
	if strings.TrimSpace(r.Username) == "" {
		return fmt.Errorf("collector %q request.username is required for request.type sql: the account the statement runs as", x.Name)
	}
	if err := checkSQLAccountKeys(d, r.Username, r.PasswordFile, r.PrivateKeyFile, true); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	switch {
	case d.serviceName && strings.TrimSpace(r.ServiceName) == "":
		return fmt.Errorf("collector %q request.service_name is required for the %s driver: the service of the database the statement runs on", x.Name, d.name)
	case d.serviceName && r.Database != "":
		return fmt.Errorf("collector %q sets request.database, which the %s driver does not take; the database is named by request.service_name", x.Name, d.name)
	case !d.serviceName && r.ServiceName != "":
		return fmt.Errorf("collector %q sets request.service_name, which only the oracle driver takes; name the database with request.database", x.Name)
	}
	if err := checkSQLNameKey("request.service_name", r.ServiceName); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if r.Database != "" && strings.TrimSpace(r.Database) == "" {
		return fmt.Errorf("collector %q request.database is written with blanks alone; name the database, or leave the key out for the account's default", x.Name)
	}
	if _, err := (templateField{"request.database", r.Database, "database"}).parse(); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	for i, arg := range r.Args {
		if _, err := (templateField{fmt.Sprintf("request.args[%d]", i), arg, "arg"}).parse(); err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
	}
	if err := checkSQLOptions(d, r.Options, r.TLS); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if r.MaxRows < 0 {
		return fmt.Errorf("collector %q request.max_rows must not be negative; 0, or leaving it out, is %d", x.Name, defaultSQLMaxRows)
	}
	if err := checkSQLConnections(r.Connections, false); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	return nil
}

// checkSQLDriver refuses a driver that is not given, that does not exist,
// or that this build does not include, saying how to build one that does.
func checkSQLDriver(collector, name string) (*sqlDriver, error) {
	if d := sqlDriverTable[name]; d != nil {
		return d, nil
	}
	built := strings.Join(BuiltSQLDrivers(), ", ")
	switch {
	case name == "":
		return nil, fmt.Errorf("collector %q has no request.driver; a sql collector names its database's driver, one of: %s", collector, built)
	case slices.Contains(knownSQLDrivers, name):
		return nil, fmt.Errorf("collector %q uses sql driver %q, which this build does not include; it was built with: %s. Build with SQL_DRIVERS=%s (make build SQL_DRIVERS=%s, or the image's SQL_DRIVERS build argument)", collector, name, built, name, name)
	}
	return nil, fmt.Errorf("collector %q has unknown request.driver %q; the sql drivers are %s, and this build has: %s", collector, name, strings.Join(knownSQLDrivers, ", "), built)
}

// checkSQLAccountKeys holds the account and credential keys of a collector,
// or of a static target, to their rules: none written with blanks alone,
// which is neither a name nor the key left out, which "" is; no line break
// in the account's name; a private key only for a driver with key-pair
// authentication, and not beside a password; and, for such a driver's
// collector, one of the two. The keys take no placeholder, which the
// configuration refuses where it is not filled.
func checkSQLAccountKeys(d *sqlDriver, username, passwordFile, privateKeyFile string, collector bool) error {
	if username != "" && strings.TrimSpace(username) == "" {
		return errors.New("request.username is written with blanks alone; name the account, or leave the key out")
	}
	if strings.ContainsFunc(username, unicode.IsControl) {
		return fmt.Errorf("request.username %q has a control character, which no account's name has", username)
	}
	for _, key := range []struct{ name, value string }{{"request.password_file", passwordFile}, {"request.private_key_file", privateKeyFile}} {
		if key.value != "" && strings.TrimSpace(key.value) == "" {
			return fmt.Errorf("%s is written with blanks alone; name the file, or leave the key out", key.name)
		}
	}
	switch {
	case privateKeyFile != "" && !d.keyPair:
		return fmt.Errorf("sets request.private_key_file, which only the snowflake driver takes, for key-pair authentication; the %s driver authenticates with request.password_file", d.name)
	case privateKeyFile != "" && passwordFile != "":
		return errors.New("sets both request.password_file and request.private_key_file; a snowflake account authenticates with one of them")
	case collector && d.keyPair && privateKeyFile == "" && passwordFile == "":
		return errors.New("sets neither request.password_file nor request.private_key_file; a snowflake account authenticates with a password or, by key-pair authentication, a private key")
	}
	return nil
}

// checkSQLNameKey refuses a name written with blanks alone or holding a
// control character.
func checkSQLNameKey(key, value string) error {
	if value != "" && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is written with blanks alone; name it, or leave the key out", key)
	}
	if strings.ContainsFunc(value, unicode.IsControl) {
		return fmt.Errorf("%s %q has a control character", key, value)
	}
	return nil
}

// checkSQLOptions holds request.options to the driver's allow-list, each
// value to those its option takes, and request.tls to what the driver can
// honour: a tls key beside an option that switches TLS off, or one the
// driver cannot apply, would be ignored without a word.
func checkSQLOptions(d *sqlDriver, options map[string]string, t model.TLSConfig) error {
	for _, name := range model.SortedKeys(options) {
		allowed, known := d.options[name]
		if !known {
			return fmt.Errorf("request.options sets %q, which the %s driver does not take; it takes: %s", name, d.name, sqlOptionNames(d))
		}
		value := options[name]
		if allowed != nil && !slices.Contains(allowed, value) {
			return fmt.Errorf("request.options.%s is %q; the %s driver takes one of %s", name, value, d.name, strings.Join(allowed, ", "))
		}
		if strings.TrimSpace(value) == "" || strings.ContainsFunc(value, unicode.IsControl) {
			return fmt.Errorf("request.options.%s is %q; write a value without control characters, or leave the option out", name, value)
		}
	}
	return checkSQLTLS(d, options, t)
}

// checkSQLTLS holds a tls block, a collector's or a static target's own, to
// what the driver can honour with the collector's options.
func checkSQLTLS(d *sqlDriver, options map[string]string, t model.TLSConfig) error {
	tlsSet := t != model.TLSConfig{}
	if (t.CertFile == "") != (t.KeyFile == "") {
		return errors.New("request.tls sets only one of cert_file and key_file; a client certificate needs both")
	}
	for _, key := range d.refusedTLS {
		if key == "server_name" && t.ServerName != "" {
			return fmt.Errorf("sets request.tls.server_name, which the %s driver cannot honour: it checks the certificate against the target's host", d.name)
		}
	}
	mode := sqlTLSModeOf(d, options, tlsSet)
	if tlsSet && mode == sqlTLSOff {
		return fmt.Errorf("sets request.tls, but request.options.%s is %q, which uses no TLS; leave tls out, or set %s to a value that uses TLS", d.tlsOption, options[d.tlsOption], d.tlsOption)
	}
	// The keys that say how the certificate is verified are ignored by a
	// mode that verifies none: the CA named would never be used. A client
	// certificate is presented all the same.
	if mode == sqlTLSInsecure && (t.CAFile != "" || t.ServerName != "") {
		return fmt.Errorf("sets request.tls.ca_file or server_name, which say how the server's certificate is verified, but request.options.%s is %q, which does not verify it; "+
			"set %s to %s to verify it with them, or leave them out", d.tlsOption, options[d.tlsOption], d.tlsOption, sqlVerifyingValue(d))
	}
	return nil
}

// sqlVerifyingValue is the value of d's TLS option that verifies the
// server's certificate, for messages.
func sqlVerifyingValue(d *sqlDriver) string {
	values := make([]string, 0, 1)
	for value, mode := range d.tlsModes {
		if mode == sqlTLSVerify {
			values = append(values, value)
		}
	}
	slices.Sort(values)
	return strings.Join(values, " or ")
}

// checkSQLConnections holds request.connections to its rules: no count or
// duration below 0, a keepalive interval of a second at least but 0, and a
// check of tcp or ping. A static target's block, target, may not set
// max_targets, which is the collector's across its targets.
func checkSQLConnections(conns *model.SQLConnections, target bool) error {
	if conns == nil {
		return nil
	}
	if target && conns.MaxTargets != nil {
		return errors.New("request.connections.max_targets is the most targets a collector keeps connections to, so a static target cannot set it; set it in the collector's request.connections")
	}
	for _, count := range []struct {
		key   string
		value *int
	}{{"max_per_target", conns.MaxPerTarget}, {"max_idle_per_target", conns.MaxIdlePerTarget}, {"max_targets", conns.MaxTargets}} {
		if count.value != nil && *count.value < 0 {
			return fmt.Errorf("request.connections.%s must not be negative; 0 sets no limit", count.key)
		}
	}
	for _, duration := range []struct {
		key   string
		value *model.Duration
	}{{"idle_timeout", conns.IdleTimeout}, {"max_lifetime", conns.MaxLifetime}} {
		if duration.value != nil && *duration.value < 0 {
			return fmt.Errorf("request.connections.%s must not be negative; 0 keeps a connection as long as the collector is configured", duration.key)
		}
	}
	if k := conns.Keepalive; k != nil {
		if k.Interval != nil && *k.Interval < 0 {
			return errors.New("request.connections.keepalive.interval must not be negative; 0 checks no connection")
		}
		if k.Interval != nil && *k.Interval > 0 && time.Duration(*k.Interval) < sqlMinKeepalive {
			return fmt.Errorf("request.connections.keepalive.interval %s is under the least, %s; set 0 to check no connection", time.Duration(*k.Interval), sqlMinKeepalive)
		}
		if k.Check != "" && k.Check != sqlCheckTCP && k.Check != sqlCheckPing {
			return fmt.Errorf("request.connections.keepalive.check is %q; want tcp, TCP keepalives on the connection, or ping, a query of the database", k.Check)
		}
	}
	return nil
}

// checkSQLTargetRequest checks what a static target's request block sets
// for a sql collector: its account and credential file, which replace the
// collector's together (a target that sets either replaces both of the
// collector's credential files with its own), and take no placeholder, as
// sshfile's do not, since a static target's own values are used as they are
// written; its own tls block, which replaces the collector's whole and is
// held to what the driver can honour with the collector's options; and its
// connection settings.
func checkSQLTargetRequest(c *model.Collector, t *model.StaticTarget) error {
	r := &t.Request
	for _, key := range []struct{ name, value string }{{"request.username", r.Username}, {"request.password_file", r.PasswordFile}, {"request.private_key_file", r.PrivateKeyFile}} {
		if strings.Contains(key.value, "{{") {
			return fmt.Errorf("%s %q has a placeholder; a static target's account and credential files are used as they are written, with no probe to fill one, so write the value out", key.name, key.value)
		}
	}
	d := sqlDriverOf(c)
	if d == nil {
		// A collector that was never validated: its target's keys are held
		// to the rules every driver's are.
		d = &sqlDriver{name: c.Request.Driver}
	}
	if err := checkSQLAccountKeys(d, r.Username, r.PasswordFile, r.PrivateKeyFile, false); err != nil {
		return err
	}
	if r.TLS != nil {
		if err := checkTargetTLS(t); err != nil {
			return err
		}
		if err := checkSQLTLS(d, c.Request.Options, *r.TLS); err != nil {
			return err
		}
	}
	return checkSQLConnections(r.Connections, true)
}

// sqlAddress is a target as a sql request reaches it: host is the name or
// address the policy matches, hostPort what is dialed, and account a
// Snowflake target's account identifier.
type sqlAddress struct {
	host, port, hostPort, account string
}

// parseSQLTarget reads a target of c: host, host:port or [v6]:port, the
// driver's port when it names none; for a driver whose targets are accounts,
// an account identifier. Nothing else may come with it: the account is
// request.username and the database request.database.
func parseSQLTarget(c *model.Collector, raw string) (sqlAddress, error) {
	raw = strings.TrimSpace(raw)
	// A collector that was never validated may name no driver this build
	// has; its target is then read as a host and a port, the port left
	// out where it names none.
	d := sqlDriverOf(c)
	if d == nil {
		d = &sqlDriver{port: func(map[string]string) string { return "" }}
	}
	if strings.ContainsAny(raw, "/?#@") {
		return sqlAddress{}, fmt.Errorf("target %q has a scheme, path, query or user in it; a sql target is only the server, host or host:port: the account is request.username and the database request.database", raw)
	}
	if d.account {
		account := strings.ToLower(raw)
		if !sqlAccount.MatchString(account) {
			return sqlAddress{}, fmt.Errorf("target %q is not a Snowflake account identifier; write the account as myorg-myaccount, or its locator as xy12345.eu-west-1", raw)
		}
		host := d.host(account)
		port := d.port(c.Request.Options)
		return sqlAddress{host: host, port: port, hostPort: net.JoinHostPort(host, port), account: account}, nil
	}
	host, port := raw, d.port(c.Request.Options)
	if h, p, err := net.SplitHostPort(raw); err == nil {
		if p == "" {
			return sqlAddress{}, fmt.Errorf("target %q ends in a colon with no port after it; write the port after the colon, or leave the colon out for the driver's port", raw)
		}
		host, port = h, p
	} else if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		host = raw[1 : len(raw)-1]
	} else if strings.Count(raw, ":") == 1 {
		return sqlAddress{}, fmt.Errorf("target %q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::1]:5432", raw)
	}
	if host == "" {
		return sqlAddress{}, fmt.Errorf("target %q names no host; a sql target is host or host:port", raw)
	}
	if n, err := strconv.Atoi(port); port != "" && (err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port) {
		return sqlAddress{}, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return sqlAddress{}, err
	}
	// An IPv6 address keeps its brackets without a port too, so that what
	// is shown and dialed is not read as an address and a port.
	hostPort := canonical
	if port != "" {
		hostPort = net.JoinHostPort(canonical, port)
	} else if strings.Contains(canonical, ":") {
		hostPort = "[" + canonical + "]"
	}
	return sqlAddress{host: canonical, port: port, hostPort: hostPort}, nil
}

// checkSQLTarget is a probe's and a static target's check of the target.
func checkSQLTarget(c *model.Collector, target string, _ bool) error {
	_, err := parseSQLTarget(c, target)
	return err
}

// sqlDisplay renders a target for logs and labels: as written, since a sql
// target holds no credentials, when it holds nothing but a host, a port or
// an account.
func sqlDisplay(target string) string {
	target = strings.TrimSpace(target)
	if target == "" || strings.ContainsAny(target, "/?#@ \t") || strings.ContainsFunc(target, unicode.IsControl) {
		return "<invalid target>"
	}
	return target
}

// sqlLabel is the url label of a statement: driver://host:port/ and the
// database, or an Oracle service, as written, its placeholders as they are.
// It holds neither the statement nor a credential.
func sqlLabel(target string, c *model.Collector, _ RequestOverrides) (string, error) {
	address, err := parseSQLTarget(c, target)
	if err != nil {
		return "", err
	}
	database := c.Request.Database
	if d := sqlDriverOf(c); d != nil && d.serviceName {
		database = c.Request.ServiceName
	}
	return c.Request.Driver + "://" + address.hostPort + "/" + database, nil
}

// sqlBase is what a pool depends on of a collector's configuration and of a
// static target's request, besides the target: the account and credential
// files, the database as written, the options, the TLS settings, the
// connection settings, the policy and the response limit, which bounds what
// a driver reads of the server at once where the driver can be told
// (sqlConnectSpec.limit). A collector's pools whose base the
// configuration in force no longer gives are closed (sqlPoolCache.follow).
type sqlBase struct {
	driver                                 string
	username, passwordFile, privateKeyFile string
	database, serviceName                  string
	// options is the options as one text, name=value pairs in name order.
	options string
	tls     model.TLSConfig
	conns   sqlConnSettings
	policy  *targetPolicy
	limit   int64
}

// sqlConnSettings are the connection settings, with their defaults.
type sqlConnSettings struct {
	maxPerTarget, maxIdle, maxTargets   int
	idleTimeout, maxLifetime, keepalive time.Duration
	check                               string
}

// sqlBaseOf is the base of a scrape of c with overrides: the collector's
// settings, with a static target's account, credential files, tls block and
// connection settings over them.
func sqlBaseOf(c *model.Collector, overrides RequestOverrides) sqlBase {
	r := &c.Request
	b := sqlBase{
		driver:         r.Driver,
		username:       strings.TrimSpace(r.Username),
		passwordFile:   r.PasswordFile,
		privateKeyFile: r.PrivateKeyFile,
		database:       r.Database,
		serviceName:    r.ServiceName,
		tls:            r.TLS,
		conns:          sqlConnSettingsOf(r.Connections, overrides.SQLConnections),
		policy:         policyOf(c),
		limit:          responseLimit(c),
	}
	var options strings.Builder
	for _, name := range model.SortedKeys(r.Options) {
		options.WriteString(name)
		options.WriteByte('=')
		options.WriteString(r.Options[name])
		options.WriteByte(0)
	}
	b.options = options.String()
	if name := strings.TrimSpace(overrides.Username); name != "" {
		b.username = name
	}
	if overrides.PasswordFile != "" || overrides.PrivateKeyFile != "" {
		b.passwordFile, b.privateKeyFile = overrides.PasswordFile, overrides.PrivateKeyFile
	}
	// A target's own tls block replaces the collector's whole; being part
	// of the base, it is part of the pool's key (sqlPoolKey), so targets
	// with blocks of their own share no connection.
	if overrides.TLS != nil {
		b.tls = *overrides.TLS
	}
	return b
}

// sqlConnSettingsOf is the connection settings of a collector's block with
// a static target's over it, key by key, and the defaults under both.
func sqlConnSettingsOf(collector, target *model.SQLConnections) sqlConnSettings {
	s := sqlConnSettings{
		maxPerTarget: defaultSQLMaxPerTarget, maxIdle: defaultSQLMaxIdlePerTarget, maxTargets: defaultSQLMaxTargets,
		idleTimeout: defaultSQLIdleTimeout, maxLifetime: defaultSQLMaxLifetime, keepalive: defaultSQLKeepalive, check: sqlCheckTCP,
	}
	for _, conns := range []*model.SQLConnections{collector, target} {
		if conns == nil {
			continue
		}
		if conns.MaxPerTarget != nil {
			s.maxPerTarget = *conns.MaxPerTarget
		}
		if conns.MaxIdlePerTarget != nil {
			s.maxIdle = *conns.MaxIdlePerTarget
		}
		if conns.MaxTargets != nil {
			s.maxTargets = *conns.MaxTargets
		}
		if conns.IdleTimeout != nil {
			s.idleTimeout = time.Duration(*conns.IdleTimeout)
		}
		if conns.MaxLifetime != nil {
			s.maxLifetime = time.Duration(*conns.MaxLifetime)
		}
		if k := conns.Keepalive; k != nil {
			if k.Interval != nil {
				s.keepalive = time.Duration(*k.Interval)
			}
			if k.Check != "" {
				s.check = k.Check
			}
		}
	}
	return s
}

// sqlDatabase is the database of a scrape, its placeholders filled.
func sqlDatabase(c *model.Collector, overrides RequestOverrides) (string, error) {
	if !HasPathParams(c.Request.Database) {
		return c.Request.Database, nil
	}
	return templateField{"request.database", c.Request.Database, "database"}.render(overrides.Params)
}

// sqlArgs is the values bound to the statement, their placeholders filled.
func sqlArgs(c *model.Collector, overrides RequestOverrides) ([]any, error) {
	args := make([]any, len(c.Request.Args))
	for i, arg := range c.Request.Args {
		if !HasPathParams(arg) {
			args[i] = arg
			continue
		}
		value, err := templateField{fmt.Sprintf("request.args[%d]", i), arg, "arg"}.render(overrides.Params)
		if err != nil {
			return nil, err
		}
		args[i] = value
	}
	return args, nil
}

// sqlMaxRows is the most rows a statement of c may return.
func sqlMaxRows(c *model.Collector) int {
	if c.Request.MaxRows > 0 {
		return c.Request.MaxRows
	}
	return defaultSQLMaxRows
}

// fetchSQL runs a scrape's statement on the target and answers its rows.
func fetchSQL(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseSQLTarget(c, target)
	if err != nil {
		return nil, err
	}
	database, err := sqlDatabase(c, overrides)
	if err != nil {
		return nil, err
	}
	args, err := sqlArgs(c, overrides)
	if err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	base := sqlBaseOf(c, overrides)
	d := sqlDriverOf(c)
	// The host's name now; each address a connection is made to when it
	// is made, as an http request's connections are checked. A driver that
	// dials by itself has its host's addresses checked now instead.
	if base.policy != nil {
		var err error
		if d.ownDial {
			err = base.policy.checkOwnDial(ctx, address.host)
		} else {
			_, err = base.policy.check(ctx, address.host, false)
		}
		if err != nil {
			return nil, err
		}
	}
	key := sqlPoolKey{collector: c.Name, address: address, database: database, base: base}
	result, err := sqlPools.query(ctx, key, c, args)
	if err != nil {
		return nil, onSQLHost(err, address.hostPort)
	}
	return &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       result.shown,
		Rows:       result.rows,
		Columns:    result.columns,
		Target:     target,
		Collector:  c.Name,
		Duration:   time.Since(start),
	}, nil
}

// onSQLHost names the server in a failed statement's error, unless the
// error names it already.
func onSQLHost(err error, hostPort string) error {
	if strings.Contains(err.Error(), hostPort) {
		return err
	}
	return fmt.Errorf("%w (on %s)", err, hostPort)
}

// sqlCredentials reads the credential files of base, as a connection is
// made: the password, empty when there is no password_file, and the
// private key of key-pair authentication.
func sqlCredentials(base *sqlBase) (password string, key *rsa.PrivateKey, err error) {
	if base.passwordFile != "" {
		password, err = ReadCredentialFile(base.passwordFile)
		if err != nil {
			return "", nil, fmt.Errorf("reading request.password_file: %w", err)
		}
		if password == "" {
			return "", nil, fmt.Errorf("request.password_file %s is empty", base.passwordFile)
		}
		if strings.ContainsFunc(password, unicode.IsControl) {
			return "", nil, fmt.Errorf("request.password_file %s holds a control character, a line break inside the password perhaps; write the password alone on one line", base.passwordFile)
		}
	}
	if base.privateKeyFile != "" {
		key, err = sqlPrivateKey(base.privateKeyFile)
		if err != nil {
			return "", nil, err
		}
	}
	return password, key, nil
}

// sqlPrivateKey reads the RSA private key of Snowflake's key-pair
// authentication: unencrypted PKCS#8, as openssl pkcs8 -topk8 -nocrypt
// writes it, or PKCS#1.
func sqlPrivateKey(file string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading request.private_key_file: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("request.private_key_file %s holds no PEM private key", file)
	}
	switch block.Type {
	case "ENCRYPTED PRIVATE KEY":
		return nil, fmt.Errorf("request.private_key_file %s is encrypted, which the exporter cannot read; write it unencrypted with openssl pkcs8 -topk8 -nocrypt, and keep the file readable by the exporter alone", file)
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("request.private_key_file %s: %w", file, err)
		}
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("request.private_key_file %s: %w", file, err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("request.private_key_file %s holds a %T key; Snowflake's key-pair authentication takes an RSA key", file, parsed)
	}
	return key, nil
}

// sqlTLSConfig is the TLS configuration of a connection with base to host:
// none when its driver's TLS option, or the absence of tls keys, says so;
// otherwise the tls block's, its files read now, the certificate checked
// against tls.server_name or the host, and not checked at all when the
// option asks for TLS without verification, or for TLS verified only as tls
// keys say and none is set. digest tells it from another.
func sqlTLSConfig(d *sqlDriver, base *sqlBase, options map[string]string, host string) (config *tls.Config, digest string, err error) {
	tlsSet := base.tls != model.TLSConfig{}
	mode := sqlTLSModeOf(d, options, tlsSet)
	if mode == sqlTLSOff {
		return nil, "", nil
	}
	cfg, err := tlsConfig(base.tls)
	if err != nil {
		return nil, "", fmt.Errorf("request.tls: %w", err)
	}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	if mode == sqlTLSInsecure || mode == sqlTLSVerifyByKeys && !tlsSet {
		cfg.InsecureSkipVerify = true
	}
	sum := sha256.New()
	fmt.Fprintf(sum, "%s\x00%t\x00", cfg.ServerName, cfg.InsecureSkipVerify)
	for _, file := range []string{base.tls.CAFile, base.tls.CertFile, base.tls.KeyFile} {
		if file != "" {
			b, _ := os.ReadFile(file)
			sum.Write(b)
		}
		sum.Write([]byte{0})
	}
	return cfg, hex.EncodeToString(sum.Sum(nil))[:32], nil
}

// checkSQLFiles reads the credential files of a collector, or of a static
// target that sets its own, and the TLS files, the collector's or a static
// target's own block's, as making a connection reads
// them, for --dry-run: a file missing or unreadable, an empty password, a
// key that is none or is encrypted, a certificate that does not load.
// Loading the configuration reads none of them, since a file may be mounted
// after the exporter starts.
func checkSQLFiles(c *model.Collector, t *model.StaticTarget) []string {
	who := fmt.Sprintf("collector %q", c.Name)
	overrides := RequestOverrides{}
	// A target's files are read when it names its own, the collector's
	// being read as the collector's.
	credentials, ownTLS := true, true
	if t != nil {
		credentials, ownTLS = t.Request.PasswordFile != "" || t.Request.PrivateKeyFile != "", t.Request.TLS != nil
		if !credentials && !ownTLS {
			return nil
		}
		who, overrides = fmt.Sprintf("target %q", t.Name), TargetOverrides(t)
	}
	base := sqlBaseOf(c, overrides)
	var problems []error
	if credentials {
		if _, _, err := sqlCredentials(&base); err != nil {
			problems = append(problems, err)
		}
	}
	if d := sqlDriverOf(c); ownTLS && d != nil {
		if _, _, err := sqlTLSConfig(d, &base, c.Request.Options, "localhost"); err != nil {
			problems = append(problems, err)
		}
	}
	warnings := make([]string, 0, len(problems))
	for _, err := range problems {
		warnings = append(warnings, fmt.Sprintf("%s: %v; a scrape would fail on it now (the file is read when a connection is made, so one put in place later is used then)", who, err))
	}
	return warnings
}

// sqlSchemaRule is what a sql collector's request requires, as the exporter
// does (validateSQLRequest): driver, statement and username, each
// written with something other than ""; for the oracle driver, when it is
// built, service_name; and for the snowflake driver, when it is built,
// password_file or private_key_file; each driver named as the exporter
// matches it (SchemaCaselessOneOf).
func sqlSchemaRule() map[string]any {
	keys := []string{"driver", "statement", "username"}
	properties := map[string]any{}
	for _, key := range keys {
		properties[key] = SchemaWrittenKey(key)["properties"].(map[string]any)[key]
	}
	rule := map[string]any{"required": keys, "properties": properties}
	driverIs := func(name string) map[string]any {
		return map[string]any{"properties": map[string]any{"driver": map[string]any{"type": "string", "pattern": SchemaCaselessOneOf(name)}}, "required": []string{"driver"}}
	}
	var conditions []any
	for _, name := range BuiltSQLDrivers() {
		switch name {
		case SQLDriverOracle:
			conditions = append(conditions, map[string]any{"if": driverIs(name), "then": SchemaWrittenKey("service_name")})
		case SQLDriverSnowflake:
			conditions = append(conditions, map[string]any{"if": driverIs(name), "then": map[string]any{"anyOf": []any{SchemaWrittenKey("password_file"), SchemaWrittenKey("private_key_file")}}})
		}
	}
	if len(conditions) > 0 {
		rule["allOf"] = conditions
	}
	return rule
}

// sqlDatabaseName is what a placeholder may be filled with in a sql
// collector's request.database: the characters of a database name that
// need no quoting in any of the databases.
var sqlDatabaseName = regexp.MustCompile(`^[A-Za-z0-9_.$-]+$`)

// sqlTemplates are a sql collector's database, which names the database
// the statement runs on (sqlDatabaseName), and the values bound to its
// statement, sent as values.
func sqlTemplates(c *model.Collector, _ RequestOverrides) []templateField {
	var out []templateField
	if HasPathParams(c.Request.Database) {
		out = append(out, templateField{"request.database", c.Request.Database, "database"})
	}
	for i, arg := range c.Request.Args {
		if HasPathParams(arg) {
			out = append(out, templateField{fmt.Sprintf("request.args[%d]", i), arg, "arg"})
		}
	}
	return out
}

// escapeSQLDatabase refuses a value that is no database name
// (sqlDatabaseName).
func escapeSQLDatabase(where, name, value string) (string, error) {
	if !sqlDatabaseName.MatchString(value) {
		return "", fmt.Errorf("%s: the value of %s, %q, may hold only letters, digits and _ - . $, since it names the database the statement runs on", where, name, value)
	}
	return value, nil
}
