//go:build !request_type_ldap

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A build without the opt-in ldap type, the default build among them,
// refuses a configuration with an ldap collector when it loads, saying the
// type exists and is opt-in, and how to build with it: the variable of the
// Makefile and the Dockerfile, and the tag.
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
	for _, want := range []string{`collector "directory" uses request.type "ldap", which this build of the exporter does not include`, "ldap is an opt-in type, which the default build and the published image leave out", "OPT_IN_REQUEST_TYPES=ldap", "request_type_ldap"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should say %q", err, want)
		}
	}
}
