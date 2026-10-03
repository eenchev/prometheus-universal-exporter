//go:build !select_request_types || request_type_http

package exporter

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// examples/config.usgs.csv-test.yaml reads the USGS earthquake feed of the
// last hour. testdata/csv/usgs-all-hour.csv is a feed in the columns the
// USGS documents, twelve events: the places hold commas inside quotes, some
// of nst, gap, dmin, horizontalError, magError and magNst are empty, as the
// example's comments say they often are, and one event has no magnitude.

const (
	usgsConfig = "../../examples/config.usgs.csv-test.yaml"
	usgsPath   = "/earthquakes/feed/v1.0/summary/all_hour.csv"
)

func newUSGS(t *testing.T) (*standIn, *Server) {
	t.Helper()
	service, cfg := newStandIn(t, usgsConfig, map[string]standInAnswer{
		usgsPath: {"text/csv", readTestdata(t, "csv/usgs-all-hour.csv")},
	})
	if len(cfg.Collectors) != 1 || cfg.Collectors[0].Name != "earthquakes" {
		t.Fatalf("%s no longer holds the one collector earthquakes", usgsConfig)
	}
	return service, NewServer(config.NewManager(cfg, usgsConfig, slog.Default()), "python3", slog.Default())
}

// A magnitude and a depth per event of the feed, each with the event's id
// and network, the magnitude with its type and review status too. The event
// without a magnitude has its depth and no magnitude, and is reported once,
// as the example's comments promise of an incomplete row: one warning for
// the rule, with the number of rows it could not read, and the probe is
// answered. The columns the rules do not read may be empty without a word.
// The feed is asked for once, at the example's path. The series are compared
// whatever their order.
func TestTheUSGSExampleReadsTheFeedAndReportsAnIncompleteRow(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service, server := newUSGS(t)

	sameLines(t, "series", probeStandIn(t, server, service, "earthquakes", ""), []string{
		`earthquake_depth_kilometers{id="ak0269cwqv8m",network="ak"} 118.6`,
		`earthquake_depth_kilometers{id="ak0269cx1f2k",network="ak"} 45.2`,
		`earthquake_depth_kilometers{id="ci41097608",network="ci"} 8.11`,
		`earthquake_depth_kilometers{id="ci41097632",network="ci"} 3.04`,
		`earthquake_depth_kilometers{id="hv74481052",network="hv"} 0.12`,
		`earthquake_depth_kilometers{id="nc75242386",network="nc"} 6.35`,
		`earthquake_depth_kilometers{id="nc75242391",network="nc"} 1.71`,
		`earthquake_depth_kilometers{id="pr71532588",network="pr"} 7.56`,
		`earthquake_depth_kilometers{id="tx2026tkqgzb",network="tx"} 5.9814`,
		`earthquake_depth_kilometers{id="us7000rb4q",network="us"} 112.853`,
		`earthquake_depth_kilometers{id="uu80091472",network="uu"} -1.38`,
		`earthquake_depth_kilometers{id="uw62204187",network="uw"} 0.47`,
		`earthquake_magnitude{id="ak0269cwqv8m",magnitude_type="ml",network="ak",review_status="automatic"} 3.1`,
		`earthquake_magnitude{id="ak0269cx1f2k",magnitude_type="ml",network="ak",review_status="automatic"} 1.6`,
		`earthquake_magnitude{id="ci41097608",magnitude_type="ml",network="ci",review_status="automatic"} 1.02`,
		`earthquake_magnitude{id="ci41097632",magnitude_type="ml",network="ci",review_status="automatic"} 0.54`,
		`earthquake_magnitude{id="hv74481052",magnitude_type="md",network="hv",review_status="automatic"} 1.87`,
		`earthquake_magnitude{id="nc75242386",magnitude_type="md",network="nc",review_status="automatic"} 1.33`,
		`earthquake_magnitude{id="nc75242391",magnitude_type="md",network="nc",review_status="automatic"} 0.82`,
		`earthquake_magnitude{id="pr71532588",magnitude_type="md",network="pr",review_status="reviewed"} 2.09`,
		`earthquake_magnitude{id="tx2026tkqgzb",magnitude_type="ml",network="tx",review_status="automatic"} 2.3`,
		`earthquake_magnitude{id="us7000rb4q",magnitude_type="mb",network="us",review_status="reviewed"} 4.6`,
		`earthquake_magnitude{id="uw62204187",magnitude_type="ml",network="uw",review_status="reviewed"} 0.31`,
	})
	if asked := service.requests(); !slices.Equal(asked, []string{usgsPath}) {
		t.Errorf("the stand-in was asked %v, want %s once", asked, usgsPath)
	}
	loggedOnly(t, logs, `WARN earthquakes earthquake_magnitude: 1 failed: CSV column "mag" is missing`)
}

// A second probe within the minute of cache.ttl is answered from memory,
// with the same series, and the incomplete row is not reported again.
func TestTheUSGSExampleAnswersARepeatedProbeFromMemory(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service, server := newUSGS(t)

	first := probeStandIn(t, server, service, "earthquakes", "")
	logs.Reset()
	sameSeries(t, probeStandIn(t, server, service, "earthquakes", ""), first)
	if asked := len(service.requests()); asked != 1 {
		t.Errorf("the stand-in was asked %d times by two probes within cache.ttl, want once", asked)
	}
	loggedOnly(t, logs)
}
