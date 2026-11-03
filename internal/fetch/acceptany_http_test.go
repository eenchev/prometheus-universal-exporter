//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// request.accept_status: [any] accepts every status from 100 to 599, and
// none past them; it is written alone and in lower case, of a collector and
// of a static target; retry.statuses does not take it; and an entry of
// retry.statuses is refused beside it, since an accepted status is never
// retried, so a 503 the target answers is the answer, sent once.
func TestAcceptStatusAnyAcceptsEveryStatus(t *testing.T) {
	c := httpCollector(t, func(c *model.Collector) { c.Request.AcceptStatus = []string{"any"} })
	for status, want := range map[int]bool{100: true, 101: true, 200: true, 302: true, 404: true, 503: true, 599: true, 99: false, 600: false, 999: false} {
		if got := AcceptedStatus(c, RequestOverrides{}, status); got != want {
			t.Errorf("%d accepted %v, want %v", status, got, want)
		}
	}
	// A static target's [any] replaces the collector's list; the
	// collector's [any] is replaced by a target's own.
	plain := httpCollector(t, nil)
	if !AcceptedStatus(plain, RequestOverrides{AcceptStatus: []string{"any"}}, 404) || AcceptedStatus(c, RequestOverrides{AcceptStatus: []string{"2xx"}}, 404) {
		t.Error("a target's accept_status does not replace the collector's")
	}

	for _, refused := range []struct {
		accepted, retried []string
		want              string
	}{
		{[]string{"any", "2xx"}, nil, `collector "web" request.accept_status entry "any" accepts every status from 100 to 599 and is written alone, without "2xx"; take the other entries out`},
		{[]string{"503", "any"}, nil, `entry "any" accepts every status from 100 to 599 and is written alone, without "503"`},
		{[]string{"any", "any"}, nil, `entry "any" accepts every status from 100 to 599 and is written alone`},
		{[]string{"ANY"}, nil, `request.accept_status entry "ANY" is not an HTTP status from 100 to 599`},
		{[]string{"Any"}, nil, `request.accept_status entry "Any" is not an HTTP status`},
		{nil, []string{"any"}, `request.retry.statuses entry "any" is not an HTTP status from 100 to 599`},
		{[]string{"any"}, []string{"503"}, `collector "web" request.retry.statuses entry "503" is never retried: request.accept_status accepts it`},
		{[]string{"any"}, []string{"1xx"}, `entry "1xx" is never retried`},
	} {
		c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, AcceptStatus: refused.accepted, Retry: model.RetryConfig{Statuses: refused.retried}}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), refused.want) {
			t.Errorf("accept %q, retry %q: %v, want %q", refused.accepted, refused.retried, err, refused.want)
		}
	}
	// An empty retry.statuses beside it is taken: no status is retried,
	// a failed connection is.
	if err := ValidateRequest(&model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, AcceptStatus: []string{"any"}, Retry: model.RetryConfig{Statuses: []string{}}}}); err != nil {
		t.Error(err)
	}

	server, hits := answering(t, 503)
	retried := httpCollector(t, func(c *model.Collector) {
		c.Request.AcceptStatus = []string{"any"}
		c.Request.Retry = model.RetryConfig{Attempts: 2}
	})
	if response, err := FetchCollector(context.Background(), server.URL, retried, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusServiceUnavailable || hits.Load() != 1 {
		t.Errorf("a 503 accepted by [any] was sent %d times: %v", hits.Load(), err)
	}

	for request, want := range map[*model.TargetRequestConfig]string{
		{AcceptStatus: []string{"any", "5xx"}}:                                                      `target "t" request.accept_status entry "any" accepts every status from 100 to 599 and is written alone`,
		{AcceptStatus: []string{"any"}, Retry: &model.TargetRetryConfig{Statuses: []string{"404"}}}: `target "t" request.retry.statuses entry "404" is never retried: request.accept_status accepts it`,
		{Retry: &model.TargetRetryConfig{Statuses: []string{"any"}}}:                                `target "t" request.retry.statuses entry "any" is not an HTTP status`,
	} {
		target := &model.StaticTarget{Name: "t", Collector: "web", Target: "http://x", Request: *request}
		NormalizeTargetRequest(target)
		if err := CheckTargetRequest(target, plain); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", request, err, want)
		}
	}
	target := &model.StaticTarget{Name: "t", Collector: "web", Target: "http://x", Request: model.TargetRequestConfig{AcceptStatus: []string{"any"}}}
	NormalizeTargetRequest(target)
	if err := CheckTargetRequest(target, plain); err != nil {
		t.Error(err)
	}
	// The collector's own retry.statuses is checked against the target's
	// [any], which accepts what it lists.
	withRetries := httpCollector(t, func(c *model.Collector) { c.Request.Retry = model.RetryConfig{Attempts: 1, Statuses: []string{"503"}} })
	if err := CheckTargetRequest(target, withRetries); err == nil || !strings.Contains(err.Error(), `request.retry.statuses entry "503" is never retried`) {
		t.Errorf("a target's [any] under a collector retrying 503: %v", err)
	}
}
