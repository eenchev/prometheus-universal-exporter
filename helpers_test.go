package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func newCacheTestServer(t *testing.T, collectors ...model.Collector) (*exporter.Server, *config.Manager) {
	t.Helper()
	cfg := &model.Config{Collectors: collectors}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, "", slog.Default())
	return exporter.NewServer(manager, "python3", slog.Default()), manager
}

func probeOnce(t *testing.T, server *exporter.Server, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// collector_files lists further files of collectors (config/collectorfiles.go). Each
// holds a collectors list and nothing else, and a collector name is unique
// across the configuration and every file.

// get serves one request through the exporter's handler.
func get(server *exporter.Server, method, target, acceptEncoding string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	if acceptEncoding != "" {
		request.Header.Set("Accept-Encoding", acceptEncoding)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}
