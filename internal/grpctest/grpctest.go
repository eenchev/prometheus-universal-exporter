//go:build !select_request_types || request_type_grpc

package grpctest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	reflectionv1alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Service is the queue service's full name, and the files that define it.
const (
	Service   = "acme.queue.v1.QueueService"
	QueueFile = "acme/queue/v1/queue.proto"
	ShardFile = "acme/queue/v1/shard.proto"
)

// Sources are the .proto files of the queue service, by the name they are
// imported by. queue.proto imports shard.proto by its path from the root,
// so compiling it needs that root as an import path, and the well-known
// Timestamp.
var Sources = map[string]string{
	QueueFile: `syntax = "proto3";

package acme.queue.v1;

import "google/protobuf/timestamp.proto";
import "acme/queue/v1/shard.proto";

service QueueService {
  rpc GetStats(GetStatsRequest) returns (GetStatsResponse);
  rpc Watch(GetStatsRequest) returns (stream GetStatsResponse);
  rpc Push(stream GetStatsRequest) returns (GetStatsResponse);
  rpc Sync(stream GetStatsRequest) returns (stream GetStatsResponse);
}

enum State {
  STATE_UNSPECIFIED = 0;
  STATE_OK = 1;
  STATE_DEGRADED = 2;
}

message GetStatsRequest {
  string queue = 1;
  bool include_shards = 2;
  int32 limit = 3;
}

message GetStatsResponse {
  repeated Shard shards = 1;
  int64 total = 2;
  State state = 3;
  google.protobuf.Timestamp updated = 4;
  uint32 empty_count = 5;
}
`,
	ShardFile: `syntax = "proto3";

package acme.queue.v1;

message Shard {
  string id = 1;
  int64 depth = 2;
}
`,
}

var (
	compileOnce sync.Once
	compiled    *protoregistry.Files
	compileErr  error
)

// Files compiles Sources, once.
func Files(t testing.TB) *protoregistry.Files {
	t.Helper()
	compileOnce.Do(func() {
		compiler := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: protocompile.SourceAccessorFromMap(Sources),
		})}
		result, err := compiler.Compile(context.Background(), QueueFile)
		if err != nil {
			compileErr = err
			return
		}
		files := new(protoregistry.Files)
		var register func(f protoreflect.FileDescriptor) error
		register = func(f protoreflect.FileDescriptor) error {
			if _, err := files.FindFileByPath(f.Path()); err == nil {
				return nil
			}
			for i := 0; i < f.Imports().Len(); i++ {
				if err := register(f.Imports().Get(i).FileDescriptor); err != nil {
					return err
				}
			}
			return files.RegisterFile(f)
		}
		compileErr = register(result[0])
		compiled = files
	})
	if compileErr != nil {
		t.Fatal(compileErr)
	}
	return compiled
}

// WriteSources writes Sources under dir, each at its import path, and
// returns the path of queue.proto.
func WriteSources(t testing.TB, dir string) string {
	t.Helper()
	for name, source := range Sources {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, filepath.FromSlash(QueueFile))
}

// WriteProtoset writes the queue service's FileDescriptorSet to path, with
// its imports, as protoc --include_imports does, the well-known Timestamp
// left out when withWellKnown is false.
func WriteProtoset(t testing.TB, path string, withWellKnown bool) string {
	t.Helper()
	set := &descriptorpb.FileDescriptorSet{}
	Files(t).RangeFiles(func(f protoreflect.FileDescriptor) bool {
		if withWellKnown || !strings.HasPrefix(f.Path(), "google/protobuf/") {
			set.File = append(set.File, protodesc.ToFileDescriptorProto(f))
		}
		return true
	})
	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Call is one call the server answered.
type Call struct {
	Method   string
	Request  string
	Metadata metadata.MD
	// Deadline is how long the call had left when it arrived, zero when it
	// had no deadline.
	Deadline time.Duration
}

// Answer answers a call to method, whose request is JSON, with a JSON
// answer or an error, such as a status.Error.
type Answer func(ctx context.Context, method, request string) (string, error)

// Options configure a Server.
type Options struct {
	// Reflection is "v1", "v1alpha", "both" or "" for none.
	Reflection string
	// TLS serves over TLS with a certificate for 127.0.0.1 and queue.test.
	TLS bool
	// Answer answers the queue service's unary calls; nil answers {}.
	Answer Answer
}

// Server is a running test server.
type Server struct {
	// Addr is host:port.
	Addr string
	// CAFile is set with TLS: the CA a client trusts the server with.
	CAFile string
	Health *health.Server
	// ReflectionStreams counts the reflection streams clients opened.
	ReflectionStreams atomic.Int64
	grpc              *grpc.Server

	mu    sync.Mutex
	calls []Call
}

// Calls returns the calls answered so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Stop stops the server at once.
func (s *Server) Stop() { s.grpc.Stop() }

// Start runs a server until the test ends.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	files := Files(t)
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Addr: listener.Addr().String(), Health: health.NewServer()}
	var serverOpts []grpc.ServerOption
	if opts.TLS {
		cert, caFile := certificate(t)
		s.CAFile = caFile
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})))
	}
	serverOpts = append(serverOpts, grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if strings.HasSuffix(info.FullMethod, "/ServerReflectionInfo") {
			s.ReflectionStreams.Add(1)
		}
		return handler(srv, stream)
	}))
	s.grpc = grpc.NewServer(serverOpts...)
	healthpb.RegisterHealthServer(s.grpc, s.Health)
	s.grpc.RegisterService(queueService(t, files, s, opts.Answer), struct{}{})
	reflectionOpts := reflection.ServerOptions{Services: s.grpc, DescriptorResolver: files}
	switch opts.Reflection {
	case "v1":
		reflectionv1.RegisterServerReflectionServer(s.grpc, reflection.NewServerV1(reflectionOpts))
	case "v1alpha":
		reflectionv1alpha.RegisterServerReflectionServer(s.grpc, reflection.NewServer(reflectionOpts)) //nolint:staticcheck // the fallback is what is tested
	case "both":
		reflectionv1.RegisterServerReflectionServer(s.grpc, reflection.NewServerV1(reflectionOpts))
		reflectionv1alpha.RegisterServerReflectionServer(s.grpc, reflection.NewServer(reflectionOpts)) //nolint:staticcheck // the fallback is what is tested
	}
	go func() { _ = s.grpc.Serve(listener) }()
	t.Cleanup(s.grpc.Stop)
	return s
}

// queueService answers the queue service's unary methods with answer, and
// its streaming ones not at all.
func queueService(t testing.TB, files *protoregistry.Files, s *Server, answer Answer) *grpc.ServiceDesc {
	t.Helper()
	d, err := files.FindDescriptorByName(Service)
	if err != nil {
		t.Fatal(err)
	}
	service := d.(protoreflect.ServiceDescriptor)
	desc := &grpc.ServiceDesc{ServiceName: Service, HandlerType: (*any)(nil), Metadata: QueueFile}
	types := dynamicpb.NewTypes(files)
	for i := 0; i < service.Methods().Len(); i++ {
		method := service.Methods().Get(i)
		if method.IsStreamingClient() || method.IsStreamingServer() {
			desc.Streams = append(desc.Streams, grpc.StreamDesc{
				StreamName:    string(method.Name()),
				Handler:       func(any, grpc.ServerStream) error { return io.EOF },
				ServerStreams: method.IsStreamingServer(),
				ClientStreams: method.IsStreamingClient(),
			})
			continue
		}
		desc.Methods = append(desc.Methods, grpc.MethodDesc{
			MethodName: string(method.Name()),
			Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				in := dynamicpb.NewMessage(method.Input())
				if err := dec(in); err != nil {
					return nil, err
				}
				request, err := protojson.MarshalOptions{UseProtoNames: true, Resolver: types}.Marshal(in)
				if err != nil {
					return nil, err
				}
				var compact bytes.Buffer
				if err := json.Compact(&compact, request); err != nil {
					return nil, err
				}
				call := Call{Method: string(method.Name()), Request: compact.String()}
				call.Metadata, _ = metadata.FromIncomingContext(ctx)
				if deadline, ok := ctx.Deadline(); ok {
					call.Deadline = time.Until(deadline)
				}
				s.mu.Lock()
				s.calls = append(s.calls, call)
				s.mu.Unlock()
				text := "{}"
				if answer != nil {
					if text, err = answer(ctx, call.Method, call.Request); err != nil {
						return nil, err
					}
				}
				out := dynamicpb.NewMessage(method.Output())
				if err := (protojson.UnmarshalOptions{Resolver: types}).Unmarshal([]byte(text), out); err != nil {
					return nil, err
				}
				return out, nil
			},
		})
	}
	return desc
}

// certificate makes a self-signed certificate for 127.0.0.1 and queue.test,
// and writes it where a client can trust it.
func certificate(t testing.TB) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "queue.test"},
		DNSNames:              []string{"queue.test"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, caFile
}
