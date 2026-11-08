//go:build select_request_types && !request_type_ldap

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A build of select_request_types without the ldap type refuses a
// configuration with an ldap collector when it loads, saying the type exists
// and that the default build includes it, and the tag that adds it.
func TestABuildWithoutLDAPRefusesAnLDAPCollector(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: directory
    request:
      type: ldap
      searches:
        - name: people
          base: ou=people,dc=example,dc=org
          count_only: true
    transform:
      type: jq
    metrics:
      - name: people
        expression: .searches.people.count
`))
	if err == nil {
		t.Fatal("an ldap collector loaded in a build without the type")
	}
	for _, want := range []string{`collector "directory" uses request.type "ldap", which this build of the exporter does not include`, "the default build includes every type", "request_type_ldap"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should say %q", err, want)
		}
	}
}
