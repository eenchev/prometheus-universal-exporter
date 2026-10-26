//go:build (!select_request_types || request_type_sql) && sql_driver_vertica

package fetch

import (
	"testing"

	vertigo "github.com/vertica/vertica-sql-go"
)

// The vertica driver tells Vertica's error by its SQLSTATE; a network failure is not the server's.
func TestTheVerticaDriverTellsTheServersErrorByItsCode(t *testing.T) {
	checkServerAnswer(t, SQLDriverVertica, &vertigo.VError{Severity: "FATAL", ErrorCode: "3781", SQLState: "28000", Message: "Invalid username or password"}, "SQLSTATE 28000")
	checkNotServerAnswer(t, SQLDriverVertica, sqlRefused)
}
