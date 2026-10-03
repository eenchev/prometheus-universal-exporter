package transform

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The files of testdata/csv are CSV in the shapes it comes in, each written
// as the tool it stands for writes it (docs/DEVELOPMENT.md lists them, and
// internal/decode's csvfixtures test the rows each decodes into). These
// tests decode each and transform it with the rules a collector reading such
// a file would have, and hold every series, every rule's failures and every
// log line against what the file says.

func readCSVFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/csv/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// columns makes label rules of pairs of a label's name and the column it
// reads.
func columns(pairs ...string) []model.LabelRule {
	rules := make([]model.LabelRule, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		rules = append(rules, model.LabelRule{Name: pairs[i], Expression: pairs[i+1]})
	}
	return rules
}

// csvFixtureCollector is a collector reading CSV with the csv transform and
// rules, each a gauge logging its failures unless it says otherwise, as a
// loaded configuration has them, and checked as loading checks a rule.
func csvFixtureCollector(t *testing.T, response model.ResponseConfig, rules ...model.MetricRule) model.Collector {
	t.Helper()
	c := model.Collector{
		Name: "fixture", Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "csv"}, Response: response, Metrics: rules,
		Limits: model.Limits{MaxMetrics: 10000, ScriptTimeout: model.Duration(5 * time.Second), MaxOutputBytes: 1 << 20},
	}
	for i := range c.Metrics {
		rule := &c.Metrics[i]
		if rule.Type == "" {
			rule.Type = model.GaugeMetricType
		}
		if rule.ErrorMode == "" {
			rule.ErrorMode = model.ErrorModeLog
		}
		if err := CheckMetricRule(&c, rule); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// seriesLine writes a series as the text exposition writes its sample: the
// labels in name order, and a backslash, a quote and a line feed escaped.
func seriesLine(m model.Metric) string {
	escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	labels := make([]string, 0, len(m.Labels))
	for name, value := range m.Labels {
		labels = append(labels, name+`="`+escape.Replace(value)+`"`)
	}
	slices.Sort(labels)
	line := m.Name
	if len(labels) > 0 {
		line += "{" + strings.Join(labels, ",") + "}"
	}
	return line + " " + strconv.FormatFloat(m.Value, 'g', -1, 64)
}

// csvFixtureResult is what transforming a fixture gave: the series, sorted,
// each rule's failures as "<metric>: <n> failed, <n> missing, logged: <the
// first error>" (or "not logged"), sorted, and the transform's error.
type csvFixtureResult struct {
	series, failures []string
	err              error
}

// transformCSVFixture decodes a fixture as a response with the Content-Type
// given, if any, and transforms it. It holds the log against the report: a
// line for each rule that failed under log, with the number of its failures
// and the first of them, and no other line.
func transformCSVFixture(t *testing.T, c model.Collector, contentType, fixture string) csvFixtureResult {
	t.Helper()
	return transformCSVBody(t, c, contentType, readCSVFixture(t, fixture))
}

func transformCSVBody(t *testing.T, c model.Collector, contentType string, body []byte) csvFixtureResult {
	t.Helper()
	logs := testutil.CaptureLogs(t)
	r := &fetch.HTTPResponse{StatusCode: http.StatusOK, Body: body, Headers: http.Header{}}
	if contentType != "" {
		r.Headers.Set("Content-Type", contentType)
	}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	ctx, report := WithRuleReport(t.Context())
	set, err := Transform(ctx, d, r, &c, "python3")
	result := csvFixtureResult{err: err}
	if set != nil {
		for _, m := range set.Metrics {
			if m.Type != model.GaugeMetricType {
				t.Errorf("%s is a %s, want the gauge its rule makes", m.Name, m.Type)
			}
			result.series = append(result.series, seriesLine(m))
		}
	}
	slices.Sort(result.series)
	var logged []string
	for _, failure := range report.Failures() {
		how := "not logged"
		if failure.Logged {
			how = "logged"
			logged = append(logged, fmt.Sprintf("%s: %d: %v", failure.Metric, failure.Failures, failure.First))
		}
		result.failures = append(result.failures, fmt.Sprintf("%s: %d failed, %d missing, %s: %v", failure.Metric, failure.Failures, failure.Missing, how, failure.First))
	}
	slices.Sort(result.failures)
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Level, Msg, Collector, Metric, Error string
			ErrorMode                            string `json:"error_mode"`
			Failures                             uint64
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %s: %v", line, err)
		}
		if record.Level != "ERROR" || record.Msg != "metric extraction failed" || record.Collector != c.Name {
			t.Errorf("logged %s", line)
			continue
		}
		// A rule under fail is logged as it stops the transform, and is
		// in no report.
		if record.ErrorMode == model.ErrorModeFail {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %d: %s", record.Metric, record.Failures, record.Error))
	}
	slices.Sort(lines)
	slices.Sort(logged)
	if !slices.Equal(lines, logged) {
		t.Errorf("logged\n%s\nwant a line for each rule that failed under log:\n%s", strings.Join(lines, "\n"), strings.Join(logged, "\n"))
	}
	return result
}

// holds reports how the result differs from the series and the failures
// wanted, in any order.
func (r csvFixtureResult) holds(t *testing.T, series []string, failures ...string) {
	t.Helper()
	if r.err != nil {
		t.Errorf("the transform failed: %v", r.err)
	}
	series = slices.Sorted(slices.Values(series))
	if !slices.Equal(r.series, series) {
		t.Errorf("series:\n%s\nwant\n%s", strings.Join(r.series, "\n"), strings.Join(series, "\n"))
	}
	failures = slices.Sorted(slices.Values(failures))
	if !slices.Equal(r.failures, failures) {
		t.Errorf("failures:\n%s\nwant\n%s", strings.Join(r.failures, "\n"), strings.Join(failures, "\n"))
	}
}

// The series a fixture's rows become, each case a collector as someone
// reading such a file would write it. Every row gives a series of every rule
// whose column holds a number in it, with the labels read from the row's
// other columns, and what does not is the rule's failure, counted once per
// row and logged once per rule:
//
//   - RFC 4180: the labels carry the commas, quotes and line breaks of
//     quoted fields, and an empty field leaves its label off.
//   - Semicolons: whole numbers are read; a number with a decimal comma is
//     text that is no number, and fails its rule naming it.
//   - Tabs: an empty field is a missing value, logged for a required rule,
//     left out without a word for one that is not required, and counted
//     without a log line under ignore; trim_space trims the labels, and
//     the numbers are read without their blanks either way.
//   - A psql footer is a row without the columns the rules read: a missing
//     value of each, which required: false leaves out without a word.
//   - Without a header row the columns are numbers, from 1, and a number
//     past the row's last field is a missing value.
//   - Bodies in UTF-8 with a byte order mark, UTF-16 and legacy encodings
//     give the same series as their UTF-8 text, whether the byte order mark,
//     the Content-Type or response.charset names the encoding; with nothing
//     naming it the header's names are not the ones the rules read.
//   - Columns aligned with spaces are read with a space as the delimiter
//     and trim_space, a note of several words from its quotes.
//   - A delimiter ending each line leaves nothing behind; a row shorter than
//     the header is missing the values of the columns it lacks; blank lines
//     are no rows, a line of blanks is a row missing every value, and a last
//     line without a line end is read like the others.
func TestCSVFixturesBecomeTheSeriesOfTheirRows(t *testing.T) {
	optional, hundredth := false, 0.01
	semicolons := model.ResponseConfig{CSV: model.CSVConfig{Delimiter: ";"}}
	tabs := model.ResponseConfig{CSV: model.CSVConfig{Delimiter: "\t"}}
	tabsTrimmed := model.ResponseConfig{CSV: model.CSVConfig{Delimiter: "\t", TrimSpace: true}}
	pipes := model.ResponseConfig{CSV: model.CSVConfig{Delimiter: "|"}}
	noHeader := model.ResponseConfig{CSV: model.CSVConfig{Header: &optional}}
	colons := model.ResponseConfig{CSV: model.CSVConfig{Delimiter: ":", Header: &optional}}
	spaces := model.ResponseConfig{CSV: model.CSVConfig{Delimiter: " ", TrimSpace: true}}
	charset := func(name string) model.ResponseConfig { return model.ResponseConfig{Charset: name} }

	sensors := func(battery string) []model.MetricRule {
		return []model.MetricRule{
			{Name: "sensor_temperature_celsius", Expression: "temperature", Labels: columns("sensor", "sensor", "location", "location", "note", "note")},
			{Name: "sensor_humidity_percent", Expression: "humidity", Required: &optional, Labels: columns("sensor", "sensor")},
			{Name: "sensor_battery_percent", Expression: "battery", ErrorMode: battery, Labels: columns("sensor", "sensor")},
		}
	}
	queueLabels := columns("queue", "queue", "vhost", "vhost")
	queues := func(required *bool) []model.MetricRule {
		return []model.MetricRule{
			{Name: "queue_messages_ready", Expression: "messages_ready", Required: required, Labels: queueLabels},
			{Name: "queue_messages_unacked", Expression: "messages_unacked", Required: required, Labels: queueLabels},
			{Name: "queue_consumers", Expression: "consumers", Required: required, Labels: queueLabels},
			{Name: "queue_state", Expression: "state", Required: required, ValueMap: map[string]float64{"running": 1, "idle": 1, "flow": 0.5, "down": 0}, Labels: queueLabels},
		}
	}
	readingLabels := columns("sensor", "2", "at", "1")
	cities := []model.MetricRule{
		{Name: "city_temperature_celsius", Expression: "température", Labels: columns("city", "città", "country", "Land", "state", "状態", "sky", "sky")},
		{Name: "city_humidity_percent", Expression: "влажност", Labels: columns("city", "città")},
	}
	oblasti := []model.MetricRule{
		{Name: "city_temperature_celsius", Expression: "температура", Labels: columns("city", "град", "province", "област", "state", "състояние")},
		{Name: "city_humidity_percent", Expression: "влажност", Labels: columns("city", "град")},
	}
	nodeLabels := columns("node", "NODE", "role", "ROLE")
	jobs := []model.MetricRule{
		{Name: "job_duration_seconds", Expression: "duration_seconds", Labels: columns("job", "job", "state", "state")},
		{Name: "job_records", Expression: "records", Labels: columns("job", "job")},
	}

	wantSensors := []string{
		`sensor_battery_percent{sensor="th-01"} 98`,
		`sensor_battery_percent{sensor="th-02"} 97`,
		`sensor_battery_percent{sensor="th-04"} 12`,
		`sensor_battery_percent{sensor="th-06"} 64`,
		`sensor_battery_percent{sensor="th-07"} 81`,
		`sensor_battery_percent{sensor="th-09"} 100`,
		`sensor_battery_percent{sensor="th-10"} 55`,
		`sensor_battery_percent{sensor="th-11"} 9`,
		`sensor_battery_percent{sensor="th-12"} 77`,
		`sensor_humidity_percent{sensor="th-01"} 71`,
		`sensor_humidity_percent{sensor="th-02"} 68`,
		`sensor_humidity_percent{sensor="th-04"} 40`,
		`sensor_humidity_percent{sensor="th-05"} 33`,
		`sensor_humidity_percent{sensor="th-06"} 88`,
		`sensor_humidity_percent{sensor="th-07"} 45`,
		`sensor_humidity_percent{sensor="th-09"} 59`,
		`sensor_humidity_percent{sensor="th-11"} 52`,
		`sensor_humidity_percent{sensor="th-12"} 93`,
		`sensor_temperature_celsius{location="  \"dock, east\"",sensor="th-03"} 17.9`,
		`sensor_temperature_celsius{location=" office ",note="battery low",sensor="th-04"} 21.5`,
		`sensor_temperature_celsius{location="cold room 1",note="ok",sensor="th-01"} 4.2`,
		`sensor_temperature_celsius{location="freezer",note=" battery low ",sensor="th-11"} -18.4`,
		`sensor_temperature_celsius{location="garage",sensor="th-09"} 8.75`,
		`sensor_temperature_celsius{location="greenhouse",sensor="th-12"} 28.6`,
		`sensor_temperature_celsius{location="lab",note="calibrating",sensor="th-10"} 22`,
		`sensor_temperature_celsius{location="roof",note="north	side",sensor="th-06"} -3.5`,
		`sensor_temperature_celsius{location="server room",note="door open",sensor="th-05"} 24.1`,
		`sensor_temperature_celsius{sensor="th-07"} 19`,
	}
	wantSensorsTrimmed := []string{
		`sensor_battery_percent{sensor="th-01"} 98`,
		`sensor_battery_percent{sensor="th-02"} 97`,
		`sensor_battery_percent{sensor="th-04"} 12`,
		`sensor_battery_percent{sensor="th-06"} 64`,
		`sensor_battery_percent{sensor="th-07"} 81`,
		`sensor_battery_percent{sensor="th-09"} 100`,
		`sensor_battery_percent{sensor="th-10"} 55`,
		`sensor_battery_percent{sensor="th-11"} 9`,
		`sensor_battery_percent{sensor="th-12"} 77`,
		`sensor_humidity_percent{sensor="th-01"} 71`,
		`sensor_humidity_percent{sensor="th-02"} 68`,
		`sensor_humidity_percent{sensor="th-04"} 40`,
		`sensor_humidity_percent{sensor="th-05"} 33`,
		`sensor_humidity_percent{sensor="th-06"} 88`,
		`sensor_humidity_percent{sensor="th-07"} 45`,
		`sensor_humidity_percent{sensor="th-09"} 59`,
		`sensor_humidity_percent{sensor="th-11"} 52`,
		`sensor_humidity_percent{sensor="th-12"} 93`,
		`sensor_temperature_celsius{location="cold room 1",note="ok",sensor="th-01"} 4.2`,
		`sensor_temperature_celsius{location="dock, east",sensor="th-03"} 17.9`,
		`sensor_temperature_celsius{location="freezer",note="battery low",sensor="th-11"} -18.4`,
		`sensor_temperature_celsius{location="garage",sensor="th-09"} 8.75`,
		`sensor_temperature_celsius{location="greenhouse",sensor="th-12"} 28.6`,
		`sensor_temperature_celsius{location="lab",note="calibrating",sensor="th-10"} 22`,
		`sensor_temperature_celsius{location="office",note="battery low",sensor="th-04"} 21.5`,
		`sensor_temperature_celsius{location="roof",note="north	side",sensor="th-06"} -3.5`,
		`sensor_temperature_celsius{location="server room",note="door open",sensor="th-05"} 24.1`,
		`sensor_temperature_celsius{sensor="th-07"} 19`,
	}
	wantQueues := []string{
		`queue_consumers{queue="audit",vhost="/"} 1`,
		`queue_consumers{queue="emails",vhost="/notify"} 6`,
		`queue_consumers{queue="emails.bounce",vhost="/notify"} 1`,
		`queue_consumers{queue="metrics",vhost="/"} 2`,
		`queue_consumers{queue="orders",vhost="/shop"} 4`,
		`queue_consumers{queue="orders.dead",vhost="/shop"} 0`,
		`queue_consumers{queue="payments",vhost="/shop"} 2`,
		`queue_consumers{queue="payments.retry",vhost="/shop"} 1`,
		`queue_consumers{queue="search.index",vhost="/shop"} 8`,
		`queue_consumers{queue="shipping",vhost="/shop"} 3`,
		`queue_consumers{queue="sms",vhost="/notify"} 0`,
		`queue_consumers{queue="thumbnails",vhost="/media"} 0`,
		`queue_messages_ready{queue="audit",vhost="/"} 0`,
		`queue_messages_ready{queue="emails",vhost="/notify"} 4411`,
		`queue_messages_ready{queue="emails.bounce",vhost="/notify"} 2`,
		`queue_messages_ready{queue="metrics",vhost="/"} 51000`,
		`queue_messages_ready{queue="orders",vhost="/shop"} 120`,
		`queue_messages_ready{queue="orders.dead",vhost="/shop"} 17`,
		`queue_messages_ready{queue="payments",vhost="/shop"} 0`,
		`queue_messages_ready{queue="payments.retry",vhost="/shop"} 342`,
		`queue_messages_ready{queue="search.index",vhost="/shop"} 7`,
		`queue_messages_ready{queue="shipping",vhost="/shop"} 9`,
		`queue_messages_ready{queue="sms",vhost="/notify"} 88`,
		`queue_messages_ready{queue="thumbnails",vhost="/media"} 0`,
		`queue_messages_unacked{queue="audit",vhost="/"} 0`,
		`queue_messages_unacked{queue="emails",vhost="/notify"} 64`,
		`queue_messages_unacked{queue="emails.bounce",vhost="/notify"} 0`,
		`queue_messages_unacked{queue="metrics",vhost="/"} 500`,
		`queue_messages_unacked{queue="orders",vhost="/shop"} 3`,
		`queue_messages_unacked{queue="orders.dead",vhost="/shop"} 0`,
		`queue_messages_unacked{queue="payments",vhost="/shop"} 1`,
		`queue_messages_unacked{queue="payments.retry",vhost="/shop"} 12`,
		`queue_messages_unacked{queue="search.index",vhost="/shop"} 8`,
		`queue_messages_unacked{queue="shipping",vhost="/shop"} 0`,
		`queue_messages_unacked{queue="sms",vhost="/notify"} 0`,
		`queue_messages_unacked{queue="thumbnails",vhost="/media"} 0`,
		`queue_state{queue="audit",vhost="/"} 1`,
		`queue_state{queue="emails",vhost="/notify"} 0.5`,
		`queue_state{queue="emails.bounce",vhost="/notify"} 1`,
		`queue_state{queue="metrics",vhost="/"} 0.5`,
		`queue_state{queue="orders",vhost="/shop"} 1`,
		`queue_state{queue="orders.dead",vhost="/shop"} 1`,
		`queue_state{queue="payments",vhost="/shop"} 1`,
		`queue_state{queue="payments.retry",vhost="/shop"} 0.5`,
		`queue_state{queue="search.index",vhost="/shop"} 1`,
		`queue_state{queue="shipping",vhost="/shop"} 1`,
		`queue_state{queue="sms",vhost="/notify"} 0`,
		`queue_state{queue="thumbnails",vhost="/media"} 0`,
	}
	wantCities := []string{
		`city_humidity_percent{city="Kraków"} 90`,
		`city_humidity_percent{city="Québec, QC"} 74`,
		`city_humidity_percent{city="Reykjavík"} 85`,
		`city_humidity_percent{city="São Paulo"} 81`,
		`city_humidity_percent{city="Zürich"} 78`,
		`city_humidity_percent{city="Đà Nẵng"} 88`,
		`city_humidity_percent{city="İstanbul"} 66`,
		`city_humidity_percent{city="Αθήνα"} 48`,
		`city_humidity_percent{city="Пловдив"} 55`,
		`city_humidity_percent{city="София"} 62`,
		`city_humidity_percent{city="北京"} 35`,
		`city_humidity_percent{city="東京"} 70`,
		`city_temperature_celsius{city="Kraków",country="Polska",sky="🌫",state="霧"} 7.5`,
		`city_temperature_celsius{city="Québec, QC",country="Canada",sky="🌧",state="雨"} 5`,
		`city_temperature_celsius{city="Reykjavík",country="Ísland",sky="❄️",state="雪"} 3`,
		`city_temperature_celsius{city="São Paulo",country="Brasil",sky="🌧",state="雨"} 23`,
		`city_temperature_celsius{city="Zürich",country="Schweiz",sky="☁️",state="曇り"} 9.25`,
		`city_temperature_celsius{city="Đà Nẵng",country="Việt Nam",sky="⛈",state="雷雨"} 29.5`,
		`city_temperature_celsius{city="İstanbul",country="Türkiye",sky="🌥",state="曇り"} 16`,
		`city_temperature_celsius{city="Αθήνα",country="Ελλάδα",sky="☀️",state="晴れ"} 21`,
		`city_temperature_celsius{city="Пловдив",country="България",sky="🌤",state="晴れ"} 17`,
		`city_temperature_celsius{city="София",country="България",sky="☀️",state="晴れ"} 14.5`,
		`city_temperature_celsius{city="北京",country="中国",sky="🌤",state="晴れ"} 12`,
		`city_temperature_celsius{city="東京",country="日本",sky="☁️",state="曇り"} 19`,
	}
	wantOblasti := []string{
		`city_humidity_percent{city="Банско, ски зона"} 91`,
		`city_humidity_percent{city="Благоевград"} 60`,
		`city_humidity_percent{city="Бургас"} 69`,
		`city_humidity_percent{city="Варна"} 71`,
		`city_humidity_percent{city="Велико Търново"} 80`,
		`city_humidity_percent{city="Плевен"} 77`,
		`city_humidity_percent{city="Пловдив"} 55`,
		`city_humidity_percent{city="Русе"} 74`,
		`city_humidity_percent{city="София"} 62`,
		`city_humidity_percent{city="Стара Загора"} 58`,
		`city_temperature_celsius{city="Банско, ски зона",province="Благоевград",state="сняг"} -2.5`,
		`city_temperature_celsius{city="Благоевград",province="Благоевград",state="слънчево"} 15`,
		`city_temperature_celsius{city="Бургас",province="Бургас",state="дъжд"} 19`,
		`city_temperature_celsius{city="Варна",province="Варна",state="облачно"} 18.25`,
		`city_temperature_celsius{city="Велико Търново",province="Велико Търново",state="дъжд"} 11.75`,
		`city_temperature_celsius{city="Плевен",province="Плевен",state="облачно"} 12`,
		`city_temperature_celsius{city="Пловдив",province="Пловдив",state="слънчево"} 17`,
		`city_temperature_celsius{city="Русе",province="Русе",state="мъгла"} 13`,
		`city_temperature_celsius{city="София",province="София-град",state="слънчево"} 14.5`,
		`city_temperature_celsius{city="Стара Загора",province="Стара Загора",state="слънчево"} 16.5`,
	}
	wantJobs := []string{
		`job_duration_seconds{job="backup-db",state="ok"} 412.5`,
		`job_duration_seconds{job="backup-files",state="ok"} 1290`,
		`job_duration_seconds{job="export-billing",state="ok"} 96.75`,
		`job_duration_seconds{job="prune-cache",state="ok"} 0.5`,
		`job_duration_seconds{job="reindex",state="failed"} 7.5`,
		`job_duration_seconds{job="renew-certs",state="skipped"} 0`,
		`job_duration_seconds{job="rotate-logs",state="ok"} 3.25`,
		`job_duration_seconds{job="send-reports",state="ok"} 31`,
		`job_duration_seconds{job="sync-ldap",state="ok"} 12`,
		`job_duration_seconds{job="vacuum",state="ok"} 48`,
		`job_records{job="backup-db"} 18250`,
		`job_records{job="backup-files"} 96412`,
		`job_records{job="export-billing"} 5120`,
		`job_records{job="prune-cache"} 221`,
		`job_records{job="reindex"} 0`,
		`job_records{job="renew-certs"} 0`,
		`job_records{job="rotate-logs"} 14`,
		`job_records{job="send-reports"} 64`,
		`job_records{job="sync-ldap"} 830`,
		`job_records{job="vacuum"} 0`,
	}

	for _, tc := range []struct {
		name, fixture, contentType string
		response                   model.ResponseConfig
		rules                      []model.MetricRule
		series, failures           []string
	}{
		{name: "an RFC 4180 export", fixture: "tickets-rfc4180.csv", contentType: "text/csv", rules: []model.MetricRule{
			{Name: "ticket_age_hours", Expression: "age_hours", Labels: columns("id", "id", "queue", "queue", "customer", "customer", "subject", "subject")},
			{Name: "ticket_replies", Expression: "replies", Labels: columns("id", "id")},
		}, series: []string{
			`ticket_age_hours{customer="Acme, Inc.",id="10231",queue="billing",subject="Invoice 2026-0917 charged twice"} 52.5`,
			`ticket_age_hours{customer="Acme, Inc.",id="10237",queue="accounts",subject="Password reset for \"j.doe\", locked out"} 0.25`,
			`ticket_age_hours{customer="Globex",id="10242",queue="network"} 3`,
			`ticket_age_hours{customer="Globex, Ltd.",id="10234",queue="hardware",subject="Rack 12, unit 4: fan alarm"} 1.5`,
			`ticket_age_hours{customer="Hooli, LLC",id="10236",queue="network",subject="Wi-Fi guest portal certificate expired"} 6.75`,
			`ticket_age_hours{customer="Initech",id="10233",queue="hardware",subject="Printer says \"PC LOAD LETTER\""} 211.25`,
			`ticket_age_hours{customer="Initech",id="10235",queue="network",subject="VPN drops every 30 min\n(since the firewall change)"} 18`,
			`ticket_age_hours{customer="Initech",id="10241",queue="billing",subject="Quote for 5\" tablets, 20 units"} 96`,
			`ticket_age_hours{customer="Müller & Söhne GmbH",id="10232",queue="billing",subject="Refund for order #5541, second request"} 30`,
			`ticket_age_hours{customer="Stark Industries",id="10239",queue="software",subject="Crash on export: \"index out of range\"\nSteps:\n1. open report, 2. click \"Export\""} 12`,
			`ticket_age_hours{customer="Umbrella Corp",id="10238",queue="accounts",subject="New starter needs access"} 73`,
			`ticket_age_hours{customer="Wayne Enterprises, Inc.",id="10240",queue="software",subject="Licence renewal"} 340`,
			`ticket_replies{id="10231"} 4`,
			`ticket_replies{id="10232"} 2`,
			`ticket_replies{id="10233"} 7`,
			`ticket_replies{id="10234"} 0`,
			`ticket_replies{id="10235"} 3`,
			`ticket_replies{id="10236"} 1`,
			`ticket_replies{id="10237"} 0`,
			`ticket_replies{id="10238"} 5`,
			`ticket_replies{id="10239"} 2`,
			`ticket_replies{id="10240"} 9`,
			`ticket_replies{id="10241"} 1`,
			`ticket_replies{id="10242"} 0`,
		}},
		{name: "semicolons and decimal commas", fixture: "inventory-semicolon.csv", contentType: "text/csv", response: semicolons, rules: []model.MetricRule{
			{Name: "stock_items", Expression: "Bestand", Labels: columns("item", "Artikel", "site", "Lager")},
			{Name: "stock_items_minimum", Expression: "Mindestbestand", Labels: columns("item", "Artikel", "site", "Lager")},
			{Name: "stock_price_euros", Expression: "Preis", Labels: columns("item", "Artikel")},
			{Name: "stock_shelf_used_ratio", Expression: "Auslastung", Scale: &hundredth, Labels: columns("item", "Artikel")},
		}, series: []string{
			`stock_items_minimum{item="Dübel 8x40",site="Leipzig"} 1500`,
			`stock_items_minimum{item="Gewindestange M8",site="Leipzig"} 100`,
			`stock_items_minimum{item="Kabelbinder 200 mm",site="Hamburg"} 8000`,
			`stock_items_minimum{item="Mutter M4",site="Hamburg"} 5000`,
			`stock_items_minimum{item="Mutter M6",site="München"} 5000`,
			`stock_items_minimum{item="Schlossschraube M8x60",site="München"} 500`,
			`stock_items_minimum{item="Schraube M4x20",site="Hamburg"} 2000`,
			`stock_items_minimum{item="Schraube M6x40",site="Hamburg"} 2000`,
			`stock_items_minimum{item="Stahlträger IPE 100",site="Leipzig"} 10`,
			`stock_items_minimum{item="Unterlegscheibe 4,3",site="München"} 10000`,
			`stock_items_minimum{item="Unterlegscheibe 6,4",site="München"} 10000`,
			`stock_items_minimum{item="Winkelverbinder 90",site="Leipzig"} 400`,
			`stock_items{item="Dübel 8x40",site="Leipzig"} 9600`,
			`stock_items{item="Gewindestange M8",site="Leipzig"} 320`,
			`stock_items{item="Kabelbinder 200 mm",site="Hamburg"} 52000`,
			`stock_items{item="Mutter M4",site="Hamburg"} 30000`,
			`stock_items{item="Mutter M6",site="München"} 18250`,
			`stock_items{item="Schlossschraube M8x60",site="München"} 2750`,
			`stock_items{item="Schraube M4x20",site="Hamburg"} 12500`,
			`stock_items{item="Schraube M6x40",site="Hamburg"} 8400`,
			`stock_items{item="Stahlträger IPE 100",site="Leipzig"} 48`,
			`stock_items{item="Unterlegscheibe 4,3",site="München"} 44000`,
			`stock_items{item="Unterlegscheibe 6,4",site="München"} 0`,
			`stock_items{item="Winkelverbinder 90",site="Leipzig"} 1240`,
			`stock_shelf_used_ratio{item="Dübel 8x40"} 0.48`,
			`stock_shelf_used_ratio{item="Gewindestange M8"} 0.16`,
			`stock_shelf_used_ratio{item="Kabelbinder 200 mm"} 0.65`,
			`stock_shelf_used_ratio{item="Mutter M4"} 0.75`,
			`stock_shelf_used_ratio{item="Schraube M6x40"} 0.42`,
			`stock_shelf_used_ratio{item="Unterlegscheibe 4,3"} 0.88`,
			`stock_shelf_used_ratio{item="Unterlegscheibe 6,4"} 0`,
			`stock_shelf_used_ratio{item="Winkelverbinder 90"} 0.31`,
		}, failures: []string{
			`stock_price_euros: 12 failed, 0 missing, logged: value "0,04" is not a number; map text to numbers with value_map`,
			`stock_shelf_used_ratio: 4 failed, 0 missing, logged: value "62,5" is not a number; map text to numbers with value_map`,
		}},
		{name: "tabs", fixture: "sensors.tsv", contentType: "text/tab-separated-values", response: tabs, rules: sensors(model.ErrorModeLog), series: wantSensors, failures: []string{
			`sensor_temperature_celsius: 2 failed, 2 missing, logged: CSV column "temperature" is missing`,
			`sensor_battery_percent: 3 failed, 3 missing, logged: CSV column "battery" is missing`,
		}},
		{name: "tabs, trimmed", fixture: "sensors.tsv", contentType: "text/tab-separated-values", response: tabsTrimmed, rules: sensors(model.ErrorModeLog), series: wantSensorsTrimmed, failures: []string{
			`sensor_temperature_celsius: 2 failed, 2 missing, logged: CSV column "temperature" is missing`,
			`sensor_battery_percent: 3 failed, 3 missing, logged: CSV column "battery" is missing`,
		}},
		{name: "tabs, the battery under ignore", fixture: "sensors.tsv", contentType: "text/tab-separated-values", response: tabsTrimmed, rules: sensors(model.ErrorModeIgnore), series: wantSensorsTrimmed, failures: []string{
			`sensor_temperature_celsius: 2 failed, 2 missing, logged: CSV column "temperature" is missing`,
			`sensor_battery_percent: 3 failed, 3 missing, not logged: CSV column "battery" is missing`,
		}},
		{name: "a psql result with its footer", fixture: "queues-pipe.txt", contentType: "text/plain", response: pipes, rules: queues(nil), series: wantQueues, failures: []string{
			`queue_messages_ready: 1 failed, 1 missing, logged: CSV column "messages_ready" is missing`,
			`queue_messages_unacked: 1 failed, 1 missing, logged: CSV column "messages_unacked" is missing`,
			`queue_consumers: 1 failed, 1 missing, logged: CSV column "consumers" is missing`,
			`queue_state: 1 failed, 1 missing, logged: CSV column "state" is missing`,
		}},
		{name: "a psql result, its rules not required", fixture: "queues-pipe.txt", contentType: "text/plain", response: pipes, rules: queues(&optional), series: wantQueues},
		{name: "colons and no header", fixture: "accounts-colon.txt", contentType: "text/plain", response: colons, rules: []model.MetricRule{
			{Name: "account_uid", Expression: "3", Labels: columns("user", "1", "shell", "7", "comment", "5")},
			{Name: "account_gid", Expression: "4", Labels: columns("user", "1")},
		}, series: []string{
			`account_gid{user="alice"} 1000`,
			`account_gid{user="backup"} 34`,
			`account_gid{user="bin"} 2`,
			`account_gid{user="daemon"} 1`,
			`account_gid{user="deploy"} 1001`,
			`account_gid{user="nobody"} 65534`,
			`account_gid{user="postgres"} 120`,
			`account_gid{user="prometheus"} 1002`,
			`account_gid{user="root"} 0`,
			`account_gid{user="sshd"} 65534`,
			`account_gid{user="sys"} 3`,
			`account_gid{user="systemd-network"} 998`,
			`account_gid{user="www-data"} 33`,
			`account_uid{comment="Alice Example,Room 12,+359 2 555 0100,",shell="/bin/zsh",user="alice"} 1000`,
			`account_uid{comment="Deploy \"bot\" user",shell="/bin/sh",user="deploy"} 1001`,
			`account_uid{comment="PostgreSQL administrator,,,",shell="/bin/bash",user="postgres"} 112`,
			`account_uid{comment="backup",shell="/usr/sbin/nologin",user="backup"} 34`,
			`account_uid{comment="bin",shell="/usr/sbin/nologin",user="bin"} 2`,
			`account_uid{comment="daemon",shell="/usr/sbin/nologin",user="daemon"} 1`,
			`account_uid{comment="nobody",shell="/usr/sbin/nologin",user="nobody"} 65534`,
			`account_uid{comment="root",shell="/bin/bash",user="root"} 0`,
			`account_uid{comment="sys",shell="/usr/sbin/nologin",user="sys"} 3`,
			`account_uid{comment="systemd Network Management",shell="/usr/sbin/nologin",user="systemd-network"} 998`,
			`account_uid{comment="www-data",shell="/usr/sbin/nologin",user="www-data"} 33`,
			`account_uid{shell="/usr/sbin/nologin",user="prometheus"} 1002`,
			`account_uid{shell="/usr/sbin/nologin",user="sshd"} 105`,
		}},
		{name: "no header", fixture: "readings-noheader.csv", contentType: "text/csv", response: noHeader, rules: []model.MetricRule{
			{Name: "reading_temperature_celsius", Expression: "3", Labels: readingLabels},
			{Name: "reading_humidity_percent", Expression: "4", Labels: readingLabels},
			{Name: "reading_ok", Expression: "5", ValueMap: map[string]float64{"ok": 1, "low-battery": 0}, Labels: readingLabels},
			{Name: "reading_pressure_hectopascals", Expression: "6", Labels: readingLabels},
		}, series: []string{
			`reading_humidity_percent{at="2026-10-03T09:00:00Z",sensor="th-01"} 71`,
			`reading_humidity_percent{at="2026-10-03T09:00:00Z",sensor="th-02"} 68`,
			`reading_humidity_percent{at="2026-10-03T09:00:00Z",sensor="th-03"} 55`,
			`reading_humidity_percent{at="2026-10-03T09:00:00Z",sensor="th-04"} 40`,
			`reading_humidity_percent{at="2026-10-03T09:05:00Z",sensor="th-01"} 72`,
			`reading_humidity_percent{at="2026-10-03T09:05:00Z",sensor="th-02"} 69`,
			`reading_humidity_percent{at="2026-10-03T09:05:00Z",sensor="th-03"} 56`,
			`reading_humidity_percent{at="2026-10-03T09:05:00Z",sensor="th-04"} 41`,
			`reading_humidity_percent{at="2026-10-03T09:10:00Z",sensor="th-01"} 73`,
			`reading_humidity_percent{at="2026-10-03T09:10:00Z",sensor="th-02"} 70`,
			`reading_humidity_percent{at="2026-10-03T09:10:00Z",sensor="th-03"} 57`,
			`reading_humidity_percent{at="2026-10-03T09:10:00Z",sensor="th-04"} 42`,
			`reading_humidity_percent{at="2026-10-03T09:15:00Z",sensor="th-01"} 74`,
			`reading_humidity_percent{at="2026-10-03T09:15:00Z",sensor="th-02"} 71`,
			`reading_humidity_percent{at="2026-10-03T09:15:00Z",sensor="th-03"} 58`,
			`reading_humidity_percent{at="2026-10-03T09:15:00Z",sensor="th-04"} 43`,
			`reading_ok{at="2026-10-03T09:00:00Z",sensor="th-01"} 1`,
			`reading_ok{at="2026-10-03T09:00:00Z",sensor="th-02"} 1`,
			`reading_ok{at="2026-10-03T09:00:00Z",sensor="th-03"} 1`,
			`reading_ok{at="2026-10-03T09:00:00Z",sensor="th-04"} 0`,
			`reading_ok{at="2026-10-03T09:05:00Z",sensor="th-01"} 1`,
			`reading_ok{at="2026-10-03T09:05:00Z",sensor="th-02"} 1`,
			`reading_ok{at="2026-10-03T09:05:00Z",sensor="th-03"} 1`,
			`reading_ok{at="2026-10-03T09:05:00Z",sensor="th-04"} 0`,
			`reading_ok{at="2026-10-03T09:10:00Z",sensor="th-01"} 1`,
			`reading_ok{at="2026-10-03T09:10:00Z",sensor="th-02"} 1`,
			`reading_ok{at="2026-10-03T09:10:00Z",sensor="th-03"} 1`,
			`reading_ok{at="2026-10-03T09:10:00Z",sensor="th-04"} 0`,
			`reading_ok{at="2026-10-03T09:15:00Z",sensor="th-01"} 1`,
			`reading_ok{at="2026-10-03T09:15:00Z",sensor="th-02"} 1`,
			`reading_ok{at="2026-10-03T09:15:00Z",sensor="th-03"} 1`,
			`reading_ok{at="2026-10-03T09:15:00Z",sensor="th-04"} 0`,
			`reading_temperature_celsius{at="2026-10-03T09:00:00Z",sensor="th-01"} 4.2`,
			`reading_temperature_celsius{at="2026-10-03T09:00:00Z",sensor="th-02"} 4.61`,
			`reading_temperature_celsius{at="2026-10-03T09:00:00Z",sensor="th-03"} 17.92`,
			`reading_temperature_celsius{at="2026-10-03T09:00:00Z",sensor="th-04"} 21.53`,
			`reading_temperature_celsius{at="2026-10-03T09:05:00Z",sensor="th-01"} 4.3`,
			`reading_temperature_celsius{at="2026-10-03T09:05:00Z",sensor="th-02"} 4.71`,
			`reading_temperature_celsius{at="2026-10-03T09:05:00Z",sensor="th-03"} 18.02`,
			`reading_temperature_celsius{at="2026-10-03T09:05:00Z",sensor="th-04"} 21.63`,
			`reading_temperature_celsius{at="2026-10-03T09:10:00Z",sensor="th-01"} 4.4`,
			`reading_temperature_celsius{at="2026-10-03T09:10:00Z",sensor="th-02"} 4.81`,
			`reading_temperature_celsius{at="2026-10-03T09:10:00Z",sensor="th-03"} 18.12`,
			`reading_temperature_celsius{at="2026-10-03T09:10:00Z",sensor="th-04"} 21.73`,
			`reading_temperature_celsius{at="2026-10-03T09:15:00Z",sensor="th-01"} 4.5`,
			`reading_temperature_celsius{at="2026-10-03T09:15:00Z",sensor="th-02"} 4.91`,
			`reading_temperature_celsius{at="2026-10-03T09:15:00Z",sensor="th-03"} 18.22`,
			`reading_temperature_celsius{at="2026-10-03T09:15:00Z",sensor="th-04"} 21.83`,
		}, failures: []string{
			`reading_pressure_hectopascals: 16 failed, 16 missing, logged: CSV column "6" is missing`,
		}},
		{name: "UTF-8 with a byte order mark", fixture: "cities-utf8-bom.csv", contentType: "text/csv", rules: cities, series: wantCities},
		{name: "UTF-8 with a byte order mark, and a Content-Type naming another encoding", fixture: "cities-utf8-bom.csv", contentType: "text/csv; charset=windows-1252", rules: cities, series: wantCities},
		{name: "UTF-16 with a byte order mark", fixture: "cities-utf16le-bom.csv", contentType: "text/csv", rules: cities, series: wantCities},
		{name: "UTF-16 with a byte order mark, and response.charset naming another encoding", fixture: "cities-utf16le-bom.csv", response: charset("windows-1251"), rules: cities, series: wantCities},
		{name: "UTF-16 by the Content-Type", fixture: "cities-utf16be.csv", contentType: "text/csv; charset=utf-16be", rules: cities, series: wantCities},
		{name: "UTF-16 by response.charset", fixture: "cities-utf16be.csv", response: charset("utf-16be"), rules: cities, series: wantCities},
		{name: "UTF-8 without a byte order mark", fixture: "oblasti-utf8.csv", contentType: "text/csv; charset=utf-8", rules: oblasti, series: wantOblasti},
		{name: "windows-1251 by the Content-Type", fixture: "oblasti-windows-1251.csv", contentType: "text/csv; charset=windows-1251", rules: oblasti, series: wantOblasti},
		{name: "windows-1251 by response.charset", fixture: "oblasti-windows-1251.csv", response: charset("windows-1251"), rules: oblasti, series: wantOblasti},
		{name: "windows-1251 by response.charset, and a Content-Type saying UTF-8", fixture: "oblasti-windows-1251.csv", contentType: "text/csv; charset=utf-8", response: charset("windows-1251"), rules: oblasti, series: wantOblasti},
		{name: "windows-1251 that nothing names", fixture: "oblasti-windows-1251.csv", contentType: "text/csv", rules: oblasti, failures: []string{
			`city_temperature_celsius: 10 failed, 10 missing, logged: CSV column "температура" is missing`,
			`city_humidity_percent: 10 failed, 10 missing, logged: CSV column "влажност" is missing`,
		}},
		{name: "ISO 8859-1 by the Content-Type", fixture: "communes-iso-8859-1.csv", contentType: "text/csv; charset=ISO-8859-1", rules: []model.MetricRule{
			{Name: "city_temperature_celsius", Expression: "température", Labels: columns("city", "commune", "department", "département", "state", "état")},
			{Name: "city_humidity_percent", Expression: "humidité", Labels: columns("city", "commune")},
		}, series: []string{
			`city_humidity_percent{city="Angoulême"} 68`,
			`city_humidity_percent{city="Besançon"} 76`,
			`city_humidity_percent{city="Bâle (aéroport)"} 79`,
			`city_humidity_percent{city="Châlons-en-Champagne"} 83`,
			`city_humidity_percent{city="Fort-de-France"} 85`,
			`city_humidity_percent{city="L'Haÿ-les-Roses"} 72`,
			`city_humidity_percent{city="Nîmes"} 44`,
			`city_humidity_percent{city="Orléans"} 70`,
			`city_humidity_percent{city="Saint-Étienne"} 81`,
			`city_humidity_percent{city="Sète, port"} 61`,
			`city_temperature_celsius{city="Angoulême",department="Charente",state="pluie"} 15`,
			`city_temperature_celsius{city="Besançon",department="Doubs",state="nuageux"} 11.5`,
			`city_temperature_celsius{city="Bâle (aéroport)",department="Haut-Rhin",state="neige fondue"} 8`,
			`city_temperature_celsius{city="Châlons-en-Champagne",department="Marne",state="pluie"} 9.5`,
			`city_temperature_celsius{city="Fort-de-France",department="Martinique",state="orage"} 30`,
			`city_temperature_celsius{city="L'Haÿ-les-Roses",department="Val-de-Marne",state="nuageux"} 12`,
			`city_temperature_celsius{city="Nîmes",department="Gard",state="ensoleillé"} 21.25`,
			`city_temperature_celsius{city="Orléans",department="Loiret",state="ensoleillé"} 13`,
			`city_temperature_celsius{city="Saint-Étienne",department="Loire",state="brouillard"} 10`,
			`city_temperature_celsius{city="Sète, port",department="Hérault",state="ensoleillé"} 22`,
		}},
		{name: "columns aligned with spaces", fixture: "nodes-space-aligned.txt", contentType: "text/plain", response: spaces, rules: []model.MetricRule{
			{Name: "node_cpu_percent", Expression: "CPU", Labels: nodeLabels},
			{Name: "node_memory_percent", Expression: "MEM", Labels: nodeLabels},
			{Name: "node_pods", Expression: "PODS", Labels: nodeLabels},
			{Name: "node_age_days", Expression: "AGE_DAYS", Labels: nodeLabels},
			{Name: "node_ready", Expression: "STATUS", ValueMap: map[string]float64{"Ready": 1, "NotReady": 0},
				Labels: append(slices.Clone(nodeLabels), model.LabelRule{Name: "note", Expression: "NOTE", ValueMap: map[string]string{"-": ""}})},
		}, series: []string{
			`node_age_days{node="batch01",role="worker"} 3`,
			`node_age_days{node="batch02",role="worker"} 88`,
			`node_age_days{node="cache01",role="worker"} 96`,
			`node_age_days{node="cp01",role="control"} 401`,
			`node_age_days{node="cp02",role="control"} 401`,
			`node_age_days{node="cp03",role="control"} 399`,
			`node_age_days{node="db01",role="worker"} 401`,
			`node_age_days{node="db02",role="worker"} 401`,
			`node_age_days{node="ingress01",role="edge"} 150`,
			`node_age_days{node="ingress02",role="edge"} 150`,
			`node_age_days{node="web01",role="worker"} 212`,
			`node_age_days{node="web02",role="worker"} 212`,
			`node_age_days{node="web03",role="worker"} 17`,
			`node_cpu_percent{node="batch01",role="worker"} 0`,
			`node_cpu_percent{node="batch02",role="worker"} 97`,
			`node_cpu_percent{node="cache01",role="worker"} 12`,
			`node_cpu_percent{node="cp01",role="control"} 18`,
			`node_cpu_percent{node="cp02",role="control"} 16`,
			`node_cpu_percent{node="cp03",role="control"} 21`,
			`node_cpu_percent{node="db01",role="worker"} 88`,
			`node_cpu_percent{node="db02",role="worker"} 64`,
			`node_cpu_percent{node="ingress01",role="edge"} 23`,
			`node_cpu_percent{node="ingress02",role="edge"} 27`,
			`node_cpu_percent{node="web01",role="worker"} 72`,
			`node_cpu_percent{node="web02",role="worker"} 31`,
			`node_cpu_percent{node="web03",role="worker"} 5`,
			`node_memory_percent{node="batch01",role="worker"} 4`,
			`node_memory_percent{node="batch02",role="worker"} 55.5`,
			`node_memory_percent{node="cache01",role="worker"} 83.75`,
			`node_memory_percent{node="cp01",role="control"} 40`,
			`node_memory_percent{node="cp02",role="control"} 39`,
			`node_memory_percent{node="cp03",role="control"} 42.25`,
			`node_memory_percent{node="db01",role="worker"} 93.5`,
			`node_memory_percent{node="db02",role="worker"} 71`,
			`node_memory_percent{node="ingress01",role="edge"} 30`,
			`node_memory_percent{node="ingress02",role="edge"} 31.5`,
			`node_memory_percent{node="web01",role="worker"} 61.5`,
			`node_memory_percent{node="web02",role="worker"} 48`,
			`node_memory_percent{node="web03",role="worker"} 12.25`,
			`node_pods{node="batch01",role="worker"} 0`,
			`node_pods{node="batch02",role="worker"} 30`,
			`node_pods{node="cache01",role="worker"} 3`,
			`node_pods{node="cp01",role="control"} 11`,
			`node_pods{node="cp02",role="control"} 11`,
			`node_pods{node="cp03",role="control"} 11`,
			`node_pods{node="db01",role="worker"} 6`,
			`node_pods{node="db02",role="worker"} 6`,
			`node_pods{node="ingress01",role="edge"} 4`,
			`node_pods{node="ingress02",role="edge"} 4`,
			`node_pods{node="web01",role="worker"} 14`,
			`node_pods{node="web02",role="worker"} 9`,
			`node_pods{node="web03",role="worker"} 2`,
			`node_ready{node="batch01",note="kubelet stopped posting",role="worker"} 0`,
			`node_ready{node="batch02",role="worker"} 1`,
			`node_ready{node="cache01",role="worker"} 1`,
			`node_ready{node="cp01",role="control"} 1`,
			`node_ready{node="cp02",role="control"} 1`,
			`node_ready{node="cp03",note="etcd leader",role="control"} 1`,
			`node_ready{node="db01",note="disk pressure",role="worker"} 1`,
			`node_ready{node="db02",role="worker"} 1`,
			`node_ready{node="ingress01",role="edge"} 1`,
			`node_ready{node="ingress02",note="cordoned",role="edge"} 1`,
			`node_ready{node="web01",note="two words",role="worker"} 1`,
			`node_ready{node="web02",role="worker"} 1`,
			`node_ready{node="web03",note="new node",role="worker"} 1`,
		}},
		{name: "a delimiter ending every line", fixture: "hosts-trailing-delimiter.csv", contentType: "text/csv", rules: []model.MetricRule{
			{Name: "host_cpu_percent", Expression: "cpu_percent", Labels: columns("host", "host")},
			{Name: "host_memory_percent", Expression: "memory_percent", Labels: columns("host", "host")},
			{Name: "host_disk_percent", Expression: "disk_percent", Labels: columns("host", "host")},
		}, series: []string{
			`host_cpu_percent{host="batch01"} 0`,
			`host_cpu_percent{host="batch02"} 97`,
			`host_cpu_percent{host="cache01"} 12`,
			`host_cpu_percent{host="db01"} 88`,
			`host_cpu_percent{host="db02"} 64`,
			`host_cpu_percent{host="ingress01"} 23`,
			`host_cpu_percent{host="ingress02"} 27`,
			`host_cpu_percent{host="web01"} 72`,
			`host_cpu_percent{host="web02"} 31`,
			`host_cpu_percent{host="web03"} 5`,
			`host_disk_percent{host="batch01"} 22`,
			`host_disk_percent{host="batch02"} 60`,
			`host_disk_percent{host="cache01"} 5`,
			`host_disk_percent{host="db01"} 79`,
			`host_disk_percent{host="db02"} 81`,
			`host_disk_percent{host="ingress01"} 14`,
			`host_disk_percent{host="ingress02"} 14`,
			`host_disk_percent{host="web01"} 40`,
			`host_disk_percent{host="web02"} 38`,
			`host_disk_percent{host="web03"} 11`,
			`host_memory_percent{host="batch01"} 4`,
			`host_memory_percent{host="batch02"} 55`,
			`host_memory_percent{host="cache01"} 84`,
			`host_memory_percent{host="db01"} 93`,
			`host_memory_percent{host="db02"} 71`,
			`host_memory_percent{host="ingress01"} 30`,
			`host_memory_percent{host="ingress02"} 31`,
			`host_memory_percent{host="web01"} 61`,
			`host_memory_percent{host="web02"} 48`,
			`host_memory_percent{host="web03"} 12`,
		}},
		{name: "rows shorter than the header", fixture: "jobs-short-rows.csv", contentType: "text/csv", rules: jobs, series: []string{
			`job_duration_seconds{job="backup-db",state="ok"} 412.5`,
			`job_duration_seconds{job="backup-files",state="ok"} 1290`,
			`job_duration_seconds{job="export-billing",state="ok"} 96.75`,
			`job_duration_seconds{job="prune-cache",state="ok"} 0.5`,
			`job_duration_seconds{job="rotate-logs",state="ok"} 3.25`,
			`job_duration_seconds{job="sync-ldap",state="ok"} 12`,
			`job_duration_seconds{job="vacuum",state="ok"} 48`,
			`job_records{job="backup-db"} 18250`,
			`job_records{job="export-billing"} 5120`,
			`job_records{job="prune-cache"} 221`,
			`job_records{job="rotate-logs"} 14`,
			`job_records{job="sync-ldap"} 830`,
			`job_records{job="vacuum"} 0`,
		}, failures: []string{
			`job_duration_seconds: 3 failed, 3 missing, logged: CSV column "duration_seconds" is missing`,
			`job_records: 4 failed, 4 missing, logged: CSV column "records" is missing`,
		}},
		{name: "blank lines and a line of blanks", fixture: "jobs-blank-lines.csv", contentType: "text/csv", rules: jobs, series: wantJobs, failures: []string{
			`job_duration_seconds: 1 failed, 1 missing, logged: CSV column "duration_seconds" is missing`,
			`job_records: 1 failed, 1 missing, logged: CSV column "records" is missing`,
		}},
		{name: "no line end after the last row", fixture: "jobs-no-final-newline.csv", contentType: "text/csv", rules: jobs, series: wantJobs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := csvFixtureCollector(t, tc.response, tc.rules...)
			transformCSVFixture(t, c, tc.contentType, tc.fixture).holds(t, tc.series, tc.failures...)
		})
	}
}

// A header that cannot name its columns leaves reading them by number,
// without a header row, as the decode's error says. The header's line is
// then a row like the others, and its names are text where the rules read
// numbers: a failure of each rule on every scrape. A pre-script that drops
// the first row leaves the rows of values alone.
func TestCSVFixtureColumnsWithoutNamesAreReadByNumber(t *testing.T) {
	header := false
	noHeader := model.ResponseConfig{CSV: model.CSVConfig{Header: &header}}
	rules := []model.MetricRule{
		{Name: "host_memory_used_megabytes", Expression: "2", Labels: columns("host", "1")},
		{Name: "host_disk_used_percent", Expression: "4", Labels: columns("host", "1")},
	}
	want := []string{
		`host_disk_used_percent{host="batch01"} 4`,
		`host_disk_used_percent{host="batch02"} 55`,
		`host_disk_used_percent{host="cache01"} 84`,
		`host_disk_used_percent{host="db01"} 93`,
		`host_disk_used_percent{host="db02"} 71`,
		`host_disk_used_percent{host="ingress01"} 30`,
		`host_disk_used_percent{host="ingress02"} 31`,
		`host_disk_used_percent{host="web01"} 61`,
		`host_disk_used_percent{host="web02"} 48`,
		`host_disk_used_percent{host="web03"} 12`,
		`host_memory_used_megabytes{host="batch01"} 40`,
		`host_memory_used_megabytes{host="batch02"} 560`,
		`host_memory_used_megabytes{host="cache01"} 860`,
		`host_memory_used_megabytes{host="db01"} 1900`,
		`host_memory_used_megabytes{host="db02"} 1750`,
		`host_memory_used_megabytes{host="ingress01"} 300`,
		`host_memory_used_megabytes{host="ingress02"} 310`,
		`host_memory_used_megabytes{host="web01"} 412`,
		`host_memory_used_megabytes{host="web02"} 380`,
		`host_memory_used_megabytes{host="web03"} 120`,
	}
	c := csvFixtureCollector(t, noHeader, rules...)
	transformCSVFixture(t, c, "text/csv", "usage-duplicate-columns.csv").holds(t, want,
		`host_memory_used_megabytes: 1 failed, 0 missing, logged: value "used" is not a number; map text to numbers with value_map`,
		`host_disk_used_percent: 1 failed, 0 missing, logged: value "used" is not a number; map text to numbers with value_map`,
	)

	requirePython(t)
	c.Transform.PreScript = "data = data[1:]\n"
	transformCSVFixture(t, c, "text/csv", "usage-duplicate-columns.csv").holds(t, want)
}

// A number written with a decimal comma, as a spreadsheet with a German or
// Bulgarian locale exports it, is not read as a number: "62,5" is text.
//
// value_map reads the texts it lists and leaves the other cells to be read as
// numbers, and scale then multiplies both alike: a mapped 62.5 and a read 42
// are each a hundredth of what the file says. A text the map does not list
// still fails the rule, in words that name value_map.
//
// A pre-script reads them all: it rewrites the columns as numbers are
// written, the dot of the thousands taken out and the comma made a dot, and
// the rules read what it leaves.
func TestCSVFixtureDecimalCommasNeedAValueMapOrAPreScript(t *testing.T) {
	semicolons := model.ResponseConfig{CSV: model.CSVConfig{Delimiter: ";"}}
	item, hundredth := columns("item", "Artikel"), 0.01
	mapped := csvFixtureCollector(t, semicolons,
		model.MetricRule{Name: "stock_shelf_used_ratio", Expression: "Auslastung", Scale: &hundredth, Labels: item,
			ValueMap: map[string]float64{"62,5": 62.5, "45,625": 45.625, "27,5": 27.5, "4,8": 4.8}},
		model.MetricRule{Name: "stock_price_euros", Expression: "Preis", Labels: item, ValueMap: map[string]float64{"0,04": 0.04}},
	)
	transformCSVFixture(t, mapped, "text/csv", "inventory-semicolon.csv").holds(t, []string{
		`stock_shelf_used_ratio{item="Dübel 8x40"} 0.48`,
		`stock_shelf_used_ratio{item="Gewindestange M8"} 0.16`,
		`stock_shelf_used_ratio{item="Kabelbinder 200 mm"} 0.65`,
		`stock_shelf_used_ratio{item="Mutter M4"} 0.75`,
		`stock_shelf_used_ratio{item="Mutter M6"} 0.45625`,
		`stock_shelf_used_ratio{item="Schlossschraube M8x60"} 0.275`,
		`stock_shelf_used_ratio{item="Schraube M4x20"} 0.625`,
		`stock_shelf_used_ratio{item="Schraube M6x40"} 0.42`,
		`stock_shelf_used_ratio{item="Stahlträger IPE 100"} 0.048`,
		`stock_shelf_used_ratio{item="Unterlegscheibe 4,3"} 0.88`,
		`stock_shelf_used_ratio{item="Unterlegscheibe 6,4"} 0`,
		`stock_shelf_used_ratio{item="Winkelverbinder 90"} 0.31`,
		`stock_price_euros{item="Schraube M4x20"} 0.04`,
	}, `stock_price_euros: 11 failed, 0 missing, logged: value "0,09" is neither in value_map nor a number; add it to value_map, or map "*" for any other value`)

	requirePython(t)
	rewritten := csvFixtureCollector(t, semicolons,
		model.MetricRule{Name: "stock_price_euros", Expression: "Preis", Labels: item},
		model.MetricRule{Name: "stock_weight_kilograms", Expression: "Gewicht_kg", Labels: item},
		model.MetricRule{Name: "stock_shelf_used_ratio", Expression: "Auslastung", Scale: &hundredth, Labels: item},
	)
	rewritten.Transform.PreScript = `for row in data:
    for column in ("Preis", "Gewicht_kg", "Auslastung"):
        row[column] = row[column].replace(".", "").replace(",", ".")
`
	transformCSVFixture(t, rewritten, "text/csv", "inventory-semicolon.csv").holds(t, []string{
		`stock_price_euros{item="Dübel 8x40"} 0.06`,
		`stock_price_euros{item="Gewindestange M8"} 3.75`,
		`stock_price_euros{item="Kabelbinder 200 mm"} 0.015`,
		`stock_price_euros{item="Mutter M4"} 0.02`,
		`stock_price_euros{item="Mutter M6"} 0.03`,
		`stock_price_euros{item="Schlossschraube M8x60"} 0.35`,
		`stock_price_euros{item="Schraube M4x20"} 0.04`,
		`stock_price_euros{item="Schraube M6x40"} 0.09`,
		`stock_price_euros{item="Stahlträger IPE 100"} 1234.56`,
		`stock_price_euros{item="Unterlegscheibe 4,3"} 0.01`,
		`stock_price_euros{item="Unterlegscheibe 6,4"} 0.01`,
		`stock_price_euros{item="Winkelverbinder 90"} 1.5`,
		`stock_shelf_used_ratio{item="Dübel 8x40"} 0.48`,
		`stock_shelf_used_ratio{item="Gewindestange M8"} 0.16`,
		`stock_shelf_used_ratio{item="Kabelbinder 200 mm"} 0.65`,
		`stock_shelf_used_ratio{item="Mutter M4"} 0.75`,
		`stock_shelf_used_ratio{item="Mutter M6"} 0.45625`,
		`stock_shelf_used_ratio{item="Schlossschraube M8x60"} 0.275`,
		`stock_shelf_used_ratio{item="Schraube M4x20"} 0.625`,
		`stock_shelf_used_ratio{item="Schraube M6x40"} 0.42`,
		`stock_shelf_used_ratio{item="Stahlträger IPE 100"} 0.048`,
		`stock_shelf_used_ratio{item="Unterlegscheibe 4,3"} 0.88`,
		`stock_shelf_used_ratio{item="Unterlegscheibe 6,4"} 0`,
		`stock_shelf_used_ratio{item="Winkelverbinder 90"} 0.31`,
		`stock_weight_kilograms{item="Dübel 8x40"} 0.004`,
		`stock_weight_kilograms{item="Gewindestange M8"} 0.395`,
		`stock_weight_kilograms{item="Kabelbinder 200 mm"} 0.0012`,
		`stock_weight_kilograms{item="Mutter M4"} 0.001`,
		`stock_weight_kilograms{item="Mutter M6"} 0.002`,
		`stock_weight_kilograms{item="Schlossschraube M8x60"} 0.031`,
		`stock_weight_kilograms{item="Schraube M4x20"} 0.003`,
		`stock_weight_kilograms{item="Schraube M6x40"} 0.011`,
		`stock_weight_kilograms{item="Stahlträger IPE 100"} 48.6`,
		`stock_weight_kilograms{item="Unterlegscheibe 4,3"} 0.0005`,
		`stock_weight_kilograms{item="Unterlegscheibe 6,4"} 0.0009`,
		`stock_weight_kilograms{item="Winkelverbinder 90"} 0.12`,
	})
}

// numberForms is what a rule without a value_map makes of each row of
// testdata/csv/numbers.csv, by the row's form: the value it reads, "missing"
// for a cell that is empty or blanks, or the failure of a cell that holds
// text which is no number.
var numberForms = map[string]string{
	"integer":             "42",
	"zero":                "0",
	"negative":            "-17",
	"decimal":             "3.14159",
	"leading zeros":       "7",
	"no integer part":     "0.5",
	"no fraction":         "5",
	"exponent":            "1500",
	"exponent upper case": "0.002",
	"exponent with plus":  "6.02e+23",
	"leading plus":        "7",
	"blanks around":       "42",
	"quoted":              "42",
	"quoted with blanks":  "42",
	// A float64 holds 15 to 17 digits: a longer integer is the nearest one
	// it has.
	"long integer":   "1.2345678901234568e+29",
	"smallest step":  "9.007199254740992e+15",
	"NaN":            "NaN",
	"nan lower case": "NaN",
	"Inf":            "+Inf",
	"+Inf":           "+Inf",
	"-Inf":           "-Inf",
	"Infinity":       "+Inf",
	"inf lower case": "-Inf",
	"below a float":  "0",

	"empty":        "missing",
	"blanks":       "missing",
	"quoted empty": "missing",

	"hex integer":          `value "0x1F" is not a number; map text to numbers with value_map`,
	"octal":                `value "0o17" is not a number; map text to numbers with value_map`,
	"binary":               `value "0b101" is not a number; map text to numbers with value_map`,
	"thousands comma":      `value "1,234" is not a number; map text to numbers with value_map`,
	"thousands blank":      `value "1 234" is not a number; map text to numbers with value_map`,
	"thousands apostrophe": `value "1'234" is not a number; map text to numbers with value_map`,
	"decimal comma":        `value "1,5" is not a number; map text to numbers with value_map`,
	"percent":              `value "85%" is not a number; map text to numbers with value_map`,
	"currency before":      `value "$12.50" is not a number; map text to numbers with value_map`,
	"currency after":       `value "12.50 EUR" is not a number; map text to numbers with value_map`,
	"unit":                 `value "512MB" is not a number; map text to numbers with value_map`,
	"duration":             `value "1m30s" is not a number; map text to numbers with value_map`,
	"beyond a float":       `value "1e400" is beyond the range of a 64-bit float`,
	"unicode minus":        `value "−5" is not a number; map text to numbers with value_map`,
	"fullwidth digits":     `value "４２" is not a number; map text to numbers with value_map`,
	"version":              `value "1.0.0" is not a number; map text to numbers with value_map`,
	"two numbers":          `value "1 2" is not a number; map text to numbers with value_map`,
	"dash":                 `value "-" is not a number; map text to numbers with value_map`,
	"N/A":                  `value "N/A" is not a number; map text to numbers with value_map`,
	"null":                 `value "null" is not a number; map text to numbers with value_map`,
	"none":                 `value "None" is not a number; map text to numbers with value_map`,
	"true":                 `value "true" is not a number; map text to numbers with value_map`,
	"false":                `value "false" is not a number; map text to numbers with value_map`,
	"TRUE upper case":      `value "TRUE" is not a number; map text to numbers with value_map`,
	"yes":                  `value "yes" is not a number; map text to numbers with value_map`,
	"no":                   `value "no" is not a number; map text to numbers with value_map`,
	"on":                   `value "on" is not a number; map text to numbers with value_map`,
	"off":                  `value "off" is not a number; map text to numbers with value_map`,
}

// numberRows is the rows of testdata/csv/numbers.csv, by their form.
func numberRows(t *testing.T, trim bool) (forms []string, rows map[string]any) {
	t.Helper()
	c := model.Collector{Decoder: model.DecoderConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: model.CSVConfig{TrimSpace: trim}}}
	d, err := decode.Decode(&fetch.HTTPResponse{Body: readCSVFixture(t, "numbers.csv"), Headers: http.Header{}}, &c)
	if err != nil {
		t.Fatal(err)
	}
	rows = map[string]any{}
	for _, row := range d.Data.([]any) {
		form := row.(map[string]any)["form"].(string)
		if _, twice := rows[form]; twice {
			t.Fatalf("numbers.csv has two rows of the form %q", form)
		}
		forms = append(forms, form)
		rows[form] = row
	}
	return forms, rows
}

// numberOutcome is what a rule made of one row: its value, "missing" for a
// missing value that failed it, "left out" for a row it made nothing of
// without failing, or its failure's words.
func numberOutcome(t *testing.T, rule model.MetricRule, row any) string {
	t.Helper()
	c := csvFixtureCollector(t, model.ResponseConfig{}, rule)
	c.Metrics[0].ErrorMode = model.ErrorModeIgnore
	ctx, report := WithRuleReport(t.Context())
	set, err := Transform(ctx, &decode.Decoded{Kind: "csv", Data: []any{row}}, &fetch.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{}}, &c, "python3")
	failures := report.Failures()
	series := 0
	if set != nil {
		series = len(set.Metrics)
	}
	switch {
	case err != nil:
		return "the transform failed: " + err.Error()
	case series == 1 && len(failures) == 0:
		return strconv.FormatFloat(set.Metrics[0].Value, 'g', -1, 64)
	case series == 0 && len(failures) == 0:
		return "left out"
	case series == 0 && len(failures) == 1 && failures[0].Failures == 1 && failures[0].Missing == 1:
		return "missing"
	case series == 0 && len(failures) == 1 && failures[0].Failures == 1:
		return failures[0].First.Error()
	}
	return fmt.Sprintf("%d series and the failures %v", series, failures)
}

// Each way an export writes a number, a row of numbers.csv, read by a rule
// without a value_map, with and without trim_space, which makes no
// difference to a value:
//
//   - integers, decimals with or without digits before or after the dot,
//     exponents, a leading plus, blanks around the number and quotes around
//     the field are read, and so are NaN and the infinities, in any case,
//     which are exported as they are;
//   - an integer longer than a float64 holds is the nearest it has, and a
//     number too small for one is 0;
//   - an empty cell, one of blanks and an empty quoted one are missing
//     values;
//   - everything else is text that is no number and fails the rule, named
//     in its error: separators of thousands, a decimal comma, a percent
//     sign, a currency, a unit, hexadecimal, octal and binary integers,
//     digits and a minus sign that are not ASCII, the words exports write
//     for no value, and true and false, yes and no, on and off. A number
//     beyond a float64's range says so.
func TestCSVFixtureNumbersAreReadAsWrittenOrFailTheirRule(t *testing.T) {
	for _, trim := range []bool{false, true} {
		forms, rows := numberRows(t, trim)
		if len(forms) != len(numberForms) {
			t.Errorf("numbers.csv has %d rows, and %d forms are expected of it", len(forms), len(numberForms))
		}
		for _, form := range forms {
			want, known := numberForms[form]
			if !known {
				t.Errorf("numbers.csv has a row %q the test knows nothing of", form)
				continue
			}
			if got := numberOutcome(t, model.MetricRule{Name: "plain", Expression: "value"}, rows[form]); got != want {
				t.Errorf("%s (trim_space %v): got %s, want %s", form, trim, got, want)
			}
		}
	}
}

// The same rows under the settings that decide what a cell that is no number
// costs:
//
//   - required: false leaves the missing values out without a failure, and
//     excuses no text that is not a number;
//   - value_map reads the texts it lists, case-sensitively, as the numbers
//     it gives them, and scale multiplies those and the numbers read alike;
//     a number the map lists is the mapped one;
//   - a "*" in the map is the value of every cell that holds anything,
//     numbers included, and of no empty one;
//   - under log the rule's failures are one log line with their number and
//     the first of them, under ignore none, and under fail the first of
//     them fails the transform, naming the rule and the value.
func TestCSVFixtureNumbersUnderRequiredValueMapAndErrorMode(t *testing.T) {
	forms, rows := numberRows(t, false)
	optional := false
	failures, missing := 0, 0
	for _, form := range forms {
		want := numberForms[form]
		_, err := strconv.ParseFloat(want, 64)
		switch {
		case want == "missing":
			missing++
			want = "left out"
		case err != nil:
			failures++
		}
		rule := model.MetricRule{Name: "optional", Expression: "value", Required: &optional}
		if got := numberOutcome(t, rule, rows[form]); got != want {
			t.Errorf("%s, not required: got %s, want %s", form, got, want)
		}
	}

	hundredth := 0.01
	mapped := model.MetricRule{Name: "mapped", Expression: "value", Scale: &hundredth, ValueMap: map[string]float64{
		"N/A": -100, "-": -100, "null": -100, "None": -100,
		"true": 100, "false": 0, "yes": 100, "no": 0, "on": 100, "off": 0,
		"85%": 85, "1,5": 1.5, "1,234": 1234, "42": 4200,
	}}
	for form, want := range map[string]string{
		// Mapped, then scaled.
		"N/A": "-1", "dash": "-1", "null": "-1", "none": "-1", "true": "1", "false": "0", "yes": "1", "no": "0", "on": "1", "off": "0",
		"percent": "0.85", "decimal comma": "0.015", "thousands comma": "12.34",
		// A number the map lists, with or without blanks around it.
		"integer": "42", "blanks around": "42", "quoted with blanks": "42",
		// Numbers it does not list are read, and scaled.
		"negative": "-0.17", "exponent": "15", "leading zeros": "0.07", "Inf": "+Inf",
		// The map's keys are matched as they are written.
		"TRUE upper case": `value "TRUE" is neither in value_map nor a number; add it to value_map, or map "*" for any other value`,
		"currency before": `value "$12.50" is neither in value_map nor a number; add it to value_map, or map "*" for any other value`,
		"empty":           "missing",
	} {
		if got := numberOutcome(t, mapped, rows[form]); got != want {
			t.Errorf("%s, mapped and scaled: got %s, want %s", form, got, want)
		}
	}
	star := model.MetricRule{Name: "star", Expression: "value", ValueMap: map[string]float64{"N/A": -1, "*": 7}}
	for _, form := range forms {
		want := "7"
		switch {
		case form == "N/A":
			want = "-1"
		case numberForms[form] == "missing":
			want = "missing"
		}
		if got := numberOutcome(t, star, rows[form]); got != want {
			t.Errorf("%s, with a value for any other text: got %s, want %s", form, got, want)
		}
	}

	const first = `value "0x1F" is not a number; map text to numbers with value_map`
	for mode, how := range map[string]string{model.ErrorModeLog: "logged", model.ErrorModeIgnore: "not logged"} {
		c := csvFixtureCollector(t, model.ResponseConfig{}, model.MetricRule{Name: "plain", Expression: "value", ErrorMode: mode})
		result := transformCSVFixture(t, c, "text/csv", "numbers.csv")
		if len(result.series) != len(forms)-failures-missing {
			t.Errorf("under %s the rule made %d series of %d rows, %d of them no numbers and %d empty", mode, len(result.series), len(forms), failures, missing)
		}
		result.holds(t, result.series, fmt.Sprintf("plain: %d failed, %d missing, %s: %s", failures+missing, missing, how, first))
	}
	strict := csvFixtureCollector(t, model.ResponseConfig{}, model.MetricRule{Name: "strict", Expression: "value", ErrorMode: model.ErrorModeFail})
	result := transformCSVFixture(t, strict, "text/csv", "numbers.csv")
	if result.err == nil || result.err.Error() != `metric "strict": `+first || len(result.series) != 0 {
		t.Errorf("under fail: %v, with %d series; want the transform to fail on the first cell that is no number", result.err, len(result.series))
	}
	var failure *MetricFailure
	if !errors.As(result.err, &failure) || failure.Metric != "strict" {
		t.Errorf("under fail the error is %#v, want the failure of the metric strict", result.err)
	}
}

// Times in the columns of a backup report, each written another way and read
// by its rule's time_format, are the Unix seconds of the moments they name:
//
//   - a local time without a zone is read in time_zone, Sofia's summer time
//     and, on the night the clocks go back, the second 03:30;
//   - RFC 3339, with Z or an offset, and a fraction of a second kept;
//   - a German date and a 24-hour clock in Berlin's time, an American one
//     with a 12-hour clock in New York's;
//   - the compact ISO 8601 form, the Z read as the zone;
//   - the date of an HTTP header, with GMT or a numeric zone;
//   - a date alone, the start of its day in UTC;
//   - Unix seconds as they are, and milliseconds scaled to seconds; a time
//     scaled to milliseconds.
//
// An empty cell is a missing value, and a text that is no time in the format
// fails the rule, naming the text and the format.
func TestCSVFixtureTimesAreReadByTheirColumnsFormats(t *testing.T) {
	job, thousandth, thousand := columns("job", "job"), 0.001, 1000.0
	c := csvFixtureCollector(t, model.ResponseConfig{},
		model.MetricRule{Name: "backup_started_timestamp_seconds", Expression: "started_local", TimeFormat: "2006-01-02 15:04:05", TimeZone: "Europe/Sofia", Labels: job},
		model.MetricRule{Name: "backup_finished_timestamp_seconds", Expression: "finished", TimeFormat: "rfc3339", Labels: job},
		model.MetricRule{Name: "backup_verified_timestamp_seconds", Expression: "verified_de", TimeFormat: "02.01.2006 15:04", TimeZone: "Europe/Berlin", Labels: job},
		model.MetricRule{Name: "backup_next_run_timestamp_seconds", Expression: "next_run_us", TimeFormat: "Jan 2, 2006 3:04 PM", TimeZone: "America/New_York", Labels: job},
		model.MetricRule{Name: "backup_snapshot_timestamp_seconds", Expression: "snapshot", TimeFormat: "20060102T150405Z07:00", Labels: job},
		model.MetricRule{Name: "backup_uploaded_timestamp_seconds", Expression: "uploaded", TimeFormat: "rfc1123", Labels: job},
		model.MetricRule{Name: "backup_expires_timestamp_seconds", Expression: "expires", TimeFormat: "2006-01-02", Labels: job},
		model.MetricRule{Name: "backup_started_unix_seconds", Expression: "started_unix", Labels: job},
		model.MetricRule{Name: "backup_finished_unix_seconds", Expression: "finished_ms", Scale: &thousandth, Labels: job},
		model.MetricRule{Name: "backup_finished_milliseconds", Expression: "finished", TimeFormat: "rfc3339", Scale: &thousand, Labels: job},
	)
	const layout = "; write the layout as the text writes the reference time, Mon Jan 2 15:04:05 MST 2006"
	transformCSVFixture(t, c, "text/csv", "backups-times.csv").holds(t, []string{
		`backup_expires_timestamp_seconds{job="archive"} 1.893456e+09`,
		`backup_expires_timestamp_seconds{job="ci-cache"} 1.791072e+09`,
		`backup_expires_timestamp_seconds{job="db-main"} 1.7935776e+09`,
		`backup_expires_timestamp_seconds{job="db-replica"} 1.7935776e+09`,
		`backup_expires_timestamp_seconds{job="dns-zones"} 1.7954784e+09`,
		`backup_expires_timestamp_seconds{job="files-home"} 1.7915904e+09`,
		`backup_expires_timestamp_seconds{job="files-shared"} 1.7915904e+09`,
		`backup_expires_timestamp_seconds{job="git"} 1.7986752e+09`,
		`backup_expires_timestamp_seconds{job="ldap"} 1.7987616e+09`,
		`backup_expires_timestamp_seconds{job="mail"} 1.7921952e+09`,
		`backup_expires_timestamp_seconds{job="vault"} 1.7934048e+09`,
		`backup_expires_timestamp_seconds{job="wiki"} 1.791504e+09`,
		`backup_finished_milliseconds{job="archive"} 1.791e+12`,
		`backup_finished_milliseconds{job="db-main"} 1.79100796e+12`,
		`backup_finished_milliseconds{job="db-replica"} 1.7910096755e+12`,
		`backup_finished_milliseconds{job="dns-zones"} 1.79289186e+12`,
		`backup_finished_milliseconds{job="files-home"} 1.790981223e+12`,
		`backup_finished_milliseconds{job="files-shared"} 1.790985959e+12`,
		`backup_finished_milliseconds{job="git"} 1.79098935e+12`,
		`backup_finished_milliseconds{job="ldap"} 1.790975709e+12`,
		`backup_finished_milliseconds{job="mail"} 1.79098668e+12`,
		`backup_finished_milliseconds{job="vault"} 1.790996445e+12`,
		`backup_finished_milliseconds{job="wiki"} 1.790973072e+12`,
		`backup_finished_timestamp_seconds{job="archive"} 1.791e+09`,
		`backup_finished_timestamp_seconds{job="db-main"} 1.79100796e+09`,
		`backup_finished_timestamp_seconds{job="db-replica"} 1.7910096755e+09`,
		`backup_finished_timestamp_seconds{job="dns-zones"} 1.79289186e+09`,
		`backup_finished_timestamp_seconds{job="files-home"} 1.790981223e+09`,
		`backup_finished_timestamp_seconds{job="files-shared"} 1.790985959e+09`,
		`backup_finished_timestamp_seconds{job="git"} 1.79098935e+09`,
		`backup_finished_timestamp_seconds{job="ldap"} 1.790975709e+09`,
		`backup_finished_timestamp_seconds{job="mail"} 1.79098668e+09`,
		`backup_finished_timestamp_seconds{job="vault"} 1.790996445e+09`,
		`backup_finished_timestamp_seconds{job="wiki"} 1.790973072e+09`,
		`backup_finished_unix_seconds{job="archive"} 1.791e+09`,
		`backup_finished_unix_seconds{job="db-main"} 1.79100796e+09`,
		`backup_finished_unix_seconds{job="db-replica"} 1.7910096755e+09`,
		`backup_finished_unix_seconds{job="dns-zones"} 1.79289186e+09`,
		`backup_finished_unix_seconds{job="files-home"} 1.790981223e+09`,
		`backup_finished_unix_seconds{job="files-shared"} 1.790985959e+09`,
		`backup_finished_unix_seconds{job="git"} 1.79098935e+09`,
		`backup_finished_unix_seconds{job="ldap"} 1.790975709e+09`,
		`backup_finished_unix_seconds{job="mail"} 1.79098668e+09`,
		`backup_finished_unix_seconds{job="vault"} 1.790996445e+09`,
		`backup_finished_unix_seconds{job="wiki"} 1.790973072e+09`,
		`backup_next_run_timestamp_seconds{job="archive"} 1.7911116e+09`,
		`backup_next_run_timestamp_seconds{job="ci-cache"} 1.7911044e+09`,
		`backup_next_run_timestamp_seconds{job="db-main"} 1.7911188e+09`,
		`backup_next_run_timestamp_seconds{job="db-replica"} 1.7911206e+09`,
		`backup_next_run_timestamp_seconds{job="dns-zones"} 1.7929998e+09`,
		`backup_next_run_timestamp_seconds{job="files-home"} 1.79109e+09`,
		`backup_next_run_timestamp_seconds{job="files-shared"} 1.7910936e+09`,
		`backup_next_run_timestamp_seconds{job="git"} 1.7911008e+09`,
		`backup_next_run_timestamp_seconds{job="ldap"} 1.7910873e+09`,
		`backup_next_run_timestamp_seconds{job="mail"} 1.7910972e+09`,
		`backup_next_run_timestamp_seconds{job="vault"} 1.791108e+09`,
		`backup_next_run_timestamp_seconds{job="wiki"} 1.7910846e+09`,
		`backup_snapshot_timestamp_seconds{job="archive"} 1.791e+09`,
		`backup_snapshot_timestamp_seconds{job="ci-cache"} 1.7909928e+09`,
		`backup_snapshot_timestamp_seconds{job="db-main"} 1.791007207e+09`,
		`backup_snapshot_timestamp_seconds{job="db-replica"} 1.791009e+09`,
		`backup_snapshot_timestamp_seconds{job="dns-zones"} 1.7928918e+09`,
		`backup_snapshot_timestamp_seconds{job="files-home"} 1.7909784e+09`,
		`backup_snapshot_timestamp_seconds{job="files-shared"} 1.790982e+09`,
		`backup_snapshot_timestamp_seconds{job="git"} 1.7909892e+09`,
		`backup_snapshot_timestamp_seconds{job="ldap"} 1.7909757e+09`,
		`backup_snapshot_timestamp_seconds{job="mail"} 1.7909856e+09`,
		`backup_snapshot_timestamp_seconds{job="vault"} 1.7909964e+09`,
		`backup_snapshot_timestamp_seconds{job="wiki"} 1.790973e+09`,
		`backup_started_timestamp_seconds{job="ci-cache"} 1.7909928e+09`,
		`backup_started_timestamp_seconds{job="db-main"} 1.791007207e+09`,
		`backup_started_timestamp_seconds{job="db-replica"} 1.791009e+09`,
		`backup_started_timestamp_seconds{job="dns-zones"} 1.7928918e+09`,
		`backup_started_timestamp_seconds{job="files-home"} 1.7909784e+09`,
		`backup_started_timestamp_seconds{job="files-shared"} 1.790982e+09`,
		`backup_started_timestamp_seconds{job="git"} 1.7909892e+09`,
		`backup_started_timestamp_seconds{job="ldap"} 1.7909757e+09`,
		`backup_started_timestamp_seconds{job="mail"} 1.7909856e+09`,
		`backup_started_timestamp_seconds{job="vault"} 1.7909964e+09`,
		`backup_started_timestamp_seconds{job="wiki"} 1.790973e+09`,
		`backup_started_unix_seconds{job="archive"} 1.791e+09`,
		`backup_started_unix_seconds{job="ci-cache"} 1.7909928e+09`,
		`backup_started_unix_seconds{job="db-main"} 1.791007207e+09`,
		`backup_started_unix_seconds{job="db-replica"} 1.791009e+09`,
		`backup_started_unix_seconds{job="dns-zones"} 1.7928918e+09`,
		`backup_started_unix_seconds{job="files-home"} 1.7909784e+09`,
		`backup_started_unix_seconds{job="files-shared"} 1.790982e+09`,
		`backup_started_unix_seconds{job="git"} 1.7909892e+09`,
		`backup_started_unix_seconds{job="ldap"} 1.7909757e+09`,
		`backup_started_unix_seconds{job="mail"} 1.7909856e+09`,
		`backup_started_unix_seconds{job="vault"} 1.7909964e+09`,
		`backup_started_unix_seconds{job="wiki"} 1.790973e+09`,
		`backup_uploaded_timestamp_seconds{job="archive"} 1.7910006e+09`,
		`backup_uploaded_timestamp_seconds{job="db-main"} 1.7910084e+09`,
		`backup_uploaded_timestamp_seconds{job="db-replica"} 1.7910102e+09`,
		`backup_uploaded_timestamp_seconds{job="dns-zones"} 1.7928924e+09`,
		`backup_uploaded_timestamp_seconds{job="files-home"} 1.7909826e+09`,
		`backup_uploaded_timestamp_seconds{job="files-shared"} 1.7909766e+09`,
		`backup_uploaded_timestamp_seconds{job="git"} 1.7909898e+09`,
		`backup_uploaded_timestamp_seconds{job="ldap"} 1.790976e+09`,
		`backup_uploaded_timestamp_seconds{job="mail"} 1.7909871e+09`,
		`backup_uploaded_timestamp_seconds{job="vault"} 1.7909967e+09`,
		`backup_uploaded_timestamp_seconds{job="wiki"} 1.7909739e+09`,
		`backup_verified_timestamp_seconds{job="archive"} 1.7910036e+09`,
		`backup_verified_timestamp_seconds{job="db-main"} 1.7910117e+09`,
		`backup_verified_timestamp_seconds{job="db-replica"} 1.7910135e+09`,
		`backup_verified_timestamp_seconds{job="dns-zones"} 1.7928957e+09`,
		`backup_verified_timestamp_seconds{job="files-home"} 1.7909856e+09`,
		`backup_verified_timestamp_seconds{job="files-shared"} 1.7909898e+09`,
		`backup_verified_timestamp_seconds{job="git"} 1.7909931e+09`,
		`backup_verified_timestamp_seconds{job="ldap"} 1.7909796e+09`,
		`backup_verified_timestamp_seconds{job="mail"} 1.790991e+09`,
		`backup_verified_timestamp_seconds{job="vault"} 1.79100012e+09`,
		`backup_verified_timestamp_seconds{job="wiki"} 1.7909772e+09`,
	},
		`backup_started_timestamp_seconds: 1 failed, 0 missing, logged: value "in progress" is not a time in time_format "2006-01-02 15:04:05"`+layout,
		`backup_verified_timestamp_seconds: 1 failed, 0 missing, logged: value "never" is not a time in time_format "02.01.2006 15:04"`+layout,
		`backup_finished_timestamp_seconds: 1 failed, 1 missing, logged: CSV column "finished" is missing`,
		`backup_finished_milliseconds: 1 failed, 1 missing, logged: CSV column "finished" is missing`,
		`backup_uploaded_timestamp_seconds: 1 failed, 1 missing, logged: CSV column "uploaded" is missing`,
		`backup_finished_unix_seconds: 1 failed, 1 missing, logged: CSV column "finished_ms" is missing`,
	)
}
