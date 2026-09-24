package decode

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// graphiteNowIs fixes the decoder's clock for a test.
func graphiteNowIs(t *testing.T, now time.Time) {
	t.Helper()
	previous := graphiteNow
	graphiteNow = func() time.Time { return now }
	t.Cleanup(func() { graphiteNow = previous })
}

// decodeGraphiteBody decodes body with the graphite decoder and settings.
func decodeGraphiteBody(t *testing.T, body string, settings model.GraphiteConfig) []any {
	t.Helper()
	c := &model.Collector{Name: "graphite", Decoder: model.DecoderConfig{Type: "graphite"}, Response: model.ResponseConfig{Graphite: settings}}
	d, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}, c)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != "graphite" {
		t.Fatalf("decoded as %q", d.Kind)
	}
	return d.Data.(map[string]any)["series"].([]any)
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The render API's answer becomes one document per series: its path split
// into segments, its tags, the newest value and its time, and every point
// with a value, oldest first. Nulls are left out, and so is a series with
// nothing but nulls.
func TestGraphiteRenderJSON(t *testing.T) {
	graphiteNowIs(t, time.Unix(1727000100, 0))
	series := decodeGraphiteBody(t, `[
	  {"target": "app.web01.requests", "tags": {"name": "app.web01.requests"}, "datapoints": [[40, 1726999940], [42.5, 1727000000], [null, 1727000060]]},
	  {"target": "cpu.load;env=prod;host=a", "datapoints": [[0.7, 1727000000]]},
	  {"target": "total", "tags": {"name": "app.*.requests", "aggregatedBy": "sum"}, "datapoints": [[82, 1727000000]]},
	  {"target": "app.web02.requests", "datapoints": [[null, 1727000000], [null, 1727000060]]},
	  {"target": "empty", "datapoints": []}
	]`, model.GraphiteConfig{})
	want := `[` +
		`{"path":"app.web01.requests","points":[[40,1726999940],[42.5,1727000000]],"segments":["app","web01","requests"],"tags":{"name":"app.web01.requests"},"time":1727000000,"value":42.5},` +
		`{"path":"cpu.load","points":[[0.7,1727000000]],"segments":["cpu","load"],"tags":{"env":"prod","host":"a","name":"cpu.load"},"time":1727000000,"value":0.7},` +
		`{"path":"total","points":[[82,1727000000]],"segments":["total"],"tags":{"aggregatedBy":"sum","name":"app.*.requests"},"time":1727000000,"value":82}]`
	if got := asJSON(t, series); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if series := decodeGraphiteBody(t, "[]", model.GraphiteConfig{}); len(series) != 0 {
		t.Fatalf("an empty answer: %v", series)
	}
}

func TestGraphiteRenderJSONErrors(t *testing.T) {
	for body, want := range map[string]string{
		`[{"target":"a","datapoints":[[1]]}]`:            `series "a" point 0 has 1 elements`,
		`[{"target":"a","datapoints":[["x",1]]}]`:        `series "a" point 0 is [x 1], not [value, timestamp] as numbers`,
		`[{"target":"a","datapoints":[[1,null]]}]`:       `not [value, timestamp] as numbers`,
		`[{"target":"","datapoints":[]}]`:                `series 0: the series "" has no path`,
		`[{"target":"a;env","datapoints":[]}]`:           `has a tag "env" that is not name=value`,
		`[{"target":"a","datapoints":[[1,2]]}`:           "graphite render JSON",
		`[{"target":"a","datapoints":{"not":"a list"}}]`: "graphite render JSON",
	} {
		c := &model.Collector{Decoder: model.DecoderConfig{Type: "graphite"}}
		if _, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}, c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err=%v, want %q", body, err, want)
		}
	}
}

// response.graphite.value reduces the points, and max_age leaves out a
// series whose newest point is older.
func TestGraphiteValueAndMaxAge(t *testing.T) {
	graphiteNowIs(t, time.Unix(1000, 0))
	body := `[{"target":"a.b","datapoints":[[3,700],[9,800],[null,900],[6,850]]},{"target":"old","datapoints":[[1,100]]}]`
	for how, want := range map[string]float64{"": 6, "last": 6, "max": 9, "min": 3, "sum": 18, "avg": 6} {
		series := decodeGraphiteBody(t, body, model.GraphiteConfig{Value: how})
		first := series[0].(map[string]any)
		if first["value"] != want || first["time"] != 850.0 {
			t.Errorf("%q: value %v at %v, want %v at 850", how, first["value"], first["time"], want)
		}
	}
	series := decodeGraphiteBody(t, body, model.GraphiteConfig{MaxAge: model.Duration(200 * time.Second)})
	if len(series) != 1 || series[0].(map[string]any)["path"] != "a.b" {
		t.Fatalf("max_age 200s at 1000 kept %s", asJSON(t, series))
	}
	if series := decodeGraphiteBody(t, body, model.GraphiteConfig{MaxAge: model.Duration(100 * time.Second)}); len(series) != 0 {
		t.Fatalf("max_age 100s at 1000 kept %s", asJSON(t, series))
	}
	c := &model.Collector{Decoder: model.DecoderConfig{Type: "graphite"}, Response: model.ResponseConfig{Graphite: model.GraphiteConfig{Value: "median"}}}
	if _, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}, c); err == nil || !strings.Contains(err.Error(), `unknown response.graphite.value "median"`) {
		t.Fatalf("err=%v", err)
	}
}

// Carbon lines: a series per path and tags, whatever order the tags are
// written in, its lines its points; a line without a timestamp, or with -1,
// is now; NaN and infinities are left out like nulls; comments and blank
// lines are skipped.
func TestGraphiteCarbonLines(t *testing.T) {
	graphiteNowIs(t, time.Unix(2000, 0))
	series := decodeGraphiteBody(t, strings.Join([]string{
		"# written by the nightly job",
		"backup.duration_seconds 42 1000",
		"",
		"cpu.load;host=a;env=prod 0.5 1000\r",
		"backup.duration_seconds 40 1500",
		"cpu.load;env=prod;host=a 0.7 1100",
		"cpu.load;env=prod;host=b 0.1 1100",
		"queue.depth 7",
		"queue.lag 3 -1",
		"broken.gauge NaN 1000",
		"broken.gauge +Inf 1100",
	}, "\n"), model.GraphiteConfig{})
	want := `[` +
		`{"path":"backup.duration_seconds","points":[[42,1000],[40,1500]],"segments":["backup","duration_seconds"],"tags":{"name":"backup.duration_seconds"},"time":1500,"value":40},` +
		`{"path":"cpu.load","points":[[0.5,1000],[0.7,1100]],"segments":["cpu","load"],"tags":{"env":"prod","host":"a","name":"cpu.load"},"time":1100,"value":0.7},` +
		`{"path":"cpu.load","points":[[0.1,1100]],"segments":["cpu","load"],"tags":{"env":"prod","host":"b","name":"cpu.load"},"time":1100,"value":0.1},` +
		`{"path":"queue.depth","points":[[7,2000]],"segments":["queue","depth"],"tags":{"name":"queue.depth"},"time":2000,"value":7},` +
		`{"path":"queue.lag","points":[[3,2000]],"segments":["queue","lag"],"tags":{"name":"queue.lag"},"time":2000,"value":3}]`
	if got := asJSON(t, series); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Lines written for one time: the last one written is the newest.
	series = decodeGraphiteBody(t, "a.b 1 1000\na.b 2 1000\n", model.GraphiteConfig{})
	if value := series[0].(map[string]any)["value"]; value != 2.0 {
		t.Fatalf("value %v, want the last line's 2", value)
	}
	// max_age applies to carbon lines too.
	if series := decodeGraphiteBody(t, "a.b 1 1000\nc.d 1 1990\n", model.GraphiteConfig{MaxAge: model.Duration(time.Minute)}); asJSON(t, series) != `[{"path":"c.d","points":[[1,1990]],"segments":["c","d"],"tags":{"name":"c.d"},"time":1990,"value":1}]` {
		t.Fatalf("max_age kept %s", asJSON(t, series))
	}
}

func TestGraphiteCarbonLineErrors(t *testing.T) {
	for body, want := range map[string]string{
		"a.b 1 1000\na.b\n":           `carbon line 2: "a.b" has 1 fields`,
		"a.b 1 1000 extra\n":          "carbon line 1: \"a.b 1 1000 extra\" has 4 fields",
		"a.b one 1000\n":              `carbon line 1: the value "one" is not a number`,
		"a.b 1 yesterday\n":           `carbon line 1: the timestamp "yesterday" is not a number of Unix seconds`,
		"a.b;env 1 1000\n":            `carbon line 1: the series "a.b;env" has a tag "env" that is not name=value`,
		"\n\n;env=prod 1 1000\n":      `carbon line 3: the series ";env=prod" has no path`,
		"a.b 1 1000\na.b;=x 1 1000\n": `carbon line 2: the series "a.b;=x" has a tag "=x"`,
	} {
		c := &model.Collector{Decoder: model.DecoderConfig{Type: "graphite"}}
		if _, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}, c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err=%v, want %q", body, err, want)
		}
	}
}

// decoder.type auto reads a file of carbon lines by its extension, and a body
// without a content type that says by its content: every line a path and two
// numbers, and some path with a dot or a tag, which a Prometheus sample with
// a timestamp cannot have.
func TestGraphiteIsRecognised(t *testing.T) {
	if got := detectFormat(&fetch.HTTPResponse{Body: []byte("anything"), Headers: http.Header{"Content-Type": {fetch.GraphiteContentType}}}); got != "graphite" {
		t.Fatalf("by content type: %s", got)
	}
	for body, want := range map[string]string{
		"app.web01.requests 42 1727000000\n":         "graphite",
		"# nightly\na.b 1 1000\n\nc.d 2.5 1000.5\n":  "graphite",
		"cpu;host=a 1 1000\n":                        "graphite",
		"requests_total 42 1727000000000\n":          "text",
		"a.b 1 1000\nsomething else\n":               "text",
		"a.b 1\n":                                    "text",
		`up{job="a.b"} 1 1000` + "\n":                "text",
		"a.b one 1000\n":                             "text",
		"# only a comment\n":                         "text",
		`[{"target":"a.b","datapoints":[[1,1000]]}]`: "json",
	} {
		if got := detectFormat(&fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}); got != want {
			t.Errorf("%q: %s, want %s", body, got, want)
		}
	}
	// With auto, a file of carbon lines decodes as Graphite series.
	c := &model.Collector{Decoder: model.DecoderConfig{Type: "auto"}}
	d, err := Decode(&fetch.HTTPResponse{Body: []byte("a.b 1 1000\n"), Headers: http.Header{"Content-Type": {fetch.GraphiteContentType}}}, c)
	if err != nil || d.Kind != "graphite" || !reflect.DeepEqual(d.Data.(map[string]any)["series"].([]any)[0].(map[string]any)["segments"], []any{"a", "b"}) {
		t.Fatalf("d=%+v err=%v", d, err)
	}
}

// The tags always hold name, also when the render API's tags leave it out.
func TestGraphiteTagsAlwaysHoldName(t *testing.T) {
	graphiteNowIs(t, time.Unix(2000, 0))
	series := decodeGraphiteBody(t, `[{"target":"sumSeries(a.*.b)","tags":{"aggregatedBy":"sum"},"datapoints":[[1,1990]]}]`, model.GraphiteConfig{})
	if tags := series[0].(map[string]any)["tags"]; !reflect.DeepEqual(tags, map[string]any{"name": "sumSeries(a.*.b)", "aggregatedBy": "sum"}) {
		t.Fatalf("tags %v", tags)
	}
}

// Times keep their fractions, and a time far in the future does not wrap
// around into the past, as multiplying it into nanoseconds would.
func TestGraphiteTimes(t *testing.T) {
	if got := unixTime(1727000000.5); !got.Equal(time.Unix(1727000000, 500_000_000)) {
		t.Fatalf("got %v", got)
	}
	graphiteNowIs(t, time.Unix(1727000100, 0))
	series := decodeGraphiteBody(t, `[{"target":"a.b","datapoints":[[1,1727000000000]]},{"target":"c.d","datapoints":[[1,1726990000]]}]`, model.GraphiteConfig{MaxAge: model.Duration(time.Hour)})
	if len(series) != 1 || series[0].(map[string]any)["path"] != "a.b" {
		t.Fatalf("kept %s", asJSON(t, series))
	}
	// A carbon line in milliseconds is refused, naming the line.
	c := &model.Collector{Decoder: model.DecoderConfig{Type: "graphite"}}
	_, err := Decode(&fetch.HTTPResponse{Body: []byte("a.b 1 1727000000\na.b 1 1727000000000\n"), Headers: http.Header{}}, c)
	if err == nil || !strings.Contains(err.Error(), `carbon line 2: the timestamp "1727000000000" is in milliseconds, it seems; carbon lines take Unix seconds`) {
		t.Fatalf("err=%v", err)
	}
}

// A graphite collector's answer is render JSON, or it is refused as what it
// is, not read as carbon lines.
func TestAGraphiteAnswerMustBeRenderJSON(t *testing.T) {
	c := &model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeGraphite}, Decoder: model.DecoderConfig{Type: "graphite"}}
	for body, want := range map[string]string{
		"<!DOCTYPE html><html><head><title>Sign in</title></head><body>" + strings.Repeat("x", 100): `the Graphite server did not answer with render JSON, a list of series; the answer starts "<!DOCTYPE html><html><head><title>Sign in</title></head><body>xxxxxxxxxxxxxxxxxx"…`,
		`{"error": "unknown function"}`: `the answer is "{\"error\": \"unknown function\"}"`,
		"  \n":                          "the answer is empty",
		"a.b 1 1727000000\n":            `the answer is "a.b 1 1727000000"`,
	} {
		if _, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}, c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err=%v, want %q", body, err, want)
		}
	}
}

// A series answered twice — two expressions matching it — is kept once and
// counted; the same path with other points, or other tags, is another series.
func TestGraphiteDuplicateSeries(t *testing.T) {
	graphiteNowIs(t, time.Unix(2000, 0))
	d, err := Decode(&fetch.HTTPResponse{Headers: http.Header{}, Body: []byte(`[
	  {"target":"a.b","datapoints":[[1,1990]]},
	  {"target":"a.b","datapoints":[[1,1990]]},
	  {"target":"a.b","datapoints":[[2,1990]]},
	  {"target":"a.b;env=x","datapoints":[[1,1990]]},
	  {"target":"c.d","datapoints":[[null,1990]]},
	  {"target":"e.f","datapoints":[[1,10]]}
	]`)}, &model.Collector{Decoder: model.DecoderConfig{Type: "graphite"}, Response: model.ResponseConfig{Graphite: model.GraphiteConfig{MaxAge: model.Duration(time.Minute)}}})
	if err != nil {
		t.Fatal(err)
	}
	if series := d.Data.(map[string]any)["series"].([]any); len(series) != 3 {
		t.Fatalf("series %s", asJSON(t, series))
	}
	if *d.Graphite != (GraphiteReport{NoPoints: 1, Stale: 1, Duplicates: 1}) || d.Graphite.LeftOut() != 3 {
		t.Fatalf("report %+v", *d.Graphite)
	}
}

// With invalid_lines: skip, a carbon line that cannot be read is left out,
// counted, and the first one described; the rest are read.
func TestGraphiteSkipsInvalidCarbonLines(t *testing.T) {
	graphiteNowIs(t, time.Unix(2000, 0))
	c := &model.Collector{Decoder: model.DecoderConfig{Type: "graphite"}, Response: model.ResponseConfig{Graphite: model.GraphiteConfig{InvalidLines: "skip"}}}
	d, err := Decode(&fetch.HTTPResponse{Headers: http.Header{}, Body: []byte("a.b 1 1990\na.b 2 199\nc.d one 1990\ntorn.li\ne.f 3 1990\n")}, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := asJSON(t, d.Data.(map[string]any)["series"]); !strings.Contains(got, `"path":"a.b"`) || !strings.Contains(got, `"path":"e.f"`) || strings.Contains(got, "c.d") {
		t.Fatalf("series %s", got)
	}
	if d.Graphite.SkippedLines != 2 || d.Graphite.FirstSkipped != `carbon line 3: the value "one" is not a number` {
		t.Fatalf("report %+v", *d.Graphite)
	}
	// The default fails on the first.
	c.Response.Graphite.InvalidLines = ""
	if _, err := Decode(&fetch.HTTPResponse{Headers: http.Header{}, Body: []byte("a.b 1 1990\nc.d one 1990\n")}, c); err == nil || !strings.Contains(err.Error(), "carbon line 2") {
		t.Fatalf("err=%v", err)
	}
}
