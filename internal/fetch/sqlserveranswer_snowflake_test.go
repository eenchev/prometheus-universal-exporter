//go:build (!select_request_types || request_type_sql) && sql_driver_snowflake

package fetch

import (
	"testing"

	"github.com/snowflakedb/gosnowflake"
)

// The snowflake driver tells Snowflake's error by its number and SQLSTATE, and the driver's own failures, numbered from 260000, as they are; a network failure is not the server's.
func TestTheSnowflakeDriverTellsTheServersErrorByItsCode(t *testing.T) {
	checkServerAnswer(t, SQLDriverSnowflake, &gosnowflake.SnowflakeError{Number: 2003, SQLState: "42S02", Message: "SQL compilation error: Object 'SECRET_DB.PUBLIC.JOBS' does not exist or not authorized."}, "error 002003 (SQLSTATE 42S02)")
	checkServerAnswer(t, SQLDriverSnowflake, &gosnowflake.SnowflakeError{Number: 390100, Message: "Incorrect username or password was specified."}, "error 390100")
	// The driver's own failures are numbered from 260000: not the account's.
	checkNotServerAnswer(t, SQLDriverSnowflake, &gosnowflake.SnowflakeError{Number: gosnowflake.ErrCodeFailedToConnect, SQLState: "08001", Message: "failed to connect to db"})
	checkNotServerAnswer(t, SQLDriverSnowflake, sqlRefused)
}
