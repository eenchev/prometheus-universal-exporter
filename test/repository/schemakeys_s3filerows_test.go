//go:build !select_request_types || request_type_s3file

package repository

import "strings"

// s3Collector is a configuration of one s3file collector that reads one
// object, with a comment where a row writes a key of its request.
const s3Collector = `collectors:
  - name: demo
    request:
      type: s3file
      root: /exports
      path: status.txt
      # request
    transform:
      type: regex
    metrics:
      - name: v
        expression: 'v=(\d+)'
`

// s3fileSchemaKeys are the keys of an s3file collector that a schema holds
// to a pattern: its region and endpoint, and its credential files, which
// may not be blanks alone, "" being the key left out, and of which the
// access key's two go together and the session token goes with them. The
// rule that the two go together is of their being written; as it was, of
// their being there, it would have taken one of them written "". They
// are held in every build's schema, the default build's among them, which
// does not carry the opt-in type (TestEveryConstrainedKeyOfTheSchemasIsInATable),
// and the two verdicts are compared in a build with it (schemakeys_s3file_test.go).
func s3fileSchemaKeys() []schemaKey {
	request := "      # request\n"
	// An s3file collector with a static access key, and a comment where a
	// row writes beside it.
	withKey := strings.Replace(s3Collector, request, "      access_key_id_file: /id\n      secret_access_key_file: /s\n"+request, 1)
	return []schemaKey{
		{key: "collectors[].request.region", document: s3Collector, at: request, setting: "      region: %s\n", valid: "eu-west-1", invalid: `"eu west"`, absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.endpoint", document: s3Collector, at: request, setting: "      endpoint: %s\n", valid: "https://minio.example.com:9000", invalid: `"https://minio.example.com/bucket"`, absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.access_key_id_file", document: s3Collector, at: request, setting: "      access_key_id_file: %s\n      secret_access_key_file: /s\n", valid: "/id", invalid: `" "`, absent: true, emptyWas: taken, built: true},
		{key: "collectors[].request.secret_access_key_file", document: s3Collector, at: request, setting: "      secret_access_key_file: %s\n      access_key_id_file: /id\n", valid: "/s", invalid: `"\t"`, absent: true, emptyWas: taken, built: true},
		{key: "collectors[].request.session_token_file", document: withKey, at: request, setting: "      session_token_file: %s\n", valid: "/t", invalid: `" "`, absent: true, empty: true, built: true},
	}
}
