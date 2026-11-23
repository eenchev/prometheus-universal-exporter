//go:build select_request_types && !request_type_grpc

package fetch

import (
	"errors"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// ProtobufDecoder says whether this build has the protobuf decoder, which
// comes with the grpc request type (protobufdecoder.go).
const ProtobufDecoder = false

var errNoProtobufDecoder = errors.New("this build has no protobuf decoder, which comes with the grpc request type; build with grpc among REQUEST_TYPES")

// CheckProtobufDecoder refuses the decoder this build is without.
func CheckProtobufDecoder(*model.Collector) error { return errNoProtobufDecoder }

// DecodeProtobuf refuses the decoder this build is without.
func DecodeProtobuf([]byte, *model.Collector) ([]byte, error) { return nil, errNoProtobufDecoder }

func protobufDecoderNamedFiles(*model.Collector) []string { return nil }

func protobufDecoderReadFiles(*model.Collector) ([]string, string) { return nil, "" }
