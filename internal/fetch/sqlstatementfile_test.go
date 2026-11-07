//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

// statementFileCollector is a postgres collector whose statement is in the
// file at path, or written as statement too, not validated.
func statementFileCollector(path, statement string) *model.Collector {
	return &model.Collector{
		Name:    "jobs",
		Request: model.RequestConfig{Type: RequestTypeSQL, Username: "metrics", Own: SQLConfig{Driver: SQLDriverPostgres, StatementFile: path, Statement: statement}},
	}
}

// statementFileError is the error a collector whose statement file is path
// is loaded with.
func statementFileLoadError(path string) error {
	return ValidateRequest(statementFileCollector(path, ""))
}

// request.statement_file names the file the statement is read from when
// the configuration loads, in place of request.statement: an absolute path,
// written as it is, of a regular UTF-8 file of at most 1MiB holding a
// statement with no placeholder. Everything else is refused, saying what is
// wrong with it, naming the collector and the file.
func TestAStatementFileIsCheckedWhenItLoads(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string { return testutil.WriteIn(t, dir, name, content) }
	big := filepath.Join(dir, "big.sql")
	if err := os.WriteFile(big, nil, 0o600); err != nil || os.Truncate(big, maxSQLStatementFileBytes+1) != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path, statement, want string
	}{
		"both":                  {write("both.sql", "SELECT 1"), "SELECT 1", "sets both request.statement and request.statement_file; write the statement in one of them"},
		"blanks alone":          {"  ", "", "request.statement_file is written with blanks alone"},
		"blanks around":         {" " + write("a.sql", "SELECT 1"), "", "has blanks around it; write the path alone"},
		"a placeholder":         {"/etc/sql/{{param_db}}.sql", "", `request.statement_file "/etc/sql/{{param_db}}.sql" has a placeholder`},
		"a NUL":                 {"/etc/sql/a\x00.sql", "", "holds a NUL character"},
		"relative":              {"sql/jobs.sql", "", `request.statement_file "sql/jobs.sql" is a relative path; write the absolute path of the file`},
		"dot relative":          {"./jobs.sql", "", "is a relative path"},
		"missing":               {filepath.Join(dir, "missing.sql"), "", "missing.sql: no such file"},
		"a directory":           {dir, "", "is a directory; name the file of the statement in it"},
		"over the limit":        {big, "", "is 1048577 bytes, over the 1048576 a statement file may be"},
		"not UTF-8":             {write("latin1.sql", "SELECT 'caf\xe9'"), "", "is not UTF-8 text; save the statement as UTF-8"},
		"a NUL byte":            {write("nul.sql", "SELECT 1\x00"), "", "holds a NUL byte, which no statement does"},
		"empty":                 {write("empty.sql", ""), "", "holds no statement, only blanks"},
		"blanks":                {write("blanks.sql", " \n\t\r\n"), "", "holds no statement, only blanks"},
		"a byte order mark":     {write("bom-only.sql", "\uFEFF\n"), "", "holds no statement, only blanks"},
		"a placeholder in it":   {write("param.sql", "SELECT * FROM jobs WHERE queue = '{{ param_queue }}'"), "", "has a {{param_...}} placeholder in its statement, which is not filled there"},
		"neither":               {"", "", "has no request.statement; a sql collector runs one statement, written in request.statement or in the file request.statement_file names"},
		"a statement of blanks": {"", "  ", "has no request.statement"},
	} {
		err := ValidateRequest(statementFileCollector(tc.path, tc.statement))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), `collector "jobs" `) {
			t.Errorf("%s: %v\nwant %q", name, err, tc.want)
		}
	}
	if os.Geteuid() != 0 {
		unreadable := write("secret.sql", "SELECT 1")
		if err := os.Chmod(unreadable, 0); err != nil {
			t.Fatal(err)
		}
		if err := statementFileLoadError(unreadable); err == nil || !strings.Contains(err.Error(), "secret.sql: the exporter may not read it (permission denied)") {
			t.Errorf("an unreadable file: %v", err)
		}
	}
}

// The statement read is the file's text, a UTF-8 byte order mark and the
// blanks around it taken off, as a statement written in place is used, and
// a file reached through a symbolic link, as a mounted ConfigMap's are, is
// read. The statement a scrape runs is the one read.
func TestAStatementFilesStatementIsTheOneRun(t *testing.T) {
	dir := t.TempDir()
	statement := "SELECT state,\n       count(*) AS n\n  FROM jobs\n GROUP BY state"
	file := testutil.WriteIn(t, dir, "..data/jobs.sql", "\uFEFF\n"+statement+"\n\n")
	link := filepath.Join(dir, "jobs.sql")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	f := newSQLFixture(t)
	f.db.Results[statement] = sqltest.Result{Columns: []string{"state", "n"}, Rows: [][]driver.Value{{"open", int64(3)}}}
	c := f.collector(t, "jobs", SQLDriverPostgres, "", func(r *model.RequestConfig) {
		sqlOwn(r, func(o *SQLConfig) { o.StatementFile = link })
	})
	if got := sqlStatement(c); got != statement {
		t.Fatalf("the statement read is %q", got)
	}
	resp, err := FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
	if err != nil || len(resp.Rows) != 1 {
		t.Fatalf("%v, rows %v", err, resp)
	}
	if queries := f.db.Queries(); queries[len(queries)-1].Statement != statement {
		t.Errorf("ran %q", queries[len(queries)-1].Statement)
	}
}

// A configuration loaded without opening the files it names, as the target
// allocator loads it, checks the key but does not read the file, which the
// replicas mount; the debug page's playground, which never makes a request,
// does not require a statement, as a copy of a collector whose statement is
// in a file has none.
func TestAStatementFileIsNotReadWhereTheFilesAreNot(t *testing.T) {
	unread := statementFileCollector("/nowhere/jobs.sql", "")
	unread.DescriptorFilesUnread = true
	if err := ValidateRequest(unread); err != nil {
		t.Errorf("loaded without opening the files: %v", err)
	}
	relative := statementFileCollector("jobs.sql", "")
	relative.DescriptorFilesUnread = true
	if err := ValidateRequest(relative); err == nil || !strings.Contains(err.Error(), "is a relative path") {
		t.Errorf("a relative path loaded without opening the files: %v", err)
	}
	playground := statementFileCollector("", "")
	playground.RequestNotMade = true
	if err := ValidateRequest(playground); err != nil {
		t.Errorf("the playground: %v", err)
	}
	if NamedFiles(statementFileCollector("/etc/sql/jobs.sql", "")) == nil || NamedFiles(statementFileCollector("", "SELECT 1")) != nil {
		t.Error("the statement file is not among the files the collector names, or a statement written in place is")
	}
}

// A collector's YAML, which a reload compares to tell a changed collector
// and a probe's cache key is made from, changes with the statement file's
// text, as it would with a statement edited in place, the path alone the
// same.
func TestAStatementFilesTextIsPartOfTheCollectorsDefinition(t *testing.T) {
	dir := t.TempDir()
	path := testutil.WriteIn(t, dir, "jobs.sql", "SELECT 1 AS n")
	encoded := func() string {
		t.Helper()
		c := statementFileCollector(path, "")
		if err := ValidateRequest(c); err != nil {
			t.Fatal(err)
		}
		out, err := yaml.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	before := encoded()
	testutil.WriteIn(t, dir, "jobs.sql", "SELECT 2 AS n")
	after := encoded()
	if before == after || !strings.Contains(after, "statement_file: "+path) || !strings.Contains(after, "statement_file_sha256: ") {
		t.Errorf("before:\n%s\nafter:\n%s", before, after)
	}
}
