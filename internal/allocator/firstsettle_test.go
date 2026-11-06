package allocator

import (
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
// rollout, which answer every heartbeat saying their 3s interval, and a new
// one at newAddress, which answers saying its 1s. Every other request is
// answered 503. Each heartbeat the new one is sent is sent on heard.
type rollout struct {
	heard chan allocapi.Heartbeat
}

const newAddress = "10.1.0.3"

func (r *rollout) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	if req.URL.Path != allocapi.HeartbeatPath {
		http.Error(rec, "no bundle here", http.StatusServiceUnavailable)
		return rec.Result(), nil
	}
	if req.URL.Hostname() != newAddress {
		_, _ = io.WriteString(rec, `{"heartbeat_interval_seconds":3}`)
		return rec.Result(), nil
	}
	_, _ = io.WriteString(rec, `{"heartbeat_interval_seconds":1}`)
	var hb allocapi.Heartbeat
	if json.Unmarshal(body, &hb) == nil {
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
//
// The replica's client runs on the real clock, so it is run only until it
// says what it says; the new instance, on a fake clock, is then sent the
// heartbeats it would send, saying that, one each fake second.
func TestAFreshReplicaMakesANewInstanceWaitForTheOldInterval(t *testing.T) {
	said := freshReplicaSays(t)
	if said != 3 {
		t.Errorf("the fresh replica heartbeating at 1s said interval_seconds %v, want 3, the longest an instance said", said)
	}

	clock := newFakeClock()
	a := New(nil, Options{Settle: time.Second, HeartbeatInterval: time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	at := func(interval float64, members ...string) {
		t.Helper()
		for _, name := range members {
			if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: name, Advertise: name + ":9115", IntervalSeconds: interval})); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The fresh replica heartbeats every 1s from the start, saying what it
	// said.
	at(said, "fresh")
	clock.Advance(time.Second)
	at(said, "fresh")
	clock.Advance(500 * time.Millisecond)
	if got := effective(a); got != nil {
		t.Errorf("1.5s after the start the first member list formed, %v, though a replica said 3s", got)
	}
	// The replicas on the old interval reach the new instance; the fresh
	// one heartbeats on.
	at(3, "old1", "old2")
	clock.Advance(500 * time.Millisecond)
	at(said, "fresh")
	clock.Advance(500 * time.Millisecond)
	at(3, "old1", "old2")
	clock.Advance(500 * time.Millisecond)
	at(said, "fresh")
	clock.Advance(500 * time.Millisecond)
	if got, want := effective(a), []string{"fresh", "old1", "old2"}; !slices.Equal(got, want) || memberChanges(a) != 0 {
		t.Errorf("3.5s after the start the member list is %v after %d changes, want %v formed once", got, memberChanges(a), want)
	}
}

// freshReplicaSays is the interval_seconds a replica's client, finding the
// three instances of a rollout, says to the new one once every instance has
// answered: 3, or the last it said in 10s when it never says that. Until
// every instance has answered a heartbeat may say less, or none, sent before
// any answered.
func freshReplicaSays(t *testing.T) float64 {
	t.Helper()
	transport := &rollout{heard: make(chan allocapi.Heartbeat, 64)}
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
	defer func() {
		cancel()
		<-done
	}()
	timeout := time.After(10 * time.Second)
	var said float64
	for {
		select {
		case hb := <-transport.heard:
			if said = hb.IntervalSeconds; said == 3 {
				return said
			}
		case <-timeout:
			return said
		}
	}
}
