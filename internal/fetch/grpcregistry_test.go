//go:build !select_request_types || request_type_grpc

package fetch

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// chainOf is n files, each importing the next and using its message.
func chainOf(n int) []*descriptorpb.FileDescriptorProto {
	protos := make([]*descriptorpb.FileDescriptorProto, n)
	for i := range protos {
		protos[i] = &descriptorpb.FileDescriptorProto{Name: proto.String(fmt.Sprintf("f%d.proto", i)), Package: proto.String("p")}
		if i+1 < n {
			protos[i].Dependency = []string{fmt.Sprintf("f%d.proto", i+1)}
		}
	}
	return protos
}

// A chain of imports as long as a reflection service may make it is
// registered with no frame of the stack per file: with the stack held to
// 1 MiB, which a frame per file of the chain passes many times over, the
// chain is registered, and every file of it.
func TestARegistryOfALongImportChainUsesNoStackPerFile(t *testing.T) {
	n := alloctest.UnlessRaced(50_000, 20_000)
	protos := chainOf(n)
	before := debug.SetMaxStack(1 << 20)
	files, err := registryOf(protos)
	debug.SetMaxStack(before)
	if err != nil {
		t.Fatal(err)
	}
	if got := files.NumFiles(); got != n {
		t.Fatalf("%d files registered, want %d", got, n)
	}
}

// oldRegistryOf is registryOf as it was before it walked the imports on a
// stack of its own, by recursion: the oracle of what registryOf gives.
func oldRegistryOf(protos []*descriptorpb.FileDescriptorProto) (*protoregistry.Files, error) {
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

// registryOutcome is what a registry build gave: its error's text, or every
// file it registered, as the bytes of its descriptor.
func registryOutcome(t *testing.T, files *protoregistry.Files, err error) map[string]string {
	t.Helper()
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	out := map[string]string{}
	files.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(protodesc.ToFileDescriptorProto(f))
		if err != nil {
			t.Fatal(err)
		}
		out[f.Path()] = string(b)
		return true
	})
	return out
}

// randomSet is a set of up to eight files whose imports are drawn at
// random: forwards, backwards and onto themselves, so that chains, diamonds
// and cycles all come up, with now and then an import of a file the set
// does not carry, one of the well-known files, a file under a name another
// has too, a message under a name another file has, or a field of a type no
// file defines. A file uses the message of each file of the set it imports.
func randomSet(r *rand.Rand) []*descriptorpb.FileDescriptorProto {
	n := 1 + r.IntN(8)
	name := func(i int) string { return fmt.Sprintf("f%d.proto", i) }
	protos := make([]*descriptorpb.FileDescriptorProto, n)
	for i := range protos {
		message := fmt.Sprintf("M%d", i)
		if r.IntN(30) == 0 {
			message = "M0"
		}
		fd := &descriptorpb.FileDescriptorProto{Name: proto.String(name(i)), Package: proto.String("p"), Syntax: proto.String("proto3")}
		if r.IntN(30) == 0 && i > 0 {
			fd.Name = proto.String(name(r.IntN(i)))
		}
		m := &descriptorpb.DescriptorProto{Name: proto.String(message)}
		field := func(typeName string) {
			m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{
				Name:     proto.String(fmt.Sprintf("x%d", len(m.Field)+1)),
				Number:   proto.Int32(int32(len(m.Field) + 1)), //nolint:gosec // G115: a few fields
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(typeName),
			})
		}
		for range r.IntN(3) {
			j := r.IntN(n + 1)
			switch {
			case j == n && r.IntN(2) == 0:
				fd.Dependency = append(fd.Dependency, "google/protobuf/timestamp.proto")
				field(".google.protobuf.Timestamp")
			case j == n:
				fd.Dependency = append(fd.Dependency, "absent.proto")
			case !slices.Contains(fd.Dependency, name(j)):
				fd.Dependency = append(fd.Dependency, name(j))
				if j != i {
					field(fmt.Sprintf(".p.M%d", j))
				}
			}
		}
		if r.IntN(30) == 0 {
			field(".p.Nowhere")
		}
		fd.MessageType = []*descriptorpb.DescriptorProto{m}
		protos[i] = fd
	}
	r.Shuffle(len(protos), func(i, j int) { protos[i], protos[j] = protos[j], protos[i] })
	return protos
}

// registryOf gives what it gave before it walked the imports on a stack of
// its own: the same files, or the same error, over the queue service's
// descriptor set (the files a protoset_file of it carries, with and without
// the well-known Timestamp, in either order), chains, a diamond, cycles, a
// missing import, and generated sets of every shape (randomSet).
func TestARegistryOfGivesWhatItGave(t *testing.T) {
	var queue []*descriptorpb.FileDescriptorProto
	grpctest.Files(t).RangeFiles(func(f protoreflect.FileDescriptor) bool {
		queue = append(queue, protodesc.ToFileDescriptorProto(f))
		return true
	})
	withoutWellKnown := slices.DeleteFunc(slices.Clone(queue), func(fd *descriptorpb.FileDescriptorProto) bool {
		return fd.GetName() == "google/protobuf/timestamp.proto"
	})
	file := func(name string, deps ...string) *descriptorpb.FileDescriptorProto {
		return &descriptorpb.FileDescriptorProto{Name: proto.String(name), Package: proto.String("p"), Dependency: deps}
	}
	cases := map[string][]*descriptorpb.FileDescriptorProto{
		"queue":                                  queue,
		"queue reversed":                         reversed(queue),
		"queue without the well-known":           withoutWellKnown,
		"queue without the well-known, reversed": reversed(withoutWellKnown),
		"empty":                                  nil,
		"chain":                                  chainOf(5),
		"chain reversed":                         reversed(chainOf(5)),
		"diamond":                                {file("a", "b", "c"), file("b", "d"), file("c", "d"), file("d")},
		"self":                                   {file("a", "a")},
		"two in a cycle":                         {file("a", "b"), file("b", "a")},
		"three in a cycle":                       {file("x"), file("a", "b"), file("b", "c"), file("c", "a")},
		"missing":                                {file("a", "b"), file("b", "nowhere")},
		"well-known":                             {file("a", "google/protobuf/empty.proto", "google/protobuf/timestamp.proto")},
		"twice under one name":                   {file("a", "b"), file("a")},
	}
	for name, protos := range cases {
		files, err := oldRegistryOf(protos)
		want := registryOutcome(t, files, err)
		files, err = registryOf(protos)
		if got := registryOutcome(t, files, err); !maps.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", name, keysOf(got), keysOf(want))
		}
	}
	r := rand.New(rand.NewPCG(51, 52))
	seen := map[string]int{}
	for i := range alloctest.UnlessRaced(3000, 500) {
		protos := randomSet(r)
		files, err := oldRegistryOf(protos)
		want := registryOutcome(t, files, err)
		if err != nil {
			seen["error"]++
		} else {
			seen["files"]++
		}
		files, err = registryOf(protos)
		if got := registryOutcome(t, files, err); !maps.Equal(got, want) {
			t.Fatalf("set %d: got %v, want %v", i, got, want)
		}
	}
	// Both outcomes come up often: the sets are not all refused, nor all
	// taken.
	if seen["error"] < 50 || seen["files"] < 50 {
		t.Fatalf("outcomes %v: the generated sets do not cover both", seen)
	}
}

// reversed is protos in the opposite order.
func reversed(protos []*descriptorpb.FileDescriptorProto) []*descriptorpb.FileDescriptorProto {
	out := slices.Clone(protos)
	slices.Reverse(out)
	return out
}

// keysOf is an outcome as a test shows it: the error, or the files.
func keysOf(m map[string]string) []string {
	var keys []string
	for k, v := range m {
		if k == "error" {
			return []string{v}
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
