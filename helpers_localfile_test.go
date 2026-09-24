//go:build !select_request_types || request_type_localfile

package main

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
)

// The localfile request type reads a file under request.root
// (fetch/requesttype_localfile.go).

const promFile = "# HELP app_jobs_total Jobs run.\n# TYPE app_jobs_total counter\napp_jobs_total{queue=\"default\"} 7\n"

func probeFile(t *testing.T, server *exporter.Server, query string) *httpResult {
	t.Helper()
	recorder := probeOnce(t, server, "/probe?"+query, nil)
	return &httpResult{code: recorder.Code, body: recorder.Body.String()}
}

type httpResult struct {
	code int
	body string
}

func (r *httpResult) must(t *testing.T, code int, fragments ...string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("status=%d, want %d; body:\n%s", r.code, code, r.body)
	}
	for _, fragment := range fragments {
		if !strings.Contains(r.body, fragment) {
			t.Fatalf("body does not contain %q:\n%s", fragment, r.body)
		}
	}
}
