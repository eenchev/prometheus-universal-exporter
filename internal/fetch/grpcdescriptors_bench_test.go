//go:build !select_request_types || request_type_grpc

package fetch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/reflect/protoregistry"
)

// stampBenchFiles writes n descriptor files under dir and returns their
// paths, in one of two shapes: deep, each file at a realistic depth,
// /etc/exporter/protos/acme/queue/v1/fK.proto under dir; and kubernetes,
// each file a symbolic link protos/fK.proto to ..data/fK.proto, ..data a
// link to the directory that holds them, as a mounted ConfigMap is laid out.
func stampBenchFiles(tb testing.TB, dir, shape string, n int) []string {
	tb.Helper()
	paths := make([]string, n)
	for i := range paths {
		name := fmt.Sprintf("f%d.proto", i)
		switch shape {
		case "deep":
			paths[i] = filepath.Join(dir, "etc", "exporter", "protos", "acme", "queue", "v1", name)
			if err := os.MkdirAll(filepath.Dir(paths[i]), 0o750); err != nil {
				tb.Fatal(err)
			}
			if err := os.WriteFile(paths[i], []byte("syntax = \"proto3\";\n"), 0o600); err != nil {
				tb.Fatal(err)
			}
		case "kubernetes":
			protos := filepath.Join(dir, "etc", "protos")
			version := filepath.Join(protos, "..2026_10_08_00_00_00.1")
			if err := os.MkdirAll(version, 0o750); err != nil {
				tb.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(version, name), []byte("syntax = \"proto3\";\n"), 0o600); err != nil {
				tb.Fatal(err)
			}
			if i == 0 {
				if err := os.Symlink(filepath.Base(version), filepath.Join(protos, "..data")); err != nil {
					tb.Fatal(err)
				}
			}
			paths[i] = filepath.Join(protos, name)
			if err := os.Symlink(filepath.Join("..data", name), paths[i]); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return paths
}

// BenchmarkDescriptorFilesCheck is what a call of a grpc collector with
// descriptor files costs before it uses them, when they did not change:
// the check that each file the set was read from is as it was stamped
// (fileSets.getStamped), for a set of 1, 10 and 50 files in both shapes of
// stampBenchFiles.
func BenchmarkDescriptorFilesCheck(b *testing.B) {
	for _, shape := range []string{"deep", "kubernetes"} {
		for _, n := range []int{1, 10, 50} {
			b.Run(fmt.Sprintf("%s/files=%d", shape, n), func(b *testing.B) {
				paths := stampBenchFiles(b, b.TempDir(), shape, n)
				sets := &fileSets{slots: map[string]*fileSetSlot{}}
				files := new(protoregistry.Files)
				read := func() (*protoregistry.Files, []string, string, error) {
					return files, paths, filesStamp(paths), nil
				}
				if _, err := sets.getStamped("set", read); err != nil {
					b.Fatal(err)
				}
				reads := 0
				counted := func() (*protoregistry.Files, []string, string, error) {
					reads++
					return read()
				}
				b.ReportAllocs()
				for b.Loop() {
					if _, err := sets.getStamped("set", counted); err != nil {
						b.Fatal(err)
					}
				}
				if reads != 0 {
					b.Fatalf("the set was read again %d times", reads)
				}
			})
		}
	}
}
