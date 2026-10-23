//go:build (!select_request_types || request_type_sql) && sql_driver_snowflake

package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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
		name:    SQLDriverSnowflake,
		port:    func(map[string]string) string { return "443" },
		options: map[string][]string{"warehouse": nil, "role": nil, "schema": nil},
		keyPair: true,
		account: true,
		host:    func(account string) string { return account + ".snowflakecomputing.com" },
		connect: connectSnowflake,
		run:     snowflakeRun,
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
		Transporter:      &snowflakeTransport{host: spec.host, base: spec.httpTransport(), limit: spec.limit},
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
	ctx, notes := withSnowflakeNotes(ctx)
	conn, err := gosnowflake.NewConnector(gosnowflake.SnowflakeDriver{}, snowflakeConfig(spec)).Connect(ctx)
	if refused := notes.refused.Load(); err != nil && refused != nil {
		return nil, refused
	}
	return conn, err
}

// snowflakeRun runs a statement as sqlPlain does, failing with the policy's
// refusal when the exporter's dialer refused a new connection to the account
// on the way, with the response limit's failure when the account's answer
// was larger than the limit, and with errSnowflakeChunks when the
// statement's result came in more than one chunk (snowflakeTransport).
func snowflakeRun(ctx context.Context, conn *sql.Conn, statement string, args []any, timeout time.Duration) (*sql.Rows, func(), error) {
	ctx, notes := withSnowflakeNotes(ctx)
	rows, done, err := sqlPlain(ctx, conn, statement, args, timeout)
	if refused := notes.refused.Load(); err != nil && refused != nil {
		return nil, nil, refused
	}
	if tooLarge := notes.tooLarge.Load(); err != nil && tooLarge != nil {
		return nil, nil, *tooLarge
	}
	if err != nil && notes.chunked.Load() {
		return nil, nil, errSnowflakeChunks
	}
	return rows, done, err
}

// snowflakeNotesKey is the context key of snowflakeNotes.
type snowflakeNotesKey struct{}

// snowflakeNotes is where snowflakeTransport notes what it refused of a
// request made with a context: the policy's refusal of a connection made for
// it, an answer larger than the response limit (the limit's failure), and a
// result in more than one chunk.
type snowflakeNotes struct {
	refused  atomic.Pointer[TargetRefusedError]
	tooLarge atomic.Pointer[error]
	chunked  atomic.Bool
}

// withSnowflakeNotes is ctx with a place for snowflakeNotes of the requests
// made with it, and that place.
func withSnowflakeNotes(ctx context.Context) (context.Context, *snowflakeNotes) {
	notes := &snowflakeNotes{}
	return context.WithValue(ctx, snowflakeNotesKey{}, notes), notes
}

// snowflakeTransport is the transport of a snowflake connection: a request
// to a host other than the account's, and one whose connection to the
// account the exporter's dialer refused, are answered with 403 Forbidden,
// and one the account answered with a result larger than the response limit
// or in more than one chunk with the statement's failure
// (snowflakeFailedAnswer). The driver retries every
// request that fails without a response, until the scrape's deadline, and
// then reports only the deadline, while a 403 it does not retry (gosnowflake
// retry.go, isRetryableError and isRetryableStatus). A refusal of the
// connection to the account is noted where the request's context says
// (withSnowflakeNotes), for the connection or statement that made it to fail
// with it at once; another failure to connect to the account the driver
// retries as before.
//
// The server leaves each chunk of a result after the first at a presigned
// cloud storage URL, a host other than the account's, which the exporter
// does not connect to. The driver would start reading those chunks as soon
// as the first came, and its reader can wait for a chunk forever, past the
// scrape's deadline: a download that fails signals the reader without the
// lock the reader holds from its look for a failure to its wait, so the
// signal is lost when it comes in between (gosnowflake chunk_downloader.go,
// downloadChunk and snowflakeChunkDownloader.next), and the statement keeps
// its connection and its place at max_per_target, the scrape abandoned at
// its deadline. So the driver is never given a result with chunks: the
// account's answer to a statement, or to the driver's ask for a statement's
// result (a path under /queries/), is read whole, the result's first chunk
// with it (held twice for the moment, that copy and the driver's), and when
// it names more chunks it is replaced by the statement's failure, noted for
// snowflakeRun to fail the statement with errSnowflakeChunks.
//
// That answer is read to limit, the collector's response limit (0 for
// none), and no further (snowflakeRead): one larger, or gzipped and larger
// once decompressed, is replaced by the statement's failure too, noted for
// snowflakeRun to fail with the limit's failure, so that what is held of an
// answer is at most the limit twice, the copy and the driver's, rather than
// whatever the account sends or a gzip bomb decompresses to.
type snowflakeTransport struct {
	host  string
	base  *http.Transport
	limit int64
}

func (t *snowflakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Hostname(), t.host) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return snowflakeForbidden(req), nil
	}
	notes, _ := req.Context().Value(snowflakeNotesKey{}).(*snowflakeNotes)
	resp, err := t.base.RoundTrip(req)
	var refused *TargetRefusedError
	if err != nil && errors.As(err, &refused) {
		if notes != nil {
			notes.refused.Store(refused)
		}
		return snowflakeForbidden(req), nil
	}
	if err != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(req.URL.Path, "/queries/") {
		return resp, err
	}
	body, err := snowflakeRead(resp, t.limit)
	_ = resp.Body.Close()
	if errors.Is(err, errSnowflakeTooLarge) {
		if notes != nil {
			tooLarge := snowflakeLimitError(t.limit)
			notes.tooLarge.Store(&tooLarge)
		}
		return snowflakeFailedAnswer(req, snowflakeTooLargeError), nil
	}
	if err != nil {
		return nil, err
	}
	if snowflakeChunked(body) {
		if notes != nil {
			notes.chunked.Store(true)
		}
		return snowflakeFailedAnswer(req, snowflakeChunkedError), nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

// errSnowflakeTooLarge is snowflakeRead's failure for an answer larger than
// the limit.
var errSnowflakeTooLarge = errors.New("the answer is larger than the response limit")

// snowflakeRead reads resp's body, an answer of the account, holding it to
// limit bytes (none at 0), and failing with errSnowflakeTooLarge, having
// read no more than limit and a byte, when it is larger: at once when its
// Content-Length says so. A gzipped body (Content-Encoding gzip, which the
// account sends when the driver asks for it, while Go's transport takes off
// the gzip it asked for itself before this) is decompressed, to limit and a
// byte at most, and what it read is the decompressed answer, which replaces
// the gzipped one: the gzipped copy is let go of as soon as it is
// decompressed, and the driver, which would decompress it again on its own,
// reads it as it reads a plain answer. A body that is not gzip after all is
// given as it came, for the driver to fail on as before.
func snowflakeRead(resp *http.Response, limit int64) ([]byte, error) {
	if limit > 0 && resp.ContentLength > limit {
		return nil, errSnowflakeTooLarge
	}
	body, err := snowflakeReadAll(resp.Body, limit)
	if err != nil || !strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		return body, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body, nil //nolint:nilerr // not gzip after all: the driver fails on it as it came
	}
	plain, err := snowflakeReadAll(reader, limit)
	if errors.Is(err, errSnowflakeTooLarge) {
		return nil, err
	}
	if err != nil {
		return body, nil //nolint:nilerr // a broken gzip: the driver fails on it as it came
	}
	resp.Header.Del("Content-Encoding")
	resp.Uncompressed = true
	return plain, nil
}

// snowflakeReadAll is io.ReadAll of r held to limit bytes (none at 0): one
// byte past it fails with errSnowflakeTooLarge, without reading further.
func snowflakeReadAll(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(body)) > limit {
		return nil, errSnowflakeTooLarge
	}
	return body, err
}

// snowflakeLimitError is the failure of a statement whose answer from the
// account was larger than limit, the response limit, in the words of the
// sql type's other limit failures.
func snowflakeLimitError(limit int64) error {
	return model.MarkError(model.Errorf("the statement's answer from Snowflake was larger than the response limit of %d bytes (decompressed, when it came gzipped), and was refused; return fewer rows or columns, with WHERE, GROUP BY or LIMIT, or raise request.max_response_bytes or limits.max_response_bytes", model.Size(limit)), model.ErrLimitExceeded)
}

// snowflakeChunked says body, an answer of the account, is a result that
// names chunks after the first. An answer that is not JSON, or not a
// result, is not: the driver fails on it, or reads it, as before.
func snowflakeChunked(body []byte) bool {
	var answer struct {
		Data struct {
			Chunks []json.RawMessage `json:"chunks"`
		} `json:"data"`
	}
	return json.Unmarshal(body, &answer) == nil && len(answer.Data.Chunks) > 0
}

// snowflakeForbidden is a 403 Forbidden answer to req.
func snowflakeForbidden(req *http.Request) *http.Response {
	return &http.Response{
		Status: "403 Forbidden", StatusCode: http.StatusForbidden,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{}, Body: http.NoBody, Request: req,
	}
}

// snowflakeChunkedError is the failure snowflakeFailedAnswer gives the
// driver for a result in more than one chunk, which the driver returns and
// logs as it is, "error: " before it (snowflakeQuietLines).
var snowflakeChunkedError = &gosnowflake.SnowflakeError{Number: gosnowflake.ErrFailedToGetChunk, SQLState: gosnowflake.SQLStateConnectionFailure, Message: errSnowflakeChunks.Error()}

// snowflakeTooLargeError is the failure snowflakeFailedAnswer gives the
// driver for an answer larger than the response limit. Its message is the
// same whatever the limit: snowflakeRun fails the statement with the limit's
// own failure, which names it.
var snowflakeTooLargeError = &gosnowflake.SnowflakeError{Number: gosnowflake.ErrFailedToGetChunk, SQLState: gosnowflake.SQLStateConnectionFailure, Message: "the statement's result came from Snowflake larger than the exporter's response limit"}

// snowflakeQuietLines are the messages of the driver's log lines for
// snowflakeChunkedError and snowflakeTooLargeError, which the driver logs at
// error for every statement that fails (connection.go
// queryContextInternal), and which snowflakeLog logs at debug: the scrape's
// own failure says what happened.
var snowflakeQuietLines = map[string]bool{
	"error: " + snowflakeChunkedError.Error():  true,
	"error: " + snowflakeTooLargeError.Error(): true,
}

// snowflakeFailedAnswer is the account's answer to req failing the
// statement with failure, in place of a result in more than one chunk or
// larger than the response limit. It is an answer the driver neither
// retries nor reads a result from; a 403 would serve as well but for the
// driver's line about it, the same as for a refused connection.
func snowflakeFailedAnswer(req *http.Request, failure *gosnowflake.SnowflakeError) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"success": false,
		"code":    fmt.Sprintf("%06d", failure.Number),
		"message": failure.Message,
		"data":    map[string]any{"sqlState": failure.SQLState},
	})
	return &http.Response{
		Status: "200 OK", StatusCode: http.StatusOK,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// CloseIdleConnections closes the base transport's idle connections, as the
// driver's http.Client asks of its transport.
func (t *snowflakeTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

// errSnowflakeChunks is the failure of a statement whose result came in more
// than one chunk (snowflakeTransport).
var errSnowflakeChunks = errors.New("the statement's result came from Snowflake in more than one chunk, and the exporter does not fetch the chunks after the first, which the driver would read from a cloud storage URL, a host other than the account's; return fewer rows or columns, with WHERE, GROUP BY or LIMIT, so that the whole result comes in one chunk")

// snowflakeLog hands the driver's log to the exporter's logger, slog's
// default, which main makes the JSON lines every other line is: the driver
// writes its own (errors, at the level it sets itself) to stderr, as logfmt
// lines its formatter makes after taking passwords and tokens out of them,
// which this reads back. A warning is a warning, an error, fatal or panic
// message an error, an info message info, and the rest debug; a line that is
// not the formatter's is logged whole, at info. The driver's error for a
// result in more than one chunk, or larger than the response limit, is debug
// (snowflakeQuietLines). A client configuration file the driver finds on
// its first connection (SF_CLIENT_CONFIG_FILE, or sf_client_config.json
// beside the executable or in the home directory) replaces this with a log
// of the driver's own, where the file says.
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
		if snowflakeQuietLines[fields["msg"]] {
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
