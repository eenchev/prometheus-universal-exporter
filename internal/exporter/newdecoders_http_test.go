//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Each of the ndjson, logfmt, toml and ini decoders hands a jq transform a
// document its rules read as they read JSON, end to end through a probe:
// items over the records of NDJSON and logfmt, and paths into the tables of
// TOML and the sections of INI. A decoder named in the collector reads the
// body whatever its Content-Type; one left to the response is chosen by the
// Content-Type for NDJSON and TOML, which have one, and a debug probe names
// the decoder that read the body.
func TestTheNewDecodersFeedAJQTransform(t *testing.T) {
	testutil.CaptureLogs(t)
	for _, tc := range []struct {
		decoder, contentType, body string
		rules                      []model.MetricRule
		want                       []string
	}{
		{"ndjson", "text/plain", "{\"host\":\"web01\",\"used\":72}\n\n{\"host\":\"web02\",\"used\":13.5}\n",
			[]model.MetricRule{{Name: "disk_used", Items: ".[]", Expression: ".used", Labels: []model.LabelRule{{Name: "host", Expression: ".host"}}}},
			[]string{`disk_used{host="web01"} 72`, `disk_used{host="web02"} 13.5`}},
		{"auto", "application/x-ndjson", "{\"host\":\"web01\",\"used\":72}\n",
			[]model.MetricRule{{Name: "disk_used", Items: ".[]", Expression: ".used", Labels: []model.LabelRule{{Name: "host", Expression: ".host"}}}},
			[]string{`disk_used{host="web01"} 72`}},
		{"logfmt", "text/plain", "level=info path=/a duration_ms=12 cached\nlevel=warn path=/b duration_ms=7.5\n",
			[]model.MetricRule{
				{Name: "request_duration_ms", Items: ".[]", Expression: ".duration_ms", Labels: []model.LabelRule{{Name: "path", Expression: ".path"}}},
				{Name: "cached_requests", Expression: `[.[] | select(has("cached"))] | length`},
			},
			[]string{`request_duration_ms{path="/a"} 12`, `request_duration_ms{path="/b"} 7.5`, "cached_requests 1"}},
		{"toml", "text/plain", "[server]\nrequests = 1200\nload = 0.25\n\n[[disks]]\nname = \"sda\"\nused = 72\n",
			[]model.MetricRule{
				{Name: "server_requests", Expression: ".server.requests"},
				{Name: "disk_used", Items: ".disks[]", Expression: ".used", Labels: []model.LabelRule{{Name: "disk", Expression: ".name"}}},
			},
			[]string{"server_requests 1200", `disk_used{disk="sda"} 72`}},
		{"auto", "application/toml", "[server]\nrequests = 1200\n",
			[]model.MetricRule{{Name: "server_requests", Expression: ".server.requests"}},
			[]string{"server_requests 1200"}},
		{"ini", "text/plain", "uptime = 3600\n[pool]\nactive : 4\n",
			[]model.MetricRule{{Name: "uptime_seconds", Expression: ".uptime"}, {Name: "pool_active", Expression: ".pool.active"}},
			[]string{"uptime_seconds 3600", "pool_active 4"}},
	} {
		t.Run(tc.decoder+" "+tc.contentType, func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(target.Close)
			c := model.Collector{
				Name:          "c",
				Request:       model.RequestConfig{Type: fetch.RequestTypeHTTP, Method: "GET"},
				Decoder:       model.DecoderConfig{Type: tc.decoder},
				Transform:     model.TransformConfig{Type: "jq"},
				ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
				Limits:        model.Limits{MaxResponseBytes: 4096, MaxMetrics: 10},
				Metrics:       tc.rules,
			}
			for i := range c.Metrics {
				c.Metrics[i].Type = model.GaugeMetricType
			}
			server := modeServer(t, c)
			r := probe(t, server, url.QueryEscape(target.URL), "c")
			if r.Code != http.StatusOK {
				t.Fatalf("answered %d: %s", r.Code, r.Body)
			}
			assertContains(t, r.Body.String(), tc.want...)
			server.SetProbeDebug(true)
			kind := tc.decoder
			if kind == "auto" {
				kind = map[string]string{"application/x-ndjson": "ndjson", "application/toml": "toml"}[tc.contentType]
			}
			report := debugProbeGet(t, server, "collector=c&debug=true&target="+url.QueryEscape(target.URL)).Body.String()
			if !strings.Contains(report, "decode       ok") || !strings.Contains(report, " "+kind+"\n") {
				t.Errorf("the debug report does not name the %s decoder:\n%s", kind, report)
			}
		})
	}
}
