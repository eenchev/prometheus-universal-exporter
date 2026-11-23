//go:build !select_request_types || request_type_http

package exporter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A scrape stopped at limits.max_expression_memory is counted in a series
// of its own, http_exporter_expression_memory_exceeded_total, whether the
// expression was the transform's or the pagination's, beside the count of
// every scrape a limit rejected; a scrape that another limit rejected is
// not, so the two apart say which limit a collector keeps reaching.
func TestAScrapeStoppedForItsExpressionsMemoryIsCountedApart(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"items":[1,2,3]}`)
	}))
	t.Cleanup(target.Close)
	// A text doubled until it is past the megabyte it may take: stopped
	// when the value is made, with no look at the heap.
	const doubled = `reduce range(30) as $i ("x"; . + .) | length`
	collector := func(name string, change func(*model.Collector)) model.Collector {
		c := model.Collector{Name: name, Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
			Metrics: []model.MetricRule{{Name: "items", Type: model.GaugeMetricType, Items: ".items[]", Expression: ".", Labels: []model.LabelRule{{Name: "item", Expression: "tostring"}}}}}
		c.Limits.MaxExpressionMemory = 1 << 20
		change(&c)
		return c
	}
	server := modeServer(t,
		collector("transform", func(c *model.Collector) { c.Metrics[0].Expression = doubled }),
		collector("pagination", func(c *model.Collector) {
			c.Request = model.RequestConfig{Type: fetch.RequestTypeHTTP, Own: fetch.HTTPConfig{Pagination: &model.PaginationConfig{Type: "cursor", Next: doubled + " | null", Param: "after", Items: ".items"}}}
		}),
		collector("series", func(c *model.Collector) { c.Limits.MaxMetrics = 2 }),
		collector("within", func(*model.Collector) {}),
	)
	for name, status := range map[string]int{"transform": http.StatusBadGateway, "pagination": http.StatusBadGateway, "series": http.StatusBadGateway, "within": http.StatusOK} {
		for range 2 {
			if r := probeOnce(t, server, "/probe?collector="+name+"&target="+url.QueryEscape(target.URL), nil); r.Code != status {
				t.Fatalf("%s answered %d: %s", name, r.Code, r.Body)
			}
		}
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{
		`http_exporter_expression_memory_exceeded_total{collector="transform"} 2`, `http_exporter_series_limit_exceeded_total{collector="transform"} 2`,
		`http_exporter_expression_memory_exceeded_total{collector="pagination"} 2`, `http_exporter_series_limit_exceeded_total{collector="pagination"} 2`,
		`http_exporter_expression_memory_exceeded_total{collector="series"} 0`, `http_exporter_series_limit_exceeded_total{collector="series"} 2`,
		`http_exporter_expression_memory_exceeded_total{collector="within"} 0`, `http_exporter_series_limit_exceeded_total{collector="within"} 0`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("no %s in the self-metrics", want)
		}
	}
}
