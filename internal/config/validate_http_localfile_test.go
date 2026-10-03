//go:build !select_request_types || (request_type_http && request_type_localfile)

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A collector that sets no decoder, and whose transform implies none, decodes
// each response by what it says it is: warned about at load, with how.
func TestAnUnsetDecoderIsWarnedAbout(t *testing.T) {
	collector := func(name, request, transformType string) model.Collector {
		c := testutil.Collector(name, "text")
		c.Decoder = model.DecoderConfig{}
		c.Request = model.RequestConfig{Type: request}
		if request == fetch.RequestTypeLocalFile {
			c.Request.Root = t.TempDir()
			c.Request.Path = "status.json"
		}
		c.Transform = model.TransformConfig{Type: transformType}
		c.Metrics[0].Expression = ".value"
		return c
	}
	pinned := collector("pinned", fetch.RequestTypeHTTP, "jq")
	pinned.Decoder.Type = "json"
	explicit := collector("explicit", fetch.RequestTypeHTTP, "jq")
	explicit.Decoder.Type = "auto"
	implied := testutil.Collector("implied", "text") // regex, which implies text
	implied.Decoder = model.DecoderConfig{}
	cfg := &model.Config{Collectors: []model.Collector{
		collector("by_header", fetch.RequestTypeHTTP, "jq"),
		collector("by_extension", fetch.RequestTypeLocalFile, "jq"),
		pinned, explicit, implied,
	}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`collector "by_header" sets no decoder.type, so it decodes by the Content-Type header of each response`,
		`collector "by_extension" sets no decoder.type, so it decodes by the extension of each file`,
	}
	if len(cfg.Warnings) != len(want) {
		t.Fatalf("warnings=%q", cfg.Warnings)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(cfg.Warnings[i], prefix) {
			t.Errorf("warning %d = %q, want it to start %q", i, cfg.Warnings[i], prefix)
		}
	}
}
