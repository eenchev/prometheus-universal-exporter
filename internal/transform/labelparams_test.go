package transform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// Placeholders in a collector's fixed label values, where the transform
// reads them filled (labelparams.go).

// transformBeforeLabelParams is Transform as it was before a label value
// took placeholders.
func transformBeforeLabelParams(ctx context.Context, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector, pythonPath string) (*model.MetricSet, error) {
	report, _ := ctx.Value(ruleReportKey{}).(*RuleReport)
	ctx = withResponseVariables(ctx, r)
	ctx = withSeriesBudget(ctx, c.Limits.MaxMetrics)
	ctx, failures := withRuleFailures(ctx)
	defer failures.finish(c, report)
	set, borrowed, err := transformMetrics(ctx, d, r, c, pythonPath)
	var failure *MetricFailure
	if errors.As(err, &failure) && ctx.Err() != nil {
		return nil, interruptedAt(ctx, failure.Metric)
	}
	if err != nil || set == nil {
		return set, err
	}
	if borrowed && needsUTF8Repair(set) {
		set.Metrics, borrowed = slices.Clone(set.Metrics), false
	}
	if !borrowed {
		changed, first := model.SanitizeUTF8(set)
		if report != nil {
			report.addUTF8(changed, first)
		}
	}
	mapLabelValues(set, c)
	truncateLabels(set, c)
	applyCollectorLabels(set, c.Transform)
	applyMetricsPrefix(set, c.MetricsPrefix)
	if err := escapeNames(set, c.NameEscaping); err != nil {
		return nil, err
	}
	return set, nil
}

// labelFixtureRules are, for each directory of the repository's fixtures,
// the decoder its files are read with and a rule that reads something of
// any document of the kind, under each transform that reads it.
var labelFixtureRules = []struct {
	directory, decoder, transform string
	rule                          model.MetricRule
}{
	{"json", "json", "jq", model.MetricRule{Name: "numbers", Expression: "[.. | numbers] | length"}},
	{"json", "json", "jq", model.MetricRule{Name: "values", Items: "[.. | numbers][:5][]", Expression: "."}},
	{"yaml", "yaml", "yq", model.MetricRule{Name: "numbers", Expression: "[.. | numbers] | length"}},
	{"xml", "xml", "xpath", model.MetricRule{Name: "elements", Expression: "count(//*)"}},
	{"html", "html", "xpath", model.MetricRule{Name: "cells", Expression: "count(//td)"}},
	{"html", "html", "css", model.MetricRule{Name: "cell", Expression: "td", ErrorMode: model.ErrorModeIgnore}},
	{"text", "text", "regex", model.MetricRule{Name: "number", Expression: `(\d+)`, ErrorMode: model.ErrorModeIgnore}},
	{"csv", "csv", "csv", model.MetricRule{Name: "first", Expression: "1", ErrorMode: model.ErrorModeIgnore}},
	{"prometheus", "prometheus", "prometheus", model.MetricRule{Expression: ".*", ErrorMode: model.ErrorModeIgnore}},
	{"prometheus", "prometheus", "prometheus", model.MetricRule{}},
}

// labelSettings are what a collector says of its labels beside its rules,
// each a change to a collector and to its one rule.
var labelSettings = map[string]func(c *model.Collector, rule *model.MetricRule){
	"no labels": func(*model.Collector, *model.MetricRule) {},
	"a static label": func(_ *model.Collector, rule *model.MetricRule) {
		rule.Labels = []model.LabelRule{{Name: "source", Value: "api"}}
	},
	"braces of a value's own": func(c *model.Collector, rule *model.MetricRule) {
		rule.Labels = []model.LabelRule{{Name: "source", Value: "{{x}} {{param_tenant}} {{param_region:eu}}"}}
		c.Transform.Labels = map[string]string{"tenant": "{{param_tenant}}", "empty": ""}
	},
	"transform.labels": func(c *model.Collector, _ *model.MetricRule) {
		c.Transform.Labels = map[string]string{"tenant": "acme", "job": "replaced", "empty": ""}
	},
	"removed and renamed": func(c *model.Collector, rule *model.MetricRule) {
		rule.Labels = []model.LabelRule{{Name: "source", Value: "api"}, {Name: "gone", Value: "x"}}
		c.Transform.Labels = map[string]string{"tenant": "acme"}
		c.Transform.RemoveLabels = []string{"gone"}
		c.Transform.RenameLabels = map[string]string{"tenant": "customer", "source": "origin"}
	},
	"truncated": func(c *model.Collector, rule *model.MetricRule) {
		rule.Labels = []model.LabelRule{{Name: "source", Value: "a-long-static-value", Truncate: true}}
		c.Limits.MaxLabelValueLength = 8
	},
	"a prefix and escaped names": func(c *model.Collector, rule *model.MetricRule) {
		rule.Labels = []model.LabelRule{{Name: "source.name", Value: "api"}}
		c.Transform.Labels = map[string]string{"tenant.id": "acme"}
		c.MetricsPrefix, c.NameEscaping = "app", NameEscapingValues
	},
}

// A collector whose label values hold no placeholder is transformed as it
// was. Every file of the repository's fixtures, and every third under the
// race detector, is read by a rule of each
// transform that reads its kind, under each setting that gives, removes,
// renames, cuts or escapes a label, and by a prometheus pass-through; the
// transform is put beside a copy of itself as it was, with a probe's
// parameters in the context and without, and gives the same series, the
// same error and the same report of its rules, byte for byte as they are
// written. A value that reads like a placeholder and was not read as one,
// as the configuration's are, is the text it is.
func TestACollectorWithoutLabelPlaceholdersIsTransformedAsItWas(t *testing.T) {
	// Every file of each kind, and every third under the race detector,
	// the first of each kind among them.
	every := alloctest.UnlessRaced(1, 3)
	compared, series := 0, 0
	for _, fixture := range labelFixtureRules {
		files, err := filepath.Glob("../../testdata/" + fixture.directory + "/*")
		if err != nil || len(files) == 0 {
			t.Fatalf("no fixtures of %s: %v", fixture.directory, err)
		}
		for at, file := range files {
			info, err := os.Stat(file)
			if err != nil || info.IsDir() || at%every != 0 {
				continue
			}
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for name, setting := range labelSettings {
				c := model.Collector{Name: "fixture", Decoder: model.DecoderConfig{Type: fixture.decoder}, Transform: model.TransformConfig{Type: fixture.transform}, Limits: model.Limits{MaxMetrics: 100000}}
				rule := fixture.rule
				setting(&c, &rule)
				if rule.Name != "" || rule.Expression != "" {
					if rule.Type == "" && fixture.transform != "prometheus" {
						rule.Type = model.GaugeMetricType
					}
					if rule.ErrorMode == "" {
						rule.ErrorMode = model.ErrorModeLog
					}
					c.Metrics = []model.MetricRule{rule}
				}
				r := &fetch.HTTPResponse{Body: body, Headers: http.Header{}}
				d, err := decode.Decode(r, &c)
				if err != nil {
					// A fixture its decoder does not read as it is, such as
					// one in another character set, has nothing to transform.
					continue
				}
				for _, params := range []map[string]string{nil, {"param_tenant": "acme", "param_region": "us"}} {
					run := func(transform func(context.Context, *decode.Decoded, *fetch.HTTPResponse, *model.Collector, string) (*model.MetricSet, error)) (string, string, []RuleFailure) {
						ctx := LeaveRuleLoggingToCaller(context.Background())
						if params != nil {
							ctx = WithLabelParams(ctx, params)
						}
						ctx, report := WithRuleReport(ctx)
						set, err := transform(ctx, d, r, &c, "")
						if set != nil {
							series += len(set.Metrics)
						}
						return fmt.Sprintf("%#v", set), fmt.Sprint(err), report.Failures()
					}
					now, nowErr, nowFailures := run(Transform)
					was, wasErr, wasFailures := run(transformBeforeLabelParams)
					if now != was || nowErr != wasErr || fmt.Sprint(nowFailures) != fmt.Sprint(wasFailures) {
						t.Errorf("%s, %s, %s, parameters %v:\n now %s, %s, %v\n was %s, %s, %v", file, fixture.transform, name, params, now, nowErr, nowFailures, was, wasErr, wasFailures)
					}
					compared++
				}
			}
		}
	}
	// Each comparison makes the series twice.
	if compared < alloctest.UnlessRaced(1000, 300) || series < alloctest.UnlessRaced(4000, 1000) {
		t.Fatalf("%d transforms compared, of %d series", compared, series)
	}
}

// labelledCollector is a jq collector over {"up": 1} whose transform.labels
// and whose rule's label hold placeholders, read as the configuration reads
// them, and the decoded response.
func labelledCollector(t *testing.T) (*model.Collector, *decode.Decoded, *fetch.HTTPResponse) {
	t.Helper()
	c := &model.Collector{
		Name:      "tenants",
		Decoder:   model.DecoderConfig{Type: "json"},
		Transform: model.TransformConfig{Type: "jq", Labels: map[string]string{"tenant": "{{param_tenant}}", "region": "{{param_region:eu}}"}},
		Metrics:   []model.MetricRule{{Name: "status_up", Type: model.GaugeMetricType, Expression: ".up", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "source", Value: "api-{{param_tenant}}"}}}},
		Limits:    model.Limits{MaxMetrics: 100},
	}
	labels, err := fetch.ParseLabelParams(c, func(index int) string { return RuleName(&c.Metrics[index], index) })
	if err != nil || labels == nil {
		t.Fatalf("the label values were read as %+v, %v", labels, err)
	}
	c.LabelParams = labels
	r := &fetch.HTTPResponse{Body: []byte(`{"up": 1}`), Headers: http.Header{}}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	return c, d, r
}

// Transform reads a collector whose label values hold placeholders with
// them filled in from the parameters its context carries: a context that
// carries none fills each with its default, and fails the transform, naming
// the parameter, where one has none. The collector, which the probes share,
// reads afterwards as it did.
func TestTransformFillsTheLabelValuesFromItsContext(t *testing.T) {
	c, d, r := labelledCollector(t)
	written := fmt.Sprintf("%#v", *c)
	set, err := Transform(WithLabelParams(context.Background(), map[string]string{"param_tenant": "acme"}), d, r, c, "")
	if err != nil || len(set.Metrics) != 1 || !reflect.DeepEqual(set.Metrics[0].Labels, map[string]string{"tenant": "acme", "region": "eu", "source": "api-acme"}) {
		t.Fatalf("the series: %+v, %v", set, err)
	}
	set, err = Transform(WithLabelParams(context.Background(), map[string]string{"param_tenant": "globex", "param_region": "us"}), d, r, c, "")
	if err != nil || len(set.Metrics) != 1 || !reflect.DeepEqual(set.Metrics[0].Labels, map[string]string{"tenant": "globex", "region": "us", "source": "api-globex"}) {
		t.Fatalf("the series of another probe: %+v, %v", set, err)
	}
	var missing *fetch.MissingParamError
	if _, err := Transform(context.Background(), d, r, c, ""); !errors.As(err, &missing) || missing.Name != "param_tenant" || missing.Where != "transform.labels.tenant" {
		t.Fatalf("without the parameters: %v", err)
	}
	if now := fmt.Sprintf("%#v", *c); now != written {
		t.Fatalf("the collector reads\n%s\nand was written\n%s", now, written)
	}
}

// What a transform costs a collector whose label values hold no
// placeholder is what it cost: as many allocations as the transform as it
// was makes, with a probe's parameters in the context or without. One whose
// labels hold placeholders pays for its copy of the collector and no more
// for each series.
func TestATransformWithoutLabelPlaceholdersAllocatesWhatItDid(t *testing.T) {
	c, d, r := labelledCollector(t)
	plain := *c
	plain.LabelParams = nil
	plain.Transform.Labels = map[string]string{"tenant": "acme", "region": "eu"}
	plain.Metrics = []model.MetricRule{{Name: "status_up", Type: model.GaugeMetricType, Expression: ".up", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "source", Value: "api-acme"}}}}
	for name, ctx := range map[string]context.Context{
		"no parameters": context.Background(),
		"parameters":    WithLabelParams(context.Background(), map[string]string{"param_tenant": "acme"}),
	} {
		if _, err := Transform(ctx, d, r, &plain, ""); err != nil {
			t.Fatal(err)
		}
		// No more than before: under the race detector a count is seen to
		// be one more in some measurements than in others, so there the
		// new one may be one over the least seen of the old one.
		before, _ := alloctest.Allocations(100, func() { _, _ = transformBeforeLabelParams(ctx, d, r, &plain, "") })
		most := before + alloctest.UnlessRaced(0.0, 1.0)
		if now := alloctest.AllocsAtMost(100, most, func() { _, _ = Transform(ctx, d, r, &plain, "") }); now > most {
			t.Errorf("%s: a transform allocates %.0f times, and before %.0f", name, now, before)
		}
	}
	ctx := WithLabelParams(context.Background(), map[string]string{"param_tenant": "acme"})
	written, _ := alloctest.Allocations(100, func() { _, _ = Transform(ctx, d, r, &plain, "") })
	// The copy of the collector, its transform.labels, its rules, the
	// labels of the rule, and the text of the one value that is more than a
	// placeholder.
	if filled := alloctest.AllocsAtMost(100, written+7, func() { _, _ = Transform(ctx, d, r, c, "") }); filled > written+7 {
		t.Errorf("a transform that fills its labels allocates %.0f times, and one with the values written %.0f", filled, written)
	}
}
