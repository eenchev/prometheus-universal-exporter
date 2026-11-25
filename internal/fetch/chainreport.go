package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync/atomic"
	"time"
)

// What a chain of requests (chain.go) tells the rest of the exporter, and
// the state its requests share through their context. These are outside
// the http type's build constraint because the shared request code reads
// them: the cookie jar, the one relogin of a trip, the redirects kept to
// the target's origin, and the step a request is counted and traced under.

// ChainReport is how the requests of a chain went, in the order of its
// steps, the first request included, each step that was not reached too.
type ChainReport struct {
	// Answer is the collector's answer: last or all.
	Answer string
	Steps  []ChainStepReport
	// Failed is where the trip failed, written as an error starts, such as
	// step "detail" or step "last_build" item 3 of 40 ("web"); "" when no
	// step failed. FailedStep is the step's name.
	Failed     string
	FailedStep string
	// FailedItem and Items are, for a fan-out item that failed the trip,
	// its place from 1 and how many items the fan-out asked for.
	FailedItem, Items int
}

// The states of a step, as a report and the workflow diagram name them.
const (
	ChainStepOK         = "ok"
	ChainStepFailed     = "failed"
	ChainStepTimedOut   = "timed out"
	ChainStepNotReached = "not reached"
)

// ChainStepReport is how one step went.
type ChainStepReport struct {
	Name   string
	Method string
	// URL is the URL the step's request was sent to, its credentials and
	// query values masked; for a fan-out, its first item's. "" for a step
	// not reached.
	URL string
	// State is one of the ChainStep states.
	State string
	// Error is why the step failed: a status, a message, or the value not
	// found; for a fan-out under skip, the first item's that failed.
	Error string
	// Deadline is whose deadline ran out, for a step that timed out.
	Deadline string
	// Status, Headers and Bytes are the step's answer: its status, its
	// headers and its body's size; for a fan-out, those of its last item
	// answered.
	Status  int
	Headers http.Header
	Bytes   int
	// Started is when the step began, and Duration how long it took.
	Started  time.Time
	Duration time.Duration
	// Requests is how many requests the step sent, each fan-out item and
	// each retry counted.
	Requests int
	// Values are the values the step read, in name order.
	Values []ChainValueReport
	// ForEach says the step is a fan-out. ItemsTotal is how many entries
	// its list had, ItemsCut how many max_items left out, ItemsFailed how
	// many items failed, and Items each item asked for, in order.
	ForEach                           bool
	OnItemError                       string
	ItemsTotal, ItemsCut, ItemsFailed int
	Items                             []ChainItemReport
	// Excerpt is the start of the step's answer, for a debug report.
	Excerpt string
	// Trip is how the step's request went ($timings and the rest), nil for
	// a fan-out and for a step without one.
	Trip *HTTPTrip
}

// ChainValueReport is a value a step read: its name, and its text, which
// a report shows as *** for a secret value.
type ChainValueReport struct {
	Name   string
	Text   string
	Secret bool
	// Found says the value was found; an optional value not found is
	// empty.
	Found bool
}

// ChainItemReport is how one item of a fan-out went.
type ChainItemReport struct {
	// Index is the item's place, from 0, and Each its entry as text.
	Index int
	Each  string
	// Entry is the entry as the list held it, for the answer document.
	Entry    any
	URL      string
	Status   int
	Headers  http.Header
	Bytes    int
	Duration time.Duration
	// Error is why the item failed, "" when it did not.
	Error string
	// Body is the item's answer, kept for the answer document.
	Body []byte
	// Trip is how the item's request went.
	Trip *HTTPTrip
}

// Reached says the step was made.
func (s ChainStepReport) Reached() bool { return s.State != ChainStepNotReached }

// ChainError is a chain that failed at a step: the error, which starts
// with where, and how the chain went until then.
type ChainError struct {
	Report *ChainReport
	err    error
}

func (e *ChainError) Error() string { return e.Report.Failed + ": " + e.err.Error() }

func (e *ChainError) Unwrap() error { return e.err }

// chainTrip is what the requests of one trip of a chain share.
type chainTrip struct {
	// origin is the target's origin, the first request's URL, which every
	// request of the chain, redirects included, keeps to.
	origin *url.URL
	// jar holds the cookies the answers set, sent with the later steps.
	// It starts empty at each trip and is never kept.
	jar http.CookieJar
	// relogged is set once a step's answer made the login log in again:
	// once per trip.
	relogged atomic.Bool
}

type chainTripKey struct{}

// chainStepState is the step a request is made for, and the count of the
// requests it sent.
type chainStepState struct {
	name     string
	item     int // the fan-out item, from 1; 0 for none
	requests *atomic.Int64
}

type chainStepKey struct{}

// withChainTrip returns ctx for the requests of a chain whose origin is
// origin.
func withChainTrip(ctx context.Context, origin *url.URL) context.Context {
	jar, _ := cookiejar.New(nil)
	return context.WithValue(ctx, chainTripKey{}, &chainTrip{origin: origin, jar: jar})
}

// chainTripOf is the chain ctx's requests belong to, nil for none.
func chainTripOf(ctx context.Context) *chainTrip {
	trip, _ := ctx.Value(chainTripKey{}).(*chainTrip)
	return trip
}

// withChainStep returns ctx for the requests of a step, or of one of its
// items.
func withChainStep(ctx context.Context, state *chainStepState) context.Context {
	return context.WithValue(ctx, chainStepKey{}, state)
}

// chainStepOf is the step ctx's request is made for, nil for none.
func chainStepOf(ctx context.Context) *chainStepState {
	state, _ := ctx.Value(chainStepKey{}).(*chainStepState)
	return state
}

// countChainRequest counts a request a step sends.
func countChainRequest(ctx context.Context) {
	if state := chainStepOf(ctx); state != nil {
		state.requests.Add(1)
	}
}

// mayRelogin reports whether the login may log in again for an answer of
// the request in ctx: always outside a chain, and once per trip of one.
func mayRelogin(ctx context.Context) bool {
	trip := chainTripOf(ctx)
	return trip == nil || trip.relogged.CompareAndSwap(false, true)
}

// chainRedirect refuses a redirect, to u, that leaves the origin of the
// chain ctx's request belongs to.
func chainRedirect(ctx context.Context, u *url.URL) error {
	trip := chainTripOf(ctx)
	if trip == nil || sameOrigin(trip.origin, u) {
		return nil
	}
	return fmt.Errorf("redirect to %s refused: it leads to another origin than the target's, %s; a chain asks one target only: %w", RedactURL(u, MaskQueryValues), originOf(trip.origin), ErrTargetRefused)
}
