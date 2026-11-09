//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// windowTarget answers value=1, or 500 while failing, and keeps the from
// and to of every request it is sent, by path.
type windowTarget struct {
	*httptest.Server
	failing atomic.Bool
	mu      sync.Mutex
	asked   map[string][][2]int64
}

func newWindowTarget(t *testing.T) *windowTarget {
	t.Helper()
	target := &windowTarget{asked: map[string][][2]int64{}}
	target.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
		target.mu.Lock()
		target.asked[r.URL.Path] = append(target.asked[r.URL.Path], [2]int64{from, to})
		target.mu.Unlock()
		if target.failing.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	return target
}

// last is the window of the last request to path.
func (w *windowTarget) last(t *testing.T, path string) [2]int64 {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	asked := w.asked[path]
	if len(asked) == 0 {
		t.Fatalf("nothing was asked of %s", path)
	}
	return asked[len(asked)-1]
}

// A window from the last success to the trip's time starts, on the first
// trip of a target, at its fallback before the trip, and afterwards where
// the last successful trip of the same collector, target and parameters
// ended, so that one tenant's success moves no other's window. A trip that
// fails moves nothing, nor does a debug probe: the next asks for its time
// again.
func TestALastSuccessIsWhereTheNextWindowStarts(t *testing.T) {
	target := newWindowTarget(t)
	c := testutil.Collector("window", "text")
	c.Request.Path = "/{{param_tenant}}"
	c.Request.Query = map[string]string{"from": "{{unix_ms~last_success(-1h)}}", "to": "{{unix_ms}}"}
	server := modeServer(t, c)
	server.logger = testutil.QuietLogger(t)
	trip := func(tenant string, code int) [2]int64 {
		t.Helper()
		time.Sleep(3 * time.Millisecond)
		if r := probeOnce(t, server, probeOf("window", target.URL)+"&param_tenant="+tenant, nil); r.Code != code {
			t.Fatalf("a probe of %s is answered %d, want %d: %s", tenant, r.Code, code, r.Body)
		}
		return target.last(t, "/"+tenant)
	}
	first := trip("a", http.StatusOK)
	if first[0] != first[1]-time.Hour.Milliseconds() {
		t.Fatalf("the first window is %v, want an hour before its end", first)
	}
	second := trip("a", http.StatusOK)
	if second[0] != first[1] || second[1] <= first[1] {
		t.Fatalf("the second window is %v, after %v", second, first)
	}
	other := trip("b", http.StatusOK)
	if other[0] != other[1]-time.Hour.Milliseconds() {
		t.Fatalf("another tenant's first window is %v, want an hour before its end", other)
	}
	target.failing.Store(true)
	failed := trip("a", http.StatusBadGateway)
	if failed[0] != second[1] {
		t.Fatalf("the failed window is %v, after %v", failed, second)
	}
	target.failing.Store(false)
	// A debug probe starts from the last success, and keeps none.
	server.SetDebug(DebugFull)
	time.Sleep(3 * time.Millisecond)
	if r := probeOnce(t, server, probeOf("window", target.URL)+"&param_tenant=a&debug=true", nil); !strings.Contains(r.Body.String(), "A probe would have answered 200") {
		t.Fatalf("the debug probe: %s", r.Body)
	}
	if debugged := target.last(t, "/a"); debugged[0] != second[1] {
		t.Fatalf("the debug probe's window is %v, want it to start at %d", debugged, second[1])
	}
	again := trip("a", http.StatusOK)
	if again[0] != second[1] || again[1] <= failed[1] {
		t.Fatalf("the window after a failure and a debug probe is %v, want it to start at %d", again, second[1])
	}
}

// A static target's last success is its own, whether the collector's
// request or the target's parameter reads it; and a trip that reads none
// keeps none.
func TestALastSuccessIsKeptPerStaticTarget(t *testing.T) {
	plain := testutil.Collector("plain", "text")
	plain.Request.Path = "/{{param_from}}"
	reading := testutil.Collector("reading", "text")
	reading.Request.Query = map[string]string{"from": "{{datetime~last_success(-1h)}}"}
	server := modeServer(t, plain, reading)
	cfg := server.manager.Get()
	plainC, readingC := &cfg.Collectors[0], &cfg.Collectors[1]
	if !readingC.Clock.Since || plainC.Clock.Since {
		t.Fatalf("the collectors read %+v and %+v", readingC.Clock, plainC.Clock)
	}
	one := model.StaticTarget{Name: "one", Target: "http://x", Params: map[string]string{"param_from": "{{date~last_success(-1d)}}"}}
	two := model.StaticTarget{Name: "two", Target: "http://x", Params: map[string]string{"param_from": "{{date~last_success(-1d)}}"}}
	fixed := model.StaticTarget{Name: "fixed", Target: "http://x", Params: map[string]string{"param_from": "2026-11-09"}}
	keyOf := func(c *model.Collector, target *model.StaticTarget) string {
		return lastSuccessKey(c, target, target.Target, fetch.TargetOverrides(target))
	}
	if keyOf(plainC, &one) == "" || keyOf(plainC, &one) == keyOf(plainC, &two) {
		t.Fatal("two targets whose parameters read the last success share a key, or have none")
	}
	if keyOf(plainC, &fixed) != "" {
		t.Fatal("a target that reads no last success has a key")
	}
	if keyOf(readingC, &fixed) == "" || keyOf(readingC, &fixed) == lastSuccessKey(readingC, nil, "http://x", fetch.TargetOverrides(&fixed)) {
		t.Fatal("a static target shares its last success with a probe of the same target")
	}
}

// What is kept is bounded: past maxLastSuccesses, the success kept
// longest ago is forgotten; and a success that started before the one
// kept does not replace it.
func TestLastSuccessesAreBounded(t *testing.T) {
	var kept lastSuccesses
	start := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	for i := range maxLastSuccesses {
		kept.record(strconv.Itoa(i), start.Add(time.Duration(i)*time.Second))
	}
	kept.record("0", start.Add(-time.Hour))
	if got := kept.get("0"); !got.Equal(start) {
		t.Fatalf("an earlier success replaced the one kept: %v", got)
	}
	kept.record("new", start.Add(time.Hour))
	if !kept.get("0").IsZero() || kept.get("1").IsZero() || kept.get("new").IsZero() || len(kept.at) != maxLastSuccesses {
		t.Fatalf("past the bound, %d are kept, the oldest %v", len(kept.at), kept.get("0"))
	}
}
