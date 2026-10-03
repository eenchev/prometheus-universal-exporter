//go:build !select_request_types || (request_type_http && request_type_graphite)

package fetch

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

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
		"a negative retry":   {func(c *model.Collector) { c.Request.Retry.Attempts = -1 }, "retry.attempts must be from 0 to 10"},
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
