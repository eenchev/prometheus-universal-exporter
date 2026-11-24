//go:build request_type_amqp && request_type_nats

package allocator

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The allocator, built with the opt-in amqp and nats types as the
// exporters it serves are, takes their collectors, static targets and
// probe jobs: a broker and a server are targets it allocates as any other,
// the files a collector names are the replicas', and a target that is none
// of its type's is refused by it.
func TestTheAllocatorTakesAMQPAndNATSCollectors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "mounted-on-the-replicas")
	configText := `collectors:
  - name: broker
    request:
      type: amqp
      username: monitor
      password_file: ` + missing + `/password
      queues: [orders]
      tls:
        ca_file: ` + missing + `/ca.pem
    transform: {type: jq}
    metrics:
      - name: amqp_queue_messages
        expression: .queues[0].messages
  - name: bus
    request:
      type: nats
      bearer_token_file: ` + missing + `/token
      jetstream: true
      streams: [ORDERS]
    transform: {type: jq}
    metrics:
      - name: nats_rtt_seconds
        expression: .rtt_seconds
`
	targets := "interval: 1m\ntargets:\n  - name: broker_1\n    collector: broker\n    target: amqps://rabbit1.example.com\n  - name: bus_1\n    collector: bus\n    target: TARGET\n"
	p := Paths{
		Config:        testutil.WriteIn(t, dir, "main.yaml", configText),
		StaticTargets: testutil.WriteIn(t, dir, "targets.yaml", strings.Replace(targets, "TARGET", "nats1.example.com", 1)),
		ProbeTargets:  testutil.WriteIn(t, dir, "probes.yaml", "probe_jobs:\n  - name: brokers\n    collector: broker\n    targets: ['rabbit2.example.com:5672']\n  - name: buses\n    collector: bus\n    targets: ['tls://nats2.example.com']\n"),
	}
	in, err := LoadInputs(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Targets.Targets) != 2 || len(in.Probes.Jobs) != 2 {
		t.Fatalf("got %d static targets and %d probe jobs", len(in.Targets.Targets), len(in.Probes.Jobs))
	}
	p.StaticTargets = testutil.WriteIn(t, dir, "targets.yaml", strings.Replace(targets, "TARGET", "http://nats1.example.com", 1))
	if _, err := LoadInputs(p); err == nil || !strings.Contains(err.Error(), "has the scheme http://") {
		t.Errorf("a nats target that is a URL: %v", err)
	}
}
