package allocator

import (
	"sync"
	"time"
)

// fakeClock is a clock the test moves by hand (Advance): a timer's call is
// made by Advance itself, on the test's goroutine, once the clock reaches
// it, so a test of the member list's timings waits for no length of time.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock         *fakeClock
	at            time.Time
	f             func()
	stopped, done bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	pending := !t.stopped && !t.done
	t.stopped = true
	return pending
}

// Advance moves the clock on by d, making the call of every timer due on
// the way, in the order they are due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.done && !t.at.After(target) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		if next.at.After(c.now) {
			c.now = next.at
		}
		next.done = true
		c.mu.Unlock()
		next.f()
	}
}
