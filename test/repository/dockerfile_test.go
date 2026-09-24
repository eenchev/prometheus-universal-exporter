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

// The released image carries its version.
func TestDockerfileSetsTheVersion(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	for _, want := range []string{"ARG VERSION\n", "-X main.version=${VERSION}"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the Dockerfile is missing %q", want)
		}
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
		"graphite":       "select_request_types,request_type_graphite",
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
