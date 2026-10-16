//go:build (!select_request_types || request_type_sql) && sql_driver_snowflake

package fetch

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/snowflakedb/gosnowflake"
)

// The snowflake driver is snowflakedb/gosnowflake: built in only with the
// sql_driver_snowflake tag (SQL_DRIVERS=snowflake), since it brings the
// AWS and Azure SDKs and Apache Arrow with it, and with gosnowflake's
// minicore_disabled, which tools/request-type-tags.sh gives with it, so that
// its init writes no native library to disk to load. A target is the account
// identifier, such as myorg-myaccount, reached at
// <account>.snowflakecomputing.com over HTTPS, always with TLS. The account
// authenticates with password_file, or with private_key_file, an
// unencrypted PKCS#8 RSA key, by key-pair authentication. Snowflake has no
// read-only transaction (the driver refuses one): the role must be one that
// can only read. The driver aborts the statement at the server when the
// scrape's deadline comes.
//
// The driver's platform detection is turned off before any connection can
// start it: on its first connection it would otherwise ask the EC2, Azure and
// GCP metadata services and AWS STS, with the ambient AWS credentials, which
// platform it runs on, outside the exporter's dialer and policy. It reads the
// switch, an environment variable, when it detects; it has no Config field.
// The driver's log goes to the exporter's logger (snowflakeLog), at the
// driver's level of error, which its own init lowers to fatal, logging
// nothing, when GITHUB_ACTIONS is set: the exporter logs the same wherever
// it runs.
func init() {
	_ = os.Setenv("SNOWFLAKE_DISABLE_PLATFORM_DETECTION", "true")
	driverLog := gosnowflake.GetLogger() //nolint:staticcheck // SA1019: still the driver's only way to its log
	driverLog.SetOutput(snowflakeLog{})
	_ = driverLog.SetLogLevel("error")
	registerSQLDriver(&sqlDriver{
		name:      SQLDriverSnowflake,
		port:      func(map[string]string) string { return "443" },
		options:   map[string][]string{"warehouse": nil, "role": nil, "schema": nil},
		keyPair:   true,
		account:   true,
		host:      func(account string) string { return account + ".snowflakecomputing.com" },
		connect:   connectSnowflake,
		run:       snowflakeRun,
		rowsError: snowflakeRowsError,
	})
}

// snowflakeConfig is the configuration of a connection made with spec.
func snowflakeConfig(spec *sqlConnectSpec) gosnowflake.Config {
	config := gosnowflake.Config{
		Account:     spec.account,
		Host:        spec.host,
		Port:        443,
		Protocol:    "https",
		User:        spec.username,
		Password:    spec.password,
		Database:    spec.database,
		Schema:      spec.options["schema"],
		Warehouse:   spec.options["warehouse"],
		Role:        spec.options["role"],
		Application: spec.appName,
		// Every request through the exporter's dialer and TLS settings,
		// and none to another host (snowflakeTransport).
		Transporter:      &snowflakeTransport{host: spec.host, base: spec.httpTransport()},
		DisableTelemetry: true, //nolint:staticcheck // SA1019: the session parameter said to replace it does not exist yet, and the driver's telemetry is not wanted
		// Nothing is cached on the exporter's disk or in a keyring.
		ClientStoreTemporaryCredential: gosnowflake.ConfigBoolFalse, //nolint:staticcheck // SA1019: still the driver's only switch for it
	}
	if spec.privateKey != nil {
		config.Authenticator = gosnowflake.AuthTypeJwt
		config.PrivateKey = spec.privateKey
		config.Password = ""
	}
	return config
}

// connectSnowflake connects with spec, failing with the policy's refusal
// when the exporter's dialer refused the connection to the account
// (snowflakeTransport).
func connectSnowflake(ctx context.Context, spec *sqlConnectSpec) (driver.Conn, error) {
	ctx, refusal := withSnowflakeRefusal(ctx)
	conn, err := gosnowflake.NewConnector(gosnowflake.SnowflakeDriver{}, snowflakeConfig(spec)).Connect(ctx)
	if refused := refusal.Load(); err != nil && refused != nil {
		return nil, refused
	}
	return conn, err
}

// snowflakeRun runs a statement as sqlPlain does, failing with the policy's
// refusal when the exporter's dialer refused a new connection to the account
// on the way (snowflakeTransport).
func snowflakeRun(ctx context.Context, conn *sql.Conn, statement string, args []any, timeout time.Duration) (*sql.Rows, func(), error) {
	ctx, refusal := withSnowflakeRefusal(ctx)
	rows, done, err := sqlPlain(ctx, conn, statement, args, timeout)
	if refused := refusal.Load(); err != nil && refused != nil {
		return nil, nil, refused
	}
	return rows, done, err
}

// snowflakeRefusalKey is the context key of where snowflakeTransport puts
// the policy's refusal of a connection made for a request with the context.
type snowflakeRefusalKey struct{}

// withSnowflakeRefusal is ctx with a place for the policy's refusal of a
// connection made for a request with it, and that place.
func withSnowflakeRefusal(ctx context.Context) (context.Context, *atomic.Pointer[TargetRefusedError]) {
	refusal := &atomic.Pointer[TargetRefusedError]{}
	return context.WithValue(ctx, snowflakeRefusalKey{}, refusal), refusal
}

// snowflakeTransport is the transport of a snowflake connection: a request
// to a host other than the account's, and one whose connection to the
// account the exporter's dialer refused, is answered with 403 Forbidden. The
// driver retries every request that fails without a response, until the
// scrape's deadline, and then reports only the deadline, while a 403 it does
// not retry (gosnowflake retry.go, isRetryableError and isRetryableStatus),
// beyond a few immediate re-asks of its chunk downloader.
// The driver requests a host other than the account's for each chunk of a
// result after the first, which the server leaves at a presigned cloud
// storage URL: that request is answered without a connection, and the scrape
// fails at once with what snowflakeRowsError says. A refusal of the
// connection to the account is put where the request's context says
// (withSnowflakeRefusal), for the connection or statement that made it to
// fail with it at once; another failure to connect to the account the driver
// retries as before.
type snowflakeTransport struct {
	host string
	base *http.Transport
}

func (t *snowflakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.EqualFold(req.URL.Hostname(), t.host) {
		resp, err := t.base.RoundTrip(req)
		var refused *TargetRefusedError
		if err == nil || !errors.As(err, &refused) {
			return resp, err
		}
		if refusal, ok := req.Context().Value(snowflakeRefusalKey{}).(*atomic.Pointer[TargetRefusedError]); ok {
			refusal.Store(refused)
		}
		return snowflakeForbidden(req), nil
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return snowflakeForbidden(req), nil
}

// snowflakeForbidden is a 403 Forbidden answer to req.
func snowflakeForbidden(req *http.Request) *http.Response {
	return &http.Response{
		Status: "403 Forbidden", StatusCode: http.StatusForbidden,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{}, Body: http.NoBody, Request: req,
	}
}

// CloseIdleConnections closes the base transport's idle connections, as the
// driver's http.Client asks of its transport.
func (t *snowflakeTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

// errSnowflakeChunks is the failure of a statement whose result came in more
// than one chunk.
var errSnowflakeChunks = errors.New("the statement's result came from Snowflake in more than one chunk, and the exporter does not fetch the chunks after the first, which the driver would read from a cloud storage URL, a host other than the account's; return fewer rows or columns, with WHERE, GROUP BY or LIMIT, so that the whole result comes in one chunk")

// snowflakeRowsError puts the driver's failure to fetch a later chunk of a
// result, which snowflakeTransport refuses, in the operator's words. Every
// chunk after the first is at a cloud storage URL, so every such failure is
// that refusal.
func snowflakeRowsError(err error) error {
	var sfErr *gosnowflake.SnowflakeError
	if errors.As(err, &sfErr) && sfErr.Number == gosnowflake.ErrFailedToGetChunk {
		return errSnowflakeChunks
	}
	return err
}

// snowflakeLog hands the driver's log to the exporter's logger, slog's
// default, which main makes the JSON lines every other line is: the driver
// writes its own (errors, at the level it sets itself) to stderr, as logfmt
// lines its formatter makes after taking passwords and tokens out of them,
// which this reads back. A warning is a warning, an error, fatal or panic
// message an error, an info message info, and the rest debug; a line that is
// not the formatter's is logged whole, at info. The driver's error for each
// try at a later chunk of a result, which snowflakeTransport refuses, is
// debug (snowflakeChunkLine): the scrape's own failure says what happened.
// A client configuration file the driver finds on its first connection
// (SF_CLIENT_CONFIG_FILE, or sf_client_config.json beside the executable or
// in the home directory) replaces this with a log of the driver's own,
// where the file says.
type snowflakeLog struct{}

func (snowflakeLog) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		level, msg := snowflakeLogLine(line)
		slog.Default().Log(context.Background(), level, msg, "sql_driver", SQLDriverSnowflake)
	}
	return len(p), nil
}

// snowflakeLogLine is the level and the message of a line of the driver's
// log: key=value pairs, a value quoted as Go quotes a string when it has
// anything but letters, digits and -._/@^+.
func snowflakeLogLine(line string) (slog.Level, string) {
	fields := map[string]string{}
	for rest := strings.TrimSpace(line); rest != ""; rest = strings.TrimLeft(rest, " ") {
		key, after, found := strings.Cut(rest, "=")
		if !found || key == "" || strings.ContainsAny(key, " \"") {
			return slog.LevelInfo, line
		}
		var value string
		if strings.HasPrefix(after, `"`) {
			quoted, err := strconv.QuotedPrefix(after)
			if err != nil {
				return slog.LevelInfo, line
			}
			if value, err = strconv.Unquote(quoted); err != nil {
				return slog.LevelInfo, line
			}
			after = after[len(quoted):]
		} else {
			value, after, _ = strings.Cut(after, " ")
		}
		fields[key] = value
		rest = after
	}
	level, ok := fields["level"]
	if !ok {
		return slog.LevelInfo, line
	}
	switch level {
	case "warning":
		return slog.LevelWarn, fields["msg"]
	case "error":
		if snowflakeChunkLine(fields["msg"]) {
			return slog.LevelDebug, fields["msg"]
		}
		return slog.LevelError, fields["msg"]
	case "fatal", "panic":
		return slog.LevelError, fields["msg"]
	case "info":
		return slog.LevelInfo, fields["msg"]
	}
	return slog.LevelDebug, fields["msg"]
}

// snowflakeChunkFailure is what the driver's error for a try at a later
// chunk ends with: the error it makes of the answer that is not 200 OK
// (chunk_downloader.go downloadChunkHelper, ErrFailedToGetChunk, as
// SnowflakeError.Error writes it).
var snowflakeChunkFailure = fmt.Sprintf(", err: %06d (", gosnowflake.ErrFailedToGetChunk)

// snowflakeChunkLine says msg is the driver's error for a try at a later
// chunk of a result that was not answered 200 OK, which it logs once for
// each try, the first and its five more (chunk_downloader.go downloadChunk,
// checkErrorRetry; rows.go maxChunkDownloaderErrorCounter), with the chunk's
// presigned URL. snowflakeTransport answers every such try, the chunks being
// on another host, so these are its refusals, which the scrape's failure
// says in the operator's words (snowflakeRowsError).
func snowflakeChunkLine(msg string) bool {
	rest, ok := strings.CutPrefix(msg, "failed to extract HTTP response body. URL: ")
	return ok && strings.Contains(rest, snowflakeChunkFailure)
}

// httpTransport is the transport a driver that speaks HTTP is given: every
// connection through spec's dialer, which holds it to the policy and sets its
// TCP keepalives, with spec's TLS configuration and no proxy.
func (spec *sqlConnectSpec) httpTransport() *http.Transport {
	return &http.Transport{
		DialContext:         spec.dial,
		TLSClientConfig:     spec.tls,
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     90 * time.Second,
	}
}
