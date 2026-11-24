//go:build (!select_request_types || request_type_http) && (!select_request_types || request_type_grpc)

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// protobufTestCollector is a valid collector of the protobuf decoder.
func protobufTestCollector(t *testing.T) model.Collector {
	t.Helper()
	file := filepath.Join(t.TempDir(), "report.proto")
	if err := os.WriteFile(file, []byte("syntax = \"proto3\";\npackage metrics.v1;\nmessage Report { int64 total = 1; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return model.Collector{
		Name:      "report",
		Request:   model.RequestConfig{Type: "http"},
		Decoder:   model.DecoderConfig{Type: "protobuf"},
		Response:  model.ResponseConfig{Protobuf: model.ProtobufConfig{Message: "metrics.v1.Report", ProtoFiles: []string{file}}},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics:   []model.MetricRule{{Name: "total", Expression: ".total | tonumber"}},
	}
}

// response.protobuf and the protobuf decoder are checked when the
// configuration loads: the decoder needs a message and one source of its
// description, the block the decoder, and the message is looked up in the
// files, which the watch of the configuration then looks at.
func TestTheProtobufDecoderIsCheckedAtLoad(t *testing.T) {
	for name, test := range map[string]struct {
		edit func(*model.Collector)
		want string
	}{
		"another decoder":   {func(c *model.Collector) { c.Decoder.Type = "json" }, "sets response.protobuf, the message of the protobuf decoder, but its decoder is json"},
		"no message":        {func(c *model.Collector) { c.Response.Protobuf.Message = "" }, "has no response.protobuf.message"},
		"no files":          {func(c *model.Collector) { c.Response.Protobuf.ProtoFiles = nil }, "names no file that describes metrics.v1.Report"},
		"both sources":      {func(c *model.Collector) { c.Response.Protobuf.ProtosetFile = "set.binpb" }, "so keep one"},
		"an empty file":     {func(c *model.Collector) { c.Response.Protobuf.ProtoFiles = append(c.Response.Protobuf.ProtoFiles, " ") }, "response.protobuf.proto_files[1] is empty"},
		"a missing message": {func(c *model.Collector) { c.Response.Protobuf.Message = "metrics.v1.Other" }, `response.protobuf: message "metrics.v1.Other" is not in proto_files`},
		"a regex transform": {func(c *model.Collector) {
			c.Transform.Type = "regex"
			c.Metrics = []model.MetricRule{{Name: "v", Expression: "(.*)"}}
		}, "decodes with protobuf, whose document is JSON-like, and its transform is regex"},
	} {
		t.Run(name, func(t *testing.T) {
			c := protobufTestCollector(t)
			test.edit(&c)
			if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
	c := protobufTestCollector(t)
	config := &model.Config{Collectors: []model.Collector{c}}
	if err := Validate(config); err != nil {
		t.Fatal(err)
	}
	if named := namedDescriptorFiles(config); len(named) != 1 || named[0] != c.Response.Protobuf.ProtoFiles[0] {
		t.Fatalf("the watch looks at %v, want the decoder's .proto file", named)
	}
}
