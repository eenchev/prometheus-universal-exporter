package model

import (
	"strings"
	"testing"
)

// Of two labels of a series that cannot be exposed, the one named is the
// first by name on every scrape. The labels are a map, gone through in no
// order, and the failure named whichever came first: `zone` on one scrape
// and `note` on the next, which the log took for two failures taking turns
// and logged in full each time.
func TestTheLabelALimitFailureNamesIsTheSameOnEveryScrape(t *testing.T) {
	long := strings.Repeat("x", 30)
	for name, tc := range map[string]struct {
		labels map[string]string
		want   string
	}{
		"two values over the limit": {map[string]string{"zone": long, "note": long, "host": "a", "area": "b"},
			`metric "m" label "note" value is 30 bytes`},
		"two names that are no label names": {map[string]string{"z-one": "1", "n-ote": "2", "host": "a"},
			`metric "m" has label "n-ote"`},
		"a name and a value": {map[string]string{"zone": long, "n-ote": "2"},
			`metric "m" has label "n-ote"`},
	} {
		t.Run(name, func(t *testing.T) {
			for range 200 {
				set := MetricSet{Metrics: []Metric{{Name: "m", Type: GaugeMetricType, Labels: tc.labels}}}
				err := set.Validate(Limits{MaxLabelValueLength: 20})
				if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
					t.Fatalf("the failure is %v, want it to start with %s", err, tc.want)
				}
			}
		})
	}
}
