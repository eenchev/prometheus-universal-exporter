//go:build request_type_sshexec

package fetch

import (
	"context"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// TODO(sshexec): the type's tests, against a stand-in of what it reads, and a
// bullet for each in spec § 34. This one holds until Fetch is written, and
// goes then.

// Until the sshexec type is written, a collector of it validates, and its
// scrape fails, saying the type is not implemented yet.
func TestASshexecCollectorIsNotImplementedYet(t *testing.T) {
	c := &model.Collector{Name: "sshexec", Request: model.RequestConfig{Type: RequestTypeSshexec}}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	_, err := FetchCollector(context.Background(), "target", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), errNotImplementedSshexec.Error()) {
		t.Fatalf("err=%v, want %v", err, errNotImplementedSshexec)
	}
}

// The keys the opt-in sshexec type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheSshexecTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeSshexec)
}
