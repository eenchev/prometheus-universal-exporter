//go:build request_type_sip

package allocator

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The allocator, built with the opt-in sip type as the exporters it serves
// are, takes its collectors, static targets and probe jobs: a SIP server
// is a target it allocates as any other, the files a sip collector names
// are the replicas', and a target that is no server is refused by it.
func TestTheAllocatorTakesSIPCollectors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "mounted-on-the-replicas")
	configText := `collectors:
  - name: pbx
    request:
      type: sip
      sip_transport: tls
      username: monitor
      password_file: ` + missing + `/password
      tls:
        ca_file: ` + missing + `/ca.pem
    transform: {type: jq}
    metrics:
      - name: sip_up
        expression: .ok
`
	targets := "interval: 1m\ntargets:\n  - name: pbx_1\n    collector: pbx\n    target: TARGET\n"
	p := Paths{
		Config:        testutil.WriteIn(t, dir, "main.yaml", configText),
		StaticTargets: testutil.WriteIn(t, dir, "targets.yaml", strings.Replace(targets, "TARGET", "pbx1.example.com", 1)),
		ProbeTargets:  testutil.WriteIn(t, dir, "probes.yaml", "probe_jobs:\n  - name: pbxs\n    collector: pbx\n    targets: ['pbx2.example.com:5061', pbx3.example.com]\n"),
	}
	in, err := LoadInputs(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Targets.Targets) != 1 || len(in.Probes.Jobs) != 1 {
		t.Fatalf("got %d static targets and %d probe jobs", len(in.Targets.Targets), len(in.Probes.Jobs))
	}
	p.StaticTargets = testutil.WriteIn(t, dir, "targets.yaml", strings.Replace(targets, "TARGET", "sip:monitor@pbx1.example.com", 1))
	if _, err := LoadInputs(p); err == nil || !strings.Contains(err.Error(), "is a SIP URI") {
		t.Errorf("a sip target that is a URI: %v", err)
	}
}
