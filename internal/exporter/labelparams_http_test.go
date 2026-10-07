//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// labelTarget answers a path with the document its name says, /api/... with
// the JSON one, counts the requests it gets and notes their paths; failing
// makes it answer 503.
type labelTarget struct {
	*httptest.Server
	requests atomic.Int64
	failing  atomic.Bool
	mu       sync.Mutex
	paths    []string
}

// labelBodies are the documents a labelTarget answers with, by path: their
// Content-Type and their text.
var labelBodies = map[string][2]string{
	"/json": {"application/json", `{"up": 1, "src": "other", "items": [{"id": "a", "v": 1}, {"id": "b", "v": 2}]}`},
	"/yaml": {"application/yaml", "up: 1\n"},
	"/text": {"text/plain", "value=1\n"},
	"/csv":  {"text/csv", "host,cpu\nweb01,42\n"},
	"/xml":  {"application/xml", `<status><host name="web01"><cpu>42</cpu></host></status>`},
	"/html": {"text/html", `<html><body><table><tr class="row"><td class="host">web01</td><td class="cpu">42</td></tr></table></body></html>`},
	"/prom": {"text/plain; version=0.0.4", "# TYPE up gauge\nup{job=\"api\",tenant=\"own\"} 1\n"},
}

func newLabelTarget(t *testing.T) *labelTarget {
	t.Helper()
	target := &labelTarget{}
	target.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.requests.Add(1)
		target.mu.Lock()
		target.paths = append(target.paths, r.URL.Path)
		target.mu.Unlock()
		if target.failing.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/") {
			path = "/json"
		}
		body, ok := labelBodies[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", body[0])
		_, _ = w.Write([]byte(body[1]))
	}))
	t.Cleanup(target.Close)
	return target
}

func (l *labelTarget) lastPath() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.paths) == 0 {
		return ""
	}
	return l.paths[len(l.paths)-1]
}

// labelProbe probes collector at target with the rest of a query, asking
// for the text format, or for OpenMetrics.
func labelProbe(t *testing.T, server *Server, target *labelTarget, collector, query string, openMetrics bool) *httptest.ResponseRecorder {
	t.Helper()
	var header http.Header
	if openMetrics {
		header = http.Header{"Accept": {prometheus3Accept}}
	}
	return probeOnce(t, server, probePath(collector, target.URL, query), header)
}

// tenantStatusConfig is the collector the documentation shows: one
// parameter fills the request's path and two label values, and another,
// with a default, a third.
const tenantStatusConfig = `collectors:
  - name: tenant_status
    request: {type: http, path: "/api/{{param_tenant}}/status"}
    transform:
      type: jq
      labels:
        tenant: "{{param_tenant}}"
        region: "{{param_region:eu}}"
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: source, value: "api-{{param_tenant}}"}
  - name: label_only
    request: {type: http, path: /json}
    transform:
      type: jq
      labels:
        tenant: "{{param_tenant}}"
    metrics:
      - name: status_up
        expression: .up
  - name: rule_only
    request: {type: http, path: /json}
    transform: {type: jq}
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: source, value: "api-{{param_source}}"}
  - name: plain
    request: {type: http, path: /json}
    transform:
      type: jq
      labels: {site: dc1}
    metrics:
      - name: status_up
        expression: .up
`

// A probe's parameter fills the placeholders of a collector's label values
// as it fills the request's: the values of transform.labels and a rule's
// static value, the same parameter in the path and in the labels, a default
// where the probe gives none. The text format and OpenMetrics carry the
// same series.
func TestAProbeParameterFillsALabelValue(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, tenantStatusConfig)
	for _, openMetrics := range []bool{false, true} {
		got := labelProbe(t, server, target, "tenant_status", "&param_tenant=acme", openMetrics)
		if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{`status_up{region="eu",source="api-acme",tenant="acme"} 1`}) {
			t.Fatalf("OpenMetrics %v: %d\n%s", openMetrics, got.Code, got.Body.String())
		}
		if openMetrics != strings.HasSuffix(got.Body.String(), "# EOF\n") {
			t.Errorf("OpenMetrics %v: the answer ends %q", openMetrics, got.Body.String())
		}
		if path := target.lastPath(); path != "/api/acme/status" {
			t.Errorf("the request went to %s", path)
		}
		got = labelProbe(t, server, target, "tenant_status", "&param_tenant=globex&param_region=us", openMetrics)
		if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{`status_up{region="us",source="api-globex",tenant="globex"} 1`}) {
			t.Fatalf("OpenMetrics %v, another tenant: %d\n%s", openMetrics, got.Code, got.Body.String())
		}
		// An empty value is no value: the default stands.
		got = labelProbe(t, server, target, "tenant_status", "&param_tenant=acme&param_region=", openMetrics)
		if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{`status_up{region="eu",source="api-acme",tenant="acme"} 1`}) {
			t.Fatalf("OpenMetrics %v, an empty region: %d\n%s", openMetrics, got.Code, got.Body.String())
		}
	}
	// The verbose self-metrics and the collector's own carry no value of a
	// parameter.
	if metrics := selfMetrics(t, server); strings.Contains(metrics, "acme") || strings.Contains(metrics, "globex") {
		t.Errorf("the self-metrics carry a parameter's value:\n%s", metrics)
	}
}

// A value is written as it is given, with nothing of it escaped or
// refused: the exposition writers escape what a label value may not hold
// bare, a quote, a backslash and a line break, in both formats, and braces
// and text that is not ASCII are a value's own.
func TestAFilledLabelValueIsWrittenAsItIsGiven(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, tenantStatusConfig)
	value := "a\"b\\c\nd é {{param_region}} {x}"
	const escaped = `a\"b\\c\nd é {{param_region}} {x}`
	for _, openMetrics := range []bool{false, true} {
		got := labelProbe(t, server, target, "label_only", "&param_tenant="+url.QueryEscape(value), openMetrics)
		if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{`status_up{tenant="` + escaped + `"} 1`}) {
			t.Errorf("OpenMetrics %v, transform.labels: %d\n%s", openMetrics, got.Code, got.Body.String())
		}
		got = labelProbe(t, server, target, "rule_only", "&param_source="+url.QueryEscape(value), openMetrics)
		if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{`status_up{source="api-` + escaped + `"} 1`}) {
			t.Errorf("OpenMetrics %v, a rule's label: %d\n%s", openMetrics, got.Code, got.Body.String())
		}
	}
}

// The probe's rules are the request's. A label's placeholder with no value
// and no default answers 400 naming the parameter before the target is
// contacted, and so does one given an empty value; a parameter that only a
// label uses is accepted, with a path that replaces the request's too; one
// no place uses, one given twice and one that is not valid UTF-8 are
// refused.
func TestALabelParameterIsHeldToTheRulesOfARequestParameter(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, tenantStatusConfig)
	for name, tc := range map[string]struct{ collector, query, want string }{
		"no value for transform.labels":   {"label_only", "", "transform.labels.tenant needs param_tenant, which the probe did not supply and which has no default; add &param_tenant=<value> to the probe, or give it a default as {{param_tenant:<default>}}"},
		"an empty value":                  {"label_only", "&param_tenant=", "transform.labels.tenant needs param_tenant"},
		"no value for a rule's label":     {"rule_only", "", `metric "status_up" label "source" value needs param_source, which the probe did not supply and which has no default`},
		"the request's placeholder first": {"tenant_status", "", "request.path needs param_tenant"},
		"a path beside a missing value":   {"tenant_status", "&path=/json", "transform.labels.tenant needs param_tenant"},
		"a parameter nothing uses":        {"label_only", "&param_tenant=acme&param_tenat=acme", `probe parameters param_tenat are not used by collector "label_only": no placeholder in its request.path ("/json"), body, header or query values or its label values names them`},
		"a collector without any":         {"plain", "&param_tenant=acme", `probe parameters param_tenant are not used by collector "plain"`},
		"what the replaced path used":     {"tenant_status", "&path=/json&param_tenant=acme&param_version=2", "probe parameters param_version are not used: the path probe parameter replaces request.path, and nothing else in the request and no label value of the collector names them"},
		"a parameter given twice":         {"label_only", "&param_tenant=acme&param_tenant=globex", "probe parameter param_tenant is given 2 times; give it once"},
		"a value that is not UTF-8":       {"label_only", "&param_tenant=a%FFb", "transform.labels.tenant: the value of param_tenant is not valid UTF-8, which a label value must be"},
		"a rule's value not UTF-8":        {"rule_only", "&param_source=%C3", `metric "status_up" label "source" value: the value of param_source is not valid UTF-8, which a label value must be`},
	} {
		got := labelProbe(t, server, target, tc.collector, tc.query, false)
		if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), tc.want) {
			t.Errorf("%s: %d %q, want 400 with %q", name, got.Code, got.Body.String(), tc.want)
		}
	}
	if n := target.requests.Load(); n != 0 {
		t.Fatalf("the target was contacted %d times for probes that were refused", n)
	}
	for name, tc := range map[string]struct{ collector, query, want string }{
		"a parameter only a label uses":      {"label_only", "&param_tenant=acme", `status_up{tenant="acme"} 1`},
		"a rule's label alone":               {"rule_only", "&param_source=db", `status_up{source="api-db"} 1`},
		"a path that replaces the request's": {"tenant_status", "&path=/json&param_tenant=acme", `status_up{region="eu",source="api-acme",tenant="acme"} 1`},
		"a path beside a label's parameter":  {"label_only", "&path=/api/x&param_tenant=acme", `status_up{tenant="acme"} 1`},
	} {
		got := labelProbe(t, server, target, tc.collector, tc.query, false)
		if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{tc.want}) {
			t.Errorf("%s: %d\n%s", name, got.Code, got.Body.String())
		}
	}
	if n := target.requests.Load(); n != 4 {
		t.Fatalf("the target was contacted %d times for 4 probes", n)
	}
}

// Text and several placeholders mix in one value, and braces that open no
// placeholder are the value's own text: a `{{` that param_ does not follow,
// a closing pair alone.
func TestALabelValueMixesTextAndPlaceholders(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, `collectors:
  - name: mixed
    request: {type: http, path: /json}
    transform:
      type: jq
      labels:
        where: "{{param_dc}}/{{param_rack:r1}}/{{param_dc}}"
        braces: "{{x}} {{ y }} }} {{param_dc}} {param_dc} {{"
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: source, value: "{{not_a_param}}-{{param_dc}}{{param_rack:r1}}-end"}
`)
	got := labelProbe(t, server, target, "mixed", "&param_dc=fra", false)
	if want := `status_up{braces="{{x}} {{ y }} }} fra {param_dc} {{",source="{{not_a_param}}-frar1-end",where="fra/r1/fra"} 1`; got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{want}) {
		t.Fatalf("%d\n%s", got.Code, got.Body.String())
	}
	got = labelProbe(t, server, target, "mixed", "&param_dc=ams&param_rack=r9", false)
	if want := `status_up{braces="{{x}} {{ y }} }} ams {param_dc} {{",source="{{not_a_param}}-amsr9-end",where="ams/r9/ams"} 1`; got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{want}) {
		t.Fatalf("%d\n%s", got.Code, got.Body.String())
	}
}

// labelCase is a collector of one transform, written with the tokens
// TENANT and REGION where its label values go.
type labelCase struct {
	name, path, collector, transform, settings, metrics string
	// want are the series a probe of acme gets.
	want []string
}

// document writes the case's collector with tenant and region in its label
// values: placeholders, or the text a probe fills them with.
func (tc labelCase) document(tenant, region string) string {
	document := "collectors:\n  - name: c\n    request: {type: http, path: " + tc.path + "}\n" + tc.collector +
		"    transform:\n      type: " + tc.transform + "\n      labels:\n        tenant: \"TENANT\"\n        region: \"REGION\"\n" + tc.settings + tc.metrics
	return strings.NewReplacer("TENANT", tenant, "REGION", region).Replace(document)
}

const labelRuleSource = "        labels:\n          - {name: source, value: \"api-TENANT\"}\n"

// labelCases are a collector of each transform whose rules give a label a
// static value, which is every one but python, and of a prometheus
// pass-through, which has no rules; then a jq collector under each setting
// that reads a label's value after the transform made it.
var labelCases = []labelCase{
	{name: "jq", path: "/json", transform: "jq",
		metrics: "    metrics:\n      - name: status_up\n        expression: .up\n" + labelRuleSource,
		want:    []string{`status_up{region="eu",source="api-acme",tenant="acme"} 1`}},
	{name: "jq items", path: "/json", transform: "jq",
		metrics: "    metrics:\n      - name: item_value\n        items: .items[]\n        expression: .v\n        labels:\n          - {name: id, expression: .id}\n          - {name: source, value: \"api-TENANT\"}\n",
		want:    []string{`item_value{id="a",region="eu",source="api-acme",tenant="acme"} 1`, `item_value{id="b",region="eu",source="api-acme",tenant="acme"} 2`}},
	{name: "yq", path: "/yaml", transform: "yq",
		metrics: "    metrics:\n      - name: status_up\n        expression: .up\n" + labelRuleSource,
		want:    []string{`status_up{region="eu",source="api-acme",tenant="acme"} 1`}},
	{name: "regex", path: "/text", transform: "regex",
		metrics: "    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n" + labelRuleSource,
		want:    []string{`demo_value{region="eu",source="api-acme",tenant="acme"} 1`}},
	{name: "css", path: "/html", transform: "css",
		metrics: "    metrics:\n      - name: cpu\n        expression: td.cpu\n" + labelRuleSource,
		want:    []string{`cpu{region="eu",source="api-acme",tenant="acme"} 42`}},
	{name: "css items", path: "/html", transform: "css",
		metrics: "    metrics:\n      - name: cpu\n        items: tr.row\n        expression: td.cpu\n        labels:\n          - {name: host, expression: td.host}\n          - {name: source, value: \"api-TENANT\"}\n",
		want:    []string{`cpu{host="web01",region="eu",source="api-acme",tenant="acme"} 42`}},
	{name: "csv", path: "/csv", transform: "csv",
		metrics: "    metrics:\n      - name: cpu\n        expression: cpu\n        labels:\n          - {name: host, expression: host}\n          - {name: source, value: \"api-TENANT\"}\n",
		want:    []string{`cpu{host="web01",region="eu",source="api-acme",tenant="acme"} 42`}},
	{name: "xpath over XML", path: "/xml", transform: "xpath",
		metrics: "    metrics:\n      - name: cpu\n        expression: //host/cpu\n        labels:\n          - {name: host, expression: ../@name}\n          - {name: source, value: \"api-TENANT\"}\n",
		want:    []string{`cpu{host="web01",region="eu",source="api-acme",tenant="acme"} 42`}},
	{name: "xpath over HTML", path: "/html", transform: "xpath", collector: "    decoder: {type: html}\n",
		metrics: "    metrics:\n      - name: cpu\n        expression: \"//td[@class='cpu']\"\n" + labelRuleSource,
		want:    []string{`cpu{region="eu",source="api-acme",tenant="acme"} 42`}},
	{name: "prometheus rules", path: "/prom", transform: "prometheus",
		metrics: "    metrics:\n      - name: up\n" + labelRuleSource,
		want:    []string{`up{job="api",region="eu",source="api-acme",tenant="acme"} 1`}},
	{name: "a prometheus rule without a name", path: "/prom", transform: "prometheus", collector: "    limits: {max_label_value_length: 6}\n",
		metrics: "    metrics:\n      - expression: '^up$'\n        labels:\n          - {name: source, value: \"api-TENANT\", truncate: true}\n",
		want:    []string{`up{job="api",region="eu",source="api…",tenant="acme"} 1`}},
	{name: "a prometheus pass-through", path: "/prom", transform: "prometheus",
		want: []string{`up{job="api",region="eu",tenant="acme"} 1`}},

	{name: "remove_labels", path: "/json", transform: "jq", settings: "      remove_labels: [tenant, source]\n",
		metrics: "    metrics:\n      - name: status_up\n        expression: .up\n" + labelRuleSource,
		want:    []string{`status_up{region="eu"} 1`}},
	{name: "rename_labels", path: "/json", transform: "jq", settings: "      rename_labels: {tenant: customer, source: origin}\n",
		metrics: "    metrics:\n      - name: status_up\n        expression: .up\n" + labelRuleSource,
		want:    []string{`status_up{customer="acme",origin="api-acme",region="eu"} 1`}},
	{name: "truncate", path: "/json", transform: "jq", collector: "    limits: {max_label_value_length: 6}\n",
		metrics: "    metrics:\n      - name: status_up\n        expression: .up\n        labels:\n          - {name: source, value: \"api-TENANT\", truncate: true}\n",
		want:    []string{`status_up{region="eu",source="api…",tenant="acme"} 1`}},
	{name: "metrics_prefix and name_escaping", path: "/json", transform: "jq", collector: "    metrics_prefix: app\n    name_escaping: underscores\n",
		metrics: "    metrics:\n      - name: status_up\n        expression: .up\n        labels:\n          - {name: source.name, value: \"api-TENANT\"}\n",
		want:    []string{`app_status_up{region="eu",source_name="api-acme",tenant="acme"} 1`}},
	{name: "a value_map of the rule's name", path: "/json", transform: "jq",
		metrics: "    metrics:\n      - name: status_up\n        expression: .up\n" + labelRuleSource +
			"      - name: status_up\n        expression: .up\n        labels:\n          - name: source\n            expression: .src\n            value_map: {api-acme: mapped}\n",
		want: []string{`status_up{region="eu",source="mapped",tenant="acme"} 1`, `status_up{region="eu",source="other",tenant="acme"} 1`}},
}

// A filled value is treated as the same text written in the configuration:
// for each transform whose rules give a label a static value, and for
// transform.labels under every one of them, the answer of a collector
// written with placeholders, probed with their values, is byte for byte
// that of the collector written with the values, in the text format and in
// OpenMetrics; and so it is under the settings that read a label after the
// transform made it, remove_labels, rename_labels, truncate, the prefix and
// the escaping of names, and a value_map.
func TestAFilledLabelIsTheSameAsOneWritten(t *testing.T) {
	target := newLabelTarget(t)
	for _, tc := range labelCases {
		filled := labelServer(t, tc.document("{{param_tenant}}", "{{param_region:eu}}"))
		written := labelServer(t, tc.document("acme", "eu"))
		if filled.manager.Get().Collectors[0].LabelParams == nil || written.manager.Get().Collectors[0].LabelParams != nil {
			t.Fatalf("%s: the placeholders were not read as the configuration loaded", tc.name)
		}
		for _, openMetrics := range []bool{false, true} {
			got := labelProbe(t, filled, target, "c", "&param_tenant=acme", openMetrics)
			want := labelProbe(t, written, target, "c", "", openMetrics)
			if got.Code != http.StatusOK || got.Body.String() != want.Body.String() || got.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
				t.Errorf("%s, OpenMetrics %v: filled, %d\n%s\nwritten, %d\n%s", tc.name, openMetrics, got.Code, got.Body.String(), want.Code, want.Body.String())
			}
			if !slices.Equal(seriesLines(got.Body.String()), tc.want) {
				t.Errorf("%s, OpenMetrics %v: series\n%s\nwant\n%s", tc.name, openMetrics, strings.Join(seriesLines(got.Body.String()), "\n"), strings.Join(tc.want, "\n"))
			}
		}
	}
}

// A python transform's script makes the series and their labels, and
// transform.labels are applied after it as after every transform: their
// placeholders are filled, over a label of that name the script gave. The
// script is not given the parameters.
func TestTransformLabelsAreFilledAfterAPythonTransform(t *testing.T) {
	requirePython(t)
	target := newLabelTarget(t)
	server := labelServer(t, `collectors:
  - name: scripted
    request: {type: http, path: /json}
    decoder: {type: json}
    transform:
      type: python
      labels:
        tenant: "{{param_tenant}}"
        region: "{{param_region:eu}}"
      script: |
        metric(name="status_up", value=data["up"], labels={"tenant": "from-script", "kind": "scripted"})
`)
	got := labelProbe(t, server, target, "scripted", "&param_tenant=acme", false)
	if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{`status_up{kind="scripted",region="eu",tenant="acme"} 1`}) {
		t.Fatalf("%d\n%s", got.Code, got.Body.String())
	}
	if got := labelProbe(t, server, target, "scripted", "", false); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "transform.labels.tenant needs param_tenant") {
		t.Fatalf("without the parameter: %d %s", got.Code, got.Body.String())
	}
}

// A label whose filled value is empty is left off the series. For
// transform.labels that is the label left out, so a series keeps a label of
// that name it has of its own, where a value that is not empty replaces it;
// a rule's label is the label the rule does not set, so a series a
// prometheus rule passes on keeps its own too.
func TestALabelFilledToNothingIsLeftOffTheSeries(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, `collectors:
  - name: passed_on
    request: {type: http, path: /prom}
    transform:
      type: prometheus
      labels:
        tenant: "{{param_tenant:}}"
    metrics:
      - name: up
        labels:
          - {name: job, value: "{{param_job:}}"}
          - {name: source, value: "{{param_source:}}"}
  - name: read
    request: {type: http, path: /json}
    transform:
      type: jq
      labels:
        tenant: "{{param_tenant:}}"
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: source, value: "{{param_a:}}{{param_b:}}"}
          - {name: fixed, value: x}
`)
	for name, tc := range map[string]struct{ collector, query, want string }{
		"nothing given":                     {"passed_on", "", `up{job="api",tenant="own"} 1`},
		"empty values":                      {"passed_on", "&param_tenant=&param_job=&param_source=", `up{job="api",tenant="own"} 1`},
		"values replace the series' own":    {"passed_on", "&param_tenant=acme&param_job=batch&param_source=s", `up{job="batch",source="s",tenant="acme"} 1`},
		"one of them":                       {"passed_on", "&param_source=s", `up{job="api",source="s",tenant="own"} 1`},
		"a jq rule's label left off":        {"read", "", `status_up{fixed="x"} 1`},
		"a jq rule's label of two halves":   {"read", "&param_b=b", `status_up{fixed="x",source="b"} 1`},
		"a jq rule's label and the tenant":  {"read", "&param_a=a&param_b=b&param_tenant=t", `status_up{fixed="x",source="ab",tenant="t"} 1`},
		"a value of blanks is not nothing":  {"read", "&param_tenant=%20", `status_up{fixed="x",tenant=" "} 1`},
		"a jq rule's label of blanks stays": {"read", "&param_a=%20", `status_up{fixed="x",source=" "} 1`},
	} {
		got := labelProbe(t, server, target, tc.collector, tc.query, false)
		if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{tc.want}) {
			t.Errorf("%s: %d\n%s\nwant %s", name, got.Code, got.Body.String(), tc.want)
		}
	}
}

// limits.max_label_value_length measures a filled value as it measures any
// label: one within it is exported, a longer one fails the scrape in the
// validation unless its label has truncate: true, which cuts it to fit. The
// other limits count a filled label as a label.
func TestAFilledLabelValueIsHeldToTheLimits(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, `collectors:
  - name: bounded
    request: {type: http, path: /json}
    limits: {max_label_value_length: 8, max_labels_per_metric: 3}
    transform:
      type: jq
      labels:
        tenant: "{{param_tenant}}"
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: source, value: "api-{{param_tenant}}", truncate: true}
          - {name: plain, value: "{{param_plain:p}}"}
          - {name: more, value: "{{param_more:}}"}
`)
	for name, tc := range map[string]struct {
		query string
		code  int
		want  string
	}{
		"within the limit":             {"&param_tenant=acme", http.StatusOK, `status_up{plain="p",source="api-acme",tenant="acme"} 1`},
		"cut with truncate":            {"&param_tenant=acme-co", http.StatusOK, `status_up{plain="p",source="api-a…",tenant="acme-co"} 1`},
		"transform.labels too long":    {"&param_tenant=acme-corp", http.StatusBadGateway, `collector bounded validation failed: metric "status_up" label "tenant" value is 9 bytes, longer than limits.max_label_value_length 8`},
		"a rule's label too long":      {"&param_tenant=acme&param_plain=123456789", http.StatusBadGateway, `collector bounded validation failed: metric "status_up" label "plain" value is 9 bytes, longer than limits.max_label_value_length 8`},
		"one label more than the most": {"&param_tenant=acme&param_more=x", http.StatusBadGateway, "collector bounded validation failed: "},
	} {
		got := labelProbe(t, server, target, "bounded", tc.query, false)
		if got.Code != tc.code || tc.code == http.StatusOK && !slices.Equal(seriesLines(got.Body.String()), []string{tc.want}) || tc.code != http.StatusOK && !strings.Contains(got.Body.String(), tc.want) {
			t.Errorf("%s: %d\n%s\nwant %d with %s", name, got.Code, got.Body.String(), tc.code, tc.want)
		}
	}
}

// Rules are compared as they are written when the configuration loads, so
// two rules whose labels differ as written and fill to the same text load,
// and make one series twice only at the scrape whose parameters make them
// the same, which fails as a duplicate series does.
func TestRulesThatFillToTheSameSeriesFailTheScrape(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, `collectors:
  - name: twice
    request: {type: http, path: /json}
    transform: {type: jq}
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: source, value: "{{param_a}}"}
      - name: status_up
        expression: .up
        labels:
          - {name: source, value: "{{param_b}}"}
`)
	got := labelProbe(t, server, target, "twice", "&param_a=x&param_b=y", false)
	if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{`status_up{source="x"} 1`, `status_up{source="y"} 1`}) {
		t.Fatalf("different values: %d\n%s", got.Code, got.Body.String())
	}
	got = labelProbe(t, server, target, "twice", "&param_a=x&param_b=x", false)
	if got.Code != http.StatusBadGateway || !strings.Contains(got.Body.String(), `collector twice validation failed: duplicate metric series "status_up"`) {
		t.Fatalf("the same value: %d\n%s", got.Code, got.Body.String())
	}
}

const cachedLabelConfig = `collectors:
  - name: cached
    request: {type: http, path: /json}
    cache: {CACHE}
    transform:
      type: jq
      labels:
        tenant: "{{param_tenant}}"
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: source, value: "api-{{param_tenant}}"}
`

// Two probes that differ only in a parameter a label takes never get each
// other's series from the response cache: each value has an entry of its
// own, and a repeat of a probe is answered from its own entry, without a
// trip to the target.
func TestProbesThatDifferInALabelParameterAreCachedApart(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, strings.Replace(cachedLabelConfig, "CACHE", "ttl: 1m", 1))
	for round, wantRequests := range []int64{2, 2} {
		for _, tenant := range []string{"acme", "globex"} {
			got := labelProbe(t, server, target, "cached", "&param_tenant="+tenant, false)
			if want := `status_up{source="api-` + tenant + `",tenant="` + tenant + `"} 1`; got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{want}) {
				t.Fatalf("round %d, %s: %d\n%s", round, tenant, got.Code, got.Body.String())
			}
		}
		if n := target.requests.Load(); n != wantRequests {
			t.Fatalf("round %d: the target was contacted %d times, want %d", round, n, wantRequests)
		}
	}
	if metrics := selfMetrics(t, server); !strings.Contains(metrics, `http_exporter_cache_hits_total{collector="cached"} 2`) {
		t.Errorf("the repeats were not answered from the cache:\n%s", metrics)
	}
}

// A failed trip is answered with the last good result of the same probe
// (cache.stale_if_error), and never with that of a probe that gave a label
// another value: one that has no good result of its own fails.
func TestAStaleResultIsThatOfTheSameLabelParameter(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, strings.Replace(cachedLabelConfig, "CACHE", "ttl: 0s, stale_if_error: 5m", 1))
	got := labelProbe(t, server, target, "cached", "&param_tenant=acme", false)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `status_up{source="api-acme",tenant="acme"} 1`) {
		t.Fatalf("a good trip: %d\n%s", got.Code, got.Body.String())
	}
	target.failing.Store(true)
	got = labelProbe(t, server, target, "cached", "&param_tenant=globex", false)
	if got.Code != http.StatusBadGateway || strings.Contains(got.Body.String(), "acme") {
		t.Fatalf("a failed trip of a tenant without a good result: %d\n%s", got.Code, got.Body.String())
	}
	got = labelProbe(t, server, target, "cached", "&param_tenant=acme", false)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `status_up{source="api-acme",tenant="acme"} 1`) || seriesValue(t, got.Body.String(), resultStaleMetric) != 1 {
		t.Fatalf("a failed trip of the tenant with a good result: %d\n%s", got.Code, got.Body.String())
	}
	if n := target.requests.Load(); n != 3 {
		t.Fatalf("the target was contacted %d times, want 3", n)
	}
}

// Probes in flight at the same time share one request only when they are
// identical: two that differ in a label's parameter each make their own and
// each get their own series, and two of one value share one.
func TestConcurrentProbesThatDifferInALabelParameterAreNotShared(t *testing.T) {
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	server := labelServer(t, `collectors:
  - name: shared
    request: {type: http}
    transform:
      type: regex
      labels:
        tenant: "{{param_tenant}}"
    metrics:
      - name: demo_value
        expression: 'value=(\d+)'
        labels:
          - {name: source, value: "api-{{param_tenant}}"}
`)
	tenants := []string{"acme", "globex", "acme", "initech", "globex"}
	var outcomes []<-chan probeOutcome
	for _, tenant := range tenants {
		outcomes = append(outcomes, probeAsync(context.Background(), server, probePath("shared", target.URL, "&param_tenant="+tenant), nil))
	}
	waitForWaiters(t, server, len(tenants))
	target.open()
	for i, outcome := range outcomes {
		got := <-outcome
		if want := `demo_value{source="api-` + tenants[i] + `",tenant="` + tenants[i] + `"} 42`; got.code != http.StatusOK || !slices.Equal(seriesLines(got.body), []string{want}) {
			t.Errorf("probe %d of %s: %d\n%s", i, tenants[i], got.code, got.body)
		}
	}
	if n := target.requests.Load(); n != 3 {
		t.Fatalf("the target was asked %d times for the probes of 3 tenants", n)
	}
}

// A static target's params fill the label placeholders of its collector as
// they fill the request's, for the endpoint that serves the targets and for
// the OTLP export alike: the series carry the filled labels as any labels,
// beside the target's own, which are literal.
func TestAStaticTargetsParamsFillALabelValue(t *testing.T) {
	target := newLabelTarget(t)
	c := testutil.Collector("tenants", "text")
	c.Request.Path = "/text"
	// With a cache, which the targets' params key as a probe's parameters
	// do: neither target is answered with the other's entry.
	c.Cache.TTL = model.Duration(time.Minute)
	c.Transform.Labels = map[string]string{"tenant": "{{param_tenant}}", "region": "{{param_region:eu}}"}
	c.Metrics[0].Labels = []model.LabelRule{{Name: "source", Value: "api-{{param_tenant}}"}}
	cfg := &model.Config{Collectors: []model.Collector{c}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "acme", Collector: "tenants", Target: target.URL, ExportViaOTLP: true, Params: map[string]string{"param_tenant": "acme"}, Labels: map[string]string{"own": "{{literal}}"}},
		{Name: "globex", Collector: "tenants", Target: target.URL, ExportViaOTLP: true, Params: map[string]string{"param_tenant": "globex", "param_region": "us"}},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(context.Background(), 0)
	body := getStaticTargets(t, server, "/static-targets")
	for _, want := range []string{
		`demo_value{own="{{literal}}",region="eu",source="api-acme",static_target="acme",tenant="acme"} 1`,
		`demo_value{region="us",source="api-globex",static_target="globex",tenant="globex"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("the endpoint lacks %s:\n%s", want, body)
		}
	}
	exported := map[string]string{}
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			if m.Name == "demo_value" {
				exported[m.Labels["tenant"]] = m.Labels["source"] + "/" + m.Labels["region"]
			}
		}
	}
	if len(exported) != 2 || exported["acme"] != "api-acme/eu" || exported["globex"] != "api-globex/us" {
		t.Errorf("exported over OTLP: %v", exported)
	}
	if n := target.requests.Load(); n != 2 {
		t.Errorf("the target was contacted %d times for two targets", n)
	}

	// A target that leaves a label's parameter out, and one that gives a
	// parameter nothing uses, are refused when the file loads.
	for name, tc := range map[string]struct {
		params map[string]string
		want   string
	}{
		"a missing parameter": {nil, `target "t" uses collector "tenants", whose transform.labels.tenant needs param_tenant, a parameter without a default; a static target has no probe to supply it, so set it under the target's params, or give the placeholder a default`},
		"an unused parameter": {map[string]string{"param_tenant": "acme", "param_tenat": "x"}, `target "t" params param_tenat are not used by collector "tenants": no placeholder in its request or its label values names them`},
		"a value not UTF-8":   {map[string]string{"param_tenant": "a\xffb"}, `target "t" uses collector "tenants": transform.labels.tenant: the value of param_tenant is not valid UTF-8, which a label value must be`},
	} {
		refused := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "tenants", Target: target.URL, Params: tc.params}}}
		if err := config.ValidateStaticTargets(refused); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := config.ValidateStaticTargetsAgainst(refused, cfg); err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v\nwant %s", name, err, tc.want)
		}
	}
}

// A debug probe's report shows the series with their labels filled, as the
// probe would answer them, and the collectors page asks for a label's
// parameter with the request's.
func TestADebugProbeAndTheCollectorsPageKnowALabelParameter(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, tenantStatusConfig)
	server.SetProbeDebug(true)
	got := labelProbe(t, server, target, "tenant_status", "&param_tenant=acme&debug=1", false)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `status_up{region="eu",source="api-acme",tenant="acme"} 1`) || strings.Contains(got.Body.String(), `"{{param_tenant}}"`) {
		t.Fatalf("the debug report: %d\n%s", got.Code, got.Body.String())
	}
	page := getPage(t, server, "/collectors")
	requireContains(t, page,
		`name="param_tenant" required>`,
		`name="param_region" placeholder="default: eu">`,
		`name="param_source" required>`,
	)
}
