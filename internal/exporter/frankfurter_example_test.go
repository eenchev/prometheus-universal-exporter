//go:build !select_request_types || request_type_http

package exporter

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// examples/config.frankfurter.json-test.yaml reads the Frankfurter API's
// latest rates. Its pre-script turns the rates object into a list, and its
// rules stay jq. testdata/json/frankfurter-latest.json is an answer in the
// shape the API documents.

const (
	frankfurterConfig = "../../examples/config.frankfurter.json-test.yaml"
	frankfurterPath   = "/v1/latest"
)

// A rate and its inverse per currency, in the pre-script's sorted order,
// each with its currency and the base, and the day the rates are for as the
// Unix time of its start in UTC. The day is read by the rule's time_format
// from the date the pre-script passes on as text: the script itself parses
// no date. The API is asked once, for the base and the symbols the example
// names, and nothing is logged.
func TestTheFrankfurterExampleReadsTheRatesAndTheirDay(t *testing.T) {
	requirePython(t)
	logs := testutil.CaptureLogs(t)
	service, cfg := newStandIn(t, frankfurterConfig, map[string]standInAnswer{
		frankfurterPath: {"application/json", readTestdata(t, "json/frankfurter-latest.json")},
	})
	if len(cfg.Collectors) != 1 || cfg.Collectors[0].Name != "exchange_rates" {
		t.Fatalf("%s no longer holds the one collector exchange_rates", frankfurterConfig)
	}
	collector := cfg.Collectors[0]
	if script := collector.Transform.PreScript; strings.Contains(script, "datetime") || strings.Contains(script, "strptime") || strings.Contains(script, "import") {
		t.Errorf("the pre-script parses the date, which the rule's time_format reads:\n%s", script)
	}
	day := collector.Metrics[len(collector.Metrics)-1]
	if day.Name != "exchange_rate_observation_timestamp_seconds" || day.Expression != ".date" || day.TimeFormat != "2006-01-02" || day.TimeZone != "" {
		t.Errorf("the rule of the day is %+v, want .date read with time_format 2006-01-02", day)
	}
	server := NewServer(config.NewManager(cfg, frankfurterConfig, slog.Default()), "python3", slog.Default())

	sameSeries(t, probeStandIn(t, server, service, "exchange_rates", ""), []string{
		`exchange_rate{base="EUR",currency="CHF"} 0.9462`,
		`exchange_rate{base="EUR",currency="GBP"} 0.8588`,
		`exchange_rate{base="EUR",currency="USD"} 1.146`,
		`exchange_rate_inverse{base="EUR",currency="CHF"} 1.056859`,
		`exchange_rate_inverse{base="EUR",currency="GBP"} 1.164415`,
		`exchange_rate_inverse{base="EUR",currency="USD"} 0.8726`,
		// 2026-09-18T00:00:00Z, 1789689600, as the exposition writes it.
		`exchange_rate_observation_timestamp_seconds{base="EUR"} 1.7896896e+09`,
	})
	if asked := service.requests(); !slices.Equal(asked, []string{frankfurterPath + "?base=EUR&symbols=CHF%2CGBP%2CUSD"}) {
		t.Errorf("the stand-in was asked %v, want %s once with the example's query", asked, frankfurterPath)
	}
	if logs.Len() != 0 {
		t.Errorf("reading the rates logged:\n%s", logs)
	}
}
