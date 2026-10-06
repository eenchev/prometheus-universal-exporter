package transform

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A script's answer is read as it was (scriptansweroracle_test.go) but for
// two things: a label the script gave as the empty string is left off its
// series, in a python transform's metrics and in the series a pre-script
// leaves a prometheus transform, and such a series' help, type or labels of
// another kind than a series has fail the pre-script. These tests compare
// the two readings over generated answers: an answer with neither is read
// exactly as it was, error for error, and one with an empty label is read as
// it was without that label.

// seriesText writes a series out, every part of it: a float by its bits, so
// that NaN is itself, the labels in the order of their names, and whether
// there is a map of them at all.
func seriesText(m model.Metric) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%q type=%q help=%q value=%x created=%d", m.Name, m.Type, m.Help, math.Float64bits(m.Value), m.Created)
	if m.Timestamp != nil {
		fmt.Fprintf(&b, " at=%d", *m.Timestamp)
	}
	if m.Labels != nil {
		b.WriteString(" labels:")
	}
	for _, name := range model.SortedKeys(m.Labels) {
		fmt.Fprintf(&b, " %q=%q", name, m.Labels[name])
	}
	if h := m.Histogram; h != nil {
		fmt.Fprintf(&b, " histogram sum=%x count=%d %v %v", math.Float64bits(h.Sum), h.Count, h.NoSum, h.NoCount)
		for _, bucket := range h.Buckets {
			fmt.Fprintf(&b, " %x:%d", math.Float64bits(bucket.UpperBound), bucket.CumulativeCount)
		}
	}
	if s := m.Summary; s != nil {
		fmt.Fprintf(&b, " summary sum=%x count=%d %v %v", math.Float64bits(s.Sum), s.Count, s.NoSum, s.NoCount)
		for _, quantile := range s.Quantiles {
			fmt.Fprintf(&b, " %x:%x", math.Float64bits(quantile.Quantile), math.Float64bits(quantile.Value))
		}
	}
	return b.String()
}

// setText is seriesText of every series of a set, a line each.
func setText(set []model.Metric) string {
	var b strings.Builder
	for _, m := range set {
		b.WriteString(seriesText(m))
		b.WriteByte('\n')
	}
	return b.String()
}

// withoutEmptyLabels is the series as they were read, each without the
// labels that had an empty value, and how many those were. A series that
// had labels still has a map of them, as one whose labels were all None has.
func withoutEmptyLabels(was []model.Metric) (series []model.Metric, left int) {
	series = make([]model.Metric, len(was))
	for i, m := range was {
		series[i] = m
		if m.Labels == nil {
			continue
		}
		series[i].Labels = make(map[string]string, len(m.Labels))
		for name, value := range m.Labels {
			if value == "" {
				left++
				continue
			}
			series[i].Labels[name] = value
		}
	}
	return series, left
}

// answerReading is how a reading of an answer compares with the one before.
type answerReading int

const (
	// readNoAnswer is a line that is no answer or a script's error, and
	// readRefused an answer refused as it was, with the error it had.
	readNoAnswer answerReading = iota
	readRefused
	// readSame is an answer read into the series it was read into.
	readSame
	// readWithoutEmpty is one read into them without their empty labels.
	readWithoutEmpty
	// readRefusedKind is a pre-script's answer that was read and is now
	// refused for a help, a type or labels of another kind.
	readRefusedKind
)

// comparePythonTransformAnswer reads line as a python transform's answer is
// read and as it was read, and reports any difference but the one: series
// without the labels that had an empty value.
func comparePythonTransformAnswer(t *testing.T, line []byte) answerReading {
	t.Helper()
	c := &model.Collector{Name: "answer"}
	old, oldErr := oraclePythonAnswer("transform", line)
	got, gotErr := pythonResult(c, "transform", oracleTimeout, line, nil)
	if (oldErr == nil) != (gotErr == nil) || oldErr != nil && oldErr.Error() != gotErr.Error() {
		t.Fatalf("%q: error %v, was %v", line, gotErr, oldErr)
	}
	if oldErr != nil {
		return readNoAnswer
	}
	oldSet, oldErr := formerPythonSeries(context.Background(), old)
	gotSet, gotErr := pythonSeries(context.Background(), got)
	if (oldErr == nil) != (gotErr == nil) || oldErr != nil && (oldErr.Error() != gotErr.Error() || errors.Is(oldErr, model.ErrScriptFailed) != errors.Is(gotErr, model.ErrScriptFailed)) {
		t.Fatalf("%q: error %v, was %v", line, gotErr, oldErr)
	}
	if oldErr != nil {
		return readRefused
	}
	want, left := withoutEmptyLabels(oldSet.Metrics)
	if left == 0 {
		// Nothing to leave off: the series it was read into, themselves.
		want = oldSet.Metrics
	}
	if is, was := setText(gotSet.Metrics), setText(want); is != was {
		t.Fatalf("%q is read as\n%swant, as it was but for %d empty labels,\n%s", line, is, left, was)
	}
	if left == 0 {
		return readSame
	}
	for _, m := range gotSet.Metrics {
		for name, value := range m.Labels {
			if value == "" {
				t.Fatalf("%q: the series %s has the label %s empty", line, m.Name, name)
			}
		}
	}
	return readWithoutEmpty
}

// emptyLabelAnswer is an answer of a few metrics whose labels are often the
// empty string: each as metric(...) appends it, which is read without
// encoding/json, or, by hand, each with a key of the script's own, which is
// read with it (answerReader.metric).
func emptyLabelAnswer(g *answerGenerator, byHand bool) []byte {
	metrics := make([]any, 1+g.random.IntN(4))
	for i := range metrics {
		labels := make(pyDict, g.random.IntN(4))
		for at := range labels {
			labels[at] = pyPair{[]string{"l", "id", "region", "le"}[g.random.IntN(4)], []any{"", "", "x", "eu-1", " ", nil}[g.random.IntN(6)]}
			if byHand && g.chance(4) {
				// What only an entry made by hand holds: a number, a
				// boolean, the marker of a float, a string not written plain.
				labels[at].value = []any{5, 0.5, true, nonFiniteMarker + "NaN\x00", "ü", pyRaw(`"\u0000"`), pyRaw(`""`)}[g.random.IntN(7)]
			}
		}
		entry := pyDict{
			{"name", []string{"item_value", "up"}[g.random.IntN(2)]},
			{"type", []string{"gauge", "counter", ""}[g.random.IntN(3)]},
			{"value", float64(g.random.IntN(1000))},
			{"labels", labels},
			{"help", []string{"", "Items."}[g.random.IntN(2)]},
			{"timestamp", nil},
		}
		if byHand {
			entry = append(entry, pyPair{"mine", g.random.IntN(9)})
		}
		metrics[i] = entry
	}
	return pyLine(pyDict{{"ok", true}, {"log", ""}, {"metrics", metrics}})
}

// A python transform's answers, as a worker writes them and as it does not
// (answerGenerator), and answers whose labels are often empty, as
// metric(...) appends them and as a script appends them by hand: 12,000
// lines, 900 under the race detector. Each is read as it was, error for
// error and series for series, but for the labels that were the empty
// string, which are left off. Answers of both kinds are among those read
// without one, and among those read without their empty labels.
func TestPythonTransformAnswersAreReadAsTheyWereButForEmptyLabels(t *testing.T) {
	usePythonPool(t)
	answers := alloctest.UnlessRaced(4000, 300)
	g := &answerGenerator{random: rand.New(rand.NewPCG(26, 2026))}
	readings, appended, byHand := map[answerReading]int{}, 0, 0
	for range answers {
		readings[comparePythonTransformAnswer(t, g.answer())]++
		if comparePythonTransformAnswer(t, emptyLabelAnswer(g, false)) == readWithoutEmpty {
			appended++
		}
		if comparePythonTransformAnswer(t, emptyLabelAnswer(g, true)) == readWithoutEmpty {
			byHand++
		}
	}
	t.Logf("of %d random answers %d are no answer, %d refused as they were, %d read as they were and %d without their empty labels; of %d as metric(...) appends them %d are read without theirs, and of %d made by hand %d", answers, readings[readNoAnswer], readings[readRefused], readings[readSame], readings[readWithoutEmpty], answers, appended, answers, byHand)
	if readings[readSame] < answers/4 || readings[readRefused] < answers/10 || readings[readNoAnswer] < answers/20 || readings[readWithoutEmpty] < answers/100 || appended < answers/4 || byHand < answers/4 || appended > answers*9/10 || byHand > answers*9/10 {
		t.Fatal("the generators no longer write answers of every kind")
	}
}

// The same by a table: what metric(...) appends and what a script appends
// itself, with a label that is the empty string alone, beside others, under
// a name written twice, and beside None.
func TestAPythonTransformLeavesALabelGivenAsTheEmptyStringOff(t *testing.T) {
	usePythonPool(t)
	answer := func(metric string) []byte { return []byte(`{"ok": true, "log": "", "metrics": [` + metric + `]}`) }
	for metric, want := range map[string]string{
		`{"name": "m", "type": "gauge", "value": 1.0, "labels": {"l": ""}, "help": "", "timestamp": null}`:                      "labels:",
		`{"name": "m", "type": "gauge", "value": 1.0, "labels": {"l": "", "k": "v"}, "help": "", "timestamp": null}`:            `labels: "k"="v"`,
		`{"name": "m", "type": "gauge", "value": 1.0, "labels": {"k": "v", "l": "", "n": null}, "help": "", "timestamp": null}`: `labels: "k"="v"`,
		`{"name": "m", "type": "gauge", "value": 1.0, "labels": {"l": "x", "l": ""}, "help": "", "timestamp": null}`:            "labels:",
		`{"name": "m", "type": "gauge", "value": 1.0, "labels": {"l": "", "l": "x"}, "help": "", "timestamp": null}`:            `labels: "l"="x"`,
		`{"name": "m", "type": "gauge", "value": 1.0, "labels": {"l": " "}, "help": "", "timestamp": null}`:                     `labels: "l"=" "`,
		`{"name": "m", "type": "gauge", "value": 1.0, "labels": {}, "help": "", "timestamp": null}`:                             "",
		`{"name": "m", "value": 1, "labels": {"l": ""}}`:                                                                        "labels:",
		`{"name": "m", "value": 1, "labels": {"l": "", "k": 5, "n": null}}`:                                                     `labels: "k"="5"`,
		`{"name": "m", "value": 1, "labels": {"l": "x", "l": ""}, "mine": 1}`:                                                   "labels:",
		`{"name": "m", "value": 1, "labels": {"l": "", "l": "x"}, "mine": 1}`:                                                   `labels: "l"="x"`,
		`{"name": "m", "value": 1, "labels": {"l": "\u0000"}}`:                                                                  `labels: "l"="\x00"`,
		`{"name": "m", "value": 1, "labels": null}`:                                                                             "",
	} {
		line := answer(metric)
		comparePythonTransformAnswer(t, line)
		out, err := pythonResult(&model.Collector{Name: "answer"}, "transform", oracleTimeout, line, nil)
		if err != nil {
			t.Fatal(err)
		}
		set, err := pythonSeries(context.Background(), out)
		if err != nil || len(set.Metrics) != 1 {
			t.Fatalf("%s: %v, %+v", metric, err, set)
		}
		if got, _ := strings.CutPrefix(seriesText(set.Metrics[0]), seriesText(model.Metric{Name: "m", Type: model.GaugeMetricType, Value: 1})); strings.TrimSpace(got) != want {
			t.Errorf("%s: read with %q, want %q", metric, strings.TrimSpace(got), want)
		}
	}
}

// preScriptGenerator makes what a pre-script leaves a prometheus transform:
// series as the exporter reads them back from a worker's answer, their
// numbers a float64, an int or a *big.Int.
type preScriptGenerator struct {
	random *rand.Rand
}

func (g *preScriptGenerator) chance(n int) bool { return g.random.IntN(n) == 0 }

func (g *preScriptGenerator) pick(values ...any) any { return values[g.random.IntN(len(values))] }

// number is a number, seldom something else where one belongs.
func (g *preScriptGenerator) number() any {
	if g.chance(40) {
		return g.pick("x", nil, []any{1.0}, map[string]any{"v": 1.0})
	}
	return g.pick(0.0, 1.0, 2.0, 3, 17, 0.5, -1.5, "7", " 1.5 ", true, math.NaN(), math.Inf(1), 1700000000000, new(big.Int).Lsh(big.NewInt(1), 70))
}

// count is a count of observations, seldom something that is none.
func (g *preScriptGenerator) count() any {
	if g.chance(40) {
		return g.pick(2.5, -1, "x", math.NaN(), []any{1.0})
	}
	return g.pick(0, 1, 2.0, 3, 17, "5", 1e6)
}

// set puts one of values under key, or nothing when it is absent{}.
func (g *preScriptGenerator) set(series map[string]any, key string, values ...any) {
	value := g.pick(values...)
	if _, none := value.(absent); !none {
		series[key] = value
	}
}

// absent stands for a key a series does not have.
type absent struct{}

// labels is a series' labels: text under names, with now and then None, a
// number or a boolean, and seldom a list or a dict, which is an error. empty
// says a value may be the empty string.
func (g *preScriptGenerator) labels(empty bool) map[string]any {
	labels := make(map[string]any, 3)
	for range g.random.IntN(4) {
		value := g.pick("x", "eu-1", " ", "0", 5, 0.5, true, nil)
		if empty && g.chance(3) {
			value = ""
		}
		labels[g.pick("l", "id", "region", "le", "quantile").(string)] = value
	}
	if g.chance(40) {
		labels["tags"] = g.pick([]any{"a", "b"}, map[string]any{"a": "b"})
	}
	return labels
}

// series is one series. kinds says its help, type and labels may be of
// another kind than a series has, and empty that a label may be the empty
// string: without the two it is a series that was always read or refused as
// it is now.
func (g *preScriptGenerator) series(kinds, empty bool) any {
	if g.chance(200) {
		return g.pick("up", 5, nil, []any{})
	}
	series := map[string]any{}
	if g.set(series, "name", "m", "h", "s", "made"); g.chance(40) {
		g.set(series, "name", "", 5, nil, absent{})
	}
	g.set(series, "type", "gauge", "counter", "untyped", "histogram", "histogram", "summary", "summary", "", "counterr", nil, absent{})
	g.set(series, "help", "Help.", "Help.", "", nil, absent{})
	switch {
	case g.chance(8):
		series["labels"] = nil
	case !g.chance(8):
		series["labels"] = g.labels(empty)
	}
	if kinds {
		if g.chance(10) {
			series["type"] = g.pick(5, []any{"counter"}, true, map[string]any{"type": "gauge"}, 0.5)
		}
		if g.chance(10) {
			series["help"] = g.pick(5, []any{"a", "b", "c"}, false, map[string]any{}, math.NaN())
		}
		if g.chance(10) {
			series["labels"] = g.pick([]any{[]any{"a", "b"}}, []any{}, "a=b", 5, true, 0.5)
		}
	}
	if !g.chance(40) {
		series["value"] = g.number()
	}
	if g.chance(5) {
		series["timestamp"] = g.pick(1700000000000, 1.7e12, "17", nil, 0, -1.5)
		if g.chance(8) {
			series["timestamp"] = g.pick("soon", []any{1, 2, 3}, 1e19, math.NaN())
		}
	}
	if g.chance(2) {
		series["sum"] = g.number()
	}
	if g.chance(2) {
		series["count"] = g.count()
	}
	// The bounds and the quantiles in their order, one seldom there twice,
	// which a histogram and a summary are refused for.
	if g.chance(2) {
		bounds := []any{0.5, 1.0, 2.5, math.Inf(1), "0.5"}
		buckets := make([]any, g.random.IntN(5))
		for i := range buckets {
			buckets[i] = map[string]any{"le": bounds[i], "count": g.count()}
			if g.chance(40) {
				buckets[i] = g.pick("bucket", nil, map[string]any{"le": 1.0}, map[string]any{"le": nil, "count": 1}, map[string]any{"le": bounds[4], "count": 1})
			}
		}
		series[g.pick("buckets", "buckets", "buckets", "buckets", "bucket").(string)] = buckets
	}
	if g.chance(2) {
		ranks := []any{0.5, 0.9, 0.99, "0.5"}
		quantiles := make([]any, g.random.IntN(4))
		for i := range quantiles {
			quantiles[i] = map[string]any{"quantile": ranks[i], "value": g.number()}
			if g.chance(40) {
				quantiles[i] = g.pick("quantile", nil, map[string]any{"quantile": 0.5}, map[string]any{"quantile": ranks[3], "value": 1})
			}
		}
		series["quantiles"] = quantiles
	}
	return series
}

// document is what a pre-script left in data: a few series, or seldom
// something that is no such document.
func (g *preScriptGenerator) document(kinds, empty bool) any {
	if g.chance(80) {
		return g.pick(nil, "text", []any{}, map[string]any{}, map[string]any{"metrics": "up"}, map[string]any{"metrics": nil})
	}
	list := make([]any, g.random.IntN(5))
	for i := range list {
		list[i] = g.series(kinds, empty)
	}
	return map[string]any{"metrics": list}
}

// otherKind is the failure of a series whose type, help or labels is of
// another kind than a series has, the first of them, or nothing.
func otherKind(series map[string]any) string {
	name, _ := series["name"].(string)
	if _, text := series["type"].(string); !text && series["type"] != nil {
		return fmt.Sprintf(`%s type %s is not a string; give "gauge", "counter", "untyped", "histogram" or "summary"`, name, model.ShowValue(series["type"]))
	}
	if _, text := series["help"].(string); !text && series["help"] != nil {
		return fmt.Sprintf("%s help %s is not a string", name, model.ShowValue(series["help"]))
	}
	if _, mapping := series["labels"].(map[string]any); !mapping && series["labels"] != nil {
		return fmt.Sprintf("%s labels are %s, not a mapping of label names to values", name, model.ShowValue(series["labels"]))
	}
	return ""
}

// comparePreScriptDocument reads what a pre-script left a prometheus
// transform as it is read and as it was read, and reports any difference but
// the two: the first series with a type, a help or labels of another kind
// fails, unless it had no name or a series before it failed as it did, and
// the series are without the labels that had an empty value.
func comparePreScriptDocument(t *testing.T, document any) answerReading {
	t.Helper()
	old, oldErr := formerPrometheusFromPython(document)
	got, gotErr := prometheusFromPython(document)
	// The series with a failure it did not have, when one comes before the
	// first that failed as it was.
	wantErr, refusedKind := oldErr, false
	var list []any
	if holds, ok := document.(map[string]any); ok {
		list, _ = holds["metrics"].([]any)
	}
	for i, raw := range list {
		series, ok := raw.(map[string]any)
		if !ok {
			break
		}
		if _, err := formerPrometheusSeries(series); err != nil && err.Error() == "has no name" {
			break
		} else if kind := otherKind(series); kind != "" {
			wantErr, refusedKind = fmt.Errorf("data[\"metrics\"][%d]: %s", i, kind), true
			break
		} else if err != nil {
			break
		}
	}
	if (wantErr == nil) != (gotErr == nil) || wantErr != nil && wantErr.Error() != gotErr.Error() {
		t.Fatalf("%#v: error %v, want %v; it was %v", document, gotErr, wantErr, oldErr)
	}
	switch {
	case refusedKind:
		if len(got.Metrics) != 0 {
			t.Fatalf("%#v: refused, and read into %d series", document, len(got.Metrics))
		}
		return readRefusedKind
	case wantErr != nil:
		return readRefused
	}
	want, left := withoutEmptyLabels(old.Metrics)
	if left == 0 {
		want = old.Metrics
	}
	if is, was := setText(got.Metrics), setText(want); is != was || (got.Metrics == nil) != (old.Metrics == nil) {
		t.Fatalf("%#v is read as\n%swant, as it was but for %d empty labels,\n%s", document, is, left, was)
	}
	if left == 0 {
		return readSame
	}
	return readWithoutEmpty
}

// What a pre-script leaves a prometheus transform, generated: 30,000
// documents of series of every type, with and without each key, with parts
// that are wrong in every way a part was refused for, 3,000 under the race
// detector. A quarter of them have no label that is the empty string and no
// help, type or labels of another kind, and every one of those is read
// exactly as it was, series for series and error for error. Of the others,
// one with an empty label is read as it was without that label, and one
// with a help, a type or labels of another kind fails for the first such
// series, unless it failed before that series as it always did.
func TestPreScriptSeriesAreReadAsTheyWereButForEmptyLabelsAndOtherKinds(t *testing.T) {
	documents := alloctest.UnlessRaced(7500, 750)
	g := &preScriptGenerator{random: rand.New(rand.NewPCG(26, 1005))}
	neither, kinds, empties, both := map[answerReading]int{}, map[answerReading]int{}, map[answerReading]int{}, map[answerReading]int{}
	for range documents {
		reading := comparePreScriptDocument(t, g.document(false, false))
		if reading != readSame && reading != readRefused {
			t.Fatalf("a document with no empty label and no part of another kind is read otherwise than it was (%d)", reading)
		}
		neither[reading]++
		kinds[comparePreScriptDocument(t, g.document(true, false))]++
		empties[comparePreScriptDocument(t, g.document(false, true))]++
		both[comparePreScriptDocument(t, g.document(true, true))]++
	}
	t.Logf("of %d documents with neither, %d are read as they were and %d refused as they were; of %d with parts of another kind, %d are refused for one and %d read as they were; of %d with empty labels, %d are read without them; of %d with both, %d are read without their empty labels and %d refused for a part of another kind",
		documents, neither[readSame], neither[readRefused], documents, kinds[readRefusedKind], kinds[readSame], documents, empties[readWithoutEmpty], documents, both[readWithoutEmpty], both[readRefusedKind])
	if neither[readSame] < documents/4 || neither[readRefused] < documents/5 || kinds[readRefusedKind] < documents/4 || kinds[readSame] < documents/10 || empties[readWithoutEmpty] < documents/10 || both[readWithoutEmpty] < documents/20 || both[readRefusedKind] < documents/4 ||
		kinds[readWithoutEmpty] != 0 || empties[readRefusedKind] != 0 {
		t.Fatal("the generator no longer makes documents of every kind")
	}
}

// The exposition fixtures, each as a pre-script is given it
// (pythonPrometheusData) and leaves it when it changes nothing, are read
// back as they were: none of their series has an empty label or a part of
// another kind.
func TestAPreScriptThatPassesTheFixturesOnHasThemReadAsTheyWere(t *testing.T) {
	files, err := filepath.Glob("../../testdata/prometheus/*.prom")
	if err != nil || len(files) < 2 {
		t.Fatalf("%d fixtures, %v", len(files), err)
	}
	series := 0
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		c := &model.Collector{Name: "fixture", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}}
		d, err := decode.Decode(&fetch.HTTPResponse{StatusCode: 200, Body: body, Headers: http.Header{}}, c)
		if err != nil {
			t.Fatal(err)
		}
		set := d.Data.(model.MetricSet)
		if reading := comparePreScriptDocument(t, pythonPrometheusData(set)); reading != readSame {
			t.Errorf("%s is read otherwise than it was (%d)", file, reading)
		}
		series += len(set.Metrics)
	}
	if series < 20 {
		t.Errorf("the fixtures have %d series", series)
	}
}
