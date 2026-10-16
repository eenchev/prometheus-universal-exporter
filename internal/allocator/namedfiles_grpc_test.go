//go:build !select_request_types || request_type_grpc

package allocator

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The allocator checks the configuration without opening the files it
// names, which are mounted on the replicas and not on the allocator: a grpc
// collector's missing descriptor set, missing otlp.tls and web.basic_auth
// files are no refusal, a static target's message for that collector is not
// checked against the descriptor set, and a probe job of it is taken. The
// exporter's own loader still refuses the same configuration for the
// missing files, so the replicas check them. A mistake that needs no file
// is refused by the allocator as before.
func TestTheAllocatorAcceptsAConfigurationNamingFilesItDoesNotHave(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "mounted-on-the-replicas")
	configText := `otlp:
  enabled: true
  endpoint: https://collector.invalid/v1/metrics
  tls:
    ca_file: ` + missing + `/ca.pem
web:
  basic_auth:
    enabled: true
    username: admin
    password_file: ` + missing + `/password
collectors:
  - name: queue
    request:
      type: grpc
      rpc: acme.queue.v1.QueueService/GetStats
      descriptors: protoset
      protoset_file: ` + missing + `/queue.pb
      bearer_token_file: ` + missing + `/token
    transform:
      type: jq
    metrics:
      - name: depth
        expression: .depth
`
	p := Paths{
		Config:        testutil.WriteIn(t, dir, "main.yaml", configText),
		StaticTargets: testutil.WriteIn(t, dir, "targets.yaml", "interval: 30s\ntargets:\n  - name: q\n    collector: queue\n    target: q:9090\n    request: {message: '{\"no_such_field\": 1}'}\n"),
		ProbeTargets:  testutil.WriteIn(t, dir, "probes.yaml", "probe_jobs:\n  - name: queues\n    collector: queue\n    targets: [q1:9090, q2:9090]\n"),
	}
	in, err := LoadInputs(p)
	if err != nil {
		t.Fatalf("the allocator refused a configuration for files the replicas mount: %v", err)
	}
	if len(in.Targets.Targets) != 1 || len(in.Probes.Jobs) != 1 {
		t.Fatalf("got %d static targets and %d probe jobs", len(in.Targets.Targets), len(in.Probes.Jobs))
	}
	if _, err := config.Load(p.Config); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("the exporter's loader gave %v, want the configuration refused for a missing file", err)
	}
	refused := strings.Replace(configText, "      descriptors: protoset\n", "      descriptors: reflection\n", 1)
	p.Config = testutil.WriteIn(t, dir, "main.yaml", refused)
	if _, err := LoadInputs(p); err == nil || !strings.Contains(err.Error(), "sets request.protoset_file, which applies only with descriptors: protoset") {
		t.Errorf("a protoset_file with descriptors: reflection gave %v", err)
	}
}
