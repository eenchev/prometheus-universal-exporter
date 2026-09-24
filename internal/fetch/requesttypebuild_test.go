package fetch

import (
	"go/build"
	"go/build/constraint"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Request types are chosen at build time. A default build carries every type;
// -tags select_request_types,request_type_<name>,... carries only the named
// ones. Each type lives in requesttype_<name>.go behind
// `//go:build !select_request_types || request_type_<name>`, and
// requesttype_none.go fails a selective build that names no type at all.

const requestTypeGuardFile = "requesttype_none.go"

// requestTypeFiles returns the type name of every requesttype_<name>.go.
func requestTypeFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("requesttype_*.go")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == requestTypeGuardFile {
			continue
		}
		names = append(names, strings.TrimSuffix(strings.TrimPrefix(file, "requesttype_"), ".go"))
	}
	sort.Strings(names)
	return names
}

func buildConstraint(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if constraint.IsGoBuild(line) {
			return line
		}
	}
	t.Fatalf("%s has no //go:build line", file)
	return ""
}

// Every type's file carries exactly the constraint that makes the scheme work,
// and knownRequestTypes names exactly the types that have a file.
func TestEveryRequestTypeFileHasItsSelectionConstraint(t *testing.T) {
	names := requestTypeFiles(t)
	known := append([]string(nil), knownRequestTypes...)
	sort.Strings(known)
	if !reflect.DeepEqual(names, known) {
		t.Fatalf("request type files %v, but knownRequestTypes is %v", names, known)
	}
	for _, name := range names {
		file := "requesttype_" + name + ".go"
		want := "//go:build !select_request_types || request_type_" + name
		if got := buildConstraint(t, file); got != want {
			t.Errorf("%s: constraint %q, want %q", file, got, want)
		}
	}
}

// The guard must exclude itself as soon as any one type is selected, so its
// constraint names every type.
func TestTheGuardNamesEveryRequestType(t *testing.T) {
	var want strings.Builder
	want.WriteString("//go:build select_request_types")

	for _, name := range requestTypeFiles(t) {
		want.WriteString(" && !request_type_")
		want.WriteString(name)
	}

	if got := buildConstraint(t, requestTypeGuardFile); got != want.String() {
		t.Fatalf("%s: constraint %q, want %q", requestTypeGuardFile, got, want.String())
	}
}

// Which files each tag set compiles, evaluated with the Go toolchain's own
// constraint rules.
func TestBuildTagsSelectTheRequestTypeFiles(t *testing.T) {
	included := func(tags ...string) map[string]bool {
		ctx := build.Default
		ctx.BuildTags = tags
		out := map[string]bool{}
		files, _ := filepath.Glob("requesttype_*.go")
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			ok, err := ctx.MatchFile(".", file)
			if err != nil {
				t.Fatal(err)
			}
			out[file] = ok
		}
		return out
	}
	all := included()
	for _, name := range knownRequestTypes {
		if !all["requesttype_"+name+".go"] {
			t.Errorf("a default build leaves out %s", name)
		}
	}
	if all[requestTypeGuardFile] {
		t.Error("a default build compiles the guard, so it cannot build")
	}
	onlyHTTP := included("select_request_types", "request_type_http")
	if !onlyHTTP["requesttype_http.go"] || onlyHTTP["requesttype_localfile.go"] || onlyHTTP[requestTypeGuardFile] {
		t.Errorf("-tags select_request_types,request_type_http compiles %v", onlyHTTP)
	}
	onlyFiles := included("select_request_types", "request_type_localfile")
	if !onlyFiles["requesttype_localfile.go"] || onlyFiles["requesttype_http.go"] || onlyFiles[requestTypeGuardFile] {
		t.Errorf("-tags select_request_types,request_type_localfile compiles %v", onlyFiles)
	}
	none := included("select_request_types")
	if !none[requestTypeGuardFile] || none["requesttype_http.go"] {
		t.Errorf("-tags select_request_types alone compiles %v; it should compile only the guard", none)
	}
}

// Tests run in a default build, which carries every type.
func TestADefaultBuildRegistersEveryRequestType(t *testing.T) {
	known := append([]string(nil), knownRequestTypes...)
	sort.Strings(known)
	if got := BuiltRequestTypes(); !reflect.DeepEqual(got, known) {
		t.Fatalf("built %v, want every known type %v", got, known)
	}
}

func TestRegisteringARequestTypeTwicePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a duplicate registration must panic")
		}
	}()
	registerRequestType(&RequestType{Name: RequestTypeHTTP})
}

// A type's own tests are compiled only with the type, so every single-type
// selection vets with its tests, as CI does.
func TestRequestTypeTestFilesCarryTheirTypesConstraint(t *testing.T) {
	for _, name := range requestTypeFiles(t) {
		file := "requesttype_" + name + "_test.go"
		if _, err := os.Stat(file); err != nil {
			continue
		}
		want := "//go:build !select_request_types || request_type_" + name
		if got := buildConstraint(t, file); got != want {
			t.Errorf("%s: constraint %q, want %q", file, got, want)
		}
	}
}
