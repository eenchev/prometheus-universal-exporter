//go:build !select_request_types || request_type_http

package allocator

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

const dnsProbes = `probe_jobs:
  - name: nodes
    collector: demo
    target_template: http://{{address}}/health
    discovery:
      - dns_sd:
          names: [_metrics._tcp.b.test, _metrics._tcp.a.test]
          server: SERVER
          refresh_interval: 15s
        labels:
          service: __meta_dns_name
`

// A probe job's dns_sd source, written in the probe targets file, serves
// its targets as any source's are, with the meta labels for Prometheus's
// relabeling and the labels mapping's, and is named in the metrics as
// dns_sd_<n>.
func TestADNSSDSourceServesItsTargets(t *testing.T) {
	stub := testutil.NewDNSStub(t, srvRecords()...)
	p := writeInputs(t, testConfig, "", strings.Replace(dnsProbes, "SERVER", stub.Addr, 1))
	a, _ := settledAllocator(t, mustLoad(t, p), "r1")
	a.refreshDiscovery(context.Background())
	groups, err := a.SD("nodes")
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, g := range groups {
		targets = append(targets, g.Labels["__param_target"])
	}
	if want := "http://node-a.test:9100/health http://node-b.test:9100/health http://node-c.test:9200/health"; strings.Join(targets, " ") != want {
		t.Fatalf("served %v, want %s", targets, want)
	}
	labels := groups[2].Labels
	for name, want := range map[string]string{dnsNameLabel: "_metrics._tcp.a.test", dnsSRVTargetLabel: "node-c.test", dnsSRVPortLabel: "9200", "service": "_metrics._tcp.a.test"} {
		if labels[name] != want {
			t.Errorf("label %s is %q, want %q", name, labels[name], want)
		}
	}
	var metrics bytes.Buffer
	if err := a.WriteMetrics(&metrics); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`pue_allocator_discovered_targets{kind="probe",owner="nodes",source="dns_sd_0"} 3`,
		`pue_allocator_discovery_refreshes_total{kind="probe",owner="nodes",source="dns_sd_0",result="success"} 1`,
	} {
		if !strings.Contains(metrics.String(), line) {
			t.Errorf("the metrics have no %s:\n%s", line, metrics.String())
		}
	}
}
