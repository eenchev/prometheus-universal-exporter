//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"strings"
	"testing"
)

// A label name a rule or transform.labels writes that the load takes because
// transform.rename_labels renames it or remove_labels removes it — one
// beginning with "__", one with a dot under name_escaping fail, one with a
// letter outside ASCII — is served: the scrape renames or removes it before
// it escapes and validates the series, so it passes, and the series carries
// the rename's target or nothing. Such a configuration was refused at load.
// A rule named at limits.max_metric_name_length is served as well, and so is
// a histogram whose family name is at the limit and its _bucket samples' past
// it: the scrape measures the family's name, as the load does.
func TestANameTheLoadTakesAtTheEdgeOfTheNameChecksIsServed(t *testing.T) {
	address, body := labelNameTarget(t)
	at := strings.Repeat("h", 200)
	const pre = `{name: c, request: {type: http}, decoder: {type: prometheus}, `
	rule := func(label string) string {
		return `metrics: [{name: n, expression: '^m$', labels: [{name: ` + label + `, value: v}]}]}`
	}
	for name, test := range map[string]struct{ collector, body, want string }{
		"a reserved rule label renamed": {pre + `transform: {type: prometheus, rename_labels: {__tmp: tenant}}, ` + rule("__tmp"),
			"m 1\n", `n{tenant="v"} 1`},
		"a dotted rule label removed under fail": {pre + `name_escaping: fail, transform: {type: prometheus, remove_labels: [a.b]}, ` + rule("a.b"),
			"m 1\n", "n 1"},
		"a UTF-8 rule label renamed": {pre + `transform: {type: prometheus, rename_labels: {é: e}}, ` + rule("é"),
			"m 1\n", `n{e="v"} 1`},
		"a reserved transform.labels key renamed": {pre + `transform: {type: prometheus, labels: {__k: v}, rename_labels: {__k: k}}}`,
			"m 1\n", `m{k="v"} 1`},
		"a rule named at the limit": {pre + `transform: {type: prometheus}, metrics: [{name: ` + at + `, expression: '^m$'}]}`,
			"m 1\n", at + " 1"},
		"a histogram named at the limit": {pre + `transform: {type: prometheus}, metrics: [{name: ` + at + `, expression: '^h$'}]}`,
			"# TYPE h histogram\nh_bucket{le=\"+Inf\"} 2\nh_sum 3\nh_count 2\n", at + `_bucket{le="+Inf"} 2`},
	} {
		t.Run(name, func(t *testing.T) {
			server, _ := errorLengthServer(t, test.collector)
			body.Store(&test.body)
			got := probeOnce(t, server, probePath("c", address, ""), nil)
			if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), test.want+"\n") {
				t.Fatalf("answered %d %.500q; want %q", got.Code, got.Body, test.want)
			}
		})
	}
}
