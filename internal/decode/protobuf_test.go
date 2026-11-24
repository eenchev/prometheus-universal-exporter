//go:build !select_request_types || request_type_grpc

package decode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/protobuf/encoding/protowire"
)

const reportProto = `syntax = "proto3";
package metrics.v1;
message Item {
  string id = 1;
  uint32 count = 2;
}
message Report {
  string name = 1;
  int64 total = 2;
  repeated Item items = 3;
  double ratio = 4;
  bool healthy = 5;
}
`

// reportBody is a metrics.v1.Report on the wire: name, total, one item and
// a ratio, with healthy left unset.
func reportBody() []byte {
	item := protowire.AppendTag(nil, 1, protowire.BytesType)
	item = protowire.AppendString(item, "disk")
	item = protowire.AppendTag(item, 2, protowire.VarintType)
	item = protowire.AppendVarint(item, 7)
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendString(b, "nightly")
	b = protowire.AppendTag(b, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, 42)
	b = protowire.AppendTag(b, 3, protowire.BytesType)
	b = protowire.AppendBytes(b, item)
	b = protowire.AppendTag(b, 4, protowire.Fixed64Type)
	return protowire.AppendFixed64(b, 0x3FE0000000000000) // 0.5
}

func protobufCollector(t *testing.T, message string) *model.Collector {
	t.Helper()
	file := filepath.Join(t.TempDir(), "report.proto")
	if err := os.WriteFile(file, []byte(reportProto), 0o600); err != nil {
		t.Fatal(err)
	}
	return &model.Collector{
		Name:     "report",
		Decoder:  model.DecoderConfig{Type: "protobuf"},
		Response: model.ResponseConfig{Protobuf: model.ProtobufConfig{Message: message, ProtoFiles: []string{file}}},
	}
}

// The protobuf decoder reads a body as the message response.protobuf names
// and hands on what a grpc answer reads as: fields by their .proto names,
// the unset ones with their zero values, a 64-bit integer as a string.
func TestTheProtobufDecoderReadsTheMessage(t *testing.T) {
	c := protobufCollector(t, "metrics.v1.Report")
	d, err := Decode(&fetch.HTTPResponse{Body: reportBody(), Headers: map[string][]string{"Content-Type": {"application/x-protobuf"}}}, c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(d.Data)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"healthy":false,"items":[{"count":7,"id":"disk"}],"name":"nightly","ratio":0.5,"total":"42"}`
	if d.Kind != "protobuf" || string(got) != want {
		t.Fatalf("kind %q, data %s, want %s", d.Kind, got, want)
	}
}

// A body that is not the message fails the decode, and says which message
// it was read as; a message the files do not have is said so.
func TestTheProtobufDecoderRefusesWhatIsNotTheMessage(t *testing.T) {
	c := protobufCollector(t, "metrics.v1.Report")
	_, err := Decode(&fetch.HTTPResponse{Body: []byte(`{"name": "nightly"}`)}, c)
	if err == nil || !strings.Contains(err.Error(), "protobuf decode: the body is not a metrics.v1.Report message") {
		t.Fatalf("err=%v", err)
	}
	c = protobufCollector(t, "metrics.v1.Missing")
	if err := fetch.CheckProtobufDecoder(c); err == nil || !strings.Contains(err.Error(), `message "metrics.v1.Missing" is not in proto_files`) {
		t.Fatalf("err=%v", err)
	}
}
