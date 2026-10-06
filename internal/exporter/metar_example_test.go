//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// examples/metar/config.yaml reads an airport's latest METAR from the file
// the US National Weather Service keeps for its station, the station given by
// the probe's param_station, and examples/metar/static-targets.yaml has the
// exporter scrape three airports itself. testdata/text/metar-*.txt are
// reports in the form the service keeps them, two lines each, chosen for the
// ways a report is written: a plain one, a variable wind with CAVOK, gusts
// with a dew point below zero, and a North American one, with statute miles,
// inches of mercury and remarks.

const (
	metarConfig   = "../../examples/metar/config.yaml"
	metarTargets  = "../../examples/metar/static-targets.yaml"
	metarStations = "/data/observations/metar/stations/"
)

func newMETAR(t *testing.T) (*standIn, *model.Config) {
	t.Helper()
	answers := map[string]standInAnswer{}
	for _, station := range []string{"LBSF", "LBBG", "LBWN", "KJFK"} {
		answers[metarStations+station+".TXT"] = standInAnswer{"text/plain", readTestdata(t, "text/metar-"+station+".txt")}
	}
	service, cfg := newStandIn(t, metarConfig, answers)
	if len(cfg.Collectors) != 1 || cfg.Collectors[0].Name != "metar" {
		t.Fatalf("%s no longer holds the one collector metar", metarConfig)
	}
	return service, cfg
}

// Each report gives the series of what it holds and none for what it leaves
// out, with nothing logged: the station of the default, Sofia, and of
// param_station; a temperature or a dew point below zero by the rule for M; a
// pressure in inches of mercury as hectopascals; knots as metres per second;
// no direction for a variable wind, no gust without gusts, and no visibility
// for CAVOK or statute miles. Every series carries the station the report's
// line starts with, read by a group of the rule's regex before the group
// named value. The date of the first line is the report's time, in Unix
// seconds, and nothing else; the groups after the pressure and the remarks
// give nothing.
func TestTheMETARExampleReadsAReportHoweverItIsWritten(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service, cfg := newMETAR(t)
	server := NewServer(config.NewManager(cfg, metarConfig, slog.Default()), "python3", slog.Default())

	for _, tc := range []struct {
		station string
		want    []string
	}{
		{"", []string{
			// 2026-10-03T09:00:00Z, as the exposition writes a large number.
			`metar_observation_timestamp_seconds{station="LBSF"} 1.791018e+09`,
			`metar_temperature_celsius{station="LBSF"} 14`,
			`metar_dew_point_celsius{station="LBSF"} 8`,
			`metar_pressure_hectopascals{station="LBSF"} 1021`,
			`metar_wind_speed_meters_per_second{station="LBSF"} 2.057776`,
			`metar_wind_direction_degrees{station="LBSF"} 120`,
			`metar_visibility_meters{station="LBSF"} 9999`,
		}},
		{"LBBG", []string{
			`metar_observation_timestamp_seconds{station="LBBG"} 1.791018e+09`,
			`metar_temperature_celsius{station="LBBG"} 21`,
			`metar_dew_point_celsius{station="LBBG"} 15`,
			`metar_pressure_hectopascals{station="LBBG"} 1017`,
			`metar_wind_speed_meters_per_second{station="LBBG"} 1.028888`,
		}},
		{"LBWN", []string{
			`metar_observation_timestamp_seconds{station="LBWN"} 1.791018e+09`,
			`metar_temperature_celsius{station="LBWN"} 2`,
			`metar_dew_point_celsius{station="LBWN"} -1`,
			`metar_pressure_hectopascals{station="LBWN"} 998`,
			`metar_wind_speed_meters_per_second{station="LBWN"} 6.173328`,
			`metar_wind_gust_meters_per_second{station="LBWN"} 12.346656`,
			`metar_wind_direction_degrees{station="LBWN"} 40`,
			`metar_visibility_meters{station="LBWN"} 6000`,
		}},
		{"KJFK", []string{
			// 2026-01-15T08:51:00Z.
			`metar_observation_timestamp_seconds{station="KJFK"} 1.76846706e+09`,
			`metar_temperature_celsius{station="KJFK"} -2`,
			`metar_dew_point_celsius{station="KJFK"} -11`,
			// 3012 × 0.3386389, as a float64 multiplies them.
			`metar_pressure_hectopascals{station="KJFK"} 1019.9803668000001`,
			`metar_wind_speed_meters_per_second{station="KJFK"} 7.71666`,
			`metar_wind_gust_meters_per_second{station="KJFK"} 12.8611`,
			`metar_wind_direction_degrees{station="KJFK"} 310`,
		}},
	} {
		extra := ""
		if tc.station != "" {
			extra = "&param_station=" + tc.station
		}
		got := probeStandIn(t, server, service, "metar", extra)
		sameSeries(t, got, append(slices.Clone(tc.want), resultStaleMetric+" 0"))
	}
	want := []string{metarStations + "LBSF.TXT", metarStations + "LBBG.TXT", metarStations + "LBWN.TXT", metarStations + "KJFK.TXT"}
	if asked := service.requests(); !slices.Equal(asked, want) {
		t.Errorf("the stand-in was asked %v, want %v", asked, want)
	}
	if logs.Len() != 0 {
		t.Errorf("reading the reports logged:\n%s", logs)
	}
}

// A report's series are the observation's and none is a forecast's or the
// remarks'. A report may go on, after the pressure, with the wind, the
// visibility and the weather expected (BECMG, TEMPO) and with remarks, whose
// groups are written like the observation's own; each rule reads the group
// where the observation has it — the wind directly after the station and the
// time, the visibility directly after the wind, the pressure directly after
// the temperature — so a report without gusts, with a variable wind, with a
// wind in metres per second or with no wind measured gives no such series,
// where a rule that looked along the whole line took the forecast's. The
// kind of report before the station (METAR, SPECI) and AUTO or COR after the
// time are read past, a calm is a speed of 0 from 0 degrees, and a pressure
// the remarks repeat in the other unit, or the report gives in both, is one
// series. Each report is served as the station's file, the date's line before
// it, and each answer has exactly the series listed.
func TestTheMETARExampleReadsTheObservationAndNotTheForecast(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	cases := []struct {
		station, report string
		want            []string
	}{
		// The forecast has gusts and the observation none.
		{"EGLL", "EGLL 030920Z 24008KT 9999 SCT030 15/09 Q1015 BECMG 27015G25KT", []string{
			`metar_observation_timestamp_seconds{station="EGLL"} 1.791018e+09`,
			`metar_temperature_celsius{station="EGLL"} 15`,
			`metar_dew_point_celsius{station="EGLL"} 9`,
			`metar_pressure_hectopascals{station="EGLL"} 1015`,
			`metar_wind_speed_meters_per_second{station="EGLL"} 4.115552`,
			`metar_wind_direction_degrees{station="EGLL"} 240`,
			`metar_visibility_meters{station="EGLL"} 9999`,
		}},
		// The forecast has a direction and a visibility, and the observation
		// a variable wind and CAVOK.
		{"LBBG", "LBBG 030900Z VRB02KT CAVOK 21/15 Q1017 BECMG 27015KT 3000", []string{
			`metar_observation_timestamp_seconds{station="LBBG"} 1.791018e+09`,
			`metar_temperature_celsius{station="LBBG"} 21`,
			`metar_dew_point_celsius{station="LBBG"} 15`,
			`metar_pressure_hectopascals{station="LBBG"} 1017`,
			`metar_wind_speed_meters_per_second{station="LBBG"} 1.028888`,
		}},
		// A wind in metres per second is not the rules' knots, and the
		// forecast's wind in knots is not the observation's. The visibility
		// follows the wind in either unit.
		{"UUEE", "UUEE 030900Z 24005MPS 9999 SCT030 12/08 Q1012 TEMPO 27015G25KT 2000", []string{
			`metar_observation_timestamp_seconds{station="UUEE"} 1.791018e+09`,
			`metar_temperature_celsius{station="UUEE"} 12`,
			`metar_dew_point_celsius{station="UUEE"} 8`,
			`metar_pressure_hectopascals{station="UUEE"} 1012`,
			`metar_visibility_meters{station="UUEE"} 9999`,
		}},
		// No wind was measured, and no dew point.
		{"ENZV", "ENZV 030920Z AUTO /////KT 9999 FEW030 12/// Q1013 BECMG 20010G20KT 4000", []string{
			`metar_observation_timestamp_seconds{station="ENZV"} 1.791018e+09`,
			`metar_pressure_hectopascals{station="ENZV"} 1013`,
			`metar_visibility_meters{station="ENZV"} 9999`,
		}},
		{"LOWW", "METAR LOWW 030920Z 35008KT 350V070 9999 FEW040 17/09 Q1019 NOSIG", []string{
			`metar_observation_timestamp_seconds{station="LOWW"} 1.791018e+09`,
			`metar_temperature_celsius{station="LOWW"} 17`,
			`metar_dew_point_celsius{station="LOWW"} 9`,
			`metar_pressure_hectopascals{station="LOWW"} 1019`,
			`metar_wind_speed_meters_per_second{station="LOWW"} 4.115552`,
			`metar_wind_direction_degrees{station="LOWW"} 350`,
			`metar_visibility_meters{station="LOWW"} 9999`,
		}},
		{"LFPG", "SPECI LFPG 030912Z COR 27012G24KT 3000 TSRA BKN020CB 16/14 Q1009 TEMPO 32025G40KT 1500", []string{
			`metar_observation_timestamp_seconds{station="LFPG"} 1.791018e+09`,
			`metar_temperature_celsius{station="LFPG"} 16`,
			`metar_dew_point_celsius{station="LFPG"} 14`,
			`metar_pressure_hectopascals{station="LFPG"} 1009`,
			`metar_wind_speed_meters_per_second{station="LFPG"} 6.173328`,
			`metar_wind_gust_meters_per_second{station="LFPG"} 12.346656`,
			`metar_wind_direction_degrees{station="LFPG"} 270`,
			`metar_visibility_meters{station="LFPG"} 3000`,
		}},
		{"EGPH", "EGPH 030920Z AUTO 24008KT 9999 NCD 15/09 Q1015", []string{
			`metar_observation_timestamp_seconds{station="EGPH"} 1.791018e+09`,
			`metar_temperature_celsius{station="EGPH"} 15`,
			`metar_dew_point_celsius{station="EGPH"} 9`,
			`metar_pressure_hectopascals{station="EGPH"} 1015`,
			`metar_wind_speed_meters_per_second{station="EGPH"} 4.115552`,
			`metar_wind_direction_degrees{station="EGPH"} 240`,
			`metar_visibility_meters{station="EGPH"} 9999`,
		}},
		// A calm, in fog, with a runway's visual range after the visibility.
		{"EDDF", "EDDF 030920Z 00000KT 0800 R25R/1200N FG VV002 05/05 Q1025", []string{
			`metar_observation_timestamp_seconds{station="EDDF"} 1.791018e+09`,
			`metar_temperature_celsius{station="EDDF"} 5`,
			`metar_dew_point_celsius{station="EDDF"} 5`,
			`metar_pressure_hectopascals{station="EDDF"} 1025`,
			`metar_wind_speed_meters_per_second{station="EDDF"} 0`,
			`metar_wind_direction_degrees{station="EDDF"} 0`,
			`metar_visibility_meters{station="EDDF"} 800`,
		}},
		// The remarks repeat the pressure in inches of mercury.
		{"RJTT", "RJTT 030900Z 02010KT 9999 FEW030 22/15 Q1013 NOSIG RMK 1CU030 A2992", []string{
			`metar_observation_timestamp_seconds{station="RJTT"} 1.791018e+09`,
			`metar_temperature_celsius{station="RJTT"} 22`,
			`metar_dew_point_celsius{station="RJTT"} 15`,
			`metar_pressure_hectopascals{station="RJTT"} 1013`,
			`metar_wind_speed_meters_per_second{station="RJTT"} 5.14444`,
			`metar_wind_direction_degrees{station="RJTT"} 20`,
			`metar_visibility_meters{station="RJTT"} 9999`,
		}},
		// The report gives the pressure in both units.
		{"MSLP", "MSLP 030850Z 18008KT 9999 FEW020 27/21 Q1012 A2990 NOSIG", []string{
			`metar_observation_timestamp_seconds{station="MSLP"} 1.791018e+09`,
			`metar_temperature_celsius{station="MSLP"} 27`,
			`metar_dew_point_celsius{station="MSLP"} 21`,
			`metar_pressure_hectopascals{station="MSLP"} 1012`,
			`metar_wind_speed_meters_per_second{station="MSLP"} 4.115552`,
			`metar_wind_direction_degrees{station="MSLP"} 180`,
			`metar_visibility_meters{station="MSLP"} 9999`,
		}},
		// Remarks with a peak wind, a temperature to the tenth and a
		// pressure tendency, none of which is a series.
		{"KDEN", "KDEN 030853Z 18008KT 10SM SCT120 BKN200 23/M01 A3012 RMK AO2 PK WND 20028/0815 SLP132 T02331011 53012", []string{
			`metar_observation_timestamp_seconds{station="KDEN"} 1.791018e+09`,
			`metar_temperature_celsius{station="KDEN"} 23`,
			`metar_dew_point_celsius{station="KDEN"} -1`,
			`metar_pressure_hectopascals{station="KDEN"} 1019.9803668000001`,
			`metar_wind_speed_meters_per_second{station="KDEN"} 4.115552`,
			`metar_wind_direction_degrees{station="KDEN"} 180`,
		}},
	}
	answers := map[string]standInAnswer{}
	for _, tc := range cases {
		answers[metarStations+tc.station+".TXT"] = standInAnswer{"text/plain", []byte("2026/10/03 09:00\n" + tc.report + "\n")}
	}
	service, cfg := newStandIn(t, metarConfig, answers)
	server := NewServer(config.NewManager(cfg, metarConfig, slog.Default()), "python3", slog.Default())
	for _, tc := range cases {
		got := probeStandIn(t, server, service, "metar", "&param_station="+tc.station)
		sameSeries(t, got, append(slices.Clone(tc.want), resultStaleMetric+" 0"))
	}
	if logs.Len() != 0 {
		t.Errorf("reading the reports logged:\n%s", logs)
	}
}

// A station the server has no file for is the server's 404, which fails the
// probe: the collector has no series to give for it.
func TestTheMETARExampleFailsForAStationWithoutAReport(t *testing.T) {
	testutil.CaptureLogs(t)
	service, cfg := newMETAR(t)
	server := NewServer(config.NewManager(cfg, metarConfig, slog.Default()), "python3", slog.Default())
	response := probeOnce(t, server, "/probe?collector=metar&param_station=ZZZZ&retry_backoff=1ms&target="+service.URL, nil)
	if response.Code == 200 || !strings.Contains(response.Body.String(), "received HTTP status 404") {
		t.Errorf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// The target file is valid with the configuration beside it, as the exporter
// checks the two at startup. Scraped, each of its three airports asks for its
// own station's file, and its series carry the station the collector read
// from the report, the city the target adds and the target's name. The file
// names no station label of its own. It is used as shipped but for the
// address, which is the stand-in's instead of the server's.
func TestTheMETARExampleScrapesItsStaticTargets(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service, cfg := newMETAR(t)
	file, err := config.LoadStaticTargets(metarTargets)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateStaticTargets(file); err != nil {
		t.Fatalf("%s: %v", metarTargets, err)
	}
	if err := config.ValidateStaticTargetsAgainst(file, cfg); err != nil {
		t.Fatalf("%s does not match %s: %v", metarTargets, metarConfig, err)
	}
	if time.Duration(file.Interval) != 10*time.Minute || len(file.Targets) != 3 {
		t.Fatalf("interval=%s targets=%d, want the three airports every 10m", time.Duration(file.Interval), len(file.Targets))
	}
	for i := range file.Targets {
		if file.Targets[i].Target != "https://tgftp.nws.noaa.gov" {
			t.Errorf("target %s is scraped at %q, want the server", file.Targets[i].Name, file.Targets[i].Target)
		}
		file.Targets[i].Target = service.URL
		if _, set := file.Targets[i].Labels["station"]; set || file.Targets[i].Labels["city"] == "" {
			t.Errorf("target %s has the labels %v, want a city and no station: the collector reads the station", file.Targets[i].Name, file.Targets[i].Labels)
		}
	}

	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 0)
	body := getStaticTargets(t, server, "/static-targets")
	for _, want := range []string{
		`metar_observation_timestamp_seconds{city="Sofia",static_target="sofia",station="LBSF"} 1.791018e+09`,
		`metar_temperature_celsius{city="Sofia",static_target="sofia",station="LBSF"} 14`,
		`metar_visibility_meters{city="Sofia",static_target="sofia",station="LBSF"} 9999`,
		`metar_temperature_celsius{city="Burgas",static_target="burgas",station="LBBG"} 21`,
		`metar_wind_speed_meters_per_second{city="Burgas",static_target="burgas",station="LBBG"} 1.028888`,
		`metar_dew_point_celsius{city="Varna",static_target="varna",station="LBWN"} -1`,
		`metar_wind_gust_meters_per_second{city="Varna",static_target="varna",station="LBWN"} 12.346656`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	for _, unwanted := range []string{
		`metar_visibility_meters{city="Burgas"`,
		`metar_wind_direction_degrees{city="Burgas"`,
		`metar_wind_gust_meters_per_second{city="Sofia"`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("%s is a series of a report that has no such group", unwanted)
		}
	}
	asked := service.requests()
	slices.Sort(asked)
	if want := []string{metarStations + "LBBG.TXT", metarStations + "LBSF.TXT", metarStations + "LBWN.TXT"}; !slices.Equal(asked, want) {
		t.Errorf("the stand-in was asked %v, want the three stations: %v", asked, want)
	}
	if logs.Len() != 0 {
		t.Errorf("scraping the three airports logged:\n%s", logs)
	}
}
