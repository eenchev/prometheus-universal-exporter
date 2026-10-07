//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// The .proto files of these tests: a service whose file imports a
// well-known file and the file of its request, which imports another, so the
// request's Base message is two imports away from the file a collector names.
const (
	importingService = "w/api/svc.proto"
	importedTypes    = "w/common/types.proto"
	importedBase     = "w/common/base.proto"
	serviceSource    = "syntax = \"proto3\";\npackage w.api;\nimport \"google/protobuf/timestamp.proto\";\nimport \"w/common/types.proto\";\n" +
		"service S { rpc Get(w.common.Request) returns (Reply); }\nmessage Reply { int64 total = 1; google.protobuf.Timestamp at = 2; }\n"
	typesSource = "syntax = \"proto3\";\npackage w.common;\nimport \"w/common/base.proto\";\nmessage Request { string queue = 1; Base base = 2; }\n"
	baseSource  = "syntax = \"proto3\";\npackage w.common;\nmessage Base { string tenant = 1; }\n"
	// baseWithRegion is base.proto with one more field, which a message can
	// only set when this is the file compiled.
	baseWithRegion = "syntax = \"proto3\";\npackage w.common;\nmessage Base { string tenant = 1; string region = 2; }\n"
	regionMessage  = `{"queue": "orders", "base": {"region": "eu"}}`
)

// protoTimes gives each file written a modification time no file had before.
var protoTimes = time.Now().Add(-time.Hour).Truncate(time.Second)

// writeProto writes a file under dir, by its path from it, with a
// modification time of its own, and returns its path.
func writeProto(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	protoTimes = protoTimes.Add(time.Second)
	if err := os.Chtimes(path, protoTimes, protoTimes); err != nil {
		t.Fatal(err)
	}
	return path
}

// importingCollector is a grpc collector that names the service's file
// under root and resolves imports in importPaths.
func importingCollector(root string, importPaths ...string) *model.Collector {
	return &model.Collector{Name: "imports", Request: model.RequestConfig{
		Type: RequestTypeGRPC, RPC: "w.api.S/Get", Descriptors: descriptorsProto,
		ProtoFiles: []string{filepath.Join(root, filepath.FromSlash(importingService))}, ProtoImportPaths: importPaths,
	}}
}

// fitsRegion reports whether the collector's method takes a message that
// sets the field only baseWithRegion has, and the error when it does not.
func fitsRegion(c *model.Collector) error {
	method, err := staticMethod(c)
	if err != nil {
		return err
	}
	return method.checkMessage(regionMessage)
}

// compiles counts the compiles of .proto files that run does.
func compiles(run func()) int64 {
	before := protoCompiles.Load()
	run()
	return protoCompiles.Load() - before
}

// A file two imports away from the one a collector names is part of what
// the collector reads: edited, it is compiled again at the next reading, and
// a message that fits only the new file is then accepted; while nothing
// changes nothing is compiled.
func TestAnImportedFileEditedIsCompiledAgainAtTheNextReading(t *testing.T) {
	dir := t.TempDir()
	writeProto(t, dir, importingService, serviceSource)
	writeProto(t, dir, importedTypes, typesSource)
	writeProto(t, dir, importedBase, baseSource)
	c := importingCollector(dir, dir)
	if err := fitsRegion(c); err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("before the edit: %v, want the field refused", err)
	}
	writeProto(t, dir, importedBase, baseWithRegion)
	if err := fitsRegion(c); err != nil {
		t.Fatalf("after the imported file was edited: %v", err)
	}
	if n := compiles(func() {
		for range 3 {
			if err := fitsRegion(c); err != nil {
				t.Fatal(err)
			}
			ReadFiles(c)
		}
	}); n != 0 {
		t.Fatalf("%d compiles with nothing changed, want none", n)
	}
	// Broken, it fails the reading with the compiler's message, which names
	// it; mended, it is read again.
	writeProto(t, dir, importedBase, "syntax = \"proto3\";\npackage w.common;\nmessage Base { strin tenant = 1; }\n")
	if err := fitsRegion(c); err == nil || !strings.Contains(err.Error(), "base.proto:3:") {
		t.Fatalf("with the imported file broken: %v, want the compiler's message naming it", err)
	}
	writeProto(t, dir, importedBase, baseWithRegion)
	if err := fitsRegion(c); err != nil {
		t.Fatalf("with the imported file mended: %v", err)
	}
}

// What reading a collector's .proto files looked at is the files it names,
// the files those import through two levels, where the import paths resolved
// them, and the place in an earlier import path where a file was looked for
// and not found. A file of the same name in a later import path is not
// looked at, and neither is any place for a well-known file, which is built
// in. A reading that fails says what it looked at too: every place the
// missing import was looked for.
func TestReadFilesAreThePathsTheCompileLookedAt(t *testing.T) {
	first, second, third := t.TempDir(), t.TempDir(), t.TempDir()
	service := writeProto(t, second, importingService, serviceSource)
	types := writeProto(t, second, importedTypes, typesSource)
	base := writeProto(t, second, importedBase, baseSource)
	// The same names later in the import paths are never opened.
	writeProto(t, third, importedTypes, typesSource)
	writeProto(t, third, importedBase, baseWithRegion)
	c := importingCollector(second, first, second, third)
	paths, read := ReadFiles(c)
	want := []string{service}
	for _, name := range []string{importingService, importedBase, importedTypes} {
		want = append(want, filepath.Join(first, filepath.FromSlash(name)))
	}
	want = append(want, base, types)
	slices.Sort(want[1:])
	if !slices.Equal(paths, want) {
		t.Fatalf("looked at\n%s\nwant\n%s", strings.Join(paths, "\n"), strings.Join(want, "\n"))
	}
	for _, path := range paths {
		if strings.Contains(path, "google") || strings.HasPrefix(path, third) {
			t.Errorf("%s is among the paths looked at", path)
		}
	}
	if err := fitsRegion(c); err == nil {
		t.Fatal("the file of the later import path was compiled")
	}
	// The mark of the reading stays while nothing changes, and a change to
	// a file of the later import path is none.
	writeProto(t, third, importedBase, baseSource)
	if n := compiles(func() {
		if again, mark := ReadFiles(c); mark != read || !slices.Equal(again, paths) {
			t.Fatalf("a second look gives another reading: %v", again)
		}
	}); n != 0 {
		t.Fatalf("%d compiles for a second look, want none", n)
	}
	// A file gone from the import path it was found in is looked for
	// further on, and found in the third.
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if _, err := staticMethod(c); err != nil {
		t.Fatalf("with the import in a later import path only: %v", err)
	}
	if later, _ := ReadFiles(c); !slices.Contains(later, filepath.Join(third, filepath.FromSlash(importedBase))) || !slices.Contains(later, base) {
		t.Fatalf("the reading that found the import in the third import path looked at %v", later)
	}
	// Without that import path it is found nowhere, and was looked for in
	// every import path.
	missing := importingCollector(second, first, second)
	if _, err := staticMethod(missing); err == nil || !strings.Contains(err.Error(), importedBase) {
		t.Fatalf("with the import gone: %v, want an error naming it", err)
	}
	var tried []string
	if n := compiles(func() { tried, _ = ReadFiles(missing) }); n != 0 {
		t.Fatalf("%d compiles to say what the failed reading looked at, want none", n)
	}
	for _, dir := range []string{first, second} {
		if candidate := filepath.Join(dir, filepath.FromSlash(importedBase)); !slices.Contains(tried, candidate) {
			t.Errorf("the failed reading did not look at %s: %v", candidate, tried)
		}
	}
	// A reading that failed is not what a caller is answered with: whoever
	// needs the files reads them again.
	if n := compiles(func() { _, _ = staticMethod(missing) }); n != 1 {
		t.Fatalf("%d compiles for a caller after a failed reading, want 1", n)
	}
	// The file back, in either place, is a change of what was looked at.
	writeProto(t, first, importedBase, baseWithRegion)
	if err := fitsRegion(missing); err != nil {
		t.Fatalf("with the import in the first import path: %v", err)
	}
}

// A file that appears earlier in the import paths than the one that was
// compiled is the one protoc would read, and is compiled at the next
// reading, though no file that was read has changed.
func TestAFileAppearingEarlierInTheImportPathsIsCompiled(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeProto(t, second, importingService, serviceSource)
	writeProto(t, second, importedTypes, typesSource)
	writeProto(t, second, importedBase, baseSource)
	c := importingCollector(second, first, second)
	if err := fitsRegion(c); err == nil {
		t.Fatal("the field is accepted before any file has it")
	}
	_, before := ReadFiles(c)
	writeProto(t, first, importedBase, baseWithRegion)
	if err := fitsRegion(c); err != nil {
		t.Fatalf("with the file in the earlier import path: %v", err)
	}
	paths, after := ReadFiles(c)
	if after == before || !slices.Contains(paths, filepath.Join(first, filepath.FromSlash(importedBase))) {
		t.Fatalf("the reading after the file appeared is the one before it: %v", paths)
	}
}

// A descriptor file is stamped before it is read, so one that changes while
// the set is being read is read again at the next reading rather than taken
// for the file that was read: a .proto file rewritten just after it was
// opened, and a descriptor set rewritten just after it was read.
func TestADescriptorFileChangedWhileItIsReadIsReadAgain(t *testing.T) {
	t.Cleanup(func() { descriptorFileRead = nil })
	dir := t.TempDir()
	writeProto(t, dir, importingService, serviceSource)
	writeProto(t, dir, importedTypes, typesSource)
	base := writeProto(t, dir, importedBase, baseSource)
	c := importingCollector(dir, dir)
	rewritten := 0
	descriptorFileRead = func(path string) {
		if path == base && rewritten == 0 {
			rewritten++
			// Another file is put in its place, as a rename does: the one
			// that was opened is read to its end as it was.
			if err := os.Rename(writeProto(t, dir, "next.proto", baseWithRegion), base); err != nil {
				t.Error(err)
			}
		}
	}
	if err := fitsRegion(c); err == nil || rewritten != 1 {
		t.Fatalf("the first reading: %v, the file rewritten %d times", err, rewritten)
	}
	// Whoever asks next finds the reading out of date, and reads again.
	if n := compiles(func() { ReadFiles(c) }); n != 1 {
		t.Fatalf("%d compiles when the file had changed under the reading, want 1", n)
	}
	if err := fitsRegion(c); err != nil {
		t.Fatalf("the reading after the file was rewritten under the first: %v", err)
	}

	set := grpctest.WriteProtoset(t, filepath.Join(t.TempDir(), "queue.pb"), false)
	descriptorFileRead = func(path string) {
		if path == set && rewritten == 1 {
			rewritten++
			writeProto(t, filepath.Dir(set), filepath.Base(set), "no descriptor set")
		}
	}
	if _, err := descriptorFiles.protoset(set); err != nil || rewritten != 2 {
		t.Fatalf("the first reading of the set: %v, rewritten %d times", err, rewritten)
	}
	if _, err := descriptorFiles.protoset(set); err == nil || !strings.Contains(err.Error(), "is not a FileDescriptorSet") {
		t.Fatalf("the reading after the set was rewritten under the first: %v, want it read again and refused", err)
	}
}

// ReadFiles is the type's to answer: a grpc collector that reads a
// descriptor set, asks the reflection service or calls the health service
// reads no file beyond those its request names, and neither does a collector
// of a type that has no such files.
func TestOnlyProtoDescriptorsLeadToFilesTheRequestDoesNotName(t *testing.T) {
	dir := t.TempDir()
	writeProto(t, dir, importingService, serviceSource)
	for name, c := range map[string]*model.Collector{
		"a descriptor set":     {Request: model.RequestConfig{Type: RequestTypeGRPC, Descriptors: descriptorsProtoset, ProtosetFile: filepath.Join(dir, "set.pb")}},
		"reflection":           {Request: model.RequestConfig{Type: RequestTypeGRPC, Descriptors: descriptorsReflection}},
		"the health service":   {Request: model.RequestConfig{Type: RequestTypeGRPC}},
		"no request type":      {Request: model.RequestConfig{Descriptors: descriptorsProto, ProtoFiles: []string{filepath.Join(dir, importingService)}}},
		"an unknown type":      {Request: model.RequestConfig{Type: "carrier-pigeon", ProtoFiles: []string{filepath.Join(dir, importingService)}}},
		"proto files unused":   {Request: model.RequestConfig{Type: RequestTypeGRPC, Descriptors: descriptorsReflection, ProtoFiles: []string{filepath.Join(dir, importingService)}}},
		"proto, no file named": {Request: model.RequestConfig{Type: RequestTypeGRPC, Descriptors: descriptorsProto}},
	} {
		if n := compiles(func() {
			if paths, read := ReadFiles(c); len(paths) != 0 || read != "" {
				t.Errorf("%s: read %v, mark %q", name, paths, read)
			}
		}); n != 0 {
			t.Errorf("%s: %d compiles", name, n)
		}
	}
}

// oldFilesStamp is filesStamp as it was.
func oldFilesStamp(paths []string) string {
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(path)
		if st, err := os.Stat(path); err == nil {
			b.WriteString(":")
			b.WriteString(strconv.FormatInt(st.ModTime().UnixNano(), 10))
			b.WriteString(":")
			b.WriteString(strconv.FormatInt(st.Size(), 10))
		} else {
			b.WriteString(":missing")
		}
		b.WriteByte('|')
	}
	return b.String()
}

// oldCompileProto compiles .proto files as fileSets.proto did, and returns
// the files and the paths it opened.
func oldCompileProto(paths, importPaths []string) (*protoregistry.Files, []string, error) {
	names, dirs, err := protoNames(paths, importPaths)
	if err != nil {
		return nil, nil, err
	}
	read := append([]string(nil), paths...)
	resolver := &protocompile.SourceResolver{
		ImportPaths: dirs,
		Accessor: func(path string) (io.ReadCloser, error) {
			f, err := os.Open(path) //nolint:gosec // G304: the test's own files
			if err == nil && !slices.Contains(read, path) {
				read = append(read, path)
			}
			return f, err
		},
	}
	// One file at a time, so the paths are appended without a lock.
	compiler := protocompile.Compiler{Resolver: protocompile.WithStandardImports(resolver), MaxParallelism: 1}
	compiled, err := compiler.Compile(context.Background(), names...)
	if err != nil {
		return nil, nil, err
	}
	files := new(protoregistry.Files)
	for _, f := range compiled {
		if err := registerFileTree(files, f); err != nil {
			return nil, nil, err
		}
	}
	return files, read, nil
}

// descriptorsOf are the files of a registry as descriptors, by name.
func descriptorsOf(files *protoregistry.Files) map[string][]byte {
	out := map[string][]byte{}
	files.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(protodesc.ToFileDescriptorProto(f))
		if err != nil {
			panic(err)
		}
		out[f.Path()] = raw
		return true
	})
	return out
}

// Compiling .proto files gives what it gave: over the service of the other
// tests and the files of these, with and without import paths, with a file
// outside them, a file that does not compile, an import that is missing, a
// well-known file that is built in and one that is on disk, the files
// compiled are the same descriptors, a compile fails when it did and names
// what it named, and the files opened are the ones that were, with the
// places a file was not found at added. The stamp of files is the same text.
func TestCompilingProtoFilesGivesWhatItGave(t *testing.T) {
	queueDir := t.TempDir()
	queue := grpctest.WriteSources(t, queueDir)
	dir, other, onDisk := t.TempDir(), t.TempDir(), t.TempDir()
	service := writeProto(t, dir, importingService, serviceSource)
	writeProto(t, dir, importedTypes, typesSource)
	writeProto(t, dir, importedBase, baseSource)
	flat := writeProto(t, other, "flat.proto", "syntax = \"proto3\";\npackage flat;\nimport \"beside.proto\";\nmessage M { Beside b = 1; }\n")
	beside := writeProto(t, other, "beside.proto", "syntax = \"proto3\";\npackage flat;\nmessage Beside { string a = 1; }\n")
	broken := writeProto(t, other, "broken.proto", "syntax = \"proto3\";\npackage x;\nmessage M {\n  strin a = 1;\n}\n")
	lonely := writeProto(t, other, "lonely.proto", "syntax = \"proto3\";\npackage x;\nimport \"nowhere/gone.proto\";\nmessage L { Gone g = 1; }\n")
	// A well-known file on disk is the one compiled, as protoc would.
	writeProto(t, onDisk, "google/protobuf/timestamp.proto", "syntax = \"proto3\";\npackage google.protobuf;\nmessage Timestamp { int64 seconds = 1; int32 nanos = 2; string zone = 3; }\n")
	for name, tc := range map[string]struct{ paths, importPaths []string }{
		"the queue service":                  {[]string{queue}, []string{queueDir}},
		"the queue service, no import paths": {[]string{queue}, nil},
		"two levels of imports":              {[]string{service}, []string{dir}},
		"an empty import path first":         {[]string{service}, []string{other, dir}},
		"a file outside the import paths":    {[]string{service}, []string{other}},
		"files beside each other":            {[]string{flat}, nil},
		"two files named":                    {[]string{flat, beside}, nil},
		"two files of two directories":       {[]string{flat, service}, []string{other, dir}},
		"a file that does not compile":       {[]string{broken}, nil},
		"a missing import":                   {[]string{lonely}, []string{other, dir}},
		"a well-known file on disk":          {[]string{service}, []string{onDisk, dir}},
		"a file that is not there":           {[]string{filepath.Join(other, "absent.proto")}, nil},
		"no file":                            {nil, []string{dir}},
	} {
		t.Run(name, func(t *testing.T) {
			wantFiles, wantRead, wantErr := oldCompileProto(tc.paths, tc.importPaths)
			files, looked, stamp, err := compileProto(tc.paths, tc.importPaths)
			if (err == nil) != (wantErr == nil) {
				t.Fatalf("compiled with %v, and before with %v", err, wantErr)
			}
			if err != nil {
				if want := wantErr.Error(); strings.TrimPrefix(err.Error(), "compiling proto_files: ") != want {
					t.Fatalf("the error is %q, and was %q", err, want)
				}
			} else {
				got, want := descriptorsOf(files), descriptorsOf(wantFiles)
				if len(got) != len(want) {
					t.Fatalf("%d files compiled, and %d before", len(got), len(want))
				}
				for path, raw := range want {
					if string(got[path]) != string(raw) {
						t.Errorf("%s is compiled to another descriptor", path)
					}
				}
				// Every file that was opened is looked at, and what is
				// looked at besides is a place where no file is.
				for _, path := range wantRead {
					if !slices.Contains(looked, path) {
						t.Errorf("%s was opened and is not among the paths looked at: %v", path, looked)
					}
				}
				for _, path := range looked {
					if _, statErr := os.Stat(path); !slices.Contains(wantRead, path) && statErr == nil {
						t.Errorf("%s is looked at, exists, and was not opened before", path)
					}
				}
			}
			if !slices.Equal(looked[:len(tc.paths)], tc.paths) || !slices.IsSorted(looked[len(tc.paths):]) {
				t.Errorf("the paths looked at are not the named files and then the others in order: %v", looked)
			}
			for _, path := range looked {
				if strings.Contains(path, "google") && !strings.HasPrefix(path, onDisk) {
					t.Errorf("a well-known file was looked for at %s", path)
				}
			}
			if stamp != filesStamp(looked) || filesStamp(looked) != oldFilesStamp(looked) {
				t.Errorf("the stamp of the paths looked at is\n%s\nthe one of the files now\n%s\nand as it was written before\n%s", stamp, filesStamp(looked), oldFilesStamp(looked))
			}
		})
	}
}

// oldReadFiles is ReadFiles as it was before it took a request.type however
// it is written: it looked the type up as the collector had it.
func oldReadFiles(c *model.Collector) (paths []string, read string) {
	if rt := requestTypeOf(c); rt != nil && rt.ReadFiles != nil {
		return rt.ReadFiles(c)
	}
	return nil, ""
}

// ReadFiles takes a collector's request.type as ValidateRequest names it,
// so the watch can ask about a collector that is not validated yet. Of a
// collector whose type is written as the types are named, which every
// validated one's is, it answers what it answered: over a table of every
// type there is, one there is not and none, with each source of
// descriptors, with and without .proto files and import paths, the paths
// and the mark are the ones it gave. Of the same collector with its type in
// capitals or with blanks around it, where it answered nothing, it answers
// what it does for the type as it is named; and the reading is shared, so
// asking in every spelling compiles the files once.
func TestReadFilesTakesTheTypeAsItIsNamedHoweverItIsWritten(t *testing.T) {
	dir := t.TempDir()
	service := writeProto(t, dir, importingService, serviceSource)
	writeProto(t, dir, importedTypes, typesSource)
	writeProto(t, dir, importedBase, baseSource)
	spellings := func(name string) []string {
		return []string{strings.ToUpper(name), " " + name + "\t", strings.ToUpper(name[:1]) + name[1:] + " "}
	}
	collectors, withFiles := 0, 0
	if n := compiles(func() {
		for _, name := range append(slices.Clone(knownRequestTypes), "carrier-pigeon", "") {
			for _, descriptors := range []string{"", descriptorsProto, descriptorsProtoset, descriptorsReflection, "Proto"} {
				for _, files := range [][]string{nil, {service}} {
					for _, importPaths := range [][]string{nil, {dir}} {
						c := &model.Collector{Name: "c", Request: model.RequestConfig{Type: name, Descriptors: descriptors, ProtoFiles: files, ProtoImportPaths: importPaths}}
						paths, read := ReadFiles(c)
						wantPaths, wantRead := oldReadFiles(c)
						if !slices.Equal(paths, wantPaths) || read != wantRead {
							t.Errorf("type %q, descriptors %q, files %v, import paths %v: read %v with the mark %q; it read %v with %q", name, descriptors, files, importPaths, paths, read, wantPaths, wantRead)
						}
						collectors++
						if len(paths) > 0 {
							withFiles++
						}
						if name == "" {
							continue
						}
						for _, written := range spellings(name) {
							other := *c
							other.Request.Type = written
							if again, mark := ReadFiles(&other); !slices.Equal(again, wantPaths) || mark != wantRead {
								t.Errorf("type written %q, descriptors %q, files %v: read %v with the mark %q, and %v with %q for the type as it is named", written, descriptors, files, again, mark, wantPaths, wantRead)
							}
							if old, mark := oldReadFiles(&other); len(old) != 0 || mark != "" {
								t.Errorf("type written %q: it read %v before", written, old)
							}
						}
					}
				}
			}
		}
	}); n != 2 {
		t.Errorf("%d compiles, want one for the files with their import path and one without", n)
	}
	if collectors < 100 || withFiles < 4 {
		t.Fatalf("the table held %d collectors, %d of them with files read; too few to say", collectors, withFiles)
	}
}

// The compile that tells the watch which files a collector's .proto files
// import, asked for before the collector is validated, is the one its
// validation then uses: a collector written `type: GRPC` is compiled once by
// the two together, and its validation reads no file again.
func TestTheValidationUsesTheCompileThatLearntTheImports(t *testing.T) {
	dir := t.TempDir()
	writeProto(t, dir, importingService, serviceSource)
	writeProto(t, dir, importedTypes, typesSource)
	writeProto(t, dir, importedBase, baseSource)
	c := importingCollector(dir, dir)
	c.Request.Type = "GRPC"
	var paths []string
	if n := compiles(func() { paths, _ = ReadFiles(c) }); n != 1 || len(paths) != 3 {
		t.Fatalf("asking for the files of the collector as written: %d compiles, the files %v; want one compile and the three files", n, paths)
	}
	if n := compiles(func() {
		if err := ValidateRequest(c); err != nil {
			t.Fatal(err)
		}
	}); n != 0 || c.Request.Type != RequestTypeGRPC {
		t.Fatalf("validating the collector compiled %d times after its files were asked for, and its type is %q", n, c.Request.Type)
	}
}
