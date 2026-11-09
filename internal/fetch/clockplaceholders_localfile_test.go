//go:build !select_request_types || request_type_localfile

package fetch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A clock placeholder names a file by the day, and one whose format holds
// a slash, which would be a directory, is refused when the configuration
// loads, naming the placeholder.
func TestAClockPlaceholderNamesALocalFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "2026-11-08.prom"), []byte("up 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	atClock(t, time.Date(2026, time.November, 9, 8, 0, 0, 0, time.UTC))
	c := model.Collector{Name: "files", Request: model.RequestConfig{Type: RequestTypeLocalFile, Root: root, Path: "{{date:-1d}}.prom"}}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	answer, err := FetchCollector(context.Background(), "", &c, RequestOverrides{}, nil)
	if err != nil || !strings.Contains(string(answer.Body), "up 1") {
		t.Fatalf("read %+v, %v", answer, err)
	}
	us := model.Collector{Name: "files", Request: model.RequestConfig{Type: RequestTypeLocalFile, Root: root, Path: "{{date_us}}.prom"}}
	err = ValidateRequest(&us)
	if err == nil || !strings.Contains(err.Error(), `the clock placeholder {{date_us}} writes values such as "11/09/2026", which no probe could use`) {
		t.Fatalf("got %v", err)
	}
}
