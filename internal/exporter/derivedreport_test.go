package exporter

import (
	"bytes"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A debug probe's report marks a derived family as derived, with what it
// computes, under the name it is exported by, and lists the derived entries
// that gave no series apart from the rules; a hidden entry, which is not
// exported, is in neither.
func TestTheDebugReportMarksDerivedSeries(t *testing.T) {
	c := &model.Collector{
		Name: "memcached", MetricsPrefix: "mc", Transform: model.TransformConfig{Type: "regex"},
		Metrics: []model.MetricRule{{Name: "hits_total", Expression: "x"}, {Name: "misses_total", Expression: "y"}},
		Derived: []model.DerivedRule{
			{Name: "gets", Hidden: true, Sum: []string{"hits_total", "misses_total"}},
			{Name: "hit_ratio", Ratio: []string{"hits_total", "gets"}},
			{Name: "hits_by_pool", Sum: []string{"hits_total"}, By: &[]string{"pool"}},
		},
	}
	set := &model.MetricSet{Metrics: []model.Metric{{Name: "mc_hits_total"}, {Name: "mc_misses_total"}, {Name: "mc_hit_ratio"}}}
	var b bytes.Buffer
	writeTransform(&b, c, set, nil, "")
	report := b.String()
	for _, want := range []string{
		"    mc_hits_total: 1\n",
		"    mc_hit_ratio: 1 (derived: ratio of hits_total, gets)\n",
		"  Derived entries that gave no series: mc_hits_by_pool\n",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("no %q in\n%s", want, report)
		}
	}
	if strings.Contains(report, "gets:") || strings.Contains(report, "mc_gets") {
		t.Errorf("the hidden entry is in the report:\n%s", report)
	}
}
