//go:build !select_request_types || request_type_http

package exporter

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// The uptime example answers what docs/REQUESTS.md's "Uptime probes" says
// it answers: of a 200, a 503, a redirect, a TLS site and a closed port,
// whether the site is up, its status, the phases of its request, the
// redirects, the TLS connection and its certificate's expiry, and the probe
// series report_failures adds; a site over plain HTTP has no TLS series and
// logs no failure for them, and the example lints clean.
func TestTheUptimeExampleAnswersTheDocumentedSeries(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	cfg, err := config.Load("../../configs/config.uptime.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if findings := transform.LintConfiguration(cfg); len(findings) > 0 {
		t.Errorf("the example does not lint clean: %+v", findings)
	}
	site := http.NewServeMux()
	site.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	site.HandleFunc("/down", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	site.Handle("/moved", http.RedirectHandler("/", http.StatusMovedPermanently))
	plain := httptest.NewServer(site)
	defer plain.Close()
	secure := httptest.NewTLSServer(site)
	defer secure.Close()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "http://" + closed.Addr().String()
	_ = closed.Close()

	server := flightServer(t, cfg.Collectors...)
	expiry := strconv.FormatFloat(float64(secure.Certificate().NotAfter.Unix()), 'g', -1, 64)
	for _, probe := range []struct {
		target, extra string
		want, absent  []string
	}{
		{plain.URL, "", []string{"site_up 1", "site_http_status_code 200", `site_http_duration_seconds{phase="first_byte"}`, `site_http_duration_seconds{phase="total"}`, "site_http_redirects 0", "site_http_ssl 0", "http_exporter_probe_success 1"},
			[]string{`phase="tls"`, `phase="dns"`, "site_tls_", "http_exporter_probe_failed_stage"}},
		{plain.URL + "/down", "", []string{"site_up 0", "site_http_status_code 503", "site_http_redirects 0", "http_exporter_probe_success 1"}, []string{"http_exporter_probe_failed_stage"}},
		{plain.URL + "/moved", "", []string{"site_up 1", "site_http_status_code 200", "site_http_redirects 1", "http_exporter_probe_success 1"}, nil},
		{secure.URL, "&insecure_skip_verify=true", []string{"site_up 1", "site_http_ssl 1", `site_http_duration_seconds{phase="tls"}`, `site_http_duration_seconds{phase="connect"}`, "site_tls_cert_expiry_timestamp_seconds " + expiry, `site_tls_version_info{version="1.3"} 1`, "http_exporter_probe_success 1"}, nil},
		{secure.URL, "", []string{"http_exporter_probe_success 0", `http_exporter_probe_failed_stage{stage="http"} 1`, "http_exporter_probe_elapsed_seconds "}, []string{"site_"}},
		{closedURL, "", []string{"http_exporter_probe_success 0", `http_exporter_probe_failed_stage{stage="http"} 1`}, []string{"site_"}},
	} {
		got := probeOnce(t, server, probePath("site", probe.target, probe.extra), nil)
		body := got.Body.String()
		if got.Code != http.StatusOK {
			t.Errorf("%s%s: %d %s", probe.target, probe.extra, got.Code, body)
			continue
		}
		for _, want := range probe.want {
			if !strings.Contains(body, "\n"+want) && !strings.HasPrefix(body, want) {
				t.Errorf("%s%s: no %q in\n%s", probe.target, probe.extra, want, body)
			}
		}
		for _, absent := range probe.absent {
			if strings.Contains(body, absent) {
				t.Errorf("%s%s: %q in\n%s", probe.target, probe.extra, absent, body)
			}
		}
	}
	if strings.Contains(logs.String(), "metric extraction failed") {
		t.Errorf("a rule of the example failed:\n%s", logs)
	}
}
