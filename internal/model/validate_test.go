package model

import (
	"fmt"
	"regexp"
	"testing"
)

// The exposition rules' patterns for classic names, which ValidMetricName and
// ValidLabelName implement without a regular expression.
var (
	metricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNamePattern  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

func checkNameValidators(t *testing.T, name string) {
	t.Helper()
	if got, want := ValidMetricName(name), metricNamePattern.MatchString(name); got != want {
		t.Errorf("ValidMetricName(%q) = %v, the pattern says %v", name, got, want)
	}
	if got, want := ValidLabelName(name), labelNamePattern.MatchString(name); got != want {
		t.Errorf("ValidLabelName(%q) = %v, the pattern says %v", name, got, want)
	}
}

func TestNameValidatorsMatchThePatterns(t *testing.T) {
	for _, name := range []string{
		"", "a", "A", "_", ":", "0", "a0", "0a", "a:b", ":a", "_:", "a_b_c", "__name__",
		"a-b", "a.b", "a b", "a\n", "a\x00", "é", "aé", "a\xff", "ab\n", "\na",
		"http_requests_total", "job:rate5m:sum", "Z9_", "9", "a/b", "[", "`", "{", "@",
	} {
		checkNameValidators(t, name)
	}
	// Every single byte, alone and after a valid start.
	for c := 0; c < 256; c++ {
		checkNameValidators(t, string([]byte{byte(c)}))
		checkNameValidators(t, "a"+string([]byte{byte(c)}))
	}
}

func FuzzNameValidatorsMatchThePatterns(f *testing.F) {
	for _, seed := range []string{"", "a", "a:b", "0a", "a-b", "é", "a\n"} {
		f.Add(seed)
	}
	f.Fuzz(checkNameValidators)
}

// validateSet is series as a csv or prometheus probe makes them: one family,
// three labels each.
func validateSet(n int) *MetricSet {
	s := &MetricSet{}
	for i := 0; i < n; i++ {
		s.Metrics = append(s.Metrics, Metric{Name: "component_cpu_seconds", Type: GaugeMetricType, Value: 1, Labels: map[string]string{
			"id": fmt.Sprint("c", i), "name": fmt.Sprint("component ", i), "status": "operational",
		}})
	}
	return s
}

func BenchmarkMetricSetValidate(b *testing.B) {
	s := validateSet(2000)
	l := Limits{MaxMetrics: 100000, MaxLabelsPerMetric: 30, MaxLabelValueLength: 1000, MaxMetricNameLength: 200}
	b.ReportAllocs()
	for b.Loop() {
		if err := s.Validate(l); err != nil {
			b.Fatal(err)
		}
	}
}

// Series are told apart by their keys, not their hashes: with every hash the
// same, different series are still different and the same series still a
// duplicate, an empty label counting as none.
func TestSeriesSetSurvivesHashCollisions(t *testing.T) {
	metrics := []Metric{
		{Name: "a", Labels: map[string]string{"x": "1"}},
		{Name: "a", Labels: map[string]string{"x": "2"}},
		{Name: "b"},
		{Name: "a", Labels: map[string]string{"x": "2"}},
		{Name: "b", Labels: map[string]string{"y": ""}},
		{Name: "a", Labels: map[string]string{"x": "1"}},
		{Name: "a", Labels: map[string]string{"x": "3"}},
	}
	for _, collide := range []bool{false, true} {
		set := newSeriesSet(metrics)
		if collide {
			set.hash = func([]byte) uint64 { return 7 }
		}
		for i, want := range []struct{ duplicate, empty bool }{
			{false, false}, {false, false}, {false, false}, {true, false}, {true, true}, {true, false}, {false, false},
		} {
			if duplicate, empty := set.add(i); duplicate != want.duplicate || empty != want.empty {
				t.Errorf("collide=%v series %d: duplicate=%v empty=%v, want %v %v", collide, i, duplicate, empty, want.duplicate, want.empty)
			}
		}
	}
}
