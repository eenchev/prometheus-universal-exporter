//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A probe's target is the scraper's to choose, and one may be the words the
// failure log told a static target's own failures by, "static target " and
// its name: the probe's failures are the probe's all the same. While a
// reload decided what to forget by reading the target back out of each key,
// a reload that changed or removed the static target one forgot what was
// remembered of the probes of "static target one" with it, so a probe that
// kept failing was logged in full again after every such reload, and one
// that then recovered was logged as recovering from nothing. The reload now
// forgets the static target's own failure, whose next is logged in full, and
// leaves the probe's, which goes on as a repeat and is logged as recovered
// with all its failures counted.
func TestAProbeOfATargetNamedAsAStaticTargetIsNotForgottenWithThatTarget(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	document := func(site string) string {
		return staticDocument("one", "x", down.URL) + "    labels:\n      site: " + site + "\n"
	}
	r := newReloadable(t, testutil.CollectorsDocument("x"), document("a"))
	logs := &bytes.Buffer{}
	r.server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const named = "static target one"
	probeNamed := func() {
		t.Helper()
		if recorder := probeOnce(t, r.server, probePath("x", named, ""), nil); recorder.Code != http.StatusBadGateway {
			t.Fatalf("the probe of %q is answered %d, want 502: %s", named, recorder.Code, recorder.Body)
		}
	}
	scrape := func() { r.server.scrapeStaticTargets(t.Context(), 0) }
	probeNamed()
	scrape()
	probeNamed()
	scrape()
	if probes, scrapes := levelsOf(t, logs, "probe failed"), levelsOf(t, logs, "static target scrape failed"); strings.Join(probes, " ") != "ERROR DEBUG" || strings.Join(scrapes, " ") != "ERROR DEBUG" {
		t.Fatalf("before the reload the probe's failures are logged at %v and the static target's at %v, want each in full and then as a repeat:\n%s", probes, scrapes, logs)
	}
	logs.Reset()
	// The reload changes the static target one: what is remembered of it
	// is forgotten, and of nothing else.
	r.reloadBoth(testutil.CollectorsDocument("x"), document("b"))
	probeNamed()
	scrape()
	if probes := levelsOf(t, logs, "probe failed"); strings.Join(probes, " ") != "DEBUG" {
		t.Errorf("after a reload changed the static target one, the failure of the probe of %q is logged at %v, want as the repeat it is:\n%s", named, probes, logs)
	}
	if scrapes := levelsOf(t, logs, "static target scrape failed"); strings.Join(scrapes, " ") != "ERROR" {
		t.Errorf("after a reload changed the static target one, its scrape's failure is logged at %v, want in full, as a changed target's first:\n%s", scrapes, logs)
	}
	if remembered := rememberedOf(r.server, "x"); len(remembered) != 2 {
		t.Errorf("the failure log remembers %v of the collector, want the probe's failure and the static target's", remembered)
	}
}

// An http probe's target may hold any bytes, NULs among them: such a probe
// fails where its URL is read, and its failure is remembered, as any
// probe's is. While a key was its parts with a NUL between them, a target
// could be written so that the key of its probe was, byte for byte, the key
// of something else: here, for a collector whose definition could not be
// fingerprinted, so that its probes have no key of their own, the key of a
// rule of the probe of another target. Each then took the other's failure
// for its own with another error: both were logged in full on every scrape,
// and the rule's recovery would have ended the probe's failure. They are
// two failures now, each logged in full once and then as a repeat, and the
// probe of the target with NULs is answered as it was.
func TestATargetWithNULsHasNoOtherSubjectsKey(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"other": 1}`))
	}))
	defer target.Close()
	const expression = ".value #\x00"
	c := model.Collector{
		Name: "unkeyed", Request: model.RequestConfig{Type: "http", Method: "GET"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics:       []model.MetricRule{{Name: "demo_value", Type: model.GaugeMetricType, Expression: expression, ErrorMode: model.ErrorModeLog}},
		ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
	}
	server := modeServer(t, c)
	// The fingerprint every probe of this configuration gets: none.
	generation := newFingerprintGeneration(server.manager.Get())
	for i := range generation.once {
		generation.once[i].Do(func() {})
	}
	server.fingerprints.current.Store(generation)
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// crafted is the target whose probe had the key of the rule of the
	// probe of target.URL: that probe's key, the marker of a rule's key, the
	// rule's name, the length of its expression and the expression short of
	// its last byte, a NUL, which with the NUL that ended a probe's key
	// made the two that end the key of a rule without items.
	crafted := target.URL + "\x00\x00\x00rule\x00demo_value\x009\x00" + strings.TrimSuffix(expression, "\x00")
	for range 2 {
		if recorder := probeOnce(t, server, probePath("unkeyed", target.URL, ""), nil); recorder.Code != http.StatusOK {
			t.Fatalf("the probe whose rule lacks its value is answered %d, want 200: %s", recorder.Code, recorder.Body)
		}
		recorder := probeOnce(t, server, probePath("unkeyed", crafted, ""), nil)
		if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "invalid control character in URL") {
			t.Fatalf("the probe of a target with NULs is answered %d %q, want 502 and that its URL holds a control character", recorder.Code, recorder.Body)
		}
	}
	if rules, probes := levelsOf(t, logs, "metric extraction failed"), levelsOf(t, logs, "probe failed"); strings.Join(rules, " ") != "WARN DEBUG" || strings.Join(probes, " ") != "ERROR DEBUG" {
		t.Errorf("the rule's failures are logged at %v and those of the probe of the target with NULs at %v, want each in full and then as a repeat:\n%s", rules, probes, logs)
	}
	if remembered := rememberedOf(server, "unkeyed"); len(remembered) != 2 {
		t.Errorf("the failure log remembers %v of the collector, want the rule's failure and the probe's", remembered)
	}
}
