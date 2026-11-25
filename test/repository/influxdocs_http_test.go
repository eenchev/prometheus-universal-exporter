//go:build !select_request_types || request_type_http

package repository

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The two collectors of "Querying InfluxDB" in docs/CONFIGURATION.md load
// with --config.expand-env, ask InfluxDB what the page says they ask, and
// turn its answers into the page's series: InfluxQL's JSON with the jq
// transform, and Flux's annotated CSV, the page's own example of it, with
// the csv transform and response.csv.comment.
func TestQueryingInfluxDBExamples(t *testing.T) {
	t.Setenv("INFLUX_TOKEN", "s3cret")
	blocks := docBlocks(t, "docs/CONFIGURATION.md", "yaml")
	text := docBlocks(t, "docs/CONFIGURATION.md", "text")
	annotated := docBlock(t, text, "#group,")
	influx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token s3cret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/query":
			q := r.URL.Query()
			if r.Method != http.MethodGet || q.Get("db") != "telegraf" || !strings.HasPrefix(q.Get("q"), `SELECT last("usage_idle")`) || !strings.HasSuffix(q.Get("q"), `GROUP BY "host"`) {
				http.Error(w, "unexpected query "+r.URL.RawQuery, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"results":[{"statement_id":0,"series":[`+
				`{"name":"cpu","tags":{"host":"web01"},"columns":["time","idle"],"values":[["2026-11-25T10:00:00Z",97.5]]},`+
				`{"name":"cpu","tags":{"host":"web02"},"columns":["time","idle"],"values":[["2026-11-25T10:00:00Z",88]]}]}]}`)
		case "/api/v2/query":
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPost || r.URL.Query().Get("org") != "example" || r.Header.Get("Accept") != "application/csv" ||
				r.Header.Get("Content-Type") != "application/vnd.flux" || !strings.HasPrefix(string(body), `from(bucket: "telegraf")`) {
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			// InfluxDB ends its lines with a carriage return and a line
			// feed, and the answer with an empty line.
			_, _ = io.WriteString(w, strings.ReplaceAll(annotated, "\n", "\r\n")+"\r\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer influx.Close()

	for _, name := range []string{"influxql_cpu", "flux_cpu"} {
		conf := docBlock(t, blocks, "collectors:\n  - name: "+name+"\n")
		path := testutil.WriteIn(t, t.TempDir(), "config.yaml", conf)
		cfg, err := config.Load(path, config.WithEnvExpansion())
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, conf)
		}
		server := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?collector="+name+"&target="+url.QueryEscape(influx.URL), nil))
		for _, want := range []string{`influx_cpu_idle_percent{host="web01"} 97.5`, `influx_cpu_idle_percent{host="web02"} 88`} {
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
				t.Errorf("%s: missing %q: %d\n%s", name, want, recorder.Code, recorder.Body)
			}
		}
	}
}
