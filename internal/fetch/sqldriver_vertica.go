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
	"unicode"

	vertigo "github.com/vertica/vertica-sql-go"
	verticalog "github.com/vertica/vertica-sql-go/logger"
	"github.com/vertica/vertica-sql-go/parse"
)

// The vertica driver is vertica/vertica-sql-go. It is built in only with the
// sql_driver_vertica tag (SQL_DRIVERS=vertica). A statement runs in a
// transaction started READ ONLY. The driver dials by itself and takes
// neither a dialer nor a context. The target's host and every address it
// resolves to are held to the policy before a connection is made, and a
// host that cannot be looked up fails. The connection's TCP keepalives are
// the driver's own. A connection that is not made by the scrape's deadline
// is left to finish and then closed. Every call on a connection is bounded
// by its context here (verticaConn). The driver never sees a context that
// ends, so it makes no cancel request of its own. tlsmode is none, server
// (TLS, the certificate not verified) or server-strict.
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
		statement: verticaStatement,

		serverError: verticaServerError,
	})
}

// verticaServerError recognises the server's ErrorResponse, which the
// driver gives as a VError, by its SQLSTATE.
func verticaServerError(err error) (string, bool) {
	var vErr *vertigo.VError
	if !errors.As(err, &vErr) {
		return "", false
	}
	return "SQLSTATE " + vErr.SQLState, true
}

// verticaLog sends the driver's log to the exporter's logger (slog's
// default), which main sets up to write the same JSON lines as every other
// line. It replaces the driver's own output: warnings and errors (more if
// VERTICA_SQL_GO_LOG_LEVEL asks) to stdout in its own format, or to the
// file VERTICA_SQL_GO_LOG_FILE names. A warning maps to a warning, an error
// or fatal message to an error, and the rest to debug.
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

// verticaStatement refuses a statement that the vertica driver would not run
// as one statement prepared on the server with its args bound. That is a
// statement that holds more than one (verticaSingleStatement), or one the
// driver sends unprepared, as text (verticaUnprepared).
func verticaStatement(statement string) error {
	if err := verticaSingleStatement(statement); err != nil {
		return err
	}
	return verticaUnprepared(statement)
}

// verticaUnprepared refuses a statement that the driver sends with the
// simple query protocol instead of preparing it (connection.go
// PrepareContext, stmt.go QueryContextRaw). There are two kinds:
//   - a COPY ... FROM LOCAL, which the driver finds by its own reading of
//     the text as the lexer rewrote it (stmt.go isLocalCopyStatement,
//     analyzeLocalCopyStatement, topLevelSQLTokens);
//   - a function's DDL or an EXECUTE grant on one, which its UDSFAnalyzer
//     finds in the text as written (newStmt).
//
// The driver writes the args of such a statement into its text (stmt.go
// interpolate, formatArg). It doubles a ' but leaves a \ as it is. In an
// E'...', or with standard_conforming_strings off, that \ escapes the
// closing quote. So a probe's value ending in a \ ends the string at the
// next quote, and what follows is SQL. Every other statement is a single
// one (the URL sets use_prepared_statements=1, which no option overrides).
// It is prepared, and its args are bound to it as values (stmt.go
// bindAndExecute). A COPY is refused however the driver or the server reads
// it. A COPY loads rows into a table, which no metrics query does and the
// read-only transaction is there to refuse. A COPY ... FROM LOCAL would
// read a file of the exporter's host into the database at every scrape.
func verticaUnprepared(statement string) error {
	lexed, _ := verticaLex(statement)
	isCopy, local := false, false
	for _, text := range []string{statement, lexed} {
		if words := verticaDriverWords(text); len(words) > 0 && words[0] == "COPY" {
			isCopy = true
			for i := 1; i+1 < len(words); i++ {
				local = local || words[i] == "FROM" && words[i+1] == "LOCAL"
			}
		}
		for _, nested := range []bool{false, true} {
			isCopy = isCopy || strings.EqualFold(verticaFirstWord(text, nested), "COPY")
		}
	}
	switch {
	case local:
		return errors.New("request.statement is a COPY ... FROM LOCAL, which would read a file of the exporter's host into the database at every scrape, " +
			"and which the vertica driver sends unprepared, writing the args into its text, where a value ending in a \\ can end a string early; " +
			"a sql collector runs a query, such as a SELECT")
	case isCopy:
		return errors.New("request.statement is a COPY, which loads rows into a table, as the read-only transaction is there to refuse; " +
			"a sql collector runs a query, such as a SELECT")
	case vertigo.NewUDSFAnalyzer().ShouldTreatAsAtomicUnit(statement):
		return errors.New("request.statement is one the vertica driver takes for a function's DDL or an EXECUTE grant on one " +
			"(CREATE, ALTER or DROP FUNCTION, GRANT or REVOKE EXECUTE ON FUNCTION), which it sends unprepared, with no arg bound to it, " +
			"and the read-only transaction refuses; a sql collector runs a query, such as a SELECT")
	}
	return nil
}

// verticaDriverWords are the words of statement, in upper case, as the
// driver reads them to find a COPY ... FROM LOCAL (stmt.go
// topLevelSQLTokens). A word is a run of ASCII letters, digits and _ outside
// '...' and "..." (with a doubled quote inside), -- to the end of the line,
// /* ... */ (not nested) and $tag$ ... $tag$. A tag's bytes are taken as
// runes, as the driver takes them (readDollarTag, isDollarTagRune). A word
// after a quote or a comment counts as much as one before it, as it does to
// the driver. The driver's first word decides the protocol, whatever the
// server makes of the text: "/* /* */ COPY t FROM LOCAL 'f' */ SELECT ?" is
// a SELECT to a server that nests comments, and a COPY ... FROM LOCAL to the
// driver.
func verticaDriverWords(statement string) []string {
	var words []string
	start := -1
	flush := func(i int) {
		if start >= 0 {
			words = append(words, strings.ToUpper(statement[start:i]))
			start = -1
		}
	}
	for i := 0; i < len(statement); i++ {
		ch := statement[i]
		switch {
		case ch == '\'' || ch == '"':
			flush(i)
			for i++; i < len(statement); i++ {
				if statement[i] == ch {
					if i+1 < len(statement) && statement[i+1] == ch {
						i++
						continue
					}
					break
				}
			}
		case ch == '-' && i+1 < len(statement) && statement[i+1] == '-':
			flush(i)
			for i < len(statement) && statement[i] != '\n' && statement[i] != '\r' {
				i++
			}
		case ch == '/' && i+1 < len(statement) && statement[i+1] == '*':
			flush(i)
			end := strings.Index(statement[i+2:], "*/")
			if end < 0 {
				i = len(statement)
			} else {
				i += 2 + end + 1
			}
		case ch == '$' && verticaDriverDollarTag(statement[i:]) != "":
			flush(i)
			tag := verticaDriverDollarTag(statement[i:])
			if end := strings.Index(statement[i+len(tag):], tag); end < 0 {
				i = len(statement)
			} else {
				i += len(tag) + end + len(tag) - 1
			}
		case ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_':
			if start < 0 {
				start = i
			}
		default:
			flush(i)
		}
	}
	flush(len(statement))
	return words
}

// verticaDriverDollarTag is the $tag$ s begins with as the driver reads one
// (stmt.go readDollarTag): letters and digits, each byte taken for the
// rune of its value, and _, or "".
func verticaDriverDollarTag(s string) string {
	for j := 1; j < len(s); j++ {
		r := rune(s[j])
		if s[j] == '$' {
			return s[:j+1]
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			break
		}
	}
	return ""
}

// verticaFirstWord is the word statement begins with as the server reads
// it, after blanks and comments, block comments nested or not: letters,
// digits, _, $ and the bytes of multi-byte characters.
func verticaFirstWord(statement string, nested bool) string {
	i := 0
	for i < len(statement) {
		switch ch := statement[i]; {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\f' || ch == '\v':
			i++
		case ch == '-' && i+1 < len(statement) && statement[i+1] == '-':
			for i < len(statement) && statement[i] != '\n' && statement[i] != '\r' {
				i++
			}
		case ch == '/' && i+1 < len(statement) && statement[i+1] == '*':
			depth := 1
			for i += 2; i < len(statement) && depth > 0; i++ {
				switch {
				case nested && statement[i] == '/' && i+1 < len(statement) && statement[i+1] == '*':
					depth++
					i++
				case statement[i] == '*' && i+1 < len(statement) && statement[i+1] == '/':
					depth--
					i++
				}
			}
		default:
			end := i
			for end < len(statement) && verticaWordByte(statement[end]) {
				end++
			}
			return statement[i:end]
		}
	}
	return ""
}

// verticaSingleStatement refuses a statement that holds more than one. The
// driver splits what it is given (vertica-sql-go stmt.go, newStmt and
// QueryContext, by parse.SplitStatements) and runs each part on its own. A
// COMMIT among them would end the read-only transaction, and the statements
// after it would run outside it. Since vertica-sql-go 1.3.9 the driver no
// longer splits at a ; inside what it takes for a BEGIN ... END, CASE, IF,
// LOOP or WHILE block. It sends such text to the server whole, and the server
// runs every statement in it: "BEGIN; COMMIT; DROP TABLE t" and even
// "SELECT 1 AS begin; COMMIT" reach the server as one query. So the
// driver's splitting is not enough. A statement is refused when:
//   - the driver would split it, as written or with its placeholders
//     rewritten by its lexer; or
//   - verticaParts finds a ; followed by anything but blanks and comments,
//     outside quotes, quoted identifiers, comments and dollar quotes,
//     whatever block it seems to stand in.
//
// A statement the driver's lexer fails on is refused too (verticaLex).
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

// verticaLex is statement with its placeholders rewritten by the driver's
// lexer (parse.Lex), as the driver does to every statement it prepares
// (stmt.go, newStmt). It is false where the lexer panics: on a ? right
// after a ' at which it begins a string state, such as at the start of the
// text or of a line after a -- comment ("'?"). There it slices before what
// it wrote. The driver would panic on such a statement at every scrape, and
// the check, made where a configuration is loaded, would take the exporter
// down with it.
func verticaLex(statement string) (lexed string, ok bool) {
	defer func() {
		if recover() != nil {
			lexed, ok = "", false
		}
	}()
	return parse.Lex(statement), true
}

// verticaParts counts the statements in statement as Vertica's SQL has
// them: every ; outside a quote ends one. Quotes are 'text' (with a doubled
// quote inside), "name" (with "" inside), -- to the end of the line,
// /* ... */, and $tag$ ... $tag$. A part of nothing but blanks (ASCII white
// space) and comments is not a statement. It knows no blocks, so a ; never
// stands inside one. A // is not taken for a comment; since 1.3.9 the
// driver does not take it for one either. A statement that is one only if
// those were honoured is refused, which costs a metrics query nothing.
//
// Where the server's reading is not the driver's, or not a single one, the
// text is read every way the server may read it, and the most statements
// any reading finds are counted (verticaReadings). The driver, and the
// first reading, take a $ anywhere for the start of a dollar quote and a \
// for nothing. The server, like PostgreSQL (whose SQL Vertica's follows):
//   - takes a $ inside a word for part of it ("SELECT a$b$; COMMIT;
//     SELECT $b$x$b$" is three statements);
//   - takes a \ to escape the next character, a quote among them, in an
//     E'...' and, with standard_conforming_strings off, in every '...'. The
//     string then ends at a quote the driver takes for half of a doubled one;
//   - may nest block comments.
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

// verticaTLS hands out the names under which TLS configurations are
// registered with the driver. The driver keeps registered configurations in
// a map for good, and registering a name again replaces its configuration
// (connection.go RegisterTLSConfig, _tlsConfigs.add). It looks the
// configuration up by the URL's tlsmode only while it opens a connection
// (connection.go newConnection, initializeSSL). So each connection gets a
// name of its own, registered with the configuration made for it from the
// files as read for it. The name is given back once the connection is open
// or has failed, so the next connection can register its own under it.
// There are never more names than connections ever opened at once, however
// often the files change, and a connection never takes another's
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

// connectWithin runs open, a driver's connect that takes no context. It
// returns what open made, or the context's error if the context ends first;
// in that case the connection open makes later is closed. A panic of the
// driver, in the goroutine open runs in, is returned as its error.
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
// of its context. A call that takes no context (a transaction's commit and
// rollback, a statement's close) returns by the end of the context it was
// begun or prepared with. The driver reads with no deadline (connection.go
// readAll). It stops a statement only by a cancel request, sent when the
// statement's context ends, over a connection it dials itself: with no
// timeout, after a lookup of its own, past the exporter's dialer and the
// target lists, and unencrypted whatever tlsmode says (stmt.go
// QueryContextRaw, connection.go establishSocketConnection and
// sendMessageTo). So the driver is given contexts that never end, and makes
// no cancel request. Each call runs in a goroutine of its own. A call still
// running when its context ends is left to finish there, and the connection
// is given up: it is invalid to database/sql from then on, which closes it,
// and its driver connection is closed when that call returns.
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

// verticaStmt is a statement of a verticaConn. Each call is bounded by its
// context or, for one that takes none, by the one it was prepared with. The
// driver reads its rows whole before returning them (stmt.go
// collectResults), so reading them waits for nothing.
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
