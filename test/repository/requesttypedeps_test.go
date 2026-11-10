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

// sshModules are the modules only the types that reach a host over SSH,
// sshfile and sshexec, need: the SSH client and its host key check. The
// rest of golang.org/x/crypto is not theirs alone: the sql type's sqlserver
// driver links its MD4, for NTLM.
var sshModules = []string{
	"golang.org/x/crypto/ssh",
}

// sshfileOnlyModules are the modules only the sshfile request type needs:
// its SFTP client, which sshexec, running commands, does without.
var sshfileOnlyModules = []string{
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
	if files := len(files); files < 10 {
		t.Fatalf("found %d request type files under internal/fetch; the files have moved", files)
	}
	return names
}

// Each request type's own modules are linked by a build that carries the
// type and by no other: every build of one type of the tree, as
// REQUEST_TYPES names it, links the gRPC and protobuf modules only when the
// type is grpc, the SSH modules only when it is sshfile or sshexec, the
// SFTP module only when it is sshfile, and the AWS SDK only when it is
// s3file; a build of graphite, http and localfile together links none
// of them, nor does one of dns, tlscert, ntp and kubernetes, or of redis,
// tcp, ldap or icmp alone, which need nothing outside the standard library
// but, for tlscert, golang.org/x/crypto/ocsp; and the default build, of
// every type, links gRPC's, SSH's and the AWS SDK's S3 client and
// configuration, which a build of every type but s3file does not. go list
// is asked of the main package, the exporter as it ships; the packages only
// tests import, such as internal/grpctest and internal/sshtest, are not
// among its dependencies.
func TestOnlyTheTypesThatNeedThemLinkTheirModules(t *testing.T) {
	for _, types := range append(treeRequestTypes(t), "graphite,http,localfile", "dns,icmp,kubernetes,ldap,ntp,redis,tcp,tlscert") {
		packages := linkedPackages(t, types)
		for _, own := range []struct {
			requestTypes []string
			modules      []string
		}{{[]string{"grpc"}, grpcOnlyModules}, {[]string{"sshexec", "sshfile"}, sshModules}, {[]string{"sshfile"}, sshfileOnlyModules}, {[]string{"s3file"}, s3fileModules}} {
			found := linksAny(packages, own.modules)
			switch {
			case slices.Contains(own.requestTypes, types) && len(found) == 0:
				t.Errorf("REQUEST_TYPES=%s links none of %s", types, strings.Join(own.modules, ", "))
			case !slices.Contains(own.requestTypes, types) && len(found) > 0:
				t.Errorf("REQUEST_TYPES=%s links %s, which only the %s types may", types, strings.Join(found, ", "), strings.Join(own.requestTypes, " and "))
			}
		}
	}
	all := linkedPackages(t, "")
	for _, module := range []string{"google.golang.org/grpc", "github.com/bufbuild/protocompile", "golang.org/x/crypto/ssh", "github.com/pkg/sftp", "github.com/aws/aws-sdk-go-v2/service/s3", "github.com/aws/aws-sdk-go-v2/config"} {
		if len(linksAny(all, []string{module})) == 0 {
			t.Errorf("the default build does not link %s", module)
		}
	}
	var others []string
	for _, typ := range treeRequestTypes(t) {
		if typ != "s3file" {
			others = append(others, typ)
		}
	}
	if found := linksAny(linkedPackages(t, strings.Join(others, ",")), s3fileModules); len(found) > 0 {
		t.Errorf("a build of every type but s3file links %s, which only a build with the s3file type may", strings.Join(found, ", "))
	}
}

// optInTypeBuild is the build of every opt-in request type as ci.yml and
// the Makefile write it, its list empty while the tree has none.
var optInTypeBuild = regexp.MustCompile(`tools/request-type-tags\.sh '' '' ([a-z0-9,]*)\)`)

// optInTypesNone is what a build, test or lint of every opt-in request type
// does first, so that while the tree has none it says so and stops rather
// than repeat the default build's.
const optInTypesNone = `{ [ -n "$tags" ] || { echo "no opt-in request types"; exit 0; }; }`

// CI and `make ci` vet, build, test and lint a build with every opt-in
// request type beside the default build's types, which no other build
// compiles together: each of their lists names every opt-in type of the
// tree, and each stops at once when the list is empty.
func TestCIBuildsEveryOptInRequestType(t *testing.T) {
	want := optInRequestTypes(t)
	for _, c := range []struct {
		file   string
		at     int
		dollar string
	}{{".github/workflows/ci.yml", 3, "$"}, {"Makefile", 4, "$$"}} {
		text := read(t, c.file)
		matches := optInTypeBuild.FindAllStringSubmatch(text, -1)
		if len(matches) < c.at {
			t.Errorf("%s builds, lints and tests the opt-in request types %d times, want %d", c.file, len(matches), c.at)
		}
		for _, match := range matches {
			var listed []string
			if match[1] != "" {
				listed = strings.Split(match[1], ",")
			}
			slices.Sort(listed)
			if !slices.Equal(listed, want) {
				t.Errorf("%s builds the opt-in request types %v, but the tree's are %v", c.file, listed, want)
			}
		}
		for _, run := range []string{`go vet -tags "` + c.dollar + `tags"`, `go test -count=1 -tags "` + c.dollar + `tags" ./test/repository/ ./internal/fetch/`, `golangci-lint run --build-tags "` + c.dollar + `tags"`} {
			guarded := false
			for _, line := range strings.Split(text, "\n") {
				if optInTypeBuild.MatchString(line) && strings.Contains(line, run) && strings.Contains(line, strings.ReplaceAll(optInTypesNone, "$", c.dollar)) {
					guarded = true
				}
			}
			if !guarded {
				t.Errorf("%s has no line of the opt-in request types' build running %s after %s", c.file, run, strings.ReplaceAll(optInTypesNone, "$", c.dollar))
			}
		}
	}
	makefile := read(t, "Makefile")
	if !regexp.MustCompile(`(?m)^lint-opt-in-types:.*\n\ttags="\$\$\(sh tools/request-type-tags\.sh '' '' [a-z0-9,]*\)" && .*golangci-lint run --build-tags "\$\$tags"$`).MatchString(makefile) {
		t.Error("the Makefile has no lint-opt-in-types target linting the build with every opt-in request type")
	}
	for _, target := range []string{"build-opt-in-types", "test-opt-in-types", "lint-opt-in-types"} {
		if !regexp.MustCompile(`(?m)^ci:.*\b` + target + `\b`).MatchString(makefile) {
			t.Errorf("make ci does not run %s", target)
		}
	}
}

// The tcp, snmp and ntp request types, and the opt-in smtp, modbus, mqtt,
// websocket and ftp, link nothing
// outside the standard library: every package a build of one alone links
// beyond those of a build of no type at all is the standard library's or
// the module's own, tcp's client being net and crypto/tls, ntp's net and
// crypto/rand, and snmp's net and the hashes and ciphers of SNMPv3, as are
// the stand-in server of tcp's tests and agent of snmp's in
// internal/testutil, which tests alone import.
func TestTheTCPAndNTPTypesLinkNothingOutsideTheStandardLibrary(t *testing.T) {
	none := linkedPackagesTagged(t, "select_request_types")
	for _, types := range []string{"tcp", "ntp", "snmp", "smtp", "modbus", "mqtt", "websocket", "ftp"} {
		for _, pkg := range linkedPackages(t, types) {
			first, _, _ := strings.Cut(pkg, "/")
			if !slices.Contains(none, pkg) && strings.Contains(first, ".") && !strings.HasPrefix(pkg, "github.com/eenchev/prometheus-universal-exporter/") {
				t.Errorf("REQUEST_TYPES=%s links %s, which a build of no request type does not and which is not the standard library's", types, pkg)
			}
		}
	}
}

// linkedPackagesTagged lists every package the exporter binary is built
// from with the given tags as they are.
func linkedPackagesTagged(t *testing.T, tags string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	cmd := exec.Command("go", "list", "-deps", "-tags", tags, ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	return strings.Fields(string(out))
}
