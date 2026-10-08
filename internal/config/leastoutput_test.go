package config

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A limits.max_output_bytes that is set is at least the answer of a Python
// script that emits no metric (transform.MinPythonOutputBytes). These tests
// hold the check of the limits to what it was for every other limit, and the
// schemas to the same least as the loader.

// limitsAsTheyWereChecked is checkLimits as it was before an output limit
// had a least: a negative limit was all it refused.
func limitsAsTheyWereChecked(x *model.Collector) error {
	l := x.Limits
	for _, limit := range []struct {
		key   string
		value int64
	}{
		{"max_metrics", int64(l.MaxMetrics)},
		{"max_labels_per_metric", int64(l.MaxLabelsPerMetric)},
		{"max_label_value_length", int64(l.MaxLabelValueLength)},
		{"max_metric_name_length", int64(l.MaxMetricNameLength)},
		{"max_help_length", int64(l.MaxHelpLength)},
		{"max_output_bytes", int64(l.MaxOutputBytes)},
		{"max_cache_entries", int64(l.MaxCacheEntries)},
	} {
		if limit.value < 0 {
			return fmt.Errorf("collector %q limits.%s is %d, and a limit must not be negative; leave it out, or 0, for the default", x.Name, limit.key, limit.value)
		}
	}
	if l.ScriptTimeout < 0 {
		return fmt.Errorf("collector %q limits.script_timeout is %s, and a timeout must not be negative; leave it out for the default, 100ms", x.Name, time.Duration(l.ScriptTimeout))
	}
	return nil
}

// The check of a collector's limits says what it said of every set of
// limits but one that passed with an output limit from 1 to 37 bytes, which
// it now refuses naming the collector, the key, the value, the least and the
// default: over every output limit from -3 to 45 and some large ones, beside
// limits of every other key that are left out, set, and negative, with a
// negative limit and a negative timeout still refused first, in their own
// words.
func TestTheCheckOfTheLimitsIsWhatItWasButForAnOutputLimitUnderTheLeast(t *testing.T) {
	const least = transform.MinPythonOutputBytes
	outputs := []model.ByteSize{1 << 10, 1 << 20, 64 << 20, math.MaxInt32, math.MaxInt64, math.MinInt64}
	for size := model.ByteSize(-3); size <= 45; size++ {
		outputs = append(outputs, size)
	}
	others := []model.Limits{
		{},
		{MaxMetrics: 5, MaxLabelsPerMetric: 3, MaxLabelValueLength: 7, MaxMetricNameLength: 9, MaxHelpLength: 11, MaxCacheEntries: 13, ScriptTimeout: model.Duration(time.Second), MaxResponseBytes: 1, MaxScriptMemory: 1},
		{MaxMetrics: -1}, {MaxLabelsPerMetric: -1}, {MaxLabelValueLength: -1}, {MaxMetricNameLength: -1}, {MaxHelpLength: -1},
		{MaxCacheEntries: -1}, {ScriptTimeout: -1}, {MaxCacheEntries: -2, ScriptTimeout: -1},
	}
	refused, same := 0, 0
	for _, size := range outputs {
		for i := range others {
			x := &model.Collector{Name: "sized", Limits: others[i]}
			x.Limits.MaxOutputBytes = size
			was, now := limitsAsTheyWereChecked(x), checkLimits(x)
			if was == nil && size > 0 && size < model.ByteSize(least) {
				refused++
				if want := fmt.Sprintf(`collector "sized" limits.max_output_bytes is %d, and a Python script that emits no metric answers in 38 bytes, so no transform's script could answer within it; set at least 38, or leave it out, or 0, for the default, 1MiB`, size); now == nil || now.Error() != want {
					t.Errorf("an output limit of %d beside %+v: %v, want %s", size, others[i], now, want)
				}
				continue
			}
			same++
			if (was == nil) != (now == nil) || was != nil && was.Error() != now.Error() {
				t.Errorf("an output limit of %d beside %+v: the check says %v, and said %v", size, others[i], now, was)
			}
		}
	}
	// 37 sizes beside the two sets of limits that pass.
	if refused != 2*(least-1) || same != len(outputs)*len(others)-refused {
		t.Errorf("%d sets of limits were refused for their output limit and %d checked as they were; want %d and %d", refused, same, 2*(least-1), len(outputs)*len(others)-2*(least-1))
	}
}

// The schemas of the configuration and of a collector file say the least of
// limits.max_output_bytes as the loader has it: beside what a size is held
// to, a number is 0 or at least transform.MinPythonOutputBytes, and its
// description says the number. The other four sizes are described as every
// size was: a whole number from 0 or text of the pattern, and nothing more.
func TestTheSchemasHoldAnOutputLimitToTheLeastOfTheLoader(t *testing.T) {
	size := map[string]any{"type": []string{"integer", "string"}, "minimum": 0, "pattern": model.ByteSizePattern}
	for name, schema := range map[string]map[string]any{"the configuration": configSchema(), "a collector file": collectorFileSchema()} {
		collector := schema["properties"].(map[string]any)["collectors"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
		limits := collector["limits"].(map[string]any)["properties"].(map[string]any)
		request := collector["request"].(map[string]any)["properties"].(map[string]any)
		output := limits["max_output_bytes"].(map[string]any)
		want := map[string]any{"type": size["type"], "minimum": 0, "pattern": model.ByteSizePattern, "description": output["description"],
			"anyOf": []any{map[string]any{"const": 0}, map[string]any{"minimum": transform.MinPythonOutputBytes}}}
		if description, _ := output["description"].(string); !reflect.DeepEqual(output, want) || !containsAll(description, "At least 38 bytes", "0, like the key left out, is the default, 1MiB") {
			t.Errorf("%s: limits.max_output_bytes is described as %v", name, output)
		}
		for key, node := range map[string]any{"limits.max_response_bytes": limits["max_response_bytes"], "limits.max_script_memory": limits["max_script_memory"], "request.max_response_bytes": request["max_response_bytes"], "request.max_total_bytes": request["max_total_bytes"]} {
			described, _ := node.(map[string]any)
			without := map[string]any{}
			for keyword, value := range described {
				if keyword != "description" {
					without[keyword] = value
				}
			}
			if !reflect.DeepEqual(without, size) {
				t.Errorf("%s: %s is described as %v, want what every size is held to and no more", name, key, described)
			}
		}
	}
}

// containsAll reports whether text has every part.
func containsAll(text string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}
