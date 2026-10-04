//go:build !select_request_types || request_type_http

package repository

import "testing"

// listingCollector is a configuration of one http collector with one list
// of its request, allowed_targets or denied_targets, as written.
func listingCollector(key, entries string) string {
	return "collectors:\n  - name: web\n    request:\n      type: http\n      " + key + ": " + entries + "\n    transform: {type: regex}\n    metrics:\n      - name: v\n        expression: 'v=(\\d+)'\n"
}

// An entry of request.allowed_targets or request.denied_targets is written
// in ASCII, as the host of a target is: the schema and the exporter refuse
// alike an internationalised name written in its own letters, a name in
// full-width letters or with another dot, a letter whose lower case is
// ASCII, a network in full-width digits and a space outside ASCII beside the
// entry, and take alike the name in its xn-- form, a glob of one, an
// address, a network, an entry YAML reads as a number and spaces around an
// entry. An entry of nothing at all is refused by both. What else an entry
// has to be — a name, a glob, an address or a network, with no scheme, port
// or path — the exporter alone checks.
// redirect_trusted_hosts, whose schema already took ASCII alone, is refused
// by the exporter too where lower case or trimming had made ASCII of the
// entry.
func TestSchemaAndExporterAgreeOnEntriesOutsideASCII(t *testing.T) {
	schema := loadSchema(t)
	for _, key := range []string{"allowed_targets", "denied_targets"} {
		for written, accepted := range map[string]bool{
			`[api.example.com]`: true, `["*.example.com"]`: true, `[xn--bcher-kva.example]`: true, `["XN--*"]`: true, `[my_service]`: true,
			`[10.0.0.0/8]`: true, `["::1"]`: true, `["[fd00::1]"]`: true, `[192.0.2.7]`: true, `[" api.example.com "]`: true,
			`[2130706433]`: true, `[127.1]`: true, `[]`: true, `[api.example.com, xn--bcher-kva.example, 10.0.0.0/8]`: true,

			`[bücher.example]`: false, `["*.bücher.example"]`: false, `[BÜCHER.example]`: false, `[ｏrigin.test]`: false, `[origin。test]`: false,
			`["\u212A.example"]`: false, `["\u0130nternal.example"]`: false, `[１０.０.０.０/８]`: false, `[１２７.０.０.１]`: false,
			`["api.example.com\u00A0"]`: false, `["\u3000api.example.com"]`: false, `["a\u200Db.example"]`: false, `[api.example.com, bücher.example]`: false,
		} {
			agree(t, schema, key+": "+written, listingCollector(key, written), accepted)
		}
		loadersAlone(t, schema, key+": a character no name has", listingCollector(key, `["a,b"]`), `request.`+key+` entry "a,b" is not a host name`)
		loadersAlone(t, schema, key+": a URL", listingCollector(key, `["https://api.example.com"]`), `request.`+key+` entry "https://api.example.com" is not a CIDR network`)
		agree(t, schema, key+": an empty entry", listingCollector(key, `[""]`), false)
		loadersAlone(t, schema, key+": an entry of blanks", listingCollector(key, `[" "]`), `request.`+key+` has an empty entry`)
	}
	for written, accepted := range map[string]bool{
		`[xn--bcher-kva.example]`: true, `["XN--*.example"]`: true,
		`[bücher.example]`: false, `[ｏrigin.test]`: false, `["\u212A.example"]`: false, `["\u0130nternal.example"]`: false,
		`["api.example.com\u00A0"]`: false, `["\u3000api.example.com"]`: false, `[api.example.com, bücher.example]`: false,
	} {
		agree(t, schema, "redirect_trusted_hosts: "+written, trustingCollector(written), accepted)
	}
}
