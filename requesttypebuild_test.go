package main

import (
	"bytes"
	"go/build"
	"go/build/constraint"
	"os"
	"os/exec"
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
	if got := builtRequestTypes(); !reflect.DeepEqual(got, known) {
		t.Fatalf("built %v, want every known type %v", got, known)
	}
}

// A configuration naming a real type that this build left out is told so, and
// told how to get a build with it; a name that is no type at all is not.
func TestARequestTypeLeftOutOfTheBuildIsNamedAsSuch(t *testing.T) {
	saved := requestTypes[RequestTypeHTTP]
	delete(requestTypes, RequestTypeHTTP)
	registerFixtureType(t)
	t.Cleanup(func() { requestTypes[RequestTypeHTTP] = saved })

	err := (&Config{Collectors: []Collector{typedCollector(RequestTypeHTTP)}}).Validate()
	if err == nil {
		t.Fatal("a type missing from the build must be rejected")
	}
	for _, want := range []string{`request.type "http"`, "does not include", "built with: fixture", "request_type_http"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	err = (&Config{Collectors: []Collector{typedCollector("gopher")}}).Validate()
	if err == nil || !strings.Contains(err.Error(), `unsupported request.type "gopher"`) {
		t.Errorf("err=%v", err)
	}
}

func TestRegisteringARequestTypeTwicePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a duplicate registration must panic")
		}
	}()
	registerRequestType(&requestType{Name: RequestTypeHTTP})
}

// The dry-run report says which types the binary has, so a configuration can
// be checked against the build that will run it.
func TestDryRunReportListsTheBuiltRequestTypes(t *testing.T) {
	report := checkStartup(checkInputs{ConfigFile: filepath.Join(t.TempDir(), "missing.yaml")})
	if !reflect.DeepEqual(report.RequestTypes, builtRequestTypes()) {
		t.Fatalf("report lists %v, want %v", report.RequestTypes, builtRequestTypes())
	}
}

// tools/request-type-tags.sh turns REQUEST_TYPES into build tags for the
// Dockerfile and the Makefile.
func TestRequestTypeTagsScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	run := func(list string) (string, string, error) {
		cmd := exec.Command("sh", "tools/request-type-tags.sh", list)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return strings.TrimSpace(stdout.String()), stderr.String(), err
	}
	for list, want := range map[string]string{
		"":               "",
		"http":           "select_request_types,request_type_http",
		"http,http":      "select_request_types,request_type_http",
		" http , ":       "select_request_types,request_type_http",
		"http,http ":     "select_request_types,request_type_http",
		"localfile":      "select_request_types,request_type_localfile",
		"localfile,http": "select_request_types,request_type_localfile,request_type_http",
	} {
		got, stderr, err := run(list)
		if err != nil || got != want {
			t.Errorf("%q: got %q err=%v %s, want %q", list, got, err, stderr, want)
		}
	}
	for _, list := range []string{"htp", "http,grpcc", "none", "test"} {
		if got, stderr, err := run(list); err == nil || !strings.Contains(stderr, "no request type") {
			t.Errorf("%q: got %q err=%v stderr=%q, want it refused", list, got, err, stderr)
		}
	}
}

func TestDockerfileBuildsTheSelectedRequestTypes(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	for _, want := range []string{"ARG REQUEST_TYPES\n", `sh tools/request-type-tags.sh "${REQUEST_TYPES}"`, `-tags "${tags}"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the Dockerfile is missing %q", want)
		}
	}
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
