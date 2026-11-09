//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A static target's parameter value may hold a clock placeholder when the
// targets load against the configuration, and not a {{param_...}} one, nor
// one in a parameter that fills a label value.
func TestAStaticTargetsParameterIsCheckedForClockPlaceholders(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: daily
    request: {type: http, path: "/day/{{param_day}}"}
    transform:
      type: jq
      labels:
        tenant: "{{param_tenant:acme}}"
    metrics:
      - name: steps
        expression: .steps
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collectors[0].Clock.Any {
		t.Fatalf("a collector without clock placeholders reads the clock: %+v", cfg.Collectors[0].Clock)
	}
	check := func(params string) error {
		t.Helper()
		file, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", "interval: 1m\ntargets:\n  - name: me\n    collector: daily\n    target: http://api.invalid\n    params: "+params+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		return ValidateStaticTargetsAgainst(file, cfg)
	}
	if err := check(`{param_day: "{{date@Europe/Sofia}}"}`); err != nil {
		t.Errorf("a clock placeholder: %v", err)
	}
	for params, want := range map[string]string{
		`{param_day: "{{param_x}}"}`:                          `target "me" params.param_day holds {{param_x}}`,
		`{param_day: "{{date:1d}}"}`:                          `target "me" params.param_day placeholder {{date:1d}} is a clock placeholder written wrong`,
		`{param_day: "2026-11-09", param_tenant: "{{date}}"}`: `target "me" params.param_tenant holds a clock placeholder, and fills a label value of collector "daily"`,
	} {
		if err := check(params); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v\nwant %s", params, err, want)
		}
	}
	dated, err := Load(testutil.WriteFile(t, "dated.yaml", `collectors:
  - name: window
    request: {type: http, path: /events, query: {from: "{{unix_ms~last_success(-1h)}}"}}
    transform: {type: jq}
    metrics:
      - name: events
        expression: .count
`))
	if err != nil {
		t.Fatal(err)
	}
	if use := dated.Collectors[0].Clock; !use.Any || !use.Since {
		t.Fatalf("a collector that reads the last success: %+v", use)
	}
}
