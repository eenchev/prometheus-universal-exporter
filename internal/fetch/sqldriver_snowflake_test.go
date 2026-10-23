//go:build (!select_request_types || request_type_sql) && sql_driver_snowflake

package fetch

import (
	"bytes"
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
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
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
		r.Options = map[string]string{"warehouse": "metrics_wh", "role": "reader"}
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
		{func(r *model.RequestConfig) { r.Options = map[string]string{"a": "b"} }, `which the snowflake driver does not take; it takes: role, schema, warehouse`},
	} {
		c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: SQLDriverSnowflake, Statement: "q", Username: "u", PasswordFile: "/p"}}
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
// after its answer, so that every request dials.
type snowflakeStandIn struct {
	results  map[string]map[string]any
	logins   chan map[string]any
	mu       sync.Mutex
	dialed   []string
	listener string
	refuse   atomic.Bool
}

func newSnowflakeStandIn(t *testing.T) *snowflakeStandIn {
	t.Helper()
	s := &snowflakeStandIn{results: map[string]map[string]any{}, logins: make(chan map[string]any, 16)}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SQLText string `json:"sqlText"`
			Data    struct {
				ClientEnvironment map[string]any `json:"CLIENT_ENVIRONMENT"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		answer := map[string]any{"success": true, "data": map[string]any{}}
		w.Header().Set("Connection", "close")
		switch r.URL.Path {
		case "/session/v1/login-request":
			s.logins <- body.Data.ClientEnvironment
			answer["data"] = map[string]any{"token": "t", "masterToken": "m", "sessionId": 1}
		case "/queries/v1/query-request":
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

// A result that came in more than one chunk fails at once, without the
// scrape's deadline coming and with nothing dialed but the account: the
// chunks after the first are at a cloud storage URL, which the exporter
// does not connect to, and the failure says to return fewer rows or columns.
// A result in one chunk is read as before.
func TestASnowflakeResultInMoreThanOneChunkFailsAtOnce(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "reading the rows: "+errSnowflakeChunks.Error()) || ctx.Err() != nil {
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
	other := errors.New("other")
	if got := snowflakeRowsError(other); !errors.Is(got, other) || got.Error() != "other" {
		t.Error("another failure reading the rows was changed")
	}
}

// The driver's errors for its tries at a later chunk of a result, which the
// exporter refuses, are logged at debug, the scrape's failure saying what
// happened: the result in two chunks logs no error, and the six tries, the
// first and its five more, at debug. Every other error of the driver stays
// an error: the same words with another failure, and its other lines.
func TestTheSnowflakeDriversChunkErrorsAreLoggedAtDebug(t *testing.T) {
	// CaptureLogs puts the default logger back when the test ends; this one
	// keeps debug lines too.
	testutil.CaptureLogs(t)
	var out bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	f := newSQLFixture(t)
	s := newSnowflakeStandIn(t)
	columns := []map[string]any{{"name": "N", "type": "fixed", "precision": 9, "scale": 0}}
	s.results["SELECT many"] = snowflakeRows(columns, [][]string{{"1"}}, map[string]any{
		"url": "https://sfc-results.s3.eu-west-1.amazonaws.com/chunk0?X-Amz-Signature=x", "rowCount": 1, "uncompressedSize": 8, "compressedSize": 8,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := FetchCollector(ctx, "myorg-myaccount", f.collector(t, "db", SQLDriverSnowflake, "SELECT many"), RequestOverrides{}, nil); !errors.Is(err, errSnowflakeChunks) {
		t.Errorf("%v, want %q", err, errSnowflakeChunks)
	}
	tries := 0
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			t.Errorf("logged at error: %s", line)
		}
		if strings.Contains(line, `"level":"DEBUG","msg":"failed to extract HTTP response body. URL: https://sfc-results.s3.eu-west-1.amazonaws.com/chunk0`) {
			tries++
		}
	}
	if tries != 6 {
		t.Errorf("%d tries at the chunk logged at debug, want 6:\n%s", tries, out.String())
	}
	for line, want := range map[string]slog.Level{
		`level=error msg="failed to extract HTTP response body. URL: https://x/chunk0?X-Amz-Signature=****, err: 262000 (08006): failed to get a chunk of result sets. idx: 3"`: slog.LevelDebug,
		`level=error msg="failed to extract HTTP response body. URL: https://x/chunk0, err: getting chunk: connection reset"`:                                                   slog.LevelError,
		`level=error msg="failed to extract HTTP response body. URL: https://x/chunk0, err: 260000 (08006): other"`:                                                             slog.LevelError,
		`level=error msg="reading response body: unexpected EOF"`:                                                                                                               slog.LevelError,
		`level=error msg="262000 (08006): failed to get a chunk of result sets. idx: 3"`:                                                                                        slog.LevelError,
		`level=fatal msg="failed to extract HTTP response body. URL: https://x/chunk0, err: 262000 (08006): failed to get a chunk of result sets. idx: 3"`:                      slog.LevelError,
	} {
		if level, _ := snowflakeLogLine(line); level != want {
			t.Errorf("%s: %v, want %v", line, level, want)
		}
	}
}

// snowflakeLogLineBefore is snowflakeLogLine as it was, the oracle of the
// test below.
func snowflakeLogLineBefore(line string) (slog.Level, string) {
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
	case "error", "fatal", "panic":
		return slog.LevelError, fields["msg"]
	case "info":
		return slog.LevelInfo, fields["msg"]
	}
	return slog.LevelDebug, fields["msg"]
}

// Every line of the driver's log but its error for a try at a later chunk
// is read as it was: each level with messages that hold the chunk error's
// words in part, or around something else, and lines that are not the
// formatter's.
func TestOnlyTheSnowflakeChunkErrorChangedLevel(t *testing.T) {
	messages := []string{
		"closed", "", "failed to extract HTTP response body. URL: https://x/c, err: getting chunk: EOF",
		"failed to extract HTTP response body. URL: https://x/c", "x failed to extract HTTP response body. URL: https://x/c, err: 262000 (08006): y",
		"err: 262000 (08006): failed to get a chunk of result sets. idx: 0", "failed to extract HTTP response body. URL: https://x/c, err: 262000: y",
		"failed to extract HTTP response body. URL: https://x/c, err: 262001 (08006): y",
		"failed to extract HTTP response body. URL: https://x/c, err: 262000 (08006): failed to get a chunk of result sets. idx: 0",
	}
	compared, changed := 0, 0
	for _, level := range []string{"error", "fatal", "panic", "warning", "info", "debug", "trace", "ERROR", ""} {
		for _, msg := range messages {
			for _, line := range []string{
				"level=" + level + " msg=" + strconv.Quote(msg), `time="2026-10-14T10:00:00Z" level=` + level + " msg=" + strconv.Quote(msg) + ` func=gosnowflake.x`,
				"msg=" + strconv.Quote(msg), "level=" + level + " " + msg,
			} {
				gotLevel, gotMsg := snowflakeLogLine(line)
				wantLevel, wantMsg := snowflakeLogLineBefore(line)
				chunk := strings.HasPrefix(line, "level=error msg=") || strings.HasPrefix(line, `time="2026-10-14T10:00:00Z" level=error msg=`)
				chunk = chunk && msg == messages[len(messages)-1]
				if chunk {
					changed++
					wantLevel = slog.LevelDebug
				}
				if gotLevel != wantLevel || gotMsg != wantMsg {
					t.Errorf("%s: %v %q, before %v %q", line, gotLevel, gotMsg, wantLevel, wantMsg)
				}
				compared++
			}
		}
	}
	if changed != 2 || compared < 300 {
		t.Errorf("compared %d lines, %d of them the chunk error", compared, changed)
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
// a refused connection to the account: the account's answer as it came,
// another failure to connect to it as an error, which the driver retries as
// it did, and a request to another host with 403 Forbidden without a
// connection. Only the policy's refusal becomes a 403, which the driver does
// not retry, kept where the request's context says.
func TestTheSnowflakeTransportChangesOnlyARefusedConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	t.Cleanup(server.Close)
	network := errors.New("connection reset by the network")
	refusal := snowflakeRefusal("127.0.0.1")
	for _, tc := range []struct {
		name    string
		url     string
		dialErr error
		status  int
		err     error
		refused bool
	}{
		{"the account's answer", server.URL, nil, http.StatusTeapot, nil, false},
		{"another failure", server.URL, network, 0, network, false},
		{"the policy's refusal", server.URL, refusal, http.StatusForbidden, nil, true},
		{"another host", "http://sfc-results.example/chunk", refusal, http.StatusForbidden, nil, false},
	} {
		dialErr := tc.dialErr
		transport := &snowflakeTransport{host: "127.0.0.1", base: &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if dialErr != nil {
				return nil, dialErr
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}}}
		ctx, kept := withSnowflakeRefusal(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tc.url, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.RoundTrip(req)
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		if status != tc.status || !errors.Is(err, tc.err) || (kept.Load() != nil) != tc.refused {
			t.Errorf("%s: %d %v (kept %v), want %d %v", tc.name, status, err, kept.Load(), tc.status, tc.err)
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
		`time="2026-10-14T10:00:00Z" level=error msg=closed`: {slog.LevelError, "closed"},
		`level=fatal msg="a \"quoted\" word"`:                {slog.LevelError, `a "quoted" word`},
		`level=panic msg=x`:                                  {slog.LevelError, "x"},
		`level=info msg=y`:                                   {slog.LevelInfo, "y"},
		`level=debug msg=z`:                                  {slog.LevelDebug, "z"},
		`level=trace msg=z`:                                  {slog.LevelDebug, "z"},
		`not the formatter's`:                                {slog.LevelInfo, `not the formatter's`},
		`level="unclosed`:                                    {slog.LevelInfo, `level="unclosed`},
		`msg=no-level`:                                       {slog.LevelInfo, `msg=no-level`},
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
