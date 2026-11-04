//go:build !select_request_types || request_type_http

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Loading a configuration marks the collectors none of whose rules reads
// $timings, whose requests are then not timed (model.Collector.TimingsUnread),
// and leaves timed those whose rules may read them.
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
`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Collectors[0].TimingsUnread || cfg.Collectors[1].TimingsUnread {
		t.Errorf("timings unread: up %v, phases %v; want true and false", cfg.Collectors[0].TimingsUnread, cfg.Collectors[1].TimingsUnread)
	}
}
