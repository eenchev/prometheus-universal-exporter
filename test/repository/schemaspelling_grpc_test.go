//go:build !select_request_types || request_type_grpc

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// request.descriptors takes reflection, protoset and proto in lower case
// and without blanks around them, to the exporter as to the schema; the
// exporter took them in any case and with blanks around them.
func TestSchemaAndExporterAgreeOnTheSpellingOfDescriptors(t *testing.T) {
	schema := loadSchema(t)
	for descriptors, accepted := range map[string]bool{"reflection": true, "REFLECTION": false, "Reflection": false, "' reflection'": false, "'reflection '": false} {
		document := strings.Replace(queueCollector, "      # request\n", "      descriptors: "+descriptors+"\n", 1)
		problems := schemaProblems(t, schema, document)
		_, err := config.Load(testutil.WriteFile(t, "config.yaml", document))
		if (len(problems) == 0) != accepted || (err == nil) != accepted {
			t.Errorf("descriptors: %s: want accepted %v by both; the schema says %v, the exporter %v", descriptors, accepted, problems, err)
		}
	}
}
