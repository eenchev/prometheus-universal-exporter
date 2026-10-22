//go:build !select_request_types || request_type_http

package fetch

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// retryingAfter is an http collector retrying once, backoff apart, with
// retry.retry_after set to on.
func retryingAfter(t *testing.T, backoff time.Duration, on bool) *model.Collector {
	t.Helper()
	return httpCollector(t, func(c *model.Collector) {
		c.Request.Retry = model.RetryConfig{Attempts: 1, Backoff: model.Duration(backoff), RetryAfter: on}
	})
}

// Without retry.retry_after a Retry-After is not read: a 503 or a 429
// asking for no wait is retried after the backoff, which an hour long the
// deadline cuts short, so the answer kept is the first. With it, the
// retry is sent at once, as the header asks, and the trace says why.
func TestRetryAfterReplacesTheBackoffOnlyWhenAsked(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		target, hits := retryAfterTarget(t, status, "0")
		resp, traced, _ := fetchWithin(t, target.URL, retryingAfter(t, time.Hour, false), RequestOverrides{}, 500*time.Millisecond)
		if resp.StatusCode != status || hits.Load() != 1 || traced[0].Wait != "" {
			t.Errorf("off, %d: answer %d after %d requests, wait %q; want the %d, once, and no wait recorded", status, resp.StatusCode, hits.Load(), traced[0].Wait, status)
		}
		target, hits = retryAfterTarget(t, status, "0")
		resp, traced, took := fetchWithin(t, target.URL, retryingAfter(t, time.Hour, true), RequestOverrides{}, 5*time.Second)
		if resp.StatusCode != http.StatusOK || hits.Load() != 2 || took > 4*time.Second {
			t.Fatalf("on, %d: answer %d after %d requests in %s; want 200 after 2, at once", status, resp.StatusCode, hits.Load(), took)
		}
		if traced[0].Wait != "then waited 0s, as Retry-After asked" || traced[1].Wait != "" {
			t.Errorf("on, %d: the trace says %q, %q", status, traced[0].Wait, traced[1].Wait)
		}
	}
}

// A Retry-After of seconds is waited, in place of a backoff of none.
func TestRetryAfterSecondsAreWaited(t *testing.T) {
	target, hits := retryAfterTarget(t, http.StatusServiceUnavailable, "1")
	resp, traced, took := fetchWithin(t, target.URL, retryingAfter(t, 0, true), RequestOverrides{}, 10*time.Second)
	if resp.StatusCode != http.StatusOK || hits.Load() != 2 || took < time.Second {
		t.Fatalf("answer %d after %d requests in %s; want 200 after 2, a second apart", resp.StatusCode, hits.Load(), took)
	}
	if traced[0].Wait != "then waited 1s, as Retry-After asked" {
		t.Errorf("the trace says %q", traced[0].Wait)
	}
}

// A Retry-After that is an HTTP date is waited until, from now; one already
// past asks for no wait, whatever the backoff.
func TestRetryAfterDatesAreWaitedUntil(t *testing.T) {
	soon := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	target, hits := retryAfterTarget(t, http.StatusServiceUnavailable, soon)
	resp, _, took := fetchWithin(t, target.URL, retryingAfter(t, 0, true), RequestOverrides{}, 10*time.Second)
	// The date is to the second, so the wait is one second at least.
	if resp.StatusCode != http.StatusOK || hits.Load() != 2 || took < time.Second {
		t.Fatalf("a date 2s off: answer %d after %d requests in %s; want 200 after 2, a second apart at least", resp.StatusCode, hits.Load(), took)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	target, hits = retryAfterTarget(t, http.StatusServiceUnavailable, past)
	resp, traced, _ := fetchWithin(t, target.URL, retryingAfter(t, time.Hour, true), RequestOverrides{}, 5*time.Second)
	if resp.StatusCode != http.StatusOK || hits.Load() != 2 || traced[0].Wait != "then waited 0s, as Retry-After asked" {
		t.Fatalf("a date past: answer %d after %d requests, wait %q; want 200 after 2, at once", resp.StatusCode, hits.Load(), traced[0].Wait)
	}
}

// A Retry-After that is neither a number of seconds nor an HTTP date, a
// negative or fractional number among them, is not read, and the backoff is
// waited as without it; the trace says so. An answer without the header
// waits the backoff too.
func TestAnUnreadableRetryAfterFallsBackToTheBackoff(t *testing.T) {
	for _, header := range []string{"soon", "-1", "1.5", "+1", "0x10", "Tue, 30 Feb"} {
		target, hits := retryAfterTarget(t, http.StatusServiceUnavailable, header)
		resp, traced, _ := fetchWithin(t, target.URL, retryingAfter(t, 0, true), RequestOverrides{}, 5*time.Second)
		want := `then waited 0s, retry.backoff; its Retry-After "` + header + `" is neither a number of seconds nor an HTTP date`
		if resp.StatusCode != http.StatusOK || hits.Load() != 2 || traced[0].Wait != want {
			t.Errorf("%q: answer %d after %d requests, wait %q; want 200 after 2 and %q", header, resp.StatusCode, hits.Load(), traced[0].Wait, want)
		}
		target, hits = retryAfterTarget(t, http.StatusServiceUnavailable, header)
		if resp, _, _ := fetchWithin(t, target.URL, retryingAfter(t, time.Hour, true), RequestOverrides{}, 300*time.Millisecond); resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 1 {
			t.Errorf("%q with an hour's backoff: answer %d after %d requests; want the 503, once", header, resp.StatusCode, hits.Load())
		}
	}
	target, _ := retryAfterTarget(t, http.StatusServiceUnavailable, "")
	if _, traced, _ := fetchWithin(t, target.URL, retryingAfter(t, 0, true), RequestOverrides{}, 5*time.Second); traced[0].Wait != "then waited 0s, retry.backoff; the answer has no Retry-After" {
		t.Errorf("no header: the trace says %q", traced[0].Wait)
	}
}

// A Retry-After asking for longer than the probe has left is not waited
// until the deadline: the request ends at once with that answer, which
// says why it was not retried, as the trace does. A backoff of none would
// have retried it.
func TestARetryAfterLongerThanTheTimeLeftEndsTheRequest(t *testing.T) {
	for header, asks := range map[string]string{
		"120": "2m0s",
		time.Now().Add(time.Hour).UTC().Format(http.TimeFormat): "59m",
		"99999999999999999999999":                               "longer than a year",
	} {
		target, hits := retryAfterTarget(t, http.StatusServiceUnavailable, header)
		resp, traced, took := fetchWithin(t, target.URL, retryingAfter(t, 0, true), RequestOverrides{}, 3*time.Second)
		if resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 1 || took > 2*time.Second {
			t.Fatalf("%q: answer %d after %d requests in %s; want the 503, once, at once", header, resp.StatusCode, hits.Load(), took)
		}
		if !strings.HasPrefix(resp.NotRetried, "its Retry-After asks for "+asks) || !strings.Contains(resp.NotRetried, "s left of the probe's time") {
			t.Errorf("%q: the answer says %q", header, resp.NotRetried)
		}
		if traced[0].Wait != "not retried: "+resp.NotRetried {
			t.Errorf("%q: the trace says %q", header, traced[0].Wait)
		}
	}
}

// Only an answer that is retried waits for its Retry-After: a status
// retry.statuses does not list, one the collector accepts and the last
// attempt's answer are the answer, however long their Retry-After, and say
// nothing of a retry; a listed one asking for too long does.
func TestRetryAfterIsReadOnlyOnARetriedAnswer(t *testing.T) {
	for name, edit := range map[string]func(*model.Collector){
		"a status not listed": func(c *model.Collector) { c.Request.Retry.Statuses = []string{"404"} },
		"an accepted status":  func(c *model.Collector) { c.Request.AcceptStatus = []string{"2xx", "503"} },
		"the last attempt":    func(c *model.Collector) { c.Request.Retry.Attempts = 0 },
	} {
		c := httpCollector(t, func(c *model.Collector) {
			c.Request.Retry = model.RetryConfig{Attempts: 1, RetryAfter: true}
			edit(c)
		})
		target, hits := retryAfterTarget(t, http.StatusServiceUnavailable, "120")
		resp, traced, _ := fetchWithin(t, target.URL, c, RequestOverrides{}, 3*time.Second)
		if resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 1 || resp.NotRetried != "" || traced[0].Wait != "" {
			t.Errorf("%s: answer %d after %d requests, %q, wait %q; want the 503, once, and no word of a retry", name, resp.StatusCode, hits.Load(), resp.NotRetried, traced[0].Wait)
		}
	}
	target, _ := retryAfterTarget(t, http.StatusServiceUnavailable, "120")
	if resp, _, _ := fetchWithin(t, target.URL, retryingAfter(t, 0, true), RequestOverrides{}, 3*time.Second); resp.NotRetried == "" {
		t.Error("a retried status asking for too long says nothing of it")
	}
}

// A static target's retry.retry_after replaces the collector's either
// way, and is kept as the target's own when it leaves it out.
func TestAStaticTargetsRetryAfterReplacesTheCollectors(t *testing.T) {
	on, off := true, false
	if overrides := TargetOverrides(&model.StaticTarget{Request: model.TargetRequestConfig{Retry: &model.TargetRetryConfig{RetryAfter: &on}}}); overrides.RetryAfter == nil || !*overrides.RetryAfter {
		t.Fatalf("%+v", overrides)
	}
	if got := (&model.TargetRetryConfig{RetryAfter: &off}).Over(model.RetryConfig{Attempts: 1, RetryAfter: true}); !reflect.DeepEqual(got, model.RetryConfig{Attempts: 1}) {
		t.Fatalf("over the collector's: %+v", got)
	}
	if got := (&model.TargetRetryConfig{}).Over(model.RetryConfig{RetryAfter: true}); !got.RetryAfter {
		t.Fatalf("left out: %+v", got)
	}
	target, hits := retryAfterTarget(t, http.StatusServiceUnavailable, "0")
	if resp, _, _ := fetchWithin(t, target.URL, retryingAfter(t, time.Hour, false), RequestOverrides{RetryAfter: &on}, 5*time.Second); resp.StatusCode != http.StatusOK || hits.Load() != 2 {
		t.Errorf("the target's on over the collector's off: answer %d after %d requests", resp.StatusCode, hits.Load())
	}
	target, hits = retryAfterTarget(t, http.StatusServiceUnavailable, "0")
	if resp, _, _ := fetchWithin(t, target.URL, retryingAfter(t, time.Hour, true), RequestOverrides{RetryAfter: &off}, 300*time.Millisecond); resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 1 {
		t.Errorf("the target's off over the collector's on: answer %d after %d requests", resp.StatusCode, hits.Load())
	}
}

// The debug report's retry line says when a retried answer's Retry-After
// replaces the backoff, a static target's setting included.
func TestTheRetriesLineSaysRetryAfterReplacesTheBackoff(t *testing.T) {
	const said = ", such an answer's Retry-After replacing the backoff (retry.retry_after)"
	if got := RetriesOf(retryingAfter(t, time.Second, true), RequestOverrides{}); got != "up to 1, 1s apart, on a failed connection and on an answer of 408, 425, 429, 5xx that is not accepted"+said {
		t.Errorf("on: %q", got)
	}
	off := false
	if got := RetriesOf(retryingAfter(t, time.Second, true), RequestOverrides{RetryAfter: &off}); strings.Contains(got, "Retry-After") {
		t.Errorf("a target's off: %q", got)
	}
	if got := RetriesOf(retryingAfter(t, time.Second, false), RequestOverrides{}); strings.Contains(got, "Retry-After") {
		t.Errorf("off: %q", got)
	}
}
