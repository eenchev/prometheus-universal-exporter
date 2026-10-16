//go:build (!select_request_types || request_type_sql) && sql_driver_oracle

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// In a build with the oracle driver, the schema requires service_name of an
// oracle collector whatever the case of its driver and the blanks around it,
// as the exporter, which matches the driver so, does; and takes one that
// has it.
func TestTheSchemaRequiresAnOracleServiceNameWhateverTheDriversCase(t *testing.T) {
	schema := builtSchema(t, configSchemaFile)
	for _, driver := range []string{"oracle", "ORACLE", `" Oracle\t"`} {
		without := strings.Replace(sqlCollector, "      driver: postgres\n", "      driver: "+driver+"\n", 1)
		with := strings.Replace(without, "      # request\n", "      service_name: ORCL\n", 1)
		for _, tc := range []struct {
			document string
			taken    bool
		}{{without, false}, {with, true}} {
			problems := schemaProblems(t, schema, tc.document)
			_, err := config.Load(testutil.WriteFile(t, "config.yaml", tc.document))
			if (len(problems) == 0) != tc.taken || (err == nil) != tc.taken {
				t.Errorf("driver %s, service_name given %v: the schema finds %v and the exporter %v; want both to take it: %v", driver, tc.taken, problems, err, tc.taken)
			}
		}
	}
}
