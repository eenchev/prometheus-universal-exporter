//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
)

// sqlResponseSize is what the rows of result come to toward
// max_response_bytes, worked out apart from the exporter: each value that is
// not NULL its length, for text, or 8, with its column's name's length.
func sqlResponseSize(result sqltest.Result) int64 {
	var size int64
	for _, row := range result.Rows {
		for i, v := range row {
			switch x := v.(type) {
			case nil:
				continue
			case string:
				size += int64(len(x))
			default:
				size += 8
			}
			size += int64(len(result.Columns[i]))
		}
	}
	return size
}

// A column's name counts toward max_response_bytes once in each row the
// column has a value in, as the transforms read the rows, objects keyed by
// the names, and as their JSON writes them; a NULL, left out of its row,
// counts nothing, its name neither. A result of one long name and small
// values goes over a limit its values alone are under; and over generated
// results, each is taken at exactly its values' sizes and its names' lengths
// and fails as a limit one byte under them, its rows as they are read
// without a limit, so that nothing but the count changed.
func TestASQLColumnsNameCountsTowardTheResponseLimitInEachRowItHasAValueIn(t *testing.T) {
	f := newSQLFixture(t)
	fetch := func(statement string, limit int64) (*HTTPResponse, error) {
		c := f.collector(t, "db", SQLDriverPostgres, statement, func(r *model.RequestConfig) { r.MaxResponseBytes = model.ByteSize(limit) })
		return FetchCollector(context.Background(), "db.example", c, RequestOverrides{}, nil)
	}
	rows := make([][]driver.Value, 10)
	for i := range rows {
		rows[i] = []driver.Value{int64(i)}
	}
	f.db.Results["long"] = sqltest.Result{Columns: []string{strings.Repeat("c", 128)}, Rows: rows}
	if _, err := fetch("long", 1024); err == nil || !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("ten numbers of a column named with 128 characters under a limit of 1KiB: %v", err)
	}
	f.db.Results["null"] = sqltest.Result{Columns: []string{"name", "gone"}, Rows: [][]driver.Value{{"ab", nil}, {"cd", int64(1)}}}
	if size := sqlResponseSize(f.db.Results["null"]); size != 24 {
		t.Fatalf("the worked-out size is %d", size)
	}
	random := rand.New(rand.NewPCG(65, 30))
	statements := []string{"null"}
	for n := range 40 {
		columns := make([]string, 1+random.IntN(4))
		for i := range columns {
			columns[i] = fmt.Sprintf("c%d_%s", i, strings.Repeat("x", random.IntN(40)))
		}
		result := sqltest.Result{Columns: columns}
		for range random.IntN(6) {
			row := make([]driver.Value, len(columns))
			for i := range row {
				switch random.IntN(5) {
				case 0:
				case 1:
					row[i] = random.Int64()
				case 2:
					row[i] = random.Float64()
				case 3:
					row[i] = random.IntN(2) == 0
				default:
					row[i] = strings.Repeat("v", random.IntN(30))
				}
			}
			result.Rows = append(result.Rows, row)
		}
		statement := fmt.Sprintf("g%d", n)
		f.db.Results[statement] = result
		statements = append(statements, statement)
	}
	for _, statement := range statements {
		size := sqlResponseSize(f.db.Results[statement])
		unbounded, err := fetch(statement, 0)
		if err != nil {
			t.Fatalf("%s without a limit: %v", statement, err)
		}
		at, err := fetch(statement, size)
		if err != nil || !reflect.DeepEqual(at.Rows, unbounded.Rows) || string(at.Body) != string(unbounded.Body) {
			t.Errorf("%s at its size, %d: %v; rows %v, want %v", statement, size, err, at, unbounded.Rows)
		}
		if size == 0 {
			continue
		}
		if _, err := fetch(statement, size-1); err == nil || !errors.Is(err, model.ErrLimitExceeded) {
			t.Errorf("%s one byte under its size, %d: %v, want a limit", statement, size, err)
		}
	}
}
