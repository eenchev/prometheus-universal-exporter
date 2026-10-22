//go:build (!select_request_types || request_type_sql) && sql_driver_vertica

package fetch

import (
	"context"
	"crypto/tls"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	_ "unsafe" // go:linkname reads the driver's registry of TLS configurations.

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	verticalog "github.com/vertica/vertica-sql-go/logger"
	"github.com/vertica/vertica-sql-go/parse"
)

// The vertica driver's URL holds the escaped credentials, the database and
// the TLS mode it is given: none without TLS, and the name a configuration
// is registered by with TLS; its port is 5433; since it dials by itself, a
// target whose address the policy refuses is refused before a connection
// is made; and a connection not made by the deadline is given up.
func TestTheVerticaDriver(t *testing.T) {
	spec := sqlSpec(t, "5433", nil, nil)
	raw := verticaURL(spec, "none")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if u.Host != "127.0.0.1:5433" || u.User.Username() != "metrics" || password != "p@ss:w/rd?" || u.Path != "/app" || u.Query().Get("tlsmode") != "none" {
		t.Errorf("%s", raw)
	}
	if u, _ := url.Parse(verticaURL(spec, "prometheus-universal-exporter-1")); u.Query().Get("tlsmode") != "prometheus-universal-exporter-1" {
		t.Errorf("%s", u)
	}
	f := newSQLFixture(t)
	c := f.collector(t, "db", SQLDriverVertica, "SELECT 1 AS one", func(r *model.RequestConfig) { r.DeniedTargets = []string{"127.0.0.0/8"} })
	if address, err := parseSQLTarget(c, "db.example"); err != nil || address.hostPort != "db.example:5433" {
		t.Errorf("%+v %v", address, err)
	}
	if _, err := FetchCollector(context.Background(), "127.0.0.1", c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("%v, want the policy's refusal", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	held := make(chan struct{})
	defer close(held)
	if _, err := connectWithin(ctx, func() (driver.Conn, error) { <-held; return nil, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%v", err)
	}
}

// A vertica statement that holds more than one is refused when the
// configuration is loaded, as the driver would split it and run each part
// on its own, a COMMIT ending the read-only transaction: a ; counts outside
// quotes, quoted identifiers, comments and dollar quotes, and one followed
// by nothing but blanks and comments ends the one statement. Each case is
// split, or not, by the driver itself as the test expects of it, except
// those the driver since 1.3.9 takes to stand inside a BEGIN ... END or
// similar block and sends whole, which the server would still run as
// several statements: those are refused too.
func TestAVerticaStatementHoldingMoreThanOneIsRefused(t *testing.T) {
	for _, tc := range []struct {
		statement string
		refused   bool
		// blockHidden is a statement the driver itself does not split,
		// taking its ; to stand inside a block, though the server would
		// run every part of it.
		blockHidden bool
	}{
		{"COMMIT; SELECT 1 AS one", true, false},
		{"SELECT 1 AS one; COMMIT", true, false},
		{"SELECT 1;SELECT 2", true, false},
		{"SELECT 1 AS one -- ;\n; SELECT 2", true, false},
		{"SELECT 1 AS one;", false, false},
		{"SELECT 1 AS one ;  \n\t", false, false},
		{"SELECT 1 AS one; -- the end", false, false},
		{"SELECT 1 AS one; /* the end; really */", false, false},
		{"SELECT 1 AS one; // the end", true, false},
		{"SELECT ';' AS one", false, false},
		{"SELECT 'it''s; here' AS one", false, false},
		{`SELECT 1 AS "a;b"`, false, false},
		{`SELECT 1 AS "a"";b"`, false, false},
		{"SELECT $$;$$ AS one", false, false},
		{"SELECT $tag$ ; $tag$ AS one", false, false},
		{"SELECT 1 AS one -- ; COMMIT", false, false},
		{"SELECT 1 AS one /* ; COMMIT */", false, false},
		{"SELECT ? AS one, @name AS two", false, false},
		{"SELECT @name; COMMIT", true, false},
		{"BEGIN; COMMIT; SELECT 1", true, true},
		{"SELECT 1 AS begin; COMMIT", true, true},
		{"begin select 1; commit; end", true, true},
		{"SELECT 1 AS one; BEGIN; COMMIT; DROP TABLE t", true, false},
		{"SELECT CASE WHEN 1 = 1 THEN 1 END AS one; COMMIT", true, false},
		{"SELECT CASE WHEN 1 = 1 THEN 1 END AS one;", false, false},
		{"SELECT 1 AS loop; COMMIT", true, true},
	} {
		if split := len(parse.SplitStatements(tc.statement)) > 1; split != (tc.refused && !tc.blockHidden) {
			t.Errorf("%q: the driver splits it %t, the case says %t", tc.statement, split, tc.refused && !tc.blockHidden)
		}
		err := verticaSingleStatement(tc.statement)
		if (err != nil) != tc.refused {
			t.Errorf("%q: %v, want refused %t", tc.statement, err, tc.refused)
		}
		c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: SQLDriverVertica, Statement: tc.statement, Username: "metrics"}}
		if err := ValidateRequest(c); (err != nil && strings.Contains(err.Error(), "more than one statement")) != tc.refused {
			t.Errorf("%q: loading gives %v, want refused %t", tc.statement, err, tc.refused)
		}
	}
}

// The vertica driver's log reaches the exporter's logger, at the level of
// each line, rather than stdout in a format of the driver's own.
func TestTheVerticaDriversLogReachesTheExportersLogger(t *testing.T) {
	out := testutil.CaptureLogs(t)
	verticalog.New("connection").Warn("server sent a notice: %s", "disk low")
	verticalog.New("connection").Error("lost the connection")
	logged := out.String()
	for _, want := range []string{
		`"level":"WARN","msg":"server sent a notice: disk low","sql_driver":"vertica","driver_logger":"connection"`,
		`"level":"ERROR","msg":"lost the connection"`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log lacks %s:\n%s", want, logged)
		}
	}
}

// verticaDriverTLSConfigs is the driver's registry of TLS configurations
// (connection.go tlsConfigs), read for its size.
//
//go:linkname verticaDriverTLSConfigs github.com/vertica/vertica-sql-go.tlsConfigs
var verticaDriverTLSConfigs *struct {
	m map[string]*tls.Config
	sync.RWMutex
}

func verticaRegisteredTLSConfigs() int {
	verticaDriverTLSConfigs.RLock()
	defer verticaDriverTLSConfigs.RUnlock()
	return len(verticaDriverTLSConfigs.m)
}

// verticaTLSListener answers the vertica driver's TLS request and sends
// the name its TLS handshake asks for (SNI) on the channel it returns,
// ending the handshake there.
func verticaTLSListener(t *testing.T) (port string, names chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	names = make(chan string, 64)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				request := make([]byte, 8)
				if _, err := io.ReadFull(conn, request); err != nil {
					return
				}
				if _, err := conn.Write([]byte{'S'}); err != nil {
					return
				}
				_ = tls.Server(conn, &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					names <- hello.ServerName
					return nil, errors.New("the stand-in ends the handshake here")
				}}).Handshake()
			}()
		}
	}()
	_, port, _ = net.SplitHostPort(listener.Addr().String())
	return port, names
}

// A vertica connection's TLS handshake is made with the configuration made
// for it, from the files as they were read for it, the one before it
// having been made with other files under what was the same digest of
// them, as when the files change between the reads; and the driver's
// registry of configurations grows by no more than the connections opened
// at once, however many configurations there have been: one at a time, by
// one name at most, and eight at once, by eight at most, each handshake
// still taking its own.
func TestAVerticaConnectionTakesItsOwnTLSConfigurationAndTheRegistryStaysBounded(t *testing.T) {
	port, names := verticaTLSListener(t)
	connect := func(serverName, digest string) {
		spec := sqlSpec(t, port, &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, nil) //nolint:gosec // G402: the stand-in has no certificate to verify
		spec.tlsDigest = digest
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if conn, err := connectVertica(ctx, spec); err == nil {
			_ = conn.Close()
			t.Errorf("%s: connected to the stand-in", serverName)
		}
	}
	before := verticaRegisteredTLSConfigs()
	for i, serverName := range []string{"a.example", "b.example", "c.example"} {
		connect(serverName, "rotated")
		if got := <-names; got != serverName {
			t.Errorf("connection %d asked for %q, its configuration names %q", i, got, serverName)
		}
	}
	rounds := alloctest.UnlessRaced(50, 20)
	for i := range rounds {
		serverName := fmt.Sprintf("rotation-%d.example", i)
		connect(serverName, serverName)
		if got := <-names; got != serverName {
			t.Errorf("rotation %d asked for %q, want %q", i, got, serverName)
		}
	}
	if grown := verticaRegisteredTLSConfigs() - before; grown > 1 {
		t.Errorf("the driver's registry grew by %d over %d connections made one at a time", grown, rounds+3)
	}
	var wg sync.WaitGroup
	want := map[string]bool{}
	for i := range 8 {
		serverName := fmt.Sprintf("together-%d.example", i)
		want[serverName] = true
		wg.Go(func() { connect(serverName, serverName) })
	}
	wg.Wait()
	for range 8 {
		got := <-names
		if !want[got] {
			t.Errorf("a connection asked for %q, which no configuration left to ask for names", got)
		}
		delete(want, got)
	}
	if grown := verticaRegisteredTLSConfigs() - before; grown > 8 {
		t.Errorf("the driver's registry grew by %d with eight connections made at once", grown)
	}
}

// The vertica connection is guarded by one that runs statements, resets its
// session and says whether it is valid exactly as it does.
func TestTheGuardedVerticaConnectionIsWhatItIs(t *testing.T) {
	assertGuardedLike(t, SQLDriverVertica, &verticaConn{})
}
