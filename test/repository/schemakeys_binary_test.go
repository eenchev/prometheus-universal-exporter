//go:build !select_request_types || request_type_http

package repository

import "testing"

// binaryCollector is a configuration of one http collector of the binary
// decoder, with a comment where a row writes a key of the layout and of its
// field.
const binaryCollector = `collectors:
  - name: frame
    request:
      type: http
    decoder:
      type: binary
    response:
      binary:
        # layout
        fields:
          - name: value
            type: u16
            # field
    transform:
      type: jq
    metrics:
      - name: value
        expression: .value
`

// binarySchemaKeys are the keys of response.binary that a schema holds to
// allowed values: the layout's byte order, a field's and a field's type.
func binarySchemaKeys() []schemaKey {
	return []schemaKey{
		{key: "collectors[].response.binary.byte_order", document: binaryCollector, at: "        # layout\n", setting: "        byte_order: %s\n", valid: "little", invalid: "middle", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].response.binary.fields[].byte_order", document: binaryCollector, at: "            # field\n", setting: "            byte_order: %s\n", valid: "little", invalid: "Little", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].response.binary.fields[].type", document: binaryCollector, at: "            type: u16\n", setting: "            type: %s\n", valid: "i32", invalid: "u24"},
	}
}

// The keys of response.binary get one verdict from the schema and the
// exporter, left out, written "", written well and written badly: a byte
// order written "" is big, the default, and a field's type is required.
func TestSchemaAndExporterAgreeOnBinaryKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, binarySchemaKeys())
}
