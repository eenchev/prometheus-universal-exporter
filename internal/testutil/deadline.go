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

// DeadlineEndedByHand is a context that ends as one whose deadline has
// passed does, when the test calls end and not before. A test of what
// happens at a deadline waits for what must have happened by then, and then
// ends it, so that nothing depends on how long the machine took to get
// there: with a deadline on the clock, what had to come first had that long
// and no longer. The context names no time for its deadline, having none.
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
