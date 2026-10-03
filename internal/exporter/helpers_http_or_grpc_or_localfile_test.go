//go:build !select_request_types || request_type_http || request_type_grpc || request_type_localfile

package exporter

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func selfMetrics(t *testing.T, server *Server) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/self-metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("self-metrics status=%d", recorder.Code)
	}
	return recorder.Body.String()
}
