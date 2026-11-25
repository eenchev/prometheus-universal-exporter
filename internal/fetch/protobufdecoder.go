//go:build !select_request_types || request_type_grpc

package fetch

import (
	"fmt"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The protobuf decoder reads a body as one protobuf message and hands on
// the JSON a grpc answer is written as (grpcJSON), so the two read alike:
// fields by their names in the .proto file, unset fields with their zero
// values, and 64-bit integers as strings. The message's type comes from the
// files response.protobuf names, read and kept as a grpc request's
// descriptor files are (descriptorFiles). So the decoder is in the builds
// that have the grpc request type and in no other, since the modules that
// read descriptors are linked for that type alone.

// ProtobufDecoder says whether this build has the protobuf decoder.
const ProtobufDecoder = true

// protobufMessage is the type of a collector's response.protobuf.message.
func protobufMessage(p model.ProtobufConfig) (protoreflect.MessageDescriptor, error) {
	var (
		files  *protoregistry.Files
		source string
		err    error
	)
	if p.ProtosetFile != "" {
		files, err = descriptorFiles.protoset(p.ProtosetFile)
		source = "protoset_file " + p.ProtosetFile
	} else {
		files, err = descriptorFiles.proto(p.ProtoFiles, p.ProtoImportPaths)
		source = "proto_files"
	}
	if err != nil {
		return nil, err
	}
	found, err := files.FindDescriptorByName(protoreflect.FullName(p.Message))
	if err != nil {
		return nil, fmt.Errorf("message %q is not in %s; write its full name, package and all, such as metrics.v1.Report", p.Message, source)
	}
	message, is := found.(protoreflect.MessageDescriptor)
	if !is {
		return nil, fmt.Errorf("%q in %s is not a message", p.Message, source)
	}
	return message, nil
}

// CheckProtobufDecoder reads the files of a collector's response.protobuf
// and finds its message, so a file that does not compile or a message that
// is not there fails the configuration rather than every scrape.
func CheckProtobufDecoder(c *model.Collector) error {
	_, err := protobufMessage(c.Response.Protobuf)
	return err
}

// DecodeProtobuf reads body as the message of the collector's
// response.protobuf and returns it as JSON.
func DecodeProtobuf(body []byte, c *model.Collector) ([]byte, error) {
	desc, err := protobufMessage(c.Response.Protobuf)
	if err != nil {
		return nil, err
	}
	message := dynamicpb.NewMessage(desc)
	if err := proto.Unmarshal(body, message); err != nil {
		return nil, fmt.Errorf("the body is not a %s message: %w", desc.FullName(), err)
	}
	return grpcJSON.Marshal(message)
}

// protobufDecoderNamedFiles are the files a collector's response.protobuf
// names, which checking it reads when the configuration loads.
func protobufDecoderNamedFiles(c *model.Collector) []string {
	if c.Decoder.Type != "protobuf" {
		return nil
	}
	p := c.Response.Protobuf
	var files []string
	if p.ProtosetFile != "" {
		files = append(files, p.ProtosetFile)
	}
	return append(files, p.ProtoFiles...)
}

// protobufDecoderReadFiles is what compiling the .proto files of a
// collector's response.protobuf looked at (fileSets.protoLooked).
func protobufDecoderReadFiles(c *model.Collector) (paths []string, read string) {
	p := c.Response.Protobuf
	if c.Decoder.Type != "protobuf" || p.ProtosetFile != "" || len(p.ProtoFiles) == 0 {
		return nil, ""
	}
	return descriptorFiles.protoLooked(p.ProtoFiles, p.ProtoImportPaths)
}
