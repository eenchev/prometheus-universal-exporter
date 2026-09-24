package transform

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// UTF-8 metric and label names, and name_escaping (nameescaping.go).

func TestEscapeName(t *testing.T) {
	for _, tc := range []struct {
		name, scheme string
		label        bool
		want         string
	}{
		{"http_requests_total", NameEscapingUnderscores, false, "http_requests_total"},
		{"http_requests_total", NameEscapingValues, false, "http_requests_total"},
		{"job:rate5m", NameEscapingValues, false, "job:rate5m"},
		{"http.server.duration", NameEscapingUnderscores, false, "http_server_duration"},
		{"http.server.duration", NameEscapingValues, false, "U__http_2e_server_2e_duration"},
		{"a_b.c", NameEscapingValues, false, "U__a__b_2e_c"},
		{"1st", NameEscapingUnderscores, false, "_st"},
		{"1st", NameEscapingValues, false, "U___31_st"},
		{"température", NameEscapingUnderscores, false, "temp_rature"},
		{"température", NameEscapingValues, false, "U__temp_e9_rature"},
		{"cpu😀", NameEscapingValues, false, "U__cpu_1f600_"},
		{"bad\xffname", NameEscapingValues, false, "U__bad_FFFD_name"},
		// Colons are classic in metric names, not in label names.
		{"a:b", NameEscapingUnderscores, true, "a_b"},
		{"a:b", NameEscapingValues, true, "U__a_3a_b"},
		{"service.name", NameEscapingUnderscores, true, "service_name"},
		{"service.name", NameEscapingFail, true, "service.name"},
	} {
		if got := escapeName(tc.name, tc.scheme, !tc.label); got != tc.want {
			t.Errorf("escapeName(%q, %s, label=%v) = %q, want %q", tc.name, tc.scheme, tc.label, got, tc.want)
		}
	}
}

// A label map shared among a transform's metrics is not changed in place.
func TestEscapeNamesCopiesSharedLabels(t *testing.T) {
	shared := map[string]string{"service.name": "api"}
	set := &model.MetricSet{Metrics: []model.Metric{{Name: "a.b", Labels: shared}, {Name: "c", Labels: shared}}}
	if err := escapeNames(set, NameEscapingUnderscores); err != nil {
		t.Fatal(err)
	}
	if set.Metrics[0].Name != "a_b" || set.Metrics[0].Labels["service_name"] != "api" || set.Metrics[1].Labels["service_name"] != "api" {
		t.Fatalf("%+v", set.Metrics)
	}
	if _, ok := shared["service.name"]; !ok || len(shared) != 1 {
		t.Fatalf("the shared map was changed: %v", shared)
	}
}
