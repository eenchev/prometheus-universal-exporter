//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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

// scriptParamsConfig has a python transform and a pre_script that read
// the probe's parameters, and a script that reads none.
const scriptParamsConfig = `collectors:
  - name: script
    request: {type: http, path: /json}
    transform:
      type: python
      script: |
        for item in data["items"]:
            if params.get("id") in (None, item["id"]):
                metric(name="item_value", value=item["v"], labels={"id": item["id"], "zone": params.get('zone', "none")})
  - name: pre
    request: {type: http, path: /json}
    transform:
      type: jq
      pre_script: |
        data = {"up": data["up"], "asked": params["who"] if "who" in params else None, "given": len(params)}
    metrics:
      - name: up
        expression: .up
        labels:
          - {name: asked, expression: .asked}
          - {name: given, expression: .given}
  - name: reads_none
    request: {type: http, path: /json}
    transform:
      type: python
      script: |
        metric(name="up", value=data["up"])
`

// A Python script reads the probe's parameters as params, a dict of each
// by its name, in a python transform and in a pre_script, as a jq
// expression reads $params. A parameter the script names is taken, any
// parameter by a script that reads params otherwise, and none by a script
// that does not read it.
func TestAPythonScriptReadsTheProbesParameters(t *testing.T) {
	requirePython(t)
	target := newLabelTarget(t)
	server := labelServer(t, scriptParamsConfig)
	for name, tc := range map[string]struct {
		collector, query string
		want             []string
	}{
		"none given":            {"script", "", []string{`item_value{id="a",zone="none"} 1`, `item_value{id="b",zone="none"} 2`}},
		"two that are named":    {"script", "&param_id=b&param_zone=eu", []string{`item_value{id="b",zone="eu"} 2`}},
		"an empty one":          {"script", "&param_id=&param_zone=eu", []string{`item_value{id="a",zone="eu"} 1`, `item_value{id="b",zone="eu"} 2`}},
		"a pre_script":          {"pre", "&param_who=me&param_other=x", []string{`up{asked="me",given="2"} 1`}},
		"a pre_script, none":    {"pre", "", []string{`up{given="0"} 1`}},
		"a script without them": {"reads_none", "", []string{`up 1`}},
	} {
		got := labelProbe(t, server, target, tc.collector, tc.query, false)
		if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), tc.want) {
			t.Errorf("%s: %d\n%s\nwant %v", name, got.Code, got.Body.String(), tc.want)
		}
	}
	for name, tc := range map[string]struct{ collector, query, want string }{
		"a name the script does not read": {"script", "&param_idd=a", `probe parameters param_idd are not used by collector "script": no placeholder in its request.path ("/json"), body, header or query values or its label values names them, and its script reads none of them as params["<name>"]`},
		"a script that reads none":        {"reads_none", "&param_id=a", `probe parameters param_id are not used by collector "reads_none"`},
	} {
		got := labelProbe(t, server, target, tc.collector, tc.query, false)
		if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), tc.want) {
			t.Errorf("%s: %d %q, want 400 with %q", name, got.Code, got.Body.String(), tc.want)
		}
	}
}

// clockConfig has a jq collector, a python one and a pre_script that read
// the scrape's clock, and one that reads none.
const clockConfig = `collectors:
  - name: jq
    request: {type: http, path: /json}
    transform: {type: jq}
    metrics:
      - name: scrape_time_seconds
        expression: $scrape_time
      - name: since_last_success_seconds
        expression: 'if $last_success then $scrape_time - $last_success else -1 end'
        labels:
          - {name: tenant, value: "{{param_tenant:none}}"}
  - name: script
    request: {type: http, path: /json}
    transform:
      type: python
      script: |
        metric(name="scrape_time_seconds", value=scrape_time)
        metric(name="since_last_success_seconds", value=-1 if last_success is None else scrape_time - last_success)
        metric(name="last_success_seconds", value=last_success or 0)
  - name: pre
    request: {type: http, path: /json}
    transform:
      type: jq
      pre_script: |
        data = {"at": scrape_time, "first": last_success is None}
    metrics:
      - name: scrape_time_seconds
        expression: .at
      - name: first_scrape
        expression: .first
  - name: plain
    request: {type: http, path: /json}
    transform: {type: jq}
    metrics:
      - name: up
        expression: .up
`

// A jq or yq expression reads when its scrape began as $scrape_time and
// when the last scrape that succeeded began as $last_success, in seconds
// since the epoch, and a Python script, a pre_script too, as scrape_time
// and last_success. There is no last success at a first scrape, null and
// None; after one that succeeded it is that scrape's time, kept for each
// target and each set of parameters, and a scrape that failed keeps the
// one before it. A collector that reads neither keeps nothing.
func TestATransformReadsTheScrapesClock(t *testing.T) {
	requirePython(t)
	target := newLabelTarget(t)
	server := labelServer(t, clockConfig)
	value := func(collector, query, name string) float64 {
		t.Helper()
		got := labelProbe(t, server, target, collector, query, false)
		for _, line := range seriesLines(got.Body.String()) {
			if series, text, _ := strings.Cut(line, " "); series == name || strings.HasPrefix(series, name+"{") {
				n, err := strconv.ParseFloat(text, 64)
				if err != nil {
					t.Fatalf("%s: %s: %v", collector, line, err)
				}
				return n
			}
		}
		t.Fatalf("%s%s: %d, no %s in\n%s", collector, query, got.Code, name, got.Body.String())
		return 0
	}
	for _, collector := range []string{"jq", "script"} {
		before := float64(time.Now().UnixNano()) / 1e9
		first := value(collector, "", "scrape_time_seconds")
		if after := float64(time.Now().UnixNano()) / 1e9; first < before-0.001 || first > after+0.001 {
			t.Errorf("%s: the scrape's time is %f, between %f and %f", collector, first, before, after)
		}
		time.Sleep(20 * time.Millisecond)
		second := value(collector, "", "scrape_time_seconds")
		if since := value(collector, "", "since_last_success_seconds"); since <= 0 || since > 60 || second <= first {
			t.Errorf("%s: %f seconds since the last success, the scrapes at %f and %f", collector, since, first, second)
		}
	}
	// A first scrape has none, and each set of parameters its own.
	if since := value("jq", "&param_tenant=new", "since_last_success_seconds"); since != -1 {
		t.Errorf("a first scrape of other parameters: %f since the last success, want none", since)
	}
	if since := value("jq", "&param_tenant=new", "since_last_success_seconds"); since <= 0 {
		t.Errorf("its second: %f since the last success", since)
	}
	if first := value("pre", "", "first_scrape"); first != 1 {
		t.Errorf("a pre_script's first scrape: first_scrape %f", first)
	}
	if first := value("pre", "", "first_scrape"); first != 0 {
		t.Errorf("a pre_script's second scrape: first_scrape %f", first)
	}
	// A scrape that fails keeps the last success it started from.
	succeeded := value("script", "", "scrape_time_seconds")
	target.failing.Store(true)
	if got := labelProbe(t, server, target, "script", "", false); got.Code == http.StatusOK {
		t.Fatalf("the target is failing, and the probe answered %d", got.Code)
	}
	target.failing.Store(false)
	if last := value("script", "", "last_success_seconds"); last != succeeded {
		t.Errorf("after a failed scrape the last success is %f, want the scrape before it, %f", last, succeeded)
	}
	if value("plain", "", "up") != 1 || len(server.lastSuccesses.at) != 4 {
		t.Errorf("%d last successes are kept, want those of jq, jq with a tenant, script and pre", len(server.lastSuccesses.at))
	}
}
