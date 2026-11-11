package transform

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// metric() takes a value of any kind of number that says what float it is,
// a Decimal and a Fraction as well as an int, a float, a bool and a numeric
// string; a label value of bytes is the text they hold; and a timestamp
// may be a datetime that names its zone, which is its moment in
// milliseconds.
func TestMetricTakesTheNumbersAndTimesAScriptHas(t *testing.T) {
	requirePython(t)
	set, err := runWorkerScript(t, workerCollector("kinds", `import datetime, decimal, fractions
metric(name="decimal", value=decimal.Decimal("1.25"))
metric(name="fraction", value=fractions.Fraction(1, 4))
metric(name="text", value=" 12 ")
metric(name="flag", value=True)
metric(name="labelled", value=1, labels={"raw": b"caf\xc3\xa9", "broken": b"a\xffb"})
metric(name="dated", value=1, timestamp=datetime.datetime(2026, 1, 2, 3, 4, 5, 500000, tzinfo=datetime.timezone.utc))
metric(name="zoned", value=1, timestamp=datetime.datetime(2026, 1, 2, 5, 4, 5, tzinfo=datetime.timezone(datetime.timedelta(hours=2))))
metric(name="millis", value=1, timestamp=1767323045500.9)
metric(name="old", value=1, timestamp=86400000)`))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]model.Metric{}
	for _, m := range set.Metrics {
		byName[m.Name] = m
	}
	for name, want := range map[string]float64{"decimal": 1.25, "fraction": 0.25, "text": 12, "flag": 1} {
		if got := byName[name].Value; got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if labels := byName["labelled"].Labels; labels["raw"] != "café" || labels["broken"] != "a�b" {
		t.Errorf("the labels of bytes are %q", labels)
	}
	for name, want := range map[string]int64{"dated": 1767323045500, "zoned": 1767323045000, "millis": 1767323045500, "old": 86400000} {
		if at := byName[name].Timestamp; at == nil || *at != want {
			t.Errorf("%s has the timestamp %v, want %d", name, at, want)
		}
	}
}

// What metric() cannot take fails the script on its own line, saying what
// to do: a timestamp that reads as seconds, a datetime without a zone, a
// whole number too large for a float, and what is no number.
func TestMetricRefusesWhatReadsWrongSayingWhatToDo(t *testing.T) {
	requirePython(t)
	for name, tc := range map[string]struct{ script, want string }{
		"seconds":          {`import time` + "\n" + `metric(name="m", value=1, timestamp=1767323045.5)`, `ValueError: metric 'm' timestamp 1767323045.5 reads as seconds since the epoch; a timestamp is milliseconds, so multiply it by 1000, or pass a datetime with a time zone`},
		"a naive datetime": {`import datetime` + "\n" + `metric(name="m", value=1, timestamp=datetime.datetime(2026, 1, 2))`, `ValueError: metric 'm' timestamp is a datetime without a time zone, which is no single moment; give it one, as datetime.now(timezone.utc) has`},
		"a date":           {`import datetime` + "\n" + `metric(name="m", value=1, timestamp=datetime.date(2026, 1, 2))`, `ValueError: metric 'm' timestamp datetime.date(2026, 1, 2) is not a number`},
		"a huge int":       {`metric(name="m", value=10**400)`, `ValueError: metric 'm' value is a whole number of 401 digits, too large for a float, which every sample is`},
		"a huge decimal":   {`import decimal` + "\n" + `metric(name="m", value=decimal.Decimal("NaN").fma(1, 1) if False else decimal.Decimal("sNaN"))`, `ValueError: metric 'm' value Decimal('sNaN') is not a number`},
		"bytes":            {`metric(name="m", value=b"12")`, `ValueError: metric 'm' value b'12' is not a number`},
		"none":             {`metric(name="m", value=None)`, `ValueError: metric 'm' value None is not a number`},
	} {
		_, err := runWorkerScript(t, workerCollector("refused", tc.script))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `File "<collector-python>", line `) || strings.Contains(err.Error(), "OverflowError") {
			t.Errorf("%s: %v\nwant the script's line and %s", name, err, tc.want)
		}
	}
}

// What a script leaves that JSON cannot carry, a set, bytes, a date or a
// Decimal, in a pre-script's data or in an entry of metrics, fails the
// script in one line that names the kind and what to make of it, and not
// in the frames of the json module, which name nothing of the script's.
func TestWhatJSONCannotCarryIsSaidInALine(t *testing.T) {
	requirePython(t)
	r := &fetch.HTTPResponse{Body: []byte(`{"a":1}`)}
	d := &decode.Decoded{Kind: "json", Data: map[string]any{"a": 1}, Raw: r.Body}
	for kind, script := range map[string]string{
		"set":      `data["s"] = {1, 2}`,
		"bytes":    `data["b"] = b"x"`,
		"datetime": "import datetime\ndata[\"d\"] = datetime.datetime(2026, 1, 2)",
		"Decimal":  "import decimal\ndata[\"n\"] = decimal.Decimal(\"1.5\")",
	} {
		c := workerCollector("left", "")
		_, err := executePythonPreScript(context.Background(), "python3", script, d, r, c)
		want := "python pre-script failed: TypeError: what the script left in data holds a " + kind + ", which cannot be handed back to the exporter: a str, an int, a float, a bool, None, a list, a tuple and a dict can; make it one of those, as list(a_set), str(a_date) or float(a_decimal) do"
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v\nwant %s", kind, err, want)
		}
	}
	_, err := runWorkerScript(t, workerCollector("left", `metrics.append({"name": "m", "value": 1, "labels": {"a": {1, 2}}})`))
	if err == nil || !strings.Contains(err.Error(), "TypeError: what the script left in metrics holds a set, which cannot be handed back") || strings.Contains(err.Error(), "json/encoder.py") {
		t.Errorf("an entry of metrics: %v", err)
	}
	// The worker serves the next script.
	if set, err := runWorkerScript(t, workerCollector("left", `metric(name="m", value=1)`)); err != nil || len(set.Metrics) != 1 {
		t.Errorf("the script after: %+v, %v", set, err)
	}
}

// A help text a script gives a later series of a name alone is the
// metric's: every series of the name has it, so the one written first
// carries it. One the script gives the first series stays that one.
func TestAHelpTextGivenALaterSeriesIsTheMetrics(t *testing.T) {
	requirePython(t)
	set, err := runWorkerScript(t, workerCollector("help", `metric(name="jobs", value=1, labels={"q": "a"})
metric(name="jobs", value=2, labels={"q": "b"}, help="Jobs waiting")
metric(name="jobs", value=3, labels={"q": "c"}, help="another text")
metric(name="other", value=1)
metric(name="first", value=1, labels={"q": "a"}, help="said first")
metric(name="first", value=2, labels={"q": "b"})`))
	if err != nil {
		t.Fatal(err)
	}
	var helps []string
	for _, m := range set.Metrics {
		helps = append(helps, m.Help)
	}
	if got := strings.Join(helps, "|"); got != "Jobs waiting|Jobs waiting|another text||said first|said first" {
		t.Errorf("the help texts are %s", got)
	}
}

// A script reads the scrape's clock as scrape_time and last_success, in
// seconds since the epoch, and so does a jq expression as $scrape_time and
// $last_success; without a last success, and without a clock, each is None
// and null.
func TestAScriptAndAnExpressionReadTheScrapesClock(t *testing.T) {
	requirePython(t)
	now, since := time.Unix(1767323045, 250000000), time.Unix(1767323000, 0)
	script := workerCollector("clock", `metric(name="at", value=-1 if scrape_time is None else scrape_time)
metric(name="last", value=-1 if last_success is None else last_success)`)
	rules := model.Collector{Name: "clock", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{
		{Name: "at", Expression: "$scrape_time // -1"}, {Name: "last", Expression: "$last_success // -1"}}}
	for name, tc := range map[string]struct {
		ctx      context.Context
		at, last float64
	}{
		"a clock and a last success": {WithScrapeClock(context.Background(), now, since), 1767323045.25, 1767323000},
		"no last success":            {WithScrapeClock(context.Background(), now, time.Time{}), 1767323045.25, -1},
		"no clock":                   {context.Background(), -1, -1},
	} {
		r := &fetch.HTTPResponse{Body: []byte(`{}`)}
		d := &decode.Decoded{Kind: "json", Data: map[string]any{}, Raw: r.Body}
		for kind, c := range map[string]*model.Collector{"python": script, "jq": &rules} {
			set, err := Transform(tc.ctx, d, r, c, "python3")
			if err != nil || len(set.Metrics) != 2 || set.Metrics[0].Value != tc.at || set.Metrics[1].Value != tc.last {
				t.Errorf("%s, %s: %+v, %v; want %v and %v", name, kind, set, err, tc.at, tc.last)
			}
		}
	}
}
