package config

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A prometheus rule without a type keeps the type of what it passes through,
// so none is filled in; every other rule is a gauge unless it says otherwise,
// and cannot be a histogram or a summary, which only a series that is one
// has the buckets or quantiles of.
func TestRuleTypes(t *testing.T) {
	prom := model.Collector{Name: "p", Request: model.RequestConfig{Type: "http"}, Transform: model.TransformConfig{Type: "prometheus"},
		Metrics: []model.MetricRule{{Expression: "^a"}, {Expression: "^h", Type: model.HistogramMetricType}}}
	cfg := &model.Config{Collectors: []model.Collector{prom}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Metrics[0].Type; got != "" {
		t.Fatalf("a prometheus rule without a type became %q", got)
	}
	jq := testutil.Collector("j", "json")
	jq.Transform.Type = "jq"
	jq.Metrics = []model.MetricRule{{Name: "v", Expression: ".v"}}
	cfg = &model.Config{Collectors: []model.Collector{jq}}
	if err := Validate(cfg); err != nil || cfg.Collectors[0].Metrics[0].Type != model.GaugeMetricType {
		t.Fatalf("err=%v type=%q", err, cfg.Collectors[0].Metrics[0].Type)
	}
	for _, kind := range []model.MetricType{model.HistogramMetricType, model.SummaryMetricType} {
		jq.Metrics = []model.MetricRule{{Name: "v", Expression: ".v", Type: kind}}
		err := Validate(&model.Config{Collectors: []model.Collector{jq}})
		if err == nil || !strings.Contains(err.Error(), `metric "v" has type `+string(kind)+`, which only a prometheus transform can give`) {
			t.Errorf("%s: err=%v", kind, err)
		}
	}
}

// Without a header row, a csv rule and its labels name columns by number.
func TestCSVColumnsWithoutAHeader(t *testing.T) {
	header := false
	c := testutil.Collector("c", "csv")
	c.Transform.Type = "csv"
	c.Response.CSV.Header = &header
	c.Metrics = []model.MetricRule{{Name: "v", Expression: "2", Labels: []model.LabelRule{{Name: "host", Expression: "1"}, {Name: "env", Value: "prod"}}}}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}
	for _, rule := range []model.MetricRule{
		{Name: "v", Expression: "cpu"},
		{Name: "v", Expression: "0"},
		{Name: "v", Expression: "02"},
		{Name: "v", Expression: "2", Labels: []model.LabelRule{{Name: "host", Expression: "server"}}},
	} {
		c.Metrics = []model.MetricRule{rule}
		if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err == nil || !strings.Contains(err.Error(), "response.csv.header is false, so columns are named by number, from 1") {
			t.Errorf("%+v: err=%v", rule, err)
		}
	}
}

// A request path with a query or a fragment, and a header value with a
// control character, are refused at load, in a collector and a static
// target; so is a static target whose scheme the collector does not allow.
func TestRequestValuesCheckedAtLoad(t *testing.T) {
	for name, test := range map[string]struct {
		edit func(*model.Collector)
		want string
	}{
		"a query in the path":      {func(c *model.Collector) { c.Request.Path = "/status?format=json" }, `request.path "/status?format=json" has a ? in it`},
		"a fragment in the path":   {func(c *model.Collector) { c.Request.Path = "/status#top" }, "has a # in it"},
		"a line break in a header": {func(c *model.Collector) { c.Request.Headers = map[string]string{"X-A": "a\r\nX-B: b"} }, `request.headers X-A has the control character '\r'`},
	} {
		c := testutil.Collector("web", "text")
		test.edit(&c)
		if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err=%v, want %q", name, err, test.want)
		}
	}
	c := testutil.Collector("web", "text")
	c.Request.Headers = map[string]string{"X-Tab": "a\tb"}
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		target model.StaticTarget
		want   string
	}{
		"a query in the path": {model.StaticTarget{Name: "t", Collector: "web", Target: "http://a.invalid", Request: model.TargetRequestConfig{Path: "/s?x=1", PathSet: true}}, `target "t" request.path "/s?x=1" has a ? in it`},
		"a control character": {model.StaticTarget{Name: "t", Collector: "web", Target: "http://a.invalid", Request: model.TargetRequestConfig{Headers: map[string]string{"X-A": "a\nb"}}}, `target "t" request.headers X-A has the control character`},
		"an ftp target":       {model.StaticTarget{Name: "t", Collector: "web", Target: "ftp://a.invalid/x"}, `target "t": target scheme "ftp" is not allowed; request.allowed_schemes allows http, https`},
	} {
		file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{test.target}}
		err := ValidateStaticTargets(file)
		if err == nil {
			err = ValidateStaticTargetsAgainst(file, cfg)
		}
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err=%v, want %q", name, err, test.want)
		}
	}
}
