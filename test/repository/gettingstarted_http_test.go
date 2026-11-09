//go:build !select_request_types || request_type_http

package repository

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

// docs/GETTING-STARTED.md does what it says: its collector loads, a probe of
// a stand-in for the API that answers the JSON the page shows is answered
// with the series the page shows, asked with the query the page writes; its
// static target file loads against the collector; and its Prometheus scrape
// configuration is YAML, its target the page's.
func TestTheGettingStartedPageWorksAsWritten(t *testing.T) {
	const page = "docs/GETTING-STARTED.md"
	answer := docBlock(t, docBlocks(t, page, "json"), "{")
	var asked url.Values
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/forecast" {
			http.NotFound(w, r)
			return
		}
		asked = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	defer api.Close()

	blocks := docBlocks(t, page, "yaml")
	conf := docBlock(t, blocks, "collectors:\n  - name: weather\n")
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	server := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?collector=weather&target="+url.QueryEscape(api.URL), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("the probe is answered %d\n%s", recorder.Code, recorder.Body)
	}
	shown := docBlock(t, docBlocks(t, page, "text"), "# HELP weather_temperature_celsius")
	for _, line := range strings.Split(strings.TrimSpace(shown), "\n") {
		if !strings.Contains(recorder.Body.String(), line+"\n") {
			t.Errorf("the page shows %q, which the probe's answer does not have:\n%s", line, recorder.Body)
		}
	}
	if asked.Get("latitude") != "42.6977" || asked.Get("current") != "temperature_2m,wind_speed_10m" || asked.Get("wind_speed_unit") != "ms" {
		t.Errorf("the API was asked %v", asked)
	}

	// A value the expression finds nothing for is only left out, the rest
	// served, and a target that cannot be reached fails the probe, as the
	// page says after its curl.
	answer = `{"current": {"wind_speed_10m": 3.1}}`
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?collector=weather&target="+url.QueryEscape(api.URL), nil))
	if body := recorder.Body.String(); recorder.Code != http.StatusOK || strings.Contains(body, "weather_temperature_celsius") || !strings.Contains(body, "weather_wind_speed_meters_per_second 3.1\n") {
		t.Errorf("an answer without the temperature is answered %d\n%s", recorder.Code, body)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?collector=weather&target="+url.QueryEscape(gone.URL), nil))
	if recorder.Code < 500 || !strings.Contains(recorder.Body.String(), "failed") {
		t.Errorf("a target that cannot be reached is answered %d\n%s", recorder.Code, recorder.Body)
	}

	targets := testutil.WriteIn(t, t.TempDir(), "static-targets.yaml", docBlock(t, blocks, "interval: 5m\n"))
	file, err := config.LoadStaticTargets(targets)
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}

	var scrape struct {
		ScrapeConfigs []struct {
			MetricsPath   string              `yaml:"metrics_path"`
			Params        map[string][]string `yaml:"params"`
			StaticConfigs []struct {
				Targets []string `yaml:"targets"`
			} `yaml:"static_configs"`
		} `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal([]byte(docBlock(t, blocks, "scrape_configs:\n")), &scrape); err != nil || len(scrape.ScrapeConfigs) != 1 {
		t.Fatalf("the scrape configuration: %v", err)
	}
	job := scrape.ScrapeConfigs[0]
	if job.MetricsPath != "/probe" || len(job.Params["collector"]) != 1 || job.Params["collector"][0] != "weather" || len(job.StaticConfigs) != 1 || job.StaticConfigs[0].Targets[0] != "https://api.open-meteo.com" {
		t.Fatalf("the scrape configuration is %+v", job)
	}
}
