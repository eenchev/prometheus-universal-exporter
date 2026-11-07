//go:build !select_request_types || request_type_ntp

package repository

import "testing"

// ntpCollector is a configuration of one ntp collector, with a comment
// where a row writes a key of its request.
const ntpCollector = `collectors:
  - name: clock
    request:
      type: ntp
      # request
    transform:
      type: jq
    metrics:
      - name: offset
        expression: .offset_seconds
`

// ntpSchemaKeys are the keys of an ntp collector that a schema holds to a
// rule: sample_interval, a duration of at least 1s, 0 the default; of the
// common part it takes only the target lists, which http's rows hold.
func ntpSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.sample_interval", of: "an ntp collector's", document: ntpCollector, at: request, setting: "      sample_interval: %s\n", valid: "5s", invalid: "often", absent: true, duration: true},
		{key: "collectors[].request.sample_interval", of: "an ntp collector's under its least", document: ntpCollector, at: request, setting: "      sample_interval: %s\n", valid: "1s", invalid: "500ms", absent: true, invalidWas: taken},
	}
}

// The keys of an ntp collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnNTPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, ntpSchemaKeys())
}
