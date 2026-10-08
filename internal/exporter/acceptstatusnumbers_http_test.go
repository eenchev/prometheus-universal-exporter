//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A status of request.accept_status written as a number YAML reads as one —
// with a point, an exponent, in hex or octal, with an underscore, a sign or
// leading zeros — is the status it is to a request too: a probe of a
// collector and a scrape of a static target, each read from its file as the
// exporter reads it, decode the answer of that status, and fail on another,
// as the schemas, which are handed the number, say. 503.0 and 0x1F7 were
// refused at load, and +503 and 0503 taken there and matched no status; so
// were "0503" and '+503' in quotes, which are text, taken as the status they
// read as, and now accept it.
func TestAStatusWrittenAsANumberIsTheStatusARequestAccepts(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, "value=4\n")
	}))
	defer target.Close()
	collector := func(list string) string {
		return "collectors:\n  - name: text\n    request:\n      type: http\n      accept_status: [" + list + "]\n    transform: {type: regex}\n    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n"
	}
	targets := func(list string) string {
		return "interval: 1m\ntargets:\n  - name: own\n    collector: text\n    target: " + target.URL + "\n    request: {accept_status: [" + list + "]}\n"
	}
	for written, accepted := range map[string]bool{
		"503": true, "503.0": true, "5.03e2": true, "0x1F7": true, "0o767": true, "5_03": true, "+503": true, "0503": true,
		`"0503"`: true, `'+503'`: true, `" 0503 "`: true,
		"5.04e2": false, "0x1F8": false, "200.0": false, "5_04": false,
	} {
		cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", collector(written)))
		if err != nil {
			t.Fatalf("accept_status: [%s]: %v", written, err)
		}
		got := probeOnce(t, flightServer(t, cfg.Collectors...), probePath("text", target.URL, ""), nil)
		if decoded := got.Code == http.StatusOK && strings.Contains(got.Body.String(), "demo_value 4\n"); decoded != accepted {
			t.Errorf("a probe of a collector of accept_status: [%s]: %d %s; want the 503 decoded %v", written, got.Code, got.Body, accepted)
		} else if !accepted && !strings.Contains(got.Body.String(), "received HTTP status 503") {
			t.Errorf("a probe of a collector of accept_status: [%s]: %d %s; want the status failed", written, got.Code, got.Body)
		}

		plain, err := config.Load(testutil.WriteFile(t, "config.yaml", collector("2xx")))
		if err != nil {
			t.Fatal(err)
		}
		file, err := config.LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", targets(written)))
		if err != nil {
			t.Fatalf("a target's accept_status: [%s]: %v", written, err)
		}
		server := newStaticServer(t, plain, file)
		server.logger = testutil.QuietLogger(t)
		server.scrapeStaticTargets(t.Context(), 0)
		if decoded := strings.Contains(getStaticTargets(t, server, "/static-targets"), `demo_value{static_target="own"} 4`); decoded != accepted {
			t.Errorf("a static target of accept_status: [%s]: want the 503 decoded %v", written, accepted)
		}
	}
}
