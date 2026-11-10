//go:build !select_request_types || request_type_smtp

package repository

// smtpCollector is a configuration of one smtp collector with an account,
// with a comment where a row writes a key of its request.
const smtpCollector = `collectors:
  - name: demo
    request:
      type: smtp
      username: monitor@example.com
      password_file: /etc/exporter/password
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .greeting.code
`

// smtpSchemaKeys are the keys of an smtp collector that a schema holds to
// a rule: starttls_mode and auth, each one of a few values, "" the key
// left out.
func smtpSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.starttls_mode", document: smtpCollector, at: request, setting: "      starttls_mode: %s\n", valid: "required", invalid: "always", absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.auth", document: smtpCollector, at: request, setting: "      auth: %s\n", valid: "login", invalid: "cram-md5", absent: true, empty: true, emptyWas: refused, built: true},
	}
}
