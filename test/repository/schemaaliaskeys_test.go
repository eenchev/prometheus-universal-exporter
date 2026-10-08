//go:build !select_request_types || request_type_http

package repository

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A key written as an alias, *k : value with &k ttl elsewhere, is the key its
// anchor holds to the schema as it is to YAML, an editor and the exporter:
// the schema was handed the anchor's name as the key, and refused a valid
// otlp, cache or top-level x- key so written that the exporter loads, while
// an unknown key behind an alias is refused by both.
func TestSchemaAndExporterAgreeOnAKeyThatIsAnAlias(t *testing.T) {
	schema := loadSchema(t)
	const names = "x-names: [&on enabled, &ep endpoint, &t ttl, &extra x-extra, &bad endpiont, &ttll ttll]\n"
	cached := func(cache string) string {
		return names + "collectors:\n  - name: demo\n    request: {type: http, path: /status}\n    transform: {type: regex}\n    metrics: [{name: demo_value, expression: 'v=(\\d+)'}]\n    cache: " + cache + "\n"
	}
	agree(t, schema, "otlp", names+"otlp: {*on : true, *ep : 'http://collector.invalid:4318/v1/metrics'}\n"+testutil.MinimalConfig, true)
	agree(t, schema, "cache", cached("{*t : 1m}"), true)
	agree(t, schema, "a top-level x- key", names+"*extra : {anything: 1}\n"+testutil.MinimalConfig, true)
	agree(t, schema, "otlp's unknown key", names+"otlp: {*on : true, *bad : x}\n"+testutil.MinimalConfig, false)
	agree(t, schema, "cache's unknown key", cached("{*ttll : 1m}"), false)
}
