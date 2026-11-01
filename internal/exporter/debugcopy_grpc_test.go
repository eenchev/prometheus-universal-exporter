//go:build !select_request_types || request_type_grpc

package exporter

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Edit a copy of a grpc collector that reads its descriptors from files,
// a descriptor set or .proto files, leaves the files out, as it leaves out
// every key naming one, and the copy loads as a playground, whose request
// is never made, with and without --web.debug.dangerous.
func TestEditACopyOfAGRPCCollectorWithDescriptorFilesLoads(t *testing.T) {
	dir := t.TempDir()
	set := grpctest.WriteProtoset(t, filepath.Join(dir, "queue.pb"), true)
	proto := testutil.WriteIn(t, dir, "health.proto", "syntax = \"proto3\";\npackage h;\nmessage Req {}\nmessage Res { int32 up = 1; }\nservice S { rpc Check(Req) returns (Res); }\n")
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: protoset
    request:
      type: grpc
      rpc: acme.queue.v1.QueueService/GetStats
      descriptors: protoset
      protoset_file: `+set+`
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
  - name: proto
    request:
      type: grpc
      rpc: h.S/Check
      descriptors: proto
      proto_files: [`+proto+`]
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
`))
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Collectors {
		x := &cfg.Collectors[i]
		for _, dangerous := range []bool{false, true} {
			copied, _, err := copiedCollectorYAML(x, dangerous)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(copied, set) || strings.Contains(copied, proto) {
				t.Errorf("the copy of %s names a file:\n%s", x.Name, copied)
			}
			if _, err := config.ParseCollector([]byte(copied), refusedOnDebugPage); err != nil {
				t.Errorf("dangerous=%v: the copy of %s does not load as a playground: %v\n%s", dangerous, x.Name, err, copied)
			}
		}
	}
}
