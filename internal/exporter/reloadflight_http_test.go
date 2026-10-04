//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Identical probes in flight share a trip only when they are of the same stay
// of their collector (probeflight.go): a trip writes under the collector's
// name what the probe that started it may.

// A probe of the collector in force does not join the trip of a probe that
// read the collector in an earlier stay, though the two have the same
// definition and so the same key: one reload changed the collector and
// another changed it back, or one removed it and another brought it back,
// while the earlier probe was at its target. It makes a trip of its own,
// whose result is cached and whose failure is logged in full and remembered,
// as a probe of a collector that stands has them; the earlier probe's trip
// leaves nothing. Joined to that trip, it was answered and left nothing
// either: no result for the next probe within cache.ttl, and a failure
// nobody was told of.
func TestAProbeDoesNotJoinTheTripOfAnEarlierStayOfItsCollector(t *testing.T) {
	for _, between := range []string{"changed and changed back", "removed and brought back"} {
		for _, fails := range []bool{false, true} {
			where := fmt.Sprintf("the collector %s, the target failing %v", between, fails)
			logs := testutil.CaptureLogs(t)
			status := http.StatusOK
			if fails {
				status = http.StatusInternalServerError
			}
			target := newGatedTarget(t, status, "value=42\n")
			other, original := cachingCollector("other", time.Hour), cachingCollector("x", time.Hour)
			changed := cachingCollector("x", time.Hour)
			changed.Metrics[0].Name = "renamed_value"
			server, _ := newCacheTestServer(t, other, original)
			path := probePath("x", target.URL, "")
			earlier := probeAsync(context.Background(), server, path, nil)
			testutil.WaitFor(t, "the earlier probe to reach the target", func() bool { return target.requests.Load() == 1 })
			// Each reload is followed before the next is made.
			if between == "changed and changed back" {
				reloadTo(t, server, other, changed)
			} else {
				reloadTo(t, server, other)
			}
			selfMetrics(t, server)
			reloadTo(t, server, other, original)
			selfMetrics(t, server)
			inForce := probeAsync(context.Background(), server, path, nil)
			testutil.WaitFor(t, "the probe of the collector in force to reach the target, on a trip of its own", func() bool { return target.requests.Load() == 2 })
			target.open()
			wantCode := http.StatusOK
			if fails {
				wantCode = http.StatusBadGateway
			}
			if a, b := <-earlier, <-inForce; a.code != wantCode || b.code != wantCode {
				t.Fatalf("%s: the earlier probe was answered %d and the one of the collector in force %d, want %d", where, a.code, b.code, wantCode)
			}
			if got := seriesValue(t, selfMetrics(t, server), `http_exporter_probes_coalesced_total{collector="x"}`); got != 0 {
				t.Errorf("%s: %v probes of the collector in force shared a trip, want none", where, got)
			}
			held, remembered := cachedOf(server, "x"), rememberedOf(server, "x")
			if fails {
				if !slices.Equal(remembered, []string{"http_status x1"}) || strings.Count(logs.String(), `"msg":"probe failed"`) != 1 {
					t.Errorf("%s: the failure of the probe of the collector in force is remembered as %v, want once, and logged in full once:\n%s", where, remembered, logs)
				}
				continue
			}
			if held != 1 || len(remembered) != 0 {
				t.Errorf("%s: %d results are cached under the collector in force and the failures %v remembered, want its probe's result alone", where, held, remembered)
			}
			// The next probe within cache.ttl is answered from that result.
			if next := probeOnce(t, server, path, nil); next.Code != http.StatusOK || target.requests.Load() != 2 {
				t.Errorf("%s: the next probe was answered %d after %d requests, want from the cache, after 2", where, next.Code, target.requests.Load())
			}
		}
	}
}

// Probes of one stay of a collector share a trip as they did, whatever
// reloads come between them: over every sequence of four steps — an
// identical probe arriving, a reload that adds a collector beside theirs, one
// that removes it again, and the same configuration loaded anew — made while
// the first probe is at its target, the target is asked once, every probe
// that arrived is answered by that one trip, and its result is cached, as on
// a server that was never reloaded.
func TestProbesOfOneStayShareATripThroughReloads(t *testing.T) {
	testutil.CaptureLogs(t)
	shared, beside := cachingCollector("shared", time.Hour), cachingCollector("beside", time.Hour)
	steps := []string{"probe", "add", "remove", "same"}
	const length = 4
	sequences := 1
	for range length {
		sequences *= len(steps)
	}
	for sequence := range sequences {
		target := newGatedTarget(t, http.StatusOK, "value=42\n")
		server, _ := newCacheTestServer(t, shared)
		path := probePath("shared", target.URL, "")
		outcomes := []<-chan probeOutcome{probeAsync(context.Background(), server, path, nil)}
		testutil.WaitFor(t, "the first probe to reach the target", func() bool { return target.requests.Load() == 1 })
		var taken []string
		for rest := sequence; len(taken) < length; rest /= len(steps) {
			step := steps[rest%len(steps)]
			taken = append(taken, step)
			switch step {
			case "probe":
				outcomes = append(outcomes, probeAsync(context.Background(), server, path, nil))
				waitForWaiters(t, server, len(outcomes))
				continue
			case "add":
				reloadTo(t, server, shared, beside)
			case "remove":
				reloadTo(t, server, shared)
			default:
				reloadTo(t, server, server.manager.Get().Collectors...)
			}
			selfMetrics(t, server)
		}
		target.open()
		for i, outcome := range outcomes {
			if got := <-outcome; got.code != http.StatusOK {
				t.Fatalf("after %v probe %d was answered %d: %s", taken, i, got.code, got.body)
			}
		}
		if n, held := target.requests.Load(), cachedOf(server, "shared"); n != 1 || held != 1 {
			t.Fatalf("after %v the target was asked %d times for %d identical probes and %d results are cached, want once and one", taken, n, len(outcomes), held)
		}
		if got := seriesValue(t, selfMetrics(t, server), `http_exporter_probes_coalesced_total{collector="shared"}`); int(got) != len(outcomes)-1 {
			t.Fatalf("after %v %v probes shared the trip, want %d", taken, got, len(outcomes)-1)
		}
		target.Close()
	}
}
