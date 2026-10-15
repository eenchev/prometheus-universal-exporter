//go:build select_request_types && !request_type_sql

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A build without the sql type refuses a configuration with a sql collector
// when it loads, saying the type exists and the build left it out, and
// which tag would include it.
func TestABuildWithoutSQLRefusesASQLCollector(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: db
    request:
      type: sql
      driver: postgres
      username: metrics
      statement: SELECT 1 AS one
    transform:
      type: csv
`))
	if err == nil {
		t.Fatal("a sql collector loaded in a build without the type")
	}
	for _, want := range []string{`collector "db" uses request.type "sql", which this build of the exporter does not include`, "request_type_sql"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should say %q", err, want)
		}
	}
}
