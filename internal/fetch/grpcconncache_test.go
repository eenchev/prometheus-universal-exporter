//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc/connectivity"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// A connection no call holds is closed and forgotten when its idle time
// has passed, with no further call to set it off, so grpc-go does not go on
// reconnecting to a target nobody asks about. One a call holds is kept
// however long the call takes, and its idle time starts when the call ends.
func TestGRPCAConnectionNoCallHoldsIsClosedOnItsOwn(t *testing.T) {
	cache := &grpcConnCache{entries: map[grpcConnKey]*grpcConnEntry{}, idleTime: 50 * time.Millisecond}
	unused, err := cache.get(grpcConnKey{dial: "127.0.0.1:1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	held, err := cache.get(grpcConnKey{dial: "127.0.0.1:2"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cache.release(unused)
	waitFor(t, "the unused connection to be closed", func() bool {
		return unused.conn.GetState() == connectivity.Shutdown && cache.size() == 1
	})
	time.Sleep(200 * time.Millisecond)
	if state := held.conn.GetState(); state == connectivity.Shutdown || cache.size() != 1 {
		t.Fatalf("the connection a call holds is %s, %d kept", state, cache.size())
	}
	cache.release(held)
	waitFor(t, "the connection to be closed once its call ended", func() bool {
		return held.conn.GetState() == connectivity.Shutdown && cache.size() == 0
	})
}

// A connection taken again within its idle time is not closed by the timer
// its earlier release started.
func TestGRPCAConnectionTakenAgainOutlivesItsFirstIdleTime(t *testing.T) {
	cache := &grpcConnCache{entries: map[grpcConnKey]*grpcConnEntry{}, idleTime: 100 * time.Millisecond}
	key := grpcConnKey{dial: "127.0.0.1:1"}
	entry, err := cache.get(key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cache.release(entry)
	again, err := cache.get(key, time.Now())
	if err != nil || again != entry {
		t.Fatalf("%v; the same connection: %v", err, again == entry)
	}
	time.Sleep(300 * time.Millisecond)
	if state := entry.conn.GetState(); state == connectivity.Shutdown {
		t.Fatal("the idle time of the first call closed the connection the second holds")
	}
	cache.release(again)
}

// The cache keeps at most its bound of connections: a new one past it
// closes the connection no call holds that was used longest ago. One a call
// holds is never closed under it: while calls hold more than the bound, as
// many are kept, and the excess is closed as their calls end.
func TestGRPCTheConnectionsKeptAreBounded(t *testing.T) {
	cache := &grpcConnCache{entries: map[grpcConnKey]*grpcConnEntry{}, most: 2}
	start := time.Now()
	get := func(port string, at time.Duration) *grpcConnEntry {
		t.Helper()
		entry, err := cache.get(grpcConnKey{dial: "127.0.0.1:" + port}, start.Add(at))
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	closed := func(entry *grpcConnEntry) bool { return entry.conn.GetState() == connectivity.Shutdown }
	a := get("1", 0)
	cache.release(a)
	b := get("2", time.Second)
	cache.release(b)
	// a was used before b: the third connection closes it.
	c := get("3", 2*time.Second)
	if !closed(a) || closed(b) || cache.size() != 2 {
		t.Fatalf("closed: a %v, b %v; %d kept", closed(a), closed(b), cache.size())
	}
	// c is held, so b, though used after a, is the one closed now.
	d := get("4", 3*time.Second)
	if !closed(b) || closed(c) || cache.size() != 2 {
		t.Fatalf("closed: b %v, c %v; %d kept", closed(b), closed(c), cache.size())
	}
	// c and d are held: a fifth connection is kept beside them.
	e := get("5", 4*time.Second)
	if closed(c) || closed(d) || closed(e) || cache.size() != 3 {
		t.Fatalf("a connection a call holds was closed, or the fifth not kept: %d kept", cache.size())
	}
	// When c's call ends, the cache is back to its bound.
	cache.release(c)
	if !closed(c) || closed(d) || closed(e) || cache.size() != 2 {
		t.Fatalf("closed: c %v, d %v, e %v; %d kept", closed(c), closed(d), closed(e), cache.size())
	}
	cache.release(d)
	cache.release(e)
	if cache.size() != 2 {
		t.Fatalf("%d kept", cache.size())
	}
}

// A reflection answer is forgotten when its TTL has passed, with no further
// call to set it off; at most the cache's bound of answers, and of bytes,
// are kept, the one a probe used longest ago forgotten first; and an answer
// larger than the bound of bytes alone is not kept.
func TestGRPCReflectionAnswersAreForgottenOnTheirOwnAndBounded(t *testing.T) {
	answers := &reflected{entries: map[reflectedKey]*reflectedEntry{}, asking: map[reflectedKey]*reflectionCall{}, ttl: time.Hour, most: 3, mostBytes: 100}
	key := func(service string) reflectedKey {
		return reflectedKey{conn: grpcConnKey{dial: "127.0.0.1:1"}, service: service}
	}
	start := time.Now()
	keep := func(service string, size int64, at time.Duration) {
		answers.mu.Lock()
		defer answers.mu.Unlock()
		answers.keepLocked(key(service), &reflectedEntry{files: &protoregistry.Files{}, fetched: start.Add(at), size: size, used: start.Add(at)})
	}
	// take is a probe that finds the answer kept.
	take := func(service string, at time.Duration) {
		t.Helper()
		if _, err := answers.files(context.Background(), &grpcConnEntry{}, key(service), 0, start.Add(at)); err != nil {
			t.Fatal(err)
		}
	}
	kept := func() []string {
		answers.mu.Lock()
		defer answers.mu.Unlock()
		var services []string
		for k := range answers.entries {
			services = append(services, k.service)
		}
		slices.Sort(services)
		return append(services, strconv.FormatInt(answers.bytes, 10))
	}
	keep("a", 10, 0)
	keep("b", 10, time.Second)
	keep("c", 10, 2*time.Second)
	take("a", 3*time.Second)
	// A fourth answer forgets b, which a probe used longest ago.
	keep("d", 10, 4*time.Second)
	if got := kept(); !slices.Equal(got, []string{"a", "c", "d", "30"}) {
		t.Fatalf("kept %v", got)
	}
	// An answer of 85 bytes forgets the two used longest ago, c and a.
	keep("e", 85, 5*time.Second)
	if got := kept(); !slices.Equal(got, []string{"d", "e", "95"}) {
		t.Fatalf("kept %v", got)
	}
	// An answer larger than the bound alone is not kept, and forgets none.
	keep("f", 101, 6*time.Second)
	if got := kept(); !slices.Equal(got, []string{"d", "e", "95"}) {
		t.Fatalf("kept %v", got)
	}
	answers.forget(key("e"))
	if got := kept(); !slices.Equal(got, []string{"d", "10"}) {
		t.Fatalf("kept %v after forgetting e", got)
	}

	answers.mu.Lock()
	answers.ttl = 50 * time.Millisecond
	answers.mu.Unlock()
	keep("g", 10, 7*time.Second)
	waitFor(t, "the answer to be forgotten after its TTL", func() bool {
		got := kept()
		return slices.Equal(got, []string{"d", "10"})
	})
}
