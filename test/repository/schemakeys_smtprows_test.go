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

// smtpTokenCollector is a configuration of one smtp collector that logs in
// with a token of the refresh token grant, with a comment where a row
// writes a key of its xoauth2 block.
const smtpTokenCollector = `collectors:
  - name: demo
    request:
      type: smtp
      username: monitor@example.com
      xoauth2:
        token_url: https://login.example.com/token
        client_id: app
        client_secret_file: /etc/exporter/secret
        refresh_token_file: /etc/exporter/refresh
        # xoauth2
    transform:
      type: jq
    metrics:
      - name: v
        expression: .greeting.code
`

// smtpSchemaKeys are the keys of an smtp collector that a schema holds to
// a rule: starttls_mode, auth and the grant of xoauth2, each one of a few
// values, "" the key left out. auth is held to the mechanisms smtp speaks,
// which apop, pop3's, is none of.
func smtpSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.starttls_mode", document: smtpCollector, at: request, setting: "      starttls_mode: %s\n", valid: "required", invalid: "always", absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.auth", document: smtpCollector, at: request, setting: "      auth: %s\n", valid: "cram-md5", invalid: "apop", invalidWas: taken, absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.xoauth2.grant", document: smtpTokenCollector, at: "        # xoauth2\n", setting: "        grant: %s\n", valid: "refresh_token", invalid: "password", absent: true, empty: true, emptyWas: refused, built: true},
	}
}
