package transform

import (
	"errors"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A pre-script of a prometheus transform that left a series' help, type or
// labels as something a series does not have there — a number for the help,
// a list for the type, a list of pairs for the labels — had it read as none
// given: the series was exported without its help, untyped, or without its
// labels, which is another series than the one the script meant and may be
// one the scrape already has. Each fails the pre-script now, as a name, a
// value or a timestamp of another kind always did, naming the series, the
// key, what stands there and what belongs there. None, and a key that is
// not there, are none given, as they were, and so is a type or a help of "".
func TestAPrometheusPreScriptSeriesWithAHelpTypeOrLabelsOfAnotherKindFails(t *testing.T) {
	const types = `; give "gauge", "counter", "untyped", "histogram" or "summary"`
	const mapping = ", not a mapping of label names to values"
	for name, tc := range map[string]struct {
		key   string
		value any
		want  string
	}{
		"a number for the help":          {"help", 5, "made help 5 is not a string"},
		"a float for the help":           {"help", 0.5, "made help 0.5 is not a string"},
		"a boolean for the help":         {"help", true, "made help true is not a string"},
		"a list for the help":            {"help", []any{"a", "b", "c"}, "made help an array of 3 items is not a string"},
		"a dict for the help":            {"help", map[string]any{"en": "Help."}, "made help an object with 1 key is not a string"},
		"a list for the type":            {"type", []any{"counter"}, "made type an array of 1 item is not a string" + types},
		"a number for the type":          {"type", 5, "made type 5 is not a string" + types},
		"a boolean for the type":         {"type", false, "made type false is not a string" + types},
		"a dict for the type":            {"type", map[string]any{}, "made type an object with 0 keys is not a string" + types},
		"a list of pairs for the labels": {"labels", []any{[]any{"a", "b"}}, "made labels are an array of 1 item" + mapping},
		"an empty list for the labels":   {"labels", []any{}, "made labels are an array of 0 items" + mapping},
		"text for the labels":            {"labels", "a=b", `made labels are "a=b"` + mapping},
		"a number for the labels":        {"labels", 5, "made labels are 5" + mapping},
		"a boolean for the labels":       {"labels", true, "made labels are true" + mapping},
		"None for the help":              {"help", nil, ""},
		"None for the type":              {"type", nil, ""},
		"None for the labels":            {"labels", nil, ""},
		"an empty help":                  {"help", "", ""},
		"an empty type":                  {"type", "", ""},
		"no labels":                      {"labels", map[string]any{}, ""},
	} {
		series := map[string]any{"name": "made", "type": "gauge", "help": "Made.", "labels": map[string]any{"a": "b"}, "value": 2.0}
		series[tc.key] = tc.value
		set, err := prometheusFromPython(map[string]any{"metrics": []any{map[string]any{"name": "g", "value": 1.0}, series}})
		if tc.want != "" {
			if want := `data["metrics"][1]: ` + tc.want; err == nil || err.Error() != want {
				t.Errorf("%s: err=%v, want %q", name, err, want)
			}
			continue
		}
		if err != nil || len(set.Metrics) != 2 {
			t.Errorf("%s: %v, %d series", name, err, len(set.Metrics))
			continue
		}
		// What the key was left as none of is none given: an untyped series,
		// no help, no labels; the other two are as the script left them.
		want := model.Metric{Name: "made", Type: model.GaugeMetricType, Help: "Made.", Labels: map[string]string{"a": "b"}, Value: 2}
		switch tc.key {
		case "help":
			want.Help = ""
		case "type":
			want.Type = model.UntypedMetricType
		case "labels":
			want.Labels = nil
		}
		if got := set.Metrics[1]; seriesText(got) != seriesText(want) {
			t.Errorf("%s: read as %s, want %s", name, seriesText(got), seriesText(want))
		}
	}
	// Without the key the series is as with None for it.
	set, err := prometheusFromPython(map[string]any{"metrics": []any{map[string]any{"name": "made", "value": 2.0}}})
	if want := (model.Metric{Name: "made", Type: model.UntypedMetricType, Value: 2}); err != nil || len(set.Metrics) != 1 || seriesText(set.Metrics[0]) != seriesText(want) {
		t.Errorf("a series of a name and a value: %v, %+v", err, set.Metrics)
	}
}

// The first of them is the one reported, in the order type, help, labels,
// after a name that is none and before a label, a timestamp or a value that
// is wrong: one series has one failure, the same on every scrape.
func TestAPrometheusPreScriptSeriesReportsItsFirstKeyOfAnotherKind(t *testing.T) {
	for want, series := range map[string]map[string]any{
		"has no name": {"name": 5, "type": 5, "help": 5, "labels": 5, "value": "x"},
		`made type 5 is not a string; give "gauge", "counter", "untyped", "histogram" or "summary"`: {"name": "made", "type": 5, "help": 5, "labels": 5, "value": "x"},
		"made help 5 is not a string":                                                                         {"name": "made", "type": "gauge", "help": 5, "labels": 5, "value": "x"},
		"made labels are 5, not a mapping of label names to values":                                           {"name": "made", "help": "Made.", "labels": 5, "timestamp": "soon", "value": "x"},
		`made help an array of 2 items is not a string`:                                                       {"name": "made", "help": []any{1, 2}, "labels": map[string]any{"l": []any{1}}, "value": 1.0},
		`made label "l" is an array of 1 values, not a single value; select one, or join them with join(",")`: {"name": "made", "help": "Made.", "labels": map[string]any{"l": []any{1}}, "timestamp": "soon", "value": 1.0},
	} {
		_, err := prometheusFromPython(map[string]any{"metrics": []any{series}})
		if err == nil || err.Error() != `data["metrics"][0]: `+want {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
}

// Through a transform, with a script: each of the three fails the scrape as
// the script's failure, as a timestamp of another kind does, and a script
// that leaves None there, or leaves the three as it was given them, is read
// back. A type that is text and is no type is read as written, as it was,
// and is refused where every series is checked, as a rule's or a metric(...)'s
// is: it is not exported.
func TestAPrometheusPreScriptLeavingAHelpTypeOrLabelsOfAnotherKindFailsAsAScript(t *testing.T) {
	requirePython(t)
	const body = "# HELP up_thing Help.\n# TYPE up_thing gauge\nup_thing{a=\"b\"} 1\n"
	c := model.Collector{Name: "pre", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(), Transform: model.TransformConfig{Type: "prometheus"}}
	for script, want := range map[string]string{
		`data["metrics"][0]["help"] = 5`:                                             `python pre-script: data["metrics"][0]: up_thing help 5 is not a string`,
		`data["metrics"][0]["type"] = ["counter"]`:                                   `python pre-script: data["metrics"][0]: up_thing type an array of 1 item is not a string; give "gauge", "counter", "untyped", "histogram" or "summary"`,
		`data["metrics"][0]["labels"] = [["a", "b"]]`:                                `python pre-script: data["metrics"][0]: up_thing labels are an array of 1 item, not a mapping of label names to values`,
		`data["metrics"][0]["labels"] = ("a", "b")`:                                  `python pre-script: data["metrics"][0]: up_thing labels are an array of 2 items, not a mapping of label names to values`,
		`data["metrics"][0]["timestamp"] = [1, 2, 3]`:                                `python pre-script: data["metrics"][0]: up_thing timestamp an array of 3 items is not a number of milliseconds`,
		`data["metrics"].append({"name": "made", "value": 1, "help": float("nan")})`: `python pre-script: data["metrics"][1]: made help NaN is not a string`,
	} {
		c.Transform.PreScript = script
		_, err := runBody(t, c, "text/plain", body)
		if err == nil || err.Error() != want || !errors.Is(err, model.ErrScriptFailed) {
			t.Errorf("%s: err=%v (a script's failure: %v), want %q", script, err, errors.Is(err, model.ErrScriptFailed), want)
		}
	}
	for script, want := range map[string]model.Metric{
		`data = data`: {Name: "up_thing", Type: model.GaugeMetricType, Help: "Help.", Labels: map[string]string{"a": "b"}, Value: 1},
		`data["metrics"][0]["help"] = None; data["metrics"][0]["type"] = None; data["metrics"][0]["labels"] = None`: {Name: "up_thing", Type: model.UntypedMetricType, Value: 1},
		`del data["metrics"][0]["help"], data["metrics"][0]["type"], data["metrics"][0]["labels"]`:                  {Name: "up_thing", Type: model.UntypedMetricType, Value: 1},
		`data["metrics"][0]["type"] = "counterr"`:                                                                   {Name: "up_thing", Type: "counterr", Help: "Help.", Labels: map[string]string{"a": "b"}, Value: 1},
	} {
		c.Transform.PreScript = script
		set, err := runBody(t, c, "text/plain", body)
		if err != nil || len(set.Metrics) != 1 || seriesText(set.Metrics[0]) != seriesText(want) {
			t.Errorf("%s: %v, %+v, want %s", script, err, set, seriesText(want))
			continue
		}
		err = set.Validate(model.Limits{})
		if strings.Contains(script, "counterr") != (err != nil) || err != nil && err.Error() != `metric "up_thing" has invalid type "counterr"` {
			t.Errorf("%s: the series is checked as %v", script, err)
		}
	}
}

// An entry a python transform appends to metrics by hand always failed for
// the three: the transform's own answer is read by pythonMetric.metric,
// which names the metric and the key.
func TestAPythonTransformEntryWithAHelpTypeOrLabelsOfAnotherKindFails(t *testing.T) {
	requirePython(t)
	for script, want := range map[string]string{
		`metrics.append({"name": "m", "value": 1, "help": 5})`:              `python transform: metric "m" help 5 is not a string`,
		`metrics.append({"name": "m", "value": 1, "type": ["counter"]})`:    `python transform: metric "m" type an array of 1 item is not a string; give "gauge", "counter" or "untyped"`,
		`metrics.append({"name": "m", "value": 1, "labels": [["a", "b"]]})`: `python transform: metric "m" labels are an array of 1 item, not a mapping of label names to values`,
	} {
		_, err := runWorkerScript(t, workerCollector("kinds", script))
		if err == nil || err.Error() != want || !errors.Is(err, model.ErrScriptFailed) {
			t.Errorf("%s: err=%v, want %q as a script's failure", script, err, want)
		}
	}
}
