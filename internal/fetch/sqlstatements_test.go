//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
)

// statements are request.statements of n entries, each SELECT <i>.
func statements(n int) []SQLStatement {
	out := make([]SQLStatement, n)
	for i := range out {
		out[i] = SQLStatement{Name: fmt.Sprintf("s%d", i), Statement: fmt.Sprintf("SELECT %d", i)}
	}
	return out
}

// request.statements is refused beside statement, statement_file, args or
// arg_types, past 16 entries, and with an entry without a name, of a name
// no rule reads as .name, of a name another has, without a statement, with
// a probe's placeholder in its statement, or with args its types do not
// fit, each naming the entry.
func TestSQLStatementsAreCheckedWhenTheConfigurationLoads(t *testing.T) {
	f := newSQLFixture(t)
	for name, tc := range map[string]struct {
		change func(*SQLConfig)
		want   string
	}{
		"beside statement":      {func(o *SQLConfig) { o.Statement = "q" }, "sets both request.statements and request.statement;"},
		"beside statement_file": {func(o *SQLConfig) { o.StatementFile = "/q.sql" }, "sets both request.statements and request.statement_file;"},
		"beside args":           {func(o *SQLConfig) { o.Args = []string{"1"} }, "sets both request.statements and request.args;"},
		"beside arg_types":      {func(o *SQLConfig) { o.ArgTypes = []string{"int"} }, "sets both request.statements and request.arg_types;"},
		"seventeen":             {func(o *SQLConfig) { o.Statements = statements(17) }, "request.statements has 17 entries, more than 16"},
		"no name":               {func(o *SQLConfig) { o.Statements[1].Name = "" }, "request.statements[1] has no name"},
		"a name with a dash":    {func(o *SQLConfig) { o.Statements[1].Name = "queue-size" }, `request.statements[1].name "queue-size" is not letters, digits and _`},
		"a name twice":          {func(o *SQLConfig) { o.Statements[1].Name = "s0" }, `request.statements[1].name "s0" is request.statements[0]'s too`},
		"no statement":          {func(o *SQLConfig) { o.Statements[0].Statement = " " }, "request.statements[0] has no statement"},
		"a placeholder":         {func(o *SQLConfig) { o.Statements[0].Statement = "SELECT {{param_x}}" }, "request.statements[0].statement has a {{param_...}} placeholder"},
		"types without args":    {func(o *SQLConfig) { o.Statements[0].ArgTypes = []string{"int"} }, "sets request.statements[0].arg_types without request.statements[0].args"},
		"an arg not its type": {func(o *SQLConfig) {
			o.Statements[1].Args, o.Statements[1].ArgTypes = []string{"ten"}, []string{"int"}
		}, `request.statements[1].args[0] is "ten", which is no whole number`},
	} {
		own := SQLConfig{Driver: SQLDriverPostgres, Statements: statements(2)}
		tc.change(&own)
		c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Username: "metrics", PasswordFile: f.passwordFile, Own: own}}
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v\nwant %q", name, err, tc.want)
		}
	}
	c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Username: "metrics", PasswordFile: f.passwordFile, Own: SQLConfig{Driver: SQLDriverPostgres, Statements: statements(16)}}}
	if err := ValidateRequest(c); err != nil {
		t.Errorf("16 statements: %v", err)
	}
	if !SQLNamedResults(c) {
		t.Error("a collector of statements has no named results")
	}
}

// The statements' rows are held to the response limit together, as
// max_rows holds each; a debug trace records each statement as a request,
// its name among its headers, the first on the connection as it was got
// and the rest on it reused.
func TestSQLStatementsAreBoundedAndTracedTogether(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["SELECT a"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{strings.Repeat("x", 600)}}}
	f.db.Results["SELECT b"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{strings.Repeat("y", 600)}}}
	c := f.collector(t, "db", SQLDriverPostgres, "", func(r *model.RequestConfig) {
		sqlOwn(r, func(o *SQLConfig) {
			o.Statement = ""
			o.Statements = []SQLStatement{{Name: "a", Statement: "SELECT a"}, {Name: "b", Statement: "SELECT b"}}
		})
	})
	ctx, trace := WithRequestTrace(context.Background())
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, "db.example", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string][]map[string]string
	if err := json.Unmarshal(resp.Body, &doc); err != nil || len(doc["a"]) != 1 || len(doc["b"]) != 1 || resp.Rows != nil || resp.NamedRows == nil {
		t.Fatalf("answered %s, %v", resp.Body, err)
	}
	requests := trace.Requests()
	if len(requests) != 2 || requests[0].Header.Get("Sql-Statement") != "a" || requests[1].Header.Get("Sql-Statement") != "b" ||
		requests[1].Header.Get("Sql-Connection") != "reused" || !strings.HasPrefix(requests[1].Outcome, "returned 1 rows") {
		t.Errorf("traced %+v", requests)
	}

	c.Request.MaxResponseBytes = 1000
	_, err = FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "request.statements[1], b") {
		t.Errorf("two statements of 601 bytes each under a limit of 1000: %v", err)
	}
}
