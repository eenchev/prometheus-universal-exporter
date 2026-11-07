//go:build !select_request_types || request_type_sql

package fetch

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
//     read-only setting (clickhouse). SQL Server cannot, and runs it in a
//     transaction always rolled back; Snowflake runs it as it is. Both need
//     an account that can only read.
//   - It is bounded by the scrape's deadline, which the drivers cancel it
//     at, and by the database's own time limit where it has one the
//     exporter can set: postgres's statement_timeout, MySQL's
//     max_execution_time, MariaDB's max_statement_time and SQL Server's
//     LOCK_TIMEOUT.
//   - Connections are kept in a pool per collector, target and connection
//     settings, shared by the collector's scrapes of the target, and closed
//     when a reload removes or changes the collector (sqlpool.go).
//
// The drivers live in sqldriver_<name>.go files, each the only file that
// imports its module (sqldriver.go): mysql, postgres and sqlserver with the
// type, the others only with their sql_driver_<name> tag. Without this
// type's build constraint none of them is linked.

// SQLConfig is a sql collector's own request keys: the database driver,
// the database, the one statement run and the values bound to its
// placeholders, the driver's allow-listed settings, and an Oracle
// database's service; the most rows a statement may return, 0 for the
// default; and how the type keeps connections to a target. The account and
// its credential files are of the common part, which other types take too.
type SQLConfig struct {
	Driver    string `yaml:"driver,omitempty"`
	Database  string `yaml:"database,omitempty"`
	Statement string `yaml:"statement,omitempty"`
	// StatementFile is the file the statement is read from, in place of
	// Statement, when the configuration loads (readSQLStatementFile); the
	// text read is statementText, which the statement run is.
	StatementFile string `yaml:"statement_file,omitempty"`
	statementText string
	Args          []string `yaml:"args,omitempty"`
	// ArgTypes are the types args are bound as, one for each, by its
	// place: text, int, float or bool (sqlArgTypes). Left out, every arg
	// is bound as text.
	ArgTypes    []string              `yaml:"arg_types,omitempty"`
	Options     map[string]string     `yaml:"options,omitempty"`
	ServiceName string                `yaml:"service_name,omitempty"`
	MaxRows     int                   `yaml:"max_rows,omitempty"`
	Connections *model.SQLConnections `yaml:"connections,omitempty"`
	// Statements are several statements, each with its name and its own
	// args, run in order on one connection, in place of statement,
	// statement_file, args and arg_types: the document holds each one's
	// rows under its name (docs/SQL.md#several-statements).
	Statements []SQLStatement `yaml:"statements,omitempty"`
}

// SQLStatement is one entry of request.statements: its name, which its rows
// are under in the document, the statement, and the values bound to its
// placeholders, with their types, as request.args and request.arg_types
// are of a collector's one statement.
type SQLStatement struct {
	Name      string   `yaml:"name"`
	Statement string   `yaml:"statement"`
	Args      []string `yaml:"args,omitempty"`
	ArgTypes  []string `yaml:"arg_types,omitempty"`
}

// maxSQLStatements bounds request.statements: a handful of statements of
// one database, not a script.
const maxSQLStatements = 16

// sqlStatementName is what a request.statements entry's name is made of,
// which a rule reads its rows by: letters, digits and _, not beginning with
// a digit, as a jq field written .name takes it.
var sqlStatementName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// sqlRun is one statement a scrape runs: its name, "" for a collector's one
// statement, its text, and its args, filled and typed.
type sqlRun struct {
	name, statement string
	args            []any
}

// MarshalYAML writes the keys as a configuration writes them, and, for a
// statement read from statement_file, a digest of the text read: the YAML
// of a collector is what a reload compares to tell a changed collector and
// what a probe's cache key is made from (model.RequestConfig.MarshalYAML),
// and a statement file edited changes the collector as much as its
// statement edited in place would. Nothing reads the YAML back.
func (s SQLConfig) MarshalYAML() (any, error) {
	type plain SQLConfig
	if s.StatementFile == "" {
		return plain(s), nil
	}
	sum := sha256.Sum256([]byte(s.statementText))
	return struct {
		plain  `yaml:",inline"`
		Digest string `yaml:"statement_file_sha256"`
	}{plain(s), hex.EncodeToString(sum[:])}, nil
}

// sqlStatement is the statement a sql collector runs: its statement, or the
// text read from its statement_file when the configuration loaded.
func sqlStatement(c *model.Collector) string {
	own := sqlRequest(c)
	if own.StatementFile != "" {
		return own.statementText
	}
	return own.Statement
}

// maxSQLStatementFileBytes bounds a statement file: a statement is a few
// lines, and a file that is not one, a log or a dump at the path by
// mistake, is refused rather than read whole when the configuration loads.
const maxSQLStatementFileBytes = 1 << 20

// sqlStatementPlaceholder is a {{param_...}} placeholder in a statement,
// which no probe fills: a probe's values go in args.
var sqlStatementPlaceholder = regexp.MustCompile(`\{\{\s*` + PathParamPrefix)

// checkSQLStatementFileKey refuses a statement_file that is not an absolute
// path written as it is meant: with blanks around it, a placeholder, which
// nothing fills in a file's path, a NUL, or relative, which would be read
// from whatever directory the exporter, or a replica of the target
// allocator, was started in.
func checkSQLStatementFileKey(path string) error {
	switch {
	case strings.TrimSpace(path) == "":
		return errors.New("request.statement_file is written with blanks alone; name the file, or write the statement in request.statement")
	case strings.TrimSpace(path) != path:
		return fmt.Errorf("request.statement_file %q has blanks around it; write the path alone", path)
	case strings.Contains(path, "{{"):
		return fmt.Errorf("request.statement_file %q has a placeholder; the file is read when the configuration loads, with no probe to fill one, so write the path out", path)
	case strings.ContainsRune(path, 0):
		return fmt.Errorf("request.statement_file %q holds a NUL character, which no path does", path)
	case !filepath.IsAbs(path):
		return fmt.Errorf("request.statement_file %q is a relative path; write the absolute path of the file, such as /etc/exporter/sql/jobs.sql, which reads the same whatever directory the exporter starts in", path)
	}
	return nil
}

// readSQLStatementFile reads the statement of the file at path, refusing
// what is no statement file: no regular file (a directory, a named pipe,
// which would hold the load until something wrote to it, a device, a
// socket), a file over maxSQLStatementFileBytes, text that is not UTF-8 or
// holds a NUL, a file of blanks and nothing else, and a statement with a
// placeholder. A UTF-8 byte order mark at its start, as some editors write,
// and the blanks around the statement, are taken off.
func readSQLStatementFile(path string) (string, error) {
	where := "request.statement_file " + path
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", where, statementFileError(err))
	}
	if err := checkStatementFileInfo(where, info); err != nil {
		return "", err
	}
	// Opened only once it is known to be a regular file: opening a named
	// pipe would wait for a writer.
	f, err := os.Open(path) // #nosec G304 -- the configuration names the file, as it names its other files
	if err != nil {
		return "", fmt.Errorf("%s: %w", where, statementFileError(err))
	}
	defer func() { _ = f.Close() }()
	// What was opened is checked again: the path may have been replaced
	// between the two.
	if info, err = f.Stat(); err != nil {
		return "", fmt.Errorf("%s: %w", where, err)
	}
	if err := checkStatementFileInfo(where, info); err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSQLStatementFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", where, err)
	}
	if len(raw) > maxSQLStatementFileBytes {
		return "", fmt.Errorf("%s grew past %d bytes while it was read; a statement file holds one statement", where, maxSQLStatementFileBytes)
	}
	raw = bytes.TrimPrefix(raw, []byte("\ufeff"))
	switch {
	case !utf8.Valid(raw):
		return "", fmt.Errorf("%s is not UTF-8 text; save the statement as UTF-8", where)
	case bytes.IndexByte(raw, 0) >= 0:
		return "", fmt.Errorf("%s holds a NUL byte, which no statement does; is it the file meant?", where)
	}
	statement := strings.TrimSpace(string(raw))
	switch {
	case statement == "":
		return "", fmt.Errorf("%s holds no statement, only blanks; write the one statement the collector runs in it", where)
	case sqlStatementPlaceholder.MatchString(statement):
		return "", fmt.Errorf("%s has a {{param_...}} placeholder in its statement, which is not filled there and would be sent as written; write the driver's own placeholder in the statement and the probe's parameter in request.args", where)
	}
	return statement, nil
}

// checkStatementFileInfo refuses a statement file that is no regular file,
// or is over maxSQLStatementFileBytes.
func checkStatementFileInfo(where string, info os.FileInfo) error {
	switch mode := info.Mode(); {
	case mode.IsDir():
		return fmt.Errorf("%s is a directory; name the file of the statement in it", where)
	case !mode.IsRegular():
		return fmt.Errorf("%s is no regular file (%s); name a file that holds the statement", where, mode.Type())
	case info.Size() > maxSQLStatementFileBytes:
		return fmt.Errorf("%s is %d bytes, over the %d a statement file may be; a statement file holds one statement", where, info.Size(), maxSQLStatementFileBytes)
	}
	return nil
}

// statementFileError is err, an error of finding or opening a statement
// file, said plainly for the two a person meets.
func statementFileError(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("no such file")
	case errors.Is(err, fs.ErrPermission):
		return errors.New("the exporter may not read it (permission denied)")
	}
	return err
}

// sqlRequest is a sql collector's own request keys: the zero SQLConfig of a
// collector built in code whose request has not been validated, which sets
// them (ValidateRequest).
func sqlRequest(c *model.Collector) SQLConfig {
	own, _ := c.Request.Own.(SQLConfig)
	return own
}

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeSQL,
		Fields: []string{
			"username", "password_file", "private_key_file", "tls", "max_response_bytes",
			"allowed_targets", "denied_targets",
		},
		Config:       SQLConfig{},
		KeyRules:     sqlKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout", "username", "password_file", "private_key_file", "connections", "tls"},
		Requester:    sqlRequester{},
		Rows:         true,
		TargetHint:   sqlTargetHint,
		AnswerHint:   "Paste the rows as JSON, a list of objects of each row's columns by name, as the Response of a run shows them",
		Templates:    sqlTemplates,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			if len(sqlRequest(c).Statements) > 0 {
				return unusedParamsIn(c, unused, "its request.database, its statements' args or its label values")
			}
			return unusedParamsIn(c, unused, "its request.database, its args or its label values")
		},
		SchemaRule:         sqlSchemaRule,
		CheckTargetRequest: checkSQLTargetRequest,
		Stage:              "sql",
		CheckFiles:         checkSQLFiles,
		// The statement file, which the configuration is checked against
		// and which a reload reads again, is watched with the files the
		// configuration names (config.namedFiles).
		NamedFiles: func(c *model.Collector) []string {
			if file := sqlRequest(c).StatementFile; file != "" {
				return []string{file}
			}
			return nil
		},
		Follow:           sqlPools.follow,
		CloseConnections: sqlPools.closeAll,
		Connections:      sqlPools.collectorConnections,
		ConnectionLimit:  sqlPools.limitCounts,
		ConnectionPools:  sqlPools.collectorPools,
		Families: []string{
			"http_exporter_sql_connections", "http_exporter_sql_idle_connections", "http_exporter_sql_targets",
			"http_exporter_sql_connects_total", "http_exporter_sql_keepalive_failures_total",
			"http_exporter_sql_limit_waits_total", "http_exporter_sql_limit_failures_total", "http_exporter_sql_rows",
		},
	})
	sqlNamedResults = func(c *model.Collector) bool { return len(sqlRequest(c).Statements) > 0 }
	templateEscapers["database"] = escapeSQLDatabase
	// An arg of a type but text is sent as a value, as one of text is, and
	// checked against its type once filled.
	for _, kind := range sqlArgKinds {
		templateEscapers[kind] = func(_, _, value string) (string, error) { return value, nil }
		templateRenderedRules[kind] = sqlArgRule(kind)
	}
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
	return sqlDriverTable[sqlRequest(c).Driver]
}

func validateSQLRequest(x *model.Collector) error {
	r := &x.Request
	own := sqlRequest(x)
	own.Driver = strings.ToLower(strings.TrimSpace(own.Driver))
	x.Request.Own = own
	d, err := checkSQLDriver(x.Name, own.Driver)
	if err != nil {
		return err
	}
	if len(own.Statements) > 0 {
		if err := checkSQLStatements(own, d); err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
	} else if err := loadSQLStatement(x, d); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if strings.TrimSpace(r.Username) == "" {
		return fmt.Errorf("collector %q request.username is required for request.type sql: the account the statement runs as", x.Name)
	}
	if err := checkSQLAccountKeys(d, r.Username, r.PasswordFile, r.PrivateKeyFile, true); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	switch {
	case d.serviceName && strings.TrimSpace(own.ServiceName) == "":
		return fmt.Errorf("collector %q request.service_name is required for the %s driver: the service of the database the statement runs on", x.Name, d.name)
	case d.serviceName && own.Database != "":
		return fmt.Errorf("collector %q sets request.database, which the %s driver does not take; the database is named by request.service_name", x.Name, d.name)
	case !d.serviceName && own.ServiceName != "":
		return fmt.Errorf("collector %q sets request.service_name, which only the oracle driver takes; name the database with request.database", x.Name)
	}
	if err := checkSQLNameKey("request.service_name", own.ServiceName); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if own.Database != "" && strings.TrimSpace(own.Database) == "" {
		return fmt.Errorf("collector %q request.database is written with blanks alone; name the database, or leave the key out for the account's default", x.Name)
	}
	if _, err := (templateField{"request.database", own.Database, "database"}).parse(); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if err := checkSQLArgs("request.", own.Args, own.ArgTypes); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if err := checkSQLOptions(d, own.Options, r.TLS); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if own.MaxRows < 0 {
		return fmt.Errorf("collector %q request.max_rows must not be negative; 0, or leaving it out, is %d", x.Name, defaultSQLMaxRows)
	}
	if err := checkSQLConnections(own.Connections, false); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	return nil
}

// checkSQLArgs checks the args of a statement, at prefix (request. or
// request.statements[i].), and their types.
func checkSQLArgs(prefix string, args, types []string) error {
	if err := checkSQLArgTypes(prefix, args, types); err != nil {
		return err
	}
	for i := range args {
		field := sqlArgFieldOf(prefix, args, types, i)
		placeholders, err := field.parse()
		if err != nil {
			return err
		}
		if err := checkSQLArgDefaults(field, placeholders); err != nil {
			return err
		}
	}
	return nil
}

// checkSQLStatements checks request.statements: not beside statement,
// statement_file, args or arg_types, which are those of a collector's one
// statement; at most maxSQLStatements entries, each with a name of its own,
// a statement the driver takes, and its args.
func checkSQLStatements(own SQLConfig, d *sqlDriver) error {
	for key, set := range map[string]bool{"statement": own.Statement != "", "statement_file": own.StatementFile != "", "args": len(own.Args) > 0, "arg_types": len(own.ArgTypes) > 0} {
		if set {
			return fmt.Errorf("sets both request.statements and request.%s; with statements, each entry has its statement and its args", key)
		}
	}
	if len(own.Statements) > maxSQLStatements {
		return fmt.Errorf("request.statements has %d entries, more than %d; a collector runs a few statements of one database on one connection, so split them into collectors", len(own.Statements), maxSQLStatements)
	}
	seen := map[string]int{}
	for i, s := range own.Statements {
		where := fmt.Sprintf("request.statements[%d]", i)
		switch {
		case s.Name == "":
			return fmt.Errorf("%s has no name; its rows are under its name in the document", where)
		case !sqlStatementName.MatchString(s.Name):
			return fmt.Errorf("%s.name %q is not letters, digits and _, beginning with a letter or _, as a rule reads it, .%s", where, s.Name, "name")
		}
		if first, ok := seen[s.Name]; ok {
			return fmt.Errorf("%s.name %q is request.statements[%d]'s too; each statement's rows are under a name of their own", where, s.Name, first)
		}
		seen[s.Name] = i
		if strings.TrimSpace(s.Statement) == "" {
			return fmt.Errorf("%s has no statement", where)
		}
		if sqlStatementPlaceholder.MatchString(s.Statement) {
			return fmt.Errorf("%s.statement has a {{param_...}} placeholder, which is not filled there and would be sent as written; write the driver's own placeholder in the statement and the probe's parameter in its args", where)
		}
		if d.statement != nil {
			if err := d.statement(s.Statement); err != nil {
				return fmt.Errorf("%s.statement: %w", where, err)
			}
		}
		if err := checkSQLArgs(where+".", s.Args, s.ArgTypes); err != nil {
			return err
		}
	}
	return nil
}

// loadSQLStatement checks the collector's statement, written in
// request.statement or in the file request.statement_file names, which it
// reads into the collector's own keys, as the statement the collector runs.
// The file is not read when the configuration is loaded without opening
// the files it names, as the target allocator loads it
// (model.Collector.DescriptorFilesUnread): the replicas, which mount it,
// read and check it when they load the bundle. Nor is the statement
// required of the debug page's playground, which never makes a request
// and refuses a key naming a file (model.Collector.RequestNotMade): a copy
// of a collector whose statement is in a file has none.
func loadSQLStatement(x *model.Collector, d *sqlDriver) error {
	own := sqlRequest(x)
	switch {
	case own.StatementFile != "" && own.Statement != "":
		return errors.New("sets both request.statement and request.statement_file; write the statement in one of them")
	case own.StatementFile != "":
		if err := checkSQLStatementFileKey(own.StatementFile); err != nil {
			return err
		}
		if x.DescriptorFilesUnread {
			return nil
		}
		text, err := readSQLStatementFile(own.StatementFile)
		if err != nil {
			return err
		}
		own.statementText = text
		x.Request.Own = own
		if d.statement != nil {
			if err := d.statement(text); err != nil {
				return fmt.Errorf("request.statement_file %s: %w", own.StatementFile, err)
			}
		}
		return nil
	case strings.TrimSpace(own.Statement) == "":
		if x.RequestNotMade && own.Statement == "" {
			return nil
		}
		return errors.New("has no request.statement; a sql collector runs one statement, written in request.statement or in the file request.statement_file names, such as SELECT state, count(*) AS n FROM jobs GROUP BY state")
	}
	if d.statement != nil {
		if err := d.statement(own.Statement); err != nil {
			return err
		}
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
		d = &sqlDriver{name: sqlRequest(c).Driver}
	}
	if err := checkSQLAccountKeys(d, r.Username, r.PasswordFile, r.PrivateKeyFile, false); err != nil {
		return err
	}
	if r.TLS != nil {
		if err := checkTargetTLS(t); err != nil {
			return err
		}
		if err := checkSQLTLS(d, sqlRequest(c).Options, *r.TLS); err != nil {
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
		port := d.port(sqlRequest(c).Options)
		return sqlAddress{host: host, port: port, hostPort: net.JoinHostPort(host, port), account: account}, nil
	}
	host, port := raw, d.port(sqlRequest(c).Options)
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

// sqlTargetHint is what the collectors page and the debug page say of a
// target: the server, host or host:port, the port the driver's when it
// names none, or for a driver whose targets are accounts, the account.
func sqlTargetHint(c *model.Collector) (string, string) {
	d := sqlDriverOf(c)
	switch {
	case d == nil:
		return "Database server", "host or host:port"
	case d.account:
		return "Account", "the account identifier, such as myorg-myaccount"
	}
	if port := d.port(sqlRequest(c).Options); port != "" {
		return "Database server", "host or host:" + port
	}
	return "Database server", "host or host:port"
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
// database, or an Oracle service, as written, escaped as a URL's path is
// (pathURL), its placeholders as they are.
// It holds neither the statement nor a credential.
func sqlLabel(target string, c *model.Collector, _ RequestOverrides) (string, error) {
	address, err := parseSQLTarget(c, target)
	if err != nil {
		return "", err
	}
	database := sqlRequest(c).Database
	if d := sqlDriverOf(c); d != nil && d.serviceName {
		database = sqlRequest(c).ServiceName
	}
	return pathURL(sqlRequest(c).Driver, address.hostPort, "/"+database), nil
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
	own := sqlRequest(c)
	b := sqlBase{
		driver:         own.Driver,
		username:       strings.TrimSpace(r.Username),
		passwordFile:   r.PasswordFile,
		privateKeyFile: r.PrivateKeyFile,
		database:       own.Database,
		serviceName:    own.ServiceName,
		tls:            r.TLS,
		conns:          sqlConnSettingsOf(own.Connections, overrides.SQLConnections),
		policy:         policyOf(c),
		limit:          responseLimit(c),
	}
	var options strings.Builder
	for _, name := range model.SortedKeys(own.Options) {
		options.WriteString(name)
		options.WriteByte('=')
		options.WriteString(own.Options[name])
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
	if !HasPathParams(sqlRequest(c).Database) {
		return sqlRequest(c).Database, nil
	}
	return templateField{"request.database", sqlRequest(c).Database, "database"}.render(overrides.Params)
}

// sqlRuns are the statements a scrape of c runs: its one statement, or
// each of request.statements, in order, with its args filled.
func sqlRuns(c *model.Collector, overrides RequestOverrides) ([]sqlRun, error) {
	own := sqlRequest(c)
	if len(own.Statements) == 0 {
		args, err := sqlArgs("request.", own.Args, own.ArgTypes, overrides)
		if err != nil {
			return nil, err
		}
		return []sqlRun{{statement: sqlStatement(c), args: args}}, nil
	}
	runs := make([]sqlRun, len(own.Statements))
	for i, s := range own.Statements {
		args, err := sqlArgs(fmt.Sprintf("request.statements[%d].", i), s.Args, s.ArgTypes, overrides)
		if err != nil {
			return nil, err
		}
		runs[i] = sqlRun{name: s.Name, statement: s.Statement, args: args}
	}
	return runs, nil
}

// sqlArgs is the values bound to a statement, at prefix, their
// placeholders filled, each of the type its arg_types gives it.
func sqlArgs(prefix string, list, types []string, overrides RequestOverrides) ([]any, error) {
	args := make([]any, len(list))
	for i, arg := range list {
		field := sqlArgFieldOf(prefix, list, types, i)
		value := arg
		if HasPathParams(arg) {
			var err error
			if value, err = field.render(overrides.Params); err != nil {
				return nil, err
			}
		}
		typed, err := sqlTypedArg(field.where, sqlArgTypeOf(types, i), value)
		if err != nil {
			return nil, err
		}
		args[i] = typed
	}
	return args, nil
}

// The types an arg is bound as (request.arg_types): text, the default, as
// written; int, a whole number of 64 bits; float, a floating-point number
// of 64 bits; bool, true or false in any case. Each is handed to the
// driver as Go's own type of it, which the driver binds as the database's:
// MySQL's LIMIT ? takes no text before 8.0.22, and a comparison of a number
// with text may compare them as text.
const (
	sqlArgText  = "text"
	sqlArgInt   = "int"
	sqlArgFloat = "float"
	sqlArgBool  = "bool"
)

// sqlArgTypes are the types an arg may be bound as.
var sqlArgTypes = []string{sqlArgText, sqlArgInt, sqlArgFloat, sqlArgBool}

// sqlArgKinds are the kinds of templated field of the args of each type
// but text, whose kind is arg: a value that a placeholder fills is checked
// against its type when it is filled (sqlArgRule), so that a probe whose
// parameter is no number is answered 400 before the server is asked, as a
// static target's is refused when its file loads.
var sqlArgKinds = map[string]string{sqlArgInt: "sql_int", sqlArgFloat: "sql_float", sqlArgBool: "sql_bool"}

// sqlArgTypeOf is the type the arg at i is bound as, of types.
func sqlArgTypeOf(types []string, i int) string {
	if i < len(types) && types[i] != "" {
		return types[i]
	}
	return sqlArgText
}

// sqlArgFieldOf is the arg at i of a statement's args, at prefix, as a
// templated field, of the kind its type takes.
func sqlArgFieldOf(prefix string, args, types []string, i int) templateField {
	kind := "arg"
	if k, ok := sqlArgKinds[sqlArgTypeOf(types, i)]; ok {
		kind = k
	}
	return templateField{fmt.Sprintf("%sargs[%d]", prefix, i), args[i], kind}
}

// sqlTypedArg is value, the arg at where, as its type takes it.
func sqlTypedArg(where, typ, value string) (any, error) {
	switch typ {
	case sqlArgInt:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s is %q, which is no whole number from -9223372036854775808 to 9223372036854775807, written in decimal digits with no blanks, as its type in request.arg_types, int, takes", where, value)
		}
		return n, nil
	case sqlArgFloat:
		f, err := strconv.ParseFloat(value, 64)
		// A hexadecimal float, 0x1p-2, and one with underscores, which Go
		// reads, are written no way a database does; Inf and NaN are no
		// finite number, and a number past float64's range fails to parse.
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || strings.ContainsAny(value, "_xXpP") {
			return nil, fmt.Errorf("%s is %q, which is no finite decimal number, such as 0.25 or 1e-3, as its type in request.arg_types, float, takes", where, value)
		}
		return f, nil
	case sqlArgBool:
		switch strings.ToLower(value) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, fmt.Errorf("%s is %q, which is neither true nor false, as its type in request.arg_types, bool, takes", where, value)
	}
	return value, nil
}

// checkSQLArgTypes refuses arg_types, at prefix, that do not give a type
// for each arg, in order, or that name a type there is not.
func checkSQLArgTypes(prefix string, args, types []string) error {
	if len(types) == 0 {
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("sets %sarg_types without %sargs; the types are those of the args, by their places", prefix, prefix)
	}
	if len(types) != len(args) {
		return fmt.Errorf("%sarg_types has %d types for %d args; give a type for each arg, in their order, text for one bound as text", prefix, len(types), len(args))
	}
	for i, typ := range types {
		if !slices.Contains(sqlArgTypes, typ) {
			return fmt.Errorf("%sarg_types[%d] is %q; want one of %s, in lower case", prefix, i, typ, strings.Join(sqlArgTypes, ", "))
		}
	}
	return nil
}

// checkSQLArgDefaults refuses, when the configuration loads, an arg that is
// not of its type as it is bound when no probe parameter fills it: one
// with no placeholder, and one whose every placeholder has a default, which
// a probe that gives none of them binds.
func checkSQLArgDefaults(field templateField, placeholders []pathPlaceholder) error {
	if field.kind == "arg" {
		return nil
	}
	for _, p := range placeholders {
		if !p.HasDefault {
			return nil
		}
	}
	if len(placeholders) == 0 {
		return sqlArgRule(field.kind)(field.where, nil, field.text)
	}
	// Filling it checks it against its type (templateRenderedRules).
	field.where += ", as its placeholders' defaults fill it,"
	_, err := field.renderParsed(placeholders, nil)
	return err
}

// sqlArgRule is the rule a filled arg of kind is checked by
// (templateRenderedRules).
func sqlArgRule(kind string) func(where string, _ []pathPlaceholder, value string) error {
	typ := ""
	for t, k := range sqlArgKinds {
		if k == kind {
			typ = t
		}
	}
	return func(where string, _ []pathPlaceholder, value string) error {
		_, err := sqlTypedArg(where, typ, value)
		return err
	}
}

// sqlMaxRows is the most rows a statement of c may return.
func sqlMaxRows(c *model.Collector) int {
	if sqlRequest(c).MaxRows > 0 {
		return sqlRequest(c).MaxRows
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
	runs, err := sqlRuns(c, overrides)
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
	result, err := sqlPools.query(ctx, key, c, runs)
	if err != nil {
		return nil, onSQLHost(err, address.hostPort)
	}
	return &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       result.shown,
		Rows:       result.rows,
		NamedRows:  result.named,
		Columns:    result.columns,
		Target:     target,
		Collector:  c.Name,
		Duration:   time.Since(start),
	}, nil
}

// onSQLHost names the server in a failed statement's error, unless the
// error names it already where a probe's caller reads it too: a server's
// message that names it is withheld from the caller (sqlServerAnswer).
func onSQLHost(err error, hostPort string) error {
	if strings.Contains(model.CallerText(err), hostPort) {
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
		if _, _, err := sqlTLSConfig(d, &base, sqlRequest(c).Options, "localhost"); err != nil {
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
// does (validateSQLRequest): driver and username, and one of statement,
// statement_file and statements, each written with something other than ""
// or an empty list, and with statements no args or arg_types;
// for the oracle driver, when it is
// built, service_name; and for the snowflake driver, when it is built,
// password_file or private_key_file; each driver named as the exporter
// matches it (SchemaCaselessOneOf).
func sqlSchemaRule() map[string]any {
	keys := []string{"driver", "username"}
	properties := map[string]any{}
	for _, key := range keys {
		properties[key] = SchemaWrittenKey(key)["properties"].(map[string]any)[key]
	}
	rule := map[string]any{"required": keys, "properties": properties,
		// One of the two, written with something other than "", which is
		// the key left out, as the exporter reads it: statement written ""
		// beside statement_file is statement_file alone.
		"oneOf": []any{SchemaWrittenKey("statement"), SchemaWrittenKey("statement_file"),
			map[string]any{"required": []string{"statements"}, "properties": map[string]any{"statements": map[string]any{"minItems": 1}}}}}
	// statements is a collector's statements, beside which those of its
	// one statement, args and arg_types, are refused, as the exporter
	// refuses them.
	rule["allOf"] = []any{map[string]any{
		"if":   map[string]any{"required": []string{"statements"}, "properties": map[string]any{"statements": map[string]any{"minItems": 1}}},
		"then": map[string]any{"not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"args"}, "properties": map[string]any{"args": map[string]any{"minItems": 1}}}, map[string]any{"required": []string{"arg_types"}, "properties": map[string]any{"arg_types": map[string]any{"minItems": 1}}}}}},
	}}
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
	rule["allOf"] = append(rule["allOf"].([]any), conditions...)
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
	if HasPathParams(sqlRequest(c).Database) {
		out = append(out, templateField{"request.database", sqlRequest(c).Database, "database"})
	}
	own := sqlRequest(c)
	for i, arg := range own.Args {
		if HasPathParams(arg) {
			out = append(out, sqlArgFieldOf("request.", own.Args, own.ArgTypes, i))
		}
	}
	for j, s := range own.Statements {
		for i, arg := range s.Args {
			if HasPathParams(arg) {
				out = append(out, sqlArgFieldOf(fmt.Sprintf("request.statements[%d].", j), s.Args, s.ArgTypes, i))
			}
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

// sqlKeyRules is what the configuration schema says of a sql
// collector's own keys.
func sqlKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"driver":                          {"type": "string", "pattern": SchemaCaselessOneOf(BuiltSQLDrivers()...), "description": "sql, required: the database's driver, in any case and with blanks around it or not, as the exporter matches it. mysql, postgres and sqlserver are built in; oracle, clickhouse, snowflake and vertica only in a build with SQL_DRIVERS naming them. See docs/SQL.md."},
		"database":                        {"not": SchemaOnlyBlanks(), "description": "sql: the database the statement runs on; the account's default when left out. Not for oracle, which names it by service_name. May contain {{param_name}} placeholders, filled with letters, digits and _ - . $ only."},
		"statement_file":                  {"not": SchemaOnlyBlanks(), "pattern": "^/", "description": "sql: the absolute path of a file holding the one statement run, in place of statement, which it may not be written beside: read when the configuration loads, and again at each reload, which an edit of it triggers with --config.watch. A regular UTF-8 file of at most 1MiB, the statement with no {{param_...}} placeholder; a byte order mark and the blanks around the statement are taken off. Not read by the target allocator: mount it on every replica at the same path. See docs/SQL.md#the-statement."},
		"statement":                       {"not": SchemaOnlyBlanks(), "description": "sql, required unless statement_file names the file of it: the one statement run, written with the driver's own placeholders for args ($1 for postgres, ? for mysql, clickhouse, snowflake and vertica, @p1 for sqlserver, :1 for oracle). Its rows are read by a csv transform by column name, and by jq, yq and python as a list of objects. See docs/SQL.md#the-statement."},
		"args":                            {"description": "sql: the values of the statement's placeholders, in order, as text; sent as values, never as SQL (bound, or written in quoted as literals by the clickhouse driver). May contain {{param_name}} placeholders. Bound as text unless arg_types says otherwise."},
		"arg_types":                       {"items": map[string]any{"enum": sqlArgTypes}, "description": "sql: the type each of args is bound as, one for each, in their order: text (as written), int (a whole number of 64 bits), float (a finite number) or bool (true or false, in any case). Left out, every arg is text. A value that is not of its type fails: when the configuration loads for an arg with no placeholder, or whose placeholders all have defaults; and a probe's parameter answered 400, a static target's params refused when its file loads. See docs/SQL.md#typed-args."},
		"options":                         {"description": "sql: the driver's settings, from a short list for each: postgres sslmode and application_name; mysql tls; sqlserver encrypt; oracle ssl; clickhouse protocol, compress and secure; snowflake warehouse, role and schema; vertica tlsmode. See docs/SQL.md#drivers."},
		"service_name":                    {"not": SchemaOnlyBlanks(), "description": "sql, required for the oracle driver and for no other: the service of the database."},
		"max_rows":                        {"minimum": 0, "description": "sql: the most rows the statement may return; one more fails the scrape. Defaults to 10000."},
		"statements":                      {"maxItems": maxSQLStatements, "description": "sql: several statements, in place of statement, statement_file, args and arg_types, run in order on one connection, each in a transaction of its own as the one statement is: the document is an object of each one's rows, a list of objects, under its name, which jq, yq and python read, and not a csv transform. At most 16. See docs/SQL.md#several-statements."},
		"statements[]":                    {"required": []string{"name", "statement"}},
		"statements[].name":               {"pattern": sqlStatementName.String(), "description": "The name the statement's rows are under in the document: letters, digits and _, not starting with a digit, unique among the collector's statements."},
		"statements[].statement":          {"minLength": 1, "not": SchemaOnlyBlanks(), "description": "The statement, with the driver's own placeholders for its args, as request.statement is written."},
		"statements[].args":               {"description": "The values of the statement's placeholders, in order, as request.args are of the one statement. May contain {{param_name}} placeholders."},
		"statements[].arg_types":          {"items": map[string]any{"enum": sqlArgTypes}, "description": "The type each of the statement's args is bound as, as request.arg_types."},
		"connections":                     {"description": "sql: how the collector keeps connections to a target, a server or Snowflake account, in a pool for each of its databases and accounts, the limits counting across them. A static target may set each key but max_targets for itself. See docs/SQL.md#connections."},
		"connections.max_per_target":      {"minimum": 0, "description": "The most connections open to one target at once, across its databases and accounts, keepalive pings counted; a scrape beyond waits within its deadline for one. It bounds the target's pools, one for each database and account, too. Defaults to 2; 0 sets no limit to the connections and keeps at most 64 pools."},
		"connections.max_idle_per_target": {"minimum": 0, "description": "How many connections to a target are kept idle between scrapes, across its databases and accounts. Defaults to 1; 0 keeps none."},
		"connections.max_targets":         {"minimum": 0, "description": "The most targets, servers or Snowflake accounts, the collector keeps connections to, whatever databases and accounts it reaches there; at the limit the pools of the target used longest ago are closed. Defaults to 50; 0 sets no limit."},
		"connections.idle_timeout":        {"description": "How long an idle connection is kept. Defaults to 5m; 0 keeps it as long as the collector is configured."},
		"connections.max_lifetime":        {"description": "How long a connection is used before it is closed and made again. Defaults to 1h; 0 uses it as long as it works."},
		"connections.keepalive":           {"description": "How a kept connection is found broken."},
		"connections.keepalive.interval":  {"not": SchemaLeastDuration(sqlMinKeepalive, true), "description": "How often: the TCP keepalive interval, or how often the idle connections are pinged. At least 1s; defaults to 30s; 0 checks none."},
		"connections.keepalive.check":     {"enum": SchemaOptionalEnum([]string{"tcp", "ping"}), "description": "tcp, the default: TCP keepalives on every connection. ping: every idle connection is pinged every interval."},
	}
}
