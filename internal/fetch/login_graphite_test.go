//go:build !select_request_types || request_type_graphite

package fetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A graphite collector logs in as an http one does, its request being one
// made over HTTP, and a static target of it may log in as it does itself.
func TestAGraphiteCollectorLogsIn(t *testing.T) {
	logins.reset()
	t.Cleanup(logins.reset)
	var made atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			made.Add(1)
			_, _ = io.WriteString(w, `{"token": "graphite-token"}`)
		case "/render":
			if r.Header.Get("Authorization") != "Bearer graphite-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `[]`)
		}
	}))
	defer server.Close()
	c := graphiteCollector("app.requests")
	c.Request.Login = &model.LoginConfig{Type: "json", URL: "/login", Token: ".token", TTL: model.Duration(time.Minute), FieldFiles: map[string]string{"password": testutil.WriteFile(t, "password", "p")}}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	response, err := FetchCollector(context.Background(), server.URL, &c, RequestOverrides{}, nil)
	if err != nil || response.StatusCode != http.StatusOK || made.Load() != 1 {
		t.Fatalf("fetch: %v, %+v, %d logins", err, response, made.Load())
	}
	target := model.StaticTarget{Name: "g", Collector: c.Name, Target: server.URL, Request: model.TargetRequestConfig{Login: &model.LoginConfig{Type: "json", URL: "/login"}}}
	if err := CheckTargetRequest(&target, &c); err == nil || !strings.Contains(err.Error(), "sets 0 of token") {
		t.Errorf("a graphite target's login is not checked: %v", err)
	}
}
