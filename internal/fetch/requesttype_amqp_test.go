//go:build request_type_amqp

package fetch

import (
	"context"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// TODO(amqp): the type's tests, against a stand-in of what it reads, each
// saying in its comment what it checks. This one holds until Fetch is
// written, and goes then.

// Until the amqp type is written, a collector of it validates, and its
// scrape fails, saying the type is not implemented yet.
func TestAnAMQPCollectorIsNotImplementedYet(t *testing.T) {
	c := &model.Collector{Name: "amqp", Request: model.RequestConfig{Type: RequestTypeAMQP}}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	_, err := FetchCollector(context.Background(), "target", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), errNotImplementedAMQP.Error()) {
		t.Fatalf("err=%v, want %v", err, errNotImplementedAMQP)
	}
}

// The keys the opt-in amqp type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheAMQPTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeAMQP)
}
