//go:build (!select_request_types || request_type_sql) && sql_driver_oracle

package fetch

import (
	"testing"

	oranetwork "github.com/sijms/go-ora/v2/network"
)

// The oracle driver tells Oracle's error by its ORA- number; a network failure is not the server's.
func TestTheOracleDriverTellsTheServersErrorByItsCode(t *testing.T) {
	checkServerAnswer(t, SQLDriverOracle, &oranetwork.OracleError{ErrCode: 1017, ErrMsg: "ORA-01017: invalid username/password; logon denied"}, "ORA-01017")
	checkServerAnswer(t, SQLDriverOracle, &oranetwork.OracleError{ErrCode: 1722, ErrMsg: "ORA-01722: invalid number"}, "ORA-01722")
	checkNotServerAnswer(t, SQLDriverOracle, sqlRefused)
}
