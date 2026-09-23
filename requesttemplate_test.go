package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Placeholders in request bodies, header values and query values
// (requesttemplate.go).

func TestTemplatePlaceholdersAreParsed(t *testing.T) {
	for name, tc := range map[string]struct {
		field templateField
		want  string // an error fragment, or "" for none
		count int
	}{
		"json body with braces of its own":  {templateField{"request.body", `{"a":{"b":{{param_x|json}}}}`, "body"}, "", 1},
		"braces that are not a placeholder": {templateField{"request.body", `{{"not": "one"}}`, "body"}, "", 0},
		"a default and a filter":            {templateField{"request.body", `{{param_x:a b|form}}`, "body"}, "", 1},
		"spaces":                            {templateField{"request.body", `{{ param_x }}`, "body"}, "a placeholder with a space after {{", 0},
		"an unknown filter":                 {templateField{"request.body", `{{param_x|yaml}}`, "body"}, `unknown filter "yaml"`, 0},
		"unclosed":                          {templateField{"request.body", `{{param_x`, "body"}, "unclosed placeholder", 0},
		"a bad name":                        {templateField{"request.body", `{{param_x-y}}`, "body"}, "is not a path parameter", 0},
		"a filter in a header":              {templateField{"request.headers.X-Tenant", `{{param_x|json}}`, "header"}, "filters apply only in request.body", 0},
		"a filter in a query":               {templateField{"request.query.q", `{{param_x|raw}}`, "query"}, "filters apply only in request.body", 0},
		"an unexpanded environment default": {templateField{"request.body", `{{param_x:${X}}}`, "body"}, "a default containing a brace", 0},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := tc.field.parse()
			if tc.want == "" {
				if err != nil || len(got) != tc.count {
					t.Fatalf("got %d placeholders, %v", len(got), err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestTemplateValuesAreEncodedForTheirPlace(t *testing.T) {
	body := func(text string) templateField { return templateField{"request.body", text, "body"} }
	for _, tc := range []struct {
		field  templateField
		params map[string]string
		want   string
	}{
		{body(`{"name": {{param_x|json}}}`), map[string]string{"param_x": `a"b\c` + "\n"}, `{"name": "a\"b\\c\n"}`},
		{body(`{"name": {{param_x|json}}}`), map[string]string{"param_x": "Zoë"}, `{"name": "Zoë"}`},
		{body(`{"limit": {{param_n|number}}}`), map[string]string{"param_n": "-1.5e3"}, `{"limit": -1.5e3}`},
		{body(`a={{param_x|form}}&b=1`), map[string]string{"param_x": "x&y=z é"}, `a=x%26y%3Dz+%C3%A9&b=1`},
		{body(`<t>{{param_x|xml}}</t>`), map[string]string{"param_x": `<a & "b">`}, `<t>&lt;a &amp; &#34;b&#34;&gt;</t>`},
		{body(`raw {{param_x}} and {{param_x|raw}}`), map[string]string{"param_x": `"q"`}, `raw "q" and "q"`},
		{body(`{"region": {{param_r:eu|json}}}`), nil, `{"region": "eu"}`},
		{templateField{"request.headers.X-Tenant", "tenant-{{param_t}}", "header"}, map[string]string{"param_t": "acme"}, "tenant-acme"},
	} {
		got, err := tc.field.render(tc.params)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.field.text, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		field  templateField
		params map[string]string
		want   string
	}{
		{body(`{{param_n|number}}`), map[string]string{"param_n": "12; DROP"}, "must be a number for its |number filter"},
		{body(`{{param_n|number}}`), map[string]string{"param_n": "0x10"}, "must be a number"},
		{templateField{"request.headers.X-Tenant", "{{param_t}}", "header"}, map[string]string{"param_t": "a\r\nX-Admin: 1"}, "contains a control character"},
		{body(`{{param_x}}`), nil, "request.body needs param_x"},
	} {
		if _, err := tc.field.render(tc.params); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s with %v: err=%v, want %q", tc.field.text, tc.params, err, tc.want)
		}
	}
}

// echoTarget records the requests it receives and answers value=1.
type echoTarget struct {
	mu       sync.Mutex
	requests []echoedRequest
	server   *httptest.Server
}

type echoedRequest struct {
	method, path, query, body string
	header                    http.Header
}

func newEchoTarget(t *testing.T) *echoTarget {
	t.Helper()
	e := &echoTarget{}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.requests = append(e.requests, echoedRequest{r.Method, r.URL.Path, r.URL.RawQuery, string(body), r.Header.Clone()})
		e.mu.Unlock()
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(e.server.Close)
	return e
}

func (e *echoTarget) last(t *testing.T) echoedRequest {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.requests) == 0 {
		t.Fatal("the target was not contacted")
	}
	return e.requests[len(e.requests)-1]
}

func (e *echoTarget) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.requests)
}

func templatedCollector() Collector {
	c := testCollector("graphql", "text")
	c.Request.Method = "POST"
	c.Request.Path = "/api/{{param_tenant}}"
	c.Request.Headers = map[string]string{"Content-Type": "application/json", "X-Tenant": "{{param_tenant}}"}
	c.Request.Query = map[string]string{"region": "{{param_region:eu}}", "static": "yes"}
	c.Request.Body = `{"service": {{param_service|json}}, "limit": {{param_limit:10|number}}}`
	return c
}

func TestAProbeFillsTheBodyHeadersAndQuery(t *testing.T) {
	target := newEchoTarget(t)
	server := pathServer(t, templatedCollector())
	server.logger = quietLogger(t)
	probe := func(params string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		server.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/probe?collector=graphql&target="+url.QueryEscape(target.server.URL)+params, nil))
		return r
	}
	r := probe("&param_tenant=acme&param_service=" + url.QueryEscape(`checkout "eu"`))
	if r.Code != http.StatusOK {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	got := target.last(t)
	if got.method != "POST" || got.path != "/api/acme" || got.header.Get("X-Tenant") != "acme" {
		t.Fatalf("request %+v", got)
	}
	if got.body != `{"service": "checkout \"eu\"", "limit": 10}` {
		t.Fatalf("body %q", got.body)
	}
	query, _ := url.ParseQuery(got.query)
	if query.Get("region") != "eu" || query.Get("static") != "yes" {
		t.Fatalf("query %q", got.query)
	}

	probe("&param_tenant=acme&param_service=x&param_region=" + url.QueryEscape("us&x=1") + "&param_limit=50")
	got = target.last(t)
	query, _ = url.ParseQuery(got.query)
	if query.Get("region") != "us&x=1" || query.Get("x") != "" || !strings.Contains(got.body, `"limit": 50`) {
		t.Fatalf("query %q body %q", got.query, got.body)
	}

	before := target.count()
	for params, want := range map[string]string{
		"&param_tenant=acme": "request.body needs param_service",
		"&param_service=x":   "needs param_tenant",
		"&param_tenant=acme&param_service=x&param_limit=ten":            "must be a number for its |number filter",
		"&param_tenant=" + url.QueryEscape("a\nb") + "&param_service=x": "contains a control character",
		"&param_tenant=acme&param_service=x&param_tenat=acme":           "param_tenat are not used by collector",
	} {
		r := probe(params)
		if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), want) {
			t.Errorf("%s: %d %q, want %q", params, r.Code, r.Body.String(), want)
		}
	}
	if target.count() != before {
		t.Fatal("the target was contacted for a probe that should have been refused")
	}

	// The body probe parameter replaces the templated body, so a parameter
	// only the body used has nothing to fill.
	r = probe("&param_tenant=acme&body=" + url.QueryEscape("{}"))
	if r.Code != http.StatusOK || target.last(t).body != "{}" {
		t.Fatalf("a body override: %d %q", r.Code, target.last(t).body)
	}
	r = probe("&param_tenant=acme&param_service=x&body=" + url.QueryEscape("{}"))
	if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "param_service are not used") {
		t.Fatalf("a body parameter beside a body override: %d %s", r.Code, r.Body.String())
	}
}

func TestTemplatesAreCheckedWhenTheConfigurationLoads(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(c *Collector)
		want   string
	}{
		"a placeholder in a header name": {func(c *Collector) { c.Request.Headers = map[string]string{"X-{{param_x}}": "1"} }, "placeholders stand only in header values"},
		"a placeholder in a query name":  {func(c *Collector) { c.Request.Query = map[string]string{"{{param_x}}": "1"} }, "placeholders stand only in query values"},
		"a filter in a header value":     {func(c *Collector) { c.Request.Headers = map[string]string{"X": "{{param_x|json}}"} }, "filters apply only in request.body"},
		"a malformed body placeholder":   {func(c *Collector) { c.Request.Body = "{{param_x" }, "request.body has an unclosed placeholder"},
	} {
		t.Run(name, func(t *testing.T) {
			c := testCollector("web", "text")
			tc.change(&c)
			cfg := &Config{Collectors: []Collector{c}}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

// Two probes differing only in a parameter never share a cached result.
func TestTemplatedProbesAreCachedApart(t *testing.T) {
	target := newEchoTarget(t)
	c := templatedCollector()
	c.Cache.TTL = Duration(time.Minute)
	server := pathServer(t, c)
	server.logger = quietLogger(t)
	for _, service := range []string{"a", "b", "a"} {
		r := httptest.NewRecorder()
		server.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/probe?collector=graphql&param_tenant=t&param_service="+service+"&target="+url.QueryEscape(target.server.URL), nil))
	}
	if target.count() != 2 {
		t.Fatalf("%d requests for two distinct services and one repeat, want 2", target.count())
	}
}

// A scheduled target fills the collector's placeholders from its params.
func TestScheduledTargetParams(t *testing.T) {
	target := newEchoTarget(t)
	cfg := &Config{Collectors: []Collector{templatedCollector()}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &TargetFile{Targets: []ScheduledTarget{
		{Name: "acme", Collector: "graphql", Target: target.server.URL, Params: map[string]string{"param_tenant": "acme", "param_service": "checkout"}},
	}}
	server := newScheduledServer(t, cfg, file)
	server.logger = quietLogger(t)
	server.scrapeScheduledTargets(context.Background(), 5*time.Second)
	got := target.last(t)
	if got.path != "/api/acme" || got.header.Get("X-Tenant") != "acme" || got.body != `{"service": "checkout", "limit": 10}` {
		t.Fatalf("request %+v", got)
	}

	for name, tc := range map[string]struct {
		params map[string]string
		want   string
	}{
		"a missing parameter": {map[string]string{"param_tenant": "acme"}, `whose request.body needs param_service, a parameter without a default; a scheduled target has no probe to supply it, so set it under the target's params`},
		"an unused parameter": {map[string]string{"param_tenant": "acme", "param_service": "x", "param_tenat": "y"}, `target "t" params param_tenat are not used by collector "graphql"`},
		"an unfit value":      {map[string]string{"param_tenant": "acme", "param_service": "x", "param_limit": "many"}, "must be a number"},
		"a bad name":          {map[string]string{"tenant": "acme"}, `params has "tenant", which is not a parameter name`},
	} {
		t.Run(name, func(t *testing.T) {
			f := &TargetFile{Targets: []ScheduledTarget{{Name: "t", Collector: "graphql", Target: target.server.URL, Params: tc.params}}}
			err := f.Validate()
			if err == nil {
				err = f.ValidateAgainst(cfg)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}

	// Params are part of the cache key, so two targets differing only in them
	// do not share a result.
	a := ScheduledTarget{Name: "a", Collector: "graphql", Target: "http://x", Params: map[string]string{"param_tenant": "a"}}
	b := ScheduledTarget{Name: "b", Collector: "graphql", Target: "http://x", Params: map[string]string{"param_tenant": "b"}}
	if a.cacheQuery().Encode() == b.cacheQuery().Encode() {
		t.Fatal("targets with different params share a cache key")
	}
}
