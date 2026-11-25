//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// A grpc call needs its method's input and output message types to encode
// the JSON message and decode the response. They are built at run time
// (dynamicpb) from descriptors, found where request.descriptors says:
//
//   - reflection asks the target's reflection service, grpc.reflection.v1
//     (or v1alpha when the server has only that), for the file that defines
//     the service and the files it imports. The answer is cached per
//     connection and service for reflectionTTL, so a scrape costs one call
//     instead of two. It is asked again sooner when the method is not found
//     or the answer does not decode, as after a server upgrade. A separate
//     timer then drops it, whether or not another call comes. At most
//     maxReflectionAnswers answers, with maxReflectionBytes of files in
//     total, are kept, and the least recently used is dropped first. This
//     bound is needed because a probe chooses its target, and each target's
//     answer can be as large as the collector's reflection limit.
//   - protoset reads a FileDescriptorSet from protoset_file.
//   - proto compiles the .proto files of proto_files, resolving imports in
//     proto_import_paths, with the well-known types built in.
//
// protoset and proto are read when the configuration loads, so a missing
// method or a message that does not fit fails the configuration. They are
// read again at a call when one of their files has changed: the descriptor
// set, a .proto file, or a file one imports. The configuration watch checks
// the same files, both while the configuration is in force and while a
// reload is refused (ReadFiles). The health service's types are compiled
// into the exporter, so it needs none of the three.

// reflectionTTL is how long a reflection answer is used before the server is
// asked again.
const reflectionTTL = 10 * time.Minute

// maxReflectionAnswers and maxReflectionBytes bound the reflection answers
// kept across collectors: their number, and the total bytes of files the
// server sent for them. An answer is tens of kilobytes, so the byte bound
// leaves room for a thousand services of the largest APIs. An answer larger
// than the byte bound by itself is used by the probes that asked for it and
// not kept.
const (
	maxReflectionAnswers = 1000
	maxReflectionBytes   = 128 << 20
)

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

// findMethod looks up rpc, package.Service/Method, among files. source says
// where the files came from, for the errors. A server streaming method is
// found only when streamed (the collector's stream_messages is set), and a
// unary one only when it is not.
func findMethod(files *protoregistry.Files, rpc, source string, streamed bool) (grpcMethod, error) {
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
		return grpcMethod{}, fmt.Errorf("%s is a bidirectional streaming method; only unary and server streaming methods can be called", rpc)
	case method.IsStreamingClient():
		return grpcMethod{}, fmt.Errorf("%s is a client streaming method; only unary and server streaming methods can be called", rpc)
	case method.IsStreamingServer() && !streamed:
		return grpcMethod{}, fmt.Errorf("%s is a server streaming method; set request.stream_messages to read the first messages of its stream", rpc)
	case !method.IsStreamingServer() && streamed:
		return grpcMethod{}, fmt.Errorf("%s is a unary method; request.stream_messages is for a server streaming method, so take it out", rpc)
	}
	return grpcMethod{desc: method, types: dynamicpb.NewTypes(files)}, nil
}

// staticMethod is the method of a collector whose descriptors are read
// without asking the server: protoset, proto, or the built-in health
// service's when descriptors is left out.
func staticMethod(c *model.Collector) (grpcMethod, error) {
	streamed := grpcRequest(c).StreamMessages > 0
	switch grpcRequest(c).Descriptors {
	case "":
		return findMethod(protoregistry.GlobalFiles, grpcRequest(c).RPC, "the built-in health service", streamed)
	case descriptorsProtoset:
		files, err := descriptorFiles.protoset(grpcRequest(c).ProtosetFile)
		if err != nil {
			return grpcMethod{}, err
		}
		return findMethod(files, grpcRequest(c).RPC, "protoset_file "+grpcRequest(c).ProtosetFile, streamed)
	case descriptorsProto:
		files, err := descriptorFiles.proto(grpcRequest(c).ProtoFiles, grpcRequest(c).ProtoImportPaths)
		if err != nil {
			return grpcMethod{}, err
		}
		return findMethod(files, grpcRequest(c).RPC, "proto_files", streamed)
	}
	return grpcMethod{}, fmt.Errorf("descriptors %q are not read from files", grpcRequest(c).Descriptors)
}

// fileSets keeps what protoset and proto read, by the files they read, and
// reads them again when one changed. Each set has a lock of its own, so a
// slow compile of one collector's files keeps no other collector waiting.
type fileSets struct {
	mu    sync.Mutex
	slots map[string]*fileSetSlot
}

// fileSetSlot is one set's lock and its last reading: entry when the set
// could be read, and failed, without files, when it could not. A set that
// could not be read is read again by whoever needs its files (get). failed
// only records what that reading looked at (looked).
type fileSetSlot struct {
	mu     sync.Mutex
	entry  *fileSetEntry
	failed *fileSetEntry
}

type fileSetEntry struct {
	// paths are all paths the reading looked at: the files it read, and the
	// places where it looked for a file and found none. stamp records each
	// path's state just before it was looked at. So a file changed while the
	// set was read no longer matches its stamp, and the set is read again.
	paths []string
	stamp string
	files *protoregistry.Files
}

// current reports whether the paths the reading looked at are as they were.
func (e *fileSetEntry) current() bool {
	return e != nil && e.stamp == filesStamp(e.paths)
}

var descriptorFiles = &fileSets{slots: map[string]*fileSetSlot{}}

// descriptorFileRead, set by tests, is called with the path of a descriptor
// file that has just been read, or opened to be, so a test can change the
// file at that moment.
var descriptorFileRead func(path string)

// protoCompiles counts the compiles of .proto files, for tests.
var protoCompiles atomic.Int64

// filesStamp describes files as they are on disk now. For each it records
// the modification time and size, which file the path leads to and its
// permissions, or that there is no file. This way the check at each call
// sees what the configuration watch sees (config's filesStamp):
//   - A file replaced by pointing a symbolic link elsewhere is a different
//     file even when its time and size match the old one. This is how
//     Kubernetes publishes a new version of a mounted Secret or ConfigMap,
//     by swapping ..data.
//   - A file whose permissions changed may no longer be readable.
//
// The file a path leads to is identified by fileIdentity. On the systems the
// exporter is built for, the one stat that follows the path already gives
// it, and the watch stamps it too. So a file renamed over by one with the
// same time, size and permissions is a different file to both. It is not
// found by resolving the path's links, as the watch also does, because that
// costs a stat of each path component at every call. The numbers are
// written through num, which allocates nothing.
func filesStamp(paths []string) string {
	var b strings.Builder
	var num [20]byte
	for _, path := range paths {
		b.WriteString(path)
		// The paths are the files the configuration names (G703 is reported
		// only in some builds, as gosec's taint analysis sees more callers).
		if st, err := os.Stat(path); err == nil { //nolint:gosec // see above
			b.WriteString(":")
			b.Write(strconv.AppendInt(num[:0], st.ModTime().UnixNano(), 10))
			b.WriteString(":")
			b.Write(strconv.AppendInt(num[:0], st.Size(), 10))
			b.WriteString(":")
			if dev, ino, resolved := fileIdentity(path, st); resolved != "" {
				b.WriteString(resolved)
			} else {
				b.Write(strconv.AppendUint(num[:0], dev, 10))
				b.WriteString(".")
				b.Write(strconv.AppendUint(num[:0], ino, 10))
			}
			b.WriteString(":")
			b.Write(strconv.AppendUint(num[:0], uint64(st.Mode()), 8))
		} else {
			b.WriteString(":missing")
		}
		b.WriteByte('|')
	}
	return b.String()
}

// fileIdentity identifies the file st is, where st is the stat of path
// following its links. It uses the device and inode the stat already holds
// (FileIdentity), which the configuration watch stamps too. Where the system
// does not provide them, it resolves the path's links, as the watch also does.
func fileIdentity(path string, st os.FileInfo) (dev, ino uint64, resolved string) {
	if dev, ino, ok := FileIdentity(st); ok {
		return dev, ino, ""
	}
	resolved, _ = filepath.EvalSymlinks(path)
	return 0, 0, resolved
}

// pathStamp is the stamp of one path: the stamp of several is theirs, one
// after the other.
func pathStamp(path string) string { return filesStamp([]string{path}) }

// fileSetRead is what reading a set gives: the files, every path the reading
// looked at, and how each was just before it was (fileSetEntry).
type fileSetRead func() (files *protoregistry.Files, paths []string, stamp string, err error)

// slot returns the slot kept under key, locked.
func (s *fileSets) slot(key string) *fileSetSlot {
	s.mu.Lock()
	slot := s.slots[key]
	if slot == nil {
		slot = &fileSetSlot{}
		s.slots[key] = slot
	}
	s.mu.Unlock()
	slot.mu.Lock()
	return slot
}

// readLocked reads the set of a locked slot and keeps what that gave.
func (slot *fileSetSlot) readLocked(read fileSetRead) (*fileSetEntry, error) {
	files, paths, stamp, err := read()
	if err != nil {
		slot.entry, slot.failed = nil, &fileSetEntry{paths: paths, stamp: stamp}
		return slot.failed, err
	}
	slot.entry, slot.failed = &fileSetEntry{paths: paths, stamp: stamp, files: files}, nil
	return slot.entry, nil
}

// get is getStamped for a reading that does not say how its files were
// before it read them: they are stamped when it has.
func (s *fileSets) get(key string, read func() (*protoregistry.Files, []string, error)) (*protoregistry.Files, error) {
	return s.getStamped(key, func() (*protoregistry.Files, []string, string, error) {
		files, paths, err := read()
		return files, paths, filesStamp(paths), err
	})
}

// getStamped returns the set kept under key when its files are as they
// were, and reads it with read otherwise.
func (s *fileSets) getStamped(key string, read fileSetRead) (*protoregistry.Files, error) {
	slot := s.slot(key)
	defer slot.mu.Unlock()
	if slot.entry.current() {
		return slot.entry.files, nil
	}
	entry, err := slot.readLocked(read)
	if err != nil {
		return nil, err
	}
	return entry.files, nil
}

// looked returns the paths that the last reading of the set kept under key
// looked at, and their stamp, which tells that reading apart from another.
// The set is read now if it was never read, or if one of those paths has
// changed. Whether it can be read does not change the result.
func (s *fileSets) looked(key string, read fileSetRead) (paths []string, stamp string) {
	slot := s.slot(key)
	defer slot.mu.Unlock()
	for _, entry := range []*fileSetEntry{slot.entry, slot.failed} {
		if entry.current() {
			return entry.paths, entry.stamp
		}
	}
	entry, _ := slot.readLocked(read)
	return entry.paths, entry.stamp
}

// protoset reads a FileDescriptorSet.
func (s *fileSets) protoset(path string) (*protoregistry.Files, error) {
	return s.getStamped("protoset\x00"+path, func() (*protoregistry.Files, []string, string, error) {
		paths := []string{path}
		stamp := filesStamp(paths)
		raw, err := os.ReadFile(path)
		if hook := descriptorFileRead; hook != nil {
			hook(path)
		}
		if err != nil {
			return nil, paths, stamp, fmt.Errorf("reading protoset_file: %w", err)
		}
		set := &descriptorpb.FileDescriptorSet{}
		if err := proto.Unmarshal(raw, set); err != nil {
			return nil, paths, stamp, fmt.Errorf("protoset_file %s is not a FileDescriptorSet: %w", path, err)
		}
		files, err := registryOf(set.GetFile())
		if err != nil {
			return nil, paths, stamp, fmt.Errorf("protoset_file %s: %w; write it with the files it imports, as protoc --include_imports or buf build does", path, err)
		}
		return files, paths, stamp, nil
	})
}

// standardImports resolves the well-known files that are built in, and
// nothing else.
var standardImports = protocompile.WithStandardImports(protocompile.ResolverFunc(func(string) (protocompile.SearchResult, error) {
	return protocompile.SearchResult{}, fs.ErrNotExist
}))

// protoKey is the key the set of .proto files is kept under.
func protoKey(paths, importPaths []string) string {
	return "proto\x00" + strings.Join(paths, "\x00") + "\x01" + strings.Join(importPaths, "\x00")
}

// proto compiles .proto files. Without import paths, each file's directory
// is an import path and the file is compiled by its name in it. With them,
// each file must be under one and is compiled by its path from it, as
// protoc -I does.
func (s *fileSets) proto(paths, importPaths []string) (*protoregistry.Files, error) {
	return s.getStamped(protoKey(paths, importPaths), func() (*protoregistry.Files, []string, string, error) {
		return compileProto(paths, importPaths)
	})
}

// protoLooked returns what the last compile of the .proto files looked at
// (looked): the files themselves, the files they import at any depth as the
// import paths resolved them, and the places in an earlier import path
// where such a file was looked for and not found.
func (s *fileSets) protoLooked(paths, importPaths []string) (looked []string, stamp string) {
	return s.looked(protoKey(paths, importPaths), func() (*protoregistry.Files, []string, string, error) {
		return compileProto(paths, importPaths)
	})
}

// compileProto compiles .proto files and reports what it looked at: every
// path it opened or tried to open, the named files first and the others in
// order, each stamped just before it was opened. A file is looked for in
// each import path in turn, as protoc does. So the places in earlier import
// paths where it was not found are included: a file that appears there is
// compiled from then on. Places where a well-known file,
// google/protobuf/*.proto, was not found are not included, since those files
// are built in. Import paths after the one where a file was found are never
// checked for it.
func compileProto(paths, importPaths []string) (*protoregistry.Files, []string, string, error) {
	protoCompiles.Add(1)
	stamps := map[string]string{}
	for _, path := range paths {
		stamps[path] = pathStamp(path)
	}
	var mu sync.Mutex
	var found []string
	// looked is the paths and their stamps, whatever became of the compile.
	looked := func() ([]string, string) {
		mu.Lock()
		defer mu.Unlock()
		slices.Sort(found)
		all := append(slices.Clone(paths), found...)
		var stamp strings.Builder
		for _, path := range all {
			stamp.WriteString(stamps[path])
		}
		return all, stamp.String()
	}
	names, dirs, err := protoNames(paths, importPaths)
	if err != nil {
		all, stamp := looked()
		return nil, all, stamp, err
	}
	resolver := protocompile.ResolverFunc(func(name string) (protocompile.SearchResult, error) {
		// What this one name was looked for at, in order.
		var tried, was []string
		source := &protocompile.SourceResolver{
			ImportPaths: dirs,
			Accessor: func(path string) (io.ReadCloser, error) {
				tried, was = append(tried, path), append(was, pathStamp(path))
				f, err := os.Open(path)
				if hook := descriptorFileRead; hook != nil && err == nil {
					hook(path)
				}
				return f, err
			},
		}
		result, err := source.FindFileByPath(name)
		if err != nil {
			// The well-known files are built in, and a name the import
			// paths do not have is looked for among them, as
			// protocompile.WithStandardImports does.
			if standard, standardErr := standardImports.FindFileByPath(name); standardErr == nil {
				return standard, nil
			}
		}
		// A named file keeps the stamp it was given before the compile began.
		mu.Lock()
		for i, path := range tried {
			if _, known := stamps[path]; !known {
				found, stamps[path] = append(found, path), was[i]
			}
		}
		mu.Unlock()
		return result, err
	})
	compiled, err := (&protocompile.Compiler{Resolver: resolver}).Compile(context.Background(), names...)
	all, stamp := looked()
	if err != nil {
		return nil, all, stamp, fmt.Errorf("compiling proto_files: %w", err)
	}
	files := new(protoregistry.Files)
	for _, f := range compiled {
		if err := registerFileTree(files, f); err != nil {
			return nil, all, stamp, err
		}
	}
	return files, all, stamp, nil
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

// registryOf builds the files of a descriptor set or of a reflection answer.
// An import missing from the set is taken from the types built into the
// exporter when it has them, such as the well-known types. Otherwise it is
// an error that names the import.
//
// Imports are followed depth first, and each file is registered after the
// files it imports. This uses an explicit stack instead of recursion. A
// chain of imports can be as long as the set, which a reflection service
// chooses. With one frame per file, a long enough chain could overflow the
// goroutine's stack, which is a fatal error and cannot be recovered.
func registryOf(protos []*descriptorpb.FileDescriptorProto) (*protoregistry.Files, error) {
	byName := make(map[string]*descriptorpb.FileDescriptorProto, len(protos))
	for _, fd := range protos {
		byName[fd.GetName()] = fd
	}
	files := new(protoregistry.Files)
	adding := map[string]bool{}
	// enter starts a file: it gives the file when its imports are to be
	// followed, and nil when it is registered already, or built in and
	// registered now.
	enter := func(name string) (*descriptorpb.FileDescriptorProto, error) {
		if _, err := files.FindFileByPath(name); err == nil {
			return nil, nil
		}
		fd, ok := byName[name]
		if !ok {
			builtIn, err := protoregistry.GlobalFiles.FindFileByPath(name)
			if err != nil {
				return nil, fmt.Errorf("the import %s is missing", name)
			}
			return nil, registerFileTree(files, builtIn)
		}
		if adding[name] {
			return nil, fmt.Errorf("%s imports itself", name)
		}
		adding[name] = true
		return fd, nil
	}
	// following is a file whose imports are being followed, and the next
	// of them.
	type following struct {
		fd   *descriptorpb.FileDescriptorProto
		next int
	}
	var stack []following
	for _, top := range protos {
		fd, err := enter(top.GetName())
		if err != nil {
			return nil, err
		}
		if fd != nil {
			stack = append(stack, following{fd: fd})
		}
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			if deps := f.fd.GetDependency(); f.next < len(deps) {
				dep := deps[f.next]
				f.next++
				fd, err := enter(dep)
				if err != nil {
					return nil, err
				}
				if fd != nil {
					stack = append(stack, following{fd: fd})
				}
				continue
			}
			file, err := protodesc.NewFile(f.fd, files)
			if err != nil {
				return nil, err
			}
			if err := files.RegisterFile(file); err != nil {
				return nil, err
			}
			stack = stack[:len(stack)-1]
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
	// bytes is the size of the answers kept. ttl, most and mostBytes are
	// reflectionTTL, maxReflectionAnswers and maxReflectionBytes when zero;
	// the tests set smaller ones.
	bytes     int64
	ttl       time.Duration
	most      int
	mostBytes int64
}

// reflectionCall is an in-flight question to a reflection service, which the
// probes needing the same answer wait for. conn is the connection it was
// sent on. A probe on a newer connection does not wait for a question on a
// connection that was dropped for not answering.
type reflectionCall struct {
	conn  *grpc.ClientConn
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
	// size is the bytes of files the server sent for the answer, used the
	// last time a probe took it, and expiry forgets it after the TTL.
	size   int64
	used   time.Time
	expiry *time.Timer
}

var reflectionAnswers = &reflected{entries: map[reflectedKey]*reflectedEntry{}, asking: map[reflectedKey]*reflectionCall{}}

// reflectionQuestionTimeout bounds a question to a reflection service. The
// question belongs to no single probe. It runs detached from the context of
// the probe that started it, so that probe's deadline or cancellation does
// not fail the others waiting for the same answer. It is also not ended when
// no probe waits for it any more. So a server slower than a probe's timeout
// still gets its answer through, and the answer serves the following probes.
// What it may take meanwhile is bounded by size instead (askReflection).
var reflectionQuestionTimeout = 30 * time.Second

// maxReflectionFiles is the maximum number of files a reflection question
// takes for one service. A reflection service sends the service's file and
// every file it imports. That is tens of files, and a few hundred in the
// largest APIs, so 4096 leaves an order of magnitude to spare. It bounds
// what the response limit alone does not: a server that answers with
// endless tiny files, each importing the next, costs a round trip, a map
// entry and a descriptor per file.
const maxReflectionFiles = 4096

// reflectionLimit is the maximum bytes of files a collector's reflection
// question takes: its response limit, but never less than the default one.
// The operator sets the limit for responses, and a collector may set it as
// low as a kilobyte. The service's descriptors are not a response: they can
// be larger, and the operator does not size them. 10 MiB is more than any
// real service's files, so the floor bounds a hostile server as well as the
// limit does. A collector that raises the limit above it raises this too.
func reflectionLimit(c *model.Collector) int64 {
	return max(responseLimit(c), defaultResponseLimit)
}

// files returns the service's files, asking the server when no answer is
// younger than reflectionTTL. limit bounds the bytes of files the question
// takes (askReflection, reflectionLimit). It is the limit of the probe that
// asks, and probes that share the question share its limit. Every caller,
// including the one that started the question, waits for the shared answer
// only as long as its own context allows. The question holds its connection
// until it ends. So a probe that gives up and drops a connection it thinks
// is dead does not close it under a question a slow server is still
// answering. The answer arrives and serves the following probes.
func (r *reflected) files(ctx context.Context, held *grpcConnEntry, key reflectedKey, limit int64, now time.Time) (*protoregistry.Files, error) {
	conn := held.conn
	r.mu.Lock()
	if entry := r.entries[key]; entry != nil {
		entry.used = now
		r.mu.Unlock()
		return entry.files, nil
	}
	call := r.asking[key]
	if call == nil || call.conn != conn {
		call = &reflectionCall{conn: conn, done: make(chan struct{})}
		r.asking[key] = call
		// The question keeps the probe's values (its metadata) but not its
		// deadline or cancellation.
		detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), reflectionQuestionTimeout)
		grpcConns.hold(held)
		go func() {
			defer grpcConns.release(held)
			defer cancel()
			var files *protoregistry.Files
			var size int64
			err := panicToError(func() (err error) {
				files, size, err = askReflection(detached, conn, key.service, limit)
				return err
			})
			r.mu.Lock()
			call.files, call.err = files, err
			if r.asking[key] == call {
				delete(r.asking, key)
			}
			if err == nil {
				r.keepLocked(key, &reflectedEntry{files: files, fetched: now, size: size, used: now})
			}
			r.mu.Unlock()
			close(call.done)
		}()
	}
	r.mu.Unlock()
	select {
	case <-call.done:
		return call.files, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// keepLocked keeps an answer for the TTL, replacing any kept under its key.
// While the cache would exceed its bounds, it drops the least recently used
// answers. An answer larger than the byte bound by itself is not kept.
func (r *reflected) keepLocked(key reflectedKey, entry *reflectedEntry) {
	ttl, most, mostBytes := r.ttl, r.most, r.mostBytes
	if ttl <= 0 {
		ttl = reflectionTTL
	}
	if most <= 0 {
		most = maxReflectionAnswers
	}
	if mostBytes <= 0 {
		mostBytes = maxReflectionBytes
	}
	r.forgetLocked(key)
	if entry.size > mostBytes {
		return
	}
	for len(r.entries) >= most || r.bytes+entry.size > mostBytes {
		var oldestKey reflectedKey
		var oldest *reflectedEntry
		for k, kept := range r.entries {
			if oldest == nil || kept.used.Before(oldest.used) {
				oldestKey, oldest = k, kept
			}
		}
		r.forgetLocked(oldestKey)
	}
	r.entries[key] = entry
	r.bytes += entry.size
	entry.expiry = time.AfterFunc(ttl, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.entries[key] == entry {
			r.forgetLocked(key)
		}
	})
}

// forget drops an answer, so the next call asks again.
func (r *reflected) forget(key reflectedKey) {
	r.mu.Lock()
	r.forgetLocked(key)
	r.mu.Unlock()
}

func (r *reflected) forgetLocked(key reflectedKey) {
	entry := r.entries[key]
	if entry == nil {
		return
	}
	if entry.expiry != nil {
		entry.expiry.Stop()
	}
	delete(r.entries, key)
	r.bytes -= entry.size
}

// reflectionStream asks a reflection service for files, by the symbol they
// define or by name. v1 and v1alpha are the same protocol under two names.
type reflectionStream interface {
	ask(symbol, filename string) ([][]byte, error)
	close()
}

// askReflection asks the server's reflection service for the file that
// defines service and every file it imports. The server chooses how many
// files that is and how large they are. So the question takes at most limit
// bytes of files (reflectionLimit) and maxReflectionFiles files, and fails
// at whichever it passes first.
func askReflection(ctx context.Context, conn *grpc.ClientConn, service string, limit int64) (*protoregistry.Files, int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := openReflection(ctx, conn)
	if err != nil {
		return nil, 0, err
	}
	defer stream.close()
	answers, err := stream.ask(service, "")
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, 0, fmt.Errorf("the server's reflection service does not know the service %s: %w", service, err)
		}
		return nil, 0, err
	}
	byName := map[string]*descriptorpb.FileDescriptorProto{}
	// imports are the files' imports still to be looked at, each with the
	// file that names it: a file the server sends again under the same name
	// replaces the first, and only the one kept counts.
	type importOf struct {
		fd  *descriptorpb.FileDescriptorProto
		dep string
	}
	var imports []importOf
	var received, size int64
	collect := func(raw [][]byte) error {
		for _, b := range raw {
			received++
			size += int64(len(b))
			if received > maxReflectionFiles {
				return model.MarkError(fmt.Errorf("the reflection service sent more than %d files for the service %s, the most the exporter takes for one service, whose files and imports are far fewer; the server's reflection answer is broken, so use descriptors: protoset or proto", maxReflectionFiles, service), model.ErrLimitExceeded)
			}
			if size > limit {
				return model.MarkError(fmt.Errorf("the reflection service's files for the service %s come to more than %d bytes, the most a reflection question takes: the collector's response limit, or 10 MiB when that is smaller; raise request.max_response_bytes or limits.max_response_bytes past it, or use descriptors: protoset or proto", service, limit), model.ErrLimitExceeded)
			}
			fd := &descriptorpb.FileDescriptorProto{}
			if err := proto.Unmarshal(b, fd); err != nil {
				return fmt.Errorf("the reflection service answered a file that does not decode: %w", err)
			}
			byName[fd.GetName()] = fd
			for _, dep := range fd.GetDependency() {
				imports = append(imports, importOf{fd: fd, dep: dep})
			}
		}
		return nil
	}
	if err := collect(answers); err != nil {
		return nil, 0, err
	}
	// A server sends the imports it has not sent before on the stream;
	// anything still missing is asked for by name, unless the exporter
	// has it built in.
	for len(imports) > 0 {
		next := imports[len(imports)-1]
		imports = imports[:len(imports)-1]
		name := next.dep
		if byName[next.fd.GetName()] != next.fd {
			continue
		}
		if _, ok := byName[name]; ok {
			continue
		}
		if _, err := protoregistry.GlobalFiles.FindFileByPath(name); err == nil {
			continue
		}
		answers, err := stream.ask("", name)
		if err != nil {
			return nil, 0, fmt.Errorf("asking the reflection service for %s: %w", name, err)
		}
		if err := collect(answers); err != nil {
			return nil, 0, err
		}
		if _, ok := byName[name]; !ok {
			return nil, 0, fmt.Errorf("the reflection service did not send %s, which the service's files import", name)
		}
	}
	protos := make([]*descriptorpb.FileDescriptorProto, 0, len(byName))
	for _, fd := range byName {
		protos = append(protos, fd)
	}
	files, err := registryOf(protos)
	if err != nil {
		return nil, 0, fmt.Errorf("the reflection service's answer: %w", err)
	}
	return files, size, nil
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
