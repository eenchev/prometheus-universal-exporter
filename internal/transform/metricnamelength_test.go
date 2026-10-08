package transform

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A rule's name is held at load to limits.max_metric_name_length as it is
// exported, and to nothing more: over classic, dotted, reserved, blank and
// long names, a rule with a bad expression beside them, under every
// name_escaping, with and without a metrics_prefix, under a jq, a prometheus
// and a python transform, at limits from 1 to past the exported length,
// CheckMetricRule says what it says with the limit left 0 — the check as it
// was, kept as the oracle — except of a name it otherwise takes, without a
// prefix (ValidateMetricsPrefix measures the prefixed name), exported past
// the limit: then the length is refused first, naming the rule, the length,
// the exported name where escaping changed it, and the limit, and every
// other problem after it as before.
func TestARuleNameIsHeldToTheMetricNameLimitAndNothingMore(t *testing.T) {
	names := []string{"up", "a.b", "__up", "1up", " ", "é", "a:b", strings.Repeat("m", 199), strings.Repeat("m", 200), strings.Repeat("m", 201), "a." + strings.Repeat("b", 197), strings.Repeat("é", 101)}
	changed := 0
	for _, kind := range []string{"jq", "prometheus", "python"} {
		for _, escaping := range []string{"", NameEscapingFail, NameEscapingUnderscores, NameEscapingValues} {
			for _, prefix := range []string{"", "p"} {
				for _, name := range names {
					for _, expression := range []string{"", "(", ".v"} {
						exported := ExportedMetricName(&model.Collector{NameEscaping: escaping}, name)
						for _, limit := range []int{1, len(exported) - 1, len(exported), len(exported) + 1, 200} {
							x := &model.Collector{Name: "c", NameEscaping: escaping, MetricsPrefix: prefix, Transform: model.TransformConfig{Type: kind}}
							r := &model.MetricRule{Name: name, Expression: expression}
							was := CheckMetricRule(x, r)
							x.Limits.MaxMetricNameLength = limit
							got := CheckMetricRule(x, r)
							if prefix != "" || checkMetricName(x, name) != nil || limit <= 0 || len(exported) <= limit {
								if fmt.Sprint(got) != fmt.Sprint(was) {
									t.Errorf("%q under %s %q prefix %q at %d: %v, was %v", name, kind, escaping, prefix, limit, got, was)
								}
								continue
							}
							changed++
							want := fmt.Sprintf("collector \"c\" metric %q is %d bytes, longer than limits.max_metric_name_length %d, so every series", name, len(name), limit)
							if exported != name {
								want = fmt.Sprintf("collector \"c\" metric %q is exported as %q under name_escaping %s, %d bytes, longer than limits.max_metric_name_length %d, so every series", name, exported, escaping, len(exported), limit)
							}
							if got == nil || !strings.HasPrefix(got.Error(), want) || was != nil && !strings.Contains(got.Error(), was.Error()) {
								t.Errorf("%q under %s %q at %d: %v, want %q and then %v", name, kind, escaping, limit, got, want, was)
							}
						}
					}
				}
			}
		}
	}
	if changed < 100 {
		t.Errorf("%d verdicts changed", changed)
	}
}
