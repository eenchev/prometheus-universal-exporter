//go:build (!select_request_types || request_type_sql) && (sql_driver_clickhouse || sql_driver_snowflake)

package fetch

import (
	"context"
	"database/sql"
	"time"
)

// sqlPlain runs a statement as it is, outside any transaction, for a driver
// that makes it read-only by a setting (clickhouse) or not at all
// (snowflake, whose driver refuses a read-only transaction):
// the account must be one that can only read.
func sqlPlain(ctx context.Context, conn *sql.Conn, statement string, args []any, _ time.Duration) (*sql.Rows, func(), error) {
	rows, err := conn.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, nil, err
	}
	return rows, func() { _ = rows.Close() }, nil
}
