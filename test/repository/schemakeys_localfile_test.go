//go:build !select_request_types || request_type_localfile

package repository

import (
	"strings"
	"testing"
)

// fileCollector is a configuration of one localfile collector that reads
// one file, with a comment where a row writes a key of its request.
const fileCollector = `collectors:
  - name: demo
    request:
      type: localfile
      root: /var/lib/app
      path: status.txt
      # request
    transform:
      type: regex
    metrics:
      - name: v
        expression: 'v=(\d+)'
`

// localfileSchemaKeys are the keys of a localfile collector that a schema
// holds to a pattern, and root, which it requires: written "", root is as
// missing as left out, which the schema took.
func localfileSchemaKeys() []schemaKey {
	request := "      # request\n"
	directory := strings.Replace(fileCollector, "      path: status.txt\n", "      files: ['*.txt']\n", 1)
	return []schemaKey{
		{key: "collectors[].request.root", document: fileCollector, at: "      root: /var/lib/app\n", setting: "      root: %s\n", valid: "/var/lib/app", emptyWas: taken, booleanAlone: `request.root "true" must be an absolute path`, numberAlone: `request.root "1" must be an absolute path`},
		{key: "collectors[].request.method", of: "of a localfile collector", document: fileCollector, at: request, setting: "      method: %s\n", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.max_age", document: fileCollector, at: request, setting: "      max_age: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "collectors[].request.max_total_bytes", document: directory, at: request, setting: "      max_total_bytes: %s\n", valid: "1MiB", invalid: "lots", absent: true, size: true},
	}
}

// The keys of a localfile collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly: root is
// required, and root: "" is refused by both as root left out is; method: ""
// is the key a localfile collector does not have; max_age takes the
// number 0 as the duration it is; and max_total_bytes takes a whole number
// of bytes however YAML writes the number.
func TestSchemaAndExporterAgreeOnLocalfileKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, localfileSchemaKeys())
}
