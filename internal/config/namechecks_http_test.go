//go:build !select_request_types || request_type_http

package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// loadNameCheckCollector loads a configuration of the one collector written
// in flow style.
func loadNameCheckCollector(t *testing.T, collector string) (*model.Config, error) {
	t.Helper()
	return Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", "collectors:\n  - "+collector+"\n"))
}

// A label name a rule or transform.labels writes that transform.rename_labels
// renames or transform.remove_labels removes loads whatever its characters:
// one Prometheus reserves, beginning with "__", one with a dot or a letter
// outside ASCII under name_escaping fail, and one underscores would escape to
// a reserved name. The scrape escapes and validates the series after the
// renames and removals, so such a name is never exported, and the load
// refused a configuration whose every scrape passed. A key of
// transform.labels that remove_labels removes, which adds nothing, loads as
// well. The same names where nothing takes them off, a rename of another
// label or a rename to them, are refused in the words they were, and a name
// of blanks alone is refused taken off or not.
func TestALabelNameARenameOrRemovalTakesOffLoadsWhateverItIs(t *testing.T) {
	const pre = `{name: c, request: {type: http}, decoder: {type: prometheus}, `
	rule := func(label string) string {
		return `metrics: [{name: n, expression: '^m$', labels: [{name: ` + label + `, value: v}]}]}`
	}
	for _, collector := range []string{
		pre + `transform: {type: prometheus, rename_labels: {__tmp: tenant}}, ` + rule("__tmp"),
		pre + `transform: {type: prometheus, remove_labels: [__tmp]}, ` + rule("__tmp"),
		pre + `transform: {type: prometheus, rename_labels: {a.b: ab}}, ` + rule("a.b"),
		pre + `name_escaping: fail, transform: {type: prometheus, remove_labels: [a.b]}, ` + rule("a.b"),
		pre + `transform: {type: prometheus, rename_labels: {é: e}}, ` + rule("é"),
		pre + `name_escaping: underscores, transform: {type: prometheus, rename_labels: {..site: site}}, ` + rule("..site"),
		pre + `transform: {type: prometheus, labels: {a.b: v}, remove_labels: [a.b]}}`,
		pre + `transform: {type: prometheus, labels: {__k: v}, rename_labels: {__k: k}}}`,
		`{name: c, request: {type: http}, transform: {type: jq, rename_labels: {__tmp: tenant}}, metrics: [{name: n, expression: .v, labels: [{name: __tmp, expression: .t}]}]}`,
	} {
		if _, err := loadNameCheckCollector(t, collector); err != nil {
			t.Errorf("%s: refused with %v", collector, err)
		}
	}
	for collector, want := range map[string]string{
		pre + `transform: {type: prometheus, rename_labels: {other: tenant}}, ` + rule("__tmp"):   `collector "c" metric "n": label name "__tmp" starts with __, which Prometheus reserves for its own labels`,
		pre + `transform: {type: prometheus, rename_labels: {tenant: __tmp}}, ` + rule("__tmp"):   `collector "c" metric "n": label name "__tmp" starts with __, which Prometheus reserves for its own labels`,
		pre + `transform: {type: prometheus, remove_labels: [other]}, ` + rule("a.b"):             `collector "c" metric "n" has invalid label name "a.b"; set the collector's name_escaping to underscores or values to export it escaped`,
		pre + `name_escaping: underscores, transform: {type: prometheus}, ` + rule("..site"):      `label name "..site" is exported as "__site" under name_escaping underscores, which starts with __`,
		pre + `transform: {type: prometheus, labels: {__k: v}, rename_labels: {k: __k}}}`:         `collector "c" transform.labels: label name "__k" starts with __, which Prometheus reserves for its own labels`,
		pre + `transform: {type: prometheus, rename_labels: {" ": tenant}}, ` + rule(`" "`):       `collector "c" metric "n" has a label without a name`,
		pre + `transform: {type: prometheus, labels: {" ": v}, remove_labels: [" "]}}`:            `collector "c" transform.labels has invalid label name " "`,
		pre + `transform: {type: prometheus, labels: {a.b: v}, rename_labels: {a.b: c.d}}}`:       `collector "c" transform.rename_labels "a.b" to invalid label name "c.d"`,
		pre + `transform: {type: prometheus, rename_labels: {__tmp: __tenant}}, ` + rule("__tmp"): `collector "c" transform.rename_labels "__tmp": label name "__tenant" starts with __`,
	} {
		if _, err := loadNameCheckCollector(t, collector); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", collector, err, want)
		}
	}
}

// A rule's literal name that the collector exports longer than
// limits.max_metric_name_length, 200 bytes by default, is refused when the
// configuration loads, naming the collector, the rule, the length and the
// limit, where it loaded and failed every scrape at validation: the name as
// name_escaping exports it, under every transform, a prometheus rule's and a
// python rule's among them. A name at the limit loads, one over the default
// loads where the limit is raised, and so does a histogram whose family name
// is at the limit, as the scrape measures the family's name and not its
// samples' _bucket. With metrics_prefix the prefixed name is refused in the
// words it was. A prometheus rule without a name has no literal name to
// measure.
func TestARuleNameOverTheMetricNameLimitIsRefusedAtLoad(t *testing.T) {
	over, at := strings.Repeat("m", 201), strings.Repeat("m", 200)
	jq := func(name, more string) string {
		return `{name: c, request: {type: http}, transform: {type: jq}` + more + `, metrics: [{name: "` + name + `", expression: .v}]}`
	}
	const advice = "so every series of that name would fail validation; shorten the name or raise limits.max_metric_name_length"
	dotted := "a." + strings.Repeat("b", 195)
	escaped := "U__a_2e_" + strings.Repeat("b", 195)
	for collector, want := range map[string]string{
		jq(over, ""): fmt.Sprintf(`collector "c" metric %q is 201 bytes, longer than limits.max_metric_name_length 200, %s`, over, advice),
		jq(over, ", limits: {max_metric_name_length: 0}"):   fmt.Sprintf(`collector "c" metric %q is 201 bytes, longer than limits.max_metric_name_length 200`, over),
		jq("abcd", ", limits: {max_metric_name_length: 3}"): `collector "c" metric "abcd" is 4 bytes, longer than limits.max_metric_name_length 3, ` + advice,
		jq(dotted, ", name_escaping: values"):               fmt.Sprintf(`collector "c" metric %q is exported as %q under name_escaping values, %d bytes, longer than limits.max_metric_name_length 200, %s`, dotted, escaped, len(escaped), advice),
		jq(at, ", metrics_prefix: p"):                       fmt.Sprintf(`collector "c" metric %q is exported as %q, which is longer than limits.max_metric_name_length 200`, at, "p_"+at),
		`{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}, metrics: [{name: ` + over + `, expression: '^h$'}]}`: fmt.Sprintf(`collector "c" metric %q is 201 bytes`, over),
		`{name: c, request: {type: http}, decoder: {type: json}, transform: {type: python, script: "metric('x', 1)"}, metrics: [{name: ` + over + `}]}`:    fmt.Sprintf(`collector "c" metric %q is 201 bytes`, over),
	} {
		if _, err := loadNameCheckCollector(t, collector); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.120s: error %v, want %q", collector, err, want)
		}
	}
	for _, collector := range []string{
		jq(at, ""),
		jq(over, ", limits: {max_metric_name_length: 201}"),
		jq(dotted, ", name_escaping: underscores"),
		jq(strings.Repeat("m", 198), ", metrics_prefix: p"),
		`{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}, metrics: [{name: ` + at + `, expression: '^h$', type: histogram}]}`,
		`{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}, metrics: [{expression: '^` + over + `$'}]}`,
	} {
		if _, err := loadNameCheckCollector(t, collector); err != nil {
			t.Errorf("%.120s: refused with %v", collector, err)
		}
	}
}
