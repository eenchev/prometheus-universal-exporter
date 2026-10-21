//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// answering is a target that answers every request with status, and how
// many requests it was sent.
func answering(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

// sentFor is how many requests a collector with retry.statuses retried
// (nil for the default) and one retry sends a target answering status.
func sentFor(t *testing.T, retried []string, status int) int32 {
	t.Helper()
	server, hits := answering(t, status)
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.Retry = model.RetryConfig{Attempts: 1, Statuses: retried}
	})
	if _, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	return hits.Load()
}

// Without retry.statuses the statuses retried are those they always were,
// 408, 425, 429 and every 5xx; a list replaces that set, so a 404 is
// retried and a 500 no longer; a class retries each of its statuses; and an
// empty list retries no status, while a connection that failed is retried
// still.
func TestRetryStatusesReplaceTheDefaultSet(t *testing.T) {
	for status, want := range map[int]int32{408: 2, 425: 2, 429: 2, 500: 2, 503: 2, 599: 2, 404: 1, 400: 1, 409: 1, 302: 1} {
		if got := sentFor(t, nil, status); got != want {
			t.Errorf("by default, a %d answer was sent %d times, want %d", status, got, want)
		}
	}
	if !reflect.DeepEqual(DefaultRetryStatuses, []string{"408", "425", "429", "5xx"}) {
		t.Errorf("the default set is %v", DefaultRetryStatuses)
	}
	for status, want := range map[int]int32{404: 2, 503: 2, 500: 1, 429: 1} {
		if got := sentFor(t, []string{"404", "503"}, status); got != want {
			t.Errorf("retry.statuses [404, 503]: a %d answer was sent %d times, want %d", status, got, want)
		}
	}
	for status, want := range map[int]int32{404: 2, 418: 2, 499: 2, 503: 1, 500: 1} {
		if got := sentFor(t, []string{"4xx"}, status); got != want {
			t.Errorf("retry.statuses [4xx]: a %d answer was sent %d times, want %d", status, got, want)
		}
	}
	for _, status := range []int{503, 429, 408} {
		if got := sentFor(t, []string{}, status); got != 1 {
			t.Errorf("retry.statuses []: a %d answer was sent %d times, want once", status, got)
		}
	}
	// A connection that breaks before the answer is retried under [].
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.Retry = model.RetryConfig{Attempts: 1, Statuses: []string{}}
	})
	if response, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("retry.statuses []: a broken connection was not retried: response=%v err=%v hits=%d", response, err, hits.Load())
	}
}

// retry.statuses is written and checked as accept_status is: statuses from
// 100 to 599 and classes, normalized, the rest refused in accept_status's
// words. An entry every status of which is accepted could never be retried
// and is refused, naming it; a class only some of whose statuses are
// accepted is taken, and the accepted ones are still not retried.
func TestRetryStatusesAreCheckedAsAcceptStatusIs(t *testing.T) {
	for _, bad := range []string{"600", "99", "6xx", "2x", "ok", ""} {
		c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, Retry: model.RetryConfig{Statuses: []string{bad}}}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), `collector "web" request.retry.statuses entry "`+bad+`" is not an HTTP status from 100 to 599 or a class such as 2xx`) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	c := httpCollector(t, func(c *model.Collector) { c.Request.Retry.Statuses = []string{" 4XX ", "+0503", "0429"} })
	if !reflect.DeepEqual(c.Request.Retry.Statuses, []string{"4xx", "503", "429"}) {
		t.Errorf("normalized: %q", c.Request.Retry.Statuses)
	}
	for _, refused := range []struct {
		accepted, retried []string
		want              string
	}{
		{[]string{"2xx", "503"}, []string{"404", "503"}, `collector "web" request.retry.statuses entry "503" is never retried: request.accept_status accepts it`},
		{[]string{"5xx"}, []string{"502"}, `entry "502" is never retried: request.accept_status accepts it`},
		{[]string{"5xx"}, []string{"5XX"}, `entry "5xx" is never retried`},
		{nil, []string{"200"}, `collector "web" request.retry.statuses entry "200" is never retried: it is a 2xx, which a collector without request.accept_status accepts`},
		{nil, []string{"2xx"}, `entry "2xx" is never retried: it is a 2xx`},
	} {
		c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, AcceptStatus: refused.accepted, Retry: model.RetryConfig{Statuses: refused.retried}}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), refused.want) {
			t.Errorf("accept %v, retry %v: %v", refused.accepted, refused.retried, err)
		}
	}
	for _, taken := range []struct{ accepted, retried []string }{
		{[]string{"2xx", "503"}, []string{"5xx"}},
		{[]string{"200"}, []string{"2xx"}},
		{nil, []string{"404", "301", "100"}},
		{[]string{"404"}, []string{}},
	} {
		c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, AcceptStatus: taken.accepted, Retry: model.RetryConfig{Statuses: taken.retried}}}
		if err := ValidateRequest(&c); err != nil {
			t.Errorf("accept %v, retry %v: %v", taken.accepted, taken.retried, err)
		}
	}
	// The accepted 503 of a retried 5xx is the answer; a 502 is retried.
	partly := httpCollector(t, func(c *model.Collector) {
		c.Request.AcceptStatus = []string{"2xx", "503"}
		c.Request.Retry = model.RetryConfig{Attempts: 1, Statuses: []string{"5xx"}}
	})
	for status, want := range map[int]int32{503: 1, 502: 2} {
		server, hits := answering(t, status)
		if _, err := FetchCollector(context.Background(), server.URL, partly, RequestOverrides{}, nil); err != nil || hits.Load() != want {
			t.Errorf("a %d answer was sent %d times, want %d: %v", status, hits.Load(), want, err)
		}
	}
	// The bound on attempts is as it was.
	bounded := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, Retry: model.RetryConfig{Attempts: MaxRetryAttempts + 1, Statuses: []string{"404"}}}}
	if err := ValidateRequest(&bounded); err == nil || !strings.Contains(err.Error(), "request.retry.attempts must be from 0 to 10") {
		t.Errorf("attempts past the bound: %v", err)
	}
}

// A static target's retry.statuses replaces the collector's whole, is
// normalized when its file is loaded, is part of the overrides a scrape
// makes, and is checked against the accept_status in force for the target:
// its own, or else the collector's, whichever of the two lists the target
// sets.
func TestAStaticTargetsRetryStatuses(t *testing.T) {
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.AcceptStatus = []string{"2xx", "503"}
		c.Request.Retry = model.RetryConfig{Attempts: 1, Statuses: []string{"502"}}
	})
	target := &model.StaticTarget{Name: "t", Collector: c.Name, Target: "http://x", Request: model.TargetRequestConfig{Retry: &model.TargetRetryConfig{Statuses: []string{" 4XX "}}}}
	NormalizeTargetRequest(target)
	if !reflect.DeepEqual(target.Request.Retry.Statuses, []string{"4xx"}) {
		t.Fatalf("normalized: %q", target.Request.Retry.Statuses)
	}
	if err := CheckTargetRequest(target, c); err != nil {
		t.Fatal(err)
	}
	overrides := TargetOverrides(target)
	if !reflect.DeepEqual(overrides.RetryStatuses, []string{"4xx"}) || overrides.RetryAttempts != nil {
		t.Fatalf("overrides %+v", overrides)
	}
	if got := target.Request.Retry.Over(c.Request.Retry); !reflect.DeepEqual(got.Statuses, []string{"4xx"}) || got.Attempts != 1 {
		t.Fatalf("over the collector's: %+v", got)
	}
	for status, want := range map[int]int32{404: 2, 502: 1} {
		server, hits := answering(t, status)
		if _, err := FetchCollector(context.Background(), server.URL, c, overrides, nil); err != nil || hits.Load() != want {
			t.Errorf("the target's [4xx]: a %d answer was sent %d times, want %d: %v", status, hits.Load(), want, err)
		}
	}
	for request, want := range map[*model.TargetRequestConfig]string{
		{Retry: &model.TargetRetryConfig{Statuses: []string{"600"}}}:                                       `target "t" request.retry.statuses entry "600" is not an HTTP status from 100 to 599 or a class such as 2xx`,
		{Retry: &model.TargetRetryConfig{Statuses: []string{"503"}}}:                                       `target "t" request.retry.statuses entry "503" is never retried: request.accept_status accepts it`,
		{AcceptStatus: []string{"5xx"}}:                                                                    `target "t" request.retry.statuses entry "502" is never retried`,
		{AcceptStatus: []string{"404"}, Retry: &model.TargetRetryConfig{Statuses: []string{"404", "500"}}}: `target "t" request.retry.statuses entry "404" is never retried`,
	} {
		target.Request = *request
		if err := CheckTargetRequest(target, c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err=%v, want %q", request, err, want)
		}
	}
	// Its own accept_status without 503 lets it retry the 503 it lists.
	target.Request = model.TargetRequestConfig{AcceptStatus: []string{"2xx"}, Retry: &model.TargetRetryConfig{Statuses: []string{"503"}}}
	if err := CheckTargetRequest(target, c); err != nil {
		t.Fatal(err)
	}
}

// A debug report says how a request is retried: how often, how far apart
// and on which statuses — the default set, a list, a static target's, or
// none but a failed connection — and nothing where no retry is made.
func TestRetriesOfSaysWhichStatusesAreRetried(t *testing.T) {
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.Retry = model.RetryConfig{Attempts: 2, Backoff: model.Duration(1e9)}
	})
	if got := RetriesOf(c, RequestOverrides{}); got != "up to 2, 1s apart, on a failed connection and on an answer of 408, 425, 429, 5xx that is not accepted" {
		t.Errorf("default: %q", got)
	}
	c.Request.Retry.Statuses = []string{"404", "5xx"}
	if got := RetriesOf(c, RequestOverrides{}); !strings.HasSuffix(got, "on an answer of 404, 5xx that is not accepted") {
		t.Errorf("a list: %q", got)
	}
	if got := RetriesOf(c, RequestOverrides{RetryStatuses: []string{}}); got != "up to 2, 1s apart, on a failed connection alone: retry.statuses is empty" {
		t.Errorf("a target's []: %q", got)
	}
	none := 0
	if got := RetriesOf(c, RequestOverrides{RetryAttempts: &none}); got != "" {
		t.Errorf("no attempts: %q", got)
	}
	c.Request.Method = http.MethodPost
	if got := RetriesOf(c, RequestOverrides{}); got != "" {
		t.Errorf("a POST without non_idempotent: %q", got)
	}
}
