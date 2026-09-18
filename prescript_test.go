package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const workerReshapeScript = `import re
data = {"workers": [{"name": m.group(1), "cpu": int(m.group(2))}
                    for m in re.finditer(r"Worker (\S+) CPU: (\d+)%", response.text)]}
`

func scriptLimits() Limits { return Limits{ScriptTimeout: Duration(5 * time.Second)} }

func validated(t *testing.T, c Collector) *Collector {
	t.Helper()
	cfg := &Config{Collectors: []Collector{c}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return &cfg.Collectors[0]
}

func transformResponse(t *testing.T, c *Collector, body, contentType string) (*MetricSet, error) {
	t.Helper()
	response := &HTTPResponse{Body: []byte(body), Headers: make(http.Header)}
	if contentType != "" {
		response.Headers.Set("Content-Type", contentType)
	}
	decoded, err := decode(response, c)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return transform(context.Background(), decoded, response, c, "python3")
}

// The headline case: a pre-script reshapes an unstructured response and the
// metrics are then declared exactly like any other collector's.
func TestPreScriptReshapesTextForOrdinaryMetricRules(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("Worker a CPU: 42%\nWorker b CPU: 7%\n"))
	}))
	defer target.Close()

	cfg := &Config{Collectors: []Collector{{
		Name:      "worker_cpu",
		Transform: TransformConfig{Type: "jq", PreScript: workerReshapeScript},
		Limits:    scriptLimits(),
		Metrics: []MetricRule{{
			Name:        "vendor_worker_cpu",
			Description: "Worker CPU utilization",
			Type:        GaugeMetricType,
			ErrorMode:   "log",
			Expression:  ".workers[].cpu",
			Labels:      []LabelRule{{Name: "worker", Type: "expression", Expression: ".workers[].name"}},
		}},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())
	response := probeOnce(t, server, "/probe?target="+target.URL+"&collector=worker_cpu", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, want := range []string{
		"# HELP vendor_worker_cpu Worker CPU utilization",
		"# TYPE vendor_worker_cpu gauge",
		`vendor_worker_cpu{worker="a"} 42`,
		`vendor_worker_cpu{worker="b"} 7`,
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("exposition missing %q:\n%s", want, response.Body.String())
		}
	}
}

func TestPreScriptPromotesOnlyForStructuredTransforms(t *testing.T) {
	decodedFor := func(kind string) *Decoded {
		switch kind {
		case "csv":
			return &Decoded{Kind: "csv", Data: []any{}, Raw: []byte("cpu\n42\n")}
		case "html":
			return &Decoded{Kind: "html", Raw: []byte("<span>42</span>")}
		case "xml":
			return &Decoded{Kind: "xml", Raw: []byte("<status><cpu>42</cpu></status>")}
		default:
			return &Decoded{Kind: kind, Data: "Worker a CPU: 42%", Raw: []byte("Worker a CPU: 42%")}
		}
	}
	const structured = `data = {"workers": [{"name": "a", "cpu": 42}]}`
	// CSS and XPath consume the markup a pre-script returns, so they are asked
	// for the string result their contract defines.
	const markup = `data = data`
	tests := []struct {
		transform string
		kind      string
		script    string
		want      string
	}{
		{transform: "", kind: "text", script: structured, want: "json"},
		{transform: "none", kind: "text", script: structured, want: "json"},
		{transform: "jq", kind: "text", script: structured, want: "json"},
		{transform: "yq", kind: "text", script: structured, want: "json"},
		{transform: "jq", kind: "csv", script: structured, want: "json"},
		{transform: "jq", kind: "html", script: structured, want: "json"},
		{transform: "jq", kind: "xml", script: structured, want: "json"},
		{transform: "jq", kind: "json", script: structured, want: "json"},
		{transform: "csv", kind: "csv", script: structured, want: "csv"},
		{transform: "regex", kind: "text", script: structured, want: "text"},
		{transform: "python", kind: "text", script: structured, want: "text"},
		{transform: "css", kind: "html", script: markup, want: "html"},
		{transform: "xpath", kind: "xml", script: markup, want: "xml"},
	}
	for _, test := range tests {
		t.Run(test.transform+"/"+test.kind, func(t *testing.T) {
			c := &Collector{
				Name:      "promotion",
				Transform: TransformConfig{Type: test.transform, PreScript: test.script},
				Limits:    scriptLimits(),
			}
			decoded := decodedFor(test.kind)
			response := &HTTPResponse{Body: decoded.Raw, Headers: make(http.Header)}
			out, err := applyPreScript(context.Background(), decoded, response, c, "python3")
			if err != nil {
				t.Fatal(err)
			}
			if out.Kind != test.want {
				t.Fatalf("kind=%q, want %q", out.Kind, test.want)
			}
			if test.want == "json" {
				if _, ok := out.Data.(map[string]any); !ok {
					t.Fatalf("promoted data is %T, want a structured value", out.Data)
				}
			}
		})
	}
}

func TestPreScriptScalarResultKeepsDecodedFormat(t *testing.T) {
	c := validated(t, Collector{
		Name:      "scalar",
		Transform: TransformConfig{Type: "regex", PreScript: `data = data.replace("cpu", "value")`},
		Limits:    scriptLimits(),
		Metrics:   []MetricRule{{Name: "worker_value", Type: GaugeMetricType, Expression: `value=(\d+)`}},
	})
	set, err := transformResponse(t, c, "cpu=42\n", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 1 || set.Metrics[0].Value != 42 {
		t.Fatalf("unexpected metrics: %#v", set.Metrics)
	}
}

func TestPreScriptStillReparsesHTML(t *testing.T) {
	c := validated(t, Collector{
		Name:      "html_prescript",
		Response:  ResponseConfig{Format: "html"},
		Transform: TransformConfig{Type: "css", PreScript: `data = data.replace("42", "7")`},
		Limits:    scriptLimits(),
		Metrics:   []MetricRule{{Name: "page_value", Type: GaugeMetricType, Expression: "#value"}},
	})
	set, err := transformResponse(t, c, `<html><body><span id="value">42</span></body></html>`, "text/html")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 1 || set.Metrics[0].Value != 7 {
		t.Fatalf("HTML pre-script output was not reparsed: %#v", set.Metrics)
	}
}

func TestPreScriptDoesNotPromoteForTheCSVTransform(t *testing.T) {
	c := validated(t, Collector{
		Name:      "csv_prescript",
		Transform: TransformConfig{Type: "csv", PreScript: `data = data + [{"server": "extra", "cpu": "9"}]`},
		Limits:    scriptLimits(),
		Metrics: []MetricRule{{
			Name:       "server_cpu",
			Type:       GaugeMetricType,
			Expression: "cpu",
			Labels:     []LabelRule{{Name: "server", Type: "expression", Expression: "server"}},
		}},
	})
	set, err := transformResponse(t, c, "server,cpu\nalpha,42\n", "text/csv")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 {
		t.Fatalf("expected the CSV transform to keep consuming rows, got %#v", set.Metrics)
	}
	for _, metric := range set.Metrics {
		if metric.Labels["server"] == "" {
			t.Fatalf("row labels were lost: %#v", set.Metrics)
		}
	}
}

func TestUnstructuredPreScriptResultStillFailsStructuredTransforms(t *testing.T) {
	c := validated(t, Collector{
		Name:      "unstructured",
		Transform: TransformConfig{Type: "jq", PreScript: `data = "still plain text"`},
		Limits:    scriptLimits(),
		Metrics:   []MetricRule{{Name: "value", Type: GaugeMetricType, Expression: ".value"}},
	})
	_, err := transformResponse(t, c, "cpu=42\n", "text/plain")
	if err == nil || !strings.Contains(err.Error(), "cannot map response format") {
		t.Fatalf("error=%v, want an incompatible response format error", err)
	}
}

func TestPreScriptPromotionSupportsArrayResults(t *testing.T) {
	c := validated(t, Collector{
		Name:      "array_prescript",
		Transform: TransformConfig{Type: "jq", PreScript: `data = [{"zone": "a", "cpu": 1}, {"zone": "b", "cpu": 2}]`},
		Limits:    scriptLimits(),
		Metrics: []MetricRule{{
			Name:       "zone_cpu",
			Type:       GaugeMetricType,
			Expression: ".[].cpu",
			Labels:     []LabelRule{{Name: "zone", Type: "expression", Expression: ".[].zone"}},
		}},
	})
	set, err := transformResponse(t, c, "ignored\n", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 || set.Metrics[0].Labels["zone"] != "a" || set.Metrics[1].Value != 2 {
		t.Fatalf("unexpected metrics: %#v", set.Metrics)
	}
}
