//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// examples/open-meteo/config.yaml reads the current weather of a place from
// Open-Meteo's /v1/forecast, the place given by the probe's param_latitude
// and param_longitude, and examples/open-meteo/static-targets.yaml has the
// exporter scrape three places itself. testdata/json/open-meteo-forecast.json
// is an answer in the shape the API documents, for the grid point nearest
// Sofia. These tests run both files as shipped against a stand-in for the API,
// so they run with the rest of the suite.

const (
	openMeteoConfig  = "../../examples/open-meteo/config.yaml"
	openMeteoTargets = "../../examples/open-meteo/static-targets.yaml"
	// Everything the collector asks the API for, as the example lists it.
	openMeteoCurrent = "temperature_2m,relative_humidity_2m,apparent_temperature,precipitation,weather_code,cloud_cover,pressure_msl,wind_speed_10m,wind_direction_10m,wind_gusts_10m,is_day"
)

// openMeteoAPI stands in for https://api.open-meteo.com: it answers
// /v1/forecast with the fixture, or with status when that is set, and keeps
// every query it was asked.
type openMeteoAPI struct {
	*httptest.Server
	mu      sync.Mutex
	queries []url.Values
	status  int
}

func (api *openMeteoAPI) asked() []url.Values {
	api.mu.Lock()
	defer api.mu.Unlock()
	return slices.Clone(api.queries)
}

func (api *openMeteoAPI) answerWith(status int) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.status = status
}

// newOpenMeteo starts the stand-in and loads the example's configuration. The
// collector allows https only, so the stand-in speaks TLS, and the one thing
// the loaded configuration is given that the file does not have is the
// stand-in's certificate to verify it with.
func newOpenMeteo(t *testing.T) (*openMeteoAPI, *model.Config) {
	t.Helper()
	answer := readFixture(t, "open-meteo-forecast.json")
	api := &openMeteoAPI{}
	api.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/forecast" {
			http.NotFound(w, r)
			return
		}
		api.mu.Lock()
		api.queries = append(api.queries, r.URL.Query())
		status := api.status
		api.mu.Unlock()
		if status != 0 {
			http.Error(w, `{"error":true,"reason":"unavailable"}`, status)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(answer)
	}))
	t.Cleanup(api.Close)

	cfg, err := config.Load(openMeteoConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Collectors) != 1 || cfg.Collectors[0].Name != "open_meteo_current" {
		t.Fatalf("%s no longer holds the one collector open_meteo_current", openMeteoConfig)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	if err := os.WriteFile(ca, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Collectors[0].Request.TLS.CAFile = ca
	return api, cfg
}

// probeOpenMeteo probes the example's collector at the stand-in, with extra
// query parameters as a scrape configuration's params would add them, and
// returns the answer's series. The age of the result, which the collector's
// cache.stale_if_error adds beside the stale marker, is the one series whose
// value is the clock's: it has to be there, and is left out of what is
// returned.
func probeOpenMeteo(t *testing.T, server *Server, api *openMeteoAPI, extra string) []string {
	t.Helper()
	response := probeOnce(t, server, "/probe?collector=open_meteo_current&target="+url.QueryEscape(api.URL)+extra, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	all := samples(response.Body.String())
	series := slices.DeleteFunc(slices.Clone(all), func(line string) bool {
		return strings.HasPrefix(line, resultAgeMetric+" ")
	})
	if len(series) != len(all)-1 {
		t.Errorf("the answer has %d %s series, want one:\n%s", len(all)-len(series), resultAgeMetric, response.Body.String())
	}
	return series
}

// The series of the fixture: every rule of the example, each carrying the grid
// point the API answered for, percentages as ratios and the observation time
// as Unix seconds.
var openMeteoSeries = []string{
	`weather_temperature_celsius{latitude="42.6875",longitude="23.3125"} 14.3`,
	`weather_apparent_temperature_celsius{latitude="42.6875",longitude="23.3125"} 12.9`,
	`weather_relative_humidity_ratio{latitude="42.6875",longitude="23.3125"} 0.62`,
	`weather_precipitation_millimeters{latitude="42.6875",longitude="23.3125"} 0`,
	`weather_cloud_cover_ratio{latitude="42.6875",longitude="23.3125"} 1`,
	`weather_pressure_msl_hectopascals{latitude="42.6875",longitude="23.3125"} 1019.4`,
	`weather_wind_speed_meters_per_second{latitude="42.6875",longitude="23.3125"} 2.31`,
	`weather_wind_gusts_meters_per_second{latitude="42.6875",longitude="23.3125"} 5.6`,
	`weather_wind_direction_degrees{latitude="42.6875",longitude="23.3125"} 274`,
	`weather_code{latitude="42.6875",longitude="23.3125"} 3`,
	`weather_is_day{latitude="42.6875",longitude="23.3125"} 1`,
	`weather_observation_timestamp_seconds{latitude="42.6875",longitude="23.3125"} 1.7909361e+09`,
}

// A probe that names no place asks for Sofia, the placeholders' defaults, in
// metres per second, and every rule of the example finds its value: twelve
// series and the marker that the answer is not a stale one, with nothing
// logged.
func TestTheOpenMeteoExampleReadsTheCurrentWeather(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	api, cfg := newOpenMeteo(t)
	server := NewServer(config.NewManager(cfg, openMeteoConfig, slog.Default()), "python3", slog.Default())

	got := probeOpenMeteo(t, server, api, "")
	want := append(slices.Clone(openMeteoSeries), resultStaleMetric+" 0")
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	asked := api.asked()
	if len(asked) != 1 {
		t.Fatalf("the API was asked %d times, want once", len(asked))
	}
	wantQuery := url.Values{
		"latitude":        {"42.6977"},
		"longitude":       {"23.3219"},
		"current":         {openMeteoCurrent},
		"wind_speed_unit": {"ms"},
	}
	if asked[0].Encode() != wantQuery.Encode() {
		t.Errorf("the API was asked\n%s\nwant\n%s", asked[0].Encode(), wantQuery.Encode())
	}
	if logs.Len() != 0 {
		t.Errorf("reading the answer logged:\n%s", logs)
	}
}

// param_latitude and param_longitude of the probe choose the place. Each place
// is cached on its own: within the five minutes of cache.ttl a second probe of
// the same place is answered from memory, and one of another place asks the
// API.
func TestTheOpenMeteoExampleAsksForThePlaceTheProbeNames(t *testing.T) {
	testutil.CaptureLogs(t)
	api, cfg := newOpenMeteo(t)
	server := NewServer(config.NewManager(cfg, openMeteoConfig, slog.Default()), "python3", slog.Default())

	const varna = "&param_latitude=43.2141&param_longitude=27.9147"
	first := probeOpenMeteo(t, server, api, varna)
	if again := probeOpenMeteo(t, server, api, varna); !slices.Equal(again, first) {
		t.Errorf("the second probe of the same place differs from the first:\n%s\nfirst:\n%s", strings.Join(again, "\n"), strings.Join(first, "\n"))
	}
	probeOpenMeteo(t, server, api, "&param_latitude=42.1354&param_longitude=24.7453")
	probeOpenMeteo(t, server, api, "")

	var places []string
	for _, query := range api.asked() {
		places = append(places, query.Get("latitude")+","+query.Get("longitude"))
		if query.Get("wind_speed_unit") != "ms" || query.Get("current") != openMeteoCurrent {
			t.Errorf("a request lost the rest of the configured query: %s", query.Encode())
		}
	}
	if want := []string{"43.2141,27.9147", "42.1354,24.7453", "42.6977,23.3219"}; !slices.Equal(places, want) {
		t.Errorf("the API was asked for %v, want %v", places, want)
	}
}

// While the API is down the last result of a place is served, marked stale,
// for the hour of cache.stale_if_error, after the one retry the example
// configures. The probe shortens the two seconds between the attempts, as a
// probe may, so the test does not wait for them.
func TestTheOpenMeteoExampleServesTheLastWeatherWhileTheAPIIsDown(t *testing.T) {
	testutil.CaptureLogs(t)
	api, cfg := newOpenMeteo(t)
	server := NewServer(config.NewManager(cfg, openMeteoConfig, slog.Default()), "python3", slog.Default())
	const fast = "&retry_backoff=1ms"

	probeOpenMeteo(t, server, api, fast)
	// Ten minutes on, the answer is past cache.ttl and within stale_if_error.
	ageEntries(server, 10*time.Minute)
	api.answerWith(http.StatusServiceUnavailable)

	got := probeOpenMeteo(t, server, api, fast)
	want := append(slices.Clone(openMeteoSeries), resultStaleMetric+" 1")
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if asked := len(api.asked()); asked != 3 {
		t.Errorf("the API was asked %d times, want 3: the first probe, the failed one and its one retry", asked)
	}

	// Past the hour there is nothing left to serve, and the probe fails as the
	// API does.
	ageEntries(server, time.Hour)
	response := probeOnce(t, server, "/probe?collector=open_meteo_current&target="+url.QueryEscape(api.URL)+fast, nil)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "received HTTP status 503") {
		t.Errorf("after stale_if_error: status=%d body=%s", response.Code, response.Body.String())
	}
}

// The target file is valid with the configuration beside it, as the exporter
// checks the two at startup: every target's collector exists and its params
// fill placeholders the collector has. Scraped, each of its three places asks
// the API for its own coordinates, and its series carry the city and the
// target's name beside the grid point. The file is used as shipped but for the
// address, which is the stand-in's instead of the API's.
func TestTheOpenMeteoExampleScrapesItsStaticTargets(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	api, cfg := newOpenMeteo(t)
	file, err := config.LoadStaticTargets(openMeteoTargets)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateStaticTargets(file); err != nil {
		t.Fatalf("%s: %v", openMeteoTargets, err)
	}
	if err := config.ValidateStaticTargetsAgainst(file, cfg); err != nil {
		t.Fatalf("%s does not match %s: %v", openMeteoTargets, openMeteoConfig, err)
	}
	if time.Duration(file.Interval) != 10*time.Minute || len(file.Targets) != 3 {
		t.Fatalf("interval=%s targets=%d, want the three places every 10m", time.Duration(file.Interval), len(file.Targets))
	}
	for i := range file.Targets {
		if file.Targets[i].Target != "https://api.open-meteo.com" {
			t.Errorf("target %s is scraped at %q, want the API", file.Targets[i].Name, file.Targets[i].Target)
		}
		file.Targets[i].Target = api.URL
	}

	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	body := getStaticTargets(t, server, "/static-targets")
	for _, want := range []string{
		`weather_temperature_celsius{city="Sofia",latitude="42.6875",longitude="23.3125",static_target="sofia"} 14.3`,
		`weather_temperature_celsius{city="Plovdiv",latitude="42.6875",longitude="23.3125",static_target="plovdiv"} 14.3`,
		`weather_temperature_celsius{city="Varna",latitude="42.6875",longitude="23.3125",static_target="varna"} 14.3`,
		`weather_observation_timestamp_seconds{city="Varna",latitude="42.6875",longitude="23.3125",static_target="varna"} 1.7909361e+09`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	for _, name := range []string{"sofia", "plovdiv", "varna"} {
		if count := strings.Count(body, `static_target="`+name+`"} `); count < len(openMeteoSeries) {
			t.Errorf("target %s has %d series, want the %d of the collector's rules:\n%s", name, count, len(openMeteoSeries), body)
		}
	}
	var places []string
	for _, query := range api.asked() {
		places = append(places, query.Get("latitude")+","+query.Get("longitude"))
	}
	slices.Sort(places)
	if want := []string{"42.1354,24.7453", "42.6977,23.3219", "43.2141,27.9147"}; !slices.Equal(places, want) {
		t.Errorf("the API was asked for %v, want Plovdiv, Sofia and Varna: %v", places, want)
	}
	if logs.Len() != 0 {
		t.Errorf("scraping the three places logged:\n%s", logs)
	}
}
