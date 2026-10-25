//go:build (!select_request_types || request_type_sql) && sql_driver_vertica

package fetch

import (
	"strings"
	"testing"

	"github.com/vertica/vertica-sql-go/parse"
)

// verticaFuzzSeeds are the statements of
// TestAVerticaStatementHoldingMoreThanOneIsRefused, and those that hide a ;
// from the driver and from a reading that knows less of Vertica's SQL than
// its server: an extended string's \', a $ inside a word, a nested comment;
// and COPY statements the driver would send unprepared, the args written
// into their text, or the server would run, refused as COPY.
var verticaFuzzSeeds = []string{
	"COMMIT; SELECT 1 AS one",
	"SELECT 1 AS one; COMMIT",
	"SELECT 1;SELECT 2",
	"SELECT 1 AS one -- ;\n; SELECT 2",
	"SELECT 1 AS one;",
	"SELECT 1 AS one ;  \n\t",
	"SELECT 1 AS one; -- the end",
	"SELECT 1 AS one; /* the end; really */",
	"SELECT 1 AS one; // the end",
	"SELECT ';' AS one",
	"SELECT 'it''s; here' AS one",
	`SELECT 1 AS "a;b"`,
	`SELECT 1 AS "a"";b"`,
	"SELECT $$;$$ AS one",
	"SELECT $tag$ ; $tag$ AS one",
	"SELECT 1 AS one -- ; COMMIT",
	"SELECT 1 AS one /* ; COMMIT */",
	"SELECT ? AS one, @name AS two",
	"SELECT @name; COMMIT",
	"BEGIN; COMMIT; SELECT 1",
	"SELECT 1 AS begin; COMMIT",
	"begin select 1; commit; end",
	"SELECT 1 AS one; BEGIN; COMMIT; DROP TABLE t",
	"SELECT CASE WHEN 1 = 1 THEN 1 END AS one; COMMIT",
	"SELECT CASE WHEN 1 = 1 THEN 1 END AS one;",
	"SELECT 1 AS loop; COMMIT",
	"SELECT 1 AS one\r; COMMIT",
	"SELECT 1 AS one /* unterminated ; COMMIT",
	"SELECT 'unterminated ; COMMIT",
	"SELECT $1$; COMMIT; $1$",
	"SELECT E'\\''; COMMIT; --'",
	"SELECT a$b$; COMMIT; SELECT $b$x$b$",
	"SELECT 1 /* /* */ ' */; COMMIT; SELECT '1'",
	"SELECT 1 AS one;\xa0",
	"'?",
	"'@A",
	"@A@A; COMMIT",
	"SELECT 1 AS one -- c\n'?'",
	"SELECT 1 AS x$$; COMMIT",
	"SELECT 1 AS one /* /* */ */",
	"SELECT 'a\\''; COMMIT; --'",
	"COPY t FROM LOCAL '/f' DELIMITER ?",
	"copy /* c */ t -- d\n from LOCAL '/f' delimiter ?",
	"/* /* */ COPY t FROM LOCAL '/f' */ SELECT ? AS one",
	`"x" COPY t FROM LOCAL '/f' DELIMITER ?`,
	"COPY t FROM STDIN",
	"SELECT 1 AS copy, ? AS two",
}

// FuzzVerticaStatementSplitting holds the refusal of a vertica statement
// holding more than one (verticaSingleStatement) to what the driver and the
// server would make of any text: it never panics; a text the driver would
// split, as written or as its lexer rewrites it, or verticaParts finds more
// than one statement in, is refused; so is a text in which a reading of
// Vertica's SQL as its server reads it (verticaServerParts), standard
// strings or not, finds more than one statement; two statements joined by a
// ; are refused. The one normalisation there is, the driver's lexer, is
// read as the driver splits it, once: it is not idempotent ("@A@A" is
// "?@A", then "??"), and a text it fails on is refused. A text after a COPY,
// as the driver or the server reads one, is refused (verticaStatement),
// whatever it is. Its seeds run as a test in every build with the
// vertica driver (go test -tags sql_driver_vertica -run=^$
// -fuzz=FuzzVerticaStatementSplitting ./internal/fetch/ fuzzes it).
func FuzzVerticaStatementSplitting(f *testing.F) {
	for _, seed := range verticaFuzzSeeds {
		f.Add(seed, "SELECT 2")
	}
	f.Add("SELECT 'a'", `SELECT "b"`)
	f.Add("SELECT 1 -- c\n", "/* d */ SELECT 2")
	f.Fuzz(func(t *testing.T, statement, other string) {
		checkVerticaRefusal(t, statement)
		_ = verticaStatement(statement)
		for _, copied := range []string{"COPY " + statement, "/* /* */ COPY t FROM LOCAL '/f' */ " + statement} {
			if verticaStatement(copied) == nil {
				t.Fatalf("%q, a COPY to the driver, is let through", copied)
			}
		}
		// Two statements, each one whole whatever the server's reading,
		// joined by a ;.
		if verticaWholeStatement(statement) && verticaWholeStatement(other) {
			for _, join := range []string{";", "; ", "\n;\n"} {
				if joined := statement + join + other; verticaSingleStatement(joined) == nil {
					t.Fatalf("%q, two statements, is let through", joined)
				}
			}
		}
	})
}

// checkVerticaRefusal fails t unless statement is refused wherever the
// driver, verticaParts or a reading of the server's finds more than one
// statement in it.
func checkVerticaRefusal(t *testing.T, statement string) {
	t.Helper()
	refused := verticaSingleStatement(statement) != nil
	lexed, ok := verticaLex(statement)
	if !ok {
		if !refused {
			t.Fatalf("%q, which the driver's lexer fails on, is let through", statement)
		}
		return
	}
	for _, text := range []string{statement, lexed} {
		if n := len(parse.SplitStatements(text)); n > 1 && !refused {
			t.Fatalf("%q is let through, though the driver splits %q in %d", statement, text, n)
		}
		if n := verticaParts(text); n > 1 && !refused {
			t.Fatalf("%q is let through, though verticaParts finds %d statements in %q", statement, n, text)
		}
		for _, reading := range verticaServerReadings {
			if n, _ := verticaServerParts(text, reading); n > 1 && !refused {
				t.Fatalf("%q is let through, though the server, %+v, would find %d statements in %q", statement, reading, n, text)
			}
		}
	}
}

// verticaWholeStatement reports whether statement is one statement ending
// outside any quote and comment in every reading of the server's.
func verticaWholeStatement(statement string) bool {
	for _, reading := range verticaServerReadings {
		if n, closed := verticaServerParts(statement, reading); n != 1 || !closed {
			return false
		}
	}
	return true
}

// verticaServerReading is how the server may read a statement:
// standard_conforming_strings off makes a \ escape in every '...', and
// block comments may nest, as in PostgreSQL, whose SQL Vertica's follows.
type verticaServerReading struct {
	backslashes, nested bool
}

var verticaServerReadings = []verticaServerReading{{false, false}, {false, true}, {true, false}, {true, true}}

// verticaServerParts counts the statements in statement as the server would,
// written apart from verticaParts to check it: a word is letters, digits, _,
// $ and any byte of a multi-byte character, so a $ inside one starts no
// dollar quote; an E'...' begun outside a word, and with reading.backslashes
// every '...', takes \ to escape the next byte; and so on as verticaParts.
// closed says whether it ends outside every quote and comment.
func verticaServerParts(statement string, reading verticaServerReading) (parts int, closed bool) {
	word := func(c byte) bool {
		return c == '_' || c == '$' || c >= 0x80 || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	content := false
	closed = true
	s := statement
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			escapes := reading.backslashes || i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') && (i == 1 || !word(s[i-2]))
			content, closed = true, false
			for i++; i < len(s); i++ {
				if escapes && s[i] == '\\' {
					i++
					continue
				}
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						i++
						continue
					}
					closed = true
					break
				}
			}
		case c == '"':
			content, closed = true, false
			for i++; i < len(s); i++ {
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						i++
						continue
					}
					closed = true
					break
				}
			}
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			closed = false
			for ; i < len(s); i++ {
				if s[i] == '\n' || s[i] == '\r' {
					closed = true
					break
				}
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			depth := 1
			closed = false
			for i += 2; i < len(s); i++ {
				switch {
				case reading.nested && s[i] == '/' && i+1 < len(s) && s[i+1] == '*':
					depth++
					i++
				case s[i] == '*' && i+1 < len(s) && s[i+1] == '/':
					depth--
					i++
				}
				if depth == 0 {
					closed = true
					break
				}
			}
		case c == '$' && (i == 0 || !word(s[i-1])):
			content = true
			if tag, ok := verticaDollarTag(s[i:]); ok {
				closed = false
				if end := strings.Index(s[i+len(tag):], tag); end >= 0 {
					i += len(tag) + end + len(tag) - 1
					closed = true
				} else {
					i = len(s)
				}
			}
		case c == ';':
			if content {
				parts++
			}
			content = false
		case c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '\f' && c != '\v':
			content = true
		}
	}
	if content {
		parts++
	}
	return parts, closed
}
