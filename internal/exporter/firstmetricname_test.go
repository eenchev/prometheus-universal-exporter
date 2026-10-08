package exporter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// utf8Warning is the message of the warning of repaired UTF-8.
const utf8Warning = "label values or help text were not valid UTF-8; the invalid bytes were replaced with U+FFFD. If the target uses another encoding without declaring it, set response.charset"

// bareServer is an exporter of no collector, which logs nowhere until a
// test gives it a logger.
func bareServer() *Server {
	quiet := slog.New(slog.DiscardHandler)
	return NewServer(config.NewManager(&model.Config{}, "", quiet), "python3", quiet)
}

// timelessLogger writes to out as JSON, or as text, from debug level on and
// without the time, so that two lines of the same thing are the same bytes.
func timelessLogger(out io.Writer, json bool) *slog.Logger {
	options := &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}
	if json {
		return slog.New(slog.NewJSONHandler(out, options))
	}
	return slog.New(slog.NewTextHandler(out, options))
}

// noteUTF8RepairsAsItWas is noteUTF8Repairs as it was when its warning named
// the first metric whole, however long the name.
func (s *Server) noteUTF8RepairsAsItWas(ctx context.Context, repaired utf8Repairs, rec statsRecorder, c *model.Collector, target, keyTarget, file string) {
	key := aspectKey(c.Name, keyTarget, file, utf8Aspect)
	changed, first := repaired.count, repaired.first
	if changed == 0 {
		s.tripRecovered(ctx, rec.read, key, "output is valid UTF-8 again", "collector", c.Name, "target", target)
		return
	}
	rec.update(func(x *serverStats) { x.invalidUTF8 += changed })
	s.tripFailed(ctx, rec.read, slog.LevelWarn, key, "label values or help text were not valid UTF-8; the invalid bytes were replaced with U+FFFD. If the target uses another encoding without declaring it, set response.charset", "utf8", nil, "collector", c.Name, "target", target, "values", changed, "first_metric", first)
}

// The warning of repaired UTF-8 names the first metric repaired before the
// set is validated, when the name is as long as the target made it. A name
// of 200 bytes or fewer is logged as it was, the line the same bytes as the
// line that named every name whole (noteUTF8RepairsAsItWas), as JSON and as
// text, for a response's and for a directory's file's: ordinary names, an
// empty one, names with spaces and quotes, of bytes that are no UTF-8, of
// exactly 200 bytes, and 400 generated ones. A longer name is logged by its
// first 200 bytes, or the one to three fewer that end between two
// characters, and its length, as an error shows a name: one of 201 bytes,
// one with a character of two, three or four bytes across byte 200, and one
// of a megabyte, whose line was a megabyte and is under 700 bytes.
func TestTheUTF8WarningNamesItsFirstMetricByItsFirst200BytesAndItsLength(t *testing.T) {
	c := &model.Collector{Name: "depot"}
	const target = "http://depot.internal/metrics"
	logged := func(json bool, file, name string, was bool) string {
		server := bareServer()
		var out bytes.Buffer
		server.logger = timelessLogger(&out, json)
		note := server.noteUTF8Repairs
		if was {
			note = server.noteUTF8RepairsAsItWas
		}
		shown := target
		if file != "" {
			shown = file
		}
		note(context.Background(), utf8Repairs{count: 3, first: name}, statsRecorder{}, c, shown, target, file)
		return out.String()
	}
	names := []string{"v", "depot_pallets", "", "queue depth", `queue "depth"`, "größe_€", "caf\xe9", strings.Repeat("\xff", 200), strings.Repeat("a", 199), strings.Repeat("a", 200), strings.Repeat("a", 198) + "é", strings.Repeat("€", 66) + "ab"}
	random := rand.New(rand.NewPCG(41, 1))
	alphabet := []string{"a", "_", ":", " ", `"`, "=", "é", "€", "𝄞", "\xff", "\n"}
	for len(names) < 412 {
		var b strings.Builder
		for length := random.IntN(201); b.Len() < length; {
			if part := alphabet[random.IntN(len(alphabet))]; b.Len()+len(part) <= 200 {
				b.WriteString(part)
			}
		}
		names = append(names, b.String())
	}
	for _, name := range names {
		for _, json := range []bool{true, false} {
			for _, file := range []string{"", "pallets.prom"} {
				got, want := logged(json, file, name, false), logged(json, file, name, true)
				if got != want || !strings.Contains(got, "first_metric") {
					t.Fatalf("a first metric of %d bytes, %q, is logged as\n%s\nand was as\n%s", len(name), name, got, want)
				}
			}
		}
	}
	megabyte := strings.Repeat("n", 1<<20)
	for _, c := range []struct{ what, name, want string }{
		{"a name of 201 bytes", strings.Repeat("a", 201), strings.Repeat("a", 200) + "... (201 bytes)"},
		{"a character of two bytes across byte 200", strings.Repeat("a", 199) + "éz", strings.Repeat("a", 199) + "... (202 bytes)"},
		{"a character of three bytes across byte 200", strings.Repeat("a", 198) + "€z", strings.Repeat("a", 198) + "... (202 bytes)"},
		{"a character of four bytes across byte 200", strings.Repeat("a", 197) + "𝄞z", strings.Repeat("a", 197) + "... (202 bytes)"},
		{"a character that ends at byte 200", strings.Repeat("a", 198) + "éz", strings.Repeat("a", 198) + "é... (201 bytes)"},
		{"a name of a megabyte", megabyte, strings.Repeat("n", 200) + "... (1048576 bytes)"},
	} {
		line := logged(true, "", c.name, false)
		// What the line would be for a name that is the text wanted: the
		// same line, so nothing else of it changed.
		if want := logged(true, "", c.want, true); line != want || !strings.Contains(line, `"first_metric":"`+c.want+`"`) {
			t.Errorf("%s is logged as %d bytes,\n%.900s\nwant\n%s", c.what, len(line), line, want)
		}
		if text := logged(false, "", c.name, false); !strings.Contains(text, ` first_metric="`+c.want+`"`+"\n") {
			t.Errorf("%s is logged as text in %d bytes,\n%.900s\nwant it to end first_metric=%q", c.what, len(text), text, c.want)
		}
	}
	if was, is := len(logged(true, "", megabyte, true)), len(logged(true, "", megabyte, false)); was < 1<<20 || is > 700 {
		t.Errorf("the line of a name of a megabyte was %d bytes and is %d, want over a megabyte and under 700", was, is)
	}
}

// What the warning of a name of a megabyte costs and leaves is no part of
// the name: the line is made in a few kilobytes where it was made in more
// than the megabyte, and the failure log remembers the warning by the
// collector, the address and the file, under a key of some fifty bytes with
// no error's text, as it did. The name is no part of that key, so the
// warning of another first metric, however it starts, is the same failure
// again and a repeat, logged at debug level, and the output that is valid
// again recovers it once, with every scrape counted.
func TestTheUTF8WarningOfALongNameCostsAndLeavesNothingOfTheName(t *testing.T) {
	server := bareServer()
	var out bytes.Buffer
	server.logger = timelessLogger(&out, true)
	c := &model.Collector{Name: "depot"}
	const target = "http://depot.internal/metrics"
	note := func(count uint64, first string) {
		server.noteUTF8Repairs(context.Background(), utf8Repairs{count: count, first: first}, statsRecorder{}, c, target, target, "")
	}
	megabyte := strings.Repeat("n", 1<<20)
	const most = 16 << 10
	if size := alloctest.BytesAtMost(5, most, func() { note(1, megabyte) }); size > most {
		t.Errorf("the warning of a name of a megabyte is logged in %d bytes allocated, want at most %d", size, most)
	}
	out.Reset()
	server.failures = newFailureLog()
	note(1, megabyte)
	note(1, megabyte+"n")
	note(1, strings.Repeat("o", 300))
	note(2, "short")
	records := loggedRecords(t, &out)
	if len(records) != 4 {
		t.Fatalf("four scrapes logged %d lines", len(records))
	}
	for i, record := range records {
		level, repeat := "WARN", any(nil)
		if i > 0 {
			level, repeat = "DEBUG", true
		}
		if record["level"] != level || record["repeat"] != repeat || record["msg"] != utf8Warning {
			t.Errorf("scrape %d is logged at %v with repeat %v, want %s and %v: %.300v", i+1, record["level"], record["repeat"], level, repeat, record)
		}
	}
	for i, want := range []string{strings.Repeat("n", 200) + "... (1048576 bytes)", strings.Repeat("n", 200) + "... (1048577 bytes)", strings.Repeat("o", 200) + "... (300 bytes)", "short"} {
		if records[i]["first_metric"] != want {
			t.Errorf("scrape %d names its first metric %.300v, want %q", i+1, records[i]["first_metric"], want)
		}
	}
	key := aspectKey("depot", target, "", utf8Aspect)
	server.failures.mu.Lock()
	kept := 0
	for bytes, st := range server.failures.entries {
		kept += len(bytes) + len(st.key.bytes) + len(st.key.collector) + len(st.key.target) + len(st.trip) + len(st.stage) + len(st.err)
	}
	st := server.failures.entries[key.bytes]
	if len(server.failures.entries) != 1 || st == nil || st.stage != "utf8" || st.err != "" || st.failures != 4 || kept > 200 {
		t.Errorf("the failure log remembers %d failures in %d bytes, the warning as %+v, want one of four scrapes in at most 200 bytes, with no text", len(server.failures.entries), kept, st)
	}
	server.failures.mu.Unlock()
	out.Reset()
	note(0, "")
	if records := loggedRecords(t, &out); len(records) != 1 || records[0]["msg"] != "output is valid UTF-8 again" || records[0]["failures"] != float64(4) {
		t.Errorf("the output that is valid again is logged as %.300v, want one recovery of 4 failures", records)
	}
}

// A static target's metric left out of the static targets endpoint is named
// in its lines as an error names a metric: one of 200 bytes or fewer whole,
// as it was, and a longer one, which a collector whose
// limits.max_metric_name_length was raised lets through, by its first 200
// bytes and its length, where the line was as long as the name; so is the
// family it clashes with. The clash is remembered under the whole name, as
// it was: two names that are the same for 200 bytes are two clashes, each
// logged once and each said to be back on the endpoint once.
func TestAStaticTargetsMetricLeftOutIsNamedByItsFirst200BytesAndItsLength(t *testing.T) {
	server := bareServer()
	var out bytes.Buffer
	server.logger = timelessLogger(&out, true)
	const left, back = "static target metric left out of the static targets endpoint", "static target metric back on the static targets endpoint"
	within, first, second := strings.Repeat("w", 200), strings.Repeat("m", 100<<10)+"_a", strings.Repeat("m", 100<<10)+"_b"
	family := strings.Repeat("f", 300)
	gauge := func(name string) model.Metric { return model.Metric{Name: name, Type: model.GaugeMetricType, Value: 1} }
	counter := func(name string) model.Metric {
		return model.Metric{Name: name, Type: model.CounterMetricType, Value: 1}
	}
	histogram := model.Metric{Name: family, Type: model.HistogramMetricType, Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 1}}, Sum: 1, Count: 1}}
	a := namedSet{name: "a", set: model.MetricSet{Metrics: []model.Metric{gauge("shared"), gauge(within), gauge(first), gauge(second), histogram}}}
	b := namedSet{name: "b", set: model.MetricSet{Metrics: []model.Metric{counter("shared"), counter(within), counter(first), counter(second), gauge(family + "_count")}}}
	fixed := namedSet{name: "b", set: model.MetricSet{Metrics: []model.Metric{gauge("shared")}}}
	// lines are the metrics the lines of msg name, each with the family it
	// clashes with when the line names one, sorted.
	lines := func(msg string) []string {
		t.Helper()
		var named []string
		for _, record := range loggedRecords(t, &out) {
			if record["msg"] != msg || record["level"] == "DEBUG" {
				continue
			}
			name, _ := record["metric"].(string)
			if with, clashes := record["clashes_with"].(string); clashes {
				name += " with " + with
			}
			named = append(named, name)
		}
		if size := out.Len(); size > 8<<10 {
			t.Errorf("the lines of %q are %d bytes, want a few hundred each", msg, size)
		}
		out.Reset()
		sort.Strings(named)
		return named
	}
	cut := strings.Repeat("m", 200) + fmt.Sprintf("... (%d bytes)", len(first))
	series, owner := strings.Repeat("f", 200)+"... (306 bytes)", strings.Repeat("f", 200)+"... (300 bytes)"
	server.mergeStaticTargets([]namedSet{a, b})
	if got, want := lines(left), []string{series + " with " + owner, cut, cut, "shared", within}; !slices.Equal(got, want) {
		t.Errorf("the metrics left out are named %.900q, want %.900q", got, want)
	}
	server.mergeStaticTargets([]namedSet{a, b})
	if got := lines(left); len(got) != 0 {
		t.Errorf("the same clashes on the next read are logged again: %.900q", got)
	}
	server.failures.mu.Lock()
	if len(server.failures.entries) != 5 || server.failures.entries[staticClashKey("b", first).bytes] == nil || server.failures.entries[staticClashKey("b", second).bytes] == nil {
		t.Errorf("the failure log remembers %d clashes, want five, each under its whole name", len(server.failures.entries))
	}
	server.failures.mu.Unlock()
	server.mergeStaticTargets([]namedSet{a, fixed})
	if got, want := lines(back), []string{series, cut, cut, "shared", within}; !slices.Equal(got, want) {
		t.Errorf("the metrics back on the endpoint are named %.900q, want %.900q", got, want)
	}
}

// A metric name an OTLP export had as two kinds under one resource is named
// in its line as an error names a metric: one of 200 bytes or fewer whole,
// as it was, and a longer one by its first 200 bytes and its length. The
// clash is remembered under the whole name, as it was: two names that are
// the same for 200 bytes are two clashes, each logged once.
func TestAnOTLPNameOfTwoKindsIsNamedByItsFirst200BytesAndItsLength(t *testing.T) {
	server := bareServer()
	var out bytes.Buffer
	server.logger = timelessLogger(&out, true)
	identity := otlpResourceIdentity{ServiceName: "svc"}
	within, first, second := strings.Repeat("w", 200), strings.Repeat("m", 100<<10)+"_a", strings.Repeat("m", 100<<10)+"_b"
	var clashes []otlpNameClash
	for _, name := range []string{"m", within, first, second} {
		clashes = append(clashes, otlpNameClash{name: name, kept: otlpKindSum, leftOut: []int{otlpKindGauge}, points: 1})
	}
	server.logOTLPNameClashes(identity, "resource", clashes)
	if size := out.Len(); size > 4<<10 {
		t.Errorf("the lines of four names are %d bytes, want a few hundred each", size)
	}
	var named []string
	for _, record := range loggedRecords(t, &out) {
		name, _ := record["metric"].(string)
		named = append(named, name)
		if record["level"] != "WARN" || record["kind"] != "sum" || record["left_out_kind"] != "gauge" || record["service_name"] != "svc" {
			t.Errorf("a name of two kinds is logged as %.400v", record)
		}
	}
	cut := strings.Repeat("m", 200) + fmt.Sprintf("... (%d bytes)", len(first))
	if want := []string{"m", within, cut, cut}; fmt.Sprint(named) != fmt.Sprint(want) {
		t.Errorf("the names of two kinds are logged as %.900q, want %.900q", named, want)
	}
	out.Reset()
	server.logOTLPNameClashes(identity, "resource", clashes)
	for _, record := range loggedRecords(t, &out) {
		if record["level"] != "DEBUG" || record["repeat"] != true {
			t.Errorf("the same name on the next export is logged again: %.400v", record)
		}
	}
	server.failures.mu.Lock()
	if len(server.failures.entries) != 4 || server.failures.entries[otlpNameClashKey("resource", first).bytes] == nil || server.failures.entries[otlpNameClashKey("resource", second).bytes] == nil {
		t.Errorf("the failure log remembers %d names, want four, each under its whole name", len(server.failures.entries))
	}
	server.failures.mu.Unlock()
}
