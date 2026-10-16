//go:build !select_request_types

package repository

import (
	"maps"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// sqlDefaultModules are the drivers every build with the sql request type
// links, and sqlOptInModules, by driver, what each opt-in driver brings:
// its own module, and, for snowflake, the AWS and Azure SDKs and Apache
// Arrow, which no other build may carry.
var (
	sqlDefaultModules = []string{"github.com/go-sql-driver/mysql", "github.com/jackc/pgx/v5", "github.com/microsoft/go-mssqldb"}
	sqlOptInModules   = map[string][]string{
		"oracle":     {"github.com/sijms/go-ora/v2"},
		"clickhouse": {"github.com/ClickHouse/clickhouse-go/v2", "github.com/ClickHouse/ch-go"},
		"snowflake":  {"github.com/snowflakedb/gosnowflake", "github.com/aws/aws-sdk-go-v2", "github.com/Azure/azure-sdk-for-go", "github.com/apache/arrow-go"},
		"vertica":    {"github.com/vertica/vertica-sql-go"},
	}
)

// The database drivers are linked only by the builds that carry them: a
// build without the sql type links none, a build with it links the three
// default drivers and none of the opt-in ones, the default build included,
// and a build with an opt-in driver, as SQL_DRIVERS names it, links that
// driver's modules and no other's. sqlOptInModules names every opt-in
// driver of the tree, a sqldriver_<name>.go built only with its
// sql_driver_<name> tag.
func TestOnlyTheBuildsWithASQLDriverLinkIt(t *testing.T) {
	var optIn []string
	for _, modules := range sqlOptInModules {
		optIn = append(optIn, modules...)
	}
	everyDriver := append(slices.Clone(sqlDefaultModules), optIn...)
	for _, types := range []string{"graphite", "grpc", "http", "localfile", "sshfile", "graphite,grpc,http,localfile,sshfile"} {
		if found := linksAny(linkedPackages(t, types), everyDriver); len(found) > 0 {
			t.Errorf("REQUEST_TYPES=%s links %s, which only a build with the sql type may", types, strings.Join(found, ", "))
		}
	}
	for _, types := range []string{"sql", ""} {
		packages := linkedPackages(t, types)
		for _, module := range sqlDefaultModules {
			if len(linksAny(packages, []string{module})) == 0 {
				t.Errorf("REQUEST_TYPES=%q links no %s", types, module)
			}
		}
		if found := linksAny(packages, optIn); len(found) > 0 {
			t.Errorf("REQUEST_TYPES=%q without SQL_DRIVERS links %s", types, strings.Join(found, ", "))
		}
	}
	for driver, modules := range sqlOptInModules {
		packages := linkedPackagesWith(t, "sql", driver)
		for _, module := range modules {
			if len(linksAny(packages, []string{module})) == 0 {
				t.Errorf("SQL_DRIVERS=%s links no %s", driver, module)
			}
		}
		for other, theirs := range sqlOptInModules {
			if found := linksAny(packages, theirs); other != driver && len(found) > 0 {
				t.Errorf("SQL_DRIVERS=%s links %s, which only the %s driver may", driver, strings.Join(found, ", "), other)
			}
		}
	}
	files, err := filepath.Glob("internal/fetch/sqldriver_*.go")
	if err != nil {
		t.Fatal(err)
	}
	var inTree []string
	for _, file := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "sqldriver_"), ".go")
		if !strings.HasSuffix(file, "_test.go") && strings.HasPrefix(read(t, file), "//go:build (!select_request_types || request_type_sql) && sql_driver_"+name+"\n") {
			inTree = append(inTree, name)
		}
	}
	slices.Sort(inTree)
	if listed := slices.Sorted(maps.Keys(sqlOptInModules)); !slices.Equal(inTree, listed) {
		t.Errorf("the tree's opt-in sql drivers are %v, and the test knows %v", inTree, listed)
	}
}

// optInDriverBuild is the build of every opt-in sql driver as ci.yml and
// the Makefile write it.
var optInDriverBuild = regexp.MustCompile(`tools/request-type-tags\.sh '' ([a-z,]+)\)`)

// CI and `make ci` vet, build and test a build with every opt-in sql
// driver, which no other build compiles: each of their lists names every
// opt-in driver of the tree.
func TestCIBuildsEveryOptInSQLDriver(t *testing.T) {
	want := slices.Sorted(maps.Keys(sqlOptInModules))
	for _, file := range []string{".github/workflows/ci.yml", "Makefile"} {
		matches := optInDriverBuild.FindAllStringSubmatch(read(t, file), -1)
		if len(matches) < 2 {
			t.Errorf("%s builds and tests the opt-in sql drivers %d times, want a build and a test", file, len(matches))
		}
		for _, match := range matches {
			listed := strings.Split(match[1], ",")
			slices.Sort(listed)
			if !slices.Equal(listed, want) {
				t.Errorf("%s builds the sql drivers %v, but the tree's opt-in drivers are %v", file, listed, want)
			}
		}
	}
}

// The build with every opt-in sql driver is linted too, in CI and by `make
// ci`: no other lint run compiles the drivers' files, and a build with more
// packages can give gosec's analyses more to see. ci.yml writes the tags out
// for its lint action, so they must be what tools/request-type-tags.sh makes
// of the tree's opt-in drivers, minicore_disabled with snowflake's included.
func TestCILintsTheBuildWithEveryOptInSQLDriver(t *testing.T) {
	drivers := slices.Sorted(maps.Keys(sqlOptInModules))
	tags, err := exec.Command("sh", "tools/request-type-tags.sh", "", strings.Join(drivers, ",")).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Split(strings.TrimSpace(string(tags)), ",")
	if !slices.Contains(want, "minicore_disabled") {
		t.Errorf("tools/request-type-tags.sh makes %v of the opt-in drivers, without snowflake's minicore_disabled", want)
	}
	lint := regexp.MustCompile(`(?m)golangci/golangci-lint-action@\S+\n\s+with:\n(?:\s+\w+:.*\n)*?\s+args:\s*--build-tags\s+(\S+)`).FindStringSubmatch(read(t, ".github/workflows/ci.yml"))
	if lint == nil {
		t.Fatal("ci.yml has no golangci-lint step with --build-tags for the opt-in sql drivers")
	}
	listed := strings.Split(lint[1], ",")
	slices.Sort(listed)
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Errorf("ci.yml lints the build tags %v, want the opt-in drivers' %v", listed, want)
	}
	makefile := read(t, "Makefile")
	if !regexp.MustCompile(`(?m)^lint-sql-drivers:.*\n\ttags="\$\$\(sh tools/request-type-tags\.sh '' [a-z,]+\)" && golangci-lint run --build-tags "\$\$tags"$`).MatchString(makefile) {
		t.Error("the Makefile has no lint-sql-drivers target linting the build with every opt-in sql driver")
	}
	if !regexp.MustCompile(`(?m)^ci:.*\blint-sql-drivers\b`).MatchString(makefile) {
		t.Error("make ci does not run lint-sql-drivers")
	}
}

// The tests that compare the committed configuration and collector file
// schemas with the ones the code generates run in the build make schemas
// generates them in, with the sql type's default drivers alone: a build
// with an opt-in driver generates a schema that names it, as its loader
// takes it, which the agreement tests hold it to (builtSchema). So the
// constraint of their file leaves out the tag of every opt-in driver of the
// tree, and a driver added to the tree is left out there too.
func TestTheCommittedSchemaComparisonsRunInTheDefaultBuild(t *testing.T) {
	terms := []string{"//go:build !select_request_types"}
	for _, driver := range slices.Sorted(maps.Keys(sqlOptInModules)) {
		terms = append(terms, "!sql_driver_"+driver)
	}
	want := strings.Join(terms, " && ")
	if first, _, _ := strings.Cut(read(t, "test/repository/schema_fullbuild_test.go"), "\n"); first != want {
		t.Errorf("test/repository/schema_fullbuild_test.go is built under %q, want %q", first, want)
	}
}
