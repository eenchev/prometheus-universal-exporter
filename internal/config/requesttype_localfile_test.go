//go:build !select_request_types || request_type_localfile

package config

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func TestLocalFileValidation(t *testing.T) {
	root := t.TempDir()
	valid := fileCollector("files", root+"/", "app.prom")
	cfg := &model.Config{Collectors: []model.Collector{valid}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Request.Root; got != root {
		t.Fatalf("root=%q, want it cleaned to %q", got, root)
	}
	for name, tc := range map[string]struct {
		change func(*model.Collector)
		want   string
	}{
		"no root":             {func(c *model.Collector) { c.Request.Root = "" }, "request.root is required"},
		"relative root":       {func(c *model.Collector) { c.Request.Root = "var/lib" }, "must be an absolute path"},
		"filesystem root":     {func(c *model.Collector) { c.Request.Root = "/" }, "must not be the filesystem root"},
		"absolute path":       {func(c *model.Collector) { c.Request.Path = "/etc/passwd" }, "must be relative to request.root"},
		"escaping path":       {func(c *model.Collector) { c.Request.Path = "../secret" }, "leads outside request.root"},
		"escaping deeper":     {func(c *model.Collector) { c.Request.Path = "a/../../secret" }, "leads outside request.root"},
		"negative max_age":    {func(c *model.Collector) { c.Request.MaxAge = model.Duration(-time.Second) }, "max_age must not be negative"},
		"bad placeholder":     {func(c *model.Collector) { c.Request.Path = "{{tenant}}.json" }, "not a path parameter"},
		"an http key":         {func(c *model.Collector) { c.Request.Method = "POST" }, `request.method, which does not apply to request.type "localfile"`},
		"an http credential":  {func(c *model.Collector) { c.Request.BearerToken = "t" }, "request.bearer_token, which does not apply"},
		"negative size limit": {func(c *model.Collector) { c.Request.MaxResponseBytes = -1 }, "max_response_bytes must not be negative"},
	} {
		t.Run(name, func(t *testing.T) {
			c := fileCollector("files", root, "app.prom")
			tc.change(&c)
			err := Validate(&model.Config{Collectors: []model.Collector{c}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
	// The localfile keys do not apply to http.
	for key, change := range map[string]func(*model.Collector){
		"root":    func(c *model.Collector) { c.Request.Root = root },
		"max_age": func(c *model.Collector) { c.Request.MaxAge = model.Duration(time.Minute) },
	} {
		c := testutil.Collector("web", "text")
		change(&c)
		err := Validate(&model.Config{Collectors: []model.Collector{c}})
		if err == nil || !strings.Contains(err.Error(), "request."+key+`, which does not apply to request.type "http"`) {
			t.Errorf("%s on http: err=%v", key, err)
		}
	}
}
