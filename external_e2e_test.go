package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

// These tests probe real third-party endpoints over the internet. They are
// opt-in, and off by default, because the rest of the suite is deliberately
// local-only: a test that reaches the network fails when the network is down,
// when a sandbox blocks egress, and when somebody else's service has a bad
// afternoon — none of which say anything about this code. Making that the
// default would train everyone to ignore a red suite.
//
// Run them deliberately:
//
//	EXTERNAL_E2E=1 go test -run TestExternal -v ./...
//	make test-external
//
// They are the only check that the shipped demo configurations still match the
// shape of the services they describe, which a stub replaying a captured
// response cannot tell you: a feed that renames a column or a field goes on
// passing every local test and fails here.
const externalE2EEnv = "EXTERNAL_E2E"

func requireExternalE2E(t *testing.T) {
	t.Helper()
	if os.Getenv(externalE2EEnv) == "" {
		t.Skipf("set %s=1 to run the tests that probe real third-party endpoints", externalE2EEnv)
	}
}

// externalCase is one demo configuration and the host it was written against.
// The target is not in the configuration file: a collector describes the
// request, and /probe supplies the host, which is the whole point of the split.
type externalCase struct {
	name string
	// config is the demo configuration, which is also what a reader is invited
	// to run by hand from the comments at the top of the file.
	config string
	// collector is the collector inside it to probe.
	collector string
	// target is the host the demo names.
	target string
	// wantMetrics must all be present in the response.
	wantMetrics []string
	// wantLabel is a label that must appear on the first metric, so a response
	// that decodes but loses its per-row or per-entry structure is caught.
	wantLabel string
}

var externalCases = []externalCase{
	{
		name:        "frankfurter/json",
		config:      "testdata/config.frankfurter.json-test.yaml",
		collector:   "exchange_rates",
		target:      "https://api.frankfurter.dev",
		wantMetrics: []string{"exchange_rate", "exchange_rate_inverse", "exchange_rate_observation_timestamp_seconds"},
		wantLabel:   `currency="USD"`,
	},
	{
		name:        "usgs/csv",
		config:      "testdata/config.usgs.csv-test.yaml",
		collector:   "earthquakes",
		target:      "https://earthquake.usgs.gov",
		wantMetrics: []string{"earthquake_magnitude", "earthquake_depth_kilometers"},
		wantLabel:   `network=`,
	},
}

func TestExternalDemoConfigurationsStillMatchTheirSources(t *testing.T) {
	requireExternalE2E(t)
	for _, test := range externalCases {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := LoadConfig(test.config)
			if err != nil {
				t.Fatalf("%s: %v", test.config, err)
			}
			if err := ValidatePythonScripts("python3", cfg); err != nil {
				t.Fatalf("%s: %v", test.config, err)
			}
			server := NewServer(NewConfigManager(cfg, test.config, slog.Default()), "python3", slog.Default())

			probe := fmt.Sprintf("/probe?collector=%s&target=%s",
				url.QueryEscape(test.collector), url.QueryEscape(test.target))
			response := probeOnce(t, server, probe, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("probing %s returned %d: %s\n(a non-2xx here is usually the service, not this code)",
					test.target, response.Code, strings.TrimSpace(response.Body.String()))
			}
			body := response.Body.String()
			for _, metric := range test.wantMetrics {
				if !strings.Contains(body, metric+"{") && !strings.Contains(body, metric+" ") {
					t.Errorf("%s did not produce %s; the source may have changed shape:\n%s",
						test.target, metric, firstLines(body, 15))
				}
			}
			if !strings.Contains(body, test.wantLabel) {
				t.Errorf("%s produced no series carrying %s:\n%s", test.target, test.wantLabel, firstLines(body, 15))
			}
			if samples := countSamples(body); samples == 0 {
				t.Errorf("%s produced no samples at all:\n%s", test.target, firstLines(body, 15))
			}
		})
	}
}

// A second probe inside the cache window must not reach the network again, and
// must return exactly what the first one did. This is the one place the cache
// meets a real response rather than a stub.
func TestExternalProbeIsServedFromTheCache(t *testing.T) {
	requireExternalE2E(t)
	test := externalCases[0]
	cfg, err := LoadConfig(test.config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collectors[0].Cache <= 0 {
		t.Skipf("%s does not configure a cache", test.config)
	}
	server := NewServer(NewConfigManager(cfg, test.config, slog.Default()), "python3", slog.Default())

	probe := fmt.Sprintf("/probe?collector=%s&target=%s",
		url.QueryEscape(test.collector), url.QueryEscape(test.target))
	first := probeOnce(t, server, probe, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", first.Code, strings.TrimSpace(first.Body.String()))
	}
	second := probeOnce(t, server, probe, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", second.Code, strings.TrimSpace(second.Body.String()))
	}
	if first.Body.String() != second.Body.String() {
		t.Error("a cached probe returned something different from the scrape that filled it")
	}
	exposition := selfMetrics(t, server)
	if !strings.Contains(exposition, fmt.Sprintf("http_exporter_cache_hits_total{collector=%q} 1", test.collector)) {
		t.Errorf("the second probe should have been a cache hit:\n%s", firstLines(exposition, 30))
	}
}

func countSamples(exposition string) int {
	samples := 0
	for _, line := range strings.Split(exposition, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if _, err := strconv.ParseFloat(fields[len(fields)-1], 64); err == nil {
			samples++
		}
	}
	return samples
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n")
}
