package transform

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// greedy is a jq program that holds some hundred megabytes before it ends.
const greedy = "[range(5000000)] | length"

// A jq or yq transform that takes the exporter's memory past the collector's
// limits.max_expression_memory is stopped, wherever in a rule the program
// is, and whatever the rule's error mode: the transform fails as a whole,
// as a limit exceeded, naming the rule and the limit, and nothing is counted
// or logged against the rule. Without the limit the same program runs to
// its end.
func TestATransformPastItsExpressionMemoryIsStopped(t *testing.T) {
	const limit = 8 << 20
	for name, rule := range map[string]model.MetricRule{
		"value":        {Name: "greedy", Expression: greedy},
		"label":        {Name: "greedy", Expression: ".a", Labels: []model.LabelRule{{Name: "l", Expression: greedy}}},
		"items":        {Name: "greedy", Items: "[range(5000000)] | .[0]", Expression: "."},
		"item's value": {Name: "greedy", Items: ".a", Expression: greedy},
		"item's label": {Name: "greedy", Items: ".a", Expression: ".", Labels: []model.LabelRule{{Name: "l", Expression: greedy}}},
	} {
		for _, transform := range []string{"jq", "yq"} {
			for _, mode := range []string{model.ErrorModeIgnore, model.ErrorModeFail} {
				if transform == "yq" && (name != "value" || mode != model.ErrorModeFail) {
					continue
				}
				t.Run(name+" of "+transform+" under "+mode, func(t *testing.T) {
					logs := testutil.CaptureLogs(t)
					rule.ErrorMode = mode
					c := model.Collector{Name: "c", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: transform},
						Limits:  model.Limits{MaxExpressionMemory: limit},
						Metrics: []model.MetricRule{{Name: "first", Expression: ".a"}, rule}}
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					set, failures, err := transformWith(ctx, t, c, "application/json", `{"a":1}`)
					if set != nil || !errors.Is(err, model.ErrLimitExceeded) {
						t.Fatalf("series %+v, err %v; want a limit exceeded and no series", set, err)
					}
					if want := `the transform was stopped at metric "greedy": the exporter's memory grew by more than limits.max_expression_memory, 8388608 bytes, while it ran; make the expression keep less at once, or raise the limit`; err.Error() != want {
						t.Fatalf("err %q, want %q", err, want)
					}
					var failure *MetricFailure
					if errors.As(err, &failure) || len(failures) != 0 || logs.Len() != 0 {
						t.Fatalf("counted against the rule: %v, %+v; logged: %s", failure, failures, logs)
					}
				})
			}
		}
	}

	// The limit is what a transform may hold, not what it may make: one that
	// makes several times the limit of garbage, holding none of it, runs to
	// its end. So does the greedy one whose collector has no limit.
	// Under the race detector, where a program runs ten times as long, the
	// two are a tenth of what they are.
	garbage, unbounded := "last(range(5000000))", greedy
	if raceDetector {
		garbage, unbounded = "last(range(500000))", "[range(500000)] | length"
	}
	for name, c := range map[string]model.Collector{
		"garbage":  {Limits: model.Limits{MaxExpressionMemory: 16 << 20}, Metrics: []model.MetricRule{{Name: "n", Expression: garbage}}},
		"no limit": {Metrics: []model.MetricRule{{Name: "n", Expression: unbounded}}},
	} {
		c.Name, c.Decoder.Type, c.Transform.Type = "c", "json", "jq"
		set, failures, err := transformWith(context.Background(), t, c, "application/json", `{"a":1}`)
		if err != nil || len(failures) != 0 || set == nil || len(set.Metrics) != 1 {
			t.Errorf("%s: series %+v, failures %+v, err %v; want the one series", name, set, failures, err)
		}
	}
}

// A transform that ends before its memory was looked at is left alone, and
// so is one whose time ran out: that is still the deadline's failure.
func TestTheExpressionMemoryWatchLeavesOtherTransformsAsTheyWere(t *testing.T) {
	c := model.Collector{Name: "c", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Limits: model.Limits{MaxExpressionMemory: 1 << 20}, Metrics: []model.MetricRule{{Name: "n", Expression: ".a"}}}
	for range 50 {
		if set, _, err := transformWith(context.Background(), t, c, "application/json", `{"a":1}`); err != nil || set == nil {
			t.Fatalf("series %+v, err %v", set, err)
		}
	}
	c.Metrics[0].Expression = endless
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.Limits.MaxExpressionMemory = 1 << 30
	if _, _, err := transformWith(ctx, t, c, "application/json", `{"a":1}`); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("err %v, want the deadline's", err)
	}
}
