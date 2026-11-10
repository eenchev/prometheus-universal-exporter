//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A target that refuses the credential is never answered for with a stale
// result, and neither are the probes failure_backoff keeps from it after:
// they stand for that refusal, and would otherwise hand out, for as long as
// stale_if_error lasts, what a credential since revoked was given. A
// target backed off from for another failure is still answered stale.
func TestAProbeBackedOffFromARefusedCredentialIsNotAnsweredStale(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusOK)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code := int(status.Load()); code != http.StatusOK {
			http.Error(w, "no", code)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	defer target.Close()
	stale := backoffCollector("stale", 1, time.Minute, time.Minute)
	stale.Cache.StaleIfError = model.Duration(time.Hour)
	server := modeServer(t, stale)
	server.logger = testutil.QuietLogger(t)
	clock := newBackoffClock(server)
	probeURL := probeOf("stale", target.URL)
	if r := probeOnce(t, server, probeURL, nil); r.Code != http.StatusOK {
		t.Fatalf("the first probe is answered %d: %s", r.Code, r.Body)
	}
	status.Store(http.StatusUnauthorized)
	for i, want := range []string{"received HTTP status 401", "is backing off after 1 consecutive failed scrapes"} {
		r := probeOnce(t, server, probeURL, nil)
		if r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), want) {
			t.Fatalf("probe %d after the credential was refused is answered %d: %s; want %s and no stale result", i+1, r.Code, r.Body, want)
		}
	}
	// The next try fails otherwise: the probes kept from the target after
	// it are answered stale again.
	status.Store(http.StatusInternalServerError)
	clock.advance(2 * time.Minute)
	for i := range 2 {
		r := probeOnce(t, server, probeURL, nil)
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "http_exporter_result_stale 1") {
			t.Fatalf("probe %d after a 500 is answered %d: %s; want the last good result", i+1, r.Code, r.Body)
		}
	}
}
