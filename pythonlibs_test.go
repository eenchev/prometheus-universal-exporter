package main

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The image installs lxml, PyYAML and python-dateutil. BeautifulSoup is no
// longer installed: lxml.html parses HTML, and BeautifulSoup could not even be
// imported inside the sandbox, because it imports logging, which imports the
// blocked threading module.

func pythonLibraryCollector(libs ...string) Collector {
	return Collector{
		Name:      "python",
		Request:   RequestConfig{Type: RequestTypeHTTP},
		Response:  ResponseConfig{Format: "text"},
		Transform: TransformConfig{Type: "python", Script: `metric(name="v", value=1)`, Libraries: libs},
		Metrics:   []MetricRule{},
		Limits:    Limits{MaxMetrics: 10},
	}
}

func TestSupportedPythonLibrariesValidate(t *testing.T) {
	for _, lib := range []string{"lxml", "PyYAML", "yaml", "python-dateutil", "dateutil"} {
		cfg := Config{Collectors: []Collector{pythonLibraryCollector(lib)}}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: %v", lib, err)
		}
	}
}

func TestBeautifulSoupIsRejectedWithAPointerToLxml(t *testing.T) {
	for _, lib := range []string{"beautifulsoup4", "bs4"} {
		cfg := Config{Collectors: []Collector{pythonLibraryCollector("lxml", lib)}}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "no longer installs") || !strings.Contains(err.Error(), "lxml.html") {
			t.Errorf("%s: err=%v", lib, err)
		}
	}
	cfg := Config{Collectors: []Collector{pythonLibraryCollector("requests")}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `unsupported Python library "requests"; the supported libraries are lxml, PyYAML and python-dateutil`) {
		t.Errorf("err=%v", err)
	}
}

// lxml.html is what replaces BeautifulSoup, so it has to work inside the
// sandbox, not merely be installed.
func TestLxmlHTMLParsesInsideThePythonSandbox(t *testing.T) {
	if err := exec.Command("python3", "-c", "import lxml.html").Run(); err != nil {
		t.Skip("python3 with lxml is not available")
	}
	c := pythonLibraryCollector("lxml")
	c.Transform.Script = `
import lxml.html
doc = lxml.html.fromstring(response.text)
for row in doc.xpath('//table[@id="servers"]//tr[td]'):
    name, cpu = [cell.text_content().strip() for cell in row.xpath('./td')]
    metric(name="server_cpu", value=float(cpu), labels={"server": name})
`
	c.Limits.ScriptTimeout = Duration(5e9)
	body := `<html><body><table id="servers"><tr><th>Server</th><th>CPU</th></tr>
<tr><td>web01</td><td>72</td></tr><tr><td>web02</td><td>18.5</td></tr></table></body></html>`
	r := &HTTPResponse{Body: []byte(body), Headers: http.Header{}}
	d := &Decoded{Kind: "text", Data: body, Raw: r.Body}
	set, err := executePython(context.Background(), "python3", c.Transform.Script, d, r, &c)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 || set.Metrics[0].Labels["server"] != "web01" || set.Metrics[0].Value != 72 || set.Metrics[1].Value != 18.5 {
		t.Fatalf("metrics=%#v", set.Metrics)
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
