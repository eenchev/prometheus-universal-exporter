//go:build request_type_ldap

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An ldap collector is answered with a JSON document, so its decoder is
// json without a word, and a decoder or a transform that cannot read JSON
// is refused. Its searches are read from YAML with their defaults left
// out: no scope, no filter, no attributes and no max_entries, and a filter
// of an extensible match is taken.
func TestAnLDAPCollectorDecodesJSON(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: directory
    request:
      type: ldap
      start_tls: true
      searches:
        - name: people
          base: ou=people,dc=example,dc=org
        - name: disabled
          base: dc=example,dc=org
          scope: one
          filter: (userAccountControl:1.2.840.113556.1.4.803:=2)
          count_only: true
    transform:
      type: jq
    metrics:
      - name: people
        expression: .searches.people.count
`))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Collectors[0]
	s := c.Request.Searches[0]
	if c.Decoder.Type != "json" || !c.Request.StartTLS || s.Scope != "" || s.Filter != "" || s.Attributes != nil || s.MaxEntries != 0 || len(cfg.Warnings) != 0 {
		t.Errorf("decoder %q, request %+v, warnings %q", c.Decoder.Type, c.Request, cfg.Warnings)
	}
	for transform, want := range map[string]string{"regex": "transforms with regex, which cannot read the JSON an ldap collector is answered with", "jq\n    decoder:\n      type: yaml": "decodes with yaml, but an ldap collector's answer is JSON"} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: directory
    request:
      type: ldap
      searches:
        - name: people
    transform:
      type: `+transform+`
    metrics:
      - name: v
        expression: .searches
`))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", transform, err)
		}
	}
}
