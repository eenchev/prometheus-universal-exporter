//go:build !select_request_types || request_type_http

package config

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// Placeholders in a collector's fixed label values (fetch/labelparams.go),
// as the configuration reads and checks them when it loads.

// The placeholders of a collector's label values are read once, when the
// configuration loads, and kept with the collector: the values of
// transform.labels and the static value of a rule's label, each named as
// its errors name it. A collector whose labels hold none has nothing kept.
func TestTheLabelPlaceholdersAreReadWhenTheConfigurationLoads(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: tenant_status
    request: {type: http, path: "/api/{{param_tenant}}/status"}
    transform:
      type: jq
      labels:
        tenant: "{{param_tenant}}"
        region: "{{param_region:eu}}"
        site: dc1
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: source, value: "api-{{param_tenant}}"}
          - {name: fixed, value: "{{not_a_param}}"}
          - {name: read, expression: '.regions["{{param_region}}"]'}
  - name: plain
    request: {type: http}
    transform:
      type: jq
      labels: {site: "{{dc}}"}
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: fixed, value: x}
`))
	if err != nil {
		t.Fatal(err)
	}
	labels := cfg.Collectors[0].LabelParams
	if labels == nil || len(labels.Collector) != 2 || len(labels.Rules) != 1 {
		t.Fatalf("read as %+v", labels)
	}
	var where []string
	for _, template := range append(slices.Clone(labels.Collector), labels.Rules...) {
		where = append(where, template.Where)
	}
	if want := []string{"transform.labels.region", "transform.labels.tenant", `metric "status_up" label "source" value`}; !slices.Equal(where, want) {
		t.Errorf("the values that hold placeholders: %q", where)
	}
	if cfg.Collectors[1].LabelParams != nil {
		t.Errorf("a collector without a label placeholder has %+v", cfg.Collectors[1].LabelParams)
	}
	// What a probe is checked by and what the collectors page lists know
	// the labels' parameters with the request's.
	params := fetch.RequestParams(&cfg.Collectors[0])
	if len(params) != 2 || params[0].Name != "param_region" || params[0].Default != "eu" || params[1].Name != "param_tenant" || !params[1].Required {
		t.Errorf("the collector's parameters: %+v", params)
	}
}

// A placeholder of a label value that is not well formed stops the load,
// naming the collector, the value and what is wrong with it: a space after
// the braces, a filter, which a label value does not take, a placeholder
// left open. A rule without a name is named by its place. A default that
// is not valid UTF-8 cannot be written: a file that is not UTF-8 is no
// configuration, and stops the load before any of it is read.
func TestALabelPlaceholderThatIsNotWellFormedStopsTheLoad(t *testing.T) {
	for name, tc := range map[string]struct{ value, want string }{
		"a space":  {`"{{ param_x }}"`, " has a placeholder with a space after {{; write {{param_<name>}} without spaces"},
		"a filter": {`"{{param_x|json}}"`, " placeholder {{param_x|json}} has a filter; a label value is written one way, as it is given, so write the placeholder without a |, which a default cannot hold either"},
		"unclosed": {`"api-{{param_x"`, ` has an unclosed placeholder at "{{param_x"; write {{param_name}} or {{param_name:default}}`},
	} {
		got := problemsOf(t, checkedCollector("", "", "")+"  - name: b\n    request: {type: http}\n    transform:\n      type: jq\n      labels:\n        tenant: "+tc.value+"\n    metrics:\n      - name: m\n        expression: .x\n")
		if want := `collector "b" transform.labels.tenant` + tc.want; len(got) != 1 || got[0] != want {
			t.Errorf("%s, in transform.labels: %q\nwant %q", name, got, want)
		}
		got = problemsOf(t, checkedCollector("", "", "        labels:\n          - name: tenant\n            value: "+tc.value+"\n"))
		if want := `collector "a" metric "m" label "tenant" value` + tc.want; len(got) != 1 || got[0] != want {
			t.Errorf("%s, in a rule's label: %q\nwant %q", name, got, want)
		}
		got = problemsOf(t, collectorRules("prometheus", "", "      - name: up\n      - expression: '^node_'\n        labels:\n          - name: tenant\n            value: "+tc.value+"\n"))
		if want := `collector "node" metrics rule 2 label "tenant" value` + tc.want; len(got) != 1 || got[0] != want {
			t.Errorf("%s, in the label of a rule without a name: %q\nwant %q", name, got, want)
		}
	}
	_, err := Load(testutil.WriteFile(t, "config.yaml", checkedCollector("", "    transform: {type: jq, labels: {tenant: \"{{param_x:a\xffb}}\"}}\n", "")))
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("a default that is not UTF-8: %v", err)
	}
}

// A python rule's label takes no value, with a placeholder or without: the
// script sets the labels of its series, and the refusal says where a
// constant for every series goes, which takes placeholders.
func TestAPythonRulesLabelTakesNoValueWithAPlaceholder(t *testing.T) {
	got := problemsOf(t, collectorRules("python", "", "      - name: up\n        labels:\n          - name: tenant\n            value: \"{{param_tenant}}\"\n            truncate: true\n"))
	if len(got) != 1 || !strings.HasPrefix(got[0], `collector "node" metric "up" label "tenant" sets value, which a python rule's label does not take`) || !strings.HasSuffix(got[0], "for a constant on every series of the collector, set transform.labels") {
		t.Errorf("a python rule's label with a placeholder: %q", got)
	}
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: node\n    request: {type: http}\n    transform:\n      type: python\n      script: metric('v', 'gauge', 1)\n      labels:\n        tenant: \"{{param_tenant}}\"\n"))
	if err != nil || cfg.Collectors[0].LabelParams == nil {
		t.Errorf("transform.labels of a python collector: %v", err)
	}
}

// A label value that holds placeholders is held to
// limits.max_label_value_length, when the configuration loads, as a probe
// that gives no parameter fills it: its own text and its defaults, a
// placeholder without a default counting for nothing. One that is too long
// so fails every probe that leaves the parameters out, and is refused as a
// constant too long is, with what a constant is spared by: truncate: true
// and remove_labels. A value_map of the rule's name that lists neither the
// value its defaults give nor "*" leaves that value as it is, measured and
// refused alike (TestATemplatedLabelIsMeasuredAsItsValueMapMapsIt).
func TestALabelValueWithPlaceholdersIsMeasuredByItsDefaults(t *testing.T) {
	const small = "    limits:\n      max_label_value_length: 8\n"
	wide := func(value, settings string) []string {
		return problemsOf(t, "collectors:\n  - name: a\n    request: {type: http}\n"+small+"    transform:\n      type: jq\n      labels:\n        tenant: \""+value+"\"\n"+settings+"    metrics:\n      - name: m\n        expression: .x\n")
	}
	rule := func(value, label, settings, more string) []string {
		return problemsOf(t, "collectors:\n  - name: a\n    request: {type: http}\n"+small+"    transform:\n      type: jq\n"+settings+"    metrics:\n      - name: m\n        expression: .x\n        labels:\n          - name: tenant\n            value: \""+value+"\"\n"+label+more)
	}
	for value, length := range map[string]int{
		"{{param_tenant}}":                          0,
		"{{param_tenant:acme}}":                     0,
		"api-{{param_tenant}}":                      0,
		"{{param_a:1234}}{{param_b}}{c}":            0,
		"api-{{param_a:1234}}":                      0,
		"{{param_tenant:acme-corp}}":                9,
		"tenant-{{param_tenant}}-x":                 9,
		"api-{{param_a:12}}-{{param_b:34}}{}":       11,
		"{{param_a:12345}}{{param_b}}{{c}}":         10,
		"api-{{param_a:1234}}{{param_b:5}}":         9,
		"é{{param_a:é}}{{param_b:é}}é{{param_p:}}é": 10,
	} {
		wantWide, wantRule := []string(nil), []string(nil)
		if length > 0 {
			wantWide = []string{fmt.Sprintf(`collector "a" transform.labels "tenant" is %d bytes once its placeholders take their defaults, longer than limits.max_label_value_length 8, so every series of a probe that gives them no other value would fail validation; shorten the text or the defaults, or raise the limit`, length)}
			wantRule = []string{fmt.Sprintf(`collector "a" metric "m" label "tenant" value is %d bytes once its placeholders take their defaults, longer than limits.max_label_value_length 8, so every series of a probe that gives them no other value would fail validation; shorten the text or the defaults, set truncate: true on the label, or raise the limit`, length)}
		}
		if !strings.Contains(value, "{{param_") {
			// No placeholder: a constant, measured and refused as one.
			wantWide = []string{fmt.Sprintf(`collector "a" transform.labels "tenant" is %d bytes, longer than limits.max_label_value_length 8, so every series would fail validation; shorten it or raise the limit`, length)}
		}
		if got := wide(value, ""); !slices.Equal(got, wantWide) {
			t.Errorf("transform.labels %q: %q\nwant %q", value, got, wantWide)
		}
		if got := rule(value, "", "", ""); !slices.Equal(got, wantRule) {
			t.Errorf("a rule's label %q: %q\nwant %q", value, got, wantRule)
		}
		// What spares a constant spares a value with placeholders.
		if got := wide(value, "      remove_labels: [tenant]\n"); got != nil {
			t.Errorf("transform.labels %q under remove_labels: %q", value, got)
		}
		if got := rule(value, "            truncate: true\n", "", ""); got != nil {
			t.Errorf("a rule's label %q with truncate: %q", value, got)
		}
		if got := rule(value, "", "      remove_labels: [tenant]\n", ""); got != nil {
			t.Errorf("a rule's label %q under remove_labels: %q", value, got)
		}
		if got := rule(value, "", "", "      - name: m\n        expression: .y\n        labels:\n          - name: tenant\n            expression: .t\n            value_map: {a: b}\n"); !slices.Equal(got, wantRule) {
			t.Errorf("a rule's label %q beside a value_map of the rule's name that maps none of it: %q\nwant %q", value, got, wantRule)
		}
	}
	// A constant is measured as it was.
	if got := wide("acme-corp", ""); !slices.Equal(got, []string{`collector "a" transform.labels "tenant" is 9 bytes, longer than limits.max_label_value_length 8, so every series would fail validation; shorten it or raise the limit`}) {
		t.Errorf("a constant in transform.labels: %q", got)
	}
	if got := rule("acme-corp", "", "", ""); !slices.Equal(got, []string{`collector "a" metric "m" label "tenant" value is 9 bytes, longer than limits.max_label_value_length 8, so every series would fail validation; shorten it, set truncate: true on the label, or raise the limit`}) {
		t.Errorf("a constant in a rule's label: %q", got)
	}
}

// A rule's label value that holds placeholders, where a value_map of the
// rule's name maps the label, is measured as the map makes it. A "*" entry
// longer than the limit is what every value the probe gives that the map
// does not list becomes, so the label is refused whatever its defaults are,
// naming the entry, where it once loaded and failed every scrape that gave
// such a value; truncate: true and remove_labels spare it, as they spare a
// constant. A long entry the map lists is the probe's to reach: the label
// loads, unless its defaults give that entry's value, which every probe that
// leaves the parameters out then gives. A constant beside the same maps is
// refused, or loads, as it was.
func TestATemplatedLabelIsMeasuredAsItsValueMapMapsIt(t *testing.T) {
	rules := func(value, label, settings, valueMap string) []string {
		return problemsOf(t, "collectors:\n  - name: a\n    request: {type: http}\n    limits:\n      max_label_value_length: 8\n    transform:\n      type: jq\n"+settings+"    metrics:\n      - name: m\n        expression: .x\n        labels:\n          - name: tenant\n            value: \""+value+"\"\n"+label+
			"      - name: m\n        expression: .y\n        labels:\n          - name: tenant\n            expression: .t\n            value_map: "+valueMap+"\n")
	}
	const (
		star   = `collector "a" metric "m" label "tenant" value holds {{param_...}} placeholders, and the value_map of the metric's name maps every value it does not list, by its "*" entry, to 13 bytes, longer than limits.max_label_value_length 8, so every series of a probe that gives a value not listed would fail validation; shorten the "*" entry or take it out, set truncate: true on the label, or raise the limit`
		listed = `collector "a" metric "m" label "tenant" value is mapped once its placeholders take their defaults, by the "acme" entry of the value_map of the metric's name, to 13 bytes, longer than limits.max_label_value_length 8, so every series of a probe that gives them no other value would fail validation; shorten that entry or change the defaults, set truncate: true on the label, or raise the limit`
		long   = `collector "a" metric "m" label "tenant" value is 13 bytes, longer than limits.max_label_value_length 8, so every series would fail validation; shorten it, set truncate: true on the label, or raise the limit`
	)
	for _, tc := range []struct{ value, valueMap, want string }{
		// "*" too long: refused whatever the defaults, a listed one too.
		{"{{param_tenant}}", `{"*": overlongvalue}`, star},
		{"{{param_tenant:x}}", `{"*": overlongvalue}`, star},
		{"api-{{param_tenant}}", `{acme: short, "*": overlongvalue}`, star},
		{"{{param_tenant:acme}}", `{acme: short, "*": overlongvalue}`, star},
		// A long listed entry: the probe's, unless the defaults give it.
		{"{{param_tenant}}", `{acme: overlongvalue}`, ""},
		{"{{param_tenant:x}}", `{acme: overlongvalue, "*": short}`, ""},
		{"api-{{param_tenant:acme}}", `{acme: overlongvalue}`, ""},
		{"{{param_tenant:acme}}", `{acme: overlongvalue}`, listed},
		{"{{param_tenant:ac}}{{param_rest:me}}", `{acme: overlongvalue, "*": short}`, listed},
		// Defaults too long by themselves, mapped short or left alone.
		{"{{param_tenant:acme-corp}}", `{acme-corp: short}`, ""},
		{"{{param_tenant:acme-corp}}", `{"*": ""}`, ""},
		{"{{param_tenant:acme-corp}}", `{acme: short}`, `collector "a" metric "m" label "tenant" value is 9 bytes once its placeholders take their defaults, longer than limits.max_label_value_length 8, so every series of a probe that gives them no other value would fail validation; shorten the text or the defaults, set truncate: true on the label, or raise the limit`},
		// The constants beside the same maps, as they were.
		{"x", `{"*": overlongvalue}`, long},
		{"acme", `{acme: short, "*": overlongvalue}`, ""},
		{"acme", `{acme: overlongvalue}`, long},
		{"x", `{acme: overlongvalue}`, ""},
	} {
		var want []string
		if tc.want != "" {
			want = []string{tc.want}
		}
		if got := rules(tc.value, "", "", tc.valueMap); !slices.Equal(got, want) {
			t.Errorf("%q under value_map %s:\n got %q\nwant %q", tc.value, tc.valueMap, got, want)
		}
		if got := rules(tc.value, "            truncate: true\n", "", tc.valueMap); got != nil {
			t.Errorf("%q under value_map %s with truncate: %q", tc.value, tc.valueMap, got)
		}
		if got := rules(tc.value, "", "      remove_labels: [tenant]\n", tc.valueMap); got != nil {
			t.Errorf("%q under value_map %s under remove_labels: %q", tc.value, tc.valueMap, got)
		}
	}
}

// A label that loads beside a value_map whose long entries the map lists
// fails the scrape of a probe that gives one of those values, in the
// validation, as a long value the label is given does; a probe that gives
// another value is mapped short, or left as it is, and passes.
func TestAProbeThatFillsALongMappedValueFailsItsScrape(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: a
    request: {type: http}
    limits: {max_label_value_length: 8}
    transform: {type: jq}
    metrics:
      - name: m
        expression: .x
        labels:
          - {name: tenant, value: "{{param_tenant}}"}
      - name: m
        expression: .y
        labels:
          - {name: tenant, expression: .t, value_map: {acme: overlongvalue, globex: short}}
`))
	if err != nil {
		t.Fatal(err)
	}
	c := &cfg.Collectors[0]
	r := &fetch.HTTPResponse{Body: []byte(`{"x": 1, "y": 2, "t": "umbrella"}`), Headers: http.Header{}}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	scrape := func(tenant string) error {
		set, err := transform.Transform(transform.WithLabelParams(context.Background(), map[string]string{"param_tenant": tenant}), d, r, c, "")
		if err != nil {
			return err
		}
		return set.Validate(c.Limits)
	}
	if err := scrape("acme"); err == nil || !strings.Contains(err.Error(), `metric "m" label "tenant" value is 13 bytes, longer than limits.max_label_value_length 8`) {
		t.Errorf("a probe that gives a value mapped long: %v", err)
	}
	for _, tenant := range []string{"globex", "initech"} {
		if err := scrape(tenant); err != nil {
			t.Errorf("a probe that gives %q: %v", tenant, err)
		}
	}
}

// The checks that hold a collector's rules against each other compare a
// label's value as it is written. Two rules alike but for a value written
// with different placeholders are two rules, and load, though a probe may
// fill both alike; two that write the same placeholder are the same rule;
// and a static label stays one that cannot be required, whatever its
// placeholders fill to.
func TestRulesAreComparedByTheirLabelsAsWritten(t *testing.T) {
	rules := func(first, second string) string {
		return collectorRules("jq", "", "      - name: up\n        expression: .up\n        labels:\n          - {name: source, value: \""+first+"\"}\n      - name: up\n        expression: .up\n        labels:\n          - {name: source, value: \""+second+"\"}\n")
	}
	if got := problemsOf(t, rules("{{param_a}}", "{{param_b}}")); got != nil {
		t.Errorf("rules that differ as written: %q", got)
	}
	if got := problemsOf(t, rules("{{param_a}}", "{{param_a:x}}")); got != nil {
		t.Errorf("rules that differ in a default: %q", got)
	}
	if got := problemsOf(t, rules("{{param_a}}", "{{param_a}}")); len(got) != 1 || !strings.HasPrefix(got[0], `collector "node" metrics rule 1 and rule 2 are the same rule of metric "up"`) {
		t.Errorf("rules alike as written: %q", got)
	}
	pair := func(first, second string) string {
		return collectorRules("prometheus", "", "      - name: up\n        labels:\n          - {name: source, value: \""+first+"\"}\n      - expression: '^up'\n        labels:\n          - {name: source, value: \""+second+"\"}\n")
	}
	if got := problemsOf(t, pair("{{param_a}}", "{{param_b}}")); got != nil {
		t.Errorf("a name and a pattern whose labels differ as written: %q", got)
	}
	if got := problemsOf(t, pair("{{param_a}}", "{{param_a}}")); len(got) != 1 || !strings.HasPrefix(got[0], `collector "node" metrics rule 1 and rule 2 both pass on metric "up"`) {
		t.Errorf("a name and a pattern whose labels are alike as written: %q", got)
	}
	got := problemsOf(t, collectorRules("jq", "", "      - name: up\n        expression: .up\n        labels:\n          - {name: source, value: \"{{param_a:}}\", required: true}\n"))
	if len(got) != 1 || !strings.HasPrefix(got[0], `collector "node" metric "up" label "source" cannot be required: `) {
		t.Errorf("a required label with a placeholder for a value: %q", got)
	}
}

// A label whose value holds a placeholder can be neither required nor
// mapped by a value_map of its own, as no label with a value can, and is
// told so for a reason that is true of it. It was told that "its value is
// always there", which the value of a probe is not, and to "write the value
// it maps to instead", which the configuration cannot know. A constant is
// told what it was, byte for byte, as is a value whose braces open no
// placeholder; a python rule's label, which takes no value, is refused as
// one. What the second refusal sends the operator to loads: a value_map on
// the label of that name that another rule of the metric's name reads with
// an expression.
func TestALabelWithAPlaceholderIsToldWhyItIsNotRequiredOrMapped(t *testing.T) {
	label := func(transformType, rest string) []string {
		rule := "      - name: up\n        expression: .up\n"
		if transformType == "prometheus" || transformType == "python" {
			rule = "      - name: up\n"
		}
		return problemsOf(t, collectorRules(transformType, "", rule+"        labels:\n          - "+rest+"\n"))
	}
	const (
		required     = `collector "node" metric "up" label "l" cannot be required: its value has a {{param_...}} placeholder, so it comes from the probe, and required is for a label an expression reads from the response. A probe that leaves out a parameter whose placeholder has no default is answered 400, and a placeholder with an empty default, {{param_<name>:}}, leaves the label off because the configuration says so; remove required`
		mapped       = `collector "node" metric "up" label "l" sets value_map beside a value with a {{param_...}} placeholder, and a label with a value takes no value_map: have the probe give the parameter the value wanted, or, where another rule of the same metric name reads a label "l" with an expression, set the value_map on that label, which maps the label for the series of every rule of that name`
		staticOnce   = `collector "node" metric "up" label "l" has a static value, so it cannot be required; its value is always there`
		staticMapped = `collector "node" metric "up" label "l" sets value_map with a static value; write the value it maps to instead`
	)
	for _, tc := range []struct{ transform, label, want string }{
		{"jq", `{name: l, value: "{{param_a:}}", required: true}`, required},
		{"jq", `{name: l, value: "{{param_a}}", required: true}`, required},
		{"jq", `{name: l, value: "api-{{param_a:x}}", required: true}`, required},
		{"jq", `{name: l, value: "{{{param_a}}}", required: true}`, required},
		{"prometheus", `{name: l, value: "{{param_a:}}", required: true}`, required},
		{"jq", `{name: l, value: "{{param_a}}", value_map: {a: b}}`, mapped},
		{"jq", `{name: l, value: "api-{{param_a:x}}", value_map: {a: b, "*": c}}`, mapped},
		// A constant, and braces that open no placeholder.
		{"jq", `{name: l, value: x, required: true}`, staticOnce},
		{"jq", `{name: l, value: "{{x}} {param_a}", required: true}`, staticOnce},
		{"prometheus", `{name: l, value: x, required: true}`, staticOnce},
		{"python", `{name: l, value: x, required: true}`, staticOnce},
		{"jq", `{name: l, value: x, value_map: {a: b}}`, staticMapped},
		{"jq", `{name: l, value: "{{x}} {param_a}", value_map: {a: b}}`, staticMapped},
		// A python rule's label takes no value, and none from a probe.
		{"python", `{name: l, value: "{{param_a:}}", required: true}`, `collector "node" metric "up" label "l" cannot be required: a python transform's labels come from its script, not from label expressions`},
	} {
		if got := label(tc.transform, tc.label); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s, %s:\n got %q\nwant %q", tc.transform, tc.label, got, tc.want)
		}
	}
	// Neither setting, and the value loads; so does the value_map where the
	// refusal says it can stand.
	if got := label("jq", `{name: l, value: "{{param_a:}}"}`); got != nil {
		t.Errorf("a value with a placeholder: %q", got)
	}
	got := problemsOf(t, collectorRules("jq", "", "      - name: up\n        expression: .up\n        labels:\n          - {name: l, value: \"{{param_a}}\"}\n"+
		"      - name: up\n        expression: .other\n        labels:\n          - {name: l, expression: .l, value_map: {a: b}}\n"))
	if got != nil {
		t.Errorf("a value_map on the label another rule of the name reads: %q", got)
	}
}

// A static target fills the label placeholders of its collector under
// params, as it fills the request's: every placeholder must be filled, by
// params or a default, and every entry of params must fill one, or the file
// is refused naming the target, the collector and the parameter. The
// target's own labels are literal.
func TestAStaticTargetFillsItsCollectorsLabelPlaceholders(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: tenant_status
    request: {type: http, path: /status}
    transform:
      type: jq
      labels:
        tenant: "{{param_tenant}}"
        region: "{{param_region:eu}}"
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: source, value: "api-{{param_service}}"}
`))
	if err != nil {
		t.Fatal(err)
	}
	check := func(target string) error {
		t.Helper()
		file, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", "interval: 1m\ntargets:\n  - name: acme\n    collector: tenant_status\n    target: http://api.invalid\n"+target))
		if err != nil {
			t.Fatal(err)
		}
		return ValidateStaticTargetsAgainst(file, cfg)
	}
	if err := check("    labels: {own: \"{{param_tenant}}\"}\n    params: {param_tenant: acme, param_service: checkout}\n"); err != nil {
		t.Errorf("a target that fills every placeholder: %v", err)
	}
	if err := check("    params: {param_tenant: acme, param_service: checkout, param_region: us}\n"); err != nil {
		t.Errorf("a target that fills a default too: %v", err)
	}
	for name, tc := range map[string]struct{ target, want string }{
		"no params":           {"", `target "acme" uses collector "tenant_status", whose transform.labels.tenant needs param_tenant, a parameter without a default; a static target has no probe to supply it, so set it under the target's params, or give the placeholder a default`},
		"a rule's left out":   {"    params: {param_tenant: acme}\n", `target "acme" uses collector "tenant_status", whose metric "status_up" label "source" value needs param_service, a parameter without a default; a static target has no probe to supply it, so set it under the target's params, or give the placeholder a default`},
		"an empty value":      {"    params: {param_tenant: \"\", param_service: checkout}\n", `whose transform.labels.tenant needs param_tenant`},
		"an entry of nothing": {"    params: {param_tenant: acme, param_service: checkout, param_tenat: x}\n", `target "acme" params param_tenat are not used by collector "tenant_status": no placeholder in its request or its label values names them`},
	} {
		if err := check(tc.target); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v\nwant %s", name, err, tc.want)
		}
	}
}

// Reading the label values for placeholders changes the verdict on no
// collector that has none: of every collector of the shipped
// configurations, the examples, those under configs and the fixtures', as
// it is written, nothing is read and nothing refused, so its load goes on
// as it did; and each of those files that loads has no collector with a
// label placeholder kept.
func TestNoShippedCollectorHasALabelPlaceholder(t *testing.T) {
	var last *model.Collector
	collectors := 0
	files, _ := shippedRules(t, func(path string, x *model.Collector, _ model.MetricRule) {
		if x == last {
			return
		}
		last = x
		collectors++
		labels, err := fetch.ParseLabelParams(x, func(index int) string { return fmt.Sprintf("rule %d", index) })
		if labels != nil || err != nil {
			t.Errorf("%s, collector %q: %+v, %v", path, x.Name, labels, err)
		}
		if fetch.TemplatedRuleLabel(x, 0, 0) || len(fetch.TemplatedFields(x)) != len(fetch.TemplatedFields(withRules(x, nil))) {
			t.Errorf("%s, collector %q: a label value is named as filled", path, x.Name)
		}
	})
	if files < 10 || collectors < 20 {
		t.Fatalf("%d files and %d collectors were looked at", files, collectors)
	}
}
