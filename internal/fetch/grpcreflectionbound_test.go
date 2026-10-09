//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// endlessReflection is a hostile reflection service: it answers every
// question about a file with batch files, each importing the next, the last
// importing a file it has not sent yet, for ever. Each file carries padding
// bytes of an option, so its size is the test's to choose.
type endlessReflection struct {
	reflectionv1.UnimplementedServerReflectionServer
	batch   int
	padding int
	// hold, when set, holds every answer about a file until it is closed.
	hold    <-chan struct{}
	streams atomic.Int64
	files   atomic.Int64
	bytes   atomic.Int64
}

func (s *endlessReflection) ServerReflectionInfo(stream grpc.BidiStreamingServer[reflectionv1.ServerReflectionRequest, reflectionv1.ServerReflectionResponse]) error {
	s.streams.Add(1)
	padding := strings.Repeat("x", s.padding)
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		resp := &reflectionv1.ServerReflectionResponse{}
		if req.GetFileContainingSymbol() == "" && req.GetFileByFilename() == "" {
			resp.MessageResponse = &reflectionv1.ServerReflectionResponse_ListServicesResponse{ListServicesResponse: &reflectionv1.ListServiceResponse{}}
		} else {
			if s.hold != nil {
				<-s.hold
			}
			var raw [][]byte
			for range s.batch {
				n := s.files.Add(1)
				fd := &descriptorpb.FileDescriptorProto{
					Name:       proto.String(fmt.Sprintf("f%d.proto", n)),
					Package:    proto.String("p"),
					Dependency: []string{fmt.Sprintf("f%d.proto", n+1)},
				}
				if s.padding > 0 {
					fd.Options = &descriptorpb.FileOptions{GoPackage: proto.String(padding)}
				}
				b, err := proto.Marshal(fd)
				if err != nil {
					return err
				}
				s.bytes.Add(int64(len(b)))
				raw = append(raw, b)
			}
			resp.MessageResponse = &reflectionv1.ServerReflectionResponse_FileDescriptorResponse{FileDescriptorResponse: &reflectionv1.FileDescriptorResponse{FileDescriptorProto: raw}}
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

// startEndless serves s on a new port and returns its address.
func startEndless(t *testing.T, s *endlessReflection) string {
	t.Helper()
	ln := grpctest.Listen(t)
	server := grpc.NewServer()
	reflectionv1.RegisterServerReflectionServer(server, s)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(server.Stop)
	return ln.Addr().String()
}

// A reflection service that keeps naming new imports, each file large, is
// followed only as far as the collector's response limit, or 10 MiB when the
// limit is smaller: the question fails, as a limit, saying which bound it
// passed and what to do, once the files it took come to more than that, and
// the server has sent no more than that and the file that passed it. A
// collector holding its answers to a kilobyte takes 10 MiB of files, and one
// whose limit is past 10 MiB takes as much as its limit. A failed question
// is not kept: the next probe asks again, and fails the same way at once,
// not at the question's own timeout.
func TestAReflectionQuestionTakesNoMoreBytesThanTheResponseLimit(t *testing.T) {
	hostile := &endlessReflection{batch: 1, padding: 1 << 20}
	addr := startEndless(t, hostile)
	sent := int64(0)
	for _, limit := range []model.ByteSize{1024, 1024, defaultResponseLimit + 2<<20} {
		c := grpcCollector()
		c.Request.MaxResponseBytes = limit
		bound := max(int64(limit), defaultResponseLimit)
		_, err := FetchCollector(context.Background(), addr, validGRPC(t, c), RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("come to more than %d bytes, the most a reflection question takes: the collector's response limit, or 10 MiB when that is smaller; raise request.max_response_bytes or limits.max_response_bytes past it, or use descriptors: protoset or proto", bound)) {
			t.Fatalf("limit %d: got %v, want the failure at %d bytes", limit, err, bound)
		}
		if !errors.Is(err, model.ErrLimitExceeded) {
			t.Fatalf("limit %d: %v is not counted as a limit", limit, err)
		}
		if most := sent + bound + int64(hostile.padding) + 64; hostile.bytes.Load() > most {
			t.Fatalf("limit %d: the server sent %d bytes to the question, more than the bound and a file", limit, hostile.bytes.Load()-sent)
		}
		sent = hostile.bytes.Load()
	}
	if n := hostile.streams.Load(); n != 3 {
		t.Fatalf("%d reflection streams, want one per probe: a failed question is not kept", n)
	}
}

// A reflection service that sends more files than any service has, each
// tiny, is stopped at maxReflectionFiles, whatever the response limit
// allows: the question fails saying so. The files form one chain, each
// importing the next, the longest import chain the files can make, and the
// files the server sent are those of the one answer that passed the bound,
// sent whole, and none after it.
func TestAReflectionQuestionTakesNoMoreThanTheMostFiles(t *testing.T) {
	hostile := &endlessReflection{batch: maxReflectionFiles/2 + 1}
	addr := startEndless(t, hostile)
	_, err := FetchCollector(context.Background(), addr, validGRPC(t, grpcCollector()), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("the reflection service sent more than %d files for the service %s, the most the exporter takes for one service", maxReflectionFiles, grpctest.Service)) {
		t.Fatalf("got %v, want the failure of too many files", err)
	}
	if !errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("%v is not counted as a limit", err)
	}
	if n := hostile.files.Load(); n != int64(2*hostile.batch) {
		t.Fatalf("the server sent %d files, want the two answers of %d that pass the bound", n, hostile.batch)
	}
}

// Probes that miss together still share one question when it fails at a
// bound: one reflection stream, and every probe gets its failure.
func TestProbesShareAReflectionQuestionThatFailsAtABound(t *testing.T) {
	// The server holds its first answer until every probe waits for the
	// question: a failure is not kept, so a probe that came after it would
	// ask again.
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	hostile := &endlessReflection{batch: maxReflectionFiles + 1, hold: hold}
	addr := startEndless(t, hostile)
	c := validGRPC(t, grpcCollector())
	const probes = 4
	errs := make(chan error, probes)
	probe := func() {
		_, err := FetchCollector(context.Background(), addr, c, RequestOverrides{}, nil)
		errs <- err
	}
	go probe()
	waitFor(t, "the first probe's question to reach the server", func() bool { return hostile.streams.Load() > 0 })
	for range probes - 1 {
		go probe()
	}
	key := grpcConnKey{dial: addr, policy: policyOf(c)}
	waitFor(t, "the other probes to join the question", func() bool { return grpcConns.holders(key) == probes+1 })
	release()
	for range probes {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "files for the service") {
			t.Fatalf("got %v, want the failure of too many files", err)
		}
	}
	if n := hostile.streams.Load(); n != 1 {
		t.Fatalf("%d reflection streams for %d probes, want 1", n, probes)
	}
}

// A service whose files come to just the question's limit is answered; one
// byte less and the question fails at the limit. The bound is the files'
// bytes as they arrive, the measure the limit is for an answer, all the
// question's answers together: the server sends the service's file first,
// and its imports when they are asked for by name.
func TestAReflectionAnswerOfJustTheLimitIsTaken(t *testing.T) {
	var protos []*descriptorpb.FileDescriptorProto
	size := 0
	grpctest.Files(t).RangeFiles(func(f protoreflect.FileDescriptor) bool {
		// A well-known file is built into the exporter, and not asked for.
		if _, err := protoregistry.GlobalFiles.FindFileByPath(f.Path()); err == nil {
			return true
		}
		fd := protodesc.ToFileDescriptorProto(f)
		b, err := proto.Marshal(fd)
		if err != nil {
			t.Fatal(err)
		}
		size += len(b)
		if f.Path() == grpctest.QueueFile {
			protos = append([]*descriptorpb.FileDescriptorProto{fd}, protos...)
		} else {
			protos = append(protos, fd)
		}
		return true
	})
	graph := &graphReflection{}
	graph.serve(protos, false)
	conn, err := grpc.NewClient(startGraph(t, graph), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := askReflection(context.Background(), conn, grpctest.Service, int64(size)); err != nil {
		t.Fatalf("files of %d bytes against a limit of %d: %v", size, size, err)
	}
	_, err = askReflection(context.Background(), conn, grpctest.Service, int64(size-1))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("come to more than %d bytes", size-1)) {
		t.Fatalf("files of %d bytes against a limit of %d: got %v, want the limit's failure", size, size-1, err)
	}
}

// A collector that holds its answers to a kilobyte still takes its
// service's descriptors, larger than that, by reflection.
func TestAReflectionQuestionIsNotHeldToASmallResponseLimit(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: statsAnswer})
	c := grpcCollector()
	c.Request.MaxResponseBytes = 1024
	checked := validGRPC(t, c)
	if _, err := FetchCollector(context.Background(), server.Addr, checked, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	size := 0
	grpctest.Files(t).RangeFiles(func(f protoreflect.FileDescriptor) bool {
		size += proto.Size(protodesc.ToFileDescriptorProto(f))
		return true
	})
	if size <= 1024 {
		t.Fatalf("the service's files are %d bytes, within the limit: the test shows nothing", size)
	}
}

// graphReflection serves a set of files: the first for any symbol, each by
// its name, and with each the files it imports that the stream has not had
// yet when sendImports is set, as grpc-go's reflection service does; a
// server without it makes the client ask for every import by name.
type graphReflection struct {
	reflectionv1.UnimplementedServerReflectionServer
	mu          sync.Mutex
	protos      []*descriptorpb.FileDescriptorProto
	sendImports bool
}

func (s *graphReflection) serve(protos []*descriptorpb.FileDescriptorProto, sendImports bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.protos, s.sendImports = protos, sendImports
}

func (s *graphReflection) ServerReflectionInfo(stream grpc.BidiStreamingServer[reflectionv1.ServerReflectionRequest, reflectionv1.ServerReflectionResponse]) error {
	s.mu.Lock()
	protos, sendImports := s.protos, s.sendImports
	s.mu.Unlock()
	byName := map[string]*descriptorpb.FileDescriptorProto{}
	for _, fd := range protos {
		byName[fd.GetName()] = fd
	}
	sent := map[string]bool{}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		resp := &reflectionv1.ServerReflectionResponse{}
		var fd *descriptorpb.FileDescriptorProto
		switch {
		case req.GetFileContainingSymbol() != "":
			fd = protos[0]
		case req.GetFileByFilename() != "":
			fd = byName[req.GetFileByFilename()]
		}
		switch {
		case req.GetFileContainingSymbol() == "" && req.GetFileByFilename() == "":
			resp.MessageResponse = &reflectionv1.ServerReflectionResponse_ListServicesResponse{ListServicesResponse: &reflectionv1.ListServiceResponse{}}
		case fd == nil:
			resp.MessageResponse = &reflectionv1.ServerReflectionResponse_ErrorResponse{ErrorResponse: &reflectionv1.ErrorResponse{ErrorCode: 5, ErrorMessage: "not found"}}
		default:
			var raw [][]byte
			var send func(fd *descriptorpb.FileDescriptorProto)
			send = func(fd *descriptorpb.FileDescriptorProto) {
				sent[fd.GetName()] = true
				b, _ := proto.Marshal(fd)
				raw = append(raw, b)
				if !sendImports {
					return
				}
				for _, dep := range fd.GetDependency() {
					if d := byName[dep]; d != nil && !sent[dep] {
						send(d)
					}
				}
			}
			send(fd)
			resp.MessageResponse = &reflectionv1.ServerReflectionResponse_FileDescriptorResponse{FileDescriptorResponse: &reflectionv1.FileDescriptorResponse{FileDescriptorProto: raw}}
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

// oldAskReflection is askReflection as it was before its bounds: the oracle
// of what it gives within them.
func oldAskReflection(ctx context.Context, conn *grpc.ClientConn, service string) (*protoregistry.Files, error) {
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

// Within its bounds a reflection question gives what it gave: the same
// files from grpc-go's own reflection service, v1 and v1alpha, and from
// generated sets of files (randomSet), served with their imports and
// without, so that every import is asked for by name. A set the old
// question failed on fails. The text of that failure is not compared: the
// old question met the files in the order of a map, so which of two faults
// it named first was chance.
func TestAReflectionQuestionGivesWhatItGaveWithinItsBounds(t *testing.T) {
	ask := func(addr string) (func(service string) (want, got map[string]string), func()) {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		return func(service string) (want, got map[string]string) {
			files, err := oldAskReflection(context.Background(), conn, service)
			want = registryOutcome(t, files, err)
			files, err = askReflection(context.Background(), conn, service, defaultResponseLimit)
			got = registryOutcome(t, files, err)
			return want, got
		}, func() { _ = conn.Close() }
	}
	for _, version := range []string{"v1", "v1alpha"} {
		server := grpctest.Start(t, grpctest.Options{Reflection: version})
		question, done := ask(server.Addr)
		want, got := question(grpctest.Service)
		if _, failed := want["error"]; failed || !maps.Equal(got, want) {
			t.Fatalf("%s: got %v, want %v", version, keysOf(got), keysOf(want))
		}
		done()
	}
	graph := &graphReflection{}
	addr := startGraph(t, graph)
	question, done := ask(addr)
	defer done()
	r := rand.New(rand.NewPCG(52, 51))
	seen := map[string]int{}
	for i := range alloctest.UnlessRaced(400, 100) {
		protos := randomSet(r)
		graph.serve(protos, i%2 == 0)
		want, got := question("p.S")
		_, failed := want["error"]
		_, fails := got["error"]
		seen[strconv.FormatBool(failed)]++
		if failed != fails || !failed && !maps.Equal(got, want) {
			t.Fatalf("set %d: got %v, want %v", i, keysOf(got), keysOf(want))
		}
	}
	if seen["true"] < 10 || seen["false"] < 10 {
		t.Fatalf("outcomes %v: the generated sets do not cover both", seen)
	}
}

// startGraph serves s on a new port and returns its address.
func startGraph(t *testing.T, s *graphReflection) string {
	t.Helper()
	ln := grpctest.Listen(t)
	server := grpc.NewServer()
	reflectionv1.RegisterServerReflectionServer(server, s)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(server.Stop)
	return ln.Addr().String()
}
