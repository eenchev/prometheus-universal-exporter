package transform

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A label a script gave as the empty string was exported as l="", where a
// rule's expression label that comes out empty is left off its series
// (missingRequiredLabel) and a script's None is too: to Prometheus m{l=""}
// is the series m, and OTLP got an attribute that says nothing. It is left
// off now wherever a script's answer is read: what metric(...) appends, an
// entry a script appends to metrics itself, and a series a pre-script leaves
// a prometheus transform. A value of one blank is a value and is kept, and
// so is every other label of the series.
func TestALabelAScriptGivesAsTheEmptyStringIsLeftOff(t *testing.T) {
	requirePython(t)
	labels := func(set *model.MetricSet) string {
		var b strings.Builder
		for _, m := range set.Metrics {
			text, _ := strings.CutPrefix(seriesText(model.Metric{Name: m.Name, Labels: m.Labels}), seriesText(model.Metric{Name: m.Name}))
			b.WriteString(m.Name)
			b.WriteString(text)
			b.WriteByte('\n')
		}
		return b.String()
	}
	// One script for the transform and one for the pre-script, so that the
	// test starts two workers: what metric(...) appends, an entry appended
	// by hand, and whatever writes itself as nothing, which is the empty
	// string too.
	set, err := runWorkerScript(t, workerCollector("empty", `
metric("alone", "gauge", 1, {"l": ""})
metric("beside", "gauge", 1, {"l": "", "k": "v", "b": " ", "n": None})
metric("valued", "gauge", 1, {"l": "x"})
metrics.append({"name": "entry", "value": 1, "labels": {"l": ""}})
metrics.append({"name": "entry_beside", "value": 1, "labels": {"l": "", "k": 5}})
class Nothing:
    def __str__(self): return ""
metric("written", "gauge", 1, {"l": Nothing(), "k": "v"})
`))
	if want := "alone labels:\nbeside labels: \"b\"=\" \" \"k\"=\"v\"\nvalued labels: \"l\"=\"x\"\nentry labels:\nentry_beside labels: \"k\"=\"5\"\nwritten labels: \"k\"=\"v\"\n"; err != nil {
		t.Fatal(err)
	} else if labels(set) != want {
		t.Errorf("a python transform is read as %q, want %q", labels(set), want)
	}

	c := model.Collector{Name: "pre", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(), Transform: model.TransformConfig{Type: "prometheus"}}
	c.Transform.PreScript = `
by_name = {series["name"]: series for series in data["metrics"]}
by_name["added"]["labels"]["l"] = ""
by_name["replaced"]["labels"]["a"] = ""
by_name["beside"]["labels"].update({"l": "", "b": " ", "n": None})
data["metrics"].append({"name": "made", "value": 2, "labels": {"l": "", "k": "v"}})
`
	set, err = runBody(t, c, "text/plain", "added{a=\"b\"} 1\nreplaced{a=\"b\"} 1\nbeside{a=\"b\"} 1\nkept{a=\"b\"} 1\n")
	if want := "added labels: \"a\"=\"b\"\nreplaced labels:\nbeside labels: \"a\"=\"b\" \"b\"=\" \"\nkept labels: \"a\"=\"b\"\nmade labels: \"k\"=\"v\"\n"; err != nil {
		t.Fatal(err)
	} else if labels(set) != want {
		t.Errorf("a pre-script of a prometheus transform is read as %q, want %q", labels(set), want)
	}
}

// The series a script meant as m{l=""} beside m are the series m twice, as
// they are to Prometheus, and the scrape is refused for the duplicate where
// every series is checked, as it was: the check always took a label with an
// empty value for none (model.MetricSet.Validate). It names the series as it
// is read now, without the label.
func TestSeriesAScriptTellsApartByAnEmptyLabelAreDuplicates(t *testing.T) {
	requirePython(t)
	for _, script := range []string{
		`metric("m", "gauge", 1, {"l": ""}); metric("m", "gauge", 2)`,
		`metric("m", "gauge", 1, {"l": "", "k": "v"}); metrics.append({"name": "m", "value": 2, "labels": {"k": "v", "j": ""}})`,
	} {
		set, err := runWorkerScript(t, workerCollector("twice", script))
		if err != nil || len(set.Metrics) != 2 {
			t.Fatalf("%s: %v, %+v", script, err, set)
		}
		if err := set.Validate(model.Limits{}); err == nil || err.Error() != `duplicate metric series "m"` {
			t.Errorf("%s: checked as %v, want the duplicate refused", script, err)
		}
	}
	c := model.Collector{Name: "pre", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(), Transform: model.TransformConfig{Type: "prometheus"}}
	c.Transform.PreScript = `data["metrics"].append({"name": "m", "type": "gauge", "value": 2, "labels": {"l": ""}})`
	set, err := runBody(t, c, "text/plain", "# TYPE m gauge\nm 1\n")
	if err != nil || len(set.Metrics) != 2 {
		t.Fatalf("%v, %+v", err, set)
	}
	if err := set.Validate(model.Limits{}); err == nil || err.Error() != `duplicate metric series "m"` {
		t.Errorf("checked as %v, want the duplicate refused", err)
	}
}

// The rule is about the value the script gave, before anything is made of
// it: a python rule's truncate: true cuts the label that has a value and
// finds none where the script gave the empty string, and the rules of a
// prometheus transform read a pre-script's series without the label, so a
// label of theirs read from it is left off as well, and a required one is
// missing.
func TestAnEmptyLabelOfAScriptIsLeftOffBeforeTheRulesReadIt(t *testing.T) {
	requirePython(t)
	python := model.Collector{Name: "cut", Decoder: model.DecoderConfig{Type: "text"}, Limits: scriptLimits(),
		Transform: model.TransformConfig{Type: "python", Script: `metric("m", "gauge", 1, {"note": "", "id": "1"}); metric("m", "gauge", 2, {"note": "a long note", "id": "2"})`},
		Metrics:   []model.MetricRule{{Name: "m", Labels: []model.LabelRule{{Name: "note", Expression: "note", Truncate: true}}}}}
	python.Limits.MaxLabelValueLength = 6
	set, err := runBody(t, python, "text/plain", "x")
	if err != nil || len(set.Metrics) != 2 {
		t.Fatalf("%v, %+v", err, set)
	}
	if first, second := set.Metrics[0].Labels, set.Metrics[1].Labels; len(first) != 1 || first["id"] != "1" || len(second) != 2 || second["note"] != "a l…" {
		t.Errorf("the series have the labels %q and %q, want id alone and a note cut to 6 bytes", first, second)
	}

	rules := model.Collector{Name: "pre", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(),
		Transform: model.TransformConfig{Type: "prometheus", PreScript: `data["metrics"][0]["labels"]["site"] = ""`},
		Metrics:   []model.MetricRule{{Name: "up_thing", Labels: []model.LabelRule{{Name: "location", Expression: "site"}}}}}
	set, err = runBody(t, rules, "text/plain", "# TYPE up_thing gauge\nup_thing{site=\"rack1\",a=\"b\"} 1\n")
	if err != nil || len(set.Metrics) != 1 || len(set.Metrics[0].Labels) != 1 || set.Metrics[0].Labels["a"] != "b" {
		t.Fatalf("%v, %+v, want the series with a alone", err, set)
	}
	rules.Metrics[0].Labels[0].Required = true
	rules.Metrics[0].ErrorMode = "fail"
	if _, err = runBody(t, rules, "text/plain", "# TYPE up_thing gauge\nup_thing{site=\"rack1\",a=\"b\"} 1\n"); err == nil || !strings.Contains(err.Error(), `metric "up_thing" label "location" is missing`) {
		t.Errorf("err=%v, want the required label missing", err)
	}
}

// A prometheus transform without a script leaves the label off as well: a
// label a target's own exposition gives as empty was the decoder's to read
// and stayed on the series, so that a pre-script that only passed data on
// changed the answer. The decoder leaves it off now (decode's
// parsePrometheusText), and the series is the one a pre-script that changes
// nothing leaves. Beside the same series without the label it is that
// series twice, refused as the duplicate it is, as it was: the check always
// read such a label as none, and said then that the empty label was why.
func TestAnEmptyLabelOfATargetsExpositionIsLeftOff(t *testing.T) {
	c := model.Collector{Name: "plain", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}}
	set, err := runBody(t, c, "text/plain", "m{l=\"\",k=\"v\"} 1\n")
	if err != nil || len(set.Metrics) != 1 || len(set.Metrics[0].Labels) != 1 || set.Metrics[0].Labels["k"] != "v" {
		t.Fatalf("%v, %+v, want the series with k alone", err, set)
	}
	if err := set.Validate(model.Limits{}); err != nil {
		t.Errorf("checked as %v", err)
	}
	set, err = runBody(t, c, "text/plain", "m{l=\"\"} 1\nm 2\n")
	if err != nil || len(set.Metrics) != 2 || len(set.Metrics[0].Labels) != 0 {
		t.Fatalf("%v, %+v, want two series without labels", err, set)
	}
	if err := set.Validate(model.Limits{}); err == nil || err.Error() != `duplicate metric series "m"` {
		t.Errorf("checked as %v, want the duplicate refused", err)
	}
	// The check itself is what it was: series that differ only in a label
	// with an empty value, which no decoder and no script hands on now, are
	// duplicates, and the error says why.
	apart := model.MetricSet{Metrics: []model.Metric{{Name: "m", Type: model.GaugeMetricType, Labels: map[string]string{"l": ""}}, {Name: "m", Type: model.GaugeMetricType}}}
	if err := apart.Validate(model.Limits{}); err == nil || err.Error() != `duplicate metric series "m": Prometheus reads a label with an empty value as no label, so series that differ only in one are the same series` {
		t.Errorf("checked as %v, want the duplicate refused for the empty label", err)
	}
}
