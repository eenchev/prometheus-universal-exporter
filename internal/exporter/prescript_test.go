package exporter

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

const workerReshapeScript = `import re
data = {"workers": [{"name": m.group(1), "cpu": int(m.group(2))}
                    for m in re.finditer(r"Worker (\S+) CPU: (\d+)%", response.text)]}
`

// The headline case: a pre-script reshapes an unstructured response and the
// metrics are then declared exactly like any other collector's.
func TestPreScriptReshapesTextForOrdinaryMetricRules(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("Worker a CPU: 42%\nWorker b CPU: 7%\n"))
	}))
	defer target.Close()

	cfg := &model.Config{Collectors: []model.Collector{{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "worker_cpu",
		Transform: model.TransformConfig{Type: "jq", PreScript: workerReshapeScript},
		Limits:    scriptLimits(),
		Metrics: []model.MetricRule{{
			Name:        "vendor_worker_cpu",
			Description: "Worker CPU utilization",
			Type:        model.GaugeMetricType,
			ErrorMode:   "log",
			Expression:  ".workers[].cpu",
			Labels:      []model.LabelRule{{Name: "worker", Expression: ".workers[].name"}},
		}},
	}}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
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
