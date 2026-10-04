package transform

import (
	"strings"
	"testing"
)

// Of several labels of a script's series whose values are no text, the
// failure names the first by name on every scrape. The labels are a map,
// gone through in no order, and the failure named whichever came first: `a`
// on one scrape and `c` on the next, which the log took for failures taking
// turns and logged in full each time. A label that is None is left out, and
// is no failure to name.
func TestTheLabelAScriptsFailureNamesIsTheSameOnEveryScrape(t *testing.T) {
	labels := func() map[string]any {
		return map[string]any{"d": []any{7.0, 8.0}, "b": []any{3.0, 4.0}, "a": nil, "c": []any{5.0, 6.0}, "e": "text"}
	}
	for range 200 {
		_, err := pythonMetric{Name: "up", Value: 1.0, Labels: labels()}.metric()
		if err == nil || !strings.HasPrefix(err.Error(), `metric "up" label "b" `) {
			t.Fatalf("a script's metric fails with %v, want its label b named", err)
		}
		_, err = prometheusSeries(map[string]any{"name": "up", "value": 1.0, "labels": labels()})
		if err == nil || !strings.Contains(err.Error(), `label "b" `) {
			t.Fatalf("a pre-script's series fails with %v, want its label b named", err)
		}
	}
}
