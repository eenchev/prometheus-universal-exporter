package repository

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// grpcOnlyModules are the modules only the grpc request type needs: a build
// without it must not link any of them, so a binary built with, say,
// REQUEST_TYPES=http carries no gRPC or protobuf code at all.
var grpcOnlyModules = []string{
	"google.golang.org/grpc",
	"google.golang.org/protobuf",
	"google.golang.org/genproto",
	"github.com/bufbuild/protocompile",
}

// linkedPackages lists every package the exporter binary is built from with
// the given request types.
func linkedPackages(t *testing.T, types string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	tags, err := exec.Command("sh", "tools/request-type-tags.sh", types).Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "-tags", strings.TrimSpace(string(tags)), ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	return strings.Fields(string(out))
}

func linksAny(packages, modules []string) []string {
	var found []string
	for _, pkg := range packages {
		for _, module := range modules {
			if pkg == module || strings.HasPrefix(pkg, module+"/") {
				found = append(found, pkg)
			}
		}
	}
	return found
}

// Every build without the grpc type leaves gRPC and protobuf out; a build
// with it alone links them.
func TestOnlyTheGRPCRequestTypeLinksGRPC(t *testing.T) {
	for _, types := range []string{"http", "localfile", "graphite", "graphite,http,localfile"} {
		if found := linksAny(linkedPackages(t, types), grpcOnlyModules); len(found) > 0 {
			t.Errorf("REQUEST_TYPES=%s links %s", types, strings.Join(found, ", "))
		}
	}
	grpc := linkedPackages(t, "grpc")
	for _, module := range []string{"google.golang.org/grpc", "github.com/bufbuild/protocompile"} {
		if len(linksAny(grpc, []string{module})) == 0 {
			t.Errorf("REQUEST_TYPES=grpc does not link %s", module)
		}
	}
}
