//go:build !select_request_types || request_type_graphite

package config

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

const graphiteConfig = `collectors:
  - name: graphite_app
    request:
      type: graphite
      targets:
        - app.*.requests.count
        - "seriesByTag('name=cpu.load', 'env={{param_env:prod}}')"
    response:
      graphite:
        max_age: 2m
    transform:
      type: jq
    metrics:
      - name: app_requests
        items: .series[]
        expression: .value
        labels:
          - name: host
            expression: .segments[1]
`

// A graphite collector loads with its defaults, reads with the graphite
// decoder without being told, and is not warned about its decoder.
func TestAGraphiteCollectorLoads(t *testing.T) {
	cfg, err := Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", graphiteConfig))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Collectors[0]
	if c.Decoder.Type != "graphite" || c.Request.Path != "/render" || c.Request.From != "-15min" || c.Request.Until != "now" || time.Duration(c.Response.Graphite.MaxAge) != 2*time.Minute {
		t.Fatalf("collector %+v", c)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("warnings %v", cfg.Warnings)
	}
	// Validating again, as a reload of a loaded configuration does, changes
	// nothing.
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	// Unknown keys under response.graphite are refused in the file's terms.
	_, err = Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", strings.Replace(graphiteConfig, "max_age: 2m", "maxage: 2m", 1)))
	if err == nil || !strings.Contains(err.Error(), `unknown key "maxage" in response.graphite`) {
		t.Fatalf("err=%v", err)
	}
}

// A static target of a graphite collector may set its own expressions and
// window, written out in full.
func TestGraphiteStaticTargets(t *testing.T) {
	cfg, err := Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", graphiteConfig))
	if err != nil {
		t.Fatal(err)
	}
	load := func(targets string) error {
		file, err := LoadStaticTargets(testutil.WriteIn(t, t.TempDir(), "static-targets.yaml", "interval: 1m\ntargets:\n"+targets))
		if err != nil {
			return err
		}
		if err := ValidateStaticTargets(file); err != nil {
			return err
		}
		return ValidateStaticTargetsAgainst(file, cfg)
	}
	if err := load(`  - collector: graphite_app
    target: http://graphite.example:8080
    params:
      param_env: staging
  - collector: graphite_app
    target: http://graphite.example:8080
    request:
      targets: [db.*.connections]
      from: -1h
      until: -1min
`); err != nil {
		t.Fatal(err)
	}
	for targets, want := range map[string]string{
		"  - name: t\n    collector: graphite_app\n    target: http://graphite.example:8080\n    request:\n      targets: [\"app.{{param_host}}.x\"]\n": `target "t" request.targets[0] cannot use {{param_...}} placeholders`,
		"  - name: t\n    collector: graphite_app\n    target: http://graphite.example:8080\n    request:\n      targets: [\"f(a\"]\n":                  `target "t": request.targets[0] "f(a": has a ( that is never closed`,
		"  - name: t\n    collector: graphite_app\n    target: graphite.example:8080\n":                                                                 `target "t": must have an absolute target URL`,
		"  - name: t\n    collector: graphite_app\n    target: http://graphite.example:8080\n    request:\n      targets: a.b\n":                        `expected a list of values, not a string`,
		"  - name: t\n    collector: graphite_app\n    target: http://graphite.example:8080\n    request:\n      method: POST\n":                        `sets request.method, which does not apply to collector "graphite_app"`,
	} {
		if err := load(targets); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err=%v, want %q", targets, err, want)
		}
	}
}
