//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// web.basic_auth and otlp do nothing until enabled turns them on. Settings
// without the switch were ignored without a word — credentials that protected
// nothing, an endpoint nothing was exported to — so a block that sets any
// other key must say enabled, either way.

// A block that sets keys but not enabled is refused, naming the block, the
// keys and both ways out, with the line of the first key; both blocks are
// reported in one pass.
func TestSettingsWithoutEnabledAreRefused(t *testing.T) {
	document := `web:
  basic_auth:
    username: admin
    password: s3cret
otlp:
  endpoint: http://collector:4318/v1/metrics
` + testutil.MinimalConfig
	_, err := Load(testutil.WriteFile(t, "config.yaml", document))
	if err == nil {
		t.Fatal("credentials and an endpoint without enabled loaded")
	}
	for _, want := range []string{
		"line 3: web.basic_auth sets username and password but not enabled; say enabled: true to turn it on, or enabled: false to keep the settings without using them",
		"line 6: otlp sets endpoint but not enabled; say enabled: true to turn it on, or enabled: false to keep the settings without using them",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	// An enabled left empty says nothing, and a key set to its zero value
	// is still a key the block sets.
	for name, block := range map[string]string{
		"an empty enabled": "otlp:\n  enabled:\n  endpoint: http://collector:4318/v1/metrics\n",
		"a zero value":     "otlp: {probe_attributes: false}\n",
		"a merged setting": "x-otlp: &otlp {endpoint: 'http://collector:4318/v1/metrics'}\notlp: {<<: *otlp, interval: 10s}\n",
		// The block's own enabled is the one read, and it is empty.
		"an empty enabled over a merged one": "x-otlp: &otlp {enabled: true, endpoint: 'http://collector:4318/v1/metrics'}\notlp: {<<: *otlp, enabled: }\n",
		"files, not values":                  "web: {basic_auth: {username_file: /run/u, password_file: /run/p}}\n",
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", block+testutil.MinimalConfig)); err == nil || !strings.Contains(err.Error(), "but not enabled; say enabled: true") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// enabled: false keeps a block's settings without using or checking them,
// enabled: true uses them, a merge key may supply the switch, and a block
// that sets nothing needs none. A block written with no value at all is not
// one that sets nothing: it is a key with no value, refused as every such
// key is (TestAKeyAnEntryOrAMappingValueWithNoValueIsRefused).
func TestEnabledSaidEitherWayIsAccepted(t *testing.T) {
	for name, test := range map[string]struct {
		block      string
		otlp, auth bool
	}{
		"off, settings kept": {"web:\n  basic_auth: {enabled: false, username: admin}\notlp: {enabled: false, endpoint: 'not a url', interval: 1ns}\n", false, false},
		"on":                 {"web:\n  basic_auth: {enabled: true, username: admin, password: s3cret}\notlp: {enabled: true, endpoint: 'http://collector:4318/v1/metrics'}\n", true, true},
		"merged in":          {"x-on: &on {enabled: true}\notlp: {<<: *on, endpoint: 'http://collector:4318/v1/metrics'}\n", true, false},
		"empty blocks":       {"web: {basic_auth: {}}\notlp: {}\n", false, false},
	} {
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", test.block+testutil.MinimalConfig))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if auth := cfg.Web.BasicAuth != nil && cfg.Web.BasicAuth.Enabled; cfg.OTLP.Enabled != test.otlp || auth != test.auth {
			t.Errorf("%s: otlp enabled %v, web.basic_auth enabled %v", name, cfg.OTLP.Enabled, auth)
		}
	}
	_, err := Load(testutil.WriteFile(t, "config.yaml", "web:\n  basic_auth:\notlp:\n"+testutil.MinimalConfig))
	if want := "line 2: basic_auth has nothing after its colon, which YAML reads as no value at all; write its value, or take the key out; line 3: otlp has nothing after its colon, which YAML reads as no value at all; write its value, or take the key out"; err == nil || err.Error() != want {
		t.Errorf("blocks with no value at all: %v\nwant %s", err, want)
	}
}

// The two blocks decode themselves to see whether enabled was said, and still
// refuse what the file's decoder refuses, in the file's terms: an unknown
// key, a value of the wrong kind, and a block that is not a mapping, each
// with its line and together with the missing switch.
func TestSwitchedBlocksAreStillCheckedAsTheRestOfTheFile(t *testing.T) {
	for name, test := range map[string]struct{ block, want string }{
		"unknown otlp key":        {"otlp:\n  enabled: true\n  endpint: http://collector:4318\n", `line 3: unknown key "endpint" in otlp`},
		"unknown basic_auth key":  {"web:\n  basic_auth:\n    enabled: true\n    user: admin\n", `line 4: unknown key "user" in web.basic_auth`},
		"unknown nested key":      {"otlp:\n  enabled: true\n  tls:\n    ca: /ca.pem\n", `line 4: unknown key "ca" in tls`},
		"a list for a value":      {"otlp:\n  enabled: true\n  endpoint: [a]\n", "line 3: expected a single value, not a list"},
		"a bad duration":          {"otlp:\n  enabled: true\n  interval: soon\n", `line 3: "soon" is not a duration`},
		"otlp as a list":          {"otlp: [enabled]\n", "line 1: otlp must be a mapping, not a list"},
		"basic_auth as a value":   {"web:\n  basic_auth: admin\n", "line 2: web.basic_auth must be a mapping, not a string"},
		"both an unknown key and": {"otlp:\n  endpint: http://collector:4318\n", `line 2: unknown key "endpint" in otlp; line 2: otlp sets endpint but not enabled`},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", test.block+testutil.MinimalConfig))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}
