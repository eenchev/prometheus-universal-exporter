package config

import (
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func pathCollector(path string) model.Collector {
	c := testutil.Collector("tenants", "text")
	c.Request.Path = path
	return c
}

func scriptLimits() model.Limits { return model.Limits{ScriptTimeout: model.Duration(5 * time.Second)} }

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
