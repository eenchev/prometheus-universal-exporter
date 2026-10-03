//go:build !select_request_types || request_type_http

package exporter

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The examples that read a public service are run, as shipped, against a
// stand-in that answers in the service's documented shape, so they are
// covered with the rest of the suite and without the network
// (docs/DEVELOPMENT.md). The opt-in external suite probes the services
// themselves (external_e2e_test.go).

// standInAnswer is what the stand-in sends for one path.
type standInAnswer struct {
	contentType string
	body        []byte
}

// standIn stands in for a public service: it answers the paths it was given,
// or every path with status when that is set, anything else with 404, and
// keeps the path and query of every request.
type standIn struct {
	*httptest.Server
	mu     sync.Mutex
	asked  []string
	status int
}

func (s *standIn) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

func (s *standIn) answerWith(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// readTestdata reads a file of the repository's testdata directory.
func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// newStandIn starts a stand-in for answers and loads the example at path.
// The examples allow https only, so the stand-in speaks TLS, and the one
// thing the loaded configuration is given that the file does not have is the
// stand-in's certificate to verify it with, on every collector.
func newStandIn(t *testing.T, path string, answers map[string]standInAnswer) (*standIn, *model.Config) {
	t.Helper()
	service := &standIn{}
	service.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.mu.Lock()
		service.asked = append(service.asked, r.URL.RequestURI())
		status := service.status
		service.mu.Unlock()
		answer, known := answers[r.URL.Path]
		switch {
		case status != 0:
			http.Error(w, "unavailable", status)
		case !known:
			http.NotFound(w, r)
		default:
			w.Header().Set("Content-Type", answer.contentType)
			_, _ = w.Write(answer.body)
		}
	}))
	t.Cleanup(service.Close)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: service.Certificate().Raw})
	if err := os.WriteFile(ca, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Collectors {
		cfg.Collectors[i].Request.TLS.CAFile = ca
	}
	return service, cfg
}

// probeStandIn probes collector at the stand-in, with extra query parameters
// as a scrape configuration's params would add them, and returns the answer's
// series. The age of the result, which a collector with cache.stale_if_error
// adds beside the stale marker, is the one series whose value is the clock's:
// it is left out of what is returned.
func probeStandIn(t *testing.T, server *Server, service *standIn, collector, extra string) []string {
	t.Helper()
	response := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(service.URL)+extra, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	return slices.DeleteFunc(samples(response.Body.String()), func(line string) bool {
		return strings.HasPrefix(line, resultAgeMetric+" ")
	})
}

// sameSeries reports the series that differ from the ones wanted.
func sameSeries(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
