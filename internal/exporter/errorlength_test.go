package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// loggedRecords are the lines of a JSON log, each as the object it is.
func loggedRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not JSON: %.300s", line)
		}
		records = append(records, record)
	}
	return records
}

// The failure log remembers a failure by no more than the 2,000 bytes of
// any failure whoever reports it: an error of megabytes that reaches the log
// unbounded is remembered by the first 2,000 bytes of what it is recognised
// by, ending with the mark for its length, so the same error again, longer
// or shorter, is a repeat, and nothing of the megabytes is kept. A failure
// within the bound is remembered by what it was, and one bounded before it
// is reported by the same text as the log would have cut it to.
func TestTheFailureLogRemembersAFailureWithinTheBound(t *testing.T) {
	log := newFailureLog()
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	remembered := func(key subjectKey) string {
		log.mu.Lock()
		defer log.mu.Unlock()
		return log.entries[key.bytes].err
	}
	long := func(size int) error {
		return model.Errorf("row %d: the target said %s", model.Position(size), strings.Repeat("s", size))
	}
	key := probeFailureKey("c", "http://db.internal/", "")
	log.failed(logger, slog.LevelError, key, "probe failed", "transform", long(1<<20))
	kept := remembered(key)
	if want := ("row #: the target said " + strings.Repeat("s", model.MaxFailureBytes))[:model.MaxFailureBytes-len("... (# bytes)")] + "... (# bytes)"; kept != want {
		t.Errorf("a failure of a megabyte is remembered by %d bytes, %.60q ... %q", len(kept), kept, kept[max(0, len(kept)-30):])
	}
	log.failed(logger, slog.LevelError, key, "probe failed", "transform", long(1<<19))
	log.mu.Lock()
	if st := log.entries[key.bytes]; st.failures != 2 || st.suppressed != 1 {
		t.Errorf("the same failure half as long is counted as %d failures, %d of them repeats", st.failures, st.suppressed)
	}
	log.mu.Unlock()
	bounded := probeFailureKey("c", "http://bounded.internal/", "")
	log.failed(logger, slog.LevelError, bounded, "probe failed", "transform", model.BoundedFailure(long(1<<20)))
	if got := remembered(bounded); got != kept {
		t.Errorf("the failure bounded before it is reported is remembered by %.60q ... %q, and unbounded by %.60q ... %q", got, got[max(0, len(got)-30):], kept, kept[max(0, len(kept)-30):])
	}
	short := probeFailureKey("c", "http://short.internal/", "")
	log.failed(logger, slog.LevelError, short, "probe failed", "transform", long(40))
	if got, want := remembered(short), "row #: the target said "+strings.Repeat("s", 40); got != want {
		t.Errorf("a short failure is remembered by %q, want %q", got, want)
	}
}

// checkFileFamiliesBeforeTheRestWereCounted is checkFileFamilies as it was
// when its error named every metric that clashes, however many.
func checkFileFamiliesBeforeTheRestWereCounted(set *model.MetricSet, families map[string][]model.Metric, typeFrom map[string]string) *fileFailure {
	var conflicts []string
	reported := map[string]bool{}
	for _, m := range set.Metrics {
		if existing, ok := families[m.Name]; ok && existing[0].Type != m.Type && !reported[m.Name] {
			reported[m.Name] = true
			conflicts = append(conflicts, fmt.Sprintf("%s is a %s here but a %s in %s", m.Name, m.Type, existing[0].Type, typeFrom[m.Name]))
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	return &fileFailure{"merge", fmt.Errorf("%s: a metric has one type across the directory's files", strings.Join(conflicts, "; "))}
}

// A file whose metrics clash in type with another file's is refused in the
// words it was while the clashes it names are within 1,200 bytes together:
// for generated files of up to two hundred families, some typed otherwise than in
// the file before, the failure is what naming every clash gave, to the
// letter, in its stage and in what it is recognised by, and a file that
// clashes in nothing is refused by neither. Past that, the first clashes are
// named as they were, the rest are counted, and the error goes on to say
// what a clash is; it is recognised with the mark for the count, so the same
// files clashing in more families are one failure to the log.
func TestAFileIsRefusedForTheClashesItNamesAndTheRestCounted(t *testing.T) {
	random := rand.New(rand.NewPCG(36, 7))
	types := []model.MetricType{model.GaugeMetricType, model.CounterMetricType, model.UntypedMetricType}
	as, counted := 0, 0
	for range alloctest.UnlessRaced(1500, 300) {
		families, typeFrom := map[string][]model.Metric{}, map[string]string{}
		var set model.MetricSet
		for i := range random.IntN(200) {
			name := fmt.Sprintf("family_%d_%s", i, strings.Repeat("n", random.IntN(40)))
			first := types[random.IntN(len(types))]
			families[name], typeFrom[name] = []model.Metric{{Name: name, Type: first}}, "a.prom"
			here := first
			if random.IntN(3) == 0 {
				here = types[random.IntN(len(types))]
			}
			// A family has several series, and is named once.
			for range 1 + random.IntN(2) {
				set.Metrics = append(set.Metrics, model.Metric{Name: name, Type: here})
			}
		}
		was, got := checkFileFamiliesBeforeTheRestWereCounted(&set, families, typeFrom), checkFileFamilies(&set, families, typeFrom)
		if (was == nil) != (got == nil) {
			t.Fatalf("the file is refused with %v, and was with %v", got, was)
		}
		if was == nil {
			continue
		}
		list := strings.TrimSuffix(was.err.Error(), ": a metric has one type across the directory's files")
		clashes := strings.Split(list, "; ")
		named, shown := 0, 0
		for named < len(clashes) && shown <= mergeShownBytes {
			shown += len(clashes[named])
			named++
		}
		if named == len(clashes) {
			as++
			if got.stage != was.stage || got.err.Error() != was.err.Error() || model.SameFailureText(got.err) != model.SameFailureText(was.err) {
				t.Fatalf("the file is refused at %s with\n%s\nand was at %s with\n%s", got.stage, got.err, was.stage, was.err)
			}
			continue
		}
		counted++
		want := strings.Join(clashes[:named], "; ") + fmt.Sprintf("; and %d more: a metric has one type across the directory's files", len(clashes)-named)
		if got.stage != "merge" || got.err.Error() != want || len(want) > 1500 {
			t.Fatalf("the file is refused at %s in %d bytes with\n%s\nwant\n%s", got.stage, len(got.err.Error()), got.err, want)
		}
		if same := model.SameFailureText(got.err); same != strings.Join(clashes[:named], "; ")+"; and # more: a metric has one type across the directory's files" {
			t.Fatalf("the failure is recognised by\n%s", same)
		}
	}
	if floor := alloctest.UnlessRaced(400, 80); as < floor || counted < floor {
		t.Errorf("%d files were refused as they were and %d with clashes counted, want %d of each", as, counted, floor)
	}
}

// A probe, a debug probe and a static target's scrape that panic with a
// megabyte of text are answered, reported and logged with its first 2,000
// bytes and its length: the 500 a probe is answered, the line of a debug
// report that says what a probe would have answered and a scrape would have
// published, and the panic of each log line. They were the megabyte each.
// The stack is logged beside it as it was, and a panic of a few words is
// reported as it was.
func TestAPanicOfAMegabyteIsReportedWithinTheBound(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	said := "boom"
	fetch.RequestTypes["panics"] = &fetch.RequestType{
		Name:         "panics",
		Fields:       []string{"path"},
		TargetFields: []string{"path"},
		Validate:     func(*model.Collector) error { return nil },
		Fetch: func(context.Context, string, *model.Collector, fetch.RequestOverrides, http.Header) (*fetch.HTTPResponse, error) {
			panic(said)
		},
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "panics") })
	broken := testutil.Collector("broken", "text")
	broken.Request = model.RequestConfig{Type: "panics", Path: "/data"}
	static := model.StaticTarget{Name: "broken", Collector: "broken", Target: "panics://data"}
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{broken}}, &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{static}})
	server.SetProbeDebug(true)
	get := func(path string) string {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder.Body.String()
	}
	for _, text := range []string{"boom", strings.Repeat("b", alloctest.UnlessRaced(1<<20, 1<<17))} {
		said = text
		want := text
		if len(text) > model.MaxFailureBytes {
			mark := fmt.Sprintf("... (%d bytes)", len(text))
			want = text[:model.MaxFailureBytes-len(mark)] + mark
		}
		logs.Reset()
		if body := get("/probe?collector=broken&target=panics://data"); body != "probe failed: internal error: "+want+"\n" {
			t.Errorf("a probe that panics with %d bytes is answered %d, %.80q ... %q", len(text), len(body), body, body[max(0, len(body)-40):])
		}
		if report := get("/probe?collector=broken&debug=true&target=panics://data"); !strings.Contains(report, "A probe would have answered 500: probe failed: internal error: "+want+".\n") || len(report) > 4*model.MaxFailureBytes+2000 {
			t.Errorf("a debug probe that panics with %d bytes reports %d bytes: %.600s", len(text), len(report), report)
		}
		if report := get("/static-targets?debug=broken"); !strings.Contains(report, "The scrape would have published target up 0: the scrape failed: internal error: "+want+".\n") || len(report) > 4*model.MaxFailureBytes+2000 {
			t.Errorf("a debug scrape that panics with %d bytes reports %d bytes: %.600s", len(text), len(report), report)
		}
		server.scrapeTarget(t.Context(), server.manager.Get(), static)
		panics := 0
		for _, record := range loggedRecords(t, logs) {
			if _, panicked := record["panic"]; !panicked {
				continue
			}
			if panics++; record["panic"] != want || record["msg"] != "probe panicked; its stack is in the exporter's log" && !strings.Contains(fmt.Sprint(record["stack"]), "goroutine ") {
				t.Errorf("a panic of %d bytes is logged by %q as %.80q with the stack %.40q", len(text), record["msg"], record["panic"], record["stack"])
			}
		}
		if panics != 4 {
			t.Errorf("%d lines log a panic of %d bytes, want those of the probe, of the two debug reports and of the scrape:\n%.2000s", panics, len(text), logs)
		}
	}
}
