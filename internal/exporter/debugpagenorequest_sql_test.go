//go:build !select_request_types || request_type_sql

package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A sql playground reads pasted rows, as a sql collector's driver hands
// them on, and connects to no database.
func TestASQLPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, source bool) string {
			password := ""
			if source {
				password = ", password_file: " + testutil.WriteFile(t, "password", "pw\n")
			}
			return "name: " + name + "\nrequest: {type: sql, driver: postgres, username: u" + password + ", statement: SELECT jobs FROM queue}\ntransform: {type: jq}\nmetrics: [{name: jobs, items: '.[]', expression: .jobs}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `[{"jobs": 7}]`, series: "jobs 7\n",
	})
	requirePlaygroundRefusesFiles(t, "sql")
}
