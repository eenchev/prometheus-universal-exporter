package transform

import "testing"

// The escaping schemes a scraper asks for are Prometheus's model's: the
// vectors are those of its escaping tests. A classic name is never escaped,
// under dots either, which rewrites the underscores of the names it does
// escape; allow-utf-8 keeps every name. A label name keeps the classic
// label characters, without the colon a metric name may have.
func TestEscapeNameVectors(t *testing.T) {
	for _, c := range []struct {
		name, underscores, dots, values string
	}{
		{"no:escaping_required", "no:escaping_required", "no:escaping_required", "no:escaping_required"},
		{"mysystem.prod.west.cpu.load", "mysystem_prod_west_cpu_load", "mysystem_dot_prod_dot_west_dot_cpu_dot_load", "U__mysystem_2e_prod_2e_west_2e_cpu_2e_load"},
		{"mysystem.prod.west.cpu.load_total", "mysystem_prod_west_cpu_load_total", "mysystem_dot_prod_dot_west_dot_cpu_dot_load__total", "U__mysystem_2e_prod_2e_west_2e_cpu_2e_load__total"},
		{"http.status:sum", "http_status:sum", "http_dot_status:sum", "U__http_2e_status:sum"},
		{"label with 😱", "label_with__", "label__with____", "U__label_20_with_20__1f631_"},
		{"æ", "_", "__", "U___e6_"},
		{"1st", "_st", "__st", "U___31_st"},
	} {
		for scheme, want := range map[string]string{NameEscapingUnderscores: c.underscores, NameEscapingDots: c.dots, NameEscapingValues: c.values, NameEscapingAllowUTF8: c.name} {
			if got := EscapeName(c.name, scheme, true); got != want {
				t.Errorf("%s of %q: %q, want %q", scheme, c.name, got, want)
			}
		}
	}
	for scheme, want := range map[string]string{NameEscapingUnderscores: "service_name", NameEscapingDots: "service_dot_name", NameEscapingValues: "U__service_2e_name"} {
		if got := EscapeName("service.name", scheme, false); got != want {
			t.Errorf("%s of the label service.name: %q, want %q", scheme, got, want)
		}
	}
	if got := EscapeName("a:b", NameEscapingDots, false); got != "a__b" {
		t.Errorf("dots of the label a:b: %q", got)
	}
}
