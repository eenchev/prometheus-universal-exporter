//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// tickingClock makes clockNow move a second at every reading.
func tickingClock(t *testing.T) {
	t.Helper()
	var mu sync.Mutex
	now := time.Date(2026, 11, 9, 23, 59, 58, 0, time.UTC)
	previous := clockNow
	clockNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Second)
		return now
	}
	t.Cleanup(func() { clockNow = previous })
}

// The clock placeholders of one trip are of one moment, wherever they
// stand, and a retry sends them again as they were: at a second before
// midnight, a path and a query value that each read the clock would be of
// two days.
func TestTheClockPlaceholdersOfATripAreOfOneMoment(t *testing.T) {
	tickingClock(t)
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+r.URL.Query().Get("at")+" "+r.Header.Get("X-At"))
		first := len(seen) == 1
		mu.Unlock()
		if first {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.Path = "/{{date}}"
		c.Request.Query = map[string]string{"at": "{{datetime}}"}
		c.Request.Headers = map[string]string{"X-At": "{{unix}}"}
		c.Request.Retry = model.RetryConfig{Attempts: 2, Backoff: model.Duration(time.Millisecond)}
	})
	if !c.Clock.Any || c.Clock.Since {
		t.Fatalf("the collector's clock use is %+v", c.Clock)
	}
	if _, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	want := "/2026-11-09 2026-11-09T23:59:59Z 1794268799"
	if len(seen) != 2 || seen[0] != want || seen[1] != want {
		t.Fatalf("the target was sent %q, want %q twice", seen, want)
	}
}

// A request with a clock placeholder is not revalidated, a collector's or
// a static target's: yesterday's validators say nothing of today's answer,
// and a target asked If-Modified-Since would answer today's request 304.
func TestARequestWithAClockPlaceholderIsNotRevalidated(t *testing.T) {
	dated := httpCollector(t, func(c *model.Collector) { c.Request.Path = "/{{date}}" })
	plain := httpCollector(t, func(c *model.Collector) { c.Request.Path = "/{{param_day}}" })
	if Revalidable(dated, RequestOverrides{}) {
		t.Fatal("a request with a clock placeholder is revalidated")
	}
	if !Revalidable(plain, RequestOverrides{Params: map[string]string{"param_day": "2026-11-09"}}) {
		t.Fatal("a request without one is not")
	}
	target := model.StaticTarget{Name: "t", Params: map[string]string{"param_day": "{{date}}"}}
	if Revalidable(plain, TargetOverrides(&target)) {
		t.Fatal("a static target whose parameter holds a clock placeholder is revalidated")
	}
	literal := model.StaticTarget{Name: "t", Params: map[string]string{"param_day": "{{ day }}"}}
	if !Revalidable(plain, TargetOverrides(&literal)) {
		t.Fatal("a static target whose parameter holds braces that open no clock placeholder is not revalidated")
	}
}

// A static target's parameter value may hold clock placeholders, filled in
// at each trip from the trip's clock, and checked when the targets load as
// they are written at clockSample. One that holds a {{param_...}}
// placeholder, or fills a label value, is refused.
func TestAStaticTargetsParameterMayHoldClockPlaceholders(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	c := httpCollector(t, func(c *model.Collector) { c.Request.Path = "/day/{{param_day}}" })
	target := model.StaticTarget{Name: "sofia", Target: server.URL, Collector: c.Name, Params: map[string]string{"param_day": "d-{{date:-1d@Europe/Sofia}}"}}
	if err := CheckTargetParams(c, &target); err != nil {
		t.Fatal(err)
	}
	if unused, err := CheckRequestParams(c, TargetOverrides(&target)); err != nil || len(unused) != 0 {
		t.Fatalf("checked: %v, %v", unused, err)
	}
	atClock(t, time.Date(2026, 11, 9, 22, 30, 0, 0, time.UTC))
	if _, err := FetchCollector(context.Background(), server.URL, c, TargetOverrides(&target), nil); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/day/d-2026-11-09" {
		t.Fatalf("the target was asked for %q", paths)
	}
	if target.Params["param_day"] != "d-{{date:-1d@Europe/Sofia}}" {
		t.Fatalf("the target's own value was written over: %q", target.Params["param_day"])
	}
	for value, want := range map[string]string{
		"{{param_other}}": "may hold clock placeholders, such as {{date}}, which each scrape fills in, and no {{param_...}} one",
		"{{date|json}}":   "has a filter",
		"{{date:1d}}":     "a clock placeholder written wrong",
	} {
		bad := model.StaticTarget{Name: "bad", Params: map[string]string{"param_day": value}}
		if err := CheckTargetParams(c, &bad); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), `target "bad"`) {
			t.Errorf("%s: %v, want an error with %q", value, err, want)
		}
	}
	labelled := httpCollector(t, func(c *model.Collector) {
		c.Request.Path = "/day/{{param_day}}"
		c.Transform.Labels = map[string]string{"day": "{{param_day}}"}
	})
	params, err := ParseLabelParams(labelled, func(int) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	labelled.LabelParams = params
	if err := CheckTargetParams(labelled, &target); err == nil || !strings.Contains(err.Error(), "fills a label value") {
		t.Fatalf("a clock in a labelled parameter: %v", err)
	}
	if err := CheckTargetParams(labelled, &model.StaticTarget{Name: "fixed", Params: map[string]string{"param_day": "2026-11-09"}}); err != nil {
		t.Fatal(err)
	}
}
