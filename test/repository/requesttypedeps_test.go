package repository

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	return linkedPackagesOf(t, types, drivers, "")
}

// linkedPackagesOf is linkedPackagesWith of a build with opt-in request
// types too, as OPT_IN_REQUEST_TYPES names them.
func linkedPackagesOf(t *testing.T, types, drivers, optIn string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	tags, err := exec.Command("sh", "tools/request-type-tags.sh", types, drivers, optIn).Output()
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

// s3fileModules are the modules the s3file request type brings: the AWS
// SDK, its S3 client and the configuration that finds credentials, and
// smithy-go beneath them. The sql type's opt-in snowflake driver links the
// AWS SDK too, its S3 client among it, for its stage transfers; that build
// is the sql drivers' test's to check (TestOnlyTheBuildsWithASQLDriverLinkIt),
// and asks for snowflake, as a build with s3file asks for it.
var s3fileModules = []string{
	"github.com/aws/aws-sdk-go-v2",
	"github.com/aws/smithy-go",
}

// optInRequestTypes are the opt-in request types of the tree: each
// internal/fetch/requesttype_<name>.go built under its own tag alone.
func optInRequestTypes(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("internal/fetch/requesttype_*.go")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "requesttype_"), ".go")
		if !strings.HasSuffix(file, "_test.go") && strings.HasPrefix(read(t, file), "//go:build request_type_"+name+"\n") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		t.Fatal("found no opt-in request type under internal/fetch; the files have moved")
	}
	return names
}

// Each request type's own modules are linked by a build that carries the
// type and by no other: every build of one type, as REQUEST_TYPES names
// it, links the gRPC and protobuf modules only when the type is grpc, the
// SSH and SFTP modules only when it is sshfile, and the AWS SDK only when
// it is s3file; a build of graphite, http and localfile together links none
// of them, nor does one of dns, tlscert and kubernetes, which need nothing
// outside the standard library; and the default build, of every type but the opt-in
// s3file, links gRPC's and SSH's and no AWS SDK, which only a build asking
// for s3file (OPT_IN_REQUEST_TYPES) links. go list is asked of the main package, the exporter as it ships;
// the packages only tests import, such as internal/grpctest and
// internal/sshtest, are not among its dependencies.
func TestOnlyTheTypesThatNeedThemLinkTheirModules(t *testing.T) {
	for _, types := range []string{"dns", "graphite", "grpc", "http", "kubernetes", "localfile", "s3file", "sql", "sshfile", "tlscert", "graphite,http,localfile", "dns,kubernetes,tlscert"} {
		packages := linkedPackages(t, types)
		for _, own := range []struct {
			requestType string
			modules     []string
		}{{"grpc", grpcOnlyModules}, {"sshfile", sshfileOnlyModules}, {"s3file", s3fileModules}} {
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
	if found := linksAny(all, s3fileModules); len(found) > 0 {
		t.Errorf("the default build links %s, which only a build with the opt-in s3file type may", strings.Join(found, ", "))
	}
	for _, build := range []struct{ types, optIn string }{{"", "s3file"}, {"http", "s3file"}} {
		packages := linkedPackagesOf(t, build.types, "", build.optIn)
		for _, module := range []string{"github.com/aws/aws-sdk-go-v2/service/s3", "github.com/aws/aws-sdk-go-v2/config"} {
			if len(linksAny(packages, []string{module})) == 0 {
				t.Errorf("REQUEST_TYPES=%q OPT_IN_REQUEST_TYPES=%s links no %s", build.types, build.optIn, module)
			}
		}
	}
}

// optInTypeBuild is the build of every opt-in request type as ci.yml and
// the Makefile write it.
var optInTypeBuild = regexp.MustCompile(`tools/request-type-tags\.sh '' '' ([a-z0-9,]+)\)`)

// CI and `make ci` vet, build, test and lint a build with every opt-in
// request type beside the default build's types, which no other build
// compiles together: each of their lists names every opt-in type of the
// tree, and ci.yml's lint step, which writes its tags out, lints the tags
// tools/request-type-tags.sh makes of them.
func TestCIBuildsEveryOptInRequestType(t *testing.T) {
	want := optInRequestTypes(t)
	for _, file := range []string{".github/workflows/ci.yml", "Makefile"} {
		text := read(t, file)
		matches := optInTypeBuild.FindAllStringSubmatch(text, -1)
		if len(matches) < 2 {
			t.Errorf("%s builds and tests the opt-in request types %d times, want a build and a test", file, len(matches))
		}
		for _, match := range matches {
			listed := strings.Split(match[1], ",")
			slices.Sort(listed)
			if !slices.Equal(listed, want) {
				t.Errorf("%s builds the opt-in request types %v, but the tree's are %v", file, listed, want)
			}
		}
	}
	tags, err := exec.Command("sh", "tools/request-type-tags.sh", "", "", strings.Join(want, ",")).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, ".github/workflows/ci.yml"), "args: --build-tags "+strings.TrimSpace(string(tags))+"\n") {
		t.Errorf("ci.yml has no golangci-lint step with --build-tags %s, the opt-in request types' tags", strings.TrimSpace(string(tags)))
	}
	makefile := read(t, "Makefile")
	if !regexp.MustCompile(`(?m)^lint-opt-in-types:.*\n\ttags="\$\$\(sh tools/request-type-tags\.sh '' '' [a-z0-9,]+\)" && golangci-lint run --build-tags "\$\$tags"$`).MatchString(makefile) {
		t.Error("the Makefile has no lint-opt-in-types target linting the build with every opt-in request type")
	}
	for _, target := range []string{"build-opt-in-types", "test-opt-in-types", "lint-opt-in-types"} {
		if !regexp.MustCompile(`(?m)^ci:.*\b` + target + `\b`).MatchString(makefile) {
			t.Errorf("make ci does not run %s", target)
		}
	}
}
