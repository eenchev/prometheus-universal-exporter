//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"strings"
	"testing"
)

func TestProbeParametersFollowTheRequestType(t *testing.T) {
	registerFixtureType(t)
	server := pathServer(t, fixtureCollector())
	base := "target=fixture://data&collector=fixed"

	for _, param := range []string{"method=POST", "timeout=5s", "header_X-Tenant=a", "param_tenant=a", "retry_attempts=2"} {
		response := probeQueryString(t, server, base+"&"+param)
		name, _, _ := strings.Cut(param, "=")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), name) || !strings.Contains(response.Body.String(), `"fixture"`) {
			t.Errorf("%s: status=%d body=%s, want a 400 naming the parameter and the type", param, response.Code, response.Body.String())
		}
	}
	// A parameter the type accepts is fine, and one no type knows is ignored
	// as it always was, since a monitor may carry parameters of its own.
	for _, param := range []string{"path=/other", "unrelated=1"} {
		if response := probeQueryString(t, server, base+"&"+param); response.Code != http.StatusOK {
			t.Errorf("%s: status=%d body=%s", param, response.Code, response.Body.String())
		}
	}
}

// Every override http documents is accepted for an http collector.
func TestHTTPAcceptsEveryHTTPOverride(t *testing.T) {
	var recorder pathRecorder
	target := recorder.serve(t)
	c := pathCollector("/api/{{param_tenant}}")
	c.Request.ForwardHeaders = []string{"X-Tenant"}
	server := pathServer(t, c)
	response := probeWith(t, server, target.URL, "&param_tenant=a&method=GET&timeout=50s&body=x&insecure_skip_verify=false"+
		"&follow_redirects=false&enable_http2=false&retry_attempts=0&retry_backoff=0s&header_X-Tenant=a")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
