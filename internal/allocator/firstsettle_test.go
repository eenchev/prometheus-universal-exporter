package allocator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/allocclient"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// rollout is a transport of three allocator instances: two from before a
// rollout, which answer every heartbeat saying their 3s interval and every
// other request 503, and a new one at newAddress, the allocator under test,
// whose handler answers it. Each heartbeat the new one hears is sent on
// heard once handled.
type rollout struct {
	handler http.Handler
	heard   chan allocapi.Heartbeat
}

const newAddress = "10.1.0.3"

func (r *rollout) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	if req.URL.Hostname() != newAddress {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		if req.URL.Path == allocapi.HeartbeatPath {
			_, _ = io.WriteString(rec, `{"heartbeat_interval_seconds":3}`)
		} else {
			http.Error(rec, "no bundle here", http.StatusServiceUnavailable)
		}
		return rec.Result(), nil
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	inner := req.Clone(req.Context())
	inner.Body = io.NopCloser(bytes.NewReader(body))
	r.handler.ServeHTTP(rec, inner)
	var hb allocapi.Heartbeat
	if req.URL.Path == allocapi.HeartbeatPath && json.Unmarshal(body, &hb) == nil {
		select {
		case r.heard <- hb:
		default:
		}
	}
	return rec.Result(), nil
}

// noReloads is a replica's configuration manager that is never asked to
// reload: the new instance forms no member list it would fetch a bundle of.
type noReloads struct{}

func (noReloads) CheckReplacement(string, string) error { return errors.New("no reload expected") }
func (noReloads) ReloadReplacing(string, string, func() (func() error, error)) error {
	return errors.New("no reload expected")
}

// The reviewer's scenario: a rollout shortens the heartbeat interval from
// 3s to 1s. A replica that started after the new instance hears it, and
// the two instances from before the rollout, still saying 3s, so it
// heartbeats every 1s and says 3s: the longest any instance it heartbeats
// has said, not the interval it heartbeats at. The new instance, heard
// from that replica first, so waits 3s for the replicas still on the old
// interval, which reach it at their next lookup on it, and its first member
// list holds all three, and does not change as they arrive; heard 1s, it
// formed at 1s of the fresh replica alone.
func TestAFreshReplicaMakesANewInstanceWaitForTheOldInterval(t *testing.T) {
	clock := newFakeClock()
	a := New(nil, Options{Settle: time.Second, HeartbeatInterval: time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	transport := &rollout{handler: a.Handler(""), heard: make(chan allocapi.Heartbeat, 64)}
	client, err := allocclient.New(allocclient.Options{
		URL: "http://pue-allocator.monitoring.svc:9116", CacheDir: t.TempDir(), Member: "fresh", Advertise: "fresh:9115",
		Manager: noReloads{}, Logger: testutil.QuietLogger(t), Transport: transport,
		Resolve: func(context.Context, string) ([]string, error) {
			return []string{"10.1.0.1", "10.1.0.2", newAddress}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// next is the fresh replica's next heartbeat the new instance handles.
	next := func() allocapi.Heartbeat {
		t.Helper()
		for len(transport.heard) > 0 {
			<-transport.heard
		}
		select {
		case hb := <-transport.heard:
			return hb
		case <-time.After(10 * time.Second):
			t.Fatal("the fresh replica did not heartbeat the new instance in 10s")
			return allocapi.Heartbeat{}
		}
	}
	// Its first heartbeat may say none, sent before any instance answered;
	// the one after it says what the instances it heard said, at once.
	deadline := time.Now().Add(5 * time.Second)
	hb := next()
	for hb.IntervalSeconds == 0 && time.Now().Before(deadline) {
		hb = next()
	}
	if hb.IntervalSeconds != 3 {
		t.Errorf("the fresh replica heartbeating at 1s said interval_seconds %v, want 3, the longest an instance said", hb.IntervalSeconds)
	}

	old := func() {
		t.Helper()
		for _, name := range []string{"old1", "old2"} {
			if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: name, Advertise: name + ":9115", IntervalSeconds: 3})); err != nil {
				t.Fatal(err)
			}
		}
	}
	clock.Advance(1500 * time.Millisecond)
	if got := effective(a); got != nil {
		t.Errorf("1.5s after the start the first member list formed, %v, though a replica said 3s", got)
	}
	// The replicas on the old interval reach the new instance; the fresh
	// one heartbeats on, each heartbeat handled before the clock moves on.
	old()
	next()
	clock.Advance(time.Second)
	next()
	old()
	clock.Advance(time.Second)
	next()
	if got, want := effective(a), []string{"fresh", "old1", "old2"}; !slices.Equal(got, want) || memberChanges(a) != 0 {
		t.Errorf("3.5s after the start the member list is %v after %d changes, want %v formed once", got, memberChanges(a), want)
	}
}
