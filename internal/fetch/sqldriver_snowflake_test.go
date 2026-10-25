//go:build (!select_request_types || request_type_sql) && sql_driver_snowflake

package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"github.com/snowflakedb/gosnowflake"
)

// A snowflake target is the account identifier, reached at
// <account>.snowflakecomputing.com:443; the account authenticates with a
// password or an unencrypted RSA key, one of them required; the options name
// the warehouse, the role and the schema; and every request goes through the
// exporter's transport, with nothing stored on disk.
func TestTheSnowflakeDriver(t *testing.T) {
	f := newSQLFixture(t)
	c := f.collector(t, "db", SQLDriverSnowflake, "SELECT 1 AS one", func(r *model.RequestConfig) {
		sqlOwn(r, func(o *SQLConfig) { o.Options = map[string]string{"warehouse": "metrics_wh", "role": "reader"} })
	})
	address, err := parseSQLTarget(c, "MyOrg-MyAccount")
	if err != nil || address.account != "myorg-myaccount" || address.hostPort != "myorg-myaccount.snowflakecomputing.com:443" {
		t.Errorf("%+v %v", address, err)
	}
	if _, err := parseSQLTarget(c, "myorg-myaccount.snowflakecomputing.com:443"); err == nil {
		t.Error("a host and port was taken for an account")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := testutil.WriteIn(t, t.TempDir(), "key.p8", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
	encrypted := testutil.WriteIn(t, t.TempDir(), "encrypted.p8", "-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n")
	read, err := sqlPrivateKey(keyFile)
	if err != nil || !read.Equal(key) {
		t.Errorf("%v", err)
	}
	if _, err := sqlPrivateKey(encrypted); err == nil || !strings.Contains(err.Error(), "is encrypted, which the exporter cannot read; write it unencrypted with openssl pkcs8 -topk8 -nocrypt") {
		t.Errorf("%v", err)
	}
	spec := sqlSpec(t, "443", nil, map[string]string{"warehouse": "metrics_wh", "role": "reader", "schema": "public"})
	spec.account, spec.host = "myorg-myaccount", "myorg-myaccount.snowflakecomputing.com"
	config := snowflakeConfig(spec)
	if config.Account != "myorg-myaccount" || config.Host != spec.host || config.Port != 443 || config.Password != "p@ss:w/rd?" || config.Warehouse != "metrics_wh" ||
		config.Role != "reader" || config.Schema != "public" || config.Transporter == nil || !config.DisableTelemetry || config.ClientStoreTemporaryCredential != gosnowflake.ConfigBoolFalse { //nolint:staticcheck // SA1019: what the driver is configured with (sqldriver_snowflake.go)
		t.Errorf("%+v", config)
	}
	spec.privateKey = key
	if config := snowflakeConfig(spec); config.Authenticator != gosnowflake.AuthTypeJwt || config.PrivateKey != key || config.Password != "" {
		t.Errorf("key-pair: %+v", config)
	}
	for _, tc := range []struct {
		change func(*model.RequestConfig)
		want   string
	}{
		{func(r *model.RequestConfig) { r.PasswordFile = "" }, "sets neither request.password_file nor request.private_key_file"},
		{func(r *model.RequestConfig) { r.PrivateKeyFile = "/k" }, "sets both request.password_file and request.private_key_file"},
		{func(r *model.RequestConfig) {
			sqlOwn(r, func(o *SQLConfig) { o.Options = map[string]string{"a": "b"} })
		}, `which the snowflake driver does not take; it takes: role, schema, warehouse`},
	} {
		c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Username: "u", PasswordFile: "/p", Own: SQLConfig{Driver: SQLDriverSnowflake, Statement: "q"}}}
		tc.change(&c.Request)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v, want %q", err, tc.want)
		}
	}
}

// snowflakeStandIn is a stand-in Snowflake account behind a TLS listener,
// which every snowflake connection of the sql request type reaches through
// the type's pools in place of <account>.snowflakecomputing.com. It logs in
// anyone, recording the client environment the login sent, and answers a
// statement with results[statement], the data of the driver's query
// response. dialed is every address the connections' dialer was asked for.
// Once refuse is set, the dialer refuses every connection, as a policy
// refusing the account's address would; the stand-in closes each connection
// after its answer, so that every request dials. chunk, when set, answers a
// request for a path under /chunks/, a chunk of a result the stand-in serves
// itself. answers[statement], when set, answers the statement in place of
// results, with whatever bytes it writes.
type snowflakeStandIn struct {
	results  map[string]map[string]any
	answers  map[string]http.HandlerFunc
	chunk    http.HandlerFunc
	logins   chan map[string]any
	mu       sync.Mutex
	dialed   []string
	listener string
	refuse   atomic.Bool
}

func newSnowflakeStandIn(t *testing.T) *snowflakeStandIn {
	t.Helper()
	s := &snowflakeStandIn{results: map[string]map[string]any{}, answers: map[string]http.HandlerFunc{}, logins: make(chan map[string]any, 16)}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SQLText string `json:"sqlText"`
			Data    struct {
				ClientEnvironment map[string]any `json:"CLIENT_ENVIRONMENT"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.HasPrefix(r.URL.Path, "/chunks/") && s.chunk != nil {
			s.chunk(w, r)
			return
		}
		answer := map[string]any{"success": true, "data": map[string]any{}}
		w.Header().Set("Connection", "close")
		switch r.URL.Path {
		case "/session/v1/login-request":
			s.logins <- body.Data.ClientEnvironment
			answer["data"] = map[string]any{"token": "t", "masterToken": "m", "sessionId": 1}
		case "/queries/v1/query-request":
			if answer := s.answers[body.SQLText]; answer != nil {
				answer(w, r)
				return
			}
			data, ok := s.results[body.SQLText]
			if !ok {
				answer = map[string]any{"success": false, "code": "002003", "message": "no such statement", "data": map[string]any{"sqlState": "42S02"}}
				break
			}
			answer["data"] = data
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	s.listener = server.Listener.Addr().String()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	hook := func(ctx context.Context, _ string, spec *sqlConnectSpec) (driver.Conn, error) {
		standIn := *spec
		standIn.tls = &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}
		standIn.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			s.mu.Lock()
			s.dialed = append(s.dialed, address)
			s.mu.Unlock()
			if host, _, _ := net.SplitHostPort(address); host != spec.host {
				return nil, &TargetRefusedError{Host: host, Reason: "the connection is to a host the request did not check"}
			}
			if s.refuse.Load() {
				return nil, snowflakeRefusal(spec.host)
			}
			return (&net.Dialer{}).DialContext(ctx, network, s.listener)
		}
		return connectSnowflake(ctx, &standIn)
	}
	sqlConnectHook.Store(&hook)
	t.Cleanup(func() {
		sqlConnectHook.Store(nil)
		closeSnowflakePools(t)
	})
	return s
}

// closeSnowflakePools closes every pool and waits for their connections to
// be closed. closeAll closes a pool's DB apart, and the driver's Close of a
// connection asks the account to end its session: left behind, that request
// can fail after the test, against a stand-in told to refuse or already
// closed, and the driver's error for it is logged into the next test's
// captured log.
func closeSnowflakePools(t *testing.T) {
	t.Helper()
	sqlPools.mu.Lock()
	var dbs []*sql.DB
	for _, p := range sqlPools.pools {
		dbs = append(dbs, p.db)
	}
	for p := range sqlPools.draining {
		dbs = append(dbs, p.db)
	}
	sqlPools.mu.Unlock()
	sqlPools.closeAll()
	deadline := time.Now().Add(30 * time.Second)
	for _, db := range dbs {
		for db.Stats().OpenConnections > 0 {
			if time.Now().After(deadline) {
				t.Errorf("a pool still has %d connections open 30s after it was closed", db.Stats().OpenConnections)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// addresses is every address the connections' dialer was asked for.
func (s *snowflakeStandIn) addresses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.dialed)
}

// snowflakeRows is the data of a query response whose rows, in JSON, are
// rows, of columns of the given names and Snowflake types, with chunks
// after them.
func snowflakeRows(columns []map[string]any, rows [][]string, chunks ...map[string]any) map[string]any {
	data := map[string]any{"rowtype": columns, "rowset": rows, "total": len(rows), "returned": len(rows), "queryResultFormat": "json", "queryId": "q1"}
	if len(chunks) > 0 {
		data["chunks"], data["qrmk"] = chunks, "key"
	}
	return data
}

// The snowflake driver's platform detection is off: the variable that turns
// it off is set once the driver is registered, the login tells the account
// so ("disabled" for its platforms), and by the time it is sent, after the
// detection would have finished, none of the AWS endpoints the detection
// asks, pointed here at a local server, has been asked anything.
func TestTheSnowflakeDriverDetectsNoPlatform(t *testing.T) {
	if got := os.Getenv("SNOWFLAKE_DISABLE_PLATFORM_DETECTION"); got != "true" {
		t.Errorf("SNOWFLAKE_DISABLE_PLATFORM_DETECTION is %q once the driver is registered, want true", got)
	}
	metadata := make(chan string, 32)
	aws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metadata <- r.Method + " " + r.URL.String()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer aws.Close()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ENDPOINT_URL", aws.URL)
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", aws.URL+"/")
	f := newSQLFixture(t)
	s := newSnowflakeStandIn(t)
	s.results["SELECT 1 AS one"] = snowflakeRows([]map[string]any{{"name": "ONE", "type": "fixed", "precision": 1, "scale": 0}}, [][]string{{"1"}})
	c := f.collector(t, "db", SQLDriverSnowflake, "SELECT 1 AS one")
	if _, err := FetchCollector(context.Background(), "myorg-myaccount", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	login := <-s.logins
	if platforms, _ := login["PLATFORM"].([]any); len(platforms) != 1 || platforms[0] != "disabled" {
		t.Errorf("the login's client environment gives the platforms %v, want [disabled]", login["PLATFORM"])
	}
	select {
	case request := <-metadata:
		t.Errorf("the driver asked an AWS endpoint %s", request)
	default:
	}
	for _, address := range s.addresses() {
		if address != "myorg-myaccount.snowflakecomputing.com:443" {
			t.Errorf("the driver dialed %s", address)
		}
	}
}

// A Snowflake NUMBER, which the driver names FIXED and gives as text, and a
// DECFLOAT are numbers to the transforms, a whole one an integer and one
// with a scale the number its digits write, as another database's decimal
// is.
func TestSnowflakeNumbersAreNumbers(t *testing.T) {
	f := newSQLFixture(t)
	s := newSnowflakeStandIn(t)
	s.results["SELECT numbers"] = snowflakeRows([]map[string]any{
		{"name": "N", "type": "fixed", "precision": 38, "scale": 0},
		{"name": "D", "type": "fixed", "precision": 10, "scale": 2},
		{"name": "X", "type": "decfloat"},
		{"name": "R", "type": "real"},
		{"name": "S", "type": "text"},
	}, [][]string{{"42", "1.50", "12345.678", "0.25", "7"}, {"-99999999999999999999", "-0.05", "1E+3", "2", "x"}})
	c := f.collector(t, "db", SQLDriverSnowflake, "SELECT numbers")
	resp, err := FetchCollector(context.Background(), "myorg-myaccount", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	huge, _ := new(big.Int).SetString("-99999999999999999999", 10)
	want := []any{
		map[string]any{"N": 42, "D": 1.5, "X": 12345.678, "R": 0.25, "S": "7"},
		map[string]any{"N": huge, "D": -0.05, "X": 1000.0, "R": 2, "S": "x"},
	}
	if !reflect.DeepEqual(resp.Rows, want) {
		t.Errorf("rows %#v\nwant %#v", resp.Rows, want)
	}
}

// A result that came in more than one chunk fails the statement at once,
// without the scrape's deadline coming, with nothing dialed but the account
// and nothing logged at error: the chunks after the first are at a cloud
// storage URL, which the exporter does not connect to, and the failure says
// to return fewer rows or columns. A result in one chunk is read as before.
func TestASnowflakeResultInMoreThanOneChunkFailsAtOnce(t *testing.T) {
	out := testutil.CaptureLogs(t)
	f := newSQLFixture(t)
	s := newSnowflakeStandIn(t)
	columns := []map[string]any{{"name": "N", "type": "fixed", "precision": 9, "scale": 0}}
	s.results["SELECT many"] = snowflakeRows(columns, [][]string{{"1"}}, map[string]any{
		"url": "https://sfc-results.s3.eu-west-1.amazonaws.com/chunk0?X-Amz-Signature=x", "rowCount": 1, "uncompressedSize": 8, "compressedSize": 8,
	})
	s.results["SELECT few"] = snowflakeRows(columns, [][]string{{"1"}, {"2"}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := f.collector(t, "db", SQLDriverSnowflake, "SELECT many")
	_, err := FetchCollector(ctx, "myorg-myaccount", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "running the statement: "+errSnowflakeChunks.Error()) || ctx.Err() != nil {
		t.Errorf("%v (deadline: %v), want %q", err, ctx.Err(), errSnowflakeChunks)
	}
	if !strings.Contains(errSnowflakeChunks.Error(), "more than one chunk") || !strings.Contains(errSnowflakeChunks.Error(), "return fewer rows or columns") {
		t.Errorf("%q", errSnowflakeChunks)
	}
	c = f.collector(t, "db", SQLDriverSnowflake, "SELECT few")
	if resp, err := FetchCollector(ctx, "myorg-myaccount", c, RequestOverrides{}, nil); err != nil || len(resp.Rows) != 2 {
		t.Errorf("%v %v", resp, err)
	}
	for _, address := range s.addresses() {
		if address != "myorg-myaccount.snowflakecomputing.com:443" {
			t.Errorf("the driver dialed %s", address)
		}
	}
	if strings.Contains(out.String(), `"level":"ERROR"`) {
		t.Errorf("logged at error:\n%s", out)
	}
}

// The driver's reader of a result's later chunks can miss the signal of a
// chunk download that failed and then wait for it forever, past the
// scrape's deadline: the download signals without the lock the reader holds
// from its look for a failure to its wait (gosnowflake chunk_downloader.go,
// downloadChunk and snowflakeChunkDownloader.next). The exporter never lets
// the driver start that reader: a result in more than one chunk fails when
// its first chunk comes. Here the chunk is the stand-in's own, and its
// failure comes inside the reader's window: the stand-in holds the request
// for the chunk until the driver logs, from inside the window, that it found
// no failure, and the driver's log is then silenced, so that the download
// sends its failure and its signal before the reader waits. A driver that
// started the reader would hang there; the scrape fails at once instead,
// without asking for the chunk, and gives its connection and its place at
// max_per_target back: the next scrape, at max_per_target 1, runs at once.
func TestASnowflakeResultInMoreThanOneChunkCannotHangTheScrape(t *testing.T) {
	testutil.CaptureLogs(t)
	f := newSQLFixture(t)
	s := newSnowflakeStandIn(t)
	requested := make(chan struct{}, 8)
	answer := make(chan struct{})
	var chunkRequests atomic.Int32
	s.chunk = func(w http.ResponseWriter, r *http.Request) {
		chunkRequests.Add(1)
		requested <- struct{}{}
		select {
		case <-answer:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusForbidden)
	}
	failed := make(chan struct{}, 8)
	var mu sync.Mutex
	var logged []string
	slog.SetDefault(slog.New(snowflakeLogHook{Handler: slog.Default().Handler(), hook: func(r slog.Record) {
		if strings.HasPrefix(r.Message, "failed to extract HTTP response body. URL: https://myorg-myaccount.snowflakecomputing.com/chunks/0") {
			failed <- struct{}{}
			return
		}
		if r.Level >= slog.LevelError {
			mu.Lock()
			logged = append(logged, r.Message)
			mu.Unlock()
		}
	}}))
	var window sync.Once
	inWindow := func(context.Context) {
		if !snowflakeCalledFrom("(*snowflakeChunkDownloader).checkErrorRetry") {
			return
		}
		window.Do(func() {
			select {
			case <-requested:
			case <-time.After(10 * time.Second):
				return
			}
			close(answer)
			select {
			case <-failed:
				// The download sends its failure and signals right after
				// logging it.
				time.Sleep(200 * time.Millisecond)
			case <-time.After(10 * time.Second):
			}
		})
	}
	snowflakeLogContext.Store(&inWindow)
	t.Cleanup(func() { snowflakeLogContext.Store(nil) })
	one := 1
	limit := func(r *model.RequestConfig) {
		sqlOwn(r, func(o *SQLConfig) { o.Connections = &model.SQLConnections{MaxPerTarget: &one} })
	}
	columns := []map[string]any{{"name": "N", "type": "fixed", "precision": 9, "scale": 0}}
	s.results["SELECT many"] = snowflakeRows(columns, [][]string{{"1"}}, map[string]any{
		"url": "https://myorg-myaccount.snowflakecomputing.com/chunks/0", "rowCount": 1, "uncompressedSize": 8, "compressedSize": 8,
	})
	s.results["SELECT few"] = snowflakeRows(columns, [][]string{{"1"}, {"2"}})
	scrape := func(statement string, timeout time.Duration) (*HTTPResponse, time.Duration, error) {
		t.Helper()
		c := f.collector(t, "db", SQLDriverSnowflake, statement, limit)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		type result struct {
			resp *HTTPResponse
			err  error
		}
		done := make(chan result, 1)
		start := time.Now()
		go func() {
			resp, err := FetchCollector(ctx, "myorg-myaccount", c, RequestOverrides{}, nil)
			done <- result{resp, err}
		}()
		select {
		case r := <-done:
			return r.resp, time.Since(start), r.err
		case <-time.After(timeout + 10*time.Second):
			t.Fatalf("the scrape of %q had not returned 10s after its deadline", statement)
			return nil, 0, nil
		}
	}
	_, took, err := scrape("SELECT many", 5*time.Second)
	if !errors.Is(err, errSnowflakeChunks) || took > 4*time.Second {
		t.Errorf("%v after %v, want %q at once", err, took, errSnowflakeChunks)
	}
	if n := chunkRequests.Load(); n != 0 {
		t.Errorf("the chunk was asked for %d times", n)
	}
	if resp, took, err := scrape("SELECT few", 2*time.Second); err != nil || len(resp.Rows) != 2 || took > time.Second {
		t.Errorf("the next scrape at max_per_target 1: %v %v after %v", resp, err, took)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) > 0 {
		t.Errorf("logged at error: %q", logged)
	}
}

// snowflakeLogHook hands every record of the exporter's logger to hook,
// before its handler.
type snowflakeLogHook struct {
	slog.Handler
	hook func(slog.Record)
}

func (h snowflakeLogHook) Enabled(context.Context, slog.Level) bool { return true }

func (h snowflakeLogHook) Handle(ctx context.Context, r slog.Record) error {
	h.hook(r)
	if !h.Handler.Enabled(ctx, r.Level) {
		return nil
	}
	return h.Handler.Handle(ctx, r)
}

// snowflakeLogContext, when set, is called each time the driver takes the
// fields of a line of its log from a context, before it decides whether to
// log the line, whatever its level, and outside its logger's lock. The hook
// is registered once, at startup, as the driver asks.
var snowflakeLogContext atomic.Pointer[func(context.Context)]

func init() {
	gosnowflake.RegisterLogContextHook("exporter_test", func(ctx context.Context) string {
		if hook := snowflakeLogContext.Load(); hook != nil {
			(*hook)(ctx)
		}
		return ""
	})
}

// snowflakeCalledFrom says the caller's goroutine is in the driver's
// function of the given name, receiver included.
func snowflakeCalledFrom(name string) bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, "gosnowflake."+name) {
			return true
		}
		if !more {
			return false
		}
	}
}

// snowflakeRefusal is the policy's refusal of a connection to host.
func snowflakeRefusal(host string) *TargetRefusedError {
	return &TargetRefusedError{Host: host, Reason: "its address 192.0.2.7 is in 192.0.2.0/24 in request.denied_targets"}
}

// A connection to the account that the target policy refuses fails the
// connection at once with the policy's own words, where the driver would
// retry it until the deadline and report only that: here the dialer connects
// to a local listener whose address request.denied_targets holds. Once
// connected, a statement whose request needs a new connection to the account
// that the policy refuses fails the scrape at once the same way.
func TestASnowflakeConnectionThePolicyRefusesFailsAtOnceWithTheRefusal(t *testing.T) {
	spec := sqlSpec(t, listen(t), nil, nil)
	spec.account = "myorg-myaccount"
	listener, policyDial := spec.hostPort, spec.dial
	spec.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return policyDial(ctx, network, listener)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := connectSnowflake(ctx, spec)
	var refused *TargetRefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "127.0.0.0/8 in request.denied_targets") || ctx.Err() != nil {
		t.Errorf("%v (deadline: %v), want the policy's refusal at once", err, ctx.Err())
	}
	f := newSQLFixture(t)
	s := newSnowflakeStandIn(t)
	s.results["SELECT 1 AS one"] = snowflakeRows([]map[string]any{{"name": "ONE", "type": "fixed", "precision": 1, "scale": 0}}, [][]string{{"1"}})
	c := f.collector(t, "db", SQLDriverSnowflake, "SELECT 1 AS one")
	if _, err := FetchCollector(ctx, "myorg-myaccount", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	s.refuse.Store(true)
	_, err = FetchCollector(ctx, "myorg-myaccount", c, RequestOverrides{}, nil)
	if want := snowflakeRefusal("myorg-myaccount.snowflakecomputing.com").Error(); err == nil || !strings.Contains(err.Error(), want) || ctx.Err() != nil {
		t.Errorf("%v (deadline: %v), want %q at once", err, ctx.Err(), want)
	}
}

// The transport of a snowflake connection answers as before everything but
// a refused connection to the account and a result in more than one chunk:
// the account's answer as it came, its body whole, another failure to
// connect to it as an error, which the driver retries as it did, and a
// request to another host with 403 Forbidden without a connection. The
// policy's refusal becomes a 403, which the driver does not retry, kept where
// the request's context says. The account's answer to a statement, or to an
// ask for its result, that names chunks, plain or gzipped, becomes the
// statement's failure, which the driver neither retries nor reads a result
// from, noted there. The same answer at another path, or with another
// status, and a result without chunks, are passed on.
func TestTheSnowflakeTransportChangesOnlyARefusedConnectionOrAChunkedResult(t *testing.T) {
	chunked := `{"success":true,"data":{"rowset":[["1"]],"chunks":[{"url":"https://sfc-results.example/0","rowCount":1}]}}`
	single := `{"success":true,"data":{"rowset":[["1"]],"chunks":[]}}`
	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	_, _ = zw.Write([]byte(chunked))
	_ = zw.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("answer") {
		case "chunked":
			_, _ = w.Write([]byte(chunked))
		case "chunked-error":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(chunked))
		case "gzipped":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gzipped.Bytes())
		case "single":
			_, _ = w.Write([]byte(single))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(server.Close)
	failure, _ := io.ReadAll(snowflakeFailedAnswer(nil, snowflakeChunkedError).Body)
	var answer struct {
		Success bool
		Code    string
		Message string
	}
	if err := json.Unmarshal(failure, &answer); err != nil || answer.Success || answer.Code != "262000" || answer.Message != errSnowflakeChunks.Error() {
		t.Errorf("%s: %v", failure, err)
	}
	network := errors.New("connection reset by the network")
	refusal := snowflakeRefusal("127.0.0.1")
	for _, tc := range []struct {
		name    string
		url     string
		dialErr error
		status  int
		body    string
		err     error
		refused bool
		chunked bool
	}{
		{"the account's answer", server.URL, nil, http.StatusTeapot, "", nil, false, false},
		{"another failure", server.URL, network, 0, "", network, false, false},
		{"the policy's refusal", server.URL, refusal, http.StatusForbidden, "", nil, true, false},
		{"another host", "http://sfc-results.example/chunk", refusal, http.StatusForbidden, "", nil, false, false},
		{"a result in chunks", server.URL + "/queries/v1/query-request?answer=chunked", nil, http.StatusOK, string(failure), nil, false, true},
		{"a result in chunks asked for", server.URL + "/queries/q1/result?answer=chunked", nil, http.StatusOK, string(failure), nil, false, true},
		{"a gzipped result in chunks", server.URL + "/queries/v1/query-request?answer=gzipped", nil, http.StatusOK, string(failure), nil, false, true},
		{"a result in one chunk", server.URL + "/queries/v1/query-request?answer=single", nil, http.StatusOK, single, nil, false, false},
		{"chunks at another path", server.URL + "/session/v1/login-request?answer=chunked", nil, http.StatusOK, chunked, nil, false, false},
		{"chunks with another status", server.URL + "/queries/v1/query-request?answer=chunked-error", nil, http.StatusInternalServerError, chunked, nil, false, false},
	} {
		dialErr := tc.dialErr
		transport := &snowflakeTransport{host: "127.0.0.1", base: &http.Transport{DisableCompression: true, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if dialErr != nil {
				return nil, dialErr
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}}}
		ctx, notes := withSnowflakeNotes(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tc.url, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.RoundTrip(req)
		status, body := 0, ""
		if resp != nil {
			status = resp.StatusCode
			read, _ := io.ReadAll(resp.Body)
			body = string(read)
			_ = resp.Body.Close()
		}
		if status != tc.status || !errors.Is(err, tc.err) || (notes.refused.Load() != nil) != tc.refused || notes.chunked.Load() != tc.chunked || (tc.body != "" && body != tc.body) {
			t.Errorf("%s: %d %v %q (refused %v, chunked %v), want %d %v", tc.name, status, err, body, notes.refused.Load(), notes.chunked.Load(), tc.status, tc.err)
		}
		transport.CloseIdleConnections()
	}
}

// The driver's log goes to the exporter's logger, one JSON line for each of
// the driver's lines, with sql_driver snowflake, the driver's level and its
// message as the driver's formatter leaves it, without the password. Each
// of the driver's levels is the logger's nearest, and a line that is not
// the formatter's is logged whole.
func TestTheSnowflakeDriversLogGoesToTheExportersLogger(t *testing.T) {
	snowflakeChunkedLine := "error: " + snowflakeChunkedError.Error()
	snowflakeTooLargeLine := "error: " + snowflakeTooLargeError.Error()
	out := testutil.CaptureLogs(t)
	gosnowflake.GetLogger().Errorf("the login failed with password=hunter2hunter2 for %s", "metrics") //nolint:staticcheck // SA1019: the driver's logger, as the driver logs
	records := testutil.AssertJSONLines(t, out, 1)
	if r := records[0]; r["level"] != "ERROR" || r["msg"] != "the login failed with password=**** for metrics" || r["sql_driver"] != "snowflake" {
		t.Errorf("%v", r)
	}
	for line, want := range map[string]struct {
		level slog.Level
		msg   string
	}{
		`time="2026-10-14T10:00:00Z" level=warning msg="failed http connection. err: refused. retrying...\n" func=gosnowflake.x file="retry.go:1"`: {slog.LevelWarn, "failed http connection. err: refused. retrying...\n"},
		`time="2026-10-14T10:00:00Z" level=error msg=closed`:      {slog.LevelError, "closed"},
		`level=fatal msg="a \"quoted\" word"`:                     {slog.LevelError, `a "quoted" word`},
		`level=panic msg=x`:                                       {slog.LevelError, "x"},
		"level=error msg=" + strconv.Quote(snowflakeChunkedLine):  {slog.LevelDebug, snowflakeChunkedLine},
		"level=error msg=" + strconv.Quote(snowflakeTooLargeLine): {slog.LevelDebug, snowflakeTooLargeLine},
		`level=error msg="error: 262000 (08006): other"`:          {slog.LevelError, "error: 262000 (08006): other"},
		"level=fatal msg=" + strconv.Quote(snowflakeChunkedLine):  {slog.LevelError, snowflakeChunkedLine},
		`level=info msg=y`:                                        {slog.LevelInfo, "y"},
		`level=debug msg=z`:                                       {slog.LevelDebug, "z"},
		`level=trace msg=z`:                                       {slog.LevelDebug, "z"},
		`not the formatter's`:                                     {slog.LevelInfo, `not the formatter's`},
		`level="unclosed`:                                         {slog.LevelInfo, `level="unclosed`},
		`msg=no-level`:                                            {slog.LevelInfo, `msg=no-level`},
	} {
		if level, msg := snowflakeLogLine(line); level != want.level || msg != want.msg {
			t.Errorf("%s: %v %q, want %v %q", line, level, msg, want.level, want.msg)
		}
	}
	out.Reset()
	if n, err := (snowflakeLog{}).Write([]byte("level=error msg=a\nlevel=warning msg=b\n\n")); n != 39 || err != nil {
		t.Errorf("%d %v", n, err)
	}
	if records := testutil.AssertJSONLines(t, out, 2); records[0]["msg"] != "a" || records[1]["level"] != "WARN" {
		t.Errorf("%v", records)
	}
}

// The driver logs its errors whatever the environment: its own init lowers
// its level to fatal when GITHUB_ACTIONS is set, which would leave the
// exporter's log without them on a runner of GitHub's, and the exporter
// sets it back to error. CI runs this with GITHUB_ACTIONS set.
func TestTheSnowflakeDriverLogsItsErrorsWhereverItRuns(t *testing.T) {
	if level := gosnowflake.GetLogger().GetLogLevel(); level != "error" { //nolint:staticcheck // SA1019: the driver's logger
		t.Errorf("the driver logs at %q, want error (GITHUB_ACTIONS=%q)", level, os.Getenv("GITHUB_ACTIONS"))
	}
}

// snowflakeLargeAnswer is the account's answer to a statement whose one
// row, of one text column, is a value of size bytes, in JSON.
func snowflakeLargeAnswer(size int) []byte {
	return []byte(`{"success":true,"data":{"rowtype":[{"name":"S","type":"text"}],"rowset":[["` + strings.Repeat("x", size) +
		`"]],"total":1,"returned":1,"queryResultFormat":"json","queryId":"q1"}}`)
}

// snowflakeGzip is body gzipped.
func snowflakeGzip(t *testing.T, body []byte) []byte {
	t.Helper()
	var gzipped bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gzipped, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = zw.Write(body)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return gzipped.Bytes()
}

// The account's answer to a statement is read to the collector's response
// limit and no further: an answer larger than the limit, and a gzipped one
// that is small but decompresses past it, fail the statement at once with
// the limit's failure, ErrLimitExceeded, not the driver's error and not the
// scrape's deadline, with nothing logged at error, and the exporter reads
// and holds little more than the limit of either, where it read the whole
// answer, and decompressed the whole gzip, and held it twice. A result
// within the limit is read as before, and one in more than one chunk still
// fails with errSnowflakeChunks.
func TestASnowflakeAnswerLargerThanTheResponseLimitFailsWithTheLimit(t *testing.T) {
	out := testutil.CaptureLogs(t)
	f := newSQLFixture(t)
	s := newSnowflakeStandIn(t)
	const limit = 1 << 20
	large := snowflakeLargeAnswer(32 << 20)
	bomb := snowflakeGzip(t, snowflakeLargeAnswer(64<<20))
	if len(bomb) > limit/4 {
		t.Fatalf("the gzipped answer is %d bytes", len(bomb))
	}
	s.answers["SELECT large"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "close")
		// No Content-Length: the answer is read until it is too large.
		w.(http.Flusher).Flush()
		_, _ = w.Write(large)
	}
	s.answers["SELECT bomb"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Connection", "close")
		_, _ = w.Write(bomb)
	}
	columns := []map[string]any{{"name": "N", "type": "fixed", "precision": 9, "scale": 0}}
	s.results["SELECT few"] = snowflakeRows(columns, [][]string{{"1"}, {"2"}})
	s.results["SELECT many"] = snowflakeRows(columns, [][]string{{"1"}}, map[string]any{
		"url": "https://sfc-results.s3.eu-west-1.amazonaws.com/chunk0?X-Amz-Signature=x", "rowCount": 1, "uncompressedSize": 8, "compressedSize": 8,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	collector := func(statement string) *model.Collector {
		return f.collector(t, "db", SQLDriverSnowflake, statement, func(r *model.RequestConfig) { r.MaxResponseBytes = limit })
	}
	for _, statement := range []string{"SELECT large", "SELECT bomb"} {
		c := collector(statement)
		var err error
		run := func() { _, err = FetchCollector(ctx, "myorg-myaccount", c, RequestOverrides{}, nil) }
		bytes := alloctest.BytesAtMost(1, 8<<20, run)
		if err == nil || !errors.Is(err, model.ErrLimitExceeded) || ctx.Err() != nil ||
			!strings.Contains(err.Error(), "running the statement: the statement's answer from Snowflake was larger than the response limit of") {
			t.Errorf("%s: %v (deadline: %v), want the response limit's failure", statement, err, ctx.Err())
		}
		if !alloctest.RaceDetector && bytes > 8<<20 {
			t.Errorf("%s: the scrape allocated %d bytes, the response limit being %d", statement, bytes, limit)
		}
	}
	if resp, err := FetchCollector(ctx, "myorg-myaccount", collector("SELECT few"), RequestOverrides{}, nil); err != nil || len(resp.Rows) != 2 {
		t.Errorf("%v %v", resp, err)
	}
	if _, err := FetchCollector(ctx, "myorg-myaccount", collector("SELECT many"), RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), errSnowflakeChunks.Error()) {
		t.Errorf("%v, want %q", err, errSnowflakeChunks)
	}
	if strings.Contains(out.String(), `"level":"ERROR"`) {
		t.Errorf("logged at error:\n%s", out)
	}
}

// The transport reads the account's answer to a statement to its limit: an
// answer whose Content-Length passes the limit is refused unread, one
// without a Content-Length once it has read a byte past the limit, and a
// gzipped one (Content-Encoding gzip, which Go's transport leaves when it
// did not ask for gzip itself) once it has decompressed a byte past it,
// each with the statement's failure, which the driver neither retries nor
// reads a result from, and the limit's failure noted where the request's
// context says. A gzipped answer within the limit is passed on
// decompressed, and one that is not gzip after all as it came; an answer of
// exactly the limit is passed on, and with no limit nothing is refused.
func TestTheSnowflakeTransportHoldsAnAnswerToTheResponseLimit(t *testing.T) {
	const limit = 4096
	single := `{"success":true,"data":{"rowset":[["1"]],"chunks":[]}}`
	exact := `{"success":true,"data":{"rowset":[["` + strings.Repeat("x", limit-len(single)) + `"]],"chunks":[]}}`
	large := string(snowflakeLargeAnswer(limit))
	gzipped := snowflakeGzip(t, []byte(single))
	bomb := snowflakeGzip(t, snowflakeLargeAnswer(16<<20))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("answer") {
		case "declared":
			w.Header().Set("Content-Length", strconv.Itoa(len(large)))
			_, _ = w.Write([]byte(large))
		case "undeclared":
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte(large))
		case "exact":
			_, _ = w.Write([]byte(exact))
		case "bomb":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(bomb)
		case "gzipped":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gzipped)
		case "not-gzip":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write([]byte(single))
		}
	}))
	t.Cleanup(server.Close)
	failure, _ := io.ReadAll(snowflakeFailedAnswer(nil, snowflakeTooLargeError).Body)
	for _, tc := range []struct {
		answer   string
		limit    int64
		body     string
		encoding string
		tooLarge bool
	}{
		{"declared", limit, string(failure), "", true},
		{"undeclared", limit, string(failure), "", true},
		{"bomb", limit, string(failure), "", true},
		{"exact", limit, exact, "", false},
		{"gzipped", limit, single, "", false},
		{"not-gzip", limit, single, "gzip", false},
		{"undeclared", 0, large, "", false},
	} {
		transport := &snowflakeTransport{host: "127.0.0.1", base: &http.Transport{DisableCompression: true}, limit: tc.limit}
		ctx, notes := withSnowflakeNotes(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/queries/v1/query-request?answer="+tc.answer, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.answer, err)
		}
		read, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		tooLarge := notes.tooLarge.Load()
		if resp.StatusCode != http.StatusOK || string(read) != tc.body || resp.Header.Get("Content-Encoding") != tc.encoding || (tooLarge != nil) != tc.tooLarge {
			t.Errorf("%s (limit %d): %d %.80q %q (too large %v)", tc.answer, tc.limit, resp.StatusCode, read, resp.Header.Get("Content-Encoding"), tooLarge)
		}
		if tooLarge != nil && (!errors.Is(*tooLarge, model.ErrLimitExceeded) || !strings.Contains((*tooLarge).Error(), "larger than the response limit of 4096 bytes")) {
			t.Errorf("%s: %v", tc.answer, *tooLarge)
		}
		transport.CloseIdleConnections()
	}
}
