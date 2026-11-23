//go:build !select_request_types || request_type_imap

package repository

// imapCollector is a configuration of one imap collector with an account,
// with a comment where a row writes a key of its request.
const imapCollector = `collectors:
  - name: demo
    request:
      type: imap
      username: monitor@example.com
      password_file: /etc/exporter/password
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .tls
`

// imapSchemaKeys are the keys of an imap collector that a schema holds to
// a rule of allowed values, a pattern or a length: auth, held to the
// mechanisms imap speaks, which user, pop3's, is none of. Its mailboxes
// and counts are held to their number, which the loader counts as the
// schema does.
func imapSchemaKeys() []schemaKey {
	return []schemaKey{
		{key: "collectors[].request.auth", of: "imap", document: imapCollector, at: "      # request\n", setting: "      auth: %s\n", valid: "login", invalid: "user", invalidWas: taken, absent: true, empty: true, emptyWas: refused, built: true},
	}
}
