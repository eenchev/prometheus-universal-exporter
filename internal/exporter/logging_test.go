package exporter

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A logger obtained from newLogger and slog's default must be the same
// handler, so it cannot matter which one a given call site reaches for.
func TestTheDefaultLoggerIsTheExportersLogger(t *testing.T) {
	out := testutil.CaptureLogs(t)
	slog.Default().Info("through the default logger", "which", "default")
	slog.Info("through the package function", "which", "package")
	records := testutil.AssertJSONLines(t, out, 2)
	for _, record := range records {
		if record["which"] == nil {
			t.Fatalf("attributes were dropped: %v", record)
		}
	}
}

// The watch interval bounds how stale a running configuration can be, so it
// belongs in the startup line whenever the watch is on.
func TestWatchIntervalIsReported(t *testing.T) {
	manager, _ := watchedManager(t)
	if manager.WatchEnabled() {
		t.Fatal("the watch should start disabled")
	}
	if manager.WatchInterval() != 0 {
		t.Fatalf("interval=%s with the watch off, want zero", manager.WatchInterval())
	}
	manager.SetWatchInterval(90 * time.Second)
	if !manager.WatchEnabled() || manager.WatchInterval() != 90*time.Second {
		t.Fatalf("enabled=%v interval=%s", manager.WatchEnabled(), manager.WatchInterval())
	}
	if got := manager.WatchInterval().String(); got != "1m30s" {
		t.Fatalf("the logged interval is %q, which should be a readable duration", got)
	}
}

// HELP is what a reader sees in Grafana's metric browser or in `curl` output,
// so sixteen identical lines are as good as none. This keeps every family
// described, distinctly, and keeps the descriptor list in step with what is
// actually exposed.
func TestEverySelfMetricHasItsOwnDescription(t *testing.T) {
	seen := map[string]string{}
	for _, d := range selfMetricDescriptors {
		if strings.TrimSpace(d.Help) == "" {
			t.Errorf("%s has no description", d.Name)
			continue
		}
		if strings.EqualFold(strings.TrimSpace(d.Help), "Exporter self metric.") {
			t.Errorf("%s still carries the placeholder description", d.Name)
		}
		if !strings.HasSuffix(d.Help, ".") {
			t.Errorf("%s: %q should read as a sentence", d.Name, d.Help)
		}
		if other, duplicate := seen[d.Help]; duplicate {
			t.Errorf("%s and %s share the description %q", other, d.Name, d.Help)
		}
		seen[d.Help] = d.Name
		if selfMetricHelp[d.Name] != d.Help {
			t.Errorf("%s: the indexed help does not match the descriptor", d.Name)
		}
	}
	if len(selfMetricDescriptors) != len(selfMetricNames()) {
		t.Fatal("the name list and the descriptors disagree")
	}
}

// The descriptors and the exposition must not drift: a family exposed without a
// descriptor loses its description, and a descriptor for a family nothing emits
// documents something that does not exist.
func TestSelfMetricDescriptionsMatchTheExposition(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	server := verboseServer(t, false, testutil.Collector("described", "text"))
	probeOnce(t, server, "/probe?target="+target.URL+"&collector=described", nil)
	exposition := selfMetrics(t, server)

	for _, d := range selfMetricDescriptors {
		if _, typed := requestTypeFamilies[d.Name]; typed {
			// Only its request type's collectors have it; that type's
			// tests check its description.
			continue
		}
		if !strings.Contains(exposition, "# HELP "+d.Name+" "+d.Help+"\n") {
			t.Errorf("%s is not published with its description", d.Name)
		}
	}
	if strings.Contains(exposition, "Exporter self metric") {
		t.Errorf("the placeholder description is still being served:\n%s", exposition)
	}
	// Every family the endpoint declares has a descriptor behind it.
	for _, line := range strings.Split(exposition, "\n") {
		if !strings.HasPrefix(line, "# HELP http_exporter_") {
			continue
		}
		name := strings.Fields(line)[2]
		if _, ok := selfMetricHelp[name]; ok {
			continue
		}
		// The families verbose mode adds carry their own help inline.
		if strings.HasPrefix(name, "http_exporter_request_") ||
			name == "http_exporter_collector_config_valid" || name == "http_exporter_static_targets" {
			continue
		}
		t.Errorf("%s is exposed but has no descriptor", name)
	}
}

// The self-metrics delivered over OTLP carry the same descriptions as the text
// exposition, so a dashboard built on either reads the same.
func TestSelfMetricSetCarriesTheSameDescriptions(t *testing.T) {
	server := verboseServer(t, false, testutil.Collector("described", "text"))
	set := server.selfMetricSet()
	found := map[string]bool{}
	for _, metric := range set.Metrics {
		help, ok := selfMetricHelp[metric.Name]
		if !ok {
			continue
		}
		found[metric.Name] = true
		if metric.Help != help {
			t.Errorf("%s: help=%q, want %q", metric.Name, metric.Help, help)
		}
	}
	for _, d := range selfMetricDescriptors {
		if _, typed := requestTypeFamilies[d.Name]; typed {
			continue
		}
		if !found[d.Name] {
			t.Errorf("%s is missing from the self-metric set", d.Name)
		}
	}
}
