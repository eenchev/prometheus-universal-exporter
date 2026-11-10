//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// paramsConfig has a collector whose expressions read the probe's
// parameters by name, one that reads them as a whole, and one whose
// expressions read none.
const paramsConfig = `collectors:
  - name: by_name
    request: {type: http, path: /json}
    transform: {type: jq}
    name_escaping: underscores
    metrics:
      - name: item_value
        items: '.items[] | select($params.id == null or .id == $params.id)'
        expression: .v
        labels:
          - {name: id, expression: .id}
          - {name: asked, expression: '$params["asked by"]'}
        labels_from: '{zone: $params.zone}'
  - name: as_a_whole
    request: {type: http, path: /json}
    transform: {type: yq}
    metrics:
      - name: params_given
        expression: $params | length
        labels:
          - {name: names, expression: '$params | keys | join(",")'}
  - name: reads_none
    request: {type: http, path: /json}
    transform: {type: jq}
    metrics:
      - name: up
        expression: .up
`

// A jq or yq expression reads the probe's parameters as $params, by name:
// one the probe did not give, or gave empty, is null. A parameter is taken
// by a collector one of whose expressions names it, and any parameter by
// one that reads $params otherwise; another is refused as one nothing
// uses, before the target is asked. Probes that differ in a parameter are
// answered each with its own series.
func TestAnExpressionReadsTheProbesParameters(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, paramsConfig)
	for name, tc := range map[string]struct {
		collector, query string
		want             []string
	}{
		"none given":           {"by_name", "", []string{`item_value{id="a"} 1`, `item_value{id="b"} 2`}},
		"one that selects":     {"by_name", "&param_id=b", []string{`item_value{id="b"} 2`}},
		"another value":        {"by_name", "&param_id=a", []string{`item_value{id="a"} 1`}},
		"an empty one":         {"by_name", "&param_id=", []string{`item_value{id="a"} 1`, `item_value{id="b"} 2`}},
		"in a label":           {"by_name", "&param_id=a&param_asked+by=me", []string{`item_value{asked="me",id="a"} 1`}},
		"in labels_from":       {"by_name", "&param_id=a&param_zone=eu", []string{`item_value{id="a",zone="eu"} 1`}},
		"read as a whole":      {"as_a_whole", "&param_x=1&param_y=2", []string{`params_given{names="x,y"} 2`}},
		"as a whole, none":     {"as_a_whole", "", []string{`params_given 0`}},
		"as a whole, an empty": {"as_a_whole", "&param_x=", []string{`params_given 0`}},
	} {
		got := labelProbe(t, server, target, tc.collector, tc.query, false)
		if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), tc.want) {
			t.Errorf("%s: %d\n%s\nwant %v", name, got.Code, got.Body.String(), tc.want)
		}
	}
	asked := target.requests.Load()
	for name, tc := range map[string]struct{ collector, query, want string }{
		"a name no expression reads":  {"by_name", "&param_idd=a", `probe parameters param_idd are not used by collector "by_name": no placeholder in its request.path ("/json"), body, header or query values or its label values names them, and no expression of its rules reads them as $params.<name>`},
		"a collector that reads none": {"reads_none", "&param_id=a", `probe parameters param_id are not used by collector "reads_none"`},
	} {
		got := labelProbe(t, server, target, tc.collector, tc.query, false)
		if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), tc.want) {
			t.Errorf("%s: %d %q, want 400 with %q", name, got.Code, got.Body.String(), tc.want)
		}
	}
	if n := target.requests.Load(); n != asked {
		t.Errorf("the target was asked %d times for probes that were refused", n-asked)
	}
}

// The label names labels_from reads of a response are held to the
// collector's name_escaping as every label name is: a Kubernetes label's
// name is escaped under underscores, and fails the scrape's validation
// where names are not escaped.
func TestTheNamesLabelsFromReadsAreEscapedAsEveryLabelNameIs(t *testing.T) {
	const collector = `collectors:
  - name: c
    request: {type: http, path: /json}
    transform: {type: jq}
ESCAPING    metrics:
      - name: item_value
        items: .items[]
        expression: .v
        labels_from: '{"app.kubernetes.io/name": .id}'
`
	target := newLabelTarget(t)
	server := labelServer(t, strings.Replace(collector, "ESCAPING", "    name_escaping: underscores\n", 1))
	got := labelProbe(t, server, target, "c", "", false)
	if want := []string{`item_value{app_kubernetes_io_name="a"} 1`, `item_value{app_kubernetes_io_name="b"} 2`}; got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), want) {
		t.Errorf("under underscores: %d\n%s\nwant %v", got.Code, got.Body.String(), want)
	}
	server = labelServer(t, strings.Replace(collector, "ESCAPING", "", 1))
	got = labelProbe(t, server, target, "c", "", false)
	if want := `metric "item_value" has label "app.kubernetes.io/name", which is not a classic Prometheus label name; set the collector's name_escaping to underscores or values to export it escaped`; got.Code != http.StatusBadGateway || !strings.Contains(got.Body.String(), want) {
		t.Errorf("names that are not escaped: %d %s, want 502 with %q", got.Code, got.Body.String(), want)
	}
}
