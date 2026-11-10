package allocclient

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a log the client writes while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// An exporter the allocator tells that another heartbeats under its member
// name says so in its own log, once while the answers keep saying it, with
// the name and what to do: it is the one of the two an operator may be
// looking at, and its share is scraped twice.
func TestAnExporterToldOfAnotherUnderItsNameSaysSo(t *testing.T) {
	a := newFakeAllocator(t)
	a.mu.Lock()
	a.sameName = true
	a.mu.Unlock()
	logs := &lockedBuffer{}
	startClient(t, a, func(o *Options) {
		o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		// Long enough that no pause of the test's machine is taken for
		// the answers having stopped saying it.
		o.HeartbeatInterval = 200 * time.Millisecond
	})
	const said = "the target allocator hears another exporter under this one's member name"
	for range 3 {
		a.nextHeartbeat(t)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), said) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := strings.Count(logs.String(), said); got != 1 || !strings.Contains(logs.String(), `"member":"pod-0"`) || !strings.Contains(logs.String(), "--allocator.member") {
		t.Fatalf("said %d times, want once, naming the member and the flag:\n%s", got, logs)
	}
}
