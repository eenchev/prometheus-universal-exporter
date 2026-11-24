//go:build request_type_nats

package fetch

import (
	"context"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// TODO(nats): the type's tests, against a stand-in of what it reads, each
// saying in its comment what it checks. This one holds until Fetch is
// written, and goes then.

// Until the nats type is written, a collector of it validates, and its
// scrape fails, saying the type is not implemented yet.
func TestAnNATSCollectorIsNotImplementedYet(t *testing.T) {
	c := &model.Collector{Name: "nats", Request: model.RequestConfig{Type: RequestTypeNATS}}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	_, err := FetchCollector(context.Background(), "target", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), errNotImplementedNATS.Error()) {
		t.Fatalf("err=%v, want %v", err, errNotImplementedNATS)
	}
}

// The keys the opt-in nats type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheNATSTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeNATS)
}
