//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

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
