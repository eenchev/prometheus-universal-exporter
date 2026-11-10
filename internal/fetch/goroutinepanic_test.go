package fetch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// caught is the value fn panics with, nil when it returns.
func caught(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

// A panic of a goroutine a trip started does not end the process: it is
// kept and raised on the goroutine that waited, or handed over as the
// goroutine's error and raised from it however the error was wrapped, so
// that the trip panics where its recovery is. The first of several is the
// one raised, one raised already is passed on as it is, and work that
// panics nowhere raises nothing.
func TestAGoroutinesPanicIsTheTripsOwn(t *testing.T) {
	var panicked goroutinePanics
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer panicked.keep()
			if i > 0 {
				panic(fmt.Sprintf("reading reply %d", i))
			}
		}()
	}
	wg.Wait()
	p, ok := caught(panicked.raise).(*goroutinePanic)
	if !ok || !strings.HasPrefix(p.Error(), "a goroutine of the scrape panicked: reading reply ") {
		t.Fatalf("raised %v", p)
	}
	var none goroutinePanics
	if recovered := caught(none.raise); recovered != nil {
		t.Errorf("nothing panicked, and %v was raised", recovered)
	}

	short := []int{1}
	err := panicToError(func() error { _ = short[len(short)+2]; return nil })
	if err == nil || !strings.Contains(err.Error(), "index out of range") {
		t.Fatalf("a panic as an error: %v", err)
	}
	wrapped := fmt.Errorf("listing /srv: %w", errors.Join(errors.New("another"), err))
	if recovered, _ := caught(func() { raiseGoroutinePanic(wrapped) }).(error); !errors.Is(recovered, err) {
		t.Errorf("raised %v from the wrapped error, want the panic it carries", recovered)
	}
	if recovered := caught(func() { raiseGoroutinePanic(errors.New("connection refused")) }); recovered != nil {
		t.Errorf("an error that carries no panic raised %v", recovered)
	}
	if again := panicToError(func() error { panic(err) }); !errors.Is(again, err) || !errors.Is(err, again) {
		t.Errorf("a panic raised already became %v", again)
	}
	if err := panicToError(func() error { return context.Canceled }); !errors.Is(err, context.Canceled) {
		t.Errorf("work's own error became %v", err)
	}
}
