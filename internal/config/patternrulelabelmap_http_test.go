//go:build !select_request_types || request_type_http

package config

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// scrapeWithParams decodes body as the collector's response and transforms
// it as a probe with the parameters params would, and returns the series
// and the validation's verdict on them.
func scrapeWithParams(t *testing.T, c *model.Collector, body string, params map[string]string) (*model.MetricSet, error) {
	t.Helper()
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := transform.Transform(transform.WithLabelParams(context.Background(), params), d, r, c, "")
	if err != nil {
		return nil, err
	}
	return set, set.Validate(c.Limits)
}

// patternRuleConfig is a prometheus collector with a rule without a name,
// whose expression is pattern, giving the label tenant the value value, and
// a rule named foo that maps the label tenant by a value_map whose "*" entry
// is over the limit; extra is added to the named rule's label.
func patternRuleConfig(pattern, value, extra string) string {
	return `collectors:
  - name: a
    request: {type: http}
    limits: {max_label_value_length: 8}
    transform: {type: prometheus}
    metrics:
      - expression: '` + pattern + `'
        labels:
          - {name: tenant, value: "` + value + `"}
      - name: foo
        expression: '^other$'
        labels:
          - {name: tenant, expression: tenant, value_map: {t: short, "*": overlongvalue}` + extra + `}
`
}

// patternRuleBody has a series foo, which the rule without a name keeps
// under its own name, and the series the named rule reads.
const patternRuleBody = "foo 1\nother{tenant=\"t\"} 2\n"

// failsEveryProbe reports whether the collector of doc, loaded with
// truncate: true on the label of its first rule and scraped without it, as
// the load would have taken it without the check, fails the scrape of
// every probe of a kind: no parameter, the default's value, another value.
func failsEveryProbe(t *testing.T, doc string) bool {
	t.Helper()
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", strings.Replace(doc, `"}`, `", truncate: true}`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	c := &cfg.Collectors[0]
	c.Metrics[0].Labels[0].Truncate = false
	for _, params := range []map[string]string{{}, {"param_tenant": "acme"}, {"param_tenant": "globex"}} {
		if _, err := scrapeWithParams(t, c, patternRuleBody, params); err == nil {
			return false
		}
	}
	return true
}

// A prometheus rule without a name keeps each series' own name, and the
// value_map of a rule named as a series maps that series' label at the
// scrape: the load measures the rule's static label, a constant or one with
// placeholders, against the map of every other rule's name its expression
// matches, as it is matched at the scrape, unanchored and before
// metrics_prefix, and refuses where every probe would fail, naming the
// metric whose map it is; an expression that matches no such name loads.
func TestARuleWithoutANameIsMeasuredByTheValueMapOfEachNameItsExpressionMatches(t *testing.T) {
	for _, value := range []string{"{{param_tenant:acme}}", "acme"} {
		for _, pattern := range []string{"^foo$", "fo", "^f.o$"} {
			doc := patternRuleConfig(pattern, value, "")
			_, err := Load(testutil.WriteFile(t, "config.yaml", doc))
			if err == nil || !strings.Contains(err.Error(), `metrics rule 1 label "tenant"`) || !strings.Contains(err.Error(), `the value_map of the metric "foo", a name the rule's expression matches and keeps`) || !strings.Contains(err.Error(), "max_label_value_length 8") {
				t.Errorf("value %q, expression %q: loaded with %v", value, pattern, err)
			}
			if !failsEveryProbe(t, doc) {
				t.Errorf("value %q, expression %q: refused, yet a probe passes", value, pattern)
			}
			prefixed := strings.Replace(doc, "    request:", "    metrics_prefix: vendor\n    request:", 1)
			if _, err := Load(testutil.WriteFile(t, "config.yaml", prefixed)); err == nil {
				t.Errorf("value %q, expression %q, with metrics_prefix: loaded", value, pattern)
			}
		}
		if _, err := Load(testutil.WriteFile(t, "config.yaml", patternRuleConfig("^bar$", value, ""))); err != nil {
			t.Errorf("value %q, an expression that does not match foo: %v", value, err)
		}
	}
}

// truncate: true on the label in the named rule cuts the label of every
// series of that name at the scrape, after the map, those the rule without a
// name keeps under it among them: that rule's label then loads, and every
// probe passes.
func TestTruncateInTheRuleOfTheNameSparesTheLabelOfARuleWithoutAName(t *testing.T) {
	for _, value := range []string{"{{param_tenant:acme}}", "acme"} {
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", patternRuleConfig("^foo$", value, ", truncate: true")))
		if err != nil {
			t.Fatalf("value %q: %v", value, err)
		}
		for _, params := range []map[string]string{{}, {"param_tenant": "acme"}, {"param_tenant": "globex"}} {
			set, err := scrapeWithParams(t, &cfg.Collectors[0], patternRuleBody, params)
			if err != nil {
				t.Errorf("value %q, probe %v: %v", value, params, err)
				continue
			}
			if got := tenantOf(set, "foo"); got != "overl…" {
				t.Errorf("value %q, probe %v: foo has tenant %q, want the mapped value cut", value, params, got)
			}
		}
	}
}

// tenantOf is the tenant label of the series named name in set.
func tenantOf(set *model.MetricSet, name string) string {
	for i := range set.Metrics {
		if set.Metrics[i].Name == name {
			return set.Metrics[i].Labels["tenant"]
		}
	}
	return "<no series>"
}

// truncate: true on a label in another rule of the same name cuts the
// label of every series of the name at the scrape, so the load spares the
// label of each rule of the name, a templated value and a constant alike: a
// configuration whose every probe passes loads.
func TestTruncateInAnotherRuleOfTheNameSparesTheLabelAtLoad(t *testing.T) {
	for _, value := range []string{"{{param_tenant:acme}}", "acme"} {
		doc := `collectors:
  - name: a
    request: {type: http}
    limits: {max_label_value_length: 8}
    transform: {type: jq}
    metrics:
      - name: m
        expression: .x
        labels:
          - {name: tenant, value: "` + value + `"}
      - name: m
        expression: .y
        labels:
          - {name: tenant, expression: .t, value_map: {u: short, "*": overlongvalue}, truncate: true}
`
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", doc))
		if err != nil {
			t.Fatalf("value %q: %v", value, err)
		}
		for _, params := range []map[string]string{{}, {"param_tenant": "acme"}, {"param_tenant": "globex"}} {
			if _, err := scrapeWithParams(t, &cfg.Collectors[0], `{"x": 1, "y": 2, "t": "u"}`, params); err != nil {
				t.Errorf("value %q, probe %v: %v", value, params, err)
			}
		}
		other := strings.Replace(doc, "      - name: m\n        expression: .y", "      - name: n\n        expression: .y", 1)
		if _, err := Load(testutil.WriteFile(t, "config.yaml", other)); err != nil {
			t.Errorf("value %q, the truncating rule of another name, which maps nothing of m: %v", value, err)
		}
	}
}

// truncate: true on the label of a rule without a name cuts the value the
// value_map of the series' name maps it to, not the value before the map,
// so every probe passes and the label is exported cut.
func TestTruncateOnARuleWithoutANameCutsTheMappedValue(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", strings.Replace(patternRuleConfig("^foo$", "{{param_tenant:acme}}", ""), `"}`, `", truncate: true}`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	for _, params := range []map[string]string{{}, {"param_tenant": "acme"}, {"param_tenant": "globex"}} {
		set, err := scrapeWithParams(t, &cfg.Collectors[0], patternRuleBody, params)
		if err != nil {
			t.Errorf("probe %v: %v", params, err)
			continue
		}
		if got := tenantOf(set, "foo"); got != "overl…" {
			t.Errorf("probe %v: foo has tenant %q, want the mapped value cut", params, got)
		}
	}
}
