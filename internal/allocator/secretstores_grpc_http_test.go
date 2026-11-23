//go:build (!select_request_types || request_type_grpc) && (!select_request_types || request_type_http)

package allocator

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The allocator takes a configuration whose credentials come from secret
// stores, whose protobuf decoder reads descriptor files and whose scripts
// keep state, without the files those name, which the replicas mount: a
// vault store's CA and login files, the decoder's .proto file. It reaches
// no store. A reference to a store the configuration has not is refused by
// it as by an exporter, of a collector and of a static target.
func TestTheAllocatorTakesSecretStoresAndTheProtobufDecoderWithoutTheirFiles(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "mounted-on-the-replicas")
	configText := `secret_stores:
  - name: vault-prod
    type: vault
    address: https://vault.invalid:8200
    auth: {method: kubernetes, role: exporter, jwt_file: ` + missing + `/token}
    tls: {ca_file: ` + missing + `/vault-ca.pem}
  - name: gcp
    type: gcp
    project: demo
    credentials_file: ` + missing + `/key.json
collectors:
  - name: report
    request:
      type: http
      basic_auth_file:
        username: secret://vault-prod/secret/data/report#user
        password: secret://vault-prod/secret/data/report#password
    decoder: {type: protobuf}
    response:
      protobuf:
        message: metrics.v1.Report
        proto_files: [` + missing + `/report.proto]
    transform:
      type: jq
    metrics:
      - name: report_total
        expression: .total | tonumber
  - name: counted
    request:
      type: http
      bearer_token_file: secret://gcp/api-token
    transform:
      type: python
      state: true
      script: |
        state["runs"] = state.get("runs", 0) + 1
        metric(name="runs", value=state["runs"])
    metrics: []
`
	targets := "interval: 30s\ntargets:\n  - name: one\n    collector: counted\n    target: https://api.example.com\n    request: {bearer_token_file: 'secret://STORE/api-token'}\n"
	p := Paths{
		Config:        testutil.WriteIn(t, dir, "main.yaml", configText),
		StaticTargets: testutil.WriteIn(t, dir, "targets.yaml", strings.Replace(targets, "STORE", "gcp", 1)),
		ProbeTargets:  testutil.WriteIn(t, dir, "probes.yaml", "probe_jobs:\n  - name: reports\n    collector: report\n    targets: ['https://a.example.com', 'https://b.example.com']\n"),
	}
	in, err := LoadInputs(p)
	if err != nil {
		t.Fatalf("the allocator refused a configuration for files the replicas mount: %v", err)
	}
	if len(in.Targets.Targets) != 1 || len(in.Probes.Jobs) != 1 {
		t.Fatalf("got %d static targets and %d probe jobs", len(in.Targets.Targets), len(in.Probes.Jobs))
	}
	p.StaticTargets = testutil.WriteIn(t, dir, "targets.yaml", strings.Replace(targets, "STORE", "elsewhere", 1))
	if _, err := LoadInputs(p); err == nil || !strings.Contains(err.Error(), `names the secret store "elsewhere"`) {
		t.Errorf("a target's reference to a store the configuration has not: %v", err)
	}
	p.Config = testutil.WriteIn(t, dir, "main.yaml", strings.Replace(configText, "secret://gcp/api-token", "secret://nowhere/api-token", 1))
	if _, err := LoadInputs(p); err == nil || !strings.Contains(err.Error(), `names the secret store "nowhere"`) {
		t.Errorf("a collector's reference to a store the configuration has not: %v", err)
	}
}
