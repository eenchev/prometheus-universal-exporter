//go:build request_type_radius && request_type_whois

package allocator

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The allocator, built with the opt-in radius and whois types as the
// exporters it serves are, takes their collectors, static targets and
// probe jobs: a RADIUS server and a domain are targets it allocates as any
// other, the files a radius collector names are the replicas', and a
// target that is none of its type's is refused by it.
func TestTheAllocatorTakesRADIUSAndWhoisCollectors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "mounted-on-the-replicas")
	configText := `collectors:
  - name: login
    request:
      type: radius
      secret_file: ` + missing + `/secret
      username: monitor
      password_file: ` + missing + `/password
    transform: {type: jq}
    metrics:
      - name: radius_accepted
        expression: .accepted
  - name: domain
    cache: {ttl: 12h}
    request:
      type: whois
    transform: {type: jq}
    metrics:
      - name: domain_expiry_timestamp_seconds
        expression: .expires_timestamp_seconds
`
	targets := "interval: 12h\ntargets:\n  - name: example\n    collector: domain\n    target: DOMAIN\n  - name: radius_1\n    collector: login\n    target: radius1.example.com\n"
	p := Paths{
		Config:        testutil.WriteIn(t, dir, "main.yaml", configText),
		StaticTargets: testutil.WriteIn(t, dir, "targets.yaml", strings.Replace(targets, "DOMAIN", "example.com", 1)),
		ProbeTargets:  testutil.WriteIn(t, dir, "probes.yaml", "probe_jobs:\n  - name: domains\n    collector: domain\n    targets: [example.org, example.net]\n  - name: radius\n    collector: login\n    targets: ['radius2.example.com:1812']\n"),
	}
	in, err := LoadInputs(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Targets.Targets) != 2 || len(in.Probes.Jobs) != 2 {
		t.Fatalf("got %d static targets and %d probe jobs", len(in.Targets.Targets), len(in.Probes.Jobs))
	}
	p.StaticTargets = testutil.WriteIn(t, dir, "targets.yaml", strings.Replace(targets, "DOMAIN", "https://example.com", 1))
	if _, err := LoadInputs(p); err == nil || !strings.Contains(err.Error(), "is not a domain") {
		t.Errorf("a whois target that is a URL: %v", err)
	}
}
