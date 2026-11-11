package transform

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A rule's labels_from reads an object of each item, and the item's series
// has a label for each key, with the key's value written as a label
// expression's is. A key whose value is null or empty text leaves no label,
// as does an item without the object; a label the rule names itself
// replaces a key of its name; and the labels of each item are its own.
func TestLabelsFromGivesASeriesTheKeysOfAnObjectAsLabels(t *testing.T) {
	const body = `{"cluster":"c1","items":[
		{"name":"a","up":1,"labels":{"app":"web","tier":"front","replicas":3,"canary":true,"id":123456789012345678901234567890,"none":null,"empty":""}},
		{"name":"b","up":0,"labels":{"app":"db","name":"from the object"}},
		{"name":"c","up":1},
		{"name":"d","up":1,"labels":{}}]}`
	rule := model.MetricRule{Name: "up", Items: ".items[]", Expression: ".up", LabelsFrom: ".labels",
		Labels: []model.LabelRule{{Name: "name", Expression: ".name"}, {Name: "cluster", Expression: "$root.cluster"}}}
	c := model.Collector{Name: "c", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{rule}}
	set, failures, err := transformWith(context.Background(), t, c, "application/json", body)
	if err != nil || len(failures) != 0 {
		t.Fatalf("err %v, failures %+v", err, failures)
	}
	want := []map[string]string{
		{"name": "a", "cluster": "c1", "app": "web", "tier": "front", "replicas": "3", "canary": "true", "id": "123456789012345678901234567890"},
		{"name": "b", "cluster": "c1", "app": "db"},
		{"name": "c", "cluster": "c1"},
		{"name": "d", "cluster": "c1"},
	}
	if len(set.Metrics) != len(want) {
		t.Fatalf("%d series, want %d: %+v", len(set.Metrics), len(want), set.Metrics)
	}
	for i, m := range set.Metrics {
		if !reflect.DeepEqual(m.Labels, want[i]) {
			t.Errorf("series %d has the labels %v, want %v", i, m.Labels, want[i])
		}
	}

	// An expression may make the object: here every key gets a prefix.
	c.Metrics[0].LabelsFrom = `.labels // {} | with_entries(.key |= "label_" + .)`
	c.Metrics[0].Labels = nil
	set, _, err = transformWith(context.Background(), t, c, "application/json", body)
	if err != nil || !reflect.DeepEqual(set.Metrics[1].Labels, map[string]string{"label_app": "db", "label_name": "from the object"}) {
		t.Fatalf("with a prefix: %v, %+v", err, set)
	}
}

// What labels_from reads that is no object of single values is the failure
// of its item, handled by the rule's error_mode: the rule carries on with
// the other items under log, and fails the scrape under fail. The failure
// names the item and what was wrong, and a key by the one that sorts first.
func TestLabelsFromThatIsNoObjectOfValuesFailsItsItem(t *testing.T) {
	for name, tc := range map[string]struct{ from, body, want string }{
		"a string":          {".labels", `[{"v":1,"labels":"app=web"},{"v":2}]`, `metric "m" item 0 labels_from gave a string, and labels are read from an object of names and values`},
		"an array":          {".labels", `[{"v":1,"labels":["app"]},{"v":2}]`, `metric "m" item 0 labels_from gave an array, and labels are read from an object of names and values`},
		"a number":          {".x", `[{"v":1,"x":5},{"v":2}]`, `metric "m" item 0 labels_from gave a number, and labels are read from an object of names and values`},
		"a value an object": {".labels", `[{"v":2},{"v":1,"labels":{"z":[1,2],"b":{"c":1},"a":"x"}}]`, `metric "m" item 1 labels_from key "b" is an object, not a single value; select one of its fields`},
		"an expression":     {`.labels | if has("bad") then error("bad") else . end`, `[{"v":1,"labels":{"bad":1}},{"v":2,"labels":{}}]`, `metric "m" item 0 labels_from expression: `},
		"several objects":   {".labels, .labels", `[{"v":1,"labels":{"a":"x"}}]`, `metric "m" item 0 labels_from expression: expression ".labels, .labels" produced 2 values for one item; it must produce at most one`},
	} {
		rule := model.MetricRule{Name: "m", Items: ".[]", Expression: ".v", LabelsFrom: tc.from, ErrorMode: model.ErrorModeLog}
		c := model.Collector{Name: "c", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{rule}}
		set, failures, err := transformWith(context.Background(), t, c, "application/json", tc.body)
		kept := strings.Count(tc.body, `"v":`) - 1
		if err != nil || len(failures) != 1 || failures[0].Failures != 1 || !strings.Contains(failures[0].First.Error(), tc.want) {
			t.Errorf("%s under log: err %v, failures %+v; want one failure with %q", name, err, failures, tc.want)
		} else if set == nil && kept > 0 || set != nil && len(set.Metrics) != kept {
			t.Errorf("%s under log: series %+v, want those of the other %d items", name, set, kept)
		}
		c.Metrics[0].ErrorMode = model.ErrorModeFail
		if _, _, err := transformWith(context.Background(), t, c, "application/json", tc.body); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s under fail: %v, want %q", name, err, tc.want)
		}
	}
}

// Without items a rule's labels_from is read of the whole document, once,
// and its keys are labels of every series of the rule, under the rule's
// own labels, which are paired with the series as ever. What it reads that
// is no object of single values fails the rule as a whole, under its
// error_mode, before any series is made.
func TestLabelsFromWithoutItemsLabelsEverySeriesOfTheRule(t *testing.T) {
	const body = `{"meta":{"labels":{"app":"web","tier":"front","none":null,"name":"from the object"}},"values":[1,2],"names":["a","b"]}`
	rule := model.MetricRule{Name: "v", Expression: ".values[]", LabelsFrom: ".meta.labels", Labels: []model.LabelRule{{Name: "name", Expression: ".names[]"}}}
	c := model.Collector{Name: "c", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "yq"}, Metrics: []model.MetricRule{rule, {Name: "up", Expression: "1"}}}
	set, failures, err := transformWith(context.Background(), t, c, "application/json", body)
	if err != nil || len(failures) != 0 || len(set.Metrics) != 3 {
		t.Fatalf("series %+v, failures %+v, err %v", set, failures, err)
	}
	for i, want := range []map[string]string{{"app": "web", "tier": "front", "name": "a"}, {"app": "web", "tier": "front", "name": "b"}, {}} {
		if !reflect.DeepEqual(set.Metrics[i].Labels, want) {
			t.Errorf("series %d has the labels %v, want %v", i, set.Metrics[i].Labels, want)
		}
	}
	for name, tc := range map[string]struct{ from, want string }{
		"no object":         {".values", `metric "v" labels_from gave an array, and labels are read from an object of names and values`},
		"a value an object": {".", `metric "v" labels_from key "meta" is an object, not a single value; select one of its fields`},
		"two objects":       {".meta.labels, .meta.labels", `metric "v" labels_from expression: expression ".meta.labels, .meta.labels" produced 2 values for one item; it must produce at most one`},
	} {
		c.Metrics[0].LabelsFrom, c.Metrics[0].ErrorMode = tc.from, model.ErrorModeLog
		set, failures, err := transformWith(context.Background(), t, c, "application/json", body)
		if err != nil || len(failures) != 1 || !strings.Contains(failures[0].First.Error(), tc.want) || len(set.Metrics) != 1 || set.Metrics[0].Name != "up" {
			t.Errorf("%s under log: series %+v, failures %+v, err %v; want %q and the other rule's series", name, set, failures, err, tc.want)
		}
		c.Metrics[0].ErrorMode = model.ErrorModeFail
		if _, _, err := transformWith(context.Background(), t, c, "application/json", body); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s under fail: %v, want %q", name, err, tc.want)
		}
	}
	// Nothing to read is no label, as for an item.
	c.Metrics[0].LabelsFrom, c.Metrics[0].ErrorMode = ".absent", ""
	if set, _, err := transformWith(context.Background(), t, c, "application/json", body); err != nil || !reflect.DeepEqual(set.Metrics[0].Labels, map[string]string{"name": "a"}) {
		t.Errorf("an object that is not there: %+v, %v", set, err)
	}
}
