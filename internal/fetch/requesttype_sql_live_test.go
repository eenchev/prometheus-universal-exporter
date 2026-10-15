//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The live tests run the sql request type against real databases, through
// the real drivers, when a variable names one: PUE_TEST_POSTGRES,
// PUE_TEST_MYSQL and PUE_TEST_SQLSERVER give its host:port, and the same
// with _USER and _PASSWORD its account. Without the variable a database's
// test is skipped; ci.yml's sql-live job runs them against service
// containers (make test-sql-live).

// liveSQL is a database a variable names, and a collector of it.
type liveSQL struct {
	driver, prefix string
	// types is a statement returning a row of each kind of value, and
	// want the values it returns that do not change from run to run.
	types string
	want  map[string]any
	// write is a statement that writes, which a read-only run refuses,
	// with the words of the refusal; empty for a database without a
	// read-only transaction.
	write, refused string
	// sleep is a statement that runs for five seconds.
	sleep string
}

var liveDatabases = []liveSQL{
	{
		driver: SQLDriverPostgres, prefix: "PUE_TEST_POSTGRES",
		types: "SELECT 1::int AS i, 1.5::float8 AS f, 2.25::numeric AS d, true AS b, now() AS t, 'x'::text AS s, NULL::text AS n",
		want:  map[string]any{"i": 1, "f": 1.5, "d": 2.25, "b": 1, "s": "x"},
		write: "CREATE TABLE pue_live_written (x int)", refused: "read-only transaction",
		sleep: "SELECT pg_sleep(5) AS slept",
	},
	{
		driver: SQLDriverMySQL, prefix: "PUE_TEST_MYSQL",
		types: "SELECT 1 AS i, 1.5e0 AS f, CAST(2.25 AS DECIMAL(5,2)) AS d, TRUE AS b, NOW() AS t, 'x' AS s, NULL AS n",
		want:  map[string]any{"i": 1, "f": 1.5, "d": 2.25, "b": 1, "s": "x"},
		write: "UPDATE mysql.db SET Db = Db WHERE 1 = 0", refused: "READ ONLY transaction",
		sleep: "SELECT SLEEP(5) AS slept",
	},
	{
		driver: SQLDriverSQLServer, prefix: "PUE_TEST_SQLSERVER",
		types: "SELECT CAST(1 AS int) AS i, CAST(1.5 AS float) AS f, CAST(2.25 AS decimal(5,2)) AS d, CAST(1 AS bit) AS b, SYSDATETIMEOFFSET() AS t, N'x' AS s, NULL AS n",
		want:  map[string]any{"i": 1, "f": 1.5, "d": 2.25, "b": 1, "s": "x"},
		sleep: "WAITFOR DELAY '00:00:05'; SELECT 1 AS slept",
	},
}

// Against each database a variable names: a statement's values arrive as
// the transforms read them, NULL as a column left out and the time near
// now; a statement that writes is refused where the database has a
// read-only transaction; and a statement running past the scrape's
// deadline is ended at it.
func TestLiveSQL(t *testing.T) {
	for _, db := range liveDatabases {
		t.Run(db.driver, func(t *testing.T) {
			target := os.Getenv(db.prefix)
			if target == "" {
				t.Skipf("%s names no %s database", db.prefix, db.driver)
			}
			sqlPools.closeAll()
			t.Cleanup(sqlPools.closeAll)
			passwordFile := testutil.WriteIn(t, t.TempDir(), "password", os.Getenv(db.prefix+"_PASSWORD")+"\n")
			collector := func(statement string) *model.Collector {
				c := &model.Collector{Name: "live", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: db.driver, Statement: statement, Username: os.Getenv(db.prefix + "_USER"), PasswordFile: passwordFile}}
				if err := ValidateRequest(c); err != nil {
					t.Fatal(err)
				}
				return c
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resp, err := FetchCollector(ctx, target, collector(db.types), RequestOverrides{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Rows) != 1 {
				t.Fatalf("%d rows", len(resp.Rows))
			}
			row := resp.Rows[0].(map[string]any)
			for column, want := range db.want {
				if row[column] != want {
					t.Errorf("column %s is %#v, want %#v", column, row[column], want)
				}
			}
			if _, null := row["n"]; null {
				t.Errorf("the NULL column is in the row: %v", row)
			}
			if seconds, ok := row["t"].(float64); !ok || time.Since(time.Unix(int64(seconds), 0)).Abs() > time.Hour {
				t.Errorf("the time is %#v", row["t"])
			}
			if db.write != "" {
				_, err := FetchCollector(ctx, target, collector(db.write), RequestOverrides{}, nil)
				if err == nil || !strings.Contains(err.Error(), db.refused) {
					t.Errorf("the statement that writes: %v, want a refusal of %q", err, db.refused)
				}
			}
			start := time.Now()
			_, err = FetchCollector(ctx, target, collector(db.sleep), RequestOverrides{Timeout: time.Second}, nil)
			if err == nil || !strings.Contains(err.Error(), "the scrape's deadline came first") || time.Since(start) > 4*time.Second {
				t.Errorf("the sleep: %v after %s", err, time.Since(start))
			}
		})
	}
}
