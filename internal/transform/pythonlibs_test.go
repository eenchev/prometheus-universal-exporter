package transform

import (
	"context"
	"net/http"
	"os/exec"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The image installs lxml, PyYAML and python-dateutil. BeautifulSoup is no
// longer installed: lxml.html parses HTML, and BeautifulSoup could not even be
// imported inside the sandbox, because it imports logging, which imports the
// blocked threading module.

func pythonLibraryCollector(libs ...string) model.Collector {
	return model.Collector{
		Name:      "python",
		Request:   model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Response:  model.ResponseConfig{Format: "text"},
		Transform: model.TransformConfig{Type: "python", Script: `metric(name="v", value=1)`, Libraries: libs},
		Metrics:   []model.MetricRule{},
		Limits:    model.Limits{MaxMetrics: 10},
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
	c.Limits.ScriptTimeout = model.Duration(5e9)
	body := `<html><body><table id="servers"><tr><th>Server</th><th>CPU</th></tr>
<tr><td>web01</td><td>72</td></tr><tr><td>web02</td><td>18.5</td></tr></table></body></html>`
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}
	d := &decode.Decoded{Kind: "text", Data: body, Raw: r.Body}
	set, err := ExecutePython(context.Background(), "python3", c.Transform.Script, d, r, &c)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 || set.Metrics[0].Labels["server"] != "web01" || set.Metrics[0].Value != 72 || set.Metrics[1].Value != 18.5 {
		t.Fatalf("metrics=%#v", set.Metrics)
	}
}
