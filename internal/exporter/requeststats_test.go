package exporter

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestVerboseRequestSeriesAreCapped(t *testing.T) {
	tracker := newRequestTracker()
	for i := 0; i < VerboseRequestSeriesLimit; i++ {
		stats := tracker.statsFor(requestKey{Collector: "c", URL: fmt.Sprintf("http://h/%d", i), Method: "GET"})
		if stats == nil {
			t.Fatalf("request %d was refused below the limit", i)
		}
		stats.probes++
	}
	samples, capped := tracker.Snapshot()
	if len(samples) != VerboseRequestSeriesLimit || capped {
		t.Fatalf("at the limit: samples=%d capped=%v", len(samples), capped)
	}

	if tracker.statsFor(requestKey{Collector: "c", URL: "http://h/overflow", Method: "GET"}) != nil {
		t.Fatal("a new request past the limit must be refused")
	}
	samples, capped = tracker.Snapshot()
	if len(samples) != VerboseRequestSeriesLimit {
		t.Fatalf("the limit was exceeded: %d series", len(samples))
	}
	if !capped {
		t.Fatal("reaching the limit must be reported, not silent")
	}

	// A request already tracked keeps updating after the limit is reached.
	existing := tracker.statsFor(requestKey{Collector: "c", URL: "http://h/0", Method: "GET"})
	if existing == nil {
		t.Fatal("a tracked request must keep updating past the limit")
	}
	existing.lastStatus = http.StatusServiceUnavailable
	samples, _ = tracker.Snapshot()
	for _, sample := range samples {
		if sample.Key.URL == "http://h/0" && sample.Values.lastStatus != http.StatusServiceUnavailable {
			t.Fatalf("an existing series stopped updating at the limit: %+v", sample.Values)
		}
	}
	if VerboseRequestSeriesLimit != 1000 {
		t.Fatalf("VerboseRequestSeriesLimit=%d, want the documented 1000", VerboseRequestSeriesLimit)
	}
}

// Registering is not recording: it must never invent a scrape or overwrite one.
func TestRegisteringARequestDoesNotClaimAScrape(t *testing.T) {
	tracker := newRequestTracker()
	key := requestKey{Collector: "c", URL: "http://h", Method: "GET"}
	tracker.statsFor(key)
	samples, _ := tracker.Snapshot()
	if len(samples) != 1 || !samples[0].Values.lastScrape.IsZero() || samples[0].Values.probes != 0 {
		t.Fatalf("a registered request should start empty: %+v", samples)
	}

	stats := tracker.statsFor(key)
	stats.probes = 3
	stats.lastScrape = time.Now()
	tracker.statsFor(key)
	samples, _ = tracker.Snapshot()
	if len(samples) != 1 || samples[0].Values.probes != 3 || samples[0].Values.lastScrape.IsZero() {
		t.Fatalf("registering an already scraped request must leave it alone: %+v", samples)
	}
}

// Requests nobody asks for any more expire, so their slots come back and their
// frozen timestamps leave the endpoint; the configured static targets' do not.
func TestIdleRequestsExpire(t *testing.T) {
	tracker := newRequestTracker()
	now := time.Unix(1_700_000_000, 0)
	tracker.now = func() time.Time { return now }
	idle := requestKey{Collector: "c", URL: "http://idle", Method: "GET"}
	busy := requestKey{Collector: "c", URL: "http://busy", Method: "GET"}
	static := requestKey{Collector: "c", URL: "http://static", Method: "GET"}
	tracker.statsFor(idle)
	tracker.statsFor(busy)
	tracker.setStatic(map[requestKey]bool{static: true})

	now = now.Add(VerboseRequestIdleExpiry / 2)
	tracker.existing(busy)
	now = now.Add(VerboseRequestIdleExpiry/2 + time.Second)
	tracker.expire()
	samples, _ := tracker.Snapshot()
	var urls []string
	for _, sample := range samples {
		urls = append(urls, sample.Key.URL)
	}
	if got := strings.Join(urls, " "); got != "http://busy http://static" {
		t.Fatalf("tracked after expiry: %s, want the used and the static request", got)
	}
	if VerboseRequestIdleExpiry != time.Hour {
		t.Fatalf("VerboseRequestIdleExpiry=%v, want the documented hour", VerboseRequestIdleExpiry)
	}
}

// At the limit, an idle request makes room for a new one at once rather than
// at the next scrape of the self-metrics.
func TestAnIdleRequestMakesRoomAtTheLimit(t *testing.T) {
	tracker := newRequestTracker()
	now := time.Unix(1_700_000_000, 0)
	tracker.now = func() time.Time { return now }
	for i := 0; i < VerboseRequestSeriesLimit; i++ {
		tracker.statsFor(requestKey{Collector: "c", URL: fmt.Sprintf("http://h/%d", i), Method: "GET"})
	}
	if tracker.statsFor(requestKey{Collector: "c", URL: "http://new", Method: "GET"}) != nil {
		t.Fatal("a new request was tracked past the limit")
	}
	now = now.Add(VerboseRequestIdleExpiry + time.Second)
	if tracker.statsFor(requestKey{Collector: "c", URL: "http://new", Method: "GET"}) == nil {
		t.Fatal("idle requests did not make room for a new one")
	}
	if samples, capped := tracker.Snapshot(); len(samples) != 1 || capped {
		t.Fatalf("samples=%d capped=%v, want only the new request and the limit clear", len(samples), capped)
	}
}

// Two probes of a request not tracked yet that finish at once both count.
func TestAbsorbAddsEveryCounter(t *testing.T) {
	var a, b statsValues
	for _, v := range []*statsValues{&a, &b} {
		rv := reflect.ValueOf(v).Elem()
		for i := 0; i < rv.NumField(); i++ {
			if f := rv.Field(i); f.Kind() == reflect.Uint64 {
				// The fields are unexported; the test sets them all
				// so a counter added later cannot be left out.
				reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().SetUint(1)
			}
		}
	}
	b.lastScrape = time.Unix(1, 0)
	b.lastStatus = http.StatusTeapot
	a.absorb(b)
	rv := reflect.ValueOf(a)
	for i := 0; i < rv.NumField(); i++ {
		if rv.Field(i).Kind() == reflect.Uint64 && rv.Field(i).Uint() != 2 {
			t.Errorf("%s=%d after absorb, want 2", rv.Type().Field(i).Name, rv.Field(i).Uint())
		}
	}
	if a.lastStatus != http.StatusTeapot || !a.lastScrape.Equal(b.lastScrape) {
		t.Fatalf("the later trip's values were not taken: %+v", a)
	}
}
