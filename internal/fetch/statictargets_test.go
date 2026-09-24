package fetch

import (
	"net/http"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// Every request parameter of a static target reaches the overrides its
// scrape is made with; path and body only when the file gives them.
func TestTargetOverridesCarryEveryRequestParameter(t *testing.T) {
	var file model.StaticTargetFile
	document := `targets:
  - name: legacy_eu
    collector: text
    target: http://legacy.example:8080
    request:
      method: POST
      path: /api/status
      body: raw payload
      timeout: 5s
      insecure_skip_verify: true
      follow_redirects: true
      enable_http2: true
      retry:
        attempts: 2
        backoff: 1s
  - name: plain
    collector: text
    target: http://plain.example
`
	if err := yaml.Unmarshal([]byte(document), &file); err != nil {
		t.Fatal(err)
	}
	overrides := TargetOverrides(&file.Targets[0])
	if overrides.Method != http.MethodPost || !overrides.PathSet || overrides.Path != "/api/status" {
		t.Fatalf("method/path overrides: %+v", overrides)
	}
	if overrides.Body == nil || *overrides.Body != "raw payload" || overrides.Timeout != 5*time.Second {
		t.Fatalf("body/timeout overrides: %+v", overrides)
	}
	if overrides.InsecureSkipVerify == nil || !*overrides.InsecureSkipVerify {
		t.Fatalf("tls override: %+v", overrides.InsecureSkipVerify)
	}
	if overrides.FollowRedirects == nil || !*overrides.FollowRedirects || overrides.EnableHTTP2 == nil || !*overrides.EnableHTTP2 {
		t.Fatalf("transport overrides: %+v", overrides)
	}
	if overrides.RetryAttempts == nil || *overrides.RetryAttempts != 2 || overrides.RetryBackoff == nil || *overrides.RetryBackoff != time.Second {
		t.Fatalf("retry overrides: %+v", overrides)
	}
	plain := TargetOverrides(&file.Targets[1])
	if plain.PathSet || plain.Body != nil || plain.InsecureSkipVerify != nil || plain.RetryAttempts != nil || plain.FollowRedirects != nil {
		t.Fatalf("a target without request settings overrides nothing: %+v", plain)
	}
}
