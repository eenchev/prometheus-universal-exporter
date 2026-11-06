package allocclient

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// sharedAddress is one allocator address behind which two instances answer
// in turn, as behind a Service's ClusterIP or a load balancer mid-rollout:
// instance p says 2s, instance q 4s. It notes the interval each heartbeat
// said. A bundle request is answered 503.
type sharedAddress struct {
	mu   sync.Mutex
	said []float64
}

func (f *sharedAddress) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	if req.URL.Path != allocapi.HeartbeatPath {
		http.Error(rec, "no bundle here", http.StatusServiceUnavailable)
		return rec.Result(), nil
	}
	var hb allocapi.Heartbeat
	b, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	_ = json.Unmarshal(b, &hb)
	f.mu.Lock()
	f.said = append(f.said, hb.IntervalSeconds)
	n := len(f.said)
	f.mu.Unlock()
	reply := `{"heartbeat_interval_seconds":2,"instance":"p"}`
	if n%2 == 0 {
		reply = `{"heartbeat_interval_seconds":4,"instance":"q"}`
	}
	_, _ = io.WriteString(rec, reply)
	return rec.Result(), nil
}

// Two instances answering one address in turn, saying 2s and 4s, are two
// says, not one changing at every answer: the replica heartbeats every 2s,
// says 4s, logs the change of its interval once, from 10s to 2s, and its
// heartbeats do not run back to back. Keyed by the address, what it said
// flipped between 2s and 4s at every answer, each flip of the longest
// heartbeated at once and each flip of the shortest was logged.
func TestInstancesAnsweringOneAddressInTurnDoNotFlipTheInterval(t *testing.T) {
	transport := &sharedAddress{}
	var logs syncBuffer
	c := startClient(t, newFakeAllocator(t), func(o *Options) {
		o.URL = "http://pue-allocator.monitoring.svc:9116"
		o.Transport = transport
		o.HeartbeatInterval = 0
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		o.Resolve = func(context.Context, string) ([]string, error) {
			return []string{"10.1.0.9"}, nil
		}
	})
	time.Sleep(5 * time.Second)
	if got := c.heartbeatInterval(); got != 2*time.Second {
		t.Errorf("the interval in force is %s, want 2s, the shortest said", got)
	}
	transport.mu.Lock()
	said := append([]float64(nil), transport.said...)
	transport.mu.Unlock()
	// At once, again at once for each of the first two answers, then at
	// 2s and 4s: 5, and one more for a slow start.
	if len(said) > 6 {
		t.Errorf("%d heartbeats in 5s to one address at a 2s interval, want at most 6: %v", len(said), said)
	}
	if n := len(said); n < 3 || said[n-1] != 4 || said[n-2] != 4 {
		t.Errorf("the heartbeats said %v, want 4, the longest, from the third on", said)
	}
	if n := strings.Count(logs.String(), "heartbeating at the interval the target allocator says"); n != 1 {
		t.Errorf("the change of the interval was logged %d times, want once:\n%s", n, logs.String())
	}
}

// What an instance said is kept, while its address resolves, for the member
// TTL of the longest interval said and dropped after it, so a say of an
// instance behind a shared address that is gone does not stay for ever. A
// longest interval that grows heartbeats every instance at once when it is
// longer than any said before, and, when it comes back to one said before,
// at most once an interval; one that shrinks waits for the next turn.
func TestWhatAnInstanceSaidIsKeptForTheMemberTTLAndHeartbeatsEarlyOnlyWhenItGrows(t *testing.T) {
	transport := &instances{replies: map[string]string{
		"10.1.0.4": `{"heartbeat_interval_seconds":1,"instance":"p"}`,
		"10.1.0.5": `{"heartbeat_interval_seconds":2,"instance":"q"}`,
	}}
	c := newTestClient(t, newFakeAllocator(t), t.TempDir(), func(o *Options) {
		o.URL = "http://pue-allocator.monitoring.svc:9116"
		o.Transport = transport
		o.HeartbeatInterval = 0
	})
	now := time.Unix(1e9, 0)
	c.now = func() time.Time { return now }
	beats := func() bool {
		select {
		case <-c.beat:
			return true
		default:
			return false
		}
	}
	step := func(what string, wantReported time.Duration, wantBeat bool) {
		t.Helper()
		c.mu.Lock()
		reported := c.reported
		c.mu.Unlock()
		if beat := beats(); reported != wantReported || beat != wantBeat {
			t.Errorf("%s: the heartbeats say %s and heartbeating at once is %v, want %s and %v", what, reported, beat, wantReported, wantBeat)
		}
	}
	ctx := context.Background()
	c.heartbeatOne(ctx, "10.1.0.4")
	step("p said 1s, the first", time.Second, true)
	c.heartbeatOne(ctx, "10.1.0.5")
	step("q said 2s, longer than any before", 2*time.Second, true)
	c.forgetAddresses([]string{"10.1.0.4"})
	step("q's address gone", time.Second, false)
	c.heartbeatOne(ctx, "10.1.0.5")
	step("q back within the interval, saying 2s again", 2*time.Second, false)
	now = now.Add(time.Second)
	c.forgetAddresses([]string{"10.1.0.4"})
	c.heartbeatOne(ctx, "10.1.0.5")
	step("q gone and back an interval later", 2*time.Second, true)
	// p goes on answering; q, at an address that keeps resolving, no
	// longer does: 6s, the member TTL of 2s, after its last answer it has
	// no say.
	transport.set(map[string]string{"10.1.0.4": `{"heartbeat_interval_seconds":1,"instance":"p"}`})
	now = now.Add(5999 * time.Millisecond)
	c.heartbeatOne(ctx, "10.1.0.4")
	c.heartbeatOne(ctx, "10.1.0.5")
	step("q silent for 1ms less than 6s", 2*time.Second, false)
	now = now.Add(time.Millisecond)
	c.heartbeatOne(ctx, "10.1.0.4")
	step("q silent for 6s", time.Second, false)
}
