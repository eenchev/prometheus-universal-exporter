//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// rawCodec has a test server send an answer as bytes made in advance, so
// that making it costs the test nothing while the exporter reads it.
type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }

func (rawCodec) Marshal(v any) (mem.BufferSlice, error) {
	return mem.BufferSlice{mem.SliceBuffer(v.([]byte))}, nil
}

func (rawCodec) Unmarshal(data mem.BufferSlice, v any) error {
	*v.(*[]byte) = data.Materialize()
	return nil
}

// startRawGRPC serves the queue service's GetStats with answer, as it is on
// the wire.
func startRawGRPC(t *testing.T, answer []byte) string {
	t.Helper()
	listener := grpctest.Listen(t)
	server := grpc.NewServer(grpc.ForceServerCodecV2(rawCodec{}))
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: grpctest.Service,
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "GetStats",
			Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				var in []byte
				if err := dec(&in); err != nil {
					return nil, err
				}
				return answer, nil
			},
		}},
	}, struct{}{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

// An answer of four million empty shards is 8 MB on the wire, within a
// 10 MiB limit, and 88 MB as JSON, each shard written with its zero values.
// It is refused from the count of its JSON made as it is read, before it is
// decoded or rendered: decoding it alone took gigabytes.
func TestGRPCAnAnswerWhoseJSONIsOverTheLimitIsRefusedBeforeItIsDecoded(t *testing.T) {
	const shards = 4 << 20
	answer := make([]byte, 0, 2*shards)
	for range shards {
		answer = append(answer, 0x0a, 0x00) // shards = 1, an empty Shard
	}
	addr := startRawGRPC(t, answer)
	dir := t.TempDir()
	c := grpcCollector()
	c.Request.Descriptors, c.Request.ProtoFiles, c.Request.ProtoImportPaths = "proto", []string{grpctest.WriteSources(t, dir)}, []string{dir}
	c.Request.MaxResponseBytes = 10 << 20
	checked := validGRPC(t, c)

	var (
		err  error
		took time.Duration
	)
	// Reading the answer, on both ends, is some tens of megabytes; the
	// bound is loose, for whatever else the process does meanwhile.
	allocated := alloctest.BytesAtMost(1, 100<<20, func() {
		start := time.Now()
		_, err = FetchCollector(context.Background(), addr, checked, RequestOverrides{Timeout: time.Minute}, nil)
		took = time.Since(start)
	})
	if allocated > 100<<20 {
		t.Fatalf("refusing the answer allocated %d MiB", allocated>>20)
	}
	if err == nil || !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "bytes as JSON, over the response limit of 10485760") {
		t.Fatalf("%v", err)
	}
	if code, _ := GRPCStatusCode(checked, err); code != 0 {
		t.Fatalf("code %d, want 0: the call was answered OK", code)
	}
	if took > 20*time.Second {
		t.Fatalf("refusing the answer took %s", took)
	}
}

// What the count leaves short of the JSON, floating-point numbers and the
// well-known types written at their shortest, is caught when the answer is
// rendered: an answer the count lets through is still refused when its JSON
// is over the limit.
func TestGRPCAnAnswerTheCountLetsThroughIsMeasuredAsJSON(t *testing.T) {
	durations := strings.TrimSuffix(strings.Repeat(`{"@type": "type.googleapis.com/google.protobuf.Duration", "value": "1.500s"},`, 20), ",")
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(context.Context, string, string) (string, error) {
		return `{"details": [` + durations + `]}`, nil
	}})
	c := grpcCollector()
	c.Request.RPC = grpctest.Service + "/Explain"
	checked := validGRPC(t, c)
	resp, err := FetchCollector(context.Background(), server.Addr, checked, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Each Duration counts as "0s", and is "1.500s" with "value": written.
	method, err := staticMethod(&model.Collector{Request: model.RequestConfig{Type: RequestTypeGRPC, RPC: c.Request.RPC, Descriptors: "protoset", ProtosetFile: grpctest.WriteProtoset(t, filepath.Join(t.TempDir(), "queue.pb"), true)}})
	if err != nil {
		t.Fatal(err)
	}
	out := dynamicpb.NewMessage(method.desc.Output())
	if err := (protojson.UnmarshalOptions{Resolver: answerResolver(method.types)}).Unmarshal(resp.Body, out); err != nil {
		t.Fatal(err)
	}
	wire, err := proto.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	count := jsonCount{limit: math.MaxInt64, resolver: answerResolver(method.types)}
	count.message(wire, out.Descriptor(), 0)
	if count.bad || count.n >= int64(len(resp.Body)) {
		t.Fatalf("count %d of %d bytes", count.n, len(resp.Body))
	}
	c.Request.MaxResponseBytes = model.ByteSize((count.n + int64(len(resp.Body))) / 2)
	checked = validGRPC(t, c)
	_, err = FetchCollector(context.Background(), server.Addr, checked, RequestOverrides{}, nil)
	if err == nil || !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), fmt.Sprintf("the answer is %d bytes as JSON", len(resp.Body))) {
		t.Fatalf("%v", err)
	}
}

// countSources is a schema with a field of every kind, for comparing the
// count with the JSON protojson writes.
var countSources = map[string]string{
	"t/all.proto": `syntax = "proto3";
package t;
import "google/protobuf/any.proto";
import "google/protobuf/timestamp.proto";
import "google/protobuf/duration.proto";
import "google/protobuf/struct.proto";
import "google/protobuf/wrappers.proto";
enum Color {
  COLOR_UNSPECIFIED = 0;
  RED = 1;
}
message Leaf {
  string s = 1;
  int64 i = 2;
  bool b = 3;
  Color c = 4;
}
message All {
  int32 i32 = 1; sint32 s32 = 2; uint32 u32 = 3; int64 i64 = 4; sint64 s64 = 5; uint64 u64 = 6;
  fixed32 f32 = 7; sfixed32 sf32 = 8; fixed64 f64 = 9; sfixed64 sf64 = 10; float fl = 11; double db = 12;
  bool b = 13; string s = 14; bytes by = 15; Color c = 16;
  Leaf leaf = 17;
  repeated Leaf leaves = 18;
  repeated int32 packed = 19;
  repeated string strs = 20;
  map<string, Leaf> by_name = 21;
  map<int32, string> by_num = 22;
  map<bool, Color> by_bool = 23;
  oneof pick { string ps = 24; Leaf pl = 25; }
  optional int32 opt = 26;
  google.protobuf.Any any = 27;
  repeated google.protobuf.Any anys = 28;
  google.protobuf.Timestamp ts = 29;
  google.protobuf.Duration du = 30;
  google.protobuf.Struct st = 31;
  google.protobuf.Int64Value w = 32;
  repeated Color colors = 33;
  map<string, int64> m64 = 34;
  repeated bytes bys = 35;
  repeated double dbs = 36;
  map<uint64, Leaf> by_big = 37;
}
`,
	"t/old.proto": `syntax = "proto2";
package t;
message Old {
  optional int32 a = 1 [default = 5];
  optional group G = 2 {
    optional string x = 3;
  }
  repeated int32 r = 4;
  repeated int32 p = 5 [packed = true];
  optional Old next = 6;
}
`,
}

// The count is the JSON's size where it counts exactly, and never more than
// it: an answer within the limit is never refused by the count.
func TestGRPCAnswerJSONCount(t *testing.T) {
	compiler := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
		Accessor: protocompile.SourceAccessorFromMap(countSources),
	})}
	files, err := compiler.Compile(context.Background(), "t/all.proto", "t/old.proto")
	if err != nil {
		t.Fatal(err)
	}
	registry := new(protoregistry.Files)
	var register func(f protoreflect.FileDescriptor)
	register = func(f protoreflect.FileDescriptor) {
		if _, err := registry.FindFileByPath(f.Path()); err == nil {
			return
		}
		for i := 0; i < f.Imports().Len(); i++ {
			register(f.Imports().Get(i).FileDescriptor)
		}
		if err := registry.RegisterFile(f); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		register(f)
	}
	types := dynamicpb.NewTypes(registry)
	resolver := answerResolver(types)
	for _, tc := range []struct {
		message, json string
		exact         bool
	}{
		{"t.All", `{}`, true},
		{"t.Old", `{}`, true},
		{"t.All", `{"i32": -7, "s32": -300, "u32": 4000000000, "i64": "-9223372036854775808", "s64": "-5", "u64": "18446744073709551615",
			"f32": 9, "sf32": -2147483648, "f64": "123", "sf64": "-1", "b": true, "s": "a\"b\\c\n\u0001\u001fé\t", "by": "AAECAwQ=", "c": "RED",
			"leaf": {"s": "x", "i": "1", "b": false, "c": 7}, "leaves": [{}, {"s": "y"}, {}], "packed": [1, -1, 0, 2147483647], "strs": ["", "q"],
			"by_name": {"": {}, "k\n": {"s": "v"}}, "by_num": {"-3": "", "0": "z"}, "by_bool": {"true": "RED", "false": "COLOR_UNSPECIFIED"},
			"pl": {"s": "o"}, "opt": 0, "colors": ["RED", 5], "m64": {"a": "-12"}, "bys": ["", "AQ=="], "by_big": {"18446744073709551615": {}}}`, true},
		{"t.All", `{"ps": "chosen", "by_bool": {"false": "RED"}}`, true},
		{"t.Old", `{"a": 1, "G": {"x": "y"}, "r": [1, 2], "p": [3, -4], "next": {"next": {}}}`, true},
		{"t.All", `{"fl": 1.5, "db": -1e-300, "dbs": [0, 3.25, "NaN"], "any": {"@type": "type.googleapis.com/t.Leaf", "s": "in"},
			"anys": [{"@type": "type.googleapis.com/google.protobuf.Duration", "value": "1.500s"}, {"@type": "type.googleapis.com/t.All", "leaves": [{}, {}]}],
			"ts": "2026-10-22T10:00:00.123Z", "du": "-3.5s", "st": {"a": [1, "b", null, {"c": true}]}, "w": "77"}`, false},
	} {
		md, err := types.FindMessageByName(protoreflect.FullName(tc.message))
		if err != nil {
			t.Fatal(err)
		}
		in := dynamicpb.NewMessage(md.Descriptor())
		if err := (protojson.UnmarshalOptions{Resolver: resolver}).Unmarshal([]byte(tc.json), in); err != nil {
			t.Fatalf("%s: %v", tc.json, err)
		}
		wire, err := proto.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		out := dynamicpb.NewMessage(md.Descriptor())
		if err := proto.Unmarshal(wire, out); err != nil {
			t.Fatal(err)
		}
		rendered, err := renderAnswer(out, types)
		if err != nil {
			t.Fatal(err)
		}
		count := jsonCount{limit: math.MaxInt64, resolver: resolver}
		count.message(wire, md.Descriptor(), 0)
		if count.bad || count.n > int64(len(rendered)) || tc.exact && count.n != int64(len(rendered)) {
			t.Errorf("%s: counted %d (bad %v), the JSON is %d bytes:\n%s", tc.json, count.n, count.bad, len(rendered), rendered)
		}
		// Counting stops once it is over the limit.
		if len(rendered) > 10 {
			count = jsonCount{limit: 10, resolver: resolver}
			if count.message(wire, md.Descriptor(), 0) || count.n <= 10 || count.n > int64(len(rendered)) {
				t.Errorf("%s: with a limit of 10 the count went on, or stopped at %d", tc.json, count.n)
			}
		}
	}

	// A message that does not parse is left to the decoder.
	md, _ := types.FindMessageByName("t.All")
	for _, broken := range [][]byte{{0x0a, 0x05, 0x01}, {0x00}, {0xff}} {
		count := jsonCount{limit: math.MaxInt64, resolver: resolver}
		if count.message(broken, md.Descriptor(), 0) || !count.bad {
			t.Errorf("%x: counted %d, not bad", broken, count.n)
		}
	}
}
