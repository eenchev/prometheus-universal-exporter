package allocator

import "time"

// Clock is the allocator's time: when a heartbeat was heard, when a member's
// TTL, the settle time, a draining target's interval and a long poll's wait
// run out. Every decision the allocator makes in time goes through it, so
// its tests drive membership with a clock of their own, moved by hand, and
// wait for no length of time (docs/DEVELOPMENT.md, "Repeatable tests").
type Clock interface {
	Now() time.Time
	// AfterFunc calls f, on a goroutine of its own, once d has passed.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a call AfterFunc arranged.
type Timer interface {
	// Stop keeps the call from happening, and reports whether it did.
	Stop() bool
}

// realClock is the wall clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
