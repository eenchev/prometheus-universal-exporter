//go:build request_type_radius

package fetch

import (
	"context"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// TODO(radius): the type's tests, against a stand-in of what it reads, each
// saying in its comment what it checks. This one holds until Fetch is
// written, and goes then.

// Until the radius type is written, a collector of it validates, and its
// scrape fails, saying the type is not implemented yet.
func TestARADIUSCollectorIsNotImplementedYet(t *testing.T) {
	c := &model.Collector{Name: "radius", Request: model.RequestConfig{Type: RequestTypeRADIUS}}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	_, err := FetchCollector(context.Background(), "target", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), errNotImplementedRADIUS.Error()) {
		t.Fatalf("err=%v, want %v", err, errNotImplementedRADIUS)
	}
}

// The keys the opt-in radius type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheRADIUSTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeRADIUS)
}
