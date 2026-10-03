package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Decoding, transforms and exposition are shared by every type: a collector
// whose type fetches its bytes some other way is served like any other.
func TestAProbeFetchesThroughTheCollectorsType(t *testing.T) {
	registerFixtureType(t)
	server := pathServer(t, fixtureCollector())
	response := probeQueryString(t, server, "target=fixture://data&collector=fixed")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "demo_value 5") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func probeQueryString(t *testing.T, server *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?"+query, nil))
	return recorder
}
