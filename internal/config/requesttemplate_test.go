package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func TestTemplatesAreCheckedWhenTheConfigurationLoads(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(c *model.Collector)
		want   string
	}{
		"a placeholder in a header name": {func(c *model.Collector) { c.Request.Headers = map[string]string{"X-{{param_x}}": "1"} }, "placeholders stand only in header values"},
		"a placeholder in a query name":  {func(c *model.Collector) { c.Request.Query = map[string]string{"{{param_x}}": "1"} }, "placeholders stand only in query values"},
		"a filter in a header value":     {func(c *model.Collector) { c.Request.Headers = map[string]string{"X": "{{param_x|json}}"} }, "filters apply only in request.body"},
		"a malformed body placeholder":   {func(c *model.Collector) { c.Request.Body = "{{param_x" }, "request.body has an unclosed placeholder"},
	} {
		t.Run(name, func(t *testing.T) {
			c := testutil.Collector("web", "text")
			tc.change(&c)
			cfg := &model.Config{Collectors: []model.Collector{c}}
			if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}
