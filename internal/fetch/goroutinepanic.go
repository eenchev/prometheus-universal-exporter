package fetch

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A panic of a trip is recovered where the trip's goroutine starts: a
// probe's flight and a static target's scrape answer it as a scrape that
// panicked and log it, and the exporter serves on. A goroutine the fetch
// starts itself is outside that recovery: the servers of an ntp scrape and
// the addresses of an icmp one asked at once, the reader of an ICMP socket,
// the ports of a tcp check, the requests of a kubernetes scrape, the read
// of a file or a directory and the run of a command over SSH, which a
// deadline may leave behind, a login and a gRPC reflection question shared
// by the requests waiting for them. A panic
// there, in code that reads what a target answered, would end the process,
// with every other scrape, every token and every result served stale.
//
// Each such goroutine recovers its panic, logs it with its own stack, and
// hands it to the trip: goroutinePanics keeps it for the goroutine that
// waits for the others and raises it there; panicToError makes it the
// error of work whose error is what its goroutine hands over, which
// FetchCollector raises on the trip's goroutine. Either way the trip
// panics where its recovery is, and is answered and counted as one.

// goroutinePanic is the panic of a goroutine a trip started, as the value
// the trip panics with and as the error that carries it there.
type goroutinePanic struct {
	text string
}

func (p *goroutinePanic) Error() string {
	return "a goroutine of the scrape panicked: " + p.text
}

// recoveredPanic logs a panic a goroutine of a trip recovered, with the
// stack it had, which the trip's own recovery no longer has, and returns
// it to be raised on the trip's goroutine. Its text is cut as any
// failure's is: a panic says whatever its code had at hand.
func recoveredPanic(recovered any) *goroutinePanic {
	// One raised here already, by a goroutine that waited for others, is
	// logged and is passed on as it is.
	if p, ok := recovered.(*goroutinePanic); ok {
		return p
	}
	p := &goroutinePanic{text: model.CutTo(fmt.Sprint(recovered), model.MaxFailureBytes, false)}
	slog.Default().Error("a goroutine of a scrape panicked", "panic", p.text, "stack", string(debug.Stack()))
	return p
}

// goroutinePanics keeps the first panic of the goroutines a trip waits
// for. Its zero value is ready to use.
type goroutinePanics struct {
	mu    sync.Mutex
	first *goroutinePanic
}

// keep recovers the panic of the goroutine it is deferred in, directly, and
// keeps it for raise.
func (k *goroutinePanics) keep() {
	if recovered := recover(); recovered != nil {
		k.hold(recovered)
	}
}

// hold keeps a panic a goroutine recovered itself, when it has more to do
// about it than keep does.
func (k *goroutinePanics) hold(recovered any) {
	p := recoveredPanic(recovered)
	k.mu.Lock()
	if k.first == nil {
		k.first = p
	}
	k.mu.Unlock()
}

// raise panics, on the goroutine that waited for the others, with the
// panic kept, if one was.
func (k *goroutinePanics) raise() {
	k.mu.Lock()
	p := k.first
	k.mu.Unlock()
	if p != nil {
		panic(p)
	}
}

// panicToError is work's error, or work's panic as one, for a goroutine
// whose error is what it hands the trip.
func panicToError(work func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = recoveredPanic(recovered)
		}
	}()
	return work()
}

// raiseGoroutinePanic panics with the goroutine's panic err carries, if it
// carries one, however it was wrapped on its way.
func raiseGoroutinePanic(err error) {
	var p *goroutinePanic
	if errors.As(err, &p) {
		panic(p)
	}
}
