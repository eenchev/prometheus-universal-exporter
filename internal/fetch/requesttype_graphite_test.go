//go:build !select_request_types || request_type_graphite

package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// graphiteCollector asks the render API for the given expressions.
func graphiteCollector(targets ...string) model.Collector {
	return model.Collector{
		Name:      "graphite",
		Request:   model.RequestConfig{Type: RequestTypeGraphite, Targets: targets},
		Transform: model.TransformConfig{Type: "jq"},
	}
}

func graphiteURL(t *testing.T, c *model.Collector, overrides RequestOverrides) *url.URL {
	t.Helper()
	u, err := resolveRequestURL("graphite.example:8080", c, overrides)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// A graphite request is a GET of /render with every expression as a target,
// the window and format=json; request.query adds to it.
func TestGraphiteRequestURL(t *testing.T) {
	c := graphiteCollector("app.*.requests.count", "sumSeries(app.*.errors)")
	c.Request.Query = map[string]string{"maxDataPoints": "10"}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if c.Request.Path != "/render" || c.Request.From != "-15min" || c.Request.Until != "now" || RequestMethodFor(&c, RequestOverrides{}) != http.MethodGet {
		t.Fatalf("defaults: %+v", c.Request)
	}
	u := graphiteURL(t, &c, RequestOverrides{})
	if u.Scheme != "http" || u.Host != "graphite.example:8080" || u.Path != "/render" {
		t.Fatalf("url=%s", u)
	}
	want := url.Values{
		"target": {"app.*.requests.count", "sumSeries(app.*.errors)"},
		"from":   {"-15min"}, "until": {"now"}, "format": {"json"}, "maxDataPoints": {"10"},
	}
	if got := u.Query(); !reflect.DeepEqual(got, want) {
		t.Fatalf("query %v, want %v", got, want)
	}
	// The verbose url label is the render URL without its query.
	if label, err := RequestLabelFor("graphite.example:8080", &c, RequestOverrides{}); err != nil || label != "http://graphite.example:8080/render" {
		t.Fatalf("label %q, %v", label, err)
	}

	c = graphiteCollector("a.b")
	c.Request.Path, c.Request.From, c.Request.Until = "/graphite/render", " -1h ", "-5min"
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	u = graphiteURL(t, &c, RequestOverrides{})
	if u.Path != "/graphite/render" || u.Query().Get("from") != "-1h" || u.Query().Get("until") != "-5min" {
		t.Fatalf("url=%s", u)
	}
}

// Placeholders in a target are filled from the probe, with only what a path
// node or tag value is made of, so a value cannot rewrite the expression.
func TestGraphiteTargetPlaceholders(t *testing.T) {
	c := graphiteCollector("seriesByTag('name=cpu.load', 'env={{param_env:prod}}')", "app.{{param_host}}.requests")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	u := graphiteURL(t, &c, RequestOverrides{Params: map[string]string{"param_host": "web-01"}})
	if got := u.Query()["target"]; !reflect.DeepEqual(got, []string{"seriesByTag('name=cpu.load', 'env=prod')", "app.web-01.requests"}) {
		t.Fatalf("targets %q", got)
	}
	u = graphiteURL(t, &c, RequestOverrides{Params: map[string]string{"param_host": "web-01", "param_env": "staging"}})
	if got := u.Query()["target"][0]; got != "seriesByTag('name=cpu.load', 'env=staging')" {
		t.Fatalf("target %q", got)
	}
	if _, err := resolveRequestURL("graphite:8080", &c, RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "request.targets[1] needs param_host") {
		t.Fatalf("a missing parameter: err=%v", err)
	}
	for _, value := range []string{"x')", "a,b", "*", "a b", "x'"} {
		_, err := resolveRequestURL("graphite:8080", &c, RequestOverrides{Params: map[string]string{"param_host": value}})
		if err == nil || !strings.Contains(err.Error(), "lands inside a Graphite expression") {
			t.Errorf("%q: err=%v", value, err)
		}
	}
	// A probe parameter no placeholder uses is still a mistake.
	if unused, err := CheckRequestParams(&c, RequestOverrides{Params: map[string]string{"param_host": "a", "param_hots": "b"}}); err != nil || !reflect.DeepEqual(unused, []string{"param_hots"}) {
		t.Fatalf("unused=%v err=%v", unused, err)
	}
	if params := RequestParams(&c); len(params) != 2 || params[0].Name != "param_env" || params[0].Default != "prod" || params[1].Name != "param_host" || !params[1].Required {
		t.Fatalf("params %+v", params)
	}
}

func TestGraphiteRequestValidation(t *testing.T) {
	for name, test := range map[string]struct {
		edit func(*model.Collector)
		want string
	}{
		"no targets":        {func(c *model.Collector) { c.Request.Targets = nil }, "has no request.targets"},
		"an empty target":   {func(c *model.Collector) { c.Request.Targets = []string{"a.b", " "} }, `request.targets[1] " ": is empty`},
		"an open bracket":   {func(c *model.Collector) { c.Request.Targets = []string{"sumSeries(a.b"} }, "has a ( that is never closed"},
		"a stray bracket":   {func(c *model.Collector) { c.Request.Targets = []string{"a.b)"} }, "has a ) that closes nothing"},
		"crossed brackets":  {func(c *model.Collector) { c.Request.Targets = []string{"f(a.{b,c)}"} }, "has a ) that closes nothing"},
		"an open quote":     {func(c *model.Collector) { c.Request.Targets = []string{"alias(a.b, 'x)"} }, "has a ' that is never closed"},
		"a line break":      {func(c *model.Collector) { c.Request.Targets = []string{"a.b\nc.d"} }, "control character"},
		"a bad placeholder": {func(c *model.Collector) { c.Request.Targets = []string{"a.{{param_}}"} }, "is not a path parameter"},
		"its own query":     {func(c *model.Collector) { c.Request.Query = map[string]string{"Target": "x"} }, "sets request.query Target, which a graphite collector sets itself"},
		"format":            {func(c *model.Collector) { c.Request.Query = map[string]string{"format": "csv"} }, "format always json"},
		"a spaced window":   {func(c *model.Collector) { c.Request.From = "-5 min" }, `request.from: "-5 min" has a space in it`},
		"an http key":       {func(c *model.Collector) { c.Request.Method = "POST" }, `request.method, which does not apply to request.type "graphite"`},
		"a body":            {func(c *model.Collector) { c.Request.Body = "x" }, `request.body, which does not apply`},
		"a localfile key":   {func(c *model.Collector) { c.Request.Root = "/tmp" }, `request.root, which does not apply`},
		"basic and bearer": {func(c *model.Collector) {
			c.Request.BearerToken = "t"
			c.Request.BasicAuth = &model.BasicAuth{Username: "u"}
		}, "cannot configure basic and bearer authentication together"},
		"a negative retry":   {func(c *model.Collector) { c.Request.Retry.Attempts = -1 }, "retry.attempts must not be negative"},
		"a bad header param": {func(c *model.Collector) { c.Request.Headers = map[string]string{"X": "{{param_}}"} }, "is not a path parameter"},
	} {
		t.Run(name, func(t *testing.T) {
			c := graphiteCollector("a.b")
			test.edit(&c)
			if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
	// Quoted brackets, globs and nested functions are Graphite's own.
	c := graphiteCollector(`aliasSub(sumSeries(app.{web,api}[0-9].x), '(\w+)', "\1")`, "seriesByTag('name=a', 'env=~(prod|staging)')")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	// The graphite keys are graphite's alone.
	h := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, Targets: []string{"a"}}}
	if err := ValidateRequest(&h); err == nil || !strings.Contains(err.Error(), `request.targets, which does not apply to request.type "http"`) {
		t.Fatalf("an http collector with targets: err=%v", err)
	}
	// A probe parameter of http's alone is refused.
	if err := CheckOverrideParams(&c, url.Values{"method": {"POST"}}); err == nil || !strings.Contains(err.Error(), `request.type is "graphite"`) {
		t.Fatalf("method: err=%v", err)
	}
	if err := CheckOverrideParams(&c, url.Values{"timeout": {"1s"}, "path": {"/x"}}); err != nil {
		t.Fatal(err)
	}
}

// A static target may bring its own expressions and window, written out.
func TestGraphiteStaticTargetRequest(t *testing.T) {
	c := graphiteCollector("app.{{param_host:web}}.x")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	target := &model.StaticTarget{Name: "t", Collector: c.Name, Target: "http://graphite:8080", Request: model.TargetRequestConfig{Targets: []string{"db.*.x", "db.*.y"}, From: "-1h", Until: "-1min"}}
	if err := CheckTargetRequest(target, &c); err != nil {
		t.Fatal(err)
	}
	u := graphiteURL(t, &c, TargetOverrides(target))
	if got := u.Query(); !reflect.DeepEqual(got["target"], []string{"db.*.x", "db.*.y"}) || got.Get("from") != "-1h" || got.Get("until") != "-1min" {
		t.Fatalf("query %v", got)
	}
	// Its own targets replace the collector's placeholders too.
	if unused, err := CheckRequestParams(&c, TargetOverrides(target)); err != nil || len(unused) != 0 {
		t.Fatalf("unused=%v err=%v", unused, err)
	}
	for request, want := range map[*model.TargetRequestConfig]string{
		{Targets: []string{"f(a"}}: `target "t": request.targets[0] "f(a": has a ( that is never closed`,
		{Until: "now - 1h"}:        `target "t": request.until: "now - 1h" has a space in it`,
		{Method: "POST"}:           `sets request.method, which does not apply to collector "graphite"`,
		{Body: "x", BodySet: true}: `sets request.body, which does not apply`,
	} {
		target.Request = *request
		if err := CheckTargetRequest(target, &c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err=%v, want %q", request, err, want)
		}
	}
	// A static target of an http collector cannot set them.
	h := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}}
	if err := ValidateRequest(&h); err != nil {
		t.Fatal(err)
	}
	if err := CheckTargetRequest(&model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{From: "-1h"}}, &h); err == nil || !strings.Contains(err.Error(), `request.from, which does not apply to collector "web"`) {
		t.Fatalf("err=%v", err)
	}
}

// The request goes over http's transport, credentials and all.
func TestGraphiteFetch(t *testing.T) {
	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"target":"a.b","datapoints":[[1,1727000000]]}]`))
	}))
	defer server.Close()
	c := graphiteCollector("a.b")
	c.Request.BearerToken = "secret"
	c.Request.Headers = map[string]string{"X-Grafana-Org-Id": "1"}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	resp, err := FetchCollector(context.Background(), server.URL, &c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), `"a.b"`) {
		t.Fatalf("resp=%+v", resp)
	}
	if got.Method != http.MethodGet || got.URL.Path != "/render" || got.URL.Query().Get("target") != "a.b" || got.URL.Query().Get("format") != "json" {
		t.Fatalf("request %s %s", got.Method, got.URL)
	}
	if got.Header.Get("Authorization") != "Bearer secret" || got.Header.Get("X-Grafana-Org-Id") != "1" {
		t.Fatalf("headers %v", got.Header)
	}
	if err := CheckTarget(&c, "", false); !errors.Is(err, ErrMissingTarget) {
		t.Fatalf("a probe without a target: err=%v", err)
	}
	if err := CheckTarget(&c, "graphite:8080", true); err == nil {
		t.Fatal("a static target's address must be an absolute URL")
	}
}

// Inside quotes a backslash escapes the next character, as in Graphite's
// grammar, so an escaped quote does not end the string.
func TestGraphiteEscapedQuotes(t *testing.T) {
	for _, expression := range []string{`aliasSub(a.b, 'it\'s', 'x')`, `alias(a.b, "say \"hi\"")`, `alias(a.b, 'ends in \\')`, `alias(a.b, '(')`} {
		if err := checkGraphiteExpression(expression); err != nil {
			t.Errorf("%s: %v", expression, err)
		}
	}
	for expression, want := range map[string]string{`alias(a.b, 'x\')`: "has a ' that is never closed", `alias(a.b, "x\")`: `has a " that is never closed`} {
		if err := checkGraphiteExpression(expression); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err=%v, want %q", expression, err, want)
		}
	}
}

// An expression listed twice asks for every one of its series twice.
func TestGraphiteRepeatedExpressions(t *testing.T) {
	c := graphiteCollector("a.b", "c.d", " a.b ")
	if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), `collector "graphite" request.targets[2] repeats request.targets[0], "a.b"; list each expression once`) {
		t.Fatalf("err=%v", err)
	}
	c = graphiteCollector("a.b")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	target := &model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{Targets: []string{"x.y", "x.y"}}}
	if err := CheckTargetRequest(target, &c); err == nil || !strings.Contains(err.Error(), `target "t": request.targets[1] repeats request.targets[0]`) {
		t.Fatalf("err=%v", err)
	}
}

// from and until are probe parameters too, checked as the configuration's
// are, and refused for a collector of another type.
func TestGraphiteWindowProbeParameters(t *testing.T) {
	c := graphiteCollector("a.b")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	values := url.Values{"from": {" -1h "}, "until": {"-10min"}}
	if err := CheckOverrideParams(&c, values); err != nil {
		t.Fatal(err)
	}
	overrides, err := ParseRequestOverrides(values)
	if err != nil {
		t.Fatal(err)
	}
	if q := graphiteURL(t, &c, overrides).Query(); q.Get("from") != "-1h" || q.Get("until") != "-10min" {
		t.Fatalf("query %v", q)
	}
	for _, bad := range []url.Values{{"from": {""}}, {"until": {"now - 1h"}}} {
		if _, err := ParseRequestOverrides(bad); err == nil || !strings.Contains(err.Error(), "want a Graphite time") {
			t.Errorf("%v: err=%v", bad, err)
		}
	}
	h := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}}
	if err := ValidateRequest(&h); err != nil {
		t.Fatal(err)
	}
	if err := CheckOverrideParams(&h, url.Values{"from": {"-1h"}}); err == nil || !strings.Contains(err.Error(), `probe parameters from do not apply to collector "web"`) {
		t.Fatalf("err=%v", err)
	}
}

// Expressions too long for a URL go as a form with POST, which is retried
// like a GET, since it only reads; the http_method label says POST.
func TestLongGraphiteRequestsArePosted(t *testing.T) {
	var got []*http.Request
	var forms []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = append(got, r)
		forms = append(forms, r.PostForm)
		if len(got) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	var long []string
	for i := range 60 {
		long = append(long, "servers.web"+strings.Repeat("x", 30)+string(rune('a'+i%26))+strconv.Itoa(i)+".cpu.user")
	}
	c := graphiteCollector(long...)
	c.Request.Query = map[string]string{"maxDataPoints": "5"}
	c.Request.Retry = model.RetryConfig{Attempts: 1}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if method := RequestMethodFor(&c, RequestOverrides{}); method != http.MethodPost {
		t.Fatalf("method %s", method)
	}
	if _, err := FetchCollector(context.Background(), server.URL, &c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d requests; a POST to the render API is retried", len(got))
	}
	last := got[1]
	if last.Method != http.MethodPost || last.URL.RawQuery != "" || last.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("%s %s %v", last.Method, last.URL, last.Header)
	}
	if form := forms[1]; len(form["target"]) != 60 || form.Get("format") != "json" || form.Get("from") != "-15min" || form.Get("maxDataPoints") != "5" {
		t.Fatalf("form %v", form)
	}
	// A static target's own short targets are a GET again.
	if method := RequestMethodFor(&c, RequestOverrides{Targets: []string{"a.b"}}); method != http.MethodGet {
		t.Fatalf("method %s", method)
	}
}
