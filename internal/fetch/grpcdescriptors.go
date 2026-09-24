//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/health/grpc_health_v1" // the health service's types, built in
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	reflectionv1alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// A grpc call needs its method's input and output message types, to encode
// the JSON message and decode the answer. They are built at run time
// (dynamicpb) from descriptors, which request.descriptors says where to find:
//
//   - reflection asks the target's reflection service, grpc.reflection.v1,
//     or v1alpha when the server has only that, for the file that defines
//     the service and the files it imports. The answer is kept per
//     connection and service for reflectionTTL, so a scrape costs one call
//     rather than two, and asked again sooner when the method is not found
//     or its answer does not decode, as after the server was upgraded.
//   - protoset reads a FileDescriptorSet from protoset_file.
//   - proto compiles the .proto files of proto_files, resolving imports in
//     proto_import_paths, with the well-known types built in.
//
// protoset and proto are read when the configuration loads, so a missing
// method or a message that does not fit fails the configuration, and read
// again at a call when one of their files has changed. The health service's
// types are compiled into the exporter, so it needs none of the three.

// reflectionTTL is how long a reflection answer is used before the server is
// asked again.
const reflectionTTL = 10 * time.Minute

// grpcMethod is a method whose types are known.
type grpcMethod struct {
	desc  protoreflect.MethodDescriptor
	types *dynamicpb.Types
}

// path is the method as gRPC names it on the wire: /package.Service/Method.
func (m grpcMethod) path() string {
	return "/" + string(m.desc.Parent().FullName()) + "/" + string(m.desc.Name())
}

func (m grpcMethod) input() string { return string(m.desc.Input().FullName()) }

// encode reads a JSON message into the input type. An unknown field is an
// error, so a misspelt field fails rather than being sent as nothing.
func (m grpcMethod) encode(message string) (*dynamicpb.Message, error) {
	in := dynamicpb.NewMessage(m.desc.Input())
	if err := (protojson.UnmarshalOptions{Resolver: m.types}).Unmarshal([]byte(message), in); err != nil {
		// The protobuf runtime writes its messages with a no-break space
		// after "proto:", to keep programs from matching them; a person
		// reads them.
		text := strings.ReplaceAll(err.Error(), "\u00a0", " ")
		return nil, errors.New(strings.TrimSpace(strings.TrimPrefix(text, "proto:")))
	}
	return in, nil
}

func (m grpcMethod) checkMessage(message string) error {
	_, err := m.encode(message)
	return err
}

// findMethod looks rpc, package.Service/Method, up among files; source says
// where they came from, for the errors.
func findMethod(files *protoregistry.Files, rpc, source string) (grpcMethod, error) {
	serviceName, methodName, _ := strings.Cut(rpc, "/")
	d, err := files.FindDescriptorByName(protoreflect.FullName(serviceName))
	if err != nil {
		return grpcMethod{}, fmt.Errorf("the service %s is not in %s", serviceName, source)
	}
	service, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return grpcMethod{}, fmt.Errorf("%s in %s is not a service", serviceName, source)
	}
	method := service.Methods().ByName(protoreflect.Name(methodName))
	if method == nil {
		var names []string
		for i := 0; i < service.Methods().Len(); i++ {
			names = append(names, string(service.Methods().Get(i).Name()))
		}
		return grpcMethod{}, fmt.Errorf("the service %s in %s has no method %s; it has %s", serviceName, source, methodName, strings.Join(names, ", "))
	}
	switch {
	case method.IsStreamingClient() && method.IsStreamingServer():
		return grpcMethod{}, fmt.Errorf("%s is a bidirectional streaming method; only unary methods can be called", rpc)
	case method.IsStreamingClient():
		return grpcMethod{}, fmt.Errorf("%s is a client streaming method; only unary methods can be called", rpc)
	case method.IsStreamingServer():
		return grpcMethod{}, fmt.Errorf("%s is a server streaming method; only unary methods can be called", rpc)
	}
	return grpcMethod{desc: method, types: dynamicpb.NewTypes(files)}, nil
}

// staticMethod is the method of a collector whose descriptors are read
// without asking the server: protoset, proto, or the built-in health
// service's when descriptors is left out.
func staticMethod(c *model.Collector) (grpcMethod, error) {
	switch c.Request.Descriptors {
	case "":
		return findMethod(protoregistry.GlobalFiles, c.Request.RPC, "the built-in health service")
	case descriptorsProtoset:
		files, err := descriptorFiles.protoset(c.Request.ProtosetFile)
		if err != nil {
			return grpcMethod{}, err
		}
		return findMethod(files, c.Request.RPC, "protoset_file "+c.Request.ProtosetFile)
	case descriptorsProto:
		files, err := descriptorFiles.proto(c.Request.ProtoFiles, c.Request.ProtoImportPaths)
		if err != nil {
			return grpcMethod{}, err
		}
		return findMethod(files, c.Request.RPC, "proto_files")
	}
	return grpcMethod{}, fmt.Errorf("descriptors %q are not read from files", c.Request.Descriptors)
}

// fileSets keeps what protoset and proto read, by the files they read, and
// reads them again when one changed. Each set has a lock of its own, so a
// slow compile of one collector's files keeps no other collector waiting.
type fileSets struct {
	mu    sync.Mutex
	slots map[string]*fileSetSlot
}

// fileSetSlot is one set's lock and what it last read.
type fileSetSlot struct {
	mu    sync.Mutex
	entry *fileSetEntry
}

type fileSetEntry struct {
	// paths are every file the set was read from, and stamp how they were.
	paths []string
	stamp string
	files *protoregistry.Files
}

var descriptorFiles = &fileSets{slots: map[string]*fileSetSlot{}}

// filesStamp describes files as they are on disk now.
func filesStamp(paths []string) string {
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(path)
		if st, err := os.Stat(path); err == nil {
			b.WriteString(":" + strconv.FormatInt(st.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(st.Size(), 10))
		} else {
			b.WriteString(":missing")
		}
		b.WriteByte('|')
	}
	return b.String()
}

// get returns the set kept under key when its files are as they were, and
// reads it with read otherwise.
func (s *fileSets) get(key string, read func() (*protoregistry.Files, []string, error)) (*protoregistry.Files, error) {
	s.mu.Lock()
	slot := s.slots[key]
	if slot == nil {
		slot = &fileSetSlot{}
		s.slots[key] = slot
	}
	s.mu.Unlock()
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if entry := slot.entry; entry != nil && entry.stamp == filesStamp(entry.paths) {
		return entry.files, nil
	}
	files, paths, err := read()
	if err != nil {
		slot.entry = nil
		return nil, err
	}
	slot.entry = &fileSetEntry{paths: paths, stamp: filesStamp(paths), files: files}
	return files, nil
}

// protoset reads a FileDescriptorSet.
func (s *fileSets) protoset(path string) (*protoregistry.Files, error) {
	return s.get("protoset\x00"+path, func() (*protoregistry.Files, []string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("reading protoset_file: %w", err)
		}
		set := &descriptorpb.FileDescriptorSet{}
		if err := proto.Unmarshal(raw, set); err != nil {
			return nil, nil, fmt.Errorf("protoset_file %s is not a FileDescriptorSet: %w", path, err)
		}
		files, err := registryOf(set.GetFile())
		if err != nil {
			return nil, nil, fmt.Errorf("protoset_file %s: %w; write it with the files it imports, as protoc --include_imports or buf build does", path, err)
		}
		return files, []string{path}, nil
	})
}

// proto compiles .proto files. Unless import paths are given, each file's
// directory is one, and the file is compiled by its name in it; with them,
// each file must be under one, and is compiled by its path from it, as
// protoc -I does.
func (s *fileSets) proto(paths, importPaths []string) (*protoregistry.Files, error) {
	key := "proto\x00" + strings.Join(paths, "\x00") + "\x01" + strings.Join(importPaths, "\x00")
	return s.get(key, func() (*protoregistry.Files, []string, error) {
		names, dirs, err := protoNames(paths, importPaths)
		if err != nil {
			return nil, nil, err
		}
		var mu sync.Mutex
		read := append([]string(nil), paths...)
		resolver := &protocompile.SourceResolver{
			ImportPaths: dirs,
			Accessor: func(path string) (io.ReadCloser, error) {
				f, err := os.Open(path)
				if err == nil {
					mu.Lock()
					if !slices.Contains(read, path) {
						read = append(read, path)
					}
					mu.Unlock()
				}
				return f, err
			},
		}
		compiler := protocompile.Compiler{Resolver: protocompile.WithStandardImports(resolver)}
		compiled, err := compiler.Compile(context.Background(), names...)
		if err != nil {
			return nil, nil, fmt.Errorf("compiling proto_files: %w", err)
		}
		files := new(protoregistry.Files)
		for _, f := range compiled {
			if err := registerFileTree(files, f); err != nil {
				return nil, nil, err
			}
		}
		mu.Lock()
		defer mu.Unlock()
		return files, read, nil
	})
}

// protoNames gives each .proto file the name it is compiled by, and the
// import paths the compiler resolves names in.
func protoNames(paths, importPaths []string) (names, dirs []string, err error) {
	if len(importPaths) == 0 {
		for _, path := range paths {
			dir := filepath.Dir(path)
			if !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
			names = append(names, filepath.Base(path))
		}
		return names, dirs, nil
	}
	for _, path := range paths {
		name := ""
		for _, dir := range importPaths {
			rel, err := filepath.Rel(dir, path)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
				name = filepath.ToSlash(rel)
				break
			}
		}
		if name == "" {
			return nil, nil, fmt.Errorf("proto_files %s is not under any of proto_import_paths (%s); add its directory, or the directory its package path starts in", path, strings.Join(importPaths, ", "))
		}
		names = append(names, name)
	}
	return names, importPaths, nil
}

// registerFileTree registers a file and the files it imports, each once.
func registerFileTree(files *protoregistry.Files, f protoreflect.FileDescriptor) error {
	if _, err := files.FindFileByPath(f.Path()); err == nil {
		return nil
	}
	imports := f.Imports()
	for i := 0; i < imports.Len(); i++ {
		if err := registerFileTree(files, imports.Get(i).FileDescriptor); err != nil {
			return err
		}
	}
	return files.RegisterFile(f)
}

// registryOf builds the files of a descriptor set, or of a reflection answer.
// An import the set does not carry is taken from the types built into the
// exporter when it has them, as it has the well-known types; otherwise it is
// an error naming it.
func registryOf(protos []*descriptorpb.FileDescriptorProto) (*protoregistry.Files, error) {
	byName := make(map[string]*descriptorpb.FileDescriptorProto, len(protos))
	for _, fd := range protos {
		byName[fd.GetName()] = fd
	}
	files := new(protoregistry.Files)
	adding := map[string]bool{}
	var add func(name string) error
	add = func(name string) error {
		if _, err := files.FindFileByPath(name); err == nil {
			return nil
		}
		fd, ok := byName[name]
		if !ok {
			builtIn, err := protoregistry.GlobalFiles.FindFileByPath(name)
			if err != nil {
				return fmt.Errorf("the import %s is missing", name)
			}
			return registerFileTree(files, builtIn)
		}
		if adding[name] {
			return fmt.Errorf("%s imports itself", name)
		}
		adding[name] = true
		for _, dep := range fd.GetDependency() {
			if err := add(dep); err != nil {
				return err
			}
		}
		file, err := protodesc.NewFile(fd, files)
		if err != nil {
			return err
		}
		return files.RegisterFile(file)
	}
	for _, fd := range protos {
		if err := add(fd.GetName()); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// reflected keeps reflection answers, by connection and service. Probes
// that miss together share one question to the server (asking).
type reflected struct {
	mu      sync.Mutex
	entries map[reflectedKey]*reflectedEntry
	asking  map[reflectedKey]*reflectionCall
}

// reflectionCall is a question to a reflection service in flight, which the
// probes that need the same answer wait for.
type reflectionCall struct {
	done  chan struct{}
	files *protoregistry.Files
	err   error
}

type reflectedKey struct {
	conn    grpcConnKey
	service string
}

type reflectedEntry struct {
	files   *protoregistry.Files
	fetched time.Time
}

var reflectionAnswers = &reflected{entries: map[reflectedKey]*reflectedEntry{}, asking: map[reflectedKey]*reflectionCall{}}

// files returns the service's files, asking the server when there is no
// answer younger than reflectionTTL.
func (r *reflected) files(ctx context.Context, conn *grpc.ClientConn, key reflectedKey, now time.Time) (*protoregistry.Files, error) {
	r.mu.Lock()
	r.sweepLocked(now)
	if entry := r.entries[key]; entry != nil {
		r.mu.Unlock()
		return entry.files, nil
	}
	if call := r.asking[key]; call != nil {
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.files, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &reflectionCall{done: make(chan struct{})}
	r.asking[key] = call
	r.mu.Unlock()
	call.files, call.err = askReflection(ctx, conn, key.service)
	r.mu.Lock()
	delete(r.asking, key)
	if call.err == nil {
		r.entries[key] = &reflectedEntry{files: call.files, fetched: now}
	}
	r.mu.Unlock()
	close(call.done)
	return call.files, call.err
}

// sweepLocked forgets the answers older than reflectionTTL.
func (r *reflected) sweepLocked(now time.Time) {
	for k, entry := range r.entries {
		if now.Sub(entry.fetched) > reflectionTTL {
			delete(r.entries, k)
		}
	}
}

// forget drops an answer, so the next call asks again.
func (r *reflected) forget(key reflectedKey) {
	r.mu.Lock()
	delete(r.entries, key)
	r.mu.Unlock()
}

// reflectionStream asks a reflection service for files, by the symbol they
// define or by name. v1 and v1alpha are the same protocol under two names.
type reflectionStream interface {
	ask(symbol, filename string) ([][]byte, error)
	close()
}

// askReflection asks the server's reflection service for the file that
// defines service and every file it imports.
func askReflection(ctx context.Context, conn *grpc.ClientConn, service string) (*protoregistry.Files, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := openReflection(ctx, conn)
	if err != nil {
		return nil, err
	}
	defer stream.close()
	answers, err := stream.ask(service, "")
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("the server's reflection service does not know the service %s: %w", service, err)
		}
		return nil, err
	}
	byName := map[string]*descriptorpb.FileDescriptorProto{}
	collect := func(raw [][]byte) error {
		for _, b := range raw {
			fd := &descriptorpb.FileDescriptorProto{}
			if err := proto.Unmarshal(b, fd); err != nil {
				return fmt.Errorf("the reflection service answered a file that does not decode: %w", err)
			}
			byName[fd.GetName()] = fd
		}
		return nil
	}
	if err := collect(answers); err != nil {
		return nil, err
	}
	// A server sends the imports it has not sent before on the stream;
	// anything still missing is asked for by name, unless the exporter
	// has it built in.
	for {
		var missing []string
		for _, fd := range byName {
			for _, dep := range fd.GetDependency() {
				if _, ok := byName[dep]; ok || slices.Contains(missing, dep) {
					continue
				}
				if _, err := protoregistry.GlobalFiles.FindFileByPath(dep); err == nil {
					continue
				}
				missing = append(missing, dep)
			}
		}
		if len(missing) == 0 {
			break
		}
		for _, name := range missing {
			answers, err := stream.ask("", name)
			if err != nil {
				return nil, fmt.Errorf("asking the reflection service for %s: %w", name, err)
			}
			if err := collect(answers); err != nil {
				return nil, err
			}
			if _, ok := byName[name]; !ok {
				return nil, fmt.Errorf("the reflection service did not send %s, which the service's files import", name)
			}
		}
	}
	protos := make([]*descriptorpb.FileDescriptorProto, 0, len(byName))
	for _, fd := range byName {
		protos = append(protos, fd)
	}
	files, err := registryOf(protos)
	if err != nil {
		return nil, fmt.Errorf("the reflection service's answer: %w", err)
	}
	return files, nil
}

// openReflection opens a reflection stream, v1, or v1alpha when the server
// does not implement v1.
func openReflection(ctx context.Context, conn *grpc.ClientConn) (reflectionStream, error) {
	v1, err := reflectionv1.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err == nil {
		s := &reflectionV1{stream: v1}
		// A server without v1 says so only at the first answer.
		_, askErr := s.ask("", "")
		if status.Code(askErr) != codes.Unimplemented {
			return s, nil
		}
		s.close()
	}
	if err != nil && status.Code(err) != codes.Unimplemented {
		return nil, err
	}
	alpha, err := reflectionv1alpha.NewServerReflectionClient(conn).ServerReflectionInfo(ctx) //nolint:staticcheck // v1alpha is the fallback for servers without v1
	if err != nil {
		return nil, reflectionUnavailable(err)
	}
	s := &reflectionV1alpha{stream: alpha}
	if _, err := s.ask("", ""); status.Code(err) == codes.Unimplemented {
		s.close()
		return nil, reflectionUnavailable(err)
	}
	return s, nil
}

// reflectionUnavailable says what to do about a server without reflection.
func reflectionUnavailable(err error) error {
	if status.Code(err) != codes.Unimplemented {
		return err
	}
	return fmt.Errorf("the target has no reflection service (grpc.reflection.v1 or v1alpha); register it on the server, or use descriptors: protoset or proto: %w", err)
}

// The probe opening a stream asks with neither symbol nor name, which a
// server answers with an error that is not UNIMPLEMENTED if it serves
// reflection at all; it is used to tell whether v1 is there.

type reflectionV1 struct {
	stream grpc.BidiStreamingClient[reflectionv1.ServerReflectionRequest, reflectionv1.ServerReflectionResponse]
}

func (s *reflectionV1) ask(symbol, filename string) ([][]byte, error) {
	req := &reflectionv1.ServerReflectionRequest{}
	switch {
	case symbol != "":
		req.MessageRequest = &reflectionv1.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol}
	case filename != "":
		req.MessageRequest = &reflectionv1.ServerReflectionRequest_FileByFilename{FileByFilename: filename}
	default:
		req.MessageRequest = &reflectionv1.ServerReflectionRequest_ListServices{}
	}
	if err := s.stream.Send(req); err != nil {
		return nil, streamError(s.stream.Recv, err)
	}
	resp, err := s.stream.Recv()
	if err != nil {
		return nil, err
	}
	if e := resp.GetErrorResponse(); e != nil {
		return nil, status.Error(codes.Code(e.GetErrorCode()), e.GetErrorMessage()) //nolint:gosec // G115: a status code the server sent, in range
	}
	return resp.GetFileDescriptorResponse().GetFileDescriptorProto(), nil
}

func (s *reflectionV1) close() { _ = s.stream.CloseSend() }

//nolint:staticcheck // v1alpha is deprecated, and is the fallback for servers without v1
type reflectionV1alpha struct {
	stream grpc.BidiStreamingClient[reflectionv1alpha.ServerReflectionRequest, reflectionv1alpha.ServerReflectionResponse]
}

//nolint:staticcheck,gosec // v1alpha is deprecated, and is the fallback for servers without v1; G115: a status code the server sent
func (s *reflectionV1alpha) ask(symbol, filename string) ([][]byte, error) {
	req := &reflectionv1alpha.ServerReflectionRequest{}
	switch {
	case symbol != "":
		req.MessageRequest = &reflectionv1alpha.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol}
	case filename != "":
		req.MessageRequest = &reflectionv1alpha.ServerReflectionRequest_FileByFilename{FileByFilename: filename}
	default:
		req.MessageRequest = &reflectionv1alpha.ServerReflectionRequest_ListServices{}
	}
	if err := s.stream.Send(req); err != nil {
		return nil, streamError(s.stream.Recv, err)
	}
	resp, err := s.stream.Recv()
	if err != nil {
		return nil, err
	}
	if e := resp.GetErrorResponse(); e != nil {
		return nil, status.Error(codes.Code(e.GetErrorCode()), e.GetErrorMessage())
	}
	return resp.GetFileDescriptorResponse().GetFileDescriptorProto(), nil
}

func (s *reflectionV1alpha) close() { _ = s.stream.CloseSend() }

// streamError is the status that ended a stream a Send failed on: Send
// reports only io.EOF, and the status comes from Recv.
func streamError[T any](recv func() (T, error), err error) error {
	if errors.Is(err, io.EOF) {
		if _, recvErr := recv(); recvErr != nil {
			return recvErr
		}
	}
	return err
}
