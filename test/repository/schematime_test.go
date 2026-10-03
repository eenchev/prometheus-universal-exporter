//go:build !select_request_types || request_type_http

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// timedCollector is a configuration of one collector with the transform
// named, whose one rule holds the keys as written beside its expression.
func timedCollector(transform, keys string) string {
	expression := map[string]string{"jq": ".at", "regex": "'at: (.+)'", "csv": "at", "prometheus": "'^at$'"}[transform]
	rule := "    metrics:\n      - name: at\n        expression: " + expression + "\n" + keys
	if transform == "python" {
		return "collectors:\n  - name: timed\n    request: {type: http}\n    transform:\n      type: python\n      script: metric('at', 1)\n    metrics:\n      - name: at\n" + keys
	}
	return "collectors:\n  - name: timed\n    request: {type: http}\n    transform: {type: " + transform + "}\n" + rule
}

// A rule's time_format and time_zone are keys to the schema and to the
// exporter alike: both take a name or a layout, written as text or, for a
// layout of digits alone, as YAML's number, with time_zone and scale beside
// it; both refuse time_zone without time_format, time_format beside
// value_map, and a value that is no text. What a schema cannot tell — a
// layout that is none, one without a date, one with a 12-hour clock's hour
// and no PM or with blanks around it, a sample date, a zone nobody knows, a
// transform that sets its values itself — is the exporter's to refuse, in
// its words.
//
// A layout that is a date is quoted, as the documentation writes it. Left
// unquoted the exporter reads it as the text it is, which the last check
// shows, while a reader of YAML 1.1, this test's among them, hands a
// validator a date.
func TestSchemaAndExporterAgreeOnTimeFormat(t *testing.T) {
	schema := loadSchema(t)
	for keys, accepted := range map[string]bool{
		"        time_format: rfc3339\n":                                                  true,
		"        time_format: RFC1123\n":                                                  true,
		"        time_format: \"2006-01-02\"\n":                                           true,
		"        time_format: '2006-01-02 15:04'\n        time_zone: Europe/Sofia\n":      true,
		"        time_format: 20060102\n":                                                 true,
		"        time_format: \"02.01.2006 15:04:05\"\n        scale: 1000\n":             true,
		"        time_format: '2006-01-02'\n        time_zone: UTC\n        scale: 0.5\n": true,
		"        time_zone: Europe/Sofia\n":                                               false,
		"        time_zone: UTC\n        value_map: {never: 0}\n":                         false,
		"        time_format: '2006-01-02'\n        value_map: {never: 0}\n":              false,
		"        time_format: '2006-01-02'\n        value_map: {}\n":                      false,
		"        time_format: '2006-01-02'\n        time_zone: 3\n":                       false,
		"        time_format: '2006-01-02'\n        time_zone: true\n":                    false,
		"        time_format: '2006-01-02'\n        time_zone: [UTC]\n":                   false,
		"        time_format: ['2006-01-02']\n":                                           false,
		"        time_format: {layout: '2006-01-02'}\n":                                   false,
		"        time_fromat: '2006-01-02'\n":                                             false,
	} {
		agree(t, schema, strings.TrimSpace(keys), timedCollector("jq", keys), accepted)
	}
	for _, alone := range []struct{ transform, keys, message string }{
		{"jq", "        time_format: yyyy-mm-dd\n", `collector "timed" metric "at" time_format "yyyy-mm-dd" is neither the name rfc3339 or rfc1123 nor a layout`},
		{"jq", "        time_format: '%Y-%m-%d'\n", `time_format "%Y-%m-%d" is neither the name rfc3339 or rfc1123 nor a layout`},
		{"jq", "        time_format: true\n", `time_format "true" is neither the name rfc3339 or rfc1123 nor a layout`},
		{"regex", "        time_format: '15:04'\n", `collector "timed" metric "at" time_format "15:04" has no year, month or day: a time without a date has no Unix time`},
		{"regex", "        time_format: 2006\n", `time_format "2006" has no month or day`},
		{"regex", "        time_format: '2006-01-02 03:04:05'\n", `collector "timed" metric "at" time_format "2006-01-02 03:04:05" writes the hour on a 12-hour clock (03 or 3) without PM`},
		{"regex", "        time_format: ' 2006-01-02 '\n", `collector "timed" metric "at" time_format " 2006-01-02 " begins or ends with a blank`},
		{"jq", "        time_format: '2026-10-03'\n", `time_format "2026-10-03" cannot read a time it writes, "3036-20-04", so keep its elements apart: a layout is the reference time, Mon Jan 2 15:04:05 MST 2006`},
		{"csv", "        time_format: '2006-01-02'\n        time_zone: Europe/Sofija\n", `collector "timed" metric "at" time_zone "Europe/Sofija" is not a time zone the exporter knows`},
		{"csv", "        time_format: '2006-01-02'\n        time_zone: Local\n", `time_zone "Local" is not a time zone the exporter knows`},
		{"python", "        time_format: '2006-01-02'\n", `collector "timed" metric "at" sets time_format, which the python transform does not use`},
		{"prometheus", "        time_format: rfc3339\n", `collector "timed" metric "at" sets time_format, which the prometheus transform does not use`},
	} {
		loadersAlone(t, schema, alone.transform+": "+strings.TrimSpace(alone.keys), timedCollector(alone.transform, alone.keys), alone.message)
	}
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", timedCollector("jq", "        time_format: 2006-01-02\n        time_zone: Europe/Sofia\n")))
	if err != nil || cfg.Collectors[0].Metrics[0].TimeFormat != "2006-01-02" || cfg.Collectors[0].Metrics[0].TimeZone != "Europe/Sofia" {
		t.Errorf("a layout written without quotes: %v, %+v", err, cfg)
	}
	for _, key := range []string{"time_format", "time_zone"} {
		rule := schema["properties"].(map[string]any)["collectors"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["metrics"].(map[string]any)["items"].(map[string]any)
		if description, _ := rule["properties"].(map[string]any)[key].(map[string]any)["description"].(string); description == "" {
			t.Errorf("%s has no description in the schema", key)
		}
	}
}
