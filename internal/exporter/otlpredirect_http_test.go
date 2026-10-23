//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An OTLP endpoint that redirects is not followed: the host it points to
// receives neither otlp.headers nor the body, and the export fails, its data
// points dropped and counted as a refused export's are, with a warning that
// names the status and the host the redirect leads to.
func TestOTLPExportsDoNotFollowRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var elsewhere atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				elsewhere.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(other.Close)
			var asked atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked.Add(1)
				http.Redirect(w, r, other.URL+"/v1/metrics", status)
			}))
			t.Cleanup(endpoint.Close)

			otlp := otlpConfig(endpoint.URL + "/v1/metrics")
			otlp.Headers = map[string]string{"X-Api-Key": "secret"}
			server := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlp}, nil)
			var logs bytes.Buffer
			server.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			queueProbeMetric(server, "probe_value", 1)
			server.exportOTLP(context.Background(), time.Minute)

			if elsewhere.Load() != 0 {
				t.Fatalf("the redirect was followed: the other host received %d requests", elsewhere.Load())
			}
			if asked.Load() != 1 {
				t.Fatalf("the endpoint was asked %d times, want once", asked.Load())
			}
			exposition := selfMetrics(t, server)
			if seriesValue(t, exposition, `http_exporter_otlp_exports_total{result="failure"}`) != 1 {
				t.Fatalf("a redirected export was not a failure:\n%s", exposition)
			}
			if got := seriesValue(t, exposition, "http_exporter_otlp_points_dropped_total"); got != 1 {
				t.Fatalf("dropped %v points, want the probe metric", got)
			}
			if _, ok := pendingValue(server, "probe_value"); ok {
				t.Fatal("the data of a redirected export was kept")
			}
			otherHost := strings.TrimPrefix(other.URL, "http://")
			log := logs.String()
			for _, want := range []string{"redirect, which is not followed", `"status":` + strconv.Itoa(status), `"location_host":"` + otherHost + `"`, "otlp.endpoint"} {
				if !strings.Contains(log, want) {
					t.Errorf("the warning lacks %q:\n%s", want, log)
				}
			}
			if strings.Contains(log, "secret") {
				t.Errorf("the warning carries the header's value:\n%s", log)
			}
		})
	}
}

// The error of a redirected attempt names the status and the host, and says
// it was not followed; a redirect without a Location says it names none.
func TestARedirectedOTLPAttemptSaysWhereItLed(t *testing.T) {
	for name, location := range map[string]string{"to another host": "http://collector.example:4318/v1/metrics", "without a Location": ""} {
		t.Run(name, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if location != "" {
					w.Header().Set("Location", location)
				}
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			t.Cleanup(endpoint.Close)
			server := otlpServer(t, endpoint.URL)
			cfg := otlpConfig(endpoint.URL + "/v1/metrics")
			answer, err := server.sendOTLP(context.Background(), cfg, model.TLSConfig{}, []byte("{}"), time.Second)
			if err == nil || answer.retryable {
				t.Fatalf("a redirect was not a refusal: %v %+v", err, answer)
			}
			want := "collector.example:4318"
			if location == "" {
				want = "no host"
			} else if u, _ := url.Parse(location); u.Host != want {
				t.Fatalf("the test's host is %q", u.Host)
			}
			for _, part := range []string{"307", want, "not followed", "otlp.endpoint"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("the error %q lacks %q", err, part)
				}
			}
		})
	}
}
