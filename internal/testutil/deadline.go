package testutil

import (
	"context"
	"sync"
)

// handDeadline is a context whose deadline the test ends itself.
type handDeadline struct {
	context.Context
	ended chan struct{}
}

// DeadlineEndedByHand is a context that ends like one whose deadline has
// passed, but only when the test calls end. A test of what happens at a
// deadline waits for what must have happened by then, and then ends it. So
// nothing depends on how long the machine took to get there. With a
// deadline on the clock, what had to come first got only that long. The
// context names no time for its deadline, since it has none.
func DeadlineEndedByHand() (ctx context.Context, end func()) {
	c := &handDeadline{Context: context.Background(), ended: make(chan struct{})}
	return c, sync.OnceFunc(func() { close(c.ended) })
}

func (c *handDeadline) Done() <-chan struct{} { return c.ended }

func (c *handDeadline) Err() error {
	select {
	case <-c.ended:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
