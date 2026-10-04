//go:build !select_request_types || request_type_graphite

package repository

import "testing"

// graphiteCollector is a configuration of one graphite collector, with a
// comment where a row writes a key of the collector and of its request.
const graphiteCollector = `collectors:
  - name: demo
    # collector
    request:
      type: graphite
      targets: [app.requests.count]
      # request
    transform:
      type: jq
    metrics:
      - name: requests
        items: .series[]
        expression: .value
`

// graphiteSchemaKeys are the keys of a graphite collector that a schema
// holds to allowed values or a pattern: how the points of a series become
// its value, what a line that cannot be read does, and how old a series
// may be, a duration.
func graphiteSchemaKeys() []schemaKey {
	collector := "    # collector\n"
	return []schemaKey{
		{key: "collectors[].response.graphite.value", document: graphiteCollector, at: collector, setting: "    response:\n      graphite:\n        value: %s\n", valid: "max", invalid: "median", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].response.graphite.invalid_lines", document: graphiteCollector, at: collector, setting: "    response:\n      graphite:\n        invalid_lines: %s\n", valid: "skip", invalid: "drop", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].response.graphite.max_age", document: graphiteCollector, at: collector, setting: "    response:\n      graphite:\n        max_age: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "collectors[].request.method", of: "of a graphite collector", document: graphiteCollector, at: "      # request\n", setting: "      method: %s\n", absent: true, empty: true, emptyWas: refused},
	}
}

// The keys of a graphite collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly:
// response.graphite.value: "" is last, the default, invalid_lines: "" is
// fail, and method: "" is the key a graphite collector does not have.
func TestSchemaAndExporterAgreeOnGraphiteKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, graphiteSchemaKeys())
}
