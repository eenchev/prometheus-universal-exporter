//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// panickingConnector is a pool's connector whose driver makes one good
// connection and panics on every one after it, as a driver does on an
// answer it reads without checking.
func panickingConnector(t *testing.T) *sql.DB {
	t.Helper()
	stand := &sqltest.DB{Results: map[string]sqltest.Result{}}
	var calls atomic.Int32
	hook := func(ctx context.Context, _ string, _ *sqlConnectSpec) (driver.Conn, error) {
		if calls.Add(1) == 1 {
			return stand.Connect(ctx)
		}
		panic("index out of range [0] with length 0")
	}
	sqlConnectHook.Store(&hook)
	t.Cleanup(func() { sqlConnectHook.Store(nil) })
	pool := &sqlPool{driver: sqlDriverTable[SQLDriverSQLServer], key: sqlPoolKey{collector: "db", address: sqlAddress{host: "127.0.0.1", port: "1433", hostPort: "127.0.0.1:1433"}}}
	db := sql.OpenDB(&sqlConnector{cache: newSQLPoolCache(), pool: pool})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A driver that panics while connecting fails the connection with a
// sentence naming the driver, rather than the process: in the scrape's
// goroutine, where the connection database/sql counted as open is counted
// closed again, so no place at max_per_target is lost; and in database/sql's
// own goroutine that opens connections for the scrapes waiting for one,
// where a panic would have ended the test binary, the waiting scrape is
// given the error.
func TestADriverThatPanicsWhileConnectingFailsTheConnectionNotTheProcess(t *testing.T) {
	db := panickingConnector(t)
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		conn, err := db.Conn(ctx)
		if err == nil {
			_ = conn.Close()
		}
		waited <- err
	}()
	for db.Stats().WaitCount == 0 {
		select {
		case err := <-waited:
			t.Fatalf("the second scrape did not wait: %v", err)
		default:
			runtime.Gosched()
		}
	}
	// database/sql discards a connection the function given Raw fails with
	// driver.ErrBadConn, and opens one for the waiting scrape in its own
	// goroutine.
	_ = first.Raw(func(any) error { return driver.ErrBadConn })
	_ = first.Close()
	err = <-waited
	if err == nil || !strings.Contains(err.Error(), "the sqlserver driver failed on what the server sent (it panicked: index out of range") {
		t.Errorf("the waiting scrape got %v", err)
	}
	if _, err := db.Conn(ctx); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("a connection in the scrape's goroutine gave %v", err)
	}
	if open := db.Stats().OpenConnections; open != 0 {
		t.Errorf("%d connections counted open after every connection failed", open)
	}
}

// pingConnOf pings an idle connection of the pool as the keepalive does.
func pingConnOf(t *testing.T, pool *sqlPool) (bool, error) {
	t.Helper()
	conn, err := pool.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	return pingConn(context.Background(), pool, conn)
}

// pingPanickingConn is a driver's connection whose ping panics.
type pingPanickingConn struct{}

func (pingPanickingConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not used") }
func (pingPanickingConn) Close() error                        { return nil }
func (pingPanickingConn) Begin() (driver.Tx, error)           { return nil, errors.New("not used") }
func (pingPanickingConn) Ping(context.Context) error          { panic("index out of range [1] with length 1") }

// A keepalive ping whose driver panics is a failed ping, not the end of the
// process: the ping runs in the keepalive's own goroutine.
func TestAKeepalivePingWhoseDriverPanicsFails(t *testing.T) {
	hook := func(context.Context, string, *sqlConnectSpec) (driver.Conn, error) { return pingPanickingConn{}, nil }
	sqlConnectHook.Store(&hook)
	t.Cleanup(func() { sqlConnectHook.Store(nil) })
	pool := &sqlPool{driver: sqlDriverTable[SQLDriverMySQL], target: &sqlTarget{sem: make(chan struct{}, 1)},
		key: sqlPoolKey{collector: "db", address: sqlAddress{host: "127.0.0.1", port: "3306", hostPort: "127.0.0.1:3306"}}}
	cache := newSQLPoolCache()
	pool.db = sql.OpenDB(&sqlConnector{cache: cache, pool: pool})
	t.Cleanup(func() { _ = pool.db.Close() })
	conn, err := pool.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	round := keepaliveOnce(cache, pool, time.Minute)
	if round != (sqlKeepaliveRound{failed: 1}) {
		t.Errorf("round %+v, want one failed ping", round)
	}
	_, err = pingConnOf(t, pool)
	if err == nil || !strings.Contains(err.Error(), "the mysql driver failed on what the server sent (it panicked: index out of range [1]") {
		t.Errorf("ping: %v", err)
	}
	if len(pool.target.sem) != 0 {
		t.Error("the ping's place at max_per_target was kept")
	}
}

// A sqlserver server whose prelogin answer has an ENCRYPTION option with no
// byte, which go-mssqldb reads without checking and panics on, fails the
// connection with a sentence saying what is wrong, whatever encrypt says
// but strict, whose answer comes inside TLS and is left to the recovery of
// a panic.
func TestASQLServerAnswerWithAnEmptyEncryptionOptionIsRefused(t *testing.T) {
	for _, options := range []map[string]string{nil, {"encrypt": "false"}, {"encrypt": "true"}, {"encrypt": "disable"}} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			if _, err := tdsReadPacket(conn); err == nil {
				// VERSION, then ENCRYPTION at offset 17 with length 0.
				_, _ = conn.Write(tdsPacket(4, 1, []byte{0x00, 0, 11, 0, 6, 0x01, 0, 17, 0, 0, 0xff, 16, 0, 0x07, 0xd0, 0, 0, 0}))
			}
			_, _ = tdsReadPacket(conn)
		}()
		_, port, _ := net.SplitHostPort(listener.Addr().String())
		config, _, err := sqlTLSConfig(sqlDriverTable[SQLDriverSQLServer], &sqlBase{}, options, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		spec := sqlSpec(t, port, config, options)
		spec.dial = sqlDial(nil, "127.0.0.1", 0)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = sqlConnectRecovering(ctx, SQLDriverSQLServer, connectSQLServer, spec)
		cancel()
		if !errors.Is(err, errSQLServerEmptyEncryption) {
			t.Errorf("%v: %v, want the empty option refused", options, err)
		}
		_ = listener.Close()
	}
}

// The prelogin guard refuses an empty ENCRYPTION option whether or not it
// lets an answer that the server cannot encrypt through, and only when it
// is the option the driver keeps, the last one given.
func TestThePreloginGuardRefusesAnEmptyEncryptionOption(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  []byte
		refused bool
	}{
		{"empty", tdsPacket(4, 1, []byte{0x01, 0, 6, 0, 0, 0xff, 0}), true},
		{"empty after one on", tdsPacket(4, 1, []byte{0x01, 0, 11, 0, 1, 0x01, 0, 12, 0, 0, 0xff, 1, 0}), true},
		{"empty, its offset past the end", tdsPacket(4, 1, []byte{0x01, 0, 6, 0, 0, 0xff}), false},
		{"on after an empty one", tdsPacket(4, 1, []byte{0x01, 0, 11, 0, 0, 0x01, 0, 11, 0, 1, 0xff, 1}), false},
		{"none at all", tdsPacket(4, 1, []byte{0x00, 0, 6, 0, 1, 0xff, 9}), false},
	} {
		for _, allow := range []bool{false, true} {
			client, server := net.Pipe()
			go func() {
				_, _ = server.Write(tc.answer)
				_ = server.Close()
			}()
			buf := make([]byte, 64)
			_, err := (&sqlServerPreloginGuard{Conn: client, allowUnencrypted: allow}).Read(buf)
			_ = client.Close()
			if errors.Is(err, errSQLServerEmptyEncryption) != tc.refused {
				t.Errorf("%s, unencrypted allowed %t: %v", tc.name, allow, err)
			}
		}
	}
	notSup := tdsPreloginAnswer(sqlServerEncryptNotSupported)
	client, server := net.Pipe()
	go func() {
		_, _ = server.Write(notSup)
		_ = server.Close()
	}()
	if _, err := (&sqlServerPreloginGuard{Conn: client, allowUnencrypted: true}).Read(make([]byte, 64)); err != nil {
		t.Errorf("an answer that the server cannot encrypt, let through, failed: %v", err)
	}
	_ = client.Close()
}

// The prelogin guard's reading of the ENCRYPTION option gives what it gave
// before for every answer but one whose option the driver keeps is empty:
// the function as it was is the oracle, over generated option tables.
func TestThePreloginEncryptionIsWhatItWasButForAnEmptyOption(t *testing.T) {
	before := func(payload []byte) int {
		const terminator, traceID, encryption, optionSize = 0xff, 0x05, 0x01, 5
		value := -1
		for offset := 0; ; offset += optionSize {
			if offset >= len(payload) {
				return -1
			}
			token := payload[offset]
			if token == terminator {
				return value
			}
			if offset+optionSize >= len(payload) {
				return -1
			}
			start := int(payload[offset+1])<<8 | int(payload[offset+2])
			length := int(payload[offset+3])<<8 | int(payload[offset+4])
			if token == traceID {
				continue
			}
			if start+length > len(payload) || start >= len(payload) {
				return -1
			}
			if token == encryption {
				value = -1
				if length > 0 {
					value = int(payload[start])
				}
			}
		}
	}
	compared, empty := 0, 0
	for seed := range 4000 {
		// Up to three options of tokens 0, 1, 5 and others, each pointing at
		// data within, at or past the end, then perhaps a terminator.
		n := seed % 4
		payload := make([]byte, 0, 32)
		for i := range n {
			token := []byte{0x00, 0x01, 0x05, 0x02}[(seed>>(2*i+2))%4]
			length := (seed >> (i + 8)) % 3
			start := 5*n + 1 + (seed>>(i+11))%4
			payload = append(payload, token, byte(start>>8), byte(start), 0, byte(length))
		}
		if seed%7 != 0 {
			payload = append(payload, 0xff)
		}
		payload = append(payload, byte(seed), byte(seed>>3), 2, 0)
		got, want := sqlServerPreloginEncryption(payload), before(payload)
		if got == sqlServerEncryptEmpty {
			empty++
			if want != -1 {
				t.Errorf("%x: empty, before %d", payload, want)
			}
			continue
		}
		compared++
		if got != want {
			t.Errorf("%x: %d, before %d", payload, got, want)
		}
	}
	if compared < 1000 || empty < 50 {
		t.Errorf("compared %d answers, %d with an empty option", compared, empty)
	}
}

// The dialer of the sql request type makes TCP connections alone: a driver
// asking for UDP, as go-mssqldb does of the SQL Server Browser, or for any
// other network, is refused with a sentence saying so, under a policy or
// without one, before anything is sent; TCP of either family still
// connects.
func TestTheSQLDialerMakesTCPConnectionsAlone(t *testing.T) {
	allow, err := compileTargetPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []*targetPolicy{nil, allow} {
		for _, network := range []string{"udp", "udp4", "unix", "ip4:1"} {
			conn, err := sqlDial(policy, "127.0.0.1", 0)(context.Background(), network, "127.0.0.1:1434")
			if err == nil {
				_ = conn.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "the driver asked for a "+network+" connection to 127.0.0.1:1434, and the exporter makes TCP connections alone") {
				t.Errorf("%s: %v", network, err)
			}
		}
		for _, network := range []string{"tcp", "tcp4"} {
			conn, err := sqlDial(policy, "127.0.0.1", 0)(context.Background(), network, net.JoinHostPort("127.0.0.1", listen(t)))
			if err != nil {
				t.Errorf("%s: %v", network, err)
				continue
			}
			_ = conn.Close()
		}
	}
}

// postgresEnvironment is every variable pgx reads (pgconn
// parseEnvSettings), PGSERVICE aside, each set to a value unlike what the
// collector says, some that pgx would refuse, and a password file that
// holds a password for the target.
func postgresEnvironment(t *testing.T) map[string]string {
	t.Helper()
	passfile := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(passfile, []byte("*:*:*:*:frompassfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"PGHOST": "elsewhere,another", "PGPORT": "notaport", "PGDATABASE": "envdb", "PGUSER": "envuser", "PGPASSWORD": "env",
		"PGPASSFILE": passfile, "PGAPPNAME": "env", "PGCONNECT_TIMEOUT": "soon", "PGSSLMODE": "verify-full",
		"PGSSLKEY": "/nonexistent/key", "PGSSLCERT": "/nonexistent/cert", "PGSSLSNI": "0", "PGSSLROOTCERT": "/nonexistent/root",
		"PGSSLPASSWORD": "x", "PGSSLNEGOTIATION": "direct", "PGTARGETSESSIONATTRS": "read-write", "PGSERVICEFILE": "/nonexistent/services",
		"PGTZ": "Mars/Olympus", "PGOPTIONS": "-c search_path=evil", "PGMINPROTOCOLVERSION": "3.2", "PGMAXPROTOCOLVERSION": "latest",
		"PGCHANNELBINDING": "require", "PGREQUIREAUTH": "md5",
	}
}

// postgresFields is a pgx configuration's fields as the tests compare
// them: a function by whether it is set, everything else as it is.
func postgresFields(config *pgx.ConnConfig) map[string]any {
	fields := map[string]any{}
	v := reflect.ValueOf(config.Config)
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		if field.Type.Kind() == reflect.Func {
			fields[field.Name] = !v.Field(i).IsNil()
			continue
		}
		fields[field.Name] = v.Field(i).Interface()
	}
	fields["StatementCacheCapacity"] = config.StatementCacheCapacity
	fields["DescriptionCacheCapacity"] = config.DescriptionCacheCapacity
	fields["DefaultQueryExecMode"] = config.DefaultQueryExecMode
	return fields
}

// Every PG* variable pgx reads changes nothing of a postgres connection's
// configuration, those pgx would refuse included: the configuration is the
// one made with none set. PGSERVICE, which pgx would read service settings
// for whatever the connection string says, is refused with a sentence
// saying to unset it.
func TestNoPGVariableChangesThePostgresConfiguration(t *testing.T) {
	environment := postgresEnvironment(t)
	for name := range environment {
		t.Setenv(name, "")
	}
	t.Setenv("PGSERVICE", "")
	specs := []*sqlConnectSpec{sqlSpec(t, "5432", nil, nil), sqlSpec(t, "6543", nil, map[string]string{"application_name": "billing"})}
	var want []map[string]any
	for _, spec := range specs {
		config, err := postgresConfig(spec)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, postgresFields(config))
	}
	for name, value := range environment {
		t.Setenv(name, value)
	}
	for i, spec := range specs {
		config, err := postgresConfig(spec)
		if err != nil {
			t.Fatalf("with the variables set: %v", err)
		}
		got := postgresFields(config)
		for name, value := range want[i] {
			if !reflect.DeepEqual(got[name], value) {
				t.Errorf("%s is %v with the variables set, %v without", name, got[name], value)
			}
		}
	}
	t.Setenv("PGSERVICE", "billing")
	if _, err := postgresConfig(specs[0]); err == nil || !strings.Contains(err.Error(), "the exporter's environment sets PGSERVICE") {
		t.Errorf("with PGSERVICE set: %v", err)
	}
}

// The postgres configuration, with no PG* variable set, is what it was
// before every variable was pinned: the function as it was is the oracle,
// over ports, TLS and options.
func TestThePostgresConfigurationIsWhatItWasWithoutTheVariables(t *testing.T) {
	for name := range postgresEnvironment(t) {
		t.Setenv(name, "")
	}
	t.Setenv("PGSERVICE", "")
	before := func(spec *sqlConnectSpec) (*pgx.ConnConfig, error) {
		config, err := pgx.ParseConfig("sslmode=disable")
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
		config.LookupFunc = func(_ context.Context, host string) ([]string, error) { return []string{host}, nil }
		name := spec.appName
		if value, ok := spec.options["application_name"]; ok {
			name = value
		}
		config.RuntimeParams = map[string]string{"application_name": name}
		return config, nil
	}
	compared := 0
	for _, port := range []string{"5432", "1", "65535", "70000", "x"} {
		for _, config := range []*sqlConnectSpec{sqlSpec(t, port, nil, nil), sqlSpec(t, port, nil, map[string]string{"application_name": "billing", "sslmode": "require"})} {
			got, gotErr := postgresConfig(config)
			want, wantErr := before(config)
			if (gotErr != nil) != (wantErr != nil) {
				t.Errorf("%s: %v, before %v", port, gotErr, wantErr)
				continue
			}
			if gotErr != nil {
				continue
			}
			compared++
			if g, w := postgresFields(got), postgresFields(want); !reflect.DeepEqual(g, w) {
				t.Errorf("%s %v: %v, before %v", port, config.options, g, w)
			}
		}
	}
	if compared < 6 {
		t.Errorf("compared %d configurations", compared)
	}
}

// Every field of pgx's configuration is one this test has looked at: set by
// postgresConfig, pinned by postgresPinned against the PG* variable that
// sets it, or one no variable sets. A field a later pgx adds fails it, to
// be looked at: a variable may set it.
func TestEveryFieldOfThePgxConfigurationIsLookedAt(t *testing.T) {
	known := []string{
		// Set by postgresConfig.
		"Host", "Port", "Database", "User", "Password", "TLSConfig", "ConnectTimeout", "DialFunc", "LookupFunc", "RuntimeParams",
		"KerberosSrvName", "KerberosSpn", "Fallbacks", "SSLNegotiation", "ValidateConnect", "AfterConnect",
		// Pinned by postgresPinned.
		"MinProtocolVersion", "MaxProtocolVersion", "ChannelBinding", "RequireAuth",
		// Set by no variable: pgx's own defaults.
		"BuildFrontend", "MaxProtocolMessageBodyLen", "BuildContextWatcherHandler", "AfterNetConnect", "OnNotice", "OnNotification",
		"OnPgError", "OAuthTokenProvider",
	}
	typ := reflect.TypeFor[pgconn.Config]()
	for i := range typ.NumField() {
		if field := typ.Field(i); field.IsExported() && !slices.Contains(known, field.Name) {
			t.Errorf("pgx's configuration has a field this test has not looked at, %s: check whether a PG* variable sets it, and pin it in postgresPinned or set it in postgresConfig", field.Name)
		}
	}
}

// A driver that dials by itself has its host's addresses held to the
// policy before it connects, and a host whose addresses could not be
// looked up fails, saying why, since the driver would look the name up
// itself and connect to what that gives; a host behind a proxy keeps the
// proxy's rule, passing on its name when the collector names no address.
func TestAnOwnDialDriversHostThatCouldNotBeLookedUpFails(t *testing.T) {
	restore := resolveHost
	t.Cleanup(func() { resolveHost = restore })
	resolveHost = func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("lookup timed out")
	}
	policy, err := compileTargetPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.checkOwnDial(context.Background(), "db.example.com"); err == nil ||
		err.Error() != "db.example.com has to be looked up here to hold its addresses to the target lists before the driver, which dials by itself, connects, and the lookup failed: lookup timed out" {
		t.Errorf("%v", err)
	}
	if _, err := policy.check(context.Background(), "db.example.com", true); err != nil {
		t.Errorf("behind a proxy: %v", err)
	}
	if err := policy.checkOwnDial(context.Background(), "127.0.0.1"); err != nil {
		t.Errorf("an address needs no lookup: %v", err)
	}
	resolveHost = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	if err := policy.checkOwnDial(context.Background(), "db.example.com"); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("a name resolving to a cloud metadata address: %v", err)
	}
}

// The policy's check before a request gives what it gave before the check
// of a driver that dials by itself was added beside it, for every policy,
// host, lookup and resolve: the function as it was is the oracle.
func TestThePolicyCheckIsWhatItWas(t *testing.T) {
	restore := resolveHost
	t.Cleanup(func() { resolveHost = restore })
	before := func(p *targetPolicy, ctx context.Context, host string, resolve bool) (bool, error) {
		host, err := canonicalHost(host)
		if err != nil {
			return false, err
		}
		nameAllowed, err := p.checkName(host)
		if err != nil || !p.needsAddrs(nameAllowed) {
			return nameAllowed, err
		}
		if addr, literal := literalAddr(host); literal {
			return nameAllowed, p.checkAddr(host, addr, nameAllowed)
		}
		if !resolve {
			return nameAllowed, nil
		}
		addrs, err := resolveHost(ctx, host)
		if err != nil {
			return nameAllowed, p.unresolved(host, nameAllowed, err)
		}
		for _, addr := range addrs {
			if err := p.checkAddr(host, addr, nameAllowed); err != nil {
				return nameAllowed, err
			}
		}
		return nameAllowed, nil
	}
	lookups := map[string]func(context.Context, string) ([]netip.Addr, error){
		"fails": func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") },
		"public": func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("192.0.2.7")}, nil
		},
		"loopback": func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		"metadata": func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		},
	}
	lists := [][2][]string{{nil, nil}, {{"db.example.com"}, nil}, {{"192.0.2.0/24"}, nil}, {nil, {"127.0.0.0/8"}}, {{"*.example.com"}, {"192.0.2.7"}}}
	compared := 0
	for _, list := range lists {
		policy, err := compileTargetPolicy(list[0], list[1])
		if err != nil {
			t.Fatal(err)
		}
		for name, lookup := range lookups {
			resolveHost = lookup
			for _, host := range []string{"db.example.com", "other.test", "127.0.0.1", "192.0.2.7", "[::1]", "0x7f.1", "DB.Example.COM."} {
				for _, resolve := range []bool{false, true} {
					gotAllowed, gotErr := policy.check(context.Background(), host, resolve)
					wantAllowed, wantErr := before(policy, context.Background(), host, resolve)
					if gotAllowed != wantAllowed || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
						t.Errorf("%v %s %s %t: %t %v, before %t %v", list, name, host, resolve, gotAllowed, gotErr, wantAllowed, wantErr)
					}
					compared++
				}
			}
		}
	}
	if compared != len(lists)*len(lookups)*7*2 {
		t.Errorf("compared %d", compared)
	}
}

// The tls keys that say how the server's certificate is verified, ca_file
// and server_name, beside a TLS option that does not verify it are refused
// at load with a sentence naming the option that does; a client
// certificate beside it, and either key beside the verifying value or no
// option, is accepted.
func TestVerificationKeysBesideAnOptionThatDoesNotVerifyAreRefused(t *testing.T) {
	for _, tc := range []struct {
		driver, option, insecure, verifying string
	}{
		{SQLDriverPostgres, "sslmode", "require", "verify-full"},
		{SQLDriverMySQL, "tls", "skip-verify", "true"},
	} {
		d := sqlDriverTable[tc.driver]
		for _, keys := range []model.TLSConfig{{CAFile: "/etc/ssl/db-ca.pem"}, {ServerName: "db.internal"}} {
			err := checkSQLOptions(d, map[string]string{tc.option: tc.insecure}, keys)
			want := fmt.Sprintf("sets request.tls.ca_file or server_name, which say how the server's certificate is verified, but request.options.%s is %q, which does not verify it; set %s to %s to verify it with them, or leave them out",
				tc.option, tc.insecure, tc.option, tc.verifying)
			if err == nil || err.Error() != want {
				t.Errorf("%s %+v: %v", tc.driver, keys, err)
			}
			if err := checkSQLOptions(d, map[string]string{tc.option: tc.verifying}, keys); err != nil {
				t.Errorf("%s %+v beside %s: %v", tc.driver, keys, tc.verifying, err)
			}
			if err := checkSQLOptions(d, nil, keys); err != nil {
				t.Errorf("%s %+v with no option: %v", tc.driver, keys, err)
			}
		}
		if err := checkSQLOptions(d, map[string]string{tc.option: tc.insecure}, model.TLSConfig{CertFile: "c.pem", KeyFile: "k.pem"}); err != nil {
			t.Errorf("%s: a client certificate beside %s: %v", tc.driver, tc.insecure, err)
		}
		c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: tc.driver, Statement: "SELECT 1", Username: "u",
			Options: map[string]string{tc.option: tc.insecure}, TLS: model.TLSConfig{CAFile: "/etc/ssl/db-ca.pem"}}}
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), "which does not verify it") {
			t.Errorf("%s: loading gives %v", tc.driver, err)
		}
	}
}

// The check of a driver's options and tls keys gives what it gave before
// for every combination but a verification key beside an option that does
// not verify: the function as it was is the oracle, over every driver built
// in, every value of its TLS option and a table of tls settings.
func TestTheSQLOptionsCheckIsWhatItWasButForVerificationKeys(t *testing.T) {
	before := func(d *sqlDriver, options map[string]string, tlsKeys model.TLSConfig) error {
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
		tlsSet := tlsKeys != model.TLSConfig{}
		if (tlsKeys.CertFile == "") != (tlsKeys.KeyFile == "") {
			return errors.New("request.tls sets only one of cert_file and key_file; a client certificate needs both")
		}
		for _, key := range d.refusedTLS {
			if key == "server_name" && tlsKeys.ServerName != "" {
				return fmt.Errorf("sets request.tls.server_name, which the %s driver cannot honour: it checks the certificate against the target's host", d.name)
			}
		}
		if tlsSet && sqlTLSModeOf(d, options, tlsSet) == sqlTLSOff {
			return fmt.Errorf("sets request.tls, but request.options.%s is %q, which uses no TLS; leave tls out, or set %s to a value that uses TLS", d.tlsOption, options[d.tlsOption], d.tlsOption)
		}
		return nil
	}
	settings := []model.TLSConfig{{}, {CAFile: "ca"}, {ServerName: "db"}, {CertFile: "c", KeyFile: "k"}, {CertFile: "c"}, {InsecureSkipVerify: true},
		{CAFile: "ca", ServerName: "db", CertFile: "c", KeyFile: "k"}, {InsecureSkipVerify: true, ServerName: "db"}}
	compared, changed := 0, 0
	for name, d := range sqlDriverTable {
		values := []string{"", "bogus"}
		for value := range d.tlsModes {
			values = append(values, value)
		}
		for _, value := range values {
			var options map[string]string
			if value != "" && d.tlsOption != "" {
				options = map[string]string{d.tlsOption: value}
			}
			for _, keys := range settings {
				got, want := checkSQLOptions(d, options, keys), before(d, options, keys)
				if want == nil && sqlTLSModeOf(d, options, true) == sqlTLSInsecure && (keys.CAFile != "" || keys.ServerName != "") {
					changed++
					if got == nil {
						t.Errorf("%s %v %+v: accepted", name, options, keys)
					}
					continue
				}
				compared++
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%s %v %+v: %v, before %v", name, options, keys, got, want)
				}
			}
		}
	}
	if compared < 100 || changed < 4 {
		t.Errorf("compared %d, changed %d", compared, changed)
	}
}
