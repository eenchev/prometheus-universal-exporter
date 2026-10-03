//go:build !select_request_types || request_type_http

package exporter

import (
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// examples/config.ecb.xml-test.yaml reads the ECB's euro foreign exchange
// reference rates. testdata/xml/ecb-eurofxref-daily.xml is a document in the
// shape the ECB publishes: every element in a namespace, the Cubes in the
// default one, the rates in attributes written with single quotes.

const (
	ecbConfig = "../../examples/config.ecb.xml-test.yaml"
	ecbPath   = "/stats/eurofxref/eurofxref-daily.xml"
)

func newECB(t *testing.T) (*standIn, *Server) {
	t.Helper()
	service, cfg := newStandIn(t, ecbConfig, map[string]standInAnswer{
		ecbPath: {"text/xml", readTestdata(t, "xml/ecb-eurofxref-daily.xml")},
	})
	if len(cfg.Collectors) != 1 || cfg.Collectors[0].Name != "ecb_reference_rates" {
		t.Fatalf("%s no longer holds the one collector ecb_reference_rates", ecbConfig)
	}
	return service, NewServer(config.NewManager(cfg, ecbConfig, slog.Default()), "python3", slog.Default())
}

// One series per rate of the document, each with its currency, one for the
// set with the sender, and one whose value is the day the rates are for, read
// from the time attribute by time_format as the Unix time of that day's start
// in UTC; no series carries the date as a label. The prefixes of
// response.namespaces find the Cubes of the default namespace and the gesmes:
// elements, and the currency is read from the attribute beside each rate.
// Nothing is logged, and the document is asked for once.
func TestTheECBExampleReadsTheReferenceRates(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service, server := newECB(t)

	got := probeStandIn(t, server, service, "ecb_reference_rates", "")
	want := []string{
		`ecb_euro_reference_rate{currency="USD"} 1.1712`,
		`ecb_euro_reference_rate{currency="JPY"} 173.25`,
		`ecb_euro_reference_rate{currency="CZK"} 24.318`,
		`ecb_euro_reference_rate{currency="DKK"} 7.4641`,
		`ecb_euro_reference_rate{currency="GBP"} 0.8713`,
		`ecb_euro_reference_rate{currency="HUF"} 389.45`,
		`ecb_euro_reference_rate{currency="PLN"} 4.2585`,
		`ecb_euro_reference_rate{currency="RON"} 5.0893`,
		`ecb_euro_reference_rate{currency="SEK"} 11.026`,
		`ecb_euro_reference_rate{currency="CHF"} 0.9342`,
		`ecb_euro_reference_rate{currency="ISK"} 142.6`,
		`ecb_euro_reference_rate{currency="NOK"} 11.6855`,
		`ecb_euro_reference_rate{currency="TRY"} 48.7216`,
		`ecb_euro_reference_rate{currency="AUD"} 1.7748`,
		`ecb_euro_reference_rate{currency="BRL"} 6.2501`,
		`ecb_euro_reference_rate{currency="CAD"} 1.6321`,
		`ecb_euro_reference_rate{currency="CNY"} 8.3412`,
		`ecb_euro_reference_rate{currency="HKD"} 9.1139`,
		`ecb_euro_reference_rate{currency="IDR"} 19512.44`,
		`ecb_euro_reference_rate{currency="ILS"} 3.876`,
		`ecb_euro_reference_rate{currency="INR"} 103.9045`,
		`ecb_euro_reference_rate{currency="KRW"} 1643.82`,
		`ecb_euro_reference_rate{currency="MXN"} 21.512`,
		`ecb_euro_reference_rate{currency="MYR"} 4.9296`,
		`ecb_euro_reference_rate{currency="NZD"} 2.0147`,
		`ecb_euro_reference_rate{currency="PHP"} 68.062`,
		`ecb_euro_reference_rate{currency="SGD"} 1.5102`,
		`ecb_euro_reference_rate{currency="THB"} 37.912`,
		`ecb_euro_reference_rate{currency="ZAR"} 20.218`,
		`ecb_euro_reference_rates{sender="European Central Bank"} 29`,
		// 2026-10-02T00:00:00Z, 1790899200, as the exposition writes it.
		`ecb_euro_reference_rates_timestamp_seconds 1.7908992e+09`,
		resultStaleMetric + " 0",
	}
	sameSeries(t, got, want)
	for _, line := range got {
		if strings.Contains(line, "date=") || strings.Contains(line, "2026-10-02") {
			t.Errorf("%s carries the date as a label", line)
		}
	}
	if asked := service.requests(); !slices.Equal(asked, []string{ecbPath}) {
		t.Errorf("the stand-in was asked %v, want %s once", asked, ecbPath)
	}
	if logs.Len() != 0 {
		t.Errorf("reading the rates logged:\n%s", logs)
	}
}

// A second probe within the ten minutes of cache.ttl is answered from memory.
// While the site is down the last rates are served, marked stale, for the day
// of cache.stale_if_error, after the one retry the example configures, and
// past the day the probe fails as the site does.
func TestTheECBExampleServesTheLastRatesWhileTheSiteIsDown(t *testing.T) {
	testutil.CaptureLogs(t)
	service, server := newECB(t)
	const fast = "&retry_backoff=1ms"

	first := probeStandIn(t, server, service, "ecb_reference_rates", fast)
	sameSeries(t, probeStandIn(t, server, service, "ecb_reference_rates", fast), first)
	if asked := len(service.requests()); asked != 1 {
		t.Fatalf("the stand-in was asked %d times by two probes within cache.ttl, want once", asked)
	}

	ageEntries(server, time.Hour)
	service.answerWith(http.StatusServiceUnavailable)
	stale := probeStandIn(t, server, service, "ecb_reference_rates", fast)
	want := append(slices.Clone(first[:len(first)-1]), resultStaleMetric+" 1")
	sameSeries(t, stale, want)
	if asked := len(service.requests()); asked != 3 {
		t.Errorf("the stand-in was asked %d times, want 3: the first probe, the failed one and its one retry", asked)
	}

	ageEntries(server, 24*time.Hour)
	response := probeOnce(t, server, "/probe?collector=ecb_reference_rates&target="+url.QueryEscape(service.URL)+fast, nil)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "received HTTP status 503") {
		t.Errorf("after stale_if_error: status=%d body=%s", response.Code, response.Body.String())
	}
}
