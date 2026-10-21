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
	return linkedPackagesWith(t, types, "")
}

// linkedPackagesWith is linkedPackages of a build with the sql request
// type's opt-in drivers too, as SQL_DRIVERS names them.
func linkedPackagesWith(t *testing.T, types, drivers string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	tags, err := exec.Command("sh", "tools/request-type-tags.sh", types, drivers).Output()
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

// sshfileOnlyModules are the modules only the sshfile request type needs:
// its SSH client and host key check, and its SFTP client. The rest of
// golang.org/x/crypto is not the type's alone: the sql type's sqlserver
// driver links its MD4, for NTLM.
var sshfileOnlyModules = []string{
	"golang.org/x/crypto/ssh",
	"github.com/pkg/sftp",
}

// Each request type's own modules are linked by a build that carries the
// type and by no other: every build of one type, as REQUEST_TYPES names
// it, links the gRPC and protobuf modules only when the type is grpc, and
// the SSH and SFTP modules only when it is sshfile; a build of graphite,
// http and localfile together links neither, nor does one of dns and
// tlscert, which need nothing outside the standard library; and the
// default build, of every type, links both. go list is asked of the main package, the exporter as it ships;
// the packages only tests import, such as internal/grpctest and
// internal/sshtest, are not among its dependencies.
func TestOnlyTheTypesThatNeedThemLinkTheirModules(t *testing.T) {
	for _, types := range []string{"dns", "graphite", "grpc", "http", "localfile", "sql", "sshfile", "tlscert", "graphite,http,localfile", "dns,tlscert"} {
		packages := linkedPackages(t, types)
		for _, own := range []struct {
			requestType string
			modules     []string
		}{{"grpc", grpcOnlyModules}, {"sshfile", sshfileOnlyModules}} {
			found := linksAny(packages, own.modules)
			switch {
			case types == own.requestType && len(found) == 0:
				t.Errorf("REQUEST_TYPES=%s links none of %s", types, strings.Join(own.modules, ", "))
			case types != own.requestType && len(found) > 0:
				t.Errorf("REQUEST_TYPES=%s links %s, which only the %s type may", types, strings.Join(found, ", "), own.requestType)
			}
		}
	}
	all := linkedPackages(t, "")
	for _, module := range []string{"google.golang.org/grpc", "github.com/bufbuild/protocompile", "golang.org/x/crypto/ssh", "github.com/pkg/sftp"} {
		if len(linksAny(all, []string{module})) == 0 {
			t.Errorf("the default build does not link %s", module)
		}
	}
}
