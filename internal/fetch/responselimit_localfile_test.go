//go:build !select_request_types || request_type_localfile

package fetch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A file is read only up to the size limit. The file is sparse, so it takes
// no disk, but reading all of it would allocate its whole gigabyte; what the
// read allocated shows how much of it was read.
func TestALocalFileIsNotReadPastTheLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "huge.prom"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(root, "huge.prom"), 1<<30); err != nil {
		t.Skipf("cannot make a sparse file here: %v", err)
	}
	c := fileCollector("files", root, "huge.prom")
	c.Request.MaxResponseBytes = 1024
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := fetchLocalFile(context.Background(), "", &c, RequestOverrides{}, nil)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("err=%v, want a limit error", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<20 {
		t.Fatalf("reading a 1 GiB file with a 1 KiB limit allocated %d bytes", allocated)
	}
}
