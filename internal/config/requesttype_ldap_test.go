//go:build request_type_ldap

package config

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/ldaptest"
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
	own := c.Request.Own.(fetch.LDAPConfig)
	s := own.Searches[0]
	if c.Decoder.Type != "json" || !own.StartTLS || s.Scope != "" || s.Filter != "" || s.Attributes != nil || s.MaxEntries != 0 || len(cfg.Warnings) != 0 {
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

// A tls block written empty, tls: {}, makes an ldap scrape of a bare
// host:port speak TLS from the start, verified with the system roots, and
// its label ldaps://: the bind's password never goes to a plaintext server,
// which used to be where it went. ldap:// with the block is refused.
func TestAnEmptyTLSBlockMakesAnLDAPScrapeSpeakTLS(t *testing.T) {
	password := testutil.WriteFile(t, "password", "secret")
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: directory
    request:
      type: ldap
      tls: {}
      bind_dn: cn=exporter,dc=example,dc=org
      password_file: `+password+`
      searches:
        - name: people
          base: ou=people,dc=example,dc=org
          count_only: true
    transform: {type: jq}
    metrics: [{name: people, expression: .searches.people.count}]
`))
	if err != nil {
		t.Fatal(err)
	}
	c := &cfg.Collectors[0]
	scrape := func(target string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := fetch.FetchCollector(ctx, target, c, fetch.RequestOverrides{}, nil)
		return err
	}
	secure := ldaptest.NewTLS(t)
	if label, err := fetch.RequestLabelFor(secure.Addr, c, fetch.RequestOverrides{}); err != nil || label != "ldaps://"+secure.Addr {
		t.Errorf("label %q, %v", label, err)
	}
	// The stand-in's certificate is not among the system roots.
	if err := scrape(secure.Addr); err == nil || !strings.Contains(err.Error(), "TLS handshake with "+secure.Addr) {
		t.Errorf("a TLS server: %v", err)
	}
	plain := ldaptest.New(t)
	if err := scrape(plain.Addr); err == nil {
		t.Error("a plaintext server was scraped")
	}
	for _, r := range plain.Requests() {
		if r.Op == "BIND" {
			t.Errorf("the bind went to a plaintext server: %+v", r)
		}
	}
	if _, err := fetch.RequestLabelFor("ldap://"+plain.Addr, c, fetch.RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "target asks for plaintext with ldap://, but the collector sets request.tls") {
		t.Errorf("ldap:// with tls: {}: %v", err)
	}
}
