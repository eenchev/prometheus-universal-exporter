package transform

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// decodedBody decodes body with c's decoder and the Content-Type given.
func decodedBody(t *testing.T, c model.Collector, contentType, body string) (*decode.Decoded, *fetch.HTTPResponse) {
	t.Helper()
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{"Content-Type": {contentType}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	return d, r
}

// transformWith decodes body with c's decoder and transforms it with ctx,
// returning what the context's report gathered of its rules' failures too.
func transformWith(ctx context.Context, t *testing.T, c model.Collector, contentType, body string) (*model.MetricSet, []RuleFailure, error) {
	t.Helper()
	d, r := decodedBody(t, c, contentType, body)
	ctx, report := WithRuleReport(ctx)
	set, err := Transform(ctx, d, r, &c, "python3")
	return set, report.Failures(), err
}

// endless is a jq program that runs until its context ends.
const endless = "last(range(1e12))"

// A deadline that passes while a rule is being evaluated is not that rule's
// failure. Whatever the rule's error mode, the transform fails as a whole
// with the context's error, naming the rule it was at: nothing is answered
// of the rules that worked, nothing is counted or logged against the rule,
// and the failure is not a MetricFailure, which an error_mode of fail makes
// of a rule that could not read its value. Under ignore and log the rule was
// skipped, and the scrape passed with the series of the rule after it.
func TestADeadlineInARuleFailsTheTransform(t *testing.T) {
	for name, slow := range map[string]model.MetricRule{
		"value":        {Name: "slow", Expression: endless},
		"label":        {Name: "slow", Expression: ".a", Labels: []model.LabelRule{{Name: "l", Expression: endless}}},
		"items":        {Name: "slow", Items: ".a, " + endless, Expression: "."},
		"item's value": {Name: "slow", Items: ".a", Expression: endless},
		"item's label": {Name: "slow", Items: ".a", Expression: ".", Labels: []model.LabelRule{{Name: "l", Expression: endless}}},
	} {
		for _, mode := range []string{model.ErrorModeIgnore, model.ErrorModeLog, model.ErrorModeFail} {
			t.Run(name+" under "+mode, func(t *testing.T) {
				logs := testutil.CaptureLogs(t)
				slow.ErrorMode = mode
				c := model.Collector{Name: "slow", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
					Metrics: []model.MetricRule{slow, {Name: "fast", Expression: ".a", ErrorMode: mode}}}
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				set, failures, err := transformWith(ctx, t, c, "application/json", `{"a":1}`)
				if set != nil || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("series %+v, err %v; want the deadline's error and no series", set, err)
				}
				var failure *MetricFailure
				if errors.As(err, &failure) {
					t.Fatalf("the deadline is reported as the failure of rule %q: %v", failure.Metric, err)
				}
				if want := `the transform was stopped at metric "slow": context deadline exceeded`; err.Error() != want {
					t.Fatalf("err %q, want %q", err, want)
				}
				if len(failures) != 0 || logs.Len() != 0 {
					t.Fatalf("counted against the rules: %+v; logged: %s", failures, logs)
				}
			})
		}
	}
}

// A context cancelled because its caller went away is treated as a deadline
// is: a rule that fails once it is done fails the transform with the
// context's error, whatever made the rule fail.
func TestARuleFailingAfterCancellationFailsTheTransform(t *testing.T) {
	c := model.Collector{Name: "gone", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{Name: "absent", Expression: ".missing", ErrorMode: model.ErrorModeIgnore}, {Name: "fast", Expression: ".a"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	set, failures, err := transformWith(ctx, t, c, "application/json", `{"a":1}`)
	if set != nil || !errors.Is(err, context.Canceled) || len(failures) != 0 {
		t.Fatalf("series %+v, failures %+v, err %v", set, failures, err)
	}
	// With a live context the same rule is skipped and counted.
	set, failures, err = transformWith(context.Background(), t, c, "application/json", `{"a":1}`)
	if err != nil || len(set.Metrics) != 1 || len(failures) != 1 || failures[0].Metric != "absent" {
		t.Fatalf("series %+v, failures %+v, err %v", set, failures, err)
	}
}

// The transforms that walk a response themselves — the nodes an XPath
// selects, the items of a css rule, the rows of a CSV, the matches of a
// regex — ask after the context before each, so a response of many stops
// when the probe's deadline passes instead of being read to its end. A jq
// program stops by itself.
func TestTransformsStopWhenTheContextIsDone(t *testing.T) {
	for name, tc := range map[string]struct {
		collector         model.Collector
		contentType, body string
		want              string
	}{
		"xpath": {model.Collector{Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"},
			Metrics: []model.MetricRule{{Name: "n", Expression: "//v"}}},
			"application/xml", "<r><v>1</v><v>2</v></r>", `the transform was stopped at metric "n": context canceled`},
		"html xpath": {model.Collector{Decoder: model.DecoderConfig{Type: "html"}, Transform: model.TransformConfig{Type: "xpath"},
			Metrics: []model.MetricRule{{Name: "n", Expression: "//li"}}},
			"text/html", "<html><body><ul><li>1</li><li>2</li></ul></body></html>", `the transform was stopped at metric "n": context canceled`},
		"css items": {model.Collector{Decoder: model.DecoderConfig{Type: "html"}, Transform: model.TransformConfig{Type: "css"},
			Metrics: []model.MetricRule{{Name: "n", Items: "li", Expression: "b"}}},
			"text/html", "<html><body><ul><li><b>1</b></li><li><b>2</b></li></ul></body></html>", `the transform was stopped at metric "n": context canceled`},
		"csv": {model.Collector{Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"},
			Metrics: []model.MetricRule{{Name: "n", Expression: "v"}}},
			"text/csv", "v\n1\n2\n", "the transform was stopped before it finished: context canceled"},
		"regex": {model.Collector{Decoder: model.DecoderConfig{Type: "text"}, Transform: model.TransformConfig{Type: "regex"},
			Metrics: []model.MetricRule{{Name: "n", Expression: `(\d+)`}}},
			"text/plain", "1\n2\n", `the transform was stopped at metric "n": context canceled`},
	} {
		t.Run(name, func(t *testing.T) {
			c := tc.collector
			c.Name = "stopped"
			c.Metrics[0].ErrorMode = model.ErrorModeIgnore
			if set, _, err := transformWith(context.Background(), t, c, tc.contentType, tc.body); err != nil || len(set.Metrics) != 2 {
				t.Fatalf("with a live context: %+v, %v", set, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			set, failures, err := transformWith(ctx, t, c, tc.contentType, tc.body)
			if set != nil || !errors.Is(err, context.Canceled) || err.Error() != tc.want || len(failures) != 0 {
				t.Fatalf("series %+v, failures %+v, err %v; want %s", set, failures, err, tc.want)
			}
			var failure *MetricFailure
			if errors.As(err, &failure) {
				t.Fatalf("reported as the failure of rule %q", failure.Metric)
			}
		})
	}
}

// A rule that failed before the deadline is still counted when a later rule
// runs into it: what the report holds is what the rules themselves could not
// read.
func TestRuleFailuresBeforeTheDeadlineAreKept(t *testing.T) {
	testutil.CaptureLogs(t)
	c := model.Collector{Name: "mixed", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{Name: "absent", Expression: ".missing", ErrorMode: model.ErrorModeLog}, {Name: "slow", Expression: endless, ErrorMode: model.ErrorModeLog}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, failures, err := transformWith(ctx, t, c, "application/json", `{"a":1}`)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `"slow"`) {
		t.Fatalf("err %v", err)
	}
	if len(failures) != 1 || failures[0].Metric != "absent" || failures[0].Missing != 1 {
		t.Fatalf("failures %+v, want the one of the rule that failed on its own", failures)
	}
}
