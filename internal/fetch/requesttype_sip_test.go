//go:build request_type_sip

package fetch

import (
	"context"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// TODO(sip): the type's tests, against a stand-in of what it reads, each
// saying in its comment what it checks. This one holds until Fetch is
// written, and goes then.

// Until the sip type is written, a collector of it validates, and its
// scrape fails, saying the type is not implemented yet.
func TestAnSIPCollectorIsNotImplementedYet(t *testing.T) {
	c := &model.Collector{Name: "sip", Request: model.RequestConfig{Type: RequestTypeSIP}}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	_, err := FetchCollector(context.Background(), "target", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), errNotImplementedSIP.Error()) {
		t.Fatalf("err=%v, want %v", err, errNotImplementedSIP)
	}
}

// The keys the opt-in sip type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheSIPTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeSIP)
}
