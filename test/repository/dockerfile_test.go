package repository

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The released image carries its version and revision: the build stage has
// no git, and .dockerignore keeps .git out, so the revision is passed in.
func TestDockerfileSetsTheVersion(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	for _, want := range []string{"ARG VERSION\n", "-X main.version=${VERSION}", "ARG REVISION\n", "-X main.revision=${REVISION}"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the Dockerfile is missing %q", want)
		}
	}
	release, err := os.ReadFile(".github/workflows/release.yml")
	if err == nil && !strings.Contains(string(release), "REVISION=${{ github.sha }}") {
		t.Error("the release workflow does not pass the image its revision")
	}
	ignore, err := os.ReadFile(".dockerignore")
	if err != nil || !strings.Contains(string(ignore), "\n.git\n") {
		t.Errorf(".dockerignore does not keep .git out of the build context: %v", err)
	}
}

// The image carries no BeautifulSoup and no pip, and its lxml is at or above
// 6.1.0, the first release without CVE-2026-41066.
func TestDockerfileImageContents(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	dockerfile := string(raw)
	if strings.Contains(strings.ToLower(dockerfile), "beautifulsoup") {
		t.Error("the Dockerfile still installs BeautifulSoup")
	}
	if !strings.Contains(dockerfile, "pip uninstall -y pip") {
		t.Error("the Dockerfile no longer removes pip from the runtime image")
	}
	if !strings.Contains(dockerfile, "apt-get upgrade") {
		t.Error("the Dockerfile no longer applies Debian security updates at build time")
	}
	match := regexp.MustCompile(`(?m)^ARG LXML_VERSION=(\d+)\.(\d+)`).FindStringSubmatch(dockerfile)
	if match == nil {
		t.Fatal("the Dockerfile no longer pins LXML_VERSION")
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	if major < 6 || major == 6 && minor < 1 {
		t.Errorf("LXML_VERSION %s.%s is older than 6.1.0, which fixes CVE-2026-41066", match[1], match[2])
	}
}

// The image names its user by number, and the chart runs the pod as the same
// user: Kubernetes can verify runAsNonRoot only for a numeric user, so an
// image whose USER is a name would not start under the chart's defaults.
func TestTheImageUserIsNumeric(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	users := regexp.MustCompile(`(?m)^USER\s+(\S+)\s*$`).FindAllStringSubmatch(string(raw), -1)
	if len(users) == 0 {
		t.Fatal("the Dockerfile sets no USER, so the image runs as root")
	}
	last := users[len(users)-1][1]
	if !regexp.MustCompile(`^[1-9][0-9]*(:[0-9]+)?$`).MatchString(last) {
		t.Fatalf("the image's USER %q is not a non-zero number, which runAsNonRoot cannot verify", last)
	}
	uid, _, _ := strings.Cut(last, ":")
	values := readChartFile(t, "values.yaml")
	for _, key := range []string{"runAsUser", "runAsGroup", "fsGroup"} {
		if !strings.Contains(values, "\n  "+key+": "+uid+"\n") {
			t.Errorf("podSecurityContext.%s is not the image's user, %s", key, uid)
		}
	}
}

// tools/request-type-tags.sh turns REQUEST_TYPES, SQL_DRIVERS and
// OPT_IN_REQUEST_TYPES into build tags for the Dockerfile and the Makefile.
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
		"graphite":       "select_request_types,request_type_graphite",
		"grpc":           "select_request_types,request_type_grpc",
		"http,grpc":      "select_request_types,request_type_http,request_type_grpc",
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
	// A second list adds the sql type's opt-in drivers: to every type, or
	// to a build whose types include sql, snowflake with gosnowflake's
	// minicore_disabled, once. A default driver, one that does not exist and
	// drivers for a build without sql are refused.
	withDrivers := func(types, drivers string) (string, string, error) {
		cmd := exec.Command("sh", "tools/request-type-tags.sh", types, drivers)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return strings.TrimSpace(stdout.String()), stderr.String(), err
	}
	for lists, want := range map[[2]string]string{
		{"", ""}:                        "",
		{"", "oracle"}:                  "sql_driver_oracle",
		{"", "oracle,snowflake,oracle"}: "sql_driver_oracle,sql_driver_snowflake,minicore_disabled",
		{"", "snowflake,snowflake"}:     "sql_driver_snowflake,minicore_disabled",
		{"sql", "snowflake"}:            "select_request_types,request_type_sql,sql_driver_snowflake,minicore_disabled",
		{"sql", "vertica"}:              "select_request_types,request_type_sql,sql_driver_vertica",
		{"http,sql", "clickhouse"}:      "select_request_types,request_type_http,request_type_sql,sql_driver_clickhouse",
		{"http", ""}:                    "select_request_types,request_type_http",
	} {
		got, stderr, err := withDrivers(lists[0], lists[1])
		if err != nil || got != want {
			t.Errorf("%q: got %q err=%v %s, want %q", lists, got, err, stderr, want)
		}
	}
	for lists, want := range map[[2]string]string{
		{"", "mysql"}:      `the sql driver "mysql" is built in with the sql request type and needs no SQL_DRIVERS entry`,
		{"", "db2"}:        `no sql driver "db2"`,
		{"http", "oracle"}: `SQL drivers "oracle" need the sql request type, which the request types "http" leave out`,
	} {
		if got, stderr, err := withDrivers(lists[0], lists[1]); err == nil || !strings.Contains(stderr, want) {
			t.Errorf("%q: got %q err=%v stderr=%q, want %q", lists, got, err, stderr, want)
		}
	}
	// A third list adds the opt-in request types, which no build carries
	// unless asked: to the default build's types, or to those REQUEST_TYPES
	// lists, which may name one too. A type of every build, and one that
	// does not exist, are refused.
	withOptIn := func(types, drivers, optIn string) (string, string, error) {
		cmd := exec.Command("sh", "tools/request-type-tags.sh", types, drivers, optIn)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return strings.TrimSpace(stdout.String()), stderr.String(), err
	}
	for lists, want := range map[[3]string]string{
		{"", "", "s3file"}:                 "request_type_s3file",
		{"", "", "s3file,s3file"}:          "request_type_s3file",
		{"http", "", "s3file"}:             "select_request_types,request_type_http,request_type_s3file",
		{"s3file", "", ""}:                 "select_request_types,request_type_s3file",
		{"s3file", "", "s3file"}:           "select_request_types,request_type_s3file",
		{"", "snowflake", "s3file"}:        "sql_driver_snowflake,minicore_disabled,request_type_s3file",
		{"sql,s3file", "oracle", "s3file"}: "select_request_types,request_type_sql,request_type_s3file,sql_driver_oracle",
		{"", "", "redis"}:                  "request_type_redis",
		{"", "", "s3file,redis"}:           "request_type_s3file,request_type_redis",
		{"redis", "", ""}:                  "select_request_types,request_type_redis",
		{"http", "", "redis,s3file"}:       "select_request_types,request_type_http,request_type_redis,request_type_s3file",
		{"", "", "tcp"}:                    "request_type_tcp",
		{"", "", "s3file,redis,tcp"}:       "request_type_s3file,request_type_redis,request_type_tcp",
		{"tcp", "", ""}:                    "select_request_types,request_type_tcp",
		{"", "", "ldap"}:                   "request_type_ldap",
		{"", "", "s3file,redis,tcp,ldap"}:  "request_type_s3file,request_type_redis,request_type_tcp,request_type_ldap",
		{"ldap", "", ""}:                   "select_request_types,request_type_ldap",
	} {
		got, stderr, err := withOptIn(lists[0], lists[1], lists[2])
		if err != nil || got != want {
			t.Errorf("%q: got %q err=%v %s, want %q", lists, got, err, stderr, want)
		}
	}
	for optIn, want := range map[string]string{
		"http":  `the request type "http" is in every build and needs no OPT_IN_REQUEST_TYPES entry`,
		"s3":    `no opt-in request type "s3"`,
		"none":  `no opt-in request type "none"`,
		"s3fil": `no opt-in request type "s3fil"`,
	} {
		if got, stderr, err := withOptIn("", "", optIn); err == nil || !strings.Contains(stderr, want) {
			t.Errorf("%q: got %q err=%v stderr=%q, want %q", optIn, got, err, stderr, want)
		}
	}
}

func TestDockerfileBuildsTheSelectedRequestTypes(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	for _, want := range []string{"ARG REQUEST_TYPES\n", "ARG SQL_DRIVERS\n", "ARG OPT_IN_REQUEST_TYPES\n", `sh tools/request-type-tags.sh "${REQUEST_TYPES}" "${SQL_DRIVERS}" "${OPT_IN_REQUEST_TYPES}"`, `-tags "${tags}"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the Dockerfile is missing %q", want)
		}
	}
}

// The chart README says which Python the image's interpreter is, by its base
// image, so readers know what their scripts run on. The Dockerfile's
// PYTHON_VERSION is updated by a workflow, in a pull request that touches
// nothing else: this fails that pull request until the README says the same.
func TestChartReadmeNamesTheImagesPython(t *testing.T) {
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	match := regexp.MustCompile(`(?m)^ARG PYTHON_VERSION=(3\.[0-9]+)$`).FindSubmatch(dockerfile)
	if match == nil {
		t.Fatal("the Dockerfile no longer pins PYTHON_VERSION as 3.MINOR")
	}
	if !bytes.Contains(dockerfile, []byte("\nFROM python:${PYTHON_VERSION}-slim\n")) {
		t.Fatal("the image is no longer built on python:${PYTHON_VERSION}-slim, which the chart README names")
	}
	readme := readChartFile(t, "README.md")
	named := regexp.MustCompile("`python:([0-9][0-9.]*)-slim`").FindAllStringSubmatch(readme, -1)
	if len(named) == 0 {
		t.Fatal("the chart README no longer names the image's Python base; this test must not pass by finding nothing")
	}
	for _, name := range named {
		if name[1] != string(match[1]) {
			t.Errorf("the chart README names python:%s-slim, but the Dockerfile pins PYTHON_VERSION=%s", name[1], match[1])
		}
	}
}

// Every RUN after an ARG depends on the argument's value, so a build argument
// declared before the module download makes go mod download miss the layer
// cache whenever its value changes: REVISION changes with every commit. The
// build stage declares its arguments after the module download.
func TestDockerfileBuildArgumentsFollowTheModuleDownload(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	inBuild, downloaded := false, false
	declared := map[string]bool{}
	for i, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			if inBuild {
				inBuild = false
				continue
			}
			inBuild = strings.EqualFold(fields[len(fields)-1], "build")
		case "ARG":
			if !inBuild || len(fields) < 2 {
				continue
			}
			name, _, _ := strings.Cut(fields[1], "=")
			declared[name] = true
			if !downloaded {
				t.Errorf("Dockerfile:%d: the build stage declares ARG %s before go mod download, which then depends on it", i+1, name)
			}
		case "RUN":
			if inBuild && strings.Contains(line, "go mod download") {
				downloaded = true
			}
		}
	}
	if !downloaded {
		t.Fatal("the Dockerfile's build stage no longer runs go mod download")
	}
	for _, name := range []string{"VERSION", "REVISION", "TARGETOS", "TARGETARCH"} {
		if !declared[name] {
			t.Errorf("the build stage no longer declares ARG %s", name)
		}
	}
}
