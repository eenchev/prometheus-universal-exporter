//go:build !select_request_types || request_type_grpc

package exporter

// A benchmark of a whole probe of a grpc collector whose descriptors are
// read from files, so it measures, with the call to a server on this
// machine, the check at each call that the files did not change
// (docs/DEVELOPMENT.md "Measuring speed"):
//
//	go test -run '^$' -bench 'ProbeGRPC' -benchtime 2s ./internal/exporter/
//
// proto names queue.proto, which imports shard.proto, so the check looks at
// two files; protoset names one FileDescriptorSet.

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
)

func BenchmarkProbeGRPC(b *testing.B) {
	upstream := grpctest.Start(b, grpctest.Options{Answer: queueAnswer})
	dir := b.TempDir()
	root := filepath.Join(dir, "etc", "exporter", "protos")
	queue := grpctest.WriteSources(b, root)
	set := grpctest.WriteProtoset(b, filepath.Join(dir, "queue.pb"), false)
	const collector = "    request:\n      type: grpc\n      rpc: acme.queue.v1.QueueService/GetStats\n" +
		"      message: '{\"queue\": \"orders\", \"include_shards\": true}'\n"
	const metrics = "    transform:\n      type: jq\n    metrics:\n      - name: queue_total\n        expression: .total\n"
	yaml := "collectors:\n" +
		"  - name: proto\n" + collector + "      descriptors: proto\n      proto_files: [" + queue + "]\n      proto_import_paths: [" + root + "]\n" + metrics +
		"  - name: protoset\n" + collector + "      descriptors: protoset\n      protoset_file: " + set + "\n" + metrics
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		b.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		b.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewServer(config.NewManager(cfg, path, quiet), "python3", quiet).Handler()
	for _, name := range []string{"proto", "protoset"} {
		b.Run(name, func(b *testing.B) {
			target := "/probe?collector=" + name + "&target=" + url.QueryEscape(upstream.Addr)
			b.ReportAllocs()
			for b.Loop() {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
				if recorder.Code != http.StatusOK {
					b.Fatal(recorder.Code, recorder.Body.String())
				}
			}
		})
	}
}
