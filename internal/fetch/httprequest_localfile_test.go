//go:build !select_request_types || (request_type_http && request_type_localfile)

package fetch

import (
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A path with a query or a fragment is refused, in a probe's path parameter
// too; a localfile path may hold either, being a file's name.
func TestAPathHoldsNoQuery(t *testing.T) {
	c := httpCollector(t, nil)
	if err := CheckOverrideParams(c, url.Values{"path": {"/s?a=1"}}); err == nil || !strings.Contains(err.Error(), `probe parameter path "/s?a=1" has a ? in it`) {
		t.Fatalf("err=%v", err)
	}
	if err := CheckOverrideParams(c, url.Values{"path": {"/s"}}); err != nil {
		t.Fatal(err)
	}
	file := model.Collector{Name: "f", Request: model.RequestConfig{Type: RequestTypeLocalFile, Root: t.TempDir(), Path: "odd?name#1.prom"}}
	if err := ValidateRequest(&file); err != nil {
		t.Fatalf("a localfile path with ? and #: %v", err)
	}
}
