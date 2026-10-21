//go:build !select_request_types || request_type_http

package repository

import (
	"strings"
	"testing"
)

// loggingCollector is a configuration of one http collector whose
// request.login is the block written, indented under login:, beside the
// request's further keys.
func loggingCollector(login, request string) string {
	return "collectors:\n  - name: api\n    request:\n      type: http\n" + request + "      login:\n" + login + "    transform: {type: regex}\n    metrics: [{name: v, expression: 'v=(\\d+)'}]\n"
}

// The committed schema and the exporter agree on request.login: which keys
// each kind takes, which it requires, and that a cookie's token takes no
// header or prefix of its own. What a schema cannot tell — a header whose
// name does not read as a credential's, a URL with credentials in it, a
// login beside another credential sent in Authorization, a jq expression
// that does not compile — the exporter refuses alone, in its words.
func TestSchemaAndExporterAgreeOnALogin(t *testing.T) {
	schema := loadSchema(t)
	const (
		json   = "        type: json\n        url: /login\n        field_files: {password: /run/secrets/password}\n"
		oauth2 = "        type: oauth2\n        url: https://idp.example/oauth/token\n        client_secret_file: /run/secrets/client\n"
	)
	for login, accepted := range map[string]bool{
		json + "        token: .access_token\n        expires_in: .expires_in\n        fields: {tenant: acme}\n":             true,
		json + "        token_header: X-Subject-Token\n        header: X-Auth-Token\n        prefix: ''\n        ttl: 15m\n": true,
		json + "        token_cookie: JSESSIONID\n":             true,
		json + "        token: .t\n        token_header: X-T\n": false,
		json: false,
		json + "        token: .t\n        client_id: x\n":                                                            false,
		json + "        token: .t\n        scopes: []\n":                                                              false,
		"        type: json\n        url: /login\n        token: .t\n":                                                false,
		"        type: json\n        url: /login\n        token: .t\n        field_files: {}\n":                       false,
		json + "        token_cookie: SID\n        header: X-Auth-Token\n":                                            false,
		json + "        token_cookie: SID\n        prefix: ''\n":                                                      false,
		oauth2 + "        client_id: exporter\n        scopes: [metrics]\n        endpoint_params: {audience: api}\n": true,
		oauth2 + "        client_id_file: /run/secrets/id\n        auth_style: params\n":                              true,
		oauth2 + "        client_id: x\n        auth_style: ''\n":                                                     true,
		oauth2: false,
		oauth2 + "        client_id: x\n        client_id_file: /i\n":                                false,
		oauth2 + "        client_id: x\n        token: .t\n":                                         false,
		oauth2 + "        client_id: x\n        fields: {a: b}\n":                                    false,
		oauth2 + "        client_id: x\n        auth_style: auto\n":                                  false,
		oauth2 + "        client_id: x\n        endpoint_params: {grant_type: password}\n":           false,
		"        type: oauth2\n        url: https://idp.example/token\n        client_id: x\n":       false,
		"        type: form\n        url: /login\n        token: .t\n        field_files: {p: /p}\n": false,
		"        type: json\n        token: .t\n        field_files: {p: /p}\n":                      false,
		"        type: json\n        url: ''\n        token: .t\n        field_files: {p: /p}\n":     false,
	} {
		agree(t, schema, strings.ReplaceAll(strings.TrimSpace(login), "\n        ", "; "), loggingCollector(login, ""), accepted)
	}
	for _, alone := range []struct{ login, request, message string }{
		{json + "        token: .t\n        header: X-Tenant\n", "", `request.login.header "X-Tenant" does not read as a credential's name`},
		{"        type: json\n        url: https://user:pw@idp.example/login\n        token: .t\n        field_files: {p: /p}\n", "", "request.login.url holds credentials"},
		{json + "        token: .t\n", "      bearer_token_file: /run/token\n", "sets request.login and bearer_token, which both send Authorization"},
		{json + "        token: '.['\n", "", `request.login.token ".[" does not compile`},
	} {
		loadersAlone(t, schema, strings.TrimSpace(alone.login), loggingCollector(alone.login, alone.request), alone.message)
	}
}
