//go:build !select_request_types || request_type_http || request_type_graphite || request_type_grpc

package exporter

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func getStaticTargets(t *testing.T, server *Server, path string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s answered %d: %s", path, recorder.Code, recorder.Body)
	}
	if err := parseExposition(recorder.Body.Bytes()); err != nil {
		t.Fatalf("%s is not a valid exposition: %v\n%s", path, err, recorder.Body)
	}
	return recorder.Body.String()
}
