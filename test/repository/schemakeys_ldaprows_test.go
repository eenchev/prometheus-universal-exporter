//go:build !select_request_types || request_type_ldap

package repository

import "strings"

// ldapCollector is a configuration of one ldap collector of one search,
// with a comment where a row writes a key of its request, and another
// where a row writes a key of its search.
const ldapCollector = `collectors:
  - name: demo
    request:
      type: ldap
      # request
      searches:
        - name: people
          # search
    transform:
      type: jq
    metrics:
      - name: v
        expression: .searches.people.count
`

// ldapSearchless is ldapCollector with no search, where a row writes
// searches itself.
const ldapSearchless = `collectors:
  - name: demo
    request:
      type: ldap
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .searches.people.count
`

// ldapSchemaKeys are the keys of an ldap collector that a schema holds to
// a rule: a search's name, letters, digits and _ not starting with a
// digit, which a search needs and may not be written ""; its scope, base,
// one or sub, "" the default; an attribute it asks for, a name or an OID,
// * or +; and bind_dn and password_file, each of which needs the other
// beside it, so that one alone, or written "" beside the other, is
// refused. They are held in every build's schema, the default build's
// among them, which does not carry the opt-in type
// (TestEveryConstrainedKeyOfTheSchemasIsInATable), and the two verdicts
// are compared in a build with it (schemakeys_ldap_test.go).
func ldapSchemaKeys() []schemaKey {
	request, search := "      # request\n", "          # search\n"
	return []schemaKey{
		{key: "collectors[].request.searches[].name", document: ldapSearchless, at: request, setting: "      searches:\n        - name: %s\n", valid: "people", invalid: "all-people", built: true},
		{key: "collectors[].request.searches[].scope", document: ldapCollector, at: search, setting: "          scope: %s\n", valid: "one", invalid: "SUB", absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.searches[].attributes[]", document: ldapCollector, at: search, setting: "          attributes: [%s]\n", valid: "cn", invalid: `"given name"`, absent: true, built: true},
		{key: "collectors[].request.bind_dn", document: ldapCollector, at: request, setting: "      password_file: /var/run/secrets/ldap/password\n      bind_dn: %s\n", valid: "cn=exporter,dc=example,dc=org", invalid: `" "`, absent: true, emptyWas: taken, booleanAlone: `request.bind_dn "true" is not a DN`, numberAlone: `request.bind_dn "1" is not a DN`, built: true},
		// A target's own bind_dn needs its own password_file, which alone
		// is the password of the collector's bind_dn: "" is the target's
		// bind_dn left out, the collector's then bound as.
		{key: "targets[].request.bind_dn", file: inTargetFile, configuration: strings.Replace(ldapCollector, request, "      bind_dn: cn=exporter,dc=example,dc=org\n      password_file: /p\n", 1), document: "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: ldap.example\n    request:\n      password_file: /t\n      # target\n", at: "      # target\n", setting: "      bind_dn: %s\n", valid: "cn=tenant,dc=example,dc=org", invalid: `" "`, absent: true, empty: true, booleanAlone: `request.bind_dn "true" is not a DN`, numberAlone: `request.bind_dn "1" is not a DN`, built: true},
		{key: "collectors[].request.password_file", of: "an ldap collector's, beside bind_dn", document: ldapCollector, at: request, setting: "      bind_dn: cn=exporter,dc=example,dc=org\n      password_file: %s\n", valid: "/var/run/secrets/ldap/password", invalid: `" "`, absent: true, emptyWas: taken, invalidWas: taken, built: true},
	}
}
