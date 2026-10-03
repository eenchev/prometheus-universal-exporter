//go:build !select_request_types || request_type_http

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// trustingCollector is a configuration of one http collector whose
// request.redirect_trusted_hosts is as written.
func trustingCollector(hosts string) string {
	return "collectors:\n  - name: web\n    request:\n      type: http\n      follow_redirects: true\n      redirect_trusted_hosts: " + hosts + "\n    transform: {type: regex}\n    metrics:\n      - name: v\n        expression: 'v=(\\d+)'\n"
}

// An entry of request.redirect_trusted_hosts is a host name, a glob of one
// or an IP address, in brackets or not, with spaces around it, a name also
// with a final dot; the schema and the exporter refuse alike an empty entry,
// a URL, a path, a network, a host with a port, an IPv6 address with a final
// dot, a glob written like a range of addresses, and wildcards alone in any
// spelling but "*". An address written like an IPv6 one that is none is the
// exporter's alone to refuse, and so is the key under a request type that
// follows no redirects, which the schema does not tell apart by type for
// any key.
func TestSchemaAndExporterAgreeOnTheTrustedHosts(t *testing.T) {
	schema := loadSchema(t)
	for written, accepted := range map[string]bool{
		`[api.example.com]`: true, `["*.example.com"]`: true, `["API-?.Internal."]`: true, `[my_service]`: true, `["*"]`: true,
		`[192.168.1.7]`: true, `["::1"]`: true, `["[fd00::1]"]`: true, `["fe80::1%eth0"]`: true, `[" api.example.com "]`: true,
		`[api.example.com, "*.cdn.example.com", 10.0.0.7]`: true, `[]`: true,
		`[" * "]`: true, `["10-*"]`: true, `["*.10.example"]`: true, `["1*a"]`: true, `["?a"]`: true, `[1.2.3.4.]`: true, `["[api.example.com]."]`: true,
		`["::1:80"]`: true, `["[fe80::1%eth0]"]`: true, `["::ffff:1.2.3.4"]`: true, `[0x7f.0.0.1]`: true, `[a..b]`: true, `["-"]`: true,

		`["10.*"]`: false, `["192.168.*.*"]`: false, `["10.0.0.?"]`: false, `["[10.*]"]`: false, `["*.1."]`: false, `["1*"]`: false,
		`["**"]`: false, `["*."]`: false, `["[*]"]`: false, `["*.*"]`: false, `["?"]`: false, `["*?"]`: false, `["[**]."]`: false,
		`["fe80::*"]`: false, `["*:*"]`: false, `["10.*:80"]`: false, `["::1."]`: false, `["[::1]."]`: false, `["fe80::1%eth0."]`: false, `["fe80::1%"]`: false,

		`[""]`: false, `[" "]`: false, `["https://api.example.com"]`: false, `[api.example.com/v1]`: false, `["api.example.com:8443"]`: false,
		`["[::1]:8443"]`: false, `[10.0.0.0/8]`: false, `["fd00::/8"]`: false, `["a b.example"]`: false, `[api.example.com, ""]`: false,
		`["fe80::zz:1"]`: false, `[[api.example.com]]`: false, `api.example.com`: false, `[example.com..]`: false, `[.example.com]`: false,
	} {
		agree(t, schema, "redirect_trusted_hosts: "+written, trustingCollector(written), accepted)
	}
	loadersAlone(t, schema, "an address that is none", trustingCollector(`["fe80:::1"]`), `request.redirect_trusted_hosts entry "fe80:::1" is not a host name`)
	loadersAlone(t, schema, "an address with too many parts", trustingCollector(`["1:2:3:4:5:6:7:8:9"]`), `request.redirect_trusted_hosts entry "1:2:3:4:5:6:7:8:9" is not a host name`)
}

// A static target's request block has no redirect_trusted_hosts: the list is
// the collector's, and both the static targets schema and the exporter
// refuse the key there.
func TestAStaticTargetCannotSetTheTrustedHosts(t *testing.T) {
	document := "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: http://a.example\n    request:\n      follow_redirects: true\n      redirect_trusted_hosts: [cdn.example]\n"
	if problems := schemaProblems(t, loadSchemaFile(t, staticTargetsSchemaFile), document); len(problems) == 0 {
		t.Error("the static targets schema accepts request.redirect_trusted_hosts")
	}
	if _, err := config.LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", document)); err == nil || !strings.Contains(err.Error(), `unknown key "redirect_trusted_hosts" in a static target's request`) {
		t.Errorf("the exporter says %v of request.redirect_trusted_hosts in a static target, want the key refused", err)
	}
}
