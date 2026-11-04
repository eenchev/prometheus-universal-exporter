//go:build !select_request_types || request_type_http

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Loading a configuration marks the collectors none of whose rules reads
// $timings or $remote_ip, whose requests are then not traced
// (model.Collector.TimingsUnread), and leaves traced those whose rules may
// read them: $http_version is the answer's, which needs no trace.
func TestALoadedCollectorIsTimedOnlyWhereARuleReadsItsTimings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	document := `collectors:
  - name: up
    request: {type: http}
    decoder: {type: none}
    transform: {type: jq}
    metrics:
      - name: up
        expression: 'if $tls == null then $status else 0 end'
      - name: http_version
        expression: '$http_version | tonumber'
  - name: phases
    request: {type: http}
    decoder: {type: none}
    transform: {type: jq}
    metrics:
      - name: phase_seconds
        items: '$timings | to_entries[] | select(.value != null)'
        expression: .value
        labels:
          - name: phase
            expression: .key
  - name: address
    request: {type: http}
    decoder: {type: none}
    transform: {type: jq}
    metrics:
      - name: ip_protocol
        expression: 'if ($remote_ip | contains(":")) then 6 else 4 end'
`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Collectors[0].TimingsUnread || cfg.Collectors[1].TimingsUnread || cfg.Collectors[2].TimingsUnread {
		t.Errorf("timings unread: up %v, phases %v, address %v; want true, false and false", cfg.Collectors[0].TimingsUnread, cfg.Collectors[1].TimingsUnread, cfg.Collectors[2].TimingsUnread)
	}
}
