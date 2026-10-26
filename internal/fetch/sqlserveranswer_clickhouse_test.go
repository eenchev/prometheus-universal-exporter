//go:build (!select_request_types || request_type_sql) && sql_driver_clickhouse

package fetch

import (
	"testing"

	chproto "github.com/ClickHouse/clickhouse-go/v2/lib/proto"
)

// The clickhouse driver tells ClickHouse's exception, whose message quotes the statement, by its code; a network failure is not the server's.
func TestTheClickHouseDriverTellsTheServersErrorByItsCode(t *testing.T) {
	checkServerAnswer(t, SQLDriverClickHouse, &chproto.Exception{Code: 62, Name: "DB::Exception", Message: "Syntax error: failed at position 8 (secret_column): SELECT secret_column FROM"}, "code 62")
	checkNotServerAnswer(t, SQLDriverClickHouse, sqlRefused)
}
