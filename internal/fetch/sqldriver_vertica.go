//go:build (!select_request_types || request_type_sql) && sql_driver_vertica

package fetch

import (
	"context"
	"crypto/tls"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	vertigo "github.com/vertica/vertica-sql-go"
	verticalog "github.com/vertica/vertica-sql-go/logger"
	"github.com/vertica/vertica-sql-go/parse"
)

// The vertica driver is vertica/vertica-sql-go: built in only with the
// sql_driver_vertica tag (SQL_DRIVERS=vertica). A statement runs in a
// transaction started READ ONLY. The driver dials by itself, taking neither
// a dialer nor a context: the target's host and every address it resolves
// to are held to the policy before a connection is made, a host that could
// not be looked up failing, the connection's TCP keepalives are the
// driver's own, and a connection that is not made by the scrape's deadline
// is left to finish and then closed. Every call on a connection is bounded
// by its context here (verticaConn), the driver never seeing a context that
// ends, so that it makes no cancel request of its own. tlsmode is none,
// server (TLS, the certificate not verified) or server-strict.
func init() {
	verticalog.SetLogger(verticaLog{})
	registerSQLDriver(&sqlDriver{
		name:      SQLDriverVertica,
		port:      func(map[string]string) string { return "5433" },
		options:   map[string][]string{"tlsmode": {"none", "server", "server-strict"}},
		tlsOption: "tlsmode",
		tlsModes:  map[string]sqlTLSMode{"none": sqlTLSOff, "server": sqlTLSInsecure, "server-strict": sqlTLSVerify},
		ownDial:   true,
		connect:   connectVertica,
		run:       sqlReadOnlyTx(nil),
		readOnly:  "transaction",
		statement: verticaSingleStatement,
	})
}

// verticaLog hands the driver's log to the exporter's logger, slog's
// default, which main makes the JSON lines every other line is: the driver
// writes its own (warnings and errors, unless VERTICA_SQL_GO_LOG_LEVEL asks
// for more) to stdout in a format of its own, or to the file
// VERTICA_SQL_GO_LOG_FILE names, which this replaces. A warning is a
// warning, an error or fatal message an error, and the rest debug.
type verticaLog struct{}

func (verticaLog) Write(prefix, name, msg string) {
	level := slog.LevelDebug
	switch prefix {
	case "WARN":
		level = slog.LevelWarn
	case "ERROR", "FATAL":
		level = slog.LevelError
	}
	slog.Default().Log(context.Background(), level, msg, "sql_driver", SQLDriverVertica, "driver_logger", name)
}

func (verticaLog) Close() {}

// verticaSingleStatement refuses a statement that holds more than one. The
// driver splits what it is given (vertica-sql-go stmt.go, newStmt and
// QueryContext, by parse.SplitStatements) and runs each part on its own, so
// a COMMIT among them would end the read-only transaction and the
// statements after it would run outside it. Since vertica-sql-go 1.3.9 the
// driver no longer splits at a ; inside what it takes for a BEGIN ... END,
// CASE, IF, LOOP or WHILE block, and sends such text to the server whole,
// which runs every statement in it: "BEGIN; COMMIT; DROP TABLE t" and even
// "SELECT 1 AS begin; COMMIT" reach the server as one query. So the
// driver's splitting is not enough to go by: a statement is refused when
// the driver would split it, as written or as its lexer rewrites the
// placeholders of, or when verticaParts finds a ; followed by anything but
// blanks and comments outside quotes, quoted identifiers, comments and
// dollar quotes, whatever block it seems to stand in. A statement the
// driver's lexer fails on is refused too (verticaLex).
func verticaSingleStatement(statement string) error {
	lexed, ok := verticaLex(statement)
	if !ok {
		return errors.New("request.statement is one the vertica driver's lexer fails on, as it does on a ? right after a quote it takes to end a string " +
			"(\"'?\", or a ' and a ? at the start of a line after a -- comment); write a blank or a parenthesis between them")
	}
	if len(parse.SplitStatements(statement)) > 1 || len(parse.SplitStatements(lexed)) > 1 ||
		verticaParts(statement) > 1 || verticaParts(lexed) > 1 {
		return errors.New("request.statement holds more than one statement, which the vertica driver would run one after another, " +
			"the later ones outside the read-only transaction; write a single statement (a ; may end it, followed by nothing but blanks and comments)")
	}
	return nil
}

// verticaLex is statement as the driver's lexer rewrites its placeholders
// (parse.Lex), which it does to every statement it prepares (stmt.go,
// newStmt), and false where the lexer panics: on a ? right after a ' it
// begins a string state at, as at the start of the text or of a line after a
// -- comment ("'?"), it slices before what it wrote. The driver would panic
// on such a statement at every scrape, and the check, made where a
// configuration is loaded, would take the exporter down with it.
func verticaLex(statement string) (lexed string, ok bool) {
	defer func() {
		if recover() != nil {
			lexed, ok = "", false
		}
	}()
	return parse.Lex(statement), true
}

// verticaParts counts the statements in statement as Vertica's SQL has
// them, every ; outside a quote ending one: 'text' with a doubled quote
// inside, "name" with "" inside, -- to the end of the line, /* ... */, and
// $tag$ ... $tag$. A part of nothing but blanks (ASCII white space) and
// comments is no statement. It knows no blocks, so a ; never stands inside
// one, and a // is not taken for a comment, as the driver since 1.3.9 does
// not take it for one either: a statement that is one only if those were
// honoured is refused, which costs a metrics query nothing.
//
// Where the server's reading is not the driver's, or not one, the text is
// read every way the server may read it, and the most statements any
// reading finds are counted (verticaReadings): the driver, and the first
// reading, take a $ anywhere for the start of a dollar quote and a \ for
// nothing; the server, like PostgreSQL, whose SQL Vertica's follows, takes
// a $ inside a word for part of it ("SELECT a$b$; COMMIT; SELECT $b$x$b$"
// is three statements), a \ in an E'...' to escape the next character, a
// quote among them, so that the string ends at a quote the driver takes for
// half of a doubled one, and in every '...' with
// standard_conforming_strings off, and may nest block comments.
func verticaParts(statement string) int {
	most := 0
	for _, reading := range verticaReadings {
		most = max(most, verticaPartsAs(statement, reading))
	}
	return most
}

// verticaReading is one way of reading a statement's quotes and comments.
type verticaReading struct {
	// words takes a $ inside a word, after a letter, a digit, a _, a $ or
	// a byte of a multi-byte character, for part of it rather than the
	// start of a dollar quote.
	words bool
	// escapes takes a \ in an E'...' begun outside a word to escape the
	// next byte, and backslashes in every '...'.
	escapes, backslashes bool
	// nested lets block comments nest.
	nested bool
}

// verticaReadings are the driver's reading and the server's.
var verticaReadings = []verticaReading{
	{},
	{words: true, escapes: true},
	{words: true, escapes: true, nested: true},
	{words: true, escapes: true, backslashes: true},
	{words: true, escapes: true, backslashes: true, nested: true},
}

// verticaWordByte reports whether c may stand inside a word of Vertica's
// SQL: a letter, a digit, _, $, or a byte of a multi-byte character.
func verticaWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// verticaPartsAs counts the statements in statement as reading reads it.
func verticaPartsAs(statement string, reading verticaReading) int {
	parts, content := 0, false
	end := func() {
		if content {
			parts++
		}
		content = false
	}
	for i := 0; i < len(statement); i++ {
		ch := statement[i]
		switch {
		case ch == '\'' || ch == '"':
			content = true
			escapes := ch == '\'' && (reading.backslashes || reading.escapes && i > 0 &&
				(statement[i-1] == 'E' || statement[i-1] == 'e') && (i == 1 || !verticaWordByte(statement[i-2])))
			for i++; i < len(statement); i++ {
				if escapes && statement[i] == '\\' {
					i++
					continue
				}
				if statement[i] == ch {
					if i+1 < len(statement) && statement[i+1] == ch {
						i++
						continue
					}
					break
				}
			}
		case ch == '-' && i+1 < len(statement) && statement[i+1] == '-':
			for i < len(statement) && statement[i] != '\n' && statement[i] != '\r' {
				i++
			}
		case ch == '/' && i+1 < len(statement) && statement[i+1] == '*':
			depth := 1
			for i += 2; i < len(statement) && depth > 0; i++ {
				switch {
				case reading.nested && statement[i] == '/' && i+1 < len(statement) && statement[i+1] == '*':
					depth++
					i++
				case statement[i] == '*' && i+1 < len(statement) && statement[i+1] == '/':
					depth--
					i++
				}
			}
			i--
		case ch == '$' && (!reading.words || i == 0 || !verticaWordByte(statement[i-1])):
			content = true
			if tag, ok := verticaDollarTag(statement[i:]); ok {
				if found := strings.Index(statement[i+len(tag):], tag); found >= 0 {
					i += len(tag) + found + len(tag) - 1
				} else {
					i = len(statement)
				}
			}
		case ch == ';':
			end()
		case ch != ' ' && ch != '\t' && ch != '\n' && ch != '\r' && ch != '\f' && ch != '\v':
			content = true
		}
	}
	end()
	return parts
}

// verticaDollarTag is the $tag$ that s begins with, tag letters, digits and
// _ and not beginning with a digit, or $$.
func verticaDollarTag(s string) (string, bool) {
	for j := 1; j < len(s); j++ {
		c := s[j]
		switch {
		case c == '$':
			return s[:j+1], true
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || (c >= '0' && c <= '9' && j > 1):
		default:
			return "", false
		}
	}
	return "", false
}

// verticaTLS hands out the names TLS configurations are registered with
// the driver by. The driver keeps what is registered in a map for good, a
// name registered again taking the new configuration (connection.go
// RegisterTLSConfig, _tlsConfigs.add), and looks the configuration up by
// the URL's tlsmode only while it opens a connection (connection.go
// newConnection, initializeSSL). So each connection is made with a name of
// its own, registered with the configuration made for it, from the files
// as they are read for it, and the name is given back once the connection
// is open or has failed, for the next connection to register its own
// under: there are never more names than connections ever opened at once,
// however often the files change, and a connection never takes another's
// configuration.
var verticaTLS struct {
	mu sync.Mutex
	// free are the names given back.
	free []string
	// names counts the names made.
	names int
}

// verticaTLSName registers config with the driver under a name no
// connection being opened holds, and returns it, with the function that
// gives it back.
func verticaTLSName(config *tls.Config) (name string, release func(), err error) {
	verticaTLS.mu.Lock()
	if n := len(verticaTLS.free); n > 0 {
		name, verticaTLS.free = verticaTLS.free[n-1], verticaTLS.free[:n-1]
	} else {
		verticaTLS.names++
		// The driver reads tlsmode in lower case (connection.go
		// newConnection), as the name is.
		name = "prometheus-universal-exporter-" + strconv.Itoa(verticaTLS.names)
	}
	verticaTLS.mu.Unlock()
	release = func() {
		verticaTLS.mu.Lock()
		verticaTLS.free = append(verticaTLS.free, name)
		verticaTLS.mu.Unlock()
	}
	if err := vertigo.RegisterTLSConfig(name, config); err != nil {
		release()
		return "", nil, err
	}
	return name, release, nil
}

// verticaURL is the driver's URL of a connection made with spec, with its
// TLS mode: none, or the name its TLS configuration is registered by.
func verticaURL(spec *sqlConnectSpec, tlsMode string) string {
	query := url.Values{"client_label": {spec.appName}, "use_prepared_statements": {"1"}, "tlsmode": {tlsMode}}
	u := url.URL{Scheme: "vertica", User: url.UserPassword(spec.username, spec.password), Host: spec.hostPort, Path: "/" + spec.database, RawQuery: query.Encode()}
	return u.String()
}

func connectVertica(ctx context.Context, spec *sqlConnectSpec) (driver.Conn, error) {
	conn, err := connectWithin(ctx, func() (driver.Conn, error) {
		tlsMode := "none"
		if spec.tls != nil {
			name, release, err := verticaTLSName(spec.tls)
			if err != nil {
				return nil, err
			}
			defer release()
			tlsMode = name
		}
		return (&vertigo.Driver{}).Open(verticaURL(spec, tlsMode))
	})
	if err != nil {
		return nil, err
	}
	return newVerticaConn(conn)
}

// connectWithin runs open, a driver's connect that takes no context, and
// returns what it made, or the context's error when it ends first: the
// connection open makes after that is closed. A panic of the driver, in the
// goroutine open runs in, is its error.
func connectWithin(ctx context.Context, open func() (driver.Conn, error)) (driver.Conn, error) {
	type made struct {
		conn driver.Conn
		err  error
	}
	done := make(chan made, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- made{nil, sqlDriverPanic(SQLDriverVertica, r)}
			}
		}()
		conn, err := open()
		done <- made{conn, err}
	}()
	select {
	case m := <-done:
		return m.conn, m.err
	case <-ctx.Done():
		go func() {
			if m := <-done; m.conn != nil {
				_ = m.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// verticaDriverConn is what vertica-sql-go's connection is to database/sql
// (connection.go): a connection that begins transactions, prepares
// statements, answers pings and resets its session.
type verticaDriverConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.Pinger
	driver.SessionResetter
}

// verticaDriverStmt is what its statement is (stmt.go).
type verticaDriverStmt interface {
	driver.Stmt
	driver.StmtExecContext
	driver.StmtQueryContext
}

// verticaConn is a vertica connection whose every call returns by the end
// of its context, or, for one that takes none (a transaction's commit and
// rollback, a statement's close), of the context it was begun or prepared
// with. The driver reads with no deadline (connection.go readAll), and
// stops a statement only by a cancel request it sends when the statement's
// context ends, over a connection it dials itself with no timeout, after a
// lookup of its own, past the exporter's dialer and the target lists, and
// unencrypted whatever tlsmode says (stmt.go QueryContextRaw,
// connection.go establishSocketConnection and sendMessageTo). So the driver
// is given contexts that never end, and makes no cancel request; each call
// runs in a goroutine of its own, and one still running at its context's
// end is left to finish there, the connection given up: it is invalid to
// database/sql from then on, which closes it, and its driver connection is
// closed when that call returns.
type verticaConn struct {
	conn verticaDriverConn
	// slot is held while the driver runs on the connection: by a call, and,
	// after a call gave up at its context's end, by its goroutine until the
	// driver returns.
	slot chan struct{}
	// broken says the connection is given up: a call gave up, or was not
	// run, and what the connection is in the middle of is unknown.
	broken atomic.Bool
}

// errVerticaConn is the error of a vertica connection that is not the
// driver's as the exporter knows it.
var errVerticaConn = errors.New("the vertica driver made a connection of a kind the exporter does not know; build the exporter with the vertica-sql-go version its go.mod names")

func newVerticaConn(conn driver.Conn) (driver.Conn, error) {
	inner, ok := conn.(verticaDriverConn)
	if !ok {
		_ = conn.Close()
		return nil, errVerticaConn
	}
	return &verticaConn{conn: inner, slot: make(chan struct{}, 1)}, nil
}

// within runs call on the connection, in a goroutine, with a context that
// does not end, and returns what it returns, or, when ctx ends first, an
// error saying so, the connection given up.
func (c *verticaConn) within(ctx context.Context, call func(ctx context.Context) error) error {
	if c.broken.Load() {
		return driver.ErrBadConn
	}
	select {
	case c.slot <- struct{}{}:
	case <-ctx.Done():
		c.broken.Store(true)
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-c.slot
		c.broken.Store(true)
		return err
	}
	done := make(chan error, 1)
	go func() {
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					c.broken.Store(true)
					err = sqlDriverPanic(SQLDriverVertica, r)
				}
			}()
			return call(context.WithoutCancel(ctx))
		}()
		<-c.slot
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		c.broken.Store(true)
		return fmt.Errorf("the vertica server had not answered when the scrape's deadline came, and the connection is given up: %w", ctx.Err())
	}
}

func (c *verticaConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *verticaConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	var stmt driver.Stmt
	if err := c.within(ctx, func(ctx context.Context) (err error) {
		stmt, err = c.conn.PrepareContext(ctx, query)
		return err
	}); err != nil {
		return nil, err
	}
	inner, ok := stmt.(verticaDriverStmt)
	if !ok {
		_ = stmt.Close()
		c.broken.Store(true)
		return nil, errVerticaConn
	}
	return &verticaStmt{conn: c, stmt: inner, ctx: ctx}, nil
}

func (c *verticaConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *verticaConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	var tx driver.Tx
	if err := c.within(ctx, func(ctx context.Context) (err error) {
		tx, err = c.conn.BeginTx(ctx, opts)
		return err
	}); err != nil {
		return nil, err
	}
	return &verticaTx{conn: c, tx: tx, ctx: ctx}, nil
}

func (c *verticaConn) Ping(ctx context.Context) error {
	return c.within(ctx, c.conn.Ping)
}

func (c *verticaConn) ResetSession(ctx context.Context) error {
	return c.within(ctx, c.conn.ResetSession)
}

// IsValid says the connection is not given up, for database/sql, which
// closes one that is rather than keep it.
func (c *verticaConn) IsValid() bool {
	return !c.broken.Load()
}

// Close closes the driver's connection, at once, or, given up, when the
// call still running on it returns.
func (c *verticaConn) Close() error {
	closeConn := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = sqlDriverPanic(SQLDriverVertica, r)
			}
		}()
		return c.conn.Close()
	}
	if c.broken.Load() {
		go func() {
			c.slot <- struct{}{}
			_ = closeConn()
		}()
		return nil
	}
	return closeConn()
}

// verticaTx is a transaction of a verticaConn, its commit and rollback
// bounded by the context it was begun with.
type verticaTx struct {
	conn *verticaConn
	tx   driver.Tx
	// ctx bounds the commit and the rollback, which take no context.
	ctx context.Context
}

func (t *verticaTx) Commit() error {
	return t.conn.within(t.ctx, func(context.Context) error { return t.tx.Commit() })
}

func (t *verticaTx) Rollback() error {
	return t.conn.within(t.ctx, func(context.Context) error { return t.tx.Rollback() })
}

// verticaStmt is a statement of a verticaConn, each call bounded by its
// context, or, for one that takes none, by the one it was prepared with.
// Its rows are read whole by the driver before they are returned
// (stmt.go collectResults), so reading them waits for nothing.
type verticaStmt struct {
	conn *verticaConn
	stmt verticaDriverStmt
	// ctx bounds Close and the calls of old, which take no context.
	ctx context.Context
}

func (s *verticaStmt) Close() error {
	return s.conn.within(s.ctx, func(context.Context) error { return s.stmt.Close() })
}

func (s *verticaStmt) NumInput() int { return s.stmt.NumInput() }

func (s *verticaStmt) Exec(args []driver.Value) (driver.Result, error) {
	var result driver.Result
	err := s.conn.within(s.ctx, func(context.Context) (err error) {
		result, err = s.stmt.Exec(args)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *verticaStmt) Query(args []driver.Value) (driver.Rows, error) {
	var rows driver.Rows
	err := s.conn.within(s.ctx, func(context.Context) (err error) {
		rows, err = s.stmt.Query(args)
		return err
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *verticaStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	var result driver.Result
	err := s.conn.within(ctx, func(ctx context.Context) (err error) {
		result, err = s.stmt.ExecContext(ctx, args)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *verticaStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	var rows driver.Rows
	err := s.conn.within(ctx, func(ctx context.Context) (err error) {
		rows, err = s.stmt.QueryContext(ctx, args)
		return err
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}
