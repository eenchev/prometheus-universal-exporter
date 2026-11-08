package exporter

import (
	"context"
	"testing"
	"time"
)

// takeAll takes n slots of collector, each at once.
func takeAll(t *testing.T, s *staticSlots, collector string, n int) {
	t.Helper()
	for i := range n {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		err := s.acquire(ctx, collector)
		cancel()
		if err != nil {
			t.Fatalf("%s's slot %d: %v", collector, i+1, err)
		}
	}
}

// A collector alone in the file may hold every slot.
func TestAStaticCollectorAloneHoldsEverySlot(t *testing.T) {
	s := newStaticSlots()
	s.shape(4, 1)
	takeAll(t, s, "sql", 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.acquire(ctx, "sql"); err == nil {
		t.Error("a fifth slot of four was taken")
	}
}

// A collector whose targets are slow cannot hold the slot another
// collector needs: beyond its share it leaves one free for each other
// collector holding none, which takes it at once.
func TestASlowStaticCollectorLeavesASlotForTheOthers(t *testing.T) {
	s := newStaticSlots()
	s.shape(8, 2)
	takeAll(t, s, "sql", 7)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.acquire(ctx, "sql"); err == nil {
		t.Error("the slow collector took the last slot, leaving none for the other")
	}
	takeAll(t, s, "http", 1)
	// Of three collectors and eight slots, two holding none: one beyond
	// its share holds six at most.
	s3 := newStaticSlots()
	s3.shape(8, 3)
	takeAll(t, s3, "sql", 6)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	if err := s3.acquire(ctx2, "sql"); err == nil {
		t.Error("the slow collector took a seventh slot of eight with two others holding none")
	}
	takeAll(t, s3, "http", 1)
	takeAll(t, s3, "dns", 1)
}

// A slot freed goes to the waiting scrape whose collector holds the fewest,
// before one of a collector holding more that began to wait earlier.
func TestAFreedStaticSlotGoesToACollectorUnderItsShare(t *testing.T) {
	s := newStaticSlots()
	s.shape(4, 2)
	takeAll(t, s, "sql", 3)
	takeAll(t, s, "http", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sqlGot, httpGot := make(chan error, 1), make(chan error, 1)
	go func() { sqlGot <- s.acquire(ctx, "sql") }()
	time.Sleep(20 * time.Millisecond)
	go func() { httpGot <- s.acquire(ctx, "http") }()
	time.Sleep(20 * time.Millisecond)
	s.release("sql")
	select {
	case err := <-httpGot:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the freed slot did not go to the collector under its share")
	}
	select {
	case <-sqlGot:
		t.Error("the collector over its share took a slot too")
	case <-time.After(20 * time.Millisecond):
	}
	if s.heldBy("sql") != 2 || s.heldBy("http") != 2 {
		t.Errorf("held: sql %d, http %d", s.heldBy("sql"), s.heldBy("http"))
	}
	s.release("http")
	if err := <-sqlGot; err != nil {
		t.Fatal(err)
	}
}

// A waiter whose scrape ends gives up its place; a larger limit hands the
// slots to those waiting.
func TestAStaticSlotWaitEndsWithItsScrapeAndAReloadGrants(t *testing.T) {
	s := newStaticSlots()
	s.shape(1, 1)
	takeAll(t, s, "a", 1)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { got <- s.acquire(ctx, "a") }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-got; err == nil {
		t.Fatal("a cancelled wait took a slot")
	}
	waiting := make(chan error, 1)
	go func() { waiting <- s.acquire(context.Background(), "a") }()
	time.Sleep(20 * time.Millisecond)
	s.shape(2, 1)
	select {
	case err := <-waiting:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a larger concurrency did not hand the waiter a slot")
	}
}
