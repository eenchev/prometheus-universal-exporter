//go:build !select_request_types || request_type_http

package fetch

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An entry of request.accept_status that is text with a sign, leading zeros
// or blanks around a status, "+503", "0503" or " 503 ", is refused, of a
// collector, of its retry.statuses and of a static target, naming the form
// an entry takes: the schemas' pattern refuses it, and the exporter took it
// as the status 503. A class in either case is taken, in lower case.
func TestAStatusWithASignLeadingZerosOrBlanksIsRefused(t *testing.T) {
	entries := []string{"503", "2XX", "4xX"}
	if err := normalizeAcceptStatus(entries); err != nil {
		t.Fatal(err)
	}
	if want := []string{"503", "2xx", "4xx"}; !slices.Equal(entries, want) {
		t.Errorf("the entries are %q, want %q", entries, want)
	}
	for _, entry := range []string{"0503", "+503", " 503 ", "503 ", " 503", "+0200", "0404", " 2xx", "5XX ", "\t503", "503\n"} {
		want := fmt.Sprintf("entry %q is not an HTTP status from 100 to 599 written with its three digits, such as 503, or a class such as 2xx", entry)
		if err := normalizeAcceptStatus([]string{entry}); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %s", entry, err, want)
		}
		c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, AcceptStatus: []string{entry}}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), `collector "web" request.accept_status `+want) {
			t.Errorf("collector, %q: %v", entry, err)
		}
		c = model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, Retry: model.RetryConfig{Statuses: []string{entry}}}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), `collector "web" request.retry.statuses `+want) {
			t.Errorf("retry.statuses, %q: %v", entry, err)
		}
		// A target's entries were written in lower case when its file was
		// loaded, and are named so.
		target := model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{AcceptStatus: []string{entry}}}
		NormalizeTargetRequest(&target)
		want = fmt.Sprintf("entry %q is not an HTTP status", strings.ToLower(entry))
		if err := CheckTargetRequest(&target, &model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}}); err == nil || !strings.Contains(err.Error(), `request.accept_status `+want) {
			t.Errorf("target, %q: %v", entry, err)
		}
	}
}
