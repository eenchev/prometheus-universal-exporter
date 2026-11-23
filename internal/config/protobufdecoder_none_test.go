//go:build select_request_types && !request_type_grpc && request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A build without the grpc request type has no protobuf decoder, and says
// so when a configuration asks for it.
func TestABuildWithoutGRPCRefusesTheProtobufDecoder(t *testing.T) {
	c := model.Collector{
		Name:      "report",
		Request:   model.RequestConfig{Type: "http"},
		Decoder:   model.DecoderConfig{Type: "protobuf"},
		Response:  model.ResponseConfig{Protobuf: model.ProtobufConfig{Message: "metrics.v1.Report", ProtoFiles: []string{"report.proto"}}},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics:   []model.MetricRule{{Name: "total", Expression: ".total"}},
	}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err == nil || !strings.Contains(err.Error(), "this build has no protobuf decoder, which comes with the grpc request type") {
		t.Fatalf("err=%v", err)
	}
}
