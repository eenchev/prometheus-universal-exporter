//go:build !select_request_types || request_type_localfile

package fetch

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The largest limit that can be written, as someone writes "no limit", reads
// a file whole, whichever of the two keys holds it: the one byte read past
// the limit does not wrap round to a read of nothing.
func TestTheLargestResponseLimitReadsAFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "up.prom"), []byte("up 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*model.Collector){
		"request.max_response_bytes": func(c *model.Collector) { c.Request.MaxResponseBytes = math.MaxInt64 },
		"limits.max_response_bytes":  func(c *model.Collector) { c.Limits.MaxResponseBytes = math.MaxInt64 },
	} {
		c := fileCollector("files", root, "up.prom")
		edit(&c)
		if err := ValidateRequest(&c); err != nil {
			t.Fatal(err)
		}
		resp, err := FetchCollector(context.Background(), "", &c, RequestOverrides{}, nil)
		if err != nil || string(resp.Body) != "up 1\n" {
			t.Errorf("%s: resp=%+v err=%v, want the file whole", name, resp, err)
		}
	}
}
