//go:build !select_request_types || request_type_localfile

package fetch

import (
	"context"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A file and a directory are read on goroutines of their own, which a
// deadline may leave behind. A panic there ended the exporter; it is now
// the fetch's own panic, raised on the trip's goroutine, where a probe and
// a static target's scrape recover it and answer.
func TestAPanicWhileAFileIsReadIsTheFetchsOwn(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "app.prom", promFile)
	testutil.WriteIn(t, root, "other.prom", promFile)
	onFileRead(t, func(string) { panic("the file's reader") })
	for name, c := range map[string]*model.Collector{
		"a file":      validated(t, fileCollector("files", root, "app.prom")),
		"a directory": validated(t, dirCollector("dir", root, "*.prom")),
	} {
		recovered := caught(func() {
			_, err := FetchCollector(context.Background(), "", c, RequestOverrides{}, nil)
			t.Errorf("%s: the fetch returned %v", name, err)
		})
		if p, ok := recovered.(*goroutinePanic); !ok || !strings.Contains(p.Error(), "the file's reader") {
			t.Errorf("%s: the fetch panicked with %v", name, recovered)
		}
	}
}
